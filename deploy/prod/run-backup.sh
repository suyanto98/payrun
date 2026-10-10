#!/usr/bin/env bash
# Backup harian PayRun, dijalankan cron host (/etc/cron.d/payrun-backup, 02:15 UTC: setelah LedgerPay,
# tidak bersamaan di VM yang sama) sebagai user deploy. Memakai image backup dari rilis aktif. Gagal →
# exit ≠ 0 + log.
set -euo pipefail
ROOT=${PR_ROOT:-/srv/payrun}
cd "$ROOT/current"
PR_VERSION=$(basename "$(readlink -f "$ROOT/current")")
export PR_ENV_DIR="$ROOT/env" PR_VERSION PR_BACKUP_HOST_DIR="$ROOT/backups"
exec docker compose -f deploy/prod/compose.prod.yml --profile backup run --rm backup
