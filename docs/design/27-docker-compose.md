# Step 9.1 — Docker & Docker Compose

> Bagian dari [Step 9](00-index.md). Topologi, keputusan satu VM / satu Postgres / tiga database, dan guard demo (O15) mengikuti `00-index.md`.
> Kontrol yang diwujudkan di sini: C-10 (security headers), C-20 (TLS, jaringan internal, DB tidak dipublish), C-28 (hardening container), C-45 (memori login → batas memori service), dan O21 (port internal, profile `obs`, `stop_grace_period`, role seed perf).
> Potongan Dockerfile, Compose, dan Caddyfile di bawah adalah **kerangka desain** yang representatif, bukan file final. File final ditulis saat coding dengan dokumen ini sebagai acuan.

---

## 1. Image

### 1.1 Daftar image

| Image | Isi | Dipakai oleh service | Build tag |
|---|---|---|---|
| `ghcr.io/<owner>/ledgerpay` | Satu binary Go (`api`, `worker`, `migrate`, `seed`, `healthcheck`) | `ledgerpay-api`, `ledgerpay-worker`, `ledgerpay-migrate` | `release` (default) / `demo` |
| `ghcr.io/<owner>/payrun` | Satu binary Go (subcommand sama) | `payrun-api`, `payrun-worker`, `payrun-migrate` | `release` / `demo` |
| `ghcr.io/<owner>/mock-provider` | Binary Go mock PG + bank (+ halaman checkout server-rendered) | `mock-provider`, `mock-migrate` | — (selalu "demo" secara sifat; tidak pernah dipakai di production nyata) |
| `ghcr.io/<owner>/edge` | **Caddy + aset SPA kedua project** (hasil `vite build`) | `caddy` | — |

Tag: `<git-sha>` (immutable, dipakai deploy) + `main` (bergerak, untuk kenyamanan lokal). Image `demo` diberi suffix tag `-demo` (`<sha>-demo`). Lihat §9 dan Step 9 `02`.

### 1.2 Keputusan: SPA disajikan langsung oleh Caddy

| Opsi | Kelebihan | Kekurangan |
|---|---|---|
| Container nginx terpisah per SPA | Pola umum | Dua container tambahan (±20–40 MB masing-masing), satu hop proxy lagi, konfigurasi header ganda (Caddy + nginx) |
| Volume statis bersama, diisi saat deploy | Tanpa container tambahan | Langkah deploy tambahan (copy ke volume); versi aset dan versi image terpisah, rawan tidak sinkron |
| **Image `edge` = Caddy + aset SPA dibangun di image yang sama** | Satu sumber untuk TLS, header (C-10), routing, dan aset statis; aset dan konfigurasi selalu satu versi (satu tag SHA); tanpa container tambahan | Mengubah SPA berarti me-rebuild image `edge` (murah; build multi-stage ber-cache) |

**Dipilih:** image `edge`. Security header SPA hanya didefinisikan di satu tempat. Rollback SPA sama dengan rollback image `edge` ke SHA sebelumnya. Karena image `edge` memuat aset dari dua repo, image ini dibangun di repo `infra` (Step 9 `02`), yang mengambil artefak `web-dist` dari CI masing-masing repo project.

### 1.3 Dockerfile service Go (kerangka)

```dockerfile
# syntax=docker/dockerfile:1.7
ARG GO_VERSION=1.25
FROM golang:${GO_VERSION}-bookworm@sha256:<digest> AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG BUILD_TAGS=""                       # "" untuk release, "demo" untuk image demo
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -tags "${BUILD_TAGS}" \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/app ./cmd/ledgerpay

FROM gcr.io/distroless/static-debian12:nonroot@sha256:<digest>
ARG VERSION REVISION CREATED
LABEL org.opencontainers.image.source="https://github.com/<owner>/ledgerpay" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.created="${CREATED}"
COPY --from=build /out/app /app
COPY --from=build /src/db/migrations /migrations          # dibaca oleh subcommand migrate
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/app"]
CMD ["api"]
HEALTHCHECK --interval=10s --timeout=2s --retries=3 CMD ["/app", "healthcheck", "--url=http://127.0.0.1:9090/healthz"]
```

| Aturan | Alasan |
|---|---|
| `CGO_ENABLED=0` + `distroless/static` | Binary statis, tanpa libc, tanpa shell, tanpa package manager. Permukaan serangan minimal (C-28) |
| Base image di-pin **by digest** | Build reproducible; perubahan base image lewat PR Dependabot (C-27) |
| `-trimpath`, `-ldflags -X main.version` | Path build lokal tidak bocor ke binary; versi (git SHA) muncul di log, `app_build_info`, dan `/readyz` |
| **Subcommand `healthcheck`** | Distroless tidak punya `curl`/`wget`. Binary yang sama melakukan GET ke `/healthz` port internal dan exit 0/1. Tanpa menambah tool ke image |
| Migrasi ikut di image | Subcommand `migrate` memakai SQL di `/migrations`. Versi migrasi selalu sama dengan versi binary (dicek oleh `/readyz`) |
| Image demo | Build yang sama dengan `BUILD_TAGS=demo`; menambah direktori `/migrations_demo` (§9) |

### 1.4 Dockerfile SPA dan image `edge` (kerangka)

