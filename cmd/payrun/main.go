// Command payrun adalah satu binary dengan beberapa subcommand (ADR-001):
//
//	payrun api           # server API publik (:8080) + ops (:9090)
//	payrun worker        # job runner: kalkulasi payroll async, maintenance (+ ops :9090)
//	payrun migrate up    # jalankan migrasi (role payrun_owner)
//	payrun verify-run <tenant-id> <run-id>  # recompute dari snapshot (PI-12); exit 1 bila ada perbedaan
//	payrun healthcheck   # dipakai HEALTHCHECK container (image distroless tidak punya curl)
//
// Subcommand lain ditambahkan saat pertama kali dibutuhkan: `seed` (data demo).
package main

import (
	"context"
	"encoding/json"
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

	"github.com/google/uuid"

	"payrun/db/migrations"
	migrationsdemo "payrun/db/migrations_demo"
	"payrun/internal/app"
	"payrun/internal/modules/iam"
	"payrun/internal/modules/payroll"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/config"
	"payrun/internal/platform/db"
	"payrun/internal/platform/faults"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/mail"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/observability"
	"payrun/internal/platform/ratelimit"
	"payrun/internal/platform/storage"
	"payrun/internal/platform/tracing"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: payrun <api|worker|migrate up|verify-run|seed|healthcheck>")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "api":
		err = runAPI()
	case "worker":
		err = runWorker()
	case "perf-recheck":
		err = runPerfRecheck(os.Args[2:])
	case "seed":
		err = runSeed(os.Args[2:])
	case "verify-run":
		err = runVerify(os.Args[2:])
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
		Keyring: cfg.Keyring, BlindIndex: cfg.BlindIndex, DemoMode: cfg.DemoMode, DemoToken: cfg.DemoToken,
	}
	if cfg.DemoMode && !faults.Enabled() {
		logger.Warn("demo.mode_ignored", "reason", "DEMO_MODE=true but this is a release build (no demo tag)")
	}
	if deps.Storage, err = objectStorage(cfg, logger); err != nil {
		return err
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

func runWorker() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateWorker(); err != nil {
		return err
	}
	logger := observability.NewLogger(os.Stdout, cfg.LogLevel, "worker", cfg.AppEnv, cfg.Version)
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := tracing.Setup(ctx, "payrun-worker", cfg.Version, cfg.OTLPEndpoint)
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.Background()) }()
	metrics.SetBuildInfo("worker", cfg.Version)
	// Pool worker 10 (Step 7 `01` §6); lock_timeout 5 dtk: chunk menunggu lock run lebih lama dari API.
	pool, err := db.NewPool(ctx, cfg.DatabaseURL, db.Options{
		MaxConns: 10, StatementTimeout: cfg.DBStatementTimeout, LockTimeout: 5 * time.Second, IdleInTxTimeout: cfg.DBIdleInTxTimeout,
	})
	if err != nil {
		return err
	}
	defer pool.Close()
	metrics.RegisterPool("worker", pool)
	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	rc, err := db.CheckAppRole(startCtx, pool) // worker juga tunduk pada RLS (handler membuka tx tenant)
	cancel()
	if err != nil {
		return err
	}
	logger.Info("db.role_check_passed", "role", rc.Role)

	deps := app.Deps{Logger: logger, Pool: pool, Clock: clock.System{}, IDs: idgen.V7{}}
	if deps.Storage, err = objectStorage(cfg, logger); err != nil {
		return err
	}
	if err := deps.ValidateWorker(); err != nil {
		return err
	}
	if !cfg.PayrollCalcEnabled {
		logger.Warn("worker.kill_switch", "payroll_calc_disabled", true)
	}
	workerID := cfg.WorkerID
	if workerID == "" {
		workerID, _ = os.Hostname()
	}
	// ID unik per START (hostname:pid:uuid): worker yang restart tidak pernah dianggap pemilik lease lamanya.
	id := fmt.Sprintf("%s:%d:%s", workerID, os.Getpid(), uuid.NewString()[:8])
	w := app.NewWorker(deps, app.WorkerConfig{ID: id, ChunkSize: cfg.PayrollChunkSize, Lease: cfg.JobLeaseTTL, PayrollConcurrency: cfg.PayrollWorkerConcurrency, DisablePayrollCalc: !cfg.PayrollCalcEnabled,
		DisablePayslipRender: !cfg.PayslipRenderEnabled})
	ops := &http.Server{Addr: cfg.OpsAddr, Handler: withPprof(cfg.PprofEnabled, app.WorkerOpsRouter(w, cfg.WorkerPollInterval)), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := serve(ops); err != nil {
			logger.Error("worker.ops_failed", "err", err.Error())
			stop()
		}
	}()
	defer func() { _ = ops.Shutdown(context.Background()) }()
	logger.Info("worker.started", "worker_id", id, "ops_addr", cfg.OpsAddr, "chunk_size", cfg.PayrollChunkSize)
	w.Run(observability.WithLogger(ctx, logger), cfg.WorkerPollInterval)
	logger.Info("worker.stopped")
	return nil
}

