-- Modul compensation (PF-07): component_definitions, salary_assignments, employee_components. Semua
-- [tenant]. Nilai numeric dibaca/ditulis sebagai pgtype.Numeric (koefisien + eksponen, eksak) dan
-- dikonversi ke decimal di Go; JSON mengirim string (ADR-P06). Tidak ada float di jalur uang.

-- name: ListComponentDefs :many
SELECT tenant_id, id, code, name, category, stage, calc_type,
       default_amount, rate, ruleset_ref, base_ref, day_source, cap_min, cap_max, taxable, bpjs_base, rounding, is_system, status, version
FROM component_definitions
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(category)::text IS NULL OR category = sqlc.narg(category))
ORDER BY is_system DESC, code
LIMIT 200;

-- name: GetComponentDef :one
SELECT tenant_id, id, code, name, category, stage, calc_type,
       default_amount, rate, ruleset_ref, base_ref, day_source, cap_min, cap_max, taxable, bpjs_base, rounding, is_system, status, version
FROM component_definitions WHERE id = @id;

-- name: CreateComponentDef :one
INSERT INTO component_definitions (tenant_id, id, code, name, category, stage, calc_type, default_amount, rate,
                                   base_ref, day_source, cap_min, cap_max, taxable, bpjs_base, rounding, created_by, updated_by)
VALUES (@tenant_id, @id, @code, @name, @category, 'EARNING', @calc_type, sqlc.narg(default_amount)::numeric,
        sqlc.narg(rate)::numeric, sqlc.narg(base_ref), sqlc.narg(day_source), sqlc.narg(cap_min)::numeric,
        sqlc.narg(cap_max)::numeric, @taxable, @bpjs_base, @rounding, @actor, @actor)
RETURNING id;

-- name: UpdateComponentDef :one
-- C3 parsial (NULL = tidak diubah); code/category/calc_type immutable. Guard versi (If-Match).
UPDATE component_definitions SET
    name           = coalesce(sqlc.narg(name), name),
    default_amount = coalesce(sqlc.narg(default_amount)::numeric, default_amount),
    rate           = coalesce(sqlc.narg(rate)::numeric, rate),
    base_ref       = coalesce(sqlc.narg(base_ref), base_ref),
    cap_min        = coalesce(sqlc.narg(cap_min)::numeric, cap_min),
    cap_max        = coalesce(sqlc.narg(cap_max)::numeric, cap_max),
    taxable        = coalesce(sqlc.narg(taxable), taxable),
    bpjs_base      = coalesce(sqlc.narg(bpjs_base), bpjs_base),
    rounding       = coalesce(sqlc.narg(rounding), rounding),
    updated_by = @actor, version = version + 1
WHERE id = @id AND version = @version
RETURNING id;

-- name: DeactivateComponentDef :execrows
UPDATE component_definitions SET status = 'INACTIVE', updated_by = @actor, version = version + 1
WHERE id = @id AND status = 'ACTIVE';

-- name: ListSalary :many
SELECT tenant_id, id, employee_id, base_salary, lower(valid_during)::date AS valid_from,
       upper_inf(valid_during)::bool AS open_ended, coalesce(upper(valid_during), lower(valid_during))::date AS valid_to,
       change_reason, created_by, created_at
FROM salary_assignments
WHERE employee_id = @employee_id
  AND (sqlc.narg(as_of)::date IS NULL OR valid_during @> sqlc.narg(as_of)::date)
ORDER BY lower(valid_during);

-- name: CloseSalaryAt :execrows
-- Pola effective-dated (ADR-P04): record yang berlaku pada @from (dan dimulai SEBELUM @from) ditutup di
-- @from. Record yang dimulai tepat di @from atau sesudahnya tidak disentuh: INSERT berikutnya akan
-- bentrok di EXCLUDE → 409 OVERLAPPING_PERIOD ("batalkan perubahan masa depan dulu").
UPDATE salary_assignments
SET valid_during = daterange(lower(valid_during), @valid_from::date, '[)'), updated_by = @actor, version = version + 1
WHERE employee_id = @employee_id AND valid_during @> @valid_from::date AND lower(valid_during) < @valid_from::date;

-- name: InsertSalary :one
INSERT INTO salary_assignments (tenant_id, id, employee_id, base_salary, valid_during, change_reason, created_by, updated_by)
VALUES (@tenant_id, @id, @employee_id, @base_salary::numeric, daterange(@valid_from::date, NULL, '[)'), sqlc.narg(change_reason), @actor, @actor)
RETURNING id;

-- name: ListEmployeeComponents :many
SELECT ec.tenant_id, ec.id, ec.employee_id, cd.id AS component_id, cd.code, cd.name, cd.calc_type,
       ec.amount, ec.rate, lower(ec.valid_during)::date AS valid_from,
       upper_inf(ec.valid_during)::bool AS open_ended, coalesce(upper(ec.valid_during), lower(ec.valid_during))::date AS valid_to
FROM employee_components ec
JOIN component_definitions cd ON cd.tenant_id = ec.tenant_id AND cd.id = ec.component_id
WHERE ec.employee_id = @employee_id
  AND (sqlc.narg(as_of)::date IS NULL OR ec.valid_during @> sqlc.narg(as_of)::date)
ORDER BY cd.code, lower(ec.valid_during);

-- name: CloseEmployeeComponentAt :execrows
UPDATE employee_components
SET valid_during = daterange(lower(valid_during), @valid_from::date, '[)'), updated_by = @actor, version = version + 1
WHERE employee_id = @employee_id AND component_id = @component_id
  AND valid_during @> @valid_from::date AND lower(valid_during) < @valid_from::date;

-- name: InsertEmployeeComponent :one
INSERT INTO employee_components (tenant_id, id, employee_id, component_id, amount, rate, valid_during, created_by, updated_by)
VALUES (@tenant_id, @id, @employee_id, @component_id, sqlc.narg(amount)::numeric, sqlc.narg(rate)::numeric,
        daterange(@valid_from::date, sqlc.narg(to_date)::date, '[)'), @actor, @actor)
RETURNING id;

-- name: ListAllComponentDefs :many
-- Validasi graf: SEMUA definisi (tanpa LIMIT tampilan C1; review S3).
SELECT tenant_id, id, code, name, category, stage, calc_type, default_amount, rate, ruleset_ref, base_ref, day_source,
       cap_min, cap_max, taxable, bpjs_base, rounding, is_system, status, version
FROM component_definitions ORDER BY code;

-- name: LockComponentGraph :exec
-- Perubahan dependensi diserialkan per tenant: dua PATCH paralel (A→B dan B→A) tidak bisa sama-sama
-- lolos validasi siklus di Read Committed (Step 7 `22` §1, review S3).
SELECT pg_advisory_xact_lock(hashtextextended('comp:' || @tenant_id::text, 0));

-- name: GetEmployeeComponentAt :one
-- Record komponen yang berlaku pada @valid_from (untuk mempertahankan ekornya saat override sementara).
SELECT id, amount, rate, upper_inf(valid_during)::bool AS open_ended, coalesce(upper(valid_during), lower(valid_during))::date AS to_date
FROM employee_components
WHERE employee_id = @employee_id AND component_id = @component_id AND valid_during @> @valid_from::date
  AND lower(valid_during) < @valid_from::date;
