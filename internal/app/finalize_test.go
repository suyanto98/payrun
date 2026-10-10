package app_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------- finalize (S6, Y11) + Demo C + HR override

// approvedRun: Maret 2026 dengan gaji untuk ketiga employee, dihitung, di-submit PAYROLL_ADMIN, dan
// di-approve OWNER.
func (e env) approvedRun(t *testing.T, f *tenantFx) uuid.UUID {
	t.Helper()
	run := e.salariedRun(t, f)
	ap := e.submitRun(t, f.tokens[roleP], run, map[string]any{})
	if r, b := e.approve(t, f.tokens[roleO], ap); r.StatusCode != http.StatusOK {
		t.Fatalf("approve: %d %v", r.StatusCode, b)
	}
	return run
}

func (e env) finalize(t *testing.T, token string, run uuid.UUID, idemKey string) (result, map[string]any) {
	t.Helper()
	return e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/finalize", token, map[string]string{"Idempotency-Key": idemKey}, nil)
}

func (e env) periodStatus(t *testing.T, tenant, run uuid.UUID) (status string, finalRun *uuid.UUID) {
	t.Helper()
	e.inTenant(t, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT p.status, p.finalized_run_id FROM payroll_periods p JOIN payroll_runs r ON r.period_id = p.id
			WHERE r.id = $1`, run).Scan(&status, &finalRun)
	})
	return status, finalRun
}

// TestFinalize_LocksPeriodAndRun (PF-14, ADR-P07): run APPROVED → FINALIZED, periode FINALIZED dengan
// finalized_run_id; setelahnya run immutable (RUN_FINALIZED) dan semua penulis input periode itu ditolak
// (PERIOD_LOCKED). Versi kalkulasi lama dibersihkan job payroll.cleanup_versions. Hasil tetap terverifikasi.
func TestFinalize_LocksPeriodAndRun(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o, p := f.tokens[roleO], f.tokens[roleP]
	run := e.salariedRun(t, f)
	if r, b := e.doH(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/calculate", p, key(), nil); r.StatusCode != http.StatusAccepted { // versi 2
		t.Fatalf("recalculate: %d %v", r.StatusCode, b)
	}
	e.drainJobs(t)
	ap := e.submitRun(t, p, run, map[string]any{})
	e.approve(t, o, ap)

	if r, b := e.finalize(t, f.tokens[roleH], run, newID().String()); r.StatusCode != http.StatusForbidden {
		t.Fatalf("HR_ADMIN finalize: want 403, got %d %v", r.StatusCode, b)
	}
	r, got := e.finalize(t, p, run, newID().String())
	if r.StatusCode != http.StatusOK || got["status"] != "FINALIZED" || got["finalized_by"] != f.users[roleP].String() || got["finalized_at"] == nil {
		t.Fatalf("finalize: %d %v", r.StatusCode, got)
	}
	if st, fr := e.periodStatus(t, f.id, run); st != "FINALIZED" || fr == nil || *fr != run {
		t.Fatalf("period: %s %v", st, fr)
	}
	base := "/api/v1/payroll-runs/" + run.String()
	_, item := e.firstItem(t, f, run)
	for name, call := range map[string]func() (result, map[string]any){
		"recalculate": func() (result, map[string]any) { return e.doH(t, http.MethodPost, base+"/calculate", p, key(), nil) },
		"cancel": func() (result, map[string]any) {
			return e.do(t, http.MethodPost, base+"/cancel", p, map[string]any{"reason": "Salah periode"})
		},
		"exclude": func() (result, map[string]any) {
			return e.do(t, http.MethodPost, base+"/items/"+item.String()+"/exclude", p, map[string]any{"reason": "Dibayar manual bulan ini"})
		},
		"finalize again": func() (result, map[string]any) { return e.finalize(t, p, run, newID().String()) },
	} {
		if r, b := call(); r.StatusCode != http.StatusConflict || b["code"] != "RUN_FINALIZED" {
			t.Errorf("%s on a finalized run: want 409 RUN_FINALIZED, got %d %v", name, r.StatusCode, b)
		}
	}
	if r, b := e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/salary-changes", f.tokens[roleH], key(),
		map[string]any{"base_salary": "7000000.00", "valid_from": "2026-03-20"}); r.StatusCode != http.StatusConflict || b["code"] != "PERIOD_LOCKED" {
		t.Fatalf("salary change in the finalized period: want PERIOD_LOCKED, got %d %v", r.StatusCode, b)
	}
	if r, b := e.submitLeave(t, f.tokens[roleE], e.leaveTypeID(t, f.id, "ANNUAL"), "2026-03-23", "2026-03-23"); r.StatusCode != http.StatusConflict || b["code"] != "PERIOD_LOCKED" {
		t.Fatalf("leave in the finalized period: want PERIOD_LOCKED, got %d %v", r.StatusCode, b)
	}

	e.drainJobs(t) // payroll.cleanup_versions
	var old, current int
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE calc_version = 1), count(*) FILTER (WHERE calc_version = 2)
			FROM payroll_items WHERE run_id = $1`, run).Scan(&old, &current)
	})
	if old != 0 || current != 3 {
		t.Fatalf("cleanup_versions: want 0 old and 3 current items, got %d / %d", old, current)
	}
	if rep := e.verifyRun(t, f.id, run); !rep.OK() || rep.Status != "FINALIZED" {
		t.Fatalf("verify-run after finalize: %+v", rep)
	}
}

