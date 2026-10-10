package app_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"net/http"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	gen "payrun/api/gen"
)

// ---------------------------------------------------------------- payslip (S7, PF-15, PF-22, PI-17)

// runPayslips: S3 sebagai admin → employee_id → payslip.
func (e env) runPayslips(t *testing.T, token string, run uuid.UUID) map[uuid.UUID]map[string]any {
	t.Helper()
	r, b := e.do(t, http.MethodGet, "/api/v1/payslips?run_id="+run.String(), token, nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("list payslips: %d %v", r.StatusCode, b)
	}
	assertMatchesSpec(t, "run payslips", b, &gen.RunPayslipList{})
	out := map[uuid.UUID]map[string]any{}
	for _, x := range b["data"].([]any) {
		p := x.(map[string]any)
		out[uuid.MustParse(p["employee_id"].(string))] = p
	}
	return out
}

var maskedAccount = regexp.MustCompile(`^(.+ )?••••[0-9]{4}$`)

// TestPayslipAccess_Matrix (DoD S7, PI-17, PF-15/PF-22): finalize membuat payslip PENDING + job render;
// sebelum PDF siap unduhan 409 PAYSLIP_NOT_READY (Retry-After). Pemilik melihat dan mengunduh slipnya
// tanpa audit; OWNER/HR/PAYROLL melihat slip siapa pun dan SETIAP akses ter-audit; manajer (atasan
// langsung pun) dan karyawan lain mendapat 404 tanpa URL.
func TestPayslipAccess_Matrix(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o, h, p, m, emp := f.tokens[roleO], f.tokens[roleH], f.tokens[roleP], f.tokens[roleM], f.tokens[roleE]
	run := e.approvedRun(t, f)
	if r, b := e.finalize(t, p, run, newID().String()); r.StatusCode != http.StatusOK {
		t.Fatalf("finalize: %d %v", r.StatusCode, b)
	}
	slips := e.runPayslips(t, p, run)
	if len(slips) != 3 {
		t.Fatalf("want one payslip per OK item, got %d", len(slips))
	}
	mine := uuid.MustParse(slips[f.empSelf]["id"].(string))
	other := uuid.MustParse(slips[f.empOther]["id"].(string))
	if slips[f.empSelf]["pdf_status"] != "PENDING" {
		t.Fatalf("before render: %v", slips[f.empSelf])
	}
	r, b := e.do(t, http.MethodGet, "/api/v1/payslips/"+mine.String()+"/download", emp, nil)
	if r.StatusCode != http.StatusConflict || b["code"] != "PAYSLIP_NOT_READY" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("download before render: want 409 PAYSLIP_NOT_READY + Retry-After, got %d %v", r.StatusCode, b)
	}

	e.drainJobs(t) // payslip.render
	key := "tenants/" + f.id.String() + "/payslips/" + run.String() + "/" + f.empSelf.String() + ".pdf"
	pdf, ok := e.docs.Get(key)
	if !ok || !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("object %s missing after render", key)
	}
	for emp, s := range e.runPayslips(t, p, run) {
		if s["pdf_status"] != "RENDERED" {
			t.Fatalf("payslip of %s: %v", emp, s)
		}
	}
	var sum []byte
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT checksum_sha256 FROM payslips WHERE id = $1`, mine).Scan(&sum)
	})
	if want := sha256.Sum256(pdf); !bytes.Equal(sum, want[:]) {
		t.Fatal("checksum_sha256 does not match the stored object")
	}

	// S1/S2: pemilik.
	r, list := e.do(t, http.MethodGet, "/api/v1/me/payslips?year=2026", emp, nil)
	assertMatchesSpec(t, "my payslips", list, &gen.PayslipSummaryList{})
	if data := list["data"].([]any); r.StatusCode != http.StatusOK || len(data) != 1 || data[0].(map[string]any)["id"] != mine.String() ||
		data[0].(map[string]any)["period"] != "2026-03" {
		t.Fatalf("S1 as owner: %d %v", r.StatusCode, list)
	}
	if _, list := e.do(t, http.MethodGet, "/api/v1/me/payslips", o, nil); len(list["data"].([]any)) != 0 {
		t.Fatalf("S1 for an account without an employee: %v", list)
	}
	viewed, downloaded := e.auditCount(t, f.id, "payslip.viewed_by_admin"), e.auditCount(t, f.id, "payslip.downloaded")
	r, slip := e.do(t, http.MethodGet, "/api/v1/me/payslips/"+mine.String(), emp, nil)
	assertMatchesSpec(t, "payslip", slip, &gen.Payslip{})
	if r.StatusCode != http.StatusOK || len(slip["lines"].([]any)) == 0 || slip["totals"].(map[string]any)["net"] != slips[f.empSelf]["net"] ||
		slip["pdf_status"] != "RENDERED" || slip["employee"].(map[string]any)["employee_no"] != "EMP-0001" {
		t.Fatalf("S2 as owner: %d %v", r.StatusCode, slip)
	}
	if acc, ok := slip["bank_account_masked"].(string); ok && !maskedAccount.MatchString(acc) {
		t.Fatalf("bank account not masked: %q", acc)
	}
	r, dl := e.do(t, http.MethodGet, "/api/v1/payslips/"+mine.String()+"/download", emp, nil)
	assertMatchesSpec(t, "payslip download", dl, &gen.PayslipDownload{})
	u, _ := url.Parse(dl["url"].(string))
	exp, _ := time.Parse(time.RFC3339Nano, dl["expires_at"].(string))
	if r.StatusCode != http.StatusOK || u.Query().Get("expires") != "300" || !exp.Equal(e.clk.Now().Add(5*time.Minute)) || u.Path != "/"+key {
		t.Fatalf("S5 as owner: %d %v", r.StatusCode, dl)
	}
	if e.auditCount(t, f.id, "payslip.viewed_by_admin") != viewed || e.auditCount(t, f.id, "payslip.downloaded") != downloaded {
		t.Fatal("owner access must not be audited")
	}

	// payslip_download_requires_authz: bukan pemilik dan bukan admin → 404, tanpa URL.
	for name, c := range map[string]struct {
		token, path string
	}{
		"employee, another's slip via /me":   {emp, "/api/v1/me/payslips/" + other.String()},
		"employee, another's slip download":  {emp, "/api/v1/payslips/" + other.String() + "/download"},
		"employee, admin endpoint (own)":     {emp, "/api/v1/payslips/" + mine.String()},
		"manager of the owner, detail":       {m, "/api/v1/payslips/" + mine.String()},
		"manager of the owner, download":     {m, "/api/v1/payslips/" + mine.String() + "/download"},
		"admin via /me (not their own slip)": {o, "/api/v1/me/payslips/" + mine.String()},
	} {
		r, b := e.do(t, http.MethodGet, c.path, c.token, nil)
		if r.StatusCode != http.StatusNotFound || b["url"] != nil {
			t.Errorf("%s: want 404 without url, got %d %v", name, r.StatusCode, b)
		}
	}
	if r, _ := e.do(t, http.MethodGet, "/api/v1/payslips?run_id="+run.String(), emp, nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("S3 as employee: want 403, got %d", r.StatusCode)
	}

	// PI-17: setiap akses admin ter-audit (dua kali baca = dua baris), juga unduhan.
	for _, tok := range []string{h, h, o} {
		if r, b := e.do(t, http.MethodGet, "/api/v1/payslips/"+mine.String(), tok, nil); r.StatusCode != http.StatusOK {
			t.Fatalf("S4 as admin: %d %v", r.StatusCode, b)
		}
	}
	if r, b := e.do(t, http.MethodGet, "/api/v1/payslips/"+mine.String()+"/download", h, nil); r.StatusCode != http.StatusOK {
		t.Fatalf("S5 as HR: %d %v", r.StatusCode, b)
	}
	if got := e.auditCount(t, f.id, "payslip.viewed_by_admin"); got != viewed+3 {
		t.Fatalf("payslip.viewed_by_admin: want +3, got %d → %d", viewed, got)
	}
	if got := e.auditCount(t, f.id, "payslip.downloaded"); got != downloaded+1 {
		t.Fatalf("payslip.downloaded: want +1, got %d → %d", downloaded, got)
	}
}