func serve(s *http.Server) error {
	if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// runVerify: recompute setiap item run dari snapshot-nya (ADR-P07, Demo C). Berjalan sebagai payrun_app
// di bawah RLS tenant yang disebut; laporan JSON ke stdout.
func runVerify(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: payrun verify-run <tenant-id> <run-id>")
	}
	tid, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("tenant-id: %w", err)
	}
	rid, err := uuid.Parse(args[1])
	if err != nil {
		return fmt.Errorf("run-id: %w", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.DatabaseURL == "" {
		return errors.New("DATABASE_URL wajib (role payrun_app)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := db.NewPool(ctx, cfg.DatabaseURL, db.Options{MaxConns: 2, StatementTimeout: time.Minute, LockTimeout: cfg.DBLockTimeout, IdleInTxTimeout: time.Minute})
	if err != nil {
		return err
	}
	defer pool.Close()
	if _, err := db.CheckAppRole(ctx, pool); err != nil {
		return err
	}
	rep, err := payroll.NewService(pool, clock.System{}, idgen.V7{}, nil).VerifyRun(ctx, tid, rid)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return err
	}
	if !rep.OK() {
		return fmt.Errorf("%d of %d items differ from their snapshot", len(rep.Mismatches), rep.ItemsChecked)
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
	if err := migrationsdemo.MigrateUp(ctx, cfg.MigrateDatabaseURL); err != nil { // no-op di build release
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

// objectStorage: klien S3 bila S3_ENDPOINT diisi (ADR-012); kosong → tanpa PDF payslip (peringatan).
func objectStorage(cfg config.Config, logger *slog.Logger) (storage.Store, error) {
	if cfg.S3Endpoint == "" {
		logger.Warn("storage.disabled", "reason", "S3_ENDPOINT empty; payslip PDFs are not rendered")
		return nil, nil
	}
	return storage.NewS3(storage.Config{Endpoint: cfg.S3Endpoint, PublicEndpoint: cfg.S3PublicEndpoint, Region: cfg.S3Region,
		Bucket: cfg.S3Bucket, AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey})
}

// runSeed: `payrun seed <smoke|perf>` — dataset load test (COPY + gate invariant). Butuh SEED_DATABASE_URL
// (superuser, HANYA di container perf/dev) dan DATABASE_URL (payrun_app, untuk gate lewat RLS).
func runSeed(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: payrun seed <smoke|perf>")
	}
	profile, ok := app.SeedProfiles[args[0]]
	if !ok {
		return fmt.Errorf("unknown seed profile %q (smoke | perf)", args[0])
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.AppEnv != "dev" && cfg.AppEnv != "test" && cfg.AppEnv != "perf" {
		return fmt.Errorf("seed refuses to run in APP_ENV=%s (dev/test/perf only)", cfg.AppEnv)
	}
	superDSN := os.Getenv("SEED_DATABASE_URL")
	if superDSN == "" || cfg.DatabaseURL == "" {
		return errors.New("SEED_DATABASE_URL (superuser) and DATABASE_URL (payrun_app) are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	pool, err := db.NewPool(ctx, cfg.DatabaseURL, db.Options{MaxConns: 4, StatementTimeout: time.Minute, LockTimeout: cfg.DBLockTimeout, IdleInTxTimeout: time.Minute})
	if err != nil {
		return err
	}
	defer pool.Close()
	rep, err := app.Seed(ctx, superDSN, pool, profile, 42)
	if err != nil {
		return err
	}
	fmt.Printf("seed %s OK: %d tenants, %d employees, %d attendance rows; big tenant %s (owner %s); COPY %.1fs, total (incl. VACUUM + gate) %.1fs\n",
		rep.Profile, rep.Tenants, rep.Employees, rep.Attendance, rep.BigTenant, app.SeedOwnerEmail(0), rep.LoadSeconds, rep.TotalSec)
	return nil
}

// runPerfRecheck: `payrun perf-recheck <tenant> <run>` — eksperimen E-2: durasi stale detection kalau SEMUA
// karyawan diverifikasi ulang (pembanding change log O12). Read-only, APP_ENV dev/test/perf saja.
func runPerfRecheck(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: payrun perf-recheck <tenant-id> <run-id>")
	}
	tid, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("tenant-id: %w", err)
	}
	rid, err := uuid.Parse(args[1])
	if err != nil {
		return fmt.Errorf("run-id: %w", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.AppEnv != "dev" && cfg.AppEnv != "test" && cfg.AppEnv != "perf" {
		return fmt.Errorf("perf-recheck refuses to run in APP_ENV=%s", cfg.AppEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	pool, err := db.NewPool(ctx, cfg.DatabaseURL, db.Options{MaxConns: 2, StatementTimeout: 10 * time.Minute, LockTimeout: cfg.DBLockTimeout, IdleInTxTimeout: time.Minute})
	if err != nil {
		return err
	}
	defer pool.Close()
	rep, err := payroll.NewService(pool, clock.System{}, idgen.V7{}, nil).FullRecheck(ctx, tid, rid)
	if err != nil {
		return err
	}
	fmt.Printf("full recheck: %d employees, %d changed, %.3fs\n", rep.Employees, rep.Changed, rep.Duration.Seconds())
	return nil
}
