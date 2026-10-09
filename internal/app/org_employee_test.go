package app_test

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"payrun/internal/platform/ratelimit"
)

// relaxedTiers: semua tier dilonggarkan. Test S2 memanggil banyak endpoint per user; yang diuji di
// sini logika domain dan otorisasi, bukan rate limit (diuji terpisah).
func relaxedTiers() *ratelimit.Tiers {
	t := ratelimit.DefaultTiers()
	loose := []ratelimit.Rule{{Events: 100000, Per: time.Minute}}
	t.AuthStrict.Rules, t.AuthIP.Rules, t.Read.Rules, t.Write.Rules, t.HeavyCalculate.Rules = loose, loose, loose, loose, loose
	return &t
}

// ---------------------------------------------------------------- org (G1–G4)

func TestDepartments_CRUDAndConcurrency(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	tok := f.tokens[roleH]

	if r, b := e.do(t, http.MethodPost, "/api/v1/departments", tok, map[string]any{"code": "WH", "name": "Lain"}); r.StatusCode != http.StatusConflict || b["code"] != "DUPLICATE_CODE" {
		t.Fatalf("duplicate code: want 409 DUPLICATE_CODE, got %d %v", r.StatusCode, b)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/departments", tok, map[string]any{"code": "fin", "name": ""}); r.StatusCode != http.StatusBadRequest || len(b["errors"].([]any)) != 2 {
		t.Fatalf("validation: want 400 with 2 field errors, got %d %v", r.StatusCode, b)
	}
	path := "/api/v1/departments/" + f.dept.String()
	if r, b := e.do(t, http.MethodPatch, path, tok, map[string]any{"name": "Gudang Utama"}); r.StatusCode != http.StatusPreconditionRequired || b["code"] != "PRECONDITION_REQUIRED" {
		t.Fatalf("PATCH without If-Match: want 428, got %d %v", r.StatusCode, b)
	}
	r, b := e.doH(t, http.MethodPatch, path, tok, map[string]string{"If-Match": `"1"`}, map[string]any{"name": "Gudang Utama"})
	if r.StatusCode != http.StatusOK || b["name"] != "Gudang Utama" || r.Header.Get("ETag") != `"2"` {
		t.Fatalf("PATCH: %d %v %v", r.StatusCode, b, r.Header)
	}
	if r, b := e.doH(t, http.MethodPatch, path, tok, map[string]string{"If-Match": `"1"`}, map[string]any{"name": "Lagi"}); r.StatusCode != http.StatusPreconditionFailed || b["code"] != "VERSION_CONFLICT" {
		t.Fatalf("stale If-Match: want 412, got %d %v", r.StatusCode, b)
	}
	if r, b := e.doH(t, http.MethodPatch, path, tok, map[string]string{"If-Match": `"2"`}, map[string]any{"name": "nul\u0000byte"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("control character in name: want 400 (not 500), got %d %v", r.StatusCode, b)
	}
	if r, b := e.doH(t, http.MethodPatch, path, tok, map[string]string{"If-Match": `"2"`}, map[string]any{"name": "X", "code": "NEW"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("code is immutable (unknown field): want 400, got %d %v", r.StatusCode, b)
	}
	for range 2 { // deactivate natural idempotent
		if r, b := e.do(t, http.MethodPost, path+"/deactivate", tok, nil); r.StatusCode != http.StatusOK || b["status"] != "INACTIVE" {
			t.Fatalf("deactivate: %d %v", r.StatusCode, b)
		}
	}
	_, list := e.do(t, http.MethodGet, "/api/v1/departments", f.tokens[roleE], nil)
	if data := list["data"].([]any); len(data) != 0 {
		t.Fatalf("default list shows ACTIVE only: %v", list)
	}
	_, list = e.do(t, http.MethodGet, "/api/v1/departments?status=INACTIVE", f.tokens[roleE], nil)
	if data := list["data"].([]any); len(data) != 1 {
		t.Fatalf("INACTIVE filter: %v", list)
	}
	if r, _ := e.do(t, http.MethodGet, "/api/v1/departments?sort=name", tok, nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown filter must be 400, got %d", r.StatusCode)
	}
	if n := e.auditCount(t, f.id, "department.deactivated"); n != 1 {
		t.Fatalf("deactivate audited once (second call is a no-op), got %d", n)
	}
}

// ---------------------------------------------------------------- employees (W1–W5)

// W2 idempotency: retry dengan key + body sama → replay (tanpa employee kedua); body lain → 422;
// tanpa key → 400.
func TestEmployee_CreateIdempotency(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	key := map[string]string{"Idempotency-Key": newID().String()}
	body := employeeBody("EMP-0100", "Rudi Hartono", "3174012304950100")
	r1, b1 := e.doH(t, http.MethodPost, "/api/v1/employees", f.tokens[roleH], key, body)
	r2, b2 := e.doH(t, http.MethodPost, "/api/v1/employees", f.tokens[roleH], key, body)
	if r1.StatusCode != http.StatusCreated || r2.StatusCode != http.StatusCreated || b1["id"] != b2["id"] || r2.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v / %d %v %v", r1.StatusCode, b1, r2.StatusCode, b2, r2.Header)
	}
	body["full_name"] = "Orang Lain"
	if r, b := e.doH(t, http.MethodPost, "/api/v1/employees", f.tokens[roleH], key, body); r.StatusCode != http.StatusUnprocessableEntity || b["code"] != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("key reuse: want 422, got %d %v", r.StatusCode, b)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/employees", f.tokens[roleH], body); r.StatusCode != http.StatusBadRequest || b["code"] != "IDEMPOTENCY_KEY_REQUIRED" {
		t.Fatalf("missing key: want 400, got %d %v", r.StatusCode, b)
	}
	// Key yang sama dipakai user lain di tenant yang sama = key berbeda (scope per user).
	if r, b := e.doH(t, http.MethodPost, "/api/v1/employees", f.tokens[roleO], key, employeeBody("EMP-0101", "Sari", "3174012304950101")); r.StatusCode != http.StatusCreated || r.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("same key, other user: %d %v", r.StatusCode, b)
	}
}

