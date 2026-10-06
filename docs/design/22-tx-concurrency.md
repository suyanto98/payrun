# Step 7.2 — PayRun: Transaction & Concurrency

> Bagian dari [Step 7](00-index.md). Isolation default, urutan lock global, kebijakan retry, pola chunk (§5), O3, dan O12 di `00-index.md` **mengikat**.
> Rujukan: ADR-011, ADR-006 (fencing `CompleteInTx`), ADR-P02 (`WithTenantTx`), ADR-P05, ADR-P07, ADR-P09 (+ amandemen); Step 3 `04` §7–8, `07`, `08`; Step 4 `06-payrun-schema.md`; Step 5 `04-payrun-api.md`.

---

## 0. Ringkasan keputusan di file ini

| # | Keputusan | Kenapa |
|---|---|---|
| D1 | **Lock "pembuatan periode" lewat baris `tenant_settings`.** Setiap penulis input mengambil `tenant_settings … FOR KEY SHARE` **sebelum** membaca/mengunci periode. Pembuatan baris `payroll_periods` (di `POST /payroll-runs`) mengambil `tenant_settings … FOR UPDATE` | Menutup celah O12 untuk periode yang **belum ada** saat penulis berjalan (§4.d2). `FOR KEY SHARE` tidak saling memblok antar penulis, dan tidak bertabrakan dengan edit settings biasa (`FOR NO KEY UPDATE`) |
| D2 | Urutan lock global PayRun mendapat langkah **1b `tenant_settings`** di antara `idempotency_keys` dan `payroll_periods` | Konsekuensi D1. **Perlu diselaraskan di `00-index.md` §2** |
| D3 | `PeriodGuard.Lock` mengunci periode dalam urutan `(year, month)` ascending, lalu menulis `payroll_input_changes` untuk setiap periode non-final yang tersentuh | Anti-deadlock untuk rentang multi-bulan; O12 |
| D4 | `payroll.calc_chunk` txWrite mengambil `payroll_runs … FOR UPDATE` **lebih dulu**, baru upsert item | Mengikuti urutan global (run sebelum item); menyerialkan chunk-chunk dari run yang sama hanya selama milidetik txWrite |
| D5 | Submit menjalankan pengecekan stale **tanpa** lock periode (pre-check ramah). **Approve dan finalize** adalah pengecekan otoritatif (periode `FOR UPDATE`) | Submit tidak perlu memblok penulis; keputusan final terjadi saat approve/finalize |
| D6 | Pengecekan stale juga mendeteksi **perubahan eligibility**: kandidat tanpa item tetapi sekarang eligible, atau item yang sekarang tidak eligible → `RUN_STALE` | Karyawan baru/resign di tengah periode adalah perubahan input juga (PI-07) |
| D7 | Kolom `payroll_runs.calculation_started_at` (Step 4) diganti nama menjadi **`calc_started_at`** dan diberi semantik O12 | Satu kolom, bukan dua; nama mengikuti `00-index.md` |
| D8 | Nama job: **`payroll.plan`** (ADR-P05, Step 4) = "orchestrate" di Step 2/brief | Nama kanonik yang sudah ada di skema `jobs.dedupe_key` |

---

## 1. Katalog operasi

Notasi lock mengikuti urutan global: `[1]` idempotency_keys · `[1b]` tenant_settings · `[2]` payroll_periods · `[3]` payroll_runs · `[4]` approval_requests · `[5]` leave_balances · `[6]` baris subjek · `[7]` insert/upsert hasil, jobs, audit. **RC** = Read Committed, **RR-RO** = Repeatable Read Read Only. Semua transaksi tenant dibungkus `WithTenantTx` (§2).

