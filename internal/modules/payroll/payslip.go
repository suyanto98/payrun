package payroll

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"payrun/internal/modules/payroll/payslippdf"
	"payrun/internal/modules/payroll/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/audit"
	"payrun/internal/platform/db"
	"payrun/internal/platform/faults"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/jobs"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/observability"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/storage"
	"payrun/internal/platform/tenantctx"
	"payrun/internal/platform/testhook"
)

// Payslip (PF-15, PF-22, PI-17, ADR-012). Baris payslips dibuat PENDING di transaksi finalize (satu per
// item OK); PDF dirender job payslip.render per batch 20 dan disimpan di object key deterministik
// tenants/{tenant}/payslips/{run}/{employee}.pdf. Slip web dibaca dari payroll_lines (bukan PDF).
//
// Akses (file 17 §2.4): pemilik (employees.user_id = principal) melihat slipnya sendiri tanpa audit;
// OWNER/PAYROLL_ADMIN/HR_ADMIN melihat slip siapa pun, SETIAP akses ter-audit di transaksi yang sama
// (BR10). Selain itu 404. Role admin dibaca ulang dari DB (R2-6).

const (
	KindPayslipRender = "payslip.render"
	payslipBatch      = 20
	pdfRetryAfter     = 30 // detik, Retry-After PAYSLIP_NOT_READY
)

type renderPayload struct {
	RunID      uuid.UUID   `json:"run_id"`
	PayslipIDs []uuid.UUID `json:"payslip_ids"`
}

func payslipKey(tenant, run, employee uuid.UUID) string {
	return "tenants/" + tenant.String() + "/payslips/" + run.String() + "/" + employee.String() + ".pdf"
}

// createPayslips: di transaksi finalize. Item OK versi final → payslip PENDING + job render per 20.
func createPayslips(ctx context.Context, q *store.Queries, tx pgx.Tx, ids func() uuid.UUID, tid, runID uuid.UUID, ver int32, corr string) (int, error) {
	items, err := q.OKItemsOfVersion(ctx, store.OKItemsOfVersionParams{RunID: runID, CalcVersion: ver})
	if err != nil || len(items) == 0 {
		return 0, err
	}
	p := store.InsertPayslipsParams{TenantID: tid, RunID: runID}
	for _, it := range items {
		p.Ids = append(p.Ids, ids())
		p.EmployeeIds = append(p.EmployeeIds, it.EmployeeID)
		p.ItemIds = append(p.ItemIds, it.ID)
		p.ObjectKeys = append(p.ObjectKeys, payslipKey(tid, runID, it.EmployeeID))
	}
	if err := q.InsertPayslips(ctx, p); err != nil {
		return 0, err
	}
	for b := 0; b*payslipBatch < len(p.Ids); b++ {
		batch := p.Ids[b*payslipBatch : min((b+1)*payslipBatch, len(p.Ids))]
		if _, err := jobs.Enqueue(ctx, tx, jobs.Spec{Kind: KindPayslipRender, MaxAttempts: 5, CorrelationID: corr,
			Dedupe: fmt.Sprintf("%s:%s:%d", KindPayslipRender, runID, b), Payload: renderPayload{RunID: runID, PayslipIDs: batch}}); err != nil {
			return 0, err
		}
	}
	return len(p.Ids), nil
}

// ---------------------------------------------------------------- job payslip.render

