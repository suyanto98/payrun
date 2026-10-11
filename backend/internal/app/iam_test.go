package app_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"payrun/internal/platform/ratelimit"
)

// ---------------------------------------------------------------- login & tenant (PF-02, ADR-P02)

// User dengan satu membership: login langsung membawa tenant itu (token ber-tid), dan /me menampilkannya.
func TestLogin_SingleTenantIsSelected(t *testing.T) {
	e := newEnv(t)
	dewi := e.createUser(t, "dewi@alpha.test")
	alpha := e.createTenant(t, "alpha", dewi)

	token, cookie, body := e.login(t, "DEWI@alpha.test") // email case-insensitive
	if activeTenantID(body) != alpha.String() || cookie == "" {
		t.Fatalf("want active_tenant %s and a refresh cookie, got %v (cookie %q)", alpha, body, cookie)
	}
	if u, _ := body["user"].(map[string]any); u["email"] != "dewi@alpha.test" {
		t.Fatalf("login response must carry the user: %v", body)
	}
	resp, me := e.do(t, http.MethodGet, "/api/v1/me", token, nil)
	if resp.StatusCode != http.StatusOK || activeTenantID(me) != alpha.String() || me["employee_id"] != nil {
		t.Fatalf("/me: %d %v", resp.StatusCode, me)
	}
	if roles := me["active_tenant"].(map[string]any)["roles"].([]any); len(roles) != 1 || roles[0] != "OWNER" {
		t.Fatalf("/me roles: %v", roles)
	}
}

// User multi-tenant: login tanpa tid; switch-tenant menerbitkan token ber-tid setelah cek membership,
// dan dicatat di audit tenant tujuan.
func TestLogin_MultiTenantThenSwitch(t *testing.T) {
	e := newEnv(t)
	dewi, budi, k := e.createUser(t, "dewi@alpha.test"), e.createUser(t, "budi@beta.test"), e.createUser(t, "k@payroll.test")
	alpha, beta := e.createTenant(t, "alpha", dewi), e.createTenant(t, "beta", budi)
	e.addMember(t, alpha, k, "PAYROLL_ADMIN")
	e.addMember(t, beta, k, "HR_ADMIN", "MANAGER")

	token, _, body := e.login(t, "k@payroll.test")
	if body["active_tenant"] != nil {
		t.Fatalf("multi-tenant login must not pick a tenant: %v", body)
	}
	resp, ms := e.do(t, http.MethodGet, "/api/v1/me/memberships", token, nil)
	if data, _ := ms["data"].([]any); resp.StatusCode != http.StatusOK || len(data) != 2 {
		t.Fatalf("memberships: %d %v", resp.StatusCode, ms)
	}

	resp, sw := e.do(t, http.MethodPost, "/api/v1/session/switch-tenant", token, map[string]string{"tenant_id": beta.String()})
	if resp.StatusCode != http.StatusOK || activeTenantID(sw) != beta.String() || sw["user"] != nil {
		t.Fatalf("switch: %d %v", resp.StatusCode, sw)
	}
	betaToken := sw["access_token"].(string)
	_, me := e.do(t, http.MethodGet, "/api/v1/me", betaToken, nil)
	if roles := me["active_tenant"].(map[string]any)["roles"].([]any); activeTenantID(me) != beta.String() || len(roles) != 2 {
		t.Fatalf("/me after switch: %v", me)
	}
	e.inTenant(t, beta, func(tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'session.tenant_switched' AND actor_id = $1`, k).Scan(&n)
		if n != 1 {
			t.Errorf("switch must be audited in the target tenant, got %d", n)
		}
		return err
	})
}

// Switch ke tenant yang bukan milik user = 404 (tidak dibedakan dari "tidak ada"); tenant disuspend
// = 403; input bukan UUID atau field tak dikenal = 400.
func TestSwitchTenant_Errors(t *testing.T) {
	e := newEnv(t)
	dewi, budi := e.createUser(t, "dewi@alpha.test"), e.createUser(t, "budi@beta.test")
	alpha, beta := e.createTenant(t, "alpha", dewi), e.createTenant(t, "beta", budi)
	gamma := e.createTenant(t, "gamma", dewi)
	token, _, _ := e.login(t, "dewi@alpha.test")

	for name, c := range map[string]struct {
		body any
		want int
	}{
		"other tenant (not a member)": {map[string]string{"tenant_id": beta.String()}, http.StatusNotFound},
		"tenant does not exist":       {map[string]string{"tenant_id": newID().String()}, http.StatusNotFound},
		"not a uuid":                  {map[string]string{"tenant_id": "alpha"}, http.StatusBadRequest},
		"unknown field":               {map[string]string{"tenant_id": alpha.String(), "role": "OWNER"}, http.StatusBadRequest},
	} {
		if resp, body := e.do(t, http.MethodPost, "/api/v1/session/switch-tenant", token, c.body); resp.StatusCode != c.want {
			t.Errorf("%s: want %d, got %d %v", name, c.want, resp.StatusCode, body)
		}
	}
	e.inTenant(t, gamma, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE tenants SET status = 'SUSPENDED', suspended_at = now()`)
		return err
	})
	if resp, body := e.do(t, http.MethodPost, "/api/v1/session/switch-tenant", token, map[string]string{"tenant_id": gamma.String()}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("suspended tenant: want 403, got %d %v", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, http.MethodPost, "/api/v1/session/switch-tenant", "", map[string]string{"tenant_id": alpha.String()}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without token: want 401, got %d", resp.StatusCode)
	}
}

