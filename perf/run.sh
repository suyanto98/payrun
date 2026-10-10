#!/usr/bin/env bash
# Menjalankan satu skenario load test PayRun terhadap perf env lokal (make perf-up + perf-seed) dan menyimpan
# bukti ke perf/results/<tanggal>-<sha>/<skenario>/ (docs/design/26-performance-plan.md §5–§8):
#   perf/run.sh pr1 [tenant-index]      run besar; PR_PERF_WORKERS=1|2|4 PR_PERF_CHUNK=25|50|100
#   perf/run.sh pr3                     baca 100.000 karyawan dengan RLS (k6; VUS, DURATION; RATE=iterasi/dtk → open model)
#   perf/run.sh pr4                     E-2: approve dengan 0/10/200/1.000 perubahan vs recompute penuh
# Per run: env mesin, sampel CPU/RAM container, top pg_stat_statements, cek invariant. Invariant gagal → exit 1.
set -euo pipefail
cd "$(dirname "$0")/.."
SCENARIO=${1:?usage: perf/run.sh pr1 [tenant-index]|pr3|pr4}
SHA=$(git rev-parse --short HEAD)
TAG=$SCENARIO
[ "$SCENARIO" = pr1 ] && TAG="pr1-t${2:-0}-w${PR_PERF_WORKERS:-4}-c${PR_PERF_CHUNK:-50}"
[ "$SCENARIO" = pr3 ] && [ -n "${RATE:-}" ] && TAG="pr3-rate${RATE}"
OUT=perf/results/$(date -u +%F)-$SHA/$TAG
mkdir -p "$OUT"
PERF=(env PR_HTTP_PORT=8091 PR_OPS_PORT=9290 PR_WORKER_OPS_PORT=9291 PR_PG_PORT=55443 PR_MAILPIT_UI_PORT=8035 PR_S3_PORT=8343
  docker compose -p payrun-perf -f deploy/compose.yml -f deploy/perf/compose.perf.yml)
PG=payrun-perf-postgres-1
API=http://localhost:8091/api/v1
PW='correct horse battery'
sql() { docker exec "$PG" psql -U postgres -d payrun -tAX -c "SET search_path = payrun, public; $1" | tail -n +2; }
log() { printf '[perf %s] %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$OUT/run.log"; }
fail() { log "INVARIANT FAILED: $*"; exit 1; }
login() { curl -fsS -X POST "$API/auth/login" -H 'Content-Type: application/json' -d "{\"email\":\"$1\",\"password\":\"$PW\"}" | jq -r .access_token; }
call() { local m=$1 p=$2 t=$3; shift 3; curl -sS -X "$m" "$API$p" -H "Authorization: Bearer $t" -H 'Content-Type: application/json' "$@"; }
uuid() { cat /proc/sys/kernel/random/uuid; }

# Sampel CPU/RAM bertimestamp (≈ tiap 2 dtk).
(
  while true; do
    docker stats --no-stream --format '{{.Name}},{{.CPUPerc}},{{.MemUsage}}' payrun-perf-api-1 "$PG" payrun-perf-worker-1 \
      2>/dev/null | sed "s/^/$(date +%s),/"
  done
) > "$OUT/docker-stats.csv" & STATS=$!
# Ringkasan k6 mentah berisi token login (setup_data): selalu dihapus, juga saat run diputus (Ctrl-C).
trap 'pkill -P $STATS 2>/dev/null || true; kill $STATS 2>/dev/null || true; rm -f "$OUT/k6-summary.raw.json"' EXIT
sql "SELECT pg_stat_statements_reset()" >/dev/null
{
  echo "git $SHA ($(git log -1 --format=%cI)) — working tree may contain uncommitted changes: $(git status --porcelain | wc -l) files"
  echo "machine: $(lscpu | sed -n 's/^Model name: *//p') / $(nproc) threads / $(free -h | awk '/Mem/{print $2}') RAM (shared with desktop apps + k6 + dev stack)"
  echo "postgres: $(docker exec "$PG" postgres --version); shared_buffers=1GB work_mem=16MB (deploy/perf/compose.perf.yml)"
  echo "dataset: $(sql "SELECT count(*) FROM tenants") tenants, $(sql "SELECT count(*) FROM employees") employees, $(sql "SELECT count(*) FROM attendance_records") attendance rows"
} > "$OUT/env.txt"
log "scenario $TAG → $OUT"

top_queries() {
  sql "SELECT round(total_exec_time)::int AS total_ms, calls, round(mean_exec_time::numeric, 2) AS mean_ms, left(regexp_replace(query, '\s+', ' ', 'g'), 160)
       FROM pg_stat_statements WHERE query NOT ILIKE '%pg_stat_statements%' ORDER BY total_exec_time DESC LIMIT 15" > "$OUT/top-queries.txt"
}

# calc_run <payroll-token> <run-id>: calculate → tunggu CALCULATED; cetak durasi DB (calc_started_at → calculated_at).
calc_run() {
  local tok=$1 run=$2 st
  call POST "/payroll-runs/$run/calculate" "$tok" -H "Idempotency-Key: $(uuid)" -o /dev/null
  for _ in $(seq 1 1800); do
    st=$(call GET "/payroll-runs/$run" "$tok" | jq -r .status)
    case "$st" in CALCULATED) break ;; FAILED) fail "run $run FAILED" ;; esac
    sleep 0.5
  done
  [ "$st" = CALCULATED ] || fail "run $run not CALCULATED after 15 min"
}

