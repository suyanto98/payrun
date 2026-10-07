package app_test

import (
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func key() map[string]string { return map[string]string{"Idempotency-Key": newID().String()} }

// W2 dengan org + salary, lalu W9 kenaikan gaji: record lama ditutup tepat di valid_from (half-open),
// riwayat dua baris, detail menampilkan penempatan dan atasan saat ini.
func TestSalary_EffectiveDatedChange(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	body := employeeBody("EMP-0700", "Tono Gaji", "3174012304950700")
	body["org"] = map[string]any{"department_id": f.dept, "position_id": f.pos, "manager_employee_id": f.empManager}
	body["salary"] = map[string]any{"base_salary": "6000000.00"}
	r, emp := e.doH(t, http.MethodPost, "/api/v1/employees", f.tokens[roleH], key(), body)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("create with org + salary: %d %v", r.StatusCode, emp)
	}
	org, _ := emp["current_org"].(map[string]any)
	if dep, _ := org["department"].(map[string]any); dep["name"] != "Gudang" || org["valid_from"] != "2026-01-05" {
		t.Fatalf("current_org: %v", emp["current_org"])
	}
	if mgr, _ := org["manager"].(map[string]any); mgr["id"] != f.empManager.String() {
		t.Fatalf("manager: %v", org)
	}
	id := emp["id"].(string)
	path := "/api/v1/employees/" + id + "/salary-changes"

	raise := map[string]any{"base_salary": "6600000.00", "valid_from": "2026-03-15", "change_reason": "Kenaikan tahunan"}
	k := key()
	r1, s1 := e.doH(t, http.MethodPost, path, f.tokens[roleH], k, raise)
	r2, s2 := e.doH(t, http.MethodPost, path, f.tokens[roleH], k, raise)
	if r1.StatusCode != http.StatusCreated || r2.Header.Get("Idempotent-Replayed") != "true" || s1["id"] != s2["id"] {
		t.Fatalf("salary change + replay: %d %v / %v", r1.StatusCode, s1, r2.Header)
	}
	_, hist := e.do(t, http.MethodGet, "/api/v1/employees/"+id+"/salary-history", f.tokens[roleP], nil)
	sal := hist["salary"].([]any)
	first, second := sal[0].(map[string]any), sal[1].(map[string]any)
	if len(sal) != 2 || first["base_salary"] != "6000000.00" || first["valid_to"] != "2026-03-15" ||
		second["base_salary"] != "6600000.00" || second["valid_from"] != "2026-03-15" || second["valid_to"] != nil {
		t.Fatalf("history must be [6.0M, 15 Mar) + [6.6M, ∞): %v", sal)
	}
	_, asOf := e.do(t, http.MethodGet, "/api/v1/employees/"+id+"/salary-history?as_of=2026-03-14", f.tokens[roleP], nil)
	if s := asOf["salary"].([]any); len(s) != 1 || s[0].(map[string]any)["base_salary"] != "6000000.00" {
		t.Fatalf("as_of the day before the raise: %v", s)
	}
	// Perubahan SEBELUM perubahan masa depan yang sudah ada → EXCLUDE → 409.
	if r, b := e.doH(t, http.MethodPost, path, f.tokens[roleH], key(), map[string]any{"base_salary": "6300000.00", "valid_from": "2026-02-01"}); r.StatusCode != http.StatusConflict || b["code"] != "OVERLAPPING_PERIOD" {
		t.Fatalf("change before an existing future record: want 409 OVERLAPPING_PERIOD, got %d %v", r.StatusCode, b)
	}
	if r, b := e.doH(t, http.MethodPost, path, f.tokens[roleH], key(), map[string]any{"base_salary": "6300000.00", "valid_from": "2025-12-31"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("valid_from before hire_date: want 400, got %d %v", r.StatusCode, b)
	}
	if r, b := e.doH(t, http.MethodPost, path, f.tokens[roleH], key(), map[string]any{"base_salary": "6.300.000", "valid_from": "2026-05-01"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("money must be a decimal string: want 400, got %d %v", r.StatusCode, b)
	}
	if e.auditCount(t, f.id, "salary.changed") != 1 || e.auditCount(t, f.id, "salary.created") != 1 {
		t.Fatal("salary create + change must be audited once each (replay is not)")
	}
	// List: departemen/posisi saat ini + filter department_id.
	_, list := e.do(t, http.MethodGet, "/api/v1/employees?department_id="+f.dept.String(), f.tokens[roleP], nil)
	data := list["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["position"].(map[string]any)["name"] != "Staf" {
		t.Fatalf("department filter: %v", list)
	}
}

// Dua HR mengubah gaji pada tanggal yang sama secara paralel: tepat satu menang, yang lain 409
// OVERLAPPING_PERIOD (EXCLUDE), tidak pernah dua record beririsan (PI-09).
func TestSalary_ConcurrentChangesOneWins(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	path := "/api/v1/employees/" + f.empOther.String() + "/salary-changes"
	e.doH(t, http.MethodPost, path, f.tokens[roleO], key(), map[string]any{"base_salary": "5000000.00", "valid_from": "2026-01-05"})
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, tok := range []string{f.tokens[roleO], f.tokens[roleH]} {
		wg.Add(1)
		go func(i int, tok string) {
			defer wg.Done()
			r, _ := e.doH(t, http.MethodPost, path, tok, key(), map[string]any{"base_salary": "5500000.00", "valid_from": "2026-04-01"})
			codes[i] = r.StatusCode
		}(i, tok)
	}
	wg.Wait()
	created, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		}
	}
	if created != 1 || conflict != 1 {
		t.Fatalf("parallel changes on the same date: want one 201 and one 409, got %v", codes)
	}
	_, hist := e.do(t, http.MethodGet, "/api/v1/employees/"+f.empOther.String()+"/salary-history", f.tokens[roleO], nil)
	if n := len(hist["salary"].([]any)); n != 2 {
		t.Fatalf("history must have exactly 2 records, got %d", n)
	}
}

