-- Modul payroll (PF-13): periode, run, item, line, snapshot. Semua [tenant]. Urutan lock global
-- (Step 7 `22` §1): [1] idempotency → [1b] tenant_settings → [2] payroll_periods → [3] payroll_runs → [7] hasil/jobs.
--
-- InputProvider (query Input*) membaca tabel modul lain (employee, compensation, tenancy) secara
-- read-only di txRead chunk: itulah perannya menurut desain (Step 3 `06` §1). Penulisan tetap milik
-- modul pemilik tabel.

-- ---------------------------------------------------------------- periode & run

-- name: LockTenantSettings :one
-- [1b] FOR UPDATE: pembuatan periode baru menyerialkan diri terhadap penulis input (D1).
SELECT timezone, cutoff_day_of_next_month, to_char(cutoff_time_local, 'HH24:MI')::text AS cutoff_time
FROM tenant_settings WHERE tenant_id = @tenant_id FOR UPDATE;

-- name: InsertPeriod :exec
INSERT INTO payroll_periods (tenant_id, id, year, month, cutoff_at)
VALUES (@tenant_id, @id, @year, @month, @cutoff_at)
ON CONFLICT ON CONSTRAINT uq_payroll_periods_month DO NOTHING;

-- name: LockPeriodByMonth :one
-- [2]
SELECT tenant_id, id, year, month, status, cutoff_at FROM payroll_periods
WHERE year = @year AND month = @month FOR UPDATE;

-- name: LockPeriod :one
-- [2] calculate (O12: menunggu semua penulis FOR SHARE selesai).
SELECT tenant_id, id, year, month, status, cutoff_at FROM payroll_periods WHERE id = @id FOR UPDATE;

-- name: ActiveRunForPeriod :one
SELECT id FROM payroll_runs WHERE period_id = @period_id AND status <> 'CANCELLED';

-- name: InsertRun :exec
INSERT INTO payroll_runs (tenant_id, id, period_id, created_by) VALUES (@tenant_id, @id, @period_id, @actor);

-- name: SetPeriodStatus :exec
UPDATE payroll_periods SET status = @status WHERE id = @id AND status <> 'FINALIZED';

-- name: GetRun :one
SELECT r.tenant_id, r.id, r.period_id, p.year, p.month, r.status, r.calc_version, r.chunks_total, r.chunks_done,
       r.eligible_count, r.ruleset_code, r.ruleset_version, r.total_gross, r.total_deductions, r.total_net,
       r.item_ok_count, r.item_error_count, r.item_excluded_count, r.warning_count, r.failure_reason,
       r.calc_started_at, r.calculated_at, r.created_by, r.created_at, r.cancelled_at, r.cancel_reason,
       r.approval_request_id, r.submitted_by, r.submitted_at, r.approved_by, r.approved_at
FROM payroll_runs r JOIN payroll_periods p ON p.tenant_id = r.tenant_id AND p.id = r.period_id
WHERE r.id = @id;

