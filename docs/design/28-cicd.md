# Step 9.2 — CI/CD Pipeline

> Bagian dari [Step 9](00-index.md). Mengikat: GitHub Actions, GHCR, tag = git SHA, deploy via SSH dengan health gate + rollback otomatis, tanpa staging di V1, secret lewat sops + age, image `release` dan `demo` dibangun terpisah.
>
> **Amandemen Step 12 (otoritatif):** (R4-7) secret V1 dari GitHub environment secrets, sops + age = Production Enhancement; (R4-9) image `release` dibangun + dipindai trivy tetapi **tidak di-push** di V1, hanya `<sha>-demo` yang di-push dan di-deploy; (S5) forced command `deploy-gate` = Production Enhancement, V1 memakai user `deploy` dengan SSH key khusus dan `deploy.sh` dipanggil langsung; (S16) ruleset Redocly kustom ditunda; (S19) perf smoke nightly ditunda sampai setelah M4/M7; restore drill mingguan di repo `infra` baru ada setelah PayRun S0 (sebelum itu dijalankan manual).
> Sumber gate: Step 6 `04` §3 (security pipeline), Step 8 `02` §7 (gating test), Step 8 `03` §9 (perf smoke), Step 4 `07` §2.5 (katalog RLS), ADR-013.

---

## 1. Gambaran pipeline

Rantai minimal dari master prompt tetap terlihat utuh di jalur `main`. Jalur lain hanya menambah atau mengurangi langkah.

```text
                ┌────────────── rantai minimal master prompt ──────────────┐
PUSH ke main:   Git Push → CI → Lint → Unit Test → Integration Test → Build → Docker Image → Deploy → Health Check
                                  │         │              │             │         │            │          │
                                  │         │              │             │      trivy gate   migrate    /readyz + smoke
                                  │         │              │             │      SBOM/attest  (expand)   gagal → rollback
                                  ▼         ▼              ▼             ▼
PULL REQUEST:   lint ─┬─ unit+golden+property ─┐
                      ├─ security static ──────┼─▶ integration + contract + migration catalogue ─▶ build (load lokal, tanpa push)
                      └─ (paralel) ────────────┘                                                     └─▶ trivy ─▶ compose up ─▶ E2E
                                                                                                    (chaos: hanya label/path)
NIGHTLY:        demo-verify · property 1 juta iterasi · chaos penuh · perf smoke · rescan dependency & image
                · TestTraceability_DocsReferenceExistingTests · ZAP baseline (P2)
MANUAL:         rollback.yml (input: SHA) · perf.yml (buat VM perf efemeral → seed → k6 → laporan → hapus VM)
INFRA REPO:     deploy-infra.yml (Caddy, compose shared, konfigurasi observability, skrip backup)
```

---

## 2. Daftar workflow

### 2.1 Per repo project (`ledgerpay`, `payrun`)

| File | Trigger | Job (urutan `needs`) | Secret / environment | Durasi target |
|---|---|---|---|---|
| `pr.yml` | `pull_request` ke `main` | `lint`, `unit`, `security-static` (paralel) → `integration` → `build-local` → `e2e`; `chaos` (kondisional) | Tidak ada secret (aman untuk PR dari fork) | ≤ 10 mnt (tanpa chaos) |
| `main.yml` | `push` ke `main` | `verify` (lint + unit + security-static + integration, dijalankan ulang di commit merge) → `build-push` → `scan` → `deploy` (environment `demo`) → `post-deploy-smoke` | `demo`: `DEPLOY_SSH_KEY`, `DEPLOY_KNOWN_HOSTS`, `DEPLOY_HOST`, `SOPS_AGE_KEY`; GHCR via `GITHUB_TOKEN` | ≤ 15 mnt |
| `nightly.yml` | `schedule` (02:00 WIB = 19:00 UTC) + `workflow_dispatch` | `demo-verify`, `property-long`, `chaos-full`, `perf-smoke`, `rescan`, `traceability` (paralel, independen) | — (restore drill dipindah ke `infra/.github/workflows/restore-drill.yml`, mingguan) | 30–45 mnt |
| `rollback.yml` | `workflow_dispatch` (input `sha`, `reason`) | `guard` → `deploy` (tanpa migrasi) → `post-deploy-smoke` | Sama dengan `main.yml` (environment `demo`) | ≤ 5 mnt |
| `perf.yml` | `workflow_dispatch` (input `scenario`, `vm_size`) | `provision-vm` → `deploy-perf` → `seed` → `k6` → `collect` (pg_stat_statements, pprof) → `report` → `destroy-vm` (`if: always()`) | Environment `perf`: token API penyedia VM, SSH key perf | 30–120 mnt |

