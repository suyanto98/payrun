-- Sprint 7: adjustment (PF-19, PI-15) dan payslip (PF-15, PF-22, PI-17). Skema mengikuti
-- docs/design/12-db-schema.md §7.6–7.7 dengan amandemen final review:
--   - target adjustment disimpan sebagai (target_year, target_month), bukan FK periode (R1-4): periode
--     target boleh belum dibuat saat adjustment dicatat.
--   - ix_payslips_item (C4).
-- Penolakan target FINALIZED + pencatatan stale lewat PeriodGuard aplikasi (payroll_input_changes),
-- bukan trigger guard_period_*: adjustment justru DIBUAT untuk periode asal yang sudah final.

-- +goose Up

-- ---------------------------------------------------------------- adjustments (PF-19)
CREATE TABLE adjustments (
    tenant_id        uuid          NOT NULL,
    id               uuid          NOT NULL,
    employee_id      uuid          NOT NULL,
    origin_period_id uuid          NOT NULL,
    target_year      smallint      NOT NULL,
    target_month     smallint      NOT NULL,
    component_code   text          NOT NULL,
    amount           numeric(18,2) NOT NULL,
    taxable          boolean       NOT NULL DEFAULT true,
    reason           text          NOT NULL,
    status           text          NOT NULL DEFAULT 'PENDING_APPLY',
    applied_run_id   uuid,
    applied_at       timestamptz,
    cancelled_by     uuid,
    cancelled_at     timestamptz,
    cancel_reason    text,
    created_at       timestamptz   NOT NULL DEFAULT now(),
    updated_at       timestamptz   NOT NULL DEFAULT now(),
    created_by       uuid,
    CONSTRAINT pk_adjustments PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_adjustments_employee FOREIGN KEY (tenant_id, employee_id) REFERENCES employees (tenant_id, id),
    CONSTRAINT fk_adjustments_origin_period FOREIGN KEY (tenant_id, origin_period_id) REFERENCES payroll_periods (tenant_id, id),
    CONSTRAINT fk_adjustments_applied_run FOREIGN KEY (tenant_id, applied_run_id) REFERENCES payroll_runs (tenant_id, id),
    CONSTRAINT ck_adjustments_amount CHECK (amount <> 0),
    CONSTRAINT ck_adjustments_target_month CHECK (target_month BETWEEN 1 AND 12),
    CONSTRAINT ck_adjustments_component CHECK (component_code IN ('OVERTIME_CORRECTION', 'DEDUCTION_CORRECTION', 'MANUAL')),
    CONSTRAINT ck_adjustments_status CHECK (status IN ('PENDING_APPLY', 'APPLIED', 'CANCELLED')),
    CONSTRAINT ck_adjustments_applied CHECK ((status = 'APPLIED') = (applied_run_id IS NOT NULL AND applied_at IS NOT NULL)),
    CONSTRAINT ck_adjustments_cancelled CHECK ((status = 'CANCELLED') = (cancelled_at IS NOT NULL))
);
CREATE INDEX ix_adjustments_target ON adjustments (tenant_id, target_year, target_month, employee_id) WHERE status = 'PENDING_APPLY';
CREATE INDEX ix_adjustments_created ON adjustments (tenant_id, created_at DESC, id);
CREATE INDEX ix_adjustments_employee ON adjustments (tenant_id, employee_id);
CREATE INDEX ix_adjustments_origin_period ON adjustments (tenant_id, origin_period_id);
CREATE INDEX ix_adjustments_applied_run ON adjustments (tenant_id, applied_run_id);
CREATE TRIGGER trg_adjustments_updated_at BEFORE UPDATE ON adjustments
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- PI-15: APPLIED/CANCELLED final. P0001 → 409 RUN_FINALIZED (pemetaan yang sama dengan PI-04).
-- +goose StatementBegin
CREATE FUNCTION adjustments_applied_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'adjustment % is % and immutable', OLD.id, OLD.status USING ERRCODE = 'P0001';
END
$$;
-- +goose StatementEnd
CREATE TRIGGER adjustments_applied_immutable BEFORE UPDATE OR DELETE ON adjustments
  FOR EACH ROW WHEN (OLD.status IN ('APPLIED', 'CANCELLED')) EXECUTE FUNCTION adjustments_applied_immutable();

