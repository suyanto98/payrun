#!/usr/bin/env bash
# Tambahan provisioning VM BERSAMA untuk PayRun (IDEMPOTEN). Jalankan SETELAH bootstrap.sh LedgerPay
# (user deploy, Docker, ufw, swap sudah ada), sebagai root:
#   sudo bash deploy/prod/bootstrap.sh
# Membuat: network docker `edge` (dipakai bersama edge LedgerPay), folder site Caddy bersama,
# folder /srv/payrun, dan cron backup PayRun.
set -euo pipefail
id deploy >/dev/null 2>&1 || { echo "run LedgerPay deploy/prod/bootstrap.sh first (user deploy missing)"; exit 1; }
docker network inspect edge >/dev/null 2>&1 || docker network create edge
install -d -m 755 -o deploy -g deploy /srv/edge /srv/edge/sites
touch /srv/deploy.lock && chown deploy:deploy /srv/deploy.lock
install -d -m 755 -o deploy -g deploy /srv/payrun /srv/payrun/releases /srv/payrun/logs
install -d -m 700 -o deploy -g deploy /srv/payrun/env
# Backup ditulis container (user postgres, uid 70). Isinya terenkripsi age; private key tidak di VM.
install -d -m 750 -o 70 -g 70 /srv/payrun/backups
cat > /etc/cron.d/payrun-backup <<C
CRON_TZ=UTC
15 2 * * * deploy /srv/payrun/current/deploy/prod/run-backup.sh >> /srv/payrun/logs/backup.log 2>&1
C
chmod 644 /etc/cron.d/payrun-backup
echo "payrun bootstrap done. Next: deploy LedgerPay (edge with sites import), then deploy/prod/deploy.sh <host> <sha>"
