package app_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"payrun/api/gen"
)

// assertMatchesSpec: respons nyata cocok dengan tipe hasil generate dari api/openapi.yaml (ADR-003,
// spec-first). Dua arah:
//   - field yang dikirim server tetapi TIDAK ada di spec → gagal (decode DisallowUnknownFields);
//   - field wajib di spec (tag json tanpa omitempty) yang TIDAK dikirim server → gagal.
//
// Bukan validator JSON Schema penuh (pattern/enum tidak dicek), tetapi menangkap drift yang paling
// sering: field diganti nama, ditambah, atau dihapus di salah satu sisi saja.
func assertMatchesSpec(t *testing.T, name string, body map[string]any, into any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Errorf("%s: response does not match the OpenAPI schema: %v\n%s", name, err, raw)
		return
	}
	rt := reflect.TypeOf(into).Elem()
	for i := range rt.NumField() {
		tag := rt.Field(i).Tag.Get("json")
		key, opts, _ := strings.Cut(tag, ",")
		if key == "" || key == "-" || strings.Contains(opts, "omitempty") {
			continue
		}
		if _, ok := body[key]; !ok {
			t.Errorf("%s: required field %q (spec) missing from the response", name, key)
		}
	}
}

// TestContract_S1ResponsesMatchSpec: semua respons sukses endpoint Sprint 1 terhadap skemanya.
func TestContract_S1ResponsesMatchSpec(t *testing.T) {
	e := newEnv(t)
	_, signup := e.signup(t, newID().String(), signupBody("alpha", "dewi@alpha.test"))
	assertMatchesSpec(t, "signup", signup, &gen.SignupResult{})
	_, replay := e.signup(t, newID().String(), signupBody("beta", "budi@beta.test"))
	assertMatchesSpec(t, "signup (other tenant)", replay, &gen.SignupResult{})

	e.do(t, http.MethodPost, "/api/v1/auth/verify-email", "", map[string]string{"token": e.mailer.lastToken(t, "dewi@alpha.test"), "password": password})
	resp, login := e.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": "dewi@alpha.test", "password": password})
	assertMatchesSpec(t, "login", login, &gen.Session{})
	token := login["access_token"].(string)

	_, me := e.do(t, http.MethodGet, "/api/v1/me", token, nil)
	assertMatchesSpec(t, "me", me, &gen.Me{})
	_, ms := e.do(t, http.MethodGet, "/api/v1/me/memberships", token, nil)
	assertMatchesSpec(t, "memberships", ms, &gen.MembershipList{})
	_, tn := e.do(t, http.MethodGet, "/api/v1/tenant", token, nil)
	assertMatchesSpec(t, "tenant", tn, &gen.Tenant{})
	_, sw := e.do(t, http.MethodPost, "/api/v1/session/switch-tenant", token, map[string]string{"tenant_id": activeTenantID(login)})
	assertMatchesSpec(t, "switch-tenant", sw, &gen.Session{})
	_, rf := e.cookiePost(t, "/api/v1/auth/refresh", cookieValue(resp), map[string]string{"X-Active-Tenant": activeTenantID(login)})
	assertMatchesSpec(t, "refresh", rf, &gen.Session{})

	// Error memakai Problem (RFC 9457).
	_, prob := e.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"email": "x@y.test", "password": "wrong password"})
	assertMatchesSpec(t, "problem", prob, &gen.Problem{})
}

