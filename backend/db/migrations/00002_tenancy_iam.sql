-- Sprint 1: tenancy + IAM + tabel shared (docs/design/10-db-shared-tables.md, 12-db-schema.md §1–§2,
-- 13-db-rls-consistency-indexing.md §1–§2). Setiap tabel ber-tenant lahir dengan ENABLE + FORCE RLS
-- dan policy-nya di migrasi yang sama (PRD: "tabel tenant baru wajib RLS di migrasi yang sama").
--
-- Mode eksklusif (final review C2): transaksi tenant hanya punya app.tenant_id, transaksi platform hanya
-- app.user_id. Policy "self" mensyaratkan current_tenant() IS NULL. Semua policy memakai helper
-- current_tenant()/current_user_id() (nullif '' → NULL; final review C3), dibungkus subselect (initplan).

-- +goose Up

-- ---------------------------------------------------------------- users (global, RLS self + member)
CREATE TABLE users (
    id                 uuid        NOT NULL,
    email              text        NOT NULL,
    password_hash      text        NOT NULL,
    display_name       text        NOT NULL,
    status             text        NOT NULL DEFAULT 'ACTIVE',
    failed_login_count int         NOT NULL DEFAULT 0,
    locked_until       timestamptz,
    last_login_at      timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    version            int         NOT NULL DEFAULT 1,
    CONSTRAINT pk_users PRIMARY KEY (id),
    CONSTRAINT ck_users_status CHECK (status IN ('ACTIVE', 'LOCKED', 'DISABLED')),
    CONSTRAINT ck_users_email_format CHECK (position('@' in email) > 1 AND length(email) <= 254),
    CONSTRAINT ck_users_failed_login_non_negative CHECK (failed_login_count >= 0),
    CONSTRAINT ck_users_display_name_len CHECK (length(display_name) BETWEEN 1 AND 80)
);
-- Unik case-insensitive; nama index = nama yang dipetakan ke EMAIL_ALREADY_REGISTERED. Unique index
-- tidak difilter RLS, jadi email user tenant lain tetap terdeteksi saat signup.
CREATE UNIQUE INDEX uq_users_email_lower ON users (lower(email));
CREATE TRIGGER trg_users_updated_at BEFORE UPDATE ON users
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- tenants (akar tenant)
CREATE TABLE tenants (
    id           uuid        NOT NULL,
    slug         text        NOT NULL,
    name         text        NOT NULL,
    status       text        NOT NULL DEFAULT 'ACTIVE',
    suspended_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_tenants PRIMARY KEY (id),
    CONSTRAINT ck_tenants_status CHECK (status IN ('ACTIVE', 'SUSPENDED')),
    CONSTRAINT ck_tenants_slug_format CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
    CONSTRAINT ck_tenants_suspended CHECK ((status = 'SUSPENDED') = (suspended_at IS NOT NULL)),
    CONSTRAINT ck_tenants_name_len CHECK (length(name) BETWEEN 2 AND 120)
);
CREATE UNIQUE INDEX uq_tenants_slug ON tenants (lower(slug));
CREATE TRIGGER trg_tenants_updated_at BEFORE UPDATE ON tenants
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- tenant_settings (1:1)
CREATE TABLE tenant_settings (
    tenant_id                    uuid         NOT NULL,
    timezone                     text         NOT NULL DEFAULT 'Asia/Jakarta',
    work_days                    smallint[]   NOT NULL DEFAULT '{1,2,3,4,5}',
    proration_basis              text         NOT NULL DEFAULT 'WORKING_DAYS',
    cutoff_day_of_next_month     smallint     NOT NULL DEFAULT 2,
    cutoff_time_local            time         NOT NULL DEFAULT '23:59',
    overtime_basis               text         NOT NULL DEFAULT 'BASE',
    overtime_max_hours_per_day   numeric(4,1)          DEFAULT 4.0,
    incomplete_attendance_policy text         NOT NULL DEFAULT 'COUNT_AS_PRESENT',
    ruleset_code                 text         NOT NULL DEFAULT 'ID-SIMPLIFIED',
    net_change_warning_pct       numeric(5,2) NOT NULL DEFAULT 20.00,
    created_at                   timestamptz  NOT NULL DEFAULT now(),
    updated_at                   timestamptz  NOT NULL DEFAULT now(),
    created_by                   uuid,
    updated_by                   uuid,
    version                      int          NOT NULL DEFAULT 1,
    CONSTRAINT pk_tenant_settings PRIMARY KEY (tenant_id),
    CONSTRAINT fk_tenant_settings_tenant_id FOREIGN KEY (tenant_id) REFERENCES tenants (id),
    CONSTRAINT ck_tenant_settings_work_days CHECK (work_days <@ '{1,2,3,4,5,6,7}' AND cardinality(work_days) BETWEEN 1 AND 7),
    CONSTRAINT ck_tenant_settings_proration CHECK (proration_basis IN ('WORKING_DAYS', 'CALENDAR_DAYS')),
    CONSTRAINT ck_tenant_settings_cutoff_day CHECK (cutoff_day_of_next_month BETWEEN 1 AND 28),
    CONSTRAINT ck_tenant_settings_overtime_basis CHECK (overtime_basis IN ('BASE', 'BASE_PLUS_FIXED')),
    CONSTRAINT ck_tenant_settings_incomplete CHECK (incomplete_attendance_policy IN ('COUNT_AS_PRESENT', 'COUNT_AS_ABSENT'))
);
CREATE TRIGGER trg_tenant_settings_updated_at BEFORE UPDATE ON tenant_settings
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- memberships
CREATE TABLE memberships (
    tenant_id     uuid        NOT NULL,
    id            uuid        NOT NULL,
    user_id       uuid,
    invited_email text,
    roles         text[]      NOT NULL,
    status        text        NOT NULL DEFAULT 'INVITED',
    accepted_at   timestamptz,
    revoked_at    timestamptz,
    revoked_by    uuid,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    updated_by    uuid,
    version       int         NOT NULL DEFAULT 1,
    CONSTRAINT pk_memberships PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_memberships_tenant_id FOREIGN KEY (tenant_id) REFERENCES tenants (id),
    CONSTRAINT fk_memberships_user_id FOREIGN KEY (user_id) REFERENCES users (id),
    CONSTRAINT uq_memberships_user UNIQUE (tenant_id, user_id),
    CONSTRAINT ck_memberships_status CHECK (status IN ('INVITED', 'ACTIVE', 'REVOKED')),
    CONSTRAINT ck_memberships_roles CHECK (cardinality(roles) >= 1
        AND roles <@ ARRAY['OWNER', 'HR_ADMIN', 'PAYROLL_ADMIN', 'MANAGER', 'EMPLOYEE']),
    CONSTRAINT ck_memberships_user_when_active CHECK (status = 'INVITED' OR user_id IS NOT NULL),
    CONSTRAINT ck_memberships_invite_target CHECK (user_id IS NOT NULL OR invited_email IS NOT NULL)
);
CREATE UNIQUE INDEX uq_memberships_invited_email ON memberships (tenant_id, lower(invited_email)) WHERE status = 'INVITED';
-- Desain: partial (user_id) WHERE status = 'ACTIVE'. Dibuat PENUH karena juga menjadi index FK
-- fk_memberships_user_id (final review C4: index FK harus mencakup semua baris). Jalur login/daftar tenant
-- (lintas tenant, satu-satunya index yang tidak diawali tenant_id) tetap memakainya.
CREATE INDEX ix_memberships_user ON memberships (user_id);
CREATE TRIGGER trg_memberships_updated_at BEFORE UPDATE ON memberships
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- PI-16: setiap tenant selalu punya minimal satu OWNER aktif. Deferred: transfer kepemilikan
-- (tambah owner baru, cabut yang lama) dalam satu transaksi tetap lolos.
-- +goose StatementBegin
CREATE FUNCTION ensure_tenant_has_owner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM payrun.memberships m
                 WHERE m.tenant_id = OLD.tenant_id AND m.status = 'ACTIVE'
                   AND 'OWNER' = ANY (m.roles)) THEN
    RAISE EXCEPTION 'tenant % must keep at least one active OWNER', OLD.tenant_id
      USING ERRCODE = 'P0004';
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER memberships_owner_guard
  AFTER UPDATE OR DELETE ON memberships
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW
  WHEN (('OWNER' = ANY (OLD.roles)) AND OLD.status = 'ACTIVE')
  EXECUTE FUNCTION ensure_tenant_has_owner();

