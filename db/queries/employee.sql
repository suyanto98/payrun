-- Modul employee (PF-06, PF-20). Semua query [tenant] (db.WithTenantTx). Kolom PII hanya ciphertext,
-- blind index, dan last4; dekripsi di aplikasi (ADR-P08). hire_date/end_date berasal dari span
-- employment_spans terakhir: end_date = hari kerja terakhir (inklusif) = upper(span) - 1.

-- name: CreateEmployee :exec
INSERT INTO employees (tenant_id, id, employee_no, full_name, work_email, ptkp_status,
                       nik_enc, nik_bidx, nik_last4, npwp_enc, npwp_bidx, npwp_last4,
                       bank_name, bank_account_enc, bank_account_last4, bank_account_holder,
                       pii_key_version, created_by, updated_by)
VALUES (@tenant_id, @id, @employee_no, @full_name, @work_email, @ptkp_status,
        @nik_enc, @nik_bidx, @nik_last4, @npwp_enc, @npwp_bidx, @npwp_last4,
        @bank_name, @bank_account_enc, @bank_account_last4, @bank_account_holder,
        @pii_key_version, @actor, @actor);

-- name: CreateSpan :exec
INSERT INTO employment_spans (tenant_id, id, employee_id, span, created_by, updated_by)
VALUES (@tenant_id, @id, @employee_id, daterange(@hire_date::date, NULL, '[)'), @actor, @actor);

-- name: GetEmployee :one
SELECT e.tenant_id, e.id, e.employee_no, e.full_name, e.work_email, e.user_id, e.status, e.ptkp_status,
       e.nik_last4, e.npwp_last4, e.bank_name, e.bank_account_last4, e.bank_account_holder, e.version,
       lower(s.span)::date AS hire_date, upper_inf(s.span)::bool AS open_ended,
       coalesce(upper(s.span) - 1, lower(s.span))::date AS end_date  -- dipakai hanya jika NOT open_ended
FROM employees e
JOIN LATERAL (SELECT span FROM employment_spans sp
              WHERE sp.tenant_id = e.tenant_id AND sp.employee_id = e.id
              ORDER BY lower(sp.span) DESC LIMIT 1) s ON true
WHERE e.id = @id;

-- name: GetEmployeeIDByUser :one
-- Scope SELF (file 17 §1.2): employee yang tertaut ke user di tenant aktif.
SELECT id FROM employees WHERE user_id = @user_id;

