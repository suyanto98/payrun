package app_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------- PeriodGuard (S5, PI-13/PI-14, O12)

// finalizeRun: run CALCULATED → FINALIZED + periode FINALIZED lewat superuser (endpoint finalize baru
// di S6). Trigger tetap aktif; hanya RLS yang dilewati.
func (e env) finalizeRun(t *testing.T, run uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	sp := e.tdb.Super(t)
	if _, err := sp.Exec(ctx, `UPDATE payrun.payroll_runs SET status = 'FINALIZED', finalized_at = now() WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Exec(ctx, `UPDATE payrun.payroll_periods p SET status = 'FINALIZED', finalized_run_id = r.id, finalized_at = now()
		FROM payrun.payroll_runs r WHERE r.id = $1 AND p.id = r.period_id`, run); err != nil {
		t.Fatal(err)
	}
}

// inputChanges: employee → changed_at untuk periode run.
func (e env) inputChanges(t *testing.T, tenant, run uuid.UUID) map[uuid.UUID]time.Time {
	t.Helper()
	out := map[uuid.UUID]time.Time{}
	e.inTenant(t, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(), `SELECT c.employee_id, c.changed_at FROM payroll_input_changes c
			JOIN payroll_runs r ON r.period_id = c.period_id WHERE r.id = $1`, run)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var at time.Time
			if err := rows.Scan(&id, &at); err != nil {
				return err
			}
			out[id] = at
		}
		return rows.Err()
	})
	return out
}

func (e env) calcStartedAt(t *testing.T, tenant, run uuid.UUID) time.Time {
	t.Helper()
	var at time.Time
	e.inTenant(t, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT calc_started_at FROM payroll_runs WHERE id = $1`, run).Scan(&at)
	})
	return at
}

