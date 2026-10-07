package app_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------- signup (PF-01) + verifikasi email (R2-8)

func signupBody(slug, email string) map[string]any {
	return map[string]any{
		"company_name": "PT " + slug, "slug": slug, "timezone": "Asia/Makassar",
		"owner": map[string]string{"email": email, "password": password, "display_name": "Dewi"},
	}
}

func (e env) signup(t *testing.T, key string, body map[string]any) (result, map[string]any) {
	t.Helper()
	h := map[string]string{}
	if key != "" {
		h["Idempotency-Key"] = key
	}
	return e.doH(t, http.MethodPost, "/api/v1/tenants/signup", "", h, body)
}

func (e env) countTenants(t *testing.T) int {
	t.Helper()
	var n int
	// payrun_app tanpa context melihat 0 baris tenants (RLS), jadi yang dihitung adalah signup yang
	// selesai di platform_idempotency_keys (tanpa RLS): satu baris 201 per tenant yang dibuat.
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM platform_idempotency_keys WHERE response_status = 201`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func tenantOf(body map[string]any) (id, status string) {
	tn, _ := body["tenant"].(map[string]any)
	id, _ = tn["id"].(string)
	status, _ = tn["status"].(string)
	return id, status
}

// Email baru: tenant PENDING_VERIFICATION, login berhasil tetapi tenant belum bisa dipilih; link di
// email mengaktifkan tenant, setelah itu switch-tenant dan GET /tenant berjalan.
func TestSignup_NewUserTenantPendingUntilEmailVerified(t *testing.T) {
	e := newEnv(t)
	resp, body := e.signup(t, newID().String(), signupBody("alpha", "dewi@alpha.test"))
	tid, status := tenantOf(body)
	if resp.StatusCode != http.StatusCreated || status != "PENDING_VERIFICATION" || body["email_verification_required"] != true ||
		body["active_tenant"] != nil || cookieValue(resp) == "" || body["access_token"] == nil {
		t.Fatalf("signup: %d %v", resp.StatusCode, body)
	}
	token := body["access_token"].(string)
	if u, _ := body["user"].(map[string]any); u["email_verified"] != false {
		t.Fatalf("new user must be unverified: %v", body["user"])
	}
	if r, b := e.do(t, http.MethodGet, "/api/v1/tenant", token, nil); r.StatusCode != http.StatusForbidden || b["detail"] != "Select a tenant first" {
		t.Fatalf("GET /tenant without tid: want 403 Select a tenant first, got %d %v", r.StatusCode, b)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/session/switch-tenant", token, map[string]string{"tenant_id": tid}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("switch to a pending tenant: want 403, got %d %v", r.StatusCode, b)
	}

	link := e.mailer.lastToken(t, "dewi@alpha.test")
	if r, b := e.do(t, http.MethodPost, "/api/v1/auth/verify-email", "", map[string]string{"token": link, "password": password}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("verify: %d %v", r.StatusCode, b)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/auth/verify-email", "", map[string]string{"token": link, "password": password}); r.StatusCode != http.StatusGone || b["code"] != "EMAIL_VERIFICATION_INVALID" {
		t.Fatalf("link reuse: want 410 EMAIL_VERIFICATION_INVALID, got %d %v", r.StatusCode, b)
	}

	r, sw := e.do(t, http.MethodPost, "/api/v1/session/switch-tenant", token, map[string]string{"tenant_id": tid})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("switch after verification: %d %v", r.StatusCode, sw)
	}
	r, tn := e.do(t, http.MethodGet, "/api/v1/tenant", sw["access_token"].(string), nil)
	settings, _ := tn["settings"].(map[string]any)
	if r.StatusCode != http.StatusOK || tn["status"] != "ACTIVE" || tn["slug"] != "alpha" || r.Header.Get("ETag") != `"1"` ||
		settings["timezone"] != "Asia/Makassar" || settings["cutoff_time_local"] != "23:59" || settings["net_change_warning_pct"] != "20.00" ||
		settings["overtime_max_hours_per_day"] != "4.0" {
		t.Fatalf("GET /tenant: %d %v (etag %q)", r.StatusCode, tn, r.Header.Get("ETag"))
	}
	if _, me := e.do(t, http.MethodGet, "/api/v1/me", token, nil); me["email_verified"] != true {
		t.Fatalf("/me after verification: %v", me)
	}
	// Owner sekarang login dengan satu tenant aktif → langsung terpilih.
	if _, _, lb := e.login(t, "dewi@alpha.test"); activeTenantID(lb) != tid {
		t.Fatalf("login after activation must select the tenant: %v", lb)
	}
}

// Idempotency (platform_idempotency_keys): retry dengan key + body sama → replay 201 yang sama tanpa
// token dan tanpa tenant kedua; key sama dengan email lain → 422; tanpa key → 400.
func TestSignup_Idempotency(t *testing.T) {
	e := newEnv(t)
	key := newID().String()
	r1, b1 := e.signup(t, key, signupBody("alpha", "dewi@alpha.test"))
	r2, b2 := e.signup(t, key, signupBody("alpha", "dewi@alpha.test"))
	id1, _ := tenantOf(b1)
	id2, _ := tenantOf(b2)
	if r1.StatusCode != http.StatusCreated || r2.StatusCode != http.StatusCreated || id1 != id2 ||
		r2.Header.Get("Idempotent-Replayed") != "true" || b2["access_token"] != nil {
		t.Fatalf("replay: first %d %v, second %d %v %v", r1.StatusCode, b1, r2.StatusCode, b2, r2.Header)
	}
	if n := e.countTenants(t); n != 1 {
		t.Fatalf("retry must not create a second tenant, got %d", n)
	}
	if e.mailer.count() != 1 {
		t.Fatalf("replay must not resend the verification email, sent %d", e.mailer.count())
	}
	if r, b := e.signup(t, key, signupBody("alpha", "other@alpha.test")); r.StatusCode != http.StatusUnprocessableEntity || b["code"] != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("same key, different email: want 422, got %d %v", r.StatusCode, b)
	}
	if r, b := e.signup(t, "", signupBody("beta", "budi@beta.test")); r.StatusCode != http.StatusBadRequest || b["code"] != "IDEMPOTENCY_KEY_REQUIRED" {
		t.Fatalf("missing key: %d %v", r.StatusCode, b)
	}
	if r, b := e.signup(t, "short", signupBody("beta", "budi@beta.test")); r.StatusCode != http.StatusBadRequest || b["code"] != "IDEMPOTENCY_KEY_INVALID" {
		t.Fatalf("short key: %d %v", r.StatusCode, b)
	}
}

// Double-click: dua signup paralel dengan key sama → tepat satu tenant; respons kedua replay (atau 409
// REQUEST_IN_PROGRESS kalau masih menunggu), tidak pernah 5xx atau tenant ganda.
func TestSignup_ConcurrentSameKeyCreatesOneTenant(t *testing.T) {
	e := newEnv(t)
	key := newID().String()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		codes []int
	)
	for range 2 {
		wg.Go(func() {
			r, _ := e.signup(t, key, signupBody("alpha", "dewi@alpha.test"))
			mu.Lock()
			codes = append(codes, r.StatusCode)
			mu.Unlock()
		})
	}
	wg.Wait()
	for _, c := range codes {
		if c != http.StatusCreated && c != http.StatusConflict {
			t.Fatalf("parallel signup: unexpected status %v", codes)
		}
	}
	if n := e.countTenants(t); n != 1 {
		t.Fatalf("want exactly one tenant, got %d (codes %v)", n, codes)
	}
}

// Slug dipakai tenant lain → 409 DUPLICATE_CODE dengan field slug (unique index tidak difilter RLS).
func TestSignup_DuplicateSlug(t *testing.T) {
	e := newEnv(t)
	e.signup(t, newID().String(), signupBody("alpha", "dewi@alpha.test"))
	r, b := e.signup(t, newID().String(), signupBody("alpha", "budi@beta.test"))
	errs, _ := b["errors"].([]any)
	if r.StatusCode != http.StatusConflict || b["code"] != "DUPLICATE_CODE" || len(errs) != 1 || errs[0].(map[string]any)["field"] != "slug" {
		t.Fatalf("duplicate slug: %d %v", r.StatusCode, b)
	}
}

// Email yang sudah punya akun: password wajib cocok. Akun terverifikasi → tenant baru langsung ACTIVE
// dan terpilih; password salah → 401 dan tidak ada tenant dibuat.
func TestSignup_ExistingVerifiedUserAddsTenant(t *testing.T) {
	e := newEnv(t)
	e.signup(t, newID().String(), signupBody("alpha", "dewi@alpha.test"))
	e.do(t, http.MethodPost, "/api/v1/auth/verify-email", "", map[string]string{"token": e.mailer.lastToken(t, "dewi@alpha.test"), "password": password})

	wrong := signupBody("beta", "dewi@alpha.test")
	wrong["owner"].(map[string]string)["password"] = "wrong password!"
	if r, b := e.signup(t, newID().String(), wrong); r.StatusCode != http.StatusUnauthorized || b["code"] != "INVALID_CREDENTIALS" {
		t.Fatalf("existing email, wrong password: want 401, got %d %v", r.StatusCode, b)
	}
	r, b := e.signup(t, newID().String(), signupBody("beta", "dewi@alpha.test"))
	id, status := tenantOf(b)
	if r.StatusCode != http.StatusCreated || status != "ACTIVE" || activeTenantID(b) != id || b["email_verification_required"] != false {
		t.Fatalf("second tenant for a verified user: %d %v", r.StatusCode, b)
	}
	_, ms := e.do(t, http.MethodGet, "/api/v1/me/memberships", b["access_token"].(string), nil)
	if data, _ := ms["data"].([]any); len(data) != 2 {
		t.Fatalf("user must own both tenants: %v", ms)
	}
}

// Review S1 (pre-hijacking): penyerang mendaftar lebih dulu dengan email korban. Signup korban (password
// lain) tidak ditolak 401, melainkan mengganti kredensial akun yang belum terverifikasi: sesi dan link
// penyerang mati, password penyerang tidak berlaku lagi. Setelah email terverifikasi, reclaim berhenti.
func TestSignup_UnverifiedEmailCanBeReclaimed(t *testing.T) {
	e := newEnv(t)
	attacker := signupBody("evil", "victim@corp.test")
	attacker["owner"].(map[string]string)["password"] = "attacker password!"
	ra, ba := e.signup(t, newID().String(), attacker)
	if ra.StatusCode != http.StatusCreated {
		t.Fatalf("attacker signup: %d %v", ra.StatusCode, ba)
	}
	attackerCookie, attackerLink := cookieValue(ra), e.mailer.lastToken(t, "victim@corp.test")

	rv, bv := e.signup(t, newID().String(), signupBody("victim", "victim@corp.test"))
	if _, st := tenantOf(bv); rv.StatusCode != http.StatusCreated || st != "PENDING_VERIFICATION" {
		t.Fatalf("victim signup over an unverified account: want 201 pending, got %d %v", rv.StatusCode, bv)
	}
	if r, _ := e.cookiePost(t, "/api/v1/auth/refresh", attackerCookie, nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("attacker session must be revoked, refresh got %d", r.StatusCode)
	}
	if r, _ := e.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": "victim@corp.test", "password": "attacker password!"}); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("attacker password must stop working, login got %d", r.StatusCode)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/auth/verify-email", "", map[string]string{"token": attackerLink, "password": password}); r.StatusCode != http.StatusGone {
		t.Fatalf("link issued before the reclaim must be dead: %d %v", r.StatusCode, b)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/auth/verify-email", "", map[string]string{"token": e.mailer.lastToken(t, "victim@corp.test"), "password": password}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("victim verifies: %d %v", r.StatusCode, b)
	}
	if r, b := e.signup(t, newID().String(), attacker); r.StatusCode != http.StatusUnauthorized || b["code"] != "INVALID_CREDENTIALS" {
		t.Fatalf("verified account cannot be reclaimed: want 401, got %d %v", r.StatusCode, b)
	}
	var n int
	if err := e.tdb.Super(t).QueryRow(context.Background(),
		`SELECT count(*) FROM payrun.audit_log WHERE action = 'auth.unverified_account_reclaimed'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("reclaim must be audited once (n=%d, err=%v)", n, err)
	}
}

