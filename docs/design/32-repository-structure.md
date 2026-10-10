# Step 10.1 — Repository Structure

> **Amandemen (PayRun, 2026-10-10):** tata letak luar repo `payrun` disamakan dengan pola `backend/` + `frontend/`: seluruh Go module (tree `cmd/`, `internal/`, `db/`, `api/` di bawah) berada di `backend/`, SPA (`web/` di bawah) menjadi `frontend/`. Isi dan aturan di dalam module tidak berubah (modul tetap `internal/modules/<x>`, depguard sama). LedgerPay belum diubah.

> Bagian dari [Step 10](00-index.md). Keputusan di `00-index.md` §4 **mengikat**: polyrepo `ledgerpay`, `payrun`, `infra`.
> Phase 14 master prompt: struktur disesuaikan dengan arsitektur yang dipilih (modular monolith, satu binary per project, ADR-001), tanpa memaksakan monorepo.

---

## 1. Kenapa polyrepo, dan apa yang disalin

### 1.1 Ringkasan alasan
| Pertimbangan | Polyrepo (dipilih) | Monorepo |
|---|---|---|
| Cara interviewer menilai | Satu link = satu project utuh (README, CI, riwayat commit, issue) | Interviewer harus memahami struktur workspace dulu |
| Kode runtime yang dibagi | Tidak ada. Skeleton **disalin** (Step 1 §1.1) | Akan menggoda untuk membuat `pkg/shared`, sehingga coupling tumbuh diam-diam |
| CI | Workflow sederhana per repo | Butuh path filter, matrix per project, cache terpisah |
| Deploy | Image per project, tag SHA per repo | Sama, tetapi trigger lebih rumit |
| Biaya | Fix skeleton harus di-port manual ke repo kedua | Satu fix berlaku untuk semua |

### 1.2 Apa yang disalin dari `ledgerpay` ke `payrun`
| Disalin | Tidak disalin |
|---|---|
| `internal/platform/{config,db,httpx,auth,jobs,observability,clock,idgen,faults,testhooks}` | `internal/modules/*` (domain berbeda) |
| `cmd/<proj>/main.go` (pola subcommand) | Mock provider |
| Konfigurasi lint (`.golangci.yml`), Makefile, template workflow, Dockerfile | Migrasi dan query |
| Helper test (`test/fixture` dasar, invariant checker framework, testcontainers bootstrap) | Golden data |

### 1.3 Menjaga salinan agar tidak drift
- File **`SKELETON_VERSION`** di root kedua repo (contoh `skeleton 1.3.0`), plus bagian *Skeleton* di `CHANGELOG.md`.
- Setiap perubahan di `internal/platform` repo mana pun diberi label PR `skeleton`. Saat merge, buat issue "port ke repo lain" yang merujuk commit tersebut.
- Port dilakukan dengan `git diff <old>..<new> -- internal/platform | git apply` di repo tujuan, lalu direview sebagai PR biasa.
- PayRun **menambah** sesuatu di platform (`WithTenantTx`, `WithPlatformTx`, tenant guard decode). Tambahan ini diberi build file terpisah (`db/tenant.go`) supaya diff port dari LedgerPay tetap bersih.

> **Trade-off yang jujur:** drift bisa terjadi, dan setiap fix keamanan di skeleton harus diterapkan dua kali. Itu diterima karena (a) hanya ada dua repo, (b) skeleton kecil dan jarang berubah setelah Sprint 1, dan (c) shared module membuat versioning, release, dan `replace` directive jadi pekerjaan tambahan. Jalur migrasinya: kalau ada repo ketiga, ekstrak `platform` menjadi module `github.com/<user>/goplatform` yang diberi tag semver.

---

## 2. `ledgerpay/`

