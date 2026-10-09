package app_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"payrun/internal/app"
	"payrun/internal/platform/jobs"
)

// ---------------------------------------------------------------- helper payroll run (S4)

// drainJobs menjalankan worker sampai tidak ada job yang jatuh tempo. Job yang gagal dan dijadwalkan
// ulang (backoff) tidak ditunggu: test memeriksa status run, bukan menunggu retry.
func (e env) drainJobs(t *testing.T) {
	t.Helper()
	for range 100 {
		n, err := e.worker.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("jobs did not drain after 100 rounds")
}

func (e env) createRun(t *testing.T, token, period string) uuid.UUID {
	t.Helper()
	r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs", token, key(), map[string]any{"period": period})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create run %s: %d %v", period, r.StatusCode, b)
	}
	return uuid.MustParse(b["id"].(string))
}

// calculatedRun: create → calculate → worker sampai selesai; run harus CALCULATED.
func (e env) calculatedRun(t *testing.T, token, period string) (uuid.UUID, map[string]any) {
	t.Helper()
	id := e.createRun(t, token, period)
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+id.String()+"/calculate", token, key(), map[string]any{}); r.StatusCode != http.StatusAccepted {
		t.Fatalf("calculate %s: %d %v", period, r.StatusCode, b)
	}
	e.drainJobs(t)
	_, run := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+id.String(), token, nil)
	if run["status"] != "CALCULATED" {
		t.Fatalf("run %s after worker: want CALCULATED, got %v", period, run)
	}
	return id, run
}

// runItems: item versi terkini (satu halaman, cukup untuk fixture kecil).
func (e env) runItems(t *testing.T, token string, run uuid.UUID) []map[string]any {
	t.Helper()
	r, b := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+run.String()+"/items?limit=200", token, nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("items: %d %v", r.StatusCode, b)
	}
	var out []map[string]any
	for _, it := range b["data"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

// errorItem: run CALCULATED dengan item ERROR. Employee fixture belum punya gaji → NO_SALARY.
func (e env) errorItem(t *testing.T, token, period string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	run, _ := e.calculatedRun(t, token, period)
	for _, it := range e.runItems(t, token, run) {
		if it["status"] == "ERROR" {
			return run, uuid.MustParse(it["id"].(string))
		}
	}
	t.Fatalf("run %s has no ERROR item", period)
	return uuid.Nil, uuid.Nil
}

// ---------------------------------------------------------------- wiring end-to-end

// TestPayrollRun_CalculateEndToEnd: API → outbox → worker (payroll.plan → 2 chunk) → run CALCULATED.
// Tiga employee fixture, chunk 2: satu ber-gaji (OK), dua tanpa gaji (ERROR NO_SALARY). Total run =
// jumlah item OK (PI-11).
func TestPayrollRun_CalculateEndToEnd(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o, p := f.tokens[roleO], f.tokens[roleP]
	if r, b := e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/salary-changes", o, key(),
		map[string]any{"base_salary": "6000000.00", "valid_from": "2026-01-05"}); r.StatusCode != http.StatusCreated {
		t.Fatalf("salary: %d %v", r.StatusCode, b)
	}

	run, got := e.calculatedRun(t, p, "2026-03")
	prog := got["progress"].(map[string]any)
	if prog["eligible_count"] != float64(3) || prog["chunks_total"] != float64(2) || prog["chunks_done"] != float64(2) {
		t.Fatalf("progress: want 3 eligible in 2/2 chunks, got %v", prog)
	}
	counts := got["counts"].(map[string]any)
	if counts["ok"] != float64(1) || counts["error"] != float64(2) {
		t.Fatalf("counts: want ok=1 error=2, got %v", counts)
	}

	sum := decimal.Zero
	for _, it := range e.runItems(t, p, run) {
		switch it["status"] {
		case "OK":
			if it["employee_id"] != f.empOther.String() {
				t.Errorf("OK item for unexpected employee: %v", it)
			}
			sum = sum.Add(decimal.RequireFromString(it["net"].(string)))
		case "ERROR":
			if codes, _ := it["error_codes"].([]any); len(codes) == 0 || codes[0] != "NO_SALARY" {
				t.Errorf("ERROR item: want NO_SALARY, got %v", it)
			}
		}
	}
	if totals := got["totals"].(map[string]any); totals["net"] != sum.StringFixed(2) {
		t.Fatalf("run total net %v != sum of OK items %s", totals["net"], sum.StringFixed(2))
	}

	// Recalculate: calc_version naik, item versi lama tidak terlihat (jumlah item tetap 3).
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/calculate", p, key(), map[string]any{}); r.StatusCode != http.StatusAccepted {
		t.Fatalf("recalculate: %d %v", r.StatusCode, b)
	}
	e.drainJobs(t)
	_, again := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+run.String(), p, nil)
	if again["status"] != "CALCULATED" || again["calc_version"] != float64(2) {
		t.Fatalf("after recalculate: want CALCULATED v2, got %v", again)
	}
	if n := len(e.runItems(t, p, run)); n != 3 {
		t.Fatalf("after recalculate: want 3 current items, got %d", n)
	}
}

