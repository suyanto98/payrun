-- platform/jobs PayRun (disalin dari LedgerPay + tenant_id). jobs memakai RLS (migrasi 00004): tanpa
-- tenant context (claim worker, scheduler) semua baris terlihat; di transaksi tenant hanya job tenant itu.

-- name: EnqueueJob :execrows
-- Outbox: dipanggil DI DALAM transaksi bisnis. tenant_id = tenant transaksi (current_tenant(); NULL
-- untuk job platform), dan trigger jobs_enforce_tenant menolak selain itu. Dedupe selama job hidup.
-- Semua jadwal memakai jam DATABASE (now()), sama dengan acuan klaim: satu sumber waktu.
INSERT INTO jobs (id, kind, payload, tenant_id, max_attempts, run_at, dedupe_key, correlation_id, trace_parent)
VALUES (sqlc.arg(id), sqlc.arg(kind), sqlc.arg(payload), payrun.current_tenant(), sqlc.arg(max_attempts),
        now() + make_interval(secs => sqlc.arg(delay_secs)::float8), sqlc.narg(dedupe_key), sqlc.narg(correlation_id), sqlc.narg(trace_parent))
ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL AND status IN ('queued', 'running') DO NOTHING;

-- name: EnqueueScheduledJob :execrows
-- Scheduler: satu job per bucket waktu SELAMANYA (selama retensi), bukan hanya selama queued/running.
-- NOT EXISTS menutup celah "job bucket ini sudah succeeded lalu replika lain meng-enqueue lagi";
-- ON CONFLICT tetap menangani dua replika yang meng-enqueue bersamaan.
INSERT INTO jobs (id, kind, payload, max_attempts, run_at, dedupe_key)
SELECT sqlc.arg(id), sqlc.arg(kind), '{}'::jsonb, sqlc.arg(max_attempts), now(), sqlc.arg(dedupe_key)::text
WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE dedupe_key = sqlc.arg(dedupe_key)::text)
ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL AND status IN ('queued', 'running') DO NOTHING;

-- name: ClaimJobs :many
-- [none] Klaim lintas tenant (tanpa tenant context) dengan lease. Job 'running' yang lease-nya habis (worker mati) ikut diklaim ulang.
UPDATE jobs SET status = 'running', locked_by = sqlc.arg(worker),
       locked_until = now() + make_interval(secs => sqlc.arg(lease_secs)::float8), attempts = attempts + 1
WHERE id IN (
  SELECT id FROM jobs
  WHERE kind = ANY(sqlc.arg(kinds)::text[])
    AND ((status = 'queued' AND run_at <= now()) OR (status = 'running' AND locked_until < now()))
  ORDER BY run_at
  LIMIT sqlc.arg(batch)
  FOR UPDATE SKIP LOCKED)
RETURNING id, kind, payload, tenant_id, attempts, max_attempts, correlation_id, trace_parent;

-- name: CompleteJob :execrows
-- FENCING: hanya pemegang lease saat ini. 0 baris = lease sudah diambil worker lain (zombie) →
-- pemanggil me-rollback SELURUH transaksi bisnisnya.
UPDATE jobs SET status = 'succeeded', completed_at = now(), locked_until = NULL
WHERE id = sqlc.arg(id) AND locked_by = sqlc.arg(worker) AND status = 'running';

-- name: FailJob :one
-- Percobaan gagal (fenced): retry dengan backoff, atau dead (permanent / percobaan habis).
UPDATE jobs
SET status = CASE WHEN sqlc.arg(dead)::bool OR attempts >= max_attempts THEN 'dead' ELSE 'queued' END,
    run_at = now() + make_interval(secs => sqlc.arg(backoff_secs)::float8), last_error = sqlc.arg(last_error), locked_by = NULL, locked_until = NULL,
    completed_at = CASE WHEN sqlc.arg(dead)::bool OR attempts >= max_attempts THEN now() END
WHERE id = sqlc.arg(id) AND locked_by = sqlc.arg(worker) AND status = 'running'
RETURNING status;

-- name: HeartbeatJob :execrows
UPDATE jobs SET locked_until = now() + make_interval(secs => sqlc.arg(lease_secs)::float8)
WHERE id = sqlc.arg(id) AND locked_by = sqlc.arg(worker) AND status = 'running';

-- name: ReleaseJob :exec
-- Graceful shutdown: job yang dibatalkan dikembalikan ke antrean segera (percobaan tidak dihitung).
UPDATE jobs SET status = 'queued', run_at = now(), locked_by = NULL, locked_until = NULL,
       attempts = greatest(attempts - 1, 0)
WHERE id = sqlc.arg(id) AND locked_by = sqlc.arg(worker) AND status = 'running';

-- name: DeleteFinishedJobs :execrows
-- Retensi: succeeded > 7 hari, dead > 30 hari (batch, dipanggil job maintenance.jobs_cleanup).
DELETE FROM jobs WHERE id IN (
  SELECT id FROM jobs
  WHERE (status = 'succeeded' AND completed_at < now() - interval '7 days')
     OR (status = 'dead' AND completed_at < now() - interval '30 days')
  LIMIT 5000);

-- name: JobStats :many
-- Gauge periodik (15 dtk): kedalaman antrean, umur job jatuh tempo tertua, outbox lag.
SELECT kind, status, count(*)::bigint AS n,
       coalesce(extract(epoch FROM now() - min(run_at) FILTER (WHERE status = 'queued' AND run_at <= now())), 0)::float8 AS oldest_due_secs,
       coalesce(extract(epoch FROM now() - min(run_at) FILTER (WHERE status = 'queued' AND run_at <= now() AND attempts = 0)), 0)::float8 AS lag_secs
FROM jobs WHERE status IN ('queued', 'running', 'dead')
GROUP BY kind, status;
