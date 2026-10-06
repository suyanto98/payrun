// Package app merakit dependency dan router. Ini satu-satunya tempat yang boleh mengenal
// semua modul sekaligus (composition root); modul tidak saling meng-import (ADR-001).
//
// Sprint 0: belum ada modul domain. Router sudah memasang middleware platform lengkap (request id,
// recover, tracing, metrics, security header, client IP, access log, body limit) supaya setiap modul
// yang ditambahkan mulai Sprint 1 otomatis mendapatkannya.
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
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/db/migrations"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/ratelimit"
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
}

// ValidateAPI memastikan dependency wajib API terisi. Wiring yang lupa harus menggagalkan boot,
// bukan meledak di tengah request (pelajaran LedgerPay Sprint 6).
func (d Deps) ValidateAPI() error {
	var errs []error
	for name, missing := range map[string]bool{
		"Logger": d.Logger == nil, "Pool": d.Pool == nil, "Tokens": d.Tokens == nil, "Clock": d.Clock == nil, "IDs": d.IDs == nil,
	} {
		if missing {
			errs = append(errs, fmt.Errorf("app: dependency %s is required", name))
		}
	}
	return errors.Join(errs...)
}

// APIRouter adalah router API publik, di-mount di /api/v1.
func APIRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID(d.Logger), httpx.Recover, tracing.Middleware, httpx.Metrics, httpx.SecurityHeaders,
		httpx.ClientIP(d.TrustedProxies), httpx.AccessLog, httpx.BodyLimit(httpx.MaxBodyBytes))
	// Route tidak dikenal dan method salah sama-sama 404 problem+json: tidak membocorkan route mana
	// yang ada (dan konsisten dengan aturan "resource tenant lain = 404").
	r.NotFound(func(w http.ResponseWriter, r *http.Request) { httpx.WriteError(w, r, apperr.ErrNotFound) })
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) { httpx.WriteError(w, r, apperr.ErrNotFound) })
	r.Route("/api/v1", func(chi.Router) {
		// Modul didaftarkan di sini mulai Sprint 1: tenancy (signup), iam (session, switch tenant).
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