-- ---------------------------------------------------------------- refresh_tokens (tanpa RLS, §2.2)
CREATE TABLE refresh_tokens (
    id                uuid        NOT NULL,
    user_id           uuid        NOT NULL,
    family_id         uuid        NOT NULL,
    token_hash        bytea       NOT NULL,
    parent_id         uuid,
    replaced_by       uuid,
    issued_at         timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL,
    family_expires_at timestamptz NOT NULL,
    used_at           timestamptz,
    revoked_at        timestamptz,
    revoke_reason     text,
    user_agent        text,
    ip                inet,
    CONSTRAINT pk_refresh_tokens PRIMARY KEY (id),
    CONSTRAINT fk_refresh_tokens_user_id FOREIGN KEY (user_id) REFERENCES users (id),
    CONSTRAINT fk_refresh_tokens_parent_id FOREIGN KEY (parent_id) REFERENCES refresh_tokens (id),
    CONSTRAINT uq_refresh_tokens_token_hash UNIQUE (token_hash),
    CONSTRAINT ck_refresh_tokens_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT ck_refresh_tokens_expiry CHECK (expires_at > issued_at AND expires_at <= family_expires_at),
    CONSTRAINT ck_refresh_tokens_revoke_reason CHECK (revoke_reason IS NULL OR revoke_reason IN
        ('LOGOUT', 'REUSE_DETECTED', 'PASSWORD_CHANGED', 'ADMIN', 'MEMBERSHIP_REVOKED')),
    CONSTRAINT ck_refresh_tokens_revoked_has_reason CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL))
);
CREATE INDEX ix_refresh_tokens_user_id       ON refresh_tokens (user_id);
CREATE INDEX ix_refresh_tokens_parent_id     ON refresh_tokens (parent_id);
CREATE INDEX ix_refresh_tokens_family_active ON refresh_tokens (family_id) WHERE revoked_at IS NULL;
CREATE INDEX ix_refresh_tokens_expires_at    ON refresh_tokens (expires_at);

