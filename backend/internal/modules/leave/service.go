// Package leave: jenis cuti, saldo, dan request cuti (PF-09, L1–L5) + efek approval LEAVE_REQUEST
// (ADR-P09). Saldo berkurang DI TRANSAKSI APPROVE (PI-10): saldo FOR UPDATE, CHECK ck_leave_balances_min
// sebagai jaring pengaman; saldo tidak cukup → 422 dan approval tetap PENDING.
//
// Keputusan S6 (catatan implementasi doc 16): saldo dibuat lazy saat approve pertama dengan kuota tahunan
// penuh (akrual = P2); jenis tanpa kuota (SICK, UNPAID) tanpa saldo; rentang cuti tidak boleh melewati
// pergantian tahun; pembatalan cuti APPROVED (saldo kembali) = P1.
package leave

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"payrun/internal/modules/leave/store"
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

const SubjectType = "LEAVE_REQUEST"

// EmployeeSelf: employee milik user (dipetakan composition root dari employee.Self).
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
	WorkingDaysTx(ctx context.Context, tx pgx.Tx, from, to time.Time) (int, error)
}

// ApprovalBrief: approval terbaru sebuah request (dipetakan dari modul approval).
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

// Members: role aktif user (approver dicek ulang di DB saat memutuskan).
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

// ---------------------------------------------------------------- decimal ↔ numeric

func toDec(n pgtype.Numeric) *decimal.Decimal {
	if !n.Valid || n.Int == nil {
		return nil
	}
	v := decimal.NewFromBigInt(n.Int, n.Exp)
	return &v
}

func toNum(d decimal.Decimal) pgtype.Numeric {
	return pgtype.Numeric{Int: d.Coefficient(), Exp: d.Exponent(), Valid: true}
}

func fixed1(n pgtype.Numeric) *string {
	d := toDec(n)
	if d == nil {
		return nil
	}
	v := d.StringFixed(1)
	return &v
}

// ---------------------------------------------------------------- L1 jenis cuti

type LeaveType struct {
	ID              uuid.UUID `json:"id"`
	Code            string    `json:"code"`
	Name            string    `json:"name"`
	IsPaid          bool      `json:"is_paid"`
	AnnualQuotaDays *string   `json:"annual_quota_days"`
	MaxNegativeDays string    `json:"max_negative_days"`
	Status          string    `json:"status"`
}

func (s *Service) ListTypes(ctx context.Context) ([]LeaveType, error) {
	out := []LeaveType{}
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		rs, err := store.New(tx).ListLeaveTypes(ctx)
		if err != nil {
			return err
		}
		for _, r := range rs {
			if err := db.GuardTenant(ctx, "leave_types", r.TenantID); err != nil {
				return err
			}
			out = append(out, LeaveType{ID: r.ID, Code: r.Code, Name: r.Name, IsPaid: r.IsPaid, AnnualQuotaDays: fixed1(r.AnnualQuotaDays),
				MaxNegativeDays: *fixed1(r.MaxNegativeDays), Status: r.Status})
		}
		return nil
	})
	return out, db.MapError(err)
}

// ---------------------------------------------------------------- L2 saldo saya

type Balance struct {
	LeaveType   LeaveTypeRef `json:"leave_type"`
	Year        int          `json:"year"`
	BalanceDays *string      `json:"balance_days"` // null: jenis tanpa kuota
	PendingDays string       `json:"pending_days"` // informasi, tidak mengurangi saldo
}

type LeaveTypeRef struct {
	ID     uuid.UUID `json:"id"`
	Code   string    `json:"code"`
	Name   string    `json:"name"`
	IsPaid bool      `json:"is_paid"`
}

