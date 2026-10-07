// Package app merakit dependency dan router. Ini satu-satunya tempat yang boleh mengenal
// semua modul sekaligus (composition root); modul tidak saling meng-import (ADR-001).
//
// Router memasang middleware platform lengkap (request id, tracing, metrics, security header, client
// IP, access log, recover, body limit) sebelum modul mana pun, sehingga setiap modul mendapatkannya.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/db/migrations"
	"payrun/internal/modules/iam"
	"payrun/internal/modules/tenancy"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/mail"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/ratelimit"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tracing"
)

// Deps adalah dependency yang dibuat oleh main (atau test).
type Deps struct {
	Logger *slog.Logger
	Pool   *pgxpool.Pool
	Tokens *auth.Tokens
	Clock  clock.Clock
	IDs    idgen.Generator

	CookieSecure   bool
	AllowedOrigins []string
	TrustedProxies []netip.Prefix
	PublicBaseURL  string
	Tiers          *ratelimit.Tiers // nil → ratelimit.DefaultTiers()

	IAM    iam.Config
	Mailer mail.Sender // verifikasi email (Mailpit di dev)
}

// ValidateAPI memastikan dependency wajib API terisi. Wiring yang lupa harus menggagalkan boot,
// bukan meledak di tengah request (pelajaran LedgerPay Sprint 6).
func (d Deps) ValidateAPI() error {
	var errs []error
	for name, missing := range map[string]bool{
		"Logger": d.Logger == nil, "Pool": d.Pool == nil, "Tokens": d.Tokens == nil, "Clock": d.Clock == nil, "IDs": d.IDs == nil, "Mailer": d.Mailer == nil,
	} {
		if missing {
			errs = append(errs, fmt.Errorf("app: dependency %s is required", name))
		}
	}
	if d.IAM.Argon2.Memory == 0 || d.IAM.RefreshTTL <= 0 || d.IAM.RefreshFamilyTTL < d.IAM.RefreshTTL {
		errs = append(errs, errors.New("app: IAM config (Argon2, RefreshTTL <= RefreshFamilyTTL) is required"))
	}
	return errors.Join(errs...)
}

// APIRouter adalah router API publik, di-mount di /api/v1.
func APIRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	// Recover di DALAM Metrics dan AccessLog: panic diubah menjadi 500 sebelum keduanya mencatat,
	// sehingga request yang panic tetap muncul di log akses dan app_http_requests_total{status="500"}.
	r.Use(httpx.RequestID(d.Logger), tracing.Middleware, httpx.Metrics, httpx.SecurityHeaders,
		httpx.ClientIP(d.TrustedProxies), httpx.AccessLog, httpx.Recover, httpx.BodyLimit(httpx.MaxBodyBytes))
	// Route tidak dikenal dan method salah sama-sama 404 problem+json: tidak membocorkan route mana
	// yang ada (dan konsisten dengan aturan "resource tenant lain = 404").
	r.NotFound(func(w http.ResponseWriter, r *http.Request) { httpx.WriteError(w, r, apperr.ErrNotFound) })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) { httpx.WriteError(w, r, apperr.ErrNotFound) })
	tiers := ratelimit.DefaultTiers()
	if d.Tiers != nil {
		tiers = *d.Tiers
	}
	limiter := ratelimit.New(d.Clock)
	iamCfg := d.IAM
	if iamCfg.PublicBaseURL == "" {
		iamCfg.PublicBaseURL = d.PublicBaseURL
	}
	iamSvc := iam.NewService(d.Pool, d.Tokens, d.Clock, d.IDs, iamCfg, d.Mailer)
	iamH := iam.NewHandler(iamSvc, iam.HandlerConfig{
		CookieSecure: d.CookieSecure, AllowedOrigins: d.AllowedOrigins, Limiter: limiter, Tiers: tiers,
	})
	tenancySvc := tenancy.NewService(d.Pool, iamSvc, d.Clock, d.IDs)
	iamSvc.OnEmailVerified(tenancySvc.ActivatePendingTenants) // verifikasi email → tenant PENDING menjadi ACTIVE
	tenancyH := tenancy.NewHandler(tenancySvc, tenancy.HandlerConfig{
		Strict:           iamH.Strict,
		StartSession:     sessionStarter(iamSvc),
		SetRefreshCookie: iamH.SetRefreshCookie,
	})

	byUser := func(r *http.Request) string { p, _ := auth.PrincipalFrom(r.Context()); return p.UserID.String() }
	r.Route("/api/v1", func(v1 chi.Router) {
		authed := v1.With(auth.RequireUser(d.Tokens))
		read := authed.With(limiter.Middleware(tiers.Read, byUser))
		write := authed.With(limiter.Middleware(tiers.Write, byUser))
		// Endpoint tenant-scoped: token tanpa `tid` → 403 "Select a tenant first".
		tenantRead := authed.With(auth.RequireTenant, limiter.Middleware(tiers.Read, byUser))
		iamH.Routes(v1, read, write)
		tenancyH.Routes(v1.With(limiter.Middleware(tiers.AuthIP, ratelimit.ByIP)), tenantRead)
	})
	return r
}

// OpsRouter: /healthz, /readyz, dan /metrics di port internal (tidak diekspos Caddy; Step 8 `01` §2).
func OpsRouter(pool *pgxpool.Pool) http.Handler {
	r := chi.NewRouter()
	// Liveness: proses hidup. Sengaja TIDAK memeriksa DB: kalau DB mati, me-restart API tidak menolong.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Readiness: boleh menerima trafik? DB terjangkau dan skema cukup baru untuk binary ini
	// (versi DB >= minimum binary, bukan "=", supaya rollback app N-1 tetap ready).
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		checks := map[string]string{}
		ready := true
		v, err := migrations.SchemaVersion(ctx, pool)
		switch {
		case err != nil:
			checks["database"] = "unreachable"
			ready = false
		case v < migrations.MinSchemaVersion:
			checks["database"] = "ok"
			checks["schema"] = "too old"
			ready = false
		default:
			checks["database"] = "ok"
			checks["schema"] = "ok"
		}
		status, state := http.StatusOK, "ready"
		if !ready {
			status, state = http.StatusServiceUnavailable, "not_ready"
		}
		metrics.Ready.WithLabelValues("api").Set(boolFloat(ready))
		httpx.WriteJSON(w, status, map[string]any{"status": state, "checks": checks, "schema_version": v, "min_schema_version": migrations.MinSchemaVersion})
	})
	r.Handle("/metrics", metrics.Handler())
	return r
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// sessionStarter mengadaptasi iam.StartSession untuk respons signup tenancy (tenancy tidak mengenal
// tipe iam; ADR-001).
func sessionStarter(svc *iam.Service) func(ctx context.Context, userID, tenantID uuid.UUID, meta reqmeta.Meta) (map[string]any, string, time.Duration, error) {
	return func(ctx context.Context, userID, tenantID uuid.UUID, meta reqmeta.Meta) (map[string]any, string, time.Duration, error) {
		sess, err := svc.StartSession(ctx, userID, tenantID, "auth.login_succeeded", meta)
		if err != nil {
			return nil, "", 0, err
		}
		r := iam.NewSessionResponse(sess, true)
		return map[string]any{"access_token": r.AccessToken, "token_type": r.TokenType, "expires_in": r.ExpiresIn,
			"user": r.User, "active_tenant": r.ActiveTenant}, sess.RefreshToken, sess.RefreshMaxAge, nil
	}
}
