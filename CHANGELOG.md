# Changelog

## Skeleton

Skeleton (`internal/platform`, `cmd`, `deploy`, CI) disalin dari LedgerPay, bukan dibagi sebagai module
(docs/design/32-repository-structure.md). Setiap perubahan skeleton dicatat di sini dan harus di-port ke repo lain.

### 0.0.12 — PayRun Sprint 10 (belum di-port ke LedgerPay)
- **Layout repo (2026-10-10):** Go module pindah ke `backend/` (cmd, internal, db, api, go.mod, sqlc.yaml, .golangci.yml),
  SPA ke `frontend/`; Makefile, Dockerfile (`COPY backend/`), compose, workflow (`working-directory: backend`) menyesuaikan.
  Meta test membedakan `moduleRoot` (backend/) dan `repoRoot`. Saat port ke LedgerPay: path sumber di sini = `backend/<path LedgerPay>`.
- `meta`: `TestTraceability_DocsReferenceExistingTests` versi PayRun (README, docs/*.md, ADR-P*, laporan perf, katalog
  §4.4–4.6, §5.2, §5.3), `TestObservability_AlertsLinkToRunbook` (annotation runbook → heading yang ada di
  `docs/runbook.md`), `TestOpenAPI_MoneyFieldsAreStrings`, `TestSPA_ErrorMessagesCoverCatalogue`. Cek link runbook
  sudah di-port ke LedgerPay 2026-10-10 (annotation LedgerPay memang sudah menunjuk `docs/runbook.md`; yang belum ada
  hanya test penjaganya).
- `metrics`: `payrun_payroll_total_mismatch_runs` (gauge) diisi; `ReproducibilityFailures` diinisialisasi 0.
- `db`: `TestWithTenantTx_RetryKeepsTenant` (tenant disetel ulang di setiap percobaan retry).
- Test env: invariant checker di `t.Cleanup` (pola LedgerPay, invariant PayRun).
- `deploy/compose.demo.yml` + `make up-demo` / `seed-demo` / `demo-live` / `demo-down` (stack demo lokal terpisah);
  `make api-docs`.

### 0.0.11 — PayRun Sprint 9 (perbaikan `jobs.Runner.Run` + test-nya sudah di-port ke LedgerPay 2026-10-10; item lain belum)
- **`jobs.Runner.Run`: tidak lagi tidur selama interval polling saat semua slot penuh.** Goroutine yang selesai
  memberi sinyal slot kosong; interval hanya saat antrean kosong. Terukur di PR-1: run 10.000 karyawan 26 → 10 dtk
  (loop lama membatasi ±concurrency job per interval berapa pun cepatnya job). Test
  `TestJobRunner_FullCapacity_ClaimsAsSoonAsSlotFrees` (gagal di kode lama: 10 dtk vs 0,3 dtk).
- `config`: `PAYROLL_WORKER_CONCURRENCY` (1–32, default 4).
- `db.CustomPlan(tx)` (baru): wrapper DBTX yang mengirim query sebagai unnamed statement
  (`pgx.QueryExecModeCacheDescribe`) → selalu custom plan. Untuk query dengan filter opsional
  `($n IS NULL OR …)` saja; generic plan salah memilih index (PR-3: daftar karyawan 8 → 1,5 ms). Test
  `TestCustomPlan_NeverLeavesNamedPreparedStatement`. LedgerPay: cek query filter opsional (riwayat wallet, ops console).
- Catatan Postgres: `COPY FROM` ditolak untuk tabel ber-RLS bila role tidak BYPASSRLS → batch insert memakai
  `INSERT … SELECT unnest(...)`.

### 0.0.10 — PayRun Sprint 8 (belum di-port ke LedgerPay)
- `faults`: signature `Check(ctx, q, point, tenantID)`; tabel demo PayRun berisi `after_n`/`times`/`fired`/`action`/`expires_at` (27 §9.2), aksi `exit`/`panic`/`delay`.
- `db/migrations_demo` + `migrate up` menjalankan migrasi demo (no-op di build release).
- `config`: `JOB_LEASE_TTL` (default 30s, minimal 5s) → `WorkerConfig.Lease`.
- `dbtest.TestDB.OwnerDSN()`.
- `deploy`: image backup + `restore.sh`, `scripts/restore-drill.sh`, `deploy/prod` (edge bersama), workflow `main`/`rollback`/`nightly`.

### 0.0.9 — PayRun Sprint 7 (belum di-port ke LedgerPay)
- `storage` (baru): `Store` (PUT + presigned GET), implementasi `S3` (minio-go, SigV4, endpoint publik terpisah untuk presign) dan `Memory` (test); `PresignTTL` 5 menit.
- `apperr`: `PAYSLIP_NOT_READY`.
- `db.MapError`: `P0001`/`P0002` juga menaikkan `payrun_finalized_mutation_attempts_total` / `payrun_period_lock_bypass_total` (label tabel).
- `metrics`: bucket `runBuckets`, `SizeBucket`, inisialisasi 0 untuk label INV dan outcome; metrik domain PayRun S7.
- `app.Worker`: lane opsional `documents` (aktif bila `Deps.Storage` diisi); `config`: `S3_PUBLIC_ENDPOINT`.
- `deploy`: Prometheus + Grafana (profile `obs`), Jaeger (`obs-full`), rules + promtool test, generator dashboard; `make up-obs`, `test-alerts`, `dashboards`.

### 0.0.8 — PayRun Sprint 6 (belum di-port ke LedgerPay)
- `apperr`: `OVERLAPPING_LEAVE`, `INSUFFICIENT_LEAVE_BALANCE` (+ extension `Balance`), `EMPLOYEE_NOT_ACTIVE`, `CLOCK_OUT_WITHOUT_CLOCK_IN`, `NO_APPROVER_AVAILABLE`; `httpx.Problem` menulis `balance`.
- `db.MapError`: `ex_leave_requests_no_overlap` → `OVERLAPPING_LEAVE`, `ex_overtime_requests_no_overlap` → `OVERLAPPING_PERIOD`, `ck_leave_balances_min` → `INSUFFICIENT_LEAVE_BALANCE`.

### 0.0.7 — PayRun Sprint 5 (belum di-port ke LedgerPay)
- `apperr`: extension per code (`EmployeesChanged`, `ChangedCount`, `ApprovalStatus`, `ErrorCount`) + kode `RUN_STALE`, `APPROVAL_ALREADY_DECIDED`, `RUN_HAS_ERRORS`, `DECISION_NOTE_REQUIRED`, `SELF_APPROVAL_REASON_REQUIRED`, `SELF_APPROVAL_FORBIDDEN`; `httpx.Problem` menulisnya (`approval_status`, bukan `status`).
- `db.MapError`: `P0002` (trigger PeriodGuard) → `PERIOD_LOCKED`.
- Khusus PayRun: paket `periodguard` (kontrak PeriodGuard; implementasi di modul payroll).

### 0.0.6 — PayRun Sprint 4 (belum di-port ke LedgerPay)
- `idempotency.Hash`: body kosong di-hash sama dengan `{}` (endpoint tanpa body seperti calculate/finalize tidak lagi `MALFORMED_JSON`).
- `jobs` (baru di PayRun, dari LedgerPay): ditambah `tenant_id`; enqueue di tx tenant, claim tanpa tenant context, `CompleteInTx` di tx tenant handler.
- `app.Worker` + `WorkerOpsRouter`: lane `payroll` dan `maintenance`, readiness per lane, kill switch per kind.
- `config`: `ValidateWorker`, `WORKER_ID`, `WORKER_POLL_INTERVAL`, `PAYROLL_CHUNK_SIZE`.
- `apperr`: `ACTIVE_RUN_EXISTS`, `PERIOD_LOCKED`, `RUN_FINALIZED`, `LAST_OWNER`; `db.MapError`: `P0001` → `RUN_FINALIZED`, `P0004` → `LAST_OWNER`.
- `ratelimit`: tier `PIIView` (`pii-view`, 30/jam), ikut dilonggarkan `Unlimited()`.
- Khusus PayRun: `db.WithTenantTxForOptions` (txRead chunk REPEATABLE READ READ ONLY).

### 0.0.5 — PayRun Sprint 3 (belum di-port ke LedgerPay)
- `apperr`: `OVERLAPPING_PERIOD`, `MANAGER_CYCLE`, `COMPONENT_CYCLE`, `INVALID_COMPONENT_REFERENCE`.
- `db.MapError`: `ex_*_no_overlap` (23P01) → `OVERLAPPING_PERIOD`; `P0005` → `MANAGER_CYCLE`.

### 0.0.4 — PayRun Sprint 2 (belum di-port ke LedgerPay)
- `idempotency` (baru di PayRun, dari LedgerPay pola a): tenant-scoped (`tenant_id` + user sebagai client), key kedaluwarsa dianggap baru, `Outcome.Header` untuk ETag/Location.
- `httpx`: `IfMatchVersion` (428/412), `SetETag`, `ParseID` (bukan UUID → 404), `KeyCursor` (keyset teks), `Limit`, `OnlyQuery` (filter tak dikenal → 400).
- `crypto`: `BlindIndex` (HMAC-SHA256, kunci terpisah) dan `Keyring.ActiveVersion`.
- `audit` (baru): `audit.Record` di transaksi yang sama dengan perubahan.
- `auth`: `HasAnyRole`, `RequireAnyRole`, konstanta role.
- `apperr`: `VERSION_CONFLICT` (412), `PRECONDITION_REQUIRED` (428).
- Khusus PayRun: `db.GuardTenant` (C-46), metrik `payrun_tenant_isolation_violations_total`.

### 0.0.3 — PayRun Sprint 1 (belum di-port ke LedgerPay)
- `observability`: redaction atribut sensitif (`password`, `token`, `authorization`, `cookie`, `nik`, …) lewat `ReplaceAttr`, dan tipe `Sensitive[T]` (C-23, ADR-P08).
- `mail` (baru): pengirim SMTP dengan timeout, STARTTLS, dan penolakan header injection.
- `reqmeta` (baru): metadata request untuk audit, dipakai lintas modul.
- `httpx`: `SetTenantID` + `tenant_id` di log akses.
- `apperr`: `WithFields`.
- Khusus PayRun (tidak perlu di-port): `tenantctx`, `db.WithTenantTx`/`WithPlatformTx`, claim `tid` + `auth.RequireTenant`.

### 0.0.2 — PayRun Sprint 0 (belum di-port ke LedgerPay)
- `app`: `httpx.Recover` dipindah ke dalam `Metrics`/`AccessLog`; request yang panic tercatat di log akses dan metrik sebagai 500.
- `crypto`: `ParseKeyring` menolak versi kunci kosong atau yang mengandung `:` (pemisah versi di ciphertext).
- `httpx.Decode`: JSON terpotong menjadi `MALFORMED_JSON`; sisa data seperti `{"a":1}}` ditolak.
- `config`: di luar dev/test/perf, `ALLOWED_ORIGINS` dan `PUBLIC_BASE_URL` wajib https dan bukan localhost; `REFRESH_TTL <= REFRESH_FAMILY_TTL`.
- `deploy/postgres/init`: password role dikirim sebagai variabel psql, bukan diinterpolasi ke SQL.
- `Makefile`/CI: `gen-api`/`gen-check` (oapi-codegen), `trivy-fs`, gitleaks memakai `.gitleaks.toml`.

### 0.0.1 — salinan awal dari LedgerPay