ensure_run() { # <token> <period> → id run aktif (buat kalau belum ada)
  local tok=$1 period=$2 r
  r=$(call POST /payroll-runs "$tok" -H "Idempotency-Key: $(uuid)" -d "{\"period\":\"$period\"}")
  echo "$r" | jq -r '.id // .existing_resource_id'
}

check_totals() { # <run> : Σ item OK = total run, satu item per eligible
  local run=$1
  local row
  row=$(sql "SELECT r.item_ok_count + r.item_error_count + r.item_excluded_count, r.eligible_count,
                    r.total_net = (SELECT coalesce(sum(net), 0) FROM payroll_items i WHERE i.run_id = r.id AND i.calc_version = r.calc_version AND i.status = 'OK'),
                    (SELECT count(*) FROM payroll_items i WHERE i.run_id = r.id AND i.calc_version = r.calc_version),
                    (SELECT count(DISTINCT employee_id) FROM payroll_items i WHERE i.run_id = r.id AND i.calc_version = r.calc_version)
             FROM payroll_runs r WHERE r.id = '$run'")
  IFS='|' read -r counted eligible sum_ok items distinct <<< "$row"
  [ "$counted" = "$eligible" ] && [ "$items" = "$eligible" ] && [ "$distinct" = "$eligible" ] && [ "$sum_ok" = t ] \
    || fail "run $run: counts $counted / eligible $eligible / items $items / distinct $distinct / Σ=total $sum_ok"
  echo "$eligible"
}

case "$SCENARIO" in
  pr1)
    IDX=${2:-0}
    log "worker: concurrency ${PR_PERF_WORKERS:-4}, chunk ${PR_PERF_CHUNK:-50}"
    PR_PERF_WORKERS=${PR_PERF_WORKERS:-4} PR_PERF_CHUNK=${PR_PERF_CHUNK:-50} "${PERF[@]}" up -d --wait worker >/dev/null 2>&1
    TOK=$(login "payroll-$IDX@seed.payrun.test")
    RUN=$(ensure_run "$TOK" 2026-09)
    before=$(curl -fsS http://localhost:9291/metrics | awk '/^payrun_payroll_chunk_duration_seconds_(sum|count) /{print $1"="$2}' | tr '\n' ' ')
    for rep in 1 2 3; do
      calc_run "$TOK" "$RUN"
      n=$(check_totals "$RUN")
      secs=$(sql "SELECT round(extract(epoch FROM calculated_at - calc_started_at)::numeric, 2) FROM payroll_runs WHERE id = '$RUN'")
      chunks=$(sql "SELECT chunks_total FROM payroll_runs WHERE id = '$RUN'")
      log "rep $rep: $n employees, $chunks chunks, calc_started_at → CALCULATED ${secs}s ($(awk "BEGIN{printf \"%.0f\", $n/$secs}") employees/s)"
    done
    after=$(curl -fsS http://localhost:9291/metrics | awk '/^payrun_payroll_chunk_duration_seconds_(sum|count) /{print $1"="$2}' | tr '\n' ' ')
    log "chunk duration metric before: $before | after: $after"
    ;;
  pr3)
    sql "SELECT json_agg(json_build_object('owner', 'owner-' || substr(t.slug, 6) || '@seed.payrun.test', 'size',
           (SELECT count(*) FROM employees e WHERE e.tenant_id = t.id),
           'depts', (SELECT json_agg(d.id) FROM departments d WHERE d.tenant_id = t.id)))
         FROM tenants t WHERE t.slug LIKE 'seed-%'" > "$OUT/tenants.json"
    docker run --rm --network host -u "$(id -u)" -v "$PWD/perf:/perf" -e BASE_URL="$API" -e TENANTS_FILE="/perf/${OUT#perf/}/tenants.json" \
      -e VUS="${VUS:-300}" -e DURATION="${DURATION:-10m}" -e RATE="${RATE:-0}" grafana/k6:1.2.3 run --quiet \
      --summary-export "/perf/${OUT#perf/}/k6-summary.raw.json" /perf/k6/pr3_read.js 2>&1 | tee "$OUT/k6.txt" || k6fail=1
    python3 - "$OUT/k6-summary.raw.json" "$OUT/k6-summary.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1])); d.pop("setup_data", None); json.dump(d, open(sys.argv[2], "w"), indent=1)  # token tidak boleh ter-commit
