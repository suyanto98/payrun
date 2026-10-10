-- platform/idempotency (ADR-L02 pola a), tenant-scoped: semua query di dalam WithTenantTx, sehingga
-- key tenant lain tidak pernah terlihat (RLS tenant_isolation) dan tenant_id selalu tenant aktif.

-- name: InsertKey :one
-- Kalau key yang sama sedang ditulis transaksi lain yang belum selesai, INSERT ini MENUNGGU di unique
-- index sampai transaksi itu commit/rollback (dibatasi lock_timeout). Konflik → DO NOTHING → 0 baris
-- (pgx.ErrNoRows) → pemanggil membaca baris yang sudah ada.
INSERT INTO idempotency_keys (tenant_id, id, client_id, endpoint, key, request_hash, expires_at)
VALUES (@tenant_id, @id, @client_id, @endpoint, @key, @request_hash, @expires_at)
ON CONFLICT ON CONSTRAINT uq_idempotency_keys_scope DO NOTHING
RETURNING id;

-- name: GetKey :one
SELECT id, request_hash, response_status, response_body, expires_at
FROM idempotency_keys
WHERE tenant_id = @tenant_id AND client_id = @client_id AND endpoint = @endpoint AND key = @key;

-- name: DeleteKey :exec
-- Key kedaluwarsa (retensi 7 hari) dianggap baru: dihapus lalu INSERT diulang.
DELETE FROM idempotency_keys WHERE tenant_id = @tenant_id AND id = @id;

-- name: CompleteKey :exec
UPDATE idempotency_keys
SET response_status = @response_status, response_body = @response_body,
    resource_type = @resource_type, resource_id = @resource_id
WHERE tenant_id = @tenant_id AND id = @id;
