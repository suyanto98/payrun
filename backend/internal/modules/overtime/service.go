// Package overtime: request lembur (PF-10, V1–V3) + efek approval OVERTIME_REQUEST (ADR-P09). Hanya
// lembur APPROVED yang tanggalnya di periode dan disetujui ≤ cut-off periode masuk payroll (PI-08,
// difilter InputProvider). Melewati batas jam per hari = warning, bukan blokir (PF-10 AC3).
package overtime

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"payrun/internal/modules/overtime/store"
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

const (
	SubjectType   = "OVERTIME_REQUEST"
	maxDuration   = 12 * time.Hour // V1 (DB CHECK ≤ 24 jam sebagai jaring pengaman)
	WarnOverLimit = "OVERTIME_OVER_LIMIT"
)

type EmployeeSelf struct {
	EmployeeID    uuid.UUID
	ManagerUserID *uuid.UUID
	Active        bool
}

type Employees interface {
	SelfTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, on time.Time) (EmployeeSelf, bool, error)
	DirectReportsTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) ([]uuid.UUID, error)
}

type Calendar interface {
	LocationTx(ctx context.Context, tx pgx.Tx) (*time.Location, error)
	OvertimeLimitTx(ctx context.Context, tx pgx.Tx) (string, error)
}

type ApprovalBrief struct {
	ID       uuid.UUID `json:"id"`
	Approver struct {
		ID          uuid.UUID `json:"id"`
		DisplayName *string   `json:"display_name"`
	} `json:"approver"`
	Status string `json:"status"`
}

type Approvals interface {
	CreateForEmployeeTx(ctx context.Context, tx pgx.Tx, subjectType string, subjectID, requester uuid.UUID, managerUser *uuid.UUID, meta reqmeta.Meta) (uuid.UUID, error)
	CancelPendingTx(ctx context.Context, tx pgx.Tx, subjectType string, subjectID, requester uuid.UUID) (bool, error)
	BriefsTx(ctx context.Context, tx pgx.Tx, subjectType string, ids []uuid.UUID) (map[uuid.UUID]ApprovalBrief, error)
}

type Members interface {
	ActiveRolesTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) ([]string, error)
}

type Service struct {
	pool      *pgxpool.Pool
	clock     clock.Clock
	ids       idgen.Generator
	employees Employees
	calendar  Calendar
	guard     periodguard.Guard
	approvals Approvals
	members   Members
}

func NewService(pool *pgxpool.Pool, c clock.Clock, ids idgen.Generator, e Employees, cal Calendar, g periodguard.Guard, a Approvals, m Members) *Service {
	return &Service{pool: pool, clock: c, ids: ids, employees: e, calendar: cal, guard: g, approvals: a, members: m}
}

func toDec(n pgtype.Numeric) decimal.Decimal {
	if !n.Valid || n.Int == nil {
		return decimal.Zero
	}
	return decimal.NewFromBigInt(n.Int, n.Exp)
}

func toNum(d decimal.Decimal) pgtype.Numeric {
	return pgtype.Numeric{Int: d.Coefficient(), Exp: d.Exponent(), Valid: true}
}

// Request: OvertimeRequest (16-api §16).
type Request struct {
	ID         uuid.UUID      `json:"id"`
	EmployeeID uuid.UUID      `json:"employee_id"`
	WorkDate   string         `json:"work_date"`
	StartAt    time.Time      `json:"start_at"`
	EndAt      time.Time      `json:"end_at"`
	Hours      string         `json:"hours"`
	Reason     string         `json:"reason"`
	Status     string         `json:"status"`
	ApprovedAt *time.Time     `json:"approved_at"`
	Approval   *ApprovalBrief `json:"approval"`
	Warnings   []string       `json:"warnings"`
}

func warnings(hours decimal.Decimal, limit string) []string {
	if limit != "" {
		if max, err := decimal.NewFromString(limit); err == nil && hours.GreaterThan(max) {
			return []string{WarnOverLimit}
		}
	}
	return []string{}
}

func (s *Service) load(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Request, store.GetOvertimeRequestRow, error) {
	r, err := store.New(tx).GetOvertimeRequest(ctx, id)
	if err != nil {
		return Request{}, r, err
	}
	if err := db.GuardTenant(ctx, "overtime_requests", r.TenantID); err != nil {
		return Request{}, r, err
	}
	limit, err := s.calendar.OvertimeLimitTx(ctx, tx)
	if err != nil {
		return Request{}, r, err
	}
	out := Request{ID: r.ID, EmployeeID: r.EmployeeID, WorkDate: r.WorkDate.Format(time.DateOnly), StartAt: r.StartAt, EndAt: r.EndAt,
		Hours: toDec(r.Hours).StringFixed(2), Reason: r.Reason, Status: r.Status, ApprovedAt: r.ApprovedAt, Warnings: warnings(toDec(r.Hours), limit)}
	briefs, err := s.approvals.BriefsTx(ctx, tx, SubjectType, []uuid.UUID{id})
	if err != nil {
		return Request{}, r, err
	}
	if b, ok := briefs[id]; ok {
		out.Approval = &b
	}
	return out, r, nil
}