// TestContract_S2ResponsesMatchSpec: respons sukses endpoint Sprint 2 terhadap skemanya, termasuk
// versi dasar employee (MANAGER/EMPLOYEE) dan daftar.
func TestContract_S2ResponsesMatchSpec(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o := f.tokens[roleO]
	_, depts := e.do(t, http.MethodGet, "/api/v1/departments", o, nil)
	assertMatchesSpec(t, "departments", depts, &gen.OrgUnitList{})
	_, dept := e.do(t, http.MethodPost, "/api/v1/positions", o, map[string]any{"code": "MGR", "name": "Manajer"})
	assertMatchesSpec(t, "position", dept, &gen.OrgUnit{})
	_, full := e.do(t, http.MethodGet, "/api/v1/employees/"+f.empSelf.String(), o, nil)
	assertMatchesSpec(t, "employee (full)", full, &gen.Employee{})
	_, basic := e.do(t, http.MethodGet, "/api/v1/employees/"+f.empSelf.String(), f.tokens[roleE], nil)
	assertMatchesSpec(t, "employee (basic)", basic, &gen.Employee{})
	_, list := e.do(t, http.MethodGet, "/api/v1/employees?limit=1", o, nil)
	assertMatchesSpec(t, "employees", list, &gen.EmployeeList{})
	_, inv := e.do(t, http.MethodPost, "/api/v1/invitations", o, map[string]any{"email": "new@alpha.test", "roles": []string{"MANAGER"}})
	assertMatchesSpec(t, "invitation", inv, &gen.Invitation{})
	_, members := e.do(t, http.MethodGet, "/api/v1/members", o, nil)
	assertMatchesSpec(t, "members", members, &gen.MemberList{})
	u := e.createUser(t, "new@alpha.test")
	e.verifyUser(t, u)
	tok, _, _ := e.login(t, "new@alpha.test")
	_, acc := e.do(t, http.MethodPost, "/api/v1/invitations/accept", tok, map[string]string{"token": inviteToken(t, inv)})
	assertMatchesSpec(t, "accepted invitation", acc, &gen.AcceptedInvitation{})
	_, me := e.do(t, http.MethodGet, "/api/v1/me", f.tokens[roleE], nil)
	if me["employee_id"] != f.empSelf.String() {
		t.Errorf("/me employee_id must be the linked employee in the active tenant: %v", me)
	}
}

// TestContract_S3ResponsesMatchSpec: respons sukses endpoint Sprint 3 terhadap skemanya.
func TestContract_S3ResponsesMatchSpec(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o := f.tokens[roleO]
	_, hols := e.do(t, http.MethodGet, "/api/v1/holidays?year=2026", o, nil)
	assertMatchesSpec(t, "holidays", hols, &gen.HolidayList{})
	_, hol := e.do(t, http.MethodPost, "/api/v1/holidays", o, map[string]any{"date": "2026-05-01", "name": "Hari buruh"})
	assertMatchesSpec(t, "holiday", hol, &gen.Holiday{})
	_, defs := e.do(t, http.MethodGet, "/api/v1/component-definitions", o, nil)
	assertMatchesSpec(t, "component definitions", defs, &gen.ComponentDefinitionList{})
	_, def := e.do(t, http.MethodPost, "/api/v1/component-definitions", o, map[string]any{"code": "TRANSPORT", "name": "Uang transport",
		"category": "EARNING", "calc_type": "PER_DAY", "default_amount": "20000.00", "day_source": "PRESENT"})
	assertMatchesSpec(t, "component definition", def, &gen.ComponentDefinition{})
	emp := f.empOther.String()
	_, oa := e.doH(t, http.MethodPost, "/api/v1/employees/"+emp+"/org-assignments", o, key(),
		map[string]any{"valid_from": "2026-01-05", "department_id": f.dept, "position_id": f.pos, "manager_employee_id": f.empManager})
	assertMatchesSpec(t, "org assignment", oa, &gen.OrgAssignment{})
	_, sal := e.doH(t, http.MethodPost, "/api/v1/employees/"+emp+"/salary-changes", o, key(), map[string]any{"base_salary": "6000000.00", "valid_from": "2026-01-05"})
	assertMatchesSpec(t, "salary assignment", sal, &gen.SalaryAssignment{})
	_, ec := e.doH(t, http.MethodPost, "/api/v1/employees/"+emp+"/component-assignments", o, key(), map[string]any{"component_id": f.comp, "valid_from": "2026-01-05"})
	assertMatchesSpec(t, "employee component", ec, &gen.EmployeeComponent{})
	_, hist := e.do(t, http.MethodGet, "/api/v1/employees/"+emp+"/salary-history", o, nil)
	assertMatchesSpec(t, "salary history", hist, &gen.SalaryHistory{})
	_, full := e.do(t, http.MethodGet, "/api/v1/employees/"+emp, o, nil)
	assertMatchesSpec(t, "employee with current_org", full, &gen.Employee{})
	_, managerOnly := e.do(t, http.MethodGet, "/api/v1/employees/"+f.empSelf.String(), o, nil)
	assertMatchesSpec(t, "employee with manager only", managerOnly, &gen.Employee{})
	_, list := e.do(t, http.MethodGet, "/api/v1/employees", o, nil)
	assertMatchesSpec(t, "employee list with org", list, &gen.EmployeeList{})
}

