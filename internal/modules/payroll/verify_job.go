package payroll

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"

	"payrun/internal/platform/jobs"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/observability"
)

// KindVerifyRun: verifikasi run setelah finalize (DoD C3). Tanpa job ini metrik INV reproducibility dan
// total mismatch hanya hidup di proses CLI `payrun verify-run`, yang tidak pernah di-scrape Prometheus:
// "0" di dashboard berarti "tidak pernah diukur", bukan "sehat".
const KindVerifyRun = "payroll.verify_run"

// totalMismatch: run (per proses worker) yang verifikasi terakhirnya menemukan selisih total. Sumber gauge
// payrun_payroll_total_mismatch_runs: verifikasi ulang yang bersih menghapus run dari himpunan.
var totalMismatch = struct {
	sync.Mutex
	runs map[uuid.UUID]bool
}{runs: map[uuid.UUID]bool{}}

func setTotalMismatch(run uuid.UUID, bad bool) {
	totalMismatch.Lock()
	defer totalMismatch.Unlock()
	if bad {
		totalMismatch.runs[run] = true
	} else {
		delete(totalMismatch.runs, run)
	}
	metrics.PayrollTotalMismatchRuns.WithLabelValues().Set(float64(len(totalMismatch.runs)))
}

// verifyRun: VerifyRun (read-only, di bawah RLS tenant job). Perbedaan BUKAN error job (retry tidak
// mengubah data yang tersimpan): job selesai, metrik INV naik, log error memuat rinciannya.
func (j *Jobs) verifyRun(ctx context.Context, job *jobs.Job) error {
	if job.TenantID == uuid.Nil {
		return jobs.Permanent(errors.New("payroll.verify_run without tenant"))
	}
	var p planPayload
	if err := job.Decode(&p); err != nil {
		return err
	}
	rep, err := (&Service{pool: j.pool}).VerifyRun(ctx, job.TenantID, p.RunID)
	if err != nil {
		return err
	}
	setTotalMismatch(p.RunID, !rep.Totals.OK)
	log := observability.Logger(ctx)
	if !rep.OK() {
		log.Error("payroll.verify_failed", "run_id", p.RunID.String(), "items_checked", rep.ItemsChecked,
			"mismatches", len(rep.Mismatches), "totals_ok", rep.Totals.OK, "items", rep.Totals.Items, "eligible", rep.Totals.Eligible)
		return nil
	}
	log.Info("payroll.verify_ok", "run_id", p.RunID.String(), "items_checked", rep.ItemsChecked)
	return nil
}