-- name: ListRuns :many
SELECT r.tenant_id, r.id, p.year, p.month, r.status, r.calc_version, r.total_net, r.created_at
FROM payroll_runs r JOIN payroll_periods p ON p.tenant_id = r.tenant_id AND p.id = r.period_id
WHERE (sqlc.narg(year)::smallint IS NULL OR (p.year = sqlc.narg(year) AND p.month = sqlc.narg(month)::smallint))
  AND (sqlc.narg(status)::text IS NULL OR r.status = sqlc.narg(status))
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (r.created_at, r.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY r.created_at DESC, r.id DESC
LIMIT @lim;

-- name: StartCalculation :one
-- [3] guard status: hanya DRAFT/CALCULATED/FAILED → CALCULATING (CALCULATING ditolak, Step 3 `08`).
-- calc_started_at = clock_timestamp() SAMBIL memegang periode FOR UPDATE (batas bawah stale O12).
UPDATE payroll_runs
SET status = 'CALCULATING', calc_version = calc_version + 1, chunks_total = 0, chunks_done = 0,
    eligible_count = NULL, input_fingerprint = NULL, total_gross = NULL, total_deductions = NULL, total_net = NULL,
    item_ok_count = NULL, item_error_count = NULL, item_excluded_count = NULL, warning_count = NULL,
    failure_reason = NULL, calculated_at = NULL, calc_started_at = clock_timestamp()
WHERE id = @id AND status IN ('DRAFT', 'CALCULATED', 'FAILED')
RETURNING calc_version;

-- name: LockRun :one
-- [3]
SELECT tenant_id, id, period_id, status, calc_version, chunks_total, chunks_done, eligible_count
FROM payroll_runs WHERE id = @id FOR UPDATE;

-- name: SetPlan :exec
UPDATE payroll_runs SET eligible_count = @eligible_count, chunks_total = @chunks_total WHERE id = @id;

-- name: IncrementChunksDone :one
UPDATE payroll_runs SET chunks_done = chunks_done + 1 WHERE id = @id RETURNING chunks_done, chunks_total;

-- name: RunVersionSummary :one
-- PI-05: total dihitung dengan SUM di transaksi chunk terakhir (bukan diakumulasi per chunk).
SELECT coalesce(sum(gross) FILTER (WHERE status = 'OK'), 0)::numeric AS gross,
       coalesce(sum(deductions) FILTER (WHERE status = 'OK'), 0)::numeric AS deductions,
       coalesce(sum(net) FILTER (WHERE status = 'OK'), 0)::numeric AS net,
       count(*) FILTER (WHERE status = 'OK')::int AS ok_count,
       count(*) FILTER (WHERE status = 'ERROR')::int AS error_count,
       count(*) FILTER (WHERE status = 'EXCLUDED')::int AS excluded_count,
       count(*) FILTER (WHERE cardinality(warning_codes) > 0)::int AS warning_count,
       count(*)::int AS item_count
FROM payroll_items WHERE run_id = @run_id AND calc_version = @calc_version;

-- name: ItemFingerprints :many
SELECT input_fingerprint FROM payroll_items WHERE run_id = @run_id AND calc_version = @calc_version ORDER BY employee_id;

-- name: CompleteCalculation :exec
UPDATE payroll_runs
SET status = 'CALCULATED', total_gross = @gross, total_deductions = @deductions, total_net = @net,
    item_ok_count = @ok_count, item_error_count = @error_count, item_excluded_count = @excluded_count,
    warning_count = @warning_count, input_fingerprint = @fingerprint, ruleset_code = @ruleset_code,
    ruleset_version = @ruleset_version, calculated_at = clock_timestamp()
WHERE id = @id AND status = 'CALCULATING' AND calc_version = @calc_version;

-- name: FailRun :execrows
UPDATE payroll_runs SET status = 'FAILED', failure_reason = @reason
WHERE id = @id AND status = 'CALCULATING' AND calc_version = @calc_version;

-- name: CancelRun :one
UPDATE payroll_runs SET status = 'CANCELLED', cancelled_by = @actor, cancelled_at = clock_timestamp(), cancel_reason = @reason
WHERE id = @id AND status IN ('DRAFT', 'CALCULATED', 'FAILED')
RETURNING period_id;

-- name: UpdateRunCounts :exec
-- Exclude item: ERROR berkurang, EXCLUDED bertambah; total tidak berubah (item ERROR tidak dihitung).
UPDATE payroll_runs SET item_error_count = item_error_count - 1, item_excluded_count = item_excluded_count + 1
WHERE id = @id AND status = 'CALCULATED';

-- ---------------------------------------------------------------- item / line / snapshot

-- name: DeleteChunkItems :exec
-- Penulisan ulang chunk di versi yang sama (cascade line + snapshot). Normalnya tidak ada (fencing).
DELETE FROM payroll_items WHERE run_id = @run_id AND calc_version = @calc_version AND employee_id = ANY(@employee_ids::uuid[]);

-- name: InsertItem :exec
INSERT INTO payroll_items (tenant_id, id, run_id, employee_id, calc_version, status, gross, deductions, net,
                           error_codes, warning_codes, issues, input_fingerprint, employee_no, employee_name, department_id)
VALUES (@tenant_id, @id, @run_id, @employee_id, @calc_version, @status, sqlc.narg(gross)::numeric, sqlc.narg(deductions)::numeric,
        sqlc.narg(net)::numeric, @error_codes, @warning_codes, @issues, @input_fingerprint, @employee_no, @employee_name, sqlc.narg(department_id));

-- name: InsertLine :exec
INSERT INTO payroll_lines (tenant_id, id, run_id, calc_version, item_id, sort_order, code, name, category, stage,
                           amount, basis, explanation, taxable, rounding)
VALUES (@tenant_id, @id, @run_id, @calc_version, @item_id, @sort_order, @code, @name, @category, @stage,
        @amount::numeric, @basis, @explanation, @taxable, @rounding);

-- name: InsertSnapshot :exec
INSERT INTO payroll_item_snapshots (tenant_id, item_id, run_id, calc_version, snapshot_version, input, ruleset_code, ruleset_version, input_fingerprint)
VALUES (@tenant_id, @item_id, @run_id, @calc_version, @snapshot_version, @input, @ruleset_code, @ruleset_version, @input_fingerprint);

-- name: ListItems :many
-- Y6: versi terkini saja; cursor (employee_no, id); kolom denormalisasi (R3-4).
SELECT tenant_id, id, employee_id, employee_no, employee_name, department_id, status, gross, deductions, net,
       error_codes, warning_codes, excluded_reason
FROM payroll_items
WHERE run_id = @run_id AND calc_version = @calc_version
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(department_id)::uuid IS NULL OR department_id = sqlc.narg(department_id))
  AND (NOT @has_warning::bool OR cardinality(warning_codes) > 0)
  AND (sqlc.narg(after_key)::text IS NULL OR (employee_no, id) > (sqlc.narg(after_key)::text, sqlc.narg(after_id)::uuid))
