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
SELECT e.tenant_id, e.id, e.employee_no, e.full_name, e.status, lower(s.span)::date AS hire_date
FROM employees e
JOIN LATERAL (SELECT span FROM employment_spans sp
              WHERE sp.tenant_id = e.tenant_id AND sp.employee_id = e.id
              ORDER BY lower(sp.span) DESC LIMIT 1) s ON true
WHERE (sqlc.narg(status)::text IS NULL OR e.status = sqlc.narg(status))
  AND (sqlc.narg(after_key)::text IS NULL OR (e.employee_no, e.id) > (sqlc.narg(after_key)::text, sqlc.narg(after_id)::uuid))
ORDER BY e.employee_no, e.id
LIMIT @lim;

-- name: ListEmployeesByName :many
-- Dengan q: prefix pada lower(full_name) atau employee_no (tanpa pg_trgm di V1), urut lower(full_name), id.
-- prefix sudah di-escape aplikasi (%, _, \).
SELECT e.tenant_id, e.id, e.employee_no, e.full_name, e.status, lower(s.span)::date AS hire_date,
       lower(e.full_name)::text AS sort_key
FROM employees e
JOIN LATERAL (SELECT span FROM employment_spans sp
              WHERE sp.tenant_id = e.tenant_id AND sp.employee_id = e.id
              ORDER BY lower(sp.span) DESC LIMIT 1) s ON true
WHERE (lower(e.full_name) LIKE @prefix::text || '%' ESCAPE '\' OR lower(e.employee_no) LIKE @prefix::text || '%' ESCAPE '\')
  AND (sqlc.narg(status)::text IS NULL OR e.status = sqlc.narg(status))
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
