// Package iam: login, sesi (refresh token berotasi), profil, dan pemilihan tenant (PF-02, ADR-009,
// ADR-P02). Satu-satunya modul (bersama tenancy) yang memakai jalur platform db.WithPlatformTx.
//
// Mode transaksi (13-db-rls §2.4): users/memberships/tenants dibaca di mode platform (app.user_id);
// refresh_tokens tanpa RLS (lookup berdasarkan hash 256-bit); audit tenant ditulis di mode tenant.
package iam

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/internal/modules/iam/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/db"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/mail"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/observability"
	"payrun/internal/platform/reqmeta"
)

type Config struct {
	Argon2           auth.Argon2Params
	RefreshTTL       time.Duration
	RefreshFamilyTTL time.Duration
	// HashConcurrency: hash argon2 paralel maksimum (C-45). 0 → 4 (desain: VM 2 vCPU / 4 GiB).
	HashConcurrency int
	// PublicBaseURL: basis link di email (verifikasi), contoh https://payrun.example.com.
	PublicBaseURL string
	// VerificationTTL: masa berlaku link verifikasi email. 0 → 24 jam.
	VerificationTTL time.Duration
}

// Kebijakan sesi dan lockout (ADR-009 + catatan implementasi, C-03, C-05).
const (
	refreshGrace     = 10 * time.Second // refresh paralel dari dua tab tidak dianggap pencurian
	lockoutThreshold = 5                // gagal ke-5 mulai mengunci
	lockoutBase      = time.Minute      // lalu berlipat: 1, 2, 4, 8 menit …
	lockoutMax       = 15 * time.Minute // … maksimal 15 menit
	hashMaxWait      = 2 * time.Second  // antre slot argon2 maksimal 2 dtk, lalu 503
)

