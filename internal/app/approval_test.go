package app_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"payrun/api/gen"
)

// ---------------------------------------------------------------- approval + submit (S5, M6)

// salariedRun: Maret 2026 dengan gaji untuk ketiga employee fixture (semua item OK), CALCULATED.
func (e env) salariedRun(t *testing.T, f *tenantFx) uuid.UUID {
	t.Helper()
	for _, emp := range []uuid.UUID{f.empSelf, f.empOther, f.empManager} {
		if r, b := e.doH(t, http.MethodPost, "/api/v1/employees/"+emp.String()+"/salary-changes", f.tokens[roleO], key(),
			map[string]any{"base_salary": "5000000.00", "valid_from": "2026-01-05"}); r.StatusCode != http.StatusCreated {
			t.Fatalf("salary: %d %v", r.StatusCode, b)
		}
	}
	run, got := e.calculatedRun(t, f.tokens[roleP], "2026-03")
	if c := got["counts"].(map[string]any); c["ok"] != float64(3) {
		t.Fatalf("want 3 OK items, got %v", c)
	}
	return run
}

func (e env) approve(t *testing.T, token string, approval uuid.UUID) (result, map[string]any) {
	t.Helper()
	return e.do(t, http.MethodPost, "/api/v1/approvals/"+approval.String()+"/approve", token, map[string]any{})
}