```text
ledgerpay/
├── cmd/
│   ├── ledgerpay/main.go            # subcommand: api | worker | migrate | seed | healthcheck
│   └── mockprovider/main.go         # binary mock PG + Bank (ADR-L05); image terpisah, repo sama
├── internal/
│   ├── platform/                    # TIDAK boleh import internal/modules/*
│   │   ├── config/                  # struct Config + Load() dari env, validasi fail-fast
│   │   ├── db/                      # pool, WithTx + retry 40001/40P01, error map (constraint → code)
│   │   ├── httpx/                   # router per grup (user/merchant/ops/webhooks), middleware,
│   │   │                            #   decode strict, Problem Details, pagination, rate limit
│   │   ├── auth/                    # JWT (kid), refresh rotation, password (argon2 + semaphore C-45),
│   │   │                            #   API key hashing, principal di context
│   │   ├── jobs/                    # claim SKIP LOCKED, lease/heartbeat, CompleteInTx (fencing),
│   │   │                            #   backoff, Permanent, scheduler (dedupe per bucket)
│   │   ├── observability/           # slog JSON + redaction, metrics, OTel tracer, request/correlation ID
│   │   ├── clock/                   # interface Clock (fake di test)
│   │   ├── idgen/                   # UUIDv7 (deterministik di unit test)
│   │   ├── egress/                  # HTTP client webhook keluar dengan guard SSRF (C-44)
│   │   ├── faults/                  # //go:build demo — titik injeksi faults.Check (demo_faults)
│   │   └── testhooks/               # //go:build testhooks — Pause / advisory-lock hook; no-op di rilis
│   ├── modules/
│   │   ├── identity/                # user, refresh token, merchant, API key (ops-only)
│   │   ├── ledger/                  # account, journal, entry, PostJournal(), verifikasi — tanpa dependency
│   │   ├── wallet/                  # baca saldo + riwayat (read-only)
│   │   ├── idempotency/             # Begin/Complete/Replay (ADR-L02)
│   │   ├── transfer/
│   │   ├── payment/                 # payment intent, confirm, refund, expiry
│   │   ├── topup/                   # top-up + recovery point + provider client
│   │   ├── payout/                  # state machine + bank client + status check
│   │   ├── webhook/                 # inbox (masuk) + delivery (keluar)
│   │   ├── recon/                   # import settlement, matching, PROVIDER_SETTLEMENT
│   │   ├── audit/
│   │   ├── ops/                     # read model + aksi ops (memanggil service modul lain)
│   │   └── demo/                    # //go:build demo — /api/v1/demo/* (O15)
│   └── mockprovider/                # implementasi mock: charges, payouts, settlement, admin, skenario
├── api/
│   ├── openapi.yaml                 # SUMBER KEBENARAN kontrak (ADR-003)
│   ├── redocly.yaml                 # ruleset lint (problem+json, uang = string)
│   └── gen/                         # oapi-codegen: types + chi server interface (di-commit, dicek di CI)
├── db/
│   ├── migrations/                  # goose SQL, NNNNN_<desc>.sql; *_contract_* untuk fase contract
│   ├── migrations_demo/             # demo_faults, dsb. — tabel versi goose_demo_version
│   ├── migrations_perf/             # objek khusus perf (jika ada)
│   ├── migrations_mock/             # skema database mockprovider
│   ├── queries/<module>.sql         # input sqlc, satu file per modul pemilik tabel
│   └── sqlc.yaml
├── web/                             # SPA Vue 3 + TS
│   ├── src/
│   │   ├── pages/                   # Login, Wallet, Transfer, Checkout, Payouts, Ops*, DemoToolbar
│   │   ├── components/
│   │   ├── composables/             # useWallet, useTransfer, … (memanggil api/)
│   │   ├── api/                     # client typed hasil openapi-typescript + fetch wrapper
│   │   │                            #   (Idempotency-Key, refresh, Problem Details)
│   │   └── stores/                  # Pinia: session (access token di memori)
│   ├── e2e/                         # Playwright: journey kritis + demo
│   └── package.json
├── test/
│   ├── fixture/                     # builder: NewUser, NewMerchant, FundedWallet, …
│   ├── invariants/                  # checker LI-01..LI-15, dipanggil di t.Cleanup
│   ├── testdb/                      # bootstrap testcontainers + CREATE DATABASE … TEMPLATE
│   ├── chaos/                       # suite chaos (toxiproxy opsional, kill worker via subprocess)
│   ├── k6/                          # LP-1 … LP-9 (Step 8 `03`)
│   └── demo-verify/                 # skrip headless Demo A–D (nightly)
├── deploy/
│   ├── Dockerfile                   # multi-stage → distroless nonroot; build arg TAGS=demo
│   ├── Dockerfile.mockprovider
│   ├── Dockerfile.web               # build SPA → artefak web-dist (dipakai image edge di infra)
│   ├── compose.yml                  # stack project ini saja (dev/CI)
│   ├── compose.dev.yml
│   └── compose.ci.yml               # E2E/demo-verify di CI
├── docs/
│   ├── adr/                         # ADR-001…013 + ADR-L01…L08 (nomor dipertahankan)
│   ├── design/                      # dokumen version 2 yang relevan (dikurasi, lihat §7)
│   ├── runbooks/                    # dari Step 8 `01` §8 + Step 9 `03` §8
│   └── demo/                        # skrip demo, screenshot, link video
├── .github/
│   ├── workflows/{pr,main,nightly,rollback,perf}.yml
│   └── pull_request_template.md
├── Makefile
├── .golangci.yml                    # depguard = aturan batas modul (§5)
├── go.mod / go.sum
├── README.md
├── SKELETON_VERSION
├── CHANGELOG.md
└── LICENSE                          # MIT
```

