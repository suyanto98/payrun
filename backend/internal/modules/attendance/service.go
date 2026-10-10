// Package attendance: catatan kehadiran harian (PF-08, D1–D4). Satu catatan per (employee, work_date);
// work_date = tanggal lokal tenant saat clock-in (shift malam: clock-out boleh keesokan harinya).
//
// Input payroll: catatan INCOMPLETE masuk engine sesuai incomplete_attendance_policy; hari kerja tanpa
// catatan = hadir (engine). Setiap penulis lewat PeriodGuard (tanggal di periode FINALIZED → 409
// PERIOD_LOCKED; periode non-final → change log O12), trigger guard_period_date sebagai jaring pengaman.
package attendance

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/internal/modules/attendance/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/audit"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/db"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/periodguard"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tenantctx"
)

// Employees: employee milik user dan scope atasan (modul employee, ADR-001).
type Employees interface {
	SelfTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, on time.Time) (EmployeeSelf, bool, error)
	ActiveOnTx(ctx context.Context, tx pgx.Tx, employeeID uuid.UUID, on time.Time) (found, active bool, err error)
	DirectReportsTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) ([]uuid.UUID, error)
}

// EmployeeSelf: employee milik user (dipetakan composition root dari employee.Self).
type EmployeeSelf struct {
	EmployeeID uuid.UUID
	Active     bool
}

// Calendar: zona waktu dan hari kerja tenant (modul tenancy).
type Calendar interface {
	LocationTx(ctx context.Context, tx pgx.Tx) (*time.Location, error)
	WorkingDaysTx(ctx context.Context, tx pgx.Tx, from, to time.Time) (int, error)
}

type Service struct {
	pool      *pgxpool.Pool
	clock     clock.Clock
	ids       idgen.Generator
	employees Employees
	calendar  Calendar
	guard     periodguard.Guard
}

func NewService(pool *pgxpool.Pool, c clock.Clock, ids idgen.Generator, e Employees, cal Calendar, g periodguard.Guard) *Service {
	return &Service{pool: pool, clock: c, ids: ids, employees: e, calendar: cal, guard: g}
}

// Record: AttendanceRecord (16-api §16).
type Record struct {
	ID               uuid.UUID  `json:"id"`
	EmployeeID       uuid.UUID  `json:"employee_id"`
	WorkDate         string     `json:"work_date"`
	ClockInAt        *time.Time `json:"clock_in_at"`
	ClockOutAt       *time.Time `json:"clock_out_at"`
	Status           string     `json:"status"`
	Source           string     `json:"source"`
	CorrectionReason *string    `json:"correction_reason"`
	Version          int32      `json:"version"`
}

func toRecord(r store.GetRecordRow) Record {
	return Record{ID: r.ID, EmployeeID: r.EmployeeID, WorkDate: r.WorkDate.Format(time.DateOnly), ClockInAt: r.ClockInAt,
		ClockOutAt: r.ClockOutAt, Status: r.Status, Source: r.Source, CorrectionReason: r.CorrectionReason, Version: r.Version}
}

// localDate: tanggal kalender lokal tenant dari sebuah waktu, sebagai tengah malam UTC (tipe date).
func localDate(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
}

// self: employee milik pemanggil yang aktif pada tanggal kerja; selain itu 422 EMPLOYEE_NOT_ACTIVE.
func (s *Service) self(ctx context.Context, tx pgx.Tx, user uuid.UUID, on time.Time) (uuid.UUID, error) {
	me, ok, err := s.employees.SelfTx(ctx, tx, user, on)
	if err != nil {
		return uuid.Nil, err
	}
	if !ok || !me.Active {
		return uuid.Nil, apperr.ErrEmployeeNotActive
	}
	return me.EmployeeID, nil
}