// C1–C4: komponen sistem hasil seed signup, validasi graf (referensi tidak ada, siklus, nonaktifkan
// komponen yang masih direferensikan), dan perlindungan komponen sistem.
func TestComponents_GraphAndSystemRules(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	tok := f.tokens[roleH]
	_, list := e.do(t, http.MethodGet, "/api/v1/component-definitions", tok, nil)
	system := 0
	var pph map[string]any
	for _, d := range list["data"].([]any) {
		m := d.(map[string]any)
		if m["is_system"] == true {
			system++
		}
		if m["code"] == "PPH21" {
			pph = m
		}
	}
	if system != 9 || pph == nil {
		t.Fatalf("signup seeds 9 system components: %d %v", system, list)
	}
	create := func(body map[string]any) (result, map[string]any) {
		return e.do(t, http.MethodPost, "/api/v1/component-definitions", tok, body)
	}
	if r, b := create(map[string]any{"code": "TUNJ_X", "name": "X", "category": "EARNING", "calc_type": "PERCENT_OF", "rate": "0.1", "base_ref": "TIDAK_ADA"}); r.StatusCode != http.StatusUnprocessableEntity || b["code"] != "INVALID_COMPONENT_REFERENCE" {
		t.Fatalf("reference to a missing component: %d %v", r.StatusCode, b)
	}
	if r, b := create(map[string]any{"code": "TUNJ_Y", "name": "Y", "category": "EARNING", "calc_type": "PERCENT_OF", "rate": "0.1", "base_ref": "TAXABLE_GROSS"}); r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("earning based on a later stage (TAXABLE_GROSS): want 422, got %d %v", r.StatusCode, b)
	}
	if r, b := create(map[string]any{"code": "TUNJ_Z", "name": "Z", "category": "EARNING", "calc_type": "FIXED", "rate": "0.1"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("FIXED with rate: want 400, got %d %v", r.StatusCode, b)
	}
	// A = 10% × TUNJ_JABATAN (fixture), B = 50% × A; lalu A → B membentuk siklus.
	_, a := create(map[string]any{"code": "TUNJ_A", "name": "A", "category": "EARNING", "calc_type": "PERCENT_OF", "rate": "0.1", "base_ref": "TUNJ_JABATAN"})
	if r, b := create(map[string]any{"code": "TUNJ_B", "name": "B", "category": "EARNING", "calc_type": "PERCENT_OF", "rate": "0.5", "base_ref": "TUNJ_A"}); r.StatusCode != http.StatusCreated {
		t.Fatalf("chain: %d %v", r.StatusCode, b)
	}
	r, b := e.doH(t, http.MethodPatch, "/api/v1/component-definitions/"+a["id"].(string), tok, map[string]string{"If-Match": `"1"`}, map[string]any{"base_ref": "TUNJ_B"})
	if r.StatusCode != http.StatusUnprocessableEntity || b["code"] != "COMPONENT_CYCLE" {
		t.Fatalf("cycle via PATCH: want 422 COMPONENT_CYCLE, got %d %v", r.StatusCode, b)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/component-definitions/"+a["id"].(string)+"/deactivate", tok, nil); r.StatusCode != http.StatusUnprocessableEntity || b["code"] != "INVALID_COMPONENT_REFERENCE" {
		t.Fatalf("deactivate a referenced component: want 422, got %d %v", r.StatusCode, b)
	}
	// Komponen sistem: hanya nama yang boleh diubah, tidak bisa dinonaktifkan.
	pphPath := "/api/v1/component-definitions/" + pph["id"].(string)
	if r, b := e.doH(t, http.MethodPatch, pphPath, tok, map[string]string{"If-Match": `"1"`}, map[string]any{"rounding": "UP"}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("change a system component parameter: want 403, got %d %v", r.StatusCode, b)
	}
	if r, b := e.doH(t, http.MethodPatch, pphPath, tok, map[string]string{"If-Match": `"1"`}, map[string]any{"name": "Pajak penghasilan"}); r.StatusCode != http.StatusOK {
		t.Fatalf("rename a system component: %d %v", r.StatusCode, b)
	}
	if r, _ := e.do(t, http.MethodPost, pphPath+"/deactivate", tok, nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("deactivate a system component: want 403, got %d", r.StatusCode)
	}
	// W10: komponen sistem tidak bisa ditugaskan manual.
	if r, b := e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/component-assignments", tok, key(),
		map[string]any{"component_id": pph["id"], "valid_from": "2026-02-01"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("assign a system component: want 400, got %d %v", r.StatusCode, b)
	}
}