// renderPayslips: per slip (PENDING/FAILED saja → idempoten): render PDF di memori → PUT object →
// transaksi pendek RENDERED + checksum. Slip gagal ditandai FAILED dan batch berlanjut; di akhir job
// error (retry, backoff default) bila ada yang gagal. Crash di tengah batch: slip yang sudah RENDERED
// dilewati saat retry; slip yang PUT-nya sudah terjadi tapi belum ditandai ditulis ulang ke key yang
// sama dengan byte identik (render deterministik).
func (j *Jobs) renderPayslips(ctx context.Context, job *jobs.Job) error {
	if job.TenantID == uuid.Nil {
		return jobs.Permanent(errors.New("payslip.render without tenant"))
	}
	if j.docs == nil {
		return errors.New("payslip.render: object storage not configured")
	}
	var p renderPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return jobs.Permanent(err)
	}
	start := metrics.Start()
	var rows []store.PayslipsForRenderRow
	lines := map[uuid.UUID][]store.ListLinesRow{}
	err := db.WithTenantTxFor(ctx, j.pool, job.TenantID, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		if rows, err = q.PayslipsForRender(ctx, p.PayslipIDs); err != nil {
			return err
		}
		for _, r := range rows {
			if r.TenantID != job.TenantID {
				metrics.TenantIsolationViolations.WithLabelValues("payslips").Inc()
				return jobs.Permanent(fmt.Errorf("payslip %s belongs to another tenant", r.ID))
			}
			if r.Status == "RENDERED" {
				continue
			}
			if lines[r.ItemID], err = q.ListLines(ctx, r.ItemID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	failed := 0
	for _, r := range rows {
		if r.Status == "RENDERED" {
			continue
		}
		sum, size, rerr := j.renderOne(ctx, r, lines[r.ItemID])
		werr := db.WithTenantTxFor(ctx, j.pool, job.TenantID, func(tx pgx.Tx) error {
			q := store.New(tx)
			if rerr != nil {
				return q.MarkPayslipFailed(ctx, store.MarkPayslipFailedParams{LastError: ptr(truncate(rerr.Error(), 500)), ID: r.ID})
			}
			at := j.clock.Now()
			_, err := q.MarkPayslipRendered(ctx, store.MarkPayslipRenderedParams{Checksum: sum[:], SizeBytes: &size, At: &at, ID: r.ID})
			return err
		})
		if werr != nil {
			return werr
		}
		if rerr != nil {
			failed++
			metrics.PayslipsRendered.WithLabelValues("failed").Inc()
			observability.Logger(ctx).Warn("payslip.render_failed", "payslip_id", r.ID, "error", rerr.Error())
		} else {
			metrics.PayslipsRendered.WithLabelValues("rendered").Inc()
		}
		// Titik hook test crash di tengah batch (PFS-10): slip ini sudah RENDERED, sisanya belum.
		if err := testhook.Pause(ctx, "payslip.render.after_slip"); err != nil {
			return err
		}
	}
	metrics.PayslipRenderDuration.WithLabelValues().Observe(metrics.Since(start))
	if failed > 0 {
		return fmt.Errorf("payslip.render: %d of %d payslips failed", failed, len(rows))
	}
	return db.WithTenantTxFor(ctx, j.pool, job.TenantID, func(tx pgx.Tx) error { return jobs.CompleteInTx(ctx, tx, job) })
}

func (j *Jobs) renderOne(ctx context.Context, r store.PayslipsForRenderRow, ls []store.ListLinesRow) ([32]byte, int32, error) {
	slip, err := slipOf(r, ls)
	if err != nil {
		return [32]byte{}, 0, err
	}
	pdf, err := payslippdf.Render(slip)
	if err != nil {
		return [32]byte{}, 0, err
	}
	if err := j.docs.Put(ctx, r.ObjectKey, pdf, "application/pdf"); err != nil {
		return [32]byte{}, 0, fmt.Errorf("put object: %w", err)
	}
	faults.Check(ctx, j.pool, faults.PointPayslipAfterPut, r.TenantID) // demo PFS-10: objek ada, DB belum RENDERED
	return sha256.Sum256(pdf), int32(len(pdf)), nil                    //nolint:gosec // G115: PDF slip jauh di bawah 2 GB
}

func slipOf(r store.PayslipsForRenderRow, ls []store.ListLinesRow) (payslippdf.Slip, error) {
	dec := func(s string) decimal.Decimal { d, _ := decimal.NewFromString(s); return d }
	s := payslippdf.Slip{TenantName: r.TenantName, Period: periodText(r.Year, r.Month), EmployeeNo: r.EmployeeNo, EmployeeName: r.EmployeeName,
		Position: r.PositionName, Department: r.DepartmentName, BankMasked: bankMasked(r.BankName, r.BankAccountLast4),
		Gross: dec(r.Gross), Deductions: dec(r.Deductions), Net: dec(r.Net), Ruleset: strings.TrimSpace(r.RulesetCode + " " + r.RulesetVersion)}
	if r.FinalizedAt == nil {
		return s, errors.New("run is not finalized")
	}
	s.FinalizedAt = *r.FinalizedAt
	for _, l := range ls {
		amt := toDec(l.Amount)
		if amt == nil {
			return s, errors.New("line without amount")
		}
		s.Lines = append(s.Lines, payslippdf.Line{Name: l.Name, Earning: l.Category == "EARNING", Amount: *amt, Explanation: l.Explanation})
	}
	return s, nil
}

// bankMasked: "BCA ••••1234" (PF-15 AC3); nomor penuh tidak pernah keluar dari modul employee.
func bankMasked(name, last4 *string) string {
	if last4 == nil {
		return ""
	}
	out := "••••" + *last4
	if name != nil && *name != "" {
		out = *name + " " + out
	}
	return out
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ---------------------------------------------------------------- model API (16-api §16)

type PayslipEmployee struct {
	EmployeeNo string `json:"employee_no"`
	FullName   string `json:"full_name"`
	Position   string `json:"position"`
	Department string `json:"department"`
}

type Payslip struct {
	ID                uuid.UUID       `json:"id"`
	RunID             uuid.UUID       `json:"run_id"`
	EmployeeID        uuid.UUID       `json:"employee_id"`
	Period            string          `json:"period"`
	Employee          PayslipEmployee `json:"employee"`
	BankAccountMasked *string         `json:"bank_account_masked"`
	Lines             []Line          `json:"lines"`
	Totals            Totals          `json:"totals"`
	Ruleset           RulesetRef      `json:"ruleset"`
	PDFStatus         string          `json:"pdf_status"`
	FinalizedAt       time.Time       `json:"finalized_at"`
}

type PayslipSummary struct {
	ID          uuid.UUID `json:"id"`
	Period      string    `json:"period"`
	Net         string    `json:"net"`
	FinalizedAt time.Time `json:"finalized_at"`
	PDFStatus   string    `json:"pdf_status"`
}

type RunPayslip struct {
	ID           uuid.UUID  `json:"id"`
	EmployeeID   uuid.UUID  `json:"employee_id"`
	EmployeeNo   string     `json:"employee_no"`
	EmployeeName string     `json:"employee_name"`
	Period       string     `json:"period"`
	Net          string     `json:"net"`
	PDFStatus    string     `json:"pdf_status"`
	RenderedAt   *time.Time `json:"rendered_at"`
	Attempts     int16      `json:"attempts"`
}

type PayslipDownload struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SetStorage: object storage untuk URL unduhan (S5). Tanpa storage → S5 PAYSLIP_NOT_READY.
func (s *Service) SetStorage(st storage.Store) { s.docs = st }

func (s *Service) selfEmployee(ctx context.Context, q *store.Queries, user uuid.UUID) (uuid.UUID, bool, error) {
	id, err := q.SelfEmployee(ctx, &user)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return id, err == nil, err
}

// payslipAccess: pemilik → owner=true (tanpa audit); admin pembaca (role dari DB) → owner=false; lainnya 404.
func (s *Service) payslipAccess(ctx context.Context, tx pgx.Tx, q *store.Queries, actor uuid.UUID, ps store.GetPayslipRow, selfOnly bool) (owner bool, err error) {
	if err := db.GuardTenant(ctx, "payslips", ps.TenantID); err != nil {
		return false, err
	}
	self, ok, err := s.selfEmployee(ctx, q, actor)
	if err != nil {
		return false, err
	}
	if ok && self == ps.EmployeeID {
		return true, nil
	}
	if selfOnly {
		return false, apperr.ErrNotFound
	}
	if _, err := s.readerRolesTx(ctx, tx, actor); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Service) loadPayslip(ctx context.Context, q *store.Queries, ps store.GetPayslipRow) (Payslip, error) {
	rows, err := q.PayslipsForRender(ctx, []uuid.UUID{ps.ID})
	if err != nil {
		return Payslip{}, err
	}
	if len(rows) != 1 || ps.FinalizedAt == nil {
		return Payslip{}, apperr.ErrNotFound
	}
	r := rows[0]
	out := Payslip{ID: ps.ID, RunID: ps.RunID, EmployeeID: ps.EmployeeID, Period: periodText(ps.Year, ps.Month),
		Employee: PayslipEmployee{EmployeeNo: r.EmployeeNo, FullName: r.EmployeeName, Position: r.PositionName, Department: r.DepartmentName},
		Totals:   Totals{Gross: r.Gross, Deductions: r.Deductions, Net: r.Net}, Ruleset: RulesetRef{Code: r.RulesetCode, Version: r.RulesetVersion},
		PDFStatus: ps.Status, FinalizedAt: *ps.FinalizedAt, Lines: []Line{}}
	if m := bankMasked(r.BankName, r.BankAccountLast4); m != "" {
		out.BankAccountMasked = &m
	}
	ls, err := q.ListLines(ctx, ps.ItemID)
	if err != nil {
		return Payslip{}, err
	}
	for _, l := range ls {
		out.Lines = append(out.Lines, Line{SortOrder: l.SortOrder, Code: l.Code, Name: l.Name, Category: l.Category, Stage: l.Stage,
			Amount: *fixed(l.Amount), Basis: l.Basis, Explanation: l.Explanation, Taxable: l.Taxable, Rounding: l.Rounding, AdjustmentID: l.AdjustmentID})
	}
	return out, nil
}

// GetPayslip: S2 (selfOnly, pemilik saja) dan S4 (admin; akses non-pemilik ter-audit payslip.viewed_by_admin).
func (s *Service) GetPayslip(ctx context.Context, actor, id uuid.UUID, selfOnly bool, meta reqmeta.Meta) (Payslip, error) {
	tid, _ := tenantctx.From(ctx)
	var out Payslip
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		ps, err := q.GetPayslip(ctx, id)
		if err != nil {
			return err
		}
		owner, err := s.payslipAccess(ctx, tx, q, actor, ps, selfOnly)
		if err != nil {
			return err
		}
		if out, err = s.loadPayslip(ctx, q, ps); err != nil {
			return err
		}
		if owner && selfOnly {
			metrics.PayslipAccess.WithLabelValues("owner_employee").Inc()
			return nil
		}
		metrics.PayslipAccess.WithLabelValues("admin").Inc()
		return audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "payslip.viewed_by_admin",
			TargetType: "payslip_access", TargetID: id, Diff: map[string]any{"employee_id": ps.EmployeeID, "run_id": ps.RunID}}, meta)
	})
	return out, db.MapError(err)
}

