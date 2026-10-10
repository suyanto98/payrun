#!/usr/bin/env bash
# Demo live PayRun A–C terhadap stack demo lokal (make up-demo && make seed-demo; docs/demo-script.md).
# Setiap janji demo diasersi: kalau tidak terpenuhi, skrip berhenti dengan pesan "DEMO FAILED".
#   scripts/demo-live.sh a|b|c|all
# Demo C memakai run dari Demo B (jalankan b dulu, atau all). Data demo dipakai sekali: ulangi dengan
# make demo-down && make up-demo && make seed-demo.
set -euo pipefail
cd "$(dirname "$0")/.."
API=${PR_DEMO_API:-http://localhost:8082/api/v1}
TOKEN_DEMO=${PR_DEMO_TOKEN:-local-demo-token-not-a-secret}
PW='correct horse battery' # app.SeedPassword
DC=(env PR_HTTP_PORT=8082 PR_OPS_PORT=9192 PR_WORKER_OPS_PORT=9193 PR_PG_PORT=55435 PR_MAILPIT_UI_PORT=8026 PR_S3_PORT=8335
  docker compose -p payrun-demo -f deploy/compose.yml -f deploy/compose.demo.yml)
PERIOD=2026-09

say()  { printf '\n\033[1m▶ %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✔\033[0m %s\n' "$*"; }
fail() { printf '  \033[31m✘ DEMO FAILED: %s\033[0m\n' "$*"; exit 1; }
uuid() { cat /proc/sys/kernel/random/uuid; }
login() { curl -fsS -X POST "$API/auth/login" -H 'Content-Type: application/json' -d "{\"email\":\"$1\",\"password\":\"$PW\"}" | jq -r .access_token; }
# call <method> <path> <token> [curl args...] → $BODY dan $OUT_CODE (bukan subshell, supaya keduanya terlihat).
call() {
  local m=$1 p=$2 t=$3 out; shift 3
  out=$(curl -sS -X "$m" "$API$p" -H "Authorization: Bearer $t" -H 'Content-Type: application/json' -w '\n%{http_code}' "$@")
  OUT_CODE=${out##*$'\n'}; BODY=${out%$'\n'*}
}
expect() { # <kode HTTP> <deskripsi> ; memakai $OUT_CODE dari call terakhir
  [ "$OUT_CODE" = "$1" ] || fail "$2: want HTTP $1, got $OUT_CODE"
  ok "$2 → $1"
}
tenant_id() { docker exec payrun-demo-postgres-1 psql -U postgres -d payrun -tAX -c "SELECT id FROM payrun.tenants WHERE slug = '$1'"; }
run_id_file=build/demo-run-id

OWNER_A=$(login owner-0@seed.payrun.test); PAY_A=$(login payroll-0@seed.payrun.test)
OWNER_B=$(login owner-1@seed.payrun.test)

demo_a() {
  say "Demo A — isolasi tenant (Alpha = seed-0, Beta = seed-1)"
  local emp body n
  call GET "/employees?limit=1" "$OWNER_A"; emp=$(jq -r '.data[0].id' <<<"$BODY")
  call GET "/employees/$emp" "$OWNER_B"; body=$BODY; expect 404 "A1 OWNER Beta membaca karyawan Alpha (bukan 403: keberadaannya pun tidak bocor)"
  [ "$(jq -r .code <<<"$body")" = NOT_FOUND ] || fail "A1 code: $body"
  call GET "/employees?limit=100" "$OWNER_B"; n=$(jq '[.data[] | select(.id == "'"$emp"'")] | length' <<<"$BODY")
  [ "$n" = 0 ] || fail "A2 Alpha employee appears in Beta's list"; ok "A2 daftar karyawan Beta: 0 baris Alpha"
  # A3: tid di JWT diganti ke tenant Alpha tanpa re-sign → signature tidak cocok.
  local tid_a forged; tid_a=$(tenant_id seed-0)
  forged=$(python3 -c '
import base64, json, sys
h, p, s = sys.argv[1].split(".")
d = json.loads(base64.urlsafe_b64decode(p + "=" * (-len(p) % 4))); d["tid"] = sys.argv[2]
print(h + "." + base64.urlsafe_b64encode(json.dumps(d).encode()).decode().rstrip("=") + "." + s)' "$OWNER_B" "$tid_a")
  call GET "/employees?limit=1" "$forged"; expect 401 "A3 claim tid diedit tanpa re-sign"
  call POST /employees "$OWNER_B" -H "Idempotency-Key: $(uuid)" -d "{\"tenant_id\":\"$tid_a\",\"employee_no\":\"X-1\",\"full_name\":\"Penyusup\"}"
  expect 400 "A4 tenant_id di body (field tidak dikenal, mass assignment)"
}

wait_status() { # <run> <status> <detik>
  local st prog
  for _ in $(seq 1 $(( $3 * 2 ))); do
    call GET "/payroll-runs/$1" "$PAY_A"
    read -r st prog < <(jq -r '"\(.status) \(.progress.chunks_done // 0)/\(.progress.chunks_total // 0)"' <<<"$BODY")
    printf '\r  … %s %s   ' "$st" "$prog"
    [ "$st" = "$2" ] && { printf '\n'; return 0; }
    [ "$st" = FAILED ] && { printf '\n'; fail "run FAILED"; }
    sleep 0.5
  done
  printf '\n'; fail "run not $2 after $3 s (last $st)"
}

demo_b() {
  say "Demo B — run ganda + worker mati di tengah chunk, lalu pulih"
  local body run restarts_before restarts_after
  call POST /payroll-runs "$PAY_A" -H "Idempotency-Key: $(uuid)" -d "{\"period\":\"$PERIOD\"}"; body=$BODY; expect 201 "B1 buat run $PERIOD"
  run=$(jq -r .id <<<"$body"); mkdir -p build; echo "$run" > "$run_id_file"
  call POST /payroll-runs "$PAY_A" -H "Idempotency-Key: $(uuid)" -d "{\"period\":\"$PERIOD\"}"; body=$BODY; expect 409 "B2 admin lain membuat run periode sama"
  [ "$(jq -r .code <<<"$body")" = ACTIVE_RUN_EXISTS ] && [ "$(jq -r .existing_resource_id <<<"$body")" = "$run" ] || fail "B2 body: $body"
  ok "B2 ACTIVE_RUN_EXISTS menunjuk run pertama"
  call POST /demo/faults "$OWNER_A" -H "X-Demo-Token: $TOKEN_DEMO" \
    -d '{"point":"payroll.calc_chunk.before_commit","after_n":3,"times":1,"action":"exit"}' >/dev/null
  expect 201 "B3 fault demo: worker exit tepat sebelum COMMIT chunk ke-4 (10 karyawan per chunk)"
  restarts_before=$(docker inspect -f '{{.RestartCount}}' payrun-demo-worker-1)
  call POST "/payroll-runs/$run/calculate" "$PAY_A" -H "Idempotency-Key: $(uuid)"; expect 202 "B4 calculate (async)"
  wait_status "$run" CALCULATED 90
  restarts_after=$(docker inspect -f '{{.RestartCount}}' payrun-demo-worker-1)
  [ "$restarts_after" -gt "$restarts_before" ] || fail "worker did not die (restart count $restarts_before → $restarts_after)"
  ok "B5 worker mati dan di-restart ($restarts_before → $restarts_after); chunk ke-4 di-rollback, diambil ulang setelah lease 10 dtk"
  local out; out=$("${DC[@]}" exec -T api /app/payrun verify-run "$(tenant_id seed-0)" "$run")
  jq -c '{status, items_checked, mismatches}' <<<"$out" | sed 's/^/  /'
  jq -e '.items_checked == 100 and (.mismatches | length) == 0' <<<"$out" >/dev/null || fail "verify-run: $out"
  ok "B6 verify-run: 100 item dihitung ulang dari snapshot tersimpan, 0 selisih"
}

demo_c() {
  say "Demo C — approve, finalize, lalu data final tidak bisa diubah"
  local run ap emp body
  [ -f "$run_id_file" ] || fail "run Demo B tidak ada: jalankan scripts/demo-live.sh b dulu"
  run=$(cat "$run_id_file")
  call POST "/payroll-runs/$run/submit" "$PAY_A" -d '{}'; body=$BODY; expect 200 "C1 PAYROLL_ADMIN submit"
  ap=$(jq -r .approval_request_id <<<"$body")
  call POST "/approvals/$ap/approve" "$PAY_A" -d '{}'; expect 403 "C2 pengaju meng-approve run sendiri (SoD)"
  call POST "/approvals/$ap/approve" "$OWNER_A" -d '{}'; expect 200 "C3 OWNER approve (cek stale: input tidak berubah)"
  call POST "/payroll-runs/$run/finalize" "$PAY_A" -H "Idempotency-Key: $(uuid)" -d '{}'; expect 200 "C4 finalize: periode dan run terkunci"
  call GET "/employees?limit=1" "$OWNER_A"; emp=$(jq -r '.data[0].id' <<<"$BODY")
  call POST "/employees/$emp/salary-changes" "$OWNER_A" -H "Idempotency-Key: $(uuid)" -d '{"base_salary":"9999000.00","valid_from":"2026-09-15"}'; body=$BODY
  expect 409 "C5 kenaikan gaji surut ke periode final"
  [ "$(jq -r .code <<<"$body")" = PERIOD_LOCKED ] || fail "C5 code: $body"
  call POST "/employees/$emp/salary-changes" "$OWNER_A" -H "Idempotency-Key: $(uuid)" -d '{"base_salary":"9999000.00","valid_from":"2026-10-01"}'
  expect 201 "C6 kenaikan mulai bulan berikutnya tetap boleh (hanya rentang yang berubah diperiksa)"
  local out; out=$("${DC[@]}" exec -T api /app/payrun verify-run "$(tenant_id seed-0)" "$run")
  jq -e '.status == "FINALIZED" and .items_checked == 100 and (.mismatches | length) == 0' <<<"$out" >/dev/null || fail "verify-run after finalize: $out"
  ok "C7 verify-run setelah finalize: hasil tersimpan = recompute dari snapshot"
}

case "${1:-all}" in
  a) demo_a ;; b) demo_b ;; c) demo_c ;;
  all) demo_a; demo_b; demo_c ;;
  *) echo "usage: $0 a|b|c|all"; exit 2 ;;
esac
say "selesai"