type Service struct {
	pool      *pgxpool.Pool
	tokens    *auth.Tokens
	clock     clock.Clock
	ids       idgen.Generator
	cfg       Config
	mailer    mail.Sender
	hasher    *auth.Hasher
	dummyHash string
	// onEmailVerified dipanggil di transaksi verifikasi (mode platform user). Diisi composition root
	// dengan aktivasi tenant PENDING_VERIFICATION milik user (modul tenancy; ADR-001: iam tidak
	// meng-import tenancy).
	onEmailVerified func(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error
}

func NewService(pool *pgxpool.Pool, tokens *auth.Tokens, c clock.Clock, ids idgen.Generator, cfg Config, mailer mail.Sender) *Service {
	n := cfg.HashConcurrency
	if n <= 0 {
		n = 4
	}
	if cfg.VerificationTTL <= 0 {
		cfg.VerificationTTL = 24 * time.Hour
	}
	return &Service{pool: pool, tokens: tokens, clock: c, ids: ids, cfg: cfg, mailer: mailer,
		hasher: auth.NewHasher(cfg.Argon2, n, hashMaxWait), dummyHash: auth.DummyHash(cfg.Argon2)}
}

// OnEmailVerified memasang hook aktivasi (lihat field onEmailVerified).
func (s *Service) OnEmailVerified(fn func(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error) {
	s.onEmailVerified = fn
}

// User adalah representasi user yang aman dikirim ke client (tanpa password_hash).
type User struct {
	ID            uuid.UUID `json:"id"`
	Email         string    `json:"email"`
	DisplayName   string    `json:"display_name"`
	EmailVerified bool      `json:"email_verified"`
}

func userFrom(u store.GetUserRow) User {
	return User{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName, EmailVerified: u.EmailVerifiedAt != nil}
}

// ActiveTenant adalah tenant yang menjadi `tid` token, dengan role user di dalamnya.
type ActiveTenant struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Roles []string  `json:"roles"`
}

// Session adalah hasil login/refresh/signup: access token (body) + refresh token mentah (cookie
// HttpOnly; tidak pernah disimpan mentah di DB). RefreshToken kosong = grace window (lihat Refresh).
type Session struct {
	AccessToken     string
	AccessExpiresIn time.Duration // dihitung dengan clock service → konsisten di test
	RefreshToken    string
	RefreshMaxAge   time.Duration
	User            User
	ActiveTenant    *ActiveTenant
}

type LoginInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Credentials adalah hasil VerifyCredentials: user yang password-nya cocok.
type Credentials struct {
	UserID uuid.UUID
}

// VerifyCredentials memeriksa email + password. Semua kegagalan (email tidak ada, password salah,
// akun tidak aktif, akun terkunci) menghasilkan error yang SAMA PERSIS (401 INVALID_CREDENTIALS, tanpa
// Retry-After: header itu sendiri membocorkan akun yang terkunci; Step 12 R2-5 + catatan LedgerPay),
// dan argon2 SELALU dijalankan (dummy hash untuk email tak dikenal) supaya waktu respons juga tidak
// membocorkan email terdaftar (T-SH-01, C-03). Gagal password menaikkan counter lockout.
// Dipakai login dan signup dengan akun yang sudah ada.
func (s *Service) VerifyCredentials(ctx context.Context, email, password string, meta reqmeta.Meta) (Credentials, error) {
	row, found, err := authLookup(ctx, s.pool, email)
	if err != nil {
		return Credentials{}, err
	}
	if !found {
		if _, err := s.hasher.Verify(ctx, password, s.dummyHash); err != nil {
			return Credentials{}, err
		}
		metrics.AuthnFailures.WithLabelValues("invalid_credentials").Inc()
		observability.Logger(ctx).Info("auth.login_failed", "reason", "unknown_email")
		return Credentials{}, apperr.ErrInvalidCredentials
	}
	now := s.clock.Now()
	locked := row.LockedUntil != nil && row.LockedUntil.After(now)
	ok, err := s.hasher.Verify(ctx, password, row.PasswordHash) // tetap dijalankan saat terkunci (timing)
	if err != nil {
		return Credentials{}, err
	}
	if locked || !ok || row.Status != "ACTIVE" {
		reason, metric := "bad_password", "invalid_credentials"
		switch {
		case locked:
			reason, metric = "locked", "locked" // percobaan selama terkunci tidak memperpanjang lock
		case row.Status != "ACTIVE":
			reason = "inactive"
		}
		metrics.AuthnFailures.WithLabelValues(metric).Inc()
		if err := s.recordFailure(ctx, row.ID, reason, !locked && !ok, now, meta); err != nil {
			return Credentials{}, err
		}
		return Credentials{}, apperr.ErrInvalidCredentials
	}
	return Credentials{UserID: row.ID}, nil
}

// Login: kredensial → sesi baru. active_tenant = satu-satunya membership ACTIVE (tenant ACTIVE) kalau
// user hanya punya satu; selain itu null dan token tanpa `tid` (client memanggil switch-tenant).
func (s *Service) Login(ctx context.Context, in LoginInput, meta reqmeta.Meta) (Session, error) {
	cred, err := s.VerifyCredentials(ctx, in.Email, in.Password, meta)
	if err != nil {
		return Session{}, err
	}
	return s.StartSession(ctx, cred.UserID, uuid.Nil, "auth.login_succeeded", meta)
}

// StartSession menerbitkan refresh token family baru + access token untuk user yang sudah
// terautentikasi (login, atau signup yang baru saja membuat tenant). preferTenant != Nil → tenant itu
// (membership tetap dicek); Nil → pilihan otomatis Login.
func (s *Service) StartSession(ctx context.Context, userID, preferTenant uuid.UUID, action string, meta reqmeta.Meta) (Session, error) {
	rawRefresh, refreshHash, err := newRefreshToken()
	if err != nil {
		return Session{}, err
	}
	now := s.clock.Now()
	sess := Session{RefreshToken: rawRefresh, RefreshMaxAge: s.cfg.RefreshTTL}
	err = db.WithPlatformTx(ctx, s.pool, userID, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.RecordLoginSuccess(ctx, store.RecordLoginSuccessParams{ID: userID, Now: &now}); err != nil {
			return err
		}
		u, err := q.GetUser(ctx, userID)
		if err != nil {
			return err
		}
		sess.User = userFrom(u)
		if sess.ActiveTenant, err = s.pickTenant(ctx, q, userID, preferTenant); err != nil {
			return err
		}
		if err := q.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
			ID: s.ids.New(), UserID: userID, FamilyID: s.ids.New(), TokenHash: refreshHash,
			IssuedAt: now, ExpiresAt: now.Add(s.cfg.RefreshTTL), FamilyExpiresAt: now.Add(s.cfg.RefreshFamilyTTL),
			UserAgent: nonEmpty(truncate(meta.UserAgent, 512)), Ip: meta.IP,
		}); err != nil {
			return err
		}
		return s.audit(ctx, q, nil, userID, action, "", meta)
	})
	if err != nil {
		return Session{}, db.MapError(err)
	}
	return s.signAccess(sess, userID, now)
}