-- ---------------------------------------------------------------- idempotency_keys (tenant-scoped)
CREATE TABLE idempotency_keys (
    tenant_id       uuid        NOT NULL,
    id              uuid        NOT NULL,
    client_id       uuid        NOT NULL,
    endpoint        text        NOT NULL,
    key             text        NOT NULL,
    request_hash    bytea       NOT NULL,
    response_status smallint,
    response_body   jsonb,
    resource_type   text,
    resource_id     uuid,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_idempotency_keys PRIMARY KEY (tenant_id, id),
    CONSTRAINT fk_idempotency_keys_tenant_id FOREIGN KEY (tenant_id) REFERENCES tenants (id),
    CONSTRAINT uq_idempotency_keys_scope UNIQUE (tenant_id, client_id, endpoint, key),
    CONSTRAINT ck_idempotency_keys_key_len CHECK (length(key) BETWEEN 8 AND 255),
    CONSTRAINT ck_idempotency_keys_hash_len CHECK (octet_length(request_hash) = 32),
    CONSTRAINT ck_idempotency_keys_response_pair CHECK ((response_status IS NULL) = (response_body IS NULL))
);
CREATE INDEX ix_idempotency_keys_expires_at ON idempotency_keys (expires_at);
CREATE TRIGGER trg_idempotency_keys_updated_at BEFORE UPDATE ON idempotency_keys
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- platform_idempotency_keys (signup)
CREATE TABLE platform_idempotency_keys (
    id              uuid        NOT NULL,
    endpoint        text        NOT NULL,
    key             text        NOT NULL,
    request_hash    bytea       NOT NULL,
    response_status smallint,
    response_body   jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    CONSTRAINT pk_platform_idempotency_keys PRIMARY KEY (id),
    CONSTRAINT uq_platform_idempotency_keys_scope UNIQUE (endpoint, key),
    CONSTRAINT ck_platform_idempotency_keys_key_len CHECK (length(key) BETWEEN 8 AND 255),
    CONSTRAINT ck_platform_idempotency_keys_hash_len CHECK (octet_length(request_hash) = 32),
    CONSTRAINT ck_platform_idempotency_keys_response_pair CHECK ((response_status IS NULL) = (response_body IS NULL))
);
CREATE INDEX ix_platform_idempotency_keys_expires_at ON platform_idempotency_keys (expires_at);

