# PayRun — developer commands. `make help` untuk daftar target.

GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

COMPOSE := docker compose -f deploy/compose.yml
REDOCLY := npx --yes @redocly/cli@1
# trivy fs (C-26, docs/design/28-cicd.md job security-static): vuln dependency + misconfig (Dockerfile,
# compose, workflow). Gagal hanya untuk CRITICAL/HIGH yang sudah punya fix. Versi sama dengan LedgerPay.
TRIVY_FS := docker run --rm -v "$$PWD:/src:ro" aquasec/trivy:0.65.0 fs --no-progress --scanners vuln,misconfig \
	--severity CRITICAL,HIGH --ignore-unfixed --exit-code 1 --skip-dirs docs /src

.PHONY: help tools api-docs up up-demo seed-demo demo-live demo-down up-storage perf-up perf-seed perf-run perf-down up-obs up-obs-full dashboards test-alerts backup-dev restore-drill test-demo images down down-volumes logs ps migrate build test test-integration demo-verify lint lint-go lint-api lint-actions lint-shell gen gen-api gen-sql gen-check security trivy-fs cover web-dev web-check

help: ## Tampilkan daftar target
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

tools: ## Pasang toolchain Go (sqlc, goose, oapi-codegen, golangci-lint, actionlint, govulncheck)
	go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
	go install github.com/pressly/goose/v3/cmd/goose@v3.28.0
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
	go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
	go install golang.org/x/vuln/cmd/govulncheck@v1.1.4

# ------------------------------------------------------------------ local stack
up: up-storage ## Jalankan storage + postgres + migrate + api + worker (build ulang image), tunggu sampai /readyz hijau
	$(COMPOSE) up -d --build --wait api worker

up-storage: ## Object storage S3 (SeaweedFS) + buat bucket private payrun-private
	$(COMPOSE) --profile storage up -d --wait storage
	$(COMPOSE) --profile storage run --rm storage-init

# Perf env: project terpisah + port sendiri (stack dev boleh tetap jalan). Baseline mesin ini, bukan klaim kapasitas.
PERF := PR_HTTP_PORT=8091 PR_OPS_PORT=9290 PR_WORKER_OPS_PORT=9291 PR_PG_PORT=55443 PR_MAILPIT_UI_PORT=8035 PR_S3_PORT=8343 \
	docker compose -p payrun-perf -f deploy/compose.yml -f deploy/perf/compose.perf.yml

# Demo lokal: project terpisah, port sendiri, data sendiri (docs/demo-script.md). SPA: PR_HTTP_PORT=8082 PR_WEB_PORT=5176 make web-dev.
DEMO := PR_HTTP_PORT=8082 PR_OPS_PORT=9192 PR_WORKER_OPS_PORT=9193 PR_PG_PORT=55435 PR_MAILPIT_UI_PORT=8026 PR_S3_PORT=8335 PR_WEB_PORT=5176 \
	docker compose -p payrun-demo -f deploy/compose.yml -f deploy/compose.demo.yml

perf-up: ## Perf env lokal (pg_stat_statements, pprof, rate limit off); PR_PERF_WORKERS/PR_PERF_CHUNK untuk PR-1
	$(PERF) up -d --build --wait api worker

perf-seed: ## Seed 1.000 tenant / 100.000 karyawan + gate invariant; PROFILE=smoke untuk kecil
	$(PERF) --profile seed run --build --rm seed $${PROFILE:-perf}

perf-run: ## Satu skenario: make perf-run S=pr1|pr3|pr4 (bukti di perf/results/)
	./perf/run.sh $(S)

perf-down: ## Hentikan perf env DAN hapus datanya
	$(PERF) --profile seed down -v

up-obs: up-storage ## Stack + Prometheus + Grafana (Grafana http://localhost:3001, Prometheus http://localhost:9098)
	$(COMPOSE) --profile obs up -d --build --wait api worker prometheus grafana