```dockerfile
# Di repo masing-masing project: menghasilkan artefak statis (bukan image runtime)
FROM node:22-bookworm-slim@sha256:<digest> AS web
WORKDIR /web
COPY web/package.json web/yarn.lock ./
RUN --mount=type=cache,target=/usr/local/share/.cache/yarn yarn install --frozen-lockfile
COPY web/ .
ARG VITE_API_BASE=/api/v1
RUN yarn build                       # → /web/dist (tanpa inline script, sesuai CSP C-10)

# Di repo infra: image edge
FROM caddy:2-alpine@sha256:<digest>
COPY Caddyfile /etc/caddy/Caddyfile
COPY --from=ledgerpay-web /web/dist /srv/ledgerpay
COPY --from=payrun-web    /web/dist /srv/payrun
```

Caddy berjalan sebagai user non-root dengan capability `NET_BIND_SERVICE` saja (satu-satunya pengecualian `cap_drop: ALL`, karena harus bind port 80/443).

---

## 2. Struktur file Compose

| File | Isi | Dipakai di |
|---|---|---|
| `compose.yml` | Base: `caddy`, `postgres`, `minio` (+ `minio-init`), `ledgerpay-{migrate,api,worker}`, `payrun-{migrate,api,worker}`, `mock-provider` (+ `mock-migrate`), network, volume, hardening, logging | Semua environment |
| `compose.dev.yml` | Build dari source + hot reload (`air`), port dipublish **hanya ke `127.0.0.1`** (api 8081/8082, Postgres 5432, MinIO console), Caddy dengan TLS internal (`tls internal`) untuk `*.localhost` | Laptop |
| `compose.obs.yml` | Profile `obs` (**V1**): `prometheus`, `grafana`, `node_exporter`. Profile opsional **`obs-full`**: + `loki`, `promtail`, `jaeger`, `alertmanager` (dinyalakan saat demo tracing/log saja). Rule alert tetap dievaluasi Prometheus dan dicek `promtool` di CI (Step 12: R4-6) | Laptop (opsional), VM demo, VM perf |
| `compose.demo.yml` | Image tag `<sha>-demo`, `DEMO_MODE=true`, `DEMO_TOKEN`, `JOB_LEASE_TTL=10s`, `MOCK_ADMIN_ENABLED=true` (admin mock tetap hanya di jaringan `internal`) | VM demo publik, CI E2E, `make demo` |
| `compose.perf.yml` | Profile `perf`: service `seed` (role `payrun_seed` / superuser seed LedgerPay), `k6` runner, `pg_stat_statements` + `auto_explain` aktif, tanpa `DEMO_MODE` | VM perf efemeral |

**Kombinasi per environment**

| Environment | Perintah |
|---|---|
| Laptop (harian) | `docker compose -f compose.yml -f compose.dev.yml up` |
| Laptop + observability | `… -f compose.obs.yml --profile obs up` |
| CI E2E / demo-verify | `-f compose.yml -f compose.demo.yml up --wait` (image hasil build CI) |
| VM demo publik | `-f compose.yml -f compose.demo.yml -f compose.obs.yml --profile obs up -d` (tambah `--profile obs-full` hanya saat demo tracing/log) |
| VM perf | `-f compose.yml -f compose.obs.yml -f compose.perf.yml --profile obs --profile perf up -d` |

`--wait` (Compose v2) menunggu semua healthcheck hijau. Dipakai CI dan deploy sebagai bagian dari health gate (Step 9 `02`).

---

## 3. Service per service

### 3.1 Tabel service (VM demo, 8 GB)

| Service | Image / command | Network | Port | `depends_on` | Healthcheck | Restart | Limit (mem / cpu) |
|---|---|---|---|---|---|---|---|
| `caddy` | `edge` | edge | **80, 443 dipublish** (satu-satunya) | api dua project (`service_started`) | `caddy validate` saat start; admin API dimatikan | `unless-stopped` | 128 MB / 0.5 |
| `postgres` | `postgres:17-bookworm@sha256:<digest>` | internal | 5432 internal | — | `pg_isready -U postgres` | `unless-stopped` | 2 GB / 2.0 |
| `minio` | `minio/minio@sha256:<digest>` | internal | 9000 internal | — | `mc ready local` | `unless-stopped` | 256 MB / 0.5 |
| `minio-init` | `minio/mc` (one-shot) | internal | — | `minio` healthy | — | `no` | 64 MB |
| `ledgerpay-migrate` | `ledgerpay` / `migrate up` | internal | — | `postgres` healthy | — | `no` (one-shot) | 128 MB |
| `ledgerpay-api` | `ledgerpay` / `api` | edge, internal, obs | 8080 (edge), 9090 (internal/obs) | `ledgerpay-migrate` `service_completed_successfully` | `/app healthcheck` → `:9090/healthz` | `unless-stopped` | 512 MB / 1.0 |
| `ledgerpay-worker` | `ledgerpay` / `worker` | internal, obs | 9090 | `ledgerpay-migrate` completed | sama | `unless-stopped` | 384 MB / 0.75 |
| `payrun-migrate` | `payrun` / `migrate up` | internal | — | `postgres` healthy | — | `no` | 128 MB |
| `payrun-api` | `payrun` / `api` | edge, internal, obs | 8080, 9090 | `payrun-migrate` completed | sama | `unless-stopped` | 512 MB / 1.0 |
| `payrun-worker` | `payrun` / `worker` | internal, obs | 9090 | `payrun-migrate` completed, `minio-init` completed | sama | `unless-stopped` | 512 MB / 1.0 (render PDF) |
| `mock-migrate` | `mock-provider` / `migrate up` | internal | — | `postgres` healthy | — | `no` | 64 MB |
| `mock-provider` | `mock-provider` / `serve` | edge (hanya untuk `/checkout/*` lewat Caddy), internal, obs | 8080, 9090 | `mock-migrate` completed | sama | `unless-stopped` | 128 MB / 0.5 |
| `prometheus` | profile obs | obs | 9090 internal | — | `/-/ready` | `unless-stopped` | 512 MB / 0.5 |
| `grafana` | profile obs | obs, edge (opsional) | 3000 internal | `prometheus` | `/api/health` | `unless-stopped` | 256 MB / 0.5 |
| `loki` | profile obs | obs | 3100 internal | — | `/ready` | `unless-stopped` | 512 MB / 0.5 |
| `promtail` | profile obs | obs | — | `loki` | — | `unless-stopped` | 128 MB / 0.25 |
| `jaeger` | profile obs (all-in-one, storage memori/badger) | obs | 4317 OTLP internal, 16686 UI internal | — | `/` | `unless-stopped` | 384 MB / 0.5 |
| `alertmanager` | profile obs | obs | 9093 internal | — | `/-/ready` | `unless-stopped` | 64 MB / 0.1 |