### 2.2 Repo `infra`

| File | Trigger | Job | Catatan |
|---|---|---|---|
| `ci.yml` | `pull_request` | `sops` decrypt-check (format saja, tanpa membuka nilai), `caddy validate`, `docker compose config` untuk semua profile, `promtool check rules`, `amtool check-config`, `shellcheck`, `actionlint`, `compose_hardening_check` (C-28) | Tanpa secret |
| `deploy-infra.yml` | `push` ke `main` pada path `edge/`, `compose/`, `observability/`, `scripts/` | `validate` → `deploy` (environment `demo`): sync file → `caddy reload` (tanpa downtime) → `docker compose up -d` untuk service shared (postgres, minio, stack obs) → health check | Perubahan pada service `postgres` **wajib** lewat approval environment manual, karena restart DB berarti downtime kedua project |

---

## 3. Pipeline Pull Request

| # | Job | Alat & perintah | Kebijakan gagal | Catatan |
|---|---|---|---|---|
| 1 | `lint` (Go) | `golangci-lint run` (gosec, errcheck, staticcheck, govet, **depguard** dengan aturan batas modul ADR-001: `platform/*` tidak boleh import `modules/*`, `payroll/engine` tidak boleh import `platform/db`/`time`) | Fail pada temuan apa pun; `//nolint` wajib beralasan | Konfigurasi `.golangci.yml` di repo |
| 1 | `lint` (generated) | `sqlc diff` + `sqlc vet`; `oapi-codegen` dijalankan ulang lalu `git diff --exit-code` | Fail jika kode hasil generate tidak sama dengan yang di-commit | Mencegah spec/query drift |
| 1 | `lint` (OpenAPI) | **`redocly lint api/openapi.yaml`** (Redocly CLI) dengan ruleset: operationId wajib, semua respons 4xx/5xx `application/problem+json`, field uang bertipe `string` (`TestContract_MoneyIsString` versi statis) | Fail | Dipilih karena satu alat sekaligus bisa bundle dan preview docs |
| 1 | `lint` (frontend) | `eslint` (`vue/no-v-html: error`, C-11), `prettier --check`, `vue-tsc --noEmit` | Fail | |
| 1 | `lint` (infra) | `actionlint` (workflow), `hadolint` (Dockerfile), `pinned_actions_check` (semua `uses:` di-pin ke SHA, C-27) | Fail | |
| 2 | `unit` | `go test -race -short -count=1 ./...` via `gotestsum --junitfile`; termasuk golden test payroll dan property test 10k iterasi | Fail; timeout 2 mnt | Tanpa Docker |
| 2 | `security-static` | `govulncheck ./...` (fail jika vuln **terjangkau**), `osv-scanner` untuk `package-lock.json` (fail HIGH/CRITICAL), `gitleaks detect` (C-19), `trivy fs --scanners vuln,misconfig` (fail CRITICAL/HIGH yang punya fix) | Fail | C-26 |
| 3 | `integration` | `go test -race -count=1 -tags integration ./...`, paralel per package (`-p 4`); **testcontainers-go**, bukan GitHub service container | Fail; timeout 10 mnt | Alasan memilih testcontainers: setiap package membuat DB template dan clone per test (Step 8 `02`), terhubung sebagai `<proj>_app`; perilaku sama persis di laptop dan CI |
| 3 | `integration` (contract) | Test kontrak terhadap `openapi.yaml` (client hasil generate) + kontrak mock-provider | Fail | Satu job dengan integration agar container Postgres dipakai ulang |
| 3 | `integration` (migration catalogue) | Migrasi dari nol → query katalog: RLS + `FORCE` di semua tabel ber-`tenant_id` (PayRun, Step 4 `07` §2.5), tidak ada `SECURITY DEFINER` selain allowlist `payrun.auth_lookup_user`, `<proj>_app` bukan owner dan tanpa `BYPASSRLS`, tabel append-only tanpa grant UPDATE/DELETE, setiap FK tenant-scoped menyertakan `tenant_id`; `TestDBErrorMapping_AllCatalogueConstraints` | Fail | |
| 4 | `build-local` | `docker buildx build --load` untuk image `release` dan `demo` (`--build-arg BUILD_TAGS=demo`); **tidak di-push** | Fail | Tidak ada tag `pr-<n>` di GHCR: menghindari sampah registry dan kebutuhan secret di PR fork |
| 4 | `build-local` (meta) | `TestBuild_NoTesthooksInRelease`, `TestBuild_DemoEndpointsAbsentWithoutDemoMode` (cek simbol/rute di binary `release`) | Fail | O15 |
| 4 | `scan-local` | `trivy image` untuk kedua image hasil `--load` | Fail CRITICAL/HIGH yang punya fix | Sama dengan gate di `main` |
| 5 | `e2e` | `docker compose -f compose.yml -f compose.ci.yml up -d --wait` → `npx playwright test` (jalur kritis: login, transfer, top-up via checkout mock, payment checkout / PayRun: login, clock-in, request & approve cuti, run payroll kecil, lihat slip) + `headers_*_present` | Fail; timeout 10 mnt | **Berjalan di setiap PR.** Suite dijaga kecil (Step 8: < 5 mnt) agar budget tetap aman |
| 6 | `chaos` | Suite chaos (toxiproxy, kill worker via `demo_faults`) | Fail **jika dijalankan** | Hanya jika PR berlabel `chaos` **atau** menyentuh path `internal/platform/jobs/`, `modules/payout/`, `modules/payroll/run/`, `modules/webhook/` (filter `dorny/paths-filter` yang di-pin SHA) |

