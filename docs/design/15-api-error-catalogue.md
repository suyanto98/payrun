# Step 5.1 — Error Catalogue

> Format respons: RFC 9457 (lihat [`00-index.md`](00-index.md) §4). `code` adalah kontrak: tidak boleh diganti nama tanpa versi API baru.
> Kolom **Retry?**: apakah request **yang sama persis** (dengan `Idempotency-Key` yang sama) layak diulang.
> **Aturan:** endpoint di file 02/04 hanya boleh memakai code dari katalog ini. Code baru harus ditambahkan di sini lebih dulu.

---

## 1. Generic (kedua project)

| HTTP | Code | Arti | Retry? | Tindakan client | Sumber |
|---|---|---|---|---|---|
| 400 | `VALIDATION_ERROR` | Field tidak valid; detail di `errors[]` | Tidak | Perbaiki input | Validator |
| 400 | `MALFORMED_JSON` | Body bukan JSON valid | Tidak | Perbaiki body | Decoder |
| 400 | `IDEMPOTENCY_KEY_REQUIRED` | Header `Idempotency-Key` wajib tetapi tidak ada | Tidak | Kirim dengan key | ADR-L02 |
| 400 | `INVALID_CURSOR` | Cursor rusak atau kedaluwarsa | Tidak | Mulai dari halaman pertama | Pagination |
| 401 | `UNAUTHENTICATED` | Token tidak ada atau tidak valid | Tidak | Login ulang | Middleware auth |
| 401 | `TOKEN_EXPIRED` | Access token kedaluwarsa | Ya, setelah refresh | Panggil `/auth/refresh`, lalu ulangi | Middleware auth |
| 401 | `INVALID_CREDENTIALS` | Email/password salah (pesan tidak membedakan keduanya) | Tidak | — | Login |
| 401 | `REFRESH_TOKEN_REUSED` | Refresh token lama dipakai ulang; seluruh family dicabut | Tidak | Login ulang | ADR-009 |
| 403 | `FORBIDDEN` | Resource terlihat, tetapi aksi tidak diizinkan untuk role ini | Tidak | — | Policy (ADR-010) |
| 403 | ~~`ACCOUNT_LOCKED`~~ | **Tidak dikirim ke client (Step 12 R2-5).** Login terkunci menjawab `401 INVALID_CREDENTIALS` + `Retry-After`, sama persis dengan kredensial salah, agar tidak menjadi oracle enumerasi akun. Status lock hanya di log/metrik | Setelah `Retry-After` | Tunggu | `users.locked_until` |
| 404 | `NOT_FOUND` | Resource tidak ada **atau** tidak boleh diketahui pemanggil | Tidak | — | Scoping query / RLS |
| 409 | `REQUEST_IN_PROGRESS` | Request dengan key yang sama masih diproses | Ya, sama persis | Ulangi dengan backoff | ADR-L02 (`lock_timeout`) |
| 409 | `RESOURCE_BUSY` | Lock resource tidak didapat dalam `lock_timeout` (2 dtk) | Ya, sama persis | Ulangi dengan backoff | ADR-011 (`55P03`) |
| 409 | `INVALID_STATE_TRANSITION` | Aksi tidak sah untuk status resource saat ini | Tidak | Muat ulang resource | Guard `WHERE status = …` |
| 409 | `IMMUTABLE_RECORD` | Mencoba mengubah data append-only / immutable | Tidak | — | Trigger `42501` |
| 409 | `EMAIL_ALREADY_REGISTERED` | Email sudah terdaftar | Tidak | Login atau reset | `uq_users_email` |
| 412 | `VERSION_CONFLICT` | `If-Match` tidak cocok; resource diubah pihak lain | Tidak | Muat ulang, terapkan ulang perubahan | `version` |
| 413 | `PAYLOAD_TOO_LARGE` | Body melebihi batas | Tidak | Perkecil | HTTP layer |
| 415 | `UNSUPPORTED_MEDIA_TYPE` | Content-Type tidak didukung | Tidak | — | HTTP layer |
| 422 | `IDEMPOTENCY_KEY_REUSED` | Key yang sama dipakai dengan payload berbeda | Tidak | Pakai key baru | ADR-L02 (`request_hash`) |
| 428 | `PRECONDITION_REQUIRED` | `If-Match` wajib tetapi tidak dikirim | Tidak | Kirim `If-Match` | HTTP layer |
| 429 | `RATE_LIMITED` | Melebihi rate limit | Setelah `Retry-After` | Backoff | Rate limiter |
| 500 | `INTERNAL_ERROR` | Error tak terduga (detail hanya di log, tertaut `request_id`) | Ya, dengan key yang sama | Backoff | — |
| 503 | `SERVICE_UNAVAILABLE` | Dependency (DB) tidak tersedia; readiness gagal | Ya, dengan key yang sama | Backoff | `platform/db` |