PY
    leaks=$(jq -r '.metrics.cross_tenant_rows.count // 0' "$OUT/k6-summary.json")
    log "cross-tenant rows seen by VUs: $leaks"
    [ "$leaks" = 0 ] || fail "$leaks rows of another tenant returned"
    if [ -n "${k6fail:-}" ]; then top_queries; log "k6 thresholds FAILED (see k6.txt)"; exit 1; fi
    ;;
  pr4)
    OWNER=$(login owner-0@seed.payrun.test); PAY=$(login payroll-0@seed.payrun.test)
    TID=$(sql "SELECT id FROM tenants WHERE slug = 'seed-0'")
    RUN=$(ensure_run "$PAY" 2026-10)
    st=$(call GET "/payroll-runs/$RUN" "$PAY" | jq -r .status)
    if [ "$st" = PENDING_APPROVAL ]; then # sisa run sebelumnya: kembalikan ke CALCULATED
      ap=$(call GET "/payroll-runs/$RUN" "$PAY" | jq -r .approval_request_id)
      call POST "/approvals/$ap/reject" "$OWNER" -d '{"note":"perf reset"}' -o /dev/null
    fi
    calc_run "$PAY" "$RUN"; N=$(check_totals "$RUN")
    log "run $RUN CALCULATED with $N employees"
    approve_times() { # <label> <reps> → p50/p95 durasi approve (ms) + kode respons
      local label=$1 reps=$2 t codes=() times=()
      for _ in $(seq 1 "$reps"); do
        read -r code t < <(curl -sS -o "$OUT/approve-body.json" -w '%{http_code} %{time_total}\n' -X POST "$API/approvals/$AP/approve" \
          -H "Authorization: Bearer $OWNER" -H 'Content-Type: application/json' -d '{}')
        codes+=("$code:$(jq -r '.code // .approval.status // "?"' "$OUT/approve-body.json")"); times+=("$t")
      done
      printf '%s\n' "${times[@]}" | sort -n | awk -v l="$label" -v c="$(printf '%s\n' "${codes[@]}" | sort | uniq -c | tr '\n' ' ')" \
        '{a[NR]=$1*1000} END{p50=a[int((NR+1)/2)]; p95=a[int(NR*0.95+0.999)]; printf "%s: n=%d p50=%.1fms p95=%.1fms max=%.1fms responses=[%s]\n", l, NR, p50, p95, a[NR], c}' \
        | tee -a "$OUT/run.log"
    }
    salary_change() { # <count> <offset>: kenaikan gaji karyawan ke-offset… lewat API (PeriodGuard mencatat perubahan)
      local ids
      ids=$(sql "SELECT id FROM employees WHERE tenant_id = '$TID' ORDER BY employee_no OFFSET $2 LIMIT $1")
      for id in $ids; do
        call POST "/employees/$id/salary-changes" "$OWNER" -H "Idempotency-Key: $(uuid)" \
          -d '{"base_salary":"9999000.00","valid_from":"2026-10-15"}' -o /dev/null -w '%{http_code}\n'
      done | sort | uniq -c | tr '\n' ' '
    }
    AP=$(call POST "/payroll-runs/$RUN/submit" "$PAY" -d '{}' | jq -r .approval_request_id)
    log "submitted, approval $AP"
    log "changes 10: $(salary_change 10 0)";    approve_times "E-2 10 changes (change log)" 20
    log "changes +190: $(salary_change 190 10)"; approve_times "E-2 200 changes (change log, max verified)" 20
    log "changes +800: $(salary_change 800 200)"; approve_times "E-2 1000 changes (> 200 → RUN_STALE without verification)" 20
    for rep in 1 2 3; do
      r=$("${PERF[@]}" --profile seed run --rm tools perf-recheck "$TID" "$RUN" 2>/dev/null | tail -1)
      log "E-2 full recompute (all employees) rep $rep: $r"
    done
    call POST "/approvals/$AP/reject" "$OWNER" -d '{"note":"perf: recalculate for 0 changes"}' -o /dev/null
    calc_run "$PAY" "$RUN"; check_totals "$RUN" >/dev/null
    AP=$(call POST "/payroll-runs/$RUN/submit" "$PAY" -d '{}' | jq -r .approval_request_id)
    approve_times "E-2 0 changes (approve succeeds; one shot, state changes)" 1
    st=$(call GET "/payroll-runs/$RUN" "$PAY" | jq -r .status)
    [ "$st" = APPROVED ] || fail "run not APPROVED after 0-change approve (status $st)"
    ;;
  *) echo "unknown scenario $SCENARIO"; exit 2 ;;
esac
top_queries
log "done → $OUT"
