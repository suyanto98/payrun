package migrations_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"payrun/internal/platform/db/dbtest"
)

// TestRoles_LeastPrivilegeAttributes (docs/design/13-db-rls-consistency-indexing.md §1.1, DoD Sprint 0):
// atribut keempat role sesuai desain. Yang paling penting: role aplikasi tidak bisa melewati RLS,
// dan satu-satunya role BYPASSRLS (backup) hanya bisa membaca.
func TestRoles_LeastPrivilegeAttributes(t *testing.T) {
	dbtest.SkipIfShort(t)
	tdb := dbtest.New(t)
	type attrs struct {
		super, bypass, createRole, createDB, ownsSchema, readAll bool
	}
	want := map[string]attrs{
		"payrun_owner":    {ownsSchema: true},
		"payrun_app":      {},
		"payrun_readonly": {},
		"payrun_backup":   {bypass: true, readAll: true},
	}
	for role, w := range want {
		var got attrs
		err := tdb.Owner(t).QueryRow(context.Background(), `
			SELECT r.rolsuper, r.rolbypassrls, r.rolcreaterole, r.rolcreatedb,
			       EXISTS (SELECT 1 FROM pg_namespace n WHERE n.nspname = 'payrun' AND n.nspowner = r.oid),
			       pg_has_role(r.oid, 'pg_read_all_data', 'MEMBER')
			FROM pg_roles r WHERE r.rolname = $1`, role).
			Scan(&got.super, &got.bypass, &got.createRole, &got.createDB, &got.ownsSchema, &got.readAll)
		if err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		if got != w {
			t.Errorf("%s: got %+v, want %+v", role, got, w)
		}
	}
}