**Urutan dan biaya:** job 1–2 paralel (±3 mnt), integration (±4 mnt), build + E2E (±4 mnt). Total ±10 mnt. Chaos menambah ±6 mnt bila aktif.

---

## 4. Pipeline `main` (deploy ke VM demo)

### 4.1 Job

| Job | Isi | Gate |
|---|---|---|
| `verify` | Lint, unit, security-static, integration (sama dengan PR, dijalankan ulang di commit hasil merge untuk menangkap konflik semantik antar-PR). E2E **tidak** diulang di sini, karena sudah lewat di PR dan akan diwakili smoke test pasca-deploy | Wajib hijau |
| `build-push` | (Step 12: R4-9) varian **release** dibangun + dipindai saja (tidak di-push); `docker buildx build --push` hanya `ghcr.io/<owner>/<proj>:<sha>-demo` (**demo**, yang di-deploy). Rencana awal dua varian: `ghcr.io/<owner>/<proj>:<sha>` (**release**) dan `ghcr.io/<owner>/<proj>:<sha>-demo` (**demo**), plus tag bergerak `main` / `main-demo` (hanya informatif, deploy tidak pernah memakainya). `--provenance=mode=max --sbom=true` (attestation buildx). Web SPA ikut sebagai stage di image yang sama atau image `-web` terpisah (diputuskan di `01`) | — |
| `scan` | `trivy image` pada digest yang baru di-push | Fail CRITICAL/HIGH yang punya fix → **tidak deploy** |
| `deploy` | Environment `demo` (protection: hanya branch `main`; reviewer manual **opsional**, default nonaktif agar alur demo cepat). Menjalankan `deploy.sh` di VM (§5, §11) dengan image **`-demo`** | Health gate di dalam skrip; gagal → rollback otomatis + job merah |
| `post-deploy-smoke` | Dari runner GitHub (sudut pandang internet): `GET https://<host>/api/v1/healthz` (via Caddy), lalu skrip smoke (§4.3) | Gagal → panggil rollback + job merah |

**Signing (P2):** `cosign sign --yes` dengan keyless OIDC GitHub pada digest, dan `deploy.sh` menjalankan `cosign verify` sebelum pull. Ditunda karena manfaatnya kecil untuk satu VM yang menarik dari registry milik sendiri. Dicatat di Production Enhancement (`04`).

