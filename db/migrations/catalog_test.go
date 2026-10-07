package migrations_test

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/internal/platform/db/dbtest"
)

// Test katalog RLS (docs/design/13-db-rls-consistency-indexing.md §2.5). Setiap query punya test
// negatif ("test the test"): objek pelanggar dibuat dengan sengaja dan query harus menangkapnya.
// Tanpa itu, query katalog yang salah tulis selalu lolos dan memberi rasa aman palsu.

// Allowlist §2.2: tabel tanpa RLS yang disengaja, masing-masing dengan penjaga pengganti.
var tablesWithoutRLS = []string{"rulesets", "refresh_tokens", "platform_idempotency_keys", "email_verifications", "goose_db_version"}

func strings1(t *testing.T, pool *pgxpool.Pool, sql string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatal(err)
	}
}

// (a) Setiap tabel di luar allowlist: ENABLE + FORCE RLS + minimal satu policy.
const qTablesWithoutRLS = `
	SELECT c.relname FROM pg_class c
	WHERE c.relnamespace = 'payrun'::regnamespace AND c.relkind IN ('r', 'p')
	  AND c.relname <> ALL ($1)
	  AND (NOT c.relrowsecurity OR NOT c.relforcerowsecurity
	       OR NOT EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid))
	ORDER BY 1`

func tablesMissingRLS(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), qTablesWithoutRLS, tablesWithoutRLS)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCatalog_EveryTableHasForcedRLS(t *testing.T) {
	dbtest.SkipIfShort(t)
	if missing := tablesMissingRLS(t, dbtest.New(t).Owner(t)); len(missing) > 0 {
		t.Fatalf("tabel tanpa ENABLE+FORCE RLS+policy (di luar allowlist §2.2): %v", missing)
	}
}

func TestCatalog_RLSCheckDetectsViolations(t *testing.T) {
	dbtest.SkipIfShort(t)
	owner := dbtest.New(t).Owner(t)
	exec(t, owner, `CREATE TABLE probe_no_rls (tenant_id uuid);
		CREATE TABLE probe_not_forced (tenant_id uuid); ALTER TABLE probe_not_forced ENABLE ROW LEVEL SECURITY;
		CREATE POLICY p ON probe_not_forced USING (tenant_id = (SELECT current_tenant()));
		CREATE TABLE probe_no_policy (tenant_id uuid);
		ALTER TABLE probe_no_policy ENABLE ROW LEVEL SECURITY; ALTER TABLE probe_no_policy FORCE ROW LEVEL SECURITY`)
	got := tablesMissingRLS(t, owner)
	for _, want := range []string{"probe_no_rls", "probe_not_forced", "probe_no_policy"} {
		if !slices.Contains(got, want) {
			t.Errorf("query katalog (a) tidak menangkap %s; hasil %v", want, got)
		}
	}
}

// (b) FK ke tabel ber-tenant_id harus menyertakan tenant_id (composite FK, PI-02): FK tunggal ke
// (id) membuat baris tenant A bisa menunjuk baris tenant B. FK ke tenants(id) sendiri dikecualikan.
const qFKWithoutTenant = `
	SELECT con.conname FROM pg_constraint con
	JOIN pg_class ref ON ref.oid = con.confrelid AND ref.relnamespace = 'payrun'::regnamespace
	WHERE con.contype = 'f'
	  AND EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = ref.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped)
	  AND NOT EXISTS (SELECT 1 FROM unnest(con.confkey) k
	                  JOIN pg_attribute a ON a.attrelid = ref.oid AND a.attnum = k
	                  WHERE a.attname = 'tenant_id')
	ORDER BY 1`

func TestCatalogue_TenantFKsComposite(t *testing.T) {
	dbtest.SkipIfShort(t)
	if bad := strings1(t, dbtest.New(t).Owner(t), qFKWithoutTenant); len(bad) > 0 {
		t.Fatalf("FK ke tabel tenant-scoped tanpa tenant_id: %v", bad)
	}
}

