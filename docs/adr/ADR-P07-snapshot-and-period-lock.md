# ADR-P07 — Input Fingerprint, Snapshot, dan Period Lock

> Status: **Accepted** · Tanggal: 2026-10-01 · Berlaku untuk: **PayRun**

## Context / Problem

Input payroll terus berubah sampai detik terakhir: absensi dikoreksi, lembur baru di-approve, kenaikan gaji diinput terlambat. Dua pertanyaan harus selalu bisa dijawab:
1. **Sebelum final:** apakah hasil kalkulasi yang akan di-approve masih sesuai dengan data saat ini?
2. **Sesudah final:** angka final dihitung dari data versi mana, dan apakah bisa dihitung ulang dengan hasil identik (BR11)?

Selain itu, run `FINALIZED` tidak boleh berubah (BR3), dan koreksi harus tetap bisa dilakukan.

## Options

| Opsi | Cara | Kelebihan | Kekurangan |
|---|---|---|---|
| A. Tanpa lock | Approve kapan saja | Sederhana | Hasil yang di-approve bisa berbeda dari data saat ini tanpa ada yang tahu |
| B. Hard lock saat run dimulai | Semua input periode dikunci sejak `calculate` | Konsisten | HR tidak bisa memperbaiki data yang ditemukan salah saat review, harus cancel run |
| **C. Fingerprint + stale detection; hard lock hanya saat finalize** | Edit tetap boleh sebelum final; approve ditolak jika input berubah setelah kalkulasi | HR tetap bisa memperbaiki; approve dijamin sesuai data | Perlu hash input yang kanonik |
| D. Snapshot seluruh input saat calculate, abaikan perubahan berikutnya | Kalkulasi selalu dari snapshot | Konsisten | Perubahan sah setelah calculate diam-diam diabaikan |

## Decision

**C.**

### Fingerprint

```text
employee_fingerprint = SHA-256( canonical_json(EmployeeInput) || ruleset_version )
run_fingerprint      = SHA-256( concat(sorted_by_employee_id(employee_fingerprint)) )
```
- `canonical_json`: key terurut, decimal sebagai string ternormalisasi, tanggal ISO, array terurut berdasarkan ID/tanggal. Tanpa ini, data yang sama bisa menghasilkan hash berbeda.
- Disimpan di `payroll_items.input_fingerprint` dan `payroll_runs.input_fingerprint` per `calc_version`.

### Approve guard

```text
submit / approve:
  hitung ulang run_fingerprint dari data saat ini (tanpa menjalankan engine)
  jika ≠ fingerprint kalkulasi → 409 RUN_STALE {employees_changed: [...]} → wajib recalculate
```
Daftar karyawan yang berubah ditampilkan, sehingga HR tahu apa yang berubah sejak kalkulasi.

### Status periode

| Status | Edit input (absensi, cuti, lembur, gaji) untuk tanggal di periode ini |
|---|---|
| `OPEN` | Boleh |
| `PROCESSING` (ada run aktif yang belum final) | Boleh, tetapi run menjadi *stale* (terdeteksi oleh guard) |
| `FINALIZED` | **Ditolak** `PERIOD_LOCKED` (dicek di service layer + trigger DB sebagai jaring pengaman) |

### Finalize

Dalam satu transaksi:
1. Guard fingerprint sekali lagi.
2. Run `APPROVED → FINALIZED`; periode → `FINALIZED`.
3. Setiap `payroll_items` sudah memuat `input_snapshot JSONB` (EmployeeInput lengkap) dan `ruleset_version`. Snapshot ditulis saat item dihitung, sehingga finalize tidak perlu menyalin ulang data.
4. Trigger menolak UPDATE/DELETE pada `payroll_items` milik run `FINALIZED`.
5. Enqueue job payslip dan export (outbox).

### Reproducibility test

