-- Modul iam (ADR-009, ADR-P02). Mode transaksi tiap query ditulis di komentarnya:
--   [none]     tanpa context (auth_lookup_user, refresh_tokens: tanpa RLS)
--   [platform] db.WithPlatformTx(user): users self, memberships self (baca), tenants milik user
--   [tenant]   db.WithTenantTxFor(tenant)

-- auth_lookup_user (SECURITY DEFINER, [none]) ditulis tangan di internal/modules/iam/lookup.go: sqlc
-- tidak bisa menyimpulkan kolom fungsi RETURNS TABLE.

-- name: RecordLoginSuccess :exec
-- [platform]
UPDATE users
SET last_login_at = @now, failed_login_count = 0, locked_until = NULL
WHERE id = @id;

-- name: RecordLoginFailure :one
-- [platform] Lockout bertahap (C-03): increment atomik di SQL. Mulai gagal ke-threshold: kunci
-- base_secs * 2^(n - threshold), maksimal max_secs. Eksponen dibatasi 30 SEBELUM power(): least() di luar
-- tidak mencegah overflow float8 (± 1000 gagal di atas threshold), yang membuat lockout berhenti bekerja.
UPDATE users
SET failed_login_count = failed_login_count + 1,
    locked_until = CASE
      WHEN failed_login_count + 1 >= sqlc.arg(threshold)::int
      THEN sqlc.arg(now)::timestamptz + make_interval(secs => least(sqlc.arg(base_secs)::float8 * power(2, least(failed_login_count + 1 - sqlc.arg(threshold)::int, 30)), sqlc.arg(max_secs)::float8))
      ELSE locked_until
    END
WHERE id = sqlc.arg(id)
RETURNING failed_login_count, locked_until;

-- name: GetUser :one
-- [platform]
SELECT id, email, display_name, status, email_verified_at
FROM users
WHERE id = @id;

-- name: CreateUser :exec
-- [platform] app.user_id = id baru (WITH CHECK users_self). Duplikat email → uq_users_email_lower.
INSERT INTO users (id, email, password_hash, display_name)
VALUES (@id, @email, @password_hash, @display_name);

-- name: AddOwnerMembership :exec
-- [tenant] Membership OWNER pendaftar tenant (signup).
INSERT INTO memberships (tenant_id, id, user_id, roles, status, accepted_at, created_by)
VALUES (@tenant_id, @id, @user_id, ARRAY['OWNER'], 'ACTIVE', @now, @user_id);

-- name: MarkEmailVerified :execrows
-- [platform]
UPDATE users SET email_verified_at = @now WHERE id = @id AND email_verified_at IS NULL;

-- name: ReclaimUnverifiedUser :execrows
-- [platform] Signup ulang dengan email yang BELUM terverifikasi dan password berbeda (review S1: cegah
-- pre-hijacking): kredensial pendaftar sebelumnya diganti. Guard email_verified_at IS NULL di WHERE:
-- verifikasi paralel yang menang lebih dulu membuat 0 baris (pemanggil menolak signup).
UPDATE users
SET password_hash = @password_hash, display_name = @display_name, failed_login_count = 0, locked_until = NULL
WHERE id = @id AND email_verified_at IS NULL AND status = 'ACTIVE';

-- name: RevokeUserRefreshTokens :exec
-- [none] Semua sesi user dicabut (kredensial berganti).
UPDATE refresh_tokens SET revoked_at = @now, revoke_reason = @reason
WHERE user_id = @user_id AND revoked_at IS NULL;

-- name: GetEmailVerification :one
-- [none] Tanpa lock: dipakai sebelum memeriksa password (argon2 tidak dijalankan sambil memegang lock).
SELECT user_id FROM email_verifications WHERE token_hash = @token_hash;

-- name: CreateEmailVerification :exec
-- [none]
INSERT INTO email_verifications (id, user_id, token_hash, created_at, expires_at)
VALUES (@id, @user_id, @token_hash, @created_at, @expires_at);

-- name: InvalidatePendingVerifications :exec
-- [none] Link lama mati saat link baru dikirim (hanya link terakhir yang berlaku).
UPDATE email_verifications SET used_at = @now WHERE user_id = @user_id AND used_at IS NULL;

-- name: GetEmailVerificationForUpdate :one
-- [none] FOR UPDATE: dua klik paralel pada link yang sama diserialkan; yang kedua melihat used_at.
SELECT id, user_id, expires_at, used_at
FROM email_verifications
WHERE token_hash = @token_hash
FOR UPDATE;

-- name: MarkEmailVerificationUsed :exec
-- [none]
UPDATE email_verifications SET used_at = @now WHERE id = @id;

