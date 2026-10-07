package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/internal/platform/db"
	"payrun/internal/platform/db/dbtest"
	"payrun/internal/platform/tenantctx"
)

// Isolation harness Sprint 1 (docs/design/13-db-rls-consistency-indexing.md §2, 06-multi-tenancy.md §3).
// Semua test terhubung sebagai payrun_app (NOBYPASSRLS, bukan owner): RLS yang "terlihat jalan" karena
// test berjalan sebagai superuser adalah risiko utama Sprint 1 (roadmap S1).

type fixture struct {
	alpha, beta uuid.UUID // tenant
	dewi        uuid.UUID // OWNER Alpha
	budi        uuid.UUID // OWNER Beta
	konsultan   uuid.UUID // anggota Alpha DAN Beta (user multi-tenant, kasus C2)
}

func newID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

// createUser memakai jalur produksi: fase platform (app.user_id = user baru) lolos WITH CHECK users_self.
func createUser(t *testing.T, pool *pgxpool.Pool, email string) uuid.UUID {
	t.Helper()
	id := newID()
	err := db.WithPlatformTx(context.Background(), pool, id, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`INSERT INTO users (id, email, password_hash, display_name) VALUES ($1, $2, 'x', $2)`, id, email)
		return err
	})
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return id
}

// createTenant: tenant + settings + membership OWNER di satu transaksi tenant (pola signup).
func createTenant(t *testing.T, pool *pgxpool.Pool, slug string, owner uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := newID()
	err := db.WithTenantTxFor(ctx, pool, id, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1, $2, $2)`, id, slug); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO tenant_settings (tenant_id) VALUES ($1)`, id); err != nil {
			return err
		}
		return addMember(ctx, tx, id, owner, "OWNER")
	})
	if err != nil {
		t.Fatalf("create tenant %s: %v", slug, err)
	}
	return id
}

func addMember(ctx context.Context, tx pgx.Tx, tenant, user uuid.UUID, role string) error {
	_, err := tx.Exec(ctx, `INSERT INTO memberships (tenant_id, id, user_id, roles, status, accepted_at)
		VALUES ($1, $2, $3, ARRAY[$4], 'ACTIVE', now())`, tenant, newID(), user, role)
	return err
}

