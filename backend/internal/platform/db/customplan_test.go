package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/platform/db"
	"payrun/internal/platform/db/dbtest"
)

// CustomPlan (PR-3, S9): query lewat wrapper tidak pernah menjadi prepared statement bernama di server,
// sehingga Postgres tidak bisa beralih ke generic plan setelah 5 eksekusi. Kontrol: mode default
// (cache statement) memang meninggalkan prepared statement, jadi test ini tidak lolos secara kebetulan.
func TestCustomPlan_NeverLeavesNamedPreparedStatement(t *testing.T) {
	dbtest.SkipIfShort(t)
	ctx := context.Background()
	tdb := dbtest.New(t)
	const sqlCustom = `SELECT $1::uuid IS NULL /* custom */`
	const sqlDefault = `SELECT $1::uuid IS NULL /* default */`
	conn, err := tdb.App.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		q := db.CustomPlan(tx)
		var id *uuid.UUID
		for range 7 { // > 5: ambang plancache beralih ke generic plan
			var isNull bool
			if err := q.QueryRow(ctx, sqlCustom, id).Scan(&isNull); err != nil || !isNull {
				t.Fatalf("custom: %v %v", isNull, err)
			}
			if err := tx.QueryRow(ctx, sqlDefault, id).Scan(&isNull); err != nil {
				t.Fatalf("default: %v", err)
			}
		}
		var custom, dflt int
		// Pola dipecah ('cus' || 'tom'): query penghitung ini sendiri ter-prepare dan tidak boleh ikut terhitung.
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE statement LIKE '%/* cus' || 'tom */%'),
		                                   count(*) FILTER (WHERE statement LIKE '%/* def' || 'ault */%')
		                            FROM pg_prepared_statements WHERE NOT from_sql`).Scan(&custom, &dflt); err != nil {
			return err
		}
		if custom != 0 || dflt != 1 {
			t.Fatalf("named prepared statements: custom %d (want 0), default %d (want 1)", custom, dflt)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
