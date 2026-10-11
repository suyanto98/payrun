-- Sprint 5: approval generik (ADR-P09), change log input payroll untuk deteksi stale (O12, desain transaksi & concurrency
-- `22` §3.3), dan PeriodGuard di DB (PI-14, jaring pengaman di bawah PeriodGuard aplikasi).
--
-- Ditunda: trigger PeriodGuard untuk attendance/leave/overtime (tabelnya dibuat di S6), cleanup
-- payroll_input_changes > 90 hari setelah finalize (job bersama finalize, S6).

-- +goose Up

-- ---------------------------------------------------------------- approval_requests (ADR-P09, PI-11)
CREATE TABLE approval_requests (
    tenant_id            uuid        NOT NULL,
    id                   uuid        NOT NULL,
    subject_type         text        NOT NULL,
    subject_id           uuid        NOT NULL,
    requester_id         uuid        NOT NULL,
    approver_id          uuid        NOT NULL,
    status               text        NOT NULL DEFAULT 'PENDING',
    decision_note        text,
    decided_by           uuid,
    is_override          boolean     NOT NULL DEFAULT false,
    self_approved        boolean     NOT NULL DEFAULT false,
    self_approval_reason text,
    decided_at           timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_approval_requests PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_approval_requests_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id),
    CONSTRAINT fk_approval_requests_requester FOREIGN KEY (requester_id) REFERENCES users (id),
    CONSTRAINT fk_approval_requests_approver FOREIGN KEY (approver_id) REFERENCES users (id),
    CONSTRAINT fk_approval_requests_decided_by FOREIGN KEY (decided_by) REFERENCES users (id),
    CONSTRAINT ck_approval_requests_subject_type CHECK (subject_type IN ('LEAVE_REQUEST', 'OVERTIME_REQUEST', 'PAYROLL_RUN')),
    CONSTRAINT ck_approval_requests_status CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'CANCELLED')),
    -- PI-11 (versi terkoreksi desain domain): requester ≠ approver, kecuali pengecualian satu admin untuk run.
    CONSTRAINT ck_approval_requests_not_self CHECK (approver_id <> requester_id
        OR (self_approved AND subject_type = 'PAYROLL_RUN' AND self_approval_reason IS NOT NULL)),
    CONSTRAINT ck_approval_requests_self_flag CHECK (NOT self_approved OR approver_id = requester_id),
    CONSTRAINT ck_approval_requests_decided CHECK ((status = 'PENDING') = (decided_at IS NULL)),
    CONSTRAINT ck_approval_requests_reject_note CHECK (status <> 'REJECTED' OR decision_note IS NOT NULL),
    CONSTRAINT ck_approval_requests_override CHECK (NOT is_override OR decided_by IS DISTINCT FROM approver_id)
);
CREATE UNIQUE INDEX uq_approval_requests_pending ON approval_requests (tenant_id, subject_type, subject_id) WHERE status = 'PENDING';
CREATE INDEX ix_approval_requests_inbox ON approval_requests (tenant_id, approver_id, created_at DESC) WHERE status = 'PENDING';
CREATE INDEX ix_approval_requests_subject ON approval_requests (tenant_id, subject_type, subject_id);
CREATE INDEX ix_approval_requests_created ON approval_requests (tenant_id, created_at DESC, id);
CREATE INDEX ix_approval_requests_requester ON approval_requests (requester_id);
CREATE INDEX ix_approval_requests_approver ON approval_requests (approver_id);
CREATE INDEX ix_approval_requests_decided_by ON approval_requests (decided_by);
CREATE TRIGGER trg_approval_requests_updated_at BEFORE UPDATE ON approval_requests
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Run menyimpan approval terakhirnya (respons PayrollRun.approval_request_id). Tanpa FK: approval
-- polimorfik dibuat di transaksi submit yang sama (12-db-schema §6.6).
ALTER TABLE payroll_runs ADD COLUMN approval_request_id uuid;

-- ---------------------------------------------------------------- payroll_input_changes (O12)
-- Satu baris per (periode, karyawan); changed_at = clock_timestamp() SETELAH PeriodGuard mendapat lock
-- periode, sehingga perubahan yang commit setelah kalkulasi membaca selalu punya changed_at >
-- calc_started_at (`22-tx-concurrency.md` §4.d1).
CREATE TABLE payroll_input_changes (
    tenant_id    uuid        NOT NULL,
    period_id    uuid        NOT NULL,
    employee_id  uuid        NOT NULL,
    changed_at   timestamptz NOT NULL,
    change_count int         NOT NULL DEFAULT 1,
    CONSTRAINT pk_payroll_input_changes PRIMARY KEY (tenant_id, period_id, employee_id),
    CONSTRAINT fk_payroll_input_changes_period FOREIGN KEY (tenant_id, period_id) REFERENCES payroll_periods (tenant_id, id),
    CONSTRAINT fk_payroll_input_changes_employee FOREIGN KEY (tenant_id, employee_id) REFERENCES employees (tenant_id, id),
    CONSTRAINT ck_payroll_input_changes_count CHECK (change_count >= 1)
);
CREATE INDEX ix_payroll_input_changes_period_changed ON payroll_input_changes (tenant_id, period_id, changed_at);
CREATE INDEX ix_payroll_input_changes_employee ON payroll_input_changes (tenant_id, employee_id);

