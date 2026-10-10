-- Modul overtime (PF-10). Semua [tenant]. Approve: [1b][2] PeriodGuard (work_date) → [4] approval →
-- [6] request → [7] change log/audit (Step 7 `22` §1, tanpa saldo).

-- name: InsertOvertimeRequest :exec
-- Tumpang tindih jam dengan PENDING/APPROVED → ex_overtime_requests_no_overlap → 409 OVERLAPPING_PERIOD.
INSERT INTO overtime_requests (tenant_id, id, employee_id, work_date, start_at, end_at, hours, reason, created_by, updated_by)
VALUES (@tenant_id, @id, @employee_id, @work_date, @start_at, @end_at, @hours, @reason, @actor, @actor);

-- name: GetOvertimeRequest :one
SELECT tenant_id, id, employee_id, work_date, start_at, end_at, hours, reason, status, approved_at, created_by
FROM overtime_requests WHERE id = @id;

-- name: ApproveOvertimeRequest :execrows
-- approved_at dari jam aplikasi (sama dengan semua waktu bisnis lain): dibandingkan dengan cutoff_at periode (PI-08).
UPDATE overtime_requests SET status = 'APPROVED', approved_at = @at, updated_by = @actor, version = version + 1
WHERE id = @id AND status = 'PENDING';

-- name: CloseOvertimeRequest :execrows
UPDATE overtime_requests SET status = @status, updated_by = @actor, version = version + 1
WHERE id = @id AND status = 'PENDING';

-- name: ListOvertimeRequests :many
SELECT tenant_id, id, employee_id, work_date, start_at, end_at, hours, reason, status, approved_at, created_at
FROM overtime_requests
WHERE (sqlc.narg(employee_ids)::uuid[] IS NULL OR employee_id = ANY (sqlc.narg(employee_ids)::uuid[]))
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(from_date)::date IS NULL OR (work_date >= sqlc.narg(from_date)::date AND work_date < sqlc.narg(to_date)::date))
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT @lim;

-- name: OvertimeSummaries :many
-- Ringkasan inbox: "Lembur 23 Mar 2026 · 4 jam · Tono".
SELECT r.id, r.work_date, r.hours, e.full_name
FROM overtime_requests r JOIN employees e ON e.tenant_id = r.tenant_id AND e.id = r.employee_id
WHERE r.id = ANY (@ids::uuid[]);
