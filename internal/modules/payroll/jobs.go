package payroll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"payrun/internal/modules/payroll/engine"
	"payrun/internal/modules/payroll/store"
	"payrun/internal/platform/db"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/jobs"
	"payrun/internal/platform/observability"
	"payrun/internal/platform/testhook"
)

// Eksekusi run (ADR-P05, Step 7 `22` §3.2):
//
//	calculate (HTTP)  periode FOR UPDATE → run CALCULATING, calc_version+1 → job payroll.plan
//	payroll.plan      run FOR UPDATE (guard versi) → eligible → K job calc_chunk (dedupe per chunk)
//	payroll.calc_chunk txRead RR READ ONLY (input) → compute (engine, di luar tx) →
//	                  txWrite RC: run FOR UPDATE (guard status+versi) → item/line/snapshot →
//	                  chunks_done+1 → chunk terakhir: total = SUM, fingerprint run → CALCULATED →
//	                  CompleteInTx (fencing: worker zombie rollback seluruh txWrite)
//
// Chunk dengan calc_version usang (recalculate, job duplikat) = no-op + complete.

const (
	KindPlan  = "payroll.plan"
	KindChunk = "payroll.calc_chunk"
)

type planPayload struct {
	RunID       uuid.UUID `json:"run_id"`
	CalcVersion int32     `json:"calc_version"`
}

type chunkPayload struct {
	RunID       uuid.UUID   `json:"run_id"`
	CalcVersion int32       `json:"calc_version"`
	ChunkNo     int         `json:"chunk_no"`
	EmployeeIDs []uuid.UUID `json:"employee_ids"`
}

// Jobs menyediakan definisi job payroll untuk runner worker.
type Jobs struct {
	pool      *pgxpool.Pool
	ids       idgen.Generator
	chunkSize int
}

func NewJobs(pool *pgxpool.Pool, ids idgen.Generator, chunkSize int) *Jobs {
	if chunkSize <= 0 {
		chunkSize = 50
	}
	return &Jobs{pool: pool, ids: ids, chunkSize: chunkSize}
}

// Kinds: payroll.plan, payroll.calc_chunk (gagal permanen → run FAILED, PFS-05), payroll.cleanup_versions.
func (j *Jobs) Kinds() []jobs.Kind {
	return []jobs.Kind{
		{Name: KindPlan, Handler: j.plan, MaxAttempts: 5, Timeout: 30 * time.Second, OnDead: j.onDead},
		{Name: KindChunk, Handler: j.chunk, MaxAttempts: 5, Timeout: 60 * time.Second, OnDead: j.onDead},
		{Name: KindCleanupVersions, Handler: j.cleanupVersions, MaxAttempts: 5, Timeout: 5 * time.Minute, Heartbeat: true},
	}
}

func (j *Jobs) plan(ctx context.Context, job *jobs.Job) error {
	if job.TenantID == uuid.Nil {
		return jobs.Permanent(errors.New("payroll.plan without tenant"))
	}
	var p planPayload
	if err := job.Decode(&p); err != nil {
		return err
	}
	return db.WithTenantTxFor(ctx, j.pool, job.TenantID, func(tx pgx.Tx) error {
		q := store.New(tx)
		run, err := q.LockRun(ctx, p.RunID)
		if errors.Is(err, pgx.ErrNoRows) {
			return jobs.Permanent(fmt.Errorf("run %s not found", p.RunID))
		}
		if err != nil {
			return err
		}
		if run.Status != "CALCULATING" || run.CalcVersion != p.CalcVersion {
			return jobs.CompleteInTx(ctx, tx, job) // versi usang / run sudah pindah status: no-op
		}
		info, err := q.GetRun(ctx, p.RunID)
		if err != nil {
			return err
		}
		from, to := monthRange(int(info.Year), int(info.Month))
		eligible, err := q.EligibleEmployees(ctx, store.EligibleEmployeesParams{FromDate: from, ToDate: to})
		if err != nil {
			return err
		}
		chunks := (len(eligible) + j.chunkSize - 1) / j.chunkSize
		if err := q.SetPlan(ctx, store.SetPlanParams{EligibleCount: ptr(int32(len(eligible))), ChunksTotal: int32(chunks), ID: p.RunID}); err != nil { //nolint:gosec // G115
			return err
		}
		if chunks == 0 { // Step 12 R1-3: run 0 karyawan langsung CALCULATED dengan total 0
			if err := j.finish(ctx, q, job.TenantID, p.RunID, p.CalcVersion, 0, info.Year, info.Month); err != nil {
				return err
			}
			return jobs.CompleteInTx(ctx, tx, job)
		}
		for i := range chunks {
			end := min((i+1)*j.chunkSize, len(eligible))
			if _, err := jobs.Enqueue(ctx, tx, jobs.Spec{Kind: KindChunk, MaxAttempts: 5, CorrelationID: job.CorrelationID,
				Dedupe:  fmt.Sprintf("calc_chunk:%s:%d:%d", p.RunID, p.CalcVersion, i+1),
				Payload: chunkPayload{RunID: p.RunID, CalcVersion: p.CalcVersion, ChunkNo: i + 1, EmployeeIDs: eligible[i*j.chunkSize : end]}}); err != nil {
				return err
			}
		}
		return jobs.CompleteInTx(ctx, tx, job)
	})
}