up-obs-full: up-storage ## up-obs + Jaeger; trace api/worker dikirim lewat OTLP (Jaeger http://localhost:16687)
	PR_OTLP_ENDPOINT=http://jaeger:4318/v1/traces $(COMPOSE) --profile obs-full up -d --build --wait api worker prometheus grafana jaeger

dashboards: ## Generate JSON dashboard Grafana dari deploy/grafana/gen_dashboards.py
	python3 deploy/grafana/gen_dashboards.py

test-alerts: ## Validasi + unit test alert rules Prometheus (promtool, termasuk A-13 sintetis)
	docker run --rm -v "$$PWD/deploy/prometheus/rules:/r:ro" -w /r --entrypoint promtool prom/prometheus:v3.5.0 check rules payrun.yml platform.yml
	docker run --rm -v "$$PWD/deploy/prometheus:/p:ro" -w /p/tests --entrypoint promtool prom/prometheus:v3.5.0 test rules payrun_test.yml
	docker run --rm -v "$$PWD/deploy/prometheus:/p:ro" -w /p --entrypoint promtool prom/prometheus:v3.5.0 check config prometheus.yml

down: ## Hentikan stack (data tetap di volume)
	$(COMPOSE) --profile storage --profile obs-full down

down-volumes: ## Hentikan stack DAN hapus data postgres + storage
	$(COMPOSE) --profile storage --profile obs-full down -v

logs: ## Ikuti log api
	$(COMPOSE) logs -f api

ps: ## Status container
	$(COMPOSE) ps

migrate: ## Jalankan migrasi lagi (one-shot container, role owner)
	$(COMPOSE) run --rm migrate

# ------------------------------------------------------------------ build & test
build: ## Build binary ke bin/payrun
	cd backend && CGO_ENABLED=0 go build -trimpath -o ../bin/payrun ./cmd/payrun

test: ## Unit test (tanpa Docker), dengan race detector
	cd backend && go test -race -short ./...

test-integration: ## Integration test (butuh Docker), termasuk test interleaving (build tag testhooks)
	cd backend && go test -race -count=1 -tags testhooks ./...

demo-verify: ## Demo A (isolasi tenant), Demo B (crash & resume + run ganda), Demo C (final immutable, adjustment ke bulan berikutnya, recompute dari snapshot) secara headless
	cd backend && go test -count=1 -tags testhooks -v -run 'TestTenantIsolation_DemoA|TestPayrollRun_CrashAndResume|TestRecompute_FromSnapshotMatchesStoredResults|TestFinalizedRun_DirectUpdateRejected|TestDemoC_FinalizedPayrollIsImmutable' ./internal/app/

up-demo: ## Stack demo lokal (project payrun-demo, API :8082, data terpisah dari dev): build tag demo + DEMO_MODE; lalu make seed-demo
	$(DEMO) --profile storage up -d --wait storage
	$(DEMO) --profile storage run --rm storage-init
	$(DEMO) up -d --build --wait api worker

seed-demo: ## Dua tenant demo (seed-0: 100 karyawan, seed-1: 10–50) ke DB demo yang KOSONG; password app.SeedPassword
	$(DEMO) --profile seed run --rm seed demo

demo-live: ## Demo A–C live dengan asersi terhadap stack demo (S=a|b|c|all; data sekali pakai)
	./scripts/demo-live.sh $${S:-all}

demo-down: ## Hentikan stack demo DAN hapus datanya (dev stack tidak tersentuh)
	$(DEMO) --profile seed --profile storage down -v

test-demo: ## Test build tag demo (guard endpoint /demo/*, fault sekali pakai di chunk ke-N)
	cd backend && go test -count=1 -tags demo -run 'TestDemo_' ./internal/app/

