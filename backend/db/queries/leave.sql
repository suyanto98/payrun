-- Modul leave (PF-09). Semua [tenant]. Approve: [1b][2] PeriodGuard → [4] approval → [5] saldo FOR UPDATE
-- → [6] request → [7] change log/audit (Step 7 `22` §3.1).

-- name: ListLeaveTypes :many
SELECT tenant_id, id, code, name, is_paid, annual_quota_days, max_negative_days, status FROM leave_types ORDER BY code;

-- name: GetLeaveType :one
SELECT tenant_id, id, code, name, is_paid, annual_quota_days, max_negative_days, status FROM leave_types WHERE id = @id;

-- name: InsertLeaveRequest :exec
-- Tumpang tindih dengan PENDING/APPROVED → ex_leave_requests_no_overlap (23P01) → 409 OVERLAPPING_LEAVE.
INSERT INTO leave_requests (tenant_id, id, employee_id, leave_type_id, dates, days, reason, created_by, updated_by)
VALUES (@tenant_id, @id, @employee_id, @leave_type_id, daterange(@start_date::date, @end_exclusive::date, '[)'), @days, @reason, @actor, @actor);

-- name: GetLeaveRequest :one
SELECT r.tenant_id, r.id, r.employee_id, r.leave_type_id, lt.code AS type_code, lt.name AS type_name, lt.is_paid,
       lt.annual_quota_days, lt.max_negative_days, lower(r.dates)::date AS start_date, (upper(r.dates) - 1)::date AS end_date,
       r.days, r.reason, r.status, r.approved_at, r.created_by
FROM leave_requests r JOIN leave_types lt ON lt.tenant_id = r.tenant_id AND lt.id = r.leave_type_id
WHERE r.id = @id;

-- name: ApproveLeaveRequest :execrows
-- approved_at dari jam aplikasi (sama dengan semua waktu bisnis lain): dibandingkan dengan cutoff_at periode (PI-08).
UPDATE leave_requests SET status = 'APPROVED', approved_at = @at, updated_by = @actor, version = version + 1
WHERE id = @id AND status = 'PENDING';

-- name: CloseLeaveRequest :execrows
-- Reject / cancel: hanya dari PENDING.
UPDATE leave_requests SET status = @status, updated_by = @actor, version = version + 1
WHERE id = @id AND status = 'PENDING';

-- name: EnsureLeaveBalance :exec
-- Saldo dibuat lazy saat approve pertama (kuota tahunan penuh; akrual PF-26 = P2).
INSERT INTO leave_balances (tenant_id, id, employee_id, leave_type_id, year, balance_days, min_allowed_days)
VALUES (@tenant_id, @id, @employee_id, @leave_type_id, @year, @balance_days, @min_allowed_days)
ON CONFLICT ON CONSTRAINT uq_leave_balances_key DO NOTHING;

-- name: LockLeaveBalance :one
-- [5] Dua approve paralel atas saldo yang sama diserialkan (race (a), PI-10).
SELECT id, balance_days, min_allowed_days FROM leave_balances
WHERE employee_id = @employee_id AND leave_type_id = @leave_type_id AND year = @year
FOR UPDATE;

-- name: DeductLeaveBalance :exec
-- Atomik + ck_leave_balances_min sebagai jaring pengaman walaupun lock terlupa.
UPDATE leave_balances SET balance_days = balance_days - @days WHERE id = @id;

-- name: MyLeaveBalances :many
-- L2: per jenis cuti ACTIVE: saldo tersimpan (bila sudah ada) dan hari PENDING di tahun itu.
SELECT lt.id, lt.code, lt.name, lt.is_paid, lt.annual_quota_days, lt.max_negative_days,
       b.balance_days,
       coalesce((SELECT sum(r.days) FROM leave_requests r
                 WHERE r.employee_id = @employee_id AND r.leave_type_id = lt.id AND r.status = 'PENDING'
                   AND extract(year FROM lower(r.dates)) = @year::int), 0)::numeric(6,1) AS pending_days
FROM leave_types lt
LEFT JOIN leave_balances b ON b.tenant_id = lt.tenant_id AND b.leave_type_id = lt.id AND b.employee_id = @employee_id AND b.year = @year::smallint
WHERE lt.status = 'ACTIVE'
ORDER BY lt.code;

-- name: ListLeaveRequests :many
-- L5: scope (employee_ids NULL = semua), status, overlap rentang; keyset created_at DESC, id DESC.
SELECT r.tenant_id, r.id, r.employee_id, r.leave_type_id, lt.code AS type_code, lt.name AS type_name, lt.is_paid,
       lower(r.dates)::date AS start_date, (upper(r.dates) - 1)::date AS end_date, r.days, r.reason, r.status,
       r.approved_at, r.created_at
FROM leave_requests r JOIN leave_types lt ON lt.tenant_id = r.tenant_id AND lt.id = r.leave_type_id
WHERE (sqlc.narg(employee_ids)::uuid[] IS NULL OR r.employee_id = ANY (sqlc.narg(employee_ids)::uuid[]))
  AND (sqlc.narg(status)::text IS NULL OR r.status = sqlc.narg(status))
  AND (sqlc.narg(from_date)::date IS NULL OR r.dates && daterange(sqlc.narg(from_date)::date, sqlc.narg(to_date)::date, '[)'))
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (r.created_at, r.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY r.created_at DESC, r.id DESC
LIMIT @lim;

-- name: LeaveSummaries :many
-- Ringkasan subjek inbox approval: "Cuti tidak dibayar 25 Mar 2026 · 1 hari · Tono".
SELECT r.id, lt.name AS type_name, lower(r.dates)::date AS start_date, (upper(r.dates) - 1)::date AS end_date, r.days, e.full_name
FROM leave_requests r
JOIN leave_types lt ON lt.tenant_id = r.tenant_id AND lt.id = r.leave_type_id
JOIN employees e ON e.tenant_id = r.tenant_id AND e.id = r.employee_id
WHERE r.id = ANY (@ids::uuid[]);
