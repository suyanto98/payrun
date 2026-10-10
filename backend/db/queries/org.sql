-- Modul org (PF-05): departments, positions. Semua query [tenant] (db.WithTenantTx). Dua tabel
-- berbentuk sama; query ditulis per tabel karena sqlc tidak menerima nama tabel sebagai parameter.

-- name: ListDepartments :many
-- Master data kecil: tanpa pagination, dibatasi 500 per tenant (16-api §G1).
SELECT tenant_id, id, code, name, status, version FROM departments
WHERE status = @status ORDER BY code LIMIT 500;

-- name: GetDepartment :one
SELECT tenant_id, id, code, name, status, version FROM departments WHERE id = @id;

-- name: CreateDepartment :one
INSERT INTO departments (tenant_id, id, code, name, created_by, updated_by)
VALUES (@tenant_id, @id, @code, @name, @actor, @actor)
RETURNING tenant_id, id, code, name, status, version;

-- name: UpdateDepartment :one
-- Optimistic concurrency: 0 baris = versi lama (412) atau tidak ada (404); pemanggil membedakannya.
UPDATE departments SET name = @name, updated_by = @actor, version = version + 1
WHERE id = @id AND version = @version
RETURNING tenant_id, id, code, name, status, version;

-- name: DeactivateDepartment :one
-- Natural idempotent: baris yang sudah INACTIVE tidak diubah (0 baris → pemanggil membaca ulang).
UPDATE departments SET status = 'INACTIVE', updated_by = @actor, version = version + 1
WHERE id = @id AND status = 'ACTIVE'
RETURNING tenant_id, id, code, name, status, version;

-- name: ListPositions :many
SELECT tenant_id, id, code, name, status, version FROM positions
WHERE status = @status ORDER BY code LIMIT 500;

-- name: GetPosition :one
SELECT tenant_id, id, code, name, status, version FROM positions WHERE id = @id;

-- name: CreatePosition :one
INSERT INTO positions (tenant_id, id, code, name, created_by, updated_by)
VALUES (@tenant_id, @id, @code, @name, @actor, @actor)
RETURNING tenant_id, id, code, name, status, version;

-- name: UpdatePosition :one
UPDATE positions SET name = @name, updated_by = @actor, version = version + 1
WHERE id = @id AND version = @version
RETURNING tenant_id, id, code, name, status, version;

-- name: DeactivatePosition :one
UPDATE positions SET status = 'INACTIVE', updated_by = @actor, version = version + 1
WHERE id = @id AND status = 'ACTIVE'
RETURNING tenant_id, id, code, name, status, version;
