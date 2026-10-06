// Package tenantctx membawa tenant aktif request di context (ADR-P02). Diisi HANYA oleh middleware
// auth dari claim `tid` access token yang sudah diverifikasi, lalu dibaca db.WithTenantTx. Tenant tidak
// pernah berasal dari path, header, query, atau body (docs/design/16-api-endpoints.md §1).
//
// Package kecil tersendiri supaya platform/db tidak perlu meng-import platform/auth.
package tenantctx

import (
	"context"

	"github.com/google/uuid"
)

type key struct{}

// With menaruh tenant aktif di context.
func With(ctx context.Context, tenantID uuid.UUID) context.Context {
	return context.WithValue(ctx, key{}, tenantID)
}

// From mengembalikan tenant aktif; ok=false kalau request tidak punya tenant (token tanpa `tid`).
func From(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(key{}).(uuid.UUID)
	return id, ok && id != uuid.Nil
}
