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
       r.approval_request_id, r.submitted_by, r.submitted_at, r.approved_by, r.approved_at,
       r.finalized_by, r.finalized_at
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

-- name: CompleteCalculation :one
-- duration_secs: calc_started_at → CALCULATED (metrik payrun_payroll_run_duration_seconds).
UPDATE payroll_runs
SET status = 'CALCULATED', total_gross = @gross, total_deductions = @deductions, total_net = @net,
    item_ok_count = @ok_count, item_error_count = @error_count, item_excluded_count = @excluded_count,
    warning_count = @warning_count, input_fingerprint = @fingerprint, ruleset_code = @ruleset_code,
    ruleset_version = @ruleset_version, calculated_at = clock_timestamp()
WHERE id = @id AND status = 'CALCULATING' AND calc_version = @calc_version
RETURNING extract(epoch FROM calculated_at - coalesce(calc_started_at, calculated_at))::float8 AS duration_secs;

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

-- name: InsertItemsBatch :exec
-- writeChunk (S9 PR-1): satu INSERT … unnest per chunk (COPY ditolak untuk tabel ber-RLS). error/warning
-- codes dikirim sebagai teks dipisah koma (kode katalog tanpa koma); department nol → NULL.
INSERT INTO payroll_items (tenant_id, id, run_id, employee_id, calc_version, status, gross, deductions, net,
                           error_codes, warning_codes, issues, input_fingerprint, employee_no, employee_name, department_id)
SELECT @tenant_id, u.id, @run_id, u.employee_id, @calc_version, u.status, u.gross, u.deductions, u.net,
       CASE WHEN u.errs = '' THEN '{}'::text[] ELSE string_to_array(u.errs, ',') END,
       CASE WHEN u.warns = '' THEN '{}'::text[] ELSE string_to_array(u.warns, ',') END,
       u.issues, u.fp, u.no, u.name, nullif(u.dept, '00000000-0000-0000-0000-000000000000'::uuid)
FROM (SELECT unnest(@ids::uuid[]) AS id, unnest(@employee_ids::uuid[]) AS employee_id, unnest(@statuses::text[]) AS status, unnest(@gross::numeric[]) AS gross, unnest(@deductions::numeric[]) AS deductions, unnest(@net::numeric[]) AS net, unnest(@errs::text[]) AS errs, unnest(@warns::text[]) AS warns, unnest(@issues::jsonb[]) AS issues, unnest(@fps::bytea[]) AS fp, unnest(@nos::text[]) AS no, unnest(@names::text[]) AS name, unnest(@depts::uuid[]) AS dept) u;

-- name: InsertLinesBatch :exec
INSERT INTO payroll_lines (tenant_id, id, run_id, calc_version, item_id, sort_order, code, name, category, stage,
                           amount, basis, explanation, taxable, rounding, adjustment_id)
SELECT @tenant_id, u.id, @run_id, @calc_version, u.item_id, u.sort_order, u.code, u.name, u.category, u.stage,
       u.amount, u.basis, u.explanation, u.taxable, u.rounding, nullif(u.adj, '00000000-0000-0000-0000-000000000000'::uuid)
FROM (SELECT unnest(@ids::uuid[]) AS id, unnest(@item_ids::uuid[]) AS item_id, unnest(@sort_orders::smallint[]) AS sort_order, unnest(@codes::text[]) AS code, unnest(@names::text[]) AS name, unnest(@categories::text[]) AS category, unnest(@stages::text[]) AS stage, unnest(@amounts::numeric[]) AS amount, unnest(@bases::jsonb[]) AS basis, unnest(@explanations::text[]) AS explanation, unnest(@taxables::bool[]) AS taxable, unnest(@roundings::text[]) AS rounding, unnest(@adjs::uuid[]) AS adj) u;