// Backup: BYPASSRLS + pg_read_all_data, tetapi TIDAK bisa menulis atau membuat objek. Kalau
// kredensialnya bocor, dampaknya baca (dump dienkripsi), bukan mengubah data gaji.
func TestRoles_BackupCannotWrite(t *testing.T) {
	dbtest.SkipIfShort(t)
	ctx := context.Background()
	tdb := dbtest.New(t)
	if _, err := tdb.Owner(t).Exec(ctx, `CREATE TABLE probe (id int PRIMARY KEY); INSERT INTO probe VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	backup := tdb.As(t, "payrun_backup")
	var n int
	if err := backup.QueryRow(ctx, `SELECT count(*) FROM payrun.probe`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("backup must read every table: n=%d err=%v", n, err)
	}
	for _, stmt := range []string{
		`INSERT INTO payrun.probe VALUES (2)`,
		`UPDATE payrun.probe SET id = 3`,
		`DELETE FROM payrun.probe`,
		`CREATE TABLE payrun.evil (id int)`,
	} {
		_, err := backup.Exec(ctx, stmt)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s: want 42501 insufficient_privilege, got %v", stmt, err)
		}
	}
}

// Jebakan string kosong (Step 12 C3), dibuktikan di satu koneksi yang sama (MaxConns 1, seperti
// koneksi pool yang dipakai ulang request berikutnya):
//  1. di dalam transaksi, set_config(..., true) terlihat oleh current_tenant();
//  2. setelah COMMIT, nilainya hilang (transaction-local: tidak bocor ke request berikutnya), TETAPI
//     current_setting('app.tenant_id', true) sekarang ” — bukan NULL seperti di koneksi baru;
//  3. cast mentah ”::uuid gagal 22P02, sedangkan current_tenant() mengembalikan NULL (fail closed).
func TestCurrentTenant_EmptyStringAfterLocalSetIsNull(t *testing.T) {
	dbtest.SkipIfShort(t)
	ctx := context.Background()
	pool := dbtest.New(t, dbtest.MaxConns(1)).App
	tenant := uuid.Must(uuid.NewV7())

	var raw *string
	if err := pool.QueryRow(ctx, `SELECT current_setting('app.tenant_id', true)`).Scan(&raw); err != nil || raw != nil {
		t.Fatalf("fresh connection: want NULL, got %v (err %v)", raw, err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant.String()); err != nil {
		t.Fatal(err)
	}
	var inside *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT current_tenant()`).Scan(&inside); err != nil || inside == nil || *inside != tenant {
		t.Fatalf("inside tx: want %s, got %v (err %v)", tenant, inside, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Koneksi yang sama (MaxConns 1): nilai tenant sudah hilang, tetapi meninggalkan string kosong.
	if err := pool.QueryRow(ctx, `SELECT current_setting('app.tenant_id', true)`).Scan(&raw); err != nil || raw == nil || *raw != "" {
		t.Fatalf("after commit on the same connection: want '' (empty string), got %v (err %v)", raw, err)
	}
	var after *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT current_tenant()`).Scan(&after); err != nil || after != nil {
		t.Fatalf("current_tenant() after commit: want NULL (fail closed), got %v (err %v)", after, err)
	}
	_, err = pool.Exec(ctx, `SELECT current_setting('app.tenant_id', true)::uuid`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "22P02" {
		t.Fatalf("raw cast of '' must fail with 22P02 (this is why policies use the helper), got %v", err)
	}
}

// Helper harus STABLE (bisa di-initplan lewat subselect) dan BUKAN SECURITY DEFINER.
func TestCurrentTenant_HelperIsStableAndInvoker(t *testing.T) {
	dbtest.SkipIfShort(t)
	rows, err := dbtest.New(t).App.Query(context.Background(), `
		SELECT p.proname, p.provolatile, p.prosecdef FROM pg_proc p
		WHERE p.pronamespace = 'payrun'::regnamespace AND p.proname IN ('current_tenant', 'current_user_id')`)
	if err != nil {
		t.Fatal(err)
	}
	type fn struct {
		name     string
		volatile string
		secdef   bool
	}
	fns, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (fn, error) {
		var f fn
		err := r.Scan(&f.name, &f.volatile, &f.secdef)
		return f, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fns) != 2 {
		t.Fatalf("want 2 helpers, got %v", fns)
	}
	for _, f := range fns {
		if f.volatile != "s" || f.secdef {
			t.Errorf("%s: want STABLE (s) and SECURITY INVOKER, got volatile=%s secdef=%t", f.name, f.volatile, f.secdef)
		}
	}
}

// btree_gist terpasang dan EXCLUDE bekerja untuk pola effective dating (ADR-P04): rentang setengah
// terbuka [from, to) yang bersentuhan diterima, yang tumpang tindih ditolak 23P01, dan tenant lain
// dengan employee id yang sama tidak saling mengganggu.
func TestMigrations_BtreeGistExcludeForEffectiveDating(t *testing.T) {
	dbtest.SkipIfShort(t)
	ctx := context.Background()
	owner := dbtest.New(t).Owner(t)
	if _, err := owner.Exec(ctx, `CREATE TABLE salary_probe (
		tenant_id uuid NOT NULL, employee_id uuid NOT NULL, valid daterange NOT NULL,
		CONSTRAINT ex_salary_probe_no_overlap EXCLUDE USING gist (tenant_id WITH =, employee_id WITH =, valid WITH &&))`); err != nil {
		t.Fatalf("EXCLUDE USING gist with uuid needs btree_gist: %v", err)
	}
	tA, tB, emp := uuid.New(), uuid.New(), uuid.New()
	ins := func(tenant uuid.UUID, rng string) error {
		_, err := owner.Exec(ctx, `INSERT INTO salary_probe VALUES ($1, $2, $3::daterange)`, tenant, emp, rng)
		return err
	}
	for _, ok := range []struct {
		tenant uuid.UUID
		rng    string
	}{{tA, "[2026-01-01,2026-03-15)"}, {tA, "[2026-03-15,)"}, {tB, "[2026-01-01,)"}} {
		if err := ins(ok.tenant, ok.rng); err != nil {
			t.Fatalf("%s must be accepted (adjacent half-open ranges / other tenant): %v", ok.rng, err)
		}
	}
	err := ins(tA, "[2026-03-01,2026-04-01)")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23P01" || pgErr.ConstraintName != "ex_salary_probe_no_overlap" {
		t.Fatalf("overlapping range: want 23P01 on ex_salary_probe_no_overlap, got %v", err)
	}
}

// TestRuleset_SeedMatchesEngineFixture: payload ruleset di DB = fixture yang dipakai golden test
// engine. Kalau keduanya menyimpang, golden test hijau tetapi run sungguhan menghitung angka lain.
func TestRuleset_SeedMatchesEngineFixture(t *testing.T) {
	dbtest.SkipIfShort(t)
	fixture, err := os.ReadFile("../../internal/modules/payroll/engine/testdata/ruleset_ID-SIMPLIFIED_2026.1.json")
	if err != nil {
		t.Fatal(err)
	}
	tdb := dbtest.New(t)
	var equal, shaOK bool
	if err := tdb.App.QueryRow(context.Background(), `
		SELECT payload = $1::jsonb, payload_sha256 = sha256(convert_to(payload::text, 'UTF8'))
		FROM rulesets WHERE code = 'ID-SIMPLIFIED' AND version = '2026.1'`, string(fixture)).Scan(&equal, &shaOK); err != nil {
		t.Fatal(err)
	}
	if !equal || !shaOK {
		t.Fatalf("seeded ruleset payload equal to engine fixture: %v, sha256 consistent: %v", equal, shaOK)
	}
	if _, err := tdb.Owner(t).Exec(context.Background(), `UPDATE rulesets SET label = 'x'`); err == nil {
		t.Fatal("rulesets must be immutable even for the owner (PI-12)")
	}
}