// PF-20: respons hanya memuat PII ter-mask; di DB hanya ciphertext (plaintext tidak muncul di baris
// mentah mana pun), dan payrun_readonly tidak bisa membaca kolom ciphertext.
func TestEmployee_PIIMaskedAndEncryptedAtRest(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	_, b := e.do(t, http.MethodGet, "/api/v1/employees/"+f.empSelf.String(), f.tokens[roleP], nil)
	bank, _ := b["bank"].(map[string]any)
	if b["nik_masked"] != "************0001" || b["npwp_masked"] != "***********0001" || bank["account_masked"] != "******7890" {
		t.Fatalf("masked PII: %v", b)
	}
	for _, k := range []string{"nik", "npwp", "nik_enc", "account_number"} {
		if _, ok := b[k]; ok {
			t.Fatalf("response must not carry %q: %v", k, b)
		}
	}
	var raw []byte
	if err := e.tdb.Super(t).QueryRow(context.Background(),
		`SELECT convert_to(row_to_json(e)::text, 'UTF8') FROM payrun.employees e WHERE id = $1`, f.empSelf).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"3174012304950001", "174012304950001", "1234567890"} {
		if bytes.Contains(raw, []byte(plain)) {
			t.Fatalf("plaintext %s stored in the employees row", plain)
		}
	}
	if _, err := e.tdb.As(t, "payrun_readonly").Exec(context.Background(), `SELECT nik_enc FROM payrun.employees`); err == nil {
		t.Fatal("payrun_readonly must not read ciphertext columns")
	}
}