| Operasi | Transaction boundary | Isolation | Lock (urutan global) | Race yang dijaga | Retry | Idempotency | Rollback |
|---|---|---|---|---|---|---|---|
| Tenant signup | 1 tx, dua fase berurutan (Step 12: C2): **fase platform** (`app.user_id` saja) → user; lalu `set_config('app.tenant_id', new_id, true)` + `set_config('app.user_id', '', true)` → **fase tenant**: tenant + membership OWNER + `tenant_settings` + seed default | RC | `platform_idempotency_keys` INSERT; unique `uq_tenants_slug`, `uq_users_email` | Double submit; slug sama dari dua orang | 40P01/40001 ≤3× | `platform_idempotency_keys` (wajib) | Semua atau tidak sama sekali; tidak ada tenant setengah jadi |
| Invite | 1 tx: membership `INVITED` + invitation (token hash) + audit | RC | [6] INSERT; `uq_memberships_invited_email` | Undangan ganda ke email sama | ≤3× | Natural (unique email per tenant) | — |
| Accept invitation | 1 tx: guard `invitations.accepted_at IS NULL` + membership `INVITED → ACTIVE` + audit | RC | [6] `UPDATE … WHERE status='INVITED'` | Token dipakai dua kali paralel | ≤3× | Guard single-use | 0 baris → `410 INVITATION_INVALID` |
| Ubah role / revoke | 1 tx: update roles/status + audit; deferred trigger PI-16 menghitung OWNER aktif saat COMMIT | RC | [6] membership target `FOR UPDATE` | Dua OWNER saling mencabut bersamaan (lihat §4 catatan) | ≤3× | Natural (set roles absolut) | Trigger gagal → `409 LAST_OWNER` |
| Switch tenant | Read-only (+ audit INSERT) | RC | — | Membership dicabut bersamaan → token lama hidup ≤ 15 menit (accepted risk, ADR-P02) | — | n.a. | — |
| Employee create | 1 tx: employee + employment_span + PeriodGuard + audit | RC | [1b] → [2] SHARE (bulan `hire_date`) → [6] INSERT → [7] input_changes | NIK/nomor ganda (unique); hire date di periode final | ≤3× | `Idempotency-Key` | `409 NIK_TAKEN` / `EMPLOYEE_NO_TAKEN` / `PERIOD_LOCKED` |
| Employee patch | 1 tx: `UPDATE … WHERE id=$1 AND version=$v` + audit | RC | [6] (row lock implisit oleh UPDATE) | Lost update dua HR | ≤3× | `If-Match` | 0 baris → `412 VERSION_CONFLICT` |
| Terminate | 1 tx: tutup `employment_spans` + status + PeriodGuard + audit | RC | [1b] → [2] SHARE (rentang berubah) → [6] → [7] | Terminate di periode final; terminate + recalculate bersamaan | ≤3× | Natural (status target) | `PERIOD_LOCKED` |
| Salary change | 1 tx: tutup record lama + INSERT baru + PeriodGuard + audit | RC | [1b] → [2] SHARE (rentang yang berubah, D3) → [6] record lama `FOR UPDATE` → INSERT → [7] | Dua perubahan gaji paralel (§4.h) | ≤3× | `Idempotency-Key` + EXCLUDE | `23P01` → `409 OVERLAPPING_PERIOD` |
| Component definition change | 1 tx: `UPDATE … WHERE version=$v` + validasi graf siklus + audit | RC | [6] definisi `FOR UPDATE` (+ `pg_advisory_xact_lock('comp:'||tenant)` saat mengubah dependensi) | Dua perubahan membentuk siklus bersama | ≤3× | `If-Match` | `422 COMPONENT_CYCLE` / `412` |
| Clock in / out | 1 tx: upsert `attendance_records (tenant, employee, work_date)` + PeriodGuard | RC | [1b] → [2] SHARE → [6] upsert → [7] | Double tap clock-in | ≤3× | Natural (unique per hari; clock-in kedua = return existing) | `PERIOD_LOCKED` |
| Attendance correction | 1 tx: update + alasan + PeriodGuard + audit | RC | [1b] → [2] SHARE → [6] `UPDATE … WHERE version` → [7] | Koreksi saat approve (§4.c) | ≤3× | `If-Match` | `412` / `PERIOD_LOCKED` |
| CSV import (job `attendance.import`) | 1 tx per batch ±500 baris: PeriodGuard semua bulan di batch → upsert → error rows → progres | RC | [1b] → [2] SHARE (bulan unik di batch, ascending) → [6] upsert → [7] | Import ulang file sama; batch diulang setelah crash | Job retry (ADR-006) | `uq_attendance_imports_file` + upsert per (employee, work_date) + `CompleteInTx` per batch | Batch gagal → hanya batch itu rollback; baris final dicatat |
| Leave request | 1 tx: request `PENDING` + approval `PENDING` (approver snapshot) | RC | [4] INSERT → [6] INSERT | Request tumpang tindih paralel | ≤3× | `Idempotency-Key` | `23P01` → `409 OVERLAPPING_LEAVE` |
| Leave approve | 1 tx (detail §3.1) | RC | [1b] → [2] SHARE → [4] guard → [5] `FOR UPDATE` → [6] → [7] | Saldo habis oleh dua approve (§4.a); approve vs reject (§4.b) | ≤3× | Guard `WHERE status='PENDING'` | `INSUFFICIENT_LEAVE_BALANCE` → approval tetap `PENDING` |
| Overtime approve | Sama dengan cuti, tanpa [5] | RC | [1b] → [2] SHARE (`work_date`) → [4] → [6] → [7] | Approve vs reject | ≤3× | Guard | — |
| Reject / cancel approval | 1 tx: approval → `REJECTED`/`CANCELLED` + subjek + audit | RC | [4] guard → [6] | Approve vs reject vs cancel | ≤3× | Guard | 0 baris → `409 APPROVAL_ALREADY_DECIDED` |
| Create run | 1 tx: `tenant_settings FOR UPDATE` → `INSERT payroll_periods … ON CONFLICT DO NOTHING` → `SELECT … FOR UPDATE` (Step 12: R1-8, tanpa TOCTOU/`23505`) (+ `cutoff_at` beku) + run `DRAFT` + period → `PROCESSING` | RC | [1] → [1b] **`FOR UPDATE`** (hanya jika periode baru, D1) → [2] INSERT/`FOR UPDATE` → [3] INSERT | Dua admin (§4.e); periode baru vs penulis input (§4.d2) | ≤3× | `Idempotency-Key` + `uq_payroll_runs_active_per_period` | `409 ACTIVE_RUN_EXISTS` (+ `existing_resource_id`) |
| Calculate | 1 tx (detail §3.2) | RC | [2] `FOR UPDATE` → [3] guard → [7] job `payroll.plan` | Penulis in-flight (O12); calculate ganda | ≤3× | Guard status (`DRAFT/CALCULATED/FAILED → CALCULATING`) | `409 INVALID_STATE_TRANSITION` |
| `payroll.plan` (job) | 1 tx: daftar eligible + `eligible_count` + `chunks_total` + INSERT K chunk job + `CompleteInTx` | RC | [3] `FOR UPDATE` (guard `calc_version`) → [7] jobs (dedupe per chunk) | Job terkirim dua kali; versi lama | Job retry | `dedupe_key calc_chunk:run:ver:n` + fencing | Versi tidak cocok → no-op + complete |
| `payroll.calc_chunk` (job) | txRead (RR-RO) → compute → txWrite (RC) (§3.2) | RR-RO / RC | txWrite: [3] `FOR UPDATE` → [7] upsert item/line/snapshot | Chunk dobel; recalculate saat chunk lama jalan (§4.f); zombie vs finalize (§4.g) | Job retry; 40001 tidak mungkin (RO) | `uq_payroll_items_version` + fencing + guard versi | txWrite rollback → job diulang dari txRead |
| Exclude item | 1 tx: item `ERROR → EXCLUDED` + hitung ulang ringkasan run + audit | RC | [3] `FOR UPDATE` (status `CALCULATED`) → [7] item | Exclude saat recalculate | ≤3× | Natural | `409 INVALID_STATE_TRANSITION` |
| Submit | 1 tx: pre-check stale (tanpa lock periode, D5) + tidak ada `ERROR` + hitung admin aktif (pengecualian satu admin) + run → `PENDING_APPROVAL` + approval | RC | [3] guard → [4] INSERT | Submit ganda; self-approval palsu | ≤3× | Guard + `uq_approval_requests_pending` | `RUN_HAS_ERRORS` / `RUN_STALE` / `SELF_APPROVAL_REASON_REQUIRED` |
| Approve `PAYROLL_RUN` | 1 tx (detail §3.3) | RC | [2] `FOR UPDATE` → [3] guard → [4] guard → [7] | Input berubah (§4.c, §4.d); approve vs reject | ≤3× | Guard | `RUN_STALE` → approval tetap `PENDING` |
| Reject `PAYROLL_RUN` | 1 tx: approval → `REJECTED` (note wajib) + run → `CALCULATED` | RC | [3] guard → [4] guard | Approve vs reject | ≤3× | Guard | `DECISION_NOTE_REQUIRED` / `APPROVAL_ALREADY_DECIDED` |
| Finalize | 1 tx (detail §3.4) | RC | [2] `FOR UPDATE` → [3] guard → [6] adjustments `FOR UPDATE` → [7] payslips + jobs | Zombie chunk (§4.g); input berubah setelah approve | ≤3× | Guard status `APPROVED` | `RUN_STALE` / `409` |
| Cancel run | 1 tx: run → `CANCELLED` + periode → `OPEN` jika tidak ada run aktif lain + audit | RC | [2] `FOR UPDATE` → [3] guard | Cancel vs calculate (`CALCULATING` ditolak) | ≤3× | Guard | `409 INVALID_STATE_TRANSITION` |
| Adjustment create | 1 tx: INSERT `PENDING_APPLY` dengan `(target_year, target_month)` (Step 12: R1-4, bukan FK ke periode yang mungkin belum ada) + PeriodGuard target jika periodenya sudah ada + audit | RC | [1b] → [2] SHARE (target, jika ada) → [6] → [7] input_changes (jika periode target ada) | Adjustment masuk ke run yang sudah dikalkulasi → stale | ≤3× | `Idempotency-Key` | — |
| Adjustment cancel | 1 tx: guard `PENDING_APPLY` + PeriodGuard target | RC | [1b] → [2] SHARE → [6] guard → [7] | Cancel vs finalize yang menerapkannya | ≤3× | Guard | `409` |
| Adjustment apply | Di dalam tx finalize: `UPDATE … SET status='APPLIED' WHERE status='PENDING_APPLY' AND id = ANY($ids_in_lines)` | RC | (bagian finalize) | Diterapkan dua kali (PI-15) | — | Guard + `uq_payroll_lines_adjustment_per_run` | Ikut finalize |
| `payslip.render` (job) | Render + upload **di luar** tx → 1 tx: payslip `RENDERED` + checksum + `CompleteInTx` | RC | [6] payslip `UPDATE … WHERE status <> 'RENDERED'` → [7] | Render ganda; crash setelah upload | Job retry | Object key deterministik (menimpa) + guard | Upload ulang menimpa objek yang sama |
| Cleanup (job platform) | Batch kecil (≤1.000 baris) per tx: hapus job `succeeded` > 7 hari, idempotency kedaluwarsa, `payroll_input_changes` periode final > 90 hari | RC | Tanpa lock eksplisit; `DELETE … WHERE ctid IN (SELECT … LIMIT n)` | Bersaing dengan worker | Ulang di tick berikutnya | Natural | — |

