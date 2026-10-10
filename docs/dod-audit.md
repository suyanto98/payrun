# Audit Definition of Done — PayRun M7

Audit terhadap `docs/design/31-definition-of-done.md` §1 (item PayRun; item khusus LedgerPay tidak berlaku di repo
ini). Tanggal: 2026-10-10, Sprint 10.

Status: ✅ terpenuhi dengan bukti · ⚠️ sebagian (dijelaskan) · ⏳ **menunggu deploy / repo di GitHub / waktu**
(tidak bisa dibuktikan secara lokal; latihan lokalnya disebut).

Ringkasan: semua fitur P0 dan P1 selesai; P2/P3 didaftar di README dengan alasan. Semua item yang bisa dibuktikan
secara lokal terpenuhi atau ditandai sebagian. Sprint 10 menutup tujuh celah yang ditemukan audit ini sendiri (lihat
bagian terakhir). Yang tersisa membutuhkan environment publik dan waktu: 7 hari metrik INV = 0, 7 malam
`demo-verify`, video, URL publik, CI yang benar-benar berjalan di GitHub, angka performa di VM.

## Fitur
| ID | Fitur | Status | Bukti |
|---|---|---|---|
| PF-01…PF-14, PF-17, PF-18, PF-20 | P0 | ✅ | Test per modul (katalog `docs/design/25-testing-strategy.md` §4), Demo A–C (`make demo-verify`, `make demo-live`) |
| PF-15, PF-16, PF-19, PF-22, PF-24, PF-29 | P1: slip web, audit akses slip, adjustment, slip PDF, metrik/dashboard tenant, rate limit | ✅ | `TestPayslipAccess_Matrix`, `TestPayslipPDF_DeterministicBytes`, `TestAdjustment_AppliedOnce`, dashboard *Payroll & Tenancy*, tier rate limit di `internal/platform/ratelimit` |
| PF-21, PF-23, PF-25…PF-28 | P2 | ✅ didaftar | README "Deliberately not built" (PF-21/PF-23 diturunkan di Step 12) |
| PF-30…PF-33 | P3 | ✅ didaftar | README |

## §1 per item