-- payroll_lines.adjustment_id (kolom + uq per run dari 00007) kini punya FK.
ALTER TABLE payroll_lines ADD CONSTRAINT fk_payroll_lines_adjustment
    FOREIGN KEY (tenant_id, adjustment_id) REFERENCES adjustments (tenant_id, id);
ALTER TABLE payroll_lines ADD CONSTRAINT ck_payroll_lines_adjustment CHECK ((code = 'ADJUSTMENT') = (adjustment_id IS NOT NULL));
CREATE INDEX ix_payroll_lines_adjustment ON payroll_lines (tenant_id, adjustment_id);

-- ---------------------------------------------------------------- payslips (PF-15, PF-22)
-- Dibuat PENDING di transaksi finalize, satu per item OK; PDF dirender job payslip.render (eventual).
-- Slip web dibaca dari payroll_lines, bukan dari PDF.
CREATE TABLE payslips (
    tenant_id       uuid        NOT NULL,
    id              uuid        NOT NULL,
    run_id          uuid        NOT NULL,
    employee_id     uuid        NOT NULL,
    item_id         uuid        NOT NULL,
    status          text        NOT NULL DEFAULT 'PENDING',
    object_key      text        NOT NULL,
    checksum_sha256 bytea,
    size_bytes      int,
    rendered_at     timestamptz,
    attempts        smallint    NOT NULL DEFAULT 0,
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_payslips PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_payslips_run FOREIGN KEY (tenant_id, run_id) REFERENCES payroll_runs (tenant_id, id),
    CONSTRAINT fk_payslips_employee FOREIGN KEY (tenant_id, employee_id) REFERENCES employees (tenant_id, id),
    CONSTRAINT fk_payslips_item FOREIGN KEY (tenant_id, item_id) REFERENCES payroll_items (tenant_id, id),
    CONSTRAINT uq_payslips_run_employee UNIQUE (tenant_id, run_id, employee_id),
    CONSTRAINT uq_payslips_object_key UNIQUE (object_key),
    CONSTRAINT ck_payslips_status CHECK (status IN ('PENDING', 'RENDERED', 'FAILED')),
    CONSTRAINT ck_payslips_object_key CHECK (object_key = 'tenants/' || tenant_id::text || '/payslips/' || run_id::text
                                             || '/' || employee_id::text || '.pdf'),
    CONSTRAINT ck_payslips_rendered CHECK ((status = 'RENDERED') = (rendered_at IS NOT NULL AND checksum_sha256 IS NOT NULL))
);
CREATE INDEX ix_payslips_employee ON payslips (tenant_id, employee_id, created_at DESC);
CREATE INDEX ix_payslips_pending ON payslips (tenant_id, run_id) WHERE status <> 'RENDERED';
CREATE INDEX ix_payslips_item ON payslips (tenant_id, item_id);
CREATE TRIGGER trg_payslips_updated_at BEFORE UPDATE ON payslips
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- RLS + grant
ALTER TABLE adjustments ENABLE ROW LEVEL SECURITY;
ALTER TABLE adjustments FORCE  ROW LEVEL SECURITY;
ALTER TABLE payslips    ENABLE ROW LEVEL SECURITY;
ALTER TABLE payslips    FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON adjustments
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON payslips
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));

-- Tidak ada DELETE: adjustment dibatalkan (CANCELLED), payslip tetap ada selama run ada.
GRANT SELECT, INSERT, UPDATE ON adjustments, payslips TO payrun_app;
GRANT SELECT ON adjustments, payslips TO payrun_readonly;

-- +goose Down
DROP TABLE payslips;
DROP INDEX ix_payroll_lines_adjustment;
ALTER TABLE payroll_lines DROP CONSTRAINT ck_payroll_lines_adjustment;
ALTER TABLE payroll_lines DROP CONSTRAINT fk_payroll_lines_adjustment;
DROP TABLE adjustments;
DROP FUNCTION adjustments_applied_immutable();
