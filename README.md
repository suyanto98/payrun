# PayRun

> Multi-tenant workforce and payroll platform: one database, many companies, and no tenant can ever see another tenant's data.

**Status:** Sprint 5 (approval + stale detection). Sprint 4 selesai: payroll run asinkron per chunk lewat `payrun worker` (resumable, fencing lease, guard `calc_version`), role change/revoke dengan penjaga owner terakhir, full view PII ter-audit; di atas Sprint 1–3 (tenancy, IAM, RLS, employee + PII terenkripsi, kompensasi effective-dated, payroll engine dengan 31 golden case, contoh Tono Maret 2026 = Rp6.888.609) ([roadmap](docs/design/30-roadmap.md)).

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
| `web/` | SPA (Vue 3 + TypeScript): login, tenant picker, employees |
| `deploy/` | Dockerfile (distroless), dev compose, Postgres init (roles) |
| `docs/` | Design documents and ADRs ([index](docs/README.md)) |

## Security model in one paragraph

The API connects as `payrun_app` (`NOBYPASSRLS`, not the owner) and refuses to start otherwise. Every tenant table has `ENABLE` + `FORCE ROW LEVEL SECURITY` with policies built on `payrun.current_tenant()`, which returns `NULL` (zero rows) when no tenant is set; a catalogue test fails the build for any table that does not. Repositories also compare each decoded row's `tenant_id` with the request tenant (C-46). NIK, NPWP and bank account numbers are encrypted with AES-256-GCM bound to tenant + column, with a separate HMAC blind index for uniqueness. Details: [ADR-P01](docs/adr/ADR-P01-multi-tenancy-pooled-rls.md), [multi-tenancy](docs/design/06-multi-tenancy.md), [RLS & indexing](docs/design/13-db-rls-consistency-indexing.md).

## License

MIT