// Email tidak terdaftar dan password salah menghasilkan respons yang identik (selain request_id).
func TestLogin_ErrorsAreIndistinguishable(t *testing.T) {
	e := newEnv(t)
	e.createTenant(t, "alpha", e.createUser(t, "dewi@alpha.test"))
	r1, b1 := e.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": "nobody@alpha.test", "password": password})
	r2, b2 := e.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": "dewi@alpha.test", "password": "wrong password"})
	delete(b1, "request_id")
	delete(b2, "request_id")
	if r1.StatusCode != http.StatusUnauthorized || r2.StatusCode != http.StatusUnauthorized || fmt.Sprint(b1) != fmt.Sprint(b2) || b1["code"] != "INVALID_CREDENTIALS" {
		t.Fatalf("login errors differ:\n unknown email: %d %v\n wrong password: %d %v", r1.StatusCode, b1, r2.StatusCode, b2)
	}
}

// Lockout bertahap (C-03): gagal ke-5 mengunci 1 menit. Selama terkunci, password BENAR pun ditolak
// dengan respons yang sama persis dengan password salah, tanpa Retry-After (final review R2-5: status lock
// tidak boleh menjadi oracle enumerasi akun). Setelah lock lewat, login berhasil dan counter direset.
func TestLogin_ProgressiveLockout(t *testing.T) {
	e := newEnv(t)
	e.createTenant(t, "alpha", e.createUser(t, "dewi@alpha.test"))
	bad := map[string]string{"email": "dewi@alpha.test", "password": "wrong password"}
	for range 5 {
		e.do(t, http.MethodPost, "/api/v1/auth/login", "", bad)
	}
	r, body := e.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": "dewi@alpha.test", "password": password})
	if r.StatusCode != http.StatusUnauthorized || body["code"] != "INVALID_CREDENTIALS" || r.Header.Get("Retry-After") != "" {
		t.Fatalf("locked account: want plain 401 INVALID_CREDENTIALS without Retry-After, got %d %v %v", r.StatusCode, body, r.Header)
	}
	e.clk.Advance(61 * time.Second)
	if _, _, body := e.login(t, "dewi@alpha.test"); body["access_token"] == nil {
		t.Fatal("login after the lock expired must succeed")
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM auth_lookup_user('dewi@alpha.test') WHERE failed_login_count = 0 AND locked_until IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("successful login must reset the lockout counter (n=%d, err=%v)", n, err)
	}
}

