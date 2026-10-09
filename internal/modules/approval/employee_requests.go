package approval

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/modules/approval/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/db"
	"payrun/internal/platform/reqmeta"
)

// Approval cuti/lembur (S6, ADR-P09 + amandemen, file 17 §3.2): approver di-snapshot saat submit =
// atasan langsung saat ini (S14) → HR_ADMIN → OWNER, tidak pernah requester sendiri (CHECK DB juga).

// Memberships: membership aktif (modul iam), dipasang composition root.
type Memberships interface {
	ActiveRolesTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) ([]string, error)
	ActiveUsersWithRoleTx(ctx context.Context, tx pgx.Tx, role string) ([]uuid.UUID, error)
}

// SetMemberships memasang Memberships (wajib sebelum CreateForEmployeeTx).
func (s *Service) SetMemberships(m Memberships) { s.members = m }

// ResolveApproverTx: approver untuk request karyawan. managerUser = user atasan langsung (boleh nil).
func (s *Service) ResolveApproverTx(ctx context.Context, tx pgx.Tx, requester uuid.UUID, managerUser *uuid.UUID) (uuid.UUID, error) {
	if s.members == nil {
		return uuid.Nil, errors.New("approval: memberships not wired")
	}
	if managerUser != nil && *managerUser != requester {
		roles, err := s.members.ActiveRolesTx(ctx, tx, *managerUser)
		if err != nil {
			return uuid.Nil, err
		}
		if len(roles) > 0 { // atasan masih anggota aktif tenant
			return *managerUser, nil
		}
	}
	for _, role := range []string{auth.RoleHRAdmin, auth.RoleOwner} {
		users, err := s.members.ActiveUsersWithRoleTx(ctx, tx, role)
		if err != nil {
			return uuid.Nil, err
		}
		for _, u := range users {
			if u != requester {
				return u, nil
			}
		}
	}
	return uuid.Nil, apperr.ErrNoApproverAvailable
}

// Brief: approval terbaru sebuah subjek (LeaveRequest/OvertimeRequest.approval).
type Brief struct {
	ID       uuid.UUID `json:"id"`
	Approver Person    `json:"approver"`
	Status   string    `json:"status"`
}

// CreateForEmployeeTx: resolve approver lalu buat approval di transaksi submit subjek.
func (s *Service) CreateForEmployeeTx(ctx context.Context, tx pgx.Tx, subjectType string, subjectID, requester uuid.UUID, managerUser *uuid.UUID, meta reqmeta.Meta) (uuid.UUID, error) {
	approver, err := s.ResolveApproverTx(ctx, tx, requester, managerUser)
	if err != nil {
		return uuid.Nil, err
	}
	return s.CreateTx(ctx, tx, NewRequest{SubjectType: subjectType, SubjectID: subjectID, Requester: requester, Approver: approver}, meta)
}

// CancelPendingTx: requester membatalkan approval PENDING subjeknya. false → sudah diputuskan / bukan miliknya.
func (s *Service) CancelPendingTx(ctx context.Context, tx pgx.Tx, subjectType string, subjectID, requester uuid.UUID) (bool, error) {
	n, err := store.New(tx).CancelPendingApproval(ctx, store.CancelPendingApprovalParams{Actor: &requester, SubjectType: subjectType, SubjectID: subjectID})
	return n == 1, db.MapError(err)
}

// BriefsTx: approval terbaru per subjek.
func (s *Service) BriefsTx(ctx context.Context, tx pgx.Tx, subjectType string, ids []uuid.UUID) (map[uuid.UUID]Brief, error) {
	rs, err := store.New(tx).ApprovalsBySubjects(ctx, store.ApprovalsBySubjectsParams{SubjectType: subjectType, SubjectIds: ids})
	if err != nil {
		return nil, db.MapError(err)
	}
	out := make(map[uuid.UUID]Brief, len(rs))
	for _, r := range rs {
		out[r.SubjectID] = Brief{ID: r.ID, Approver: Person{ID: r.ApproverID, DisplayName: r.ApproverName}, Status: r.Status}
	}
	return out, nil
}