> **Catatan ubah role (PI-16):** dua OWNER yang saling mencabut di dua transaksi paralel masing-masing melihat "masih ada satu OWNER lain" di snapshot-nya. Deferred trigger saja tidak cukup di RC karena keduanya bisa commit. Penjaganya: kedua transaksi mengambil `pg_advisory_xact_lock(hashtextextended('owners:'||tenant_id, 0))` sebelum mengubah role `OWNER`, sehingga trigger kedua melihat hasil commit yang pertama. (Pola sama dengan advisory lock siklus manager di Step 4.)

---

## 2. RLS × transaksi

| Aturan | Detail |
|---|---|
| Semua transaksi tenant lewat `WithTenantTx` | Termasuk **txRead** chunk (RR-RO). Tanpa `app.tenant_id`, policy mengembalikan 0 baris (*fail closed*): chunk akan menghitung "tidak ada karyawan", dan guard `count(items) = eligible_count` menangkapnya |
| `set_config('app.tenant_id', $1, true)` | Transaction-local. Hilang otomatis saat `COMMIT`/`ROLLBACK`; koneksi yang kembali ke pool tidak membawa tenant apa pun. **Pool leakage mustahil secara konstruksi**, dan dibuktikan oleh `pool_leakage_after_commit` |
| Retry wrapper | `WithTenantTx` membuka transaksi baru di setiap percobaan, jadi `set_config` dijalankan ulang otomatis. Wrapper **tidak** boleh me-retry di dalam transaksi yang sama (setelah error, transaksi Postgres sudah *aborted*) |
| Urutan di awal tx | `BEGIN [ISOLATION LEVEL …]` → `set_config('app.tenant_id')` → `SET LOCAL lock_timeout/statement_timeout` → query. Untuk RR, `set_config` adalah statement pertama, dan snapshot RR baru diambil pada query **data** pertama (set_config tidak membaca tabel), sehingga tidak ada masalah urutan |
| `WithPlatformTx` | Signup, login lookup, daftar membership sendiri. Menyetel **hanya** `app.user_id`. Signup berpindah ke fase tenant di tx yang sama (set `app.tenant_id`, kosongkan `app.user_id`) |
| **Mode eksklusif (Step 12: C2)** | Satu transaksi tidak pernah memiliki `app.tenant_id` dan `app.user_id` aktif bersamaan. `WithTenantTx` hanya menyetel `app.tenant_id`. Policy self mensyaratkan `current_tenant() IS NULL`, sehingga user multi-tenant tidak melihat barisnya di tenant lain. Test: `TestRLS_SelfPoliciesInactiveInTenantMode` |
| Job worker | Claim job **di luar** konteks tenant (tabel `jobs` tanpa RLS, ADR-006 / Step 4 `01`). Handler membaca `jobs.tenant_id` (kolom, bukan payload) lalu membuka `WithTenantTx(job.tenant_id)` untuk semua kerja bisnis. `CompleteInTx` berjalan di dalam tx tenant yang sama |
| Advisory lock | Kunci advisory selalu diawali tenant (`'owners:'||tenant_id`, `'org:'||tenant_id`, `'comp:'||tenant_id`) sehingga tenant tidak saling memblok |