// TestEmployee_NikUniquePerTenant (DoD M5, PI-18): NIK unik per tenant lewat blind index; NIK yang
// sama di tenant lain sah. Pesan tidak menyebut pemiliknya. Begitu juga employee_no.
func TestEmployee_NikUniquePerTenant(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	alpha := e.newTenantFx(t, "alpha")
	beta := e.newTenantFx(t, "beta")
	// Fixture: empSelf Alpha dan Beta sudah memakai NIK yang SAMA (…0001) — sah karena beda tenant.
	e.createEmployee(t, alpha.tokens[roleH], "EMP-0200", "Asli", "3174012304950777")
	dup := employeeBody("EMP-0201", "Kembar", "3174 0123 0495 0777") // NIK sama, format berbeda
	r, b := e.doH(t, http.MethodPost, "/api/v1/employees", alpha.tokens[roleH], map[string]string{"Idempotency-Key": newID().String()}, dup)
	if r.StatusCode != http.StatusConflict || b["code"] != "NIK_TAKEN" || b["detail"] != nil {
		t.Fatalf("duplicate NIK in Alpha: want 409 NIK_TAKEN without detail, got %d %v", r.StatusCode, b)
	}
	if r, b := e.doH(t, http.MethodPost, "/api/v1/employees", beta.tokens[roleH], map[string]string{"Idempotency-Key": newID().String()}, dup); r.StatusCode != http.StatusCreated {
		t.Fatalf("same NIK in another tenant is fine: %d %v", r.StatusCode, b)
	}
	sameNo := employeeBody("EMP-0001", "Nomor Kembar", "3174012304950999")
	if r, b := e.doH(t, http.MethodPost, "/api/v1/employees", alpha.tokens[roleH], map[string]string{"Idempotency-Key": newID().String()}, sameNo); r.StatusCode != http.StatusConflict || b["code"] != "EMPLOYEE_NO_TAKEN" {
		t.Fatalf("duplicate employee_no: %d %v", r.StatusCode, b)
	}
	// PATCH ke NIK milik employee lain → NIK_TAKEN juga.
	r, b = e.doH(t, http.MethodPatch, "/api/v1/employees/"+alpha.empOther.String(), alpha.tokens[roleH], map[string]string{"If-Match": `"1"`}, map[string]any{"nik": "3174012304950001"})
	if r.StatusCode != http.StatusConflict || b["code"] != "NIK_TAKEN" {
		t.Fatalf("PATCH to a taken NIK: %d %v", r.StatusCode, b)
	}
}

// Scope baca: EMPLOYEE melihat dirinya sendiri (versi dasar tanpa PII/PTKP), karyawan lain 404, dan
// daftar 403. MANAGER tanpa bawahan (S2) melihat daftar kosong.
func TestEmployee_ReadScopes(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	r, self := e.do(t, http.MethodGet, "/api/v1/employees/"+f.empSelf.String(), f.tokens[roleE], nil)
	if r.StatusCode != http.StatusOK || self["employee_no"] != "EMP-0001" {
		t.Fatalf("self: %d %v", r.StatusCode, self)
	}
	for _, k := range []string{"nik_masked", "npwp_masked", "bank", "ptkp_status"} {
		if _, ok := self[k]; ok {
			t.Fatalf("basic view must not carry %q: %v", k, self)
		}
	}
	if r, _ := e.do(t, http.MethodGet, "/api/v1/employees/"+f.empOther.String(), f.tokens[roleE], nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("other employee for EMPLOYEE: want 404, got %d", r.StatusCode)
	}
	if r, _ := e.do(t, http.MethodGet, "/api/v1/employees", f.tokens[roleE], nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("list for EMPLOYEE: want 403, got %d", r.StatusCode)
	}
	// MANAGER: hanya bawahan langsung saat ini (empSelf), bukan empOther atau dirinya sendiri.
	r, b := e.do(t, http.MethodGet, "/api/v1/employees", f.tokens[roleM], nil)
	data, _ := b["data"].([]any)
	if r.StatusCode != http.StatusOK || len(data) != 1 || data[0].(map[string]any)["id"] != f.empSelf.String() {
		t.Fatalf("MANAGER sees direct reports only: got %d %v", r.StatusCode, b)
	}
}

