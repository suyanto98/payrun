# PayRun

> Multi-tenant workforce and payroll platform: one database, many companies, and no tenant can ever see another tenant's data.

**Status:** Sprint 9 selesai (performa, subset V1 PR-1/PR-3/PR-4; [laporan](perf/results/2026-10-10-fed1df1/REPORT.md)). Berikutnya Sprint 10 (docs + demo, 🏁 M7). Sprint 8: demo faults (build tag `demo`), backup `payrun_backup` + restore drill, deploy scripts `deploy/prod` + workflow main/rollback/nightly (deploy publik ditunda sampai aplikasi siap dipakai). Sprint 7: slip gaji web + PDF deterministik (job `payslip.render`, presigned URL 5 menit), adjustment pasca-final, metrik/dashboard/alert PayRun. Sprint 6: absensi, cuti, lembur + finalize (Demo C). Sprint 5 (🏁 M6): approval + deteksi stale, `payrun verify-run`, Demo B otomatis. Sprint 4: payroll run asinkron per chunk lewat `payrun worker`. Sprint 1–3: tenancy, IAM, RLS, PII terenkripsi, kompensasi effective-dated, payroll engine 33 golden case (Tono Maret 2026 = Rp6.888.609) ([roadmap](docs/design/30-roadmap.md)).

## Quick start

Requirements: Go (version in `go.mod`), Docker, Node 22 + Yarn (SPA and `make lint-api`).

```bash
make tools        # sqlc, goose, oapi-codegen, golangci-lint, actionlint, govulncheck
make up           # postgres + migrate + api + worker; waits until /readyz is green
curl -s localhost:9190/readyz   # ops port (health/metrics), localhost only
make test         # unit tests (no Docker)
make test-integration           # Postgres via testcontainers, incl. interleaving tests
make web-dev      # SPA on http://localhost:5174 (proxies /api to the local API)
make help         # all targets
```

If port 5174 is taken, run both with the same port: `PR_WEB_PORT=5175 make up` and `PR_WEB_PORT=5175 make web-dev` (the API checks the SPA origin for CSRF).

Dev ports: API `127.0.0.1:8081`, ops `127.0.0.1:9190`, Postgres `127.0.0.1:55433` (chosen to run alongside LedgerPay).

## Layout

| Path | Content |
|---|---|
| `cmd/payrun` | Single binary: `api`, `worker`, `migrate up`, `healthcheck` |
| `internal/platform` | Skeleton shared in spirit with LedgerPay (db, auth, httpx, ratelimit, crypto, metrics, tracing); see `CHANGELOG.md` |
| `internal/app` | Composition root (routers, wiring) |
| `db/migrations` | goose migrations, embedded in the binary; run as `payrun_owner` |
| `api/openapi.yaml` | API contract (source of truth, ADR-003); `api/gen` is generated from it |
| `api/authz_matrix.yaml` | Authorization matrix as data; every route must have a row (test fails otherwise) |
| `web/` | SPA (Vue 3 + TypeScript): login, verifikasi email, tenant picker, employees, payroll run, approval, absensi, cuti, lembur |
| `deploy/` | Dockerfile (distroless), dev compose, Postgres init (roles) |
| `docs/` | Design documents and ADRs ([index](docs/README.md)) |

## Performance baseline (developer laptop, not a capacity claim)

Measured on an i3-1315U laptop (8 threads, 14 GB, k6 on the same machine) with 1,000 tenants / 100,000 employees
([full report](perf/results/2026-10-10-fed1df1/REPORT.md)):

- A payroll run for 10,000 synthetic employees finishes in about 10 s with 4 workers (13.8 s with 1). Scaling stops after
  2 workers because every chunk takes the run row lock while writing; that is already well under the 60 s per 1,000
  employees target, so it is documented rather than fixed.
- Employee/attendance reads under RLS: p95 10–20 ms at 227 req/s (about half of the saturation point); saturation at
  about 450 req/s. **Zero rows from another tenant** across four runs (657,000 responses, every employee checked).
- First bottleneck found: the employee list joined all 10,000 employees of a tenant before taking a 50-row page
  (generic plan, optional filters). Selecting the page first, then joining, raised read throughput from 37 to 433 req/s.
- Approve with stale detection: p95 31 ms (10 changed employees), 55 ms (200), versus about 1.2 s for a full recompute
  that would hold the period lock; approve stays synchronous (experiment E-2).

## Security model in one paragraph

The API connects as `payrun_app` (`NOBYPASSRLS`, not the owner) and refuses to start otherwise. Every tenant table has `ENABLE` + `FORCE ROW LEVEL SECURITY` with policies built on `payrun.current_tenant()`, which returns `NULL` (zero rows) when no tenant is set; a catalogue test fails the build for any table that does not. Repositories also compare each decoded row's `tenant_id` with the request tenant (C-46). NIK, NPWP and bank account numbers are encrypted with AES-256-GCM bound to tenant + column, with a separate HMAC blind index for uniqueness. Details: [ADR-P01](docs/adr/ADR-P01-multi-tenancy-pooled-rls.md), [multi-tenancy](docs/design/06-multi-tenancy.md), [RLS & indexing](docs/design/13-db-rls-consistency-indexing.md).

## License

MIT
