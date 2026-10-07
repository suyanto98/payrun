package iam

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/modules/iam/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/audit"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/db"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tenantctx"
)

// Undangan member (PF-02, U1–U3). Token "<tenant_id>.<secret>": hanya SHA-256 secret yang disimpan,
// link ditampilkan sekali di respons (MVP tanpa email undangan). Accept wajib: login, email
// terverifikasi (R2-8), email = email undangan, token belum dipakai/kedaluwarsa. Semua kegagalan token
// dan email-tidak-cocok → satu code 410 INVITATION_INVALID (tidak bisa dipakai menebak).

const invitationTTL = 7 * 24 * time.Hour

var allRoles = []string{auth.RoleOwner, auth.RoleHRAdmin, auth.RolePayroll, auth.RoleManager, auth.RoleEmployee}

// EmployeeLinker: tautan user ↔ employee milik modul employee, dipasang composition root (ADR-001).
type EmployeeLinker interface {
	CheckLinkableTx(ctx context.Context, tx pgx.Tx, employeeID uuid.UUID) error
	LinkUserTx(ctx context.Context, tx pgx.Tx, employeeID, userID uuid.UUID) error
	// SelfID: employee milik user di tenant aktif (GET /me), nil kalau tidak ada / tanpa tenant.
	SelfID(ctx context.Context, userID uuid.UUID) (*uuid.UUID, error)
}

// SetEmployeeLinker memasang implementasi EmployeeLinker.
func (s *Service) SetEmployeeLinker(l EmployeeLinker) { s.linker = l }

type InviteInput struct {
	Email      string     `json:"email"`
	Roles      []string   `json:"roles"`
	EmployeeID *uuid.UUID `json:"employee_id"`
}

