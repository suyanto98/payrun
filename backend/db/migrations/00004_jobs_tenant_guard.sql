-- Sprint 1 hardening (review keamanan S0–S1): jobs sebelumnya hanya dijaga trigger BEFORE INSERT.
-- payrun_app punya SELECT/UPDATE/DELETE tanpa RLS, sehingga transaksi tenant A bisa membaca payload
-- job tenant B, atau memindahkan job ke tenant lain lewat UPDATE tenant_id (worker lalu menjalankannya
-- di WithTenantTxFor(jobs.tenant_id) = tenant korban).
--
-- Penjaga baru:
--   1. RLS + FORCE: transaksi tenant hanya melihat/menulis job tenant-nya sendiri (atau job platform,
--      tenant_id NULL, yang memang boleh di-enqueue tanpa tenant). Tanpa tenant context (worker yang
--      meng-claim lintas tenant, mode platform) semua baris terlihat, sama seperti sebelumnya.
--   2. tenant_id immutable: trigger menolak UPDATE yang mengubahnya, di mode apa pun.
--   3. payrun_readonly tidak lagi membaca payload/last_error (bisa memuat PII di sprint berikutnya).

-- +goose Up

ALTER TABLE jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE jobs FORCE  ROW LEVEL SECURITY;
CREATE POLICY jobs_tenant ON jobs
  USING      ((SELECT current_tenant()) IS NULL OR tenant_id = (SELECT current_tenant()))
  WITH CHECK ((SELECT current_tenant()) IS NULL OR tenant_id = (SELECT current_tenant()));

-- +goose StatementBegin
CREATE FUNCTION jobs_forbid_tenant_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id THEN
    RAISE EXCEPTION 'jobs.tenant_id is immutable'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ck_jobs_tenant_immutable';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER trg_jobs_forbid_tenant_change BEFORE UPDATE OF tenant_id ON jobs
  FOR EACH ROW EXECUTE FUNCTION jobs_forbid_tenant_change();

REVOKE SELECT ON jobs FROM payrun_readonly;
GRANT SELECT (id, kind, tenant_id, status, attempts, max_attempts, run_at, locked_by, locked_until,
              dedupe_key, correlation_id, trace_parent, created_at, updated_at, completed_at)
  ON jobs TO payrun_readonly;

-- +goose Down
REVOKE SELECT ON jobs FROM payrun_readonly;
GRANT SELECT ON jobs TO payrun_readonly;
DROP TRIGGER trg_jobs_forbid_tenant_change ON jobs;
DROP FUNCTION jobs_forbid_tenant_change();
DROP POLICY jobs_tenant ON jobs;
ALTER TABLE jobs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE jobs DISABLE ROW LEVEL SECURITY;
