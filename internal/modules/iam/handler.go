package iam

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"payrun/internal/platform/apperr"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/ratelimit"
	"payrun/internal/platform/reqmeta"
)

// RefreshCookieName dan path mengikuti ADR-009: cookie hanya dikirim browser ke /api/v1/auth/*.
const (
	RefreshCookieName = "pr_refresh"
	refreshCookiePath = "/api/v1/auth"
)

// HandlerConfig: pengaturan HTTP iam.
type HandlerConfig struct {
	CookieSecure   bool
	AllowedOrigins []string // cek Origin di endpoint cookie (CSRF, C-08)
	Limiter        *ratelimit.Limiter
	Tiers          ratelimit.Tiers
}

type Handler struct {
	svc *Service
	cfg HandlerConfig
}

func NewHandler(svc *Service, cfg HandlerConfig) *Handler {
	return &Handler{svc: svc, cfg: cfg}
}

// Routes: /auth/* publik (tier auth-ip per IP; login/refresh juga auth-strict), /me* dan
// /session/switch-tenant butuh access token (dengan atau tanpa `tid`).
func (h *Handler) Routes(public, read, write chi.Router) {
	public.Group(func(r chi.Router) {
		r.Use(h.cfg.Limiter.Middleware(h.cfg.Tiers.AuthIP, ratelimit.ByIP))
		r.Post("/auth/login", h.login)
		r.Post("/auth/verify-email", h.verifyEmail)
		r.Group(func(r chi.Router) { // diautentikasi lewat cookie → CSRF guard
			r.Use(httpx.CSRFGuard(h.cfg.AllowedOrigins))
			r.Post("/auth/refresh", h.refresh)
			r.Post("/auth/logout", h.logout)
		})
	})
	read.Get("/me", h.me)
	read.Get("/me/memberships", h.memberships)
	write.Post("/session/switch-tenant", h.switchTenant)
	write.Post("/auth/resend-verification", h.resendVerification)
	// U3: Bearer tanpa `tid` (tenant berasal dari token undangan), tier auth-ip per IP.
	write.With(h.cfg.Limiter.Middleware(h.cfg.Tiers.AuthIP, ratelimit.ByIP)).Post("/invitations/accept", h.acceptInvitation)
}

// TenantRoutes: endpoint member tenant-scoped (U1, U2), hanya OWNER/HR_ADMIN (file 17 §2.1).
func (h *Handler) TenantRoutes(read, write chi.Router) {
	admin := auth.RequireAnyRole(auth.RoleOwner, auth.RoleHRAdmin)
	read.With(admin).Get("/members", h.listMembers)
	write.With(admin).Post("/invitations", h.invite)
}

func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) {
	if err := httpx.OnlyQuery(r, "status", "role", "limit", "cursor"); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	limit, err := httpx.Limit(r, 20, 100)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	qs := r.URL.Query()
	ms, page, err := h.svc.ListMembers(r.Context(), MemberQuery{Status: qs.Get("status"), Role: qs.Get("role"), Cursor: qs.Get("cursor"), Limit: limit})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": ms, "page": page})
}

func (h *Handler) invite(w http.ResponseWriter, r *http.Request) {
	var in InviteInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	inv, err := h.svc.Invite(r.Context(), p, in, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	// invite_url memuat secret: jangan di-cache di mana pun.
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusCreated, inv)
}

func (h *Handler) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	m, err := h.svc.AcceptInvitation(r.Context(), p.UserID, in.Token, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"membership": m})
}

// Strict menerapkan tier auth-strict untuk (IP, subjek). Email tak terdaftar ikut dibatasi, sehingga
// rate limit tidak bisa dipakai membedakan email yang ada dan tidak (C-03, R2-5). Diekspor untuk signup.
func (h *Handler) Strict(w http.ResponseWriter, r *http.Request, subject string) bool {
	ok, wait := h.cfg.Limiter.Allow(h.cfg.Tiers.AuthStrict, httpx.ClientIPFrom(r).String()+"|"+strings.ToLower(subject))
	if !ok {
		ratelimit.Reject(w, r, h.cfg.Tiers.AuthStrict.Name, wait)
	}
	return ok
}

// SessionResponse adalah body login/refresh/signup/switch-tenant (16-api-endpoints A1).
type SessionResponse struct {
	AccessToken  string        `json:"access_token"`
	TokenType    string        `json:"token_type"`
	ExpiresIn    int64         `json:"expires_in"`
	User         *User         `json:"user,omitempty"`
	ActiveTenant *ActiveTenant `json:"active_tenant"`
}

