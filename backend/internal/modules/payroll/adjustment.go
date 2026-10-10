package payroll

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"payrun/internal/modules/payroll/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/audit"
	"payrun/internal/platform/db"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/periodguard"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tenantctx"
)

// Adjustment (PF-19, Step 3 `07` §11, PI-15): koreksi periode FINALIZED yang masuk ke run periode
// target sebagai baris ADJUSTMENT. Target disimpan sebagai (tahun, bulan) (R1-4). Pembuatan dan
// pembatalan lewat PeriodGuard periode target: target FINALIZED → 409 PERIOD_LOCKED; target yang
// sudah dikalkulasi tercatat di payroll_input_changes → run stale (PF-19 AC4, ADR-P07). PENDING_APPLY
// → APPLIED di transaksi finalize (ApplyAdjustments).

var adjustmentComponents = map[string]bool{"OVERTIME_CORRECTION": true, "DEDUCTION_CORRECTION": true, "MANUAL": true}

var maxAdjustment = decimal.NewFromInt(1_000_000_000)

type Adjustment struct {
	ID            uuid.UUID  `json:"id"`
	EmployeeID    uuid.UUID  `json:"employee_id"`
	OriginPeriod  string     `json:"origin_period"`
	TargetPeriod  string     `json:"target_period"`
	ComponentCode string     `json:"component_code"`
	Amount        string     `json:"amount"`
	Taxable       bool       `json:"taxable"`
	Reason        string     `json:"reason"`
	Status        string     `json:"status"`
	AppliedRunID  *uuid.UUID `json:"applied_run_id"`
	AppliedAt     *time.Time `json:"applied_at"`
	CancelledAt   *time.Time `json:"cancelled_at"`
	CreatedBy     *uuid.UUID `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
}

type AdjustmentInput struct {
	EmployeeID    uuid.UUID `json:"employee_id"`
	OriginPeriod  string    `json:"origin_period"`
	TargetPeriod  string    `json:"target_period"`
	ComponentCode string    `json:"component_code"`
	Amount        string    `json:"amount"`
	Taxable       *bool     `json:"taxable"`
	Reason        string    `json:"reason"`
}

type CancelAdjustmentInput struct {
	Reason string `json:"reason"`
}

func toAdjustment(r store.GetAdjustmentRow) Adjustment {
	return Adjustment{ID: r.ID, EmployeeID: r.EmployeeID, OriginPeriod: periodText(r.OriginYear, r.OriginMonth),
		TargetPeriod: periodText(r.TargetYear, r.TargetMonth), ComponentCode: r.ComponentCode, Amount: r.Amount, Taxable: r.Taxable,
		Reason: r.Reason, Status: r.Status, AppliedRunID: r.AppliedRunID, AppliedAt: r.AppliedAt, CancelledAt: r.CancelledAt,
		CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt}
}

func fieldPeriod(field, v string) (int, int, error) {
	y, m, err := parsePeriod(v)
	if err != nil {
		var e *apperr.Error
		if errors.As(err, &e) && len(e.Fields) == 1 {
			f := e.Fields[0]
			f.Field = field
			return 0, 0, apperr.Validation(f)
		}
		return 0, 0, err
	}
	return y, m, nil
}

func validateAdjustment(in AdjustmentInput) (decimal.Decimal, [4]int, error) {
	var fields []apperr.FieldError
	var ym [4]int
	if in.EmployeeID == uuid.Nil {
		fields = append(fields, apperr.FieldError{Field: "employee_id", Code: "REQUIRED"})
	}
	if !adjustmentComponents[in.ComponentCode] {
		fields = append(fields, apperr.FieldError{Field: "component_code", Code: "INVALID_ENUM", Message: "OVERTIME_CORRECTION, DEDUCTION_CORRECTION or MANUAL"})
	}
	amt, err := decimal.NewFromString(in.Amount)
	switch {
	case err != nil:
		fields = append(fields, apperr.FieldError{Field: "amount", Code: "FORMAT", Message: "decimal string, e.g. \"76300.58\""})
	case amt.IsZero():
		fields = append(fields, apperr.FieldError{Field: "amount", Code: "OUT_OF_RANGE", Message: "must not be zero"})
	case !amt.Equal(amt.Round(2)):
		fields = append(fields, apperr.FieldError{Field: "amount", Code: "FORMAT", Message: "at most 2 decimal places"})
	case amt.Abs().GreaterThan(maxAdjustment):
		fields = append(fields, apperr.FieldError{Field: "amount", Code: "OUT_OF_RANGE", Message: "absolute value at most 1000000000"})
	}
	if r := strings.TrimSpace(in.Reason); r == "" || utf8.RuneCountInString(r) > 500 {
		fields = append(fields, apperr.FieldError{Field: "reason", Code: "LENGTH", Message: "1–500 characters"})
	}
	oy, om, oerr := fieldPeriod("origin_period", in.OriginPeriod)
	ty, tm, terr := fieldPeriod("target_period", in.TargetPeriod)
	for _, e := range []error{oerr, terr} {
		var ae *apperr.Error
		if errors.As(e, &ae) {
			fields = append(fields, ae.Fields...)
		}
	}
	if oerr == nil && terr == nil && ty*12+tm <= oy*12+om {
		fields = append(fields, apperr.FieldError{Field: "target_period", Code: "OUT_OF_RANGE", Message: "must be after origin_period"})
	}
	if len(fields) > 0 {
		return amt, ym, apperr.Validation(fields...)
	}
	return amt, [4]int{oy, om, ty, tm}, nil
}

// CreateAdjustmentTx (J1) di transaksi idempotency. Urutan: validasi → employee ada (404) → periode
// asal FINALIZED → PeriodGuard periode target ([1b] → [2] FOR SHARE, FINALIZED → PERIOD_LOCKED) →
// employee aktif di bulan target → INSERT → catat perubahan input (stale) → audit.
func (s *Service) CreateAdjustmentTx(ctx context.Context, tx pgx.Tx, actor uuid.UUID, in AdjustmentInput, meta reqmeta.Meta) (Adjustment, error) {
	amt, ym, err := validateAdjustment(in)
	if err != nil {
		return Adjustment{}, err
	}
	tid, _ := tenantctx.From(ctx)
	q := store.New(tx)
	if ok, err := q.EmployeeExists(ctx, in.EmployeeID); err != nil {
		return Adjustment{}, db.MapError(err)
	} else if !ok {
		return Adjustment{}, apperr.ErrNotFound.WithDetail("Employee not found")
	}
	origin, err := q.PeriodByMonth(ctx, store.PeriodByMonthParams{Year: int16(ym[0]), Month: int16(ym[1])}) //nolint:gosec // G115
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Adjustment{}, db.MapError(err)
	}
	if err != nil || origin.Status != "FINALIZED" {
		return Adjustment{}, apperr.Validation(apperr.FieldError{Field: "origin_period", Code: "NOT_FINALIZED",
			Message: "adjustments correct a finalized period"})
	}
	from, to := monthRange(ym[2], ym[3])
	record, err := PeriodGuard{}.Lock(ctx, tx, periodguard.Change{Employees: []uuid.UUID{in.EmployeeID}, Range: periodguard.Range{From: from, To: &to}})
	if err != nil {
		return Adjustment{}, err
	}
	if ok, err := q.EmployeeActiveInMonth(ctx, store.EmployeeActiveInMonthParams{EmployeeID: in.EmployeeID, FromDate: from, ToDate: to}); err != nil {
		return Adjustment{}, db.MapError(err)
	} else if !ok {
		return Adjustment{}, apperr.ErrEmployeeNotActive.WithDetail("The employee is not employed in the target period")
	}
	taxable := true
	if in.Taxable != nil {
		taxable = *in.Taxable
	}
	id := s.ids.New()
	if err := q.InsertAdjustment(ctx, store.InsertAdjustmentParams{TenantID: tid, ID: id, EmployeeID: in.EmployeeID, OriginPeriodID: origin.ID,
		TargetYear: int16(ym[2]), TargetMonth: int16(ym[3]), ComponentCode: in.ComponentCode, Amount: toNum(amt), //nolint:gosec // G115
		Taxable: taxable, Reason: strings.TrimSpace(in.Reason), CreatedBy: &actor}); err != nil {
		return Adjustment{}, db.MapError(err)
	}
	if err := record(ctx); err != nil {
		return Adjustment{}, err
	}
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "adjustment.created",
		TargetType: "adjustment", TargetID: id, Diff: map[string]any{"employee_id": in.EmployeeID, "origin_period": in.OriginPeriod,
			"target_period": in.TargetPeriod, "component_code": in.ComponentCode, "amount": amt.StringFixed(2), "taxable": taxable}}, meta); err != nil {
		return Adjustment{}, err
	}
	return s.loadAdjustment(ctx, q, id)
}

func (s *Service) loadAdjustment(ctx context.Context, q *store.Queries, id uuid.UUID) (Adjustment, error) {
	r, err := q.GetAdjustment(ctx, id)
	if err != nil {
		return Adjustment{}, db.MapError(err)
	}
	if err := db.GuardTenant(ctx, "adjustments", r.TenantID); err != nil {
		return Adjustment{}, err
	}
	return toAdjustment(r), nil
}

// CancelAdjustment (J3): hanya PENDING_APPLY (selain itu 409 INVALID_STATE_TRANSITION; trigger PI-15
// sebagai lapis kedua). Target belum final → PeriodGuard + catat perubahan (run CALCULATED jadi stale).
// Target sudah final (adjustment tertinggal karena item ERROR/EXCLUDED) → batal tanpa pencatatan.
func (s *Service) CancelAdjustment(ctx context.Context, actor, id uuid.UUID, in CancelAdjustmentInput, meta reqmeta.Meta) (Adjustment, error) {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > 500 {
		return Adjustment{}, apperr.Validation(apperr.FieldError{Field: "reason", Code: "LENGTH", Message: "1–500 characters"})
	}
	tid, _ := tenantctx.From(ctx)
	var out Adjustment
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		head, err := q.LockAdjustment(ctx, id)
		if err != nil {
			return db.MapError(err)
		}
		if head.Status != "PENDING_APPLY" {
			return apperr.ErrInvalidState.WithDetail("Only a PENDING_APPLY adjustment can be cancelled (status " + head.Status + ")")
		}
		record := periodguard.Recorder(func(context.Context) error { return nil })
		target, err := q.PeriodByMonth(ctx, store.PeriodByMonthParams{Year: head.TargetYear, Month: head.TargetMonth})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return db.MapError(err)
		}
		if err != nil || target.Status != "FINALIZED" {
			from, to := monthRange(int(head.TargetYear), int(head.TargetMonth))
			if record, err = (PeriodGuard{}).Lock(ctx, tx, periodguard.Change{Employees: []uuid.UUID{head.EmployeeID},
				Range: periodguard.Range{From: from, To: &to}}); err != nil {
				return err
			}
		}
		now := s.clock.Now()
		if n, err := q.CancelAdjustment(ctx, store.CancelAdjustmentParams{Actor: &actor, At: &now, Reason: &reason, ID: id}); err != nil {
			return db.MapError(err)
		} else if n != 1 {
			return apperr.ErrInvalidState
		}
		if err := record(ctx); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "adjustment.cancelled",
			TargetType: "adjustment", TargetID: id, Diff: map[string]any{"reason": reason}}, meta); err != nil {
			return err
		}
		out, err = s.loadAdjustment(ctx, q, id)
		return err
	})
	return out, err
}

// ListAdjustments (J2): cursor (created_at DESC, id DESC).
func (s *Service) ListAdjustments(ctx context.Context, employeeID, targetPeriod, status, cursor string, limit int32) ([]Adjustment, httpx.Page, error) {
	page := httpx.Page{}
	params := store.ListAdjustmentsParams{Lim: limit + 1}
	if employeeID != "" {
		id, err := uuid.Parse(employeeID)
		if err != nil {
			return nil, page, apperr.Validation(apperr.FieldError{Field: "employee_id", Code: "FORMAT"})
		}
		params.EmployeeID = &id
	}
	if targetPeriod != "" {
		y, m, err := fieldPeriod("target_period", targetPeriod)
		if err != nil {
			return nil, page, err
		}
		yy, mm := int16(y), int16(m) //nolint:gosec // G115
		params.TargetYear, params.TargetMonth = &yy, &mm
	}
	if status != "" {
		if status != "PENDING_APPLY" && status != "APPLIED" && status != "CANCELLED" {
			return nil, page, apperr.Validation(apperr.FieldError{Field: "status", Code: "INVALID_ENUM"})
		}
		params.Status = &status
	}
	if cursor != "" {
		c, err := httpx.DecodeCursor(cursor)
		if err != nil {
			return nil, page, err
		}
		params.CursorAt, params.CursorID = &c.At, &c.ID
	}
	out := []Adjustment{}
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := store.New(tx).ListAdjustments(ctx, params)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := db.GuardTenant(ctx, "adjustments", r.TenantID); err != nil {
				return err
			}
			out = append(out, toAdjustment(store.GetAdjustmentRow(r)))
		}
		return nil
	})
	if err != nil {
		return nil, page, db.MapError(err)
	}
	if int32(len(out)) > limit { //nolint:gosec // G115
		out = out[:limit]
		last := out[len(out)-1]
		c := httpx.EncodeCursor(httpx.Cursor{At: last.CreatedAt, ID: last.ID})
		page = httpx.Page{NextCursor: &c, HasMore: true}
	}
	return out, page, nil
}