// ClockIn (D1): idempoten secara natural. Catatan baru → created=true (201); clock-in kedua di hari
// yang sama → catatan yang ada (200), tanpa menimpa waktu pertama.
func (s *Service) ClockIn(ctx context.Context, p auth.Principal, meta reqmeta.Meta) (Record, bool, error) {
	tid, _ := tenantctx.From(ctx)
	now := s.clock.Now()
	var out Record
	created := false
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		loc, err := s.calendar.LocationTx(ctx, tx)
		if err != nil {
			return err
		}
		day := localDate(now, loc)
		emp, err := s.self(ctx, tx, p.UserID, day)
		if err != nil {
			return err
		}
		q := store.New(tx)
		if r, err := q.GetRecordByDay(ctx, store.GetRecordByDayParams{EmployeeID: emp, WorkDate: day}); err == nil {
			out = toRecord(store.GetRecordRow(r)) // tap ganda: tanpa lock dan tanpa change log
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		record, err := s.guard.Lock(ctx, tx, periodguard.Change{Employees: []uuid.UUID{emp}, Range: periodguard.Day(day)})
		if err != nil {
			return err
		}
		r, err := q.InsertClockIn(ctx, store.InsertClockInParams{TenantID: tid, ID: s.ids.New(), EmployeeID: emp, WorkDate: day, At: &now, Actor: &p.UserID})
		if errors.Is(err, pgx.ErrNoRows) { // tap paralel menang lebih dulu
			cur, err := q.GetRecordByDay(ctx, store.GetRecordByDayParams{EmployeeID: emp, WorkDate: day})
			out = toRecord(store.GetRecordRow(cur))
			return err
		}
		if err != nil {
			return err
		}
		out, created = toRecord(store.GetRecordRow(r)), true
		return record(ctx)
	})
	return out, created, db.MapError(err)
}

// ClockOut (D2): menutup catatan INCOMPLETE terakhir ≤ 24 jam (shift malam). Clock-out kedua → catatan
// yang sudah COMPLETE hari itu (200, waktu pertama tidak ditimpa). Tanpa clock-in → 422.
func (s *Service) ClockOut(ctx context.Context, p auth.Principal, meta reqmeta.Meta) (Record, error) {
	now := s.clock.Now()
	var out Record
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		loc, err := s.calendar.LocationTx(ctx, tx)
		if err != nil {
			return err
		}
		today := localDate(now, loc)
		me, ok, err := s.employees.SelfTx(ctx, tx, p.UserID, today)
		if err != nil {
			return err
		}
		if !ok {
			return apperr.ErrEmployeeNotActive
		}
		q := store.New(tx)
		since := now.Add(-24 * time.Hour)
		open, err := q.OpenRecordSince(ctx, store.OpenRecordSinceParams{EmployeeID: me.EmployeeID, Since: &since})
		if errors.Is(err, pgx.ErrNoRows) {
			if r, err := q.GetRecordByDay(ctx, store.GetRecordByDayParams{EmployeeID: me.EmployeeID, WorkDate: today}); err == nil && r.Status == "COMPLETE" {
				out = toRecord(store.GetRecordRow(r))
				return nil
			}
			return apperr.ErrClockOutWithoutIn
		}
		if err != nil {
			return err
		}
		record, err := s.guard.Lock(ctx, tx, periodguard.Change{Employees: []uuid.UUID{me.EmployeeID}, Range: periodguard.Day(open.WorkDate)})
		if err != nil {
			return err
		}
		r, err := q.ClockOut(ctx, store.ClockOutParams{At: &now, Actor: &p.UserID, ID: open.ID})
		if errors.Is(err, pgx.ErrNoRows) { // ditutup paralel di antara baca dan lock
			cur, err := q.GetRecord(ctx, open.ID)
			out = toRecord(cur)
			return err
		}
		if err != nil {
			return err
		}
		out = toRecord(store.GetRecordRow(r))
		return record(ctx)
	})
	return out, db.MapError(err)
}

// ---------------------------------------------------------------- D3 list

type ListQuery struct {
	EmployeeID, Period, From, To, Date, Cursor string
	Limit                                      int32
}

type Summary struct {
	PresentDays    int64 `json:"present_days"`
	IncompleteDays int64 `json:"incomplete_days"`
	WorkingDays    int   `json:"working_days"`
}

func tenantWide(p auth.Principal) bool {
	return p.HasAnyRole(auth.RoleOwner, auth.RoleHRAdmin, auth.RolePayroll)
}

// scope: karyawan yang boleh dilihat. nil = seluruh tenant (O/H/P). MANAGER = diri + bawahan langsung;
// lainnya = diri sendiri (file 17 D3).
func (s *Service) scope(ctx context.Context, tx pgx.Tx, p auth.Principal, today time.Time) ([]uuid.UUID, error) {
	if tenantWide(p) {
		return nil, nil
	}
	ids := []uuid.UUID{}
	if me, ok, err := s.employees.SelfTx(ctx, tx, p.UserID, today); err != nil {
		return nil, err
	} else if ok {
		ids = append(ids, me.EmployeeID)
	}
	if p.HasRole(auth.RoleManager) {
		reports, err := s.employees.DirectReportsTx(ctx, tx, p.UserID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, reports...)
	}
	return ids, nil
}