// Review S1: power(2, n - threshold) dulu overflow float8 setelah ± 1000 gagal di atas threshold
// (dicapai penyerang sabar: satu percobaan per lock 15 menit), sehingga setiap password salah menjadi
// 500 dan lockout berhenti bekerja. Eksponen dibatasi: counter besar tetap mengunci maksimal 15 menit.
func TestLogin_LockoutSurvivesHugeFailureCount(t *testing.T) {
	e := newEnv(t)
	e.createTenant(t, "alpha", e.createUser(t, "dewi@alpha.test"))
	if _, err := e.tdb.Super(t).Exec(context.Background(),
		`UPDATE payrun.users SET failed_login_count = 5000 WHERE email = 'dewi@alpha.test'`); err != nil {
		t.Fatal(err)
	}
	r, body := e.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": "dewi@alpha.test", "password": "wrong password"})
	if r.StatusCode != http.StatusUnauthorized || body["code"] != "INVALID_CREDENTIALS" {
		t.Fatalf("wrong password with a huge counter: want 401 INVALID_CREDENTIALS, got %d %v", r.StatusCode, body)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM auth_lookup_user('dewi@alpha.test')
		WHERE failed_login_count = 5001 AND abs(extract(epoch FROM locked_until - $1::timestamptz)) < 1`, e.clk.Now().Add(15*time.Minute)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("counter must increase and lock for the 15 minute maximum (n=%d, err=%v)", n, err)
	}
}

// ---------------------------------------------------------------- refresh token (C-05)

func TestRefresh_RotatesToken(t *testing.T) {
	e := newEnv(t)
	e.createTenant(t, "alpha", e.createUser(t, "dewi@alpha.test"))
	_, c0, _ := e.login(t, "dewi@alpha.test")
	resp, body := e.cookiePost(t, "/api/v1/auth/refresh", c0, nil)
	c1 := cookieValue(resp)
	if resp.StatusCode != http.StatusOK || c1 == "" || c1 == c0 {
		t.Fatalf("refresh must rotate the cookie: %d %v (new=%q)", resp.StatusCode, body, c1)
	}
	if r, _ := e.do(t, http.MethodGet, "/api/v1/me", body["access_token"].(string), nil); r.StatusCode != http.StatusOK {
		t.Fatalf("new access token must work, got %d", r.StatusCode)
	}
}

// refresh_reuse_revokes_family: token lama dipakai lagi di luar grace → seluruh family dicabut,
// termasuk token terbaru milik korban, dan dicatat di audit.
func TestRefresh_ReuseRevokesFamily(t *testing.T) {
	e := newEnv(t)
	e.createTenant(t, "alpha", e.createUser(t, "dewi@alpha.test"))
	_, c0, _ := e.login(t, "dewi@alpha.test")
	r1, _ := e.cookiePost(t, "/api/v1/auth/refresh", c0, nil)
	c1 := cookieValue(r1)

	e.clk.Advance(11 * time.Second) // lewat grace window 10 dtk
	r2, b2 := e.cookiePost(t, "/api/v1/auth/refresh", c0, nil)
	if r2.StatusCode != http.StatusUnauthorized || b2["code"] != "REFRESH_TOKEN_REUSED" {
		t.Fatalf("reuse: want 401 REFRESH_TOKEN_REUSED, got %d %v", r2.StatusCode, b2)
	}
	if r3, b3 := e.cookiePost(t, "/api/v1/auth/refresh", c1, nil); r3.StatusCode != http.StatusUnauthorized || b3["code"] != "UNAUTHENTICATED" {
		t.Fatalf("latest token of a revoked family must be dead, got %d %v", r3.StatusCode, b3)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM refresh_tokens WHERE revoke_reason = 'REUSE_DETECTED'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("both tokens of the family must be revoked, got %d (%v)", n, err)
	}
}

// Dua tab me-refresh token yang sama bersamaan → keduanya 200, tepat satu menerima cookie baru, sesi
// tidak dicabut (FOR UPDATE menserialkan keduanya; yang kedua masuk grace window).
func TestRefresh_ParallelWithinGrace(t *testing.T) {
	e := newEnv(t)
	e.createTenant(t, "alpha", e.createUser(t, "dewi@alpha.test"))
	_, c0, _ := e.login(t, "dewi@alpha.test")
	var (
		mu      sync.Mutex
		results []result
		wg      sync.WaitGroup
	)
	for range 2 {
		wg.Go(func() {
			r, _ := e.cookiePost(t, "/api/v1/auth/refresh", c0, nil)
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		})
	}
	wg.Wait()
	withCookie := 0
	for _, r := range results {
		if r.StatusCode != http.StatusOK {
			t.Fatalf("parallel refresh within grace must succeed, got %d", r.StatusCode)
		}
		if cookieValue(r) != "" {
			withCookie++
		}
	}
	if withCookie != 1 {
		t.Fatalf("exactly one response may rotate the cookie, got %d", withCookie)
	}
}

// Batas absolut family 30 hari: walaupun di-refresh terus (sliding 7 hari), sesi berakhir.
func TestRefresh_FamilyAbsoluteExpiry(t *testing.T) {
	e := newEnv(t)
	e.createTenant(t, "alpha", e.createUser(t, "dewi@alpha.test"))
	_, c, _ := e.login(t, "dewi@alpha.test")
	for range 5 { // 5 × 6 hari = 30 hari
		e.clk.Advance(6 * 24 * time.Hour)
		r, b := e.cookiePost(t, "/api/v1/auth/refresh", c, nil)
		if r.StatusCode != http.StatusOK {
			if r.StatusCode == http.StatusUnauthorized && b["code"] == "UNAUTHENTICATED" {
				return // family habis tepat pada hari ke-30
			}
			t.Fatalf("unexpected refresh failure: %d %v", r.StatusCode, b)
		}
		c = cookieValue(r)
	}
	e.clk.Advance(time.Hour)
	if r, _ := e.cookiePost(t, "/api/v1/auth/refresh", c, nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("family past 30 days must be dead, got %d", r.StatusCode)
	}
}

// ADR-P02: refresh dengan X-Active-Tenant mempertahankan tid HANYA kalau membership masih ACTIVE, dan
// roles dibaca ulang dari DB. Membership dicabut → 200 dengan active_tenant null, token tanpa tid.
func TestRefresh_ActiveTenantRecheckedInDB(t *testing.T) {
	e := newEnv(t)
	dewi, k := e.createUser(t, "dewi@alpha.test"), e.createUser(t, "k@payroll.test")
	alpha := e.createTenant(t, "alpha", dewi)
	e.addMember(t, alpha, k, "EMPLOYEE")
	_, c, _ := e.login(t, "k@payroll.test")
	hdr := func(c string) map[string]string { return map[string]string{"X-Active-Tenant": alpha.String()} }

	e.inTenant(t, alpha, func(tx pgx.Tx) error { // role dinaikkan oleh owner
		_, err := tx.Exec(context.Background(), `UPDATE memberships SET roles = ARRAY['PAYROLL_ADMIN'] WHERE user_id = $1`, k)
		return err
	})
	r, b := e.cookiePost(t, "/api/v1/auth/refresh", c, hdr(c))
	if roles, _ := b["active_tenant"].(map[string]any)["roles"].([]any); r.StatusCode != http.StatusOK || len(roles) != 1 || roles[0] != "PAYROLL_ADMIN" {
		t.Fatalf("refresh must re-read roles from the DB: %d %v", r.StatusCode, b)
	}
	c = cookieValue(r)

	e.inTenant(t, alpha, func(tx pgx.Tx) error { // membership dicabut
		_, err := tx.Exec(context.Background(), `UPDATE memberships SET status = 'REVOKED', revoked_at = now() WHERE user_id = $1`, k)
		return err
	})
	e.clk.Advance(time.Minute)
	r, b = e.cookiePost(t, "/api/v1/auth/refresh", c, hdr(c))
	if r.StatusCode != http.StatusOK || b["active_tenant"] != nil {
		t.Fatalf("revoked membership: want 200 with active_tenant null, got %d %v", r.StatusCode, b)
	}
	_, me := e.do(t, http.MethodGet, "/api/v1/me", b["access_token"].(string), nil)
	if me["active_tenant"] != nil {
		t.Fatalf("token after revocation must carry no tenant: %v", me)
	}
	if r, b := e.cookiePost(t, "/api/v1/auth/refresh", cookieValue(r), map[string]string{"X-Active-Tenant": "not-a-uuid"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid X-Active-Tenant: want 400, got %d %v", r.StatusCode, b)
	}
}

// CSRF (C-08): endpoint cookie menolak request tanpa X-Requested-With: fetch atau dari origin asing.
func TestCSRF_CookieEndpointsGuarded(t *testing.T) {
	e := newEnv(t)
	e.createTenant(t, "alpha", e.createUser(t, "dewi@alpha.test"))
	_, c, _ := e.login(t, "dewi@alpha.test")
	for name, h := range map[string]map[string]string{
		"missing X-Requested-With": {"X-Requested-With": ""},
		"foreign origin":           {"Origin": "https://evil.example"},
	} {
		for _, path := range []string{"/api/v1/auth/refresh", "/api/v1/auth/logout"} {
			if r, _ := e.cookiePost(t, path, c, h); r.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s: want 403, got %d", name, path, r.StatusCode)
			}
		}
	}
	if r, _ := e.cookiePost(t, "/api/v1/auth/refresh", c, map[string]string{"Origin": "http://localhost:5173"}); r.StatusCode != http.StatusOK {
		t.Fatalf("allowed origin must pass, got %d", r.StatusCode)
	}
}

// Logout mencabut family dan selalu 204 (juga untuk cookie yang sudah dicabut).
func TestLogout_RevokesSessionIdempotently(t *testing.T) {
	e := newEnv(t)
	e.createTenant(t, "alpha", e.createUser(t, "dewi@alpha.test"))
	_, c, _ := e.login(t, "dewi@alpha.test")
	for range 2 {
		if r, _ := e.cookiePost(t, "/api/v1/auth/logout", c, nil); r.StatusCode != http.StatusNoContent {
			t.Fatalf("logout: want 204, got %d", r.StatusCode)
		}
	}
	if r, _ := e.cookiePost(t, "/api/v1/auth/refresh", c, nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refresh after logout: want 401, got %d", r.StatusCode)
	}
}

// /me tanpa token, dengan token rusak, atau token kedaluwarsa → 401.
func TestMe_RequiresValidToken(t *testing.T) {
	e := newEnv(t)
	e.createTenant(t, "alpha", e.createUser(t, "dewi@alpha.test"))
	token, _, _ := e.login(t, "dewi@alpha.test")
	if r, _ := e.do(t, http.MethodGet, "/api/v1/me", "", nil); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: want 401, got %d", r.StatusCode)
	}
	if r, _ := e.do(t, http.MethodGet, "/api/v1/me", token+"x", nil); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("tampered token: want 401, got %d", r.StatusCode)
	}
	e.clk.Advance(16 * time.Minute)
	if r, b := e.do(t, http.MethodGet, "/api/v1/me", token, nil); r.StatusCode != http.StatusUnauthorized || b["code"] != "TOKEN_EXPIRED" {
		t.Errorf("expired token: want 401 TOKEN_EXPIRED, got %d %v", r.StatusCode, b)
	}
}

// auth-strict per (IP, email): email ke-6 dari IP yang sama ditolak 429 + Retry-After, email lain dan
// IP lain tidak terpengaruh. Email tak terdaftar dibatasi sama (bukan oracle enumerasi).
func TestRateLimit_LoginPerIPAndEmail(t *testing.T) {
	tiers := ratelimit.DefaultTiers()
	e := newEnvTiers(t, &tiers)
	e.createTenant(t, "alpha", e.createUser(t, "dewi@alpha.test"))
	from := func(ip, email string) result {
		r, _ := e.doH(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"X-Forwarded-For": ip},
			map[string]string{"email": email, "password": "wrong password"})
		return r
	}
	for _, email := range []string{"dewi@alpha.test", "ghost@alpha.test"} {
		for range 5 {
			from("203.0.113.7", email)
		}
		if r := from("203.0.113.7", email); r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
			t.Fatalf("%s: 6th attempt from the same IP: want 429 with Retry-After, got %d", email, r.StatusCode)
		}
		if r := from("198.51.100.9", email); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: another IP must not be limited, got %d", email, r.StatusCode)
		}
	}
}