func (s *Service) MyBalances(ctx context.Context, p auth.Principal, year int) ([]Balance, error) {
	if year == 0 {
		year = s.clock.Now().Year()
	}
	if year < 2000 || year > 2100 {
		return nil, apperr.Validation(apperr.FieldError{Field: "year", Code: "OUT_OF_RANGE", Message: "must be 2000..2100"})
	}
	out := []Balance{}
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		me, ok, err := s.employees.SelfTx(ctx, tx, p.UserID, time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC))
		if err != nil {
			return err
		}
		if !ok {
			return apperr.ErrEmployeeNotActive
		}
		rs, err := store.New(tx).MyLeaveBalances(ctx, store.MyLeaveBalancesParams{EmployeeID: me.EmployeeID, Year: int32(year)}) //nolint:gosec // G115: 2000..2100
		if err != nil {
			return err
		}
		for _, r := range rs {
			b := Balance{LeaveType: LeaveTypeRef{ID: r.ID, Code: r.Code, Name: r.Name, IsPaid: r.IsPaid}, Year: year, PendingDays: *fixed1(r.PendingDays)}
			switch {
			case r.BalanceDays.Valid:
				b.BalanceDays = fixed1(r.BalanceDays)
			case r.AnnualQuotaDays.Valid: // belum pernah dipakai: kuota penuh
				b.BalanceDays = fixed1(r.AnnualQuotaDays)
			}
			out = append(out, b)
		}
		return nil
	})
	return out, db.MapError(err)
}

// ---------------------------------------------------------------- model request

type Request struct {
	ID         uuid.UUID      `json:"id"`
	EmployeeID uuid.UUID      `json:"employee_id"`
	LeaveType  LeaveTypeRef   `json:"leave_type"`
	StartDate  string         `json:"start_date"`
	EndDate    string         `json:"end_date"`
	Days       string         `json:"days"`
	Reason     *string        `json:"reason"`
	Status     string         `json:"status"`
	ApprovedAt *time.Time     `json:"approved_at"`
	Approval   *ApprovalBrief `json:"approval"`
}

func (s *Service) load(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Request, store.GetLeaveRequestRow, error) {
	r, err := store.New(tx).GetLeaveRequest(ctx, id)
	if err != nil {
		return Request{}, r, err
	}
	if err := db.GuardTenant(ctx, "leave_requests", r.TenantID); err != nil {
		return Request{}, r, err
	}
	out := Request{ID: r.ID, EmployeeID: r.EmployeeID, LeaveType: LeaveTypeRef{ID: r.LeaveTypeID, Code: r.TypeCode, Name: r.TypeName, IsPaid: r.IsPaid},
		StartDate: r.StartDate.Format(time.DateOnly), EndDate: r.EndDate.Format(time.DateOnly), Days: *fixed1(r.Days), Reason: r.Reason,
		Status: r.Status, ApprovedAt: r.ApprovedAt}
	briefs, err := s.approvals.BriefsTx(ctx, tx, SubjectType, []uuid.UUID{id})
	if err != nil {
		return Request{}, r, err
	}
	if b, ok := briefs[id]; ok {
		out.Approval = &b
	}
	return out, r, nil
}

// ---------------------------------------------------------------- L3 submit

type SubmitInput struct {
	LeaveTypeID uuid.UUID `json:"leave_type_id"`
	StartDate   string    `json:"start_date"`
	EndDate     string    `json:"end_date"` // inklusif
	Reason      *string   `json:"reason"`
}

func parseDate(field, v string) (time.Time, error) {
	d, err := time.Parse(time.DateOnly, v)
	if err != nil || d.Year() < 2000 || d.Year() > 2100 {
		return time.Time{}, apperr.Validation(apperr.FieldError{Field: field, Code: "FORMAT", Message: "must be YYYY-MM-DD"})
	}
	return d, nil
}

