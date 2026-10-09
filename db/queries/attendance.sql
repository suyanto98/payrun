-- Modul attendance (PF-08). Semua [tenant]. Urutan lock (Step 7 `22` §1): [1b] tenant_settings → [2]
-- periode FOR SHARE (PeriodGuard) → [6] attendance_records → [7] change log/audit.

-- name: InsertClockIn :one
-- D1: satu catatan per (employee, work_date). Clock-in kedua → 0 baris (ON CONFLICT) → baca yang ada.
INSERT INTO attendance_records (tenant_id, id, employee_id, work_date, clock_in_at, status, source, created_by, updated_by)
VALUES (@tenant_id, @id, @employee_id, @work_date, @at, 'INCOMPLETE', 'CLOCK', @actor, @actor)
ON CONFLICT ON CONSTRAINT uq_attendance_records_day DO NOTHING
RETURNING tenant_id, id, employee_id, work_date, clock_in_at, clock_out_at, status, source, correction_reason, version;

-- name: GetRecordByDay :one
SELECT tenant_id, id, employee_id, work_date, clock_in_at, clock_out_at, status, source, correction_reason, version
FROM attendance_records WHERE employee_id = @employee_id AND work_date = @work_date;

-- name: OpenRecordSince :one
-- D2: catatan INCOMPLETE terakhir yang clock-in-nya ≤ 24 jam lalu (shift melewati tengah malam).
SELECT tenant_id, id, employee_id, work_date, clock_in_at, clock_out_at, status, source, correction_reason, version
FROM attendance_records
WHERE employee_id = @employee_id AND status = 'INCOMPLETE' AND clock_in_at IS NOT NULL AND clock_in_at >= @since
ORDER BY work_date DESC LIMIT 1;

-- name: ClockOut :one
-- [6] Guard INCOMPLETE: clock-out kedua tidak menimpa yang pertama.
UPDATE attendance_records
SET clock_out_at = @at, status = 'COMPLETE', updated_by = @actor, version = version + 1
WHERE id = @id AND status = 'INCOMPLETE' AND clock_in_at < @at
RETURNING tenant_id, id, employee_id, work_date, clock_in_at, clock_out_at, status, source, correction_reason, version;

-- name: GetRecord :one
SELECT tenant_id, id, employee_id, work_date, clock_in_at, clock_out_at, status, source, correction_reason, version
FROM attendance_records WHERE id = @id;

-- name: CorrectRecord :one
-- D4 (PF-08 AC3): koreksi HR dengan alasan; guard versi (If-Match). status diturunkan dari kedua waktu.
UPDATE attendance_records
SET clock_in_at = @clock_in_at, clock_out_at = @clock_out_at,
    status = CASE WHEN @clock_in_at::timestamptz IS NOT NULL AND @clock_out_at::timestamptz IS NOT NULL THEN 'COMPLETE' ELSE 'INCOMPLETE' END,
    source = 'CORRECTION', correction_reason = @reason, updated_by = @actor, version = version + 1
WHERE id = @id AND version = @version
RETURNING tenant_id, id, employee_id, work_date, clock_in_at, clock_out_at, status, source, correction_reason, version;

-- name: ListRecords :many
-- D3: filter karyawan (NULL = semua dalam scope, lihat employee_ids), rentang tanggal; keyset (work_date, id).
SELECT tenant_id, id, employee_id, work_date, clock_in_at, clock_out_at, status, source, correction_reason, version
FROM attendance_records
WHERE work_date >= @from_date AND work_date < @to_date
  AND (sqlc.narg(employee_ids)::uuid[] IS NULL OR employee_id = ANY (sqlc.narg(employee_ids)::uuid[]))
  AND (sqlc.narg(after_date)::date IS NULL OR (work_date, id) > (sqlc.narg(after_date)::date, sqlc.narg(after_id)::uuid))
ORDER BY work_date, id
LIMIT @lim;

-- name: RecordSummary :one
-- D3 ringkasan satu karyawan dalam satu periode.
SELECT count(*) FILTER (WHERE status = 'COMPLETE')   AS present_days,
       count(*) FILTER (WHERE status = 'INCOMPLETE') AS incomplete_days
FROM attendance_records WHERE employee_id = @employee_id AND work_date >= @from_date AND work_date < @to_date;
