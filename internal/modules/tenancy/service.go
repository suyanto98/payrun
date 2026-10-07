// Package tenancy: signup tenant, profil + settings tenant, aktivasi tenant setelah verifikasi email
// (PF-01). Tabel milik modul ini: tenants, tenant_settings, platform_idempotency_keys. User dan
// membership milik iam, diakses lewat interface Accounts (ADR-001: modul tidak saling meng-import).
package tenancy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/internal/modules/tenancy/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/db"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/observability"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tenantctx"
)

// Accounts adalah bagian iam yang dibutuhkan signup. Implementasi: *iam.Service (composition root).
type Accounts interface {
	SignupIdentity(ctx context.Context, email, password string, meta reqmeta.Meta) (userID uuid.UUID, exists, verified bool, err error)
	HashPassword(ctx context.Context, password string) (string, error)
	CreateUserTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, email, passwordHash, displayName string) error
	AddOwnerTx(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) error
	SendVerification(ctx context.Context, userID uuid.UUID) error
}

type Service struct {
	pool     *pgxpool.Pool
	accounts Accounts
	clock    clock.Clock
	ids      idgen.Generator
}

func NewService(pool *pgxpool.Pool, accounts Accounts, c clock.Clock, ids idgen.Generator) *Service {
	return &Service{pool: pool, accounts: accounts, clock: c, ids: ids}
}

const (
	signupEndpoint = "POST /tenants/signup"
	idemTTL        = 24 * time.Hour // platform_idempotency_keys (10-db-shared-tables §3.3)
	defaultTZ      = "Asia/Jakarta"
)

type OwnerInput struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type SignupInput struct {
	CompanyName string     `json:"company_name"`
	Slug        string     `json:"slug"`
	Owner       OwnerInput `json:"owner"`
	Timezone    string     `json:"timezone"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9](-?[a-z0-9]){2,39}$`)

func (in *SignupInput) normalizeAndValidate() error {
	in.CompanyName = strings.TrimSpace(in.CompanyName)
	in.Owner.Email = strings.TrimSpace(in.Owner.Email)
	in.Owner.DisplayName = strings.TrimSpace(in.Owner.DisplayName)
	if in.Timezone == "" {
		in.Timezone = defaultTZ
	}
	var fe []apperr.FieldError
	if n := utf8.RuneCountInString(in.CompanyName); n < 2 || n > 120 {
		fe = append(fe, apperr.FieldError{Field: "company_name", Code: "LENGTH", Message: "2–120 characters"})
	}
	if !slugRe.MatchString(in.Slug) {
		fe = append(fe, apperr.FieldError{Field: "slug", Code: "INVALID_FORMAT", Message: "3–40 of a-z, 0-9 and single hyphens"})
	}
	if a, err := mail.ParseAddress(in.Owner.Email); err != nil || a.Address != in.Owner.Email || len(in.Owner.Email) > 254 {
		fe = append(fe, apperr.FieldError{Field: "owner.email", Code: "INVALID_FORMAT"})
	}
	if n := utf8.RuneCountInString(in.Owner.Password); n < 10 || n > 128 {
		fe = append(fe, apperr.FieldError{Field: "owner.password", Code: "LENGTH", Message: "10–128 characters"})
	}
	if n := utf8.RuneCountInString(in.Owner.DisplayName); n < 1 || n > 80 {
		fe = append(fe, apperr.FieldError{Field: "owner.display_name", Code: "LENGTH", Message: "1–80 characters"})
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil || in.Timezone == "Local" {
		fe = append(fe, apperr.FieldError{Field: "timezone", Code: "INVALID", Message: "IANA time zone name"})
	}
	if len(fe) > 0 {
		return apperr.Validation(fe...)
	}
	return nil
}

// requestHash: SHA-256 atas field yang menentukan "permintaan yang sama", TANPA password. Hash ini
// disimpan di DB; SHA-256 atas JSON berisi password bisa di-crack offline (hash cepat, bukan argon2).
// Email ikut (10-db-shared §3.3): key yang dipakai ulang untuk email lain → 422.
func (in SignupInput) requestHash() []byte {
	b, _ := json.Marshal([]string{in.CompanyName, in.Slug, strings.ToLower(in.Owner.Email), in.Owner.DisplayName, in.Timezone})
	sum := sha256.Sum256(b)
	return sum[:]
}

// TenantView adalah bagian tenant di respons signup (dan satu-satunya isi yang di-replay: tanpa token,
// tanpa PII; token didapat lewat login biasa).
type TenantView struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	Slug   string    `json:"slug"`
	Status string    `json:"status"`
}

type SignupBody struct {
	Tenant                    TenantView `json:"tenant"`
	EmailVerificationRequired bool       `json:"email_verification_required"`
}

type SignupResult struct {
	Body     SignupBody
	Replayed bool
	UserID   uuid.UUID
}