ORDER BY employee_no, id
LIMIT @lim;

-- name: GetItem :one
SELECT i.tenant_id, i.id, i.run_id, i.employee_id, i.employee_no, i.employee_name, i.department_id, i.calc_version,
       i.status, i.gross, i.deductions, i.net, i.error_codes, i.warning_codes, i.issues, i.excluded_reason
FROM payroll_items i JOIN payroll_runs r ON r.tenant_id = i.tenant_id AND r.id = i.run_id
WHERE i.id = @id AND i.run_id = @run_id AND i.calc_version = r.calc_version;

-- name: ListLines :many
SELECT sort_order, code, name, category, stage, amount, basis, explanation, taxable, rounding
FROM payroll_lines WHERE item_id = @item_id ORDER BY sort_order;

-- name: GetSnapshot :one
SELECT input, ruleset_code, ruleset_version, input_fingerprint FROM payroll_item_snapshots WHERE item_id = @item_id;

-- name: ExcludeItem :one
UPDATE payroll_items SET status = 'EXCLUDED', excluded_reason = @reason, excluded_by = @actor, excluded_at = clock_timestamp()
WHERE id = @id AND run_id = @run_id AND status = 'ERROR'
RETURNING id;

-- ---------------------------------------------------------------- InputProvider (read-only)

-- name: InputSettings :one
SELECT timezone, work_days, proration_basis, overtime_basis, coalesce(overtime_max_hours_per_day::text, '')::text AS overtime_max,
       incomplete_attendance_policy, ruleset_code
FROM tenant_settings WHERE tenant_id = @tenant_id;

-- name: InputRuleset :one
SELECT code, version, payload FROM rulesets WHERE code = @code AND valid_during @> @on_date::date;

-- name: InputHolidays :many
SELECT holiday_date FROM holidays WHERE holiday_date >= @from_date AND holiday_date < @to_date ORDER BY holiday_date;

-- name: EligibleEmployees :many
-- PI-07: aktif minimal satu hari di periode (employment_spans && periode), urut id (chunk stabil).
SELECT DISTINCT e.id FROM employees e
JOIN employment_spans s ON s.tenant_id = e.tenant_id AND s.employee_id = e.id
WHERE s.span && daterange(@from_date::date, @to_date::date, '[)')
ORDER BY e.id;

