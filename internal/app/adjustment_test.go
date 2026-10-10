package app_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	gen "payrun/api/gen"
)

// ---------------------------------------------------------------- adjustment (S7, PF-19, PI-15)

func (e env) createAdjustment(t *testing.T, token string, body map[string]any) (result, map[string]any) {
	t.Helper()
	return e.doH(t, http.MethodPost, "/api/v1/adjustments", token, key(), body)
}

func adjBody(emp uuid.UUID, origin, target, amount string) map[string]any {
	return map[string]any{"employee_id": emp, "origin_period": origin, "target_period": target,
		"component_code": "OVERTIME_CORRECTION", "amount": amount, "reason": "lembur kurang 2 jam"}
}

// adjustmentLines: baris ADJUSTMENT (adjustment_id, amount, kategori) versi kalkulasi terkini run.
func (e env) adjustmentLines(t *testing.T, tenant, run uuid.UUID) map[uuid.UUID][2]string {
	t.Helper()
	out := map[uuid.UUID][2]string{}
	e.inTenant(t, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT l.adjustment_id, l.amount::text, l.category FROM payroll_lines l
			JOIN payroll_runs r ON r.id = l.run_id AND r.calc_version = l.calc_version WHERE l.run_id = $1 AND l.code = 'ADJUSTMENT'`, run)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var amt, cat string
			if err := rows.Scan(&id, &amt, &cat); err != nil {
				return err
			}
			out[id] = [2]string{amt, cat}
		}
		return rows.Err()
	})
	return out
}

func (e env) adjustmentState(t *testing.T, tenant, id uuid.UUID) (status string, appliedRun *uuid.UUID) {
	t.Helper()
	e.inTenant(t, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT status, applied_run_id FROM adjustments WHERE id = $1`, id).Scan(&status, &appliedRun)
	})
	return status, appliedRun
}