// DownloadPayslip (S5): pemilik atau admin (ter-audit payslip.downloaded). URL presigned dibuat SETELAH
// transaksi otorisasi + audit COMMIT (C-40) dan tidak pernah di-log.
func (s *Service) DownloadPayslip(ctx context.Context, actor, id uuid.UUID, meta reqmeta.Meta) (PayslipDownload, error) {
	tid, _ := tenantctx.From(ctx)
	var ps store.GetPayslipRow
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		if ps, err = q.GetPayslip(ctx, id); err != nil {
			return err
		}
		owner, err := s.payslipAccess(ctx, tx, q, actor, ps, false)
		if err != nil {
			return err
		}
		if ps.Status != "RENDERED" || s.docs == nil {
			return apperr.ErrPayslipNotReady.WithRetryAfter(pdfRetryAfter)
		}
		if owner {
			metrics.PayslipAccess.WithLabelValues("owner_employee").Inc()
			return nil
		}
		metrics.PayslipAccess.WithLabelValues("admin").Inc()
		return audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "payslip.downloaded",
			TargetType: "payslip_access", TargetID: id, Diff: map[string]any{"employee_id": ps.EmployeeID, "run_id": ps.RunID}}, meta)
	})
	if err != nil {
		return PayslipDownload{}, db.MapError(err)
	}
	expires := s.clock.Now().Add(storage.PresignTTL)
	u, err := s.docs.PresignGet(ctx, ps.ObjectKey, storage.PresignTTL, "slip-gaji-"+periodText(ps.Year, ps.Month)+".pdf")
	if err != nil {
		return PayslipDownload{}, err
	}
	return PayslipDownload{URL: u.String(), ExpiresAt: expires}, nil
}

