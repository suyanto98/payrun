# Image backup/restore (docs/design/29-deploy-migrations-backup.md §5): pg_dump/pg_restore versi SAMA
# dengan server (17), age untuk enkripsi, rclone untuk offsite S3-compatible (R2/B2).
FROM postgres:17-alpine
RUN apk add --no-cache age rclone curl coreutils \
 # gosu dari image postgres tidak dipakai (USER postgres, entrypoint sendiri) dan membawa Go lama.
 && rm -f /usr/local/bin/gosu
COPY deploy/backup/backup.sh deploy/backup/restore.sh /usr/local/bin/
USER postgres
ENTRYPOINT ["/usr/local/bin/backup.sh"]
