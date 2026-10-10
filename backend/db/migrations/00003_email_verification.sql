-- Sprint 1: verifikasi email (Step 12 R2-8; keputusan S1: tenant hasil signup berstatus
-- PENDING_VERIFICATION sampai email owner diverifikasi lewat link sekali pakai).
-- Desain: docs/design/13-db-rls-consistency-indexing.md, "Catatan implementasi (Sprint 1)".

-- +goose Up

ALTER TABLE users ADD COLUMN email_verified_at timestamptz;
GRANT SELECT (email_verified_at) ON users TO payrun_app, payrun_readonly;

ALTER TABLE tenants DROP CONSTRAINT ck_tenants_status;
ALTER TABLE tenants ADD CONSTRAINT ck_tenants_status CHECK (status IN ('PENDING_VERIFICATION', 'ACTIVE', 'SUSPENDED'));

-- Token verifikasi: hanya SHA-256 token (32 byte acak) + user_id. Tanpa RLS (allowlist, seperti
-- refresh_tokens): endpoint verifikasi tidak terautentikasi (link di email), lookup selalu berdasarkan
-- hash yang tidak bisa ditebak, dan tabel tidak memuat data tenant maupun PII. Hanya modul iam.
CREATE TABLE email_verifications (
    id         uuid        NOT NULL,
    user_id    uuid        NOT NULL,
    token_hash bytea       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    CONSTRAINT pk_email_verifications PRIMARY KEY (id),
    CONSTRAINT fk_email_verifications_user_id FOREIGN KEY (user_id) REFERENCES users (id),
    CONSTRAINT uq_email_verifications_token_hash UNIQUE (token_hash),
    CONSTRAINT ck_email_verifications_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT ck_email_verifications_expiry CHECK (expires_at > created_at)
);
CREATE INDEX ix_email_verifications_user_id ON email_verifications (user_id);
GRANT SELECT, INSERT, UPDATE ON email_verifications TO payrun_app;

-- +goose Down
DROP TABLE email_verifications;
ALTER TABLE tenants DROP CONSTRAINT ck_tenants_status;
ALTER TABLE tenants ADD CONSTRAINT ck_tenants_status CHECK (status IN ('ACTIVE', 'SUSPENDED'));
ALTER TABLE users DROP COLUMN email_verified_at;