---

## 3. Operasi kritis secara rinci

### 3.1 Leave approve
```sql
-- WithTenantTx(tenant)
SELECT dates, employee_id, leave_type_id, days FROM leave_requests WHERE id = $req;   -- tanpa lock: hanya untuk tahu bulan mana
-- [1b] [2] PeriodGuard.Lock(tx, employee, dates)
SELECT 1 FROM tenant_settings WHERE tenant_id = $t FOR KEY SHARE;
SELECT id, status FROM payroll_periods
 WHERE period_range && $dates ORDER BY year, month FOR SHARE;            -- FINALIZED → 409 PERIOD_LOCKED
-- [4] keputusan (guard)
UPDATE approval_requests SET status='APPROVED', decided_by=$actor, decided_at=now()
 WHERE id = $appr AND status = 'PENDING' AND (approver_id = $actor OR $is_hr_override)
 RETURNING id;                                                           -- 0 baris → 409 APPROVAL_ALREADY_DECIDED / 404
-- [5] saldo
SELECT balance_days FROM leave_balances
 WHERE employee_id = $e AND leave_type_id = $lt AND year = $y FOR UPDATE;
UPDATE leave_balances SET balance_days = balance_days - $days WHERE …;  -- CHECK ck_leave_balances_min → 23514
-- [6] subjek
UPDATE leave_requests SET status='APPROVED', approved_at=now() WHERE id=$req AND status='PENDING';
-- [7] input change + audit + outbox
INSERT INTO payroll_input_changes (…, changed_at) VALUES (…, clock_timestamp())  -- per periode non-final yang tersentuh
  ON CONFLICT (tenant_id, period_id, employee_id) DO UPDATE SET changed_at = EXCLUDED.changed_at, change_count = payroll_input_changes.change_count + 1;
INSERT INTO audit_log …; INSERT INTO jobs (kind='notify.leave_decided') …;
COMMIT;
```

| Titik gagal | Akibat | Final state |
|---|---|---|
| Sebelum COMMIT (crash, `23514`) | Seluruh tx rollback | Approval tetap `PENDING`, saldo utuh. `23514` → `422 INSUFFICIENT_LEAVE_BALANCE` |
| `55P03` saat menunggu periode (approve run memegang `FOR UPDATE`) | `409 RESOURCE_BUSY` | Tidak ada perubahan; manager klik ulang |
| COMMIT, response hilang | Klik ulang → guard `PENDING` gagal | `409 APPROVAL_ALREADY_DECIDED` (+ `status: APPROVED`); UI menampilkan "sudah disetujui" |

### 3.2 Calculate + chunk
```sql
-- (a) POST /payroll-runs/{id}/calculate   — WithTenantTx
SELECT id FROM payroll_periods WHERE id = $period FOR UPDATE;          -- [2] menunggu semua penulis FOR SHARE selesai
UPDATE payroll_runs
   SET status='CALCULATING', calc_version = calc_version + 1,
       chunks_total = 0, chunks_done = 0, calc_started_at = clock_timestamp()
 WHERE id = $run AND status IN ('DRAFT','CALCULATED','FAILED')
 RETURNING calc_version;                                               -- [3]; 0 baris → 409
INSERT INTO jobs (kind='payroll.plan', tenant_id, payload={run, calc_version}, dedupe_key='plan:'||run||':'||ver);
COMMIT;                                                                -- lock periode dilepas; penulis baru akan punya changed_at > calc_started_at

-- (b) payroll.plan   — WithTenantTx(job.tenant_id)
SELECT calc_version, status FROM payroll_runs WHERE id = $run FOR UPDATE;   -- versi ≠ job → CompleteInTx + selesai (no-op)
-- daftar eligible (employment_spans && period), ORDER BY employee_id, pecah per chunk_size
UPDATE payroll_runs SET eligible_count = $n, chunks_total = $k WHERE id = $run;
-- Step 12 (R1-3): k = 0 → di tx yang sama: total 0, fingerprint run kosong, status 'CALCULATED',
--   warning NO_ELIGIBLE_EMPLOYEES, lalu CompleteInTx + COMMIT (tanpa chunk). Test: TestPayrollRun_ZeroEligibleCompletes
INSERT INTO jobs (kind='payroll.calc_chunk', …, dedupe_key='calc_chunk:'||run||':'||ver||':'||i) … ON CONFLICT DO NOTHING;  -- K baris
jobs.CompleteInTx(job);  COMMIT;

-- (c) payroll.calc_chunk
txRead:  BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY; set_config(tenant);
         InputProvider.* untuk employee_ids di chunk (7 query batch, tanpa N+1); COMMIT;
compute: engine.Calculate + fingerprint per employee (CPU, di luar tx)
txWrite: BEGIN; set_config(tenant);
         SELECT status, calc_version FROM payroll_runs WHERE id=$run FOR UPDATE;   -- [3]
           -- status ≠ CALCULATING atau versi ≠ job → jobs.CompleteInTx + COMMIT (no-op)
         INSERT payroll_items … ON CONFLICT (tenant_id, run_id, employee_id, calc_version) DO UPDATE …;
         DELETE/INSERT payroll_lines, payroll_item_snapshots untuk item tersebut;     -- [7]
         UPDATE payroll_runs SET chunks_done = chunks_done + 1 WHERE id=$run RETURNING chunks_done, chunks_total;
         jobs.CompleteInTx(job);                                                   -- fencing: locked_by = me
         -- chunk terakhir: SUM item OK → total, fingerprint run, cek count = eligible_count,
         --                status → CALCULATED (trigger payroll_runs_totals_check memverifikasi)
         COMMIT;
```