-- name: ListActiveMemberships :many
-- [platform] Tenant terlihat hanya lewat cabang self tenants_current (membership ACTIVE).
SELECT m.tenant_id, t.name AS tenant_name, t.status AS tenant_status, m.roles
FROM memberships m
JOIN tenants t ON t.id = m.tenant_id
WHERE m.user_id = sqlc.arg(user_id)::uuid AND m.status = 'ACTIVE'
ORDER BY t.name, m.tenant_id
LIMIT 100;

-- name: GetActiveMembership :one
-- [platform] Cek membership untuk menerbitkan `tid` (login, refresh, switch-tenant).
SELECT m.tenant_id, t.name AS tenant_name, t.status AS tenant_status, m.roles
FROM memberships m
JOIN tenants t ON t.id = m.tenant_id
WHERE m.user_id = sqlc.arg(user_id)::uuid AND m.tenant_id = sqlc.arg(tenant_id)::uuid AND m.status = 'ACTIVE';

-- name: CreateRefreshToken :exec
-- [none]
INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, parent_id, issued_at, expires_at, family_expires_at, user_agent, ip)
VALUES (@id, @user_id, @family_id, @token_hash, @parent_id, @issued_at, @expires_at, @family_expires_at, @user_agent, @ip);

-- name: GetRefreshTokenForUpdate :one
-- [none] FOR UPDATE: dua refresh paralel dengan token yang sama diserialkan di baris ini, sehingga
-- yang kedua pasti melihat used_at milik yang pertama (grace window, bukan reuse palsu).
SELECT id, user_id, family_id, used_at, revoked_at, expires_at, family_expires_at
FROM refresh_tokens
WHERE token_hash = @token_hash
FOR UPDATE;

-- name: MarkRefreshTokenUsed :exec
-- [none]
UPDATE refresh_tokens SET used_at = @now, replaced_by = @replaced_by WHERE id = @id;

-- name: RevokeFamily :execrows
-- [none]
UPDATE refresh_tokens SET revoked_at = @now, revoke_reason = @reason
WHERE family_id = @family_id AND revoked_at IS NULL;

-- name: GetFamilyByTokenHash :one
-- [none]
SELECT family_id, user_id FROM refresh_tokens WHERE token_hash = @token_hash;

-- name: InsertAudit :exec
-- [none]/[platform] → tenant_id NULL (baris platform); [tenant] → tenant_id = tenant aktif.
INSERT INTO audit_log (id, tenant_id, actor_type, actor_id, action, target_type, target_id, request_id, ip, user_agent, reason)
VALUES (@id, @tenant_id, @actor_type, @actor_id, @action, @target_type, @target_id, @request_id, @ip, @user_agent, @reason);

-- ---------------------------------------------------------------- members & invitations (Sprint 2)

-- name: ActiveMemberEmailExists :one
-- [tenant] Email sudah anggota ACTIVE tenant ini? users terlihat lewat users_tenant_member_read.
SELECT EXISTS (SELECT 1 FROM memberships m JOIN users u ON u.id = m.user_id
               WHERE m.status = 'ACTIVE' AND lower(u.email) = lower(@email));

-- name: CreateInvitedMembership :exec
-- [tenant] Duplikat undangan INVITED untuk email yang sama → uq_memberships_invited_email.
INSERT INTO memberships (tenant_id, id, invited_email, roles, status, created_by, updated_by)
VALUES (@tenant_id, @id, lower(@email), @roles, 'INVITED', @actor, @actor);

-- name: CreateInvitation :exec
-- [tenant]
INSERT INTO invitations (tenant_id, id, membership_id, employee_id, token_hash, email, expires_at, created_at, created_by)
VALUES (@tenant_id, @id, @membership_id, @employee_id, @token_hash, lower(@email), @expires_at, @created_at, @actor);

-- name: GetInvitationForUpdate :one
-- [tenant] (WithTenantTxFor tenant dari token) FOR UPDATE: dua accept paralel diserialkan.
SELECT tenant_id, id, membership_id, employee_id, email, expires_at, accepted_at, revoked_at
FROM invitations WHERE token_hash = @token_hash
FOR UPDATE;

-- name: AcceptMembership :one
-- [tenant] Guard status INVITED; user sudah punya membership di tenant ini → uq_memberships_user.
UPDATE memberships
SET status = 'ACTIVE', user_id = @user_id, accepted_at = @now, updated_by = @user_id, version = version + 1
WHERE id = @id AND status = 'INVITED'
RETURNING tenant_id, roles, status;

-- name: MarkInvitationAccepted :exec
-- [tenant]
UPDATE invitations SET accepted_at = @now WHERE id = @id AND accepted_at IS NULL;

-- name: ListMembers :many
-- [tenant] Urut created_at DESC, id DESC (keyset). user hanya terlihat untuk anggota ACTIVE
-- (users_tenant_member_read); undangan INVITED dan member REVOKED → user null.
SELECT m.tenant_id, m.id, m.user_id, u.email AS user_email, u.display_name AS user_display_name,
       m.invited_email, m.roles, m.status, m.accepted_at, m.version, m.created_at
