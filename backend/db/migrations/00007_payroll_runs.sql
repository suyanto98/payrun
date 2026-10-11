-- Sprint 4: payroll run (PF-13). Desain: docs/design/12-db-schema.md §7.1–7.5, `22-tx-concurrency.md` §3.2.
-- Invariant di DB (jaring terakhir di bawah guard aplikasi):
--   PI-03 satu run aktif per periode            → uq_payroll_runs_active_per_period
--   PI-04 run final/batal immutable             → payroll_runs_terminal_immutable,
--                                                  forbid_change_on_finalized_run (FOR SHARE pada run)
--   PI-05 total run = Σ item OK, item = Σ line  → payroll_runs_totals_check, payroll_items_sum_check (deferred)
--   PI-06 satu item per (run, employee, versi)  → uq_payroll_items_version
-- Ditunda: adjustments + FK payroll_lines.adjustment_id (S7), payroll_input_changes + PeriodGuard (S5/S6),
-- payslips (S7).

-- +goose Up

-- ---------------------------------------------------------------- payroll_periods
CREATE TABLE payroll_periods (
    tenant_id        uuid        NOT NULL,
    id               uuid        NOT NULL,
    year             smallint    NOT NULL,
    month            smallint    NOT NULL,
    period_range     daterange   NOT NULL GENERATED ALWAYS AS
        (daterange(make_date(year, month, 1), (make_date(year, month, 1) + interval '1 month')::date)) STORED,
    cutoff_at        timestamptz NOT NULL,
    status           text        NOT NULL DEFAULT 'OPEN',
    finalized_run_id uuid,
    finalized_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_payroll_periods PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_payroll_periods_tenant_id FOREIGN KEY (tenant_id) REFERENCES tenants (id),
    CONSTRAINT uq_payroll_periods_month UNIQUE (tenant_id, year, month),
    CONSTRAINT ck_payroll_periods_month CHECK (month BETWEEN 1 AND 12),
    CONSTRAINT ck_payroll_periods_year CHECK (year BETWEEN 2000 AND 2100),
    CONSTRAINT ck_payroll_periods_status CHECK (status IN ('OPEN', 'PROCESSING', 'FINALIZED')),
    CONSTRAINT ck_payroll_periods_finalized CHECK ((status = 'FINALIZED') = (finalized_run_id IS NOT NULL AND finalized_at IS NOT NULL)),
    CONSTRAINT ck_payroll_periods_cutoff CHECK (cutoff_at >= lower(period_range)::timestamptz)
);
CREATE TRIGGER trg_payroll_periods_updated_at BEFORE UPDATE ON payroll_periods
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION payroll_periods_final_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'FINALIZED' THEN
    RAISE EXCEPTION 'payroll period % is finalized', OLD.id USING ERRCODE = 'P0001';
  END IF;
  RETURN COALESCE(NEW, OLD);
END $$;
-- +goose StatementEnd
CREATE TRIGGER trg_payroll_periods_final_immutable BEFORE UPDATE OR DELETE ON payroll_periods
  FOR EACH ROW EXECUTE FUNCTION payroll_periods_final_immutable();