// NewSessionResponse dipakai juga oleh tenancy (signup).
func NewSessionResponse(s Session, withUser bool) SessionResponse {
	out := SessionResponse{AccessToken: s.AccessToken, TokenType: "Bearer",
		ExpiresIn: int64(s.AccessExpiresIn.Seconds()), ActiveTenant: s.ActiveTenant}
	if withUser {
		u := s.User
		out.User = &u
	}
	return out
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var in LoginInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if !h.Strict(w, r, "login|"+in.Email) {
		return
	}
	if err := validateLogin(in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	sess, err := h.svc.Login(r.Context(), in, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	h.SetRefreshCookie(w, sess.RefreshToken, sess.RefreshMaxAge)
	httpx.WriteJSON(w, http.StatusOK, NewSessionResponse(sess, true))
}

func validateLogin(in LoginInput) error {
	var fe []apperr.FieldError
	if len(in.Email) == 0 || len(in.Email) > 254 || !strings.Contains(in.Email, "@") {
		fe = append(fe, apperr.FieldError{Field: "email", Code: "INVALID_FORMAT"})
	}
	if n := len([]rune(in.Password)); n < 1 || n > 128 { // tanpa minimum saat login (16-api A1)
		fe = append(fe, apperr.FieldError{Field: "password", Code: "LENGTH", Message: "1–128 characters"})
	}
	if len(fe) > 0 {
		return apperr.Validation(fe...)
	}
	return nil
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var active uuid.UUID
	if v := r.Header.Get("X-Active-Tenant"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil || id == uuid.Nil {
			httpx.WriteError(w, r, apperr.Validation(apperr.FieldError{Field: "X-Active-Tenant", Code: "INVALID_FORMAT", Message: "must be a UUID"}))
			return
		}
		active = id
	}
	// Key auth-strict untuk refresh = IP + SESI (hash cookie), bukan IP saja (temuan LedgerPay S5: SPA
	// me-refresh setiap reload, sehingga limit per IP me-logout semua user di belakang satu NAT). Tanpa
	// cookie tidak dihitung (langsung 401 tanpa query DB); tier auth-ip tetap membatasi per IP.
	raw := refreshCookie(r)
	if raw != "" {
		sum := sha256.Sum256([]byte(raw))
		if !h.Strict(w, r, "refresh|"+hex.EncodeToString(sum[:8])) {
			return
		}
	}
	sess, err := h.svc.Refresh(r.Context(), raw, active, reqmeta.From(r))
	if err != nil {
		if apperr.As(err).Code == apperr.ErrRefreshReused.Code {
			h.SetRefreshCookie(w, "", -1) // family sudah dicabut: hapus cookie di browser
		}
		httpx.WriteError(w, r, err)
		return
	}
	if sess.RefreshToken != "" { // grace window: tidak ada token baru, cookie penerus sudah di browser
		h.SetRefreshCookie(w, sess.RefreshToken, sess.RefreshMaxAge)
	}
	httpx.WriteJSON(w, http.StatusOK, NewSessionResponse(sess, true))
}

// logout selalu 204 (cookie tidak valid pun "berhasil" logout) dan selalu menghapus cookie.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Logout(r.Context(), refreshCookie(r), reqmeta.From(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	h.SetRefreshCookie(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context()) // dijamin RequireUser
	u, active, err := h.svc.Me(r.Context(), p)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var empID *uuid.UUID
	if active != nil && h.svc.linker != nil {
		if empID, err = h.svc.linker.SelfID(r.Context(), p.UserID); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		User
		ActiveTenant *ActiveTenant `json:"active_tenant"`
		EmployeeID   *uuid.UUID    `json:"employee_id"` // employee milik user di tenant aktif (S2)
	}{u, active, empID})
}

func (h *Handler) memberships(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	ms, err := h.svc.Memberships(r.Context(), p.UserID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": ms})
}

func (h *Handler) switchTenant(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TenantID string `json:"tenant_id"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	tid, err := uuid.Parse(in.TenantID)
	if err != nil || tid == uuid.Nil {
		httpx.WriteError(w, r, apperr.Validation(apperr.FieldError{Field: "tenant_id", Code: "INVALID_FORMAT", Message: "must be a UUID"}))
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	sess, err := h.svc.SwitchTenant(r.Context(), p.UserID, tid, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, NewSessionResponse(sess, false))
}

// verifyEmail: tanpa auth (link di email), tetapi wajib password akun saat ini (lihat
// Service.VerifyEmail). auth-strict per IP membatasi tebakan token (yang toh 256 bit) dan spam ke
// endpoint; password salah ikut menaikkan counter lockout akun.
func (h *Handler) verifyEmail(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if !h.Strict(w, r, "verify-email") {
		return
	}
	if err := h.svc.VerifyEmail(r.Context(), in.Token, in.Password, reqmeta.From(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// resendVerification: auth-strict per user (setiap panggilan mengirim email: cegah spam ke inbox).
func (h *Handler) resendVerification(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	if !h.Strict(w, r, "resend-verification|"+p.UserID.String()) {
		return
	}
	if err := h.svc.ResendVerification(r.Context(), p.UserID); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func refreshCookie(r *http.Request) string {
	c, err := r.Cookie(RefreshCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// SetRefreshCookie: maxAge < 0 menghapus cookie. Diekspor untuk signup.
func (h *Handler) SetRefreshCookie(w http.ResponseWriter, value string, maxAge time.Duration) {
	c := &http.Cookie{ //nolint:gosec // G124: Secure=false hanya diizinkan di dev/test; dijaga config.ValidateAPI.
		Name:     RefreshCookieName,
		Value:    value,
		Path:     refreshCookiePath,
		HttpOnly: true,                    // tidak terbaca JavaScript (mitigasi XSS)
		Secure:   h.cfg.CookieSecure,      // hanya lewat HTTPS (false hanya di dev)
		SameSite: http.SameSiteStrictMode, // tidak dikirim dari situs lain (mitigasi CSRF)
		MaxAge:   int(maxAge.Seconds()),
	}
	if maxAge < 0 {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}