func (e env) firstItem(t *testing.T, f *tenantFx, run uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	items := e.runItems(t, f.tokens[roleP], run)
	if len(items) == 0 {
		t.Fatal("run has no items")
	}
	return run, uuid.MustParse(items[0]["id"].(string))
}

// TestFinalize_StaleAfterApproveRejected (Step 7 `22` §3.4, §4.c): input berubah SETELAH approve → finalize
// memeriksa stale ulang → 409 RUN_STALE; run tetap APPROVED dan periode tidak terkunci.
func TestFinalize_StaleAfterApproveRejected(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	run := e.approvedRun(t, f)
	e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/salary-changes", f.tokens[roleH], key(),
		map[string]any{"base_salary": "7000000.00", "valid_from": "2026-03-20"})
	r, b := e.finalize(t, f.tokens[roleP], run, newID().String())
	if r.StatusCode != http.StatusConflict || b["code"] != "RUN_STALE" || len(b["employees_changed"].([]any)) != 1 {
		t.Fatalf("finalize with input changed after approve: want RUN_STALE, got %d %v", r.StatusCode, b)
	}
	if _, got := e.do(t, http.MethodGet, "/api/v1/payroll-runs/"+run.String(), f.tokens[roleP], nil); got["status"] != "APPROVED" {
		t.Fatalf("run must stay APPROVED: %v", got)
	}
	if st, _ := e.periodStatus(t, f.id, run); st == "FINALIZED" {
		t.Fatal("period must not be finalized")
	}
}

// TestDemoC_FinalizedPayrollIsImmutable (Demo C bagian 1–5, docs/design/33-demo-script.md §3.5): setelah
// finalize lewat API: koreksi absensi Maret → PERIOD_LOCKED; mengubah item → RUN_FINALIZED; UPDATE langsung
// ke DB sebagai payrun_app → P0001; koreksi yang benar = adjustment Maret → April (baris "Koreksi" di run
// April); Maret tetap identik dan bisa dihitung ulang dari snapshot.
func TestDemoC_FinalizedPayrollIsImmutable(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	m := &matrixRunner{e: e, f: f, items: map[uuid.UUID]uuid.UUID{}}
	m.seq.Store(40) // work_date absensi target: Maret 2026
	rec := m.target(t, "attendance_self", true)
	run := e.approvedRun(t, f)
	if r, b := e.finalize(t, f.tokens[roleP], run, newID().String()); r.StatusCode != http.StatusOK {
		t.Fatalf("finalize: %d %v", r.StatusCode, b)
	}

	r, b := e.doH(t, http.MethodPatch, "/api/v1/attendance/"+rec.String(), f.tokens[roleH], map[string]string{"If-Match": `"1"`},
		map[string]any{"clock_out_at": "2026-01-01T09:00:00Z", "correction_reason": "Koreksi setelah final"})
	if r.StatusCode != http.StatusConflict || b["code"] != "PERIOD_LOCKED" {
		t.Fatalf("1. attendance correction after finalize: want PERIOD_LOCKED, got %d %v", r.StatusCode, b)
	}
	_, item := e.firstItem(t, f, run)
	if r, b := e.do(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/items/"+item.String()+"/exclude", f.tokens[roleP],
		map[string]any{"reason": "Mencoba mengubah item final"}); r.StatusCode != http.StatusConflict || b["code"] != "RUN_FINALIZED" {
		t.Fatalf("2. changing an item after finalize: want RUN_FINALIZED, got %d %v", r.StatusCode, b)
	}
	var pgErr *pgconn.PgError
	err := e.tryInTenant(f.id, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE payroll_items SET net = net + 1 WHERE id = $1`, item)
		return err
	})
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Fatalf("3. direct UPDATE as payrun_app: want P0001, got %v", err)
	}
	marchNet := e.runPayslips(t, f.tokens[roleP], run)[f.empSelf]["net"]

	r, adj := e.createAdjustment(t, f.tokens[roleH], adjBody(f.empSelf, "2026-03", "2026-04", "76300.58"))
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("4. adjustment March → April: want 201, got %d %v", r.StatusCode, adj)
	}
	april := e.mustCalculated(t, f, "2026-04")
	var expl string
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT l.explanation FROM payroll_lines l JOIN payroll_items i ON i.id = l.item_id
			WHERE l.run_id = $1 AND l.code = 'ADJUSTMENT' AND i.employee_id = $2 AND l.adjustment_id = $3`, april, f.empSelf, adj["id"]).Scan(&expl)
	})
	if !strings.HasPrefix(expl, "Koreksi periode 2026-03") {
		t.Fatalf("4. April line: %q", expl)
	}
	if got := e.runPayslips(t, f.tokens[roleP], run)[f.empSelf]["net"]; got != marchNet {
		t.Fatalf("5. March net changed after the adjustment: %v → %v", marchNet, got)
	}
	if rep := e.verifyRun(t, f.id, run); !rep.OK() || rep.Status != "FINALIZED" {
		t.Fatalf("5. finalized March must stay reproducible: %+v", rep)
	}
}