type computed struct {
	id     uuid.UUID
	res    engine.Result
	input  []byte
	fp     []byte
	meta   empMeta
	issues []byte
}

func (j *Jobs) chunk(ctx context.Context, job *jobs.Job) error {
	if job.TenantID == uuid.Nil {
		return jobs.Permanent(errors.New("payroll.calc_chunk without tenant"))
	}
	var p chunkPayload
	if err := job.Decode(&p); err != nil {
		return err
	}
	// txRead: satu snapshot konsisten (RR READ ONLY; 40001 tidak mungkin pada transaksi read-only).
	var data chunkData
	var stale bool
	var year, month int16
	err := db.WithTenantTxForOptions(ctx, j.pool, job.TenantID, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		q := store.New(tx)
		run, err := q.GetRun(ctx, p.RunID)
		if errors.Is(err, pgx.ErrNoRows) {
			return jobs.Permanent(fmt.Errorf("run %s not found", p.RunID))
		}
		if err != nil {
			return err
		}
		if run.Status != "CALCULATING" || run.CalcVersion != p.CalcVersion {
			stale = true
			return nil
		}
		year, month = run.Year, run.Month
		data, err = loadChunk(ctx, q, job.TenantID, int(run.Year), int(run.Month), p.EmployeeIDs)
		return err
	})
	if err != nil {
		return err
	}

	// compute: CPU saja, di luar transaksi.
	var results []computed
	if !stale {
		for _, id := range p.EmployeeIDs {
			in, ok := data.inputs[id]
			if !ok {
				continue // employee hilang di antara plan dan chunk: count ≠ eligible_count → run FAILED
			}
			raw, err := json.Marshal(in)
			if err != nil {
				return err
			}
			res := engine.Calculate(in, data.rs)
			issues := make([]map[string]string, 0, len(res.Errors)+len(res.Warnings))
			for _, is := range res.Errors {
				issues = append(issues, map[string]string{"kind": "error", "code": is.Code, "detail": is.Detail})
			}
			for _, is := range res.Warnings {
				issues = append(issues, map[string]string{"kind": "warning", "code": is.Code, "detail": is.Detail})
			}
			ib, _ := json.Marshal(issues)
			results = append(results, computed{id: id, res: res, input: raw, fp: fingerprint(raw, data.rsCode, data.rsVersion), meta: data.meta[id], issues: ib})
		}
	}
	if err := testhook.Pause(ctx, "payroll.calc_chunk.before_write"); err != nil {
		return err
	}

	// txWrite: run FOR UPDATE lebih dulu (urutan global [3] → [7], D4).
	return db.WithTenantTxFor(ctx, j.pool, job.TenantID, func(tx pgx.Tx) error {
		q := store.New(tx)
		run, err := q.LockRun(ctx, p.RunID)
		if err != nil {
			return err
		}
		if stale || run.Status != "CALCULATING" || run.CalcVersion != p.CalcVersion {
			observability.Logger(ctx).Info("payroll.chunk_stale_noop", "run_id", p.RunID.String(), "calc_version", p.CalcVersion)
			return jobs.CompleteInTx(ctx, tx, job)
		}
		if err := q.DeleteChunkItems(ctx, store.DeleteChunkItemsParams{RunID: p.RunID, CalcVersion: p.CalcVersion, EmployeeIds: p.EmployeeIDs}); err != nil {
			return err
		}
		for _, c := range results {
			if err := j.writeItem(ctx, q, job.TenantID, p.RunID, p.CalcVersion, c, data); err != nil {
				return err
			}
		}
		prog, err := q.IncrementChunksDone(ctx, p.RunID)
		if err != nil {
			return err
		}
		if prog.ChunksDone == prog.ChunksTotal {
			if err := j.finish(ctx, q, job.TenantID, p.RunID, p.CalcVersion, eligibleOf(run), year, month); err != nil {
				return err
			}
		}
		return jobs.CompleteInTx(ctx, tx, job) // fencing: lease diambil worker lain → ErrLeaseLost → rollback
	})
}

func eligibleOf(r store.LockRunRow) int32 {
	if r.EligibleCount == nil {
		return 0
	}
	return *r.EligibleCount
}

