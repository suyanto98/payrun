package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"payrun/internal/platform/apperr"
	"payrun/internal/platform/db"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/tenantctx"
)

// tenant_guard_detects_foreign_row (C-46): baris tenant lain yang lolos (seharusnya mustahil karena
// RLS) menggagalkan request dengan 500 dan menaikkan metrik INVARIANT, bukan dikirim ke client.
func TestTenantGuard_DetectsForeignRow(t *testing.T) {
	alpha, beta := uuid.New(), uuid.New()
	ctx := tenantctx.With(context.Background(), alpha)
	if err := db.GuardTenant(ctx, "employees", alpha); err != nil {
		t.Fatalf("own row: %v", err)
	}
	before := testutil.ToFloat64(metrics.TenantIsolationViolations.WithLabelValues("employees"))
	err := db.GuardTenant(ctx, "employees", beta)
	if !errors.Is(err, apperr.ErrInternal) {
		t.Fatalf("foreign row: want INTERNAL_ERROR, got %v", err)
	}
	if got := testutil.ToFloat64(metrics.TenantIsolationViolations.WithLabelValues("employees")); got != before+1 {
		t.Fatalf("violation metric: got %v, want %v", got, before+1)
	}
	if err := db.GuardTenant(context.Background(), "employees", alpha); !errors.Is(err, db.ErrNoTenantContext) {
		t.Fatalf("no tenant context must fail closed, got %v", err)
	}
}