-- ---------------------------------------------------------------- jobs (tanpa RLS; dijaga trigger)
CREATE TABLE jobs (
    id             uuid        NOT NULL,
    kind           text        NOT NULL,
    payload        jsonb       NOT NULL DEFAULT '{}',
    tenant_id      uuid,
    status         text        NOT NULL DEFAULT 'queued',
    attempts       int         NOT NULL DEFAULT 0,
    max_attempts   int         NOT NULL DEFAULT 10,
    run_at         timestamptz NOT NULL DEFAULT now(),
    locked_by      text,
    locked_until   timestamptz,
    last_error     text,
    dedupe_key     text,
    correlation_id text,
    trace_parent   text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    completed_at   timestamptz,
    CONSTRAINT pk_jobs PRIMARY KEY (id),
    CONSTRAINT ck_jobs_status CHECK (status IN ('queued', 'running', 'succeeded', 'dead')),
    CONSTRAINT ck_jobs_attempts CHECK (attempts >= 0 AND max_attempts > 0),
    CONSTRAINT ck_jobs_running_has_lease CHECK (status <> 'running' OR (locked_by IS NOT NULL AND locked_until IS NOT NULL)),
    CONSTRAINT ck_jobs_terminal_has_completed CHECK (status NOT IN ('succeeded', 'dead') OR completed_at IS NOT NULL)
);
CREATE UNIQUE INDEX uq_jobs_dedupe_key ON jobs (dedupe_key) WHERE dedupe_key IS NOT NULL AND status IN ('queued', 'running');
CREATE INDEX ix_jobs_claimable         ON jobs (run_at)            WHERE status = 'queued';
CREATE INDEX ix_jobs_lease_expired     ON jobs (locked_until)      WHERE status = 'running';
CREATE INDEX ix_jobs_dead              ON jobs (kind, completed_at) WHERE status = 'dead';
CREATE INDEX ix_jobs_succeeded_cleanup ON jobs (completed_at)      WHERE status = 'succeeded';
CREATE TRIGGER trg_jobs_updated_at BEFORE UPDATE ON jobs
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- jobs.tenant_id harus sama dengan tenant transaksi yang meng-enqueue (shared §4.1): request tenant A
-- tidak bisa membuat job untuk tenant B, dan job tenant tidak bisa dibuat di luar transaksi tenant.
-- +goose StatementBegin
CREATE FUNCTION jobs_enforce_tenant() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE ctx uuid := payrun.current_tenant();
BEGIN
  IF ctx IS NOT NULL AND NEW.tenant_id IS DISTINCT FROM ctx THEN
    RAISE EXCEPTION 'jobs.tenant_id (%) must match app.tenant_id (%)', NEW.tenant_id, ctx
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ck_jobs_tenant_matches_context';
  END IF;
  IF ctx IS NULL AND NEW.tenant_id IS NOT NULL THEN
    RAISE EXCEPTION 'tenant job must be enqueued inside a tenant transaction'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ck_jobs_tenant_matches_context';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER trg_jobs_enforce_tenant BEFORE INSERT ON jobs
  FOR EACH ROW EXECUTE FUNCTION jobs_enforce_tenant();

