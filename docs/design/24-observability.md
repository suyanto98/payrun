# Step 8.1 — Observability (LedgerPay & PayRun)

> Bagian dari [Step 8](00-index.md). Konvensi metrik, log, kardinalitas, dan SLO di `00-index.md` bersifat mengikat.
> Dokumen ini mengonsolidasikan **semua** metrik yang muncul di Step 1–7 (open items O4 + O14) ke satu katalog dengan nama final. Tabel pemetaan nama lama → nama final ada di §4.

## Daftar isi
1. [Logging](#1-logging)
2. [Health](#2-health)
3. [Katalog metrik](#3-katalog-metrik)
4. [Pemetaan nama lama → final](#4-pemetaan-nama-lama--nama-final)
5. [Tracing](#5-tracing)
6. [Dashboard](#6-dashboard)
7. [Alert rules](#7-alert-rules)
8. [Runbook](#8-runbook)
9. [Observability di demo](#9-observability-di-demo)

---

## 1. Logging

### 1.1 Skema field

Semua log berupa satu objek JSON per baris (`log/slog` JSON handler) ke stdout. Di profile `obs`, Promtail mengirimnya ke Loki dengan label stream **hanya** `service`, `env`, dan `level`. Field lain di-*parse* dengan `| json` saat query, karena label Loki dengan kardinalitas tinggi sama mahalnya dengan label Prometheus.

| Field | Tipe | Kapan ada | Contoh |
|---|---|---|---|
| `ts` | RFC 3339 nano UTC | selalu | `2026-03-15T08:30:00.123456Z` |
| `level` | `DEBUG`/`INFO`/`WARN`/`ERROR` | selalu | `INFO` |
| `msg` | event name stabil (§1.3) | selalu | `transfer.completed` |
| `service` | `api` / `worker` / `mock` | selalu | `api` |
| `project` | `ledgerpay` / `payrun` | selalu | `ledgerpay` |
| `env` | `local` / `demo` / `staging` | selalu | `demo` |
| `version` | git SHA pendek | selalu | `8f5fd57` |
| `request_id` | string | request HTTP | `01JPA3…` |
| `correlation_id` | string | request HTTP, job, webhook | `01JPA3…` |
| `trace_id`, `span_id` | hex | jika ada span aktif | `4bf92f35…` |
| `route` | template rute | request HTTP | `POST /v1/transfers` |
| `status`, `duration_ms` | int | log akhir request | `201`, `14` |
| `user_id` | uuid | principal user | |
| `merchant_id` | uuid | principal merchant (LedgerPay) | |
| `tenant_id` | uuid | PayRun, semua log tenant-scoped | |
| `role` | string | aksi ops/admin | `OPS` |
| `job_id`, `job_kind`, `attempt` | uuid/string/int | log job | `payout.submit`, `2` |
| ID bisnis | uuid | sesuai konteks | `transfer_id`, `topup_id`, `payout_id`, `intent_id`, `journal_id`, `run_id`, `calc_version`, `chunk_no`, `employee_id` |
| `error_code` | code katalog (Step 5) | respons 4xx/5xx | `INSUFFICIENT_BALANCE` |
| `error_class` | `retryable`/`permanent` | kegagalan job | |
| `err` | string ringkas | `ERROR`/`WARN` | `context deadline exceeded` |
| `pg_code`, `constraint` | string | error DB terpetakan | `23514`, `ck_accounts_balance_non_negative` |

**Tidak pernah dilog** (C-23, allowlist): password, token (access/refresh/undangan), API key, header `Authorization`/`Cookie`, signature webhook, body request/response endpoint auth, nilai PII (NIK, NPWP, nomor rekening, gaji), `response_body` idempotency, isi CSV. Nilai yang perlu dirujuk diganti ID atau versi ter-mask (`****1234`).

### 1.2 Kebijakan level

| Level | Dipakai untuk | Contoh |
|---|---|---|
| `INFO` | Satu log akhir per request (access log) + event bisnis penting | `http.request`, `transfer.completed`, `payroll_run.finalized` |
| `WARN` | Kondisi yang ditangani sistem, tetapi menarik bagi operator | `payout.unknown`, `job.lease_lost`, `webhook.late`, `idempotency.wait_timeout`, `ratelimit.rejected` |
| `ERROR` | Butuh perhatian manusia, atau menandakan bug | `job.dead`, `ledger.invariant_violation`, `db.unmapped_error`, respons 5xx |
| `DEBUG` | Mati di `demo`/`staging`; hanya `local` | Detail SQL plan, payload mock |

4xx karena kesalahan client dicatat sebagai `INFO` (bagian dari access log), bukan `WARN`. Kalau tidak, log akan dibanjiri `INSUFFICIENT_BALANCE` yang memang perilaku normal.

### 1.3 Event name (`msg`)

Format `<domain>.<event>` (huruf kecil, titik). Nama event adalah **kontrak** untuk query dan alert berbasis log, jadi tidak diganti sembarangan.

| Domain | Event |
|---|---|
| Platform | `http.request`, `http.panic_recovered`, `db.tx_retry`, `db.unmapped_error`, `db.pool_acquire_timeout`, `ratelimit.rejected`, `config.loaded`, `server.started`, `server.shutdown` |
| Auth | `auth.login_succeeded`, `auth.login_failed`, `auth.account_locked`, `auth.refresh_rotated`, `auth.refresh_reuse_detected`, `auth.csrf_rejected` |
| Job | `job.enqueued`, `job.claimed`, `job.succeeded`, `job.failed`, `job.dead`, `job.lease_lost`, `job.fenced` |
| LedgerPay | `transfer.completed`, `topup.created`, `topup.succeeded`, `topup.expired`, `payment.succeeded`, `payment.refunded`, `payout.requested`, `payout.submitted`, `payout.succeeded`, `payout.failed`, `payout.unknown`, `payout.status_checked`, `payout.needs_reconciliation`, `webhook.received`, `webhook.signature_invalid`, `webhook.duplicate`, `webhook.late`, `webhook.conflict`, `webhook_delivery.attempted`, `webhook_delivery.blocked`, `recon.run_completed`, `recon.mismatch`, `ledger.verification_completed`, `ledger.invariant_violation`, `ops.action` |
| PayRun | `tenant.signed_up`, `membership.role_changed`, `membership.sod_conflict`, `approval.decided`, `payroll_run.calculation_requested`, `payroll_run.chunk_completed`, `payroll_run.calculated`, `payroll_run.failed`, `payroll_run.stale_rejected`, `payroll_run.approved`, `payroll_run.finalized`, `payslip.rendered`, `payslip.accessed`, `period.lock_rejected`, `tenant.isolation_violation`, `import.completed` |

### 1.4 Sampling
Tidak ada sampling log di V1. Volume demo kecil, dan kehilangan satu log di jalur uang lebih mahal daripada biaya penyimpanan. V2: access log `INFO` untuk `GET` sukses bisa disampling 10%. Event bisnis dan semua `WARN`/`ERROR` tidak pernah disampling.

### 1.5 Model korelasi

| ID | Cakupan | Dibuat oleh | Dipropagasi lewat |
|---|---|---|---|
| `request_id` | Satu request HTTP | Middleware (atau `X-Request-ID` dari client yang valid) | Header respons, body error RFC 9457 |
| `correlation_id` | Satu **alur bisnis** (bisa melintasi banyak request, job, dan panggilan keluar) | = `request_id` request pertama yang memulai alur | `jobs.correlation_id` → ctx job → header **`X-Correlation-ID`** pada panggilan keluar (provider, webhook merchant) → job turunan mewarisi |
| `trace_id` | Satu trace OpenTelemetry | OTel SDK | `traceparent` (W3C) di header keluar dan di `jobs.trace_parent` (§5.3) |

Aturan:
- Panggilan keluar membawa **dua** header: `X-Request-ID` baru per panggilan (supaya log provider bisa dicocokkan per HTTP call) dan `X-Correlation-ID` milik alur. Step 7 `03` §10 menulis "header `X-Request-ID` pada panggilan keluar". Dokumen ini mempertegasnya menjadi dua header.
- Webhook masuk: `correlation_id` = `correlation_id` yang dikirim provider di payload (mock mengembalikan `metadata.correlation_id` dari charge), atau, kalau tidak ada, ID event inbox. Dengan begitu, top-up bisa dilacak dari `POST /topups` sampai kredit saldo.
- Job turunan (`payout.status_check:{id}:{n}`, `payroll.calc_chunk`, `payslip.render`) mewarisi `correlation_id` job induknya.

### 1.6 Contoh log

**Transfer (satu request, satu baris akhir + satu event bisnis):**
```json
{"ts":"2026-03-15T08:30:00.101Z","level":"INFO","msg":"transfer.completed","service":"api","project":"ledgerpay","env":"demo","version":"8f5fd57","request_id":"01JPA3K9","correlation_id":"01JPA3K9","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","user_id":"0192…a1","transfer_id":"0192…f0","journal_id":"0192…f1","idempotency_replayed":false}
{"ts":"2026-03-15T08:30:00.103Z","level":"INFO","msg":"http.request","service":"api","project":"ledgerpay","env":"demo","version":"8f5fd57","request_id":"01JPA3K9","correlation_id":"01JPA3K9","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","route":"POST /v1/transfers","status":201,"duration_ms":14,"user_id":"0192…a1"}
```

**Demo C: payout timeout → worker di-kill → recovery** (satu `correlation_id = 01JPB7Q2`):
```json
{"ts":"…:00.000Z","level":"INFO","msg":"payout.requested","service":"api","correlation_id":"01JPB7Q2","payout_id":"0192…p1","user_id":"0192…a1","journal_id":"0192…h1"}
{"ts":"…:00.640Z","level":"INFO","msg":"job.claimed","service":"worker","correlation_id":"01JPB7Q2","job_id":"0192…j1","job_kind":"payout.submit","attempt":1}
{"ts":"…:00.645Z","level":"INFO","msg":"payout.submitted","service":"worker","correlation_id":"01JPB7Q2","payout_id":"0192…p1","job_id":"0192…j1"}
{"ts":"…:00.700Z","level":"INFO","msg":"mock.payout_processed","service":"mock","correlation_id":"01JPB7Q2","payout_ref":"mp_8812","scenario":"timeout_after_process","idempotency_key":"0192…p1"}
{"ts":"…:10.646Z","level":"WARN","msg":"payout.unknown","service":"worker","correlation_id":"01JPB7Q2","payout_id":"0192…p1","err":"context deadline exceeded"}
{"ts":"…:41.002Z","level":"INFO","msg":"job.claimed","service":"worker","correlation_id":"01JPB7Q2","job_id":"0192…j2","job_kind":"payout.status_check","attempt":1,"was_lease_expired":false}
{"ts":"…:41.050Z","level":"INFO","msg":"payout.status_checked","service":"worker","correlation_id":"01JPB7Q2","payout_id":"0192…p1","provider_status":"SUCCEEDED","check_no":1}
{"ts":"…:41.058Z","level":"INFO","msg":"payout.succeeded","service":"worker","correlation_id":"01JPB7Q2","payout_id":"0192…p1","journal_id":"0192…s1"}
```
Satu query `{project="ledgerpay"} | json | correlation_id="01JPB7Q2"` menampilkan seluruh cerita: api → worker → mock → worker. Mock memproses payout **sekali** (`idempotency_key = payout_id`).

---

## 2. Health

### 2.1 Endpoint

| Endpoint | Proses | Memeriksa | Timeout | Tidak memeriksa (sengaja) |
|---|---|---|---|---|
| `GET /healthz` | api, worker, mock | Proses hidup dan event loop responsif (handler mengembalikan 200) | — | Dependency apa pun. Liveness yang memeriksa DB membuat restart berantai saat DB lambat |
| `GET /readyz` | api | (1) `SELECT 1` ke DB, timeout 1 dtk; (2) versi migrasi DB **≥ versi minimum** yang dikompilasi ke binary (bukan `=`; dengan `=`, app N-1 langsung not-ready setelah migrasi expand dan rollback app tidak pernah bisa ready, Step 9 `03`); (3) pool tidak jenuh (`acquired < max` atau acquire sukses dalam 200 ms) | 1,5 dtk total | mock-provider (api tetap melayani transfer saat provider mati; degradasi ditangani state machine) |
| `GET /readyz` | worker | (1) DB ping; (2) migrasi; (3) **heartbeat claim loop**: setiap lane menulis timestamp in-memory tiap iterasi; ready jika semua lane < 3× polling interval (±1,5 dtk; lane panjang < 30 dtk) | 1,5 dtk | Antrean kosong/penuh (bukan urusan readiness) |

Endpoint health dilayani di **port internal** yang sama dengan `/metrics` (misalnya `:9090`), tidak lewat Caddy, sehingga tidak bisa dipanggil dari internet. Di port publik hanya ada `/api/v1/...`.

### 2.2 Respons
```json
// 200 OK
{ "status": "ready", "version": "8f5fd57", "checks": {
    "db": { "status": "ok", "latency_ms": 2 },
    "migrations": { "status": "ok", "expected": 42, "actual": 42 },
    "db_pool": { "status": "ok", "acquired": 3, "max": 20 } } }

// 503 Service Unavailable
{ "status": "not_ready", "version": "8f5fd57", "checks": {
    "db": { "status": "fail", "error": "timeout after 1s" },
    "migrations": { "status": "unknown" }, "db_pool": { "status": "ok", "acquired": 0, "max": 20 } } }
```
Pesan error di health tidak memuat connection string atau hostname internal.

Gauge `app_ready{service}` (1/0) diperbarui setiap kali `/readyz` dievaluasi, dan juga oleh loop internal tiap 10 dtk, supaya alert tidak bergantung pada ada atau tidaknya pemanggil eksternal.

### 2.3 Tabel dependency

| Dependency | Dipakai oleh | Jika mati | Health | Metrik |
|---|---|---|---|---|
| PostgreSQL | api, worker | api → `503 SERVICE_UNAVAILABLE`; worker berhenti claim | readiness gagal | `app_db_pool_*`, `app_ready` |
| mock-provider | worker (LedgerPay), api (create charge best-effort) | Top-up tanpa `checkout_url` (fallback `topup.recover`); payout → `UNKNOWN` → status check | Tidak di readiness | `app_outbound_requests_total{target="provider",outcome}` |
| Endpoint merchant | worker (`webhook.deliver`) | Retry/backoff → `dead` | Tidak | `ledgerpay_webhook_deliveries_total{outcome}` |
| MinIO/S3 (PayRun P1) | worker (`payslip.render`), api (presign) | Payslip `FAILED` → retry; download `PAYSLIP_NOT_READY` | Tidak (degradasi fitur, bukan sistem) | `app_outbound_requests_total{target="storage"}` |
| Loki/Prometheus/Jaeger | — | Tidak berdampak ke aplikasi (fire-and-forget; exporter OTLP non-blocking dengan buffer) | Tidak | — |

### 2.4 Pemakaian oleh compose & Caddy
- `docker-compose.yml`: `healthcheck` setiap service memakai `/healthz` (liveness → restart). `depends_on: condition: service_healthy` hanya untuk `postgres` (pg_isready) dan job `migrate` (`service_completed_successfully`).
- **Urutan startup:** `postgres` (healthy) → `migrate` (selesai, exit 0) → `api`, `worker`, `mock` → `caddy`. `api` menolak ready sampai versi migrasi DB ≥ versi minimum binary, sehingga image baru dengan migrasi yang belum dijalankan tidak pernah menerima trafik.
- Caddy: `reverse_proxy` dengan `health_uri /readyz` (port internal), `health_interval 5s`. Upstream yang tidak ready dikeluarkan dari rotasi. Ini relevan di V2 dengan api×N; di V1 dengan satu instance, Caddy mengembalikan 502/503 yang jujur.
- Deploy (Step 9): gate "health check" di pipeline menunggu `/readyz` 200 setelah restart, sebelum menganggap deploy sukses.

---

## 3. Katalog metrik

**Prinsip tipe:** *event* → **counter** (`_total`). *Keadaan yang ditemukan oleh job verifikasi atau query periodik* → **gauge** (tanpa `_total`). Keputusan ini mengubah beberapa nama dari step sebelumnya (contoh: `ledger_unbalanced_journal_total` adalah hasil verifikasi, jadi menjadi gauge `ledgerpay_ledger_unbalanced_journals`). Lihat §4.

**Bucket standar:**
- `HTTP`: 0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2.5, 5 (dtk). Bucket 0.2 dan 0.3 sengaja ada karena itulah batas SLO.
- `DB`: 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5
- `JOB`: 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300
- `RUN` (payroll): 1, 5, 10, 30, 60, 120, 300, 600, 1800

Kolom **Alert** merujuk §7. **INV** = business invariant: nilai normal selalu 0, dan alert-nya critical.

### 3.1 Platform / shared (`app_`)

| Metrik | Tipe | Label | Arti | Alert | Sumber |
|---|---|---|---|---|---|
| `app_build_info` | gauge (=1) | `service`, `version`, `go_version` | Versi yang berjalan | — | — |
| `app_ready` | gauge | `service` | 1 = ready, 0 = tidak | A-09 | §2 |
| `app_http_requests_total` | counter | `route`, `method`, `status` | Request selesai. `route` = template (terbatas), `status` = kode HTTP | A-06 | LF-13, PF-17 |
| `app_http_request_duration_seconds` | histogram (HTTP) | `route`, `method` | Latency server-side | A-07 | SLO |
| `app_http_in_flight_requests` | gauge | — | Request yang sedang diproses | — | — |
| `app_http_rejected_total` | counter | `reason` (`body_too_large`, `unknown_field`, `bad_json`, `unsupported_media`) | Request ditolak sebelum handler | — | Step 6 T-SH-11/12 |
| `app_rate_limited_total` | counter | `tier` | Respons 429 | — | Step 5 §7 |
| `app_errors_total` | counter | `code` (code katalog Step 5) | Respons error per code. Label terbatas karena katalognya tertutup | — | Step 5 `01` |
| `app_db_pool_acquired_conns` / `app_db_pool_idle_conns` / `app_db_pool_max_conns` | gauge | `service` | Statistik `pgxpool.Stat()` | A-08 | Step 7 `01` §6 |
| `app_db_pool_acquire_duration_seconds` | histogram (DB) | `service` | Waktu menunggu koneksi | A-08 | Step 7 |
| `app_db_pool_acquire_timeout_total` | counter | `service` | Acquire > 2 dtk → `503` | A-08 | Step 7 `01` |
| `app_db_query_duration_seconds` | histogram (DB) | `query` (nama query sqlc, contoh `PostJournalEntries`) | Latency per query. Nama query sqlc terbatas (ditentukan di kode), jadi aman sebagai label | — | — |
| `app_db_tx_retries_total` | counter | `reason` (`deadlock`, `serialization`) | Retry `WithTx` | — | ADR-011 |
| `app_db_deadlocks_total` | counter | — | `40P01` terdeteksi (sebelum retry) | — | Step 7 `01` §4 |
| `app_db_lock_timeouts_total` | counter | `kind` (`resource`, `idempotency`) | `55P03` → `RESOURCE_BUSY` / `REQUEST_IN_PROGRESS` | — | ADR-L02/L03 |
| `app_db_constraint_violations_total` | counter | `constraint` (nama constraint terpetakan) | Pelanggaran constraint yang terpetakan ke error domain. Lonjakan constraint yang "seharusnya ditangkap APP lebih dulu" = indikasi bug | — | Step 3 `02` |
| `app_db_unmapped_errors_total` | counter | `pg_code` | Error DB tanpa pemetaan → 500 | A-06 | Step 6 T-SH-21 |
| `app_job_enqueued_total` | counter | `kind` | Job ditulis ke outbox | — | Step 7 `03` §10 |
| `app_job_queue_depth` | gauge | `kind`, `status` | Jumlah job per status (query periodik 15 dtk) | — | Step 7 |
| `app_job_oldest_age_seconds` | gauge | `kind` | `now() − min(run_at)` untuk job `queued` yang jatuh tempo | A-05 | Step 7 |
| `app_outbox_lag_seconds` | gauge | — | Umur job tertua yang belum pernah diklaim (`attempts = 0`): seberapa lama side effect tertunda sejak commit | A-03 | Step 7, SLO |
| `app_backup_last_success_timestamp_seconds` | gauge | `db` | Waktu backup terakhir yang sukses (via textfile collector node_exporter; ditambahkan dari Step 9 `03`). Alert jika > 26 jam | — | Step 9
| `app_job_duration_seconds` | histogram (JOB) | `kind`, `outcome` (`ok`, `retry`, `dead`, `fenced`) | Durasi eksekusi | — | Step 7 |
| `app_job_failures_total` | counter | `kind`, `class` (`retryable`, `permanent`) | Kegagalan per percobaan | — | Step 7 |
| `app_job_dead_total` | counter | `kind` | Job masuk DLQ | A-04 | ADR-006 |
| `app_job_lease_expired_total` | counter | `kind` | Klaim mengambil job dengan lease kedaluwarsa (worker sebelumnya mati/lambat) | — | Step 7 |
| `app_job_lease_lost_total` | counter | `kind` | Worker kehilangan lease saat heartbeat | — | Step 7 |
| `app_job_fenced_total` | counter | `kind` | `CompleteInTx` 0 baris → transaksi di-rollback (zombie worker) | — | ADR-006, PFS-02 |
| `app_outbound_requests_total` | counter | `target` (`provider`, `bank`, `merchant_webhook`, `storage`), `outcome` (`ok`, `http_4xx`, `http_5xx`, `timeout`, `conn_error`, `blocked`) | Panggilan HTTP keluar | — | — |
| `app_outbound_request_duration_seconds` | histogram (HTTP) | `target` | Latency panggilan keluar | — | — |
| `app_authn_failures_total` | counter | `reason` (`invalid_credentials`, `locked`, `token_invalid`, `token_expired`) | Kegagalan autentikasi | — | Step 6 T-SH-01/03 |
| `app_authn_lockouts_total` | counter | — | Akun dikunci (lockout bertahap) | — | C-03 |
| `app_refresh_reuse_detected_total` | counter | — | Reuse refresh token → family dicabut | A-11 | C-05 |
| `app_csrf_rejected_total` | counter | `reason` (`missing_header`, `bad_origin`) | Refresh/logout ditolak | — | C-08 |
| `app_authz_denied_total` | counter | `outcome` (`forbidden_403`, `hidden_404`) | Penolakan otorisasi. Lonjakan `hidden_404` dari satu principal = indikasi enumerasi/BOLA | — | Step 6 T-SH-13 |
| `app_idempotency_replays_total` | counter | `route` | Respons replay (`Idempotent-Replayed`) | — | ADR-L02 |
| `app_idempotency_key_reused_total` | counter | `route` | `422 IDEMPOTENCY_KEY_REUSED` | — | ADR-L02 |
| `app_invalid_state_transitions_total` | counter | `entity` | Guard `WHERE status=…` 0 baris → 409 | — | Step 3 |
| `app_pii_ciphertext_by_version` | gauge | `key_version` | Jumlah ciphertext per versi kunci (untuk memantau re-encrypt; dipakai PayRun) | — | Step 6 `04` §6.3 |

> `csp_violation_total` (Step 6 T-SH-09) **tidak** dijadikan metrik V1. Laporan CSP (`report-to`) membutuhkan endpoint penerima publik yang menambah permukaan serangan. Pelanggaran CSP diuji lewat E2E (Step 8 `02`). V2: endpoint report terpisah dengan rate limit.

### 3.2 LedgerPay (`ledgerpay_`)

**Integritas ledger (INV)**

| Metrik | Tipe | Label | Arti | Alert | Sumber |
|---|---|---|---|---|---|
| `ledgerpay_ledger_balance_drift_accounts` | gauge **INV** | — | Jumlah akun materialized dengan `balance ≠ Σ entries` pada verifikasi terakhir (job `ledger.verify`) | A-01 | LI-03, ADR-L01 |
| `ledgerpay_ledger_unbalanced_journals` | gauge **INV** | — | Jumlah journal dengan Σ D ≠ Σ C atau < 2 entry (verifikasi; seharusnya mustahil karena deferred trigger) | A-01 | LI-01 |
| `ledgerpay_ledger_global_imbalance_minor` | gauge **INV** | — | Σ seluruh debit − Σ seluruh kredit (minor unit). Normalnya 0 | A-01 | LI-01, ADR-L01 |
| `ledgerpay_ledger_mutation_attempts_total` | counter **INV** | `table` | Trigger append-only menolak UPDATE/DELETE pada `journals`/`journal_entries` | A-01 | LI-04 |
| `ledgerpay_payment_refund_drift_intents` | gauge **INV** | — | Intent dengan `refunded_amount ≠ Σ refunds` (verifikasi) | A-01 | LI-08 |
| `ledgerpay_ledger_verification_last_success_timestamp_seconds` | gauge | — | Waktu verifikasi terakhir yang sukses. Kalau job verifikasi mati, gauge INV di atas jadi basi, jadi alert-nya ikut memantau ini | A-01b | — |

**Pergerakan uang**

| Metrik | Tipe | Label | Arti | Alert | Sumber |
|---|---|---|---|---|---|
| `ledgerpay_transfers_total` | counter | `outcome` (`completed`, `insufficient_balance`, `invalid_recipient`, `self_transfer`, `frozen`, `limit_exceeded`) | Hasil transfer (eksekusi pertama, bukan replay) | — | LF-04 |
| `ledgerpay_topups_total` | counter | `status` (`created`, `succeeded`, `failed`, `expired`, `late_credited`) | Transisi top-up | — | LF-07 |
| `ledgerpay_payments_total` | counter | `outcome` (`succeeded`, `expired`, `insufficient_balance`, `double_confirm_rejected`) | Hasil konfirmasi pembayaran | — | LF-10, LI-13 |
| `ledgerpay_refunds_total` | counter | `outcome` (`succeeded`, `exceeds_payment`, `insufficient_balance`) | Hasil refund | — | LF-16 |
| `ledgerpay_refund_amount_minor_total` | counter | — (**tanpa** label `merchant`) | Nominal refund. Analisis per merchant lewat SQL/ops console, bukan metrik | — | Step 6 T-LP (refund abuse) |
| `ledgerpay_payouts_total` | counter | `status` (`requested`, `submitted`, `succeeded`, `failed`, `unknown`, `needs_reconciliation`) | Transisi payout | — | LF-11, ADR-L06 |
| `ledgerpay_payout_unknown` | gauge | — | Jumlah payout berstatus `UNKNOWN` saat ini | — | ADR-L06 |
| `ledgerpay_payout_unknown_oldest_age_seconds` | gauge | — | Umur payout `UNKNOWN` tertua | A-02 | ADR-L06, SLO |
| `ledgerpay_payout_status_checks_total` | counter | `result` (`succeeded`, `failed`, `still_unknown`, `not_found`) | Hasil status query | — | ADR-L06 |
| `ledgerpay_payout_provider_mismatch_total` | counter | — | Respons provider tidak cocok (amount/rekening) → `UNKNOWN` + recon | — | Amandemen ADR-L06 |
| `ledgerpay_payout_resolution_seconds` | histogram (`JOB` bucket + 600, 1800, 3600, 21600) | `path` (`direct`, `status_check`, `recon`, `manual`) | Waktu dari `UNKNOWN` sampai final (SLI "≥ 95% < 1 jam") | — | SLO |

**Webhook**

| Metrik | Tipe | Label | Arti | Alert | Sumber |
|---|---|---|---|---|---|
| `ledgerpay_webhook_inbox_received_total` | counter | `outcome` (`accepted`, `duplicate`, `signature_invalid`, `stale_timestamp`) | Hasil endpoint `/webhooks/provider` | — | ADR-L08 |
| `ledgerpay_webhook_signature_failures_total` | counter | — | Signature tidak valid (subset di atas, dipisah untuk alert) | A-10 | C-32 |
| `ledgerpay_webhook_inbox_processed_total` | counter | `outcome` (`applied`, `duplicate`, `stale`, `conflict`) | Hasil pemrosesan inbox (Step 4 O2) | — | Step 4 |
| `ledgerpay_webhook_late_total` | counter | — | Kredit dari webhook sukses setelah top-up `EXPIRED` (status lunak) | — | ADR-L08 |
| `ledgerpay_webhook_conflict_total` | counter | — | Konflik status final → recon item | — | FS-07 |
| `ledgerpay_webhook_inbox_pending` | gauge | — | Event `RECEIVED` yang belum diproses | — | Step 7 |
| `ledgerpay_webhook_inbox_dead_total` | counter | — | Event inbox → `DEAD` (8 percobaan, atau amount mismatch) | A-04 | ADR-L08 |
| `ledgerpay_webhook_verify_fetch_total` | counter | `outcome` (`match`, `mismatch`, `error`) | Verify-by-fetch sebelum kredit | — | Amandemen ADR-L08 |
| `ledgerpay_webhook_deliveries_total` | counter | `outcome` (`delivered`, `http_error`, `timeout`, `blocked`) | Percobaan webhook keluar ke merchant | — | LF-15 |
| `ledgerpay_webhook_delivery_dead_total` | counter | — | Delivery keluar → `dead` setelah 7 percobaan | A-04 | LF-15 |
| `ledgerpay_webhook_delivery_blocked_total` | counter | `reason` (`private_ip`, `metadata_ip`, `scheme`, `redirect`) | Egress guard SSRF menolak | — | C-44 |

**Reconciliation & ops**

| Metrik | Tipe | Label | Arti | Alert | Sumber |
|---|---|---|---|---|---|
| `ledgerpay_recon_runs_total` | counter | `outcome` (`completed`, `failed`) | Recon run | A-12 | LF-17 |
| `ledgerpay_recon_mismatches_total` | counter | `category` (5 kategori Step 3) | Mismatch baru ditemukan | — | LF-17 |
| `ledgerpay_recon_open_items` | gauge | `category` | Item `OPEN` saat ini | — | LF-18 |
| `ledgerpay_recon_open_item_oldest_age_seconds` | gauge | — | Umur item `OPEN` tertua | — | Step 3 `02` |
| `ledgerpay_ops_actions_total` | counter | `action` | Aksi ops ter-audit (resolve, retry, replay, API key) | — | LF-18 |
| `ledgerpay_merchant_api_requests_total` | counter | `route`, `status` (tanpa `merchant`) | Trafik API merchant | — | Step 6 `04` |

### 3.3 PayRun (`payrun_`)

**Integritas & isolasi (INV)**

| Metrik | Tipe | Label | Arti | Alert | Sumber |
|---|---|---|---|---|---|
| `payrun_tenant_isolation_violations_total` | counter **INV** | `site` (`repo_decode`, `job_handler`, `storage_key`) | Pemeriksaan defensif menemukan baris/objek dengan `tenant_id ≠ tenant context` (lihat catatan di bawah) | A-13 | PF-24, T-PR-01 |
| `payrun_finalized_mutation_attempts_total` | counter **INV** | `table` | Trigger `P0001` menolak perubahan item/line/snapshot run final | A-13 | PI-04 |
| `payrun_period_lock_bypass_total` | counter **INV** | `table` | Trigger `P0002` menolak penulisan yang **lolos** dari `PeriodGuard` APP (jalur yang lupa memanggil guard) | A-13 | PI-14 |
| `payrun_payroll_total_mismatch_runs` | gauge **INV** | — | Run final dengan Σ item ≠ total (job `payrun verify-run`) | A-13 | PI-05 |
| `payrun_reproducibility_failures_total` | counter **INV** | — | Recompute dari snapshot ≠ hasil tersimpan (job verifikasi sampel run final) | A-13 | PI-12 |
| `payrun_verification_last_success_timestamp_seconds` | gauge | — | Waktu verifikasi terakhir yang sukses | A-01b | — |

> **Bagaimana `payrun_tenant_isolation_violations_total` bisa terisi**, padahal RLS seharusnya membuatnya mustahil: lapisan repository memeriksa `row.tenant_id == ctx.Tenant` saat men-decode setiap baris tenant-scoped (biaya satu perbandingan UUID). Handler job memeriksa `jobs.tenant_id == payload.tenant_id` (jika ada di payload). Pembuat presigned URL memeriksa prefix `tenants/{ctx.tenant}/`. Kalau RLS salah konfigurasi (policy hilang, `FORCE` terlupa), metrik ini yang pertama menyala, **dan** request tersebut digagalkan (`500` + log `ERROR tenant.isolation_violation`). Di CI, catalogue test RLS (C-36) mencegah konfigurasi itu sampai ke deploy. Jadi metrik ini adalah detektor lapis kedua, bukan satu-satunya penjaga.

**Payroll run**

| Metrik | Tipe | Label | Arti | Alert | Sumber |
|---|---|---|---|---|---|
| `payrun_payroll_runs_total` | counter | `outcome` (`calculated`, `failed`, `approved`, `rejected`, `finalized`, `cancelled`) | Transisi run | A-12 | ADR-P05 |
| `payrun_payroll_run_duration_seconds` | histogram (`RUN`) | `size_bucket` (`lt_100`, `lt_1k`, `lt_10k`, `ge_10k`) | Durasi `calc_started_at` → `CALCULATED` | — | SLO, PF-24 |
| `payrun_payroll_items_calculated_total` | counter | `status` (`ok`, `error`) | Item yang dihitung | — | PF-24 |
| `payrun_payroll_item_errors_total` | counter | `code` (kode validasi engine, katalog tertutup: `NEGATIVE_NET`, `COMPONENT_CYCLE`, …) | Item `ERROR` per kode | — | Step 3 `06` §13 |
| `payrun_payroll_chunk_duration_seconds` | histogram (`JOB`) | — | Durasi txRead + compute + txWrite per chunk | — | Step 7 §5 |
| `payrun_run_stale_rejections_total` | counter | — | Approve/finalize ditolak `RUN_STALE` | — | ADR-P07, O12 |
| `payrun_approve_stale_candidates` | histogram (1, 5, 10, 50, 100, 500, 1000) | — | Jumlah kandidat change log per approve (biaya O12) | — | Step 7 O12 |
| `payrun_approvals_total` | counter | `subject` (`leave`, `overtime`, `payroll_run`), `decision` (`approved`, `rejected`, `cancelled`) | Keputusan approval | — | ADR-P09 |
| `payrun_approval_self_approved_total` | counter | — | Self-approve run (pengecualian satu admin), dipantau | — | ADR-P09 |
| `payrun_membership_sod_conflicts` | gauge | — | Jumlah membership aktif dengan `HR_ADMIN` + `PAYROLL_ADMIN` | — | Amandemen ADR-010 |
| `payrun_period_lock_rejections_total` | counter | `source` (`attendance`, `leave`, `overtime`, `salary`, `employment`) | `409 PERIOD_LOCKED` dari `PeriodGuard` (perilaku normal) | — | PI-14 |

**Dokumen & import**

| Metrik | Tipe | Label | Arti | Alert | Sumber |
|---|---|---|---|---|---|
| `payrun_payslips_rendered_total` | counter | `outcome` (`rendered`, `failed`) | Render PDF | — | PF-22 |
| `payrun_payslips_pending` | gauge | — | Payslip `PENDING`/`FAILED` saat ini | — | PFS-10 |
| `payrun_payslip_render_duration_seconds` | histogram (`JOB`) | — | Per batch 20 slip | — | Step 7 `03` |
| `payrun_payslip_access_total` | counter | `actor` (`owner_employee`, `admin`) | Akses slip. Akses `admin` selalu ter-audit | — | PI-17 |
| `payrun_attendance_imports_total` | counter | `outcome` (`completed`, `failed`, `duplicate`) | Import CSV | — | PF-21 |
| `payrun_attendance_import_rows_total` | counter | `result` (`ok`, `error`) | Baris diproses | — | PF-21 |

---

## 4. Pemetaan nama lama → nama final

Dokumen Step 1–7 tidak diubah satu per satu. Tabel ini menjadi rujukan, dan nama final dipakai sejak Step 8.

| Nama di dokumen sebelumnya | Nama final | Catatan |
|---|---|---|
| `ledger_balance_drift_total` | `ledgerpay_ledger_balance_drift_accounts` | Hasil verifikasi → gauge |
| `ledger_unbalanced_journal_total` | `ledgerpay_ledger_unbalanced_journals` | Gauge |
| `ledger_imbalance_total` | `ledgerpay_ledger_global_imbalance_minor` | Gauge, satuan minor unit |
| `ledger_mutation_attempt_total`, `ledger_constraint_violation_total` | `ledgerpay_ledger_mutation_attempts_total` + `app_db_constraint_violations_total{constraint}` | Dipisah: trigger append-only vs constraint lain |
| `payment_refund_drift_total` | `ledgerpay_payment_refund_drift_intents` | Gauge |
| `payment_double_confirm_rejected_total` | `ledgerpay_payments_total{outcome="double_confirm_rejected"}` | Dilebur |
| `payout_unknown_total` | `ledgerpay_payout_unknown` (gauge) + `ledgerpay_payouts_total{status="unknown"}` (counter) | Keadaan vs event |
| `payout_unknown_oldest_seconds`, `payout_unknown_oldest_age_seconds` | `ledgerpay_payout_unknown_oldest_age_seconds` | |
| `idempotency_replay_total` | `app_idempotency_replays_total{route}` | Shared |
| `idempotency_conflict_total` | `app_idempotency_key_reused_total{route}` | |
| `idempotency_wait_timeout_total` | `app_db_lock_timeouts_total{kind="idempotency"}` | |
| `webhook_signature_failure_total` | `ledgerpay_webhook_signature_failures_total` | |
| `webhook_signature_valid_total`, `webhook_duplicate_total` | `ledgerpay_webhook_inbox_received_total{outcome}` | Dilebur |
| `webhook_status_conflict_total`, `webhook_conflict_total` | `ledgerpay_webhook_conflict_total` | |
| `webhook_late_total` | `ledgerpay_webhook_late_total` | |
| `webhook_inbox_dead_total` | `ledgerpay_webhook_inbox_dead_total` | |
| `webhook_inbox_pending` | `ledgerpay_webhook_inbox_pending` | |
| `webhook_delivery_failure_total` | `ledgerpay_webhook_deliveries_total{outcome!="delivered"}` | |
| `webhook_delivery_dead_total` | `ledgerpay_webhook_delivery_dead_total` | |
| `webhook_delivery_blocked_total` | `ledgerpay_webhook_delivery_blocked_total{reason}` | |
| `refund_amount_total{merchant}` | `ledgerpay_refund_amount_minor_total` | **Label `merchant` dihapus** (kardinalitas tak terbatas) |
| `recon_mismatch_total` | `ledgerpay_recon_mismatches_total{category}` | |
| `recon_run_failed_total` | `ledgerpay_recon_runs_total{outcome="failed"}` | |
| `recon_open_items_oldest_age_seconds` | `ledgerpay_recon_open_item_oldest_age_seconds` | |
| `merchant_api_requests_total` | `ledgerpay_merchant_api_requests_total` | Tanpa label merchant |
| `job_queue_depth` | `app_job_queue_depth` | |
| `job_oldest_age_seconds` | `app_job_oldest_age_seconds` | |
| `job_dead_total` | `app_job_dead_total` | |
| `job_lease_expired_total` | `app_job_lease_expired_total` | |
| `job_lease_lost_total` | `app_job_lease_lost_total` | |
| `job_fenced_total` | `app_job_fenced_total` | |
| `job_failures_total`, `job_retry_total` | `app_job_failures_total{class}` | |
| `job_enqueued_total` | `app_job_enqueued_total` | |
| `job_duration_seconds` | `app_job_duration_seconds` | |
| `outbox_lag_seconds` | `app_outbox_lag_seconds` | |
| `db_deadlock_total` | `app_db_deadlocks_total` | |
| `db_tx_retries_total` | `app_db_tx_retries_total` | |
| `db_pool_acquire_timeout_total` | `app_db_pool_acquire_timeout_total` | |
| `db_pool_wait_seconds` | `app_db_pool_acquire_duration_seconds` | |
| `db_constraint_violation_total` | `app_db_constraint_violations_total` | |
| `db_unexpected_error_total` | `app_db_unmapped_errors_total` | |
| `http_requests_total` | `app_http_requests_total` | |
| `http_request_rejected_total`, `validation_error_total` | `app_http_rejected_total{reason}` + `app_errors_total{code="VALIDATION_ERROR"}` | |
| `invalid_state_transition_total` | `app_invalid_state_transitions_total{entity}` | |
| `rate_limited_total` | `app_rate_limited_total{tier}` | |
| `auth_login_failed_total`, `auth_token_invalid_total` | `app_authn_failures_total{reason}` | |
| `authn_lockout_total`, `auth_account_locked_total` | `app_authn_lockouts_total` | |
| `refresh_reuse_detected_total`, `auth_refresh_reuse_total` | `app_refresh_reuse_detected_total` | |
| `auth_csrf_rejected_total` | `app_csrf_rejected_total{reason}` | |
| `authz_denied_total` | `app_authz_denied_total{outcome}` | |
| `pii_ciphertext_version_total` | `app_pii_ciphertext_by_version{key_version}` | Gauge |
| `csp_violation_total` | — (tidak dibuat di V1) | Lihat catatan §3.1 |
| `tenant_isolation_violation_total` | `payrun_tenant_isolation_violations_total{site}` | |
| `payroll_finalized_mutation_attempt_total` | `payrun_finalized_mutation_attempts_total{table}` | |
| `period_lock_bypass_total` | `payrun_period_lock_bypass_total{table}` | |
| `payroll_total_mismatch_total` | `payrun_payroll_total_mismatch_runs` | Gauge |
| `payroll_reproducibility_failure_total` | `payrun_reproducibility_failures_total` | |
| `payroll_run_duration_seconds` | `payrun_payroll_run_duration_seconds{size_bucket}` | |
| `payroll_items_calculated_total` | `payrun_payroll_items_calculated_total{status}` | |
| `payroll_item_errors_total` | `payrun_payroll_item_errors_total{code}` | |
| `payroll_run_stale_total` | `payrun_run_stale_rejections_total` | |
| `approval_self_approved_total` | `payrun_approval_self_approved_total` | |
| `payslip_pending_total` | `payrun_payslips_pending` | Gauge |

Bukan metrik (cocok dengan pola grep tetapi merupakan kolom/field): `failed_login_count`, `*_count` di recon/payroll summary (`item_ok_count`, `missing_internal_count`, …), `raw_body_bytes`, `expires_in_seconds`.

---

## 5. Tracing

### 5.1 Span

| Span | Dibuat oleh | Nama | Atribut (tanpa PII) |
|---|---|---|---|
| HTTP server | `otelhttp` middleware | `POST /v1/transfers` (template rute) | `http.route`, `http.response.status_code`, `app.request_id`, `app.user_id` / `app.merchant_id` / `app.tenant_id` |
| Use case | Service layer (manual, hanya use case utama) | `transfer.execute`, `payout.submit`, `payroll.calc_chunk` | ID bisnis (`app.payout_id`, `app.run_id`, `app.chunk_no`) |
| Transaksi DB | Wrapper `WithTx` | `db.tx` | `db.tx.attempt`, `db.tx.outcome` (`commit`/`rollback`/`retry`) |
| Query DB | `otelpgx` | Nama query sqlc (`PostJournalEntries`) | `db.system=postgresql`, `db.operation`. **Tanpa** nilai parameter, dan tanpa teks SQL lengkap (nama query sudah cukup dan bebas PII) |
| Job | Worker runner | `job <kind>` | `app.job_id`, `app.job_kind`, `app.attempt`, `app.correlation_id`; *link* ke span producer |
| HTTP keluar | `otelhttp.Transport` | `POST provider /v1/payouts` | `server.address`, `http.response.status_code`, `app.outbound.target` |

### 5.2 Atribut yang dilarang
Email, nama, NIK/NPWP, nomor rekening, nominal gaji, isi body, token, API key, query string yang berisi data pengguna. Nominal transaksi LedgerPay **juga tidak** dimasukkan sebagai atribut span. Nominal ada di DB dan log bisnis yang dikontrol, dan trace tidak perlu memuatnya. Semua atribut span melalui helper `trace.SafeAttrs(...)` dengan allowlist kunci.

### 5.3 Menyambung request → job (keputusan)
**`traceparent` disimpan di kolom `jobs.trace_parent text NULL`**, bukan di payload. Kolom ini ditulis saat enqueue (di transaksi bisnis yang sama) dari span aktif.

- Alasan memakai kolom: payload adalah kontrak domain (Step 7 `03`, hanya berisi ID). Konteks tracing adalah metadata infrastruktur yang dibaca runner secara generik, sama seperti `correlation_id`.
- Worker membuat span `job <kind>` sebagai **root baru dengan link** ke span producer, bukan sebagai child. Job bisa berjalan menit atau jam kemudian, dan trace dengan durasi berjam-jam sulit dibaca. Link tetap membuat Jaeger bisa melompat dari request ke job.
- Job turunan menyalin `trace_parent` dari span job induk.
- **Delta skema:** tambahkan `trace_parent text NULL` ke `jobs` (Step 4 `01-shared-tables.md`). Dicatat sebagai open item di `00-index.md`.

### 5.4 Sampling
- **V1 (demo/staging):** 100% (`AlwaysSample`). Volumenya kecil, dan saat demo setiap trace harus ada.
- **V2:** `ParentBased(TraceIDRatioBased(0.1))`, ditambah *tail sampling* di collector: selalu simpan trace dengan error, latency > p99, atau span `payout.*`/`payroll_run.*`.
- Exporter OTLP berjalan non-blocking (batch processor, antrean terbatas, drop saat penuh) supaya Jaeger yang mati tidak memperlambat request.

---

## 6. Dashboard

Semua dashboard berupa JSON di `deploy/grafana/dashboards/` dan di-provision otomatis (`deploy/grafana/provisioning/`). Tidak ada dashboard yang dibuat lewat klik di UI tanpa diekspor ke repo.

> **Amandemen Step 12 (S15): V1 hanya 3 dashboard.** (1) **LedgerPay — Money & Integrity** = gabungan Money Movement + Integrity & Recon; (2) **Platform** = gabungan HTTP & DB + Jobs; (3) **PayRun — Payroll & Tenancy** = gabungan Payroll Runs + Tenancy & Security. Panel di tabel di bawah tetap menjadi acuan isi; pemisahan menjadi 6 dashboard = V2. Panel "Explore logs" hanya aktif jika profile `obs-full` (Loki) menyala.

| Dashboard | Panel | Pertanyaan yang dijawab |
|---|---|---|
| **LedgerPay — Money Movement** | Rate transfer/payment/refund per `outcome`; top-up per status; payout per status; `payout_unknown` + umur tertua; distribusi `payout_resolution_seconds` per `path`; idempotency replay rate; p95 latency endpoint uang | "Apakah uang bergerak normal? Berapa yang menggantung, dan sudah berapa lama?" |
| **LedgerPay — Integrity & Recon** | Tiga gauge INV (drift, unbalanced, global imbalance) dengan warna merah jika ≠ 0; waktu verifikasi terakhir; mutation attempts; inbox `processed{outcome}` (applied/duplicate/stale/conflict); late webhook; verify-fetch mismatch; recon open items per kategori + umur; ops actions | "Apakah ledger masih benar? Apa yang lolos dari mekanisme otomatis?" |
| **Platform — HTTP & DB** | RPS per route; rasio 5xx; p50/p95/p99 per route (garis SLO); `app_errors_total` top code; rate limited per tier; pool acquired vs max; acquire duration & timeout; query p95 top 10 (`query`); deadlock/tx retry; lock timeout per kind | "Lambat atau error di mana: aplikasi, pool, atau query tertentu?" |
| **Platform — Jobs** | Queue depth per kind/status; oldest age per kind; outbox lag; durasi per kind; failures per class; dead per kind; lease expired/lost/fenced; outbound requests per target × outcome | "Apakah side effect berjalan? Ada worker macet atau zombie?" |
| **PayRun — Payroll Runs** | Run per outcome; durasi run per `size_bucket`; item error per code; chunk duration; stale rejections + distribusi kandidat approve; approvals per subject/decision; self-approved; payslip pending + render failures; import rows ok/error | "Apakah payroll bulan ini akan selesai tepat waktu? Di mana macetnya?" |
| **PayRun — Tenancy & Security** | INV: isolation violations per `site`, finalized mutation attempts, period lock bypass, total mismatch, reproducibility failures; authz denied (403 vs 404); authn failures/lockouts; refresh reuse; CSRF rejected; payslip access oleh admin; SoD conflicts; distribusi `pii_ciphertext_by_version` | "Apakah ada tanda kebocoran, eskalasi, atau penyalahgunaan akses?" |

Setiap panel INV diberi tautan "Runbook" ke §8 dan tautan "Explore logs" yang sudah berisi LogQL dengan filter `msg` yang relevan.

---

## 7. Alert rules

File `deploy/prometheus/rules/{platform,ledgerpay,payrun}.yml`. Severity: **critical** (butuh tindakan segera; di V1 notifikasi webhook lokal), **warning** (ditinjau di jam kerja).

| ID | Nama | Expr (PromQL) | For | Severity | Runbook |
|---|---|---|---|---|---|
| A-01 | `LedgerInvariantViolated` | `max(ledgerpay_ledger_balance_drift_accounts) > 0 or max(ledgerpay_ledger_unbalanced_journals) > 0 or abs(max(ledgerpay_ledger_global_imbalance_minor)) > 0 or max(ledgerpay_payment_refund_drift_intents) > 0 or increase(ledgerpay_ledger_mutation_attempts_total[5m]) > 0` | 0m | critical | §8.1 |
| A-01b | `IntegrityVerificationStale` | `time() - max(ledgerpay_ledger_verification_last_success_timestamp_seconds) > 2*3600 or time() - max(payrun_verification_last_success_timestamp_seconds) > 26*3600` | 10m | warning | §8.1 |
| A-02 | `PayoutUnknownTooOld` | `max(ledgerpay_payout_unknown_oldest_age_seconds) > 3600` | 5m | warning | §8.2 |
| A-03 | `OutboxLagHigh` | `max(app_outbox_lag_seconds) > 30` | 5m | warning | §8.3 |
| A-04 | `JobsDeadLettered` | `sum by (kind) (increase(app_job_dead_total[10m])) > 0 or increase(ledgerpay_webhook_inbox_dead_total[10m]) > 0` | 0m | warning | §8.3 |
| A-05 | `JobBacklogOld` | `max by (kind) (app_job_oldest_age_seconds) > 300` | 10m | warning | §8.3 |
| A-06 | `High5xxRatio` | `sum by (service) (rate(app_http_requests_total{status=~"5.."}[5m])) / sum by (service) (rate(app_http_requests_total[5m])) > 0.02` | 5m | critical | §8.4 |
| A-07 | `TransferLatencySLO` | `histogram_quantile(0.95, sum by (le) (rate(app_http_request_duration_seconds_bucket{route="POST /v1/transfers"}[10m]))) > 0.2` | 10m | warning | §8.4 |
| A-08 | `DBPoolSaturated` | `max by (service) (app_db_pool_acquired_conns / app_db_pool_max_conns) > 0.9 or sum by (service) (increase(app_db_pool_acquire_timeout_total[5m])) > 0` | 5m | warning | §8.4 |
| A-09 | `ServiceNotReady` | `min by (service) (app_ready) == 0 or up{job=~"ledgerpay-.*\|payrun-.*"} == 0` | 2m | critical | §8.4 |
| A-10 | `WebhookSignatureFailures` | `increase(ledgerpay_webhook_signature_failures_total[10m]) > 10` | 0m | warning | §8.5 |
| A-11 | `RefreshTokenReuseSpike` | `increase(app_refresh_reuse_detected_total[15m]) > 5` | 0m | warning | §8.5 |
| A-12 | `BatchJobFailed` | `increase(payrun_payroll_runs_total{outcome="failed"}[15m]) > 0 or increase(ledgerpay_recon_runs_total{outcome="failed"}[1h]) > 0` | 0m | warning | §8.3 |
| A-13 | `PayRunInvariantViolated` | `sum(increase(payrun_tenant_isolation_violations_total[5m])) > 0 or sum(increase(payrun_finalized_mutation_attempts_total[5m])) > 0 or sum(increase(payrun_period_lock_bypass_total[5m])) > 0 or max(payrun_payroll_total_mismatch_runs) > 0 or increase(payrun_reproducibility_failures_total[1h]) > 0` | 0m | critical | §8.6 |
| A-14 | `BackupStale` (Step 12: R4-11) | `time() - max(app_backup_last_success_timestamp_seconds) > 26*3600` | 10m | warning | Step 9 `03` runbook "restore from backup" / cek job backup |
| A-15 | `DiskAlmostFull` (Step 12: R4-11) | `1 - node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"} > 0.85` (node_exporter) | 15m | warning | Step 9 `03` runbook "disk full" |
| A-16 | `TLSCertExpiringSoon` (Step 12: R4-11) | `min(app_tls_cert_expiry_timestamp_seconds) - time() < 14*86400`. Metrik diisi cron harian di VM (`openssl s_client` per domain → textfile collector node_exporter), cara paling sederhana tanpa blackbox exporter | 1h | warning | Cek log ACME Caddy, DNS, port 80 terbuka; Step 9 `03` provisioning |

Catatan:
- `increase()` pada counter yang baru muncul bisa memberi false negative pada sampel pertama. Untuk metrik INV, counter diinisialisasi ke 0 saat startup untuk setiap nilai label yang diketahui (`table`, `site`), sehingga kenaikan pertama pun tertangkap.
- A-07 sengaja dibuat sederhana (ambang p95 langsung), bukan *multi-window burn rate*. Volume demo terlalu kecil untuk burn rate yang bermakna. Burn rate adalah peningkatan V2.

---

## 8. Runbook

Format: **Dampak → Lihat dulu → Investigasi → Pulihkan → Setelahnya**. Query log memakai Loki (`{project="…"} | json | …`).

### 8.1 `LedgerInvariantViolated` / `IntegrityVerificationStale`
- **Dampak:** saldo mungkin salah, atau ledger tidak bisa dipercaya. Ini insiden tertinggi di LedgerPay.
- **Lihat dulu:** dashboard *LedgerPay — Integrity & Recon*: gauge mana yang ≠ 0 dan sejak kapan (bandingkan dengan deploy terakhir: `app_build_info`).
- **Investigasi:**
  ```sql
  -- akun yang drift (dibaca dalam REPEATABLE READ READ ONLY)
  SELECT a.id, a.balance, COALESCE(SUM(CASE e.direction WHEN 'C' THEN e.amount ELSE -e.amount END),0) AS from_entries
  FROM accounts a LEFT JOIN journal_entries e ON e.account_id = a.id
  WHERE NOT a.is_system GROUP BY a.id HAVING a.balance <> COALESCE(SUM(CASE e.direction WHEN 'C' THEN e.amount ELSE -e.amount END),0);
  ```
  Arah tanda mengikuti normal side akun liability (kredit menambah). Cari journal terakhir yang menyentuh akun tersebut, lalu `correlation_id`-nya: `{project="ledgerpay"} | json | journal_id="<id>"`. Kalau `ledger_mutation_attempts_total` naik: `{project="ledgerpay", level="ERROR"} | json | msg="ledger.invariant_violation"`, dan periksa apakah ada migrasi/akses manual (audit log, `pg_stat_activity`).
- **Pulihkan:** **jangan** UPDATE saldo. Bekukan akun terdampak (`FROZEN`) lewat ops. Koreksi dengan journal `ADJUSTMENT_MANUAL` yang ber-referensi dan ter-audit, setelah akar masalah dipahami. Kalau penyebabnya bug di versi baru, rollback deploy (Step 9).
- **Setelahnya:** postmortem; tambah test yang mereproduksi; tinjau apakah constraint DB bisa mencegahnya.
- *Stale:* job `ledger.verify` / `payrun verify-run` tidak berjalan → periksa `app_job_dead_total{kind="ledger.verify"}` dan scheduler (§8.3).

### 8.2 `PayoutUnknownTooOld`
- **Dampak:** dana user tertahan > 1 jam (SLO).
- **Lihat dulu:** *Money Movement* → `payout_unknown`, umur tertua, distribusi `payout_status_checks_total{result}`.
- **Investigasi:** `GET /v1/ops/payouts?status=UNKNOWN` (Step 5). Untuk payout tertua: `{project="ledgerpay"} | json | payout_id="<id>"`, lalu cek apakah status check berjalan (`job_kind="payout.status_check"`) dan apa hasilnya. Hasil `still_unknown` terus-menerus berarti provider bermasalah; `not_found` berarti ikuti aturan safe window (ADR-L06). Cek juga `app_outbound_requests_total{target="bank",outcome}`.
- **Pulihkan:** kalau provider sehat dan status check gagal karena bug, perbaiki lalu retry job (`POST /v1/ops/jobs/{id}/retry`). Kalau ada bukti dari provider (laporan settlement/portal): `POST /v1/ops/payouts/{id}/resolve` dengan referensi bukti + alasan (wajib, `RESOLUTION_EVIDENCE_REQUIRED`). **Tidak pernah** submit ulang manual tanpa jawaban `NOT_FOUND` setelah safe window.
- **Setelahnya:** pastikan item masuk recon besok; catat waktu resolusi.

### 8.3 `OutboxLagHigh` / `JobsDeadLettered` / `JobBacklogOld` / `BatchJobFailed`
- **Dampak:** side effect tertunda (webhook merchant, payout, payslip, kalkulasi payroll).
- **Lihat dulu:** *Platform — Jobs*. Kind mana yang menumpuk? Apakah worker ready (`app_ready{service="worker"}`)? Apakah lease expired/fenced melonjak (worker crash-loop)?
- **Investigasi:**
  ```sql
  SELECT kind, status, count(*), min(run_at), max(attempts) FROM jobs
  WHERE status IN ('queued','running','dead') GROUP BY 1,2 ORDER BY 1,2;
  SELECT id, kind, attempts, last_error, correlation_id FROM jobs
  WHERE status = 'dead' ORDER BY updated_at DESC LIMIT 20;
  ```
  Telusuri `correlation_id` di Loki. Bedakan kegagalan dependency (`app_outbound_requests_total` error) dari bug (`error_class="permanent"`). Untuk payroll `FAILED`: `failure_reason` di run + item `ERROR` (`payrun_payroll_item_errors_total{code}`).
- **Pulihkan:** worker mati → restart (`docker compose restart worker`); job yang lease-nya habis akan diklaim ulang otomatis. Dependency mati → tunggu (backoff berjalan). Bug → perbaiki + deploy, lalu `POST /v1/ops/jobs/{id}/retry` untuk job `dead`. Webhook merchant `dead` → `POST /v1/ops/webhook-deliveries/{id}/replay` setelah merchant pulih. Payroll `FAILED` → HR memperbaiki data, lalu recalculate.
- **Setelahnya:** kalau satu kind sering dead, tinjau klasifikasi retryable/permanent dan `max_attempts`.

### 8.4 `High5xxRatio` / `TransferLatencySLO` / `DBPoolSaturated` / `ServiceNotReady`
- **Dampak:** user gagal bertransaksi atau lambat.
- **Lihat dulu:** *Platform — HTTP & DB*: route mana, code apa (`app_errors_total`), apakah bersamaan dengan saturasi pool atau lonjakan `app_db_unmapped_errors_total`.
- **Investigasi:** `{project=~"ledgerpay|payrun", level="ERROR"} | json | line_format "{{.route}} {{.error_code}} {{.err}}"`. Trace lambat di Jaeger (filter `http.route`, durasi > 200 ms): lihat span `db.tx` (menunggu lock?) vs query tertentu. Di DB: `SELECT * FROM pg_stat_activity WHERE state <> 'idle' ORDER BY xact_start`; lock: `pg_locks` join `pg_stat_activity` untuk `wait_event_type = 'Lock'`; query mahal: `pg_stat_statements` urut `mean_exec_time`.
- **Pulihkan:** transaksi `idle in transaction` dibatalkan otomatis oleh `idle_in_transaction_session_timeout` (10 dtk). Query liar → `pg_cancel_backend`. Lonjakan setelah deploy → rollback. Pool jenuh karena hot row (O11) → catat sebagai bukti untuk optimasi V2, dan sementara batasi rate tier `money-write`. Readiness gagal karena migrasi tidak cocok → jalankan job migrate yang tertinggal.
- **Setelahnya:** tambah index/ubah query berdasarkan `EXPLAIN ANALYZE` (Step 8 `03`).

### 8.5 `WebhookSignatureFailures` / `RefreshTokenReuseSpike`
- **Dampak:** kemungkinan serangan (webhook palsu/replay, pencurian refresh token), atau salah konfigurasi secret.
- **Investigasi:** webhook: `{project="ledgerpay"} | json | msg="webhook.signature_invalid"`, kelompokkan per IP sumber (dari log Caddy). Kalau semua event gagal setelah rotasi secret → salah konfigurasi. Kalau sporadis dari IP asing → serangan; karena verify-by-fetch dan HMAC, tidak ada saldo tercipta. Refresh reuse: `msg="auth.refresh_reuse_detected"` per `user_id` dan IP. Satu user dengan banyak IP berarti kemungkinan token dicuri.
- **Pulihkan:** salah konfigurasi → perbaiki secret (dual-secret window, Step 6 `04` §6.2). Serangan → blokir IP di Caddy/ufw. Untuk user terdampak, family sudah dicabut otomatis: paksa reset password dan beri notifikasi.
- **Setelahnya:** playbook Step 6 `04` §7.3.

### 8.6 `PayRunInvariantViolated`
- **Dampak:** potensi kebocoran lintas tenant atau payroll final yang berubah. Ini insiden tertinggi di PayRun.
- **Lihat dulu:** *PayRun — Tenancy & Security*: metrik INV mana yang naik, dan label `site`/`table`-nya.
- **Investigasi:** `{project="payrun", level="ERROR"} | json | msg=~"tenant.isolation_violation|payroll_run.finalized_mutation|period.lock_bypass"`, lalu ambil `request_id`, `route`, `tenant_id`, `user_id`. Periksa katalog RLS di DB produksi (query catalogue test C-36: tabel tanpa `relrowsecurity`/`relforcerowsecurity`, role app dengan `BYPASSRLS`). Untuk `period_lock_bypass`: cari jalur kode yang menulis tanpa `PeriodGuard` (route di log).
- **Pulihkan:** isolation violation → request sudah digagalkan oleh pemeriksaan defensif. Kalau konfigurasi RLS rusak, rollback migrasi/deploy segera dan nonaktifkan endpoint terdampak. Ikuti playbook Step 6 `04` §7.2 (dugaan kebocoran lintas tenant), termasuk penilaian kewajiban notifikasi UU PDP. Mutation/bypass → tidak ada data yang berubah (trigger menolak); perbaiki kode, deploy.
- **Setelahnya:** tambah kasus ke isolation test suite / authz matrix; postmortem wajib.

---

## 9. Observability di demo

Prinsip: setiap demo ditutup dengan **bukti dari sistem observability**, bukan hanya respons API. Layar dibagi dua: terminal/UI di kiri, Grafana/Jaeger/Loki di kanan.

| Demo | Yang ditampilkan | Detail |
|---|---|---|
| **LP-A Idempotency** | Panel `app_idempotency_replays_total{route="POST /v1/transfers"}` naik 1; log | `{project="ledgerpay"} \| json \| correlation_id="<id>"`: satu `transfer.completed`, dua `http.request` (status 201, yang kedua `idempotency_replayed=true`); gauge INV tetap 0 |
| **LP-B Concurrency** | *Money Movement*: `ledgerpay_transfers_total{outcome}` → tepat 10 `completed`, 90 `insufficient_balance`; *Platform*: p95 selama burst, `app_db_lock_timeouts_total` (idealnya 0), `app_db_deadlocks_total` = 0; *Integrity*: tiga gauge INV tetap 0 setelah `ledger.verify` dipicu manual | Trace salah satu request: span `db.tx` menunjukkan waktu menunggu lock |
| **LP-C Payout recovery** | Jaeger: trace `POST /v1/payouts` → link ke `job payout.submit` (span HTTP keluar timeout) → `job payout.status_check`; Loki: urutan log §1.6 dengan satu `correlation_id`; panel `ledgerpay_payout_unknown` naik ke 1 lalu turun ke 0; `app_job_lease_expired_total` (kalau worker di-kill di tengah); log mock menunjukkan payout diproses sekali | Panel `payout_resolution_seconds{path="status_check"}` |
| **LP-D Recon** | *Integrity & Recon*: `ledgerpay_recon_mismatches_total{category="STATUS_MISMATCH"}` +1, `recon_open_items` 1 → 0 setelah resolusi; `ledgerpay_ops_actions_total{action="resolve_recon_item"}`; audit log di ops console | — |
| **PR-A Tenant isolation** | Panel `app_authz_denied_total{outcome="hidden_404"}` naik saat user Alpha mencoba ID Beta; `payrun_tenant_isolation_violations_total` **tetap 0** (pemeriksaan defensif tidak pernah menyala karena RLS bekerja); output CI isolation suite | Log: `{project="payrun"} \| json \| user_id="<alpha>" \| status=404` |
| **PR-B Crash & resume** | *Payroll Runs*: progres chunk; `app_job_lease_expired_total{kind="payroll.calc_chunk"}` +1 setelah worker di-kill; `app_job_fenced_total` (jika zombie); durasi run; SQL `count(*)` item = jumlah karyawan | Jaeger: dua span `job payroll.calc_chunk` untuk chunk yang sama (attempt 1 terputus, attempt 2 sukses) |
| **PR-C Immutability** | `payrun_period_lock_rejections_total` naik (perilaku normal: `409 PERIOD_LOCKED`); `payrun_finalized_mutation_attempts_total` **tetap 0** kalau APP menolak duluan; naik hanya jika demo sengaja memanggil query mentah (menunjukkan trigger sebagai jaring terakhir); `payrun_run_stale_rejections_total` saat demo stale | — |