// ---------------------------------------------------------------- V1 submit

type SubmitInput struct {
	WorkDate string    `json:"work_date"`
	StartAt  time.Time `json:"start_at"`
	EndAt    time.Time `json:"end_at"`
	Reason   string    `json:"reason"`
}

// quarterHours: durasi dibulatkan ke 0,25 jam terdekat (aritmetika integer, tanpa float).
func quarterHours(d time.Duration) decimal.Decimal {
	q := (d + 7*time.Minute + 30*time.Second) / (15 * time.Minute)
	return decimal.NewFromInt(int64(q)).Div(decimal.NewFromInt(4))
}

// SubmitTx (V1) di transaksi idempotency: request PENDING + approval (atasan → HR → OWNER).
func (s *Service) SubmitTx(ctx context.Context, tx pgx.Tx, p auth.Principal, in SubmitInput, meta reqmeta.Meta) (Request, error) {
	tid, _ := tenantctx.From(ctx)
	in.Reason = strings.TrimSpace(in.Reason)
	var fe []apperr.FieldError
	workDate, derr := time.Parse(time.DateOnly, in.WorkDate)
	if derr != nil {
		fe = append(fe, apperr.FieldError{Field: "work_date", Code: "FORMAT", Message: "must be YYYY-MM-DD"})
	}
	dur := in.EndAt.Sub(in.StartAt)
	hours := quarterHours(dur)
	switch {
	case in.StartAt.IsZero() || in.EndAt.IsZero():
		fe = append(fe, apperr.FieldError{Field: "start_at", Code: "REQUIRED", Message: "start_at and end_at are required (RFC 3339)"})
	case dur <= 0:
		fe = append(fe, apperr.FieldError{Field: "end_at", Code: "OUT_OF_RANGE", Message: "must be after start_at"})
	case dur > maxDuration:
		fe = append(fe, apperr.FieldError{Field: "end_at", Code: "OUT_OF_RANGE", Message: "at most 12 hours"})
	case !hours.IsPositive():
		fe = append(fe, apperr.FieldError{Field: "end_at", Code: "OUT_OF_RANGE", Message: "at least 15 minutes"})
	}
	if n := utf8.RuneCountInString(in.Reason); n < 1 || n > 500 || strings.ContainsRune(in.Reason, 0) {
		fe = append(fe, apperr.FieldError{Field: "reason", Code: "LENGTH", Message: "must be 1..500 characters"})
	}
	if len(fe) > 0 {
		return Request{}, apperr.Validation(fe...)
	}
	loc, err := s.calendar.LocationTx(ctx, tx)
	if err != nil {
		return Request{}, err
	}
	if l := in.StartAt.In(loc); l.Year() != workDate.Year() || l.YearDay() != workDate.YearDay() {
		return Request{}, apperr.Validation(apperr.FieldError{Field: "work_date", Code: "MISMATCH", Message: "must be the tenant-local date of start_at"})
	}
	me, ok, err := s.employees.SelfTx(ctx, tx, p.UserID, workDate)
	if err != nil {
		return Request{}, err
	}
	if !ok || !me.Active {
		return Request{}, apperr.ErrEmployeeNotActive
	}
	if _, err := s.guard.Lock(ctx, tx, periodguard.Change{Employees: []uuid.UUID{me.EmployeeID}, Range: periodguard.Day(workDate)}); err != nil {
		return Request{}, err
	}
	id := s.ids.New()
	if err := store.New(tx).InsertOvertimeRequest(ctx, store.InsertOvertimeRequestParams{TenantID: tid, ID: id, EmployeeID: me.EmployeeID,
		WorkDate: workDate, StartAt: in.StartAt, EndAt: in.EndAt, Hours: toNum(hours), Reason: in.Reason, Actor: &p.UserID}); err != nil {
		return Request{}, db.MapError(err)
	}
	if _, err := s.approvals.CreateForEmployeeTx(ctx, tx, SubjectType, id, p.UserID, me.ManagerUserID, meta); err != nil {
		return Request{}, err
	}
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: "overtime.requested",
		TargetType: "overtime_request", TargetID: id, Diff: map[string]any{"work_date": in.WorkDate, "hours": hours.StringFixed(2)}}, meta); err != nil {
		return Request{}, err
	}
	out, _, err := s.load(ctx, tx, id)
	return out, db.MapError(err)
}