| Titik gagal | Akibat | Final state |
|---|---|---|
| Crash di txRead / compute | Tidak ada yang ditulis; lease habis → job diambil ulang | Chunk dihitung ulang dari awal |
| Crash di txWrite sebelum COMMIT | Rollback; lease habis → ulang | Sama |
| COMMIT sukses, worker mati sebelum ack | Job sudah `succeeded` (di tx yang sama) | Tidak diulang |
| Worker lambat (zombie), lease diambil worker lain | Worker kedua commit lebih dulu; zombie gagal di fencing `CompleteInTx` → seluruh txWrite zombie rollback | `chunks_done` naik tepat sekali |
| Input berubah di antara txRead dan txWrite | Item ditulis dengan input lama; perubahan tercatat di `payroll_input_changes` dengan `changed_at > calc_started_at` | Approve menangkapnya sebagai kandidat (O12) |
| Chunk gagal permanen | `dead` → handler `on_dead` menandai run `FAILED` + daftar employee bermasalah | HR memperbaiki data, lalu recalculate (`calc_version++`) |

### 3.3 Approve `PAYROLL_RUN` (O3 + O12 + D6)
```sql
-- WithTenantTx; dispatch dari POST /approvals/{id}/approve untuk subject_type = PAYROLL_RUN
SELECT id FROM payroll_periods WHERE id = $period FOR UPDATE;            -- [2] dunia input berhenti sejenak
SELECT status, calc_version, calc_started_at FROM payroll_runs WHERE id = $run FOR UPDATE;  -- [3] harus PENDING_APPROVAL
-- (re)cek membership & role approver di DB (ADR-009); approver ≠ pembuat kecuali self_approved
-- kandidat O12
SELECT employee_id FROM payroll_input_changes
 WHERE tenant_id = $t AND period_id = $period AND changed_at > $calc_started_at;
-- Step 12 (R1-7): jika kandidat > 200 → ROLLBACK → 409 RUN_STALE (recalculate lebih murah daripada
--   memverifikasi ribuan fingerprint sambil memegang lock periode)
-- untuk setiap kandidat (biasanya 0–puluhan):
--   cur_fp  = Fingerprint(InputProvider(kandidat))      -- fungsi yang sama dengan kalkulasi
--   item    = payroll_items @ calc_version (boleh tidak ada)
--   eligible_now = employment_spans && period
--   stale jika: (item ada AND eligible_now AND cur_fp ≠ item.input_fingerprint)
--            OR (item ada AND NOT eligible_now) OR (item tidak ada AND eligible_now)    -- D6
-- stale tidak kosong → ROLLBACK → 409 RUN_STALE {employees_changed: [...]}
UPDATE approval_requests SET status='APPROVED', … WHERE id=$appr AND status='PENDING';   -- [4]
UPDATE payroll_runs SET status='APPROVED', approved_by=$actor, approved_at=now()
 WHERE id=$run AND status='PENDING_APPROVAL';
INSERT audit_log …; COMMIT;
```

**Kenapa approve tetap O(perubahan):** kandidat hanya employee yang input-nya disentuh sejak `calc_started_at`. Run 10.000 karyawan dengan 3 koreksi absensi hanya menghitung ulang 3 fingerprint. Durasi lock `FOR UPDATE` pada periode ±puluhan milidetik; penulis input yang datang saat itu menunggu (≤ `lock_timeout` 2 dtk) lalu lanjut.

| Titik gagal | Final state |
|---|---|
| Stale | Approval dan run tidak berubah (`PENDING` / `PENDING_APPROVAL`); response menampilkan employee yang berubah; HR reject → recalculate → submit ulang |
| Crash sebelum COMMIT | Tidak ada perubahan; klik ulang |
| `55P03` (penulis input panjang memegang `FOR SHARE`) | `409 RESOURCE_BUSY`; coba lagi |

### 3.4 Finalize
```sql
SELECT … FROM payroll_periods WHERE id = $period FOR UPDATE;             -- [2]
SELECT … FROM payroll_runs WHERE id = $run FOR UPDATE;                  -- [3] harus APPROVED
-- cek stale ulang (sama dengan 3.3; perubahan bisa terjadi antara approve dan finalize)
SELECT id FROM adjustments WHERE id = ANY($adj_ids_in_lines) AND status = 'PENDING_APPLY' FOR UPDATE;   -- [6]
UPDATE adjustments SET status='APPLIED', applied_run_id=$run, applied_at=now()
 WHERE id = ANY($ids) AND status='PENDING_APPLY';                        -- count harus = len($ids), jika tidak → rollback (PI-15)
-- Step 12 (R3-2): TIDAK ada DELETE versi lama di sini (memegang lock periode). Versi lama dibersihkan
--   job payroll.cleanup_versions (batch 1.000) setelah COMMIT; semua query membaca calc_version terkini.
UPDATE payroll_runs SET status='FINALIZED', finalized_by=$actor, finalized_at=now()
 WHERE id=$run AND status='APPROVED';                                    -- trigger PI-05 memverifikasi total
UPDATE payroll_periods SET status='FINALIZED', finalized_run_id=$run, finalized_at=now() WHERE id=$period;
INSERT INTO payslips (… status='PENDING', object_key deterministik) SELECT … FROM payroll_items WHERE status='OK' …;   -- [7]
INSERT INTO jobs (kind='payslip.render', …) …;                           -- per chunk payslip
INSERT INTO jobs (kind='payroll.cleanup_versions', payload={run, keep_version=$v}) …;   -- Step 12 (R3-2)
INSERT audit_log …; COMMIT;
```
Setelah COMMIT, trigger PI-04 (item/line/snapshot) dan PI-14 (`guard_period_*`) aktif untuk periode dan run ini. Adjustment yang dibatalkan bersamaan oleh HR: transaksi cancel menunggu lock `[2] FOR SHARE` (periode target dikunci `FOR UPDATE` oleh finalize). Setelah finalize commit, cancel melihat periode target sudah `FINALIZED` → `PERIOD_LOCKED`; atau melihat status `APPLIED` → `409`.