// PI-19: rantai atasan tanpa siklus (A → B lalu B → A ditolak 422), termasuk dua permintaan PARALEL
// yang masing-masing tidak melihat yang lain: advisory lock per tenant menyerialkan keduanya.
func TestOrg_ManagerCycleRejectedEvenInParallel(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	assign := func(tok string, emp, mgr uuid.UUID) (result, map[string]any) {
		return e.doH(t, http.MethodPost, "/api/v1/employees/"+emp.String()+"/org-assignments", tok, key(),
			map[string]any{"valid_from": "2026-02-01", "department_id": f.dept, "position_id": f.pos, "manager_employee_id": mgr})
	}
	// Fixture: empSelf → empManager. Menjadikan empManager bawahan empSelf = siklus.
	if r, b := assign(f.tokens[roleH], f.empManager, f.empSelf); r.StatusCode != http.StatusUnprocessableEntity || b["code"] != "MANAGER_CYCLE" {
		t.Fatalf("direct cycle: want 422 MANAGER_CYCLE, got %d %v", r.StatusCode, b)
	}
	if r, b := assign(f.tokens[roleH], f.empOther, f.empOther); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("self as manager: want 400, got %d %v", r.StatusCode, b)
	}
	for round := range 5 {
		x := e.createEmployee(t, f.tokens[roleO], "PAR-X"+string(rune('0'+round)), "X", "31760000000000"+string(rune('1'+round))+"1")
		y := e.createEmployee(t, f.tokens[roleO], "PAR-Y"+string(rune('0'+round)), "Y", "31760000000000"+string(rune('1'+round))+"2")
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i, pair := range [][2]uuid.UUID{{x, y}, {y, x}} {
			wg.Add(1)
			go func(i int, emp, mgr uuid.UUID) {
				defer wg.Done()
				r, _ := assign(f.tokens[roleO], emp, mgr)
				codes[i] = r.StatusCode
			}(i, pair[0], pair[1])
		}
		wg.Wait()
		oneWon := (codes[0] == http.StatusCreated && codes[1] == http.StatusUnprocessableEntity) ||
			(codes[1] == http.StatusCreated && codes[0] == http.StatusUnprocessableEntity)
		if !oneWon {
			t.Fatalf("round %d: parallel X→Y and Y→X must yield one 201 and one 422, got %v", round, codes)
		}
	}
}