func TestCatalogue_TenantFKCheckDetectsViolation(t *testing.T) {
	dbtest.SkipIfShort(t)
	owner := dbtest.New(t).Owner(t)
	exec(t, owner, `CREATE TABLE probe_parent (tenant_id uuid, id uuid PRIMARY KEY);
		CREATE TABLE probe_child (tenant_id uuid, parent_id uuid CONSTRAINT fk_probe_child_parent REFERENCES probe_parent (id))`)
	if got := strings1(t, owner, qFKWithoutTenant); !slices.Contains(got, "fk_probe_child_parent") {
		t.Fatalf("query katalog (b) tidak menangkap FK tanpa tenant_id; hasil %v", got)
	}
}

// (c) SECURITY DEFINER hanya auth_lookup_user (06-multi-tenancy §2.6): fungsi definer berjalan
// sebagai owner dan bisa melewati batas yang dijaga RLS.
const qSecurityDefiner = `
	SELECT p.proname FROM pg_proc p
	WHERE p.pronamespace = 'payrun'::regnamespace AND p.prosecdef AND p.proname <> 'auth_lookup_user'
	ORDER BY 1`

func TestCatalog_SecurityDefinerAllowlist(t *testing.T) {
	dbtest.SkipIfShort(t)
	owner := dbtest.New(t).Owner(t)
	if bad := strings1(t, owner, qSecurityDefiner); len(bad) > 0 {
		t.Fatalf("SECURITY DEFINER di luar allowlist: %v", bad)
	}
	// auth_lookup_user wajib mengunci search_path (cegah hijacking).
	cfg := strings1(t, owner, `SELECT coalesce(array_to_string(proconfig, ','), '') FROM pg_proc
		WHERE pronamespace = 'payrun'::regnamespace AND proname = 'auth_lookup_user'`)
	if len(cfg) != 1 || !strings.Contains(cfg[0], "search_path=payrun, pg_temp") {
		t.Fatalf("auth_lookup_user must SET search_path = payrun, pg_temp; got %v", cfg)
	}
	exec(t, owner, `CREATE FUNCTION probe_definer() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'`)
	if got := strings1(t, owner, qSecurityDefiner); !slices.Contains(got, "probe_definer") {
		t.Fatalf("query katalog (c) tidak menangkap SECURITY DEFINER baru; hasil %v", got)
	}
}

// (d) View wajib security_invoker (view biasa berjalan dengan hak owner → melewati RLS).
const qViewsWithoutInvoker = `
	SELECT c.relname FROM pg_class c
	WHERE c.relnamespace = 'payrun'::regnamespace AND c.relkind = 'v'
	  AND NOT coalesce('security_invoker=true' = ANY (c.reloptions), false)
	ORDER BY 1`

func TestCatalog_ViewsAreSecurityInvoker(t *testing.T) {
	dbtest.SkipIfShort(t)
	owner := dbtest.New(t).Owner(t)
	if bad := strings1(t, owner, qViewsWithoutInvoker); len(bad) > 0 {
		t.Fatalf("view tanpa security_invoker: %v", bad)
	}
	exec(t, owner, `CREATE VIEW probe_view AS SELECT id FROM tenants`)
	if got := strings1(t, owner, qViewsWithoutInvoker); !slices.Contains(got, "probe_view") {
		t.Fatalf("query katalog (d) tidak menangkap view tanpa security_invoker; hasil %v", got)
	}
}

// (e) Step 12 C3: policy tidak boleh memakai current_setting( mentah (”::uuid → error acak per koneksi).
const qRawCurrentSetting = `
	SELECT tablename || '.' || policyname FROM pg_policies
	WHERE schemaname = 'payrun'
	  AND (coalesce(qual, '') LIKE '%current_setting(%' OR coalesce(with_check, '') LIKE '%current_setting(%')
	ORDER BY 1`

