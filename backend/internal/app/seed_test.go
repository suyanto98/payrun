package app_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"payrun/internal/app"
)

// TestSeed_SmokeProfilePassesGate: seed perf (profil smoke) ke DB test kosong lulus gate RLS, dan tenant
// terbesar bisa login + dihitung lewat jalur aplikasi biasa (data seed setara data hasil API).
func TestSeed_SmokeProfilePassesGate(t *testing.T) {
	e := newEnvTiers(t, relaxedTiers())
	p := app.SeedProfiles["smoke"]
	rep, err := app.Seed(context.Background(), e.tdb.SuperDSN(), e.pool, p, 7)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Employees != p.Employees || rep.Tenants != 1+p.Small+p.Medium+p.Large {
		t.Fatalf("report: %+v", rep)
	}
	if _, err := app.Seed(context.Background(), e.tdb.SuperDSN(), e.pool, p, 7); err == nil {
		t.Fatal("seeding a non-empty database must be refused")
	}
	tok, _, _ := e.login(t, app.SeedPayrollEmail(0)) // password test = app.SeedPassword; satu membership → tenant aktif
	run, got := e.calculatedRun(t, tok, "2026-09")
	if c := got["counts"].(map[string]any); c["ok"] != float64(p.Big) {
		t.Fatalf("big tenant run: want %d OK items, got %v", p.Big, c)
	}
	var att int
	e.inTenant(t, rep.BigTenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM attendance_records`).Scan(&att)
	})
	if att != p.Big*len(app.SeedAttendanceDays) || run.String() == "" {
		t.Fatalf("attendance rows in big tenant: %d", att)
	}
}
