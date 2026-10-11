#!/usr/bin/env bash
# Dijalankan DI VM oleh deploy.sh, dari folder rilis /srv/payrun/releases/<sha>.
# Recreate per service dengan health gate (docs/design/29-deploy-migrations-backup.md §6.1):
#   build/pull → migrate (gagal = berhenti, app lama tetap jalan) → worker → api → web → site edge
#   → smoke read-only lewat edge (TLS) → sukses: current = rilis ini; gagal: rilis sebelumnya dinyalakan lagi.
#
#   remote-deploy.sh <sha> [--skip-migrate]
#   PR_IMAGE_PREFIX kosong  → image dibangun di VM (deploy manual dari laptop)
#   PR_IMAGE_PREFIX=ghcr.io/<owner>/ → image yang SUDAH dibangun + dipindai CI di-pull
#   --skip-migrate          → rollback aplikasi ke rilis lama (skema tidak pernah diturunkan, expand/contract)
#
# Edge BERSAMA: TLS dilayani Caddy stack LedgerPay (container $PR_EDGE_CONTAINER). Site payrun.caddy
# disalin ke $PR_EDGE_SITES lalu `caddy reload` (graceful, LedgerPay tidak terputus).
set -euo pipefail
SHA=${1:?sha}
SKIP_MIGRATE=${2:-}
ROOT=${PR_ROOT:-/srv/payrun}
EDGE=${PR_EDGE_CONTAINER:-ledgerpay-prod-edge-1}
SITES=${PR_EDGE_SITES:-/srv/edge/sites}
export PR_ENV_DIR=$ROOT/env PR_VERSION=$SHA PR_IMAGE_PREFIX=${PR_IMAGE_PREFIX:-}
# Build arg SPA (VITE_REPO_URL, VITE_CONTACT_URL, VITE_VIDEO_URL): opsional, bukan secret.
if [ -f "$PR_ENV_DIR/build.env" ]; then set -a; . "$PR_ENV_DIR/build.env"; set +a; fi
# Satu deploy pada satu waktu di VM INI, lintas project (lock bersama dengan deploy LedgerPay): dua
# deploy yang membangun image bersamaan di VM 8 GB = OOM, dan keduanya me-reload edge yang sama.
exec 9>"${PR_DEPLOY_LOCK:-/srv/deploy.lock}"
flock -n 9 || { echo "another deploy is running (lock ${PR_DEPLOY_LOCK:-/srv/deploy.lock})"; exit 1; }
started=$(date +%s)
PORT=${PR_HTTPS_PUBLIC_PORT:-443}
C="docker compose -f deploy/prod/compose.prod.yml"
log() { printf '[deploy %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }

DOMAIN=$(docker exec "$EDGE" printenv PR_DOMAIN 2>/dev/null || true)
FILES=$(docker exec "$EDGE" printenv PR_FILES_DOMAIN 2>/dev/null || true)
[ -n "$DOMAIN" ] && [ -n "$FILES" ] || { log "edge $EDGE is not running or lacks PR_DOMAIN/PR_FILES_DOMAIN (add them to LedgerPay edge.env and redeploy LedgerPay)"; exit 1; }

prev=""
[ -L "$ROOT/current" ] && prev=$(basename "$(readlink -f "$ROOT/current")")
printf %s "$PR_IMAGE_PREFIX" > "$ROOT/releases/$SHA/.image-prefix"
log "deploying $SHA (previous: ${prev:-none})"

record() { printf '%s\t%s\t%s\t%ss\t%s\n' "$(date -u +%FT%TZ)" "$SHA" "$1" "$(( $(date +%s) - started ))" "${PR_DEPLOY_ACTOR:-manual}" >> "$ROOT/releases.log"; }

reload_edge() { docker exec "$EDGE" caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile; }
site_backup=""
install_site() {
  if [ -f "$SITES/payrun.caddy" ]; then site_backup=$(mktemp) && cp "$SITES/payrun.caddy" "$site_backup"; fi
  install -m 644 deploy/prod/payrun.caddy "$SITES/payrun.caddy.new"
  mv "$SITES/payrun.caddy.new" "$SITES/payrun.caddy"
  reload_edge
}

rollback() {
  log "FAILED — rolling back to ${prev:-<none>}"
  record failed
  if [ -n "$site_backup" ]; then cp "$site_backup" "$SITES/payrun.caddy" && reload_edge || true; fi
  [ -n "$prev" ] || { log "no previous release; stack left as is"; exit 1; }
  # Rilis lama memakai image dari mode deploy-nya sendiri (build lokal atau registry).
  (cd "$ROOT/releases/$prev" && PR_VERSION=$prev PR_IMAGE_PREFIX=$(cat .image-prefix 2>/dev/null || true) \
    docker compose -f deploy/prod/compose.prod.yml up -d --no-deps --wait payrun-worker payrun-api payrun-web)
  log "rolled back to $prev"
  exit 1
}

if [ -n "$PR_IMAGE_PREFIX" ]; then
  log "pull images ${PR_IMAGE_PREFIX}*:$SHA (built + scanned by CI)"
  $C --profile migrate --profile backup pull migrate payrun-web backup
else
  log "build images"
  $C --profile migrate --profile backup build migrate payrun-web backup
fi
log "postgres + storage + mailpit"
$C up -d --wait postgres payrun-storage mailpit
if [ "$SKIP_MIGRATE" = --skip-migrate ]; then
  log "migrate SKIPPED (rollback: the schema stays at its current version; expand/contract keeps old code compatible)"
else
  log "migrate (owner DSN only here) + bucket"
  $C --profile migrate run --rm migrate || { log "migration failed — app NOT restarted (old version keeps serving)"; record migrate_failed; exit 1; }
  $C --profile migrate run --rm storage-init >/dev/null || { log "bucket init failed"; record migrate_failed; exit 1; }
fi
log "worker (SIGTERM: stop claiming, finish in-flight ≤ 25s)"
$C up -d --no-deps --wait payrun-worker || rollback
log "api (graceful shutdown; edge holds requests during recreate)"
$C up -d --no-deps --wait payrun-api || rollback
$C up -d --no-deps --wait payrun-web || rollback
log "site payrun.caddy → edge $EDGE (caddy reload)"
install_site || rollback

log "smoke (read-only) via edge (TLS)"
RAW=(curl -sS --max-time 10 --resolve "${DOMAIN}:${PORT}:127.0.0.1" --resolve "${FILES}:${PORT}:127.0.0.1")
[ -n "${PR_SMOKE_INSECURE:-}" ] && RAW+=(-k)   # hanya uji lokal (sertifikat internal Caddy)
CURL=("${RAW[@]}" -f)
base="https://${DOMAIN}:${PORT}"
ok=""
for _ in $(seq 1 30); do "${CURL[@]}" "$base/" 2>/dev/null | grep -q '<div id="app"' && { ok=1; break; }; sleep 2; done
[ -n "$ok" ] || { log "SPA not served"; rollback; }
# API hidup dan menolak tanpa token: 401 problem+json (tanpa menulis data apa pun).
code=$("${RAW[@]}" -o /dev/null -w '%{http_code}' "$base/api/v1/me" 2>/dev/null || true)
[ "$code" = 401 ] || { log "API smoke: GET /api/v1/me want 401, got $code"; rollback; }
# files.<domain>: listing bucket dan object tanpa tanda tangan TIDAK pernah lewat (C-40).
code=$("${RAW[@]}" -o /dev/null -w '%{http_code}' "https://${FILES}:${PORT}/payrun-private/" 2>/dev/null || true)
[ "$code" = 404 ] || { log "files smoke: bucket listing want 404, got $code"; rollback; }

ln -sfn "$ROOT/releases/$SHA" "$ROOT/current"
record success
[ -n "$prev" ] && [ "$prev" != "$SHA" ] && ln -sfn "$ROOT/releases/$prev" "$ROOT/previous"
log "OK — current = $SHA"
