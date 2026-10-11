-- Sprint 2: master org (departments, positions), employees + employment_spans (PII terenkripsi,
-- ADR-P08), invitations (PF-02). Desain: docs/design/12-db-schema.md §2.3, §3.1–3.2, §4.1–4.2.
-- Setiap tabel tenant baru: RLS + FORCE + policy di file yang sama (catalogue test menolak jika tidak),
-- FK ke tabel tenant selalu composite (tenant_id, x_id).
--
-- Ditunda ke sprint pemiliknya: org_assignments (S3), trigger PeriodGuard employment_spans (S5/S6,
-- bersama payroll_periods).

-- +goose Up

-- ---------------------------------------------------------------- departments / positions
CREATE TABLE departments (
    tenant_id  uuid        NOT NULL,
    id         uuid        NOT NULL,
    code       text        NOT NULL,
    name       text        NOT NULL,
    status     text        NOT NULL DEFAULT 'ACTIVE',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    created_by uuid,
    updated_by uuid,
    version    int         NOT NULL DEFAULT 1,
    CONSTRAINT pk_departments PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_departments_tenant_id FOREIGN KEY (tenant_id) REFERENCES tenants (id),
    CONSTRAINT uq_departments_code UNIQUE (tenant_id, code),
    CONSTRAINT ck_departments_status CHECK (status IN ('ACTIVE', 'INACTIVE')),
    CONSTRAINT ck_departments_code_format CHECK (code ~ '^[A-Z0-9_]{1,20}$'),
    CONSTRAINT ck_departments_name_len CHECK (length(name) BETWEEN 1 AND 100)
);
CREATE TRIGGER trg_departments_updated_at BEFORE UPDATE ON departments
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE positions (
    tenant_id  uuid        NOT NULL,
    id         uuid        NOT NULL,
    code       text        NOT NULL,
    name       text        NOT NULL,
    status     text        NOT NULL DEFAULT 'ACTIVE',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    created_by uuid,
    updated_by uuid,
    version    int         NOT NULL DEFAULT 1,
    CONSTRAINT pk_positions PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_positions_tenant_id FOREIGN KEY (tenant_id) REFERENCES tenants (id),
    CONSTRAINT uq_positions_code UNIQUE (tenant_id, code),
    CONSTRAINT ck_positions_status CHECK (status IN ('ACTIVE', 'INACTIVE')),
    CONSTRAINT ck_positions_code_format CHECK (code ~ '^[A-Z0-9_]{1,20}$'),
    CONSTRAINT ck_positions_name_len CHECK (length(name) BETWEEN 1 AND 100)
);
CREATE TRIGGER trg_positions_updated_at BEFORE UPDATE ON positions
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- employees (PII terenkripsi)
-- *_enc: "<versi>:" + nonce + ciphertext+tag, AAD "<tenant_id>:employees.<field>" (ciphertext yang
-- disalin ke tenant/kolom lain gagal didekripsi). *_bidx: HMAC blind index (kunci terpisah) untuk
-- keunikan per tenant. *_last4: masking tanpa dekripsi.
CREATE TABLE employees (
    tenant_id           uuid        NOT NULL,
    id                  uuid        NOT NULL,
    employee_no         text        NOT NULL,
    full_name           text        NOT NULL,
    work_email          text,
    user_id             uuid,
    status              text        NOT NULL DEFAULT 'ACTIVE',
    ptkp_status         text        NOT NULL DEFAULT 'TK/0',
    nik_enc             bytea,
    nik_bidx            bytea,
    nik_last4           text,
    npwp_enc            bytea,
    npwp_bidx           bytea,
    npwp_last4          text,
    bank_name           text,
    bank_account_enc    bytea,
    bank_account_last4  text,
    bank_account_holder text,
    pii_key_version     text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    created_by          uuid,
    updated_by          uuid,
    version             int         NOT NULL DEFAULT 1,
    CONSTRAINT pk_employees PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_employees_tenant_id FOREIGN KEY (tenant_id) REFERENCES tenants (id),
    CONSTRAINT fk_employees_user_id FOREIGN KEY (user_id) REFERENCES users (id),
    CONSTRAINT uq_employees_employee_no UNIQUE (tenant_id, employee_no),
    CONSTRAINT uq_employees_nik_bidx UNIQUE (tenant_id, nik_bidx),
    CONSTRAINT uq_employees_npwp_bidx UNIQUE (tenant_id, npwp_bidx),
    CONSTRAINT uq_employees_user UNIQUE (tenant_id, user_id),
    CONSTRAINT ck_employees_status CHECK (status IN ('ACTIVE', 'RESIGNED', 'TERMINATED')),
    CONSTRAINT ck_employees_ptkp CHECK (ptkp_status IN ('TK/0', 'TK/1', 'TK/2', 'TK/3', 'K/0', 'K/1', 'K/2', 'K/3')),
    CONSTRAINT ck_employees_employee_no_format CHECK (employee_no ~ '^[A-Za-z0-9-]{1,30}$'),
    CONSTRAINT ck_employees_full_name_len CHECK (length(full_name) BETWEEN 1 AND 120),
    CONSTRAINT ck_employees_nik_pair CHECK ((nik_enc IS NULL) = (nik_bidx IS NULL) AND (nik_enc IS NULL) = (nik_last4 IS NULL)),
    CONSTRAINT ck_employees_npwp_pair CHECK ((npwp_enc IS NULL) = (npwp_bidx IS NULL) AND (npwp_enc IS NULL) = (npwp_last4 IS NULL)),
    CONSTRAINT ck_employees_bank_pair CHECK ((bank_account_enc IS NULL) = (bank_account_last4 IS NULL)),
    CONSTRAINT ck_employees_last4 CHECK ((nik_last4 IS NULL OR nik_last4 ~ '^[0-9]{4}$')
        AND (npwp_last4 IS NULL OR npwp_last4 ~ '^[0-9]{4}$')
        AND (bank_account_last4 IS NULL OR bank_account_last4 ~ '^[0-9]{4}$')),
    CONSTRAINT ck_employees_bidx_len CHECK ((nik_bidx IS NULL OR octet_length(nik_bidx) = 32)
        AND (npwp_bidx IS NULL OR octet_length(npwp_bidx) = 32))
);
-- Index FK fk_employees_user_id (final review C4); juga jalur "employee milik user ini" (self-service).
CREATE INDEX ix_employees_user_id ON employees (user_id);
CREATE INDEX ix_employees_name_prefix ON employees (tenant_id, lower(full_name) text_pattern_ops, id);
CREATE INDEX ix_employees_status ON employees (tenant_id, status);
CREATE TRIGGER trg_employees_updated_at BEFORE UPDATE ON employees
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- employment_spans (effective-dated)
CREATE TABLE employment_spans (
    tenant_id   uuid        NOT NULL,
    id          uuid        NOT NULL,
    employee_id uuid        NOT NULL,
    span        daterange   NOT NULL,
    end_reason  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid,
    updated_by  uuid,
    version     int         NOT NULL DEFAULT 1,
    CONSTRAINT pk_employment_spans PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_employment_spans_employee FOREIGN KEY (tenant_id, employee_id) REFERENCES employees (tenant_id, id),
    CONSTRAINT ex_employment_spans_no_overlap EXCLUDE USING gist (tenant_id WITH =, employee_id WITH =, span WITH &&),
    CONSTRAINT ck_employment_spans_range CHECK (NOT isempty(span) AND lower_inc(span) AND NOT upper_inc(span)
        AND lower(span) IS NOT NULL),
    CONSTRAINT ck_employment_spans_end_reason CHECK ((upper_inf(span)) = (end_reason IS NULL)
        AND (end_reason IS NULL OR end_reason IN ('RESIGNED', 'TERMINATED', 'CONTRACT_END')))
);
CREATE INDEX ix_employment_spans_employee ON employment_spans (tenant_id, employee_id);
CREATE TRIGGER trg_employment_spans_updated_at BEFORE UPDATE ON employment_spans
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- invitations
-- Token "<tenant_id>.<secret>": accept membuka WithTenantTxFor(tenant_id) lalu mencari
-- token_hash = sha256(secret) di bawah RLS tenant itu (12-db-schema §2.3). employee_id (opsional)
-- menautkan user ke employee saat accept.
CREATE TABLE invitations (
    tenant_id     uuid        NOT NULL,
    id            uuid        NOT NULL,
    membership_id uuid        NOT NULL,
    employee_id   uuid,
    token_hash    bytea       NOT NULL,
    email         text        NOT NULL,
    expires_at    timestamptz NOT NULL,
    accepted_at   timestamptz,
    revoked_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    CONSTRAINT pk_invitations PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_invitations_membership FOREIGN KEY (tenant_id, membership_id) REFERENCES memberships (tenant_id, id),
    CONSTRAINT fk_invitations_employee FOREIGN KEY (tenant_id, employee_id) REFERENCES employees (tenant_id, id),
    CONSTRAINT uq_invitations_token_hash UNIQUE (token_hash),
    CONSTRAINT ck_invitations_single_use CHECK (accepted_at IS NULL OR revoked_at IS NULL),
    CONSTRAINT ck_invitations_sha256 CHECK (octet_length(token_hash) = 32),
    CONSTRAINT ck_invitations_expiry CHECK (expires_at > created_at)
);
CREATE INDEX ix_invitations_membership ON invitations (tenant_id, membership_id);
CREATE INDEX ix_invitations_employee ON invitations (tenant_id, employee_id);

