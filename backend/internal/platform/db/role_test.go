package db_test

import (
	"context"
	"errors"
	"testing"

	"payrun/internal/platform/db"
	"payrun/internal/platform/db/dbtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

// Startup check lolos untuk payrun_app, dan MENOLAK owner, backup (BYPASSRLS), serta superuser.
// Tanpa kasus negatif, query yang salah tulis akan selalu lolos dan memberi rasa aman palsu.
func TestCheckAppRole_AcceptsAppRejectsBypass(t *testing.T) {
	dbtest.SkipIfShort(t)
	ctx := context.Background()
	tdb := dbtest.New(t)

	rc, err := db.CheckAppRole(ctx, tdb.App)
	if err != nil || rc.Role != "payrun_app" {
		t.Fatalf("payrun_app must pass: %+v %v", rc, err)
	}
	for name, pool := range map[string]db.Beginner{
		"owner (owns schema)": tdb.Owner(t),
		"backup (BYPASSRLS)":  tdb.As(t, "payrun_backup"),
		"superuser":           tdb.Super(t),
	} {
		if _, err := db.CheckAppRole(ctx, pool); !errors.Is(err, db.ErrUnsafeRole) {
			t.Errorf("%s: want ErrUnsafeRole, got %v", name, err)
		}
	}
}