// TestPayrollItem_HRReadIsAudited (file 17 ᴬ): HR_ADMIN membaca detail item → audit payroll_item.read;
// OWNER/PAYROLL_ADMIN tidak.
func TestPayrollItem_HRReadIsAudited(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	run, item := e.errorItem(t, f.tokens[roleP], "2026-02")
	path := "/api/v1/payroll-runs/" + run.String() + "/items/" + item.String()
	for _, tc := range []struct {
		role  string
		delta int
	}{{roleP, 0}, {roleO, 0}, {roleH, 1}} {
		before := e.auditCount(t, f.id, "payroll_item.read")
		r, b := e.do(t, http.MethodGet, path, f.tokens[tc.role], nil)
		if r.StatusCode != http.StatusOK || r.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %v %v", tc.role, r.StatusCode, r.Header, b)
		}
		if got := e.auditCount(t, f.id, "payroll_item.read"); got != before+tc.delta {
			t.Errorf("%s: audit payroll_item.read %d → %d, want +%d", tc.role, before, got, tc.delta)
		}
	}
}

// TestPayrollWorker_KillSwitchKeepsJobsQueued: kind payroll tidak didaftarkan → run tetap CALCULATING,
// job tidak dead; worker normal berikutnya menyelesaikannya.
func TestPayrollWorker_KillSwitchKeepsJobsQueued(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	p := f.tokens[roleP]
	run := e.createRun(t, p, "2026-04")
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/calculate", p, key(), map[string]any{}); r.StatusCode != http.StatusAccepted {
		t.Fatalf("calculate: %d %v", r.StatusCode, b)
	}
	off := e.withWorker(t, true)
	if _, err := off.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, got := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+run.String(), p, nil); got["status"] != "CALCULATING" {
		t.Fatalf("kill switch on: want CALCULATING, got %v", got)
	}
	e.drainJobs(t)
	if _, got := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+run.String(), p, nil); got["status"] != "CALCULATED" {
		t.Fatalf("kill switch off again: want CALCULATED, got %v", got)
	}
}

// ---------------------------------------------------------------- DoD Sprint 4 (tanpa hook)

// helperWorkerDSN: env var yang membuat binary test berjalan sebagai proses worker (payroll_hooks_test.go).
const helperWorkerDSN = "PAYRUN_TEST_HELPER_WORKER_DSN"

// runState membaca run + item versi terkini langsung dari DB (di bawah RLS tenant).
type runState struct {
	status                   string
	calcVersion              int32
	chunksDone, chunksTotal  int32
	items, employees, snaps  int // item versi terkini, employee berbeda, snapshot
	gross, deductions, net   decimal.NullDecimal
	sumGross, sumDed, sumNet decimal.NullDecimal // SUM item OK versi terkini
}

func (e env) runState(t *testing.T, tenant, run uuid.UUID) runState {
	t.Helper()
	var s runState
	e.inTenant(t, tenant, func(tx pgx.Tx) error {
		ctx := context.Background()
		if err := tx.QueryRow(ctx, `SELECT status, calc_version, chunks_done, chunks_total, total_gross, total_deductions, total_net
			FROM payroll_runs WHERE id = $1`, run).Scan(&s.status, &s.calcVersion, &s.chunksDone, &s.chunksTotal, &s.gross, &s.deductions, &s.net); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*), count(DISTINCT employee_id),
				sum(gross) FILTER (WHERE status = 'OK'), sum(deductions) FILTER (WHERE status = 'OK'), sum(net) FILTER (WHERE status = 'OK')
			FROM payroll_items WHERE run_id = $1 AND calc_version = $2`, run, s.calcVersion).Scan(&s.items, &s.employees, &s.sumGross, &s.sumDed, &s.sumNet); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM payroll_item_snapshots WHERE run_id = $1 AND calc_version = $2`, run, s.calcVersion).Scan(&s.snaps)
	})
	return s
}

