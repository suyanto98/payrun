package app_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/internal/app"
	"payrun/internal/modules/iam"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/crypto"
	"payrun/internal/platform/db"
	"payrun/internal/platform/db/dbtest"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/mail"
	"payrun/internal/platform/ratelimit"
)

// helperProcess: diisi file ber-tag testhooks. Test "worker mati" menjalankan ulang binary test ini
// sebagai proses worker sungguhan (tanpa container DB baru); proses itu keluar lewat os.Exit di
// tengah transaksi, persis seperti worker yang di-kill.
var helperProcess func() int

func TestMain(m *testing.M) {
	if helperProcess != nil && os.Getenv(helperWorkerDSN) != "" {
		os.Exit(helperProcess())
	}
	dbtest.Main(m)
}

// env: server API sungguhan (httptest) di atas DB test yang terhubung sebagai payrun_app, dengan jam
// palsu yang dipakai token, refresh, dan lockout.
type env struct {
	srv    *httptest.Server
	pool   *pgxpool.Pool
	clk    *clock.Fake
	mailer *fakeMailer
	tdb    dbtest.TestDB // akses superuser untuk menyiapkan state yang tidak bisa dicapai lewat API
	// handler adalah router API (dipakai test matriks untuk chi.Walk).
	handler http.Handler
	// worker: lane yang sama dengan `payrun worker`, dijalankan sinkron lewat drainJobs (chunk kecil
	// supaya run beberapa karyawan sudah melewati lebih dari satu chunk).
	worker *app.Worker
	deps   app.Deps
}

// fakeMailer merekam email alih-alih mengirim; fail = simulasi SMTP mati.
type fakeMailer struct {
	mu   sync.Mutex
	msgs []mail.Message
	fail bool
}

func (m *fakeMailer) Send(_ context.Context, msg mail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("smtp down")
	}
	m.msgs = append(m.msgs, msg)
	return nil
}

func (m *fakeMailer) setFail(v bool) { m.mu.Lock(); m.fail = v; m.mu.Unlock() }

// lastToken mengambil token dari link verifikasi terakhir yang dikirim ke alamat itu.
func (m *fakeMailer) lastToken(t *testing.T, to string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.msgs) - 1; i >= 0; i-- {
		if m.msgs[i].To != to {
			continue
		}
		_, after, ok := strings.Cut(m.msgs[i].Text, "/verify-email?token=")
		if !ok {
			continue
		}
		tok, _, _ := strings.Cut(after, "\n")
		v, err := url.QueryUnescape(tok)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	t.Fatalf("no verification email sent to %s", to)
	return ""
}

func (m *fakeMailer) count() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.msgs) }

const password = "correct horse battery"

// testTiers: tier default, kecuali auth-strict yang dilonggarkan supaya test lockout/refresh menguji
// logika sesi, bukan rate limit. Test rate limit memakai newEnvTiers(DefaultTiers()).
func testTiers() *ratelimit.Tiers {
	t := ratelimit.DefaultTiers()
	t.AuthStrict.Rules = []ratelimit.Rule{{Events: 1000, Per: time.Minute}}
	t.AuthIP.Rules = []ratelimit.Rule{{Events: 1000, Per: time.Minute}}
	return &t
}

func newEnv(t *testing.T) env { return newEnvTiers(t, testTiers()) }

func newEnvTiers(t *testing.T, tiers *ratelimit.Tiers) env {
	t.Helper()
	dbtest.SkipIfShort(t)
	tdb := dbtest.New(t)
	clk, ids := clock.NewFake(time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)), idgen.V7{}
	tokens, err := auth.NewTokens(map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")}, "k1", "payrun", 15*time.Minute, clk, ids)
	if err != nil {
		t.Fatal(err)
	}
	deps := app.Deps{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Pool: tdb.App, Tokens: tokens, Clock: clk, IDs: ids,
		// Loopback = proxy tepercaya: test mensimulasikan banyak IP client lewat X-Forwarded-For.
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")},
		AllowedOrigins: []string{"http://localhost:5173"}, PublicBaseURL: "http://localhost:5173", Tiers: tiers,
		IAM:    iam.Config{Argon2: auth.TestArgon2, RefreshTTL: 7 * 24 * time.Hour, RefreshFamilyTTL: 30 * 24 * time.Hour},
		Mailer: &fakeMailer{}, Keyring: testKeyring(t), BlindIndex: testBlindIndex(t),
	}
	if err := deps.ValidateAPI(); err != nil { // env test harus lengkap seperti produksi
		t.Fatal(err)
	}
	h := app.APIRouter(deps)
	e := env{srv: httptest.NewServer(h), pool: tdb.App, clk: clk, mailer: deps.Mailer.(*fakeMailer), tdb: tdb, handler: h,
		worker: app.NewWorker(deps, app.WorkerConfig{ID: "test:" + newID().String()[:8], ChunkSize: 2}), deps: deps}
	t.Cleanup(e.srv.Close)
	return e
}