### 4.2 Langkah di VM (dijalankan `deploy.sh`)

```text
 0. flock /opt/app/deploy.lock                 ← satu deploy sekaligus di VM, lintas repo
 1. catat PREV_SHA dari /opt/app/<proj>/current_sha
 2. sops -d secrets/<proj>.env.enc > .env.<svc>  (umask 077; file 0600 milik deploy)
 3. docker compose -p <proj> pull  (IMAGE_TAG=<sha>-demo)
 4. guard migrasi: image baru `migrate status --json` → daftar migrasi pending
       pending berlabel *_contract_* ? → hanya boleh jika SEMUA kode yang berjalan sudah ≥ rilis yang berhenti memakai kolom itu
       (aturan: migrasi contract dikirim di rilis TERPISAH setelah rilis expand sudah live; skrip menolak expand+contract dalam satu deploy)
 5. docker compose run --rm migrate up         ← one-shot, role <proj>_owner, timeout 5 mnt
 6. post-migrate check (PayRun): query katalog RLS (Step 4 `07` §2.5) → harus 0 baris
 7. docker compose up -d --no-deps worker       ← worker dulu: graceful shutdown 25 dtk (stop_grace_period 30s)
 8. tunggu worker sehat (heartbeat claim loop di /readyz internal)
 9. docker compose up -d --no-deps api          ← downtime api beberapa detik (diterima V1, lihat 00-index §4)
10. health gate: poll /readyz internal (port internal) DAN https://<host>/api/v1/healthz via Caddy
       30 percobaan × 2 dtk = 60 dtk
11. sukses → tulis current_sha, append releases.log, keluar 0
    gagal  → rollback: IMAGE_TAG=PREV_SHA-demo, ulangi 7–10 (TANPA migrasi), keluar 1
```

**Kenapa rollback aplikasi aman tanpa down-migration:** migrasi expand selalu kompatibel ke belakang (kolom baru nullable, tabel baru, index `CONCURRENTLY`). Kode lama berjalan normal di skema baru. Migrasi contract tidak pernah ikut dalam deploy yang sama dengan expand pasangannya (langkah 4), jadi target rollback (`PREV_SHA`) selalu kompatibel. Down-migration tidak dipakai di environment demo (ADR-013).

### 4.3 Smoke test pasca-deploy

| Project | Langkah | Kenapa aman diulang |
|---|---|---|
| LedgerPay | (1) Login user `smoke-a@…` (akun khusus smoke, saldo awal dari seed). (2) `GET /wallet`. (3) `POST /transfers` Rp1 dari `smoke-a` ke `smoke-b` dengan `Idempotency-Key: smoke-<sha>`. (4) Bila `smoke-a` saldonya habis, transfer arah sebaliknya di run berikutnya (skrip memilih arah berdasarkan saldo). (5) Cek `ledgerpay_ledger_unbalanced_journals == 0` via endpoint metrics internal | Key = SHA: deploy ulang SHA yang sama hanya mendapat replay, jadi tidak ada efek ganda. Membuktikan jalur uang end-to-end setiap deploy |
| PayRun | (1) Login `smoke@demo-tenant`. (2) `GET /me`, `GET /payroll-runs?limit=1`, `GET /employees?limit=1`. (3) Cek `payrun_tenant_isolation_violations_total == 0` | Read-only; tidak mengubah data tenant demo |

Akun smoke dibuat oleh seed `--profile=demo`, berupa user seed biasa dengan email yang sudah dikenal (`smoke-a@…`, `smoke-b@…`). Tidak ada flag khusus di skema; panel dashboard bisnis cukup mengecualikan email `smoke-*` lewat query, dan kredensialnya disimpan di secret environment `demo`.

---

## 5. Mekanisme deploy