> **Kenapa 5xx aman di-retry dengan key yang sama:** kegagalan sebelum commit tidak menyimpan key (ADR-L02). Kegagalan setelah commit, ketika response tidak sampai, menyimpan key beserta response-nya, sehingga retry mendapat replay (FS-03, FS-04).

---

## 2. LedgerPay

| HTTP | Code | Arti | Retry? | Extension | Sumber / constraint |
|---|---|---|---|---|---|
| 422 | `INSUFFICIENT_BALANCE` | Saldo wallet (user atau merchant untuk refund) tidak cukup | Tidak (disimpan di idempotency; pakai key baru) | — | APP + `ck_accounts_balance_non_negative` |
| 422 | `LIMIT_EXCEEDED` | Melebihi batas transaksi/saldo yang dikonfigurasi (LF-21, P2) | Tidak | `limit`, `window` | APP |
| 422 | `SELF_TRANSFER` | Pengirim = penerima | Tidak | — | APP |
| 422 | `INVALID_RECIPIENT` | Penerima tidak ditemukan atau tidak bisa menerima. Pesan generik agar tidak bisa dipakai untuk enumerasi | Tidak | — | APP |
| 422 | `ACCOUNT_FROZEN` | Akun pengirim/penerima `FROZEN` | Tidak | — | `accounts.status` |
| 422 | `CURRENCY_NOT_SUPPORTED` | Selain `IDR` | Tidak | — | APP + CHECK |
| 422 | `PAYMENT_EXPIRED` | Konfirmasi payment intent yang sudah kedaluwarsa | Tidak | — | `payment_intents.expires_at` |
| 422 | `REFUND_EXCEEDS_PAYMENT` | Σ refund akan melebihi amount pembayaran | Tidak | `refundable_amount` | APP + `ck_payment_intents_refund_bound` |
| 409 | `DUPLICATE_MERCHANT_ORDER` | `merchant_order_id` sudah punya intent aktif/berhasil | Tidak | `existing_resource_id` | `uq_payment_intents_merchant_order` |
| 409 | `API_KEY_LIMIT_REACHED` | Sudah ada 2 API key aktif | Tidak | — | `uq_merchant_api_keys_active_slot` |
| 409 | `ALREADY_REVERSED` | Journal sudah pernah di-reverse | Tidak | — | `uq_journals_reversal_of` |
| 401 | `INVALID_SIGNATURE` | Signature webhook masuk tidak valid | Tidak | — | ADR-L08 |
| 400 | `STALE_WEBHOOK` | Timestamp webhook di luar window 5 menit (anti-replay) | Tidak | — | ADR-L08 |
| 422 | `RESOLUTION_EVIDENCE_REQUIRED` | Aksi ops (resolve payout/mismatch) tanpa referensi bukti provider + alasan | Tidak | — | APP (LF-18) |

**Catatan:** webhook duplikat **bukan** error. Respons-nya `200` (ADR-L08), supaya provider berhenti retry.

---

## 3. PayRun

