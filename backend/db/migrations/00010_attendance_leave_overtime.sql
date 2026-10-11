-- Sprint 6: input domain payroll — absensi (PF-08), cuti (PF-09), lembur (PF-10). Skema mengikuti
-- docs/design/12-db-schema.md §6.1, §6.4–6.7 dengan keputusan S6 (docs/design/16-api-endpoints.md,
-- catatan implementasi Sprint 6):
--   - attendance_records TANPA import_id: import CSV (PF-21) dipindah ke P2 (final review S10).
--   - ck_leave_requests_range didefinisikan di sini (dokumen hanya menyebut namanya).
--   - leave_balances dibuat lazy saat approve pertama (kuota tahunan penuh; akrual PF-26 = P2).
-- PeriodGuard DB (PI-14) dipasang di ketiga tabel input; fungsi guard_period_* dari migrasi 00009.

-- +goose Up

-- ---------------------------------------------------------------- attendance_records (PF-08)
CREATE TABLE attendance_records (
    tenant_id         uuid        NOT NULL,
    id                uuid        NOT NULL,
    employee_id       uuid        NOT NULL,
    work_date         date        NOT NULL,
    clock_in_at       timestamptz,
    clock_out_at      timestamptz,
    status            text        NOT NULL DEFAULT 'INCOMPLETE',
    source            text        NOT NULL,
    correction_reason text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    created_by        uuid,
    updated_by        uuid,
    version           int         NOT NULL DEFAULT 1,
    CONSTRAINT pk_attendance_records PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_attendance_records_employee FOREIGN KEY (tenant_id, employee_id) REFERENCES employees (tenant_id, id),
    CONSTRAINT uq_attendance_records_day UNIQUE (tenant_id, employee_id, work_date),
    CONSTRAINT ck_attendance_records_status CHECK (status IN ('INCOMPLETE', 'COMPLETE')),
    CONSTRAINT ck_attendance_records_source CHECK (source IN ('CLOCK', 'IMPORT', 'CORRECTION')),
    CONSTRAINT ck_attendance_records_order CHECK (clock_out_at IS NULL OR clock_in_at IS NULL OR clock_out_at > clock_in_at),
    CONSTRAINT ck_attendance_records_complete CHECK ((status = 'COMPLETE') = (clock_in_at IS NOT NULL AND clock_out_at IS NOT NULL)),
    CONSTRAINT ck_attendance_records_correction CHECK (source <> 'CORRECTION' OR correction_reason IS NOT NULL)
);
CREATE INDEX ix_attendance_records_date ON attendance_records (tenant_id, work_date);
CREATE TRIGGER trg_attendance_records_updated_at BEFORE UPDATE ON attendance_records
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_attendance_records_period_guard BEFORE INSERT OR UPDATE OR DELETE ON attendance_records
  FOR EACH ROW EXECUTE FUNCTION guard_period_date('work_date');