### 2.1 Konvensi file per modul
| File | Isi | Aturan |
|---|---|---|
| `handler.go` | Decode → validasi → panggil service → respond | Tanpa SQL, tanpa logika bisnis |
| `service.go` | Use case + **boundary transaksi** (`WithTx`) | Satu-satunya tempat transaksi dibuka |
| `repo.go` | Wrapper query sqlc milik modul ini | Hanya tabel milik modul |
| `model.go` | Domain type, value object, state machine | Tanpa import `platform/db` |
| `policy.go` | Fungsi otorisasi (`CanReadPayout(principal, payout)`) | Pure function, diuji unit |
| `errors.go` | `var ErrXxx` → dipetakan ke code katalog | — |
| `jobs.go` | Handler job modul (jika ada) | Idempoten, `CompleteInTx` |
| `*_test.go` | Unit (`_test`), integration (`//go:build integration`) | Lihat Step 8 `02` |

### 2.2 Keputusan: mock provider di repo yang sama
`cmd/mockprovider` + `internal/mockprovider` tinggal di repo `ledgerpay`, tetapi dikompilasi sebagai **binary dan image terpisah**, dengan database sendiri.
- **Alasan:** kontrak mock dan LedgerPay berevolusi bersama (Step 5 `03`). Test integration memakai mock in-process (`httptest.Server`, Step 8 `02`), dan itu butuh import package-nya. Tidak ada konsumen lain.
- **Batas tetap dijaga:** depguard melarang `internal/modules/*` meng-import `internal/mockprovider` (dan sebaliknya), kecuali dari `test/`. LedgerPay hanya berbicara dengan mock lewat HTTP.
- **Repo terpisah ditolak** karena perubahan kontrak akan butuh dua PR yang tersinkron, tanpa manfaat bagi portfolio.

### 2.3 Keputusan: client frontend dibangkitkan dari OpenAPI
`openapi-typescript` menghasilkan type dari `api/openapi.yaml`, lalu wrapper `fetch` tipis menambahkan `Idempotency-Key` (UUID per aksi user), refresh otomatis saat `TOKEN_EXPIRED`, dan parsing Problem Details. Hasil generate di-commit, dan CI gagal kalau tidak sinkron (`make gen && git diff --exit-code`).

---

## 3. `payrun/`

Hanya perbedaan terhadap `ledgerpay/` yang ditampilkan.

