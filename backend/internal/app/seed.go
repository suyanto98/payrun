package app

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/internal/platform/auth"
	"payrun/internal/platform/db"
)

// SeedProfile: ukuran dataset load test (docs/design/26-performance-plan.md §4).
type SeedProfile struct {
	Name      string
	Small     int // tenant 10–50 karyawan
	Medium    int // tenant 100–500 karyawan
	Large     int // tenant besar; menyerap sisa supaya total = Employees
	Big       int // ukuran satu tenant terbesar (PR-1, PR-4)
	Employees int // total karyawan seluruh tenant
}

// Profil seed. "perf" = 1.000 tenant / 100.000 karyawan (900 kecil, 95 sedang, 4 besar, 1 × 10.000);
// "smoke" untuk CI dan uji cepat; "demo" = dua tenant untuk demo live (seed-0 100 karyawan = Alpha,
// seed-1 10–50 = Beta; docs/demo-script.md).
var SeedProfiles = map[string]SeedProfile{
	"demo":  {Name: "demo", Small: 1, Big: 100},
	"smoke": {Name: "smoke", Small: 15, Medium: 3, Large: 1, Big: 500, Employees: 2_000},
	"perf":  {Name: "perf", Small: 900, Medium: 95, Large: 4, Big: 10_000, Employees: 100_000},
}

// Akun yang dipakai skrip perf (perf/). Semua user seed memakai password yang sama.
const (
	SeedPassword = "correct horse battery"
	seedFrom     = "2025-01-01" // mulai bekerja, gaji, dan penempatan semua karyawan seed
)

// SeedOwnerEmail / SeedPayrollEmail: OWNER dan PAYROLL_ADMIN tenant ke-i (0 = tenant terbesar).
func SeedOwnerEmail(i int) string   { return fmt.Sprintf("owner-%d@seed.payrun.test", i) }
func SeedPayrollEmail(i int) string { return fmt.Sprintf("payroll-%d@seed.payrun.test", i) }

// SeedAttendanceDays: tanggal absensi yang di-seed (COMPLETE) untuk setiap karyawan (PR-3 GET /attendance).
var SeedAttendanceDays = []string{"2026-09-01", "2026-09-02", "2026-09-03"}

type SeedReport struct {
	Profile               string
	Tenants, Employees    int
	Attendance            int
	BigTenant             uuid.UUID
	LoadSeconds, TotalSec float64
}

// Seed mengisi database KOSONG dengan dataset sintetis lewat COPY (superDSN, session_replication_role =
// replica: trigger PeriodGuard, FK, dan updated_at dilewati demi kecepatan), lalu menjalankan GATE
// invariant lewat role aplikasi (appPool, RLS berlaku): seed yang gagal gate tidak boleh dipakai untuk
// mengukur apa pun. PII karyawan (NIK/NPWP/rekening) sengaja kosong: skema mengizinkan NULL dan tidak ada
// skenario perf yang mendekripsi PII.
func Seed(ctx context.Context, superDSN string, appPool *pgxpool.Pool, p SeedProfile, rngSeed uint64) (SeedReport, error) {
	rep := SeedReport{Profile: p.Name}
	start := time.Now() //nolint:forbidigo // durasi teknis
	conn, err := pgx.Connect(ctx, superDSN)
	if err != nil {
		return rep, err
	}
	defer func() { _ = conn.Close(ctx) }()
	var existing int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM payrun.employees`).Scan(&existing); err != nil {
		return rep, err
	}
	if existing > 0 {
		return rep, errors.New("seed: database already has employees; seed only into an empty database")
	}
	hash, err := auth.HashPassword(SeedPassword, auth.DefaultArgon2) // satu hash untuk semua user (2.000 × argon2 = menit)
	if err != nil {
		return rep, err
	}
	rng := rand.New(rand.NewPCG(rngSeed, rngSeed^0x9e3779b97f4a7c15)) //nolint:gosec // G404: data sintetis deterministik
	g := newSeedGen(p, rng)

	tx, err := conn.Begin(ctx)
	if err != nil {
		return rep, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		return rep, fmt.Errorf("seed: needs a superuser connection (SEED_DATABASE_URL): %w", err)
	}
	if err := g.copyAll(ctx, tx, hash); err != nil {
		return rep, err
	}
	// Komponen sistem dan jenis cuti: fungsi yang sama dengan signup (satu panggilan per tenant).
	for _, t := range g.tenants {
		if _, err := tx.Exec(ctx, `SELECT payrun.seed_system_components($1, $2)`, t.id, t.owner); err != nil {
			return rep, err
		}
		if _, err := tx.Exec(ctx, `SELECT payrun.seed_leave_types($1, $2)`, t.id, t.owner); err != nil {
			return rep, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return rep, err
	}
	rep.Tenants, rep.Employees, rep.Attendance, rep.BigTenant = len(g.tenants), g.employees, g.attendance, g.tenants[0].id
	rep.LoadSeconds = time.Since(start).Seconds() //nolint:forbidigo // durasi teknis

	for _, t := range []string{"users", "tenants", "memberships", "employees", "employment_spans", "salary_assignments",
		"org_assignments", "attendance_records", "component_definitions"} {
		if _, err := conn.Exec(ctx, "VACUUM (ANALYZE) payrun."+t); err != nil {
			return rep, err
		}
	}
	if _, err := conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`); err == nil {
		_, _ = conn.Exec(ctx, `SELECT pg_stat_statements_reset()`)
	}
	if err := g.gate(ctx, appPool); err != nil {
		return rep, fmt.Errorf("seed GATE failed (do not measure on this dataset): %w", err)
	}
	rep.TotalSec = time.Since(start).Seconds() //nolint:forbidigo // durasi teknis
	return rep, nil
}

