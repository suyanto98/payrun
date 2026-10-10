package payroll

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/modules/payroll/store"
	"payrun/internal/platform/db"
)

// Eksperimen E-2 (docs/design/26-performance-plan.md §6, PR-4): biaya approve kalau stale detection
// menghitung ulang fingerprint SEMUA karyawan, bukan hanya kandidat change log (O12). Alat ukur saja
// (`payrun perf-recheck`): satu snapshot REPEATABLE READ READ ONLY, tanpa lock periode, tanpa efek.

type RecheckReport struct {
	Employees int           `json:"employees"`
	Changed   int           `json:"changed"`
	Duration  time.Duration `json:"duration_ns"`
}

const recheckBatch = 500

// FullRecheck: input setiap karyawan eligible dibangun ulang (InputProvider yang sama dengan kalkulasi)
// dan dibandingkan dengan fingerprint item versi terkini.
func (s *Service) FullRecheck(ctx context.Context, tenantID, runID uuid.UUID) (RecheckReport, error) {
	start := time.Now() //nolint:forbidigo // durasi teknis eksperimen
	var rep RecheckReport
	err := db.WithTenantTxForOptions(ctx, s.pool, tenantID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		q := store.New(tx)
		run, err := q.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		from, to := monthRange(int(run.Year), int(run.Month))
		ids, err := q.EligibleEmployees(ctx, store.EligibleEmployeesParams{FromDate: from, ToDate: to})
		if err != nil {
			return err
		}
		for i := 0; i < len(ids); i += recheckBatch {
			batch := ids[i:min(i+recheckBatch, len(ids))]
			items, err := q.CandidateItems(ctx, store.CandidateItemsParams{RunID: run.ID, CalcVersion: run.CalcVersion, Ids: batch})
			if err != nil {
				return err
			}
			fp := make(map[uuid.UUID][]byte, len(items))
			for _, it := range items {
				fp[it.EmployeeID] = it.InputFingerprint
			}
			data, err := loadChunk(ctx, q, tenantID, int(run.Year), int(run.Month), batch)
			if err != nil {
				return err
			}
			for _, id := range batch {
				raw, err := json.Marshal(data.inputs[id])
				if err != nil {
					return err
				}
				if string(fingerprint(raw, data.rsCode, data.rsVersion)) != string(fp[id]) {
					rep.Changed++
				}
			}
			rep.Employees += len(batch)
		}
		return nil
	})
	rep.Duration = time.Since(start) //nolint:forbidigo // durasi teknis eksperimen
	return rep, db.MapError(err)
}