-- name: InputEmployees :many
-- department_id: penempatan terakhir yang beririsan dengan periode (uuid nol = tidak ada; Go → NULL).
SELECT e.id, e.employee_no, e.full_name, e.ptkp_status,
       coalesce((SELECT oa.department_id FROM org_assignments oa
        WHERE oa.tenant_id = e.tenant_id AND oa.employee_id = e.id AND oa.valid_during && daterange(@from_date::date, @to_date::date, '[)')
        ORDER BY lower(oa.valid_during) DESC LIMIT 1), '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS department_id
FROM employees e WHERE e.id = ANY(@ids::uuid[]) ORDER BY e.id;

-- name: InputSpans :many
SELECT employee_id, lower(span)::date AS from_date, upper_inf(span)::bool AS open_ended, coalesce(upper(span), lower(span))::date AS to_date
FROM employment_spans WHERE employee_id = ANY(@ids::uuid[]) AND span && daterange(@from_date::date, @to_date::date, '[)')
ORDER BY employee_id, lower(span);

-- name: InputSalary :many
SELECT employee_id, base_salary, lower(valid_during)::date AS from_date, upper_inf(valid_during)::bool AS open_ended,
       coalesce(upper(valid_during), lower(valid_during))::date AS to_date
FROM salary_assignments WHERE employee_id = ANY(@ids::uuid[]) AND valid_during && daterange(@from_date::date, @to_date::date, '[)')
ORDER BY employee_id, lower(valid_during);

-- name: InputComponents :many
SELECT ec.employee_id, ec.amount, ec.rate, lower(ec.valid_during)::date AS from_date, upper_inf(ec.valid_during)::bool AS open_ended,
       coalesce(upper(ec.valid_during), lower(ec.valid_during))::date AS to_date,
       cd.code, cd.name, cd.category, cd.stage, cd.calc_type, cd.default_amount, cd.rate AS def_rate, cd.ruleset_ref, cd.base_ref,
       cd.day_source, cd.cap_min, cd.cap_max, cd.taxable, cd.bpjs_base, cd.rounding, cd.status
FROM employee_components ec
JOIN component_definitions cd ON cd.tenant_id = ec.tenant_id AND cd.id = ec.component_id
WHERE ec.employee_id = ANY(@ids::uuid[]) AND ec.valid_during && daterange(@from_date::date, @to_date::date, '[)')
ORDER BY ec.employee_id, cd.code, lower(ec.valid_during);

-- name: InputStatutory :many
-- Komponen statutory (BPJS) berlaku untuk semua employee: definisi sistem stage STATUTORY yang aktif.
SELECT code, name, category, stage, calc_type, ruleset_ref, base_ref, rounding, status
FROM component_definitions WHERE is_system AND stage = 'STATUTORY' ORDER BY code;

-- ---------------------------------------------------------------- PeriodGuard (S5, PI-13/14, O12, D1/D3)

-- name: GuardLockSettings :exec
-- [1b] FOR KEY SHARE: penulis input tidak saling memblok, tetapi menunggu / ditunggu pembuatan periode
-- baru (LockTenantSettings FOR UPDATE, D1).
SELECT 1 FROM tenant_settings WHERE tenant_id = @tenant_id FOR KEY SHARE;

-- name: GuardLockPeriods :many
-- [2] FOR SHARE, urut (year, month) ascending (D3, anti-deadlock rentang multi-bulan).
SELECT id, status FROM payroll_periods
WHERE period_range && daterange(@from_date::date, sqlc.narg(to_date)::date)
ORDER BY year, month
FOR SHARE;

-- name: GuardLockOpenPeriods :many
-- [2] Nilai tanpa tanggal berlaku (PTKP, definisi komponen): semua periode non-final.
SELECT id, status FROM payroll_periods WHERE status <> 'FINALIZED' ORDER BY year, month FOR SHARE;

-- name: RecordInputChanges :exec
-- [7] changed_at = clock_timestamp() (bukan now() = awal transaksi), diambil setelah lock [2].
INSERT INTO payroll_input_changes (tenant_id, period_id, employee_id, changed_at)
SELECT @tenant_id, p, e, clock_timestamp()
FROM unnest(@period_ids::uuid[]) AS p CROSS JOIN unnest(@employee_ids::uuid[]) AS e
ON CONFLICT (tenant_id, period_id, employee_id)
DO UPDATE SET changed_at = EXCLUDED.changed_at, change_count = payroll_input_changes.change_count + 1;

-- name: RecordInputChangesAllEmployees :exec
-- [7] Perubahan tenant-wide (libur, komponen sistem): semua karyawan yang bekerja di periode itu.
INSERT INTO payroll_input_changes (tenant_id, period_id, employee_id, changed_at)
SELECT DISTINCT @tenant_id::uuid, p.id, s.employee_id, clock_timestamp()
FROM payroll_periods p JOIN employment_spans s ON s.span && p.period_range
WHERE p.id = ANY (@period_ids::uuid[])
ON CONFLICT (tenant_id, period_id, employee_id)
DO UPDATE SET changed_at = EXCLUDED.changed_at, change_count = payroll_input_changes.change_count + 1;

-- name: RecordInputChangesComponent :exec
-- [7] Definisi komponen tenant berubah: pemegang komponen itu di periode tersebut.
INSERT INTO payroll_input_changes (tenant_id, period_id, employee_id, changed_at)
SELECT DISTINCT @tenant_id::uuid, p.id, ec.employee_id, clock_timestamp()
FROM payroll_periods p JOIN employee_components ec ON ec.valid_during && p.period_range
WHERE p.id = ANY (@period_ids::uuid[]) AND ec.component_id = @component_id
ON CONFLICT (tenant_id, period_id, employee_id)
DO UPDATE SET changed_at = EXCLUDED.changed_at, change_count = payroll_input_changes.change_count + 1;

-- ---------------------------------------------------------------- submit / approve / reject (S5)

-- name: SubmitRun :execrows
-- [3] Guard CALCULATED → PENDING_APPROVAL (natural idempotency, Y10).
UPDATE payroll_runs SET status = 'PENDING_APPROVAL', submitted_by = @actor, submitted_at = clock_timestamp(),
       approval_request_id = @approval_request_id, approved_by = NULL, approved_at = NULL
WHERE id = @id AND status = 'CALCULATED';

-- name: ApproveRun :execrows
UPDATE payroll_runs SET status = 'APPROVED', approved_by = @actor, approved_at = clock_timestamp()
WHERE id = @id AND status = 'PENDING_APPROVAL';

-- name: RejectRun :execrows
-- Reject → kembali CALCULATED (ADR-P05); alasan tersimpan di approval.
UPDATE payroll_runs SET status = 'CALCULATED', submitted_by = NULL, submitted_at = NULL
WHERE id = @id AND status = 'PENDING_APPROVAL';

-- name: StaleCandidates :many
-- O12: karyawan dengan perubahan input setelah kalkulasi mulai membaca (changed_at > calc_started_at).
SELECT employee_id FROM payroll_input_changes
WHERE period_id = @period_id AND changed_at > @calc_started_at
ORDER BY employee_id;

-- name: CandidateItems :many
-- Item versi terkini untuk kandidat (boleh tidak ada: karyawan baru).
SELECT i.employee_id, i.input_fingerprint, e.employee_no
FROM payroll_items i JOIN employees e ON e.tenant_id = i.tenant_id AND e.id = i.employee_id
WHERE i.run_id = @run_id AND i.calc_version = @calc_version AND i.employee_id = ANY (@ids::uuid[]);

-- name: EligibleAmong :many
-- D6: kandidat yang SAAT INI eligible di periode (PI-07, sama dengan EligibleEmployees).
SELECT DISTINCT e.id, e.employee_no FROM employees e
JOIN employment_spans s ON s.tenant_id = e.tenant_id AND s.employee_id = e.id
WHERE e.id = ANY (@ids::uuid[]) AND s.span && daterange(@from_date::date, @to_date::date, '[)')
ORDER BY e.id;

-- name: RunSummaries :many
-- Ringkasan subjek untuk inbox approval.
SELECT r.id, p.year, p.month, r.status, r.total_net, r.item_ok_count, r.item_excluded_count
FROM payroll_runs r JOIN payroll_periods p ON p.tenant_id = r.tenant_id AND p.id = r.period_id
WHERE r.id = ANY (@ids::uuid[]);

-- ---------------------------------------------------------------- verify-run (S5, PI-12, ADR-P07)

-- name: VerifyItems :many
-- Item versi terkini + snapshot-nya (LEFT JOIN: snapshot hilang juga dilaporkan).
SELECT i.id, i.employee_no, i.status, i.gross, i.deductions, i.net, i.error_codes, i.warning_codes,
       i.input_fingerprint, s.item_id AS snapshot_item_id, s.snapshot_version, s.input, s.ruleset_code, s.ruleset_version,
       s.input_fingerprint AS snapshot_fingerprint
FROM payroll_items i
LEFT JOIN payroll_item_snapshots s ON s.tenant_id = i.tenant_id AND s.item_id = i.id
WHERE i.run_id = @run_id AND i.calc_version = @calc_version
ORDER BY i.employee_no, i.id;

-- name: VerifyLines :many
SELECT item_id, sort_order, code, category, stage, amount, basis, explanation, taxable, rounding
FROM payroll_lines WHERE run_id = @run_id AND calc_version = @calc_version
ORDER BY item_id, sort_order;

-- name: RulesetByVersion :one
-- Versi ruleset yang TERSIMPAN di snapshot (bukan yang berlaku hari ini).
SELECT payload FROM rulesets WHERE code = @code AND version = @version;
