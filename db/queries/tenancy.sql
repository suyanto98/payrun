-- Modul tenancy (PF-01): tenants, tenant_settings, platform_idempotency_keys (signup).
-- Mode: [tenant] db.WithTenantTx / SetTenant; [platform] app.user_id; [none] tanpa context.

-- name: CreateTenant :exec
-- [tenant] WITH CHECK tenants_current: id = tenant context (tenant baru yang dibuat signup).
INSERT INTO tenants (id, slug, name, status)
VALUES (@id, @slug, @name, @status);

-- name: CreateTenantSettings :exec
-- [tenant] Seed O6: nilai default kolom, kecuali timezone dari pendaftar.
INSERT INTO tenant_settings (tenant_id, timezone, created_by)
VALUES (@tenant_id, @timezone, @created_by);

-- name: GetTenant :one
-- [tenant] Tenant aktif + settings. RLS menjamin hanya satu baris yang terlihat; WHERE tetap eksplisit.
SELECT t.id, t.name, t.slug, t.status,
       s.timezone, s.work_days, s.proration_basis, s.cutoff_day_of_next_month,
       to_char(s.cutoff_time_local, 'HH24:MI')::text AS cutoff_time_local,  -- desimal/waktu sebagai string (konvensi API)
       s.overtime_basis, coalesce(s.overtime_max_hours_per_day::text, '')::text AS overtime_max_hours_per_day, -- '' = tanpa batas
       s.incomplete_attendance_policy, s.ruleset_code,
       s.net_change_warning_pct::text AS net_change_warning_pct, s.version
FROM tenants t
JOIN tenant_settings s ON s.tenant_id = t.id
WHERE t.id = @id;

-- name: ListPendingOwnedTenants :many
-- [platform] Tenant PENDING_VERIFICATION tempat user adalah OWNER aktif (aktivasi setelah verifikasi).
SELECT t.id
FROM tenants t
JOIN memberships m ON m.tenant_id = t.id
WHERE m.user_id = sqlc.arg(user_id)::uuid AND m.status = 'ACTIVE' AND 'OWNER' = ANY (m.roles)
  AND t.status = 'PENDING_VERIFICATION';

-- name: ActivateTenant :execrows
-- [tenant]
UPDATE tenants SET status = 'ACTIVE' WHERE id = @id AND status = 'PENDING_VERIFICATION';

-- name: ClaimPlatformIdempotencyKey :one
-- [none] Mengklaim key di transaksi signup. Konflik → tidak ada baris (lihat GetPlatformIdempotencyKey).
-- Request paralel dengan key yang sama menunggu di unique index sampai transaksi pertama selesai
-- (lock_timeout → 409 REQUEST_IN_PROGRESS).
INSERT INTO platform_idempotency_keys (id, endpoint, key, request_hash, created_at, expires_at)
VALUES (@id, @endpoint, @key, @request_hash, @created_at, @expires_at)
ON CONFLICT (endpoint, key) DO NOTHING
RETURNING id;

-- name: GetPlatformIdempotencyKey :one
-- [none]
SELECT id, request_hash, response_status, response_body, expires_at
FROM platform_idempotency_keys
WHERE endpoint = @endpoint AND key = @key;

-- name: DeletePlatformIdempotencyKey :exec
-- [none] Key kedaluwarsa (24 jam) boleh dipakai lagi.
DELETE FROM platform_idempotency_keys WHERE id = @id;

-- name: CompletePlatformIdempotencyKey :exec
-- [none]
UPDATE platform_idempotency_keys SET response_status = @response_status, response_body = @response_body
WHERE id = @id;

-- name: InsertTenantAudit :exec
-- [tenant]
INSERT INTO audit_log (id, tenant_id, actor_type, actor_id, action, target_type, target_id, request_id, ip, user_agent)
VALUES (@id, @tenant_id, @actor_type, @actor_id, @action, @target_type, @target_id, @request_id, @ip, @user_agent);

-- name: SeedSystemComponents :exec
-- [tenant] Seed O6: komponen sistem (BASE_SALARY, OVERTIME, ABSENCE_*, BPJS_*, PPH21, ADJUSTMENT).
SELECT seed_system_components(@tenant_id::uuid, @actor::uuid);

-- name: SeedLeaveTypes :exec
-- [tenant] Seed jenis cuti default (13 §6, S6): ANNUAL (dibayar, kuota 12), SICK (dibayar), UNPAID.
SELECT seed_leave_types(@tenant_id::uuid, @actor::uuid);

-- name: ListHolidays :many
-- [tenant] T4: libur satu tahun (maksimal 366 baris, tanpa pagination).
SELECT tenant_id, id, holiday_date, name, version FROM holidays
WHERE holiday_date >= @from_date AND holiday_date < @to_date
ORDER BY holiday_date;

-- name: CreateHoliday :one
-- [tenant] T5. Tanggal duplikat → uq_holidays_date → 409 DUPLICATE_CODE (field date).
INSERT INTO holidays (tenant_id, id, holiday_date, name, created_by, updated_by)
VALUES (@tenant_id, @id, @holiday_date, @name, @actor, @actor)
RETURNING tenant_id, id, holiday_date, name, version;

-- name: DeleteHoliday :one
-- [tenant] T6. Tidak ada / tenant lain → 0 baris → 404.
DELETE FROM holidays WHERE id = @id RETURNING holiday_date;

-- name: GetHolidayDate :one
-- [tenant] Tanggal libur yang akan dihapus, dibaca SEBELUM PeriodGuard (tanpa lock; urutan [1b] → [2] → [6]).
SELECT holiday_date FROM holidays WHERE id = @id;

-- name: CalendarSettings :one
-- [tenant] Zona waktu + hari kerja mingguan (kalender kerja, dipakai modul input S6).
SELECT timezone, work_days, coalesce(overtime_max_hours_per_day::text, '')::text AS overtime_max FROM tenant_settings WHERE tenant_id = @tenant_id;