-- ---------------------------------------------------------------- payroll_runs
CREATE TABLE payroll_runs (
    tenant_id           uuid          NOT NULL,
    id                  uuid          NOT NULL,
    period_id           uuid          NOT NULL,
    status              text          NOT NULL DEFAULT 'DRAFT',
    calc_version        int           NOT NULL DEFAULT 0,
    chunks_total        int           NOT NULL DEFAULT 0,
    chunks_done         int           NOT NULL DEFAULT 0,
    eligible_count      int,
    input_fingerprint   bytea,
    ruleset_code        text,
    ruleset_version     text,
    total_gross         numeric(18,2),
    total_deductions    numeric(18,2),
    total_net           numeric(18,2),
    item_ok_count       int,
    item_error_count    int,
    item_excluded_count int,
    warning_count       int,
    failure_reason      text,
    calc_started_at     timestamptz,
    calculated_at       timestamptz,
    submitted_by        uuid,
    submitted_at        timestamptz,
    approved_by         uuid,
    approved_at         timestamptz,
    finalized_by        uuid,
    finalized_at        timestamptz,
    cancelled_by        uuid,
    cancelled_at        timestamptz,
    cancel_reason       text,
    created_by          uuid,
    created_at          timestamptz   NOT NULL DEFAULT now(),
    updated_at          timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_payroll_runs PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_payroll_runs_period FOREIGN KEY (tenant_id, period_id) REFERENCES payroll_periods (tenant_id, id),
    CONSTRAINT ck_payroll_runs_status CHECK (status IN ('DRAFT', 'CALCULATING', 'CALCULATED', 'PENDING_APPROVAL',
        'APPROVED', 'FINALIZED', 'FAILED', 'CANCELLED')),
    CONSTRAINT ck_payroll_runs_chunks CHECK (chunks_done BETWEEN 0 AND chunks_total),
    CONSTRAINT ck_payroll_runs_calculated_totals CHECK (status IN ('DRAFT', 'CALCULATING', 'FAILED', 'CANCELLED')
        OR (total_net IS NOT NULL AND input_fingerprint IS NOT NULL AND total_net = total_gross - total_deductions)),
    CONSTRAINT ck_payroll_runs_finalized CHECK ((status = 'FINALIZED') = (finalized_at IS NOT NULL)),
    CONSTRAINT ck_payroll_runs_cancelled CHECK ((status = 'CANCELLED') = (cancelled_at IS NOT NULL)),
    CONSTRAINT ck_payroll_runs_fingerprint CHECK (input_fingerprint IS NULL OR octet_length(input_fingerprint) = 32),
    CONSTRAINT ck_payroll_runs_calc_started CHECK (status IN ('DRAFT', 'CANCELLED') OR calc_started_at IS NOT NULL)
);
CREATE UNIQUE INDEX uq_payroll_runs_active_per_period ON payroll_runs (tenant_id, period_id) WHERE status <> 'CANCELLED';
CREATE INDEX ix_payroll_runs_period ON payroll_runs (tenant_id, period_id, created_at DESC);
CREATE INDEX ix_payroll_runs_created ON payroll_runs (tenant_id, created_at DESC, id);
CREATE TRIGGER trg_payroll_runs_updated_at BEFORE UPDATE ON payroll_runs
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE payroll_periods ADD CONSTRAINT fk_payroll_periods_finalized_run
    FOREIGN KEY (tenant_id, finalized_run_id) REFERENCES payroll_runs (tenant_id, id);
CREATE INDEX ix_payroll_periods_finalized_run ON payroll_periods (tenant_id, finalized_run_id);

-- PI-04: run FINALIZED/CANCELLED adalah terminal.
-- +goose StatementBegin
CREATE FUNCTION payroll_runs_terminal_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status IN ('FINALIZED', 'CANCELLED') THEN
    RAISE EXCEPTION 'payroll run % is %', OLD.id, OLD.status USING ERRCODE = 'P0001';
  END IF;
  RETURN COALESCE(NEW, OLD);
END $$;
-- +goose StatementEnd
CREATE TRIGGER trg_payroll_runs_terminal_immutable BEFORE UPDATE OR DELETE ON payroll_runs
  FOR EACH ROW EXECUTE FUNCTION payroll_runs_terminal_immutable();

-- ---------------------------------------------------------------- payroll_items
CREATE TABLE payroll_items (
    tenant_id         uuid          NOT NULL,
    id                uuid          NOT NULL,
    run_id            uuid          NOT NULL,
    employee_id       uuid          NOT NULL,
    calc_version      int           NOT NULL,
    status            text          NOT NULL,
    gross             numeric(18,2),
    deductions        numeric(18,2),
    net               numeric(18,2),
    error_codes       text[]        NOT NULL DEFAULT '{}',
    warning_codes     text[]        NOT NULL DEFAULT '{}',
    issues            jsonb         NOT NULL DEFAULT '[]',
    input_fingerprint bytea         NOT NULL,
    employee_no       text          NOT NULL,
    employee_name     text          NOT NULL,
    department_id     uuid,
    excluded_reason   text,
    excluded_by       uuid,
    excluded_at       timestamptz,
    created_at        timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_payroll_items PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_payroll_items_run FOREIGN KEY (tenant_id, run_id) REFERENCES payroll_runs (tenant_id, id),
    CONSTRAINT fk_payroll_items_employee FOREIGN KEY (tenant_id, employee_id) REFERENCES employees (tenant_id, id),
    CONSTRAINT uq_payroll_items_version UNIQUE (tenant_id, run_id, employee_id, calc_version),
    CONSTRAINT ck_payroll_items_status CHECK (status IN ('OK', 'ERROR', 'EXCLUDED')),
    CONSTRAINT ck_payroll_items_ok_amounts CHECK (status <> 'OK' OR (gross IS NOT NULL AND deductions IS NOT NULL
        AND net IS NOT NULL AND net = gross - deductions AND net >= 0)),
    CONSTRAINT ck_payroll_items_ok_no_errors CHECK (status <> 'OK' OR cardinality(error_codes) = 0),
    CONSTRAINT ck_payroll_items_error_has_code CHECK (status <> 'ERROR' OR cardinality(error_codes) > 0),
    CONSTRAINT ck_payroll_items_excluded CHECK ((status = 'EXCLUDED') = (excluded_reason IS NOT NULL AND excluded_by IS NOT NULL)),
    CONSTRAINT ck_payroll_items_fingerprint CHECK (octet_length(input_fingerprint) = 32)
);
CREATE INDEX ix_payroll_items_employee ON payroll_items (tenant_id, employee_id, created_at DESC);
CREATE INDEX ix_payroll_items_review ON payroll_items (tenant_id, run_id, calc_version, employee_no, id);
CREATE INDEX ix_payroll_items_dept ON payroll_items (tenant_id, run_id, department_id);