// pickTenant (mode platform): tenant yang diminta kalau membership-nya masih ACTIVE dan tenant ACTIVE,
// atau satu-satunya membership aktif kalau tidak ada permintaan. nil = token tanpa `tid`.
func (s *Service) pickTenant(ctx context.Context, q *store.Queries, userID, prefer uuid.UUID) (*ActiveTenant, error) {
	if prefer != uuid.Nil {
		m, err := q.GetActiveMembership(ctx, store.GetActiveMembershipParams{UserID: userID, TenantID: prefer})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.TenantStatus != "ACTIVE") {
			return nil, nil // dicabut / tenant disuspend: tetap login, tanpa tid
		}
		if err != nil {
			return nil, err
		}
		return &ActiveTenant{ID: m.TenantID, Name: m.TenantName, Roles: m.Roles}, nil
	}
	ms, err := q.ListActiveMemberships(ctx, userID)
	if err != nil {
		return nil, err
	}
	var active []store.ListActiveMembershipsRow
	for _, m := range ms {
		if m.TenantStatus == "ACTIVE" {
			active = append(active, m)
		}
	}
	if len(active) != 1 {
		return nil, nil
	}
	return &ActiveTenant{ID: active[0].TenantID, Name: active[0].TenantName, Roles: active[0].Roles}, nil
}

func (s *Service) signAccess(sess Session, userID uuid.UUID, now time.Time) (Session, error) {
	tid, roles := uuid.Nil, []string(nil)
	if sess.ActiveTenant != nil {
		tid, roles = sess.ActiveTenant.ID, sess.ActiveTenant.Roles
	}
	token, exp, err := s.tokens.Issue(userID, tid, roles)
	sess.AccessToken, sess.AccessExpiresIn = token, exp.Sub(now)
	return sess, err
}

// recordFailure mencatat login gagal (audit platform) dan, untuk password salah, menaikkan counter
// lockout. Mode platform untuk user tersebut: UPDATE users hanya lolos lewat policy users_self.
func (s *Service) recordFailure(ctx context.Context, userID uuid.UUID, reason string, count bool, now time.Time, meta reqmeta.Meta) error {
	return db.MapError(db.WithPlatformTx(ctx, s.pool, userID, func(tx pgx.Tx) error {
		q := store.New(tx)
		if count {
			row, err := q.RecordLoginFailure(ctx, store.RecordLoginFailureParams{
				Threshold: lockoutThreshold, Now: now, BaseSecs: lockoutBase.Seconds(), MaxSecs: lockoutMax.Seconds(), ID: userID,
			})
			if err != nil {
				return err
			}
			if row.LockedUntil != nil && row.LockedUntil.After(now) {
				reason = "locked_now"
				metrics.AuthnLockouts.WithLabelValues().Inc()
				observability.Logger(ctx).Warn("auth.account_locked", "user_id", userID.String(),
					"failed_count", row.FailedLoginCount, "locked_until", row.LockedUntil)
			}
		}
		return s.audit(ctx, q, nil, userID, "auth.login_failed", reason, meta)
	}))
}