// ListMyPayslips (S1): slip FINALIZED milik pemanggil. Akun tanpa employee → daftar kosong.
func (s *Service) ListMyPayslips(ctx context.Context, actor uuid.UUID, year, cursor string, limit int32) ([]PayslipSummary, httpx.Page, error) {
	page := httpx.Page{}
	params := store.ListMyPayslipsParams{Lim: limit + 1}
	if year != "" {
		y, err := parseYear(year)
		if err != nil {
			return nil, page, err
		}
		params.Year = &y
	}
	if cursor != "" {
		c, err := httpx.DecodeCursor(cursor)
		if err != nil {
			return nil, page, err
		}
		params.CursorAt, params.CursorID = &c.At, &c.ID
	}
	out := []PayslipSummary{}
	var last []time.Time
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		self, ok, err := s.selfEmployee(ctx, q, actor)
		if err != nil || !ok {
			return err
		}
		params.EmployeeID = self
		rows, err := q.ListMyPayslips(ctx, params)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := db.GuardTenant(ctx, "payslips", r.TenantID); err != nil {
				return err
			}
			if r.FinalizedAt == nil {
				continue
			}
			out = append(out, PayslipSummary{ID: r.ID, Period: periodText(r.Year, r.Month), Net: r.Net, FinalizedAt: *r.FinalizedAt, PDFStatus: r.Status})
			last = append(last, r.PeriodAt)
		}
		return nil
	})
	if err != nil {
		return nil, page, db.MapError(err)
	}
	if int32(len(out)) > limit { //nolint:gosec // G115
		out = out[:limit]
		c := httpx.EncodeCursor(httpx.Cursor{At: last[limit-1], ID: out[limit-1].ID})
		page = httpx.Page{NextCursor: &c, HasMore: true}
	}
	return out, page, nil
}