### 3.5 Salary change
```sql
SELECT 1 FROM tenant_settings WHERE tenant_id=$t FOR KEY SHARE;            -- [1b]
-- changed range = [effective_from, ∞) (kasus tutup + buka record)
SELECT id, status FROM payroll_periods WHERE period_range && $changed ORDER BY year, month FOR SHARE;   -- [2]; FINALIZED → 409
SELECT id, valid_during FROM salary_assignments
 WHERE employee_id=$e AND valid_during @> $effective_from FOR UPDATE;     -- [6] record yang berlaku
UPDATE salary_assignments SET valid_during = daterange(lower(valid_during), $effective_from)
 WHERE id = $old_id AND valid_during = $expected_range;                    -- 0 baris → 409 (sudah diubah orang lain)
INSERT INTO salary_assignments (…, valid_during) VALUES (…, daterange($effective_from, NULL));   -- EXCLUDE menjaga overlap
INSERT/UPSERT payroll_input_changes (periode non-final yang beririsan $changed);                    -- [7]
INSERT audit_log …; COMMIT;
```
Trigger `guard_period_range` (Step 4 §6.5) adalah jaring pengaman. `PeriodGuard` di aplikasi memberi `409 PERIOD_LOCKED` yang ramah.

---

## 4. Race condition konkret (naif vs dijaga)

### (a) Dua approve cuti menghabiskan saldo
Saldo cuti tahunan Tono = 2 hari. Request R1 (2 hari) dan R2 (1 hari), di-approve oleh Arif dan HR (override) bersamaan.

| t | T1 (approve R1) | T2 (approve R2) | Naif (read-check-write tanpa lock) |
|---|---|---|---|
| 1 | `SELECT balance` = 2 | `SELECT balance` = 2 | keduanya lihat 2 |
| 2 | 2 ≥ 2 ✓ | 2 ≥ 1 ✓ | keduanya lolos |
| 3 | `UPDATE … = 0` | `UPDATE … = 1` | **lost update**: saldo akhir 1 atau 0, padahal 3 hari disetujui |

**Dijaga:** `SELECT … FOR UPDATE` pada saldo → T2 menunggu T1 commit → T2 membaca 0 → `0 − 1 < min_allowed_days` → CHECK `ck_leave_balances_min` gagal → rollback → `422 INSUFFICIENT_LEAVE_BALANCE`, approval R2 tetap `PENDING`. Walaupun lock terlupa, `UPDATE … SET balance_days = balance_days − $days` atomik ditambah CHECK tetap menolak saldo negatif (PI-10).

### (b) Approve vs reject pada approval yang sama
| t | T1 (Arif approve) | T2 (HR reject) |
|---|---|---|
| 1 | `UPDATE approval_requests … WHERE status='PENDING'` → lock baris | |
| 2 | | `UPDATE … WHERE status='PENDING'` → menunggu |
| 3 | COMMIT | baca ulang baris (RC *EvalPlanQual*): status `APPROVED` → 0 baris |
| 4 | `200` | `409 APPROVAL_ALREADY_DECIDED` |

Tanpa guard status, T2 akan menimpa keputusan T1. Hasilnya `REJECTED` padahal saldo sudah dikurangi.

### (c) Koreksi absensi saat approve run (O3)
| t | T1 (approve run Maret) | T2 (HR koreksi absensi 10 Maret) |
|---|---|---|
| 1 | `payroll_periods(Maret) FOR UPDATE` | |
| 2 | kandidat = ∅ → fingerprint OK | `payroll_periods(Maret) FOR SHARE` → **menunggu** |
| 3 | run → `APPROVED`, COMMIT | lock didapat; tulis koreksi + `changed_at` |
| 4 | | COMMIT |

Run disetujui dengan input yang memang sama dengan yang dihitung. Koreksi T2 terjadi **setelah** approve dan tercatat di change log. Finalize (§3.4) menjalankan cek stale ulang → `RUN_STALE` → HR memilih: reject + recalculate, atau batalkan koreksi.
**Naif (tanpa lock periode):** T1 membaca fingerprint sebelum T2 commit, T2 commit, T1 commit. Hasilnya run `APPROVED` dengan input yang sudah tidak cocok, tanpa jejak (PI-13 dilanggar).

### (d) Penulis mulai sebelum calculate, commit setelah chunk membaca (O12)
**d1, periode sudah ada:**

| t | W (koreksi absensi Tono) | C (calculate) | K (chunk berisi Tono) |
|---|---|---|---|
| 1 | BEGIN; `periods(Maret) FOR SHARE` ✓ | | |
| 2 | | `periods(Maret) FOR UPDATE` → **menunggu W** | |
| 3 | update absensi; `changed_at = clock_timestamp()` (< nanti `calc_started_at`) ; COMMIT | | |
| 4 | | lock didapat; `calc_started_at = t4`; COMMIT | |
| 5 | | | txRead: **melihat** koreksi W (commit di t3) |

Perubahan W terlihat oleh kalkulasi, sehingga tidak perlu menjadi kandidat. Ini aman.
Kebalikannya: W mendapat `FOR SHARE` **setelah** C commit (t4) → `changed_at` diambil setelah lock → `changed_at > calc_started_at` → masuk kandidat. Kalau K membaca sebelum W commit, fingerprint kandidat berbeda → `RUN_STALE`. Kalau K membaca sesudahnya, fingerprint sama → bukan stale (false positive tersaring).

**d2, periode belum ada (alasan D1):**

| t | W (koreksi absensi April) | A (create run April) |
|---|---|---|
| 1 | `tenant_settings FOR KEY SHARE` ✓; `periods(April)` → tidak ada baris, tidak ada yang dikunci | |
| 2 | | `tenant_settings FOR UPDATE` → **menunggu W** |
| 3 | tulis absensi (tanpa change row: belum ada periode); COMMIT | |
| 4 | | lock didapat; INSERT `periods(April)`; run `DRAFT`; COMMIT |
| 5 | | calculate → chunk membaca → **melihat** absensi W |