FROM memberships m
LEFT JOIN users u ON u.id = m.user_id
WHERE (sqlc.narg(status)::text IS NULL OR m.status = sqlc.narg(status))
  AND (sqlc.narg(role)::text IS NULL OR sqlc.narg(role)::text = ANY (m.roles))
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (m.created_at, m.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY m.created_at DESC, m.id DESC
LIMIT @lim;

-- name: GetPendingMembershipByEmail :one
-- [tenant] Undangan INVITED untuk email ini (re-invite: link baru menggantikan link lama). FOR UPDATE:
-- dua re-invite paralel diserialkan.
SELECT id, roles FROM memberships
WHERE status = 'INVITED' AND lower(invited_email) = lower(@email)
FOR UPDATE;

-- name: UpdateInvitedRoles :exec
-- [tenant]
UPDATE memberships SET roles = @roles, updated_by = @actor, version = version + 1
WHERE id = @id AND status = 'INVITED';

-- name: RevokePendingInvitations :exec
-- [tenant] Link lama (belum diterima) dimatikan saat undangan dikirim ulang (12-db-schema §2.3).
UPDATE invitations SET revoked_at = @now
WHERE membership_id = @membership_id AND accepted_at IS NULL AND revoked_at IS NULL;

-- name: EmployeeReservedByInvitation :one
-- [tenant] Employee sudah dicadangkan undangan lain yang masih berlaku (email lain).
SELECT EXISTS (SELECT 1 FROM invitations
               WHERE employee_id = @employee_id AND membership_id <> @membership_id
                 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > @now);

-- name: GetTenantStatus :one
-- [tenant] Accept undangan hanya untuk tenant ACTIVE.
SELECT status FROM tenants WHERE id = @id;

-- ---------------------------------------------------------------- role change & revoke (Sprint 4)

-- name: LockTenantOwners :exec
-- [tenant] PI-16: perubahan role/status membership diserialkan per tenant SEBELUM membaca baris apa
-- pun (22-tx §1 catatan): dua OWNER yang saling mencabut tidak bisa sama-sama lolos trigger deferred.
SELECT pg_advisory_xact_lock(hashtextextended('owners:' || @tenant_id::text, 0));

-- name: ActorRoles :one
-- [tenant] Role pemanggil dibaca ulang dari DB (ADR-P02): role di access token bisa basi ≤ 15 menit.
SELECT roles FROM memberships WHERE user_id = @user_id AND status = 'ACTIVE';

-- name: GetMembershipForUpdate :one
-- [tenant] [6] membership target FOR UPDATE (22-tx §1).
SELECT tenant_id, id, user_id, roles, status, version FROM memberships WHERE id = @id FOR UPDATE;

-- name: SetMembershipRoles :exec
-- [tenant]
UPDATE memberships SET roles = @roles, updated_by = @actor, version = version + 1
WHERE id = @id AND status = 'ACTIVE';

-- name: RevokeMembership :exec
-- [tenant] ACTIVE atau INVITED (membatalkan undangan) → REVOKED.
UPDATE memberships
SET status = 'REVOKED', revoked_at = @now, revoked_by = @actor, updated_by = @actor, version = version + 1
WHERE id = @id AND status IN ('ACTIVE', 'INVITED');

-- name: GetMember :one
-- [tenant] Satu member dengan bentuk yang sama seperti ListMembers (respons U4/U5).
SELECT m.tenant_id, m.id, m.user_id, u.email AS user_email, u.display_name AS user_display_name,
       m.invited_email, m.roles, m.status, m.accepted_at, m.version
FROM memberships m
LEFT JOIN users u ON u.id = m.user_id
WHERE m.id = @id;

-- name: GetRevokedMembershipByUser :one
-- [tenant] Re-invite member yang pernah dicabut (S4): baris lama diaktifkan ulang saat accept, bukan
-- baris baru (uq_memberships_user (tenant_id, user_id); riwayat tetap di satu baris).
SELECT id FROM memberships WHERE user_id = @user_id AND status = 'REVOKED' FOR UPDATE;

-- name: CloseInvitedMembership :one
-- [tenant] Baris undangan yang dipakai untuk mengaktifkan ulang baris lama ditutup (REVOKED tanpa user,
-- migrasi 00008). Rolenya pindah ke baris lama.
UPDATE memberships SET status = 'REVOKED', revoked_at = @now, updated_by = @actor, version = version + 1
WHERE id = @id AND status = 'INVITED'
RETURNING roles;

-- name: ReactivateMembership :one
-- [tenant]
UPDATE memberships
SET status = 'ACTIVE', roles = @roles, accepted_at = @now, revoked_at = NULL, revoked_by = NULL,
    updated_by = @actor, version = version + 1
WHERE id = @id AND status = 'REVOKED'
RETURNING tenant_id, roles, status;
