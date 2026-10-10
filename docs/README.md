# PayRun — Documentation

Design documents were written before coding, step by step (product → architecture → domain → database → API → security → transactions → observability/testing → DevOps → roadmap → final review). This folder contains the subset relevant to PayRun, copied verbatim from the design archive (`my-project/step-*`) in Sprint 0.

> **Living docs.** If implementation proves a design wrong, the PR that changes behaviour also updates the doc or adds an ADR amendment (PR template checklist).
> **Authority when two docs disagree:** [`design/34-final-review.md`](design/34-final-review.md) and the "Amandemen Step 12" banner in [`design/30-roadmap.md`](design/30-roadmap.md) → "Amandemen" / "Catatan implementasi" sections inside ADRs → later design docs over earlier ones.
> Cross-links inside the documents still use the original archive naming (`Step N`, `step-0N-…/file.md`); use the mapping table below.
> Some documents cover both projects (shared tables, API conventions, error catalogue, shared threats, security controls, transaction rules, background jobs, observability, testing, DevOps). The LedgerPay sections there are context only.

## Operating docs (written from the implementation, Sprint 10)
- [`architecture.md`](architecture.md): containers, tenant context per request, run state machine, modules
- [`demo-script.md`](demo-script.md): 10-minute demo, executed with assertions by `make demo-live`
- [`runbook.md`](runbook.md): one section per alert + procedures (every alert links here, checked by a test)
- [`dod-audit.md`](dod-audit.md): Definition of Done audit for M7
- Performance: [`../perf/results/2026-10-10-fed1df1/REPORT.md`](../perf/results/2026-10-10-fed1df1/REPORT.md)

## Start here (design)
1. [`design/00-prd.md`](design/00-prd.md): what we build and why
2. [`design/30-roadmap.md`](design/30-roadmap.md): sprint plan (read the Step 12 amendment banner first)
3. [`design/31-definition-of-done.md`](design/31-definition-of-done.md): what "done" means
4. [`design/06-multi-tenancy.md`](design/06-multi-tenancy.md) and [`design/13-db-rls-consistency-indexing.md`](design/13-db-rls-consistency-indexing.md): tenant isolation, the core risk of PayRun

## Architecture Decision Records — [`adr/`](adr/)

Shared ADRs (001–013) are the LedgerPay copies, including their implementation notes: PayRun reuses the same skeleton.

| ADR | Topic |
|---|---|
| [ADR-001](adr/ADR-001-modular-monolith.md) | Modular monolith |
| [ADR-002](adr/ADR-002-tech-stack-go-vue.md) | Go + Vue 3 |
| [ADR-003](adr/ADR-003-rest-api-openapi.md) | REST + spec-first OpenAPI, RFC 9457 errors |
| [ADR-004](adr/ADR-004-postgresql.md) | PostgreSQL as the only source of truth |
| [ADR-005](adr/ADR-005-data-access-sqlc.md) | sqlc + pgx, goose migrations |
| [ADR-006](adr/ADR-006-background-processing-postgres-queue.md) | Postgres-backed outbox/job queue (+ fencing) |
| [ADR-007](adr/ADR-007-redis-not-in-v1.md) | No Redis in V1 |
| [ADR-008](adr/ADR-008-caching.md) | No application cache in V1 |
| [ADR-009](adr/ADR-009-authentication.md) | JWT access + rotating refresh tokens |
| [ADR-010](adr/ADR-010-authorization.md) | RBAC + resource policies, 404 vs 403 |
| [ADR-011](adr/ADR-011-transaction-management.md) | Read Committed + explicit locks, no network in tx |
| [ADR-012](adr/ADR-012-object-storage.md) | Object storage (amended S0: SeaweedFS in dev) |
| [ADR-013](adr/ADR-013-deployment.md) | Docker Compose on one VM + CI/CD |
| [ADR-P01](adr/ADR-P01-multi-tenancy-pooled-rls.md) | Pooled tables + `tenant_id` + Postgres RLS |
| [ADR-P02](adr/ADR-P02-tenant-context.md) | Tenant context propagation |
| [ADR-P03](adr/ADR-P03-payroll-engine.md) | Payroll engine: pure function, versioned rulesets |
| [ADR-P04](adr/ADR-P04-effective-dating.md) | Effective dating with `daterange` + exclusion constraint |
| [ADR-P05](adr/ADR-P05-payroll-run-execution.md) | Payroll run: state machine + chunked async jobs |
| [ADR-P06](adr/ADR-P06-decimal-rounding.md) | Decimal & rounding policy |
| [ADR-P07](adr/ADR-P07-snapshot-and-period-lock.md) | Input fingerprint, snapshot, period lock |
| [ADR-P08](adr/ADR-P08-pii-encryption.md) | PII field encryption, blind index, masking |
| [ADR-P09](adr/ADR-P09-approval-workflow.md) | Approval workflow: generic, single-level |

