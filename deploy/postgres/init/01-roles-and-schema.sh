#!/bin/sh
# Dijalankan SEKALI oleh image postgres saat volume data masih kosong (sebagai superuser).
# Membuat role, schema, dan extension; tabel dibuat oleh migrasi (dijalankan sebagai payrun_owner).
# Versi Go dari skrip yang sama ada di internal/platform/db/dbtest (dipakai test). Ubah keduanya.
#
# Empat role (docs/design/12-db-rls-consistency-indexing.md §1, Step 9 `01`):
#   payrun_owner     pemilik schema + tabel; HANYA job migrasi. FORCE RLS tetap berlaku untuknya.
#   payrun_app       api + worker. NOBYPASSRLS dan BUKAN owner: RLS selalu berlaku (ADR-P01).
#   payrun_readonly  investigasi/laporan; SELECT saja, tetap di bawah RLS.
#   payrun_backup    pg_dump. BYPASSRLS (dump dengan role ber-RLS gagal atau kosong) + baca saja.
set -eu

BACKUP_LOGIN="NOLOGIN"
[ -n "${PAYRUN_BACKUP_PASSWORD:-}" ] && BACKUP_LOGIN="LOGIN PASSWORD '${PAYRUN_BACKUP_PASSWORD}'"
READONLY_LOGIN="NOLOGIN"
[ -n "${PAYRUN_READONLY_PASSWORD:-}" ] && READONLY_LOGIN="LOGIN PASSWORD '${PAYRUN_READONLY_PASSWORD}'"

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
  CREATE ROLE payrun_owner    LOGIN PASSWORD '${PAYRUN_OWNER_PASSWORD}' NOSUPERUSER NOCREATEDB NOCREATEROLE;
  CREATE ROLE payrun_app      LOGIN PASSWORD '${PAYRUN_APP_PASSWORD}'   NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  CREATE ROLE payrun_readonly ${READONLY_LOGIN} NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
  CREATE ROLE payrun_backup   ${BACKUP_LOGIN}   NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS;
  -- Baca semua data (predefined role PG14+), tanpa hak tulis/DDL. Kalau kredensial backup bocor,
  -- penyerang bisa membaca (karena itu dump dienkripsi), tetapi tidak bisa mengubah data.
  GRANT pg_read_all_data TO payrun_backup;
  ALTER ROLE payrun_owner    SET search_path = payrun, public;
  ALTER ROLE payrun_app      SET search_path = payrun, public;
  ALTER ROLE payrun_readonly SET search_path = payrun, public;

  REVOKE CREATE ON SCHEMA public FROM PUBLIC;
  -- btree_gist: EXCLUDE (tenant_id WITH =, employee_id WITH =, valid WITH &&) untuk effective dating
  -- (ADR-P04). Butuh superuser, jadi dibuat di sini, bukan di migrasi; migrasi 00001 memeriksanya.
  CREATE EXTENSION IF NOT EXISTS btree_gist SCHEMA public;
  CREATE SCHEMA payrun AUTHORIZATION payrun_owner;
  GRANT USAGE ON SCHEMA payrun TO payrun_app, payrun_readonly, payrun_backup;
EOSQL
