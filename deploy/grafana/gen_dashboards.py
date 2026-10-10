#!/usr/bin/env python3
"""Menghasilkan JSON dashboard Grafana (deploy/grafana/dashboards/*.json).

Dashboard adalah kode: diubah di sini, di-review di PR, di-provision otomatis. Tidak ada dashboard
yang dibuat lewat klik di UI (docs/design/24-observability.md §6). Jalankan: make dashboards

Amandemen Step 12 (S15): PayRun V1 = satu dashboard domain "PayRun — Payroll & Tenancy" (gabungan
Payroll Runs + Tenancy & Security) + satu dashboard "Platform" (HTTP, DB, jobs).
"""
import json, pathlib

DS = {"type": "prometheus", "uid": "prom"}
OUT = pathlib.Path(__file__).parent / "dashboards"
RUNBOOK = "docs/design/24-observability.md#86-payruninvariantviolated"  # path di repo (tanpa URL hosting)


def panel(pid, title, exprs, x, y, w=12, h=8, kind="timeseries", unit=None, thresholds=None, desc="", links=None):
    p = {
        "id": pid, "type": kind, "title": title, "description": desc, "datasource": DS,
        "gridPos": {"x": x, "y": y, "w": w, "h": h},
        "targets": [{"refId": chr(65 + i), "datasource": DS, "expr": e, "legendFormat": l} for i, (e, l) in enumerate(exprs)],
        "fieldConfig": {"defaults": {}, "overrides": []},
        "options": {},
    }
    if unit:
        p["fieldConfig"]["defaults"]["unit"] = unit
    if thresholds:  # stat INV: hijau di 0, merah di atas 0
        p["fieldConfig"]["defaults"]["thresholds"] = {"mode": "absolute", "steps": thresholds}
        p["fieldConfig"]["defaults"]["color"] = {"mode": "thresholds"}
        p["options"] = {"colorMode": "background", "graphMode": "none", "reduceOptions": {"calcs": ["lastNotNull"]}}
    if links:
        p["links"] = links
    return p


INV = [{"color": "green", "value": None}, {"color": "red", "value": 1}]
WARN = [{"color": "green", "value": None}, {"color": "orange", "value": 1}]


def inv(pid, title, expr, x, desc):
    return panel(pid, title, [(expr, "count")], x, 0, 5, 5, "stat", thresholds=INV, desc=desc + " Normal SELALU 0. Runbook: " + RUNBOOK)


def dashboard(uid, title, panels, desc, tags):
    return {"uid": uid, "title": title, "description": desc, "tags": tags, "timezone": "browser",
            "schemaVersion": 39, "version": 1, "refresh": "30s", "time": {"from": "now-6h", "to": "now"},
            "panels": panels}


payroll = dashboard("payrun-payroll-tenancy", "PayRun — Payroll & Tenancy", [
    # Baris INV (Tenancy & Security): merah = alert A-13 PayRunInvariantViolated.
    inv(1, "INV: tenant isolation violations (24h)", "sum(increase(payrun_tenant_isolation_violations_total[24h]))", 0,
        "Baris tenant lain lolos ke repository/job (C-46)."),
    inv(2, "INV: finalized mutation attempts (24h)", "sum(increase(payrun_finalized_mutation_attempts_total[24h]))", 5,
        "Run/periode/adjustment final hanya ditahan trigger DB (P0001)."),
    inv(3, "INV: period lock bypass (24h)", "sum(increase(payrun_period_lock_bypass_total[24h]))", 10,
        "Tulisan ke periode FINALIZED hanya ditahan trigger DB (P0002)."),
    inv(4, "INV: reproducibility failures (24h)", "sum(increase(payrun_reproducibility_failures_total[24h]))", 15,
        "verify-run: hitung ulang dari snapshot ≠ hasil tersimpan (PI-12)."),
    panel(5, "Payslips: render failures (24h)", [("sum(increase(payrun_payslips_rendered_total{outcome=\"failed\"}[24h]))", "failed")], 20, 0, 4, 5, "stat",
          thresholds=WARN, desc="PF-22 AC4: gagal → retry; dead → PAYSLIP_NOT_READY."),
    # Payroll Runs.
    panel(10, "Runs by outcome (increase 1h)", [("sum by (outcome) (increase(payrun_payroll_runs_total[1h]))", "{{outcome}}")], 0, 5,
          desc="calculated / failed / approved / rejected / finalized / cancelled"),
    panel(11, "Run duration p95 by size", [("histogram_quantile(0.95, sum by (le, size_bucket) (rate(payrun_payroll_run_duration_seconds_bucket[1h])))", "{{size_bucket}}")],
          12, 5, unit="s", desc="calc_started_at → CALCULATED. Target: 1.000 karyawan < 5 menit (26-performance-plan)."),
    panel(12, "Items calculated (rate/min)", [("sum by (status) (rate(payrun_payroll_items_calculated_total[5m])) * 60", "{{status}}")], 0, 13),
    panel(13, "Item errors by code (increase 1h)", [("sum by (code) (increase(payrun_payroll_item_errors_total[1h]))", "{{code}}")], 12, 13,
          desc="NO_SALARY, RULESET_MISSING, NEGATIVE_NET, …"),
    panel(14, "Chunk duration p50/p95", [("histogram_quantile(0.5, sum by (le) (rate(payrun_payroll_chunk_duration_seconds_bucket[15m])))", "p50"),
                                         ("histogram_quantile(0.95, sum by (le) (rate(payrun_payroll_chunk_duration_seconds_bucket[15m])))", "p95")], 0, 21, unit="s"),
    panel(15, "RUN_STALE rejections + candidate p95", [("sum by (action) (increase(payrun_run_stale_rejections_total[1h]))", "stale {{action}}"),
                                                       ("histogram_quantile(0.95, sum by (le) (rate(payrun_approve_stale_candidates_bucket[1h])))", "candidates p95")], 12, 21,
          desc="Input berubah setelah kalkulasi (ADR-P07). Kandidat > 200 → stale tanpa cek fingerprint."),
    panel(16, "Approvals by subject/decision (increase 1h)", [("sum by (subject, decision) (increase(payrun_approvals_total[1h]))", "{{subject}} {{decision}}"),
                                                              ("sum(increase(payrun_approval_self_approved_total[1h]))", "self-approved run")], 0, 29),
    panel(17, "Payslips rendered/failed + render p95", [("sum by (outcome) (increase(payrun_payslips_rendered_total[1h]))", "{{outcome}}"),
                                                        ("histogram_quantile(0.95, sum by (le) (rate(payrun_payslip_render_duration_seconds_bucket[1h])))", "batch p95 (s)")], 12, 29),
    # Tenancy & Security.
    panel(20, "Payslip access by actor (increase 1h)", [("sum by (actor) (increase(payrun_payslip_access_total[1h]))", "{{actor}}")], 0, 37,
          desc="admin = ter-audit (PI-17). Lonjakan admin di luar jadwal payroll patut dicek di audit log."),
    panel(21, "Authz denied: 403 vs 404 (rate/min)", [("sum by (status) (rate(app_http_requests_total{status=~\"403|404\"}[5m])) * 60", "{{status}}")], 12, 37),
    panel(22, "Authn failures / lockouts / refresh reuse (increase 1h)", [("sum by (reason) (increase(app_authn_failures_total[1h]))", "fail {{reason}}"),
                                                                          ("sum(increase(app_authn_lockouts_total[1h]))", "lockouts"),
                                                                          ("sum(increase(app_refresh_reuse_detected_total[1h]))", "refresh reuse")], 0, 45),
    panel(23, "PERIOD_LOCKED rejections (increase 1h)", [("sum by (source) (increase(payrun_period_lock_rejections_total[1h]))", "{{source}}")], 12, 45,
          desc="Penulis input ditolak guard aplikasi: koreksi periode final harus lewat adjustment."),
], "Apakah payroll bulan ini selesai tepat waktu, dan adakah tanda kebocoran antar-tenant atau penyalahgunaan akses?",
   ["payrun"])