| HTTP | Code | Arti | Retry? | Extension | Sumber / constraint |
|---|---|---|---|---|---|
| 409 | `PERIOD_LOCKED` | Perubahan input menyentuh periode `FINALIZED` | Tidak | `period` | `PeriodGuard` + trigger |
| 409 | `RUN_FINALIZED` | Mengubah run/item/line yang sudah final | Tidak | — | Trigger `P0001` |
| 409 | `RUN_STALE` | Input berubah sejak kalkulasi; approve ditolak | Tidak; recalculate dulu | `employees_changed[]` | Fingerprint (ADR-P07) |
| 409 | `ACTIVE_RUN_EXISTS` | Sudah ada run aktif untuk periode ini (admin lain) | Tidak | `existing_resource_id` | `uq_payroll_runs_active_per_period` |
| 409 | `APPROVAL_ALREADY_DECIDED` | Approval sudah diputuskan/dibatalkan pihak lain | Tidak | `status` | Guard `WHERE status='PENDING'` |
| 409 | `IMPORT_DUPLICATE` | File dengan hash yang sama sudah diimpor (status bukan FAILED) | Tidak | `existing_resource_id` | `uq_attendance_imports_file` |
| 409 | `LAST_OWNER` | Mencabut/menurunkan `OWNER` terakhir | Tidak | — | Trigger PI-16 |
| 409 | `MEMBERSHIP_EXISTS` | User sudah anggota tenant ini | Tidak | — | `uq_memberships_user` |
| 409 | `EMPLOYEE_NO_TAKEN` | Nomor karyawan sudah dipakai di tenant | Tidak | — | `uq_employees_employee_no` |
| 409 | `NIK_TAKEN` | NIK sudah terdaftar di tenant (blind index). Pesan tidak menampilkan siapa pemiliknya | Tidak | — | `uq_employees_nik_bidx` |
| 409 | `OVERLAPPING_PERIOD` | Record effective-dated beririsan | Tidak | — | `ex_*_no_overlap` (EXCLUDE) |
| 409 | `OVERLAPPING_LEAVE` | Cuti beririsan dengan cuti `PENDING`/`APPROVED` lain | Tidak | — | `ex_leave_requests_no_overlap` |
| 422 | `RUN_HAS_ERRORS` | Submit run yang masih punya item `ERROR` (belum di-exclude) | Tidak | `error_count` | APP |
| 422 | `INSUFFICIENT_LEAVE_BALANCE` | Saldo cuti tidak cukup saat approve | Tidak | `balance` | APP + `ck_leave_balances_min` |
| 422 | `COMPONENT_CYCLE` | Dependensi `PERCENT_OF` membentuk siklus | Tidak | `cycle[]` | APP (validasi graf) |
| 422 | `INVALID_COMPONENT_REFERENCE` | `PERCENT_OF` merujuk komponen/base yang tidak ada atau stage yang salah | Tidak | — | APP |
| 422 | `MANAGER_CYCLE` | Penugasan atasan membentuk siklus | Tidak | — | Trigger PI-19 |
| 422 | `DECISION_NOTE_REQUIRED` | Reject tanpa catatan | Tidak | — | APP |
| 422 | `SELF_APPROVAL_REASON_REQUIRED` | Self-approve run (tenant satu admin) tanpa alasan | Tidak | — | `ck_approval_requests_not_self` |
| 403 | `SELF_APPROVAL_FORBIDDEN` | Self-approval di luar pengecualian ADR-P09 | Tidak | — | APP + CHECK |
| 422 | `EMPLOYEE_NOT_ACTIVE` | Aksi untuk employee yang tidak aktif pada tanggal tersebut | Tidak | — | APP |
| 422 | `CLOCK_OUT_WITHOUT_CLOCK_IN` | Clock out tanpa clock in di hari itu | Tidak | — | APP |
| 409 | `DUPLICATE_CODE` | Kode/slug/tanggal unik pada master data sudah dipakai (slug tenant, kode departemen/posisi/komponen, tanggal libur) | Tidak | `field` | `uq_*_code` / `uq_tenants_slug` / `uq_holidays_date` |
| 409 | `PAYSLIP_NOT_READY` | PDF slip masih `PENDING` atau `FAILED` (sedang di-render/di-retry) | Ya, setelah `Retry-After` | — | `payslips.status` |
| 410 | `INVITATION_INVALID` | Token undangan kedaluwarsa, sudah dipakai, atau tidak valid (satu code agar tidak bisa dipakai menebak) | Tidak | — | `invitations` |

---

## 4. Pemetaan error database → code (dipakai `platform/db`)

| Error Postgres | Kondisi | Code |
|---|---|---|
| `23505` unique violation | Nama constraint dipetakan per tabel (Step 4 §mapping) | Code spesifik (contoh `EMAIL_ALREADY_REGISTERED`), atau alur replay untuk `uq_idempotency_keys_scope` |
| `23514` check violation | `ck_accounts_balance_non_negative` / `ck_leave_balances_min` / `ck_journals_balanced` | `INSUFFICIENT_BALANCE` / `INSUFFICIENT_LEAVE_BALANCE` / `INTERNAL_ERROR` (journal tidak seimbang = bug; alert) |
| `23P01` exclusion violation | `ex_*` | `OVERLAPPING_PERIOD` / `OVERLAPPING_LEAVE` |
| `23503` FK violation | Referensi ke resource yang tidak ada (atau milik tenant lain, terhalang composite FK) | `NOT_FOUND` (tidak membedakan keduanya) |
| `55P03` lock not available | `lock_timeout` | `RESOURCE_BUSY` atau `REQUEST_IN_PROGRESS` (pada insert idempotency) |
| `40P01` / `40001` | Deadlock / serialization | Retry internal ≤ 3× (ADR-011); habis → `RESOURCE_BUSY` |
| `57014` query canceled | `statement_timeout` | `SERVICE_UNAVAILABLE` |
| `42501` | Trigger append-only / grant | `IMMUTABLE_RECORD` |
| `P0001` | Trigger run final | `RUN_FINALIZED` |
| Koneksi gagal / `08xxx` | DB tidak terjangkau | `SERVICE_UNAVAILABLE` |

> Error yang **tidak terpetakan** selalu menjadi `500 INTERNAL_ERROR`, dan pemetaan baru harus ditambahkan secara eksplisit. Pesan Postgres mentah tidak pernah dikembalikan ke client.
