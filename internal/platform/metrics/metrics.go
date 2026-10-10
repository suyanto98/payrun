// Package metrics: satu tempat untuk semua metrik Prometheus (katalog docs/design (Step 8 `01`) §3).
//
// Aturan:
//   - event → counter (_total); keadaan hasil verifikasi/query periodik → gauge;
//   - label hanya dari himpunan TERBATAS (route template, kind job, code katalog error, outcome) —
//     tidak pernah tenant_id, user_id, employee_id, email, atau nominal (kardinalitas + PII);
//   - metrik INVARIANT (payrun_*_violations_*) normalnya selalu 0; alert-nya critical.
package metrics

import (
	"net/http"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry sendiri (bukan default global), supaya test bisa membaca nilai tanpa efek samping library lain.
var Registry = prometheus.NewRegistry()

var (
	httpBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2.5, 5} // 0.2/0.3 = batas SLO
	jobBuckets  = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}
	runBuckets  = []float64{1, 5, 10, 30, 60, 120, 300, 600, 1800}
)

func counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	Registry.MustRegister(c)
	return c
}

func gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	Registry.MustRegister(g)
	return g
}

func histogram(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
	Registry.MustRegister(h)
	return h
}

// ---------------------------------------------------------------- platform (app_)
var (
	BuildInfo        = gauge("app_build_info", "Running version (always 1).", "service", "version", "go_version")
	Ready            = gauge("app_ready", "1 = ready to serve, 0 = not ready.", "service")
	HTTPRequests     = counter("app_http_requests_total", "Completed HTTP requests.", "route", "method", "status")
	HTTPDuration     = histogram("app_http_request_duration_seconds", "Server-side HTTP latency.", httpBuckets, "route", "method")
	HTTPInFlight     = gauge("app_http_in_flight_requests", "Requests being served.")
	Errors           = counter("app_errors_total", "Error responses by catalogue code.", "code")
	RateLimited      = counter("app_rate_limited_total", "429 responses by tier.", "tier")
	DBTxRetries      = counter("app_db_tx_retries_total", "WithTx retries.", "reason")
	DBDeadlocks      = counter("app_db_deadlocks_total", "40P01 detected (before retry).")
	DBLockTimeouts   = counter("app_db_lock_timeouts_total", "55P03 lock timeouts.", "kind")
	JobsEnqueued     = counter("app_job_enqueued_total", "Jobs written to the outbox.", "kind")
	JobDuration      = histogram("app_job_duration_seconds", "Job execution time.", jobBuckets, "kind", "outcome")
	JobFailures      = counter("app_job_failures_total", "Failed job attempts.", "kind", "class")
	JobDead          = counter("app_job_dead_total", "Jobs moved to dead (DLQ).", "kind")
	JobFenced        = counter("app_job_fenced_total", "CompleteInTx found the lease taken: business tx rolled back (zombie worker).", "kind")
	JobLeaseLost     = counter("app_job_lease_lost_total", "Heartbeat found the lease taken.", "kind")
	JobQueueDepth    = gauge("app_job_queue_depth", "Jobs per kind and status (periodic query).", "kind", "status")
	JobOldestAge     = gauge("app_job_oldest_age_seconds", "Age of the oldest due queued job per kind.", "kind")
	OutboxLag        = gauge("app_outbox_lag_seconds", "Age of the oldest never-claimed due job: how long side effects are delayed after commit.")
	Outbound         = counter("app_outbound_requests_total", "Outbound HTTP calls.", "target", "outcome")
	OutboundDuration = histogram("app_outbound_request_duration_seconds", "Outbound HTTP latency.", httpBuckets, "target")
	AuthnFailures    = counter("app_authn_failures_total", "Authentication failures.", "reason")
	AuthnLockouts    = counter("app_authn_lockouts_total", "Accounts locked by the progressive lockout (C-03).")
	RefreshReuse     = counter("app_refresh_reuse_detected_total", "Refresh token reuse detected (family revoked).")
	IdemReplays      = counter("app_idempotency_replays_total", "Replayed responses (Idempotent-Replayed).", "route")
	IdemKeyReused    = counter("app_idempotency_key_reused_total", "422 IDEMPOTENCY_KEY_REUSED.", "route")
)