func (e env) approvalStatus(t *testing.T, tenant, id uuid.UUID) (approval, run string) {
	t.Helper()
	e.inTenant(t, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT a.status, r.status FROM approval_requests a
			JOIN payroll_runs r ON r.id = a.subject_id WHERE a.id = $1`, id).Scan(&approval, &run)
	})
	return approval, run
}

// TestApproval_SubmitApproveHappyPath: submit oleh PAYROLL_ADMIN → approver otomatis OWNER (satu-satunya
// admin lain) → inbox OWNER memuat ringkasan → approve → run APPROVED. Semua respons cocok skema.
func TestApproval_SubmitApproveHappyPath(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	run := e.salariedRun(t, f)
	r, sub := e.do(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/submit", f.tokens[roleP], map[string]any{})
	if r.StatusCode != http.StatusOK || sub["status"] != "PENDING_APPROVAL" || sub["submitted_by"] != f.users[roleP].String() {
		t.Fatalf("submit: %d %v", r.StatusCode, sub)
	}
	assertMatchesSpec(t, "run (submitted)", sub, &gen.PayrollRun{})
	apID := uuid.MustParse(sub["approval_request_id"].(string))

	_, inbox := e.do(t, http.MethodGet, "/api/v1/approvals/inbox", f.tokens[roleO], nil)
	assertMatchesSpec(t, "inbox", inbox, &gen.ApprovalList{})
	items := inbox["data"].([]any)
	if len(items) != 1 {
		t.Fatalf("owner inbox: want 1 pending approval, got %v", inbox)
	}
	first := items[0].(map[string]any)
	if first["subject"].(map[string]any)["summary"] != "Payroll 2026-03 · PENDING_APPROVAL · net "+mustNet(t, e, f, run)+" · 3 karyawan" {
		t.Fatalf("inbox summary: %v", first["subject"])
	}
	if _, mine := e.do(t, http.MethodGet, "/api/v1/approvals/inbox", f.tokens[roleP], nil); len(mine["data"].([]any)) != 0 {
		t.Fatalf("requester's inbox must not show the approval: %v", mine)
	}

	r, dec := e.approve(t, f.tokens[roleO], apID)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("approve: %d %v", r.StatusCode, dec)
	}
	assertMatchesSpec(t, "decision", dec, &gen.ApprovalDecision{})
	assertMatchesSpec(t, "decision.approval", dec["approval"].(map[string]any), &gen.ApprovalRequest{})
	subj := dec["subject"].(map[string]any)
	assertMatchesSpec(t, "decision.subject", subj, &gen.PayrollRun{})
	if subj["status"] != "APPROVED" || subj["approved_by"] != f.users[roleO].String() || dec["approval"].(map[string]any)["status"] != "APPROVED" {
		t.Fatalf("after approve: %v", dec)
	}
	if r, b := e.approve(t, f.tokens[roleO], apID); r.StatusCode != http.StatusConflict || b["code"] != "APPROVAL_ALREADY_DECIDED" || b["approval_status"] != "APPROVED" {
		t.Fatalf("second approve: want 409 APPROVAL_ALREADY_DECIDED (APPROVED), got %d %v", r.StatusCode, b)
	}
	assertMatchesSpec(t, "problem (decided)", mustProblem(t, e, f, apID), &gen.Problem{})
	for _, action := range []string{"payroll_run.submitted", "approval.requested", "payroll_run.approved", "approval.approved"} {
		if e.auditCount(t, f.id, action) != 1 {
			t.Errorf("audit %s must be written exactly once", action)
		}
	}
}

func mustNet(t *testing.T, e env, f *tenantFx, run uuid.UUID) string {
	t.Helper()
	_, r := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+run.String(), f.tokens[roleP], nil)
	return r["totals"].(map[string]any)["net"].(string)
}

func mustProblem(t *testing.T, e env, f *tenantFx, apID uuid.UUID) map[string]any {
	t.Helper()
	_, b := e.approve(t, f.tokens[roleO], apID)
	return b
}

// TestSubmit_InputChangedAfterCalc_ReturnsRunStaleWithEmployees (DoD, ADR-P07, O12): HR menaikkan gaji
// satu karyawan setelah kalkulasi → approve ditolak 409 RUN_STALE dengan karyawan itu; approval tetap
// PENDING dan run tetap PENDING_APPROVAL. Reject → recalculate → submit → approve berhasil.
func TestSubmit_InputChangedAfterCalc_ReturnsRunStaleWithEmployees(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	run := e.salariedRun(t, f)
	apID := e.submitRun(t, f.tokens[roleP], run, map[string]any{})

	if r, b := e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/salary-changes", f.tokens[roleH], key(),
		map[string]any{"base_salary": "6000000.00", "valid_from": "2026-03-15"}); r.StatusCode != http.StatusCreated {
		t.Fatalf("raise: %d %v", r.StatusCode, b)
	}
	r, b := e.approve(t, f.tokens[roleO], apID)
	if r.StatusCode != http.StatusConflict || b["code"] != "RUN_STALE" || b["changed_count"] != float64(1) {
		t.Fatalf("approve after input change: want 409 RUN_STALE, got %d %v", r.StatusCode, b)
	}
	assertMatchesSpec(t, "problem (stale)", b, &gen.Problem{})
	changed := b["employees_changed"].([]any)
	if len(changed) != 1 || changed[0].(map[string]any)["employee_id"] != f.empOther.String() ||
		changed[0].(map[string]any)["employee_no"] != "EMP-0002" || changed[0].(map[string]any)["reason"] != "INPUT_CHANGED" {
		t.Fatalf("employees_changed must name exactly the employee whose salary changed: %v", changed)
	}
	if a, rs := e.approvalStatus(t, f.id, apID); a != "PENDING" || rs != "PENDING_APPROVAL" {
		t.Fatalf("stale approve must change nothing: approval %s, run %s", a, rs)
	}

	if r, b := e.do(t, http.MethodPost, "/api/v1/approvals/"+apID.String()+"/reject", f.tokens[roleO], map[string]any{"note": "Gaji Rina berubah, hitung ulang"}); r.StatusCode != http.StatusOK ||
		b["subject"].(map[string]any)["status"] != "CALCULATED" {
		t.Fatalf("reject: %d %v", r.StatusCode, b)
	}
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/calculate", f.tokens[roleP], key(), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("recalculate: %d %v", r.StatusCode, b)
	}
	e.drainJobs(t)
	ap2 := e.submitRun(t, f.tokens[roleP], run, map[string]any{})
	if r, b := e.approve(t, f.tokens[roleO], ap2); r.StatusCode != http.StatusOK {
		t.Fatalf("approve after recalculation: %d %v", r.StatusCode, b)
	}
}

// TestApprove_StaleFalsePositiveFiltered (O12): PTKP diubah lalu dikembalikan → ada kandidat di change log,
// tetapi fingerprint sama → approve berhasil.
func TestApprove_StaleFalsePositiveFiltered(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	run := e.salariedRun(t, f)
	apID := e.submitRun(t, f.tokens[roleP], run, map[string]any{})
	path := "/api/v1/employees/" + f.empSelf.String()
	e.doH(t, http.MethodPatch, path, f.tokens[roleH], map[string]string{"If-Match": `"1"`}, map[string]any{"ptkp_status": "TK/0"})
	e.doH(t, http.MethodPatch, path, f.tokens[roleH], map[string]string{"If-Match": `"2"`}, map[string]any{"ptkp_status": "K/1"})
	if n := len(e.inputChanges(t, f.id, run)); n != 1 {
		t.Fatalf("the change log must hold the candidate, got %d", n)
	}
	if r, b := e.approve(t, f.tokens[roleO], apID); r.StatusCode != http.StatusOK {
		t.Fatalf("same input after revert: want 200, got %d %v", r.StatusCode, b)
	}
}

// TestApprove_StaleDetectsHireAndTermination (D6): karyawan baru yang eligible di periode, dan karyawan
// yang kini tidak lagi eligible, sama-sama membuat run stale.
func TestApprove_StaleDetectsHireAndTermination(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	run := e.salariedRun(t, f)
	apID := e.submitRun(t, f.tokens[roleP], run, map[string]any{})

	body := employeeBody("NEW-01", "Baru", "3174012304958801")
	body["hire_date"] = "2026-03-16"
	_, nb := e.doH(t, http.MethodPost, "/api/v1/employees", f.tokens[roleH], key(), body)
	newbie := nb["id"].(string)
	// Rina berhenti 27 Feb: tidak lagi bekerja di Maret.
	if r, b := e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/terminate", f.tokens[roleH], map[string]string{"If-Match": `"1"`},
		map[string]any{"type": "RESIGNED", "end_date": "2026-02-27"}); r.StatusCode != http.StatusOK {
		t.Fatalf("terminate: %d %v", r.StatusCode, b)
	}
	r, b := e.approve(t, f.tokens[roleO], apID)
	if r.StatusCode != http.StatusConflict || b["code"] != "RUN_STALE" {
		t.Fatalf("want RUN_STALE, got %d %v", r.StatusCode, b)
	}
	reasons := map[string]string{}
	for _, c := range b["employees_changed"].([]any) {
		m := c.(map[string]any)
		reasons[m["employee_id"].(string)] = m["reason"].(string)
	}
	if reasons[newbie] != "NEWLY_ELIGIBLE" || reasons[f.empOther.String()] != "NO_LONGER_ELIGIBLE" || len(reasons) != 2 {
		t.Fatalf("D6 reasons: %v", reasons)
	}
}

// TestApproval_ApproveVsRejectRace_SingleDecision (DoD, PFS-08): approve dan reject bersamaan → tepat satu
// menang, yang lain 409 APPROVAL_ALREADY_DECIDED; status run selalu sesuai keputusan pemenang.
func TestApproval_ApproveVsRejectRace_SingleDecision(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	for i := range 8 {
		run, _ := e.calculatedRun(t, f.tokens[roleP], "2025-"+itoa2(i+1))
		apID := e.submitRun(t, f.tokens[roleP], run, map[string]any{})
		statuses, codes := make([]int, 2), make([]any, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for j, path := range []string{"/approve", "/reject"} {
			wg.Go(func() {
				<-start
				r, b := e.do(t, http.MethodPost, "/api/v1/approvals/"+apID.String()+path, f.tokens[roleO], map[string]any{"note": "Balapan"})
				statuses[j], codes[j] = r.StatusCode, b["code"]
			})
		}
		close(start)
		wg.Wait()
		wins := 0
		for j := range 2 {
			switch {
			case statuses[j] == http.StatusOK:
				wins++
			case statuses[j] != http.StatusConflict || codes[j] != "APPROVAL_ALREADY_DECIDED":
				t.Fatalf("round %d: loser must get 409 APPROVAL_ALREADY_DECIDED, got %d %v", i, statuses[j], codes[j])
			}
		}
		a, rs := e.approvalStatus(t, f.id, apID)
		consistent := (a == "APPROVED" && rs == "APPROVED") || (a == "REJECTED" && rs == "CALCULATED")
		if wins != 1 || !consistent {
			t.Fatalf("round %d: want one decision consistent with the run, got wins=%d approval=%s run=%s", i, wins, a, rs)
		}
	}
}

func itoa2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}

// TestRunApprove_BySubmitter_Forbidden (DoD, BR7): submitter tidak bisa menyetujui run yang ia submit
// (403 SELF_APPROVAL_FORBIDDEN, approval tetap PENDING); admin lain yang bukan approver → 404.
func TestRunApprove_BySubmitter_Forbidden(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	run, _ := e.calculatedRun(t, f.tokens[roleP], "2025-11")
	apID := e.submitRun(t, f.tokens[roleP], run, map[string]any{})
	if r, b := e.approve(t, f.tokens[roleP], apID); r.StatusCode != http.StatusForbidden || b["code"] != "SELF_APPROVAL_FORBIDDEN" {
		t.Fatalf("submitter approving: want 403 SELF_APPROVAL_FORBIDDEN, got %d %v", r.StatusCode, b)
	}
	if r, _ := e.approve(t, f.tokens[roleH], apID); r.StatusCode != http.StatusNotFound {
		t.Fatalf("non-approver: want 404, got %d", r.StatusCode)
	}
	if a, _ := e.approvalStatus(t, f.id, apID); a != "PENDING" {
		t.Fatalf("approval must stay PENDING, got %s", a)
	}
	// Approver yang role adminnya dicabut setelah submit tidak lagi bisa memutuskan.
	if r, b := e.setRoles(t, f.tokens[roleO], e.memberOf(t, f.id, f.users[roleP]), 1, roleO, roleP); r.StatusCode != http.StatusOK {
		t.Fatalf("second owner: %d %v", r.StatusCode, b)
	}
	tokB, _, _ := e.login(t, f.emails[roleP])
	if r, b := e.setRoles(t, tokB, e.memberOf(t, f.id, f.users[roleO]), 1, roleE); r.StatusCode != http.StatusOK {
		t.Fatalf("demote approver: %d %v", r.StatusCode, b)
	}
	if r, b := e.approve(t, f.tokens[roleO], apID); r.StatusCode != http.StatusForbidden {
		t.Fatalf("demoted approver with a stale token: want 403, got %d %v", r.StatusCode, b)
	}
}

// TestSubmit_RunWithErrorItems_Rejected (DoD): submit run dengan item ERROR yang belum di-exclude → 422
// RUN_HAS_ERRORS (+ error_count); setelah semua di-exclude, submit berhasil.
func TestSubmit_RunWithErrorItems_Rejected(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	p := f.tokens[roleP]
	run, _ := e.calculatedRun(t, p, "2026-03") // tiga employee tanpa gaji → 3 ERROR
	r, b := e.do(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/submit", p, map[string]any{})
	if r.StatusCode != http.StatusUnprocessableEntity || b["code"] != "RUN_HAS_ERRORS" || b["error_count"] != float64(3) {
		t.Fatalf("submit with errors: want 422 RUN_HAS_ERRORS (3), got %d %v", r.StatusCode, b)
	}
	for _, it := range e.runItems(t, p, run) {
		e.do(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/items/"+it["id"].(string)+"/exclude", p, map[string]any{"reason": "Dibayar manual bulan ini"})
	}
	e.submitRun(t, p, run, map[string]any{})
	// Run yang menunggu approval tidak bisa dihitung ulang atau dibatalkan.
	if r, _ := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/calculate", p, key(), nil); r.StatusCode != http.StatusConflict {
		t.Fatalf("calculate while PENDING_APPROVAL: want 409, got %d", r.StatusCode)
	}
	if r, _ := e.do(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/cancel", p, map[string]any{"reason": "x y z"}); r.StatusCode != http.StatusConflict {
		t.Fatalf("cancel while PENDING_APPROVAL: want 409, got %d", r.StatusCode)
	}
	if r, _ := e.do(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/submit", p, map[string]any{}); r.StatusCode != http.StatusConflict {
		t.Fatalf("second submit: want 409, got %d", r.StatusCode)
	}
}

// TestSubmit_SelfApprovalOnlyForSingleAdminTenant (ADR-P09 + amandemen): jumlah admin dihitung server.
// Ada admin lain → alasan self-approval ditolak 403. Satu-satunya admin → tanpa alasan 422; dengan alasan
// → approval self_approved yang bisa ia setujui sendiri. Reject tanpa catatan → 422.
func TestSubmit_SelfApprovalOnlyForSingleAdminTenant(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o := f.tokens[roleO]
	run, _ := e.calculatedRun(t, o, "2025-10")
	path := "/api/v1/payroll-runs/" + run.String() + "/submit"
	if r, b := e.do(t, http.MethodPost, path, o, map[string]any{"self_approval_reason": "Sendirian"}); r.StatusCode != http.StatusForbidden || b["code"] != "SELF_APPROVAL_FORBIDDEN" {
		t.Fatalf("reason while another admin exists: want 403, got %d %v", r.StatusCode, b)
	}
	if r, b := e.do(t, http.MethodPost, path, o, map[string]any{"approver_user_id": f.users[roleH]}); r.StatusCode != http.StatusNotFound {
		t.Fatalf("approver who is not an admin: want 404, got %d %v", r.StatusCode, b)
	}
	if r, b := e.revokeMember(t, o, e.memberOf(t, f.id, f.users[roleP])); r.StatusCode != http.StatusOK {
		t.Fatalf("revoke the other admin: %d %v", r.StatusCode, b)
	}
	if r, b := e.do(t, http.MethodPost, path, o, map[string]any{}); r.StatusCode != http.StatusUnprocessableEntity || b["code"] != "SELF_APPROVAL_REASON_REQUIRED" {
		t.Fatalf("single admin without reason: want 422, got %d %v", r.StatusCode, b)
	}
	apID := e.submitRun(t, o, run, map[string]any{"self_approval_reason": "Satu-satunya admin di tenant ini"})
	if r, b := e.do(t, http.MethodPost, "/api/v1/approvals/"+apID.String()+"/reject", o, map[string]any{}); r.StatusCode != http.StatusUnprocessableEntity || b["code"] != "DECISION_NOTE_REQUIRED" {
		t.Fatalf("reject without note: want 422, got %d %v", r.StatusCode, b)
	}
	r, b := e.approve(t, o, apID)
	if r.StatusCode != http.StatusOK || b["approval"].(map[string]any)["self_approved"] != true {
		t.Fatalf("self-approve: %d %v", r.StatusCode, b)
	}
}

// TestApproval_SelfApprovalRejectedByDB (DoD, PI-11): CHECK DB menolak approval dengan approver =
// requester tanpa flag + alasan, apa pun jalur penulisnya.
func TestApproval_SelfApprovalRejectedByDB(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	u := f.users[roleO]
	for name, args := range map[string][]any{
		"self without flag":              {false, nil},
		"self flag without reason":       {true, nil},
		"self flag for leave":            {true, "alasan"},
		"flag with a different approver": {true, "alasan"},
	} {
		subjectType, approver := "PAYROLL_RUN", u
		if name == "self flag for leave" {
			subjectType = "LEAVE_REQUEST"
		}
		if name == "flag with a different approver" {
			approver = f.users[roleP]
		}
		var pgErr *pgconn.PgError
		err := e.tryInTenant(f.id, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `INSERT INTO approval_requests (tenant_id, id, subject_type, subject_id, requester_id, approver_id, self_approved, self_approval_reason)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, f.id, newID(), subjectType, newID(), u, approver, args[0], args[1])
			return err
		})
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("%s: want CHECK violation 23514, got %v", name, err)
		}
	}
}
