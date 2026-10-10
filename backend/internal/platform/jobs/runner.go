package jobs

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"

	"payrun/internal/platform/db"
	"payrun/internal/platform/jobs/store"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/observability"
	"payrun/internal/platform/tracing"
)

// Handler mengerjakan satu job. Kalau handler menulis DB, ia memanggil CompleteInTx di tx bisnisnya;
// kalau tidak, runner menyelesaikan job sendiri saat handler mengembalikan nil.
type Handler func(ctx context.Context, j *Job) error

// Kind adalah definisi satu jenis job (katalog docs/design/20-background-jobs.md §3).
type Kind struct {
	Name        string
	Handler     Handler
	MaxAttempts int                                                // default 10
	Timeout     time.Duration                                      // default 60 dtk
	Backoff     func(attempt int) time.Duration                    // default DefaultBackoff
	Heartbeat   bool                                               // job panjang: perpanjang lease setiap lease/3
	OnDead      func(ctx context.Context, tx pgx.Tx, j *Job) error // di tx yang sama dengan penandaan dead
}

// Runner adalah satu "lane" (bulkhead): loop klaim + pool goroutine sendiri untuk sekumpulan kind.
type Runner struct {
	lastLoop    atomic.Int64 // unix nano iterasi loop klaim terakhir (readiness worker)
	pool        *pgxpool.Pool
	kinds       map[string]Kind
	names       []string
	worker      string
	lease       time.Duration
	concurrency int
	schedules   []schedule
}

// RunnerConfig: Worker harus unik per proses (hostname:pid:uuid), supaya worker yang restart tidak
// pernah dianggap pemilik lease lamanya.
type RunnerConfig struct {
	Worker      string
	Lease       time.Duration // default 30 dtk
	Concurrency int           // default 4
}

func NewRunner(pool *pgxpool.Pool, cfg RunnerConfig, kinds ...Kind) *Runner {
	if cfg.Lease == 0 {
		cfg.Lease = 30 * time.Second
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 4
	}
	r := &Runner{pool: pool, kinds: map[string]Kind{}, worker: cfg.Worker, lease: cfg.Lease, concurrency: cfg.Concurrency}
	for _, k := range kinds {
		if k.MaxAttempts == 0 {
			k.MaxAttempts = 10
		}
		if k.Timeout == 0 {
			k.Timeout = 60 * time.Second
		}
		if k.Backoff == nil {
			k.Backoff = DefaultBackoff
		}
		r.kinds[k.Name] = k
		r.names = append(r.names, k.Name)
	}
	return r
}

