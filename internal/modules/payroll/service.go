// Package payroll: payroll run (PF-13): periode, run lifecycle, kalkulasi asinkron per chunk, review
// item. Engine murni ada di subpackage engine; package ini adalah lapisan I/O di sekitarnya.
package payroll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/internal/modules/payroll/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/audit"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/db"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/jobs"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tenantctx"
)

type Service struct {
	pool      *pgxpool.Pool
	clock     clock.Clock
	ids       idgen.Generator
	members   Memberships
	approvals Approvals
}

func NewService(pool *pgxpool.Pool, c clock.Clock, ids idgen.Generator, members Memberships) *Service {
	return &Service{pool: pool, clock: c, ids: ids, members: members}
}

// Memberships: role dibaca ulang dari DB untuk pembacaan hasil payroll (Y6/Y7, Step 12 R2-6: operasi
// sensitif, role di token bisa basi ≤ 15 menit). Diimplementasikan modul iam (ADR-001).
type Memberships interface {
	ActiveRolesTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) ([]string, error)
	// ActiveAdminsTx: user dengan membership ACTIVE OWNER/PAYROLL_ADMIN (calon approver run, ADR-P09).
	ActiveAdminsTx(ctx context.Context, tx pgx.Tx) ([]uuid.UUID, error)
}

// readerRolesTx: role pemanggil dari DB. Bukan lagi pembaca hasil payroll → 404 (resource run tidak
// terlihat, sama dengan middleware hidden). runner = OWNER/PAYROLL_ADMIN (tanpa audit baca detail).
func (s *Service) readerRolesTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (runner bool, err error) {
	roles, err := s.members.ActiveRolesTx(ctx, tx, userID)
	if err != nil {
		return false, err
	}
	p := auth.Principal{UserID: userID, Roles: roles}
	if !p.HasAnyRole(readers...) {
		return false, apperr.ErrNotFound
	}
	return p.HasAnyRole(runners...), nil
}

// ---------------------------------------------------------------- model API (16-api §16 PayrollRun)

type Progress struct {
	ChunksDone    int32  `json:"chunks_done"`
	ChunksTotal   int32  `json:"chunks_total"`
	EligibleCount *int32 `json:"eligible_count"`
}

type Totals struct {
	Gross      string `json:"gross"`
	Deductions string `json:"deductions"`
	Net        string `json:"net"`
}

type Counts struct {
	OK       *int32 `json:"ok"`
	Error    *int32 `json:"error"`
	Excluded *int32 `json:"excluded"`
	Warnings *int32 `json:"warnings"`
}

type RulesetRef struct {
	Code    string `json:"code"`
	Version string `json:"version"`
}

type Run struct {
	ID                uuid.UUID   `json:"id"`
	ApprovalRequestID *uuid.UUID  `json:"approval_request_id"`
	SubmittedBy       *uuid.UUID  `json:"submitted_by"`
	SubmittedAt       *time.Time  `json:"submitted_at"`
	ApprovedBy        *uuid.UUID  `json:"approved_by"`
	ApprovedAt        *time.Time  `json:"approved_at"`
	Period            string      `json:"period"`
	Status            string      `json:"status"`
	CalcVersion       int32       `json:"calc_version"`
	Progress          Progress    `json:"progress"`
	Totals            *Totals     `json:"totals"`
	Counts            Counts      `json:"counts"`
	Ruleset           *RulesetRef `json:"ruleset"`
	FailureReason     *string     `json:"failure_reason"`
	CreatedBy         *uuid.UUID  `json:"created_by"`
	CreatedAt         time.Time   `json:"created_at"`
	CalcStartedAt     *time.Time  `json:"calc_started_at"`
	CalculatedAt      *time.Time  `json:"calculated_at"`
	CancelReason      *string     `json:"cancel_reason"`
}

func periodText(year, month int16) string { return fmt.Sprintf("%04d-%02d", year, month) }