-- ---------------------------------------------------------------- audit_log (append-only)
CREATE TABLE audit_log (
    id             uuid        NOT NULL,
    tenant_id      uuid,
    actor_type     text        NOT NULL,
    actor_id       uuid,
    action         text        NOT NULL,
    target_type    text,
    target_id      uuid,
    request_id     text,
    correlation_id text,
    ip             inet,
    user_agent     text,
    diff           jsonb,
    reason         text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_audit_log PRIMARY KEY (id),
    CONSTRAINT ck_audit_log_actor_type CHECK (actor_type IN ('USER', 'SYSTEM', 'PLATFORM_OPERATOR')),
    CONSTRAINT ck_audit_log_action_format CHECK (action ~ '^[a-z_]+\.[a-z_]+$'),
    CONSTRAINT ck_audit_log_actor CHECK (actor_type = 'SYSTEM' OR actor_id IS NOT NULL)
);
CREATE INDEX ix_audit_log_tenant_target  ON audit_log (tenant_id, target_type, target_id, created_at DESC);
CREATE INDEX ix_audit_log_tenant_actor   ON audit_log (tenant_id, actor_id, created_at DESC);
CREATE INDEX ix_audit_log_tenant_action  ON audit_log (tenant_id, action, created_at DESC);
CREATE INDEX ix_audit_log_tenant_created ON audit_log (tenant_id, created_at DESC, id);
CREATE TRIGGER trg_audit_log_append_only BEFORE UPDATE OR DELETE ON audit_log
  FOR EACH ROW EXECUTE FUNCTION forbid_update_delete();

-- ---------------------------------------------------------------- RLS
ALTER TABLE users           ENABLE ROW LEVEL SECURITY;
ALTER TABLE users           FORCE  ROW LEVEL SECURITY;
ALTER TABLE tenants         ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants         FORCE  ROW LEVEL SECURITY;
ALTER TABLE tenant_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_settings FORCE  ROW LEVEL SECURITY;
ALTER TABLE memberships     ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships     FORCE  ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE  ROW LEVEL SECURITY;
ALTER TABLE audit_log       ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log       FORCE  ROW LEVEL SECURITY;

-- Template tenant.
CREATE POLICY tenant_isolation ON tenant_settings
  USING      (tenant_id = (SELECT current_tenant()))
  WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON memberships
  USING      (tenant_id = (SELECT current_tenant()))
  WITH CHECK (tenant_id = (SELECT current_tenant()));
CREATE POLICY tenant_isolation ON idempotency_keys
  USING      (tenant_id = (SELECT current_tenant()))
  WITH CHECK (tenant_id = (SELECT current_tenant()));

-- memberships: baca membership milik sendiri, HANYA di mode platform (login, daftar tenant, switch).
-- FOR SELECT saja: semua penulisan membership lewat WithTenantTx.
CREATE POLICY memberships_self_read ON memberships FOR SELECT
  USING ((SELECT current_tenant()) IS NULL
         AND user_id = (SELECT current_user_id()));

-- tenants: tenant aktif; di mode platform, tenant tempat user punya membership ACTIVE.
CREATE POLICY tenants_current ON tenants
  USING (id = (SELECT current_tenant())
         OR ((SELECT current_tenant()) IS NULL
             AND EXISTS (SELECT 1 FROM memberships m
                         WHERE m.tenant_id = tenants.id AND m.status = 'ACTIVE'
                           AND m.user_id = (SELECT current_user_id()))))
  WITH CHECK (id = (SELECT current_tenant()));