// ---------------------------------------------------------------- PayRun (payrun_)
// Metrik domain ditambahkan per sprint bersama fiturnya (Step 8 `01` §3), contoh
// payrun_payroll_runs_total (S4).
var (
	// INVARIANT (C-46): baris tenant lain lolos ke decode repository. Normalnya selalu 0; > 0 berarti
	// RLS gagal dengan cara yang tidak terduga → alert critical.
	TenantIsolationViolations = counter("payrun_tenant_isolation_violations_total",
		"Rows of another tenant seen by a tenant-scoped repository (must stay 0).", "table")
	// INVARIANT: trigger DB menolak perubahan yang seharusnya sudah ditolak aplikasi lebih dulu
	// (P0001 run/adjustment final, P0002 tanggal di periode FINALIZED). > 0 = guard aplikasi bocor.
	FinalizedMutationAttempts = counter("payrun_finalized_mutation_attempts_total",
		"Writes to finalized payroll data stopped only by a DB trigger (must stay 0).", "table")
	PeriodLockBypass = counter("payrun_period_lock_bypass_total",
		"Writes into a FINALIZED period stopped only by the DB guard trigger (must stay 0).", "table")
	// INVARIANT: verify-run menemukan hasil yang tidak bisa direproduksi dari snapshot (PI-12).
	ReproducibilityFailures = counter("payrun_reproducibility_failures_total", "verify-run items whose recomputation differs (must stay 0).")

	// Run payroll (S4–S7, Step 8 `01` §3).
	PayrollRuns         = counter("payrun_payroll_runs_total", "Payroll run transitions.", "outcome")
	PayrollRunDuration  = histogram("payrun_payroll_run_duration_seconds", "calc_started_at → CALCULATED.", runBuckets, "size_bucket")
	ItemsCalculated     = counter("payrun_payroll_items_calculated_total", "Payroll items written by calc chunks.", "status")
	ItemErrors          = counter("payrun_payroll_item_errors_total", "ERROR item codes from the engine.", "code")
	ChunkDuration       = histogram("payrun_payroll_chunk_duration_seconds", "payroll.calc_chunk duration (committed).", jobBuckets)
	StaleRejections     = counter("payrun_run_stale_rejections_total", "Approve/finalize rejected with RUN_STALE.", "action")
	StaleCandidates     = histogram("payrun_approve_stale_candidates", "Changed-input candidates checked at approve/finalize.", []float64{1, 5, 10, 50, 100, 500, 1000})
	Approvals           = counter("payrun_approvals_total", "Approval decisions.", "subject", "decision")
	SelfApproved        = counter("payrun_approval_self_approved_total", "Payroll runs approved by their own submitter (sole admin).")
	PeriodLockRejection = counter("payrun_period_lock_rejections_total", "Input writes rejected with PERIOD_LOCKED by the app guard.", "source")
	// Setiap pembacaan PII penuh (W6, S4) dan undangan dibuat/diterima (S2).
	InvitationsTotal = counter("payrun_invitations_total", "Invitation lifecycle events.", "event")

	// Dokumen (S7, PF-22/PI-17).
	PayslipsRendered      = counter("payrun_payslips_rendered_total", "Payslip PDFs rendered or failed.", "outcome")
	PayslipRenderDuration = histogram("payrun_payslip_render_duration_seconds", "payslip.render batch duration (20 slips).", jobBuckets)
	PayslipAccess         = counter("payrun_payslip_access_total", "Payslip reads/downloads by actor kind (admin = audited).", "actor")
)

func init() {
	Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	// Metrik INV dan outcome diinisialisasi 0 untuk setiap label yang diketahui: increase() pada seri
	// yang baru lahir di nilai 1 tidak terlihat, dan dashboard perlu "0" bukan "No data".
	for _, t := range []string{"payroll_runs", "payroll_periods", "payroll_items", "payroll_lines", "adjustments", "payslips", "employees"} {
		TenantIsolationViolations.WithLabelValues(t)
		FinalizedMutationAttempts.WithLabelValues(t)
		PeriodLockBypass.WithLabelValues(t)
	}
	for _, o := range []string{"calculated", "failed", "approved", "rejected", "finalized", "cancelled"} {
		PayrollRuns.WithLabelValues(o)
	}
	for _, o := range []string{"rendered", "failed"} {
		PayslipsRendered.WithLabelValues(o)
	}
}

// SizeBucket: label kardinalitas rendah untuk ukuran run (jumlah employee eligible).
func SizeBucket(n int32) string {
	switch {
	case n < 100:
		return "lt_100"
	case n < 1000:
		return "lt_1k"
	case n < 10000:
		return "lt_10k"
	}
	return "ge_10k"
}

// SetBuildInfo dipanggil sekali saat start.
func SetBuildInfo(service, version string) {
	BuildInfo.WithLabelValues(service, version, runtime.Version()).Set(1)
}

// Handler melayani /metrics (port ops internal, tidak lewat Caddy).
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{Registry: Registry})
}

// Since mengembalikan detik sejak t (helper histogram).
func Since(t time.Time) float64 { return time.Since(t).Seconds() } //nolint:forbidigo // durasi teknis, bukan waktu bisnis

// Start menandai awal pengukuran durasi teknis (pasangan Since); bukan waktu bisnis (C-31).
func Start() time.Time { return time.Now() } //nolint:forbidigo // durasi teknis, bukan waktu bisnis