| # | Item | Status | Bukti / catatan |
|---|---|---|---|
| P1–P4 | Fitur P0/P1, P2/P3 didaftar, tanpa fitur di luar matrix | ✅ | Tabel di atas |
| C1 | Setiap PI-01…PI-19 punya test yang lulus | ✅ | 19/19 baris §5.2 bertanda ✅; nama dicek `TestTraceability_DocsReferenceExistingTests` |
| C2 | Invariant checker di `t.Cleanup` setiap test integrasi | ✅ | `internal/app/invariants_test.go` (PI-04, PI-05, PI-16, sebagai superuser lintas tenant); dibuktikan menangkap total run yang dirusak oleh test yang lolos |
| C3 | Metrik INV = 0 selama 7 hari di environment demo | ⏳ | Metrik sekarang diisi oleh proses worker (job `payroll.verify_run` setelah finalize, `TestVerifyRunJob_AfterFinalize_FeedsInvariantMetrics`); 7 hari butuh environment publik |
| C4 | Golden ≥ 30 kasus | ✅ | `TestGolden` 33 kasus, `TestTonoMarch2026_LineByLine` |
| C5 | Property test 1 juta iterasi di nightly | ⚠️ | `TestProperty_Invariants` 10.000 iterasi per run di CI; varian 1 juta di nightly belum dibuat |
| V1 | Validasi strict, field tak dikenal ditolak | ✅ | Demo A4 (`tenant_id` di body → 400), `TestContract_*` |
| V2 | Semua error dari katalog | ✅ | `TestDBErrorMapping_AllCatalogueConstraints`, `TestContract_*` |
| V3 | Uang selalu string | ✅ | `TestOpenAPI_MoneyFieldsAreStrings` (statis) + respons divalidasi terhadap spec oleh `TestContract_*` |
| A2 | Authz matrix 100% endpoint, endpoint tanpa baris gagal CI | ✅ | `TestAuthzMatrix`, `TestAuthzMatrix_CoversEveryRoute` |
| A3 | Suite lintas tenant + RLS bypass + pool leakage | ✅ | `TestCrossTenant`, `TestRLS_UnfilteredQuerySeesOnlyActiveTenant`, `TestRLS_PoolLeakageAfterCommit`, `TestWithTenantTx_RetryKeepsTenant` |
| S1 | Kontrol MVP Step 6 terimplementasi + test | ⚠️ | Kontrol PayRun dipetakan ke test di `25-testing-strategy.md` §5.4; tidak diaudit ulang satu per satu di sprint ini |
| S2 | Scan CI bersih (govulncheck, trivy) | ⏳ | Job ada di `pr.yml`/`main.yml`; belum pernah berjalan di GitHub. Lokal: `make security` |
| S3 | gitleaks seluruh history bersih | ✅ | `gitleaks git --log-opts=--all` → no leaks (2026-10-10, setelah allowlist sempit untuk kunci dev) |
| S4 | Security header di semua respons | ✅ | `TestSecurityHeaders_OnEveryAPIResponse` (200/401/404/400). Header SPA (HSTS dll.) dipasang Caddy di `deploy/prod` |
| S5 | Endpoint demo tidak ada di image release | ✅ | `TestBuild_DemoEndpointsAbsentWithoutDemoMode`, `TestDemoMode_ReleaseBuildHasNoDemoRoutes` |
| D1 | Migrasi dari nol dan sebagai upgrade | ⚠️ | Dari nol: setiap test integrasi + job CI `migration`. Upgrade dari rilis sebelumnya: belum ada rilis |
| D2 | Catalogue test (RLS + FORCE, FK komposit, SECURITY DEFINER, grant) | ✅ | `TestCatalog_EveryTableHasForcedRLS`, `TestCatalogue_TenantFKsComposite`, `TestCatalog_SecurityDefinerAllowlist`, `TestCatalog_AppGrantsMatchMatrix` |
| D3 | Backup harian + restore drill 7 hari terakhir | ⏳ | `deploy/backup`, `scripts/restore-drill.sh` (cek tabel tenant > 0 + verify-run run final); jadwal butuh VM |
| D4 | Seed demo deterministik | ✅ | `make seed-demo` (seed tetap 42, gate invariant setelah seed) |
| F2 | PFS-01…PFS-10 hijau | ✅ | Matriks §5.3; dua test baru S10: `TestCalcChunk_PanicExhaustsAttempts_RunFailed` (PFS-05), `TestJob_TenantRunMismatch_NoOpAndAlert` (PFS-09) |
| F3 | Chaos suite di nightly | ⚠️ | Pengganti Toxiproxy (Step 12): hook `testhooks` + proses worker yang dimatikan, berjalan di job integration CI, bukan nightly terpisah |
| F4 | Race test dengan `-race` | ✅ | `make test-integration` (`-race`), job CI integration |
| T1 | Traceability 100% | ✅ | `TestTraceability_DocsReferenceExistingTests` (README, docs, ADR-P*, laporan perf, katalog §4.4–4.6, §5.2, §5.3) |
| T2 | Karantina flaky kosong | ✅ | Tidak ada test yang di-skip/karantina |
| T3 | CI PR hijau di `main` | ⏳ | Repo belum di GitHub. Lokal: unit, integration, lint, `test-alerts` hijau |
| T4 | Coverage dilaporkan | ✅ | `make cover` → `build/coverage.html` (indikatif) |
| O1 | Log JSON + redaction teruji | ✅ | `TestLogRedaction_PIITypes`, `TestLogRedaction_Headers`, `TestLogRedaction_AuthBody` |
| O2 | Metrik inti live | ✅ | `/metrics` api + worker; `TestObservability_RulesAndDashboardsUseDefinedMetrics` |
| O3 | Dashboard di-provision dari file | ✅ | `deploy/grafana/dashboards` (generator `gen_dashboards.py`), `make up-obs`; screenshot ⏳ |
| O4 | Alert terdefinisi + promtool | ✅ | `deploy/prometheus/rules`, `make test-alerts`; setiap alert menunjuk runbook (`TestObservability_AlertsLinkToRunbook`) |
| O5 | Trace api → job tersambung | ⚠️ | Span chunk = root baru dengan link ke request calculate (`jobs.trace_parent`); screenshot Jaeger ⏳ |
| OP1 | `/healthz`, `/readyz` (versi skema) | ✅ | Port ops; readiness per lane worker |
| OP2 | `make up` dari clone bersih | ✅ | Quick start README; `make up-demo` diuji dari volume kosong di S10 |
| OP3 | Deploy ter-script + rollback teruji | ⏳ | `deploy/prod/deploy.sh` (health gate, rollback otomatis), `rollback.yml`; deploy publik ditunda |
| OP4 | Runbook | ✅ | `docs/runbook.md` (10 alert + prosedur) |
| OP5 | RPO/RTO jujur | ✅ | README "Limitations": RPO 24 jam, RTO ±2 jam |
| OP6 | Kill switch | ✅ | `TestPayrollWorker_KillSwitchKeepsJobsQueued`; `PR_PAYSLIP_RENDER_ENABLED` |
| PF1 | Laporan baseline dengan spesifikasi environment | ✅ (laptop) | `perf/results/2026-10-10-fed1df1/REPORT.md`; angka VM ⏳ |
| PF2 | Keputusan E-2 dicatat | ✅ | REPORT §PR-4 + amandemen ADR-P07 |
| PF3 | CI perf smoke nightly | ⏳ | Ditunda setelah M7 (Step 12 S19) |
| DC1 | README struktur Step 11 | ✅ | README |
| DC2 | ADR mutakhir + amandemen | ✅ | Amandemen ADR-P07 (E-2), ADR-012 (SeaweedFS) |
| DC3 | Diagram arsitektur | ✅ | `docs/architecture.md` (container, tenant context, state run, modul) |
| DC4 | API docs statis | ✅ | `make api-docs` → `build/api.html` (Redocly) |
| DC5 | Demo script | ✅ | `docs/demo-script.md`, dijalankan dengan asersi oleh `make demo-live` |
| DC6 | Limitations jujur | ✅ | README |
| DM1 | `demo-verify` nightly 7 malam | ⏳ | `nightly.yml` siap; lokal hijau |
| DM2 | Video demo | ⏳ | Setelah deploy |
| DM3 | URL publik | ⏳ | Setelah deploy |
| DM4 | Token demo aman | ✅ | Tidak ada di README; VM memakai secret (`init-env.sh`); token default `compose.demo.yml` hanya untuk stack lokal di 127.0.0.1 |
| H1 | Lisensi | ⚠️ | `LICENSE` MIT, nama pemilik masih placeholder `<NAMA ANDA>` (diisi pemilik) |
| H2 | Tanpa TODO/FIXME di jalur kritis | ✅ | `grep` kosong di `internal`, `cmd`, `db`, `web/src` |
| H3 | Dependency di-pin + Dependabot | ✅ | `go.sum`, action di-pin ke SHA, `.github/dependabot.yml` |
| H4 | Tidak ada kode/penamaan dari pekerjaan kantor | ✅ | Review diri |
| H5 | Conventional commits | ✅ | `feat(sN)`, `fix(sN)`, `docs(sN)`; S7–S9 satu commit gabungan (pilihan pemilik) |

