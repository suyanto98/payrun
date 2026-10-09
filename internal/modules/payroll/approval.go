package payroll

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/modules/payroll/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/audit"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/db"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tenantctx"
	"payrun/internal/platform/testhook"
)

// Submit (Y10) dan keputusan approval PAYROLL_RUN (R2/R3, ADR-P07 + ADR-P09, Step 7 `22` §3.3).
//
//	submit:  run FOR UPDATE [3] → CALCULATED, tanpa item ERROR → approver dihitung server → approval [4]
//	         → run PENDING_APPROVAL. Fingerprint TIDAK dicek di sini (otoritatif saat approve/finalize).
//	approve: periode FOR UPDATE [2] (penulis input PeriodGuard FOR SHARE menunggu, O3) → run FOR UPDATE [3]
//	         → approver masih admin (DB) → decide [4] → kandidat change log sejak calc_started_at (O12)
//	         → verifikasi fingerprint + eligibility (D6) → APPROVED, atau 409 RUN_STALE (rollback penuh).
//	reject:  run FOR UPDATE [3] → decide [4] → kembali CALCULATED.

// maxStaleCandidates (Step 12 R1-7): di atas ini recalculate lebih murah daripada memverifikasi ribuan
// fingerprint sambil memegang lock periode.
const maxStaleCandidates = 200

// Approvals: pembuatan approval (modul approval), dipasang composition root (ADR-001).
type Approvals interface {
	CreateRunApprovalTx(ctx context.Context, tx pgx.Tx, runID, requester, approver uuid.UUID, self bool, reason *string, meta reqmeta.Meta) (uuid.UUID, error)
}

// SetApprovals memasang pembuat approval (wajib sebelum Submit).
func (s *Service) SetApprovals(a Approvals) { s.approvals = a }

type SubmitInput struct {
	ApproverUserID     *uuid.UUID `json:"approver_user_id"`
	SelfApprovalReason *string    `json:"self_approval_reason"`
}

// chooseApprover (ADR-P09 + amandemen): admin lain = membership ACTIVE OWNER/PAYROLL_ADMIN selain
// submitter, dihitung server (bukan klaim client). Pengecualian satu admin: self-approve dengan alasan.
func chooseApprover(actor uuid.UUID, admins []uuid.UUID, in SubmitInput) (uuid.UUID, bool, *string, error) {
	others := slices.DeleteFunc(slices.Clone(admins), func(u uuid.UUID) bool { return u == actor })
	var reason *string
	if in.SelfApprovalReason != nil {
		v := strings.TrimSpace(*in.SelfApprovalReason)
		if v != "" {
			if utf8.RuneCountInString(v) > 500 || strings.ContainsRune(v, 0) {
				return uuid.Nil, false, nil, apperr.Validation(apperr.FieldError{Field: "self_approval_reason", Code: "LENGTH", Message: "must be at most 500 characters"})
			}
			reason = &v
		}
	}
	if len(others) == 0 {
		if in.ApproverUserID != nil && *in.ApproverUserID != actor {
			return uuid.Nil, false, nil, apperr.ErrNotFound.WithDetail("approver_user_id is not an active admin of this tenant")
		}
		if reason == nil {
			return uuid.Nil, false, nil, apperr.ErrSelfApprovalReason
		}
		return actor, true, reason, nil
	}
	if reason != nil {
		return uuid.Nil, false, nil, apperr.ErrSelfApproval.WithDetail("Another admin can approve this run; self-approval is not allowed")
	}
	switch {
	case in.ApproverUserID == nil && len(others) == 1:
		return others[0], false, nil, nil
	case in.ApproverUserID == nil:
		return uuid.Nil, false, nil, apperr.Validation(apperr.FieldError{Field: "approver_user_id", Code: "REQUIRED", Message: "several admins can approve; choose one"})
	case *in.ApproverUserID == actor:
		return uuid.Nil, false, nil, apperr.ErrSelfApproval
	case !slices.Contains(others, *in.ApproverUserID):
		return uuid.Nil, false, nil, apperr.ErrNotFound.WithDetail("approver_user_id is not an active admin of this tenant")
	}
	return *in.ApproverUserID, false, nil, nil
}