Catatan:
- **Hanya `caddy` yang mempublish port ke host.** Postgres, MinIO, port internal `9090`, Grafana, dan Jaeger tidak dipublish (C-20, C-30). Akses ke Grafana/Jaeger di VM: SSH tunnel (default) atau `grafana.<domain>` dengan login + basic auth Caddy (opsional).
- **Port 8080** = port publik aplikasi (hanya `/api/v1/...`). **Port 9090** = port internal (`/healthz`, `/readyz`, `/metrics`, `/debug/pprof`), hanya di jaringan `internal`/`obs` (Step 8 `01` §2). Port 9090 milik container aplikasi tidak bentrok dengan Prometheus karena berbeda container.
- `ledgerpay-api` **tidak** `depends_on` `mock-provider`. API tetap melayani transfer walaupun provider mati. Degradasi ditangani state machine (Step 8 `01` §2).
- `depends_on: service_healthy` hanya dipakai untuk `postgres`/`minio`. Ketergantungan aplikasi ke migrasi memakai `service_completed_successfully`, jadi app tidak pernah start dengan skema lama (Step 8 `01` §2).
- `restart: unless-stopped` juga menjadi mekanisme pemulihan setelah fault demo `os.Exit(137)` (Step 7 `03`).

### 3.2 Anggaran memori

| Kelompok | VM 8 GB (disarankan) | VM 4 GB (minimum, tanpa Loki/Jaeger) |
|---|---|---|
| Postgres | 2.048 MB | 1.024 MB |
| ledgerpay-api + worker | 896 MB | 640 MB (api 384, worker 256) |
| payrun-api + worker | 1.024 MB | 768 MB (api 384, worker 384) |
| mock-provider | 128 MB | 96 MB |
| MinIO | 256 MB | 192 MB |
| Caddy | 128 MB | 96 MB |
| Prometheus + Grafana + node_exporter (profile `obs`, V1) | 704 MB | 448 MB (retensi 3 hari) |
| Loki + Promtail (profile `obs-full`, opsional) | 640 MB | — (log via `docker logs`) |
| Jaeger + Alertmanager (profile `obs-full`, opsional) | 512 MB | — (dinyalakan saat demo saja) |
| One-shot (migrate, init) | — (sementara) | — |
| **Total limit** | **±6,3 GB** dengan `obs-full`; ±5,2 GB tanpa (Step 12: V1 default tanpa `obs-full`) | **±3,3 GB** |
| Sisa untuk OS + page cache | ±1,7 GB | ±0,7 GB |

Kenapa limit api 384–512 MB cukup: semaphore argon2 (C-45) membatasi hashing ke ±4 × 64 MiB = 256 MiB. Tanpa semaphore, limit ini akan terlampaui oleh ±8 login paralel dan container di-OOM-kill. Limit memori dan C-45 saling melengkapi. Limit memaksa Go runtime menghormati batas (`GOMEMLIMIT` diset ±90% dari limit container).

### 3.3 Hardening (C-28) — berlaku untuk semua service aplikasi

```yaml
x-hardening: &hardening
  read_only: true
  tmpfs: ["/tmp:size=64m,noexec,nosuid"]
  cap_drop: ["ALL"]
  security_opt: ["no-new-privileges:true"]
  user: "65532:65532"                 # nonroot distroless
  pids_limit: 256
  logging:
    driver: json-file
    options: { max-size: "20m", max-file: "5" }

x-app: &app
  <<: *hardening
  restart: unless-stopped
  networks: [internal, obs]
  environment:
    GOMEMLIMIT: "460MiB"
    OTEL_EXPORTER_OTLP_ENDPOINT: "http://jaeger:4317"

services:
  ledgerpay-worker:
    <<: *app
    image: ghcr.io/<owner>/ledgerpay:${LEDGERPAY_TAG}
    command: ["worker"]
    env_file: [./env/ledgerpay.env]
    stop_grace_period: 30s            # graceful shutdown 25 dtk + margin (Step 7 `03`)
    depends_on:
      ledgerpay-migrate: { condition: service_completed_successfully }
    deploy:
      resources: { limits: { memory: 384m, cpus: "0.75" } }
```