```text
untuk setiap item di run FINALIZED:
  engine.Calculate(item.input_snapshot, ruleset(item.ruleset_version)) == item.result
```
Dijalankan di integration test dan sebagai perintah ops (`payrun verify-run <id>`). Ini adalah bukti di Killer Demo C.

### Koreksi setelah final

```text
Lembur Tono Maret kurang 2 jam (ditemukan April)
→ POST /adjustments {employee, origin_period: Maret, component: OVERTIME_CORRECTION, amount/hours, reason}
→ adjustment berstatus PENDING_APPLY, masuk ke run periode OPEN berikutnya (April)
→ baris slip April: "Koreksi lembur periode Maret 2026" + referensi adjustment
→ run Maret tidak tersentuh
```

## Reason

- HR tetap produktif selama review, tetapi tidak mungkin meng-approve angka yang sudah tidak sesuai data.
- Snapshot per item + versi ruleset + pure engine = reproducibility yang bisa dibuktikan, bukan diklaim.
- Pola adjustment mencerminkan praktik akuntansi: periode yang sudah ditutup tidak dibuka ulang.

## Trade-offs

- Snapshot JSONB per item memakan storage (±beberapa KB per karyawan per bulan; dapat diterima).
- Kanonikalisasi JSON harus dijaga ketat; perubahan struct `EmployeeInput` memerlukan versi skema snapshot (`snapshot_version`).
- Menghitung fingerprint saat approve membutuhkan load seluruh input run (tanpa engine); untuk tenant besar biayanya setara satu pass baca.

## Future Migration

| Pemicu | Langkah |
|---|---|
| Kebutuhan membuka ulang periode (kasus legal) | Workflow *reopen* khusus dengan approval ganda, membuat run baru sebagai *supersedes*, run lama tetap tersimpan |
| Snapshot terlalu besar | Pindahkan snapshot ke object storage (terkompresi) dengan hash di DB |
| Perubahan struktur input | `snapshot_version` + adapter untuk membaca snapshot lama |

## Interview angle

- **"HR mengubah absensi setelah kalkulasi tapi sebelum approve?"** → Fingerprint tidak cocok → approve ditolak, karyawan yang berubah ditampilkan → recalculate.
- **"Bagaimana membuktikan payroll Januari tahun lalu benar?"** → Recompute dari snapshot dengan versi ruleset tersimpan; hasil harus identik.
- **"Kenapa tidak lock input sejak run dimulai?"** → HR justru menemukan kesalahan data saat review; hard lock memaksa mereka cancel run.

## Amandemen Sprint 9 (PayRun, 2026-10-10): hasil E-2, approve tetap sinkron

Implementasi (S5) tidak menghitung ulang fingerprint seluruh run seperti bagian *Approve guard* di atas. Approve hanya
memverifikasi **kandidat dari change log** (`payroll_input_changes` sejak `calc_started_at`, O12), maksimal 200
(final review R1-7); di atas itu langsung `RUN_STALE` tanpa verifikasi. Poin *Trade-offs* ketiga ("load seluruh input run")
tidak berlaku lagi.

E-2 / PR-4 (tenant 10.000, laptop, `perf/results/2026-10-10-fed1df1/REPORT.md`): approve p95 31 ms (10 perubahan),
55 ms (200), 11 ms (1.000 → di atas batas); recompute penuh 10.000 karyawan ±1,2 dtk, yang di produksi akan memegang
lock periode `FOR UPDATE` selama itu. Aturan keputusan (pindah ke job `payroll.verify_stale` + `PENDING_VERIFICATION`
bila 1.000 perubahan > 2 dtk) **tidak terpicu**: approve tetap satu langkah sinkron. Pemicu peninjauan ulang: batas 200
dinaikkan, atau p95 approve di VM target > 500 ms.

## Related

ADR-P03 (pure engine), ADR-P05 (calc_version), ADR-P06 (normalisasi decimal), ADR-P09 (approval), `02-product-strategy.md` W4–W5, Demo C.
