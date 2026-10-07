// Command payrun adalah satu binary dengan beberapa subcommand (ADR-001):
//
//	payrun api           # server API publik (:8080) + ops (:9090)
//	payrun migrate up    # jalankan migrasi (role payrun_owner)
//	payrun healthcheck   # dipakai HEALTHCHECK container (image distroless tidak punya curl)
//
// Subcommand lain ditambahkan saat pertama kali dibutuhkan: `worker` (Sprint 4, kalkulasi async),
// `seed` (data demo), `verify-run` (recompute dari snapshot, bukti PI-12).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
	_ "time/tzdata" // validasi timezone IANA (signup, settings) tidak bergantung pada zoneinfo image

	"payrun/db/migrations"
	"payrun/internal/app"
	"payrun/internal/modules/iam"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/config"
	"payrun/internal/platform/db"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/mail"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/observability"
	"payrun/internal/platform/ratelimit"
	"payrun/internal/platform/tracing"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: payrun <api|migrate up|healthcheck>")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "api":
		err = runAPI()
	case "migrate":
		err = runMigrate(os.Args[2:])
	case "healthcheck":
		err = runHealthcheck()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runAPI() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateAPI(); err != nil { // fail fast: jangan start dengan config setengah benar
		return err
	}
	logger := observability.NewLogger(os.Stdout, cfg.LogLevel, "api", cfg.AppEnv, cfg.Version)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := tracing.Setup(ctx, "payrun-api", cfg.Version, cfg.OTLPEndpoint)
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.Background()) }()
	metrics.SetBuildInfo("api", cfg.Version)
	pool, err := db.NewPool(ctx, cfg.DatabaseURL, db.Options{
		MaxConns: cfg.DBMaxConns, StatementTimeout: cfg.DBStatementTimeout,
		LockTimeout: cfg.DBLockTimeout, IdleInTxTimeout: cfg.DBIdleInTxTimeout,
	})
	if err != nil {
		return err
	}
	defer pool.Close()
	metrics.RegisterPool("api", pool)

	// RLS hanya melindungi kalau role aplikasi tidak bisa melewatinya. DSN yang salah (contoh memakai
	// payrun_owner) = menolak start, bukan berjalan tanpa isolasi tenant.
	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	rc, err := db.CheckAppRole(startCtx, pool)
	cancel()
	if err != nil {
		return err
	}
	logger.Info("db.role_check_passed", "role", rc.Role)

	clk, ids := clock.System{}, idgen.V7{}
	tokens, err := auth.NewTokens(cfg.JWTKeys, cfg.JWTActiveKID, cfg.JWTIssuer, cfg.JWTAccessTTL, clk, ids)
	if err != nil {
		return err
	}
	tiers := ratelimit.DefaultTiers()
	if cfg.RateLimitDisabled { // perf env saja (divalidasi config)
		tiers = ratelimit.Unlimited()
		logger.Warn("ratelimit.disabled", "app_env", cfg.AppEnv)
	}
	deps := app.Deps{
		Logger: logger, Pool: pool, Tokens: tokens, Clock: clk, IDs: ids, CookieSecure: cfg.CookieSecure,
		AllowedOrigins: cfg.AllowedOrigins, TrustedProxies: cfg.TrustedProxies, PublicBaseURL: cfg.PublicBaseURL, Tiers: &tiers,
		IAM: iam.Config{Argon2: auth.DefaultArgon2, RefreshTTL: cfg.RefreshTTL, RefreshFamilyTTL: cfg.RefreshFamilyTTL},
		Mailer: mail.SMTP{Addr: cfg.SMTPAddr, From: cfg.MailFrom, Username: cfg.SMTPUsername,
			Password: cfg.SMTPPassword, Timeout: cfg.MailTimeout},
	}
	if err := deps.ValidateAPI(); err != nil {
		return err
	}

	// Timeout server menutup slowloris dan koneksi menggantung (C-29).
	api := &http.Server{Addr: cfg.HTTPAddr, Handler: app.APIRouter(deps),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	ops := &http.Server{Addr: cfg.OpsAddr, Handler: withPprof(cfg.PprofEnabled, app.OpsRouter(pool)), ReadHeaderTimeout: 5 * time.Second}

	errc := make(chan error, 2)
	go func() { errc <- serve(api) }()
	go func() { errc <- serve(ops) }()
	logger.Info("api.started", "http_addr", cfg.HTTPAddr, "ops_addr", cfg.OpsAddr)

	select {
	case err = <-errc:
	case <-ctx.Done():
		logger.Info("api.shutting_down")
	}
	// Graceful shutdown: berhenti menerima koneksi baru, selesaikan request yang sedang berjalan.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	_ = api.Shutdown(shutdownCtx)
	_ = ops.Shutdown(shutdownCtx)
	return err
}

func serve(s *http.Server) error {
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runMigrate(args []string) error {
	if len(args) != 1 || args[0] != "up" {
		return errors.New("usage: payrun migrate up")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.MigrateDatabaseURL == "" {
		return errors.New("MIGRATE_DATABASE_URL wajib (role payrun_owner)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := migrations.MigrateUp(ctx, cfg.MigrateDatabaseURL); err != nil {
		return err
	}
	fmt.Println("migrations applied")
	return nil
}

func runHealthcheck() error {
	// URL konstan (loopback): healthcheck tidak boleh bisa diarahkan ke host lain lewat input.
	const readyURL = "http://127.0.0.1:9090/readyz"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, readyURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("not ready: HTTP %d", resp.StatusCode)
	}
	return nil
}

// withPprof memasang /debug/pprof/* di port OPS internal (tidak pernah lewat Caddy), hanya kalau
// PPROF_ENABLED (perf env). Profil mutex/block ikut dinyalakan untuk analisis kontensi lock.
func withPprof(enabled bool, h http.Handler) http.Handler {
	if !enabled {
		return h
	}
	runtime.SetMutexProfileFraction(10)
	runtime.SetBlockProfileRate(int(time.Millisecond))
	mux := http.NewServeMux()
	mux.Handle("/", h)
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}
