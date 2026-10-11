-- HANYA image demo (build tag `demo`): sandbox per pengunjung demo publik ("Coba demo tanpa daftar").
-- Satu sandbox = satu tenant baru (perusahaan contoh) + tiga persona (admin payroll, owner, karyawan), dibuat
-- lewat service asli. Baris dicatat PALING AWAL (transaksi yang sama dengan tenant), sehingga sandbox yang
-- gagal di tengah pembuatan tetap ikut kedaluwarsa dan dibersihkan.
--
-- Tanpa RLS, sama seperti demo_faults: dibaca job kedaluwarsa di luar tenant context dan saat pergantian
-- persona (user → sandbox-nya). Filter dilakukan eksplisit: user harus anggota sandbox (member_user_ids).

-- +goose Up
CREATE TABLE demo_sandboxes (
    id               uuid        NOT NULL,
    tenant_id        uuid        NOT NULL,
    payroll_user_id  uuid        NOT NULL,   -- persona awal: Dina, admin payroll
    owner_user_id    uuid        NOT NULL,   -- Pak Hendra, owner (approver)
    employee_user_id uuid        NOT NULL,   -- Rizky, karyawan (slip gaji, cuti)
    member_user_ids  uuid[]      NOT NULL,
    created_at       timestamptz NOT NULL,
    expires_at       timestamptz NOT NULL,
    expired_at       timestamptz,
    CONSTRAINT pk_demo_sandboxes PRIMARY KEY (id),
    CONSTRAINT uq_demo_sandboxes_tenant UNIQUE (tenant_id),
    CONSTRAINT ck_demo_sandboxes_expiry CHECK (expires_at > created_at)
);
CREATE INDEX ix_demo_sandboxes_created_at ON demo_sandboxes (created_at);
CREATE INDEX ix_demo_sandboxes_due ON demo_sandboxes (expires_at) WHERE expired_at IS NULL;
CREATE INDEX ix_demo_sandboxes_members ON demo_sandboxes USING gin (member_user_ids);
GRANT SELECT, INSERT, UPDATE ON demo_sandboxes TO payrun_app;

-- +goose Down
DROP TABLE demo_sandboxes;
