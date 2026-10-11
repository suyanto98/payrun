package iam

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/url"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/modules/iam/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/db"
	"payrun/internal/platform/mail"
	"payrun/internal/platform/observability"
	"payrun/internal/platform/reqmeta"
)

// Bagian iam yang dipakai signup (modul tenancy) lewat interface di composition root, dan alur
// verifikasi email (final review R2-8; keputusan S1: tenant PENDING_VERIFICATION sampai email diverifikasi).

// SignupIdentity menentukan pendaftar:
//   - email baru → exists=false (password belum diperiksa);
//   - akun terverifikasi → password harus cocok (salah → 401 INVALID_CREDENTIALS dengan aturan lockout
//     yang sama seperti login);
//   - akun BELUM terverifikasi → password cocok: pendaftar yang sama; tidak cocok: reclaim=true, signup
//     mengganti kredensial akun itu (ReclaimUserTx) alih-alih menolak dengan 401. Siapa pun
//     yang lebih dulu mendaftar dengan email orang lain tidak bisa mengunci pemilik inbox keluar, dan
//     verifikasi email mewajibkan password terkini (VerifyEmail), sehingga klik link oleh pemilik inbox
//     tidak pernah mengaktifkan akun dengan password orang lain.
//
// Akun non-ACTIVE (DISABLED/LOCKED oleh admin) tidak pernah di-reclaim.
func (s *Service) SignupIdentity(ctx context.Context, email, password string, meta reqmeta.Meta) (userID uuid.UUID, exists, verified, reclaim bool, err error) {
	row, found, err := authLookup(ctx, s.pool, email)
	if err != nil || !found {
		return uuid.Nil, false, false, false, err
	}
	err = db.WithPlatformTx(ctx, s.pool, row.ID, func(tx pgx.Tx) error {
		u, err := store.New(tx).GetUser(ctx, row.ID)
		verified = u.EmailVerifiedAt != nil
		return err
	})
	if err != nil {
		return uuid.Nil, true, false, false, db.MapError(err)
	}
	if !verified && row.Status == "ACTIVE" {
		ok, err := s.hasher.Verify(ctx, password, row.PasswordHash)
		if err != nil {
			return uuid.Nil, true, false, false, err
		}
		return row.ID, true, false, !ok, nil
	}
	cred, err := s.VerifyCredentials(ctx, email, password, meta)
	if err != nil {
		return uuid.Nil, true, false, false, err
	}
	return cred.UserID, true, verified, false, nil
}

// ReclaimUserTx mengganti kredensial akun yang emailnya belum terverifikasi (signup transaksi yang
// sama): password + nama baru, semua sesi dan link verifikasi lama dicabut, audit platform. Email
// ternyata sudah diverifikasi secara paralel → 401 INVALID_CREDENTIALS (sama seperti password salah).
func (s *Service) ReclaimUserTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, passwordHash, displayName string, meta reqmeta.Meta) error {
	if err := db.SetPlatformUser(ctx, tx, id); err != nil {
		return err
	}
	q := store.New(tx)
	n, err := q.ReclaimUnverifiedUser(ctx, store.ReclaimUnverifiedUserParams{PasswordHash: passwordHash, DisplayName: displayName, ID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.ErrInvalidCredentials
	}
	now := s.clock.Now()
	if err := q.RevokeUserRefreshTokens(ctx, store.RevokeUserRefreshTokensParams{Now: &now, Reason: ptr("PASSWORD_CHANGED"), UserID: id}); err != nil {
		return err
	}
	if err := q.InvalidatePendingVerifications(ctx, store.InvalidatePendingVerificationsParams{Now: &now, UserID: id}); err != nil {
		return err
	}
	observability.Logger(ctx).Warn("auth.unverified_account_reclaimed", "user_id", id.String())
	return s.audit(ctx, q, nil, id, "auth.unverified_account_reclaimed", "", meta)
}

// HashPassword lewat semaphore argon2 yang sama dengan login (C-45: batas berlaku untuk semua jalur).
func (s *Service) HashPassword(ctx context.Context, password string) (string, error) {
	return s.hasher.Hash(ctx, password)
}

// CreateUserTx membuat user di transaksi signup: berpindah ke mode platform untuk user baru, sehingga
// INSERT lolos WITH CHECK users_self. Email duplikat → 409 EMAIL_ALREADY_REGISTERED (unique index
// tidak difilter RLS).
func (s *Service) CreateUserTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, email, passwordHash, displayName string) error {
	if err := db.SetPlatformUser(ctx, tx, id); err != nil {
		return err
	}
	return store.New(tx).CreateUser(ctx, store.CreateUserParams{ID: id, Email: email, PasswordHash: passwordHash, DisplayName: displayName})
}

// AddOwnerTx membuat membership OWNER ACTIVE. Dipanggil saat transaksi sudah di mode tenant.
func (s *Service) AddOwnerTx(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) error {
	now := s.clock.Now()
	return store.New(tx).AddOwnerMembership(ctx, store.AddOwnerMembershipParams{TenantID: tenantID, ID: s.ids.New(), UserID: &userID, Now: &now})
}