func TestCatalog_NoRawCurrentSettingInPolicies(t *testing.T) {
	dbtest.SkipIfShort(t)
	owner := dbtest.New(t).Owner(t)
	if bad := strings1(t, owner, qRawCurrentSetting); len(bad) > 0 {
		t.Fatalf("policy dengan current_setting mentah (pakai current_tenant()/current_user_id()): %v", bad)
	}
	exec(t, owner, `CREATE TABLE probe_raw (tenant_id uuid);
		CREATE POLICY raw ON probe_raw USING (tenant_id = current_setting('app.tenant_id', true)::uuid)`)
	if got := strings1(t, owner, qRawCurrentSetting); !slices.Contains(got, "probe_raw.raw") {
		t.Fatalf("query katalog (e) tidak menangkap current_setting mentah; hasil %v", got)
	}
}

// Matriks grant payrun_app (§1.2) dibandingkan PERSIS dengan katalog: tabel baru tanpa entri di sini,
// atau grant yang lebih luas dari desain (contoh DELETE di tabel bisnis), membuat CI gagal.
var appGrantMatrix = map[string]string{
	"goose_db_version":          "SELECT",
	"users":                     "INSERT,UPDATE", // SELECT per kolom, tanpa password_hash (dicek terpisah)
	"tenants":                   "INSERT,SELECT,UPDATE",
	"tenant_settings":           "INSERT,SELECT,UPDATE",
	"memberships":               "INSERT,SELECT,UPDATE",
	"refresh_tokens":            "INSERT,SELECT,UPDATE",
	"email_verifications":       "INSERT,SELECT,UPDATE",
	"idempotency_keys":          "DELETE,INSERT,SELECT,UPDATE",
	"platform_idempotency_keys": "DELETE,INSERT,SELECT,UPDATE",
	"jobs":                      "DELETE,INSERT,SELECT,UPDATE",
	"audit_log":                 "INSERT,SELECT",
}

func TestCatalog_AppGrantsMatchMatrix(t *testing.T) {
	dbtest.SkipIfShort(t)
	owner := dbtest.New(t).Owner(t)
	rows, err := owner.Query(context.Background(), `
		SELECT c.relname, coalesce(string_agg(p.privilege_type, ',' ORDER BY p.privilege_type), '')
		FROM pg_class c
		LEFT JOIN information_schema.role_table_grants p
		  ON p.table_schema = 'payrun' AND p.table_name = c.relname AND p.grantee = 'payrun_app'
		WHERE c.relnamespace = 'payrun'::regnamespace AND c.relkind IN ('r', 'p', 'v')
		GROUP BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var table, privs string
		if err := rows.Scan(&table, &privs); err != nil {
			t.Fatal(err)
		}
		got[table] = privs
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var problems []string
	for table, privs := range got {
		want, ok := appGrantMatrix[table]
		switch {
		case !ok:
			problems = append(problems, table+": tidak ada di matriks grant (tambahkan setelah review desain §1.2)")
		case privs != want:
			problems = append(problems, table+": grant "+privs+", matriks "+want)
		}
	}
	for table := range appGrantMatrix {
		if _, ok := got[table]; !ok {
			problems = append(problems, table+": ada di matriks tetapi tabelnya tidak ada")
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("grant payrun_app tidak sesuai matriks:\n  %s", strings.Join(problems, "\n  "))
	}

	cols := strings1(t, owner, `SELECT column_name FROM information_schema.column_privileges
		WHERE table_schema = 'payrun' AND table_name = 'users' AND grantee = 'payrun_app' AND privilege_type = 'SELECT'
		ORDER BY 1`)
	if slices.Contains(cols, "password_hash") || !slices.Contains(cols, "email") {
		t.Fatalf("users SELECT columns for payrun_app: want email but never password_hash, got %v", cols)
	}
}