// TestContract_S4ResponsesMatchSpec: respons sukses endpoint payroll run terhadap skemanya, dari run
// DRAFT (semua nilai hasil null) sampai CALCULATED, item OK dengan lines dan item ERROR dengan issues.
func TestContract_S4ResponsesMatchSpec(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	p := f.tokens[roleP]
	e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/salary-changes", f.tokens[roleO], key(),
		map[string]any{"base_salary": "6000000.00", "valid_from": "2026-01-05"})

	_, draft := e.doH(t, http.MethodPost, "/api/v1/payroll-runs", p, key(), map[string]any{"period": "2026-03"})
	assertMatchesSpec(t, "run (draft)", draft, &gen.PayrollRun{})
	run := draft["id"].(string)
	// Y5 tanpa body sama sekali (16-api: "tanpa body").
	r, calc := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run+"/calculate", p, key(), nil)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("calculate without body: %d %v", r.StatusCode, calc)
	}
	assertMatchesSpec(t, "run (calculating)", calc, &gen.PayrollRun{})
	e.drainJobs(t)
	_, done := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+run, p, nil)
	assertMatchesSpec(t, "run (calculated)", done, &gen.PayrollRun{})
	_, runs := e.do(t, http.MethodGet, "/api/v1/payroll-runs?period=2026-03", p, nil)
	assertMatchesSpec(t, "runs", runs, &gen.PayrollRunList{})

	_, items := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+run+"/items", p, nil)
	assertMatchesSpec(t, "items", items, &gen.PayrollItemList{})
	var okItem, errItem string
	for _, it := range items["data"].([]any) {
		m := it.(map[string]any)
		switch m["status"] {
		case "OK":
			okItem = m["id"].(string)
		case "ERROR":
			errItem = m["id"].(string)
		}
	}
	_, ok := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+run+"/items/"+okItem, p, nil)
	assertMatchesSpec(t, "item (ok, lines)", ok, &gen.PayrollItemDetail{})
	if lines, _ := ok["lines"].([]any); len(lines) == 0 {
		t.Errorf("OK item must have lines: %v", ok)
	}
	_, bad := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+run+"/items/"+errItem, p, nil)
	assertMatchesSpec(t, "item (error, issues)", bad, &gen.PayrollItemDetail{})
	_, excl := e.do(t, http.MethodPost, "/api/v1/payroll-runs/"+run+"/items/"+errItem+"/exclude", p, map[string]any{"reason": "Gaji belum diinput HR"})
	assertMatchesSpec(t, "item (excluded)", excl, &gen.PayrollItemDetail{})
	_, cancelled := e.do(t, http.MethodPost, "/api/v1/payroll-runs/"+run+"/cancel", p, map[string]any{"reason": "Salah periode"})
	assertMatchesSpec(t, "run (cancelled)", cancelled, &gen.PayrollRun{})

	// Request body: field di luar skema ditolak (additionalProperties: false).
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs", p, key(), map[string]any{"period": "2026-04", "extra": 1}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown field in CreatePayrollRunRequest: want 400, got %d %v", r.StatusCode, b)
	}
}