// TestCreateRun_ConcurrentSamePeriod_SingleRun (PFS-06, R1-8): delapan admin membuat run periode yang
// sama bersamaan dengan key berbeda → tepat satu 201; sisanya 409 ACTIVE_RUN_EXISTS yang menunjuk run
// pemenang. Tidak ada 5xx (lock timeout / deadlock) dan tidak ada run kedua.
func TestCreateRun_ConcurrentSamePeriod_SingleRun(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	const n = 8
	type out struct {
		status int
		body   map[string]any
	}
	res := make([]out, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		tok := f.tokens[roleO]
		if i%2 == 1 {
			tok = f.tokens[roleP]
		}
		wg.Go(func() {
			<-start
			r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs", tok, key(), map[string]any{"period": "2026-05"})
			res[i] = out{r.StatusCode, b}
		})
	}
	close(start)
	wg.Wait()
	var winner string
	for _, o := range res {
		if o.status == http.StatusCreated {
			if winner != "" {
				t.Fatalf("two runs created for one period: %s and %v", winner, o.body["id"])
			}
			winner = o.body["id"].(string)
		}
	}
	if winner == "" {
		t.Fatalf("no run created: %v", res)
	}
	for _, o := range res {
		if o.status == http.StatusCreated {
			continue
		}
		if o.status != http.StatusConflict || o.body["code"] != "ACTIVE_RUN_EXISTS" || o.body["existing_resource_id"] != winner {
			t.Errorf("loser: want 409 ACTIVE_RUN_EXISTS pointing at %s, got %d %v", winner, o.status, o.body)
		}
	}
	_, list := e.do(t, http.MethodGet, "/api/v1/payroll-runs?period=2026-05", f.tokens[roleO], nil)
	if runs := list["data"].([]any); len(runs) != 1 {
		t.Fatalf("want exactly 1 run for 2026-05, got %d", len(runs))
	}
	if got := e.auditCount(t, f.id, "payroll_run.created"); got != 1 {
		t.Fatalf("audit payroll_run.created: want 1, got %d", got)
	}
}

// TestCreateRun_DoubleClickReplays (DoD run_create_double_click_replays, PFS-06): klik ganda admin
// yang sama (Idempotency-Key sama). Berurutan → replay 201 dengan id yang sama (status + body
// tersimpan; header seperti Location tidak ikut di-replay, konvensi 14 §idempotency). Bersamaan → setiap
// respons adalah 201 (id sama) atau 409 REQUEST_IN_PROGRESS; tidak pernah dua run.
func TestCreateRun_DoubleClickReplays(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	p := f.tokens[roleP]
	body := map[string]any{"period": "2026-06"}

	k := key()
	r1, b1 := e.doH(t, http.MethodPost, "/api/v1/payroll-runs", p, k, body)
	r2, b2 := e.doH(t, http.MethodPost, "/api/v1/payroll-runs", p, k, body)
	if r1.StatusCode != http.StatusCreated || r2.StatusCode != http.StatusCreated || b1["id"] != b2["id"] ||
		r2.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("sequential double click: want replay of the same 201, got %d %v / %d %v %v", r1.StatusCode, b1, r2.StatusCode, b2, r2.Header)
	}

	k = key()
	body = map[string]any{"period": "2026-07"}
	statuses, ids := make([]int, 4), make([]any, 4)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			<-start
			r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs", p, k, body)
			statuses[i], ids[i] = r.StatusCode, b["id"]
			if r.StatusCode == http.StatusConflict && b["code"] != "REQUEST_IN_PROGRESS" {
				t.Errorf("concurrent double click: 409 must be REQUEST_IN_PROGRESS, got %v", b)
			}
		})
	}
	close(start)
	wg.Wait()
	var first any
	for i, st := range statuses {
		switch st {
		case http.StatusCreated:
			if first == nil {
				first = ids[i]
			} else if ids[i] != first {
				t.Fatalf("concurrent double click created two runs: %v vs %v", first, ids[i])
			}
		case http.StatusConflict:
		default:
			t.Fatalf("concurrent double click: unexpected status %d", st)
		}
	}
	if first == nil {
		t.Fatalf("concurrent double click: no 201 at all: %v", statuses)
	}
	if got := e.auditCount(t, f.id, "payroll_run.created"); got != 2 {
		t.Fatalf("two periods, one run each: audit payroll_run.created want 2, got %d", got)
	}
}

