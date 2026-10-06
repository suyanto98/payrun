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
-- base_secs * 2^(n - threshold), maksimal max_secs.
UPDATE users
SET failed_login_count = failed_login_count + 1,
    locked_until = CASE
      WHEN failed_login_count + 1 >= sqlc.arg(threshold)::int
      THEN sqlc.arg(now)::timestamptz + make_interval(secs => least(sqlc.arg(base_secs)::float8 * power(2, failed_login_count + 1 - sqlc.arg(threshold)::int), sqlc.arg(max_secs)::float8))
      ELSE locked_until
    END
WHERE id = sqlc.arg(id)
RETURNING failed_login_count, locked_until;

-- name: GetUser :one
-- [platform]
SELECT id, email, display_name, status
FROM users
WHERE id = @id;

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
