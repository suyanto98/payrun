package payroll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"payrun/internal/modules/payroll/store"
	"payrun/internal/platform/apperr"
	"payrun/internal/platform/audit"
	"payrun/internal/platform/auth"
	"payrun/internal/platform/db"
	"payrun/internal/platform/jobs"
	"payrun/internal/platform/metrics"
	"payrun/internal/platform/reqmeta"
	"payrun/internal/platform/tenantctx"
	"payrun/internal/platform/testhook"
)

// Finalize (Y11, PF-14, ADR-P07, `22-tx-concurrency.md` §3.4) di transaksi idempotency:
//
//	periode FOR UPDATE [2] → run FOR UPDATE [3] (APPROVED) → role pemanggil dari DB → cek stale ULANG
//	(input bisa berubah di antara approve dan finalize, §4.c) → run FINALIZED (trigger total PI-05) →
//	periode FINALIZED (PeriodGuard + trigger PI-14 aktif penuh) → job payroll.cleanup_versions → audit.
//
// Snapshot sudah ditulis saat kalkulasi dan sejak COMMIT dibekukan trigger PI-04. Di transaksi yang
// sama: adjustment di snapshot item OK → APPLIED (PI-15), payslip PENDING + job payslip.render (S7).

const KindCleanupVersions = "payroll.cleanup_versions"

type cleanupPayload struct {
	RunID       uuid.UUID `json:"run_id"`
	KeepVersion int32     `json:"keep_version"`
}

// FinalizeTx: dipanggil handler di dalam idempotency.Execute (replay = respons yang sama).
func (s *Service) FinalizeTx(ctx context.Context, tx pgx.Tx, actor, runID uuid.UUID, meta reqmeta.Meta) (Run, error) {
	tid, _ := tenantctx.From(ctx)
	q := store.New(tx)
	info, err := q.GetRun(ctx, runID)
	if err != nil {
		return Run{}, db.MapError(err)
	}
	if err := db.GuardTenant(ctx, "payroll_runs", info.TenantID); err != nil {
		return Run{}, err
	}
	if _, err := q.LockPeriod(ctx, info.PeriodID); err != nil { // [2]
		return Run{}, db.MapError(err)
	}
	if _, err := q.LockRun(ctx, runID); err != nil { // [3]
		return Run{}, db.MapError(err)
	}
	roles, err := s.members.ActiveRolesTx(ctx, tx, actor)
	if err != nil {
		return Run{}, err
	}
	if !(auth.Principal{Roles: roles}).HasAnyRole(runners...) {
		return Run{}, apperr.ErrForbidden.WithDetail("Only OWNER or PAYROLL_ADMIN can finalize a run")
	}
	run, err := q.GetRun(ctx, runID)
	if err != nil {
		return Run{}, db.MapError(err)
	}
	switch run.Status {
	case "APPROVED":
	case "FINALIZED":
		return Run{}, apperr.ErrRunFinalized
	default:
		return Run{}, apperr.ErrInvalidState.WithDetail("Only an APPROVED run can be finalized (status " + run.Status + ")")
	}
	stale, changed, err := s.staleEmployees(ctx, q, tid, run)
	if err != nil {
		return Run{}, err
	}
	if err := staleVerdict("finalize", stale, changed); err != nil {
		return Run{}, err
	}
	now := s.clock.Now()
	if n, err := q.FinalizeRun(ctx, store.FinalizeRunParams{Actor: &actor, At: &now, ID: runID}); err != nil {
		return Run{}, db.MapError(err)
	} else if n != 1 {
		return Run{}, apperr.ErrInvalidState
	}
	if err := q.FinalizePeriod(ctx, store.FinalizePeriodParams{FinalRun: &runID, At: &now, ID: run.PeriodID}); err != nil {
		return Run{}, db.MapError(err)
	}
	applied, err := q.ApplyAdjustments(ctx, store.ApplyAdjustmentsParams{RunID: &runID, At: &now, CalcVersion: run.CalcVersion})
	if err != nil {
		return Run{}, db.MapError(err)
	}
	slips, err := createPayslips(ctx, q, tx, s.ids.New, tid, runID, run.CalcVersion, meta.RequestID)
	if err != nil {
		return Run{}, db.MapError(err)
	}
	if run.CalcVersion > 1 {
		if _, err := jobs.Enqueue(ctx, tx, jobs.Spec{Kind: KindCleanupVersions, MaxAttempts: 5, CorrelationID: meta.RequestID,
			Dedupe: fmt.Sprintf("cleanup_versions:%s", runID), Payload: cleanupPayload{RunID: runID, KeepVersion: run.CalcVersion}}); err != nil {
			return Run{}, err
		}
	}
	// PI-05/PI-12 setelah final: verifikasi di worker (metrik INV hidup di proses yang di-scrape), satu kali per run.
	if _, err := jobs.Enqueue(ctx, tx, jobs.Spec{Kind: KindVerifyRun, MaxAttempts: 3, CorrelationID: meta.RequestID,
		Dedupe: fmt.Sprintf("verify_run:%s", runID), Payload: planPayload{RunID: runID, CalcVersion: run.CalcVersion}}); err != nil {
		return Run{}, err
	}
	if err := audit.Record(ctx, tx, s.ids.New(), audit.Entry{TenantID: tid, ActorID: actor, Action: "payroll_run.finalized",
		TargetType: "payroll_run", TargetID: runID, Diff: map[string]any{"calc_version": run.CalcVersion, "candidates": changed, "adjustments_applied": applied, "payslips": slips}}, meta); err != nil {
		return Run{}, err
	}
	// Titik hook test FS-03 di level layanan: semua efek sudah ditulis, COMMIT belum.
	if err := testhook.Pause(ctx, "payroll.finalize.before_commit"); err != nil {
		return Run{}, err
	}
	r, err := s.loadRun(ctx, q, runID)
	if err == nil {
		metrics.PayrollRuns.WithLabelValues("finalized").Inc() // replay idempotency tidak sampai sini
	}
	return r, db.MapError(err)
}

// staleVerdict: jumlah kandidat ke histogram; stale → 409 RUN_STALE (+ counter per aksi).
func staleVerdict(action string, stale []apperr.StaleEmployee, changed int) error {
	metrics.StaleCandidates.WithLabelValues().Observe(float64(changed))
	if changed > maxStaleCandidates || len(stale) > 0 {
		metrics.StaleRejections.WithLabelValues(action).Inc()
		return apperr.ErrRunStale.WithStale(stale, changed)
	}
	return nil
}

// cleanupVersions: job payroll.cleanup_versions. Menghapus item (+ line, snapshot) versi lama per batch
// 1.000 sampai habis; idempoten (hapus yang tersisa saja).
func (j *Jobs) cleanupVersions(ctx context.Context, job *jobs.Job) error {
	if job.TenantID == uuid.Nil {
		return jobs.Permanent(errors.New("payroll.cleanup_versions without tenant"))
	}
	var p cleanupPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return jobs.Permanent(err)
	}
	return db.WithTenantTxFor(ctx, j.pool, job.TenantID, func(tx pgx.Tx) error {
		q := store.New(tx)
		for {
			n, err := q.DeleteOldVersionItems(ctx, store.DeleteOldVersionItemsParams{RunID: p.RunID, KeepVersion: p.KeepVersion})
			if err != nil {
				return err
			}
			if n == 0 {
				break
			}
		}
		return jobs.CompleteInTx(ctx, tx, job)
	})
}