func toRun(r store.GetRunRow) Run {
	out := Run{ID: r.ID, Period: periodText(r.Year, r.Month), Status: r.Status, CalcVersion: r.CalcVersion,
		Progress:      Progress{ChunksDone: r.ChunksDone, ChunksTotal: r.ChunksTotal, EligibleCount: r.EligibleCount},
		Counts:        Counts{OK: r.ItemOkCount, Error: r.ItemErrorCount, Excluded: r.ItemExcludedCount, Warnings: r.WarningCount},
		FailureReason: r.FailureReason, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, CalcStartedAt: r.CalcStartedAt,
		CalculatedAt: r.CalculatedAt, CancelReason: r.CancelReason, ApprovalRequestID: r.ApprovalRequestID,
		SubmittedBy: r.SubmittedBy, SubmittedAt: r.SubmittedAt, ApprovedBy: r.ApprovedBy, ApprovedAt: r.ApprovedAt}
	if g, d, n := toDec(r.TotalGross), toDec(r.TotalDeductions), toDec(r.TotalNet); g != nil && d != nil && n != nil {
		out.Totals = &Totals{Gross: g.StringFixed(2), Deductions: d.StringFixed(2), Net: n.StringFixed(2)}
	}
	if r.RulesetCode != nil && r.RulesetVersion != nil {
		out.Ruleset = &RulesetRef{Code: *r.RulesetCode, Version: *r.RulesetVersion}
	}
	return out
}

func (s *Service) loadRun(ctx context.Context, q *store.Queries, id uuid.UUID) (Run, error) {
	r, err := q.GetRun(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if err := db.GuardTenant(ctx, "payroll_runs", r.TenantID); err != nil {
		return Run{}, err
	}
	return toRun(r), nil
}

// ---------------------------------------------------------------- Y2 create run

var periodPattern = regexp.MustCompile(`^([0-9]{4})-(0[1-9]|1[0-2])$`)

func parsePeriod(s string) (int, int, error) {
	m := periodPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, apperr.Validation(apperr.FieldError{Field: "period", Code: "FORMAT", Message: "must be YYYY-MM"})
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	if y < 2000 || y > 2100 {
		return 0, 0, apperr.Validation(apperr.FieldError{Field: "period", Code: "OUT_OF_RANGE"})
	}
	return y, mo, nil
}

// cutoffAt: tanggal cutoff_day_of_next_month bulan berikutnya, jam cutoff_time_local, zona tenant →
// UTC. Dibekukan di payroll_periods saat periode dibuat (determinisme, Step 3 `06` §4).
func cutoffAt(year, month int, tz string, day int16, hhmm string) (time.Time, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, err
	}
	h, mi := 23, 59
	if parts := strings.Split(hhmm, ":"); len(parts) == 2 {
		h, _ = strconv.Atoi(parts[0])
		mi, _ = strconv.Atoi(parts[1])
	}
	first := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
	return time.Date(first.Year(), first.Month(), int(day), h, mi, 0, 0, loc).UTC(), nil
}