// Refresh menukar refresh token dengan access token baru + refresh token baru (rotasi, C-05).
//
//   - token belum dipakai → tandai used, terbitkan penerus di family yang sama;
//   - token sudah dipakai ≤ 10 dtk lalu → refresh paralel dari tab lain: access token saja, TANPA
//     refresh token baru (penerus sudah di cookie jar browser dari respons pertama);
//   - token sudah dipakai > 10 dtk lalu → REUSE: seluruh family dicabut, audit, 401 REFRESH_TOKEN_REUSED.
//
// activeTenant (header X-Active-Tenant): `tid` dipertahankan HANYA kalau membership masih ACTIVE, dan
// roles dibaca ulang dari DB. Inilah yang membatasi akses setelah membership dicabut ≤ 15 menit (ADR-P02).
func (s *Service) Refresh(ctx context.Context, raw string, activeTenant uuid.UUID, meta reqmeta.Meta) (Session, error) {
	if raw == "" {
		return Session{}, apperr.ErrUnauthenticated
	}
	sum := sha256.Sum256([]byte(raw))
	var (
		sess   Session
		reused bool
		userID uuid.UUID
	)
	now := s.clock.Now()
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		sess, reused = Session{}, false
		q := store.New(tx)
		t, err := q.GetRefreshTokenForUpdate(ctx, sum[:])
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.ErrUnauthenticated
		}
		if err != nil {
			return err
		}
		userID = t.UserID
		if t.RevokedAt != nil {
			return apperr.ErrUnauthenticated
		}
		if t.UsedAt != nil && now.Sub(*t.UsedAt) > refreshGrace {
			reused = true
			if _, err := q.RevokeFamily(ctx, store.RevokeFamilyParams{Now: &now, Reason: ptr("REUSE_DETECTED"), FamilyID: t.FamilyID}); err != nil {
				return err
			}
			observability.Logger(ctx).Warn("auth.refresh_reuse_detected", "user_id", t.UserID.String(), "family_id", t.FamilyID.String())
			return s.audit(ctx, q, nil, t.UserID, "auth.refresh_reuse_detected", "family revoked", meta)
		}
		if t.UsedAt == nil && (!now.Before(t.ExpiresAt) || !now.Before(t.FamilyExpiresAt)) {
			return apperr.ErrUnauthenticated
		}
		// Sejak sini: data user (mode platform untuk pemilik token, transaksi yang sama).
		if err := db.SetPlatformUser(ctx, tx, t.UserID); err != nil {
			return err
		}
		u, err := q.GetUser(ctx, t.UserID)
		if err != nil {
			return err
		}
		if u.Status != "ACTIVE" {
			return apperr.ErrUnauthenticated
		}
		sess.User = userFrom(u)
		if activeTenant != uuid.Nil {
			if sess.ActiveTenant, err = s.pickTenant(ctx, q, t.UserID, activeTenant); err != nil {
				return err
			}
		}
		if t.UsedAt != nil {
			return nil // grace: access token saja
		}
		rawNew, hashNew, err := newRefreshToken()
		if err != nil {
			return err
		}
		newID := s.ids.New()
		expires := now.Add(s.cfg.RefreshTTL) // sliding 7 hari …
		if expires.After(t.FamilyExpiresAt) {
			expires = t.FamilyExpiresAt // … tetapi tidak melewati batas absolut family (30 hari sejak login)
		}
		if err := q.MarkRefreshTokenUsed(ctx, store.MarkRefreshTokenUsedParams{Now: &now, ReplacedBy: &newID, ID: t.ID}); err != nil {
			return err
		}
		if err := q.CreateRefreshToken(ctx, store.CreateRefreshTokenParams{
			ID: newID, UserID: t.UserID, FamilyID: t.FamilyID, TokenHash: hashNew, ParentID: &t.ID,
			IssuedAt: now, ExpiresAt: expires, FamilyExpiresAt: t.FamilyExpiresAt,
			UserAgent: nonEmpty(truncate(meta.UserAgent, 512)), Ip: meta.IP,
		}); err != nil {
			return err
		}
		sess.RefreshToken, sess.RefreshMaxAge = rawNew, expires.Sub(now)
		return nil
	})
	if err != nil {
		return Session{}, db.MapError(err)
	}
	if reused {
		metrics.RefreshReuse.WithLabelValues().Inc()
		return Session{}, apperr.ErrRefreshReused
	}
	return s.signAccess(sess, userID, now)
}

// Logout mencabut seluruh family dari refresh token. Selalu "berhasil": token tak dikenal atau sudah
// dicabut tidak menghasilkan error (natural idempotent).
func (s *Service) Logout(ctx context.Context, raw string, meta reqmeta.Meta) error {
	if raw == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(raw))
	now := s.clock.Now()
	return db.MapError(db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		fam, err := q.GetFamilyByTokenHash(ctx, sum[:])
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		n, err := q.RevokeFamily(ctx, store.RevokeFamilyParams{Now: &now, Reason: ptr("LOGOUT"), FamilyID: fam.FamilyID})
		if err != nil || n == 0 {
			return err
		}
		return s.audit(ctx, q, nil, fam.UserID, "auth.logged_out", "", meta)
	}))
}

