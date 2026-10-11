package migrations_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/db/migrations"
	"payrun/internal/platform/db"
	"payrun/internal/platform/db/dbtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

// Migrasi harus bisa dijalankan dari database kosong (bukan hanya dari template), dan
// menjalankannya ulang harus no-op (idempoten) — itulah yang terjadi di setiap deploy.
func TestMigrations_UpFromZero(t *testing.T) {
	dbtest.SkipIfShort(t)
	ctx := context.Background()
	_, ownerDSN := dbtest.NewEmpty(t)

	if err := migrations.MigrateUp(ctx, ownerDSN); err != nil {
		t.Fatalf("migrate up from zero: %v", err)
	}
	if err := migrations.MigrateUp(ctx, ownerDSN); err != nil {
		t.Fatalf("second migrate up should be a no-op: %v", err)
	}
	pool, err := pgxpool.New(ctx, ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	v, err := migrations.SchemaVersion(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if v < migrations.MinSchemaVersion {
		t.Fatalf("schema version %d < MinSchemaVersion %d", v, migrations.MinSchemaVersion)
	}
}

// final review C4: setiap FK harus punya index (non-partial) yang diawali kolom FK-nya. Tanpa itu,
// DELETE/UPDATE di tabel induk memicu seq scan di tabel anak untuk SETIAP baris.
func TestCatalog_EveryFKHasIndex(t *testing.T) {
	dbtest.SkipIfShort(t)
	tdb := dbtest.New(t)
	if missing := fkWithoutIndex(t, tdb.Owner(t)); len(missing) > 0 {
		t.Fatalf("FK tanpa index yang diawali kolom FK-nya: %s", strings.Join(missing, ", "))
	}
}

// "Test the test": query katalog harus BENAR-BENAR menangkap FK tanpa index. Tanpa test negatif
// ini, query yang salah tulis akan selalu lolos dan memberi rasa aman palsu.
func TestCatalog_FKIndexCheckDetectsMissingIndex(t *testing.T) {
	dbtest.SkipIfShort(t)
	tdb := dbtest.New(t)
	owner := tdb.Owner(t)
	if _, err := owner.Exec(context.Background(),
		`CREATE TABLE fk_parent (id uuid PRIMARY KEY);
		 CREATE TABLE fk_probe (id uuid PRIMARY KEY, parent_id uuid CONSTRAINT fk_probe_parent_id REFERENCES fk_parent (id))`); err != nil {
		t.Fatal(err)
	}
	if missing := fkWithoutIndex(t, owner); !slices.Contains(missing, "fk_probe.fk_probe_parent_id") {
		t.Fatalf("query katalog gagal mendeteksi FK tanpa index; hasil: %v", missing)
	}
}

func fkWithoutIndex(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT c.conrelid::regclass::text AS tbl, c.conname
		FROM pg_constraint c
		WHERE c.contype = 'f'
		  AND c.connamespace = 'payrun'::regnamespace
		  AND NOT EXISTS (
		    SELECT 1 FROM pg_index i
		    WHERE i.indrelid = c.conrelid
		      AND i.indpred IS NULL                                        -- bukan partial index
		      AND (i.indkey::int2[])[0:array_length(c.conkey, 1) - 1] = c.conkey::int2[]
		  )`)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var tbl, con string
		err := r.Scan(&tbl, &con)
		return tbl + "." + con, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return missing
}

// Setiap nama constraint di pemetaan error (platform/db) harus benar-benar ada di skema.
// Test inilah yang menangkap selisih seperti `uq_users_email` vs `uq_users_email_lower`.
func TestDBErrorMapping_AllCatalogueConstraints(t *testing.T) {
	dbtest.SkipIfShort(t)
	tdb := dbtest.New(t)
	rows, err := tdb.Owner(t).Query(context.Background(), `
		SELECT conname FROM pg_constraint WHERE connamespace = 'payrun'::regnamespace
		UNION
		SELECT indexrelid::regclass::text FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relnamespace = 'payrun'::regnamespace`)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range db.ConstraintNames() {
		if !slices.Contains(existing, name) {
			t.Errorf("constraint %q dipetakan di platform/db tetapi tidak ada di skema", name)
		}
	}
}