| Opsi | Kenapa |
|---|---|
| `read_only: true` + `tmpfs /tmp` | Penyerang yang berhasil RCE tidak bisa menulis binary/skrip ke filesystem container |
| `cap_drop: ALL` | Aplikasi tidak butuh capability Linux apa pun (port 8080/9090 > 1024) |
| `no-new-privileges` | Mencegah eskalasi lewat binary setuid (yang toh tidak ada di distroless) |
| `pids_limit` | Membatasi fork bomb |
| Rotasi log `json-file` | Disk VM tidak penuh oleh log. Loki/Promtail membaca file yang sama |
| `stop_grace_period: 30s` (worker) | Default Docker 10 dtk terlalu pendek untuk graceful shutdown 25 dtk. API memakai 15 dtk (drain HTTP) |

Test `compose_hardening_check` (C-28) mem-parse hasil `docker compose config` dan gagal jika service aplikasi tidak memiliki opsi di atas, atau jika ada service selain `caddy` yang mempublish port.

---

## 4. Postgres

| Aspek | Keputusan |
|---|---|
| Versi | Postgres **17**, di-pin by digest. Mayor upgrade = pekerjaan terencana (dump/restore atau `pg_upgrade`), bukan efek samping pull |
| Volume | Named volume `pgdata`. Tidak dipublish ke host |
| Database | `ledgerpay`, `payrun`, `mockprovider` |
| Init | `/docker-entrypoint-initdb.d/00-roles.sql` (hanya berjalan saat volume kosong) |

### 4.1 Init roles (kerangka)

```sql
-- Dijalankan sekali saat volume kosong. Password dari secret (sops → env) lewat psql -v.
CREATE ROLE ledgerpay_owner  LOGIN PASSWORD :'ledgerpay_owner_pw';
CREATE ROLE ledgerpay_app    LOGIN PASSWORD :'ledgerpay_app_pw';
CREATE ROLE ledgerpay_readonly LOGIN PASSWORD :'ledgerpay_ro_pw';
CREATE DATABASE ledgerpay OWNER ledgerpay_owner;

CREATE ROLE payrun_owner     LOGIN PASSWORD :'payrun_owner_pw';
CREATE ROLE payrun_app       LOGIN PASSWORD :'payrun_app_pw' NOBYPASSRLS;
CREATE ROLE payrun_readonly  LOGIN PASSWORD :'payrun_ro_pw'  NOBYPASSRLS;
CREATE DATABASE payrun OWNER payrun_owner;

CREATE ROLE mock_owner LOGIN PASSWORD :'mock_owner_pw';
CREATE DATABASE mockprovider OWNER mock_owner;

CREATE ROLE backup LOGIN PASSWORD :'backup_pw';           -- pg_dump; read-only (lihat catatan)
REVOKE CONNECT ON DATABASE ledgerpay, payrun, mockprovider FROM PUBLIC;
GRANT CONNECT ON DATABASE ledgerpay TO ledgerpay_app, ledgerpay_readonly, backup;
GRANT CONNECT ON DATABASE payrun    TO payrun_app, payrun_readonly, backup;
-- Grant schema/tabel dilakukan oleh migrasi masing-masing project (Step 4 §6), bukan di sini.
```

- **`payrun_seed` (BYPASSRLS) tidak ada di init ini.** Role itu hanya dibuat oleh `perf/01-seed-role.sql`, yang di-mount khusus oleh `compose.perf.yml` (Step 8 `03`). Test katalog C-36 di VM demo memastikan tidak ada role `BYPASSRLS` selain superuser `postgres`.
- **Role backup dan RLS (dikoreksi saat review):** `FORCE ROW LEVEL SECURITY` berlaku juga untuk **owner** tabel, sehingga `payrun_owner` tetap terkena RLS. pg_dump menyetel `row_security = off`, jadi dump dengan role yang terkena RLS akan **gagal dengan error**. (Kalau dipaksa `--enable-row-security`, hasilnya dump yang terlihat sukses padahal kosong, karena tidak ada `app.tenant_id`.) Karena itu dibuat role khusus **`payrun_backup`**: `LOGIN`, **`BYPASSRLS`**, hanya `SELECT` pada semua tabel + `USAGE` schema, password hanya ada di env container backup. LedgerPay dan mock memakai role `backup` biasa (tanpa RLS). Test: restore drill memverifikasi jumlah baris tabel tenant > 0.
- **Superuser `postgres`** hanya dipakai oleh init dan operasi darurat. Password-nya ada di sops, tidak di env aplikasi.

### 4.2 Konfigurasi (VM 8 GB, Postgres limit 2 GB)

| Parameter | Nilai | Alasan |
|---|---|---|
| `max_connections` | 100 | Σ pool: ledgerpay api 20 + worker 10, payrun api 20 + worker 10, mock 5 = 65; + migrate, backup, readonly, psql darurat = ±75; margin 25 (Step 7 `01` §6) |
| `shared_buffers` | 512 MB | ±25% dari limit container |
| `effective_cache_size` | 1,5 GB | Petunjuk planner (page cache container + host) |
| `work_mem` | 8 MB | 100 koneksi × beberapa node sort; aman terhadap OOM |
| `maintenance_work_mem` | 128 MB | Index build saat migrasi |
| `shared_preload_libraries` | `pg_stat_statements` (+ `auto_explain` hanya di perf) | Profil query (Step 8 `03`) |
| `log_min_duration_statement` | 250 ms | Query lambat masuk log → Loki |
| `log_lock_waits` | on (`deadlock_timeout` 1 s) | Investigasi contention (Step 7) |
| `idle_in_transaction_session_timeout` | 10 s | ADR-011 (diset juga per role) |
| `wal_level` | replica | Siap untuk PITR/replica di V2 |
| Autovacuum `jobs`, `webhook_inbox` | `autovacuum_vacuum_scale_factor = 0.02` per tabel (diset migrasi) | Tabel churn tinggi (Step 4 `04`) |

