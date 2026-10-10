//go:build testhooks

package app_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"

	"payrun/internal/platform/testhook"
)

// TestCalcChunk_PanicExhaustsAttempts_RunFailed (PFS-05): chunk yang panic di setiap percobaan dicoba ulang
// sampai MaxAttempts (runner memulihkan panic, backoff di antara percobaan), lalu dead → OnDead menandai run
// FAILED dengan alasan. Tidak ada item yang tersisa dari chunk itu (setiap percobaan di-rollback). Setelah
// penyebabnya hilang, recalculate dari FAILED menghasilkan run lengkap.
func TestCalcChunk_PanicExhaustsAttempts_RunFailed(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	p := f.tokens[roleP]
	run := e.createRun(t, p, "2026-03")
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/calculate", p, key(), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("calculate: %d %v", r.StatusCode, b)
	}
	calls := 0
	reset := testhook.Set("payroll.calc_chunk.before_write", func(context.Context) error {
		calls++
		panic("PFS-05: chunk always panics")
	})
	t.Cleanup(reset)
	for range 20 {
		if _, err := e.worker.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		// Lewati backoff: job yang dijadwal ulang langsung bisa di-claim lagi.
		if _, err := e.pool.Exec(context.Background(), `UPDATE jobs SET run_at = now() WHERE status = 'queued'`); err != nil {
			t.Fatal(err)
		}
		if e.runState(t, f.id, run).status == "FAILED" {
			break
		}
	}
	s := e.runState(t, f.id, run)
	if s.status != "FAILED" || s.items != 0 || s.chunksDone != 0 {
		t.Fatalf("after attempts are exhausted: want FAILED with 0 items, got %+v", s)
	}
	var dead, attempts int
	var reason *string
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(max(attempts), 0) FROM jobs
			WHERE kind = 'payroll.calc_chunk' AND status = 'dead' AND payload->>'run_id' = $1`, run.String()).Scan(&dead, &attempts); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT failure_reason FROM payroll_runs WHERE id = $1`, run).Scan(&reason)
	})
	if dead == 0 || attempts != 5 || calls < 5 || reason == nil || *reason == "" {
		t.Fatalf("dead chunk jobs %d, attempts %d (want 5), hook calls %d, failure_reason %v", dead, attempts, calls, reason)
	}

	reset()
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/calculate", p, key(), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("recalculate from FAILED: %d %v", r.StatusCode, b)
	}
	e.drainJobs(t)
	if s := e.runState(t, f.id, run); s.status != "CALCULATED" || s.items == 0 || s.items != s.employees || s.items != s.snaps {
		t.Fatalf("recalculate after the fault is gone: %+v", s)
	}
}