func parseDate(field, v string) (time.Time, error) {
	d, err := time.Parse(time.DateOnly, v)
	if err != nil || d.Year() < 2000 || d.Year() > 2100 {
		return time.Time{}, apperr.Validation(apperr.FieldError{Field: field, Code: "FORMAT", Message: "must be YYYY-MM-DD"})
	}
	return d, nil
}

// List (D3): ?employee_id&period=YYYY-MM | from&to (≤ 62 hari) | date (HR: rekap harian tanpa employee_id).
func (s *Service) List(ctx context.Context, p auth.Principal, lq ListQuery) ([]Record, httpx.Page, *Summary, error) {
	page := httpx.Page{}
	var from, to time.Time
	var err error
	switch {
	case lq.Period != "":
		pd, perr := time.Parse("2006-01", lq.Period)
		if perr != nil {
			return nil, page, nil, apperr.Validation(apperr.FieldError{Field: "period", Code: "FORMAT", Message: "must be YYYY-MM"})
		}
		from, to = pd, pd.AddDate(0, 1, 0)
	case lq.Date != "":
		if from, err = parseDate("date", lq.Date); err != nil {
			return nil, page, nil, err
		}
		to = from.AddDate(0, 0, 1)
	case lq.From != "" && lq.To != "":
		if from, err = parseDate("from", lq.From); err != nil {
			return nil, page, nil, err
		}
		var last time.Time
		if last, err = parseDate("to", lq.To); err != nil {
			return nil, page, nil, err
		}
		to = last.AddDate(0, 0, 1)
		if !to.After(from) || to.Sub(from) > 62*24*time.Hour {
			return nil, page, nil, apperr.Validation(apperr.FieldError{Field: "to", Code: "OUT_OF_RANGE", Message: "range must be 1..62 days"})
		}
	default:
		return nil, page, nil, apperr.Validation(apperr.FieldError{Field: "period", Code: "REQUIRED", Message: "period, date, or from+to"})
	}
	var employee *uuid.UUID
	if lq.EmployeeID != "" {
		id, err := uuid.Parse(lq.EmployeeID)
		if err != nil {
			return nil, page, nil, apperr.Validation(apperr.FieldError{Field: "employee_id", Code: "FORMAT"})
		}
		employee = &id
	}
	params := store.ListRecordsParams{FromDate: from, ToDate: to, Lim: lq.Limit + 1}
	if lq.Cursor != "" {
		c, err := httpx.DecodeCursor(lq.Cursor)
		if err != nil {
			return nil, page, nil, err
		}
		d := c.At.UTC()
		params.AfterDate, params.AfterID = &d, &c.ID
	}
	out := []Record{}
	var sum *Summary
	err = db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		loc, err := s.calendar.LocationTx(ctx, tx)
		if err != nil {
			return err
		}
		allowed, err := s.scope(ctx, tx, p, localDate(s.clock.Now(), loc))
		if err != nil {
			return err
		}
		switch {
		case employee != nil && allowed != nil && !contains(allowed, *employee):
			return apperr.ErrNotFound // di luar scope: tidak dibedakan dari "tidak ada"
		case employee != nil:
			params.EmployeeIds = []uuid.UUID{*employee}
		case allowed != nil:
			params.EmployeeIds = allowed // EMPLOYEE/MANAGER tanpa employee_id: hanya scope-nya
		}
		q := store.New(tx)
		rs, err := q.ListRecords(ctx, params)
		if err != nil {
			return err
		}
		for _, r := range rs {
			if err := db.GuardTenant(ctx, "attendance_records", r.TenantID); err != nil {
				return err
			}
			out = append(out, toRecord(store.GetRecordRow(r)))
		}
		if employee != nil && lq.Period != "" {
			c, err := q.RecordSummary(ctx, store.RecordSummaryParams{EmployeeID: *employee, FromDate: from, ToDate: to})
			if err != nil {
				return err
			}
			wd, err := s.calendar.WorkingDaysTx(ctx, tx, from, to)
			if err != nil {
				return err
			}
			sum = &Summary{PresentDays: c.PresentDays, IncompleteDays: c.IncompleteDays, WorkingDays: wd}
		}
		return nil
	})
	if err != nil {
		return nil, page, nil, db.MapError(err)
	}
	if int32(len(out)) > lq.Limit { //nolint:gosec // G115: panjang ≤ limit+1
		out = out[:lq.Limit]
		last := out[len(out)-1]
		d, _ := time.Parse(time.DateOnly, last.WorkDate)
		c := httpx.EncodeCursor(httpx.Cursor{At: d, ID: last.ID})
		page = httpx.Page{NextCursor: &c, HasMore: true}
	}
	return out, page, sum, nil
}