```text
payrun/
├── cmd/payrun/main.go               # api | worker | migrate | seed | healthcheck | verify-run (recompute dari snapshot, bukti PI-12)
├── internal/
│   ├── platform/                    # disalin dari LedgerPay (SKELETON_VERSION), ditambah:
│   │   ├── db/
│   │   │   ├── tx.go                # WithTx (dari skeleton)
│   │   │   ├── tenant.go            # WithTenantTx (set_config transaction-local), WithPlatformTx
│   │   │   └── tenantguard.go       # decode helper: row.tenant_id == ctx tenant (C-46)
│   │   ├── tenancy/                 # Tenant di context, middleware claim tid
│   │   └── storage/                 # client S3-compatible, presigned URL (ADR-012)
│   ├── modules/
│   │   ├── tenancy/                 # signup (platform idempotency), settings, seed default tenant
│   │   ├── iam/                     # membership, role, invitation, switch tenant, aturan pemberian role
│   │   ├── org/                     # department, position, org_assignments (effective-dated)
│   │   ├── employee/                # employee, PII (encrypt/mask/blind index), lifecycle
│   │   ├── compensation/            # component definition, salary assignment, ruleset
│   │   ├── attendance/              # record, koreksi, import CSV (job), kalender kerja
│   │   ├── leave/
│   │   ├── overtime/
│   │   ├── approval/                # mesin approval generik + dispatch per subject
│   │   ├── payroll/
│   │   │   ├── engine/              # PURE: Calculate(input, ruleset) — tanpa db/time/net
│   │   │   │   └── testdata/
│   │   │   │       ├── cases/<nama>.json          # input + referensi ruleset
│   │   │   │       ├── cases/<nama>.golden.json   # hasil lengkap + explanation
│   │   │   │       └── rulesets/ID-SIMPLIFIED-2026.1.json
│   │   │   ├── run/                 # state machine run, plan/chunk jobs, fingerprint, change log,
│   │   │   │                        #   PeriodGuard (FOR SHARE + tenant_settings KEY SHARE)
│   │   │   └── adjustment/
│   │   ├── payslip/                 # view, render PDF (job), presigned download + audit
│   │   ├── audit/
│   │   └── demo/                    # //go:build demo
│   └── pii/                         # AES-256-GCM keyring + HMAC blind index (ADR-P08)
├── db/
│   ├── migrations/                  # setiap tabel tenant: ENABLE + FORCE RLS + policy di migrasi yang sama
│   ├── migrations_demo/
│   ├── migrations_perf/             # role payrun_seed (BYPASSRLS) — hanya env perf
│   ├── checks/                      # SQL katalog: tabel tanpa RLS/FORCE, SECURITY DEFINER di luar allowlist,
│   │                                #   FK tanpa tenant_id, BYPASSRLS selain superuser/backup
│   ├── queries/
│   └── sqlc.yaml
├── web/src/pages/                   # SignupTenant, Login+PilihTenant, Employees, EmployeeDetail (tab gaji),
│                                    #   ApprovalInbox, PayrollRuns, RunReview, Payslips (self-service),
│                                    #   ClockInOut, LeaveRequest, Members, DemoToolbar
├── test/
│   ├── fixture/                     # Tenant("alpha") dengan user setiap role, Tenant("beta")
│   ├── invariants/                  # checker PI-01..PI-19
│   ├── isolation/                   # cross-tenant suite + authz matrix (Step 5 `05`) table-driven
│   ├── k6/                          # PR-1 … PR-7
│   └── demo-verify/                 # Demo A–C
└── …                                # deploy/, docs/, .github/, Makefile — sama dengan LedgerPay
```

Perbedaan penting:
- **`payroll/engine`** adalah package terisolasi dengan golden data di dalamnya. Depguard memastikan package ini tidak bisa menyentuh I/O (§5).
- **`db/checks/`** berisi query katalog yang dijalankan oleh migration test di CI **dan** sebagai post-migrate check saat deploy (Step 9 `02`).
- **`test/isolation/`** adalah artefak Demo A: output test-nya ditampilkan langsung di depan interviewer.

---

## 4. `infra/`

