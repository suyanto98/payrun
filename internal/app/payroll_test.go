package app_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
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