**Tanpa D1:** di t2 A tidak menunggu, membuat periode, lalu calculate commit dan chunk membaca sebelum W commit. Perubahan W tidak terlihat dan juga tidak punya baris change log. Hasilnya run yang basi lolos approve. Inilah lubang yang ditutup oleh lock `tenant_settings`.
Alternatif yang dipertimbangkan: membuat baris periode 13 bulan ke depan saat signup + job bulanan. Ditolak karena job pembuat periode tetap butuh lock yang sama, dan rentang terbuka `[Jun, ∞)` tetap tidak tercakup.

### (e) Dua admin membuat run untuk periode yang sama
| t | T1 (Hendra, key K1) | T2 (Dewi, key K2) |
|---|---|---|
| 1 | INSERT run (periode Maret) | INSERT run (periode Maret) → menunggu unique index |
| 2 | COMMIT → `201` | `23505 uq_payroll_runs_active_per_period` → `409 ACTIVE_RUN_EXISTS {existing_resource_id}` |

**Hendra klik dua kali (key K1 sama):** request kedua menunggu di unique index `idempotency_keys`, lalu me-replay response pertama → `201` + `Idempotent-Replayed: true`. Tidak ada `409`, karena memang niat yang sama (Step 5).

### (f) Recalculate saat chunk versi lama masih berjalan
| t | Chunk K3 (versi 1) | Admin |
|---|---|---|
| 1 | txRead selesai, compute | |
| 2 | | run `CALCULATED`? Belum, masih `CALCULATING` → recalculate **ditolak** `409` (dari `CALCULATING` tidak boleh) |

Recalculate hanya diizinkan dari `CALCULATED`/`FAILED`, sehingga versi lama sudah selesai semua. Satu-satunya jalan chunk versi lama masih hidup adalah job zombie/duplikat (retry setelah `FAILED` → recalculate). Penjaganya: txWrite `SELECT … FOR UPDATE` melihat `calc_version = 2 ≠ 1` → no-op + `CompleteInTx`. Seandainya guard versi terlewat, upsert menulis ke `calc_version = 1` (unique key berbeda), yang dibuang saat finalize dan tidak dihitung dalam total versi 2.

### (g) Chunk zombie vs finalize (PI-04)
Lihat Step 4 §7.3. Trigger `forbid_change_on_finalized_run` mengambil `payroll_runs … FOR SHARE`. Finalize memegang `FOR UPDATE` pada run, sehingga INSERT item zombie menunggu, lalu melihat `FINALIZED` → `P0001` → `409 RUN_FINALIZED` (di sisi job: rollback, dicatat, `CompleteInTx` gagal fencing). Dengan D4, txWrite chunk sudah mengambil `FOR UPDATE` pada run di awal, jadi chunk dan finalize terserialisasi bahkan sebelum trigger berjalan.

### (h) Dua perubahan gaji paralel untuk karyawan yang sama
HR-1 menaikkan gaji mulai 1 April, HR-2 mulai 15 April.

| t | T1 | T2 |
|---|---|---|
| 1 | record `[Jan, ∞)` `FOR UPDATE` | record `[Jan, ∞)` `FOR UPDATE` → menunggu |
| 2 | tutup → `[Jan, 1 Apr)`; INSERT `[1 Apr, ∞)`; COMMIT | |
| 3 | | lock didapat; re-evaluasi `WHERE valid_during @> 15 Apr`: baris lama sudah `[Jan, 1 Apr)` → tidak cocok lagi → 0 baris → `409` "record berubah, muat ulang" |

Kalau T2 memakai jalur lain (misalnya langsung INSERT `[15 Apr, ∞)`), constraint `EXCLUDE … valid_during WITH &&` menolak dengan `23P01` → `409 OVERLAPPING_PERIOD`. Untuk PATCH resource bervesi (employee, komponen), `If-Match` → `412 VERSION_CONFLICT`.

### (i) Penugasan atasan membentuk siklus secara paralel
T1: "Arif melapor ke Budi". T2: "Budi melapor ke Arif". Masing-masing aman jika dilihat sendiri-sendiri (cek siklus di snapshot sendiri tidak menemukan siklus).
**Dijaga:** trigger PI-19 mengambil `pg_advisory_xact_lock(hashtextextended('org:'||tenant_id, 0))` sebelum cek siklus (recursive CTE). T2 menunggu T1 commit, lalu cek siklusnya melihat penugasan T1 → `422 MANAGER_CYCLE`. Lock per tenant, sehingga tenant lain tidak terpengaruh. Kontensinya rendah karena penugasan atasan jarang.

---

## 5. Analisis deadlock

| Pasangan transaksi | Lock T1 | Lock T2 | Deadlock? | Alasan |
|---|---|---|---|---|
| Approve run vs koreksi absensi | [2] UPDATE → [3] → [4] | [1b] → [2] SHARE → [6] | Tidak | Keduanya mulai dari [2]; satu menunggu yang lain |
| Leave approve vs approve run | [1b] → [2] SHARE → [4] → [5] | [2] UPDATE → [3] → [4] | Tidak | Konflik pertama di [2]; tidak ada yang memegang [4] sambil menunggu [2] |
| Calc chunk vs finalize | [3] UPDATE → [7] | [2] → [3] UPDATE → [6] → [7] | Tidak | Chunk tidak mengambil [2]; konflik pertama di [3] |
| Create run (periode baru) vs penulis input | [1] → [1b] UPDATE → [2] | [1b] KEY SHARE → [2] SHARE | Tidak | Konflik pertama di [1b] |
| PeriodGuard rentang multi-bulan vs rentang lain | periode Mar, Apr (asc) | periode Apr, Mei (asc) | Tidak | Keduanya ascending → tidak ada siklus (D3) |
| Dua leave approve (employee berbeda, saldo berbeda) | [5] baris A | [5] baris B | Tidak | Baris berbeda |
| Penulis input vs pembuatan periode via settings edit | KEY SHARE | NO KEY UPDATE (edit settings) | Tidak konflik | Mode lock kompatibel |

