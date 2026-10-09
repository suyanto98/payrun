package payroll

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/modules/payroll/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/db"
	"payrun/internal/platform/periodguard"
	"payrun/internal/platform/tenantctx"
	"payrun/internal/platform/testhook"
)

// PeriodGuard mengimplementasikan periodguard.Guard (kontrak dan urutan lock ada di paket itu).
type PeriodGuard struct{}

var _ periodguard.Guard = PeriodGuard{}

// Lock: [1b] tenant_settings FOR KEY SHARE → [2] periode FOR SHARE (year, month ascending). Periode
// FINALIZED yang beririsan dengan rentang yang berubah → 409 PERIOD_LOCKED. Periode yang belum ada
// tidak perlu dicatat: pembuatannya (FOR UPDATE pada tenant_settings) menunggu transaksi ini, sehingga
// kalkulasi berikutnya pasti melihat perubahannya (D1).
func (PeriodGuard) Lock(ctx context.Context, tx pgx.Tx, c periodguard.Change) (periodguard.Recorder, error) {
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return nil, db.ErrNoTenantContext
	}
	q := store.New(tx)
	if err := q.GuardLockSettings(ctx, tid); err != nil {
		return nil, db.MapError(err)
	}
	var open []uuid.UUID
	if c.NotEffectiveDated {
		rows, err := q.GuardLockOpenPeriods(ctx)
		if err != nil {
			return nil, db.MapError(err)
		}
		for _, r := range rows {
			open = append(open, r.ID)
		}
	} else {
		rows, err := q.GuardLockPeriods(ctx, store.GuardLockPeriodsParams{FromDate: c.Range.From, ToDate: c.Range.To})
		if err != nil {
			return nil, db.MapError(err)
		}
		for _, r := range rows {
			if r.Status == "FINALIZED" {
				return nil, apperr.ErrPeriodLocked.WithDetail("The change touches a finalized payroll period; use an adjustment instead")
			}
			open = append(open, r.ID)
		}
	}
	// Titik hook test interleaving (O3/O12/D1): penulis memegang lock [1b]/[2] dan belum menulis.
	if err := testhook.Pause(ctx, "periodguard.after_lock"); err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		if len(open) == 0 {
			return nil
		}
		var err error
		switch {
		case c.AllEmployees:
			err = q.RecordInputChangesAllEmployees(ctx, store.RecordInputChangesAllEmployeesParams{TenantID: tid, PeriodIds: open})
		case c.ComponentID != nil:
			err = q.RecordInputChangesComponent(ctx, store.RecordInputChangesComponentParams{TenantID: tid, PeriodIds: open, ComponentID: *c.ComponentID})
		case len(c.Employees) > 0:
			err = q.RecordInputChanges(ctx, store.RecordInputChangesParams{TenantID: tid, PeriodIds: open, EmployeeIds: c.Employees})
		}
		if err != nil {
			return db.MapError(err)
		}
		// Titik hook: changed_at sudah diambil, transaksi penulis belum commit (O12 d1).
		return testhook.Pause(ctx, "periodguard.after_record")
	}, nil
}