// ---------------------------------------------------------------- V2 cancel

func (s *Service) Cancel(ctx context.Context, p auth.Principal, id uuid.UUID, meta reqmeta.Meta) (Request, error) {
	tid, _ := tenantctx.From(ctx)
	var out Request
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		_, r, err := s.load(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.CreatedBy == nil || *r.CreatedBy != p.UserID {
			return apperr.ErrNotFound
		}
		if r.Status != "PENDING" {
			return apperr.ErrInvalidState.WithDetail("Only a PENDING overtime request can be cancelled")
		}
		if ok, err := s.approvals.CancelPendingTx(ctx, tx, SubjectType, id, p.UserID); err != nil {
			return err
		} else if !ok {
			return apperr.ErrInvalidState.WithDetail("The request was decided in the meantime")
		}
		if n, err := store.New(tx).CloseOvertimeRequest(ctx, store.CloseOvertimeRequestParams{Status: "CANCELLED", Actor: &p.UserID, ID: id}); err != nil {
			return err
		} else if n != 1 {
			return apperr.ErrInvalidState
		}
		if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: "overtime.cancelled",
			TargetType: "overtime_request", TargetID: id}, meta); err != nil {
			return err
		}
		out, _, err = s.load(ctx, tx, id)
		return err
	})
	return out, db.MapError(err)
}

// ---------------------------------------------------------------- V3 list

type ListQuery struct {
	Status, EmployeeID, From, To, Cursor string
	Limit                                int32
}

// scope (file 17 V3): OWNER/HR_ADMIN/PAYROLL_ADMIN seluruh tenant; MANAGER diri + bawahan; lainnya diri.
func (s *Service) scope(ctx context.Context, tx pgx.Tx, p auth.Principal) ([]uuid.UUID, error) {
	if p.HasAnyRole(auth.RoleOwner, auth.RoleHRAdmin, auth.RolePayroll) {
		return nil, nil
	}
	ids := []uuid.UUID{}
	if me, ok, err := s.employees.SelfTx(ctx, tx, p.UserID, s.clock.Now().UTC()); err != nil {
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

var statuses = map[string]bool{"PENDING": true, "APPROVED": true, "REJECTED": true, "CANCELLED": true}

func (s *Service) List(ctx context.Context, p auth.Principal, lq ListQuery) ([]Request, httpx.Page, error) {
	page := httpx.Page{}
	params := store.ListOvertimeRequestsParams{Lim: lq.Limit + 1}
	if lq.Status != "" {
		if !statuses[lq.Status] {
			return nil, page, apperr.Validation(apperr.FieldError{Field: "status", Code: "INVALID_ENUM"})
		}
		params.Status = &lq.Status
	}
	if lq.From != "" || lq.To != "" {
		from, err1 := time.Parse(time.DateOnly, lq.From)
		to, err2 := time.Parse(time.DateOnly, lq.To)
		if err1 != nil || err2 != nil || to.Before(from) {
			return nil, page, apperr.Validation(apperr.FieldError{Field: "from", Code: "FORMAT", Message: "from and to must be YYYY-MM-DD, from ≤ to"})
		}
		toExcl := to.AddDate(0, 0, 1)
		params.FromDate, params.ToDate = &from, &toExcl
	}
	var employee *uuid.UUID
	if lq.EmployeeID != "" {
		id, err := uuid.Parse(lq.EmployeeID)
		if err != nil {
			return nil, page, apperr.Validation(apperr.FieldError{Field: "employee_id", Code: "FORMAT"})
		}
		employee = &id
	}
	if lq.Cursor != "" {
		c, err := httpx.DecodeCursor(lq.Cursor)
		if err != nil {
			return nil, page, err
		}
		params.AfterAt, params.AfterID = &c.At, &c.ID
	}
	out := []Request{}
	var createdAt []time.Time
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		allowed, err := s.scope(ctx, tx, p)
		if err != nil {
			return err
		}
		switch {
		case employee != nil && allowed != nil && !contains(allowed, *employee):
			return apperr.ErrNotFound
		case employee != nil:
			params.EmployeeIds = []uuid.UUID{*employee}
		case allowed != nil:
			params.EmployeeIds = allowed
		}
		limit, err := s.calendar.OvertimeLimitTx(ctx, tx)
		if err != nil {
			return err
		}
		rs, err := store.New(tx).ListOvertimeRequests(ctx, params)
		if err != nil {
			return err
		}
		ids := make([]uuid.UUID, 0, len(rs))
		for _, r := range rs {
			if err := db.GuardTenant(ctx, "overtime_requests", r.TenantID); err != nil {
				return err
			}
			out = append(out, Request{ID: r.ID, EmployeeID: r.EmployeeID, WorkDate: r.WorkDate.Format(time.DateOnly), StartAt: r.StartAt, EndAt: r.EndAt,
				Hours: toDec(r.Hours).StringFixed(2), Reason: r.Reason, Status: r.Status, ApprovedAt: r.ApprovedAt, Warnings: warnings(toDec(r.Hours), limit)})
			ids = append(ids, r.ID)
			createdAt = append(createdAt, r.CreatedAt)
		}
		briefs, err := s.approvals.BriefsTx(ctx, tx, SubjectType, ids)
		if err != nil {
			return err
		}
		for i := range out {
			if b, ok := briefs[out[i].ID]; ok {
				out[i].Approval = &b
			}
		}
		return nil
	})
	if err != nil {
		return nil, page, db.MapError(err)
	}
	if int32(len(out)) > lq.Limit { //nolint:gosec // G115: panjang ≤ limit+1
		out = out[:lq.Limit]
		c := httpx.EncodeCursor(httpx.Cursor{At: createdAt[lq.Limit-1], ID: out[len(out)-1].ID})
		page = httpx.Page{NextCursor: &c, HasMore: true}
	}
	return out, page, nil
}