-- name: ListEmployeesByNo :many
-- Tanpa q: urut employee_no, id (keyset). Filter status opsional.
SELECT e.tenant_id, e.id, e.employee_no, e.full_name, e.status, lower(s.span)::date AS hire_date,
       -- LEFT JOIN LATERAL: sqlc tidak menyimpulkan NULL, jadi tanpa assignment = uuid nol / '' (Go → null).
       coalesce(o.department_id, '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS department_id,
       coalesce(o.department_name, '')::text AS department_name,
       coalesce(o.position_id, '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS position_id,
       coalesce(o.position_name, '')::text AS position_name
FROM employees e
JOIN LATERAL (SELECT span FROM employment_spans sp
              WHERE sp.tenant_id = e.tenant_id AND sp.employee_id = e.id
              ORDER BY lower(sp.span) DESC LIMIT 1) s ON true
LEFT JOIN LATERAL (SELECT d.id AS department_id, d.name AS department_name, p.id AS position_id, p.name AS position_name
                   FROM org_assignments oa
                   JOIN departments d ON d.tenant_id = oa.tenant_id AND d.id = oa.department_id
                   JOIN positions p ON p.tenant_id = oa.tenant_id AND p.id = oa.position_id
                   WHERE oa.tenant_id = e.tenant_id AND oa.employee_id = e.id AND oa.valid_during @> @as_of::date) o ON true
WHERE (sqlc.narg(status)::text IS NULL OR e.status = sqlc.narg(status))
  AND (sqlc.narg(manager_id)::uuid IS NULL OR e.manager_employee_id = sqlc.narg(manager_id))
  AND (sqlc.narg(department_id)::uuid IS NULL OR o.department_id = sqlc.narg(department_id))
  AND (sqlc.narg(after_key)::text IS NULL OR (e.employee_no, e.id) > (sqlc.narg(after_key)::text, sqlc.narg(after_id)::uuid))
ORDER BY e.employee_no, e.id
LIMIT @lim;

-- name: ListEmployeesByName :many
-- Dengan q: prefix pada lower(full_name) atau employee_no (tanpa pg_trgm di V1), urut lower(full_name), id.
-- prefix sudah di-escape aplikasi (%, _, \).
SELECT e.tenant_id, e.id, e.employee_no, e.full_name, e.status, lower(s.span)::date AS hire_date,
       lower(e.full_name)::text AS sort_key, -- LEFT JOIN LATERAL: sqlc tidak menyimpulkan NULL, jadi tanpa assignment = uuid nol / '' (Go → null).
       coalesce(o.department_id, '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS department_id,
       coalesce(o.department_name, '')::text AS department_name,
       coalesce(o.position_id, '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS position_id,
       coalesce(o.position_name, '')::text AS position_name
FROM employees e
JOIN LATERAL (SELECT span FROM employment_spans sp
              WHERE sp.tenant_id = e.tenant_id AND sp.employee_id = e.id
              ORDER BY lower(sp.span) DESC LIMIT 1) s ON true
LEFT JOIN LATERAL (SELECT d.id AS department_id, d.name AS department_name, p.id AS position_id, p.name AS position_name
                   FROM org_assignments oa
                   JOIN departments d ON d.tenant_id = oa.tenant_id AND d.id = oa.department_id
                   JOIN positions p ON p.tenant_id = oa.tenant_id AND p.id = oa.position_id
                   WHERE oa.tenant_id = e.tenant_id AND oa.employee_id = e.id AND oa.valid_during @> @as_of::date) o ON true
WHERE (lower(e.full_name) LIKE @prefix::text || '%' ESCAPE '\' OR lower(e.employee_no) LIKE @prefix::text || '%' ESCAPE '\')
  AND (sqlc.narg(status)::text IS NULL OR e.status = sqlc.narg(status))
  AND (sqlc.narg(manager_id)::uuid IS NULL OR e.manager_employee_id = sqlc.narg(manager_id))
  AND (sqlc.narg(department_id)::uuid IS NULL OR o.department_id = sqlc.narg(department_id))
  AND (sqlc.narg(after_key)::text IS NULL OR (lower(e.full_name), e.id) > (sqlc.narg(after_key)::text, sqlc.narg(after_id)::uuid))
ORDER BY lower(e.full_name), e.id
LIMIT @lim;

-- name: UpdateEmployee :one
-- PATCH parsial: NULL = tidak diubah. Field PII datang sebagai triple (enc, bidx, last4) yang sudah
-- dihitung aplikasi; ganti salah satu = ganti ketiganya (ck_employees_*_pair).
UPDATE employees SET
    full_name           = coalesce(sqlc.narg(full_name), full_name),
    work_email          = coalesce(sqlc.narg(work_email), work_email),
    ptkp_status         = coalesce(sqlc.narg(ptkp_status), ptkp_status),
    nik_enc             = coalesce(sqlc.narg(nik_enc), nik_enc),
    nik_bidx            = coalesce(sqlc.narg(nik_bidx), nik_bidx),
    nik_last4           = coalesce(sqlc.narg(nik_last4), nik_last4),
    npwp_enc            = coalesce(sqlc.narg(npwp_enc), npwp_enc),
    npwp_bidx           = coalesce(sqlc.narg(npwp_bidx), npwp_bidx),
    npwp_last4          = coalesce(sqlc.narg(npwp_last4), npwp_last4),
    bank_name           = coalesce(sqlc.narg(bank_name), bank_name),
    bank_account_enc    = coalesce(sqlc.narg(bank_account_enc), bank_account_enc),
    bank_account_last4  = coalesce(sqlc.narg(bank_account_last4), bank_account_last4),
    bank_account_holder = coalesce(sqlc.narg(bank_account_holder), bank_account_holder),
    pii_key_version     = coalesce(sqlc.narg(pii_key_version), pii_key_version),
    updated_by = @actor, version = version + 1
WHERE id = @id AND version = @version
RETURNING id;

-- name: TerminateEmployee :one
-- Guard status ACTIVE + versi (If-Match): terminate kedua → 0 baris → pemanggil membedakan 404/412/409.
UPDATE employees SET status = @status, updated_by = @actor, version = version + 1
WHERE id = @id AND version = @version AND status = 'ACTIVE'
RETURNING id;

-- name: CloseOpenSpan :execrows
-- Span terbuka ditutup di end_date + 1 (batas atas eksklusif). Span harus mulai sebelum batas itu.
UPDATE employment_spans
SET span = daterange(lower(span), @end_exclusive::date, '[)'), end_reason = @end_reason, updated_by = @actor,
    version = version + 1
WHERE employee_id = @employee_id AND upper_inf(span) AND lower(span) < @end_exclusive::date;

-- name: GetEmployeeLinkForUpdate :one
-- Dipakai undangan (iam, lewat interface di composition root): employee ada di tenant ini dan
-- belum tertaut ke user. FOR UPDATE: dua undangan paralel untuk employee yang sama diserialkan.
SELECT tenant_id, user_id, status FROM employees WHERE id = @id FOR UPDATE;

-- name: LinkEmployeeUser :execrows
UPDATE employees SET user_id = @user_id, updated_by = @user_id, version = version + 1
WHERE id = @id AND user_id IS NULL;

-- name: GetEmployeeAccess :one
-- Otorisasi lintas modul (AccessTx): atasan saat ini + tanggal mulai kerja (span pertama).
SELECT e.tenant_id, e.id, e.manager_employee_id, e.status,
       (SELECT min(lower(sp.span)) FROM employment_spans sp
        WHERE sp.tenant_id = e.tenant_id AND sp.employee_id = e.id)::date AS hire_date
FROM employees e WHERE e.id = @id;

-- ---------------------------------------------------------------- org assignment & atasan (Sprint 3)
-- Nama departemen/posisi dibaca lewat JOIN read-only ke tabel modul org (tampilan saja; penulisan
-- tetap lewat modul org). Penyimpangan ADR-001 yang disengaja, dicatat di 16-api-endpoints S3.

-- name: CloseOrgAssignmentAt :execrows
UPDATE org_assignments
SET valid_during = daterange(lower(valid_during), @valid_from::date, '[)'), updated_by = @actor, version = version + 1
WHERE employee_id = @employee_id AND valid_during @> @valid_from::date AND lower(valid_during) < @valid_from::date;

-- name: InsertOrgAssignment :one
INSERT INTO org_assignments (tenant_id, id, employee_id, department_id, position_id, valid_during, created_by, updated_by)
VALUES (@tenant_id, @id, @employee_id, @department_id, @position_id, daterange(@valid_from::date, NULL, '[)'), @actor, @actor)
RETURNING id;

-- name: SetManager :execrows
-- Atasan saat ini (S14). Siklus → trigger PI-19 (P0005 → 422 MANAGER_CYCLE).
UPDATE employees SET manager_employee_id = sqlc.narg(manager_id), updated_by = @actor, version = version + 1
WHERE id = @id;

-- name: GetCurrentOrg :one
SELECT oa.id, lower(oa.valid_during)::date AS valid_from, d.id AS department_id, d.name AS department_name,
       p.id AS position_id, p.name AS position_name
FROM org_assignments oa
JOIN departments d ON d.tenant_id = oa.tenant_id AND d.id = oa.department_id
JOIN positions p ON p.tenant_id = oa.tenant_id AND p.id = oa.position_id
WHERE oa.employee_id = @employee_id AND oa.valid_during @> @as_of::date;

-- name: GetManagerRef :one
SELECT m.id, m.full_name FROM employees e JOIN employees m ON m.tenant_id = e.tenant_id AND m.id = e.manager_employee_id
WHERE e.id = @id;

-- name: GetManagerStatus :one
SELECT tenant_id, status FROM employees WHERE id = @id;

-- name: CountAssignedToday :one
-- G4: departemen/posisi yang masih dipakai assignment yang berlaku hari ini tidak boleh dinonaktifkan.
SELECT count(*) FROM org_assignments
WHERE valid_during @> @as_of::date AND (department_id = sqlc.narg(department_id) OR position_id = sqlc.narg(position_id));

-- name: GetEmployeePII :one
-- [tenant] Ciphertext PII untuk full view W6 (Sprint 4); hanya dibaca di transaksi yang juga menulis
-- audit employee.pii_viewed.
SELECT tenant_id, nik_enc, npwp_enc, bank_name, bank_account_enc, bank_account_holder
FROM employees WHERE id = @id;

-- ---------------------------------------------------------------- dipakai modul input (S6)

-- name: EmployeeSelf :one
-- [tenant] Employee milik user + apakah aktif pada tanggal itu + user atasan langsung saat ini (S14:
-- kolom manager_employee_id, bukan effective-dated). Approver cuti/lembur = atasan ini (ADR-P09).
SELECT e.id, m.user_id AS manager_user_id,
       EXISTS (SELECT 1 FROM employment_spans s WHERE s.tenant_id = e.tenant_id AND s.employee_id = e.id
               AND s.span @> @on_date::date) AS active
FROM employees e
LEFT JOIN employees m ON m.tenant_id = e.tenant_id AND m.id = e.manager_employee_id
WHERE e.user_id = @user_id;

-- name: EmployeeActiveOn :one
-- [tenant] Employee ada di tenant ini dan aktif pada tanggal itu.
SELECT EXISTS (SELECT 1 FROM employment_spans s WHERE s.employee_id = @employee_id AND s.span @> @on_date::date) AS active,
       EXISTS (SELECT 1 FROM employees e WHERE e.id = @employee_id) AS found;

-- name: DirectReportIDs :many
-- [tenant] Bawahan langsung saat ini (scope 👥 MANAGER, file 17).
SELECT id FROM employees WHERE manager_employee_id = @manager_id ORDER BY id;
