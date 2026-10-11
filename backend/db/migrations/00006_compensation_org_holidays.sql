-- Sprint 3: kompensasi (rulesets, component_definitions, salary_assignments, employee_components),
-- org assignment, atasan, dan libur tenant. Desain: docs/design/12-db-schema.md §1.3, §3.3, §5.
--
-- Amandemen final review S14: atasan TIDAK effective-dated. Kolom employees.manager_employee_id menyimpan
-- atasan saat ini (perubahan ter-audit; approver tetap di-snapshot saat submit, ADR-P09). Penempatan
-- departemen/posisi tetap effective-dated di org_assignments (effective dating juga didemokan lewat gaji).
-- Ditunda: trigger PeriodGuard (S4–S6, bersama payroll_periods).

-- +goose Up

-- ---------------------------------------------------------------- holidays (kalender kerja tenant)
CREATE TABLE holidays (
    tenant_id    uuid        NOT NULL,
    id           uuid        NOT NULL,
    holiday_date date        NOT NULL,
    name         text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_by   uuid,
    version      int         NOT NULL DEFAULT 1,
    CONSTRAINT pk_holidays PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_holidays_tenant_id FOREIGN KEY (tenant_id) REFERENCES tenants (id),
    CONSTRAINT uq_holidays_date UNIQUE (tenant_id, holiday_date),
    CONSTRAINT ck_holidays_name_len CHECK (length(name) BETWEEN 1 AND 100)
);
CREATE TRIGGER trg_holidays_updated_at BEFORE UPDATE ON holidays
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- rulesets (GLOBAL, immutable)
-- Bukan data tenant (allowlist tanpa RLS §2.2): baris baru hanya lewat migrasi (role owner); app SELECT.
CREATE TABLE rulesets (
    id             uuid        NOT NULL,
    code           text        NOT NULL,
    version        text        NOT NULL,
    valid_during   daterange   NOT NULL,
    label          text        NOT NULL,
    payload        jsonb       NOT NULL,
    payload_sha256 bytea       NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_rulesets PRIMARY KEY (id),
    CONSTRAINT uq_rulesets_code_version UNIQUE (code, version),
    CONSTRAINT ex_rulesets_no_overlap EXCLUDE USING gist (code WITH =, valid_during WITH &&),
    CONSTRAINT ck_rulesets_sha256 CHECK (octet_length(payload_sha256) = 32),
    CONSTRAINT ck_rulesets_payload_identity CHECK (payload->>'code' = code AND payload->>'version' = version)
);
CREATE TRIGGER trg_rulesets_immutable BEFORE UPDATE OR DELETE ON rulesets
  FOR EACH ROW EXECUTE FUNCTION forbid_update_delete();

-- Payload = internal/modules/payroll/engine/testdata/ruleset_ID-SIMPLIFIED_2026.1.json (dijaga
-- TestRuleset_SeedMatchesEngineFixture). Angka CONTOH: "simplified — not compliance-certified".
INSERT INTO rulesets (id, code, version, valid_during, label, payload, payload_sha256)
SELECT '01a11500-0000-7000-8000-000000000001', 'ID-SIMPLIFIED', '2026.1', daterange('2026-01-01', NULL, '[)'),
       p->>'label', p, sha256(convert_to(p::text, 'UTF8'))
FROM (SELECT $json${
  "code": "ID-SIMPLIFIED",
  "version": "2026.1",
  "label": "simplified — not compliance-certified",
  "overtime_divisor": "173",
  "overtime_tiers": [
    { "from_hour": "0", "multiplier": "1.5" },
    { "from_hour": "1", "multiplier": "2" }
  ],
  "statutory": [
    { "code": "BPJS_JHT_EE", "rate": "0.02", "rounding": "HALF_UP" },
    { "code": "BPJS_JP_EE", "rate": "0.01", "cap_max": "10000000", "rounding": "HALF_UP" },
    { "code": "BPJS_KES_EE", "rate": "0.01", "cap_max": "12000000", "rounding": "HALF_UP" }
  ],
  "tax_brackets": {
    "A": [
      { "max": "5400000", "rate": "0" },
      { "max": "7000000", "rate": "0.01" },
      { "max": "7500000", "rate": "0.015" },
      { "max": "9000000", "rate": "0.02" },
      { "max": "12000000", "rate": "0.03" },
      { "rate": "0.05" }
    ],
    "B": [
      { "max": "6200000", "rate": "0" },
      { "max": "8000000", "rate": "0.01" },
      { "max": "10000000", "rate": "0.02" },
      { "rate": "0.04" }
    ],
    "C": [
      { "max": "6600000", "rate": "0" },
      { "max": "9000000", "rate": "0.01" },
      { "max": "12000000", "rate": "0.02" },
      { "rate": "0.04" }
    ]
  },
  "tax_rounding": "DOWN"
}$json$::jsonb AS p) s;