// W1: keyset pagination (employee_no, id), pencarian prefix nama/nomor, filter status.
func TestEmployee_ListPaginationAndSearch(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	for i := 3; i <= 7; i++ {
		no := "EMP-000" + strconv.Itoa(i)
		e.createEmployee(t, f.tokens[roleO], no, "Budi "+strconv.Itoa(i), "31740123049500"+strconv.Itoa(10+i))
	}
	var seen []string
	cursor := ""
	for page := 0; page < 5; page++ {
		path := "/api/v1/employees?limit=3"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		_, b := e.do(t, http.MethodGet, path, f.tokens[roleP], nil)
		for _, it := range b["data"].([]any) {
			seen = append(seen, it.(map[string]any)["employee_no"].(string))
		}
		pg := b["page"].(map[string]any)
		if pg["has_more"] != true {
			break
		}
		cursor = pg["next_cursor"].(string)
	}
	if len(seen) != 8 || seen[0] != "EMP-0001" || seen[6] != "EMP-0007" || seen[7] != "EMP-0090" {
		t.Fatalf("pagination must return all 8 in employee_no order exactly once: %v", seen)
	}
	_, b := e.do(t, http.MethodGet, "/api/v1/employees?q=bud", f.tokens[roleP], nil)
	if data := b["data"].([]any); len(data) != 5 || data[0].(map[string]any)["full_name"] != "Budi 3" {
		t.Fatalf("prefix search: %v", b)
	}
	if _, b := e.do(t, http.MethodGet, "/api/v1/employees?q=b%25", f.tokens[roleP], nil); len(b["data"].([]any)) != 0 {
		t.Fatalf("LIKE wildcards in q must be literal: %v", b)
	}
	if r, _ := e.do(t, http.MethodGet, "/api/v1/employees?cursor=garbage", f.tokens[roleP], nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad cursor: want 400, got %d", r.StatusCode)
	}
}