func (j *Jobs) writeItem(ctx context.Context, q *store.Queries, tid, runID uuid.UUID, ver int32, c computed, data chunkData) error {
	itemID := j.ids.New()
	status := string(c.res.Status)
	params := store.InsertItemParams{TenantID: tid, ID: itemID, RunID: runID, EmployeeID: c.id, CalcVersion: ver, Status: status,
		ErrorCodes: codes(c.res.Errors), WarningCodes: codes(c.res.Warnings), Issues: c.issues, InputFingerprint: c.fp,
		EmployeeNo: c.meta.no, EmployeeName: c.meta.name, DepartmentID: c.meta.dept}
	if len(c.res.Lines) > 0 || c.res.Status == engine.StatusOK {
		params.Gross, params.Deductions, params.Net = toNum(c.res.Gross), toNum(c.res.Deductions), toNum(c.res.Net)
	}
	if err := q.InsertItem(ctx, params); err != nil {
		return err
	}
	for i, l := range c.res.Lines {
		basis, _ := json.Marshal(l.Basis)
		if err := q.InsertLine(ctx, store.InsertLineParams{TenantID: tid, ID: j.ids.New(), RunID: runID, CalcVersion: ver, ItemID: itemID,
			SortOrder: int16(i + 1), Code: l.Code, Name: l.Name, Category: string(l.Category), Stage: string(l.Stage), //nolint:gosec // G115
			Amount: toNum(l.Amount), Basis: basis, Explanation: l.Explanation, Taxable: l.Taxable, Rounding: string(l.Rounding)}); err != nil {
			return err
		}
	}
	return q.InsertSnapshot(ctx, store.InsertSnapshotParams{TenantID: tid, ItemID: itemID, RunID: runID, CalcVersion: ver,
		SnapshotVersion: snapshotVersion, Input: c.input, RulesetCode: data.rsCode, RulesetVersion: data.rsVersion, InputFingerprint: c.fp})
}

func codes(is []engine.Issue) []string {
	out := []string{}
	for _, i := range is {
		out = append(out, i.Code)
	}
	return out
}

// finish: chunk terakhir (atau plan tanpa karyawan). Total = SUM item OK (PI-05, diverifikasi trigger
// payroll_runs_totals_check); jumlah item harus = eligible_count, kalau tidak run FAILED.
func (j *Jobs) finish(ctx context.Context, q *store.Queries, tid, runID uuid.UUID, ver, eligible int32, year, month int16) error {
	sum, err := q.RunVersionSummary(ctx, store.RunVersionSummaryParams{RunID: runID, CalcVersion: ver})
	if err != nil {
		return err
	}
	if sum.ItemCount != eligible {
		_, err := q.FailRun(ctx, store.FailRunParams{Reason: ptr(fmt.Sprintf("item count %d ≠ eligible %d", sum.ItemCount, eligible)), ID: runID, CalcVersion: ver})
		return err
	}
	fps, err := q.ItemFingerprints(ctx, store.ItemFingerprintsParams{RunID: runID, CalcVersion: ver})
	if err != nil {
		return err
	}
	from, _ := monthRange(int(year), int(month))
	set, err := q.InputSettings(ctx, tid)
	if err != nil {
		return err
	}
	rs, err := loadRuleset(ctx, q, set.RulesetCode, from)
	if err != nil {
		return err
	}
	var rsCode, rsVersion *string
	if rs.Code != "" {
		rsCode, rsVersion = &rs.Code, &rs.Version
	}
	return q.CompleteCalculation(ctx, store.CompleteCalculationParams{Gross: sum.Gross, Deductions: sum.Deductions, Net: sum.Net,
		OkCount: &sum.OkCount, ErrorCount: &sum.ErrorCount, ExcludedCount: &sum.ExcludedCount, WarningCount: &sum.WarningCount,
		Fingerprint: runFingerprint(fps), RulesetCode: rsCode, RulesetVersion: rsVersion, ID: runID, CalcVersion: ver})
}

// onDead: job gagal permanen → run FAILED (PFS-05). Transaksi runner tidak punya tenant context, jadi
// pembaruan run memakai transaksi tenant tersendiri (idempoten: guard status + versi).
func (j *Jobs) onDead(ctx context.Context, _ pgx.Tx, job *jobs.Job) error {
	if job.TenantID == uuid.Nil {
		return nil
	}
	var p planPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return nil //nolint:nilerr // payload rusak: tidak ada run yang bisa ditandai
	}
	return db.WithTenantTxFor(context.WithoutCancel(ctx), j.pool, job.TenantID, func(tx pgx.Tx) error {
		_, err := store.New(tx).FailRun(ctx, store.FailRunParams{Reason: ptr(job.Kind + " failed permanently; fix the data and recalculate"),
			ID: p.RunID, CalcVersion: p.CalcVersion})
		return err
	})
}

func ptr[T any](v T) *T { return &v }