-- ---------------------------------------------------------------- payroll_lines
CREATE TABLE payroll_lines (
    tenant_id     uuid          NOT NULL,
    id            uuid          NOT NULL,
    run_id        uuid          NOT NULL,
    calc_version  int           NOT NULL,
    item_id       uuid          NOT NULL,
    sort_order    smallint      NOT NULL,
    code          text          NOT NULL,
    name          text          NOT NULL,
    category      text          NOT NULL,
    stage         text          NOT NULL,
    amount        numeric(18,2) NOT NULL,
    basis         jsonb         NOT NULL DEFAULT '{}',
    explanation   text          NOT NULL,
    taxable       boolean       NOT NULL,
    rounding      text          NOT NULL,
    adjustment_id uuid,
    created_at    timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_payroll_lines PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_payroll_lines_item FOREIGN KEY (tenant_id, item_id) REFERENCES payroll_items (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT uq_payroll_lines_order UNIQUE (tenant_id, item_id, sort_order),
    CONSTRAINT ck_payroll_lines_amount CHECK (amount >= 0),
    CONSTRAINT ck_payroll_lines_category CHECK (category IN ('EARNING', 'DEDUCTION')),
    CONSTRAINT ck_payroll_lines_rounding CHECK (rounding IN ('HALF_UP', 'DOWN', 'UP'))
);
CREATE UNIQUE INDEX uq_payroll_lines_adjustment_per_run ON payroll_lines (tenant_id, run_id, calc_version, adjustment_id)
    WHERE adjustment_id IS NOT NULL;

-- ---------------------------------------------------------------- payroll_item_snapshots
CREATE TABLE payroll_item_snapshots (
    tenant_id         uuid        NOT NULL,
    item_id           uuid        NOT NULL,
    run_id            uuid        NOT NULL,
    calc_version      int         NOT NULL,
    snapshot_version  smallint    NOT NULL,
    input             jsonb       NOT NULL,
    ruleset_code      text        NOT NULL,
    ruleset_version   text        NOT NULL,
    input_fingerprint bytea       NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_payroll_item_snapshots PRIMARY KEY (tenant_id, item_id),
    CONSTRAINT fk_payroll_item_snapshots_item FOREIGN KEY (tenant_id, item_id) REFERENCES payroll_items (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT ck_payroll_item_snapshots_sha256 CHECK (octet_length(input_fingerprint) = 32)
);

-- ---------------------------------------------------------------- PI-04 (FOR SHARE) + PI-05
-- FOR SHARE pada baris run: INSERT/UPDATE/DELETE hasil terserialisasi dengan transaksi finalize
-- (yang meng-UPDATE baris run yang sama). Tanpa lock, chunk zombie bisa lolos saat finalize belum commit.
-- Pengecualian (final review R3-2): DELETE baris versi lama oleh job cleanup.
-- +goose StatementBegin
CREATE FUNCTION forbid_change_on_finalized_run() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE t uuid := COALESCE(NEW.tenant_id, OLD.tenant_id);
        r uuid := COALESCE(NEW.run_id, OLD.run_id);
BEGIN
  IF EXISTS (SELECT 1 FROM payrun.payroll_runs pr
             WHERE pr.tenant_id = t AND pr.id = r AND pr.status = 'FINALIZED'
               AND NOT (TG_OP = 'DELETE' AND OLD.calc_version < pr.calc_version)
             FOR SHARE) THEN
    RAISE EXCEPTION 'payroll run % is finalized', r USING ERRCODE = 'P0001';
  END IF;
  RETURN COALESCE(NEW, OLD);
END $$;
-- +goose StatementEnd
CREATE TRIGGER trg_payroll_items_immutable BEFORE INSERT OR UPDATE OR DELETE ON payroll_items
  FOR EACH ROW EXECUTE FUNCTION forbid_change_on_finalized_run();
CREATE TRIGGER trg_payroll_lines_immutable BEFORE INSERT OR UPDATE OR DELETE ON payroll_lines
  FOR EACH ROW EXECUTE FUNCTION forbid_change_on_finalized_run();
CREATE TRIGGER trg_payroll_item_snapshots_immutable BEFORE INSERT OR UPDATE OR DELETE ON payroll_item_snapshots
  FOR EACH ROW EXECUTE FUNCTION forbid_change_on_finalized_run();
-- Baris slip dan snapshot tidak pernah di-UPDATE (versi baru = item baru).
CREATE TRIGGER trg_payroll_lines_no_update BEFORE UPDATE ON payroll_lines
  FOR EACH ROW EXECUTE FUNCTION forbid_update_delete();
CREATE TRIGGER trg_payroll_item_snapshots_no_update BEFORE UPDATE ON payroll_item_snapshots
  FOR EACH ROW EXECUTE FUNCTION forbid_update_delete();

-- PI-05 (item ↔ line), deferred: line di-INSERT setelah item di transaksi yang sama.
-- +goose StatementBegin
CREATE FUNCTION payroll_items_sum_check() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE g numeric; d numeric;
BEGIN
  SELECT coalesce(sum(amount) FILTER (WHERE category = 'EARNING'), 0),
         coalesce(sum(amount) FILTER (WHERE category = 'DEDUCTION'), 0)
    INTO g, d
  FROM payrun.payroll_lines WHERE tenant_id = NEW.tenant_id AND item_id = NEW.id;
  IF g <> NEW.gross OR d <> NEW.deductions THEN
    RAISE EXCEPTION 'payroll item % lines (% / %) do not sum to gross/deductions (% / %)', NEW.id, g, d, NEW.gross, NEW.deductions
      USING ERRCODE = 'P0006';
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER trg_payroll_items_sum_check AFTER INSERT OR UPDATE ON payroll_items
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.status = 'OK')
  EXECUTE FUNCTION payroll_items_sum_check();

-- PI-05 (run ↔ item): diverifikasi setiap kali run masuk CALCULATED atau FINALIZED.
-- +goose StatementBegin
CREATE FUNCTION payroll_runs_totals_check() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE g numeric; d numeric; n numeric; c int;
BEGIN
  SELECT coalesce(sum(gross), 0), coalesce(sum(deductions), 0), coalesce(sum(net), 0), count(*)
    INTO g, d, n, c
  FROM payrun.payroll_items
  WHERE tenant_id = NEW.tenant_id AND run_id = NEW.id AND calc_version = NEW.calc_version AND status = 'OK';
  IF (g, d, n, c) IS DISTINCT FROM (NEW.total_gross, NEW.total_deductions, NEW.total_net, NEW.item_ok_count) THEN
    RAISE EXCEPTION 'payroll run % totals mismatch', NEW.id USING ERRCODE = 'P0006';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER trg_payroll_runs_totals_check BEFORE UPDATE OF status, total_gross, total_deductions, total_net, item_ok_count
  ON payroll_runs FOR EACH ROW WHEN (NEW.status IN ('CALCULATED', 'FINALIZED'))
  EXECUTE FUNCTION payroll_runs_totals_check();

-- ---------------------------------------------------------------- RLS + grants
ALTER TABLE payroll_periods        ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_periods        FORCE  ROW LEVEL SECURITY;
ALTER TABLE payroll_runs           ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_runs           FORCE  ROW LEVEL SECURITY;
ALTER TABLE payroll_items          ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_items          FORCE  ROW LEVEL SECURITY;
ALTER TABLE payroll_lines          ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_lines          FORCE  ROW LEVEL SECURITY;
ALTER TABLE payroll_item_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_item_snapshots FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON payroll_periods
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON payroll_runs
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON payroll_items
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON payroll_lines
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON payroll_item_snapshots
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));