// SubmitTx (L3) di transaksi idempotency: request PENDING + approval (approver = atasan → HR → OWNER).
// Saldo TIDAK dipotong di sini. PeriodGuard tanpa change log: input payroll baru berubah saat approve.
func (s *Service) SubmitTx(ctx context.Context, tx pgx.Tx, p auth.Principal, in SubmitInput, meta reqmeta.Meta) (Request, error) {
	tid, _ := tenantctx.From(ctx)
	start, err := parseDate("start_date", in.StartDate)
	if err != nil {
		return Request{}, err
	}
	end, err := parseDate("end_date", in.EndDate)
	if err != nil {
		return Request{}, err
	}
	var fe []apperr.FieldError
	switch {
	case end.Before(start):
		fe = append(fe, apperr.FieldError{Field: "end_date", Code: "OUT_OF_RANGE", Message: "must not be before start_date"})
	case end.Sub(start) >= 60*24*time.Hour:
		fe = append(fe, apperr.FieldError{Field: "end_date", Code: "OUT_OF_RANGE", Message: "at most 60 days"})
	case end.Year() != start.Year():
		fe = append(fe, apperr.FieldError{Field: "end_date", Code: "CROSSES_YEAR", Message: "split the leave at the year boundary (balances are per year)"})
	}
	var reason *string
	if in.Reason != nil {
		v := strings.TrimSpace(*in.Reason)
		if utf8.RuneCountInString(v) > 500 || strings.ContainsRune(v, 0) {
			fe = append(fe, apperr.FieldError{Field: "reason", Code: "LENGTH", Message: "at most 500 characters"})
		}
		if v != "" {
			reason = &v
		}
	}
	if len(fe) > 0 {
		return Request{}, apperr.Validation(fe...)
	}
	q := store.New(tx)
	lt, err := q.GetLeaveType(ctx, in.LeaveTypeID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && lt.Status != "ACTIVE") {
		return Request{}, apperr.Validation(apperr.FieldError{Field: "leave_type_id", Code: "NOT_FOUND", Message: "unknown or inactive leave type"})
	}
	if err != nil {
		return Request{}, err
	}
	me, ok, err := s.employees.SelfTx(ctx, tx, p.UserID, start)
	if err != nil {
		return Request{}, err
	}
	if !ok || !me.Active {
		return Request{}, apperr.ErrEmployeeNotActive
	}
	endExcl := end.AddDate(0, 0, 1)
	days, err := s.calendar.WorkingDaysTx(ctx, tx, start, endExcl)
	if err != nil {
		return Request{}, err
	}
	if days == 0 {
		return Request{}, apperr.Validation(apperr.FieldError{Field: "start_date", Code: "NO_WORKING_DAYS", Message: "the range has no working days"})
	}
	if _, err := s.guard.Lock(ctx, tx, periodguard.Change{Employees: []uuid.UUID{me.EmployeeID}, Range: periodguard.Range{From: start, To: &endExcl}}); err != nil {
		return Request{}, err
	}
	id := s.ids.New()
	if err := q.InsertLeaveRequest(ctx, store.InsertLeaveRequestParams{TenantID: tid, ID: id, EmployeeID: me.EmployeeID, LeaveTypeID: lt.ID,
		StartDate: start, EndExclusive: endExcl, Days: toNum(decimal.NewFromInt(int64(days))), Reason: reason, Actor: &p.UserID}); err != nil {
		return Request{}, db.MapError(err)
	}
	if _, err := s.approvals.CreateForEmployeeTx(ctx, tx, SubjectType, id, p.UserID, me.ManagerUserID, meta); err != nil {
		return Request{}, err
	}
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: "leave.requested",
		TargetType: "leave_request", TargetID: id, Diff: map[string]any{"leave_type": lt.Code, "start_date": in.StartDate, "end_date": in.EndDate, "days": days}}, meta); err != nil {
		return Request{}, err
	}
	out, _, err := s.load(ctx, tx, id)
	return out, db.MapError(err)
}

// ---------------------------------------------------------------- L4 cancel