// CreateRunTx (Y2) di transaksi idempotency. Urutan lock: [1b] tenant_settings FOR UPDATE (hanya bila
// periode baru, D1) → [2] periode INSERT…ON CONFLICT DO NOTHING + FOR UPDATE (R1-8: tanpa TOCTOU) →
// [3] run DRAFT. Dua admin bersamaan menyerial di lock periode; yang kedua melihat run aktif →
// 409 ACTIVE_RUN_EXISTS + existing_resource_id.
func (s *Service) CreateRunTx(ctx context.Context, tx pgx.Tx, actor uuid.UUID, period string, meta reqmeta.Meta) (Run, error) {
	year, month, err := parsePeriod(period)
	if err != nil {
		return Run{}, err
	}
	tid, _ := tenantctx.From(ctx)
	q := store.New(tx)
	set, err := q.LockTenantSettings(ctx, tid)
	if err != nil {
		return Run{}, db.MapError(err)
	}
	loc, err := time.LoadLocation(set.Timezone)
	if err != nil {
		return Run{}, err
	}
	now := s.clock.Now().In(loc)
	limit := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
	if time.Date(year, time.Month(month), 1, 0, 0, 0, 0, loc).After(limit) {
		return Run{}, apperr.Validation(apperr.FieldError{Field: "period", Code: "OUT_OF_RANGE", Message: "at most one month ahead of the current month"})
	}
	cut, err := cutoffAt(year, month, set.Timezone, set.CutoffDayOfNextMonth, set.CutoffTime)
	if err != nil {
		return Run{}, err
	}
	if err := q.InsertPeriod(ctx, store.InsertPeriodParams{TenantID: tid, ID: s.ids.New(), Year: int16(year), Month: int16(month), CutoffAt: cut}); err != nil { //nolint:gosec // G115
		return Run{}, db.MapError(err)
	}
	per, err := q.LockPeriodByMonth(ctx, store.LockPeriodByMonthParams{Year: int16(year), Month: int16(month)}) //nolint:gosec // G115
	if err != nil {
		return Run{}, db.MapError(err)
	}
	if per.Status == "FINALIZED" {
		return Run{}, apperr.ErrPeriodLocked
	}
	if existing, err := q.ActiveRunForPeriod(ctx, per.ID); err == nil {
		return Run{}, apperr.ErrActiveRunExists.WithExistingResource(existing.String())
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Run{}, db.MapError(err)
	}
	id := s.ids.New()
	if err := q.InsertRun(ctx, store.InsertRunParams{TenantID: tid, ID: id, PeriodID: per.ID, Actor: &actor}); err != nil {
		return Run{}, db.MapError(err)
	}
	if err := q.SetPeriodStatus(ctx, store.SetPeriodStatusParams{Status: "PROCESSING", ID: per.ID}); err != nil {
		return Run{}, db.MapError(err)
	}
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "payroll_run.created",
		TargetType: "payroll_run", TargetID: id, Diff: map[string]any{"period": period}}, meta); err != nil {
		return Run{}, err
	}
	r, err := s.loadRun(ctx, q, id)
	return r, db.MapError(err)
}

// ---------------------------------------------------------------- Y5 calculate

// CalculateTx (Y5) di transaksi idempotency: [2] periode FOR UPDATE (menunggu penulis input FOR SHARE,
// O12) → [3] run → CALCULATING, calc_version+1, calc_started_at → job payroll.plan (outbox).
func (s *Service) CalculateTx(ctx context.Context, tx pgx.Tx, actor, runID uuid.UUID, meta reqmeta.Meta) (Run, error) {
	tid, _ := tenantctx.From(ctx)
	q := store.New(tx)
	cur, err := s.loadRun(ctx, q, runID)
	if err != nil {
		return Run{}, db.MapError(err)
	}
	run, err := q.GetRun(ctx, runID)
	if err != nil {
		return Run{}, db.MapError(err)
	}
	if _, err := q.LockPeriod(ctx, run.PeriodID); err != nil {
		return Run{}, db.MapError(err)
	}
	ver, err := q.StartCalculation(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, apperr.ErrInvalidState.WithDetail("Cannot calculate a run in status " + cur.Status)
	}
	if err != nil {
		return Run{}, db.MapError(err)
	}
	if _, err := jobs.Enqueue(ctx, tx, jobs.Spec{Kind: KindPlan, MaxAttempts: 5, CorrelationID: meta.RequestID,
		Dedupe: fmt.Sprintf("plan:%s:%d", runID, ver), Payload: planPayload{RunID: runID, CalcVersion: ver}}); err != nil {
		return Run{}, err
	}
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "payroll_run.calculate_requested",
		TargetType: "payroll_run", TargetID: runID, Diff: map[string]any{"calc_version": ver}}, meta); err != nil {
		return Run{}, err
	}
	r, err := s.loadRun(ctx, q, runID)
	return r, db.MapError(err)
}

// ---------------------------------------------------------------- Y3/Y4 read