// SendVerification menerbitkan link verifikasi baru (link lama dimatikan) dan mengirimnya. Dipanggil
// SETELAH transaksi signup commit: tidak ada panggilan jaringan di dalam transaksi (ADR-011). Gagal
// kirim → error dikembalikan ke pemanggil (signup hanya me-log-nya; user memakai "kirim ulang").
func (s *Service) SendVerification(ctx context.Context, userID uuid.UUID) error {
	raw, hash, err := newRefreshToken() // format yang sama: 256 bit acak, disimpan SHA-256-nya
	if err != nil {
		return err
	}
	now := s.clock.Now()
	var email string
	err = db.WithPlatformTx(ctx, s.pool, userID, func(tx pgx.Tx) error {
		q := store.New(tx)
		u, err := q.GetUser(ctx, userID)
		if err != nil {
			return err
		}
		if u.EmailVerifiedAt != nil {
			return errAlreadyVerified
		}
		email = u.Email
		if err := q.InvalidatePendingVerifications(ctx, store.InvalidatePendingVerificationsParams{Now: &now, UserID: userID}); err != nil {
			return err
		}
		return q.CreateEmailVerification(ctx, store.CreateEmailVerificationParams{
			ID: s.ids.New(), UserID: userID, TokenHash: hash, CreatedAt: now, ExpiresAt: now.Add(s.cfg.VerificationTTL),
		})
	})
	if errors.Is(err, errAlreadyVerified) {
		return nil
	}
	if err != nil {
		return db.MapError(err)
	}
	link := s.cfg.PublicBaseURL + "/verify-email?token=" + url.QueryEscape(raw)
	return s.mailer.Send(ctx, mail.Message{
		To:      email,
		Subject: "Verify your PayRun email",
		Text: "Open this link to verify your email and activate your company on PayRun:\n\n" + link +
			"\n\nThe link expires in " + s.cfg.VerificationTTL.String() + ". If you did not sign up, ignore this email.\n",
	})
}

var errAlreadyVerified = errors.New("iam: email already verified")

// ResendVerification (POST /auth/resend-verification): user yang login meminta link baru. Email yang
// sudah terverifikasi → no-op.
func (s *Service) ResendVerification(ctx context.Context, userID uuid.UUID) error {
	if err := s.SendVerification(ctx, userID); err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			return err
		}
		observability.Logger(ctx).Error("mail.verification_failed", "user_id", userID.String(), "err", err.Error())
		return apperr.ErrUnavailable.WithDetail("Could not send the email, retry shortly").WithCause(err)
	}
	return nil
}

// VerifyEmail (POST /auth/verify-email, tanpa auth: link di email). Body memuat token DAN password
// akun saat ini (review S1): pemilik inbox yang mengklik link dari signup orang lain tidak bisa
// mengaktifkan akun dengan password orang itu. Urutan: token dicari tanpa lock → password diperiksa
// (VerifyCredentials: argon2, lockout, 401 yang sama dengan login; token TIDAK terpakai) → satu
// transaksi: token dikunci, email user ditandai terverifikasi, token dipakai, tenant
// PENDING_VERIFICATION milik user diaktifkan (hook tenancy), audit. Token tidak dikenal, kedaluwarsa,
// atau sudah dipakai → 410 satu code.
func (s *Service) VerifyEmail(ctx context.Context, rawToken, password string, meta reqmeta.Meta) error {
	if rawToken == "" || len(rawToken) > 128 {
		return apperr.ErrEmailVerificationInvalid
	}
	sum := sha256.Sum256([]byte(rawToken))
	tokenUser, err := store.New(s.pool).GetEmailVerification(ctx, sum[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.ErrEmailVerificationInvalid
	}
	if err != nil {
		return db.MapError(err)
	}
	var email string
	if err := db.WithPlatformTx(ctx, s.pool, tokenUser, func(tx pgx.Tx) error {
		u, err := store.New(tx).GetUser(ctx, tokenUser)
		email = u.Email
		return err
	}); err != nil {
		return db.MapError(err)
	}
	cred, err := s.VerifyCredentials(ctx, email, password, meta)
	if err != nil {
		return err
	}
	if cred.UserID != tokenUser {
		return apperr.ErrEmailVerificationInvalid
	}
	now := s.clock.Now()
	return db.MapError(db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		v, err := q.GetEmailVerificationForUpdate(ctx, sum[:])
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.ErrEmailVerificationInvalid
		}
		if err != nil {
			return err
		}
		if v.UsedAt != nil || !now.Before(v.ExpiresAt) || v.UserID != tokenUser {
			return apperr.ErrEmailVerificationInvalid
		}
		if err := q.MarkEmailVerificationUsed(ctx, store.MarkEmailVerificationUsedParams{Now: &now, ID: v.ID}); err != nil {
			return err
		}
		if err := db.SetPlatformUser(ctx, tx, v.UserID); err != nil {
			return err
		}
		if _, err := q.MarkEmailVerified(ctx, store.MarkEmailVerifiedParams{Now: &now, ID: v.UserID}); err != nil {
			return err
		}
		if err := s.audit(ctx, q, nil, v.UserID, "auth.email_verified", "", meta); err != nil {
			return err
		}
		if s.onEmailVerified != nil {
			return s.onEmailVerified(ctx, tx, v.UserID)
		}
		return nil
	}))
}