func seed(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	var f fixture
	f.dewi = createUser(t, pool, "dewi@alpha.test")
	f.budi = createUser(t, pool, "budi@beta.test")
	f.konsultan = createUser(t, pool, "konsultan@payroll.test")
	f.alpha = createTenant(t, pool, "alpha", f.dewi)
	f.beta = createTenant(t, pool, "beta", f.budi)
	for _, tid := range []uuid.UUID{f.alpha, f.beta} {
		if err := db.WithTenantTxFor(context.Background(), pool, tid, func(tx pgx.Tx) error {
			return addMember(context.Background(), tx, tid, f.konsultan, "PAYROLL_ADMIN")
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// count menjalankan query hitung di dalam fn transaksi.
func count(t *testing.T, tx pgx.Tx, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func wantPgCode(t *testing.T, err error, code, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("%s: want SQLSTATE %s, got %v", what, code, err)
	}
}

// Tanpa tenant context (pool mentah) setiap tabel ber-RLS mengembalikan 0 baris: fail closed.
func TestRLS_NoContextReturnsZeroRows(t *testing.T) {
	dbtest.SkipIfShort(t)
	tdb := dbtest.New(t)
	seed(t, tdb.App)
	for _, table := range []string{"tenants", "tenant_settings", "memberships", "users", "audit_log", "idempotency_keys"} {
		var n int
		if err := tdb.App.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s without tenant context: want 0 rows, got %d (err %v)", table, n, err)
		}
	}
}

// Query TANPA filter tenant (seperti bug "lupa WHERE tenant_id") di dalam WithTenantTx hanya melihat
// tenant aktif. Inilah lapisan terakhir yang menyelamatkan kalau query aplikasi salah tulis.
func TestRLS_UnfilteredQuerySeesOnlyActiveTenant(t *testing.T) {
	dbtest.SkipIfShort(t)
	tdb := dbtest.New(t)
	f := seed(t, tdb.App)
	err := db.WithTenantTxFor(context.Background(), tdb.App, f.alpha, func(tx pgx.Tx) error {
		if n := count(t, tx, `SELECT count(*) FROM tenants`); n != 1 {
			t.Errorf("tenants: want 1 (Alpha), got %d", n)
		}
		if n := count(t, tx, `SELECT count(*) FROM memberships WHERE tenant_id <> $1`, f.alpha); n != 0 {
			t.Errorf("memberships of other tenants visible: %d", n)
		}
		if n := count(t, tx, `SELECT count(*) FROM tenant_settings`); n != 1 {
			t.Errorf("tenant_settings: want 1, got %d", n)
		}
		// users: hanya anggota ACTIVE Alpha (Dewi + konsultan), bukan Budi.
		if n := count(t, tx, `SELECT count(*) FROM users`); n != 2 {
			t.Errorf("users: want 2 Alpha members, got %d", n)
		}
		if n := count(t, tx, `SELECT count(*) FROM users WHERE id = $1`, f.budi); n != 0 {
			t.Errorf("Beta's owner visible from Alpha")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// WITH CHECK: dari dalam tenant Alpha tidak bisa menulis baris milik Beta (42501), termasuk lewat
// UPDATE yang memindahkan baris ke tenant lain.
func TestRLS_WithCheckRejectsWritesToOtherTenant(t *testing.T) {
	dbtest.SkipIfShort(t)
	ctx := context.Background()
	tdb := dbtest.New(t)
	f := seed(t, tdb.App)
	err := db.WithTenantTxFor(ctx, tdb.App, f.alpha, func(tx pgx.Tx) error {
		return addMember(ctx, tx, f.beta, f.dewi, "OWNER")
	})
	wantPgCode(t, err, "42501", "insert membership into Beta from Alpha")

	err = db.WithTenantTxFor(ctx, tdb.App, f.alpha, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenant_settings SET tenant_id = $1`, f.beta)
		return err
	})
	wantPgCode(t, err, "42501", "move tenant_settings row to Beta")

	err = db.WithTenantTxFor(ctx, tdb.App, f.alpha, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1, 'gamma', 'gamma')`, newID())
		return err
	})
	wantPgCode(t, err, "42501", "create a tenant other than the context tenant")
}

// Koneksi pool dipakai ulang: setelah transaksi tenant COMMIT, request berikutnya di koneksi yang
// SAMA (MaxConns 1) tanpa tenant context tidak mewarisi tenant lama (set_config is_local = true).
func TestRLS_PoolLeakageAfterCommit(t *testing.T) {
	dbtest.SkipIfShort(t)
	ctx := context.Background()
	tdb := dbtest.New(t, dbtest.MaxConns(1))
	f := seed(t, tdb.App)
	if err := db.WithTenantTxFor(ctx, tdb.App, f.alpha, func(tx pgx.Tx) error {
		if n := count(t, tx, `SELECT count(*) FROM memberships`); n != 2 {
			t.Errorf("inside Alpha: want 2 memberships, got %d", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tdb.App.QueryRow(ctx, `SELECT count(*) FROM memberships`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("same connection after commit: want 0 rows, got %d (err %v)", n, err)
	}
	// Juga setelah ROLLBACK (error di tengah transaksi).
	_ = db.WithTenantTxFor(ctx, tdb.App, f.beta, func(pgx.Tx) error { return errors.New("boom") })
	if err := tdb.App.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("same connection after rollback: want 0 rows, got %d (err %v)", n, err)
	}
}

// Step 12 C2: policy self hanya aktif di mode platform. User multi-tenant di dalam tenant Alpha TIDAK
// melihat membership-nya di Beta (kebocoran metadata), sedangkan di mode platform ia melihat keduanya.
func TestRLS_SelfPoliciesInactiveInTenantMode(t *testing.T) {
	dbtest.SkipIfShort(t)
	ctx := context.Background()
	tdb := dbtest.New(t)
	f := seed(t, tdb.App)

	if err := db.WithPlatformTx(ctx, tdb.App, f.konsultan, func(tx pgx.Tx) error {
		if n := count(t, tx, `SELECT count(*) FROM memberships`); n != 2 {
			t.Errorf("platform mode: want own 2 memberships, got %d", n)
		}
		if n := count(t, tx, `SELECT count(*) FROM tenants`); n != 2 {
			t.Errorf("platform mode: want 2 tenants the user belongs to, got %d", n)
		}
		if n := count(t, tx, `SELECT count(*) FROM users`); n != 1 {
			t.Errorf("platform mode: want only self in users, got %d", n)
		}
		if n := count(t, tx, `SELECT count(*) FROM tenant_settings`); n != 0 {
			t.Errorf("platform mode must not see tenant data, got %d tenant_settings", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Konteks tenant Alpha untuk transaksi yang SEKALIGUS punya app.user_id (seharusnya mustahil lewat
	// helper, tetapi policy tidak boleh bergantung pada helper): self tetap mati.
	if err := db.WithTenantTxFor(ctx, tdb.App, f.alpha, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.user_id', $1, true)`, f.konsultan.String()); err != nil {
			return err
		}
		if n := count(t, tx, `SELECT count(*) FROM memberships WHERE user_id = $1`, f.konsultan); n != 1 {
			t.Errorf("tenant mode: konsultan must see only the Alpha membership, got %d", n)
		}
		if n := count(t, tx, `SELECT count(*) FROM tenants`); n != 1 {
			t.Errorf("tenant mode: want only Alpha in tenants, got %d", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Mode platform hanya bisa MEMBACA membership sendiri, tidak menulis (memberships_self_read FOR SELECT).
func TestRLS_PlatformModeCannotWriteMemberships(t *testing.T) {
	dbtest.SkipIfShort(t)
	ctx := context.Background()
	tdb := dbtest.New(t)
	f := seed(t, tdb.App)
	err := db.WithPlatformTx(ctx, tdb.App, f.konsultan, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE memberships SET roles = ARRAY['OWNER'] WHERE user_id = $1`, f.konsultan)
		if err == nil && tag.RowsAffected() != 0 {
			t.Errorf("platform mode escalated own role in %d memberships", tag.RowsAffected())
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = db.WithPlatformTx(ctx, tdb.App, f.konsultan, func(tx pgx.Tx) error {
		return addMember(ctx, tx, f.alpha, f.konsultan, "OWNER")
	})
	wantPgCode(t, err, "42501", "platform mode inserting a membership")
}

// Step 12 R2-7: password_hash tidak terbaca payrun_app di jalur mana pun; hanya lewat auth_lookup_user.
func TestRLS_PasswordHashOnlyViaAuthLookup(t *testing.T) {
	dbtest.SkipIfShort(t)
	ctx := context.Background()
	tdb := dbtest.New(t)
	f := seed(t, tdb.App)
	err := db.WithPlatformTx(ctx, tdb.App, f.dewi, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT password_hash FROM users WHERE id = $1`, f.dewi)
		return err
	})
	wantPgCode(t, err, "42501", "select password_hash as payrun_app")

	// auth_lookup_user dipanggil TANPA context (login belum tahu user): case-insensitive, satu baris.
	var id uuid.UUID
	var hash string
	if err := tdb.App.QueryRow(ctx, `SELECT id, password_hash FROM auth_lookup_user($1)`, "DEWI@Alpha.Test").Scan(&id, &hash); err != nil || id != f.dewi || hash != "x" {
		t.Fatalf("auth_lookup_user: got %s %q %v", id, hash, err)
	}
	if err := tdb.App.QueryRow(ctx, `SELECT id FROM auth_lookup_user($1)`, "nobody@x.test").Scan(&id); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unknown email: want no rows, got %v", err)
	}
}

// jobs tanpa RLS dijaga trigger: tenant A tidak bisa meng-enqueue job tenant B, dan job tenant tidak
// bisa dibuat di luar transaksi tenant. Job platform (tenant_id NULL) dibuat tanpa context.
func TestJobs_EnforceTenantMatchesContext(t *testing.T) {
	dbtest.SkipIfShort(t)
	ctx := context.Background()
	tdb := dbtest.New(t)
	f := seed(t, tdb.App)
	insert := func(q interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	}, tenant *uuid.UUID) error {
		_, err := q.Exec(ctx, `INSERT INTO jobs (id, kind, tenant_id) VALUES ($1, 'test.probe', $2)`, newID(), tenant)
		return err
	}
	if err := db.WithTenantTxFor(ctx, tdb.App, f.alpha, func(tx pgx.Tx) error { return insert(tx, &f.alpha) }); err != nil {
		t.Fatalf("own tenant job: %v", err)
	}
	if err := insert(tdb.App, nil); err != nil {
		t.Fatalf("platform job without context: %v", err)
	}
	for name, run := range map[string]func() error{
		"Alpha enqueues Beta job": func() error {
			return db.WithTenantTxFor(ctx, tdb.App, f.alpha, func(tx pgx.Tx) error { return insert(tx, &f.beta) })
		},
		"Alpha enqueues platform job": func() error {
			return db.WithTenantTxFor(ctx, tdb.App, f.alpha, func(tx pgx.Tx) error { return insert(tx, nil) })
		},
		"tenant job outside tenant tx": func() error { return insert(tdb.App, &f.alpha) },
	} {
		err := run()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "ck_jobs_tenant_matches_context" {
			t.Errorf("%s: want 23514 ck_jobs_tenant_matches_context, got %v", name, err)
		}
	}
}

// Review S1: trigger INSERT saja tidak cukup. Transaksi tenant tidak boleh membaca, mengubah, menghapus,
// atau memindahkan job tenant lain; tenant_id immutable di mode apa pun (worker menjalankan job di
// tenant jobs.tenant_id); payrun_readonly tidak membaca payload.
func TestJobs_TenantIsolationAfterInsert(t *testing.T) {
	dbtest.SkipIfShort(t)
	ctx := context.Background()
	tdb := dbtest.New(t)
	f := seed(t, tdb.App)
	alphaJob, betaJob := newID(), newID()
	for tenant, id := range map[uuid.UUID]uuid.UUID{f.alpha: alphaJob, f.beta: betaJob} {
		if err := db.WithTenantTxFor(ctx, tdb.App, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO jobs (id, kind, tenant_id, payload) VALUES ($1, 'test.probe', $2, '{"secret":1}')`, id, tenant)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	err := db.WithTenantTxFor(ctx, tdb.App, f.alpha, func(tx pgx.Tx) error {
		if n := count(t, tx, `SELECT count(*) FROM jobs`); n != 1 {
			t.Errorf("Alpha sees %d jobs, want 1", n)
		}
		for name, sql := range map[string]string{
			"update payload": `UPDATE jobs SET payload = '{}' WHERE id = $1`,
			"delete":         `DELETE FROM jobs WHERE id = $1`,
		} {
			tag, err := tx.Exec(ctx, sql, betaJob)
			if err != nil || tag.RowsAffected() != 0 {
				t.Errorf("Alpha %s Beta job: rows=%d err=%v, want 0 rows", name, tag.RowsAffected(), err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Memindahkan job sendiri ke tenant lain: ditolak trigger immutability.
	err = db.WithTenantTxFor(ctx, tdb.App, f.alpha, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE jobs SET tenant_id = $1 WHERE id = $2`, f.beta, alphaJob)
		return err
	})
	wantPgCode(t, err, "23514", "Alpha moves own job to Beta")
	// Tanpa context (jalur worker) semua job terlihat, tetapi tenant_id tetap immutable.
	var pgErr *pgconn.PgError
	_, err = tdb.App.Exec(ctx, `UPDATE jobs SET tenant_id = $1 WHERE id = $2`, f.alpha, betaJob)
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "ck_jobs_tenant_immutable" {
		t.Fatalf("worker path: want ck_jobs_tenant_immutable, got %v", err)
	}
	ro := tdb.As(t, "payrun_readonly")
	if _, err := ro.Exec(ctx, `SELECT payload FROM jobs`); err == nil {
		t.Fatal("payrun_readonly must not read jobs.payload")
	}
	if _, err := ro.Exec(ctx, `SELECT id, kind, status FROM jobs`); err != nil {
		t.Fatalf("payrun_readonly metadata: %v", err)
	}
}

// PI-16: owner terakhir tidak bisa dicabut (deferred → gagal saat COMMIT, kode P0004), tetapi transfer
// kepemilikan di satu transaksi (tambah owner baru, cabut yang lama) lolos.
func TestMemberships_LastOwnerGuard(t *testing.T) {
	dbtest.SkipIfShort(t)
	ctx := context.Background()
	tdb := dbtest.New(t)
	f := seed(t, tdb.App)
	revokeOwner := func(tx pgx.Tx, user uuid.UUID) error {
		_, err := tx.Exec(ctx, `UPDATE memberships SET status = 'REVOKED', revoked_at = now() WHERE user_id = $1`, user)
		return err
	}
	err := db.WithTenantTxFor(ctx, tdb.App, f.alpha, func(tx pgx.Tx) error { return revokeOwner(tx, f.dewi) })
	wantPgCode(t, err, "P0004", "revoke the only OWNER")

	err = db.WithTenantTxFor(ctx, tdb.App, f.alpha, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE memberships SET roles = ARRAY['OWNER'] WHERE user_id = $1`, f.konsultan); err != nil {
			return err
		}
		return revokeOwner(tx, f.dewi)
	})
	if err != nil {
		t.Fatalf("ownership transfer in one transaction must pass: %v", err)
	}
}

// audit_log: baris platform (tenant_id NULL) hanya boleh ditulis tanpa tenant context, tidak terlihat
// dari tenant mana pun, dan hanya dibaca payrun_readonly (Step 12 R2-3).
func TestAudit_PlatformRowsIsolation(t *testing.T) {
	dbtest.SkipIfShort(t)
	ctx := context.Background()
	tdb := dbtest.New(t)
	f := seed(t, tdb.App)
	ins := `INSERT INTO audit_log (id, tenant_id, actor_type, action) VALUES ($1, $2, 'SYSTEM', 'auth.login_failed')`
	if _, err := tdb.App.Exec(ctx, ins, newID(), nil); err != nil {
		t.Fatalf("platform audit row without context: %v", err)
	}
	err := db.WithTenantTxFor(ctx, tdb.App, f.alpha, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, ins, newID(), nil)
		return err
	})
	wantPgCode(t, err, "42501", "platform audit row from inside a tenant")
	if err := db.WithTenantTxFor(ctx, tdb.App, f.alpha, func(tx pgx.Tx) error {
		if n := count(t, tx, `SELECT count(*) FROM audit_log WHERE tenant_id IS NULL`); n != 0 {
			t.Errorf("tenant sees %d platform audit rows", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tdb.As(t, "payrun_readonly").QueryRow(ctx, `SELECT count(*) FROM payrun.audit_log WHERE tenant_id IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("payrun_readonly: want 1 platform audit row, got %d (err %v)", n, err)
	}
	_, err = tdb.App.Exec(ctx, `UPDATE audit_log SET action = 'x.y'`)
	wantPgCode(t, err, "42501", "update audit_log (append-only)")
}

// WithTenantTx tanpa tenant di context gagal SEBELUM menyentuh DB (fail closed, tidak ada fallback).
func TestWithTenantTx_FailsClosedWithoutTenant(t *testing.T) {
	dbtest.SkipIfShort(t)
	tdb := dbtest.New(t)
	called := false
	err := db.WithTenantTx(context.Background(), tdb.App, func(pgx.Tx) error { called = true; return nil })
	if !errors.Is(err, db.ErrNoTenantContext) || called {
		t.Fatalf("want ErrNoTenantContext without calling fn, got %v (called=%t)", err, called)
	}
	f := seed(t, tdb.App)
	ctx := tenantctx.With(context.Background(), f.beta)
	if err := db.WithTenantTx(ctx, tdb.App, func(tx pgx.Tx) error {
		var slug string
		if err := tx.QueryRow(ctx, `SELECT slug FROM tenants`).Scan(&slug); err != nil || slug != "beta" {
			t.Errorf("tenant from context: want beta, got %q (%v)", slug, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