-- ---------------------------------------------------------------- RLS
ALTER TABLE departments      ENABLE ROW LEVEL SECURITY;
ALTER TABLE departments      FORCE  ROW LEVEL SECURITY;
ALTER TABLE positions        ENABLE ROW LEVEL SECURITY;
ALTER TABLE positions        FORCE  ROW LEVEL SECURITY;
ALTER TABLE employees        ENABLE ROW LEVEL SECURITY;
ALTER TABLE employees        FORCE  ROW LEVEL SECURITY;
ALTER TABLE employment_spans ENABLE ROW LEVEL SECURITY;
ALTER TABLE employment_spans FORCE  ROW LEVEL SECURITY;
ALTER TABLE invitations      ENABLE ROW LEVEL SECURITY;
ALTER TABLE invitations      FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON departments
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON positions
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON employees
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON employment_spans
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON invitations
  USING (tenant_id = (SELECT current_tenant())) WITH CHECK (tenant_id = (SELECT current_tenant()));

-- ---------------------------------------------------------------- grants (matriks §1.2)
-- Master data dan employee tidak pernah dihapus (deaktivasi / terminate), jadi tanpa DELETE.
GRANT SELECT, INSERT, UPDATE ON departments, positions, employees, employment_spans, invitations TO payrun_app;
-- payrun_readonly: tanpa kolom ciphertext/blind index (dump analis tidak membawa PII terenkripsi).
GRANT SELECT ON departments, positions, employment_spans TO payrun_readonly;
GRANT SELECT (tenant_id, id, employee_no, full_name, work_email, user_id, status, ptkp_status,
              nik_last4, npwp_last4, bank_name, bank_account_last4, bank_account_holder, pii_key_version,
              created_at, updated_at, created_by, updated_by, version) ON employees TO payrun_readonly;
GRANT SELECT (tenant_id, id, membership_id, employee_id, email, expires_at, accepted_at, revoked_at,
              created_at, created_by) ON invitations TO payrun_readonly;

-- +goose Down
DROP TABLE invitations;
DROP TABLE employment_spans;
DROP TABLE employees;
DROP TABLE positions;
DROP TABLE departments;