backup-dev: ## Backup terenkripsi DB dev ke build/backups (key drill di build/backups/drill.agekey)
	mkdir -p build/backups && chmod 777 build/backups
	docker build -q -f deploy/backup/Dockerfile -t payrun-backup:local . >/dev/null
	[ -f build/backups/drill.agekey ] || docker run --rm --entrypoint age-keygen payrun-backup:local > build/backups/drill.agekey 2>/dev/null
	docker run --rm --network payrun_default -e PGHOST=postgres -e PGUSER=payrun_backup -e PGPASSWORD=backup-dev \
	  -e PR_BACKUP_AGE_RECIPIENTS="$$(grep -o 'age1[0-9a-z]*' build/backups/drill.agekey)" -v "$$PWD/build/backups:/backups" payrun-backup:local

restore-drill: ## Restore drill backup dev terbaru (Postgres sekali pakai → migrate → tabel > 0 → verify-run)
	PR_BACKUP_IMAGE=payrun-backup:local scripts/restore-drill.sh "$$(ls build/backups/daily/*.dump.age | tail -1)" build/backups/drill.agekey

images: ## Bangun image rilis lokal (app demo, web, backup) untuk SHA HEAD
	scripts/build-images.sh "" "$$(git rev-parse HEAD)"

cover: ## Laporan coverage (indikatif, bukan gate) → build/coverage.html
	mkdir -p build && cd backend && go test -tags testhooks -coverprofile=../build/coverage.out ./internal/... >/dev/null && \
	  go tool cover -func=../build/coverage.out | tail -1 && go tool cover -html=../build/coverage.out -o ../build/coverage.html

# ------------------------------------------------------------------ lint, codegen, security
lint: lint-go lint-api lint-actions lint-shell ## Semua lint

lint-go: ## golangci-lint (gosec, depguard, ...)
	cd backend && golangci-lint run ./...

lint-shell: ## shellcheck skrip deploy
	docker run --rm -v "$$PWD:/w:ro" -w /w koalaman/shellcheck:stable -x -S warning deploy/postgres/init/*.sh deploy/backup/*.sh deploy/prod/*.sh scripts/*.sh perf/*.sh

api-docs: ## Dokumentasi API statis dari backend/api/openapi.yaml → build/api.html
	mkdir -p build && cd backend && $(REDOCLY) build-docs api/openapi.yaml -o ../build/api.html

lint-api: ## Lint kontrak OpenAPI
	cd backend && $(REDOCLY) lint api/openapi.yaml

lint-actions: ## Lint workflow GitHub Actions
	actionlint

security: ## govulncheck + gitleaks (seluruh history) + trivy fs — sama dengan job CI `security`
	cd backend && govulncheck ./...
	docker run --rm -v "$$PWD:/repo" zricethezav/gitleaks:v8.28.0 git /repo --log-opts=--all --redact --config /repo/.gitleaks.toml
	$(MAKE) trivy-fs

trivy-fs: ## trivy fs: vuln dependency + misconfig (Dockerfile, compose, workflow)
	$(TRIVY_FS)

gen: gen-api gen-sql ## Semua codegen (OpenAPI + SQL)

gen-api: ## Generate types Go dari backend/api/openapi.yaml (oapi-codegen) → backend/api/gen
	cd backend && oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml

gen-sql: ## Generate kode dari SQL (sqlc) → backend/internal/modules/*/store
	cd backend && sqlc generate

gen-check: gen ## Pastikan kode hasil generate (oapi-codegen + sqlc) sudah di-commit (dipakai CI)
	@# git diff saja tidak melihat file BARU yang belum di-commit; status --porcelain melihat keduanya.
	@test -z "$$(git status --porcelain -- backend/api/gen backend/internal/)" || { git status --porcelain -- backend/api/gen backend/internal/; git diff -- backend/api/gen backend/internal/; echo "kode generate tidak sinkron: jalankan make gen-api dan commit"; exit 1; }

# ------------------------------------------------------------------ frontend (SPA, frontend/)
web-dev: ## SPA dev server (proxy /api ke API lokal); PR_WEB_PORT harus sama dengan saat `make up`
	cd frontend && yarn install --frozen-lockfile && yarn dev

web-check: ## Typecheck + unit test + build SPA (sama dengan job CI frontend)
	cd frontend && yarn install --frozen-lockfile && yarn typecheck && yarn test:unit && yarn build