// Review S1: link verifikasi wajib password akun saat ini. Pemilik inbox yang mengklik link dari signup
// orang lain (password tidak diketahui) tidak bisa mengaktifkan akun itu; link tidak terpakai oleh
// percobaan yang gagal.
func TestVerifyEmail_RequiresCurrentPassword(t *testing.T) {
	e := newEnv(t)
	e.signup(t, newID().String(), signupBody("alpha", "dewi@alpha.test"))
	link := e.mailer.lastToken(t, "dewi@alpha.test")
	for name, body := range map[string]map[string]string{
		"wrong password":   {"token": link, "password": "not the password"},
		"missing password": {"token": link},
	} {
		if r, b := e.do(t, http.MethodPost, "/api/v1/auth/verify-email", "", body); r.StatusCode != http.StatusUnauthorized || b["code"] != "INVALID_CREDENTIALS" {
			t.Fatalf("%s: want 401 INVALID_CREDENTIALS, got %d %v", name, r.StatusCode, b)
		}
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/auth/verify-email", "", map[string]string{"token": link, "password": password}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("failed attempts must not consume the link: %d %v", r.StatusCode, b)
	}
}

// Akun belum terverifikasi dengan dua tenant pending: satu verifikasi mengaktifkan keduanya.
func TestVerifyEmail_ActivatesAllPendingOwnedTenants(t *testing.T) {
	e := newEnv(t)
	e.signup(t, newID().String(), signupBody("alpha", "dewi@alpha.test"))
	_, b := e.signup(t, newID().String(), signupBody("beta", "dewi@alpha.test"))
	if _, st := tenantOf(b); st != "PENDING_VERIFICATION" {
		t.Fatalf("unverified user's second tenant must be pending: %v", b)
	}
	e.do(t, http.MethodPost, "/api/v1/auth/verify-email", "", map[string]string{"token": e.mailer.lastToken(t, "dewi@alpha.test"), "password": password})
	token, _, _ := e.login(t, "dewi@alpha.test")
	_, ms := e.do(t, http.MethodGet, "/api/v1/me/memberships", token, nil)
	if data, _ := ms["data"].([]any); len(data) != 2 {
		t.Fatalf("both tenants must be active after verification: %v", ms)
	}
}