| Aspek | Keputusan |
|---|---|
| Koneksi | `ssh` biasa (tanpa action pihak ketiga, mengurangi permukaan supply chain), dengan `StrictHostKeyChecking=yes` dan `known_hosts` dari secret `DEPLOY_KNOWN_HOSTS` (host key di-pin) |
| User VM | `deploy`: bukan root, anggota grup `docker` (risiko ini diterima dan dicatat: akses docker ≈ root; mitigasinya key khusus + forced command) |
| Forced command (Step 12: S5 → Production Enhancement; V1 tanpa forced command) | `authorized_keys`: `command="/opt/app/bin/deploy-gate",no-port-forwarding,no-agent-forwarding,no-pty,no-X11-forwarding ssh-ed25519 …`. `deploy-gate` hanya menerima perintah dari allowlist (`deploy <proj> <sha>`, `rollback <proj> <sha>`, `status <proj>`) dan memvalidasi `<sha>` dengan regex `^[0-9a-f]{40}$` |
| Kunci | Key SSH deploy terpisah per environment, tanpa passphrase (disimpan sebagai environment secret), dirotasi setiap 90 hari (Step 6 `04` §6) |
| Konkurensi | `concurrency: { group: deploy-demo-<proj>, cancel-in-progress: false }` di GitHub (antre, tidak dibatalkan) **ditambah** `flock` di VM (lintas repo, karena dua project + infra berbagi satu VM) |
| Catatan rilis | `/opt/app/<proj>/current_sha`, `/opt/app/<proj>/releases.log` (append: waktu UTC, SHA, actor, hasil, durasi), dan GitHub Deployments API (`environment: demo`, status `success/failure`) agar riwayat terlihat di UI repo |
| Pencarian SHA sebelumnya | `current_sha` sebelum deploy = `PREV_SHA`. Rollback manual bisa memilih SHA mana pun dari `releases.log` yang statusnya `success` |

---

## 6. Workflow rollback

```text
workflow_dispatch(sha, reason)
  guard:
    - sha ada di GHCR (manifest inspect <sha>-demo)  dan  status 'success' di releases.log
    - migrasi head image target ≤ versi skema DB saat ini   (rollback app, bukan DB)
    - tidak ada migrasi *_contract_* yang diterapkan SETELAH rilis target
      (kalau ada → kolom yang dibutuhkan kode lama mungkin sudah hilang → tolak; perlu perbaikan maju/roll-forward)
  deploy: deploy.sh --skip-migrate → langkah 7–11 §4.2
  post-deploy-smoke
  audit: issue otomatis "Rollback <proj> ke <sha>: <reason>" + entri releases.log
```

Rollback skema (down-migration) **tidak** didukung lewat pipeline. Kalau migrasi data salah, perbaikannya adalah *roll-forward* (migrasi baru) atau restore dari backup (runbook di `03`).

---

## 7. Nightly

| Job | Isi | Gagal berarti |
|---|---|---|
| `demo-verify` | Compose efemeral di runner + seed `--profile=demo` + `make demo-verify`: Demo A–D LedgerPay dan A–C PayRun dijalankan headless dengan assertion (Step 8 `02` §8) | Demo live berisiko gagal: perbaiki sebelum interview |
| `property-long` | Property test 1 juta iterasi (journal seimbang, konservasi saldo, `net == Σ lines`, recompute-from-snapshot) | Bug correctness langka |
| `chaos-full` | Seluruh suite chaos, termasuk test karantina (untuk mengumpulkan data flaky) | — |
| `restore-drill` (**repo `infra`, workflow mingguan `restore-drill.yml`**) | Unduh backup terbaru dari object storage (kredensial read-only) → dekripsi age → restore ke Postgres sementara → jalankan invariant checker (LedgerPay: Σ debit = Σ kredit, drift saldo 0; PayRun: Σ item = total run, isolasi katalog) → laporkan ukuran dan durasi restore | Backup tidak bisa dipakai: RPO yang diklaim palsu |
| `perf-smoke` | k6 mini (Step 8 `03` §9): threshold fungsional keras, regresi performa > 30% = peringatan | — |
| `rescan` | `govulncheck`, `osv-scanner`, `trivy image` pada digest yang sedang berjalan di VM (diambil dari `current_sha`) | CVE baru di image live → issue otomatis |
| `traceability` | `TestTraceability_DocsReferenceExistingTests`: setiap nama test yang dirujuk di dokumen benar-benar ada | Dokumen dan kode tidak sinkron |
| `zap-baseline` (P2) | OWASP ZAP baseline terhadap URL demo | Laporan; fail hanya untuk HIGH |