// Me: profil + tenant aktif. active_tenant dibaca ulang dari DB (bukan hanya dari token), sehingga
// membership yang sudah dicabut terlihat null di sini walaupun token lama masih berlaku ≤ 15 menit.
func (s *Service) Me(ctx context.Context, p auth.Principal) (User, *ActiveTenant, error) {
	var (
		u      User
		active *ActiveTenant
	)
	err := db.WithPlatformTx(ctx, s.pool, p.UserID, func(tx pgx.Tx) error {
		q := store.New(tx)
		row, err := q.GetUser(ctx, p.UserID)
		if err != nil {
			return err
		}
		u = userFrom(row)
		if p.TenantID != uuid.Nil {
			active, err = s.pickTenant(ctx, q, p.UserID, p.TenantID)
		}
		return err
	})
	return u, active, db.MapError(err)
}

// Membership adalah satu baris GET /me/memberships.
type Membership struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	TenantName string    `json:"tenant_name"`
	Roles      []string  `json:"roles"`
	Status     string    `json:"status"`
}

// Memberships: daftar tenant milik user (mode platform; policy self). Tenant yang disuspend tidak
// ditampilkan karena tidak bisa dipilih.
func (s *Service) Memberships(ctx context.Context, userID uuid.UUID) ([]Membership, error) {
	out := []Membership{}
	err := db.WithPlatformTx(ctx, s.pool, userID, func(tx pgx.Tx) error {
		rows, err := store.New(tx).ListActiveMemberships(ctx, userID)
		for _, r := range rows {
			if r.TenantStatus == "ACTIVE" {
				out = append(out, Membership{TenantID: r.TenantID, TenantName: r.TenantName, Roles: r.Roles, Status: "ACTIVE"})
			}
		}
		return err
	})
	return out, db.MapError(err)
}

// SwitchTenant menerbitkan access token baru dengan `tid` = tenantID setelah membership ACTIVE dicek
// di DB. Bukan anggota / membership dicabut / tenant tidak ada → 404 (tidak dibedakan); tenant
// disuspend → 403. Refresh token tidak berubah (tidak terikat tenant). Audit di tenant tujuan.
func (s *Service) SwitchTenant(ctx context.Context, userID, tenantID uuid.UUID, meta reqmeta.Meta) (Session, error) {
	var sess Session
	now := s.clock.Now()
	err := db.WithPlatformTx(ctx, s.pool, userID, func(tx pgx.Tx) error {
		q := store.New(tx)
		m, err := q.GetActiveMembership(ctx, store.GetActiveMembershipParams{UserID: userID, TenantID: tenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		switch m.TenantStatus {
		case "ACTIVE":
		case "PENDING_VERIFICATION":
			return apperr.ErrForbidden.WithDetail("Email verification required to activate this tenant")
		default:
			return apperr.ErrForbidden.WithDetail("Tenant is suspended")
		}
		sess.ActiveTenant = &ActiveTenant{ID: m.TenantID, Name: m.TenantName, Roles: m.Roles}
		// Audit ditulis sebagai baris tenant (terlihat oleh owner tenant): pindah ke mode tenant.
		if err := db.SetTenant(ctx, tx, tenantID); err != nil {
			return err
		}
		return s.audit(ctx, q, &tenantID, userID, "session.tenant_switched", "", meta)
	})
	if err != nil {
		return Session{}, db.MapError(err)
	}
	return s.signAccess(sess, userID, now)
}

func (s *Service) audit(ctx context.Context, q *store.Queries, tenantID *uuid.UUID, userID uuid.UUID, action, reason string, meta reqmeta.Meta) error {
	target := "user"
	return q.InsertAudit(ctx, store.InsertAuditParams{
		ID: s.ids.New(), TenantID: tenantID, ActorType: "USER", ActorID: &userID, Action: action,
		TargetType: &target, TargetID: &userID, RequestID: nonEmpty(meta.RequestID),
		Ip: meta.IP, UserAgent: nonEmpty(truncate(meta.UserAgent, 512)), Reason: nonEmpty(reason),
	})
}

func ptr[T any](v T) *T { return &v }

// newRefreshToken: 256 bit acak (opaque). Yang disimpan hanya SHA-256-nya: hash cepat cukup karena
// token berentropi tinggi tidak bisa di-brute-force (beda dengan password; ADR-009).
func newRefreshToken() (raw string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", nil, err
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	return raw, sum[:], nil
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