platform = dashboard("payrun-platform", "PayRun — Platform (HTTP, DB, Jobs)", [
    panel(1, "RPS by route", [("sum by (route) (rate(app_http_requests_total[5m]))", "{{route}}")], 0, 0),
    panel(2, "5xx ratio", [("sum(rate(app_http_requests_total{status=~\"5..\"}[5m])) / sum(rate(app_http_requests_total[5m]))", "5xx")], 12, 0, unit="percentunit"),
    panel(3, "p95 latency by route", [("histogram_quantile(0.95, sum by (le, route) (rate(app_http_request_duration_seconds_bucket[5m])))", "{{route}}")], 0, 8, unit="s"),
    panel(4, "Errors by code (rate/min)", [("topk(10, sum by (code) (rate(app_errors_total[5m])) * 60)", "{{code}}")], 12, 8),
    panel(5, "DB pool acquired vs max", [("max by (service) (app_db_pool_acquired_conns)", "acquired {{service}}"),
                                         ("max by (service) (app_db_pool_max_conns)", "max {{service}}")], 0, 16),
    panel(6, "Tx retries / deadlocks / lock timeouts", [("sum by (reason) (increase(app_db_tx_retries_total[15m]))", "retry {{reason}}"),
                                                        ("sum(increase(app_db_deadlocks_total[15m]))", "deadlock"),
                                                        ("sum by (kind) (increase(app_db_lock_timeouts_total[15m]))", "lock timeout {{kind}}")], 12, 16),
    panel(7, "Job queue depth by kind/status", [("sum by (kind, status) (app_job_queue_depth)", "{{kind}} {{status}}")], 0, 24),
    panel(8, "Oldest queued job age + outbox lag", [("max by (kind) (app_job_oldest_age_seconds)", "{{kind}}"),
                                                    ("max(app_outbox_lag_seconds)", "outbox lag")], 12, 24, unit="s"),
    panel(9, "Job duration p95 by kind", [("histogram_quantile(0.95, sum by (le, kind) (rate(app_job_duration_seconds_bucket[15m])))", "{{kind}}")], 0, 32, unit="s"),
    panel(10, "Job failures / dead / fenced", [("sum by (kind, class) (increase(app_job_failures_total[15m]))", "fail {{kind}} {{class}}"),
                                               ("sum by (kind) (increase(app_job_dead_total[15m]))", "dead {{kind}}"),
                                               ("sum by (kind) (increase(app_job_fenced_total[15m]))", "fenced {{kind}}")], 12, 32),
], "Lambat atau error di mana: aplikasi, pool, query, atau antrean job?", ["payrun", "platform"])

OUT.mkdir(exist_ok=True)
for d in (payroll, platform):
    path = OUT / f"{d['uid']}.json"
    path.write_text(json.dumps(d, indent=2, ensure_ascii=False) + "\n")
    print("wrote", path)
