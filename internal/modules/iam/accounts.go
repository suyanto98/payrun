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
// verifikasi email (Step 12 R2-8; keputusan S1: tenant PENDING_VERIFICATION sampai email diverifikasi).

// SignupIdentity menentukan pendaftar: email baru (exists=false, password belum diperiksa), atau
// akun yang sudah ada dengan password yang cocok (kredensial salah → 401 INVALID_CREDENTIALS dengan
// aturan lockout yang sama seperti login). verified = email akun sudah terverifikasi.
func (s *Service) SignupIdentity(ctx context.Context, email, password string, meta reqmeta.Meta) (userID uuid.UUID, exists, verified bool, err error) {
	row, found, err := authLookup(ctx, s.pool, email)
	if err != nil || !found {
		return uuid.Nil, false, false, err
	}
	cred, err := s.VerifyCredentials(ctx, email, password, meta)
	if err != nil {
		return uuid.Nil, true, false, err
	}
	err = db.WithPlatformTx(ctx, s.pool, cred.UserID, func(tx pgx.Tx) error {
		u, err := store.New(tx).GetUser(ctx, row.ID)
		verified = u.EmailVerifiedAt != nil
		return err
	})
	return cred.UserID, true, verified, db.MapError(err)
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

// VerifyEmail (POST /auth/verify-email, tanpa auth: link di email). Satu transaksi: token dikunci,
// email user ditandai terverifikasi, token dipakai, tenant PENDING_VERIFICATION milik user diaktifkan
// (hook tenancy), audit. Token tidak dikenal, kedaluwarsa, atau sudah dipakai → 410 satu code.
func (s *Service) VerifyEmail(ctx context.Context, rawToken string, meta reqmeta.Meta) error {
	if rawToken == "" || len(rawToken) > 128 {
		return apperr.ErrEmailVerificationInvalid
	}
	sum := sha256.Sum256([]byte(rawToken))
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
		if v.UsedAt != nil || !now.Before(v.ExpiresAt) {
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