// Signup (T1): satu transaksi = user (jika baru) + tenant + settings + membership OWNER + audit, dengan
// idempotency di platform_idempotency_keys. Tenant ACTIVE hanya kalau email owner sudah terverifikasi;
// selain itu PENDING_VERIFICATION dan link verifikasi dikirim SETELAH commit (R2-8).
func (s *Service) Signup(ctx context.Context, key string, in SignupInput, meta reqmeta.Meta) (SignupResult, error) {
	if key == "" {
		return SignupResult{}, apperr.ErrIdempotencyKeyRequired
	}
	if len(key) < 8 || len(key) > 255 {
		return SignupResult{}, apperr.ErrIdempotencyKeyInvalid
	}
	if err := in.normalizeAndValidate(); err != nil {
		return SignupResult{}, err
	}
	reqHash := in.requestHash()
	// Cek awal TANPA transaksi: retry yang sudah selesai di-replay tanpa argon2 dan tanpa menyentuh akun.
	if res, done, err := s.replay(ctx, s.pool, key, reqHash); done || err != nil {
		return res, err
	}

	userID, exists, verified, err := s.accounts.SignupIdentity(ctx, in.Owner.Email, in.Owner.Password, meta)
	if err != nil {
		return SignupResult{}, err
	}
	var pwHash string
	if !exists {
		if pwHash, err = s.accounts.HashPassword(ctx, in.Owner.Password); err != nil { // di luar transaksi: ± 100 ms
			return SignupResult{}, err
		}
		userID = s.ids.New()
	}

	tenantID, now := s.ids.New(), s.clock.Now()
	status := "PENDING_VERIFICATION"
	if verified {
		status = "ACTIVE"
	}
	res := SignupResult{UserID: userID, Body: SignupBody{
		Tenant:                    TenantView{ID: tenantID, Name: in.CompanyName, Slug: in.Slug, Status: status},
		EmailVerificationRequired: !verified,
	}}
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		claimed, err := q.ClaimPlatformIdempotencyKey(ctx, store.ClaimPlatformIdempotencyKeyParams{
			ID: s.ids.New(), Endpoint: signupEndpoint, Key: key, RequestHash: reqHash, CreatedAt: now, ExpiresAt: now.Add(idemTTL),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// Request paralel dengan key yang sama selesai lebih dulu (kita menunggu di unique index).
			replayed, done, err := s.replay(ctx, tx, key, reqHash)
			if err == nil && !done {
				err = apperr.ErrRequestInProgress
			}
			res = replayed
			return errReplayed{err}
		}
		if err != nil {
			return err
		}
		if !exists { // fase platform: user baru (app.user_id = user baru)
			if err := s.accounts.CreateUserTx(ctx, tx, userID, in.Owner.Email, pwHash, in.Owner.DisplayName); err != nil {
				return err
			}
		}
		// Fase tenant (13-db-rls §2.3, Step 12 C2): app.tenant_id = tenant baru, app.user_id kosong.
		if err := db.SetTenant(ctx, tx, tenantID); err != nil {
			return err
		}
		if err := q.CreateTenant(ctx, store.CreateTenantParams{ID: tenantID, Slug: in.Slug, Name: in.CompanyName, Status: status}); err != nil {
			return err
		}
		if err := q.CreateTenantSettings(ctx, store.CreateTenantSettingsParams{TenantID: tenantID, Timezone: in.Timezone, CreatedBy: &userID}); err != nil {
			return err
		}
		if err := s.accounts.AddOwnerTx(ctx, tx, tenantID, userID); err != nil {
			return err
		}
		if err := s.audit(ctx, q, tenantID, userID, "tenant.created", meta); err != nil {
			return err
		}
		body, _ := json.Marshal(res.Body)
		code := int16(201)
		return q.CompletePlatformIdempotencyKey(ctx, store.CompletePlatformIdempotencyKeyParams{ID: claimed, ResponseStatus: &code, ResponseBody: body})
	})
	var rep errReplayed
	if errors.As(err, &rep) {
		return res, rep.err
	}
	if err != nil {
		mapped := db.MapError(err)
		if apperr.As(mapped).Code == apperr.ErrResourceBusy.Code { // lock_timeout menunggu key yang sama
			return SignupResult{}, apperr.ErrRequestInProgress.WithCause(err)
		}
		return SignupResult{}, mapped
	}
	if !verified {
		if err := s.accounts.SendVerification(ctx, userID); err != nil {
			// Signup tetap sukses: user bisa meminta link baru (POST /auth/resend-verification).
			observability.Logger(ctx).Error("mail.verification_failed", "user_id", userID.String(), "err", err.Error())
		}
	}
	return res, nil
}

// errReplayed membawa hasil replay keluar dari WithTx (transaksi di-rollback: tidak ada yang ditulis).
type errReplayed struct{ err error }

func (e errReplayed) Error() string { return "replayed" }