// Submit (Y10): natural idempotency lewat guard status (submit kedua → 409 INVALID_STATE_TRANSITION).
func (s *Service) Submit(ctx context.Context, actor, runID uuid.UUID, in SubmitInput, meta reqmeta.Meta) (Run, error) {
	if s.approvals == nil {
		return Run{}, fmt.Errorf("payroll: approvals not wired")
	}
	tid, _ := tenantctx.From(ctx)
	var out Run
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		runner, err := s.readerRolesTx(ctx, tx, actor)
		if err != nil {
			return err
		}
		if !runner {
			return apperr.ErrForbidden.WithDetail("Only OWNER or PAYROLL_ADMIN can submit a run")
		}
		q := store.New(tx)
		run, err := q.LockRun(ctx, runID)
		if err != nil {
			return err
		}
		if err := db.GuardTenant(ctx, "payroll_runs", run.TenantID); err != nil {
			return err
		}
		cur, err := s.loadRun(ctx, q, runID)
		if err != nil {
			return err
		}
		if cur.Status == "FINALIZED" {
			return apperr.ErrRunFinalized
		}
		if cur.Status != "CALCULATED" {
			return apperr.ErrInvalidState.WithDetail("Only a CALCULATED run can be submitted (status " + cur.Status + ")")
		}
		if cur.Counts.Error != nil && *cur.Counts.Error > 0 {
			return apperr.ErrRunHasErrors.WithErrorCount(int(*cur.Counts.Error))
		}
		admins, err := s.members.ActiveAdminsTx(ctx, tx)
		if err != nil {
			return err
		}
		approver, self, reason, err := chooseApprover(actor, admins, in)
		if err != nil {
			return err
		}
		approvalID, err := s.approvals.CreateRunApprovalTx(ctx, tx, runID, actor, approver, self, reason, meta)
		if err != nil {
			return err
		}
		if n, err := q.SubmitRun(ctx, store.SubmitRunParams{Actor: &actor, ApprovalRequestID: &approvalID, ID: runID}); err != nil {
			return err
		} else if n != 1 {
			return apperr.ErrInvalidState
		}
		if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "payroll_run.submitted",
			TargetType: "payroll_run", TargetID: runID, Reason: derefStr(reason),
			Diff: map[string]any{"approval_request_id": approvalID.String(), "approver_id": approver.String(), "self_approved": self}}, meta); err != nil {
			return err
		}
		out, err = s.loadRun(ctx, q, runID)
		return err
	})
	return out, db.MapError(err)
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---------------------------------------------------------------- approval subject PAYROLL_RUN

// ApproveSubjectTx: handler approval.Subject untuk PAYROLL_RUN.
func (s *Service) ApproveSubjectTx(ctx context.Context, tx pgx.Tx, runID, approvalID, actor uuid.UUID, decide func(context.Context) error, meta reqmeta.Meta) (any, error) {
	tid, _ := tenantctx.From(ctx)
	q := store.New(tx)
	info, err := q.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if err := db.GuardTenant(ctx, "payroll_runs", info.TenantID); err != nil {
		return nil, err
	}
	if _, err := q.LockPeriod(ctx, info.PeriodID); err != nil { // [2] dunia input berhenti sejenak (O3)
		return nil, err
	}
	if _, err := q.LockRun(ctx, runID); err != nil { // [3]
		return nil, err
	}
	// Approver dicek ulang di DB (ADR-009): approval-nya tetap terlihat olehnya, jadi 403 (bukan 404).
	roles, err := s.members.ActiveRolesTx(ctx, tx, actor)
	if err != nil {
		return nil, err
	}
	if !(auth.Principal{Roles: roles}).HasAnyRole(runners...) {
		return nil, apperr.ErrForbidden.WithDetail("The approver is no longer an OWNER or PAYROLL_ADMIN")
	}
	if err := decide(ctx); err != nil { // [4] keputusan lain yang menang lebih dulu → 409 APPROVAL_ALREADY_DECIDED
		return nil, err
	}
	run, err := q.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.Status != "PENDING_APPROVAL" || run.ApprovalRequestID == nil || *run.ApprovalRequestID != approvalID {
		return nil, apperr.ErrInvalidState.WithDetail("The run is not waiting for this approval (status " + run.Status + ")")
	}
	stale, changed, err := s.staleEmployees(ctx, q, tid, run)
	if err != nil {
		return nil, err
	}
	if changed > maxStaleCandidates || len(stale) > 0 {
		return nil, apperr.ErrRunStale.WithStale(stale, changed)
	}
	// Titik hook test O3: approve memegang periode FOR UPDATE, fingerprint sudah diverifikasi.
	if err := testhook.Pause(ctx, "payroll.approve.before_update"); err != nil {
		return nil, err
	}
	if n, err := q.ApproveRun(ctx, store.ApproveRunParams{Actor: &actor, ID: runID}); err != nil {
		return nil, err
	} else if n != 1 {
		return nil, apperr.ErrInvalidState
	}
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "payroll_run.approved",
		TargetType: "payroll_run", TargetID: runID, Diff: map[string]any{"approval_request_id": approvalID.String(), "candidates": changed}}, meta); err != nil {
		return nil, err
	}
	return s.loadRun(ctx, q, runID)
}

