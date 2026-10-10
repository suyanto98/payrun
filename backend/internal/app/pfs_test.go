package app_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/shopspring/decimal"

	"payrun/internal/platform/jobs"
	"payrun/internal/platform/metrics"
)

// TestJob_TenantRunMismatch_NoOpAndAlert (PFS-09, C-43): job payroll milik tenant Beta yang payload-nya
// menunjuk run dan karyawan Alpha (bug enqueue atau baris jobs yang dimanipulasi). Handler berjalan di
// bawah RLS tenant job, sehingga run Alpha tidak terlihat: job langsung dead (permanen, tanpa retry), run
// Alpha tidak berubah, tidak ada item yang tertulis di tenant mana pun, dan app_job_dead_total naik (alert
// JobDead di deploy/prometheus/rules/platform.yml).
func TestJob_TenantRunMismatch_NoOpAndAlert(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	alpha := e.newTenantFx(t, "alpha")
	beta := e.newTenantFx(t, "beta")
	p := alpha.tokens[roleP]
	run := e.createRun(t, p, "2026-03")
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/calculate", p, key(), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("calculate: %d %v", r.StatusCode, b)
	}
	e.drainJobs(t)
	before := e.runState(t, alpha.id, run)
	if before.status != "CALCULATED" || before.items == 0 {
		t.Fatalf("precondition: %+v", before)
	}
	deadPlan := testutil.ToFloat64(metrics.JobDead.WithLabelValues("payroll.plan"))
	deadChunk := testutil.ToFloat64(metrics.JobDead.WithLabelValues("payroll.calc_chunk"))

	// Trigger jobs_enforce_tenant hanya menjamin tenant_id = tenant transaksi; payload tidak bisa diperiksanya.
	e.inTenant(t, beta.id, func(tx pgx.Tx) error {
		ctx := context.Background()
		for _, s := range []jobs.Spec{
			{Kind: "payroll.plan", Payload: map[string]any{"run_id": run, "calc_version": before.calcVersion}, MaxAttempts: 5},
			{Kind: "payroll.calc_chunk", Payload: map[string]any{"run_id": run, "calc_version": before.calcVersion, "chunk_no": 1,
				"employee_ids": []any{alpha.empSelf, alpha.empOther}}, MaxAttempts: 5},
		} {
			if _, err := jobs.Enqueue(ctx, tx, s); err != nil {
				return err
			}
		}
		return nil
	})
	e.drainJobs(t)

	// Bandingkan nilai, bukan struct: decimal.Decimal berisi *big.Int, jadi == membandingkan pointer.
	if after := e.runState(t, alpha.id, run); stateKey(after) != stateKey(before) {
		t.Fatalf("Alpha run must be untouched by Beta's jobs:\nbefore %s\nafter  %s", stateKey(before), stateKey(after))
	}
	var dead, attempts, betaItems int
	e.inTenant(t, beta.id, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(max(attempts), 0) FROM jobs
			WHERE status = 'dead' AND payload->>'run_id' = $1`, run.String()).Scan(&dead, &attempts); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM payroll_items`).Scan(&betaItems)
	})
	if dead != 2 || attempts != 1 || betaItems != 0 {
		t.Fatalf("forged jobs: want 2 dead after 1 attempt (permanent) and 0 Beta items, got dead %d attempts %d items %d", dead, attempts, betaItems)
	}
	if testutil.ToFloat64(metrics.JobDead.WithLabelValues("payroll.plan")) != deadPlan+1 ||
		testutil.ToFloat64(metrics.JobDead.WithLabelValues("payroll.calc_chunk")) != deadChunk+1 {
		t.Fatal("app_job_dead_total must rise once per forged job (alert JobDead)")
	}
}

func stateKey(s runState) string {
	d := func(n decimal.NullDecimal) string {
		if !n.Valid {
			return "null"
		}
		return n.Decimal.String()
	}
	return fmt.Sprintf("%s v%d chunks %d/%d items %d employees %d snaps %d totals %s/%s/%s sums %s/%s/%s", s.status, s.calcVersion,
		s.chunksDone, s.chunksTotal, s.items, s.employees, s.snaps, d(s.gross), d(s.deductions), d(s.net), d(s.sumGross), d(s.sumDed), d(s.sumNet))
}

// TestSecurityHeaders_OnEveryAPIResponse (DoD S4, C-10): header keamanan ada di SETIAP respons API — sukses,
// 401, 404 rute tak dikenal, dan 400 validasi — karena middleware memasangnya sebelum handler berjalan.
func TestSecurityHeaders_OnEveryAPIResponse(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	want := map[string]string{
		"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"X-Frame-Options":         "DENY",
		"Cache-Control":           "no-store",
	}
	for _, c := range []struct {
		name, method, path, token string
		body                      any
		status                    int
	}{
		{"200", http.MethodGet, "/api/v1/employees", f.tokens[roleO], nil, http.StatusOK},
		{"401", http.MethodGet, "/api/v1/employees", "", nil, http.StatusUnauthorized},
		{"404", http.MethodGet, "/api/v1/no-such-route", f.tokens[roleO], nil, http.StatusNotFound},
		{"400", http.MethodGet, "/api/v1/employees?limit=1000", f.tokens[roleO], nil, http.StatusBadRequest},
	} {
		r, _ := e.do(t, c.method, c.path, c.token, c.body)
		if r.StatusCode != c.status {
			t.Fatalf("%s: want status %d, got %d", c.name, c.status, r.StatusCode)
		}
		for h, v := range want {
			if got := r.Header.Get(h); got != v {
				t.Errorf("%s response: header %s = %q, want %q", c.name, h, got, v)
			}
		}
	}
}