Hasil nightly dikirim sebagai ringkasan job dan, bila gagal, issue otomatis berlabel `nightly-failure` (satu issue per job, diperbarui, bukan diduplikasi).

---

## 8. Cache & kecepatan

| Cache | Mekanisme | Kunci |
|---|---|---|
| Modul Go + build cache | `actions/setup-go` dengan `cache: true` | `go.sum` |
| Cache test Go | Tidak di-cache untuk integration (`-count=1`); unit boleh memakai build cache | — |
| npm | `actions/setup-node` `cache: npm` | `package-lock.json` |
| Docker layer | `docker/build-push-action` `cache-from/to: type=gha,mode=max` | Per Dockerfile stage |
| Image testcontainers | `docker pull postgres:<versi>@sha256:…` sekali per job, lalu dipakai semua package | Digest di-pin |
| Browser Playwright | Cache `~/.cache/ms-playwright` | Versi Playwright |

Target: umpan balik PR **≤ 10 menit**, `main` sampai deploy **≤ 15 menit**. Job yang melampaui target dua kali berturut-turut ditinjau (dipecah, diparalelkan, atau dipindah ke nightly).

---

## 9. Branch protection & rilis

| Aturan | Nilai |
|---|---|
| Branch `main` | Protected: wajib PR, minimal **1 approval** (self-review dengan checklist untuk proyek solo; GitHub tidak mengizinkan approve PR sendiri, jadi aturan ini diganti dengan "require conversation resolution" + checklist PR wajib terisi), tanpa force push, linear history (squash merge) |
| Required checks | `lint`, `unit`, `security-static`, `integration`, `build-local`, `scan-local`, `e2e`, dan `chaos` *jika dijalankan* (memakai job ringkasan `pr-gate` yang menilai hasil kondisional, agar check opsional tidak memblok selamanya) |
| Commit | Conventional Commits (`feat:`, `fix:`, `refactor:`, `test:`, `docs:`, `ci:`), dicek oleh `commitlint` di PR (judul PR, karena squash) |
| Rilis | Tag semver **opsional**, hanya untuk milestone portfolio (`v0.1.0` = Demo A–C siap, `v1.0.0` = Definition of Done Step 10 terpenuhi). Changelog dibuat dari conventional commits dengan `git-cliff` saat tag. Deploy tidak bergantung pada tag |

---

## 10. Secret & permission

| Aspek | Keputusan |
|---|---|
| `GITHUB_TOKEN` | Default `permissions: {}` di level workflow. Per job hanya yang perlu: `contents: read` (semua), `packages: write` (`build-push`), `id-token: write` (hanya jika cosign keyless P2), `deployments: write` (`deploy`), `issues: write` (nightly/rollback), `pull-requests: write` (ringkasan di PR, hanya untuk PR dari repo sendiri) |
| Environment `demo` | `DEPLOY_SSH_KEY`, `DEPLOY_KNOWN_HOSTS`, `DEPLOY_HOST`, `SOPS_AGE_KEY` (private key age untuk mendekripsi `secrets/*.env.enc`), kredensial akun smoke. Hanya bisa dipakai oleh job yang berjalan dari branch `main` |
| Environment `perf` | Token API penyedia VM (scope minimal: create/delete server), SSH key perf |
| Environment `restore-drill` (repo `infra`) | `BACKUP_S3_ACCESS_KEY`/`SECRET` **read-only**, `BACKUP_DRILL_AGE_KEY` (private key **drill**, recipient kedua; key operator tetap offline, Step 9 `03`). Risiko ini diterima karena data demo sintetis, dan bisa dipersempit di V2 dengan menjalankan drill di VM terpisah yang menyimpan key sendiri |
| PR dari fork | `pull_request` (bukan `pull_request_target`), sehingga **tidak ada secret** yang tersedia. Semua job PR dirancang berjalan tanpa secret |
| Registry | Push ke GHCR memakai `GITHUB_TOKEN` (`packages: write`). VM menarik image dengan token read-only (PAT fine-grained `read:packages`, atau paket dibuat publik karena berisi kode open-source portfolio. Keputusan: **paket publik**, tanpa kredensial pull di VM) |
| V2 cloud | OIDC GitHub → peran cloud (AWS/GCP) dengan kredensial jangka pendek, menggantikan SSH key statis |