// Link kedaluwarsa → 410; kirim ulang mematikan link lama dan menerbitkan yang baru; SMTP mati saat
// signup tidak menggagalkan signup, dan kirim ulang menjawab 503 (bisa diulang).
func TestVerifyEmail_ExpiryResendAndMailFailure(t *testing.T) {
	e := newEnv(t)
	e.mailer.setFail(true)
	r, b := e.signup(t, newID().String(), signupBody("alpha", "dewi@alpha.test"))
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("signup must succeed even if the email cannot be sent: %d %v", r.StatusCode, b)
	}
	token := b["access_token"].(string)
	if r, b := e.do(t, http.MethodPost, "/api/v1/auth/resend-verification", token, nil); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("resend while SMTP is down: want 503, got %d %v", r.StatusCode, b)
	}
	e.mailer.setFail(false)
	if r, _ := e.do(t, http.MethodPost, "/api/v1/auth/resend-verification", token, nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("resend: want 204, got %d", r.StatusCode)
	}
	first := e.mailer.lastToken(t, "dewi@alpha.test")
	e.clk.Advance(25 * time.Hour)
	if r, b := e.do(t, http.MethodPost, "/api/v1/auth/verify-email", "", map[string]string{"token": first, "password": password}); r.StatusCode != http.StatusGone {
		t.Fatalf("expired link: want 410, got %d %v", r.StatusCode, b)
	}
	_, _, lb := e.login(t, "dewi@alpha.test")
	e.do(t, http.MethodPost, "/api/v1/auth/resend-verification", lb["access_token"].(string), nil)
	second := e.mailer.lastToken(t, "dewi@alpha.test")
	e.do(t, http.MethodPost, "/api/v1/auth/resend-verification", lb["access_token"].(string), nil)
	third := e.mailer.lastToken(t, "dewi@alpha.test")
	if r, _ := e.do(t, http.MethodPost, "/api/v1/auth/verify-email", "", map[string]string{"token": second, "password": password}); r.StatusCode != http.StatusGone {
		t.Fatalf("superseded link must be dead, got %d", r.StatusCode)
	}
	if r, _ := e.do(t, http.MethodPost, "/api/v1/auth/verify-email", "", map[string]string{"token": third, "password": password}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("latest link must work, got %d", r.StatusCode)
	}
	for _, bad := range []string{"", "nonsense", uuid.NewString()} {
		if r, _ := e.do(t, http.MethodPost, "/api/v1/auth/verify-email", "", map[string]string{"token": bad, "password": password}); r.StatusCode != http.StatusGone {
			t.Errorf("token %q: want 410, got %d", bad, r.StatusCode)
		}
	}
}