> **Step 12 (R4-8/S3):** repo `infra` dan image `edge` baru dibuat di **PayRun Sprint 0**. Selama LedgerPay, isi yang setara (compose demo, Caddyfile, skrip deploy/backup) tinggal di folder `deploy/` repo `ledgerpay`, dan SPA dibawa image project sendiri.

```text
infra/
├── compose/
│   ├── compose.yml                  # gabungan: caddy(edge), postgres, minio(+init), backup,
│   │                                #   ledgerpay-{migrate,api,worker}, mock-provider(+migrate),
│   │                                #   payrun-{migrate,api,worker}
│   ├── compose.obs.yml              # profile obs: prometheus, grafana, loki, promtail, jaeger,
│   │                                #   alertmanager, node_exporter
│   ├── compose.demo.yml             # image <sha>-demo, DEMO_MODE, lease 10s, mock admin internal
│   └── compose.perf.yml             # profile perf: seed, k6, pg_stat_statements/auto_explain
├── edge/
│   ├── Caddyfile                    # site per domain, header C-10, /api proxy + lb_try_duration,
│   │                                #   /checkout/* mock, files.<domain> presigned GET
│   └── Dockerfile                   # caddy + web-dist kedua project (artefak CI masing-masing repo)
├── observability/
│   ├── prometheus.yml
│   ├── rules/{platform,ledgerpay,payrun}.yml      # alert (Step 8 `01` §7)
│   ├── alertmanager.yml
│   ├── grafana/
│   │   ├── provisioning/{datasources,dashboards}/*.yml
│   │   └── dashboards/*.json        # 6 dashboard (Step 8 `01` §6), di-review lewat PR
│   ├── loki.yml
│   ├── promtail.yml
│   └── jaeger/                      # konfigurasi (jika perlu selain default)
├── postgres/
│   ├── init/                        # 3 database + role owner/app/readonly/backup/payrun_backup
│   ├── perf/01-seed-role.sql        # payrun_seed — di-mount hanya oleh compose.perf.yml
│   └── postgresql.conf              # tuning VM 8 GB, pg_stat_statements
├── secrets/
│   ├── .sops.yaml                   # recipient age
│   └── *.env.sops                   # per service; tidak ada plaintext di repo
├── scripts/
│   ├── bootstrap.sh                 # provisioning idempoten (docker, ufw, fail2ban, user deploy, …)
│   ├── deploy-gate                  # forced command SSH: allowlist + validasi SHA
│   ├── deploy.sh                    # pull → migrate → post-migrate checks → worker → api → health gate
│   ├── rollback.sh
│   ├── backup.sh                    # pg_dump per DB (role backup / payrun_backup) → age → rclone
│   ├── restore.sh
│   └── restore-drill.sh             # restore ke container sementara + invariant & row-count check
├── perf/create.sh                   # VM efemeral (SUT + k6) via CLI penyedia
├── cloud-init/user-data.yml
├── .github/workflows/
│   ├── ci.yml                       # caddy validate, compose config, promtool, amtool, shellcheck, …
│   ├── deploy-infra.yml             # sync config → caddy reload / up -d service shared
│   └── restore-drill.yml            # mingguan (Step 9 `03`)
└── README.md                        # cara membangun ulang VM dari nol (RTO 2 jam)
```

> Workflow `deploy-infra.yml` di Step 9 `02` memakai path filter `caddy/`. Path itu diselaraskan menjadi **`edge/`** sesuai tree ini.

---

## 5. Aturan dependency (ditegakkan oleh depguard)