// TestPeriodGuard_FinalizedPeriodRejectsInputWrites (PI-14): setiap penulis input yang menyentuh tanggal
// di periode FINALIZED ditolak 409 PERIOD_LOCKED tanpa efek. Perubahan yang dimulai SETELAH periode
// final tetap boleh walaupun record lamanya juga berlaku di periode final (hanya tanggal yang nilainya
// berubah yang dicek). PTKP tidak effective-dated → tidak diblokir. Trigger DB menolak jalur yang lupa
// memanggil guard.
func TestPeriodGuard_FinalizedPeriodRejectsInputWrites(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o := f.tokens[roleO]
	emp := "/api/v1/employees/" + f.empOther.String()
	if r, b := e.doH(t, http.MethodPost, emp+"/salary-changes", o, key(), map[string]any{"base_salary": "5000000.00", "valid_from": "2026-01-05"}); r.StatusCode != http.StatusCreated {
		t.Fatalf("salary: %d %v", r.StatusCode, b)
	}
	run, _ := e.calculatedRun(t, f.tokens[roleP], "2026-03")
	e.finalizeRun(t, run)

	locked := func(name string, r result, b map[string]any) {
		t.Helper()
		if r.StatusCode != http.StatusConflict || b["code"] != "PERIOD_LOCKED" {
			t.Errorf("%s in a finalized period: want 409 PERIOD_LOCKED, got %d %v", name, r.StatusCode, b)
		}
	}
	r, b := e.doH(t, http.MethodPost, emp+"/salary-changes", o, key(), map[string]any{"base_salary": "6000000.00", "valid_from": "2026-03-15"})
	locked("salary change", r, b)
	r, b = e.doH(t, http.MethodPost, emp+"/component-assignments", o, key(), map[string]any{"component_id": f.comp, "valid_from": "2026-03-01"})
	locked("component assignment", r, b)
	r, b = e.do(t, http.MethodPost, "/api/v1/holidays", o, map[string]any{"date": "2026-03-20", "name": "Libur"})
	locked("holiday create", r, b)
	r, b = e.do(t, http.MethodDelete, "/api/v1/holidays/"+f.holiday.String(), o, nil) // 2026-03-19
	locked("holiday delete", r, b)
	r, b = e.doH(t, http.MethodPost, emp+"/terminate", o, map[string]string{"If-Match": `"1"`}, map[string]any{"type": "RESIGNED", "end_date": "2026-03-20"})
	locked("terminate", r, b)
	body := employeeBody("LATE-01", "Telat", "3174012304959901")
	body["hire_date"] = "2026-03-10"
	r, b = e.doH(t, http.MethodPost, "/api/v1/employees", o, key(), body)
	locked("hire", r, b)

	// Sesudah periode final: menutup gaji [2026-01-05, ∞) di 1 April hanya mengubah [Apr, ∞).
	if r, b := e.doH(t, http.MethodPost, emp+"/salary-changes", o, key(), map[string]any{"base_salary": "6500000.00", "valid_from": "2026-04-01"}); r.StatusCode != http.StatusCreated {
		t.Fatalf("raise after the finalized period: want 201, got %d %v", r.StatusCode, b)
	}
	if r, b := e.doH(t, http.MethodPatch, emp, o, map[string]string{"If-Match": `"1"`}, map[string]any{"ptkp_status": "K/2"}); r.StatusCode != http.StatusOK {
		t.Fatalf("PTKP change is not effective-dated: want 200, got %d %v", r.StatusCode, b)
	}

	// Jaring pengaman DB (trigger guard_period_date): INSERT langsung tanpa PeriodGuard.
	var pgErr *pgconn.PgError
	err := pgx.BeginFunc(context.Background(), e.tdb.Super(t), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO payrun.holidays (tenant_id, id, holiday_date, name) VALUES ($1, $2, '2026-03-25', 'x')`, f.id, newID())
		return err
	})
	if !errors.As(err, &pgErr) || pgErr.Code != "P0002" {
		t.Fatalf("trigger must reject a holiday in a finalized period with P0002, got %v", err)
	}
}

// TestPeriodGuard_RecordsInputChanges (O12): setiap penulis input mencatat (periode non-final,
// karyawan) dengan changed_at sesudah calc_started_at, untuk karyawan yang tepat: gaji/komponen/PTKP →
// karyawan itu; libur → semua karyawan yang bekerja di periode; definisi komponen → pemegangnya;
// karyawan baru → dirinya.
func TestPeriodGuard_RecordsInputChanges(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	f := e.newTenantFx(t, "alpha")
	o := f.tokens[roleO]
	run, _ := e.calculatedRun(t, f.tokens[roleP], "2026-03")
	started := e.calcStartedAt(t, f.id, run)
	if n := len(e.inputChanges(t, f.id, run)); n != 0 {
		t.Fatalf("no input changed yet, got %d change rows", n)
	}
	expect := func(step string, want ...uuid.UUID) {
		t.Helper()
		got := e.inputChanges(t, f.id, run)
		for _, id := range want {
			at, ok := got[id]
			if !ok {
				t.Fatalf("%s: employee %s not recorded (got %v)", step, id, got)
			}
			if !at.After(started) {
				t.Fatalf("%s: changed_at %s must be after calc_started_at %s", step, at, started)
			}
		}
	}

	e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/salary-changes", o, key(), map[string]any{"base_salary": "5000000.00", "valid_from": "2026-03-15"})
	expect("salary change", f.empOther)
	if got := e.inputChanges(t, f.id, run); len(got) != 1 {
		t.Fatalf("salary change must record only that employee, got %v", got)
	}

	e.doH(t, http.MethodPatch, "/api/v1/employees/"+f.empSelf.String(), o, map[string]string{"If-Match": `"1"`}, map[string]any{"ptkp_status": "K/3"})
	expect("PTKP", f.empSelf)

	e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empManager.String()+"/component-assignments", o, key(), map[string]any{"component_id": f.comp, "valid_from": "2026-01-05"})
	expect("component assignment", f.empManager)

	if r, b := e.doH(t, http.MethodPatch, "/api/v1/component-definitions/"+f.comp.String(), o, map[string]string{"If-Match": `"1"`},
		map[string]any{"default_amount": "600000.00"}); r.StatusCode != http.StatusOK {
		t.Fatalf("definition update: %d %v", r.StatusCode, b)
	}
	expect("component definition (holder)", f.empManager)

	body := employeeBody("NEW-01", "Baru", "3174012304958801")
	body["hire_date"] = "2026-03-10"
	_, nb := e.doH(t, http.MethodPost, "/api/v1/employees", o, key(), body)
	newbie := uuid.MustParse(nb["id"].(string))
	expect("new hire", newbie)

	// Libur: semua karyawan yang bekerja di Maret, termasuk yang belum pernah dicatat.
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `DELETE FROM payroll_input_changes`)
		return err
	})
	if r, b := e.do(t, http.MethodPost, "/api/v1/holidays", o, map[string]any{"date": "2026-03-27", "name": "Libur"}); r.StatusCode != http.StatusCreated {
		t.Fatalf("holiday: %d %v", r.StatusCode, b)
	}
	expect("holiday", f.empSelf, f.empOther, f.empManager, newbie)

	// Periode lain (Februari, belum ada) tidak dicatat dan tidak dibuat.
	e.doH(t, http.MethodPost, "/api/v1/employees/"+f.empOther.String()+"/salary-changes", o, key(), map[string]any{"base_salary": "5100000.00", "valid_from": "2026-11-01"})
	var periods int
	e.inTenant(t, f.id, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM payroll_periods`).Scan(&periods)
	})
	if periods != 1 {
		t.Fatalf("PeriodGuard must never create periods, got %d", periods)
	}
}
