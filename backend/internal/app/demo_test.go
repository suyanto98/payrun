//go:build demo

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	migrationsdemo "payrun/db/migrations_demo"
	"payrun/internal/app"
	"payrun/internal/platform/faults"
)

// Build demo (docs/design/27-docker-compose.md §9). Jalankan: go test -tags demo ./internal/app/ -run Demo

const demoToken = "demo-token-0123456789abcdef"

func demoEnv(t *testing.T) (env, *tenantFx, *httptest.Server) {
	t.Helper()
	e := newEnvTiers(t, relaxedTiers())
	if err := migrationsdemo.MigrateUp(context.Background(), e.tdb.OwnerDSN()); err != nil {
		t.Fatal(err)
	}
	f := e.newTenantFx(t, "alpha")
	d := e.deps
	d.DemoMode, d.DemoToken = true, demoToken
	on := httptest.NewServer(app.APIRouter(d))
	t.Cleanup(on.Close)
	return e, f, on
}

func demoPost(t *testing.T, srv *httptest.Server, token, demo, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/v1/demo/faults", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if demo != "" {
		req.Header.Set("X-Demo-Token", demo)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var b map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&b)
	return resp.StatusCode, b
}

// TestDemo_FaultsGuarded: tiga lapis guard (runtime DEMO_MODE, X-Demo-Token, OWNER) + validasi point.
func TestDemo_FaultsGuarded(t *testing.T) {
	e, f, on := demoEnv(t)
	off := httptest.NewServer(app.APIRouter(e.deps)) // lapis runtime: DEMO_MODE=false
	t.Cleanup(off.Close)
	body := `{"point":"payroll.calc_chunk.before_commit","after_n":3}`
	if s, _ := demoPost(t, off, f.tokens[roleO], demoToken, body); s != http.StatusNotFound {
		t.Fatalf("DEMO_MODE=false: want 404, got %d", s)
	}
	if s, _ := demoPost(t, on, f.tokens[roleO], "wrong-token-0123456789abcd", body); s != http.StatusNotFound {
		t.Fatalf("wrong X-Demo-Token: want 404 (route hidden), got %d", s)
	}
	if s, _ := demoPost(t, on, f.tokens[roleO], "", body); s != http.StatusNotFound {
		t.Fatalf("missing X-Demo-Token: want 404, got %d", s)
	}
	if s, _ := demoPost(t, on, f.tokens[roleP], demoToken, body); s != http.StatusForbidden {
		t.Fatalf("PAYROLL_ADMIN: want 403 (OWNER only), got %d", s)
	}
	if s, _ := demoPost(t, on, f.tokens[roleO], demoToken, `{"point":"ledger.anything"}`); s != http.StatusBadRequest {
		t.Fatalf("unknown point: want 400, got %d", s)
	}
	if s, b := demoPost(t, on, f.tokens[roleO], demoToken, body); s != http.StatusCreated || b["after_n"] != float64(3) {
		t.Fatalf("arm fault: want 201, got %d %v", s, b)
	}
}

// TestDemo_ChunkFaultFiresAtChunkN (Demo B live, O5): fault after_n = 1 → chunk ke-2 mati tepat sebelum
// commit (exit diganti panic di test: percobaan gagal, transaksi rollback); fault hanya untuk tenant
// pemasangnya. Setelah retry run CALCULATED dengan tepat satu item per employee.
func TestDemo_ChunkFaultFiresAtChunkN(t *testing.T) {
	e, f, on := demoEnv(t)
	other := e.newTenantFx(t, "beta")
	if s, b := demoPost(t, on, f.tokens[roleO], demoToken, `{"point":"payroll.calc_chunk.before_commit","after_n":1}`); s != http.StatusCreated {
		t.Fatalf("arm: %d %v", s, b)
	}
	var exits []int
	t.Cleanup(faults.SetExitForTest(func(code int) {
		exits = append(exits, code)
		panic("demo exit") // seperti kill -9: chunk ini tidak pernah commit
	}))

	// Tenant lain tidak terkena fault Alpha.
	e.calculatedRun(t, other.tokens[roleP], "2026-03")
	if len(exits) != 0 {
		t.Fatalf("fault of tenant Alpha fired for tenant Beta (%d exits)", len(exits))
	}

	run := e.createRun(t, f.tokens[roleP], "2026-03")
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/calculate", f.tokens[roleP], key(), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("calculate: %d %v", r.StatusCode, b)
	}
	e.drainJobs(t)
	mid := e.runState(t, f.id, run)
	if len(exits) != 1 || exits[0] != 137 || mid.status != "CALCULATING" || mid.chunksDone != 1 || mid.items != 2 {
		t.Fatalf("after the fault: want one exit(137) at chunk 2 with only chunk 1 committed, got exits %v, %+v", exits, mid)
	}
	deadline := time.Now().Add(15 * time.Second)                                         //nolint:forbidigo // menunggu backoff retry (jam DB)
	for e.runState(t, f.id, run).status != "CALCULATED" && time.Now().Before(deadline) { //nolint:forbidigo // idem
		time.Sleep(300 * time.Millisecond)
		if _, err := e.worker.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	s := e.runState(t, f.id, run)
	if s.status != "CALCULATED" || s.items != 3 || s.employees != 3 || len(exits) != 1 {
		t.Fatalf("after retry: want CALCULATED with one item per employee and no second fault, got exits %v, %+v", exits, s)
	}
	var fired int
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT fired FROM demo_faults WHERE tenant_id = $1`, f.id).Scan(&fired)
	})
	if fired != 2 {
		t.Fatalf("demo_faults.fired: want 2 (after_n + times), got %d", fired)
	}
}