Untuk VM 4 GB: `shared_buffers` 256 MB, `effective_cache_size` 768 MB, `work_mem` 4 MB, limit container 1 GB.

---

## 5. MinIO (object storage PayRun)

| Aspek | Keputusan |
|---|---|
| Peran | Implementasi lokal S3 API untuk payslip PDF dan file import CSV (ADR-012). **Tidak dipakai LedgerPay** |
| Kredensial | Root user/password dari sops. Aplikasi memakai **access key khusus** dengan policy hanya `s3:GetObject/PutObject` pada bucket `payrun-private`, tanpa `ListBucket` (sesuai T-PR presigned URL: tidak ada listing) |
| Init | `minio-init` (one-shot, `mc`): buat bucket `payrun-private`, set anonymous access `none`, buat user + policy aplikasi, aktifkan lifecycle hapus `imports/` setelah 30 hari |
| Jaringan | `internal` saja. Presigned URL harus bisa diunduh browser, jadi **host di URL presigned** adalah `files.<domain>`, yang di-proxy Caddy **hanya** untuk path `GET /payrun-private/tenants/*` dengan query signature (Caddy tidak menambah auth; signature S3 yang memvalidasi). Alternatif V1 yang lebih sederhana: API me-stream file setelah otorisasi. Lihat catatan di bawah |
| Production | Diganti R2/S3 (ADR-012) dengan konfigurasi env saja (`S3_ENDPOINT`, `S3_REGION`, kredensial); kode tidak berubah |

**Catatan keputusan presigned di V1:** presigned URL ke MinIO melalui Caddy (`files.<domain>`) dipertahankan, karena itu perilaku production sebenarnya (ADR-012, C-40). Caddy mem-proxy ke MinIO **tanpa** mengubah header `Host` yang ikut ditandatangani (`header_up Host {upstream_hostport}` tidak dipakai; MinIO dikonfigurasi `MINIO_DOMAIN`/`MINIO_SERVER_URL=https://files.<domain>` agar signature cocok).

---

## 6. Caddyfile (kerangka)

```caddyfile
{
    admin off
    email ops@<domain>
    servers {
        trusted_proxies static private_ranges   # tidak ada proxy di depan Caddy di V1; X-Forwarded-For dari luar diabaikan
        timeouts { read_header 5s  read_body 15s  write 30s  idle 60s }
    }
}

(security_headers_spa) {
    header {
        Strict-Transport-Security "max-age=31536000; includeSubDomains"
        Content-Security-Policy "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; report-to csp"
        X-Content-Type-Options "nosniff"
        Referrer-Policy "strict-origin-when-cross-origin"
        Permissions-Policy "camera=(), microphone=(), geolocation=(), payment=()"
        X-Frame-Options "DENY"
        Cross-Origin-Opener-Policy "same-origin"
        -Server
    }
}

(api) {
    # Header API (CSP 'none', Referrer no-referrer, Cache-Control no-store) di-set oleh middleware API (C-10).
    request_body { max_size 64KB }
    reverse_proxy {args[0]}:8080 {
        health_uri      /readyz
        health_port     9090
        health_interval 5s
        health_timeout  2s
        lb_try_duration 10s   # tahan request selama api di-recreate → latensi, bukan 502 (Step 9 03)
        lb_try_interval 250ms
    }
}

(json_log) {
    log {
        output stdout
        format json
        # Query string tidak dicatat: format filter menghapus uri query
        format filter { wrap json  fields { request>uri query { delete } } }
    }
}

ledgerpay.<domain> {
    import security_headers_spa
    import json_log
    handle /api/* { import api ledgerpay-api }
    handle {
        root * /srv/ledgerpay
        try_files {path} /index.html          # history fallback SPA
        file_server
    }
}

payrun.<domain> {
    import security_headers_spa
    import json_log
    handle /api/v1/attendance-imports* {      # satu-satunya rute dengan body besar (CSV 5 MB)
        request_body { max_size 5MB }
        reverse_proxy payrun-api:8080
    }
    handle /api/* { import api payrun-api }
    handle {
        root * /srv/payrun
        try_files {path} /index.html
        file_server
    }
}

pay.<domain> {                               # mock-provider: HANYA halaman checkout
    import json_log
    handle /checkout/* { reverse_proxy mock-provider:8080 }
    handle { respond 404 }                    # /v1/* dan /admin/* tidak pernah lewat sini
}

files.<domain> {                             # presigned URL payslip (PayRun)
    import json_log
    @signed {
        method GET
        path /payrun-private/tenants/*
        query X-Amz-Signature=*
    }
    handle @signed { reverse_proxy minio:9000 }
    handle { respond 404 }
}
```