// LastLoop: waktu iterasi loop klaim terakhir (nol kalau belum pernah jalan).
func (r *Runner) LastLoop() time.Time {
	if n := r.lastLoop.Load(); n > 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// MaxAttempts kind (untuk producer yang meng-enqueue).
func (r *Runner) MaxAttempts(kind string) int { return r.kinds[kind].MaxAttempts }

// Run mengklaim dan mengerjakan job sampai ctx selesai, lalu menunggu job in-flight (graceful shutdown).
//
// Saat semua slot terpakai, loop menunggu sinyal "slot kosong" (bukan tidur selama interval): PR-1 (S9)
// mengukur bahwa tidur 500 ms setiap kali kapasitas penuh membatasi kalkulasi ke ±8 chunk/dtk berapa pun
// cepatnya chunk (10.000 karyawan = 25 dtk, worker dan DB setengah menganggur). Interval hanya dipakai
// kalau antrean kosong (klaim mengembalikan lebih sedikit dari slot yang tersedia).
func (r *Runner) Run(ctx context.Context, interval time.Duration) {
	sem := make(chan struct{}, r.concurrency)
	freed := make(chan struct{}, r.concurrency)
	var wg sync.WaitGroup
	log := observability.Logger(ctx)
	for ctx.Err() == nil {
		r.lastLoop.Store(time.Now().UnixNano()) //nolint:forbidigo // heartbeat teknis
		free := r.concurrency - len(sem)
		if free == 0 { // kapasitas penuh: tunggu satu job selesai (atau heartbeat readiness tiap interval)
			select {
			case <-ctx.Done():
			case <-freed:
			case <-time.After(interval):
			}
			continue
		}
		// Jangan klaim melebihi kapasitas: lease bisa habis selagi job menunggu di memori.
		claimed, err := r.claim(ctx, free)
		if err != nil && ctx.Err() == nil {
			log.Error("jobs.claim_failed", "err", err.Error())
		}
		for _, j := range claimed {
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() {
					<-sem
					select { // non-blocking: loop yang sedang mengklaim akan melihat slot kosong di iterasi berikutnya
					case freed <- struct{}{}:
					default:
					}
					wg.Done()
				}()
				r.process(ctx, j)
			}()
		}
		if len(claimed) == free {
			continue // antrean mungkin masih berisi: klaim lagi begitu ada slot
		}
		select { // antrean kosong (atau klaim gagal): tunggu interval
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
	wg.Wait() // job yang dibatalkan dikembalikan ke antrean oleh process (Release)
}

// RunOnce mengklaim satu batch dan mengerjakannya berurutan (dipakai test dan skrip).
func (r *Runner) RunOnce(ctx context.Context) (int, error) {
	claimed, err := r.claim(ctx, r.concurrency)
	if err != nil {
		return 0, err
	}
	for _, j := range claimed {
		r.process(ctx, j)
	}
	return len(claimed), nil
}

func (r *Runner) claim(ctx context.Context, batch int) ([]*Job, error) {
	rows, err := store.New(r.pool).ClaimJobs(ctx, store.ClaimJobsParams{
		Worker: &r.worker, LeaseSecs: r.lease.Seconds(), Kinds: r.names, Batch: int32(batch), //nolint:gosec // G115: batch kecil
	})
	if err != nil {
		return nil, db.MapError(err)
	}
	out := make([]*Job, 0, len(rows))
	for _, row := range rows {
		j := &Job{ID: row.ID, Kind: row.Kind, Payload: row.Payload, Attempts: int(row.Attempts), MaxAttempts: int(row.MaxAttempts), worker: r.worker}
		if row.TenantID != nil {
			j.TenantID = *row.TenantID
		}
		if row.CorrelationID != nil {
			j.CorrelationID = *row.CorrelationID
		}
		if row.TraceParent != nil {
			j.TraceParent = *row.TraceParent
		}
		out = append(out, j)
	}
	return out, nil
}

func (r *Runner) process(parent context.Context, j *Job) {
	k := r.kinds[j.Kind]
	log := observability.Logger(parent).With("job_id", j.ID.String(), "kind", j.Kind, "attempt", j.Attempts, "correlation_id", j.CorrelationID)
	if j.TenantID != uuid.Nil {
		log = log.With("tenant_id", j.TenantID.String()) // PF-17: tenant_id di setiap log job
	}
	// Context handler TIDAK diturunkan dari parent: saat shutdown, job in-flight diberi kesempatan
	// selesai (sampai timeout-nya) alih-alih langsung dibatalkan di tengah panggilan bank.
	ctx, cancel := context.WithTimeout(observability.WithLogger(context.WithoutCancel(parent), log), k.Timeout)
	defer cancel()
	// Span ROOT baru dengan LINK ke request producer (bukan child): Jaeger bisa melompat dari
	// POST /payouts ke job payout.submit tanpa trace berdurasi menit/jam.
	ctx, span := tracing.StartLinked(ctx, "job "+j.Kind, j.TraceParent,
		attribute.String("app.job_id", j.ID.String()), attribute.String("app.job_kind", j.Kind),
		attribute.Int("app.attempt", j.Attempts), attribute.String("app.correlation_id", j.CorrelationID))
	defer span.End()
	if k.Heartbeat {
		go r.heartbeat(ctx, cancel, j)
	}

	start := time.Now() //nolint:forbidigo // durasi teknis
	err := safeCall(ctx, k.Handler, j)
	bg := context.WithoutCancel(ctx) // pencatatan hasil tetap jalan walaupun ctx handler sudah habis
	outcome := "ok"
	defer func() { metrics.JobDuration.WithLabelValues(j.Kind, outcome).Observe(metrics.Since(start)) }()
	switch {
	case err == nil && !j.completed:
		err = db.WithTx(bg, r.pool, func(tx pgx.Tx) error { return CompleteInTx(bg, tx, j) })
		if errors.Is(err, ErrLeaseLost) {
			outcome = "fenced"
			metrics.JobFenced.WithLabelValues(j.Kind).Inc()
			log.Warn("jobs.lease_lost")
		} else if err != nil {
			log.Error("jobs.complete_failed", "err", err.Error())
		}
	case err == nil:
		log.Info("jobs.succeeded")
	case errors.Is(err, ErrLeaseLost):
		outcome = "fenced"
		metrics.JobFenced.WithLabelValues(j.Kind).Inc()
		log.Warn("jobs.lease_lost", "detail", "business transaction rolled back; the new lease owner finishes the job")
	default:
		outcome = r.fail(bg, k, j, err, log)
	}
}

func safeCall(ctx context.Context, h Handler, j *Job) (err error) {
	defer func() {
		if p := recover(); p != nil { // panic = percobaan gagal; worker tetap hidup
			stack := string(debug.Stack())
			if len(stack) > 2048 {
				stack = stack[:2048]
			}
			err = fmt.Errorf("panic: %v\n%s", p, stack)
		}
	}()
	return h(ctx, j)
}

func (r *Runner) fail(ctx context.Context, k Kind, j *Job, cause error, log interface {
	Warn(string, ...any)
	Error(string, ...any)
}) (outcome string) {
	class := "retryable"
	if IsPermanent(cause) {
		class = "permanent"
	}
	metrics.JobFailures.WithLabelValues(j.Kind, class).Inc()
	outcome = "retry"
	msg := cause.Error()
	if len(msg) > 2048 {
		msg = msg[:2048]
	}
	permanent := IsPermanent(cause)
	err := db.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		status, err := store.New(tx).FailJob(ctx, store.FailJobParams{
			Dead: permanent, BackoffSecs: k.Backoff(j.Attempts).Seconds(), LastError: &msg, ID: j.ID, Worker: &j.worker,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		if status == "dead" {
			outcome = "dead"
			metrics.JobDead.WithLabelValues(j.Kind).Inc()
			log.Error("jobs.dead", "err", msg, "permanent", permanent)
			if k.OnDead != nil { // bawa state bisnis ke kondisi terminal yang jujur, di tx yang sama
				return k.OnDead(ctx, tx, j)
			}
			return nil
		}
		log.Warn("jobs.retry_scheduled", "err", msg)
		return nil
	})
	if err != nil && !errors.Is(err, ErrLeaseLost) {
		log.Error("jobs.fail_update_failed", "err", err.Error())
	}
	return outcome
}

// heartbeat memperpanjang lease job panjang. Lease hilang → ctx handler dibatalkan; CompleteInTx
// juga akan gagal (fencing), jadi tidak ada tulisan dari worker zombie.
func (r *Runner) heartbeat(ctx context.Context, cancel context.CancelFunc, j *Job) {
	t := time.NewTicker(r.lease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := store.New(r.pool).HeartbeatJob(ctx, store.HeartbeatJobParams{LeaseSecs: r.lease.Seconds(), ID: j.ID, Worker: &j.worker})
			if err == nil && n == 0 {
				metrics.JobLeaseLost.WithLabelValues(j.Kind).Inc()
				observability.Logger(ctx).Warn("jobs.heartbeat_lost_lease")
				cancel()
				return
			}
		}
	}
}

// ---------------------------------------------------------------- scheduler

type schedule struct {
	kind  string
	every time.Duration
}

// Every mendaftarkan job terjadwal pada runner ini (kind harus terdaftar di runner yang sama).
func (r *Runner) Every(kind string, every time.Duration) {
	r.schedules = append(r.schedules, schedule{kind: kind, every: every})
}

// Tick meng-enqueue job terjadwal untuk bucket waktu saat ini. Aman dipanggil oleh banyak replika
// sekaligus dan berulang kali: dedupe key "<kind>:<bucket>" menjamin satu job per bucket.
func (r *Runner) Tick(ctx context.Context, now time.Time) error {
	q := store.New(r.pool)
	for _, s := range r.schedules {
		bucket := now.UTC().Truncate(s.every).Format(time.RFC3339)
		if _, err := q.EnqueueScheduledJob(ctx, store.EnqueueScheduledJobParams{
			ID: uuid.Must(uuid.NewV7()), Kind: s.kind, MaxAttempts: int32(r.kinds[s.kind].MaxAttempts), DedupeKey: s.kind + ":" + bucket, //nolint:gosec // G115
		}); err != nil {
			return db.MapError(err)
		}
	}
	return nil
}

// RunScheduler memanggil Tick setiap interval (desain: 15 dtk) sampai ctx selesai.
func (r *Runner) RunScheduler(ctx context.Context, now func() time.Time, interval time.Duration) {
	for {
		if err := r.Tick(ctx, now()); err != nil && ctx.Err() == nil {
			observability.Logger(ctx).Error("jobs.schedule_failed", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Cleanup menghapus job lama (dipakai handler maintenance.jobs_cleanup).
func Cleanup(ctx context.Context, q db.DBTX) (int64, error) {
	n, err := store.New(q).DeleteFinishedJobs(ctx)
	return n, db.MapError(err)
}
