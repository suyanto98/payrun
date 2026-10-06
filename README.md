# PayRun

> Multi-tenant workforce and payroll platform: one database, many companies, and no tenant can ever see another tenant's data.

**Status:** Sprint 0 (foundation). Skeleton, database roles + RLS helpers, CI, OpenAPI skeleton. No domain features yet; tenancy + IAM start in Sprint 1 ([roadmap](docs/design/30-roadmap.md)).

## Quick start

Requirements: Go (version in `go.mod`), Docker, Node (only for `make lint-api`).

```bash
make tools        # sqlc, goose, oapi-codegen, golangci-lint, actionlint, govulncheck
make up           # postgres + migrate + api; waits until /readyz is green
curl -s localhost:9190/readyz   # ops port (health/metrics), localhost only
make test         # unit tests (no Docker)
make test-integration           # Postgres via testcontainers, incl. interleaving tests
make help         # all targets
```

Dev ports: API `127.0.0.1:8081`, ops `127.0.0.1:9190`, Postgres `127.0.0.1:55433` (chosen to run alongside LedgerPay).

## Layout

| Path | Content |
|---|---|
| `cmd/payrun` | Single binary: `api`, `migrate up`, `healthcheck` |
| `internal/platform` | Skeleton shared in spirit with LedgerPay (db, auth, httpx, ratelimit, crypto, metrics, tracing); see `CHANGELOG.md` |
| `internal/app` | Composition root (routers, wiring) |
| `db/migrations` | goose migrations, embedded in the binary; run as `payrun_owner` |
| `api/openapi.yaml` | API contract (source of truth, ADR-003); `api/gen` is generated from it |
| `deploy/` | Dockerfile (distroless), dev compose, Postgres init (roles) |
| `docs/` | Design documents and ADRs ([index](docs/README.md)) |

## Security model in one paragraph

The API connects as `payrun_app` (`NOBYPASSRLS`, not the owner) and refuses to start otherwise. Every tenant table will have `ENABLE` + `FORCE ROW LEVEL SECURITY` with policies built on `payrun.current_tenant()`, which returns `NULL` (zero rows) when no tenant is set. Details: [ADR-P01](docs/adr/ADR-P01-multi-tenancy-pooled-rls.md), [multi-tenancy](docs/design/06-multi-tenancy.md), [RLS & indexing](docs/design/13-db-rls-consistency-indexing.md).

## License

MIT
