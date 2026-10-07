package tenancy

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/modules/tenancy/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/audit"
	"payrun/internal/platform/db"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tenantctx"
)

// Libur tenant (T4–T6): bagian dari kalender kerja yang dibaca engine (hari kerja = hari kerja mingguan
// − libur). PERIOD_LOCKED untuk tanggal di periode FINALIZED menyusul bersama payroll_periods (S4).

type Holiday struct {
	ID      uuid.UUID `json:"id"`
	Date    string    `json:"date"`
	Name    string    `json:"name"`
	Version int32     `json:"version"`
}

func (s *Service) ListHolidays(ctx context.Context, year int) ([]Holiday, error) {
	if year < 2000 || year > 2100 {
		return nil, apperr.Validation(apperr.FieldError{Field: "year", Code: "OUT_OF_RANGE", Message: "must be 2000..2100"})
	}
	out := []Holiday{}
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		rs, err := store.New(tx).ListHolidays(ctx, store.ListHolidaysParams{
			FromDate: time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC), ToDate: time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			return err
		}
		for _, r := range rs {
			if err := db.GuardTenant(ctx, "holidays", r.TenantID); err != nil {
				return err
			}
			out = append(out, Holiday{ID: r.ID, Date: r.HolidayDate.Format("2006-01-02"), Name: r.Name, Version: r.Version})
		}
		return nil
	})
	return out, db.MapError(err)
}

func (s *Service) CreateHoliday(ctx context.Context, actor uuid.UUID, date, name string, meta reqmeta.Meta) (Holiday, error) {
	name = strings.TrimSpace(name)
	var fe []apperr.FieldError
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		fe = append(fe, apperr.FieldError{Field: "date", Code: "FORMAT", Message: "must be YYYY-MM-DD"})
	}
	if n := utf8.RuneCountInString(name); n < 1 || n > 100 || strings.IndexFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		fe = append(fe, apperr.FieldError{Field: "name", Code: "LENGTH", Message: "must be 1..100 characters"})
	}
	if len(fe) > 0 {
		return Holiday{}, apperr.Validation(fe...)
	}
	tid, _ := tenantctx.From(ctx)
	var h Holiday
	err = db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		r, err := q.CreateHoliday(ctx, store.CreateHolidayParams{TenantID: tid, ID: s.ids.New(), HolidayDate: d, Name: name, Actor: &actor})
		if err != nil {
			return err
		}
		h = Holiday{ID: r.ID, Date: r.HolidayDate.Format("2006-01-02"), Name: r.Name, Version: r.Version}
		return audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "holiday.created",
			TargetType: "holiday", TargetID: h.ID, Diff: map[string]any{"date": h.Date, "name": name}}, meta)
	})
	return h, db.MapError(err)
}

func (s *Service) DeleteHoliday(ctx context.Context, actor, id uuid.UUID, meta reqmeta.Meta) error {
	tid, _ := tenantctx.From(ctx)
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		date, err := store.New(tx).DeleteHoliday(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "holiday.deleted",
			TargetType: "holiday", TargetID: id, Diff: map[string]any{"date": date.Format("2006-01-02")}}, meta)
	})
	return db.MapError(err)
}
