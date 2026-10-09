package app_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"payrun/internal/modules/payroll"
	"payrun/internal/platform/idgen"
)

// ---------------------------------------------------------------- recompute-from-snapshot (PI-12)

func (e env) verifyRun(t *testing.T, tenant, run uuid.UUID) payroll.VerifyReport {
	t.Helper()
	rep, err := payroll.NewService(e.pool, e.clk, idgen.V7{}, nil).VerifyRun(context.Background(), tenant, run)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// tamper menjalankan SQL sebagai superuser dengan trigger dimatikan (session_replication_role = replica),
// mensimulasikan perubahan langsung di DB yang melewati semua penjaga aplikasi dan trigger.
func (e env) tamper(t *testing.T, sql string, args ...any) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), e.tdb.Super(t), func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), sql, args...)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRecompute_FromSnapshotMatchesStoredResults (DoD M6, PI-12, ADR-P07): setiap item run FINAL —
// OK dengan komponen, ERROR yang di-exclude — dihitung ulang dari snapshot input + ruleset versi
// tersimpan dan hasilnya identik baris per baris. Data operasional yang berubah SETELAHNYA tidak
// berpengaruh. Perubahan langsung pada baris, total, atau snapshot terdeteksi sebagai perbedaan.
func TestRecompute_FromSnapshotMatchesStoredResults(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o := f.tokens[roleO]
	for i, emp := range []uuid.UUID{f.empSelf, f.empOther} {
		e.doH(t, http.MethodPost, "/api/v1/employees/"+emp.String()+"/salary-changes", o, key(),
			map[string]any{"base_salary": []string{"5000000.00", "7333333.00"}[i], "valid_from": "2026-01-05"})
	}
	e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/salary-changes", o, key(), map[string]any{"base_salary": "8000000.00", "valid_from": "2026-03-16"})
	e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/component-assignments", o, key(), map[string]any{"component_id": f.comp, "valid_from": "2026-01-05"})
	run, _ := e.calculatedRun(t, f.tokens[roleP], "2026-03") // empManager tanpa gaji → ERROR
	_, item := e.errorItemOf(t, f.tokens[roleP], run)
	e.do(t, http.MethodPost, "/api/v1/payroll-runs/"+run.String()+"/items/"+item.String()+"/exclude", f.tokens[roleP], map[string]any{"reason": "Gaji belum diinput HR"})
	e.finalizeRun(t, run)

	rep := e.verifyRun(t, f.id, run)
	if !rep.OK() || rep.ItemsChecked != 3 || rep.Status != "FINALIZED" {
		t.Fatalf("recompute from snapshot must reproduce every item: %+v", rep)
	}

	// Data operasional berubah setelah final (periode April): hasil verifikasi tidak berubah.
	e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empSelf.String()+"/salary-changes", o, key(), map[string]any{"base_salary": "9000000.00", "valid_from": "2026-04-01"})
	if rep := e.verifyRun(t, f.id, run); !rep.OK() {
		t.Fatalf("later operational changes must not affect a finalized run: %+v", rep)
	}

	var okItem uuid.UUID
	var empNo string
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT id, employee_no FROM payroll_items WHERE run_id = $1 AND employee_id = $2`, run, f.empOther).Scan(&okItem, &empNo)
	})
	cases := []struct {
		name, reason, do, undo string
	}{
		{"line amount", "LINE_MISMATCH",
			`UPDATE payrun.payroll_lines SET amount = amount + 1 WHERE item_id = $1 AND sort_order = 1`,
			`UPDATE payrun.payroll_lines SET amount = amount - 1 WHERE item_id = $1 AND sort_order = 1`},
		{"stored totals", "RESULT_MISMATCH",
			`UPDATE payrun.payroll_items SET gross = gross + 1, net = net + 1 WHERE id = $1`,
			`UPDATE payrun.payroll_items SET gross = gross - 1, net = net - 1 WHERE id = $1`},
		{"snapshot input", "FINGERPRINT_MISMATCH",
			`UPDATE payrun.payroll_item_snapshots SET input = jsonb_set(input, '{salary,0,base_salary}', '"9999999"') WHERE item_id = $1`,
			``},
	}
	for _, c := range cases {
		e.tamper(t, c.do, okItem)
		rep := e.verifyRun(t, f.id, run)
		if len(rep.Mismatches) != 1 || rep.Mismatches[0].ItemID != okItem || rep.Mismatches[0].EmployeeNo != empNo || rep.Mismatches[0].Reason != c.reason {
			t.Fatalf("%s tampered: want exactly one %s for %s, got %+v", c.name, c.reason, empNo, rep.Mismatches)
		}
		if c.undo != "" {
			e.tamper(t, c.undo, okItem)
			if rep := e.verifyRun(t, f.id, run); !rep.OK() {
				t.Fatalf("%s restored: %+v", c.name, rep)
			}
		}
	}
}

// TestFinalizedRun_DirectUpdateRejected (PI-04): run FINALIZED dan semua isinya immutable — lewat API
// (exclude, cancel, calculate, submit → 409) maupun SQL langsung sebagai payrun_app (trigger → P0001):
// UPDATE run, UPDATE/DELETE/INSERT item, line, snapshot. Chunk zombie yang mencoba menulis item ke run
// final juga ditolak trigger (Step 7 `22` §4.g).
func TestFinalizedRun_DirectUpdateRejected(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	p := f.tokens[roleP]
	e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/salary-changes", f.tokens[roleO], key(),
		map[string]any{"base_salary": "5000000.00", "valid_from": "2026-01-05"}) // item OK dengan lines
	run, _ := e.calculatedRun(t, p, "2026-03")
	_, item := e.errorItemOf(t, p, run)
	e.finalizeRun(t, run)
	var lines int
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM payroll_lines WHERE run_id = $1`, run).Scan(&lines)
	})
	if lines == 0 {
		t.Fatal("fixture: the run must have lines to attack")
	}

	base := "/api/v1/payroll-runs/" + run.String()
	for name, call := range map[string]func() (result, map[string]any){
		"exclude": func() (result, map[string]any) {
			return e.do(t, http.MethodPost, base+"/items/"+item.String()+"/exclude", p, map[string]any{"reason": "Dibayar manual bulan ini"})
		},
		"cancel": func() (result, map[string]any) {
			return e.do(t, http.MethodPost, base+"/cancel", p, map[string]any{"reason": "Salah periode"})
		},
		"calculate": func() (result, map[string]any) { return e.doH(t, http.MethodPost, base+"/calculate", p, key(), nil) },
		"submit":    func() (result, map[string]any) { return e.do(t, http.MethodPost, base+"/submit", p, map[string]any{}) },
	} {
		if r, b := call(); r.StatusCode != http.StatusConflict {
			t.Errorf("API %s on a finalized run: want 409, got %d %v", name, r.StatusCode, b)
		}
	}

	var calcVersion int32
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT calc_version FROM payroll_runs WHERE id = $1`, run).Scan(&calcVersion)
	})
	for name, sql := range map[string]string{
		"update run":      `UPDATE payroll_runs SET failure_reason = 'x' WHERE id = $1`,
		"update item":     `UPDATE payroll_items SET excluded_reason = 'x' WHERE run_id = $1`,
		"delete item":     `DELETE FROM payroll_items WHERE run_id = $1`,
		"delete line":     `DELETE FROM payroll_lines WHERE run_id = $1`,
		"delete snapshot": `DELETE FROM payroll_item_snapshots WHERE run_id = $1`,
		"zombie chunk insert": `INSERT INTO payroll_items (tenant_id, id, run_id, employee_id, calc_version, status, error_codes, warning_codes,
			issues, input_fingerprint, employee_no, employee_name)
			SELECT tenant_id, '` + newID().String() + `', run_id, employee_id, calc_version, status, error_codes, warning_codes, issues,
			input_fingerprint, employee_no || '-z', employee_name FROM payroll_items WHERE run_id = $1 LIMIT 1`,
	} {
		var pgErr *pgconn.PgError
		err := e.tryInTenant(f.id, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), sql, run)
			return err
		})
		if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
			t.Errorf("%s on a finalized run: want P0001, got %v", name, err)
		}
	}
	if rep := e.verifyRun(t, f.id, run); !rep.OK() || rep.CalcVersion != calcVersion {
		t.Fatalf("finalized run must still verify: %+v", rep)
	}
}
