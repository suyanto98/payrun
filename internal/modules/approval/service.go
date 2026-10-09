// Package approval: satu mesin approval satu tingkat untuk payroll run, cuti, dan lembur (PF-11,
// ADR-P09). Modul ini memiliki approval_requests; efek bisnis per subjek dijalankan handler subjek
// (didaftarkan composition root) di transaksi yang sama dengan keputusan.
//
// Urutan di setiap keputusan (R2/R3): baca approval (tanpa lock) → otorisasi → handler subjek mengunci
// subjeknya ([2]/[3]) lalu memanggil decide() — UPDATE … WHERE status='PENDING' AND approver_id=actor
// ([4]) — lalu menerapkan efeknya. Keputusan yang kalah balapan (approve vs reject) mendapat 0 baris →
// 409 APPROVAL_ALREADY_DECIDED, dan seluruh transaksi (termasuk efek) rollback.
package approval

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/internal/modules/approval/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/audit"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/db"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tenantctx"
)

const (
	SubjectPayrollRun = "PAYROLL_RUN"
	SubjectLeave      = "LEAVE_REQUEST"
	SubjectOvertime   = "OVERTIME_REQUEST"
)

// Decide menjalankan guard keputusan [4]; dipanggil handler subjek SETELAH mengunci subjeknya.
type Decide = func(ctx context.Context) error