// Cancel (L4): hanya pengaju, hanya selama PENDING; approval ikut CANCELLED.
func (s *Service) Cancel(ctx context.Context, p auth.Principal, id uuid.UUID, meta reqmeta.Meta) (Request, error) {
	tid, _ := tenantctx.From(ctx)
	var out Request
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		_, r, err := s.load(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.CreatedBy == nil || *r.CreatedBy != p.UserID {
			return apperr.ErrNotFound // bukan miliknya: tidak dibedakan dari "tidak ada" (file 17 L4)
		}
		if r.Status != "PENDING" {
			return apperr.ErrInvalidState.WithDetail("Only a PENDING leave can be cancelled")
		}
		if ok, err := s.approvals.CancelPendingTx(ctx, tx, SubjectType, id, p.UserID); err != nil {
			return err
		} else if !ok {
			return apperr.ErrInvalidState.WithDetail("The leave was decided in the meantime")
		}
		if n, err := store.New(tx).CloseLeaveRequest(ctx, store.CloseLeaveRequestParams{Status: "CANCELLED", Actor: &p.UserID, ID: id}); err != nil {
			return err
		} else if n != 1 {
			return apperr.ErrInvalidState
		}
		if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: "leave.cancelled",
			TargetType: "leave_request", TargetID: id}, meta); err != nil {
			return err
		}
		out, _, err = s.load(ctx, tx, id)
		return err
	})
	return out, db.MapError(err)
}

// ---------------------------------------------------------------- L5 list

type ListQuery struct {
	Status, EmployeeID, From, To, Cursor string
	Limit                                int32
}