// TestAdjustment_AppliedOnce (PI-15, PF-19 AC1–AC4): koreksi Maret (FINALIZED) masuk run April sebagai
// baris ADJUSTMENT; adjustment yang dibuat setelah April dikalkulasi membuat run stale, pembatalannya
// memulihkan fingerprint; finalize April → APPLIED tepat sekali; setelahnya immutable dan periode
// April menolak adjustment baru (PERIOD_LOCKED).
func TestAdjustment_AppliedOnce(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o, p, h := f.tokens[roleO], f.tokens[roleP], f.tokens[roleH]
	march := e.approvedRun(t, f)
	if r, b := e.finalize(t, p, march, newID().String()); r.StatusCode != http.StatusOK {
		t.Fatalf("finalize March: %d %v", r.StatusCode, b)
	}

	// Validasi: periode asal harus FINALIZED, target setelah asal, amount ≠ 0 dan ≤ 2 desimal.
	for name, c := range map[string]struct {
		body  map[string]any
		field string
	}{
		"origin not finalized": {adjBody(f.empOther, "2026-04", "2026-05", "1000.00"), "origin_period"},
		"target before origin": {adjBody(f.empOther, "2026-03", "2026-02", "1000.00"), "target_period"},
		"zero amount":          {adjBody(f.empOther, "2026-03", "2026-04", "0.00"), "amount"},
		"three decimals":       {adjBody(f.empOther, "2026-03", "2026-04", "1.005"), "amount"},
	} {
		r, b := e.createAdjustment(t, h, c.body)
		if r.StatusCode != http.StatusBadRequest || !hasField(b, c.field) {
			t.Errorf("%s: want 400 on %s, got %d %v", name, c.field, r.StatusCode, b)
		}
	}
	if r, b := e.createAdjustment(t, h, adjBody(newID(), "2026-03", "2026-04", "1000.00")); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown employee: want 404, got %d %v", r.StatusCode, b)
	}

	r, adj := e.createAdjustment(t, h, adjBody(f.empOther, "2026-03", "2026-04", "76300.58"))
	if r.StatusCode != http.StatusCreated || adj["status"] != "PENDING_APPLY" || adj["amount"] != "76300.58" ||
		adj["origin_period"] != "2026-03" || adj["target_period"] != "2026-04" || adj["taxable"] != true {
		t.Fatalf("create: %d %v", r.StatusCode, adj)
	}
	assertMatchesSpec(t, "adjustment", adj, &gen.Adjustment{})
	adj1 := uuid.MustParse(adj["id"].(string))

	april := e.mustCalculated(t, f, "2026-04")
	lines := e.adjustmentLines(t, f.id, april)
	if len(lines) != 1 || lines[adj1] != [2]string{"76301.00", "EARNING"} {
		t.Fatalf("April adjustment lines: %v", lines)
	}

	// Adjustment baru setelah April dikalkulasi → stale (AC4); dibatalkan → fingerprint kembali sama.
	_, adj = e.createAdjustment(t, p, adjBody(f.empSelf, "2026-03", "2026-04", "-50000.00"))
	adj2 := uuid.MustParse(adj["id"].(string))
	ap := e.submitRun(t, p, april, map[string]any{})
	if r, b := e.approve(t, o, ap); r.StatusCode != http.StatusConflict || b["code"] != "RUN_STALE" || !staleIncludes(b, f.empSelf) {
		t.Fatalf("approve with a new adjustment: want RUN_STALE incl. empSelf, got %d %v", r.StatusCode, b)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/adjustments/"+adj2.String()+"/cancel", o, map[string]any{"reason": "Salah input"}); r.StatusCode != http.StatusOK || b["status"] != "CANCELLED" {
		t.Fatalf("cancel: %d %v", r.StatusCode, b)
	}
	if r, b := e.approve(t, o, ap); r.StatusCode != http.StatusOK {
		t.Fatalf("approve after cancel: %d %v", r.StatusCode, b)
	}
	if r, b := e.finalize(t, p, april, newID().String()); r.StatusCode != http.StatusOK {
		t.Fatalf("finalize April: %d %v", r.StatusCode, b)
	}

	if st, run := e.adjustmentState(t, f.id, adj1); st != "APPLIED" || run == nil || *run != april {
		t.Fatalf("adj1 after finalize: %s %v", st, run)
	}
	if st, run := e.adjustmentState(t, f.id, adj2); st != "CANCELLED" || run != nil {
		t.Fatalf("adj2 after finalize: %s %v", st, run)
	}
	if r, b := e.do(t, http.MethodPost, "/api/v1/adjustments/"+adj1.String()+"/cancel", o, map[string]any{"reason": "x"}); r.StatusCode != http.StatusConflict || b["code"] != "INVALID_STATE_TRANSITION" {
		t.Fatalf("cancel APPLIED: want 409 INVALID_STATE_TRANSITION, got %d %v", r.StatusCode, b)
	}
	// Lapis DB (trigger adjustments_applied_immutable) menolak perubahan langsung.
	if err := e.tryInTenant(f.id, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE adjustments SET amount = 1 WHERE id = $1`, adj1)
		return err
	}); err == nil {
		t.Fatal("direct UPDATE of an APPLIED adjustment succeeded")
	}
	if r, b := e.createAdjustment(t, h, adjBody(f.empOther, "2026-03", "2026-04", "1000.00")); r.StatusCode != http.StatusConflict || b["code"] != "PERIOD_LOCKED" {
		t.Fatalf("adjustment into finalized April: want PERIOD_LOCKED, got %d %v", r.StatusCode, b)
	}

	r, list := e.do(t, http.MethodGet, "/api/v1/adjustments?target_period=2026-04&status=APPLIED", h, nil)
	assertMatchesSpec(t, "adjustments", list, &gen.AdjustmentList{})
	if data := list["data"].([]any); r.StatusCode != http.StatusOK || len(data) != 1 || data[0].(map[string]any)["applied_run_id"] != april.String() {
		t.Fatalf("list APPLIED: %d %v", r.StatusCode, list)
	}
	if rep := e.verifyRun(t, f.id, april); !rep.OK() {
		t.Fatalf("verify-run April: %+v", rep)
	}
}

func hasField(b map[string]any, field string) bool {
	fs, _ := b["errors"].([]any)
	for _, x := range fs {
		if m, ok := x.(map[string]any); ok && m["field"] == field {
			return true
		}
	}
	return false
}

func staleIncludes(b map[string]any, emp uuid.UUID) bool {
	es, _ := b["employees_changed"].([]any)
	for _, x := range es {
		if m, ok := x.(map[string]any); ok && m["employee_id"] == emp.String() {
			return true
		}
	}
	return false
}