---

## 11. Skeleton

### 11.1 `.github/workflows/main.yml` (representatif)

```yaml
name: main
on:
  push:
    branches: [main]
permissions: {}
concurrency:
  group: main-${{ github.ref }}
  cancel-in-progress: false

env:
  IMAGE: ghcr.io/${{ github.repository_owner }}/ledgerpay

jobs:
  verify:
    runs-on: ubuntu-24.04
    permissions: { contents: read }
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@<sha>
      - uses: actions/setup-go@<sha>
        with: { go-version-file: go.mod, cache: true }
      - run: make lint            # golangci-lint, sqlc diff, oapi diff, redocly, hadolint
      - run: make test-unit       # go test -race -short
      - run: make security-static # govulncheck, osv-scanner, gitleaks, trivy fs
      - run: make test-integration # testcontainers, -race, migration catalogue, contract

  build-push:
    needs: verify
    runs-on: ubuntu-24.04
    permissions: { contents: read, packages: write }
    outputs:
      digest: ${{ steps.release.outputs.digest }}
    steps:
      - uses: actions/checkout@<sha>
      - uses: docker/setup-buildx-action@<sha>
      - uses: docker/login-action@<sha>
        with: { registry: ghcr.io, username: ${{ github.actor }}, password: ${{ secrets.GITHUB_TOKEN }} }
      - id: release
        uses: docker/build-push-action@<sha>
        with:
          push: true
          tags: ${{ env.IMAGE }}:${{ github.sha }},${{ env.IMAGE }}:main
          build-args: BUILD_TAGS=
          provenance: mode=max
          sbom: true
          cache-from: type=gha,scope=release
          cache-to: type=gha,mode=max,scope=release
      - uses: docker/build-push-action@<sha>
        with:
          push: true
          tags: ${{ env.IMAGE }}:${{ github.sha }}-demo,${{ env.IMAGE }}:main-demo
          build-args: BUILD_TAGS=demo
          provenance: mode=max
          sbom: true
          cache-from: type=gha,scope=demo
          cache-to: type=gha,mode=max,scope=demo

  scan:
    needs: build-push
    runs-on: ubuntu-24.04
    permissions: { contents: read }
    steps:
      - uses: aquasecurity/trivy-action@<sha>
        with:
          image-ref: ${{ env.IMAGE }}:${{ github.sha }}-demo
          severity: CRITICAL,HIGH
          ignore-unfixed: true
          exit-code: "1"

  deploy:
    needs: scan
    runs-on: ubuntu-24.04
    environment: { name: demo, url: https://ledgerpay.example.dev }
    permissions: { contents: read, deployments: write }
    concurrency:
      group: deploy-demo-ledgerpay
      cancel-in-progress: false
    steps:
      - name: Configure SSH
        run: |
          install -m 700 -d ~/.ssh
          printf '%s\n' "${{ secrets.DEPLOY_KNOWN_HOSTS }}" > ~/.ssh/known_hosts
          printf '%s\n' "${{ secrets.DEPLOY_SSH_KEY }}"     > ~/.ssh/id_ed25519
          chmod 600 ~/.ssh/id_ed25519
      - name: Deploy (health gate + auto rollback inside)
        run: ssh -o StrictHostKeyChecking=yes deploy@${{ secrets.DEPLOY_HOST }} "deploy ledgerpay ${{ github.sha }}"

  post-deploy-smoke:
    needs: deploy
    runs-on: ubuntu-24.04
    environment: demo
    permissions: { contents: read }
    steps:
      - uses: actions/checkout@<sha>
      - run: ./scripts/smoke.sh https://ledgerpay.example.dev "${{ github.sha }}"
        env:
          SMOKE_A_PASSWORD: ${{ secrets.SMOKE_A_PASSWORD }}
          SMOKE_B_PASSWORD: ${{ secrets.SMOKE_B_PASSWORD }}
      - name: Rollback on smoke failure
        if: failure()
        run: ssh -o StrictHostKeyChecking=yes deploy@${{ secrets.DEPLOY_HOST }} "rollback ledgerpay previous"
```

