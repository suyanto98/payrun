-- Sprint 4 (U5): undangan INVITED yang dicabut menjadi REVOKED tanpa pernah punya user. CHECK lama
-- (`status = 'INVITED' OR user_id IS NOT NULL`) menolaknya. Maksud constraint sebenarnya: membership
-- ACTIVE wajib punya user; INVITED dan REVOKED boleh tanpa user.

-- +goose Up
ALTER TABLE memberships DROP CONSTRAINT ck_memberships_user_when_active;
ALTER TABLE memberships ADD CONSTRAINT ck_memberships_user_when_active CHECK (status <> 'ACTIVE' OR user_id IS NOT NULL);

-- +goose Down
-- Gagal kalau sudah ada undangan yang dicabut (REVOKED tanpa user); hapus/arsipkan baris itu dulu.
ALTER TABLE memberships DROP CONSTRAINT ck_memberships_user_when_active;
ALTER TABLE memberships ADD CONSTRAINT ck_memberships_user_when_active CHECK (status = 'INVITED' OR user_id IS NOT NULL);
