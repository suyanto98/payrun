package db

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"payrun/internal/platform/apperr"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/observability"
	"payrun/internal/platform/tenantctx"
)

// errTenantMismatch tidak pernah dikirim ke client (menjadi 500 INTERNAL_ERROR).
var errTenantMismatch = errors.New("db: row belongs to another tenant (C-46)")

// GuardTenant adalah pemeriksaan defensif C-46: repository yang men-decode baris tenant-scoped
// membandingkan row.tenant_id dengan tenant request. Seharusnya mustahil berbeda (RLS), jadi kalau
// terjadi, request digagalkan dengan 500, metrik INVARIANT naik, dan log error ditulis: kegagalan
// RLS yang tak terduga terdeteksi, bukan diam-diam mengirim data tenant lain.
func GuardTenant(ctx context.Context, table string, rowTenant uuid.UUID) error {
	want, ok := tenantctx.From(ctx)
	if !ok {
		return ErrNoTenantContext
	}
	return GuardTenantFor(ctx, want, table, rowTenant)
}

// GuardTenantFor sama dengan GuardTenant untuk jalur WithTenantTxFor (tenant eksplisit, contoh accept
// undangan): want = tenant transaksi.
func GuardTenantFor(ctx context.Context, want uuid.UUID, table string, rowTenant uuid.UUID) error {
	if rowTenant == want {
		return nil
	}
	metrics.TenantIsolationViolations.WithLabelValues(table).Inc()
	observability.Logger(ctx).Error("tenant.isolation_violation", "table", table,
		"want_tenant", want.String(), "row_tenant", rowTenant.String())
	return apperr.ErrInternal.WithCause(errTenantMismatch)
}