// replay: key sudah ada → (hasil, done=true) untuk replay atau 422; key ada tetapi belum selesai →
// done=false (pemanggil memutuskan). Key kedaluwarsa dihapus agar bisa dipakai lagi.
func (s *Service) replay(ctx context.Context, q store.DBTX, key string, reqHash []byte) (SignupResult, bool, error) {
	st := store.New(q)
	row, err := st.GetPlatformIdempotencyKey(ctx, store.GetPlatformIdempotencyKeyParams{Endpoint: signupEndpoint, Key: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return SignupResult{}, false, nil
	}
	if err != nil {
		return SignupResult{}, false, db.MapError(err)
	}
	if !s.clock.Now().Before(row.ExpiresAt) {
		return SignupResult{}, false, db.MapError(st.DeletePlatformIdempotencyKey(ctx, row.ID))
	}
	if string(row.RequestHash) != string(reqHash) {
		return SignupResult{}, true, apperr.ErrIdempotencyKeyReused
	}
	if row.ResponseStatus == nil {
		return SignupResult{}, false, nil
	}
	var res SignupResult
	if err := json.Unmarshal(row.ResponseBody, &res.Body); err != nil {
		return SignupResult{}, true, err
	}
	res.Replayed = true
	return res, true, nil
}

// ActivatePendingTenants dipanggil iam di transaksi verifikasi email (mode platform user): tenant
// PENDING_VERIFICATION tempat user OWNER aktif menjadi ACTIVE, masing-masing di mode tenant-nya.
func (s *Service) ActivatePendingTenants(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	q := store.New(tx)
	ids, err := q.ListPendingOwnedTenants(ctx, userID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := db.SetTenant(ctx, tx, id); err != nil {
			return err
		}
		if _, err := q.ActivateTenant(ctx, id); err != nil {
			return err
		}
		if err := s.audit(ctx, q, id, userID, "tenant.activated", reqmeta.Meta{}); err != nil {
			return err
		}
	}
	return nil
}

// Settings adalah tenant_settings di respons T2.
type Settings struct {
	Timezone                   string  `json:"timezone"`
	WorkDays                   []int16 `json:"work_days"`
	ProrationBasis             string  `json:"proration_basis"`
	CutoffDayOfNextMonth       int16   `json:"cutoff_day_of_next_month"`
	CutoffTimeLocal            string  `json:"cutoff_time_local"`
	OvertimeBasis              string  `json:"overtime_basis"`
	OvertimeMaxHoursPerDay     *string `json:"overtime_max_hours_per_day"`
	IncompleteAttendancePolicy string  `json:"incomplete_attendance_policy"`
	RulesetCode                string  `json:"ruleset_code"`
	NetChangeWarningPct        string  `json:"net_change_warning_pct"`
}

type Tenant struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Slug     string    `json:"slug"`
	Status   string    `json:"status"`
	Settings Settings  `json:"settings"`
	Version  int32     `json:"version"`
}

// GetTenant (T2): tenant aktif (dari claim tid) + settings. version = tenant_settings.version (ETag
// untuk PATCH /tenant/settings).
func (s *Service) GetTenant(ctx context.Context) (Tenant, error) {
	tid, _ := tenantctx.From(ctx)
	var t Tenant
	err := db.WithTenantTx(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := store.New(tx).GetTenant(ctx, tid)
		if err != nil {
			return err
		}
		t = Tenant{ID: r.ID, Name: r.Name, Slug: r.Slug, Status: r.Status, Version: r.Version, Settings: Settings{
			Timezone: r.Timezone, WorkDays: r.WorkDays, ProrationBasis: r.ProrationBasis,
			CutoffDayOfNextMonth: r.CutoffDayOfNextMonth, CutoffTimeLocal: r.CutoffTimeLocal, OvertimeBasis: r.OvertimeBasis,
			IncompleteAttendancePolicy: r.IncompleteAttendancePolicy, RulesetCode: r.RulesetCode, NetChangeWarningPct: r.NetChangeWarningPct,
		}}
		if r.OvertimeMaxHoursPerDay != "" {
			t.Settings.OvertimeMaxHoursPerDay = &r.OvertimeMaxHoursPerDay
		}
		return nil
	})
	return t, db.MapError(err)
}

func (s *Service) audit(ctx context.Context, q *store.Queries, tenantID, userID uuid.UUID, action string, meta reqmeta.Meta) error {
	target := "tenant"
	var reqID *string
	if meta.RequestID != "" {
		reqID = &meta.RequestID
	}
	var ua *string
	if meta.UserAgent != "" {
		v := meta.UserAgent
		if len(v) > 512 {
			v = v[:512]
		}
		ua = &v
	}
	return q.InsertTenantAudit(ctx, store.InsertTenantAuditParams{
		ID: s.ids.New(), TenantID: &tenantID, ActorType: "USER", ActorID: &userID, Action: action,
		TargetType: &target, TargetID: &tenantID, RequestID: reqID, Ip: meta.IP, UserAgent: ua,
	})
}