-- ---------------------------------------------------------------- component_definitions
CREATE TABLE component_definitions (
    tenant_id      uuid          NOT NULL,
    id             uuid          NOT NULL,
    code           text          NOT NULL,
    name           text          NOT NULL,
    category       text          NOT NULL,
    stage          text          NOT NULL,
    calc_type      text          NOT NULL,
    default_amount numeric(18,2),
    rate           numeric(12,6),
    ruleset_ref    text,
    base_ref       text,
    day_source     text,
    cap_min        numeric(18,2),
    cap_max        numeric(18,2),
    taxable        boolean       NOT NULL DEFAULT true,
    bpjs_base      boolean       NOT NULL DEFAULT false,
    rounding       text          NOT NULL DEFAULT 'HALF_UP',
    is_system      boolean       NOT NULL DEFAULT false,
    status         text          NOT NULL DEFAULT 'ACTIVE',
    created_at     timestamptz   NOT NULL DEFAULT now(),
    updated_at     timestamptz   NOT NULL DEFAULT now(),
    created_by     uuid,
    updated_by     uuid,
    version        int           NOT NULL DEFAULT 1,
    CONSTRAINT pk_component_definitions PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_component_definitions_tenant_id FOREIGN KEY (tenant_id) REFERENCES tenants (id),
    CONSTRAINT uq_component_definitions_code UNIQUE (tenant_id, code),
    CONSTRAINT ck_component_definitions_code_format CHECK (code ~ '^[A-Z][A-Z0-9_]{1,40}$'),
    CONSTRAINT ck_component_definitions_name_len CHECK (length(name) BETWEEN 1 AND 100),
    CONSTRAINT ck_component_definitions_category CHECK (category IN ('EARNING', 'DEDUCTION')),
    CONSTRAINT ck_component_definitions_stage CHECK (stage IN ('EARNING', 'OVERTIME', 'ABSENCE', 'STATUTORY', 'TAX', 'ADJUSTMENT')),
    CONSTRAINT ck_component_definitions_calc_type CHECK (calc_type IN ('FIXED', 'PERCENT_OF', 'PER_DAY', 'PER_HOUR_MULTIPLIER')),
    CONSTRAINT ck_component_definitions_day_source CHECK (day_source IS NULL OR day_source IN ('PRESENT', 'WORKING')),
    CONSTRAINT ck_component_definitions_rounding CHECK (rounding IN ('HALF_UP', 'DOWN', 'UP')),
    CONSTRAINT ck_component_definitions_status CHECK (status IN ('ACTIVE', 'INACTIVE')),
    CONSTRAINT ck_component_definitions_params CHECK (CASE calc_type
      WHEN 'FIXED'      THEN rate IS NULL AND base_ref IS NULL AND ruleset_ref IS NULL AND day_source IS NULL
      WHEN 'PER_DAY'    THEN day_source IS NOT NULL AND rate IS NULL AND base_ref IS NULL AND ruleset_ref IS NULL
      WHEN 'PERCENT_OF' THEN base_ref IS NOT NULL AND day_source IS NULL AND default_amount IS NULL
                             AND ((rate IS NULL) <> (ruleset_ref IS NULL))
      WHEN 'PER_HOUR_MULTIPLIER' THEN is_system AND stage = 'OVERTIME'
    END),
    CONSTRAINT ck_component_definitions_caps CHECK (cap_min IS NULL OR cap_max IS NULL OR cap_min <= cap_max),
    CONSTRAINT ck_component_definitions_rate CHECK (rate IS NULL OR (rate >= 0 AND rate <= 1)),
    CONSTRAINT ck_component_definitions_amount CHECK (default_amount IS NULL OR default_amount >= 0),
    CONSTRAINT ck_component_definitions_tenant_stage CHECK (is_system OR stage = 'EARNING')
);
CREATE TRIGGER trg_component_definitions_updated_at BEFORE UPDATE ON component_definitions
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Seed O6: komponen sistem setiap tenant. Dipanggil di transaksi signup (mode tenant, RLS berlaku)
-- dan sekali di bawah untuk tenant yang sudah ada. Bukan SECURITY DEFINER.
-- +goose StatementBegin
CREATE FUNCTION seed_system_components(p_tenant uuid, p_actor uuid) RETURNS void LANGUAGE sql AS $$
  INSERT INTO payrun.component_definitions
    (tenant_id, id, code, name, category, stage, calc_type, ruleset_ref, base_ref, taxable, bpjs_base, rounding, is_system, created_by, updated_by)
  SELECT p_tenant, gen_random_uuid(), c.code, c.name, c.category, c.stage, c.calc_type, c.ruleset_ref, c.base_ref,
         c.taxable, c.bpjs_base, c.rounding, true, p_actor, p_actor
  FROM (VALUES
    ('BASE_SALARY',          'Gaji pokok',                'EARNING',   'EARNING',    'FIXED',               NULL,          NULL,            true,  true,  'HALF_UP'),
    ('OVERTIME',             'Lembur',                    'EARNING',   'OVERTIME',   'PER_HOUR_MULTIPLIER', NULL,          NULL,            true,  false, 'HALF_UP'),
    ('ABSENCE_UNPAID_LEAVE', 'Cuti tidak dibayar',        'DEDUCTION', 'ABSENCE',    'FIXED',               NULL,          NULL,            false, false, 'HALF_UP'),
    ('ABSENCE_UNEXCUSED',    'Absen tanpa keterangan',    'DEDUCTION', 'ABSENCE',    'FIXED',               NULL,          NULL,            false, false, 'HALF_UP'),
    ('BPJS_JHT_EE',          'BPJS JHT (karyawan)',       'DEDUCTION', 'STATUTORY',  'PERCENT_OF',          'BPJS_JHT_EE', 'BPJS_WAGE',     false, false, 'HALF_UP'),
    ('BPJS_JP_EE',           'BPJS JP (karyawan)',        'DEDUCTION', 'STATUTORY',  'PERCENT_OF',          'BPJS_JP_EE',  'BPJS_WAGE',     false, false, 'HALF_UP'),
    ('BPJS_KES_EE',          'BPJS Kesehatan (karyawan)', 'DEDUCTION', 'STATUTORY',  'PERCENT_OF',          'BPJS_KES_EE', 'BPJS_WAGE',     false, false, 'HALF_UP'),
    ('PPH21',                'PPh 21 (simplified)',       'DEDUCTION', 'TAX',        'PERCENT_OF',          'TAX_BRACKETS','TAXABLE_GROSS', false, false, 'DOWN'),
    ('ADJUSTMENT',           'Koreksi',                   'EARNING',   'ADJUSTMENT', 'FIXED',               NULL,          NULL,            true,  false, 'HALF_UP')
  ) AS c(code, name, category, stage, calc_type, ruleset_ref, base_ref, taxable, bpjs_base, rounding)
  ON CONFLICT ON CONSTRAINT uq_component_definitions_code DO NOTHING
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION seed_system_components(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION seed_system_components(uuid, uuid) TO payrun_app;

-- ---------------------------------------------------------------- salary_assignments (effective-dated)
CREATE TABLE salary_assignments (
    tenant_id     uuid          NOT NULL,
    id            uuid          NOT NULL,
    employee_id   uuid          NOT NULL,
    base_salary   numeric(18,2) NOT NULL,
    valid_during  daterange     NOT NULL,
    change_reason text,
    created_at    timestamptz   NOT NULL DEFAULT now(),
    updated_at    timestamptz   NOT NULL DEFAULT now(),
    created_by    uuid,
    updated_by    uuid,
    version       int           NOT NULL DEFAULT 1,
    CONSTRAINT pk_salary_assignments PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_salary_assignments_employee FOREIGN KEY (tenant_id, employee_id) REFERENCES employees (tenant_id, id),
    CONSTRAINT ex_salary_assignments_no_overlap EXCLUDE USING gist (tenant_id WITH =, employee_id WITH =, valid_during WITH &&),
    CONSTRAINT ck_salary_assignments_range CHECK (NOT isempty(valid_during) AND lower_inc(valid_during)
        AND NOT upper_inc(valid_during) AND lower(valid_during) IS NOT NULL),
    CONSTRAINT ck_salary_assignments_non_negative CHECK (base_salary >= 0),
    CONSTRAINT ck_salary_assignments_reason_len CHECK (change_reason IS NULL OR length(change_reason) <= 200)
);
CREATE INDEX ix_salary_assignments_employee ON salary_assignments (tenant_id, employee_id);
CREATE TRIGGER trg_salary_assignments_updated_at BEFORE UPDATE ON salary_assignments
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- employee_components (effective-dated)
CREATE TABLE employee_components (
    tenant_id    uuid          NOT NULL,
    id           uuid          NOT NULL,
    employee_id  uuid          NOT NULL,
    component_id uuid          NOT NULL,
    amount       numeric(18,2),
    rate         numeric(12,6),
    valid_during daterange     NOT NULL,
    created_at   timestamptz   NOT NULL DEFAULT now(),
    updated_at   timestamptz   NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_by   uuid,
    version      int           NOT NULL DEFAULT 1,
    CONSTRAINT pk_employee_components PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_employee_components_employee FOREIGN KEY (tenant_id, employee_id) REFERENCES employees (tenant_id, id),
    CONSTRAINT fk_employee_components_component FOREIGN KEY (tenant_id, component_id) REFERENCES component_definitions (tenant_id, id),
    CONSTRAINT ex_employee_components_no_overlap EXCLUDE USING gist
        (tenant_id WITH =, employee_id WITH =, component_id WITH =, valid_during WITH &&),
    CONSTRAINT ck_employee_components_range CHECK (NOT isempty(valid_during) AND lower_inc(valid_during)
        AND NOT upper_inc(valid_during) AND lower(valid_during) IS NOT NULL),
    CONSTRAINT ck_employee_components_amount CHECK (amount IS NULL OR amount >= 0),
    CONSTRAINT ck_employee_components_rate CHECK (rate IS NULL OR rate BETWEEN 0 AND 1)
);
CREATE INDEX ix_employee_components_employee ON employee_components (tenant_id, employee_id);
CREATE INDEX ix_employee_components_component ON employee_components (tenant_id, component_id);
CREATE TRIGGER trg_employee_components_updated_at BEFORE UPDATE ON employee_components
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- org_assignments (dept + posisi, effective-dated)
CREATE TABLE org_assignments (
    tenant_id     uuid        NOT NULL,
    id            uuid        NOT NULL,
    employee_id   uuid        NOT NULL,
    department_id uuid        NOT NULL,
    position_id   uuid        NOT NULL,
    valid_during  daterange   NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    updated_by    uuid,
    version       int         NOT NULL DEFAULT 1,
    CONSTRAINT pk_org_assignments PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_org_assignments_employee FOREIGN KEY (tenant_id, employee_id) REFERENCES employees (tenant_id, id),
    CONSTRAINT fk_org_assignments_department FOREIGN KEY (tenant_id, department_id) REFERENCES departments (tenant_id, id),
    CONSTRAINT fk_org_assignments_position FOREIGN KEY (tenant_id, position_id) REFERENCES positions (tenant_id, id),
    CONSTRAINT ex_org_assignments_no_overlap EXCLUDE USING gist (tenant_id WITH =, employee_id WITH =, valid_during WITH &&),
    CONSTRAINT ck_org_assignments_range CHECK (NOT isempty(valid_during) AND lower_inc(valid_during)
        AND NOT upper_inc(valid_during) AND lower(valid_during) IS NOT NULL)
);
CREATE INDEX ix_org_assignments_employee ON org_assignments (tenant_id, employee_id);
CREATE INDEX ix_org_assignments_department ON org_assignments (tenant_id, department_id);
CREATE INDEX ix_org_assignments_position ON org_assignments (tenant_id, position_id);
CREATE TRIGGER trg_org_assignments_updated_at BEFORE UPDATE ON org_assignments
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- atasan saat ini (S14) + PI-19
ALTER TABLE employees ADD COLUMN manager_employee_id uuid;
ALTER TABLE employees ADD CONSTRAINT fk_employees_manager
    FOREIGN KEY (tenant_id, manager_employee_id) REFERENCES employees (tenant_id, id);
ALTER TABLE employees ADD CONSTRAINT ck_employees_manager_not_self CHECK (manager_employee_id IS DISTINCT FROM id);
-- Index FK + jalur "bawahan langsung saya" (scope MANAGER, approval inbox).
CREATE INDEX ix_employees_manager ON employees (tenant_id, manager_employee_id);

-- PI-19: rantai atasan tidak boleh membentuk siklus. Advisory lock per tenant menutup race
-- "A atasan B" bersamaan dengan "B atasan A" (masing-masing tidak melihat yang lain di Read Committed).
-- +goose StatementBegin
CREATE FUNCTION forbid_manager_cycle() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.manager_employee_id IS NULL THEN RETURN NEW; END IF;
  PERFORM pg_advisory_xact_lock(hashtextextended('org:' || NEW.tenant_id::text, 0));
  IF EXISTS (
    WITH RECURSIVE chain(emp) AS (
      SELECT NEW.manager_employee_id
      UNION
      SELECT e.manager_employee_id
      FROM payrun.employees e JOIN chain c ON e.id = c.emp
      WHERE e.tenant_id = NEW.tenant_id AND e.manager_employee_id IS NOT NULL
    ) SELECT 1 FROM chain WHERE emp = NEW.id
  ) THEN
    RAISE EXCEPTION 'manager cycle for employee %', NEW.id USING ERRCODE = 'P0005';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER trg_employees_no_manager_cycle BEFORE INSERT OR UPDATE OF manager_employee_id ON employees
  FOR EACH ROW EXECUTE FUNCTION forbid_manager_cycle();
GRANT SELECT (manager_employee_id) ON employees TO payrun_readonly;

-- ---------------------------------------------------------------- RLS
ALTER TABLE holidays              ENABLE ROW LEVEL SECURITY;
ALTER TABLE holidays              FORCE  ROW LEVEL SECURITY;
ALTER TABLE component_definitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE component_definitions FORCE  ROW LEVEL SECURITY;
ALTER TABLE salary_assignments    ENABLE ROW LEVEL SECURITY;
ALTER TABLE salary_assignments    FORCE  ROW LEVEL SECURITY;
ALTER TABLE employee_components   ENABLE ROW LEVEL SECURITY;
ALTER TABLE employee_components   FORCE  ROW LEVEL SECURITY;
ALTER TABLE org_assignments       ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_assignments       FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON holidays
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON component_definitions
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON salary_assignments
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON employee_components
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON org_assignments
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));