// TestRunTotals_EqualSumOfItems (PI-05/PI-11): sembilan employee dalam lima chunk (gaji berbeda,
// sebagian dengan tunjangan, tiga tanpa gaji). Total run = SUM item OK; net = gross − deductions di
// run dan setiap item; net item = Σ line EARNING − Σ line DEDUCTION. Exclude item ERROR tidak
// mengubah total, hanya counts.
func TestRunTotals_EqualSumOfItems(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o := f.tokens[roleO]
	salaries := []string{"4500000.00", "5250000.00", "6000000.00", "7333333.00", "9100000.00", "15000000.00"}
	for i, sal := range salaries {
		emp := e.createEmployee(t, o, fmt.Sprintf("EMP-1%02d", i), fmt.Sprintf("Karyawan %d", i), fmt.Sprintf("31740123049501%02d", 10+i))
		path := "/api/v1/employees/" + emp.String()
		if r, b := e.doH(t, http.MethodPost, path+"/salary-changes", o, key(), map[string]any{"base_salary": sal, "valid_from": "2026-01-05"}); r.StatusCode != http.StatusCreated {
			t.Fatalf("salary %s: %d %v", sal, r.StatusCode, b)
		}
		if i%2 == 0 {
			if r, b := e.doH(t, http.MethodPost, path+"/component-assignments", o, key(), map[string]any{"component_id": f.comp, "valid_from": "2026-01-05"}); r.StatusCode != http.StatusCreated {
				t.Fatalf("component: %d %v", r.StatusCode, b)
			}
		}
	}
	run, got := e.calculatedRun(t, f.tokens[roleP], "2026-03")
	if c := got["counts"].(map[string]any); c["ok"] != float64(6) || c["error"] != float64(3) {
		t.Fatalf("counts: want ok=6 error=3, got %v", c)
	}
	check := func(when string) runState {
		t.Helper()
		s := e.runState(t, f.id, run)
		if s.chunksTotal != 5 || s.chunksDone != 5 || s.items != 9 || s.employees != 9 || s.snaps != 9 {
			t.Fatalf("%s: want 9 items/snapshots in 5/5 chunks, got %+v", when, s)
		}
		if !s.gross.Decimal.Equal(s.sumGross.Decimal) || !s.deductions.Decimal.Equal(s.sumDed.Decimal) || !s.net.Decimal.Equal(s.sumNet.Decimal) {
			t.Fatalf("%s: run totals %s/%s/%s != SUM(items OK) %s/%s/%s", when, s.gross.Decimal, s.deductions.Decimal, s.net.Decimal,
				s.sumGross.Decimal, s.sumDed.Decimal, s.sumNet.Decimal)
		}
		if !s.net.Decimal.Equal(s.gross.Decimal.Sub(s.deductions.Decimal)) || !s.net.Decimal.IsPositive() {
			t.Fatalf("%s: run net %s != gross %s − deductions %s", when, s.net.Decimal, s.gross.Decimal, s.deductions.Decimal)
		}
		return s
	}
	before := check("after calculate")
	var bad int
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT count(*) FROM (
			  SELECT i.id FROM payroll_items i JOIN payroll_lines l ON l.item_id = i.id
			  WHERE i.run_id = $1 AND i.status = 'OK'
			  GROUP BY i.id, i.gross, i.deductions, i.net
			  HAVING i.gross <> sum(l.amount) FILTER (WHERE l.category = 'EARNING')
			      OR i.deductions <> coalesce(sum(l.amount) FILTER (WHERE l.category = 'DEDUCTION'), 0)
			      OR i.net <> i.gross - i.deductions) x`, run).Scan(&bad)
	})
	if bad != 0 {
		t.Fatalf("%d OK items whose totals differ from their lines", bad)
	}

	_, item := e.errorItemOf(t, f.tokens[roleP], run)
	if r, b := e.do(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/items/"+item.String()+"/exclude", f.tokens[roleP],
		map[string]any{"reason": "Gaji belum diinput HR"}); r.StatusCode != http.StatusOK {
		t.Fatalf("exclude: %d %v", r.StatusCode, b)
	}
	after := check("after exclude")
	if !after.net.Decimal.Equal(before.net.Decimal) {
		t.Fatalf("excluding an ERROR item must not change totals: %s → %s", before.net.Decimal, after.net.Decimal)
	}
	if _, r := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+run.String(), f.tokens[roleP], nil); r["counts"].(map[string]any)["excluded"] != float64(1) ||
		r["counts"].(map[string]any)["error"] != float64(2) {
		t.Fatalf("counts after exclude: want error=2 excluded=1, got %v", r["counts"])
	}
}

// TestCalcChunk_StaleCalcVersion_NoOp (ADR-P05): chunk calc_version 1 dikirim ulang (redelivery /
// job duplikat) saat run sudah CALCULATING versi 2. Chunk usang tidak menulis apa pun (item v1 tetap
// sama, item v2 tidak dobel, chunks_done tidak bertambah) dan job-nya selesai, tidak di-retry.
func TestCalcChunk_StaleCalcVersion_NoOp(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	p := f.tokens[roleP]
	run, _ := e.calculatedRun(t, p, "2026-03")
	v1 := e.itemIDs(t, f.id, run, 1)

	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/calculate", p, key(), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("recalculate: %d %v", r.StatusCode, b)
	}
	var staleJob uuid.UUID
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := jobs.Enqueue(ctx, tx, jobs.Spec{Kind: "payroll.calc_chunk", MaxAttempts: 5, Payload: map[string]any{
			"run_id": run, "calc_version": 1, "chunk_no": 1, "employee_ids": []uuid.UUID{f.empSelf, f.empOther, f.empManager}}}); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT id FROM jobs WHERE kind = 'payroll.calc_chunk' AND payload->>'calc_version' = '1' AND status = 'queued'`).Scan(&staleJob)
	})
	e.drainJobs(t)

	s := e.runState(t, f.id, run)
	if s.status != "CALCULATED" || s.calcVersion != 2 || s.items != 3 || s.snaps != 3 || s.chunksDone != s.chunksTotal {
		t.Fatalf("v2 must complete with 3 items and chunks_done = chunks_total, got %+v", s)
	}
	if got := e.itemIDs(t, f.id, run, 1); !slices.Equal(got, v1) {
		t.Fatalf("stale chunk rewrote v1 items: %v → %v", v1, got)
	}
	var status string
	var attempts int
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT status, attempts FROM jobs WHERE id = $1`, staleJob).Scan(&status, &attempts)
	})
	if status != "succeeded" || attempts != 1 {
		t.Fatalf("stale chunk job: want succeeded after 1 attempt (no retry), got %s/%d", status, attempts)
	}
}

// itemIDs: id item satu versi, urut (pembanding "tidak ditulis ulang").
func (e env) itemIDs(t *testing.T, tenant, run uuid.UUID, version int32) []uuid.UUID {
	t.Helper()
	var ids []uuid.UUID
	e.inTenant(t, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT id FROM payroll_items WHERE run_id = $1 AND calc_version = $2 ORDER BY id`, run, version)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	return ids
}

// errorItemOf: item ERROR pertama dari run yang sudah dihitung.
func (e env) errorItemOf(t *testing.T, token string, run uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	for _, it := range e.runItems(t, token, run) {
		if it["status"] == "ERROR" {
			return run, uuid.MustParse(it["id"].(string))
		}
	}
	t.Fatal("run has no ERROR item")
	return uuid.Nil, uuid.Nil
}

// workerWith: worker kedua dengan konfigurasi sendiri (lease, konkurensi).
func (e env) workerWith(cfg app.WorkerConfig) *app.Worker {
	if cfg.ID == "" {
		cfg.ID = "test:" + newID().String()[:8]
	}
	if cfg.ChunkSize == 0 {
		cfg.ChunkSize = 2
	}
	return app.NewWorker(e.deps, cfg)
}