## Celah yang ditemukan dan ditutup di Sprint 10
| Celah | Ditemukan oleh | Tindakan |
|---|---|---|
| 30 rujukan test di dokumen menunjuk nama yang tidak ada | `TestTraceability_DocsReferenceExistingTests` (baru, menggantikan cek PI saja) | Nama disinkronkan; setiap pengganti dicek isinya sebelum dirujuk |
| PFS-05 tanpa test (run → FAILED setelah percobaan habis) | Traceability | `TestCalcChunk_PanicExhaustsAttempts_RunFailed` (mutasi `onDead` membuatnya gagal) |
| PFS-09 tanpa test (job lintas tenant) | Traceability | `TestJob_TenantRunMismatch_NoOpAndAlert` |
| Metrik `payrun_payroll_total_mismatch_runs` tidak pernah diisi; `reproducibility_failures` hanya di proses CLI | Penulisan runbook | Job `payroll.verify_run` setelah finalize + gauge; test memanipulasi data dan melihat metrik naik |
| Tidak ada invariant checker di `t.Cleanup` (C2) | Audit ini | `invariants_test.go` |
| Tidak ada test security header (S4) dan uang = string (V3) | Audit ini | `TestSecurityHeaders_OnEveryAPIResponse`, `TestOpenAPI_MoneyFieldsAreStrings` |
| Retry `40001` dengan tenant belum diuji | Audit baris PI-01 | `TestWithTenantTx_RetryKeepsTenant` |
| Annotation runbook alert menunjuk dokumen desain, bukan runbook operasional | Audit OP4 | `docs/runbook.md` + `TestObservability_AlertsLinkToRunbook` |
