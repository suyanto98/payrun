package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// invariantChecks (DoD C2): dijalankan di t.Cleanup SETIAP test integrasi yang memakai env, sebagai superuser
// (melihat semua tenant, di luar RLS). Test yang lolos tetapi meninggalkan data yang melanggar invariant tetap
// gagal: bug yang hanya terlihat sebagai "angka aneh" di akhir alur tertangkap di test yang membuatnya.
// Invariant yang sudah dijamin constraint (unik, CHECK, FK komposit) tidak diulang di sini.
var invariantChecks = []struct{ name, sql string }{
	{"PI-05 run total = Σ OK items and one item per eligible employee", `
		SELECT r.id::text FROM payrun.payroll_runs r,
		  LATERAL (SELECT count(*) AS items, coalesce(sum(net) FILTER (WHERE status = 'OK'), 0) AS net,
		                  coalesce(sum(gross) FILTER (WHERE status = 'OK'), 0) AS gross
		           FROM payrun.payroll_items i WHERE i.run_id = r.id AND i.calc_version = r.calc_version) s
		WHERE r.status IN ('CALCULATED', 'PENDING_APPROVAL', 'APPROVED', 'FINALIZED')
		  AND (coalesce(r.total_net, 0) <> s.net OR coalesce(r.total_gross, 0) <> s.gross
		       OR s.items <> coalesce(r.eligible_count, -1))`},
	{"PI-16 every active tenant keeps an active OWNER", `
		SELECT t.slug FROM payrun.tenants t
		WHERE t.status = 'ACTIVE' AND NOT EXISTS (SELECT 1 FROM payrun.memberships m
		  WHERE m.tenant_id = t.id AND m.status = 'ACTIVE' AND 'OWNER' = ANY (m.roles))`},
	{"PI-04 a FINALIZED period points to its FINALIZED run", `
		SELECT p.id::text FROM payrun.payroll_periods p LEFT JOIN payrun.payroll_runs r ON r.id = p.finalized_run_id
		WHERE p.status = 'FINALIZED' AND (r.id IS NULL OR r.status <> 'FINALIZED' OR r.period_id <> p.id)`},
}

func (e env) checkInvariants(t *testing.T) {
	if t.Failed() {
		return // kegagalan pertama lebih informatif; data test yang gagal di tengah wajar tidak konsisten
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, e.tdb.SuperDSN())
	if err != nil {
		t.Errorf("invariant checker: %v", err)
		return
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, c := range invariantChecks {
		rows, err := conn.Query(ctx, c.sql)
		if err != nil {
			t.Errorf("invariant checker %s: %v", c.name, err)
			continue
		}
		bad, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Errorf("invariant checker %s: %v", c.name, err)
			continue
		}
		if len(bad) > 0 {
			t.Errorf("invariant violated after test: %s: %s", c.name, strings.Join(bad, ", "))
		}
	}
}