// RejectSubjectTx: run kembali CALCULATED (ADR-P05); catatan tersimpan di approval.
func (s *Service) RejectSubjectTx(ctx context.Context, tx pgx.Tx, runID, approvalID, actor uuid.UUID, note string, decide func(context.Context) error, meta reqmeta.Meta) (any, error) {
	tid, _ := tenantctx.From(ctx)
	q := store.New(tx)
	run, err := q.LockRun(ctx, runID) // [3]
	if err != nil {
		return nil, err
	}
	if err := db.GuardTenant(ctx, "payroll_runs", run.TenantID); err != nil {
		return nil, err
	}
	if err := decide(ctx); err != nil { // [4]
		return nil, err
	}
	if n, err := q.RejectRun(ctx, runID); err != nil {
		return nil, err
	} else if n != 1 {
		return nil, apperr.ErrInvalidState.WithDetail("The run is not waiting for approval (status " + run.Status + ")")
	}
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "payroll_run.rejected",
		TargetType: "payroll_run", TargetID: runID, Reason: note, Diff: map[string]any{"approval_request_id": approvalID.String()}}, meta); err != nil {
		return nil, err
	}
	return s.loadRun(ctx, q, runID)
}

// SubjectSummaries: "Payroll 2026-03 · PENDING_APPROVAL · net 6888609.00 · 3 karyawan".
func (s *Service) SubjectSummaries(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	rs, err := store.New(tx).RunSummaries(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(rs))
	for _, r := range rs {
		sum := "Payroll " + periodText(r.Year, r.Month) + " · " + r.Status
		if n := toDec(r.TotalNet); n != nil {
			sum += " · net " + n.StringFixed(2)
		}
		if r.ItemOkCount != nil {
			sum += fmt.Sprintf(" · %d karyawan", *r.ItemOkCount)
		}
		out[r.ID] = sum
	}
	return out, nil
}

// staleEmployees (O12 + D6): kandidat = karyawan dengan change log sejak calc_started_at. Untuk setiap
// kandidat: input saat ini (InputProvider yang sama dengan kalkulasi) dibandingkan dengan fingerprint
// item; karyawan yang kini (tidak lagi) eligible juga stale. Kandidat > 200 → tidak diverifikasi.
// Dipanggil di bawah lock periode FOR UPDATE, sehingga input tidak berubah selama pemeriksaan.
func (s *Service) staleEmployees(ctx context.Context, q *store.Queries, tid uuid.UUID, run store.GetRunRow) ([]apperr.StaleEmployee, int, error) {
	if run.CalcStartedAt == nil {
		return nil, 0, fmt.Errorf("payroll: run %s has no calc_started_at", run.ID)
	}
	cands, err := q.StaleCandidates(ctx, store.StaleCandidatesParams{PeriodID: run.PeriodID, CalcStartedAt: *run.CalcStartedAt})
	if err != nil || len(cands) == 0 || len(cands) > maxStaleCandidates {
		return nil, len(cands), err
	}
	items, err := q.CandidateItems(ctx, store.CandidateItemsParams{RunID: run.ID, CalcVersion: run.CalcVersion, Ids: cands})
	if err != nil {
		return nil, 0, err
	}
	byItem := make(map[uuid.UUID]store.CandidateItemsRow, len(items))
	for _, it := range items {
		byItem[it.EmployeeID] = it
	}
	from, to := monthRange(int(run.Year), int(run.Month))
	elig, err := q.EligibleAmong(ctx, store.EligibleAmongParams{Ids: cands, FromDate: from, ToDate: to})
	if err != nil {
		return nil, 0, err
	}
	eligible := make(map[uuid.UUID]string, len(elig))
	ids := make([]uuid.UUID, 0, len(elig))
	for _, e := range elig {
		eligible[e.ID] = e.EmployeeNo
		ids = append(ids, e.ID)
	}
	data, err := loadChunk(ctx, q, tid, int(run.Year), int(run.Month), ids)
	if err != nil {
		return nil, 0, err
	}
	var stale []apperr.StaleEmployee
	for _, id := range cands {
		item, hasItem := byItem[id]
		no, isEligible := eligible[id]
		switch {
		case hasItem && isEligible:
			raw, err := json.Marshal(data.inputs[id])
			if err != nil {
				return nil, 0, err
			}
			if string(fingerprint(raw, data.rsCode, data.rsVersion)) != string(item.InputFingerprint) {
				stale = append(stale, apperr.StaleEmployee{EmployeeID: id.String(), EmployeeNo: item.EmployeeNo, Reason: "INPUT_CHANGED"})
			}
		case hasItem:
			stale = append(stale, apperr.StaleEmployee{EmployeeID: id.String(), EmployeeNo: item.EmployeeNo, Reason: "NO_LONGER_ELIGIBLE"})
		case isEligible:
			stale = append(stale, apperr.StaleEmployee{EmployeeID: id.String(), EmployeeNo: no, Reason: "NEWLY_ELIGIBLE"})
		}
	}
	return stale, len(cands), nil
}