func (s *Service) GetRun(ctx context.Context, id uuid.UUID) (Run, error) {
	var out Run
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		out, err = s.loadRun(ctx, store.New(tx), id)
		return err
	})
	return out, db.MapError(err)
}

type RunListItem struct {
	ID          uuid.UUID `json:"id"`
	Period      string    `json:"period"`
	Status      string    `json:"status"`
	CalcVersion int32     `json:"calc_version"`
	TotalNet    *string   `json:"total_net"`
	CreatedAt   time.Time `json:"created_at"`
}

func (s *Service) ListRuns(ctx context.Context, period, status, cursor string, limit int32) ([]RunListItem, httpx.Page, error) {
	page := httpx.Page{}
	params := store.ListRunsParams{Lim: limit + 1}
	if period != "" {
		y, m, err := parsePeriod(period)
		if err != nil {
			return nil, page, err
		}
		yy, mm := int16(y), int16(m) //nolint:gosec // G115
		params.Year, params.Month = &yy, &mm
	}
	if status != "" {
		params.Status = &status
	}
	if cursor != "" {
		c, err := httpx.DecodeCursor(cursor)
		if err != nil {
			return nil, page, err
		}
		params.AfterAt, params.AfterID = &c.At, &c.ID
	}
	out := []RunListItem{}
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		rs, err := store.New(tx).ListRuns(ctx, params)
		if err != nil {
			return err
		}
		for _, r := range rs {
			if err := db.GuardTenant(ctx, "payroll_runs", r.TenantID); err != nil {
				return err
			}
			it := RunListItem{ID: r.ID, Period: periodText(r.Year, r.Month), Status: r.Status, CalcVersion: r.CalcVersion, CreatedAt: r.CreatedAt}
			if n := toDec(r.TotalNet); n != nil {
				v := n.StringFixed(2)
				it.TotalNet = &v
			}
			out = append(out, it)
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

// ---------------------------------------------------------------- Y6/Y7 items

type Item struct {
	ID             uuid.UUID  `json:"id"`
	EmployeeID     uuid.UUID  `json:"employee_id"`
	EmployeeNo     string     `json:"employee_no"`
	EmployeeName   string     `json:"employee_name"`
	DepartmentID   *uuid.UUID `json:"department_id"`
	Status         string     `json:"status"`
	Gross          *string    `json:"gross"`
	Deductions     *string    `json:"deductions"`
	Net            *string    `json:"net"`
	ErrorCodes     []string   `json:"error_codes"`
	WarningCodes   []string   `json:"warning_codes"`
	ExcludedReason *string    `json:"excluded_reason"`
}

type Line struct {
	SortOrder   int16           `json:"sort_order"`
	Code        string          `json:"code"`
	Name        string          `json:"name"`
	Category    string          `json:"category"`
	Stage       string          `json:"stage"`
	Amount      string          `json:"amount"`
	Basis       json.RawMessage `json:"basis"`
	Explanation string          `json:"explanation"`
	Taxable     bool            `json:"taxable"`
	Rounding    string          `json:"rounding"`
}

type ItemDetail struct {
	Item
	CalcVersion int32           `json:"calc_version"`
	Lines       []Line          `json:"lines"`
	Issues      json.RawMessage `json:"issues"`
}

type ItemQuery struct {
	Status, DepartmentID, Cursor string
	HasWarning                   bool
	Limit                        int32
}

// ListItems (Y6): item versi terkini. SETIAP request menulis audit payroll.bulk_read (Step 12 R2-6).
func (s *Service) ListItems(ctx context.Context, actor, runID uuid.UUID, iq ItemQuery, meta reqmeta.Meta) ([]Item, httpx.Page, error) {
	page := httpx.Page{}
	tid, _ := tenantctx.From(ctx)
	params := store.ListItemsParams{RunID: runID, HasWarning: iq.HasWarning, Lim: iq.Limit + 1}
	if iq.Status != "" {
		if iq.Status != "OK" && iq.Status != "ERROR" && iq.Status != "EXCLUDED" {
			return nil, page, apperr.Validation(apperr.FieldError{Field: "status", Code: "INVALID_ENUM"})
		}
		params.Status = &iq.Status
	}
	if iq.DepartmentID != "" {
		id, err := uuid.Parse(iq.DepartmentID)
		if err != nil {
			return nil, page, apperr.Validation(apperr.FieldError{Field: "department_id", Code: "FORMAT"})
		}
		params.DepartmentID = &id
	}
	if iq.Cursor != "" {
		c, err := httpx.DecodeKeyCursor(iq.Cursor)
		if err != nil {
			return nil, page, err
		}
		params.AfterKey, params.AfterID = &c.Key, &c.ID
	}
	out := []Item{}
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := s.readerRolesTx(ctx, tx, actor); err != nil {
			return err
		}
		q := store.New(tx)
		run, err := s.loadRun(ctx, q, runID)
		if err != nil {
			return err
		}
		params.CalcVersion = run.CalcVersion
		rs, err := q.ListItems(ctx, params)
		if err != nil {
			return err
		}
		for _, r := range rs {
			if err := db.GuardTenant(ctx, "payroll_items", r.TenantID); err != nil {
				return err
			}
			out = append(out, Item{ID: r.ID, EmployeeID: r.EmployeeID, EmployeeNo: r.EmployeeNo, EmployeeName: r.EmployeeName,
				DepartmentID: r.DepartmentID, Status: r.Status, Gross: fixed(r.Gross), Deductions: fixed(r.Deductions), Net: fixed(r.Net),
				ErrorCodes: r.ErrorCodes, WarningCodes: r.WarningCodes, ExcludedReason: r.ExcludedReason})
		}
		return audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "payroll.bulk_read",
			TargetType: "payroll_run", TargetID: runID, Diff: map[string]any{"rows": min(len(out), int(iq.Limit))}}, meta)
	})
	if err != nil {
		return nil, page, db.MapError(err)
	}
	if int32(len(out)) > iq.Limit { //nolint:gosec // G115
		out = out[:iq.Limit]
		last := out[len(out)-1]
		c := httpx.EncodeKeyCursor(httpx.KeyCursor{Key: last.EmployeeNo, ID: last.ID})
		page = httpx.Page{NextCursor: &c, HasMore: true}
	}
	return out, page, nil
}

