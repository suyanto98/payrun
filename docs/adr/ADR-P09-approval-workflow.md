# ADR-P09 — Approval Workflow: Generic, Single-Level, Approver Snapshot

> Status: **Accepted** · Tanggal: 2026-10-01 · Berlaku untuk: **PayRun**

## Context / Problem

Tiga jenis objek membutuhkan persetujuan: **cuti**, **lembur**, dan **payroll run**. Kebutuhannya:
- approver yang benar (atasan langsung untuk cuti/lembur; payroll approver untuk run), tidak bisa menyetujui request miliknya sendiri (BR7);
- approve dan reject yang terjadi bersamaan tidak boleh sama-sama berhasil;
- efek bisnis (contoh: saldo cuti berkurang) terjadi **tepat ketika** request disetujui, tidak lebih awal dan tidak dobel;
- jejak lengkap untuk sengketa (strategi produk, pain point Arif).

## Options

| Opsi | Kelebihan | Kekurangan |
|---|---|---|
| A. Kolom status + approver langsung di setiap tabel (leave, overtime, run) | Sederhana per fitur | Logika approval terduplikasi 3×; audit tersebar |
| **B. Tabel generik `approval_requests` + handler efek per subject type** | Satu mesin approval, satu jejak audit, inbox approver terpadu | Relasi polimorfik (tanpa FK langsung ke subject) |
| C. Workflow/BPMN engine (Camunda, Temporal) | Multi-level, timer, eskalasi | Infrastruktur dan konsep besar untuk alur satu langkah |
| D. Rule engine approval yang bisa dikonfigurasi tenant | Fleksibel | Kombinasi aturan meledak; sulit diuji |

## Decision

**B**, satu tingkat.

```text
approval_requests
  id, tenant_id
  subject_type      LEAVE_REQUEST | OVERTIME_REQUEST | PAYROLL_RUN
  subject_id        uuid
  requester_id      uuid
  approver_id       uuid        -- di-snapshot saat submit
  status            PENDING | APPROVED | REJECTED | CANCELLED
  decision_note     text        -- wajib untuk REJECTED
  decided_at, created_at
  UNIQUE (tenant_id, subject_type, subject_id) WHERE status = 'PENDING'
  CHECK  (approver_id <> requester_id
          OR (self_approved AND subject_type = 'PAYROLL_RUN' AND self_approval_reason IS NOT NULL))
```

### Penentuan approver (saat submit)

| Subject | Approver |
|---|---|
| Cuti / lembur | Atasan langsung karyawan **pada tanggal submit** (effective-dated, ADR-P04). Jika tidak ada atasan, atau atasannya adalah requester sendiri: fallback ke user dengan role `HR_ADMIN` |
| Payroll run | User dengan role `PAYROLL_ADMIN`/`OWNER` **selain** pihak yang men-submit. Jika tenant hanya punya satu admin: owner boleh self-approve dengan flag `self_approved` dan alasan wajib, tercatat di audit (trade-off untuk UMKM kecil) |

Approver di-snapshot agar perubahan struktur organisasi setelah submit tidak memindahkan request ke orang lain secara diam-diam. Reassign hanya bisa dilakukan HR secara eksplisit dan ter-audit.

### Transisi (optimistic guard)

```sql
UPDATE approval_requests
SET status = 'APPROVED', decided_at = now()
WHERE id = $1 AND status = 'PENDING' AND approver_id = $actor
RETURNING subject_type, subject_id;
-- 0 baris → 409 (sudah diputuskan / dibatalkan) atau 404 (bukan approver)
```

**Efek bisnis berada di transaksi yang sama** dengan transisi:

| Subject | Efek saat APPROVED | Efek saat REJECTED |
|---|---|---|
| Cuti | `leave_balances` dikurangi (`CHECK balance >= 0`, row lock pada saldo); request → APPROVED | Request → REJECTED |
| Lembur | Request → APPROVED (dihitung di payroll jika sebelum cut-off) | Request → REJECTED |
| Payroll run | Fingerprint guard (ADR-P07) → run `APPROVED` | Run kembali ke `CALCULATED` (ADR-P05) |