// TestApproval_HROverrideLeaveOnly (file 17 §3.3, PF-11 AC2): HR_ADMIN boleh memutuskan cuti/lembur
// menggantikan approver (is_override, audit approval.overridden), TIDAK untuk payroll run, dan tidak untuk
// request miliknya sendiri. Role HR dibaca dari DB.
func TestApproval_HROverrideLeaveOnly(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	hr := f.tokens[roleH]
	_, lv := e.submitLeave(t, f.tokens[roleE], e.leaveTypeID(t, f.id, "ANNUAL"), "2026-10-12", "2026-10-12")
	r, dec := e.approve(t, hr, approvalOf(t, lv))
	a, _ := dec["approval"].(map[string]any)
	if r.StatusCode != http.StatusOK || a["is_override"] != true || a["decided_by"] != f.users[roleH].String() {
		t.Fatalf("HR override: %d %v", r.StatusCode, dec)
	}
	if e.auditCount(t, f.id, "approval.overridden") != 1 {
		t.Fatal("override must be audited as approval.overridden")
	}

	runAp := e.submitRun(t, f.tokens[roleP], e.mustCalculated(t, f, "2025-12"), map[string]any{})
	if r, _ := e.approve(t, hr, runAp); r.StatusCode != http.StatusNotFound {
		t.Fatalf("HR override of a payroll run: want 404, got %d", r.StatusCode)
	}

	// HR tertaut ke employee dan mengajukan cuti sendiri: approver = OWNER (tidak ada HR lain); override
	// atas request sendiri → 403.
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE employees SET user_id = $1 WHERE id = $2`, f.users[roleH], f.empOther)
		return err
	})
	_, own := e.submitLeave(t, hr, e.leaveTypeID(t, f.id, "ANNUAL"), "2026-10-13", "2026-10-13")
	if own["approval"].(map[string]any)["approver"].(map[string]any)["id"] != f.users[roleO].String() {
		t.Fatalf("HR's own leave: approver must be OWNER, got %v", own["approval"])
	}
	if r, b := e.approve(t, hr, approvalOf(t, own)); r.StatusCode != http.StatusForbidden || b["code"] != "SELF_APPROVAL_FORBIDDEN" {
		t.Fatalf("HR overriding own request: want 403, got %d %v", r.StatusCode, b)
	}

	// HR diturunkan (token lama masih HR_ADMIN) → tidak bisa override lagi.
	_, lv2 := e.submitLeave(t, f.tokens[roleE], e.leaveTypeID(t, f.id, "ANNUAL"), "2026-10-14", "2026-10-14")
	if r, b := e.setRoles(t, f.tokens[roleO], e.memberOf(t, f.id, f.users[roleH]), 1, roleE); r.StatusCode != http.StatusOK {
		t.Fatalf("demote HR: %d %v", r.StatusCode, b)
	}
	if r, _ := e.approve(t, hr, approvalOf(t, lv2)); r.StatusCode != http.StatusNotFound {
		t.Fatalf("demoted HR with a stale token: want 404, got %d", r.StatusCode)
	}
}

func (e env) mustCalculated(t *testing.T, f *tenantFx, period string) uuid.UUID {
	t.Helper()
	run, _ := e.calculatedRun(t, f.tokens[roleP], period)
	return run
}