// Subject adalah handler efek per subject_type (diimplementasikan modul pemilik subjek).
type Subject interface {
	// ApproveSubjectTx: kunci subjek, decide(), terapkan efek APPROVED; mengembalikan subjek terbaru.
	ApproveSubjectTx(ctx context.Context, tx pgx.Tx, subjectID, approvalID, actor uuid.UUID, decide Decide, meta reqmeta.Meta) (any, error)
	// RejectSubjectTx: kunci subjek, decide(), terapkan efek REJECTED.
	RejectSubjectTx(ctx context.Context, tx pgx.Tx, subjectID, approvalID, actor uuid.UUID, note string, decide Decide, meta reqmeta.Meta) (any, error)
	// SubjectSummaries: ringkasan satu baris per subjek untuk inbox.
	SubjectSummaries(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

type Service struct {
	pool     *pgxpool.Pool
	ids      idgen.Generator
	subjects map[string]Subject
	members  Memberships
}

func NewService(pool *pgxpool.Pool, ids idgen.Generator) *Service {
	return &Service{pool: pool, ids: ids, subjects: map[string]Subject{}}
}

// Register memasang handler subjek (composition root).
func (s *Service) Register(subjectType string, h Subject) { s.subjects[subjectType] = h }

// ---------------------------------------------------------------- model (16-api §16 ApprovalRequest)

type Person struct {
	ID          uuid.UUID `json:"id"`
	DisplayName *string   `json:"display_name"`
}

type SubjectRef struct {
	Summary string `json:"summary"`
}

type Approval struct {
	ID                 uuid.UUID  `json:"id"`
	SubjectType        string     `json:"subject_type"`
	SubjectID          uuid.UUID  `json:"subject_id"`
	Subject            SubjectRef `json:"subject"`
	Requester          Person     `json:"requester"`
	Approver           Person     `json:"approver"`
	Status             string     `json:"status"`
	DecisionNote       *string    `json:"decision_note"`
	DecidedBy          *uuid.UUID `json:"decided_by"`
	IsOverride         bool       `json:"is_override"`
	SelfApproved       bool       `json:"self_approved"`
	SelfApprovalReason *string    `json:"self_approval_reason"`
	DecidedAt          *time.Time `json:"decided_at"`
	CreatedAt          time.Time  `json:"created_at"`
}

func toApproval(r store.GetApprovalRow) Approval {
	return Approval{ID: r.ID, SubjectType: r.SubjectType, SubjectID: r.SubjectID,
		Requester: Person{ID: r.RequesterID, DisplayName: r.RequesterName}, Approver: Person{ID: r.ApproverID, DisplayName: r.ApproverName},
		Status: r.Status, DecisionNote: r.DecisionNote, DecidedBy: r.DecidedBy, IsOverride: r.IsOverride, SelfApproved: r.SelfApproved,
		SelfApprovalReason: r.SelfApprovalReason, DecidedAt: r.DecidedAt, CreatedAt: r.CreatedAt}
}

// ---------------------------------------------------------------- create (dipanggil submit subjek)

// NewRequest: approval baru. SelfApproved hanya untuk PAYROLL_RUN tanpa admin lain (CHECK DB juga).
type NewRequest struct {
	SubjectType        string
	SubjectID          uuid.UUID
	Requester          uuid.UUID
	Approver           uuid.UUID
	SelfApproved       bool
	SelfApprovalReason *string
}

// CreateTx di transaksi submit subjek. Audit approval.requested.
func (s *Service) CreateTx(ctx context.Context, tx pgx.Tx, r NewRequest, meta reqmeta.Meta) (uuid.UUID, error) {
	tid, _ := tenantctx.From(ctx)
	id := s.ids.New()
	if err := store.New(tx).CreateApproval(ctx, store.CreateApprovalParams{TenantID: tid, ID: id, SubjectType: r.SubjectType,
		SubjectID: r.SubjectID, RequesterID: r.Requester, ApproverID: r.Approver, SelfApproved: r.SelfApproved,
		SelfApprovalReason: r.SelfApprovalReason}); err != nil {
		return uuid.Nil, db.MapError(err)
	}
	diff := map[string]any{"subject_type": r.SubjectType, "subject_id": r.SubjectID.String(), "approver_id": r.Approver.String()}
	if r.SelfApproved {
		diff["self_approved"] = true
	}
	return id, audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: r.Requester, Action: "approval.requested",
		TargetType: "approval_request", TargetID: id, Diff: diff, Reason: deref(r.SelfApprovalReason)}, meta)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---------------------------------------------------------------- R2 / R3

// Decision: approval terbaru + subjek terbaru (R2/R3).
type Decision struct {
	Approval Approval `json:"approval"`
	Subject  any      `json:"subject"`
}

func validNote(note string, required bool) (*string, error) {
	note = strings.TrimSpace(note)
	if note == "" {
		if required {
			return nil, apperr.ErrDecisionNoteRequired
		}
		return nil, nil
	}
	if utf8.RuneCountInString(note) > 500 || strings.ContainsRune(note, 0) {
		return nil, apperr.Validation(apperr.FieldError{Field: "note", Code: "LENGTH", Message: "must be at most 500 characters"})
	}
	return &note, nil
}

func (s *Service) Approve(ctx context.Context, p auth.Principal, id uuid.UUID, note string, meta reqmeta.Meta) (Decision, error) {
	n, err := validNote(note, false)
	if err != nil {
		return Decision{}, err
	}
	return s.decide(ctx, p, id, "APPROVED", n, meta)
}

func (s *Service) Reject(ctx context.Context, p auth.Principal, id uuid.UUID, note string, meta reqmeta.Meta) (Decision, error) {
	n, err := validNote(note, true)
	if err != nil {
		return Decision{}, err
	}
	return s.decide(ctx, p, id, "REJECTED", n, meta)
}

func (s *Service) decide(ctx context.Context, p auth.Principal, id uuid.UUID, status string, note *string, meta reqmeta.Meta) (Decision, error) {
	tid, _ := tenantctx.From(ctx)
	var out Decision
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		a, err := q.GetApproval(ctx, id)
		if err != nil {
			return err // tidak ada / tenant lain → 404
		}
		if err := db.GuardTenant(ctx, "approval_requests", a.TenantID); err != nil {
			return err
		}
		involved := p.UserID == a.ApproverID || p.UserID == a.RequesterID
		override := false
		if !involved {
			// Override HR (file 17 §3.3): hanya cuti/lembur, role HR_ADMIN dibaca ulang dari DB. Selain itu
			// pemanggil bukan pihak approval ini → 404 (tidak membocorkan keberadaannya).
			ok, err := s.canOverride(ctx, tx, p.UserID, a.SubjectType)
			if err != nil {
				return err
			}
			if !ok {
				return apperr.ErrNotFound
			}
			override = true
		}
		switch {
		case p.UserID == a.RequesterID && !a.SelfApproved:
			return apperr.ErrSelfApproval // requester tidak memutuskan request miliknya (BR7, PI-11)
		case a.Status != "PENDING":
			return apperr.ErrApprovalDecided.WithApprovalStatus(a.Status)
		}
		h, ok := s.subjects[a.SubjectType]
		if !ok {
			return fmt.Errorf("approval: no handler for subject type %s", a.SubjectType)
		}
		decided := false
		decideFn := func(ctx context.Context) error {
			var n int64
			var err error
			if override {
				n, err = q.DecideApprovalOverride(ctx, store.DecideApprovalOverrideParams{Status: status, Note: note, Actor: &p.UserID, ID: id})
			} else {
				n, err = q.DecideApproval(ctx, store.DecideApprovalParams{Status: status, Note: note, Actor: &p.UserID, ID: id})
			}
			if err != nil {
				return db.MapError(err)
			}
			if n == 0 { // keputusan lain menang di antara baca dan lock subjek
				cur, err := q.GetApproval(ctx, id)
				if err != nil {
					return err
				}
				return apperr.ErrApprovalDecided.WithApprovalStatus(cur.Status)
			}
			decided = true
			return nil
		}
		var subject any
		if status == "APPROVED" {
			subject, err = h.ApproveSubjectTx(ctx, tx, a.SubjectID, id, p.UserID, decideFn, meta)
		} else {
			subject, err = h.RejectSubjectTx(ctx, tx, a.SubjectID, id, p.UserID, deref(note), decideFn, meta)
		}
		if err != nil {
			return err
		}
		if !decided {
			return fmt.Errorf("approval: handler for %s did not call decide", a.SubjectType)
		}
		action := "approval.approved"
		if status == "REJECTED" {
			action = "approval.rejected"
		}
		if override {
			if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: "approval.overridden",
				TargetType: "approval_request", TargetID: id, Reason: deref(note),
				Diff: map[string]any{"decision": status, "approver_id": a.ApproverID.String(), "subject_type": a.SubjectType}}, meta); err != nil {
				return err
			}
		}
		if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: action,
			TargetType: "approval_request", TargetID: id, Reason: deref(note),
			Diff: map[string]any{"subject_type": a.SubjectType, "subject_id": a.SubjectID.String()}}, meta); err != nil {
			return err
		}
		r, err := q.GetApproval(ctx, id)
		if err != nil {
			return err
		}
		out = Decision{Approval: toApproval(r), Subject: subject}
		sum, err := h.SubjectSummaries(ctx, tx, []uuid.UUID{a.SubjectID})
		if err != nil {
			return err
		}
		out.Approval.Subject.Summary = sum[a.SubjectID]
		return nil
	})
	if err != nil {
		return Decision{}, db.MapError(err)
	}
	return out, nil
}

