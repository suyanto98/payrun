# Changelog

## Skeleton

Skeleton (`internal/platform`, `cmd`, `deploy`, CI) disalin dari LedgerPay, bukan dibagi sebagai module
(docs/design/32-repository-structure.md). Setiap perubahan skeleton dicatat di sini dan harus di-port ke repo lain.

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