// scope (file 17 L5): OWNER/HR_ADMIN seluruh tenant; MANAGER diri + bawahan; lainnya (termasuk
// PAYROLL_ADMIN) diri sendiri. nil = seluruh tenant.
func (s *Service) scope(ctx context.Context, tx pgx.Tx, p auth.Principal) ([]uuid.UUID, error) {
	if p.HasAnyRole(auth.RoleOwner, auth.RoleHRAdmin) {
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
	params := store.ListLeaveRequestsParams{Lim: lq.Limit + 1}
	if lq.Status != "" {
		if !statuses[lq.Status] {
			return nil, page, apperr.Validation(apperr.FieldError{Field: "status", Code: "INVALID_ENUM"})
		}
		params.Status = &lq.Status
	}
	if lq.From != "" || lq.To != "" {
		from, err := parseDate("from", lq.From)
		if err != nil {
			return nil, page, err
		}
		to, err := parseDate("to", lq.To)
		if err != nil {
			return nil, page, err
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
		rs, err := store.New(tx).ListLeaveRequests(ctx, params)
		if err != nil {
			return err
		}
		ids := make([]uuid.UUID, 0, len(rs))
		for _, r := range rs {
			if err := db.GuardTenant(ctx, "leave_requests", r.TenantID); err != nil {
				return err
			}
			out = append(out, Request{ID: r.ID, EmployeeID: r.EmployeeID, LeaveType: LeaveTypeRef{ID: r.LeaveTypeID, Code: r.TypeCode, Name: r.TypeName, IsPaid: r.IsPaid},
				StartDate: r.StartDate.Format(time.DateOnly), EndDate: r.EndDate.Format(time.DateOnly), Days: *fixed1(r.Days), Reason: r.Reason,
				Status: r.Status, ApprovedAt: r.ApprovedAt})
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

// ---------------------------------------------------------------- subjek approval LEAVE_REQUEST

// ApproveSubjectTx: [1b][2] PeriodGuard (FOR SHARE) → approver aktif (DB) → decide [4] → saldo FOR UPDATE
// [5] → request APPROVED [6] → change log + audit [7] (`22-tx-concurrency.md` §3.1).
func (s *Service) ApproveSubjectTx(ctx context.Context, tx pgx.Tx, id, approvalID, actor uuid.UUID, decide func(context.Context) error, meta reqmeta.Meta) (any, error) {
	tid, _ := tenantctx.From(ctx)
	now := s.clock.Now()
	q := store.New(tx)
	r, err := q.GetLeaveRequest(ctx, id) // tanpa lock: hanya untuk tahu rentang dan karyawan
	if err != nil {
		return nil, err
	}
	if err := db.GuardTenant(ctx, "leave_requests", r.TenantID); err != nil {
		return nil, err
	}
	endExcl := r.EndDate.AddDate(0, 0, 1)
	record, err := s.guard.Lock(ctx, tx, periodguard.Change{Employees: []uuid.UUID{r.EmployeeID}, Range: periodguard.Range{From: r.StartDate, To: &endExcl}})
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
	if quota := toDec(r.AnnualQuotaDays); quota != nil {
		year := int16(r.StartDate.Year()) //nolint:gosec // G115: 2000..2100
		minAllowed := toDec(r.MaxNegativeDays).Neg()
		if err := q.EnsureLeaveBalance(ctx, store.EnsureLeaveBalanceParams{TenantID: tid, ID: s.ids.New(), EmployeeID: r.EmployeeID, LeaveTypeID: r.LeaveTypeID,
			Year: year, BalanceDays: toNum(*quota), MinAllowedDays: toNum(minAllowed)}); err != nil {
			return nil, err
		}
		bal, err := q.LockLeaveBalance(ctx, store.LockLeaveBalanceParams{EmployeeID: r.EmployeeID, LeaveTypeID: r.LeaveTypeID, Year: year})
		if err != nil {
			return nil, err
		}
		have, days, floor := toDec(bal.BalanceDays), toDec(r.Days), toDec(bal.MinAllowedDays)
		if have.Sub(*days).LessThan(*floor) {
			return nil, apperr.ErrInsufficientBalance.WithBalance(have.StringFixed(1)).
				WithDetail(fmt.Sprintf("Balance %s day(s), the leave needs %s", have.StringFixed(1), days.StringFixed(1)))
		}
		if err := q.DeductLeaveBalance(ctx, store.DeductLeaveBalanceParams{Days: r.Days, ID: bal.ID}); err != nil {
			return nil, err
		}
	}
	if n, err := q.ApproveLeaveRequest(ctx, store.ApproveLeaveRequestParams{At: &now, Actor: &actor, ID: id}); err != nil {
		return nil, err
	} else if n != 1 {
		return nil, apperr.ErrInvalidState.WithDetail("The leave is no longer pending")
	}
	if err := record(ctx); err != nil {
		return nil, err
	}
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "leave.approved",
		TargetType: "leave_request", TargetID: id, Diff: map[string]any{"approval_request_id": approvalID.String(), "days": toDec(r.Days).StringFixed(1)}}, meta); err != nil {
		return nil, err
	}
	out, _, err := s.load(ctx, tx, id)
	return out, err
}

// RejectSubjectTx: decide [4] → request REJECTED [6]. Input payroll tidak berubah (cuti PENDING tidak dihitung).
func (s *Service) RejectSubjectTx(ctx context.Context, tx pgx.Tx, id, approvalID, actor uuid.UUID, note string, decide func(context.Context) error, meta reqmeta.Meta) (any, error) {
	tid, _ := tenantctx.From(ctx)
	if err := decide(ctx); err != nil {
		return nil, err
	}
	if n, err := store.New(tx).CloseLeaveRequest(ctx, store.CloseLeaveRequestParams{Status: "REJECTED", Actor: &actor, ID: id}); err != nil {
		return nil, err
	} else if n != 1 {
		return nil, apperr.ErrInvalidState.WithDetail("The leave is no longer pending")
	}
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "leave.rejected",
		TargetType: "leave_request", TargetID: id, Reason: note, Diff: map[string]any{"approval_request_id": approvalID.String()}}, meta); err != nil {
		return nil, err
	}
	out, _, err := s.load(ctx, tx, id)
	return out, err
}

var monthsID = []string{"Jan", "Feb", "Mar", "Apr", "Mei", "Jun", "Jul", "Agu", "Sep", "Okt", "Nov", "Des"}

func shortDate(t time.Time) string {
	return fmt.Sprintf("%d %s %d", t.Day(), monthsID[t.Month()-1], t.Year())
}

// SubjectSummaries: "Cuti tidak dibayar 25 Mar 2026 · 1 hari · Tono".
func (s *Service) SubjectSummaries(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	rs, err := store.New(tx).LeaveSummaries(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(rs))
	for _, r := range rs {
		when := shortDate(r.StartDate)
		if !r.EndDate.Equal(r.StartDate) {
			when += " – " + shortDate(r.EndDate)
		}
		out[r.ID] = fmt.Sprintf("%s %s · %s hari · %s", r.TypeName, when, toDec(r.Days).String(), r.FullName)
	}
	return out, nil
}