-- ---------------------------------------------------------------- leave_types (PF-09; seed saja di V1)
CREATE TABLE leave_types (
    tenant_id         uuid         NOT NULL,
    id                uuid         NOT NULL,
    code              text         NOT NULL,
    name              text         NOT NULL,
    is_paid           boolean      NOT NULL DEFAULT true,
    annual_quota_days numeric(5,1),
    max_negative_days numeric(5,1) NOT NULL DEFAULT 0,
    status            text         NOT NULL DEFAULT 'ACTIVE',
    created_at        timestamptz  NOT NULL DEFAULT now(),
    updated_at        timestamptz  NOT NULL DEFAULT now(),
    created_by        uuid,
    updated_by        uuid,
    version           int          NOT NULL DEFAULT 1,
    CONSTRAINT pk_leave_types PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_leave_types_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id),
    CONSTRAINT uq_leave_types_code UNIQUE (tenant_id, code),
    CONSTRAINT ck_leave_types_quota CHECK (annual_quota_days IS NULL OR annual_quota_days >= 0),
    CONSTRAINT ck_leave_types_negative CHECK (max_negative_days >= 0),
    CONSTRAINT ck_leave_types_status CHECK (status IN ('ACTIVE', 'INACTIVE'))
);
CREATE TRIGGER trg_leave_types_updated_at BEFORE UPDATE ON leave_types
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- leave_balances (PI-10)
CREATE TABLE leave_balances (
    tenant_id        uuid         NOT NULL,
    id               uuid         NOT NULL,
    employee_id      uuid         NOT NULL,
    leave_type_id    uuid         NOT NULL,
    year             smallint     NOT NULL,
    balance_days     numeric(6,1) NOT NULL,
    min_allowed_days numeric(6,1) NOT NULL DEFAULT 0,
    created_at       timestamptz  NOT NULL DEFAULT now(),
    updated_at       timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_leave_balances PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_leave_balances_employee FOREIGN KEY (tenant_id, employee_id) REFERENCES employees (tenant_id, id),
    CONSTRAINT fk_leave_balances_type FOREIGN KEY (tenant_id, leave_type_id) REFERENCES leave_types (tenant_id, id),
    CONSTRAINT uq_leave_balances_key UNIQUE (tenant_id, employee_id, leave_type_id, year),
    -- PI-10: saldo tidak pernah di bawah batas minus jenis cuti (denormalisasi −max_negative_days).
    CONSTRAINT ck_leave_balances_min CHECK (balance_days >= min_allowed_days),
    CONSTRAINT ck_leave_balances_min_non_positive CHECK (min_allowed_days <= 0),
    CONSTRAINT ck_leave_balances_year CHECK (year BETWEEN 2000 AND 2100)
);
CREATE INDEX ix_leave_balances_type ON leave_balances (tenant_id, leave_type_id);
CREATE TRIGGER trg_leave_balances_updated_at BEFORE UPDATE ON leave_balances
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- leave_requests (PF-09)
CREATE TABLE leave_requests (
    tenant_id          uuid         NOT NULL,
    id                 uuid         NOT NULL,
    employee_id        uuid         NOT NULL,
    leave_type_id      uuid         NOT NULL,
    dates              daterange    NOT NULL,
    days               numeric(5,1) NOT NULL,
    reason             text,
    status             text         NOT NULL DEFAULT 'PENDING',
    approved_at        timestamptz,
    cancels_request_id uuid,
    created_at         timestamptz  NOT NULL DEFAULT now(),
    updated_at         timestamptz  NOT NULL DEFAULT now(),
    created_by         uuid,
    updated_by         uuid,
    version            int          NOT NULL DEFAULT 1,
    CONSTRAINT pk_leave_requests PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_leave_requests_employee FOREIGN KEY (tenant_id, employee_id) REFERENCES employees (tenant_id, id),
    CONSTRAINT fk_leave_requests_type FOREIGN KEY (tenant_id, leave_type_id) REFERENCES leave_types (tenant_id, id),
    CONSTRAINT fk_leave_requests_cancels FOREIGN KEY (tenant_id, cancels_request_id) REFERENCES leave_requests (tenant_id, id),
    CONSTRAINT ck_leave_requests_status CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'CANCELLED')),
    CONSTRAINT ck_leave_requests_range CHECK (NOT isempty(dates) AND lower_inc(dates) AND NOT upper_inc(dates)
        AND lower(dates) IS NOT NULL AND upper(dates) IS NOT NULL),
    CONSTRAINT ck_leave_requests_days CHECK (days > 0),
    CONSTRAINT ck_leave_requests_approved CHECK ((status = 'APPROVED') = (approved_at IS NOT NULL)),
    -- PF-09 AC4: tidak tumpang tindih dengan cuti PENDING/APPROVED milik karyawan yang sama.
    CONSTRAINT ex_leave_requests_no_overlap EXCLUDE USING gist (tenant_id WITH =, employee_id WITH =, dates WITH &&)
        WHERE (status IN ('PENDING', 'APPROVED') AND cancels_request_id IS NULL)
);
CREATE INDEX ix_leave_requests_employee ON leave_requests (tenant_id, employee_id, status);
CREATE INDEX ix_leave_requests_created ON leave_requests (tenant_id, created_at DESC, id);
CREATE INDEX ix_leave_requests_type ON leave_requests (tenant_id, leave_type_id);
CREATE INDEX ix_leave_requests_cancels ON leave_requests (tenant_id, cancels_request_id);
CREATE TRIGGER trg_leave_requests_updated_at BEFORE UPDATE ON leave_requests
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_leave_requests_period_guard BEFORE INSERT OR UPDATE OR DELETE ON leave_requests
  FOR EACH ROW EXECUTE FUNCTION guard_period_range('dates');

