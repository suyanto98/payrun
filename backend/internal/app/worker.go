package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"payrun/db/migrations"
	"payrun/internal/modules/payroll"
	"payrun/internal/platform/httpx"
	"payrun/internal/platform/jobs"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/observability"
)

// Worker adalah proses latar PayRun (docs/design/20-background-jobs.md §4): satu loop klaim per LANE
// dengan pool goroutine sendiri (bulkhead). Kalkulasi payroll tenant besar tidak bisa menahan
// maintenance, dan sebaliknya.
type Worker struct {
	Payroll     *jobs.Runner // payroll.plan, payroll.calc_chunk
	Maintenance *jobs.Runner // cleanup (+ scheduler)
	Documents   *jobs.Runner // payslip.render (ADR-012); nil bila storage tidak dikonfigurasi atau dimatikan
	deps        Deps
}

// WorkerConfig: ID unik per proses (hostname:pid:uuid), lease default 30 dtk.
type WorkerConfig struct {
	ID                 string
	Lease              time.Duration
	PayrollConcurrency int  // default 4 (Step 12: test 4 worker paralel)
	ChunkSize          int  // default 50 (PAYROLL_CHUNK_SIZE, O5)
	DisablePayrollCalc bool // kill switch PR_PAYROLL_CALC_ENABLED=false
	// DisablePayslipRender: kill switch PR_PAYSLIP_RENDER_ENABLED=false (job tetap queued).
	DisablePayslipRender bool
}

// ValidateWorker memastikan dependency wajib worker terisi (boot gagal, bukan job gagal).
func (d Deps) ValidateWorker() error {
	var errs []error
	for name, missing := range map[string]bool{"Logger": d.Logger == nil, "Pool": d.Pool == nil, "Clock": d.Clock == nil, "IDs": d.IDs == nil} {
		if missing {
			errs = append(errs, fmt.Errorf("app: dependency %s is required", name))
		}
	}
	return errors.Join(errs...)
}

// NewWorker merakit semua lane.
func NewWorker(d Deps, cfg WorkerConfig) *Worker {
	if cfg.PayrollConcurrency == 0 {
		cfg.PayrollConcurrency = 4
	}
	rc := func(lane string, n int) jobs.RunnerConfig {
		return jobs.RunnerConfig{Worker: cfg.ID + ":" + lane, Lease: cfg.Lease, Concurrency: n}
	}
	// Kill switch: kind yang dimatikan tidak didaftarkan → job-nya tetap queued (tidak hilang, tidak dead)
	// dan diproses lagi begitu flag dikembalikan. Run tetap CALCULATING sampai saat itu.
	var payrollKinds []jobs.Kind
	if !cfg.DisablePayrollCalc {
		payrollKinds = payroll.NewJobs(d.Pool, d.IDs, cfg.ChunkSize).Kinds()
	}
	w := &Worker{deps: d, Payroll: jobs.NewRunner(d.Pool, rc("payroll", cfg.PayrollConcurrency), payrollKinds...)}
	if d.Storage != nil && !cfg.DisablePayslipRender {
		docs := payroll.NewJobs(d.Pool, d.IDs, cfg.ChunkSize).WithDocuments(d.Storage, d.Clock)
		w.Documents = jobs.NewRunner(d.Pool, rc("documents", 2), docs.DocumentKinds()...)
	}
	demoKinds, demoEvery := demoMaintenanceKinds(d) // kosong di build release
	w.Maintenance = jobs.NewRunner(d.Pool, rc("maintenance", 1), append(demoKinds,
		jobs.Kind{Name: "maintenance.jobs_cleanup", MaxAttempts: 3, Timeout: 2 * time.Minute,
			Handler: func(ctx context.Context, _ *jobs.Job) error {
				n, err := jobs.Cleanup(ctx, d.Pool)
				observability.Logger(ctx).Info("jobs.cleanup_done", "deleted", n)
				return err
			}},
	)...)
	w.Maintenance.Every("maintenance.jobs_cleanup", time.Hour)
	for kind, every := range demoEvery {
		w.Maintenance.Every(kind, every)
	}
	return w
}

// Ready: setiap lane mengulang loop klaim baru-baru ini (< 3× interval polling, minimal 5 dtk).
// Worker yang macet (deadlock goroutine, pool habis) menjadi not-ready walaupun prosesnya hidup.
func (w *Worker) Ready(now time.Time, poll time.Duration) map[string]bool {
	limit := max(3*poll, 5*time.Second)
	ok := func(t time.Time) bool { return !t.IsZero() && now.Sub(t) < limit }
	out := map[string]bool{"payroll": ok(w.Payroll.LastLoop()), "maintenance": ok(w.Maintenance.LastLoop())}
	if w.Documents != nil {
		out["documents"] = ok(w.Documents.LastLoop())
	}
	return out
}

// Run menjalankan semua lane sampai ctx selesai. Saat shutdown: berhenti mengklaim, job in-flight
// diberi waktu menyelesaikan diri (deadline per kind), lalu kembali.
func (w *Worker) Run(ctx context.Context, poll time.Duration) {
	var wg sync.WaitGroup
	wg.Go(func() { w.Payroll.Run(ctx, poll) })
	wg.Go(func() { w.Maintenance.Run(ctx, poll) })
	if w.Documents != nil {
		wg.Go(func() { w.Documents.Run(ctx, poll) })
	}
	wg.Go(func() { w.Maintenance.RunScheduler(ctx, w.deps.Clock.Now, 15*time.Second) })
	wg.Go(func() {
		for {
			if err := jobs.Stats(ctx, w.deps.Pool); err != nil && ctx.Err() == nil {
				observability.Logger(ctx).Warn("worker.stats_failed", "err", err.Error())
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(15 * time.Second):
			}
		}
	})
	wg.Wait()
}

// RunOnce menjalankan satu putaran setiap lane; mengembalikan jumlah job yang dikerjakan (test, skrip).
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	total := 0
	for _, r := range w.lanes() {
		n, err := r.RunOnce(ctx)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func (w *Worker) lanes() []*jobs.Runner {
	out := []*jobs.Runner{w.Payroll, w.Maintenance}
	if w.Documents != nil {
		out = append(out, w.Documents)
	}
	return out
}

// WorkerOpsRouter: /healthz (proses hidup), /readyz (DB + skema + heartbeat setiap lane), /metrics.
// Port internal; tidak diekspos Caddy.
func WorkerOpsRouter(w *Worker, poll time.Duration) http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", func(rw http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(rw, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", func(rw http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), time.Second)
		defer cancel()
		checks := map[string]string{}
		ready := true
		if v, err := migrations.SchemaVersion(ctx, w.deps.Pool); err != nil {
			checks["database"], ready = "unreachable", false
		} else if v < migrations.MinSchemaVersion {
			checks["schema"], ready = "too old", false
		}
		for lane, ok := range w.Ready(time.Now(), poll) { //nolint:forbidigo // heartbeat teknis
			if !ok {
				checks["lane:"+lane], ready = "stalled", false
			}
		}
		status, state := http.StatusOK, "ready"
		if !ready {
			status, state = http.StatusServiceUnavailable, "not_ready"
		}
		metrics.Ready.WithLabelValues("worker").Set(boolFloat(ready))
		httpx.WriteJSON(rw, status, map[string]any{"status": state, "checks": checks})
	})
	r.Handle("/metrics", metrics.Handler())
	return r
}
