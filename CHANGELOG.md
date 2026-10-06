# Changelog

## Skeleton

Skeleton (`internal/platform`, `cmd`, `deploy`, CI) disalin dari LedgerPay, bukan dibagi sebagai module
(docs/design/32-repository-structure.md). Setiap perubahan skeleton dicatat di sini dan harus di-port ke repo lain.

### 0.0.2 — PayRun Sprint 0 (belum di-port ke LedgerPay)
- `app`: `httpx.Recover` dipindah ke dalam `Metrics`/`AccessLog`; request yang panic tercatat di log akses dan metrik sebagai 500.
- `crypto`: `ParseKeyring` menolak versi kunci kosong atau yang mengandung `:` (pemisah versi di ciphertext).
- `httpx.Decode`: JSON terpotong menjadi `MALFORMED_JSON`; sisa data seperti `{"a":1}}` ditolak.
- `config`: di luar dev/test/perf, `ALLOWED_ORIGINS` dan `PUBLIC_BASE_URL` wajib https dan bukan localhost; `REFRESH_TTL <= REFRESH_FAMILY_TTL`.
- `deploy/postgres/init`: password role dikirim sebagai variabel psql, bukan diinterpolasi ke SQL.
- `Makefile`/CI: `gen-api`/`gen-check` (oapi-codegen), `trivy-fs`, gitleaks memakai `.gitleaks.toml`.

### 0.0.1 — salinan awal dari LedgerPay