// GetItem (Y7): item versi terkini + baris + issues; item versi lama → 404. Dibaca HR_ADMIN tanpa role
// runner (menurut DB) → audit (file 17 ᴬ: HR boleh membaca hasil payroll, setiap akses detail tercatat).
func (s *Service) GetItem(ctx context.Context, actor uuid.UUID, runID, itemID uuid.UUID, meta reqmeta.Meta) (ItemDetail, error) {
	tid, _ := tenantctx.From(ctx)
	var out ItemDetail
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		runner, err := s.readerRolesTx(ctx, tx, actor)
		if err != nil {
			return err
		}
		auditRead := !runner
		q := store.New(tx)
		r, err := q.GetItem(ctx, store.GetItemParams{ID: itemID, RunID: runID})
		if err != nil {
			return err
		}
		if err := db.GuardTenant(ctx, "payroll_items", r.TenantID); err != nil {
			return err
		}
		out = ItemDetail{Item: Item{ID: r.ID, EmployeeID: r.EmployeeID, EmployeeNo: r.EmployeeNo, EmployeeName: r.EmployeeName,
			DepartmentID: r.DepartmentID, Status: r.Status, Gross: fixed(r.Gross), Deductions: fixed(r.Deductions), Net: fixed(r.Net),
			ErrorCodes: r.ErrorCodes, WarningCodes: r.WarningCodes, ExcludedReason: r.ExcludedReason},
			CalcVersion: r.CalcVersion, Lines: []Line{}, Issues: r.Issues}
		lines, err := q.ListLines(ctx, itemID)
		if err != nil {
			return err
		}
		for _, l := range lines {
			out.Lines = append(out.Lines, Line{SortOrder: l.SortOrder, Code: l.Code, Name: l.Name, Category: l.Category, Stage: l.Stage,
				Amount: *fixed(l.Amount), Basis: l.Basis, Explanation: l.Explanation, Taxable: l.Taxable, Rounding: l.Rounding})
		}
		if !auditRead {
			return nil
		}
		return audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "payroll_item.read",
			TargetType: "payroll_item", TargetID: itemID}, meta)
	})
	return out, db.MapError(err)
}