func parseYear(s string) (int16, error) {
	var y int
	if _, err := fmt.Sscanf(s, "%d", &y); err != nil || y < 2000 || y > 2100 || fmt.Sprint(y) != s {
		return 0, apperr.Validation(apperr.FieldError{Field: "year", Code: "FORMAT", Message: "YYYY"})
	}
	return int16(y), nil
}

// ListRunPayslips (S3): ringkasan slip satu run, role pembaca dari DB; setiap request ter-audit
// payroll.bulk_read (run_id, jumlah baris; R2-6). Cursor (employee_no, id) seperti daftar item Y6.
func (s *Service) ListRunPayslips(ctx context.Context, actor uuid.UUID, runID, employeeID, cursor string, limit int32, meta reqmeta.Meta) ([]RunPayslip, httpx.Page, error) {
	page := httpx.Page{}
	run, err := uuid.Parse(runID)
	if err != nil {
		return nil, page, apperr.Validation(apperr.FieldError{Field: "run_id", Code: "REQUIRED", Message: "run_id (uuid) is required"})
	}
	params := store.ListRunPayslipsParams{RunID: run, Lim: limit + 1}
	if employeeID != "" {
		id, err := uuid.Parse(employeeID)
		if err != nil {
			return nil, page, apperr.Validation(apperr.FieldError{Field: "employee_id", Code: "FORMAT"})
		}
		params.EmployeeID = &id
	}
	if cursor != "" {
		c, err := httpx.DecodeKeyCursor(cursor)
		if err != nil {
			return nil, page, err
		}
		params.AfterNo, params.AfterID = &c.Key, &c.ID
	}
	tid, _ := tenantctx.From(ctx)
	out := []RunPayslip{}
	err = db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := s.readerRolesTx(ctx, tx, actor); err != nil {
			return apperr.ErrForbidden
		}
		q := store.New(tx)
		rows, err := q.ListRunPayslips(ctx, params)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := db.GuardTenant(ctx, "payslips", r.TenantID); err != nil {
				return err
			}
			out = append(out, RunPayslip{ID: r.ID, EmployeeID: r.EmployeeID, EmployeeNo: r.EmployeeNo, EmployeeName: r.EmployeeName,
				Period: periodText(r.Year, r.Month), Net: r.Net, PDFStatus: r.Status, RenderedAt: r.RenderedAt, Attempts: r.Attempts})
		}
		return audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "payroll.bulk_read",
			TargetType: "payroll_run", TargetID: run, Diff: map[string]any{"rows": min(len(out), int(limit)), "source": "payslips"}}, meta)
	})
	if err != nil {
		return nil, page, db.MapError(err)
	}
	if int32(len(out)) > limit { //nolint:gosec // G115
		out = out[:limit]
		last := out[len(out)-1]
		c := httpx.EncodeKeyCursor(httpx.KeyCursor{Key: last.EmployeeNo, ID: last.ID})
		page = httpx.Page{NextCursor: &c, HasMore: true}
	}
	return out, page, nil
}