| Aturan | Alasan |
|---|---|
| `admin off` | Admin API Caddy tidak dibutuhkan; reload lewat restart container |
| `trusted_proxies` ketat | Rate limit per IP di aplikasi bergantung pada IP klien yang benar. Header `X-Forwarded-For` dari internet tidak boleh dipercaya (T-SH rate-limit bypass) |
| Batas body per rute | 64 KB default (C-14); 5 MB hanya untuk import CSV |
| `health_uri /readyz` + `health_port 9090` | Upstream yang belum ready (misalnya migrasi belum selesai) tidak menerima trafik. Di V1 single instance: 503 jujur, bukan request yang menggantung |
| Log tanpa query string | Query bisa berisi cursor atau filter. Secret undangan ada di fragment `#…` sehingga tidak pernah sampai ke server (Step 6 T-PR) |
| `pay.<domain>` hanya `/checkout/*` | `/v1/*` (API mock) dan `/admin/*` hanya dapat diakses dari jaringan `internal` (Step 6 T-LP) |
| Rate limiting | Di aplikasi (ADR-007, token bucket per tier). Caddy tidak melakukan rate limit di V1 (modul tambahan = image custom) |

---

## 7. Network

| Network | Anggota | Tujuan |
|---|---|---|
| `edge` | `caddy`, `ledgerpay-api`, `payrun-api`, `mock-provider`, `minio`* , `grafana` (opsional) | Hanya jalur Caddy → upstream HTTP |
| `internal` (`internal: true`, tanpa akses keluar default) | Semua service aplikasi, `postgres`, `minio`, migrate, init | Komunikasi aplikasi ↔ DB/storage, worker ↔ mock-provider |
| `obs` | Service aplikasi (port 9090), `prometheus`, `grafana`, `loki`, `promtail`, `jaeger`, `alertmanager` | Scrape metrik, OTLP, log |
| `egress` | `ledgerpay-worker` | **Satu-satunya** service yang butuh akses internet: mengirim webhook ke URL merchant (dijaga egress guard C-44). Di V1 demo, merchant webhook URL biasanya endpoint lokal/`webhook.site` |

\* `minio` ada di `edge` hanya agar Caddy dapat mem-proxy `files.<domain>` (§5).

**Siapa bisa menjangkau siapa:**
- Internet → hanya `caddy:443` (dan 80 untuk redirect/ACME).
- `postgres` hanya di `internal`: tidak terjangkau dari `edge`, sehingga kompromi Caddy tidak langsung memberi akses DB.
- `mock-provider` `/admin/*`: hanya `internal` (dipanggil oleh endpoint demo api, Step 7 `03`).
- Prometheus menjangkau port 9090 aplikasi lewat `obs`. Port 9090 tidak ada di `edge`.
- `internal: true` membuat worker/api tidak bisa membuka koneksi keluar sembarangan. `ledgerpay-worker` mendapat network `egress` tambahan untuk webhook keluar. Caddy butuh internet untuk ACME (ada di `edge`, yang bukan `internal: true`).

---

## 8. Katalog environment variable

Sumber: **S** = rahasia dari file sops-terdekripsi `env/<service>.env` (0600 di VM, Step 9 `03`); **C** = konfigurasi non-rahasia di `compose.*.yml`; **D** = default di kode (opsional).

### 8.1 Umum (api & worker kedua project)

| Variabel | Wajib | Contoh | Sumber | Keterangan |
|---|---|---|---|---|
| `APP_ENV` | ya | `demo` / `dev` / `perf` | C | Memengaruhi default (misalnya HTTPS-only egress di `live`) |
| `LOG_LEVEL` | tidak | `info` | C/D | `debug` dilarang di `demo` (redaction tetap berlaku) |
| `HTTP_ADDR` / `OPS_ADDR` | tidak | `:8080` / `:9090` | D | Port publik / internal |
| `DATABASE_URL` | ya | `postgres://ledgerpay_app:…@postgres:5432/ledgerpay?sslmode=disable` | **S** | Role `_app`. `sslmode=disable` hanya karena jaringan internal Docker (V2: managed DB → `verify-full`) |
| `MIGRATE_DATABASE_URL` | ya (migrate) | role `_owner` | **S** | Hanya service `*-migrate` |
| `DB_POOL_MAX` / `DB_POOL_ACQUIRE_TIMEOUT` | tidak | `20` / `2s` | C | api 20, worker 10 (Step 7) |
| `DB_LOCK_TIMEOUT` / `DB_STATEMENT_TIMEOUT` | tidak | `2s` / `5s` (api); `5s` / per kind (worker) | D | ADR-011 |
| `JWT_SIGNING_KEYS` | ya | `{"k2026a":"base64…","k2025b":"base64…"}` | **S** | Map `kid → secret`; semua dipakai untuk verifikasi |
| `JWT_ACTIVE_KID` | ya | `k2026a` | C | Kid untuk menandatangani (rotasi Step 6 `04` §6.1) |
| `JWT_ISSUER` / `JWT_AUDIENCE` | ya | `ledgerpay` / `ledgerpay-web` | C | |
| `PASSWORD_HASH_MAX_CONCURRENCY` | tidak | `4` | D | C-45 |
| `RATE_LIMIT_OVERRIDES` | tidak | `money-write=60/m` | C | Hanya untuk perf/demo |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | tidak | `http://jaeger:4317` | C | Kosong = tracing nonaktif |
| `OTEL_TRACES_SAMPLER_ARG` | tidak | `1.0` | C | 100% di demo (Step 8) |
| `GOMEMLIMIT` | ya | `460MiB` | C | ±90% limit container |
| `JOB_LEASE_TTL` | tidak | `30s` (demo: `10s`) | C | Step 7 `03` |
| `JOB_LANES` | tidak | `payout=4,webhook=4,default=2` | C/D | Konkurensi per lane (worker) |
| `DEMO_MODE` / `DEMO_TOKEN` | tidak | `true` / `…` | C / **S** | Hanya berpengaruh di image build tag `demo` (§9). Token dikirim di header `X-Demo-Token` |
| `PROVIDER_HTTP_TIMEOUT` | tidak | `10s` (demo: `3s`) | C | Timeout panggilan ke provider; demo dipercepat agar Demo C tidak menunggu lama |
| `PAYOUT_STATUS_CHECK_FIRST_DELAY` | tidak | `30s` (demo: `5s`) | C | Jeda status check pertama (ADR-L06) |
| `PAYROLL_CHUNK_SIZE` | tidak | `50` (demo: `10`) | C | Ukuran chunk payroll (O5) |