func contains(ids []uuid.UUID, id uuid.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- subjek approval OVERTIME_REQUEST

// ApproveSubjectTx: [1b][2] PeriodGuard (work_date) → approver aktif → decide [4] → APPROVED [6] → [7].
func (s *Service) ApproveSubjectTx(ctx context.Context, tx pgx.Tx, id, approvalID, actor uuid.UUID, decide func(context.Context) error, meta reqmeta.Meta) (any, error) {
	tid, _ := tenantctx.From(ctx)
	now := s.clock.Now()
	q := store.New(tx)
	r, err := q.GetOvertimeRequest(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := db.GuardTenant(ctx, "overtime_requests", r.TenantID); err != nil {
		return nil, err
	}
	record, err := s.guard.Lock(ctx, tx, periodguard.Change{Employees: []uuid.UUID{r.EmployeeID}, Range: periodguard.Day(r.WorkDate)})
	if err != nil {
		return nil, err
	}
	if roles, err := s.members.ActiveRolesTx(ctx, tx, actor); err != nil {
		return nil, err
	} else if len(roles) == 0 {
		return nil, apperr.ErrForbidden.WithDetail("The approver is no longer a member of this company")
	}
	if err := decide(ctx); err != nil {
		return nil, err
	}
	if n, err := q.ApproveOvertimeRequest(ctx, store.ApproveOvertimeRequestParams{At: &now, Actor: &actor, ID: id}); err != nil {
		return nil, err
	} else if n != 1 {
		return nil, apperr.ErrInvalidState.WithDetail("The request is no longer pending")
	}
	if err := record(ctx); err != nil {
		return nil, err
	}
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "overtime.approved",
		TargetType: "overtime_request", TargetID: id, Diff: map[string]any{"approval_request_id": approvalID.String(), "hours": toDec(r.Hours).StringFixed(2)}}, meta); err != nil {
		return nil, err
	}
	out, _, err := s.load(ctx, tx, id)
	return out, err
}

func (s *Service) RejectSubjectTx(ctx context.Context, tx pgx.Tx, id, approvalID, actor uuid.UUID, note string, decide func(context.Context) error, meta reqmeta.Meta) (any, error) {
	tid, _ := tenantctx.From(ctx)
	if err := decide(ctx); err != nil {
		return nil, err
	}
	if n, err := store.New(tx).CloseOvertimeRequest(ctx, store.CloseOvertimeRequestParams{Status: "REJECTED", Actor: &actor, ID: id}); err != nil {
		return nil, err
	} else if n != 1 {
		return nil, apperr.ErrInvalidState.WithDetail("The request is no longer pending")
	}
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "overtime.rejected",
		TargetType: "overtime_request", TargetID: id, Reason: note, Diff: map[string]any{"approval_request_id": approvalID.String()}}, meta); err != nil {
		return nil, err
	}
	out, _, err := s.load(ctx, tx, id)
	return out, err
}

var monthsID = []string{"Jan", "Feb", "Mar", "Apr", "Mei", "Jun", "Jul", "Agu", "Sep", "Okt", "Nov", "Des"}

// SubjectSummaries: "Lembur 23 Mar 2026 · 4 jam · Tono".
func (s *Service) SubjectSummaries(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	rs, err := store.New(tx).OvertimeSummaries(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(rs))
	for _, r := range rs {
		out[r.ID] = fmt.Sprintf("Lembur %d %s %d · %s jam · %s", r.WorkDate.Day(), monthsID[r.WorkDate.Month()-1], r.WorkDate.Year(),
			toDec(r.Hours).String(), r.FullName)
	}
	return out, nil
}
