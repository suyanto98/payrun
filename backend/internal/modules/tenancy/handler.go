package tenancy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"payrun/internal/platform/apperr"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/reqmeta"
)

// HandlerConfig: fungsi milik iam yang dipakai respons signup (sesi + cookie + rate limit auth-strict),
// disuntikkan composition root (ADR-001).
type HandlerConfig struct {
	// Strict menerapkan tier auth-strict untuk (IP, subjek); false = sudah dijawab 429.
	Strict func(w http.ResponseWriter, r *http.Request, subject string) bool
	// StartSession menerbitkan sesi untuk owner; body = field sesi (access_token, user, active_tenant, …).
	StartSession func(ctx context.Context, userID, tenantID uuid.UUID, meta reqmeta.Meta) (body map[string]any, refresh string, maxAge time.Duration, err error)
	// SetRefreshCookie memasang cookie refresh (atribut ADR-009).
	SetRefreshCookie func(w http.ResponseWriter, value string, maxAge time.Duration)
}

type Handler struct {
	svc *Service
	cfg HandlerConfig
}

func NewHandler(svc *Service, cfg HandlerConfig) *Handler { return &Handler{svc: svc, cfg: cfg} }

// Routes: signup publik (auth-strict per IP+email); GET /tenant di router tenant-scoped (RequireTenant).
func (h *Handler) Routes(public, tenantRead, tenantWrite chi.Router) {
	public.Post("/tenants/signup", h.signup)
	tenantRead.Get("/tenant", h.getTenant)
	// T4 semua role (kalender kerja); T5/T6 OWNER/HR_ADMIN (file 17 §2.1).
	tenantRead.Get("/holidays", h.listHolidays)
	admin := tenantWrite.With(auth.RequireAnyRole(auth.RoleOwner, auth.RoleHRAdmin))
	admin.Post("/holidays", h.createHoliday)
	admin.Delete("/holidays/{id}", h.deleteHoliday)
}

func (h *Handler) listHolidays(w http.ResponseWriter, r *http.Request) {
	if err := httpx.OnlyQuery(r, "year"); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	year, err := strconv.Atoi(r.URL.Query().Get("year"))
	if err != nil {
		httpx.WriteError(w, r, apperr.Validation(apperr.FieldError{Field: "year", Code: "REQUIRED", Message: "?year=YYYY"}))
		return
	}
	hs, err := h.svc.ListHolidays(r.Context(), year)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": hs})
}

func (h *Handler) createHoliday(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Date string `json:"date"`
		Name string `json:"name"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	hol, err := h.svc.CreateHoliday(r.Context(), p.UserID, in.Date, in.Name, reqmeta.From(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, hol)
}

func (h *Handler) deleteHoliday(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if err := h.svc.DeleteHoliday(r.Context(), p.UserID, id, reqmeta.From(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) signup(w http.ResponseWriter, r *http.Request) {
	var in SignupInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	// Kunci rate limit dari email yang sudah dinormalisasi: spasi tambahan tidak boleh membuka bucket baru.
	if !h.cfg.Strict(w, r, "signup|"+strings.TrimSpace(in.Owner.Email)) {
		return
	}
	meta := reqmeta.From(r)
	res, err := h.svc.Signup(r.Context(), r.Header.Get("Idempotency-Key"), in, meta)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if res.Replayed {
		// Replay hanya berisi tenant (tanpa token/PII); token didapat lewat login biasa (10-db-shared §3.3).
		w.Header().Set("Idempotent-Replayed", "true")
		httpx.WriteJSON(w, http.StatusCreated, res.Body)
		return
	}
	prefer := uuid.Nil
	if res.Body.Tenant.Status == "ACTIVE" {
		prefer = res.Body.Tenant.ID
	}
	body, refresh, maxAge, err := h.cfg.StartSession(r.Context(), res.UserID, prefer, meta)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	body["tenant"] = res.Body.Tenant
	body["email_verification_required"] = res.Body.EmailVerificationRequired
	h.cfg.SetRefreshCookie(w, refresh, maxAge)
	w.Header().Set("Location", "/api/v1/tenant")
	httpx.WriteJSON(w, http.StatusCreated, body)
}

func (h *Handler) getTenant(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.GetTenant(r.Context())
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(int(t.Version))))
	httpx.WriteJSON(w, http.StatusOK, t)
}