### 8.2 Khusus LedgerPay

| Variabel | Wajib | Sumber | Keterangan |
|---|---|---|---|
| `PROVIDER_BASE_URL` | ya | C | `http://mock-provider:8080` (internal) |
| `PROVIDER_API_KEY` | ya | **S** | Kredensial LedgerPay ke mock |
| `PROVIDER_WEBHOOK_SECRETS` | ya | **S** | Secret HMAC webhook masuk; boleh dua nilai saat rotasi (`v1,v2`) |
| `PROVIDER_CHECKOUT_PUBLIC_URL` | ya | C | `https://pay.<domain>` (untuk `checkout_url` yang dikembalikan ke browser) |
| `MERCHANT_WEBHOOK_SECRET_KEY` | ya | **S** | Kunci AES untuk mengenkripsi `merchants.webhook_secret` di DB |
| `EGRESS_ALLOW_HTTP` | tidak | C | `false` di `live`; `true` di dev agar bisa ke endpoint lokal (C-44) |
| `EGRESS_DENY_CIDRS_EXTRA` | tidak | C | Tambahan daftar blokir |
| `WEBHOOK_DELIVERY_TIMEOUT` | tidak | D | `5s` (Step 5 `03`) |
| `MOCK_ADMIN_URL` / `MOCK_ADMIN_TOKEN` | demo saja | C / **S** | Dipakai endpoint demo (proxy skenario) |

### 8.3 Khusus PayRun

| Variabel | Wajib | Sumber | Keterangan |
|---|---|---|---|
| `PII_KEYRING` | ya | **S** | `{"v1":"base64-32B","v2":"…"}` (ADR-P08) |
| `PII_ACTIVE_KEY_VERSION` | ya | C | Versi untuk enkripsi baru |
| `BLIND_INDEX_KEY` | ya | **S** | Kunci HMAC terpisah dari keyring |
| `S3_ENDPOINT` / `S3_PUBLIC_ENDPOINT` | ya | C | `http://minio:9000` / `https://files.<domain>` (host presigned) |
| `S3_BUCKET` / `S3_REGION` | ya | C | `payrun-private` / `us-east-1` |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | ya | **S** | User MinIO dengan policy terbatas (§5) |
| `PRESIGN_TTL` | tidak | D | `5m` (C-40) |
| `IMPORT_MAX_BYTES` / `IMPORT_MAX_ROWS` | tidak | D | `5MB` / `50000` (C-38) |
| `PAYROLL_CHUNK_SIZE` | tidak | C | `50` (demo: `10`, O5) |

### 8.4 mock-provider

| Variabel | Sumber | Keterangan |
|---|---|---|
| `DATABASE_URL` | **S** | DB `mockprovider` |
| `WEBHOOK_TARGET_URL` | C | `http://ledgerpay-api:8080/api/v1/webhooks/provider` (jaringan internal) |
| `WEBHOOK_SECRET` | **S** | Pasangan `PROVIDER_WEBHOOK_SECRETS` |
| `API_KEYS` | **S** | Kunci yang diterima dari LedgerPay |
| `MOCK_ADMIN_ENABLED` / `MOCK_ADMIN_TOKEN` | C / **S** | `/admin/*` hanya aktif jika keduanya ada (Step 6 T-LP) |
| `PUBLIC_BASE_URL` | C | `https://pay.<domain>` |

Aplikasi memvalidasi semua variabel wajib saat start (fail fast, `caarlos0/env`). Nilai rahasia **tidak pernah** dicetak. `config dump` hanya menampilkan nama + status "set" (C-18).

---

## 9. Guard mode demo (O15) dan `demo_faults` (O18)

### 9.1 Tiga lapis guard

| Lapis | Mekanisme | Efek bila lapis ini saja yang gagal |
|---|---|---|
| **Build** | Kode rute demo dan implementasi `faults.Check` hanya ada di file `//go:build demo`. Build release memakai stub no-op (`//go:build !demo`) | Kode demo **tidak ada** di binary release; env apa pun tidak bisa menghidupkannya |
| **Runtime** | Rute demo hanya didaftarkan jika `DEMO_MODE=true` **dan** `DEMO_TOKEN` tidak kosong (start gagal jika `DEMO_MODE=true` tanpa token) | Image demo yang dijalankan tanpa flag = tanpa endpoint demo |
| **Request** | Header `X-Demo-Token` wajib (constant-time compare) + rate limit **`heavy`** | Endpoint demo tidak bisa dipanggil pengunjung demo publik |

**Penyimpanan token (Step 12: R2-4):** `DEMO_TOKEN` **tidak pernah** dimasukkan ke bundle SPA atau env build frontend. Toolbar demo meminta presenter menempelkan token saat demo; token disimpan hanya di memori tab (variabel JS), tidak di `localStorage`/`sessionStorage`/cookie, dan hilang saat tab ditutup.