// G4: departemen yang masih dipakai penempatan hari ini tidak bisa dinonaktifkan; T4–T6 libur.
func TestOrg_DeactivateInUseAndHolidays(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	if r, b := e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/org-assignments", f.tokens[roleH], key(),
		map[string]any{"valid_from": "2026-01-05", "department_id": f.dept, "position_id": f.pos}); r.StatusCode != http.StatusCreated {
		t.Fatalf("assign: %d %v", r.StatusCode, b)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/departments/"+f.dept.String()+"/deactivate", f.tokens[roleH], nil); r.StatusCode != http.StatusConflict || b["code"] != "INVALID_STATE_TRANSITION" {
		t.Fatalf("deactivate a department in use: want 409, got %d %v", r.StatusCode, b)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/holidays", f.tokens[roleH], map[string]any{"date": "2026-03-19", "name": "Dobel"}); r.StatusCode != http.StatusConflict || b["code"] != "DUPLICATE_CODE" {
		t.Fatalf("duplicate holiday date: want 409 DUPLICATE_CODE, got %d %v", r.StatusCode, b)
	}
	e.do(t, http.MethodPost, "/api/v1/holidays", f.tokens[roleH], map[string]any{"date": "2027-01-01", "name": "Tahun baru"})
	_, y26 := e.do(t, http.MethodGet, "/api/v1/holidays?year=2026", f.tokens[roleE], nil)
	if d := y26["data"].([]any); len(d) != 1 || d[0].(map[string]any)["date"] != "2026-03-19" {
		t.Fatalf("holidays 2026: %v", y26)
	}
	if r, _ := e.do(t, http.MethodDelete, "/api/v1/holidays/"+f.holiday.String(), f.tokens[roleH], nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete holiday: %d", r.StatusCode)
	}
	if r, _ := e.do(t, http.MethodDelete, "/api/v1/holidays/"+f.holiday.String(), f.tokens[roleH], nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("delete again: want 404, got %d", r.StatusCode)
	}
}

// Review S3 (High): Idempotency-Key + body yang sama untuk resource LAIN (gaji employee B) bukan replay
// respons employee A, melainkan 422 IDEMPOTENCY_KEY_REUSED. Gaji B tidak boleh "sukses" tanpa berubah.
func TestIdempotency_KeyScopedToConcretePath(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	k := key()
	body := map[string]any{"base_salary": "6000000.00", "valid_from": "2026-02-01"}
	if r, b := e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/salary-changes", f.tokens[roleH], k, body); r.StatusCode != http.StatusCreated {
		t.Fatalf("employee A: %d %v", r.StatusCode, b)
	}
	r, b := e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empSelf.String()+"/salary-changes", f.tokens[roleH], k, body)
	if r.StatusCode != http.StatusUnprocessableEntity || b["code"] != "IDEMPOTENCY_KEY_REUSED" || r.Header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("same key + body for employee B: want 422 (not a replay of A), got %d %v %v", r.StatusCode, b, r.Header)
	}
}