// ---------------------------------------------------------------- Y8 exclude, Y12 cancel

func validReason(reason string, minLen int) (string, error) {
	reason = strings.TrimSpace(reason)
	n := utf8.RuneCountInString(reason)
	if n < minLen || n > 500 || strings.ContainsRune(reason, 0) {
		return "", apperr.Validation(apperr.FieldError{Field: "reason", Code: "LENGTH", Message: fmt.Sprintf("must be %d..500 characters", minLen)})
	}
	return reason, nil
}

// Exclude (Y8): hanya item ERROR pada run CALCULATED; run FOR UPDATE lebih dulu (urutan [3] → [7]).
func (s *Service) Exclude(ctx context.Context, actor, runID, itemID uuid.UUID, reason string, meta reqmeta.Meta) (ItemDetail, error) {
	reason, err := validReason(reason, 10)
	if err != nil {
		return ItemDetail{}, err
	}
	tid, _ := tenantctx.From(ctx)
	err = db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		run, err := q.LockRun(ctx, runID)
		if err != nil {
			return err
		}
		if err := db.GuardTenant(ctx, "payroll_runs", run.TenantID); err != nil {
			return err
		}
		item, err := q.GetItem(ctx, store.GetItemParams{ID: itemID, RunID: runID})
		if err != nil {
			return err
		}
		if run.Status != "CALCULATED" || item.Status != "ERROR" {
			return apperr.ErrInvalidState.WithDetail("Only ERROR items of a CALCULATED run can be excluded")
		}
		if _, err := q.ExcludeItem(ctx, store.ExcludeItemParams{Reason: &reason, Actor: &actor, ID: itemID, RunID: runID}); err != nil {
			return err
		}
		if err := q.UpdateRunCounts(ctx, runID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "payroll_item.excluded",
			TargetType: "payroll_item", TargetID: itemID, Reason: reason}, meta)
	})
	if err != nil {
		return ItemDetail{}, db.MapError(err)
	}
	return s.GetItem(ctx, actor, runID, itemID, meta)
}

// Cancel (Y12): dari DRAFT/CALCULATED/FAILED. Periode kembali OPEN (run batal membebaskan slot unik).
func (s *Service) Cancel(ctx context.Context, actor, runID uuid.UUID, reason string, meta reqmeta.Meta) (Run, error) {
	reason, err := validReason(reason, 3)
	if err != nil {
		return Run{}, err
	}
	tid, _ := tenantctx.From(ctx)
	var out Run
	err = db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := s.loadRun(ctx, q, runID)
		if err != nil {
			return err
		}
		run, err := q.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		if _, err := q.LockPeriod(ctx, run.PeriodID); err != nil { // [2] → [3]
			return err
		}
		periodID, err := q.CancelRun(ctx, store.CancelRunParams{Actor: &actor, Reason: &reason, ID: runID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.ErrInvalidState.WithDetail("Cannot cancel a run in status " + cur.Status)
		}
		if err != nil {
			return err
		}
		if err := q.SetPeriodStatus(ctx, store.SetPeriodStatusParams{Status: "OPEN", ID: periodID}); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "payroll_run.cancelled",
			TargetType: "payroll_run", TargetID: runID, Reason: reason}, meta); err != nil {
			return err
		}
		out, err = s.loadRun(ctx, q, runID)
		return err
	})
	return out, db.MapError(err)
}