## Design documents — [`design/`](design/)

| File | Original (archive) | Content |
|---|---|---|
| `00-prd.md` | Step 12 `04` | Consolidated PRD |
| `01-overview.md` | Step 1 `00` | Two-project overview |
| `02-product-strategy.md` | Step 1 `02` | Vision, personas, workflows, scope |
| `03-features.md` | Step 2 `03` | Feature matrix (PF-xx) + specs |
| `04-architecture.md` | Step 2 `04` | Architecture V1 → V2 → V3 |
| `05-domain-model.md` | Step 3 `04` | Aggregates, state machines |
| `06-multi-tenancy.md` | Step 3 `05` | Tenancy model, tenant context, residual risks |
| `07-payroll-engine.md` | Step 3 `06` | Engine design, rulesets, determinism |
| `08-invariants.md` | Step 3 `07` | Payroll invariants PI-xx |
| `09-failure-scenarios.md` | Step 3 `08` | Failure scenarios |
| `10-db-shared-tables.md` | Step 4 `01` | users, refresh_tokens, idempotency_keys, jobs, audit_log |
| `11-db-erd.md` | Step 4 `05` | ERD |
| `12-db-schema.md` | Step 4 `06` | Table specs, constraints, triggers |
| `13-db-rls-consistency-indexing.md` | Step 4 `07` | Roles, RLS policies, consistency, indexes |
| `14-api-conventions.md` | Step 5 `00` | API conventions |
| `15-api-error-catalogue.md` | Step 5 `01` | Error codes (authoritative) |
| `16-api-endpoints.md` | Step 5 `04` | Endpoint contracts |
| `17-api-authorization-matrix.md` | Step 5 `05` | Endpoint × role matrix (PF-04 test input) |
| `18-security-shared-threats.md` | Step 6 `01` | Shared threats T-SH |
| `19-security-threat-model.md` | Step 6 `03` | PayRun threats |
| `20-security-controls.md` | Step 6 `04` | Controls C-xx, security tests |
| `21-tx-global-rules.md` | Step 7 `00` | Isolation, global lock order, retry |
| `22-tx-concurrency.md` | Step 7 `02` | Operation catalogue, race timelines |
| `23-background-jobs.md` | Step 7 `03` | Job catalogue, worker model |
| `24-observability.md` | Step 8 `01` | Logs, metrics catalogue, alerts |
| `25-testing-strategy.md` | Step 8 `02` | Test pyramid, test catalogue, traceability |
| `26-performance-plan.md` | Step 8 `03` | Load test scenarios, experiments |
| `27-docker-compose.md` | Step 9 `01` | Docker, Compose, Caddy, env vars |
| `28-cicd.md` | Step 9 `02` | CI/CD pipeline |
| `29-deploy-migrations-backup.md` | Step 9 `03` | Environments, migrations, backup, deploy |
| `30-roadmap.md` | Step 10 `03` | Sprint 0–10 |
| `31-definition-of-done.md` | Step 10 `04` | Definition of Done |
| `32-repository-structure.md` | Step 10 `01` | Repo layout, module boundaries |
| `33-demo-script.md` | Step 11 `02` | Demo script |
| `34-final-review.md` | Step 12 `01` | Final technical review (authoritative) |
