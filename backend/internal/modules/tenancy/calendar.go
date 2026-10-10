package tenancy

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"payrun/internal/modules/tenancy/store"
	"payrun/internal/platform/tenantctx"
)

// Kalender kerja tenant (S6): zona waktu untuk tanggal kerja lokal (absensi, lembur) dan jumlah hari
// kerja (hari kerja mingguan − libur) untuk hari cuti. Dipakai modul attendance/leave/overtime lewat
// interface di composition root (ADR-001).

// LocationTx: zona waktu tenant transaksi.
func (s *Service) LocationTx(ctx context.Context, tx pgx.Tx) (*time.Location, error) {
	tid, _ := tenantctx.From(ctx)
	set, err := store.New(tx).CalendarSettings(ctx, tid)
	if err != nil {
		return nil, err
	}
	return time.LoadLocation(set.Timezone)
}

// WorkingDaysTx: hari kerja di [from, to) (tanggal kalender, UTC tengah malam): hari kerja mingguan
// tenant yang bukan libur tenant.
func (s *Service) WorkingDaysTx(ctx context.Context, tx pgx.Tx, from, to time.Time) (int, error) {
	tid, _ := tenantctx.From(ctx)
	q := store.New(tx)
	set, err := q.CalendarSettings(ctx, tid)
	if err != nil {
		return 0, err
	}
	hols, err := q.ListHolidays(ctx, store.ListHolidaysParams{FromDate: from, ToDate: to})
	if err != nil {
		return 0, err
	}
	off := map[string]bool{}
	for _, h := range hols {
		off[h.HolidayDate.Format(time.DateOnly)] = true
	}
	n := 0
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		iso := int16(d.Weekday()) //nolint:gosec // G115: Weekday 0..6
		if iso == 0 {
			iso = 7
		}
		if slices.Contains(set.WorkDays, iso) && !off[d.Format(time.DateOnly)] {
			n++
		}
	}
	return n, nil
}

// OvertimeLimitTx: batas jam lembur per hari (warning OVERTIME_OVER_LIMIT, PF-10 AC3); "" = tanpa batas.
func (s *Service) OvertimeLimitTx(ctx context.Context, tx pgx.Tx) (string, error) {
	tid, _ := tenantctx.From(ctx)
	set, err := store.New(tx).CalendarSettings(ctx, tid)
	if err != nil {
		return "", err
	}
	return set.OvertimeMax, nil
}