// Review S3 (Medium): override sementara [Mar, Apr) di tengah record [Jan, ∞) mempertahankan ekornya
// [Apr, ∞) dengan nilai lama, bukan menghapus semua sesudah Maret.
func TestComponentAssignment_TemporaryOverrideKeepsTail(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	path := "/api/v1/employees/" + f.empOther.String() + "/component-assignments"
	if r, b := e.doH(t, http.MethodPost, path, f.tokens[roleH], key(), map[string]any{"component_id": f.comp, "amount": "500000.00", "valid_from": "2026-01-05"}); r.StatusCode != http.StatusCreated {
		t.Fatalf("base assignment: %d %v", r.StatusCode, b)
	}
	if r, b := e.doH(t, http.MethodPost, path, f.tokens[roleH], key(), map[string]any{"component_id": f.comp, "amount": "700000.00", "valid_from": "2026-03-01", "valid_to": "2026-04-01"}); r.StatusCode != http.StatusCreated {
		t.Fatalf("override: %d %v", r.StatusCode, b)
	}
	_, hist := e.do(t, http.MethodGet, "/api/v1/employees/"+f.empOther.String()+"/salary-history", f.tokens[roleH], nil)
	comps := hist["components"].([]any)
	type rec struct{ from, to, amount any }
	var got []rec
	for _, c := range comps {
		m := c.(map[string]any)
		got = append(got, rec{m["valid_from"], m["valid_to"], m["amount"]})
	}
	want := []rec{{"2026-01-05", "2026-03-01", "500000.00"}, {"2026-03-01", "2026-04-01", "700000.00"}, {"2026-04-01", nil, "500000.00"}}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("records: got %v, want %v", got, want)
	}
}

// Review S3 (Medium): validasi graf diserialkan per tenant. Dua PATCH paralel yang masing-masing sah
// (A → B dan B → A) tidak boleh sama-sama lolos dan membentuk siklus.
func TestComponents_ParallelCycleRejected(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	tok := f.tokens[roleO]
	for round := range 5 {
		ids := make([]string, 2)
		for i, code := range []string{"PAR_A", "PAR_B"} {
			_, b := e.do(t, http.MethodPost, "/api/v1/component-definitions", tok, map[string]any{"code": code + string(rune('0'+round)),
				"name": code, "category": "EARNING", "calc_type": "PERCENT_OF", "rate": "0.1", "base_ref": "TUNJ_JABATAN"})
			ids[i] = b["id"].(string)
		}
		codes := []string{"PAR_A" + string(rune('0'+round)), "PAR_B" + string(rune('0'+round))}
		var wg sync.WaitGroup
		status := make([]int, 2)
		for i := range 2 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r, _ := e.doH(t, http.MethodPatch, "/api/v1/component-definitions/"+ids[i], tok, map[string]string{"If-Match": `"1"`},
					map[string]any{"base_ref": codes[1-i]})
				status[i] = r.StatusCode
			}(i)
		}
		wg.Wait()
		ok := (status[0] == http.StatusOK && status[1] == http.StatusUnprocessableEntity) || (status[1] == http.StatusOK && status[0] == http.StatusUnprocessableEntity)
		if !ok {
			t.Fatalf("round %d: parallel A→B / B→A: want one 200 and one 422, got %v", round, status)
		}
	}
	for name, body := range map[string]map[string]any{
		"reserved code":          {"code": "BPJS_WAGE", "name": "X", "category": "EARNING", "calc_type": "FIXED", "default_amount": "1.00"},
		"deduction in BPJS wage": {"code": "POT_X", "name": "X", "category": "DEDUCTION", "calc_type": "FIXED", "default_amount": "1.00", "bpjs_base": true},
	} {
		if r, b := e.do(t, http.MethodPost, "/api/v1/component-definitions", tok, body); r.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d %v", name, r.StatusCode, b)
		}
	}
	if r, b := e.doH(t, http.MethodPatch, "/api/v1/component-definitions/"+f.comp.String(), tok, map[string]string{"If-Match": `"1"`}, map[string]any{"base_ref": ""}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty base_ref on FIXED: want 400 (not 500), got %d %v", r.StatusCode, b)
	}
}