> `<sha>` = commit SHA penuh setiap action (C-27). Kunci SSH ditulis ulang di job `post-deploy-smoke` sebelum rollback (dihilangkan dari skeleton agar ringkas).

### 11.2 Kerangka `deploy.sh` (di VM, dipanggil oleh `deploy-gate`)

```bash
#!/usr/bin/env bash
set -euo pipefail
umask 077

PROJ="$1"; SHA="$2"; MODE="${3:-deploy}"          # deploy | rollback
[[ "$PROJ" =~ ^(ledgerpay|payrun)$ ]] || { echo "bad project"; exit 2; }
[[ "$SHA"  =~ ^[0-9a-f]{40}$ ]]       || { echo "bad sha"; exit 2; }

APP_DIR="/opt/app/$PROJ"
exec 9>/opt/app/deploy.lock; flock -w 600 9       # satu deploy sekaligus di VM

PREV_SHA="$(cat "$APP_DIR/current_sha" 2>/dev/null || true)"
START="$(date -u +%s)"
log() { printf '%s %s %s %s\n' "$(date -u +%FT%TZ)" "$PROJ" "$SHA" "$*" >> "$APP_DIR/releases.log"; }

cd "$APP_DIR"
sops -d "secrets/$PROJ.env.enc" > .env.runtime      # 0600 via umask
export IMAGE_TAG="$SHA-demo"
compose() { docker compose -p "$PROJ" --env-file .env.runtime -f compose.yml "$@"; }

compose pull

if [[ "$MODE" == "deploy" ]]; then
  ./bin/migration-guard "$PROJ"                     # tolak expand+contract dalam satu deploy
  compose run --rm migrate up
  [[ "$PROJ" == "payrun" ]] && ./bin/rls-catalogue-check   # harus 0 baris
fi

health_gate() {
  for _ in $(seq 1 30); do
    if compose exec -T api /app/healthcheck --ready \
       && curl -fsS "https://$PROJ.$DOMAIN/api/v1/healthz" >/dev/null; then
      return 0
    fi
    sleep 2
  done
  return 1
}

roll_to() {
  export IMAGE_TAG="$1-demo"
  compose up -d --no-deps worker
  compose up -d --no-deps api
}

compose up -d --no-deps worker
compose exec -T worker /app/healthcheck --ready
compose up -d --no-deps api

if health_gate; then
  echo "$SHA" > "$APP_DIR/current_sha"
  log "success mode=$MODE dur=$(( $(date -u +%s) - START ))s"
  exit 0
fi

log "health_gate_failed mode=$MODE"
if [[ -n "$PREV_SHA" ]]; then
  roll_to "$PREV_SHA"
  health_gate && log "rolled_back to=$PREV_SHA" || log "rollback_failed to=$PREV_SHA"
fi
exit 1
```

> Image distroless tidak punya `curl`/shell, jadi pemeriksaan di dalam container memakai binary kecil `/app/healthcheck` (subcommand dari binary utama) yang memanggil `/readyz` di port internal. Detail port dan Compose ada di `01`.

---

## 12. Ringkasan: MVP vs Production Enhancement (CI/CD)

| Aspek | Portfolio MVP (V1) | Production Enhancement |
|---|---|---|
| Lingkungan | `demo` saja; `perf` efemeral manual | `staging` permanen + promosi image yang sama ke `production` |
| Strategi rilis | Recreate per service + health gate + rollback otomatis | Blue/green atau canary dengan metrik SLO sebagai gate |
| Supply chain | Action di-pin SHA, SBOM + provenance buildx, trivy, govulncheck | + cosign signing & verify, policy admission, SLSA level lebih tinggi |
| Akses deploy | SSH key + forced command + `flock` | OIDC ke cloud, tanpa kredensial statis; deploy lewat orchestrator |
| Approval | Tanpa reviewer manual | Reviewer wajib untuk production, change window |
| DAST | ZAP baseline P2 | DAST terjadwal + pentest berkala |
| Restore drill | Mingguan di runner GitHub (repo `infra`) + manual bulanan dengan key operator | Drill di lingkungan terisolasi dengan key terpisah, laporan RTO/RPO terukur |