-- ---------------------------------------------------------------- grants
-- holidays: DELETE (T6). Definisi komponen dinonaktifkan, bukan dihapus. Assignment effective-dated
-- ditutup (UPDATE valid_during), tidak dihapus: riwayat gaji adalah data audit.
GRANT SELECT, INSERT, UPDATE, DELETE ON holidays TO payrun_app;
GRANT SELECT, INSERT, UPDATE ON component_definitions, salary_assignments, employee_components, org_assignments TO payrun_app;
GRANT SELECT ON rulesets TO payrun_app;
GRANT SELECT ON holidays, rulesets, component_definitions, salary_assignments, employee_components, org_assignments TO payrun_readonly;

-- ---------------------------------------------------------------- backfill seed untuk tenant yang ada
-- Migrasi berjalan sebagai payrun_owner, dan FORCE RLS berlaku juga untuk owner. Di dalam transaksi
-- migrasi ini saja FORCE dilepas (owner lalu melewati RLS) dan dipasang lagi sebelum commit.
ALTER TABLE tenants NO FORCE ROW LEVEL SECURITY;
ALTER TABLE component_definitions NO FORCE ROW LEVEL SECURITY;
SELECT seed_system_components(t.id, NULL) FROM tenants t;
ALTER TABLE component_definitions FORCE ROW LEVEL SECURITY;
ALTER TABLE tenants FORCE ROW LEVEL SECURITY;

-- +goose Down
DROP TRIGGER trg_employees_no_manager_cycle ON employees;
DROP FUNCTION forbid_manager_cycle();
ALTER TABLE employees DROP COLUMN manager_employee_id;
DROP TABLE org_assignments;
DROP TABLE employee_components;
DROP TABLE salary_assignments;
DROP FUNCTION seed_system_components(uuid, uuid);
DROP TABLE component_definitions;
DROP TABLE rulesets;
DROP TABLE holidays;