**Permukaan endpoint demo (Step 12: S6/R4-17):** hanya `POST/DELETE /api/v1/demo/faults`, `POST /api/v1/demo/reset`, dan `POST /api/v1/demo/payroll-runs/{id}/fast-track` (PayRun). Bukti Demo C diambil dari endpoint ops dan log/metrik, bukan `/demo/evidence`. Skenario mock diatur lewat admin mock melalui SSH tunnel, bukan proxy `/demo/scenarios`.

Test:
- `TestBuild_DemoEndpointsAbsentWithoutDemoMode`: binary release + `DEMO_MODE=true` → `/api/v1/demo/*` 404, dan simbol paket `demo` tidak ada di binary (`go tool nm`).
- `TestBuild_NoTesthooksInRelease`: sama untuk build tag `testhooks` (Step 7).

### 9.2 Migrasi demo dan tabel `demo_faults`

- Direktori terpisah: `db/migrations_demo/`, dengan tabel versi goose terpisah (`goose_demo_version`). Subcommand `migrate up` di image demo menjalankan `migrations/` **lalu** `migrations_demo/`. Image release tidak memuat direktorinya (Dockerfile menyalin hanya jika `BUILD_TAGS=demo`).
- Demikian tabel ini **tidak pernah** ada di database release, dan skema release tetap sama persis dengan skema yang dites.

| Kolom | Tipe | Null | Default | Keterangan |
|---|---|---|---|---|
| `id` | `uuid` | N | — | PK (UUIDv7) |
| `point` | `text` | N | — | Titik injeksi, contoh `payout.after_bank_call`, `payroll.calc_chunk.before_commit` (Step 7 `03`) |
| `target_kind` | `text` | Y | — | Batasi ke job kind tertentu (opsional) |
| `tenant_id` | `uuid` | Y | — | PayRun: hanya untuk tenant demo tertentu (opsional) |
| `after_n` | `int` | N | `0` | Lewati N pemanggilan pertama (PayRun: mati di chunk ke-`after_n+1`) |
| `times` | `int` | N | `1` | Berapa kali fault terpicu sebelum habis |
| `fired` | `int` | N | `0` | Counter terpicu |
| `action` | `text` | N | `'exit'` | `exit` (`os.Exit(137)`), `panic`, `delay` |
| `delay_ms` | `int` | Y | — | Untuk `action = 'delay'` |
| `expires_at` | `timestamptz` | N | `now() + interval '15 minutes'` | Fault tidak tertinggal tanpa sengaja |
| `created_at` | `timestamptz` | N | `now()` | |

Constraint: `ck_demo_faults_action CHECK (action IN ('exit','panic','delay'))`, `ck_demo_faults_counts CHECK (after_n >= 0 AND times >= 1 AND fired >= 0)`, `ck_demo_faults_delay CHECK ((action = 'delay') = (delay_ms IS NOT NULL))`. `faults.Check` mengambil fault yang cocok dengan `UPDATE … SET fired = fired + 1 WHERE … AND fired < after_n + times AND expires_at > now() RETURNING …` (atomik, aman untuk beberapa worker). Fault terpicu hanya jika `fired` baru > `after_n`.

---

## 10. Workflow developer lokal

| Perintah | Isi | Waktu (perkiraan) |
|---|---|---|
| `make up` | `compose.yml + compose.dev.yml up -d --wait` (build dari source, migrasi otomatis) | Pertama kali ±4–6 menit (pull + build + cache modul); berikutnya ±20 dtk |
| `make dev` | Seperti `up`, dengan `air` hot reload untuk api/worker dan `vite dev` untuk web | ±20 dtk |
| `make obs` | Tambah profile `obs` (Grafana di `https://grafana.localhost`, Jaeger UI) | +±30 dtk |
| `make demo` | Image demo + `compose.demo.yml` + seed demo (`seed --profile demo`: user Rina/Andi, merchant Budi, Tenant Alpha/Beta dengan 100 karyawan) | ±1 menit |
| `make seed` | Seed demo saja (idempoten: `seed` memakai natural key) | ±10 dtk |
| `make test` | Unit test (`go test -race ./...` tanpa tag integration) | < 30 dtk |
| `make test-integration` | testcontainers (tidak butuh compose berjalan) | < 5 menit |
| `make e2e` | `compose.demo.yml up --wait` + Playwright | ±5 menit |
| `make demo-verify` | Script Demo A–D / A–C headless (Step 8 `02`) | ±3 menit |
| `make reset` | `docker compose down -v` (hapus volume `pgdata`, `minio`) + `make up` + `make seed` | ±1 menit |
| `make logs s=ledgerpay-worker` | `docker compose logs -f` dengan filter `jq` untuk JSON | — |

Prasyarat laptop: Docker Engine/Desktop dengan Compose v2, Go 1.25, Node 22 + yarn, `make`, `sops` + `age` (hanya untuk `make up` dengan secret demo; dev memakai `env/*.dev.env` berisi nilai dummy yang boleh di-commit).

Catatan:
- File `env/*.dev.env` berisi **nilai dummy** khusus dev (kunci acak yang hanya berlaku di laptop). Commit nilai dummy diizinkan dan gitleaks dikonfigurasi dengan allowlist untuk path itu. Nilai demo/production selalu dari sops (Step 9 `03`).
- Port yang dipublish di dev hanya bind ke `127.0.0.1`, tidak ke `0.0.0.0`, agar DB dev tidak terbuka di jaringan kafe/kantor.
