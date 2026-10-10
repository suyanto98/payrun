#!/usr/bin/env bash
# Restore satu backup terenkripsi ke database target (§5.4 langkah 3–4). Dipakai restore drill dan
# restore manual. Target harus sudah punya role + extension (deploy/postgres/init); schema payrun dibuat
# ulang dari dump (termasuk GRANT ke payrun_app/readonly/backup).
#   restore.sh <file.dump.age> <age-identity-file>
# Env: PGHOST PGUSER(superuser) PGPASSWORD [PGDATABASE=payrun]
set -euo pipefail
FILE=${1:?usage: restore.sh <file.dump.age> <identity>}
IDENTITY=${2:?age identity file}
DB=${PGDATABASE:-payrun}
if [ -f "$FILE.sha256" ]; then # integritas diverifikasi SEBELUM menyentuh database
  (cd "$(dirname "$FILE")" && sha256sum -c "$(basename "$FILE").sha256")
else
  echo "WARNING: no $FILE.sha256 next to the backup; integrity not verified" >&2
fi
age -d -i "$IDENTITY" "$FILE" | pg_restore --clean --if-exists --exit-on-error -d "$DB"
echo "restored $(basename "$FILE") into $DB"