Jika efek gagal (contoh: saldo cuti tidak cukup karena cuti lain sudah disetujui lebih dulu), seluruh transaksi rollback dan approval tetap `PENDING` dengan error yang jelas.

**Cancel:** requester bisa membatalkan selama `PENDING` (`WHERE status='PENDING' AND requester_id=$actor`). Membatalkan cuti yang sudah `APPROVED` adalah request baru (pembatalan cuti) yang mengembalikan saldo, bukan mengedit status lama.

**Audit:** setiap transisi menulis `audit_log` (actor, before/after, note) di transaksi yang sama. Notifikasi ke approver/requester lewat outbox (P3 untuk email; inbox di UI cukup untuk MVP).

## Reason

- Satu mesin approval untuk tiga subject: logika, test, dan inbox approver terpusat.
- Partial unique index + guard `WHERE status='PENDING'` menyelesaikan race approve vs reject vs cancel tanpa lock eksplisit.
- Efek di transaksi yang sama membuat "approved tetapi saldo tidak berkurang" tidak mungkin terjadi.

## Trade-offs

- `subject_id` polimorfik tanpa FK; integritas dijaga aplikasi dan test (dan cleanup saat subject dihapus, yang memang dilarang untuk subject yang sudah punya approval).
- Satu tingkat tidak memenuhi organisasi yang butuh persetujuan berjenjang.
- Fallback self-approve untuk run di tenant satu-admin adalah kompromi kontrol internal; diberi tanda jelas di audit.

## Future Migration

| Pemicu | Langkah |
|---|---|
| Kebutuhan multi-level (manager → HR) | Tambah `approval_steps (request_id, step_no, approver_id, status)`; `approval_requests` menjadi header. Aturan langkah per subject type sebagai konfigurasi tenant |
| Eskalasi otomatis (request menggantung > N hari) | Scheduled job yang menandai/meneruskan ke HR (memakai job runner, ADR-006) |
| Alur panjang dengan timer, kompensasi, dan banyak sistem | Baru pertimbangkan workflow engine (Temporal). Tidak untuk alur satu-dua langkah |

## Interview angle

- **"Approve dan reject diklik bersamaan?"** → `UPDATE … WHERE status='PENDING'`; hanya satu yang mendapatkan baris, yang lain 409.
- **"Atasan berubah setelah request dibuat?"** → Approver di-snapshot saat submit; reassign eksplisit dan ter-audit.
- **"Kenapa tidak Camunda/Temporal?"** → Alurnya satu langkah. Engine workflow menambah infrastruktur, konsep, dan titik kegagalan tanpa masalah nyata yang diselesaikan.

## Amandemen (kontrak API, 2026-10-01)

| Topik | Keputusan |
|---|---|
| Endpoint | Approval run tidak punya endpoint sendiri. Semua lewat `/approvals/{id}/approve\|reject` (satu mesin). Dispatch `PAYROLL_RUN` menjalankan fingerprint guard + `payroll_periods FOR UPDATE` (desain domain O3) |
| Kapan alasan self-approval diisi | Saat **submit** run (bukan saat approve), karena `ck_approval_requests_not_self` mewajibkan reason terisi ketika baris approval dibuat dengan approver = requester |
| Undangan | Accept wajib dilakukan oleh user login yang email-nya sama dengan `invited_email`. Secret undangan ada di fragment URL (`#…`) agar tidak bocor lewat Referer/log (threat model T-PR) |
| Fallback approver cuti/lembur | Atasan langsung → `HR_ADMIN` → `OWNER` (kalau `HR_ADMIN` satu-satunya adalah requester itu sendiri) |
| Validasi pengecualian satu admin | **Dihitung server** saat submit (jumlah membership aktif dengan role `OWNER`/`PAYROLL_ADMIN`), tidak pernah dari klaim client (threat model T-PR) |
| Override HR | Hanya untuk cuti/lembur. **Tidak** berlaku untuk `PAYROLL_RUN` |

## Related

ADR-P04 (atasan effective-dated), ADR-P05 (status run), ADR-P07 (fingerprint guard), ADR-010 (authorization berbasis relasi), ADR-006 (outbox notifikasi).
