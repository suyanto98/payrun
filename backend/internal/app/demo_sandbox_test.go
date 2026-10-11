//go:build demo

package app_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	migrationsdemo "payrun/db/migrations_demo"
	"payrun/internal/app"
	"payrun/internal/platform/ratelimit"
)

// sandboxEnv: env test + migrasi demo + API dengan DEMO_MODE=true + worker yang berjalan terus (sandbox
// menunggu worker menghitung payroll bulan lalu, persis seperti di VM demo).
func sandboxEnv(t *testing.T, tiers *ratelimit.Tiers, tweak func(*app.Deps)) (env, *httptest.Server) {
	t.Helper()
	e := newEnvTiers(t, tiers)
	if err := migrationsdemo.MigrateUp(context.Background(), e.tdb.OwnerDSN()); err != nil {
		t.Fatal(err)
	}
	d := e.deps
	d.DemoMode, d.DemoToken = true, demoToken
	if tweak != nil {
		tweak(&d)
	}
	srv := httptest.NewServer(app.APIRouter(d))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		for ctx.Err() == nil {
			if n, _ := e.worker.RunOnce(ctx); n == 0 {
				select {
				case <-ctx.Done():
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
	})
	// LIFO: worker berhenti dulu, lalu server ditutup, lalu invariant checker newEnv berjalan.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { cancel(); wg.Wait() })
	ee := e
	ee.srv = srv
	return ee, srv
}

type demoSession struct {
	status  int
	token   string
	refresh *http.Cookie
	body    map[string]any
}

func startSandbox(t *testing.T, e env, ip string) demoSession {
	t.Helper()
	r, b := e.doH(t, http.MethodPost, "/api/v1/demo/sessions", "", map[string]string{"X-Requested-With": "fetch", "X-Forwarded-For": ip}, nil)
	s := demoSession{status: r.StatusCode, body: b}
	s.token, _ = b["access_token"].(string)
	for _, c := range r.Cookies {
		if c.Name == "pr_refresh" {
			s.refresh = c
		}
	}
	return s
}

func switchPersona(t *testing.T, e env, token, persona string) (int, string) {
	t.Helper()
	r, b := e.doH(t, http.MethodPost, "/api/v1/demo/persona", token, map[string]string{"X-Requested-With": "fetch"}, map[string]string{"persona": persona})
	tok, _ := b["access_token"].(string)
	return r.StatusCode, tok
}

func listLen(t *testing.T, e env, path, token string) int {
	t.Helper()
	r, b := e.do(t, http.MethodGet, path, token, nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %v", path, r.StatusCode, b)
	}
	data, _ := b["data"].([]any)
	return len(data)
}

// Satu klik → perusahaan contoh dengan 15 karyawan, payroll bulan lalu FINAL (dihitung worker, diajukan
// Dina, di-approve Hendra), slip gaji untuk karyawan, dan cuti yang menunggu approval owner.
func TestDemoSandbox_OneClickCompanyWithPersonas(t *testing.T) {
	e, _ := sandboxEnv(t, testTiers(), nil)
	if r, b := e.do(t, http.MethodGet, "/api/v1/demo", "", nil); r.StatusCode != http.StatusOK || b["sandbox"] != true {
		t.Fatalf("GET /demo: %d %v", r.StatusCode, b)
	}
	if r, _ := e.do(t, http.MethodPost, "/api/v1/demo/sessions", "", nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("without X-Requested-With (CSRF guard): want 403, got %d", r.StatusCode)
	}
	s := startSandbox(t, e, "203.0.113.20")
	if s.status != http.StatusOK || s.token == "" || s.refresh == nil {
		t.Fatalf("demo session: %d %v", s.status, s.body)
	}
	at, _ := s.body["active_tenant"].(map[string]any)
	if at["name"] != "PT Kopi Nusantara" {
		t.Fatalf("active tenant: %v", s.body)
	}
	if n := listLen(t, e, "/api/v1/employees?limit=50", s.token); n != 15 {
		t.Fatalf("employees: %d, want 15", n)
	}
	_, runs := e.do(t, http.MethodGet, "/api/v1/payroll-runs", s.token, nil)
	list := runs["data"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["status"] != "FINALIZED" || list[0].(map[string]any)["period"] != "2026-09" {
		t.Fatalf("last month payroll: %v", runs)
	}
	// Cek ulang hasil: 15 item dihitung ulang dari snapshot, identik (PI-12).
	runID := list[0].(map[string]any)["id"].(string)
	if r, v := e.do(t, http.MethodPost, "/api/v1/demo/runs/"+runID+"/verify", s.token, nil); r.StatusCode != http.StatusOK ||
		v["ok"] != true || v["items_checked"] != float64(15) || v["mismatches"] != float64(0) {
		t.Fatalf("verify run: %d %v", r.StatusCode, v)
	}
	_, sb := e.do(t, http.MethodGet, "/api/v1/demo/sandbox", s.token, nil)
	if sb["persona"] != "payroll" || len(sb["personas"].([]any)) != 3 {
		t.Fatalf("sandbox info: %v", sb)
	}

	// Owner: cuti Rizky menunggu di kotak approval.
	status, owner := switchPersona(t, e, s.token, "owner")
	if status != http.StatusOK {
		t.Fatalf("switch to owner: %d", status)
	}
	if n := listLen(t, e, "/api/v1/approvals/inbox", owner); n != 1 {
		t.Fatalf("owner inbox: %d pending, want 1 (Rizky's leave)", n)
	}
	// Riwayat gaji Bayu: gaji lama + kenaikan mulai bulan ini (sesudah payroll bulan lalu final). Mengubah gaji
	// mulai bulan lalu (sudah final) ditolak PERIOD_LOCKED: "final itu final" yang dicoba HR di UI.
	_, emps := e.do(t, http.MethodGet, "/api/v1/employees?limit=50", owner, nil)
	var bayu string
	for _, it := range emps["data"].([]any) {
		if m := it.(map[string]any); m["employee_no"] == "KN-003" {
			bayu = m["id"].(string)
		}
	}
	_, hist := e.do(t, http.MethodGet, "/api/v1/employees/"+bayu+"/salary-history", owner, nil)
	if sal, _ := hist["salary"].([]any); len(sal) != 2 {
		t.Fatalf("Bayu salary history: %v", hist)
	}
	r, locked := e.doH(t, http.MethodPost, "/api/v1/employees/"+bayu+"/salary-changes", owner,
		map[string]string{"Idempotency-Key": "raise-" + newID().String()}, map[string]any{"base_salary": "9000000.00", "valid_from": "2026-09-01"})
	if r.StatusCode != http.StatusConflict || locked["code"] != "PERIOD_LOCKED" {
		t.Fatalf("raise into a finalized month: want 409 PERIOD_LOCKED, got %d %v", r.StatusCode, locked)
	}
	// Karyawan: slip gaji bulan lalu, dan tidak bisa membuka daftar payroll (otorisasi asli).
	status, emp := switchPersona(t, e, owner, "employee")
	if status != http.StatusOK {
		t.Fatalf("switch to employee: %d", status)
	}
	if n := listLen(t, e, "/api/v1/me/payslips", emp); n != 1 {
		t.Fatalf("employee payslips: %d, want 1", n)
	}
	if r, _ := e.do(t, http.MethodGet, "/api/v1/payroll-runs", emp, nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("employee lists payroll runs: want 403, got %d", r.StatusCode)
	}
	if status, _ := switchPersona(t, e, emp, "superadmin"); status != http.StatusBadRequest {
		t.Fatalf("unknown persona: want 400, got %d", status)
	}
}

// Dua pengunjung = dua tenant: RLS yang sama dengan pelanggan sungguhan, dan ganti peran tidak bisa
// menyeberang ke sandbox lain.
func TestDemoSandbox_VisitorsGetSeparateTenants(t *testing.T) {
	e, _ := sandboxEnv(t, testTiers(), nil)
	a := startSandbox(t, e, "203.0.113.21")
	b := startSandbox(t, e, "203.0.113.22")
	ta := a.body["active_tenant"].(map[string]any)["id"]
	tb := b.body["active_tenant"].(map[string]any)["id"]
	if ta == tb {
		t.Fatal("two visitors share one tenant")
	}
	_, ownerA := switchPersona(t, e, a.token, "owner")
	_, me := e.do(t, http.MethodGet, "/api/v1/me", ownerA, nil)
	if me["active_tenant"].(map[string]any)["id"] != ta {
		t.Fatalf("persona switch left the visitor's own sandbox: %v", me)
	}
	if n := listLen(t, e, "/api/v1/employees?limit=50", b.token); n != 15 {
		t.Fatalf("visitor B sees %d employees, want exactly its own 15", n)
	}
	// User biasa (bukan sandbox) tidak punya sandbox dan tidak bisa berganti peran.
	f := e.newTenantFx(t, "regular")
	if r, _ := e.do(t, http.MethodGet, "/api/v1/demo/sandbox", f.tokens["OWNER"], nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("regular user sandbox info: want 404, got %d", r.StatusCode)
	}
	if status, _ := switchPersona(t, e, f.tokens["OWNER"], "owner"); status != http.StatusNotFound {
		t.Fatalf("regular user persona switch: want 404, got %d", status)
	}
}

// Kedaluwarsa: tenant SUSPENDED + refresh token dicabut; data payroll final tidak disentuh.
func TestDemoSandbox_ExpirySuspendsTenantAndRevokesSessions(t *testing.T) {
	e, _ := sandboxEnv(t, testTiers(), func(d *app.Deps) { d.DemoSandboxTTL = 2 * time.Hour })
	s := startSandbox(t, e, "203.0.113.23")
	tid := s.body["active_tenant"].(map[string]any)["id"].(string)
	if n, err := app.ExpireDemoSandboxes(context.Background(), e.deps, 10); err != nil || n != 0 {
		t.Fatalf("before TTL: %d (%v)", n, err)
	}
	e.clk.Advance(2*time.Hour + time.Minute)
	if n, err := app.ExpireDemoSandboxes(context.Background(), e.deps, 10); err != nil || n != 1 {
		t.Fatalf("after TTL: %d (%v)", n, err)
	}
	var status string
	e.inTenant(t, uuid.MustParse(tid), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT status FROM tenants WHERE id = $1`, tid).Scan(&status)
	})
	if status != "SUSPENDED" {
		t.Fatalf("tenant status %q, want SUSPENDED", status)
	}
	r, _ := e.cookiePost(t, "/api/v1/auth/refresh", s.refresh.Name+"="+s.refresh.Value, nil)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refresh after expiry: want 401, got %d", r.StatusCode)
	}
}

// Penyalahgunaan: 3 sandbox/menit per IP (429) dan kuota harian global (503).
func TestDemoSandbox_RateLimitAndDailyQuota(t *testing.T) {
	tiers := testTiers()
	e, _ := sandboxEnv(t, tiers, nil) // DemoSession = tier default (3/menit per IP)
	for i := range 3 {
		if s := startSandbox(t, e, "198.51.100.30"); s.status != http.StatusOK {
			t.Fatalf("sandbox %d: %d %v", i+1, s.status, s.body)
		}
	}
	if s := startSandbox(t, e, "198.51.100.30"); s.status != http.StatusTooManyRequests {
		t.Fatalf("4th sandbox from one IP: want 429, got %d", s.status)
	}
	d := e.deps
	d.DemoMode, d.DemoToken, d.DemoSandboxDailyMax = true, demoToken, 3
	full := httptest.NewServer(app.APIRouter(d))
	t.Cleanup(full.Close)
	ef := e
	ef.srv = full
	if s := startSandbox(t, ef, "198.51.100.31"); s.status != http.StatusServiceUnavailable {
		t.Fatalf("daily quota reached: want 503, got %d", s.status)
	}
}

// Simulasi gangguan: satu chunk "mati" di tengah (panic sebelum COMMIT), diulang otomatis; hasil tetap 15 item
// untuk 15 karyawan (tidak dobel, tidak hilang). Invariant payroll dicek di akhir test (newEnv).
func TestDemoSandbox_CrashTestChunkRetriesWithoutDuplicates(t *testing.T) {
	e, _ := sandboxEnv(t, testTiers(), nil)
	s := startSandbox(t, e, "203.0.113.24")
	idem := func(k string) map[string]string { return map[string]string{"Idempotency-Key": k} }
	r, run := e.doH(t, http.MethodPost, "/api/v1/payroll-runs", s.token, idem("crash-create-"+newID().String()), map[string]string{"period": "2026-10"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create run: %d %v", r.StatusCode, run)
	}
	runID := run["id"].(string)
	if r, b := e.doH(t, http.MethodPost, "/api/v1/demo/crash-test", s.token, nil, nil); r.StatusCode != http.StatusCreated || b["armed"] != true {
		t.Fatalf("arm crash: %d %v", r.StatusCode, b)
	}
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+runID+"/calculate", s.token, idem("crash-calc-"+newID().String()), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("calculate: %d %v", r.StatusCode, b)
	}
	var cur map[string]any
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		_, cur = e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+runID, s.token, nil)
		if cur["status"] == "CALCULATED" || cur["status"] == "FAILED" {
			break
		}
	}
	if cur["status"] != "CALCULATED" {
		t.Fatalf("run after crash test: %v", cur["status"])
	}
	if ok := cur["counts"].(map[string]any)["ok"]; ok != float64(15) {
		t.Fatalf("items ok = %v, want 15 (one per employee, no duplicates)", ok)
	}
	_, tl := e.do(t, http.MethodGet, "/api/v1/demo/runs/"+runID+"/timeline", s.token, nil)
	retried := 0
	for _, c := range tl["chunks"].([]any) {
		m := c.(map[string]any)
		if m["status"] != "succeeded" {
			t.Fatalf("chunk not finished: %v", m)
		}
		if m["failed_once"] == true && m["simulated"] == true && m["attempts"] == float64(2) {
			retried++
		}
	}
	if retried != 1 || tl["crash_armed"] != false {
		t.Fatalf("timeline: want exactly one chunk failed once (simulated) and retried, fault consumed; got %v", tl)
	}
	// Token tenant lain tidak bisa memasang gangguan di sandbox ini (dan user biasa tidak punya sandbox).
	f := e.newTenantFx(t, "regular")
	if r, _ := e.doH(t, http.MethodPost, "/api/v1/demo/crash-test", f.tokens["OWNER"], nil, nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("regular tenant arming a crash: want 404, got %d", r.StatusCode)
	}
}

// Y6s di sandbox: total per departemen (3 departemen) menjumlah persis ke total net run final.
func TestDemoSandbox_DepartmentSummaryAddsUpToRunTotal(t *testing.T) {
	e, _ := sandboxEnv(t, testTiers(), nil)
	s := startSandbox(t, e, "203.0.113.25")
	_, runs := e.do(t, http.MethodGet, "/api/v1/payroll-runs", s.token, nil)
	runID := runs["data"].([]any)[0].(map[string]any)["id"].(string)
	_, run := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+runID, s.token, nil)
	r, sum := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+runID+"/summary", s.token, nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("summary: %d %v", r.StatusCode, sum)
	}
	rows := sum["by_department"].([]any)
	if len(rows) != 3 {
		t.Fatalf("departments: %d, want 3", len(rows))
	}
	var cents, employees int64
	for _, row := range rows {
		m := row.(map[string]any)
		var rp, sen int64
		if _, err := fmt.Sscanf(m["net"].(string), "%d.%d", &rp, &sen); err != nil {
			t.Fatal(err)
		}
		cents += rp*100 + sen
		employees += int64(m["employees"].(float64))
	}
	var rp, sen int64
	if _, err := fmt.Sscanf(run["totals"].(map[string]any)["net"].(string), "%d.%d", &rp, &sen); err != nil {
		t.Fatal(err)
	}
	if cents != rp*100+sen || employees != 15 {
		t.Fatalf("summary net %d sen over %d employees, run total %d sen", cents, employees, rp*100+sen)
	}
}

// Regresi: GET /demo (dipanggil setiap halaman login dibuka) tidak boleh memakan kuota login per IP.
func TestDemoSandbox_DiscoveryDoesNotConsumeLoginQuota(t *testing.T) {
	tiers := ratelimit.DefaultTiers() // auth-ip asli: 30/menit per IP
	e, _ := sandboxEnv(t, &tiers, nil)
	f := e.newTenantFx(t, "nat")
	ip := map[string]string{"X-Forwarded-For": "198.51.100.77"}
	for range 35 {
		if r, _ := e.doH(t, http.MethodGet, "/api/v1/demo", "", ip, nil); r.StatusCode != http.StatusOK {
			t.Fatalf("GET /demo: %d", r.StatusCode)
		}
	}
	r, _ := e.doH(t, http.MethodPost, "/api/v1/auth/login", "", ip, map[string]string{"email": f.emails[roleO], "password": password})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("login after 35 discovery calls from the same IP: want 200, got %d", r.StatusCode)
	}
}