type seedTenant struct {
	id, owner, payroll uuid.UUID
	size               int
}

type seedGen struct {
	p                     SeedProfile
	rng                   *rand.Rand
	tenants               []seedTenant
	employees, attendance int
}

func newSeedGen(p SeedProfile, rng *rand.Rand) *seedGen {
	g := &seedGen{p: p, rng: rng}
	sizes := []int{p.Big}
	for range p.Small {
		sizes = append(sizes, 10+rng.IntN(41))
	}
	for range p.Medium {
		sizes = append(sizes, 100+rng.IntN(401))
	}
	used := 0
	for _, s := range sizes {
		used += s
	}
	rest := max(p.Employees-used, p.Large) // tenant besar menyerap sisa → total tepat p.Employees
	for i := range p.Large {
		share := rest / p.Large
		if i == 0 {
			share += rest % p.Large
		}
		sizes = append(sizes, share)
	}
	for _, s := range sizes {
		g.tenants = append(g.tenants, seedTenant{id: uuid.Must(uuid.NewV7()), owner: uuid.Must(uuid.NewV7()), payroll: uuid.Must(uuid.NewV7()), size: s})
	}
	return g
}

func dateRangeFrom(from string) pgtype.Range[pgtype.Date] {
	t, _ := time.Parse(time.DateOnly, from)
	return pgtype.Range[pgtype.Date]{Lower: pgtype.Date{Time: t, Valid: true}, LowerType: pgtype.Inclusive, UpperType: pgtype.Unbounded, Valid: true}
}