-- Periode dan run tidak pernah dihapus (riwayat). Item/line/snapshot: DELETE untuk versi lama
-- (cleanup) dan penulisan ulang chunk; trigger PI-04 menolak semuanya setelah run final.
GRANT SELECT, INSERT, UPDATE ON payroll_periods, payroll_runs TO payrun_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON payroll_items TO payrun_app;
GRANT SELECT, INSERT, DELETE ON payroll_lines, payroll_item_snapshots TO payrun_app;
GRANT SELECT ON payroll_periods, payroll_runs, payroll_items, payroll_lines TO payrun_readonly;
-- Snapshot memuat input lengkap (termasuk nama/kalender) → hanya kolom metadata untuk readonly.
GRANT SELECT (tenant_id, item_id, run_id, calc_version, snapshot_version, ruleset_code, ruleset_version, created_at)
  ON payroll_item_snapshots TO payrun_readonly;

-- +goose Down
DROP TABLE payroll_item_snapshots;
DROP TABLE payroll_lines;
DROP TABLE payroll_items;
ALTER TABLE payroll_periods DROP CONSTRAINT fk_payroll_periods_finalized_run;
DROP TABLE payroll_runs;
DROP TABLE payroll_periods;
DROP FUNCTION payroll_runs_totals_check();
DROP FUNCTION payroll_items_sum_check();
DROP FUNCTION forbid_change_on_finalized_run();
DROP FUNCTION payroll_runs_terminal_immutable();
DROP FUNCTION payroll_periods_final_immutable();