| # | Aturan | Kenapa | Project |
|---|---|---|---|
| D1 | `internal/platform/**` ↛ `internal/modules/**` | Platform harus generik dan bisa disalin | Keduanya |
| D2 | Modul A ↛ `repo.go`/`model.go` internal modul B. Interaksi hanya lewat interface service yang diekspor modul B | Batas tabel per modul (ADR-001); memungkinkan ekstraksi V3 | Keduanya |
| D3 | `modules/ledger` ↛ modul lain mana pun | Ledger adalah inti tanpa dependency | LedgerPay |
| D4 | `modules/payroll/engine` ↛ `platform/db`, `time`*, `net/**`, `os`, `math/rand` | Determinisme (PI-12): pure function | PayRun |
| D5 | `platform/faults`, `modules/demo` hanya dikompilasi dengan build tag `demo` | Tidak ada fault injection di rilis | Keduanya |
| D6 | `platform/testhooks` hanya dengan build tag `testhooks` | Hook interleaving no-op di produksi | Keduanya |
| D7 | `db.WithPlatformTx` hanya boleh dipanggil dari `modules/tenancy`, `modules/iam` | Jalur lintas tenant yang eksplisit (ADR-P02) | PayRun |
| D8 | `internal/modules/**` ↛ `internal/mockprovider/**` (dan sebaliknya); kecuali `test/**` | Mock hanya lewat HTTP | LedgerPay |
| D9 | Hanya `platform/db` yang boleh meng-import `jackc/pgx` secara langsung (modul memakai type sqlc + `pgx.Tx` lewat interface tipis) | Satu tempat untuk error mapping dan retry | Keduanya |

\* `time.Time` sebagai type boleh. Yang dilarang adalah `time.Now()`, dan itu ditegakkan lewat `forbidigo`, karena depguard bekerja di level package.

```yaml
# .golangci.yml (potongan)
linters:
  enable: [depguard, forbidigo, gosec, errcheck, staticcheck, govet, revive]
linters-settings:
  depguard:
    rules:
      platform-is-generic:
        files: ["**/internal/platform/**"]
        deny:
          - pkg: "github.com/<user>/payrun/internal/modules"
            desc: "D1: platform tidak boleh bergantung pada modul domain"
      engine-is-pure:
        files: ["**/internal/modules/payroll/engine/**", "!**/*_test.go"]
        deny:
          - pkg: "github.com/<user>/payrun/internal/platform/db"
            desc: "D4: engine harus pure (PI-12)"
          - pkg: "net"
            desc: "D4"
          - pkg: "os"
            desc: "D4"
          - pkg: "math/rand"
            desc: "D4"
  forbidigo:
    forbid:
      - p: '^time\.Now$'
        msg: "pakai platform/clock (D4, test deterministik)"
      - p: '^db\.WithPlatformTx$'   # dikecualikan via path untuk tenancy/iam (exclude-rules)
        msg: "D7: hanya modul tenancy/iam"
```

D2 (akses internal modul lain) sulit diekspresikan sepenuhnya dengan depguard. Yang dipakai: konvensi `internal/` bertingkat (`modules/payout/internal/...` untuk repo/model yang tidak diekspor), sehingga **compiler Go sendiri** menolak import dari modul lain.

---

## 6. Makefile

| Target | Fungsi | Dipakai CI? |
|---|---|---|
| `up` | `compose.yml + compose.dev.yml up -d --wait` (build source, migrasi otomatis) | Tidak |
| `down` | Stop + hapus container (volume dipertahankan; `down-v` untuk reset data) | Tidak |
| `dev` | `up` + hot reload (`air`) + Vite dev server | Tidak |
| `obs` | Tambah profile `obs` | Tidak |
| `demo` | Image demo + `compose.demo.yml` + `seed --profile demo` | Ya (E2E) |
| `seed` | `ledgerpay seed --profile=<dev\|demo\|perf>` | Ya (perf) |
| `gen` | `sqlc generate` + `oapi-codegen` + `openapi-typescript` | Ya (`gen && git diff --exit-code`) |
| `lint` | golangci-lint, eslint, prettier, redocly lint, hadolint, actionlint | Ya |
| `test` | `go test -race ./...` (unit, golden, property pendek) | Ya |
| `test-integration` | `go test -race -tags integration ./...` (testcontainers) | Ya |
| `test-concurrency` | `-tags integration,testhooks -run 'Concurrent\|Race'` | Ya |
| `test-chaos` | Suite chaos (subprocess kill, toxiproxy opsional) | Kondisional / nightly |
| `e2e` | `compose.ci.yml up --wait` + Playwright | Ya |
| `demo-verify` | Demo A–D / A–C headless | Nightly |
| `demo-reset-b` (LedgerPay) | Kembalikan saldo Rina ke Rp100.000 untuk mengulang Demo B | — (demo) |
| `demo-intent` (LedgerPay) | Buat payment intent merchant untuk tab checkout demo | — (demo) |
| `migrate-new NAME=` | `goose create NAME sql` di `db/migrations` | Tidak |
| `migrate-up` | `<bin> migrate up` dengan DSN owner lokal | Ya (migration test) |
| `k6-smoke` | Skenario k6 kecil dengan threshold | Nightly |
| `build` | `go build` binary (release; `TAGS=demo` untuk varian demo) | Ya |
| `docker` | `docker buildx build` image release + demo | Ya (`main.yml`) |