-- ---------------------------------------------------------------- PeriodGuard DB (PI-14, 12-db-schema §6.5)
CREATE INDEX ix_payroll_periods_finalized ON payroll_periods USING gist (tenant_id, period_range) WHERE status = 'FINALIZED';

-- +goose StatementBegin
CREATE FUNCTION guard_period_date() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE col text := TG_ARGV[0]; d_new date; d_old date;
BEGIN
  IF TG_OP <> 'DELETE' THEN EXECUTE format('SELECT ($1).%I', col) INTO d_new USING NEW; END IF;
  IF TG_OP <> 'INSERT' THEN EXECUTE format('SELECT ($1).%I', col) INTO d_old USING OLD; END IF;
  IF EXISTS (SELECT 1 FROM payrun.payroll_periods p
             WHERE p.tenant_id = COALESCE(NEW.tenant_id, OLD.tenant_id)
               AND p.status = 'FINALIZED'
               AND (p.period_range @> d_new OR p.period_range @> d_old)) THEN
    RAISE EXCEPTION 'period locked' USING ERRCODE = 'P0002';
  END IF;
  RETURN COALESCE(NEW, OLD);
END $$;
-- +goose StatementEnd

-- Rentang: hanya tanggal yang NILAINYA berubah yang dicek (menutup gaji yang juga berlaku di periode
-- final tidak terblokir; 12-db-schema §6.5).
-- +goose StatementBegin
CREATE FUNCTION guard_period_range() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE col text := TG_ARGV[0]; r_new daterange; r_old daterange; changed daterange[];
BEGIN
  IF TG_OP <> 'DELETE' THEN EXECUTE format('SELECT ($1).%I', col) INTO r_new USING NEW; END IF;
  IF TG_OP <> 'INSERT' THEN EXECUTE format('SELECT ($1).%I', col) INTO r_old USING OLD; END IF;

  IF TG_OP = 'INSERT' THEN changed := ARRAY[r_new];
  ELSIF TG_OP = 'DELETE' THEN changed := ARRAY[r_old];
  ELSIF (to_jsonb(NEW) - col - 'updated_at' - 'updated_by' - 'version')
        IS DISTINCT FROM (to_jsonb(OLD) - col - 'updated_at' - 'updated_by' - 'version') THEN
    changed := ARRAY[r_old, r_new];
  ELSIF lower(r_old) = lower(r_new) THEN
    changed := ARRAY[daterange(least(upper(r_old), upper(r_new)), greatest(upper(r_old), upper(r_new)))];
  ELSIF upper(r_old) IS NOT DISTINCT FROM upper(r_new) THEN
    changed := ARRAY[daterange(least(lower(r_old), lower(r_new)), greatest(lower(r_old), lower(r_new)))];
  ELSE changed := ARRAY[r_old, r_new];
  END IF;

  IF EXISTS (SELECT 1 FROM payrun.payroll_periods p, unnest(changed) c
             WHERE p.tenant_id = COALESCE(NEW.tenant_id, OLD.tenant_id)
               AND p.status = 'FINALIZED' AND p.period_range && c) THEN
    RAISE EXCEPTION 'period locked' USING ERRCODE = 'P0002';
  END IF;
  RETURN COALESCE(NEW, OLD);
END $$;
-- +goose StatementEnd

CREATE TRIGGER trg_holidays_period_guard BEFORE INSERT OR UPDATE OR DELETE ON holidays
  FOR EACH ROW EXECUTE FUNCTION guard_period_date('holiday_date');
CREATE TRIGGER trg_salary_assignments_period_guard BEFORE INSERT OR UPDATE OR DELETE ON salary_assignments
  FOR EACH ROW EXECUTE FUNCTION guard_period_range('valid_during');
CREATE TRIGGER trg_employee_components_period_guard BEFORE INSERT OR UPDATE OR DELETE ON employee_components
  FOR EACH ROW EXECUTE FUNCTION guard_period_range('valid_during');
CREATE TRIGGER trg_employment_spans_period_guard BEFORE INSERT OR UPDATE OR DELETE ON employment_spans
  FOR EACH ROW EXECUTE FUNCTION guard_period_range('span');

-- ---------------------------------------------------------------- RLS + grant
ALTER TABLE approval_requests     ENABLE ROW LEVEL SECURITY;
ALTER TABLE approval_requests     FORCE  ROW LEVEL SECURITY;
ALTER TABLE payroll_input_changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_input_changes FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON approval_requests
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON payroll_input_changes
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));

-- Approval tidak pernah dihapus (jejak keputusan). Change log di-upsert; DELETE untuk cleanup retensi.
GRANT SELECT, INSERT, UPDATE ON approval_requests TO payrun_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON payroll_input_changes TO payrun_app;
GRANT SELECT ON approval_requests, payroll_input_changes TO payrun_readonly;

-- +goose Down
DROP TRIGGER trg_employment_spans_period_guard ON employment_spans;
DROP TRIGGER trg_employee_components_period_guard ON employee_components;
DROP TRIGGER trg_salary_assignments_period_guard ON salary_assignments;
DROP TRIGGER trg_holidays_period_guard ON holidays;
DROP FUNCTION guard_period_range();
DROP FUNCTION guard_period_date();
DROP INDEX ix_payroll_periods_finalized;
DROP TABLE payroll_input_changes;
ALTER TABLE payroll_runs DROP COLUMN approval_request_id;
DROP TABLE approval_requests;