func (g *seedGen) copyAll(ctx context.Context, tx pgx.Tx, hash string) error {
	now := time.Now().UTC() //nolint:forbidigo // waktu pembuatan data sintetis
	span := dateRangeFrom(seedFrom)
	var users, tenants, settings, members, depts, poss, emps, spans, sal, org, att [][]any
	ptkp := []string{"TK/0", "TK/1", "K/0", "K/1", "K/2", "K/3"}
	for i, t := range g.tenants {
		slug := fmt.Sprintf("seed-%d", i)
		users = append(users, []any{t.owner, SeedOwnerEmail(i), hash, fmt.Sprintf("Owner %d", i), now},
			[]any{t.payroll, SeedPayrollEmail(i), hash, fmt.Sprintf("Payroll %d", i), now})
		tenants = append(tenants, []any{t.id, slug, fmt.Sprintf("PT Seed %d", i), "ACTIVE"})
		settings = append(settings, []any{t.id, "Asia/Jakarta", t.owner})
		members = append(members, []any{t.id, uuid.Must(uuid.NewV7()), t.owner, []string{"OWNER"}, "ACTIVE", now, t.owner},
			[]any{t.id, uuid.Must(uuid.NewV7()), t.payroll, []string{"PAYROLL_ADMIN"}, "ACTIVE", now, t.owner})
		var deptIDs, posIDs []uuid.UUID
		for d := range 3 {
			did, pid := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
			deptIDs, posIDs = append(deptIDs, did), append(posIDs, pid)
			depts = append(depts, []any{t.id, did, fmt.Sprintf("D%d", d+1), fmt.Sprintf("Departemen %d", d+1), t.owner})
			poss = append(poss, []any{t.id, pid, fmt.Sprintf("P%d", d+1), fmt.Sprintf("Jabatan %d", d+1), t.owner})
		}
		for e := range t.size {
			eid := uuid.Must(uuid.NewV7())
			// Nama berawalan huruf acak: prefix search PR-3 ("Ka…") mengenai sebagian, bukan semua karyawan.
			name := fmt.Sprintf("%c%s %d", 'A'+g.rng.Int32N(26), "aryawan", e+1)
			emps = append(emps, []any{t.id, eid, fmt.Sprintf("E-%06d", e+1), name, "ACTIVE", ptkp[g.rng.IntN(len(ptkp))], t.owner})
			spans = append(spans, []any{t.id, uuid.Must(uuid.NewV7()), eid, span, t.owner})
			salary := pgtype.Numeric{}
			_ = salary.Scan(fmt.Sprintf("%d.00", 4_000_000+g.rng.IntN(110)*100_000))
			sal = append(sal, []any{t.id, uuid.Must(uuid.NewV7()), eid, salary, span, t.owner})
			k := g.rng.IntN(3)
			org = append(org, []any{t.id, uuid.Must(uuid.NewV7()), eid, deptIDs[k], posIDs[k], span, t.owner})
			for _, day := range SeedAttendanceDays {
				d, _ := time.Parse(time.DateOnly, day)
				in := d.Add(time.Hour + time.Duration(g.rng.IntN(60))*time.Minute) // 08:00–09:00 WIB
				att = append(att, []any{t.id, uuid.Must(uuid.NewV7()), eid, d, in, in.Add(9 * time.Hour), "COMPLETE", "CLOCK", t.owner})
			}
			g.employees++
		}
	}
	g.attendance = len(att)
	copies := []struct {
		table string
		cols  []string
		rows  [][]any
	}{
		{"users", []string{"id", "email", "password_hash", "display_name", "email_verified_at"}, users},
		{"tenants", []string{"id", "slug", "name", "status"}, tenants},
		{"tenant_settings", []string{"tenant_id", "timezone", "created_by"}, settings},
		{"memberships", []string{"tenant_id", "id", "user_id", "roles", "status", "accepted_at", "created_by"}, members},
		{"departments", []string{"tenant_id", "id", "code", "name", "created_by"}, depts},
		{"positions", []string{"tenant_id", "id", "code", "name", "created_by"}, poss},
		{"employees", []string{"tenant_id", "id", "employee_no", "full_name", "status", "ptkp_status", "created_by"}, emps},
		{"employment_spans", []string{"tenant_id", "id", "employee_id", "span", "created_by"}, spans},
		{"salary_assignments", []string{"tenant_id", "id", "employee_id", "base_salary", "valid_during", "created_by"}, sal},
		{"org_assignments", []string{"tenant_id", "id", "employee_id", "department_id", "position_id", "valid_during", "created_by"}, org},
		{"attendance_records", []string{"tenant_id", "id", "employee_id", "work_date", "clock_in_at", "clock_out_at", "status", "source", "created_by"}, att},
	}
	for _, c := range copies {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"payrun", c.table}, c.cols, pgx.CopyFromRows(c.rows)); err != nil {
			return fmt.Errorf("copy %s: %w", c.table, err)
		}
	}
	return nil
}

// gate: lewat role aplikasi (RLS berlaku). Setiap tenant melihat TEPAT karyawannya sendiri (jumlah = ukuran
// yang di-seed, tidak ada tenant_id lain), punya satu OWNER aktif, dan semua karyawan punya gaji + masa kerja.
func (g *seedGen) gate(ctx context.Context, appPool *pgxpool.Pool) error {
	for i, t := range g.tenants {
		var n, foreign, owners, noSalary int
		err := db.WithTenantTxFor(ctx, appPool, t.id, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM employees),
				(SELECT count(*) FROM employees WHERE tenant_id <> $1),
				(SELECT count(*) FROM memberships WHERE status = 'ACTIVE' AND 'OWNER' = ANY(roles)),
				(SELECT count(*) FROM employees e WHERE NOT EXISTS (SELECT 1 FROM salary_assignments s WHERE s.employee_id = e.id)
				   OR NOT EXISTS (SELECT 1 FROM employment_spans s WHERE s.employee_id = e.id))`, t.id).Scan(&n, &foreign, &owners, &noSalary)
		})
		if err != nil {
			return err
		}
		if n != t.size || foreign != 0 || owners != 1 || noSalary != 0 {
			return fmt.Errorf("tenant %d (%s): employees %d (want %d), foreign rows %d, owners %d, without salary/span %d",
				i, t.id, n, t.size, foreign, owners, noSalary)
		}
	}
	return nil
}
