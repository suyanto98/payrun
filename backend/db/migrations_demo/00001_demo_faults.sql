-- HANYA image demo (build tag `demo`): titik kegagalan untuk demo live (O15/O18, docs/design/27-docker-compose.md
-- §9.2). Versi dicatat di tabel goose terpisah (goose_demo_version): database release tidak pernah punya
-- tabel ini dan skema release tetap identik dengan skema yang dites.
--
-- Tanpa RLS: dibaca worker di luar tenant context (faults.Check memakai pool, supaya counter `fired` ter-commit
-- walau proses mati di tengah transaksi bisnis). Filter tenant dilakukan eksplisit lewat kolom tenant_id.

-- +goose Up
CREATE TABLE demo_faults (
    id          uuid        NOT NULL,
    point       text        NOT NULL,
    target_kind text,
    tenant_id   uuid,
    after_n     int         NOT NULL DEFAULT 0,
    times       int         NOT NULL DEFAULT 1,
    fired       int         NOT NULL DEFAULT 0,
    action      text        NOT NULL DEFAULT 'exit',
    delay_ms    int,
    created_by  uuid,
    expires_at  timestamptz NOT NULL DEFAULT now() + interval '15 minutes',
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_demo_faults PRIMARY KEY (id),
    CONSTRAINT ck_demo_faults_point CHECK (point IN ('payroll.calc_chunk.before_commit', 'payslip.render.after_put')),
    CONSTRAINT ck_demo_faults_action CHECK (action IN ('exit', 'panic', 'delay')),
    CONSTRAINT ck_demo_faults_counts CHECK (after_n >= 0 AND times >= 1 AND times <= 10 AND fired >= 0),
    CONSTRAINT ck_demo_faults_delay CHECK ((action = 'delay') = (delay_ms IS NOT NULL))
);
CREATE INDEX ix_demo_faults_point ON demo_faults (point, tenant_id);
GRANT SELECT, INSERT, UPDATE, DELETE ON demo_faults TO payrun_app;

-- +goose Down
DROP TABLE demo_faults;
