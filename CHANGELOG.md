# Changelog

## Skeleton

Skeleton (`internal/platform`, `cmd`, `deploy`, CI) disalin dari LedgerPay, bukan dibagi sebagai module
(docs/design/32-repository-structure.md). Setiap perubahan skeleton dicatat di sini dan harus di-port ke repo lain.

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
