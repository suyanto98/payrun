// Package audit menulis audit_log dari modul bisnis (S2+), di transaksi YANG SAMA dengan perubahan
// yang diaudit: kalau audit gagal ditulis, perubahannya ikut batal (tidak ada aksi tanpa jejak).
// Modul iam/tenancy (S1) memakai query sqlc-nya sendiri; isinya sama.
package audit

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/platform/reqmeta"
)

// Entry adalah satu baris audit_log tenant. TenantID wajib (baris platform hanya dari iam/tenancy).
type Entry struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID // user yang bertindak (actor_type USER)
	Action     string    // "<domain>.<aksi>", contoh "employee.created"
	TargetType string
	TargetID   uuid.UUID
	// Diff: field yang berubah. JANGAN memasukkan nilai PII mentah (ADR-P08): cukup nama field.
	Diff   map[string]any
	Reason string
}

// Record menulis entry. tx harus transaksi tenant yang sama (RLS WITH CHECK audit_tenant).
func Record(ctx context.Context, tx pgx.Tx, id uuid.UUID, e Entry, meta reqmeta.Meta) error {
	var diff []byte
	if len(e.Diff) > 0 {
		b, err := json.Marshal(e.Diff)
		if err != nil {
			return err
		}
		diff = b
	}
	ua := meta.UserAgent
	if len(ua) > 512 {
		ua = ua[:512]
	}
	_, err := tx.Exec(ctx, `-- name: AuditRecord
		INSERT INTO audit_log (id, tenant_id, actor_type, actor_id, action, target_type, target_id,
		                       request_id, ip, user_agent, diff, reason)
		VALUES ($1, $2, 'USER', $3, $4, $5, $6, nullif($7, ''), $8, nullif($9, ''), $10, nullif($11, ''))`,
		id, e.TenantID, e.ActorID, e.Action, e.TargetType, e.TargetID, meta.RequestID, meta.IP, ua, diff, e.Reason)
	return err
}