-- users: diri sendiri (mode platform) + anggota ACTIVE tenant aktif (baca saja).
CREATE POLICY users_self ON users
  USING      ((SELECT current_tenant()) IS NULL AND id = (SELECT current_user_id()))
  WITH CHECK ((SELECT current_tenant()) IS NULL AND id = (SELECT current_user_id()));
CREATE POLICY users_tenant_member_read ON users FOR SELECT
  USING (EXISTS (SELECT 1 FROM memberships m
                 WHERE m.user_id = users.id AND m.status = 'ACTIVE'
                   AND m.tenant_id = (SELECT current_tenant())));
-- auth_lookup_user (SECURITY DEFINER) berjalan sebagai payrun_owner, dan FORCE RLS berlaku juga untuk
-- owner: tanpa policy ini fungsi login selalu melihat 0 baris. Policy ini hanya untuk payrun_owner
-- (role migrasi, kredensialnya tidak ada di container api/worker), SELECT saja.
CREATE POLICY users_owner_auth_lookup ON users FOR SELECT TO payrun_owner
  USING (true);

-- audit_log: baris tenant aktif; baris platform (tenant_id NULL) hanya boleh ditulis tanpa tenant
-- context, dan hanya dibaca role investigasi.
CREATE POLICY audit_tenant ON audit_log
  USING      (tenant_id = (SELECT current_tenant()))
  WITH CHECK (tenant_id = (SELECT current_tenant())
              OR (tenant_id IS NULL AND (SELECT current_tenant()) IS NULL));
CREATE POLICY audit_platform_read ON audit_log FOR SELECT TO payrun_readonly
  USING (tenant_id IS NULL);

-- ---------------------------------------------------------------- login lookup (satu-satunya SECURITY DEFINER)
-- Satu baris untuk email persis; search_path dikunci (cegah hijacking). Dipanggil hanya dari login
-- dan signup (akun yang sudah ada), keduanya di-rate-limit auth-strict.
CREATE FUNCTION auth_lookup_user(p_email text)
RETURNS TABLE (id uuid, password_hash text, status text, failed_login_count int, locked_until timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = payrun, pg_temp AS $$
  SELECT u.id, u.password_hash, u.status, u.failed_login_count, u.locked_until
  FROM users u WHERE lower(u.email) = lower(p_email) LIMIT 1
$$;
REVOKE ALL ON FUNCTION auth_lookup_user(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_lookup_user(text) TO payrun_app;

-- ---------------------------------------------------------------- grants (matriks §1.2, per tabel)
-- users: password_hash TIDAK bisa dibaca payrun_app (final review R2-7); hash hanya lewat auth_lookup_user.
GRANT SELECT (id, email, display_name, status, failed_login_count, locked_until,
              last_login_at, created_at, updated_at, version) ON users TO payrun_app;
GRANT INSERT, UPDATE ON users TO payrun_app;
GRANT SELECT, INSERT, UPDATE ON tenants, tenant_settings, memberships, refresh_tokens TO payrun_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys, platform_idempotency_keys, jobs TO payrun_app;
GRANT SELECT, INSERT ON audit_log TO payrun_app;                -- append-only: tanpa UPDATE/DELETE

-- payrun_readonly: investigasi, tetap di bawah RLS (password_hash dan token_hash tidak terbaca).
GRANT SELECT (id, email, display_name, status, failed_login_count, locked_until,
              last_login_at, created_at, updated_at, version) ON users TO payrun_readonly;
GRANT SELECT ON tenants, tenant_settings, memberships, audit_log, jobs TO payrun_readonly;

-- +goose Down
DROP FUNCTION auth_lookup_user(text);
DROP TABLE audit_log;
DROP TABLE jobs;
DROP FUNCTION jobs_enforce_tenant();
DROP TABLE platform_idempotency_keys;
DROP TABLE idempotency_keys;
DROP TABLE refresh_tokens;
DROP TABLE memberships;
DROP FUNCTION ensure_tenant_has_owner();
DROP TABLE tenant_settings;
DROP TABLE tenants;
DROP TABLE users;
