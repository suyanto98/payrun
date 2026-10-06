# PayRun — developer commands. `make help` untuk daftar target.

GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

COMPOSE := docker compose -f deploy/compose.yml
REDOCLY := npx --yes @redocly/cli@1

.PHONY: help tools up up-storage down down-volumes logs ps migrate build test test-integration lint lint-go lint-api lint-actions lint-shell gen gen-check security cover

help: ## Tampilkan daftar target
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

tools: ## Pasang toolchain Go (sqlc, goose, golangci-lint, actionlint, govulncheck) — versi sama dengan LedgerPay
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
	go install github.com/pressly/goose/v3/cmd/goose@v3.28.0
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
	go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
	go install golang.org/x/vuln/cmd/govulncheck@v1.1.4

# ------------------------------------------------------------------ local stack
up: ## Jalankan postgres + migrate + api (build ulang image), tunggu sampai /readyz hijau
	$(COMPOSE) up -d --build --wait api

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

security: ## govulncheck + gitleaks (seluruh history)
	govulncheck ./...
	docker run --rm -v "$$PWD:/repo" zricethezav/gitleaks:v8.28.0 git /repo --log-opts=--all --redact

gen: ## Generate kode dari SQL (sqlc) — aktif mulai Sprint 1 (belum ada query)
	sqlc generate

gen-check: gen ## Pastikan kode hasil generate sudah di-commit (dipakai CI)
	git diff --exit-code -- internal/
