package iam

import (
	"context"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/modules/iam/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/audit"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/db"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tenantctx"
)

// Ubah role (U4) dan cabut membership (U5), PF-02. Aturan grant (file 17 §2.1 ᴿ): OWNER boleh semua;
// HR_ADMIN hanya MANAGER/EMPLOYEE dan hanya pada member yang tidak memegang role admin. Hak pemanggil
// dibaca ulang dari DB di dalam transaksi (ADR-P02: role di token bisa basi ≤ 15 menit), sesudah
// advisory lock `owners:<tenant>`, sehingga dua OWNER yang saling mencabut diserialkan dan yang kedua
// melihat dirinya sudah dicabut. Trigger deferred PI-16 (`P0004` → 409 LAST_OWNER) adalah lapisan akhir.

var adminRoles = []string{auth.RoleOwner, auth.RoleHRAdmin, auth.RolePayroll}

func hasAdmin(roles []string) bool {
	return slices.ContainsFunc(roles, func(r string) bool { return slices.Contains(adminRoles, r) })
}

// Precondition: hasil If-Match; Err (428/412) ditunda sampai otorisasi lolos, supaya role tanpa hak
// mendapat 403/404, bukan 428 (catatan implementasi S2).
type Precondition struct {
	Version int32
	Err     error
}

// normalizeRoles: unik, terurut, non-kosong, anggota enum.
func normalizeRoles(in []string) ([]string, error) {
	roles := slices.Clone(in)
	slices.Sort(roles)
	roles = slices.Compact(roles)
	if len(roles) == 0 {
		return nil, apperr.Validation(apperr.FieldError{Field: "roles", Code: "REQUIRED", Message: "at least one role"})
	}
	for _, r := range roles {
		if !slices.Contains(allRoles, r) {
			return nil, apperr.Validation(apperr.FieldError{Field: "roles", Code: "INVALID_ENUM", Message: "unknown role " + r})
		}
	}
	return roles, nil
}

// lockAndAuthorize: advisory lock → role pemanggil dari DB → target FOR UPDATE. Pemanggil yang sudah
// tidak OWNER/HR_ADMIN di DB → 403 walaupun tokennya masih membawa role itu.
func lockAndAuthorize(ctx context.Context, q *store.Queries, tid uuid.UUID, p auth.Principal, id uuid.UUID) ([]string, store.GetMembershipForUpdateRow, error) {
	var target store.GetMembershipForUpdateRow
	if err := q.LockTenantOwners(ctx, tid.String()); err != nil {
		return nil, target, err
	}
	actor, err := q.ActorRoles(ctx, &p.UserID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !slices.Contains(actor, auth.RoleOwner) && !slices.Contains(actor, auth.RoleHRAdmin)) {
		return nil, target, apperr.ErrForbidden.WithDetail("Your roles in this tenant changed; sign in again")
	}
	if err != nil {
		return nil, target, err
	}
	target, err = q.GetMembershipForUpdate(ctx, id)
	if err != nil {
		return nil, target, err // tidak ada / tenant lain → 404
	}
	if err := db.GuardTenant(ctx, "memberships", target.TenantID); err != nil {
		return nil, target, err
	}
	return actor, target, nil
}

