#!/usr/bin/env bash
# Deploy satu SHA PayRun ke VM bersama — dari laptop (manual) atau dari CI:
#   deploy/prod/deploy.sh <host|local> <git-sha> [--skip-migrate]
# - konfigurasi (compose, site Caddy, init SQL) dikirim dengan `git archive <sha>`;
# - env file dari deploy/prod/env/ disalin ke /srv/payrun/env (0600), KECUALI PR_SKIP_ENV=1 (CI:
#   secret sudah ada di VM sejak deploy pertama dan tidak pernah disimpan di GitHub);
# - PR_IMAGE_PREFIX=ghcr.io/<owner>/ → VM mem-pull image yang sudah dibangun + dipindai CI;
#   PR_REGISTRY_TOKEN (opsional) → docker login sementara di VM, logout setelah deploy;
# - remote-deploy.sh di VM: build/pull, migrate, site edge, smoke, rollback otomatis.
# HOST=local menjalankan semuanya di mesin ini (uji lokal, PR_ROOT ke folder sementara).
set -euo pipefail
HOST=${1:?usage: deploy.sh <host|local> <git-sha> [--skip-migrate]}
SHA=$(git rev-parse --verify "${2:?git sha}^{commit}")
SKIP_MIGRATE=${3:-}
case "$SKIP_MIGRATE" in ""|--skip-migrate) ;; *) echo "unknown option $SKIP_MIGRATE"; exit 2 ;; esac
ENV_DIR=${ENV_DIR:-$(dirname "$0")/env}
ROOT=${PR_ROOT:-/srv/payrun}
USER_AT=${DEPLOY_USER:-deploy}@${HOST}
[ -n "${PR_SKIP_ENV:-}" ] || [ -f "$ENV_DIR/api.env" ] || { echo "missing $ENV_DIR — run deploy/prod/init-env.sh first"; exit 1; }
git diff --quiet "$SHA" -- deploy/ db/ cmd/ internal/ web/ 2>/dev/null || echo "note: working tree differs from $SHA; deploying the COMMITTED $SHA"

run() { if [ "$HOST" = local ]; then bash -c "$1"; else ssh -o BatchMode=yes "$USER_AT" "$1"; fi; }
echo "== upload release $SHA"
if [ "$HOST" = local ]; then
  mkdir -p "$ROOT/releases/$SHA" && git archive "$SHA" | tar -x -C "$ROOT/releases/$SHA"
  [ -n "${PR_SKIP_ENV:-}" ] || { mkdir -p "$ROOT/env" && install -m 600 "$ENV_DIR"/*.env "$ENV_DIR"/s3.json "$ROOT/env/"; }
else
  git archive "$SHA" | ssh -o BatchMode=yes "$USER_AT" "mkdir -p $ROOT/releases/$SHA && tar -x -C $ROOT/releases/$SHA"
  if [ -z "${PR_SKIP_ENV:-}" ]; then
    ssh -o BatchMode=yes "$USER_AT" "mkdir -p $ROOT/env && chmod 700 $ROOT/env"
    scp -q "$ENV_DIR"/*.env "$ENV_DIR"/s3.json "$USER_AT:$ROOT/env/" && ssh -o BatchMode=yes "$USER_AT" "chmod 600 $ROOT/env/*"
  fi
fi
REGISTRY=${PR_IMAGE_PREFIX%%/*}
if [ -n "${PR_REGISTRY_TOKEN:-}" ] && [ -n "$REGISTRY" ]; then
  printf %s "$PR_REGISTRY_TOKEN" | run "docker login $REGISTRY -u ${PR_REGISTRY_USER:-ci} --password-stdin >/dev/null"
  trap 'run "docker logout $REGISTRY >/dev/null" || true' EXIT
fi
echo "== remote deploy"
# Setiap nilai di-quote (%q) sebelum masuk string perintah remote (pelajaran deploy LedgerPay).
q() { printf '%q' "$1"; }
run "cd $(q "$ROOT/releases/$SHA") && PR_ROOT=$(q "$ROOT") PR_IMAGE_PREFIX=$(q "${PR_IMAGE_PREFIX:-}") \
  PR_DEPLOY_ACTOR=$(q "${PR_DEPLOY_ACTOR:-manual}") PR_HTTPS_PUBLIC_PORT=$(q "${PR_HTTPS_PUBLIC_PORT:-443}") \
  PR_EDGE_CONTAINER=$(q "${PR_EDGE_CONTAINER:-ledgerpay-prod-edge-1}") PR_EDGE_SITES=$(q "${PR_EDGE_SITES:-/srv/edge/sites}") \
  PR_DEPLOY_LOCK=$(q "${PR_DEPLOY_LOCK:-/srv/deploy.lock}") PR_SMOKE_INSECURE=$(q "${PR_SMOKE_INSECURE:-}") \
  bash deploy/prod/remote-deploy.sh $(q "$SHA") $SKIP_MIGRATE"