func contains(ids []uuid.UUID, id uuid.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- D4 koreksi

// Correction: field yang tidak dikirim mempertahankan nilai saat ini; correction_reason wajib.
type Correction struct {
	ClockInAt        *time.Time `json:"clock_in_at"`
	ClockOutAt       *time.Time `json:"clock_out_at"`
	CorrectionReason string     `json:"correction_reason"`
}

// Precondition: hasil If-Match; Err ditunda sampai otorisasi lolos (catatan implementasi S2).
type Precondition struct {
	Version int32
	Err     error
}

// Correct (D4, PF-08 AC3): OWNER/HR_ADMIN; role lain 403 bila catatannya terlihat, 404 bila tidak.
// Audit before/after. Run CALCULATED untuk periodenya menjadi stale (change log).
func (s *Service) Correct(ctx context.Context, p auth.Principal, id uuid.UUID, pre Precondition, in Correction, meta reqmeta.Meta) (Record, error) {
	in.CorrectionReason = strings.TrimSpace(in.CorrectionReason)
	if n := utf8.RuneCountInString(in.CorrectionReason); n < 1 || n > 500 || strings.ContainsRune(in.CorrectionReason, 0) {
		return Record{}, apperr.Validation(apperr.FieldError{Field: "correction_reason", Code: "LENGTH", Message: "must be 1..500 characters"})
	}
	tid, _ := tenantctx.From(ctx)
	var out Record
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.GetRecord(ctx, id)
		if err != nil {
			return err
		}
		if err := db.GuardTenant(ctx, "attendance_records", cur.TenantID); err != nil {
			return err
		}
		if !p.HasAnyRole(auth.RoleOwner, auth.RoleHRAdmin) {
			loc, err := s.calendar.LocationTx(ctx, tx)
			if err != nil {
				return err
			}
			allowed, err := s.scope(ctx, tx, p, localDate(s.clock.Now(), loc))
			if err != nil {
				return err
			}
			if allowed == nil || contains(allowed, cur.EmployeeID) {
				return apperr.ErrForbidden.WithDetail("Only OWNER or HR_ADMIN can correct attendance")
			}
			return apperr.ErrNotFound
		}
		if pre.Err != nil {
			return pre.Err
		}
		if cur.Version != pre.Version {
			return apperr.ErrVersionConflict
		}
		clockIn, clockOut := cur.ClockInAt, cur.ClockOutAt
		if in.ClockInAt != nil {
			clockIn = in.ClockInAt
		}
		if in.ClockOutAt != nil {
			clockOut = in.ClockOutAt
		}
		if clockIn == nil || (clockOut != nil && !clockOut.After(*clockIn)) {
			return apperr.Validation(apperr.FieldError{Field: "clock_out_at", Code: "OUT_OF_RANGE", Message: "clock_out_at must be after clock_in_at"})
		}
		record, err := s.guard.Lock(ctx, tx, periodguard.Change{Employees: []uuid.UUID{cur.EmployeeID}, Range: periodguard.Day(cur.WorkDate)})
		if err != nil {
			return err
		}
		r, err := q.CorrectRecord(ctx, store.CorrectRecordParams{ClockInAt: clockIn, ClockOutAt: clockOut, Reason: &in.CorrectionReason,
			Actor: &p.UserID, ID: id, Version: pre.Version})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.ErrVersionConflict // diubah paralel di antara baca dan lock
		}
		if err != nil {
			return err
		}
		out = toRecord(store.GetRecordRow(r))
		if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: "attendance.corrected",
			TargetType: "attendance_record", TargetID: id, Reason: in.CorrectionReason,
			Diff: map[string]any{"before": map[string]any{"clock_in_at": cur.ClockInAt, "clock_out_at": cur.ClockOutAt},
				"after": map[string]any{"clock_in_at": clockIn, "clock_out_at": clockOut}}}, meta); err != nil {
			return err
		}
		return record(ctx)
	})
	return out, db.MapError(err)
}