**Titik rawan yang dicek di code review:**
1. **Dispatch approval engine.** Engine generik membuka `[4]` lebih dulu untuk membaca `subject_type`. Kalau handler subjek kemudian mengambil `[2]`, urutannya terbalik. **Aturan:** endpoint approve membaca approval **tanpa lock** untuk mengetahui `subject_type` dan subjek, lalu handler mengambil lock dalam urutan global ([1b]/[2] → [3] → [4] guard UPDATE → [5] → [6]).
2. **FK dan `FOR KEY SHARE` implisit.** INSERT ke `payroll_items` mengambil `KEY SHARE` pada `payroll_runs` dan `employees` (pemeriksaan FK). `KEY SHARE` tidak bertabrakan dengan `UPDATE` non-key pada run (`chunks_done`, status), jadi tidak ada deadlock antar chunk. Bertabrakan hanya dengan `DELETE`/update PK, yang tidak dilakukan. INSERT `payroll_input_changes` mengambil `KEY SHARE` pada periode, yang kompatibel dengan `FOR SHARE` milik penulis sendiri tetapi **menunggu** `FOR UPDATE` approve. Ini sengaja.
3. **Deadlock yang tetap terjadi** (bug urutan) dideteksi Postgres (`40P01`, `deadlock_timeout` 1 dtk). Wrapper me-retry ≤3×, dan metrik `db_deadlock_total` naik. Lonjakan metrik ini berarti ada pelanggaran urutan yang harus diperbaiki, bukan di-retry lebih banyak.

---

## 6. Pekerjaan panjang

| Topik | Keputusan |
|---|---|
| Kenapa chunk | Satu transaksi untuk 10.000 karyawan berarti lock panjang, satu error membatalkan semua, tidak ada progres, retry mahal, dan memori besar. Chunk memberi progres, isolasi kegagalan, dan resume (ADR-P05) |
| Ukuran chunk | Default **50** (`tenant_settings`/config). **Demo memakai 10** agar "crash di karyawan ke-37" jatuh di chunk 4 (O5). Tuning: target txWrite < 200 ms dan txRead < 500 ms; ukur di Step 9. Chunk terlalu kecil → overhead job dan kontensi baris run (`chunks_done`); terlalu besar → retry mahal |
| Kontensi `chunks_done` | Setiap txWrite meng-update baris run yang sama. Dengan 8 worker paralel dan txWrite ±50 ms, antrean di baris run kecil. Alternatif V2: tabel `payroll_run_chunks (run, ver, n, done_at)` dan run selesai saat `count = chunks_total` (tanpa baris panas) |
| Batas konkurensi | V1: worker pool global (misalnya 8 goroutine `payroll.calc_chunk`). P2: maksimal N chunk berjalan per tenant (fairness, Step 7 `03`) |
| Timeout worker | `statement_timeout`: txRead 30 dtk, txWrite 10 dtk, `payroll.plan` 30 dtk, `attendance.import` per batch 30 dtk, `payslip.render` tx metadata 5 dtk (render di luar tx, timeout render 60 dtk). `lock_timeout` worker 5 dtk. `idle_in_transaction_session_timeout` 30 dtk |
| Lease | `locked_until = now() + 2 × perkiraan durasi` (calc_chunk 60 dtk) + heartbeat untuk job yang lebih panjang (ADR-006) |

---

## 7. Test yang membuktikan setiap jaminan

| Test | Teknik | Jaminan |
|---|---|---|
| `leave_parallel_approve_respects_balance` | 2–20 goroutine approve request berbeda pada saldo yang sama (Testcontainers Postgres) | §4.a, PI-10 |
| `approval_approve_vs_reject_single_winner` | Dua goroutine dengan barrier (`sync.WaitGroup` + channel start) | §4.b, PI-11 |
| `approve_blocks_concurrent_input_write` | T1 approve ditahan di antara lock periode dan commit (hook test), T2 koreksi absensi harus menunggu | §4.c, O3 |
| `stale_detected_for_writer_committed_after_chunk_read` | Hook: tahan W setelah lock, jalankan calculate + chunk, lalu commit W; approve → `RUN_STALE` | §4.d1, O12 |
| `stale_detected_when_period_created_concurrently` | Hook: W memegang `tenant_settings KEY SHARE`; create run harus menunggu | §4.d2, D1 |
| `stale_false_positive_filtered` | Ubah lalu kembalikan nilai yang sama → kandidat ada, fingerprint sama → approve sukses | O12 |
| `stale_detects_new_hire_and_termination` | Tambah employee eligible / terminate setelah kalkulasi | D6 |
| `run_create_two_admins_one_wins` / `run_create_double_click_replays` | Paralel, key berbeda / sama | §4.e |
| `calc_chunk_old_version_noop` | Kirim job versi lama setelah recalculate | §4.f |
| `calc_chunk_zombie_fenced` | Dua worker memegang job yang sama (lease kedaluwarsa paksa) | ADR-006 fencing |
| `zombie_chunk_vs_finalize` | Insert item ditahan sebelum commit, finalize jalan | §4.g, PI-04 |
| `salary_change_parallel_one_wins` | Dua perubahan gaji paralel | §4.h, PI-09 |
| `manager_cycle_parallel_rejected` | Dua penugasan saling silang paralel | §4.i, PI-19 |
| `owner_parallel_revoke_keeps_one` | Dua OWNER saling cabut paralel | §1 catatan, PI-16 |
| `crash_resume_identical_result` | Kill worker di chunk ke-4 (failure injection), bandingkan dengan run tanpa crash | PFS-01 |
| `with_tenant_tx_retry_resets_tenant` | Paksa `40001` pada percobaan pertama; percobaan kedua tetap melihat tenant yang benar | §2 |
| `pool_leakage_after_commit` | Tx tenant A lalu query tanpa tenant di koneksi yang sama → 0 baris | §2, PI-01 |
| `lock_order_lint` | Analisis statis sederhana: daftar fungsi service + urutan tabel yang di-lock (anotasi), dicek di code review | §5 |
