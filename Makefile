# PayRun — developer commands. `make help` untuk daftar target.

GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

COMPOSE := docker compose -f deploy/compose.yml
REDOCLY := npx --yes @redocly/cli@1
# trivy fs (C-26, docs/design/28-cicd.md job security-static): vuln dependency + misconfig (Dockerfile,
# compose, workflow). Gagal hanya untuk CRITICAL/HIGH yang sudah punya fix. Versi sama dengan LedgerPay.
TRIVY_FS := docker run --rm -v "$$PWD:/src:ro" aquasec/trivy:0.65.0 fs --no-progress --scanners vuln,misconfig \
	--severity CRITICAL,HIGH --ignore-unfixed --exit-code 1 --skip-dirs docs /src

.PHONY: help tools up up-storage down down-volumes logs ps migrate build test test-integration demo-verify lint lint-go lint-api lint-actions lint-shell gen gen-api gen-sql gen-check security trivy-fs cover web-dev web-check

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
up: ## Jalankan postgres + migrate + api + worker (build ulang image), tunggu sampai /readyz hijau
	$(COMPOSE) up -d --build --wait api worker

up-storage: ## Object storage S3 (SeaweedFS) + buat bucket private payrun-private
	$(COMPOSE) --profile storage up -d --wait storage
	$(COMPOSE) --profile storage run --rm storage-init

down: ## Hentikan stack (data tetap di volume)
	$(COMPOSE) --profile storage down

down-volumes: ## Hentikan stack DAN hapus data postgres + storage
	$(COMPOSE) --profile storage down -v

logs: ## Ikuti log api
	$(COMPOSE) logs -f api

ps: ## Status container
	$(COMPOSE) ps

migrate: ## Jalankan migrasi lagi (one-shot container, role owner)
	$(COMPOSE) run --rm migrate

# ------------------------------------------------------------------ build & test
build: ## Build binary ke bin/payrun
	CGO_ENABLED=0 go build -trimpath -o bin/payrun ./cmd/payrun

test: ## Unit test (tanpa Docker), dengan race detector
	go test -race -short ./...

test-integration: ## Integration test (butuh Docker), termasuk test interleaving (build tag testhooks)
	go test -race -count=1 -tags testhooks ./...

demo-verify: ## Demo A (isolasi tenant), Demo B (crash & resume + run ganda), Demo C (final immutable + recompute dari snapshot) secara headless
	go test -count=1 -tags testhooks -v -run 'TestTenantIsolation_DemoA|TestPayrollRun_CrashAndResume|TestRecompute_FromSnapshotMatchesStoredResults|TestFinalizedRun_DirectUpdateRejected|TestDemoC_FinalizedPayrollIsImmutable' ./internal/app/

cover: ## Laporan coverage (indikatif, bukan gate) → build/coverage.html
	mkdir -p build && go test -tags testhooks -coverprofile=build/coverage.out ./internal/... >/dev/null && \
	  go tool cover -func=build/coverage.out | tail -1 && go tool cover -html=build/coverage.out -o build/coverage.html

# ------------------------------------------------------------------ lint, codegen, security
lint: lint-go lint-api lint-actions lint-shell ## Semua lint

lint-go: ## golangci-lint (gosec, depguard, ...)
	golangci-lint run ./...

lint-shell: ## shellcheck skrip deploy
	docker run --rm -v "$$PWD:/w:ro" -w /w koalaman/shellcheck:stable -x -S warning deploy/postgres/init/*.sh

lint-api: ## Lint kontrak OpenAPI
	$(REDOCLY) lint api/openapi.yaml

lint-actions: ## Lint workflow GitHub Actions
	actionlint

security: ## govulncheck + gitleaks (seluruh history) + trivy fs — sama dengan job CI `security`
	govulncheck ./...
	docker run --rm -v "$$PWD:/repo" zricethezav/gitleaks:v8.28.0 git /repo --log-opts=--all --redact --config /repo/.gitleaks.toml
	$(MAKE) trivy-fs

trivy-fs: ## trivy fs: vuln dependency + misconfig (Dockerfile, compose, workflow)
	$(TRIVY_FS)

gen: gen-api gen-sql ## Semua codegen (OpenAPI + SQL)

gen-api: ## Generate types Go dari api/openapi.yaml (oapi-codegen) → api/gen
	oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml

gen-sql: ## Generate kode dari SQL (sqlc) → internal/modules/*/store
	sqlc generate

gen-check: gen ## Pastikan kode hasil generate (oapi-codegen + sqlc) sudah di-commit (dipakai CI)
	@# git diff saja tidak melihat file BARU yang belum di-commit; status --porcelain melihat keduanya.
	@test -z "$$(git status --porcelain -- api/gen internal/)" || { git status --porcelain -- api/gen internal/; git diff -- api/gen internal/; echo "kode generate tidak sinkron: jalankan make gen-api dan commit"; exit 1; }

# ------------------------------------------------------------------ web (SPA, web/)
web-dev: ## SPA dev server (proxy /api ke API lokal); PR_WEB_PORT harus sama dengan saat `make up`
	cd web && yarn install --frozen-lockfile && yarn dev

web-check: ## Typecheck + unit test + build SPA (sama dengan job CI frontend)
	cd web && yarn install --frozen-lockfile && yarn typecheck && yarn test:unit && yarn build