// canOverride: HR_ADMIN (menurut DB) memutuskan approval cuti/lembur milik orang lain. Tidak pernah untuk
// PAYROLL_RUN (pemisahan tugas, ADR-P09 amandemen).
func (s *Service) canOverride(ctx context.Context, tx pgx.Tx, user uuid.UUID, subjectType string) (bool, error) {
	if subjectType != SubjectLeave && subjectType != SubjectOvertime {
		return false, nil
	}
	if s.members == nil {
		return false, fmt.Errorf("approval: memberships not wired")
	}
	roles, err := s.members.ActiveRolesTx(ctx, tx, user)
	if err != nil {
		return false, err
	}
	return (auth.Principal{Roles: roles}).HasRole(auth.RoleHRAdmin), nil
}

// ---------------------------------------------------------------- R1 inbox

type InboxQuery struct {
	Status, SubjectType, Scope, Cursor string
	Limit                              int32
}

var (
	statuses     = map[string]bool{"PENDING": true, "APPROVED": true, "REJECTED": true, "CANCELLED": true}
	subjectTypes = map[string]bool{SubjectPayrollRun: true, SubjectLeave: true, SubjectOvertime: true}
)

// Inbox (R1): approval milik pemanggil. HR_ADMIN dengan scope=all melihat semua (untuk override
// cuti/lembur, S6); role lain dengan scope=all → 403.
func (s *Service) Inbox(ctx context.Context, p auth.Principal, iq InboxQuery) ([]Approval, httpx.Page, error) {
	page := httpx.Page{}
	if iq.Status == "" {
		iq.Status = "PENDING"
	}
	var fe []apperr.FieldError
	if !statuses[iq.Status] {
		fe = append(fe, apperr.FieldError{Field: "status", Code: "INVALID_ENUM"})
	}
	if iq.SubjectType != "" && !subjectTypes[iq.SubjectType] {
		fe = append(fe, apperr.FieldError{Field: "subject_type", Code: "INVALID_ENUM"})
	}
	if iq.Scope != "" && iq.Scope != "all" && iq.Scope != "mine" {
		fe = append(fe, apperr.FieldError{Field: "scope", Code: "INVALID_ENUM"})
	}
	if len(fe) > 0 {
		return nil, page, apperr.Validation(fe...)
	}
	params := store.ListApprovalsParams{Status: iq.Status, Lim: iq.Limit + 1}
	if iq.Scope == "all" {
		if !p.HasRole(auth.RoleHRAdmin) {
			return nil, page, apperr.ErrForbidden.WithDetail("scope=all is for HR_ADMIN")
		}
	} else {
		params.ApproverID = &p.UserID
	}
	if iq.SubjectType != "" {
		params.SubjectType = &iq.SubjectType
	}
	if iq.Cursor != "" {
		c, err := httpx.DecodeCursor(iq.Cursor)
		if err != nil {
			return nil, page, err
		}
		params.AfterAt, params.AfterID = &c.At, &c.ID
	}
	out := []Approval{}
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		rs, err := store.New(tx).ListApprovals(ctx, params)
		if err != nil {
			return err
		}
		byType := map[string][]uuid.UUID{}
		for _, r := range rs {
			if err := db.GuardTenant(ctx, "approval_requests", r.TenantID); err != nil {
				return err
			}
			out = append(out, toApproval(store.GetApprovalRow(r)))
			byType[r.SubjectType] = append(byType[r.SubjectType], r.SubjectID)
		}
		for st, ids := range byType {
			h, ok := s.subjects[st]
			if !ok {
				continue
			}
			sum, err := h.SubjectSummaries(ctx, tx, ids)
			if err != nil {
				return err
			}
			for i := range out {
				if out[i].SubjectType == st {
					out[i].Subject.Summary = sum[out[i].SubjectID]
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, page, db.MapError(err)
	}
	if int32(len(out)) > iq.Limit { //nolint:gosec // G115: panjang ≤ limit+1
		out = out[:iq.Limit]
		last := out[len(out)-1]
		c := httpx.EncodeCursor(httpx.Cursor{At: last.CreatedAt, ID: last.ID})
		page = httpx.Page{NextCursor: &c, HasMore: true}
	}
	return out, page, nil
}