// Validasi input signup; field `tenant_id` di body ditolak (16-api §1: tenant tidak pernah dari body).
func TestSignup_Validation(t *testing.T) {
	e := newEnv(t)
	for name, mutate := range map[string]func(map[string]any){
		"slug format":     func(b map[string]any) { b["slug"] = "Alpha_Co" },
		"slug too short":  func(b map[string]any) { b["slug"] = "ab" },
		"short password":  func(b map[string]any) { b["owner"].(map[string]string)["password"] = "short" },
		"bad email":       func(b map[string]any) { b["owner"].(map[string]string)["email"] = "not-an-email" },
		"bad timezone":    func(b map[string]any) { b["timezone"] = "Mars/Olympus" },
		"tenant_id field": func(b map[string]any) { b["tenant_id"] = uuid.NewString() },
		"company too short": func(b map[string]any) {
			b["company_name"] = "X"
		},
	} {
		body := signupBody("alpha", "dewi@alpha.test")
		mutate(body)
		if r, b := e.signup(t, newID().String(), body); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d %v", name, r.StatusCode, b)
		}
	}
	if n := e.countTenants(t); n != 0 {
		t.Fatalf("invalid signups must not create tenants, got %d", n)
	}
}

// Isolasi lewat API: GET /tenant selalu tenant dari claim tid; dua tenant tidak saling terlihat, dan
// audit tenant.created hanya ada di tenant masing-masing.
func TestTenant_IsolationThroughAPI(t *testing.T) {
	e := newEnv(t)
	tokens := map[string]string{}
	for _, c := range []struct{ slug, email string }{{"alpha", "dewi@alpha.test"}, {"beta", "budi@beta.test"}} {
		_, b := e.signup(t, newID().String(), signupBody(c.slug, c.email))
		e.do(t, http.MethodPost, "/api/v1/auth/verify-email", "", map[string]string{"token": e.mailer.lastToken(t, c.email), "password": password})
		tok, _, _ := e.login(t, c.email)
		tid, _ := tenantOf(b)
		tokens[c.slug] = tok
		e.inTenant(t, uuid.MustParse(tid), func(tx pgx.Tx) error {
			var n int
			err := tx.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'tenant.created'`).Scan(&n)
			if n != 1 {
				t.Errorf("%s: want exactly its own tenant.created audit row, got %d", c.slug, n)
			}
			return err
		})
	}
	for slug, tok := range tokens {
		if _, b := e.do(t, http.MethodGet, "/api/v1/tenant", tok, nil); b["slug"] != slug {
			t.Errorf("token of %s sees tenant %v", slug, b["slug"])
		}
	}
}