-- name: InsertSnapshotsBatch :exec
INSERT INTO payroll_item_snapshots (tenant_id, item_id, run_id, calc_version, snapshot_version, input, ruleset_code, ruleset_version, input_fingerprint)
SELECT @tenant_id, u.item_id, @run_id, @calc_version, @snapshot_version, u.input, @ruleset_code, @ruleset_version, u.fp
FROM (SELECT unnest(@item_ids::uuid[]) AS item_id, unnest(@inputs::jsonb[]) AS input, unnest(@fps::bytea[]) AS fp) u;

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
SELECT sort_order, code, name, category, stage, amount, basis, explanation, taxable, rounding, adjustment_id
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
-- Query Input* menyebut tenant_id EKSPLISIT walau RLS sudah membatasi (S9 PR-1): predikat RLS memakai
-- (SELECT current_tenant()) yang nilainya tidak diketahui planner, sehingga untuk tenant besar (10% tabel)
-- estimasi baris salah dan indeks yang dipilih memindai seluruh tenant. Dengan nilai eksplisit + custom
-- plan (txRead chunk), statistik MCV tenant_id terpakai. RLS tetap lapis pengaman.
-- department_id: penempatan terakhir yang beririsan dengan periode (uuid nol = tidak ada; Go → NULL).
SELECT e.id, e.employee_no, e.full_name, e.ptkp_status,
       coalesce((SELECT oa.department_id FROM org_assignments oa
        WHERE oa.tenant_id = e.tenant_id AND oa.employee_id = e.id AND oa.valid_during && daterange(@from_date::date, @to_date::date, '[)')
        ORDER BY lower(oa.valid_during) DESC LIMIT 1), '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS department_id
FROM employees e WHERE e.tenant_id = @tenant_id AND e.id = ANY(@ids::uuid[]) ORDER BY e.id;

-- name: InputSpans :many
SELECT employee_id, lower(span)::date AS from_date, upper_inf(span)::bool AS open_ended, coalesce(upper(span), lower(span))::date AS to_date
FROM employment_spans WHERE tenant_id = @tenant_id AND employee_id = ANY(@ids::uuid[]) AND span && daterange(@from_date::date, @to_date::date, '[)')
ORDER BY employee_id, lower(span);

-- name: InputSalary :many
SELECT employee_id, base_salary, lower(valid_during)::date AS from_date, upper_inf(valid_during)::bool AS open_ended,
       coalesce(upper(valid_during), lower(valid_during))::date AS to_date
FROM salary_assignments WHERE tenant_id = @tenant_id AND employee_id = ANY(@ids::uuid[]) AND valid_during && daterange(@from_date::date, @to_date::date, '[)')
ORDER BY employee_id, lower(valid_during);

-- name: InputComponents :many
SELECT ec.employee_id, ec.amount, ec.rate, lower(ec.valid_during)::date AS from_date, upper_inf(ec.valid_during)::bool AS open_ended,
       coalesce(upper(ec.valid_during), lower(ec.valid_during))::date AS to_date,
       cd.code, cd.name, cd.category, cd.stage, cd.calc_type, cd.default_amount, cd.rate AS def_rate, cd.ruleset_ref, cd.base_ref,
       cd.day_source, cd.cap_min, cd.cap_max, cd.taxable, cd.bpjs_base, cd.rounding, cd.status
FROM employee_components ec
JOIN component_definitions cd ON cd.tenant_id = ec.tenant_id AND cd.id = ec.component_id
WHERE ec.tenant_id = @tenant_id AND ec.employee_id = ANY(@ids::uuid[]) AND ec.valid_during && daterange(@from_date::date, @to_date::date, '[)')
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

-- name: VerifyRunTotals :one
-- PI-05 (verify-run + job payroll.verify_run): total run = Σ item OK versi terkini dan satu item per
-- karyawan eligible. Trigger payroll_runs_totals_check menjaganya saat menulis; ini memeriksa data yang
-- sudah tersimpan (manipulasi langsung, bug trigger, restore).
SELECT (coalesce(r.total_gross, 0) = s.gross AND coalesce(r.total_deductions, 0) = s.deductions
        AND coalesce(r.total_net, 0) = s.net)::bool AS totals_equal,
       (coalesce(r.item_ok_count, 0) + coalesce(r.item_error_count, 0) + coalesce(r.item_excluded_count, 0) = s.items
        AND s.items = coalesce(r.eligible_count, -1))::bool AS counts_equal,
       s.items::int AS items, coalesce(r.eligible_count, 0)::int AS eligible
FROM payroll_runs r,
     LATERAL (SELECT count(*) AS items,
                     coalesce(sum(gross) FILTER (WHERE status = 'OK'), 0) AS gross,
                     coalesce(sum(deductions) FILTER (WHERE status = 'OK'), 0) AS deductions,
                     coalesce(sum(net) FILTER (WHERE status = 'OK'), 0) AS net
              FROM payroll_items i WHERE i.run_id = r.id AND i.calc_version = r.calc_version) s
WHERE r.id = @id;

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

-- ---------------------------------------------------------------- InputProvider absensi/cuti/lembur (S6)

-- name: PeriodCutoff :one
-- cutoff_at dibekukan saat periode dibuat (determinisme, PI-08).
SELECT cutoff_at FROM payroll_periods WHERE year = @year AND month = @month;

-- name: InputIncomplete :many
-- Catatan INCOMPLETE (lupa clock-out) → engine sesuai incomplete_attendance_policy. COMPLETE = hadir.
SELECT employee_id, work_date FROM attendance_records
WHERE tenant_id = @tenant_id AND employee_id = ANY (@ids::uuid[]) AND status = 'INCOMPLETE' AND work_date >= @from_date AND work_date < @to_date
ORDER BY employee_id, work_date;

-- name: InputLeaves :many
-- PI-08: cuti APPROVED yang beririsan dengan periode dan disetujui ≤ cut-off periode.
SELECT r.employee_id, lower(r.dates)::date AS start_date, upper(r.dates)::date AS end_exclusive, lt.is_paid
FROM leave_requests r JOIN leave_types lt ON lt.tenant_id = r.tenant_id AND lt.id = r.leave_type_id
WHERE r.tenant_id = @tenant_id AND r.employee_id = ANY (@ids::uuid[]) AND r.status = 'APPROVED' AND r.approved_at <= @cutoff_at
  AND r.dates && daterange(@from_date::date, @to_date::date, '[)')
ORDER BY r.employee_id, lower(r.dates);

-- name: InputOvertime :many
-- PI-08: lembur APPROVED bertanggal di periode dan disetujui ≤ cut-off; jam dijumlah per hari.
SELECT employee_id, work_date, sum(hours)::numeric AS hours FROM overtime_requests
WHERE tenant_id = @tenant_id AND employee_id = ANY (@ids::uuid[]) AND status = 'APPROVED' AND approved_at <= @cutoff_at
  AND work_date >= @from_date AND work_date < @to_date
GROUP BY employee_id, work_date
ORDER BY employee_id, work_date;

-- ---------------------------------------------------------------- finalize (S6, Y11, Step 7 `22` §3.4)

-- name: FinalizeRun :execrows
-- [3] Guard APPROVED; trigger payroll_runs_totals_check memverifikasi total (PI-05). Setelah ini terminal (PI-04).
UPDATE payroll_runs SET status = 'FINALIZED', finalized_by = @actor, finalized_at = @at
WHERE id = @id AND status = 'APPROVED';

-- name: FinalizePeriod :exec
-- [2] Periode FINALIZED: PeriodGuard aplikasi + trigger guard_period_* menolak semua penulis input (PI-14).
UPDATE payroll_periods SET status = 'FINALIZED', finalized_run_id = @final_run, finalized_at = @at
WHERE id = @id AND status <> 'FINALIZED';

-- name: DeleteOldVersionItems :execrows
-- Job payroll.cleanup_versions (Step 12 R3-2): versi lama dihapus bertahap SETELAH finalize, tidak di
-- transaksi finalize yang memegang lock periode. Line + snapshot ikut (CASCADE); trigger PI-04 mengizinkan
-- DELETE versi < calc_version run final.
DELETE FROM payroll_items WHERE id IN (
  SELECT old.id FROM payroll_items old WHERE old.run_id = @run_id AND old.calc_version < @keep_version LIMIT 1000);

-- ---------------------------------------------------------------- adjustment (S7, PF-19, PI-15)

-- name: PeriodByMonth :one
SELECT id, status FROM payroll_periods WHERE year = @year AND month = @month;

-- name: EmployeeExists :one
SELECT EXISTS (SELECT 1 FROM employees WHERE id = @id)::bool;

-- name: EmployeeActiveInMonth :one
SELECT EXISTS (SELECT 1 FROM employment_spans WHERE employee_id = @employee_id
               AND span && daterange(@from_date::date, @to_date::date, '[)'))::bool;

-- name: InsertAdjustment :exec
INSERT INTO adjustments (tenant_id, id, employee_id, origin_period_id, target_year, target_month, component_code,
                         amount, taxable, reason, created_by)
VALUES (@tenant_id, @id, @employee_id, @origin_period_id, @target_year, @target_month, @component_code,
        @amount::numeric, @taxable, @reason, @created_by);

-- name: GetAdjustment :one
SELECT a.id, a.tenant_id, a.employee_id, p.year AS origin_year, p.month AS origin_month, a.target_year, a.target_month,
       a.component_code, a.amount::text AS amount, a.taxable, a.reason, a.status, a.applied_run_id, a.applied_at,
       a.cancelled_at, a.created_by, a.created_at
FROM adjustments a JOIN payroll_periods p ON p.tenant_id = a.tenant_id AND p.id = a.origin_period_id
WHERE a.id = @id;

-- name: LockAdjustment :one
SELECT id, employee_id, target_year, target_month, status FROM adjustments WHERE id = @id FOR UPDATE;

-- name: CancelAdjustment :execrows
UPDATE adjustments SET status = 'CANCELLED', cancelled_by = @actor, cancelled_at = @at, cancel_reason = @reason
WHERE id = @id AND status = 'PENDING_APPLY';

-- name: ListAdjustments :many
-- J2: cursor (created_at DESC, id DESC); filter opsional.
SELECT a.id, a.tenant_id, a.employee_id, p.year AS origin_year, p.month AS origin_month, a.target_year, a.target_month,
       a.component_code, a.amount::text AS amount, a.taxable, a.reason, a.status, a.applied_run_id, a.applied_at,
       a.cancelled_at, a.created_by, a.created_at
FROM adjustments a JOIN payroll_periods p ON p.tenant_id = a.tenant_id AND p.id = a.origin_period_id
WHERE (sqlc.narg(employee_id)::uuid IS NULL OR a.employee_id = sqlc.narg(employee_id))
  AND (sqlc.narg(target_year)::smallint IS NULL OR (a.target_year = sqlc.narg(target_year) AND a.target_month = sqlc.narg(target_month)::smallint))
  AND (sqlc.narg(status)::text IS NULL OR a.status = sqlc.narg(status))
  AND (sqlc.narg(cursor_at)::timestamptz IS NULL OR (a.created_at, a.id) < (sqlc.narg(cursor_at), sqlc.narg(cursor_id)::uuid))
ORDER BY a.created_at DESC, a.id DESC
LIMIT @lim;

-- name: InputAdjustments :many
-- Adjustment PENDING_APPLY untuk (tahun, bulan) target; urutan (origin, id) agar input deterministik.
SELECT a.id, a.employee_id, p.year AS origin_year, p.month AS origin_month, a.amount::text AS amount, a.taxable, a.reason
FROM adjustments a JOIN payroll_periods p ON p.tenant_id = a.tenant_id AND p.id = a.origin_period_id
WHERE a.tenant_id = @tenant_id AND a.employee_id = ANY(@ids::uuid[]) AND a.target_year = @year AND a.target_month = @month AND a.status = 'PENDING_APPLY'
ORDER BY a.employee_id, p.year, p.month, a.id;

-- name: ApplyAdjustments :execrows
-- Finalize (PI-15): adjustment yang tercantum di snapshot item OK versi final → APPLIED, tepat sekali
-- (guard status + applied_run_id IS NULL). Item ERROR/EXCLUDED: adjustment tetap PENDING_APPLY.
UPDATE adjustments a SET status = 'APPLIED', applied_run_id = @run_id, applied_at = @at
WHERE a.status = 'PENDING_APPLY' AND a.applied_run_id IS NULL AND a.id IN (
    SELECT (adj->>'id')::uuid
    FROM payroll_item_snapshots s
    JOIN payroll_items i ON i.tenant_id = s.tenant_id AND i.id = s.item_id
    CROSS JOIN LATERAL jsonb_array_elements(s.input->'adjustments') AS adj
    WHERE s.run_id = @run_id AND s.calc_version = @calc_version AND i.status = 'OK');

-- ---------------------------------------------------------------- payslip (S7, PF-15, PF-22)

-- name: OKItemsOfVersion :many
SELECT id, employee_id FROM payroll_items WHERE run_id = @run_id AND calc_version = @calc_version AND status = 'OK'
ORDER BY employee_id;

-- name: InsertPayslips :exec
-- Satu payslip PENDING per item OK (transaksi finalize). object_key dibangun aplikasi, dijaga
-- ck_payslips_object_key (tenants/{tenant}/payslips/{run}/{employee}.pdf).
INSERT INTO payslips (tenant_id, id, run_id, employee_id, item_id, object_key)
SELECT @tenant_id, u.id, @run_id, u.employee_id, u.item_id, u.object_key
FROM (SELECT unnest(@ids::uuid[]) AS id, unnest(@employee_ids::uuid[]) AS employee_id, unnest(@item_ids::uuid[]) AS item_id,
             unnest(@object_keys::text[]) AS object_key) u;

-- name: PayslipsForRender :many
-- Data render PDF per payslip batch. Jabatan/departemen: penempatan terakhir yang beririsan periode.
SELECT p.id, p.tenant_id, p.status, p.object_key, p.item_id, r.finalized_at, per.year, per.month, t.name AS tenant_name,
       i.employee_no, i.employee_name, i.gross::text AS gross, i.deductions::text AS deductions, i.net::text AS net,
       coalesce(r.ruleset_code, '') AS ruleset_code, coalesce(r.ruleset_version, '') AS ruleset_version,
       e.bank_name, e.bank_account_last4,
       coalesce((SELECT ps.name FROM org_assignments oa JOIN positions ps ON ps.tenant_id = oa.tenant_id AND ps.id = oa.position_id
                 WHERE oa.tenant_id = p.tenant_id AND oa.employee_id = p.employee_id
                   AND oa.valid_during && daterange(make_date(per.year, per.month, 1), (make_date(per.year, per.month, 1) + interval '1 month')::date, '[)')
                 ORDER BY lower(oa.valid_during) DESC LIMIT 1), '')::text AS position_name,
       coalesce((SELECT d.name FROM org_assignments oa JOIN departments d ON d.tenant_id = oa.tenant_id AND d.id = oa.department_id
                 WHERE oa.tenant_id = p.tenant_id AND oa.employee_id = p.employee_id
                   AND oa.valid_during && daterange(make_date(per.year, per.month, 1), (make_date(per.year, per.month, 1) + interval '1 month')::date, '[)')
                 ORDER BY lower(oa.valid_during) DESC LIMIT 1), '')::text AS department_name
FROM payslips p
JOIN payroll_runs r ON r.tenant_id = p.tenant_id AND r.id = p.run_id
JOIN payroll_periods per ON per.tenant_id = r.tenant_id AND per.id = r.period_id
JOIN payroll_items i ON i.tenant_id = p.tenant_id AND i.id = p.item_id
JOIN employees e ON e.tenant_id = p.tenant_id AND e.id = p.employee_id
JOIN tenants t ON t.id = p.tenant_id
WHERE p.id = ANY(@ids::uuid[])
ORDER BY p.employee_id;

-- name: MarkPayslipRendered :execrows
UPDATE payslips SET status = 'RENDERED', checksum_sha256 = @checksum, size_bytes = @size_bytes, rendered_at = @at,
                    attempts = attempts + 1, last_error = NULL
WHERE id = @id AND status <> 'RENDERED';

-- name: MarkPayslipFailed :exec
UPDATE payslips SET status = 'FAILED', attempts = attempts + 1, last_error = @last_error WHERE id = @id AND status <> 'RENDERED';

-- name: PendingPayslipCount :one
SELECT count(*) FROM payslips WHERE status <> 'RENDERED';

-- name: SelfEmployee :one
SELECT id FROM employees WHERE user_id = @user_id;

-- name: GetPayslip :one
SELECT p.id, p.tenant_id, p.run_id, p.employee_id, p.item_id, p.status, p.object_key, r.finalized_at, per.year, per.month
FROM payslips p
JOIN payroll_runs r ON r.tenant_id = p.tenant_id AND r.id = p.run_id
JOIN payroll_periods per ON per.tenant_id = r.tenant_id AND per.id = r.period_id
WHERE p.id = @id;

-- name: ListMyPayslips :many
-- S1: slip FINALIZED milik employee, periode terbaru dulu. Cursor (awal bulan periode, id).
SELECT p.id, p.tenant_id, per.year, per.month, i.net::text AS net, r.finalized_at, p.status,
       make_date(per.year, per.month, 1)::timestamptz AS period_at
FROM payslips p
JOIN payroll_runs r ON r.tenant_id = p.tenant_id AND r.id = p.run_id
JOIN payroll_periods per ON per.tenant_id = r.tenant_id AND per.id = r.period_id
JOIN payroll_items i ON i.tenant_id = p.tenant_id AND i.id = p.item_id
WHERE p.employee_id = @employee_id
  AND (sqlc.narg(year)::smallint IS NULL OR per.year = sqlc.narg(year))
  AND (sqlc.narg(cursor_at)::timestamptz IS NULL
       OR (make_date(per.year, per.month, 1)::timestamptz, p.id) < (sqlc.narg(cursor_at), sqlc.narg(cursor_id)::uuid))
ORDER BY per.year DESC, per.month DESC, p.id DESC
LIMIT @lim;

-- name: ListRunPayslips :many
-- S3: ringkasan slip satu run (tanpa baris), urut employee_no. Cursor = id terakhir (urutan employee_no, id).
SELECT p.id, p.tenant_id, p.employee_id, i.employee_no, i.employee_name, i.net::text AS net, p.status, p.rendered_at, p.attempts,
       per.year, per.month
FROM payslips p
JOIN payroll_items i ON i.tenant_id = p.tenant_id AND i.id = p.item_id
JOIN payroll_runs r ON r.tenant_id = p.tenant_id AND r.id = p.run_id
JOIN payroll_periods per ON per.tenant_id = r.tenant_id AND per.id = r.period_id
WHERE p.run_id = @run_id
  AND (sqlc.narg(employee_id)::uuid IS NULL OR p.employee_id = sqlc.narg(employee_id))
  AND (sqlc.narg(after_no)::text IS NULL OR (i.employee_no, p.id) > (sqlc.narg(after_no), sqlc.narg(after_id)::uuid))
ORDER BY i.employee_no, p.id
LIMIT @lim;

-- name: SetCustomPlans :exec
-- Lihat loadChunk/customPlans (S9 PR-1): rencana per eksekusi dengan nilai tenant aktual.
SET LOCAL plan_cache_mode = force_custom_plan;

-- name: SummarizeItemsByDepartment :many
-- Y6s: total per departemen (saat perhitungan, kolom denormalisasi R3-4) untuk versi terkini, item OK saja.
-- Satu query agregat: tidak bergantung jumlah karyawan di sisi client (tenant 10.000 karyawan, PR-1).
SELECT department_id, count(*)::int AS employees,
       COALESCE(sum(gross), 0)::numeric AS gross, COALESCE(sum(deductions), 0)::numeric AS deductions,
       COALESCE(sum(net), 0)::numeric AS net
FROM payroll_items
WHERE run_id = @run_id AND calc_version = @calc_version AND status = 'OK'
GROUP BY department_id
ORDER BY sum(net) DESC NULLS LAST, department_id;
