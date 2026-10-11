#!/usr/bin/env bash
# Membuat env file per service PayRun untuk satu environment, dengan secret ACAK yang dihitung (bukan
# diketik). Hasil: deploy/prod/env/ (gitignored). Simpan salinan terenkripsi (sops/age, password manager):
# file ini satu-satunya sumber secret.
#   ./init-env.sh <payrun-domain> <files-domain> [out-dir]      contoh: payrun.example.com files.example.com
# Edge bersama (Caddy LedgerPay) butuh PR_DOMAIN dan PR_FILES_DOMAIN di edge.env LedgerPay (dicetak di akhir).
set -euo pipefail
DOMAIN=${1:?usage: init-env.sh <payrun-domain> <files-domain> [out-dir]}
FILES=${2:?files domain (presigned payslip downloads)}
OUT=${3:-$(dirname "$0")/env}
[ -e "$OUT/api.env" ] && { echo "refuse: $OUT already has secrets (delete it explicitly to regenerate)"; exit 1; }
mkdir -p "$OUT" && chmod 700 "$OUT"

rand() { head -c "$1" /dev/urandom | base64 | tr -d '\n=+/' | head -c "$2"; }    # password URL-safe
key32() { local k; k=$(head -c 32 /dev/urandom | base64 | tr -d '\n'); [ "$(printf %s "$k" | base64 -d | wc -c)" = 32 ] || exit 1; printf %s "$k"; }

PG_SUPER=$(rand 48 40); PG_OWNER=$(rand 48 40); PG_APP=$(rand 48 40); PG_BACKUP=$(rand 48 40)
JWT=$(key32); PII=$(key32); BLIND=$(key32); DEMO_TOKEN=$(rand 48 40)
S3_KEY=$(rand 24 20); S3_SECRET=$(rand 48 40)
APP_DSN="postgres://payrun_app:${PG_APP}@postgres:5432/payrun?sslmode=disable"

w() { umask 077; cat > "$OUT/$1"; }
w postgres.env <<E
POSTGRES_DB=payrun
POSTGRES_USER=postgres
POSTGRES_PASSWORD=${PG_SUPER}
PAYRUN_OWNER_PASSWORD=${PG_OWNER}
PAYRUN_APP_PASSWORD=${PG_APP}
PAYRUN_BACKUP_PASSWORD=${PG_BACKUP}
E
w migrate.env <<E
MIGRATE_DATABASE_URL=postgres://payrun_owner:${PG_OWNER}@postgres:5432/payrun?sslmode=disable
E
s3env() { cat <<E
S3_ENDPOINT=http://payrun-storage:8333
S3_PUBLIC_ENDPOINT=https://${FILES}
S3_BUCKET=payrun-private
S3_ACCESS_KEY=${S3_KEY}
S3_SECRET_KEY=${S3_SECRET}
E
}
w api.env <<E
APP_ENV=demo
LOG_LEVEL=info
DATABASE_URL=${APP_DSN}
JWT_SIGNING_KEYS={"k1":"${JWT}"}
JWT_ACTIVE_KID=k1
COOKIE_SECURE=true
ALLOWED_ORIGINS=https://${DOMAIN}
PUBLIC_BASE_URL=https://${DOMAIN}
TRUSTED_PROXIES=172.16.0.0/12
PII_KEYRING={"v1":"${PII}"}
PII_ACTIVE_KEY_VERSION=v1
PII_BLIND_INDEX_KEY=${BLIND}
$(s3env)
# Toolbar demo (O15): presenter menempelkan token ini di tab; TIDAK PERNAH masuk bundle SPA.
DEMO_MODE=true
DEMO_TOKEN=${DEMO_TOKEN}
# Demo (final review R2-8): Mailpit menangkap email verifikasi/undangan; UI via SSH tunnel (127.0.0.1:8026).
SMTP_ADDR=mailpit:1025
MAIL_FROM=no-reply@${DOMAIN}
E
w worker.env <<E
APP_ENV=demo
LOG_LEVEL=info
DATABASE_URL=${APP_DSN}
PII_KEYRING={"v1":"${PII}"}
PII_ACTIVE_KEY_VERSION=v1
PII_BLIND_INDEX_KEY=${BLIND}
$(s3env)
# Demo B: chunk 10 → "crash di karyawan ke-37" jatuh di chunk 4 (O5); lease 10s → pemulihan cepat terlihat.
PAYROLL_CHUNK_SIZE=10
JOB_LEASE_TTL=10s
E
(umask 077; cat > "$OUT/s3.json" <<E
{
  "identities": [
    {
      "name": "payrun-app",
      "credentials": [{ "accessKey": "${S3_KEY}", "secretKey": "${S3_SECRET}" }],
      "actions": ["Read:payrun-private", "Write:payrun-private"]
    }
  ]
}
E
)
# Backup dienkripsi untuk DUA recipient (§5.2): operator (key disimpan OFFLINE) dan restore drill
# (key menjadi secret CI). Private key tidak pernah ada di VM: VM hanya bisa mengenkripsi.
agekeygen() {
  if command -v age-keygen >/dev/null; then age-keygen 2>/dev/null
  else docker run --rm alpine:3.22 sh -c 'apk add -q --no-cache age >/dev/null && age-keygen 2>/dev/null'; fi
}
(umask 077; agekeygen > "$OUT/backup-operator.agekey"; agekeygen > "$OUT/backup-drill.agekey")
pub() { grep -o 'age1[0-9a-z]*' "$1" | head -1; }
w backup.env <<E
PGHOST=postgres
PGUSER=payrun_backup
PGPASSWORD=${PG_BACKUP}
PGDATABASE=payrun
PR_BACKUP_AGE_RECIPIENTS=$(pub "$OUT/backup-operator.agekey") $(pub "$OUT/backup-drill.agekey")
# Offsite (R2/B2, kredensial WRITE-ONLY). Kosong = backup hanya di disk VM.
PR_BACKUP_REMOTE=
E
echo "env files written to $OUT (mode 0600)."
echo "edge bersama: tambahkan ke edge.env LedgerPay → PR_DOMAIN=${DOMAIN} dan PR_FILES_DOMAIN=${FILES}"
echo "backup keys: move $OUT/backup-operator.agekey OFFLINE, then delete it here;"
echo "             $OUT/backup-drill.agekey → GitHub secret PR_RESTORE_DRILL_AGE_KEY. Neither is copied to the VM."
