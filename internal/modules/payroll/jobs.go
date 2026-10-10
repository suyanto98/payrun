package payroll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"

	"payrun/internal/modules/payroll/engine"
	"payrun/internal/modules/payroll/store"
	"payrun/internal/platform/clock"
	"payrun/internal/platform/db"
	"payrun/internal/platform/faults"
	"payrun/internal/platform/idgen"
	"payrun/internal/platform/jobs"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/observability"
	"payrun/internal/platform/storage"
	"payrun/internal/platform/testhook"
	"payrun/internal/platform/tracing"
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
	docs      storage.Store // payslip PDF (S7); nil = lane documents tidak dijalankan
	clock     clock.Clock
}

// WithDocuments: object storage + jam untuk job payslip.render (lane documents).
func (j *Jobs) WithDocuments(st storage.Store, c clock.Clock) *Jobs {
	j.docs, j.clock = st, c
	return j
}

// DocumentKinds: payslip.render (lane documents, ADR-012). Timeout 5 menit + heartbeat; 5 percobaan.
func (j *Jobs) DocumentKinds() []jobs.Kind {
	return []jobs.Kind{{Name: KindPayslipRender, Handler: j.renderPayslips, MaxAttempts: 5, Timeout: 5 * time.Minute, Heartbeat: true}}
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
	var done runOutcome
	err := db.WithTenantTxFor(ctx, j.pool, job.TenantID, func(tx pgx.Tx) error {
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
			if done, err = j.finish(ctx, q, job.TenantID, p.RunID, p.CalcVersion, 0, info.Year, info.Month); err != nil {
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
	if err == nil {
		done.emit()
	}
	return err
}

// runOutcome: hasil finish, dicatat ke metrik SETELAH commit (commit yang di-fence tidak terhitung).
type runOutcome struct {
	outcome  string // "", calculated, failed
	eligible int32
	duration float64
}

func (o runOutcome) emit() {
	switch o.outcome {
	case "calculated":
		metrics.PayrollRuns.WithLabelValues("calculated").Inc()
		metrics.PayrollRunDuration.WithLabelValues(metrics.SizeBucket(o.eligible)).Observe(o.duration)
	case "failed":
		metrics.PayrollRuns.WithLabelValues("failed").Inc()
	}
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
	// Span use-case di bawah span "job payroll.calc_chunk" (root baru ber-link ke request calculate, §5.3).
	ctx, span := tracing.Start(ctx, "payroll.calc_chunk", attribute.String("app.run_id", p.RunID.String()), attribute.Int("app.chunk_no", p.ChunkNo))
	defer span.End()
	start := metrics.Start()
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
	var done runOutcome
	wrote := false
	err = db.WithTenantTxFor(ctx, j.pool, job.TenantID, func(tx pgx.Tx) error {
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
		if err := j.writeChunk(ctx, q, job.TenantID, p.RunID, p.CalcVersion, results, data); err != nil {
			return err
		}
		prog, err := q.IncrementChunksDone(ctx, p.RunID)
		if err != nil {
			return err
		}
		if prog.ChunksDone == prog.ChunksTotal {
			if done, err = j.finish(ctx, q, job.TenantID, p.RunID, p.CalcVersion, eligibleOf(run), year, month); err != nil {
				return err
			}
		}
		wrote = true
		// Demo B live (build tag demo saja): worker mati di sini, item chunk ini belum commit (PFS-01).
		faults.Check(ctx, j.pool, faults.PointChunkBeforeCommit, job.TenantID)
		return jobs.CompleteInTx(ctx, tx, job) // fencing: lease diambil worker lain → ErrLeaseLost → rollback
	})
	if err != nil || !wrote {
		return err
	}
	for _, c := range results {
		metrics.ItemsCalculated.WithLabelValues(strings.ToLower(string(c.res.Status))).Inc()
		for _, is := range c.res.Errors {
			metrics.ItemErrors.WithLabelValues(is.Code).Inc()
		}
	}
	metrics.ChunkDuration.WithLabelValues().Observe(metrics.Since(start))
	done.emit()
	return nil
}

func eligibleOf(r store.LockRunRow) int32 {
	if r.EligibleCount == nil {
		return 0
	}
	return *r.EligibleCount
}

// writeChunk: item, line, dan snapshot satu chunk lewat TIGA INSERT … unnest (bukan ±7 INSERT per karyawan).
// txWrite memegang lock baris run (FOR UPDATE) selama menulis; PR-1 (S9) mengukur ±350 INSERT per chunk 50
// = ±165 ms lock per chunk → keempat worker antre di LockRun (rata-rata 215 ms menunggu). COPY tidak bisa:
// Postgres menolak COPY FROM ke tabel ber-RLS untuk role tanpa BYPASSRLS. Trigger baris (immutable) tetap
// berjalan; trigger jumlah item (constraint, deferred) diperiksa saat COMMIT.
func (j *Jobs) writeChunk(ctx context.Context, q *store.Queries, tid, runID uuid.UUID, ver int32, results []computed, data chunkData) error {
	if len(results) == 0 {
		return nil
	}
	it := store.InsertItemsBatchParams{TenantID: tid, RunID: runID, CalcVersion: ver}
	ln := store.InsertLinesBatchParams{TenantID: tid, RunID: runID, CalcVersion: ver}
	sn := store.InsertSnapshotsBatchParams{TenantID: tid, RunID: runID, CalcVersion: ver, SnapshotVersion: snapshotVersion,
		RulesetCode: data.rsCode, RulesetVersion: data.rsVersion}
	for _, c := range results {
		itemID := j.ids.New()
		var gross, ded, net pgtype.Numeric // NULL untuk item ERROR tanpa baris
		if len(c.res.Lines) > 0 || c.res.Status == engine.StatusOK {
			gross, ded, net = toNum(c.res.Gross), toNum(c.res.Deductions), toNum(c.res.Net)
		}
		dept := uuid.Nil
		if c.meta.dept != nil {
			dept = *c.meta.dept
		}
		it.Ids, it.EmployeeIds, it.Statuses = append(it.Ids, itemID), append(it.EmployeeIds, c.id), append(it.Statuses, string(c.res.Status))
		it.Gross, it.Deductions, it.Net = append(it.Gross, gross), append(it.Deductions, ded), append(it.Net, net)
		it.Errs, it.Warns = append(it.Errs, strings.Join(codes(c.res.Errors), ",")), append(it.Warns, strings.Join(codes(c.res.Warnings), ","))
		it.Issues, it.Fps = append(it.Issues, c.issues), append(it.Fps, c.fp)
		it.Nos, it.Names, it.Depts = append(it.Nos, c.meta.no), append(it.Names, c.meta.name), append(it.Depts, dept)
		for i, l := range c.res.Lines {
			basis, _ := json.Marshal(l.Basis)
			adj := uuid.Nil
			if l.Code == "ADJUSTMENT" {
				id, err := uuid.Parse(l.Basis["adjustment_id"])
				if err != nil {
					return fmt.Errorf("adjustment line without id: %w", err)
				}
				adj = id
			}
			ln.Ids, ln.ItemIds, ln.SortOrders = append(ln.Ids, j.ids.New()), append(ln.ItemIds, itemID), append(ln.SortOrders, int16(i+1)) //nolint:gosec // G115
			ln.Codes, ln.Names, ln.Categories = append(ln.Codes, l.Code), append(ln.Names, l.Name), append(ln.Categories, string(l.Category))
			ln.Stages, ln.Amounts, ln.Bases = append(ln.Stages, string(l.Stage)), append(ln.Amounts, toNum(l.Amount)), append(ln.Bases, basis)
			ln.Explanations, ln.Taxables = append(ln.Explanations, l.Explanation), append(ln.Taxables, l.Taxable)
			ln.Roundings, ln.Adjs = append(ln.Roundings, string(l.Rounding)), append(ln.Adjs, adj)
		}
		sn.ItemIds, sn.Inputs, sn.Fps = append(sn.ItemIds, itemID), append(sn.Inputs, c.input), append(sn.Fps, c.fp)
	}
	if err := q.InsertItemsBatch(ctx, it); err != nil {
		return err
	}
	if len(ln.Ids) > 0 {
		if err := q.InsertLinesBatch(ctx, ln); err != nil {
			return err
		}
	}
	return q.InsertSnapshotsBatch(ctx, sn)
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
func (j *Jobs) finish(ctx context.Context, q *store.Queries, tid, runID uuid.UUID, ver, eligible int32, year, month int16) (runOutcome, error) {
	sum, err := q.RunVersionSummary(ctx, store.RunVersionSummaryParams{RunID: runID, CalcVersion: ver})
	if err != nil {
		return runOutcome{}, err
	}
	if sum.ItemCount != eligible {
		n, err := q.FailRun(ctx, store.FailRunParams{Reason: ptr(fmt.Sprintf("item count %d ≠ eligible %d", sum.ItemCount, eligible)), ID: runID, CalcVersion: ver})
		if err != nil || n == 0 {
			return runOutcome{}, err
		}
		return runOutcome{outcome: "failed"}, nil
	}
	fps, err := q.ItemFingerprints(ctx, store.ItemFingerprintsParams{RunID: runID, CalcVersion: ver})
	if err != nil {
		return runOutcome{}, err
	}
	from, _ := monthRange(int(year), int(month))
	set, err := q.InputSettings(ctx, tid)
	if err != nil {
		return runOutcome{}, err
	}
	rs, err := loadRuleset(ctx, q, set.RulesetCode, from)
	if err != nil {
		return runOutcome{}, err
	}
	var rsCode, rsVersion *string
	if rs.Code != "" {
		rsCode, rsVersion = &rs.Code, &rs.Version
	}
	secs, err := q.CompleteCalculation(ctx, store.CompleteCalculationParams{Gross: sum.Gross, Deductions: sum.Deductions, Net: sum.Net,
		OkCount: &sum.OkCount, ErrorCount: &sum.ErrorCount, ExcludedCount: &sum.ExcludedCount, WarningCount: &sum.WarningCount,
		Fingerprint: runFingerprint(fps), RulesetCode: rsCode, RulesetVersion: rsVersion, ID: runID, CalcVersion: ver})
	if errors.Is(err, pgx.ErrNoRows) { // status/versi sudah berubah: tidak ada transisi
		return runOutcome{}, nil
	}
	if err != nil {
		return runOutcome{}, err
	}
	return runOutcome{outcome: "calculated", eligible: eligible, duration: secs}, nil
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
	failed := false
	err := db.WithTenantTxFor(context.WithoutCancel(ctx), j.pool, job.TenantID, func(tx pgx.Tx) error {
		n, err := store.New(tx).FailRun(ctx, store.FailRunParams{Reason: ptr(job.Kind + " failed permanently; fix the data and recalculate"),
			ID: p.RunID, CalcVersion: p.CalcVersion})
		failed = n > 0
		return err
	})
	if err == nil && failed {
		runOutcome{outcome: "failed"}.emit()
	}
	return err
}

func ptr[T any](v T) *T { return &v }