-- ---------------------------------------------------------------- overtime_requests (PF-10)
CREATE TABLE overtime_requests (
    tenant_id   uuid         NOT NULL,
    id          uuid         NOT NULL,
    employee_id uuid         NOT NULL,
    work_date   date         NOT NULL,
    start_at    timestamptz  NOT NULL,
    end_at      timestamptz  NOT NULL,
    hours       numeric(6,2) NOT NULL,
    reason      text         NOT NULL,
    status      text         NOT NULL DEFAULT 'PENDING',
    approved_at timestamptz,
    created_at  timestamptz  NOT NULL DEFAULT now(),
    updated_at  timestamptz  NOT NULL DEFAULT now(),
    created_by  uuid,
    updated_by  uuid,
    version     int          NOT NULL DEFAULT 1,
    CONSTRAINT pk_overtime_requests PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_overtime_requests_employee FOREIGN KEY (tenant_id, employee_id) REFERENCES employees (tenant_id, id),
    CONSTRAINT ck_overtime_requests_status CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'CANCELLED')),
    CONSTRAINT ck_overtime_requests_order CHECK (end_at > start_at),
    CONSTRAINT ck_overtime_requests_hours CHECK (hours > 0 AND hours <= 24),
    CONSTRAINT ck_overtime_requests_approved CHECK ((status = 'APPROVED') = (approved_at IS NOT NULL)),
    CONSTRAINT ex_overtime_requests_no_overlap EXCLUDE USING gist
        (tenant_id WITH =, employee_id WITH =, tstzrange(start_at, end_at) WITH &&) WHERE (status IN ('PENDING', 'APPROVED'))
);
CREATE INDEX ix_overtime_requests_employee_date ON overtime_requests (tenant_id, employee_id, work_date);
CREATE INDEX ix_overtime_requests_created ON overtime_requests (tenant_id, created_at DESC, id);
CREATE TRIGGER trg_overtime_requests_updated_at BEFORE UPDATE ON overtime_requests
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_overtime_requests_period_guard BEFORE INSERT OR UPDATE OR DELETE ON overtime_requests
  FOR EACH ROW EXECUTE FUNCTION guard_period_date('work_date');

-- ---------------------------------------------------------------- seed jenis cuti (13 §6)
-- Dipanggil di transaksi signup (sebagai payrun_app, tenant context = tenant baru) + backfill tenant lama.
-- +goose StatementBegin
CREATE FUNCTION seed_leave_types(p_tenant uuid, p_actor uuid) RETURNS void LANGUAGE sql AS $$
  INSERT INTO payrun.leave_types (tenant_id, id, code, name, is_paid, annual_quota_days, max_negative_days, created_by, updated_by)
  SELECT p_tenant, gen_random_uuid(), t.code, t.name, t.is_paid, t.quota, 0, p_actor, p_actor
  FROM (VALUES
    ('ANNUAL', 'Cuti tahunan',       true,  12.0),
    ('SICK',   'Sakit',              true,  NULL),
    ('UNPAID', 'Cuti tidak dibayar', false, NULL)
  ) AS t(code, name, is_paid, quota)
  ON CONFLICT ON CONSTRAINT uq_leave_types_code DO NOTHING
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------- RLS + grant
ALTER TABLE attendance_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE attendance_records FORCE  ROW LEVEL SECURITY;
ALTER TABLE leave_types        ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_types        FORCE  ROW LEVEL SECURITY;
ALTER TABLE leave_balances     ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_balances     FORCE  ROW LEVEL SECURITY;
ALTER TABLE leave_requests     ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_requests     FORCE  ROW LEVEL SECURITY;
ALTER TABLE overtime_requests  ENABLE ROW LEVEL SECURITY;
ALTER TABLE overtime_requests  FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON attendance_records
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON leave_types
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON leave_balances
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON leave_requests
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON overtime_requests
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));

-- Riwayat input payroll tidak pernah dihapus (status CANCELLED/REJECTED, bukan DELETE).
GRANT SELECT, INSERT, UPDATE ON attendance_records, leave_types, leave_balances, leave_requests, overtime_requests TO payrun_app;
GRANT SELECT ON attendance_records, leave_types, leave_balances, leave_requests, overtime_requests TO payrun_readonly;

-- ---------------------------------------------------------------- backfill jenis cuti untuk tenant yang ada
-- Pola 00006: migrasi berjalan sebagai payrun_owner dan FORCE RLS berlaku juga untuk owner (tanpa tenant
-- context SELECT tenants = 0 baris, backfill diam-diam kosong). FORCE dilepas hanya di transaksi ini.
ALTER TABLE tenants NO FORCE ROW LEVEL SECURITY;
ALTER TABLE leave_types NO FORCE ROW LEVEL SECURITY;
SELECT seed_leave_types(t.id, NULL) FROM tenants t;
ALTER TABLE leave_types FORCE ROW LEVEL SECURITY;
ALTER TABLE tenants FORCE ROW LEVEL SECURITY;

-- +goose Down
DROP TABLE overtime_requests;
DROP TABLE leave_requests;
DROP TABLE leave_balances;
DROP TABLE leave_types;
DROP TABLE attendance_records;
DROP FUNCTION seed_leave_types(uuid, uuid);
