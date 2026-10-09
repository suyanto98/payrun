-- Modul approval (PF-11, ADR-P09): satu mesin approval untuk run, cuti, lembur. Semua [tenant].
-- Urutan lock: subjek ([2]/[3], oleh handler subjek) → approval [4] (DecideApproval).

-- name: CreateApproval :exec
-- Dibuat oleh use case submit subjek di transaksi yang sama (polimorfik tanpa FK fisik, 12-db §6.6).
-- Pending ganda → uq_approval_requests_pending; self tanpa flag/alasan → ck_approval_requests_not_self.
INSERT INTO approval_requests (tenant_id, id, subject_type, subject_id, requester_id, approver_id, self_approved, self_approval_reason)
VALUES (@tenant_id, @id, @subject_type, @subject_id, @requester_id, @approver_id, @self_approved, @self_approval_reason);

-- name: GetApproval :one
-- display_name hanya terlihat untuk anggota ACTIVE (users_tenant_member_read) → bisa NULL.
SELECT a.tenant_id, a.id, a.subject_type, a.subject_id, a.requester_id, ru.display_name AS requester_name,
       a.approver_id, au.display_name AS approver_name, a.status, a.decision_note, a.decided_by, a.is_override,
       a.self_approved, a.self_approval_reason, a.decided_at, a.created_at
FROM approval_requests a
LEFT JOIN users ru ON ru.id = a.requester_id
LEFT JOIN users au ON au.id = a.approver_id
WHERE a.id = @id;

-- name: DecideApproval :execrows
-- [4] Guard optimistik (ADR-P09): hanya approver yang di-snapshot, hanya dari PENDING. 0 baris → keputusan
-- lain menang lebih dulu (approve vs reject, PFS-08).
UPDATE approval_requests
SET status = @status, decision_note = @note, decided_by = @actor, decided_at = clock_timestamp()
WHERE id = @id AND status = 'PENDING' AND approver_id = @actor;

-- name: ListApprovals :many
-- Inbox (R1): milik approver (atau semua untuk HR scope=all), keyset created_at DESC, id DESC.
SELECT a.tenant_id, a.id, a.subject_type, a.subject_id, a.requester_id, ru.display_name AS requester_name,
       a.approver_id, au.display_name AS approver_name, a.status, a.decision_note, a.decided_by, a.is_override,
       a.self_approved, a.self_approval_reason, a.decided_at, a.created_at
FROM approval_requests a
LEFT JOIN users ru ON ru.id = a.requester_id
LEFT JOIN users au ON au.id = a.approver_id
WHERE (sqlc.narg(approver_id)::uuid IS NULL OR a.approver_id = sqlc.narg(approver_id))
  AND a.status = @status
  AND (sqlc.narg(subject_type)::text IS NULL OR a.subject_type = sqlc.narg(subject_type))
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (a.created_at, a.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY a.created_at DESC, a.id DESC
LIMIT @lim;