// withWorker: worker kedua (proses lain) dengan kill switch kalkulasi payroll on/off.
func (e env) withWorker(t *testing.T, disablePayroll bool) *app.Worker {
	t.Helper()
	return app.NewWorker(e.deps, app.WorkerConfig{ID: "test:" + newID().String()[:8], ChunkSize: 2, DisablePayrollCalc: disablePayroll})
}

type result struct {
	StatusCode int
	Header     http.Header
	Cookies    []*http.Cookie
}

func (e env) do(t *testing.T, method, path, token string, body any) (result, map[string]any) {
	t.Helper()
	return e.doH(t, method, path, token, nil, body)
}

func (e env) doH(t *testing.T, method, path, token string, headers map[string]string, body any) (result, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, e.srv.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return result{StatusCode: resp.StatusCode, Header: resp.Header, Cookies: resp.Cookies()}, out
}

// cookiePost memanggil endpoint cookie (refresh/logout) seperti SPA: cookie + X-Requested-With.
func (e env) cookiePost(t *testing.T, path, cookie string, headers map[string]string) (result, map[string]any) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, e.srv.URL+path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: iam.RefreshCookieName, Value: cookie})
	}
	if headers == nil {
		headers = map[string]string{}
	}
	if _, ok := headers["X-Requested-With"]; !ok {
		headers["X-Requested-With"] = "fetch"
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return result{StatusCode: resp.StatusCode, Header: resp.Header, Cookies: resp.Cookies()}, out
}

func cookieValue(r result) string {
	for _, c := range r.Cookies {
		if c.Name == iam.RefreshCookieName {
			return c.Value
		}
	}
	return ""
}

// ---------------------------------------------------------------- fixture langsung di DB
// (signup lewat HTTP datang di tahap tenancy; fixture memakai jalur RLS yang sama dengan produksi)

func newID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

func (e env) createUser(t *testing.T, email string) uuid.UUID {
	t.Helper()
	hash, err := auth.HashPassword(password, auth.TestArgon2)
	if err != nil {
		t.Fatal(err)
	}
	id := newID()
	if err := db.WithPlatformTx(context.Background(), e.pool, id, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`INSERT INTO users (id, email, password_hash, display_name) VALUES ($1, $2, $3, 'Test')`, id, email, hash)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e env) createTenant(t *testing.T, slug string, owner uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := newID()
	if err := db.WithTenantTxFor(ctx, e.pool, id, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1, $2, $2)`, id, slug); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO tenant_settings (tenant_id) VALUES ($1)`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT seed_system_components($1, $2)`, id, owner); err != nil { // seed O6, sama dengan signup
			return err
		}
		return addMember(ctx, tx, id, owner, "OWNER")
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func addMember(ctx context.Context, tx pgx.Tx, tenant, user uuid.UUID, roles ...string) error {
	_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, id, user_id, roles, status, accepted_at)
		VALUES ($1, $2, $3, $4, 'ACTIVE', now())`, tenant, newID(), user, roles)
	return err
}

func (e env) addMember(t *testing.T, tenant, user uuid.UUID, roles ...string) {
	t.Helper()
	if err := db.WithTenantTxFor(context.Background(), e.pool, tenant, func(tx pgx.Tx) error {
		return addMember(context.Background(), tx, tenant, user, roles...)
	}); err != nil {
		t.Fatal(err)
	}
}

// inTenant menjalankan SQL di bawah RLS tenant (contoh: mencabut membership, membaca audit tenant).
// newMember: user terverifikasi baru + membership ACTIVE di tenant; mengembalikan id membership.
func (e env) newMember(t *testing.T, tenant uuid.UUID, email string, roles ...string) uuid.UUID {
	t.Helper()
	u := e.createUser(t, email)
	e.verifyUser(t, u)
	e.addMember(t, tenant, u, roles...)
	var id uuid.UUID
	e.inTenant(t, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT id FROM memberships WHERE user_id = $1`, u).Scan(&id)
	})
	return id
}

func (e env) inTenant(t *testing.T, tenant uuid.UUID, fn func(tx pgx.Tx) error) {
	t.Helper()
	if err := db.WithTenantTxFor(context.Background(), e.pool, tenant, fn); err != nil {
		t.Fatal(err)
	}
}

// tryInTenant: seperti inTenant, tetapi mengembalikan error (menguji penolakan DB).
func (e env) tryInTenant(tenant uuid.UUID, fn func(tx pgx.Tx) error) error {
	return db.WithTenantTxFor(context.Background(), e.pool, tenant, fn)
}

func (e env) login(t *testing.T, email string) (token, cookie string, body map[string]any) {
	t.Helper()
	resp, body := e.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": email, "password": password})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login %s: %d %v", email, resp.StatusCode, body)
	}
	return body["access_token"].(string), cookieValue(resp), body
}

func activeTenantID(body map[string]any) string {
	at, _ := body["active_tenant"].(map[string]any)
	id, _ := at["id"].(string)
	return id
}

func testKeyring(t *testing.T) *crypto.Keyring {
	t.Helper()
	kr, err := crypto.ParseKeyring(`{"v1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))+`"}`, "v1")
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

func testBlindIndex(t *testing.T) *crypto.BlindIndex {
	t.Helper()
	bi, err := crypto.ParseBlindIndexKey(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return bi
}
