#!/usr/bin/env bash
# Restore drill (docs/design/29-deploy-migrations-backup.md §5.5): backup yang belum pernah di-restore
# BELUM terbukti sebagai backup. Skrip ini memulihkan satu file backup ke Postgres SEKALI PAKAI, lalu:
#   1. restore (pg_restore, setelah sha256 diverifikasi)   2. migrate up dengan binary SEKARANG
#   3. tabel tenant tidak kosong (tenants, employees, payroll_items, audit_log > 0 baris) — dump dengan role
#      yang terkena RLS menghasilkan tabel KOSONG tanpa error, jadi ini cek yang paling penting
#   4. verify-run setiap run FINALIZED (hitung ulang dari snapshot = hasil tersimpan, PI-12)
#   5. laporkan durasi (indikator RTO)
#
#   scripts/restore-drill.sh <file.dump.age> <age-identity-file>
# Env: PR_APP_IMAGE (image payrun; default build lokal), PR_BACKUP_IMAGE (default build lokal)
set -euo pipefail
cd "$(dirname "$0")/.."
FILE=$(realpath "${1:?usage: restore-drill.sh <file.dump.age> <identity>}")
IDENTITY=$(realpath "${2:?age identity file}")
ID="pr-drill-$(date +%s)-$$"
APP_IMAGE=${PR_APP_IMAGE:-payrun-drill-app:local}
BACKUP_IMAGE=${PR_BACKUP_IMAGE:-payrun-backup:local}
log() { printf '[drill %s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
fail() { log "FAIL: $*"; exit 1; }
cleanup() { docker rm -f "$ID-pg" >/dev/null 2>&1 || true; docker network rm "$ID" >/dev/null 2>&1 || true; }
trap cleanup EXIT

if [ -z "${PR_APP_IMAGE:-}" ]; then docker build -q -f deploy/Dockerfile --target payrun -t "$APP_IMAGE" . >/dev/null; fi
if [ -z "${PR_BACKUP_IMAGE:-}" ]; then docker build -q -f deploy/backup/Dockerfile -t "$BACKUP_IMAGE" . >/dev/null; fi

pw() { head -c 24 /dev/urandom | base64 | tr -d '\n=+/'; }
SUPER=$(pw); OWNER=$(pw); APP=$(pw)
start=$(date +%s)
docker network create "$ID" >/dev/null
docker run -d --name "$ID-pg" --network "$ID" --network-alias postgres \
  -e POSTGRES_DB=payrun -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD="$SUPER" \
  -e PAYRUN_OWNER_PASSWORD="$OWNER" -e PAYRUN_APP_PASSWORD="$APP" \
  -v "$PWD/deploy/postgres/init:/docker-entrypoint-initdb.d:ro" postgres:17-alpine >/dev/null
# pg_isready lolos sebentar saat init (server sementara); tunggu sampai server FINAL menerima koneksi.
for _ in $(seq 1 60); do
  docker exec "$ID-pg" psql -U postgres -d payrun -tAc "SELECT 1 FROM pg_namespace WHERE nspname = 'payrun'" 2>/dev/null | grep -q 1 \
    && docker logs "$ID-pg" 2>&1 | grep -q "PostgreSQL init process complete" && break
  sleep 1
done
log "empty postgres ready ($(( $(date +%s) - start ))s)"

docker run --rm --network "$ID" --entrypoint restore.sh \
  -e PGHOST=postgres -e PGUSER=postgres -e PGPASSWORD="$SUPER" -e PGDATABASE=payrun \
  -v "$(dirname "$FILE"):/in:ro" -v "$IDENTITY:/run/identity:ro" \
  "$BACKUP_IMAGE" "/in/$(basename "$FILE")" /run/identity
log "restored ($(( $(date +%s) - start ))s)"

app() { docker run --rm --network "$ID" -e APP_ENV=test \
  -e MIGRATE_DATABASE_URL="postgres://payrun_owner:$OWNER@postgres:5432/payrun?sslmode=disable" \
  -e DATABASE_URL="postgres://payrun_app:$APP@postgres:5432/payrun?sslmode=disable" "$APP_IMAGE" "$@"; }
app migrate up >/dev/null

sql() { docker exec "$ID-pg" psql -U postgres -d payrun -v ON_ERROR_STOP=1 -tAc "$1"; }
for tbl in tenants employees payroll_items audit_log; do
  n=$(sql "SELECT count(*) FROM payrun.$tbl")
  [ "$n" -gt 0 ] || fail "table $tbl is empty after restore (dump taken with an RLS-bound role?)"
  counts="${counts:+$counts, }$n $tbl"
done
runs=0
while IFS='|' read -r tenant run; do
  [ -n "$run" ] || continue
  app verify-run "$tenant" "$run" >/dev/null || fail "verify-run $run: recomputation differs from stored results"
  runs=$((runs + 1))
done < <(sql "SELECT tenant_id, id FROM payrun.payroll_runs WHERE status = 'FINALIZED' ORDER BY created_at")
log "PASS: $(basename "$FILE") → $counts; $runs finalized run(s) verified; restore + migrate + verify in $(( $(date +%s) - start ))s (RTO indicator)"