type Invitation struct {
	MembershipID uuid.UUID `json:"membership_id"`
	Status       string    `json:"status"`
	InviteURL    string    `json:"invite_url"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Invite (U2). OWNER boleh semua role; HR_ADMIN hanya MANAGER/EMPLOYEE (file 17 §2.1 ᴿ).
func (s *Service) Invite(ctx context.Context, p auth.Principal, in InviteInput, meta reqmeta.Meta) (Invitation, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	var fe []apperr.FieldError
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email || len(email) > 254 {
		fe = append(fe, apperr.FieldError{Field: "email", Code: "FORMAT"})
	}
	roles := slices.Clone(in.Roles)
	slices.Sort(roles)
	roles = slices.Compact(roles)
	if len(roles) == 0 {
		fe = append(fe, apperr.FieldError{Field: "roles", Code: "REQUIRED", Message: "at least one role"})
	}
	for _, r := range roles {
		if !slices.Contains(allRoles, r) {
			fe = append(fe, apperr.FieldError{Field: "roles", Code: "INVALID_ENUM", Message: "unknown role " + r})
		}
	}
	if len(fe) > 0 {
		return Invitation{}, apperr.Validation(fe...)
	}
	if !p.HasRole(auth.RoleOwner) {
		for _, r := range roles {
			if r != auth.RoleManager && r != auth.RoleEmployee {
				return Invitation{}, apperr.ErrForbidden.WithDetail("HR_ADMIN can only invite MANAGER or EMPLOYEE")
			}
		}
	}
	tid, _ := tenantctx.From(ctx)
	secret, hash, err := newRefreshToken() // 256 bit acak; yang disimpan hanya SHA-256-nya
	if err != nil {
		return Invitation{}, err
	}
	now := s.clock.Now()
	inv := Invitation{MembershipID: s.ids.New(), Status: "INVITED", ExpiresAt: now.Add(invitationTTL),
		InviteURL: s.cfg.PublicBaseURL + "/invite#" + tid.String() + "." + secret}
	action := "membership.invited"
	err = db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		exists, err := q.ActiveMemberEmailExists(ctx, email)
		if err != nil {
			return err
		}
		if exists {
			return apperr.ErrMembershipExists
		}
		// Re-invite (review S2): undangan INVITED untuk email yang sama TIDAK menghalangi. Link lama
		// (kedaluwarsa, hilang, atau respons 201 tidak sampai) dimatikan dan diganti link baru pada
		// membership yang sama. HR_ADMIN tidak boleh menimpa undangan ber-role admin buatan OWNER.
		pending, err := q.GetPendingMembershipByEmail(ctx, email)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if err := q.CreateInvitedMembership(ctx, store.CreateInvitedMembershipParams{
				TenantID: tid, ID: inv.MembershipID, Email: email, Roles: roles, Actor: &p.UserID,
			}); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if !p.HasRole(auth.RoleOwner) {
				for _, r := range pending.Roles {
					if r != auth.RoleManager && r != auth.RoleEmployee {
						return apperr.ErrForbidden.WithDetail("HR_ADMIN cannot replace an invitation with admin roles")
					}
				}
			}
			inv.MembershipID, action = pending.ID, "membership.reinvited"
			if err := q.UpdateInvitedRoles(ctx, store.UpdateInvitedRolesParams{Roles: roles, Actor: &p.UserID, ID: pending.ID}); err != nil {
				return err
			}
			if err := q.RevokePendingInvitations(ctx, store.RevokePendingInvitationsParams{Now: &now, MembershipID: pending.ID}); err != nil {
				return err
			}
		}
		if in.EmployeeID != nil {
			if s.linker == nil {
				return errors.New("iam: employee linker not wired")
			}
			if err := s.linker.CheckLinkableTx(ctx, tx, *in.EmployeeID); err != nil {
				return err
			}
			// Satu employee hanya boleh dicadangkan satu undangan yang masih berlaku.
			reserved, err := q.EmployeeReservedByInvitation(ctx, store.EmployeeReservedByInvitationParams{
				EmployeeID: in.EmployeeID, MembershipID: inv.MembershipID, Now: now})
			if err != nil {
				return err
			}
			if reserved {
				return apperr.ErrInvalidState.WithDetail("Employee already has a pending invitation")
			}
		}
		invID := s.ids.New()
		if err := q.CreateInvitation(ctx, store.CreateInvitationParams{
			TenantID: tid, ID: invID, MembershipID: inv.MembershipID, EmployeeID: in.EmployeeID, TokenHash: hash,
			Email: email, ExpiresAt: inv.ExpiresAt, CreatedAt: now, Actor: &p.UserID,
		}); err != nil {
			return err
		}
		diff := map[string]any{"roles": roles}
		if in.EmployeeID != nil {
			diff["employee_id"] = in.EmployeeID.String()
		}
		return audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: p.UserID, Action: action,
			TargetType: "membership", TargetID: inv.MembershipID, Diff: diff}, meta)
	})
	if err != nil {
		return Invitation{}, db.MapError(err)
	}
	metrics.InvitationsTotal.WithLabelValues("created").Inc()
	return inv, nil
}

type AcceptedMembership struct {
	TenantID uuid.UUID `json:"tenant_id"`
	Roles    []string  `json:"roles"`
	Status   string    `json:"status"`
}

// parseInviteToken: "<tenant_id>.<secret base64url 32 byte>". Apa pun yang lain → 410.
func parseInviteToken(token string) (uuid.UUID, []byte, error) {
	tidRaw, secret, ok := strings.Cut(strings.TrimSpace(token), ".")
	tid, err := uuid.Parse(tidRaw)
	if !ok || err != nil || tid == uuid.Nil || len(secret) != 43 {
		return uuid.Nil, nil, apperr.ErrInvitationInvalid
	}
	sum := sha256.Sum256([]byte(secret))
	return tid, sum[:], nil
}

// AcceptInvitation (U3). Tenant transaksi berasal dari token (bukan dari `tid` pemanggil), dan baris
// undangan dicari berdasarkan hash secret di bawah RLS tenant itu (12-db-schema §2.3).
func (s *Service) AcceptInvitation(ctx context.Context, userID uuid.UUID, token string, meta reqmeta.Meta) (AcceptedMembership, error) {
	tid, hash, err := parseInviteToken(token)
	if err != nil {
		return AcceptedMembership{}, err
	}
	var email string
	var verified bool
	if err := db.WithPlatformTx(ctx, s.pool, userID, func(tx pgx.Tx) error {
		u, err := store.New(tx).GetUser(ctx, userID)
		email, verified = u.Email, u.EmailVerifiedAt != nil
		return err
	}); err != nil {
		return AcceptedMembership{}, db.MapError(err)
	}
	if !verified {
		return AcceptedMembership{}, apperr.ErrForbidden.WithDetail("Verify your email before accepting invitations")
	}
	now := s.clock.Now()
	var out AcceptedMembership
	err = db.WithTenantTxFor(ctx, s.pool, tid, func(tx pgx.Tx) error {
		q := store.New(tx)
		inv, err := q.GetInvitationForUpdate(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.ErrInvitationInvalid
		}
		if err != nil {
			return err
		}
		if err := db.GuardTenantFor(ctx, tid, "invitations", inv.TenantID); err != nil {
			return err
		}
		// Email dibandingkan byte-per-byte setelah lowercase (keduanya disimpan lowercase), BUKAN
		// strings.EqualFold yang memakai Unicode folding (contoh tanda Kelvin "K" = "k").
		if inv.AcceptedAt != nil || inv.RevokedAt != nil || !now.Before(inv.ExpiresAt) || inv.Email != strings.ToLower(email) {
			return apperr.ErrInvitationInvalid
		}
		if st, err := q.GetTenantStatus(ctx, tid); err != nil || st != "ACTIVE" {
			return apperr.ErrInvitationInvalid // tenant belum aktif / ditangguhkan: undangan tidak bisa dipakai
		}
		m, err := q.AcceptMembership(ctx, store.AcceptMembershipParams{UserID: &userID, Now: &now, ID: inv.MembershipID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.ErrInvitationInvalid
		}
		if err != nil {
			return err // uq_memberships_user → 409 MEMBERSHIP_EXISTS
		}
		if err := q.MarkInvitationAccepted(ctx, store.MarkInvitationAcceptedParams{Now: &now, ID: inv.ID}); err != nil {
			return err
		}
		if inv.EmployeeID != nil {
			if s.linker == nil {
				return errors.New("iam: employee linker not wired")
			}
			if err := s.linker.LinkUserTx(ctx, tx, *inv.EmployeeID, userID); err != nil {
				return err
			}
		}
		out = AcceptedMembership{TenantID: m.TenantID, Roles: m.Roles, Status: m.Status}
		return audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: userID, Action: "membership.accepted",
			TargetType: "membership", TargetID: inv.MembershipID}, meta)
	})
	if err != nil {
		return AcceptedMembership{}, db.MapError(err)
	}
	metrics.InvitationsTotal.WithLabelValues("accepted").Inc()
	return out, nil
}

// ---------------------------------------------------------------- U1 GET /members

type MemberUser struct {
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
}

type Member struct {
	ID           uuid.UUID   `json:"id"`
	User         *MemberUser `json:"user"`
	InvitedEmail *string     `json:"invited_email"`
	Roles        []string    `json:"roles"`
	Status       string      `json:"status"`
	AcceptedAt   *time.Time  `json:"accepted_at"`
	Version      int32       `json:"version"`
}

type MemberQuery struct {
	Status, Role, Cursor string
	Limit                int32
}

func (s *Service) ListMembers(ctx context.Context, mq MemberQuery) ([]Member, httpx.Page, error) {
	page := httpx.Page{}
	var fe []apperr.FieldError
	if mq.Status != "" && mq.Status != "ACTIVE" && mq.Status != "INVITED" && mq.Status != "REVOKED" {
		fe = append(fe, apperr.FieldError{Field: "status", Code: "INVALID_ENUM"})
	}
	if mq.Role != "" && !slices.Contains(allRoles, mq.Role) {
		fe = append(fe, apperr.FieldError{Field: "role", Code: "INVALID_ENUM"})
	}
	if len(fe) > 0 {
		return nil, page, apperr.Validation(fe...)
	}
	params := store.ListMembersParams{Lim: mq.Limit + 1}
	if mq.Status != "" {
		params.Status = &mq.Status
	}
	if mq.Role != "" {
		params.Role = &mq.Role
	}
	if mq.Cursor != "" {
		c, err := httpx.DecodeCursor(mq.Cursor)
		if err != nil {
			return nil, page, err
		}
		params.AfterAt, params.AfterID = &c.At, &c.ID
	}
	out := []Member{}
	var lastAt []time.Time
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		rs, err := store.New(tx).ListMembers(ctx, params)
		if err != nil {
			return err
		}
		for _, r := range rs {
			if err := db.GuardTenant(ctx, "memberships", r.TenantID); err != nil {
				return err
			}
			m := Member{ID: r.ID, InvitedEmail: r.InvitedEmail, Roles: r.Roles, Status: r.Status, AcceptedAt: r.AcceptedAt, Version: r.Version}
			if r.UserID != nil && r.UserEmail != nil {
				m.User = &MemberUser{ID: *r.UserID, Email: *r.UserEmail, DisplayName: deref(r.UserDisplayName)}
			}
			out = append(out, m)
			lastAt = append(lastAt, r.CreatedAt)
		}
		return nil
	})
	if err != nil {
		return nil, page, db.MapError(err)
	}
	if int32(len(out)) > mq.Limit { //nolint:gosec // G115: panjang ≤ limit+1
		out = out[:mq.Limit]
		c := httpx.EncodeCursor(httpx.Cursor{At: lastAt[mq.Limit-1], ID: out[len(out)-1].ID})
		page = httpx.Page{NextCursor: &c, HasMore: true}
	}
	return out, page, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