// ActiveRolesTx: role membership ACTIVE user di tenant transaksi (nil kalau bukan anggota aktif).
// Dipakai modul lain lewat interface di composition root untuk operasi sensitif (ADR-P02): role di
// token bisa basi ≤ 15 menit.
func (s *Service) ActiveRolesTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) ([]string, error) {
	roles, err := store.New(tx).ActorRoles(ctx, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return roles, err
}

func (s *Service) getMember(ctx context.Context, q *store.Queries, id uuid.UUID) (Member, error) {
	r, err := q.GetMember(ctx, id)
	if err != nil {
		return Member{}, err
	}
	m := Member{ID: r.ID, InvitedEmail: r.InvitedEmail, Roles: r.Roles, Status: r.Status, AcceptedAt: r.AcceptedAt, Version: r.Version}
	if r.UserID != nil && r.UserEmail != nil {
		m.User = &MemberUser{ID: *r.UserID, Email: *r.UserEmail, DisplayName: deref(r.UserDisplayName)}
	}
	return m, nil
}

// ChangeRoles (U4): roles absolut (bukan tambah/kurang), If-Match wajib. Hanya membership ACTIVE;
// undangan INVITED diubah lewat undang ulang (U2).
func (s *Service) ChangeRoles(ctx context.Context, p auth.Principal, id uuid.UUID, pre Precondition, in []string, meta reqmeta.Meta) (Member, error) {
	roles, err := normalizeRoles(in)
	if err != nil {
		return Member{}, err
	}
	tid, _ := tenantctx.From(ctx)
	var out Member
	err = db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		actor, target, err := lockAndAuthorize(ctx, q, tid, p, id)
		if err != nil {
			return err
		}
		if !slices.Contains(actor, auth.RoleOwner) && (hasAdmin(target.Roles) || hasAdmin(roles)) {
			return apperr.ErrForbidden.WithDetail("HR_ADMIN can only grant or remove MANAGER/EMPLOYEE on members without admin roles")
		}
		if pre.Err != nil {
			return pre.Err
		}
		if target.Version != pre.Version {
			return apperr.ErrVersionConflict
		}
		if target.Status != "ACTIVE" {
			return apperr.ErrInvalidState.WithDetail("Only ACTIVE members can change roles; re-invite a pending invitation instead")
		}
		if slices.Equal(target.Roles, roles) {
			out, err = s.getMember(ctx, q, id)
			return err // tidak ada perubahan: tidak ada audit, versi tetap
		}
		if err := q.SetMembershipRoles(ctx, store.SetMembershipRolesParams{Roles: roles, Actor: &p.UserID, ID: id}); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: "membership.roles_changed",
			TargetType: "membership", TargetID: id, Diff: map[string]any{"from": target.Roles, "to": roles}}, meta); err != nil {
			return err
		}
		// HR_ADMIN + PAYROLL_ADMIN pada satu orang menghapus pemisahan tugas: V1 mengizinkan, tetapi
		// tercatat (file 17 §3.1). Hanya saat kombinasi itu baru terbentuk.
		sod := func(rs []string) bool {
			return slices.Contains(rs, auth.RoleHRAdmin) && slices.Contains(rs, auth.RolePayroll)
		}
		if sod(roles) && !sod(target.Roles) {
			if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: "membership.sod_conflict",
				TargetType: "membership", TargetID: id, Diff: map[string]any{"roles": roles}}, meta); err != nil {
				return err
			}
		}
		out, err = s.getMember(ctx, q, id)
		return err
	})
	if err != nil {
		return Member{}, db.MapError(err) // P0004 saat COMMIT → 409 LAST_OWNER
	}
	return out, nil
}

// Revoke (U5): ACTIVE → REVOKED (akses hilang di refresh berikutnya; operasi sensitif memeriksa DB),
// INVITED → REVOKED (link undangan dimatikan). Sudah REVOKED → 409. Self-revoke boleh kecuali owner
// terakhir.
func (s *Service) Revoke(ctx context.Context, p auth.Principal, id uuid.UUID, reason string, meta reqmeta.Meta) (Member, error) {
	reason = strings.TrimSpace(reason)
	if n := utf8.RuneCountInString(reason); n < 3 || n > 200 || strings.ContainsRune(reason, 0) {
		return Member{}, apperr.Validation(apperr.FieldError{Field: "reason", Code: "LENGTH", Message: "must be 3..200 characters"})
	}
	tid, _ := tenantctx.From(ctx)
	now := s.clock.Now()
	var out Member
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		actor, target, err := lockAndAuthorize(ctx, q, tid, p, id)
		if err != nil {
			return err
		}
		if !slices.Contains(actor, auth.RoleOwner) && hasAdmin(target.Roles) {
			return apperr.ErrForbidden.WithDetail("HR_ADMIN can only revoke members without admin roles")
		}
		if target.Status == "REVOKED" {
			return apperr.ErrInvalidState.WithDetail("Membership is already revoked")
		}
		if err := q.RevokeMembership(ctx, store.RevokeMembershipParams{Now: &now, Actor: &p.UserID, ID: id}); err != nil {
			return err
		}
		if target.Status == "INVITED" {
			if err := q.RevokePendingInvitations(ctx, store.RevokePendingInvitationsParams{Now: &now, MembershipID: id}); err != nil {
				return err
			}
		}
		if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: "membership.revoked",
			TargetType: "membership", TargetID: id, Reason: reason, Diff: map[string]any{"from_status": target.Status, "roles": target.Roles}}, meta); err != nil {
			return err
		}
		out, err = s.getMember(ctx, q, id)
		return err
	})
	if err != nil {
		return Member{}, db.MapError(err)
	}
	return out, nil
}
