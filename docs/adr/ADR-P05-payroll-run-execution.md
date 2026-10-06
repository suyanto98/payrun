# ADR-P05 — Eksekusi Payroll Run: State Machine + Chunked Async Jobs

> Status: **Accepted** · Tanggal: 2026-10-01 · Berlaku untuk: **PayRun**

## Context / Problem

Payroll run menghitung puluhan sampai ribuan karyawan. Kebutuhannya:
- tidak boleh ada dua run aktif untuk periode yang sama (BR2), walau dua admin menekan tombol bersamaan;
- worker bisa crash di tengah; hasil akhir harus **identik** dengan run tanpa crash, tanpa item dobel (Killer Demo B);
- run bisa dihitung ulang selama belum di-approve;
- progres terlihat oleh user, dan request HTTP tidak menunggu kalkulasi selesai.

## Options

| Opsi | Cara | Masalah |
|---|---|---|
| A. Sinkron dalam request HTTP | Hitung semua lalu response | Timeout HTTP, tidak tahan crash, memblokir koneksi |
| B. Satu job, satu transaksi raksasa | Semua karyawan dalam satu tx | Lock lama, rollback total jika satu karyawan gagal, tidak ada progres, retry mengulang semua |
| C. Satu job per karyawan | Ribuan job kecil | Overhead job tinggi, deteksi selesai lebih mahal, antrean tenant lain terdesak |
| **D. Orkestrator + chunk job (±50 karyawan)** | Run dipecah ke chunk, tiap chunk satu tx | Butuh deteksi selesai dan desain idempotensi item |

## Decision

**D**, memakai job runner shared (ADR-006).

### State machine

```text
            ┌──────────── cancel ─────────────┐
            ▼                                  │
DRAFT ──calculate──▶ CALCULATING ──all chunks ok──▶ CALCULATED ──submit──▶ PENDING_APPROVAL
                         │                        ▲      │                      │      │
                         └─ chunk dead ─▶ FAILED ─┘      └─recalculate─┐   approve  reject
                                (recalculate)                          │      │      │
                                                     CALCULATING ◀─────┘      ▼      │
                                                                         APPROVED    │
                                                                             │       │
                                                                         finalize    │
                                                                             ▼       │
                                                                         FINALIZED   │
                                     CALCULATED ◀─────── (reject: catatan wajib) ────┘
CANCELLED: dari DRAFT / CALCULATED / FAILED. FINALIZED dan CANCELLED adalah status terminal.
```

- `reject` mengembalikan run ke `CALCULATED` dan menyimpan alasan di `approval_requests` (ADR-P09). Tidak ada status `REJECTED` terpisah di run, jadi riwayat penolakan ada di approval.
- `finalize` dipisah dari `approve` agar finalisasi (snapshot, period lock, memicu job payslip) adalah langkah eksplisit dan idempoten (ADR-P07).
- Setiap transisi: `UPDATE payroll_runs SET status = $new WHERE id = $id AND status = $expected` → 0 baris = 409 `INVALID_STATE_TRANSITION`.

### Satu run aktif per periode

```sql
CREATE UNIQUE INDEX uq_active_run_per_period
  ON payroll_runs (tenant_id, period_id)
  WHERE status NOT IN ('CANCELLED');
```
Dua klik bersamaan: satu INSERT berhasil, satu kena `23505` → API mengembalikan run yang sudah ada (idempoten, header `Idempotency-Key` juga didukung).

### Orkestrasi

```text
POST /payroll-runs/{id}/calculate
  tx: run DRAFT|CALCULATED|FAILED → CALCULATING, calc_version += 1,
      chunks_total = ceil(n/50), chunks_done = 0, enqueue job "payroll.plan"
payroll.plan  (tenant_id, run_id, calc_version)
  → tentukan daftar karyawan eligible (BR5), urutkan employee_id, pecah per 50
  → INSERT chunk job "payroll.calc_chunk" (unique key: run_id+calc_version+chunk_no)
payroll.calc_chunk
  tx (WithTenantTxFor):
    cek run masih CALCULATING dan calc_version cocok (jika tidak: no-op, selesai)
    load input untuk 50 karyawan (prefetch, tanpa N+1)
    engine.Calculate per karyawan
    UPSERT payroll_items ON CONFLICT (run_id, employee_id, calc_version) DO UPDATE
    UPDATE payroll_runs SET chunks_done = chunks_done + 1
      WHERE id = $run AND calc_version = $v RETURNING chunks_done, chunks_total
    jika chunks_done = chunks_total → hitung total, status → CALCULATED
```

Penanda chunk selesai dan update `chunks_done` berada di **transaksi yang sama** dengan upsert item, sehingga chunk yang di-retry setelah commit tidak menambah counter dua kali (job runner menandai job selesai di tx yang sama; lihat ADR-006). Sebagai jaring pengaman, transisi ke `CALCULATED` memverifikasi `count(items) = jumlah karyawan eligible`.

### Crash & resume

| Kapan crash | Yang terjadi |
|---|---|
| Sebelum commit chunk | Tx rollback; lease job habis → job diambil ulang → chunk dihitung ulang dari awal |
| Setelah commit chunk | Job sudah ditandai selesai dalam tx yang sama; tidak diulang |
| Chunk gagal terus (data rusak) | Setelah `max_attempts` → `dead` → run `FAILED` dengan daftar karyawan bermasalah; HR memperbaiki data → recalculate |

### Recalculation

`calc_version` naik; item versi lama tetap ada sampai run `CALCULATED` untuk versi baru, lalu dihapus (atau disimpan untuk diff). Chunk job versi lama yang masih tertinggal di antrean menjadi no-op karena versinya tidak cocok.

## Reason

- Chunk = unit transaksi + unit retry: crash hanya mengulang ≤ 50 karyawan, lock pendek.
- Idempotensi ditegakkan oleh unique constraint, bukan oleh harapan bahwa job dijalankan tepat sekali (at-least-once delivery).
- `calc_version` menyelesaikan race antara recalculation dan chunk lama.

## Trade-offs

- Lebih banyak bagian bergerak dibanding satu job: plan, chunk, counter, versi.
- Selama `CALCULATING`, total run belum tersedia (UI menampilkan progres `chunks_done/chunks_total`).
- Ukuran chunk 50 adalah tebakan awal; diukur ulang saat load test (Step 8).

## Future Migration

| Pemicu | Langkah |
|---|---|
| Tenant besar memonopoli worker (noisy neighbor) | Batas konkurensi chunk per tenant di job runner (P2) |
| Run > 10.000 karyawan terlalu lambat | Tambah instance worker (chunk sudah independen); tuning ukuran chunk |
| Butuh orkestrasi multi-langkah yang lebih kompleks | Pertimbangkan workflow engine (Temporal) hanya jika langkah bertambah signifikan |

## Interview angle

- **"Worker mati setelah menulis item tapi sebelum menandai job selesai?"** → Keduanya di satu transaksi; tidak ada keadaan di antaranya.
- **"Kenapa tidak satu transaksi untuk semua karyawan?"** → Lock panjang, satu error membatalkan semua, tidak ada progres, retry mahal.
- **"Admin klik recalculate saat chunk lama masih berjalan?"** → `calc_version` naik; chunk lama no-op karena versinya tidak cocok.

## Related

ADR-006 (job runner), ADR-P03 (engine), ADR-P07 (fingerprint & finalize), ADR-P09 (approval), ADR-011 (transaksi).