---

## 7. Lokasi dokumen desain setelah coding dimulai

| Dokumen di `version 2/` | Masuk ke `ledgerpay/docs/` | Masuk ke `payrun/docs/` |
|---|---|---|
| Step 1 (strategi) | `design/00-product.md` (bagian LedgerPay + overview) | `design/00-product.md` (bagian PayRun + overview) |
| Step 2 feature + arsitektur | `design/01-features.md`, `02-architecture.md` | idem untuk PayRun |
| ADR | `adr/ADR-001…013` + `ADR-L01…L08` | `adr/ADR-001…013` + `ADR-P01…P09` |
| Step 3–9 | File LedgerPay + bagian shared, diringkas per topik (`03-domain.md`, `04-database.md`, …) | idem untuk PayRun |
| Runbook | `runbooks/*.md` | `runbooks/*.md` |
| Infra (Step 9 `01`, `03`, `04`) | — | — (masuk `infra/README.md` + `infra/docs/`) |

**Aturan living docs:**
1. PR yang mengubah perilaku yang terdokumentasi (state machine, constraint, kontrak API, invariant) **wajib** memperbarui dokumen terkait di PR yang sama. Ini menjadi checklist di template PR.
2. Keputusan baru atau perubahan keputusan berarti **ADR baru** dengan status `Supersedes ADR-xxx`. ADR lama tidak diedit, kecuali bagian "Amandemen" yang sudah dipakai di Step 5–9.
3. Test meta `TestTraceability_DocsReferenceExistingTests` (Step 8 `02`) mencegah dokumen merujuk test yang sudah tidak ada.
4. Folder `version 2/` tetap menjadi arsip desain pra-coding dan tidak lagi diperbarui setelah dokumen dipindah ke repo.

---

## 8. Konvensi kerja

| Aspek | Konvensi |
|---|---|
| Branch | `feat/<scope>-<desc>`, `fix/<scope>-<desc>`, `chore/…`, `docs/…`; scope = nama modul (`feat/payout-status-check`) |
| Commit | Conventional Commits: `feat(payout): …`, `fix(ledger): …`, `test(transfer): …`, `docs(adr): …`; `!` untuk breaking API |
| Merge | Squash merge ke `main`; judul PR = pesan commit |
| Template PR | **Apa & kenapa** · **Invariant / state machine yang tersentuh** (LI/PI) · **Test yang ditambah** (nama) · **Dokumen/ADR diperbarui?** (ya/tidak + link) · **Migrasi** (expand/contract?) · **Screenshot** (jika UI) · Checklist: lint, test, gen sinkron |
| CODEOWNERS | Tidak dipakai (solo). Branch protection memakai *conversation resolution* + checklist (Step 9 `02`) |
| Label issue | `type:feature`, `type:bug`, `type:chore`, `area:<modul>`, `priority:P0..P3`, `skeleton` (perlu di-port), `flaky` (karantina, SLA 7 hari), `sprint:<n>` |
| Versi | Tag `vMAJOR.MINOR.PATCH` di milestone (contoh `v0.7.0` = M3 siap melamar); deploy tetap memakai SHA |
