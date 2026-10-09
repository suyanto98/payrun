//go:build testhooks

package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/internal/app"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/testhook"
)

// Exit code proses worker yang "di-kill" di tengah txWrite chunk.
const killedExitCode = 3

// Env proses anak: chunk ke berapa yang dibunuh (default 2) dan ukuran chunk (default 2).
const (
	helperKillChunk = "PAYRUN_TEST_HELPER_KILL_CHUNK"
	helperChunkSize = "PAYRUN_TEST_HELPER_CHUNK_SIZE"
)

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

func init() { helperProcess = runKilledWorker }

// runKilledWorker berjalan di proses anak (binary test yang sama, lihat TestMain). Worker sungguhan
// dengan lease 1 dtk mengerjakan plan dan chunk sebelum chunk ke-K; di chunk ke-K (default 2) proses
// keluar dengan os.Exit tepat sebelum COMMIT txWrite: koneksi putus, Postgres me-rollback, dan job chunk 2 tetap `running`
// dengan lease milik proses yang sudah mati. Persis worker yang di-kill (SIGKILL / OOM).
func runKilledWorker() int {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv(helperWorkerDSN))
	if err != nil {
		return 2
	}
	defer pool.Close()
	killAt := int32(envInt(helperKillChunk, 2)) //nolint:gosec // G115: nilai kecil dari test
	var chunks atomic.Int32
	var armed atomic.Bool
	testhook.Set("payroll.calc_chunk.before_write", func(context.Context) error {
		if chunks.Add(1) == killAt {
			armed.Store(true) // commit berikutnya di goroutine ini = txWrite chunk 2
		}
		return nil
	})
	testhook.Set("db.before_commit", func(context.Context) error {
		if armed.Load() {
			os.Exit(killedExitCode)
		}
		return nil
	})
	deps := app.Deps{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Pool: pool, Clock: clock.System{}, IDs: idgen.V7{}}
	// Konkurensi 1: chunk dikerjakan berurutan, sehingga "chunk ke-K" deterministik.
	w := app.NewWorker(deps, app.WorkerConfig{ID: "victim:" + uuid.NewString()[:8], Lease: time.Second,
		ChunkSize: envInt(helperChunkSize, 2), PayrollConcurrency: 1})
	for range 200 {
		if n, err := w.RunOnce(ctx); err != nil || n == 0 {
			break
		}
	}
	return 0 // tidak pernah sampai di chunk 2: proses induk menganggapnya gagal
}

// TestPayrollRun_WorkerKilledMidChunk_ResumesWithoutDuplicates (DoD S4, PFS-01): proses worker mati di
// tengah txWrite chunk 2 dari 2. Tidak ada tulisan chunk 2 yang tersisa; setelah lease habis worker
// lain mengambil alih dan run selesai dengan tepat satu item, satu snapshot per employee.
func TestPayrollRun_WorkerKilledMidChunk_ResumesWithoutDuplicates(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	p := f.tokens[roleP]
	run := e.createRun(t, p, "2026-03")
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/calculate", p, key(), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("calculate: %d %v", r.StatusCode, b)
	}

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperWorkerDSN+"="+e.pool.Config().ConnString())
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != killedExitCode {
		t.Fatalf("worker process must die mid-chunk (exit %d), got %v\n%s", killedExitCode, err, out)
	}

	mid := e.runState(t, f.id, run)
	if mid.status != "CALCULATING" || mid.chunksDone != 1 || mid.chunksTotal != 2 || mid.items != 2 || mid.snaps != 2 {
		t.Fatalf("after the kill: want chunk 1 committed only (2 items, 1/2 chunks), got %+v", mid)
	}
	var lockedBy string
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT locked_by FROM jobs WHERE kind = 'payroll.calc_chunk' AND status = 'running'`).Scan(&lockedBy)
	})
	if lockedBy == "" {
		t.Fatal("chunk 2 must still be leased by the dead worker")
	}

	time.Sleep(1200 * time.Millisecond) // lease 1 dtk milik proses yang mati kedaluwarsa
	e.drainJobs(t)
	s := e.runState(t, f.id, run)
	if s.status != "CALCULATED" || s.chunksDone != 2 || s.items != 3 || s.employees != 3 || s.snaps != 3 {
		t.Fatalf("after resume: want CALCULATED with exactly one item per employee, got %+v", s)
	}
	var attempts int
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT attempts FROM jobs WHERE kind = 'payroll.calc_chunk' AND payload->>'chunk_no' = '2'`).Scan(&attempts)
	})
	if attempts != 2 {
		t.Fatalf("chunk 2: want 2 attempts (killed + resumed), got %d", attempts)
	}
}

// TestCalcChunk_LeaseStolen_FencedCommitRollsBack (DoD S4): worker W1 tertahan (GC pause, jaringan)
// setelah menghitung chunk, lalu lease-nya diambil pemilik baru. Saat W1 lanjut, seluruh txWrite-nya
// (item, line, snapshot, chunks_done) rollback karena CompleteInTx tidak menemukan lease miliknya.
// Run masih CALCULATING, jadi yang diuji benar-benar fencing, bukan guard status/versi.
func TestCalcChunk_LeaseStolen_FencedCommitRollsBack(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	p := f.tokens[roleP]
	run := e.createRun(t, p, "2026-03")
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/calculate", p, key(), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("calculate: %d %v", r.StatusCode, b)
	}
	w1 := e.workerWith(app.WorkerConfig{PayrollConcurrency: 1})
	if n, err := w1.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("plan: n=%d err=%v", n, err)
	}

	reached, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	reset := testhook.Set("payroll.calc_chunk.before_write", func(ctx context.Context) error {
		if first.CompareAndSwap(false, true) {
			close(reached)
			<-release
		}
		return nil
	})
	t.Cleanup(reset)
	done := make(chan struct{})
	go func() { _, _ = w1.RunOnce(context.Background()); close(done) }() // klaim satu chunk (konkurensi 1)
	<-reached

	// Lease diambil alih (pemilik baru = "thief", masih hidup selama W1 melanjutkan).
	var stolen uuid.UUID
	if err := e.tdb.Super(t).QueryRow(context.Background(), `UPDATE payrun.jobs SET locked_by = 'thief', locked_until = now() + interval '1 hour'
		WHERE kind = 'payroll.calc_chunk' AND status = 'running' RETURNING id`).Scan(&stolen); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done

	mid := e.runState(t, f.id, run)
	if mid.status != "CALCULATING" || mid.chunksDone != 0 || mid.items != 0 || mid.snaps != 0 {
		t.Fatalf("fenced commit must roll back everything, got %+v", mid)
	}
	var status, owner string
	_ = e.tdb.Super(t).QueryRow(context.Background(), `SELECT status, locked_by FROM payrun.jobs WHERE id = $1`, stolen).Scan(&status, &owner)
	if status != "running" || owner != "thief" {
		t.Fatalf("the zombie must not touch the stolen job: got %s / %s", status, owner)
	}

	// Pemilik baru juga mati: lease habis, worker normal menyelesaikan run tanpa duplikat.
	if _, err := e.tdb.Super(t).Exec(context.Background(), `UPDATE payrun.jobs SET locked_until = now() - interval '1 second' WHERE id = $1`, stolen); err != nil {
		t.Fatal(err)
	}
	e.drainJobs(t)
	s := e.runState(t, f.id, run)
	if s.status != "CALCULATED" || s.items != 3 || s.employees != 3 || s.snaps != 3 || s.chunksDone != s.chunksTotal {
		t.Fatalf("after takeover: want CALCULATED with 3 unique items, got %+v", s)
	}
}