// W5: terminate menutup span (end_date inklusif), status berubah, terminate kedua 409; If-Match basi 412.
func TestEmployee_Terminate(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	path := "/api/v1/employees/" + f.empOther.String() + "/terminate"
	body := map[string]any{"type": "RESIGNED", "end_date": "2026-03-20", "reason": "Pindah kota"}
	if r, b := e.do(t, http.MethodPost, path, f.tokens[roleH], body); r.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("terminate without If-Match: want 428, got %d %v", r.StatusCode, b)
	}
	if r, b := e.doH(t, http.MethodPost, path, f.tokens[roleH], map[string]string{"If-Match": `"1"`},
		map[string]any{"type": "RESIGNED", "end_date": "2025-12-31"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("end_date before hire_date: want 400, got %d %v", r.StatusCode, b)
	}
	r, b := e.doH(t, http.MethodPost, path, f.tokens[roleH], map[string]string{"If-Match": `"1"`}, body)
	if r.StatusCode != http.StatusOK || b["status"] != "RESIGNED" || b["end_date"] != "2026-03-20" {
		t.Fatalf("terminate: %d %v", r.StatusCode, b)
	}
	var upper time.Time
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT upper(span) FROM employment_spans WHERE employee_id = $1`, f.empOther).Scan(&upper)
	})
	if upper.Format("2006-01-02") != "2026-03-21" {
		t.Fatalf("span upper bound must be end_date + 1 (exclusive), got %s", upper)
	}
	if r, b := e.doH(t, http.MethodPost, path, f.tokens[roleH], map[string]string{"If-Match": `"2"`}, body); r.StatusCode != http.StatusConflict || b["code"] != "INVALID_STATE_TRANSITION" {
		t.Fatalf("second terminate: want 409, got %d %v", r.StatusCode, b)
	}
	if r, b := e.doH(t, http.MethodPatch, "/api/v1/employees/"+f.empSelf.String(), f.tokens[roleH], map[string]string{"If-Match": `"1"`}, map[string]any{}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty PATCH: want 400 without bumping the version, got %d %v", r.StatusCode, b)
	}
	if n := e.auditCount(t, f.id, "employee.terminated"); n != 1 {
		t.Fatalf("terminate audit: %d", n)
	}
}

// ---------------------------------------------------------------- invitations (U1–U3)

func inviteToken(t *testing.T, b map[string]any) string {
	t.Helper()
	u, _ := b["invite_url"].(string)
	_, tok, ok := bytes.Cut([]byte(u), []byte("/invite#"))
	if !ok {
		t.Fatalf("invite_url without token: %v", b)
	}
	return string(tok)
}

// Alur lengkap: HR mengundang (role EMPLOYEE + employee_id) → user terverifikasi dengan email sama
// menerima → membership ACTIVE, employee tertaut, user bisa switch ke tenant dan melihat dirinya.
func TestInvitation_InviteAcceptAndLinkEmployee(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	emp := e.createEmployee(t, f.tokens[roleO], "EMP-0300", "Tono Baru", "3174012304950300")
	r, inv := e.do(t, http.MethodPost, "/api/v1/invitations", f.tokens[roleH], map[string]any{"email": "Tono@Alpha.test", "roles": []string{"EMPLOYEE"}, "employee_id": emp})
	if r.StatusCode != http.StatusCreated || inv["status"] != "INVITED" || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("invite: %d %v", r.StatusCode, inv)
	}
	oldToken := inviteToken(t, inv)
	// Re-invite (review S2): link lama hilang/kedaluwarsa → undangan baru untuk email yang sama
	// menggantikan link lama pada membership yang sama, bukan 409.
	r, inv2 := e.do(t, http.MethodPost, "/api/v1/invitations", f.tokens[roleH], map[string]any{"email": "tono@alpha.test", "roles": []string{"EMPLOYEE"}, "employee_id": emp})
	if r.StatusCode != http.StatusCreated || inv2["membership_id"] != inv["membership_id"] {
		t.Fatalf("re-invite: want 201 on the same membership, got %d %v", r.StatusCode, inv2)
	}
	token := inviteToken(t, inv2)
	if e.auditCount(t, f.id, "membership.reinvited") != 1 {
		t.Fatal("re-invite must be audited")
	}
	// Employee yang sama tidak boleh dicadangkan undangan lain (email lain) yang masih berlaku.
	if r, b := e.do(t, http.MethodPost, "/api/v1/invitations", f.tokens[roleH], map[string]any{"email": "lain@alpha.test", "roles": []string{"EMPLOYEE"}, "employee_id": emp}); r.StatusCode != http.StatusConflict || b["code"] != "INVALID_STATE_TRANSITION" {
		t.Fatalf("employee reserved by another pending invitation: want 409, got %d %v", r.StatusCode, b)
	}

	tono := e.createUser(t, "tono@alpha.test")
	tonoTok, _, _ := e.login(t, "tono@alpha.test")
	if r, b := e.do(t, http.MethodPost, "/api/v1/invitations/accept", tonoTok, map[string]string{"token": token}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("unverified email: want 403, got %d %v", r.StatusCode, b)
	}
	e.verifyUser(t, tono)

	other := e.createUser(t, "other@alpha.test")
	e.verifyUser(t, other)
	otherTok, _, _ := e.login(t, "other@alpha.test")
	if r, b := e.do(t, http.MethodPost, "/api/v1/invitations/accept", otherTok, map[string]string{"token": token}); r.StatusCode != http.StatusGone || b["code"] != "INVITATION_INVALID" {
		t.Fatalf("email mismatch: want 410, got %d %v", r.StatusCode, b)
	}

	if r, _ := e.do(t, http.MethodPost, "/api/v1/invitations/accept", tonoTok, map[string]string{"token": oldToken}); r.StatusCode != http.StatusGone {
		t.Fatalf("link replaced by a re-invite: want 410, got %d", r.StatusCode)
	}
	r, acc := e.do(t, http.MethodPost, "/api/v1/invitations/accept", tonoTok, map[string]string{"token": token})
	m, _ := acc["membership"].(map[string]any)
	if r.StatusCode != http.StatusOK || m["status"] != "ACTIVE" || m["tenant_id"] != f.id.String() {
		t.Fatalf("accept: %d %v", r.StatusCode, acc)
	}
	if r, _ := e.do(t, http.MethodPost, "/api/v1/invitations/accept", tonoTok, map[string]string{"token": token}); r.StatusCode != http.StatusGone {
		t.Fatalf("second accept: want 410, got %d", r.StatusCode)
	}
	_, sw := e.do(t, http.MethodPost, "/api/v1/session/switch-tenant", tonoTok, map[string]string{"tenant_id": f.id.String()})
	r, self := e.do(t, http.MethodGet, "/api/v1/employees/"+emp.String(), sw["access_token"].(string), nil)
	if r.StatusCode != http.StatusOK || self["user_id"] != tono.String() {
		t.Fatalf("invited user sees the linked employee: %d %v", r.StatusCode, self)
	}
	_, members := e.do(t, http.MethodGet, "/api/v1/members?status=ACTIVE", f.tokens[roleO], nil)
	if data := members["data"].([]any); len(data) != 6 {
		t.Fatalf("5 fixture members + Tono: %v", members)
	}
	if e.auditCount(t, f.id, "membership.invited") != 1 || e.auditCount(t, f.id, "membership.accepted") != 1 {
		t.Fatal("invite and accept must be audited")
	}
}

// HR_ADMIN hanya boleh mengundang MANAGER/EMPLOYEE (file 17 ᴿ); token rusak/tenant salah → 410.
func TestInvitation_RulesAndBadTokens(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	beta := e.newTenantFx(t, "beta")
	for _, roles := range [][]string{{"HR_ADMIN"}, {"OWNER"}, {"EMPLOYEE", "PAYROLL_ADMIN"}} {
		if r, b := e.do(t, http.MethodPost, "/api/v1/invitations", f.tokens[roleH], map[string]any{"email": "x@alpha.test", "roles": roles}); r.StatusCode != http.StatusForbidden {
			t.Fatalf("HR inviting %v: want 403, got %d %v", roles, r.StatusCode, b)
		}
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/invitations", f.tokens[roleO], map[string]any{"email": f.emails[roleP], "roles": []string{"EMPLOYEE"}}); r.StatusCode != http.StatusConflict || b["code"] != "MEMBERSHIP_EXISTS" {
		t.Fatalf("inviting an active member: want 409, got %d %v", r.StatusCode, b)
	}
	// cross_tenant_fk_rejected: employee_id milik Beta → 404 (composite FK + RLS), tidak membedakan.
	if r, b := e.do(t, http.MethodPost, "/api/v1/invitations", f.tokens[roleO], map[string]any{"email": "y@alpha.test", "roles": []string{"EMPLOYEE"}, "employee_id": beta.empOther}); r.StatusCode != http.StatusNotFound {
		t.Fatalf("employee of another tenant: want 404, got %d %v", r.StatusCode, b)
	}
	_, inv := e.do(t, http.MethodPost, "/api/v1/invitations", f.tokens[roleO], map[string]any{"email": "z@alpha.test", "roles": []string{"MANAGER"}})
	token := inviteToken(t, inv)
	z := e.createUser(t, "z@alpha.test")
	e.verifyUser(t, z)
	zTok, _, _ := e.login(t, "z@alpha.test")
	_, secret, _ := bytes.Cut([]byte(token), []byte("."))
	for name, bad := range map[string]string{
		"secret under another tenant": beta.id.String() + "." + string(secret),
		"tampered secret":             f.id.String() + "." + string(secret[:42]) + "A",
		"garbage":                     "not-a-token",
	} {
		if r, b := e.do(t, http.MethodPost, "/api/v1/invitations/accept", zTok, map[string]string{"token": bad}); r.StatusCode != http.StatusGone || b["code"] != "INVITATION_INVALID" {
			t.Fatalf("%s: want 410 INVITATION_INVALID, got %d %v", name, r.StatusCode, b)
		}
	}
	// HR_ADMIN tidak boleh menimpa undangan ber-role admin buatan OWNER.
	e.do(t, http.MethodPost, "/api/v1/invitations", f.tokens[roleO], map[string]any{"email": "admin2@alpha.test", "roles": []string{"PAYROLL_ADMIN"}})
	if r, b := e.do(t, http.MethodPost, "/api/v1/invitations", f.tokens[roleH], map[string]any{"email": "admin2@alpha.test", "roles": []string{"EMPLOYEE"}}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("HR replacing an admin invitation: want 403, got %d %v", r.StatusCode, b)
	}
	// Tenant yang ditangguhkan: undangan tidak bisa dipakai.
	if _, err := e.tdb.Super(t).Exec(context.Background(), `UPDATE payrun.tenants SET status = 'SUSPENDED', suspended_at = now() WHERE id = $1`, f.id); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.do(t, http.MethodPost, "/api/v1/invitations/accept", zTok, map[string]string{"token": token}); r.StatusCode != http.StatusGone {
		t.Fatalf("suspended tenant: want 410, got %d", r.StatusCode)
	}
	if _, err := e.tdb.Super(t).Exec(context.Background(), `UPDATE payrun.tenants SET status = 'ACTIVE', suspended_at = NULL WHERE id = $1`, f.id); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(7*24*time.Hour + time.Second)
	zTok, _, _ = e.login(t, "z@alpha.test") // access token lama ikut kedaluwarsa
	if r, _ := e.do(t, http.MethodPost, "/api/v1/invitations/accept", zTok, map[string]string{"token": token}); r.StatusCode != http.StatusGone {
		t.Fatalf("expired invitation: want 410, got %d", r.StatusCode)
	}
}
