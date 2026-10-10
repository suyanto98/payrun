package app_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"payrun/internal/modules/payroll"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/jobs"
	"payrun/internal/platform/metrics"
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

// TestVerifyRunJob_AfterFinalize_FeedsInvariantMetrics (DoD C3, PI-05, PI-12): finalize meng-enqueue
// payroll.verify_run di transaksi yang sama; worker memverifikasi run itu di bawah RLS tenant-nya, sehingga
// metrik INV hidup di proses yang di-scrape (bukan di proses CLI). Data bersih → gauge 0, counter tetap.
// Total run dimanipulasi langsung di DB → gauge 1; satu baris dimanipulasi → counter reproducibility naik;
// dipulihkan → gauge kembali 0.
func TestVerifyRunJob_AfterFinalize_FeedsInvariantMetrics(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	run := e.approvedRun(t, f)
	if r, b := e.finalize(t, f.tokens[roleP], run, key()["Idempotency-Key"]); r.StatusCode != http.StatusOK {
		t.Fatalf("finalize: %d %v", r.StatusCode, b)
	}
	repro := testutil.ToFloat64(metrics.ReproducibilityFailures.WithLabelValues())
	e.drainJobs(t)
	jobStatus := func() (n int, status string) {
		e.inTenant(t, f.id, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT count(*), coalesce(max(status), '') FROM jobs
				WHERE kind = 'payroll.verify_run' AND payload->>'run_id' = $1`, run.String()).Scan(&n, &status)
		})
		return
	}
	if n, st := jobStatus(); n != 1 || st != "succeeded" {
		t.Fatalf("finalize must enqueue exactly one verify_run job that succeeds: %d %q", n, st)
	}
	gauge := func() float64 { return testutil.ToFloat64(metrics.PayrollTotalMismatchRuns.WithLabelValues()) }
	if gauge() != 0 || testutil.ToFloat64(metrics.ReproducibilityFailures.WithLabelValues()) != repro {
		t.Fatalf("clean run: gauge %v, reproducibility %v → %v", gauge(), repro, testutil.ToFloat64(metrics.ReproducibilityFailures.WithLabelValues()))
	}
	verifyAgain := func() { // verifikasi ulang (dedupe finalize hanya sekali; ops bisa mengantrekan lagi)
		e.inTenant(t, f.id, func(tx pgx.Tx) error {
			_, err := jobs.Enqueue(context.Background(), tx, jobs.Spec{Kind: "payroll.verify_run", MaxAttempts: 3,
				Payload: map[string]any{"run_id": run, "calc_version": 1}})
			return err
		})
		e.drainJobs(t)
	}

	e.tamper(t, `UPDATE payrun.payroll_runs SET total_gross = total_gross + 1, total_net = total_net + 1 WHERE id = $1`, run)
	verifyAgain()
	if gauge() != 1 {
		t.Fatalf("run total tampered: gauge payrun_payroll_total_mismatch_runs want 1, got %v", gauge())
	}
	e.tamper(t, `UPDATE payrun.payroll_runs SET total_gross = total_gross - 1, total_net = total_net - 1 WHERE id = $1`, run)
	var item uuid.UUID
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT id FROM payroll_items WHERE run_id = $1 AND status = 'OK' LIMIT 1`, run).Scan(&item)
	})
	e.tamper(t, `UPDATE payrun.payroll_lines SET amount = amount + 1 WHERE item_id = $1 AND sort_order = 1`, item)
	verifyAgain()
	if gauge() != 0 || testutil.ToFloat64(metrics.ReproducibilityFailures.WithLabelValues()) != repro+1 {
		t.Fatalf("line tampered, totals restored: gauge want 0 got %v; reproducibility want %v got %v",
			gauge(), repro+1, testutil.ToFloat64(metrics.ReproducibilityFailures.WithLabelValues()))
	}
}
