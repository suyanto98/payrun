package jobs_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/internal/platform/db"
	"payrun/internal/platform/db/dbtest"
	"payrun/internal/platform/jobs"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

// readers: pool payrun_readonly per pool app. Di PayRun baris audit platform (tenant_id NULL) hanya
// terbaca oleh role investigasi (policy audit_platform_read), bukan oleh payrun_app.
var readers sync.Map

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbtest.SkipIfShort(t)
	tdb := dbtest.New(t)
	readers.Store(tdb.App, tdb.As(t, "payrun_readonly"))
	return tdb.App
}

func jobRow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (status string, attempts int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT status, attempts FROM jobs WHERE id = $1`, id).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	return status, attempts
}

func onlyJob(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT id FROM jobs`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// effect adalah "kerja bisnis" tiruan: satu baris audit_log di tx yang sama dengan CompleteInTx.
func effect(ctx context.Context, tx pgx.Tx, j *jobs.Job, label string) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_log (id, actor_type, action, reason) VALUES ($1, 'SYSTEM', 'test.effect', $2)`, uuid.Must(uuid.NewV7()), label)
	if err != nil {
		return err
	}
	return jobs.CompleteInTx(ctx, tx, j)
}

func effects(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	ro, _ := readers.Load(pool)
	rows, err := ro.(*pgxpool.Pool).Query(context.Background(), `SELECT reason, count(*) FROM audit_log WHERE action = 'test.effect' GROUP BY reason`)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for rows.Next() {
		var r string
		var n int
		_ = rows.Scan(&r, &n)
		out[r] = n
	}
	return out
}

func enqueue(t *testing.T, pool *pgxpool.Pool, kind string, maxAttempts int) {
	t.Helper()
	if _, err := jobs.Enqueue(context.Background(), pool, jobs.Spec{Kind: kind, Payload: map[string]string{"x": "1"}, MaxAttempts: maxAttempts}); err != nil {
		t.Fatal(err)
	}
}

// TestJobRunner_LeaseExpiry_Reclaims (FS-08): worker mati setelah klaim (tidak pernah menyelesaikan).
// Setelah lease habis, worker lain mengklaim ulang dan menyelesaikan job.
func TestJobRunner_LeaseExpiry_Reclaims(t *testing.T) {
	pool := newPool(t)
	ran := make(chan string, 2)
	kind := func(worker string, block bool) jobs.Kind {
		return jobs.Kind{Name: "test.work", Handler: func(ctx context.Context, j *jobs.Job) error {
			ran <- worker
			if block { // "mati": tidak pernah kembali selama test
				<-ctx.Done()
				return ctx.Err()
			}
			return db.WithTx(ctx, pool, func(tx pgx.Tx) error { return effect(ctx, tx, j, worker) })
		}, Timeout: 3 * time.Second}
	}
	enqueue(t, pool, "test.work", 5)
	dead := jobs.NewRunner(pool, jobs.RunnerConfig{Worker: "w-dead", Lease: time.Second}, kind("w-dead", true))
	alive := jobs.NewRunner(pool, jobs.RunnerConfig{Worker: "w-alive", Lease: time.Second}, kind("w-alive", false))

	go func() { _, _ = dead.RunOnce(context.Background()) }()
	<-ran
	if n, _ := alive.RunOnce(context.Background()); n != 0 {
		t.Fatal("a job with a live lease must not be claimed by another worker")
	}
	time.Sleep(1200 * time.Millisecond)
	if n, err := alive.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("expired lease must be reclaimed: n=%d err=%v", n, err)
	}
	status, attempts := jobRow(t, pool, onlyJob(t, pool))
	if status != "succeeded" || attempts != 2 || effects(t, pool)["w-alive"] != 1 {
		t.Fatalf("want succeeded by w-alive after 2 claims, got %s attempts=%d effects=%v", status, attempts, effects(t, pool))
	}
}

// TestJobRunner_FencedCompletion_ZombieRollsBack: W1 masih bekerja ketika lease-nya diambil W2.
// W2 menyelesaikan job. Saat W1 akhirnya memanggil CompleteInTx → ErrLeaseLost → kerja bisnis W1
// di tx yang sama ikut ROLLBACK. Efek bisnis tepat satu.
func TestJobRunner_FencedCompletion_ZombieRollsBack(t *testing.T) {
	pool := newPool(t)
	started, release := make(chan struct{}), make(chan struct{})
	var zombieErr atomic.Value
	zombie := jobs.Kind{Name: "test.work", Timeout: 10 * time.Second, Handler: func(ctx context.Context, j *jobs.Job) error {
		close(started)
		<-release // GC pause / panggilan lambat: lease habis di sini
		err := db.WithTx(ctx, pool, func(tx pgx.Tx) error { return effect(ctx, tx, j, "zombie") })
		zombieErr.Store(err)
		return err
	}}
	fresh := jobs.Kind{Name: "test.work", Handler: func(ctx context.Context, j *jobs.Job) error {
		return db.WithTx(ctx, pool, func(tx pgx.Tx) error { return effect(ctx, tx, j, "fresh") })
	}}
	enqueue(t, pool, "test.work", 5)
	w1 := jobs.NewRunner(pool, jobs.RunnerConfig{Worker: "w1", Lease: time.Second}, zombie)
	w2 := jobs.NewRunner(pool, jobs.RunnerConfig{Worker: "w2", Lease: time.Second}, fresh)

	done := make(chan struct{})
	go func() { _, _ = w1.RunOnce(context.Background()); close(done) }()
	<-started
	time.Sleep(1200 * time.Millisecond)
	if n, _ := w2.RunOnce(context.Background()); n != 1 {
		t.Fatal("w2 must take over the expired lease")
	}
	close(release)
	<-done

	if err, _ := zombieErr.Load().(error); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("zombie completion must fail with ErrLeaseLost, got %v", err)
	}
	if e := effects(t, pool); e["zombie"] != 0 || e["fresh"] != 1 {
		t.Fatalf("zombie's business write must be rolled back: %v", e)
	}
	if status, _ := jobRow(t, pool, onlyJob(t, pool)); status != "succeeded" {
		t.Fatalf("job status = %s", status)
	}
}

// TestJobRunner_PermanentError_DeadImmediately: data yang pasti rusak tidak membuang 5 percobaan;
// OnDead berjalan di tx yang sama dengan penandaan dead.
func TestJobRunner_PermanentError_DeadImmediately(t *testing.T) {
	pool := newPool(t)
	var onDead atomic.Int32
	k := jobs.Kind{Name: "test.work", MaxAttempts: 5,
		Handler: func(context.Context, *jobs.Job) error {
			return jobs.Permanent(errors.New("referenced row does not exist"))
		},
		OnDead: func(context.Context, pgx.Tx, *jobs.Job) error { onDead.Add(1); return nil }}
	enqueue(t, pool, "test.work", 5)
	r := jobs.NewRunner(pool, jobs.RunnerConfig{Worker: "w"}, k)
	_, _ = r.RunOnce(context.Background())
	status, attempts := jobRow(t, pool, onlyJob(t, pool))
	if status != "dead" || attempts != 1 || onDead.Load() != 1 {
		t.Fatalf("permanent error: want dead after 1 attempt + OnDead, got %s attempts=%d onDead=%d", status, attempts, onDead.Load())
	}
}

// Error sementara → retry dengan backoff sampai max_attempts, lalu dead. Panic = percobaan gagal,
// worker tetap hidup.
func TestJobRunner_RetryThenDead_PanicSurvives(t *testing.T) {
	pool := newPool(t)
	var calls atomic.Int32
	k := jobs.Kind{Name: "test.work", MaxAttempts: 3, Backoff: func(int) time.Duration { return 0 },
		Handler: func(context.Context, *jobs.Job) error {
			if calls.Add(1) == 1 {
				panic("boom")
			}
			return errors.New("provider unavailable")
		}}
	enqueue(t, pool, "test.work", 3)
	r := jobs.NewRunner(pool, jobs.RunnerConfig{Worker: "w"}, k)
	for range 5 {
		_, _ = r.RunOnce(context.Background())
	}
	status, attempts := jobRow(t, pool, onlyJob(t, pool))
	if status != "dead" || attempts != 3 || calls.Load() != 3 {
		t.Fatalf("want dead after exactly 3 attempts, got %s attempts=%d calls=%d", status, attempts, calls.Load())
	}
	var lastErr string
	_ = pool.QueryRow(context.Background(), `SELECT last_error FROM jobs`).Scan(&lastErr)
	if lastErr != "provider unavailable" {
		t.Fatalf("last_error = %q", lastErr)
	}
}

// TestScheduler_DedupeKey_NoDoubleSchedule: banyak replika memanggil Tick bersamaan dan berulang,
// termasuk setelah job bucket itu selesai → tetap satu job per bucket. Bucket berikutnya → job baru.
func TestScheduler_DedupeKey_NoDoubleSchedule(t *testing.T) {
	pool := newPool(t)
	noop := jobs.Kind{Name: "test.sweep", Handler: func(context.Context, *jobs.Job) error { return nil }}
	now := time.Date(2026, 10, 4, 10, 0, 7, 0, time.UTC)
	var replicas []*jobs.Runner
	for i := range 5 {
		r := jobs.NewRunner(pool, jobs.RunnerConfig{Worker: "w" + string(rune('a'+i))}, noop)
		r.Every("test.sweep", time.Minute)
		replicas = append(replicas, r)
	}
	var wg sync.WaitGroup
	for _, r := range replicas {
		wg.Add(1)
		go func() { defer wg.Done(); _ = r.Tick(context.Background(), now) }()
	}
	wg.Wait()
	count := func() int {
		var n int
		_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs`).Scan(&n)
		return n
	}
	if n := count(); n != 1 {
		t.Fatalf("5 replicas ticking the same bucket: want 1 job, got %d", n)
	}
	_, _ = replicas[0].RunOnce(context.Background()) // job bucket ini selesai
	_ = replicas[1].Tick(context.Background(), now.Add(30*time.Second))
	if n := count(); n != 1 {
		t.Fatalf("a finished bucket must not be scheduled again, got %d jobs", n)
	}
	_ = replicas[2].Tick(context.Background(), now.Add(time.Minute))
	if n := count(); n != 2 {
		t.Fatalf("next bucket must get its own job, got %d", n)
	}
}
