-- Sprint 0: fondasi skema PayRun (docs/design/13-db-rls-consistency-indexing.md §1–§2).
-- Prasyarat (dibuat skrip init sebagai superuser, bukan migrasi): role payrun_owner / payrun_app /
-- payrun_readonly / payrun_backup, schema payrun milik payrun_owner, extension btree_gist.
-- Migrasi dijalankan sebagai payrun_owner. Aplikasi berjalan sebagai payrun_app.
-- Belum ada tabel bisnis: tabel tenant pertama (tenants, memberships, ...) masuk Sprint 1, masing-masing
-- dengan ENABLE + FORCE RLS + policy di migrasi yang sama.

-- +goose Up

-- ---------------------------------------------------------------- prasyarat
-- Gagal dengan pesan jelas kalau init script tidak dijalankan, bukan error kriptik saat migrasi
-- effective dating (Sprint 3) membuat EXCLUDE USING gist.
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'btree_gist') THEN
    RAISE EXCEPTION 'extension btree_gist is missing'
      USING HINT = 'Run deploy/postgres/init/01-roles-and-schema.sh as superuser first';
  END IF;
END $$;
-- +goose StatementEnd

-- ---------------------------------------------------------------- tenant context helpers
-- Semua policy RLS memakai helper ini, tidak pernah current_setting(...)::uuid mentah (final review C3):
-- setelah GUC kustom pernah disetel di suatu koneksi (termasuk set_config(..., true) yang transaksinya
-- sudah selesai), current_setting('app.tenant_id', true) mengembalikan '' (string kosong), BUKAN NULL.
-- Cast ''::uuid gagal (22P02), sehingga request gagal acak tergantung koneksi pool mana yang didapat.
-- nullif mengubah '' menjadi NULL, dan `tenant_id = NULL` tidak pernah benar: fail closed (0 baris).
--
-- STABLE (bukan VOLATILE) supaya bisa dibungkus subselect `(SELECT payrun.current_tenant())` dan
-- dievaluasi sekali per query (initplan). BUKAN SECURITY DEFINER: berjalan dengan hak pemanggil.
CREATE FUNCTION current_tenant() RETURNS uuid LANGUAGE sql STABLE PARALLEL SAFE AS
  $$ SELECT nullif(current_setting('app.tenant_id', true), '')::uuid $$;

-- Identitas user di mode platform (login, daftar tenant, switch tenant). Mode eksklusif (final review C2):
-- satu transaksi tidak pernah memiliki app.tenant_id dan app.user_id aktif bersamaan (Sprint 1).
CREATE FUNCTION current_user_id() RETURNS uuid LANGUAGE sql STABLE PARALLEL SAFE AS
  $$ SELECT nullif(current_setting('app.user_id', true), '')::uuid $$;

-- ---------------------------------------------------------------- trigger functions
-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION forbid_update_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% is append-only: % is not allowed', TG_TABLE_NAME, TG_OP
    USING ERRCODE = 'insufficient_privilege', HINT = 'Create a correcting record instead';
END $$;
-- +goose StatementEnd

-- ---------------------------------------------------------------- grants (least privilege, C-15)
-- Fungsi baru di PostgreSQL bisa dieksekusi PUBLIC secara default; dibatasi eksplisit.
REVOKE EXECUTE ON FUNCTION current_tenant(), current_user_id() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION current_tenant(), current_user_id() TO payrun_app, payrun_readonly;
GRANT SELECT ON goose_db_version TO payrun_app;           -- readiness: cek versi skema

-- +goose Down
DROP FUNCTION forbid_update_delete();
DROP FUNCTION set_updated_at();
DROP FUNCTION current_user_id();
DROP FUNCTION current_tenant();
