# Step 8.3 — Performance Test Plan

> Bagian dari [Step 8](00-index.md). SLO (§4), prinsip performance (§6), dan O20 di `00-index.md` **mengikat**.
> Master prompt Phase 12: jangan hanya bilang "scalable". Tentukan bottleneck, ukur, lalu putuskan.
> **Semua angka di dokumen ini adalah skenario uji atau hipotesis**, bukan klaim kapasitas. Hasil nyata diisi setelah load test, dan selalu disertai spesifikasi mesin.

---

## 1. Analisis bottleneck

Format: *expected bottleneck → kenapa → cara mendeteksi → mitigasi yang sudah ada di desain → mitigasi lanjutan jika terbukti*.

### 1.1 Shared (kedua project)

| Area | Bottleneck yang diperkirakan | Kenapa | Deteksi | Sudah ada di desain | Jika terbukti |
|---|---|---|---|---|---|
| **Login (argon2id)** | **CPU dan memori**, bukan DB | argon2id m=64 MiB, t=3, p=2 ≈ 100–250 ms CPU per hash (Step 6 C-02). Di 2 vCPU, maksimal ±8–15 login/dtk. Lebih kritis: **64 MiB per hash yang berjalan bersamaan**. 20 login paralel ≈ 1,3 GB RAM, sehingga VM 4 GB bisa OOM | pprof CPU (`argon2.IDKey` dominan), RSS container, `app_http_request_duration_seconds{route="/auth/login"}` | Rate limit `auth-strict` (5/menit per IP+email), lockout | **Semaphore konkurensi hashing** (misalnya maksimal 4 hash bersamaan; sisanya menunggu ≤ 2 dtk lalu `503`). *Keputusan baru:* semaphore ini masuk **MVP**, karena tanpa itu login bisa dipakai sebagai vektor DoS memori. Lebih lanjut: turunkan `m` ke 19–32 MiB sambil menaikkan `t` (rekomendasi OWASP), atau pisahkan auth ke instance sendiri |
| **Connection pool** | Pool api (20) habis saat banyak request menunggu lock | Request yang menunggu `lock_timeout` 2 dtk memegang koneksi | `db_pool_in_use`, `db_pool_acquire_timeout_total`, `pg_stat_activity` (`wait_event_type = Lock`) | Acquire timeout 2 dtk → `503`; transaksi < 50 ms; tanpa network di dalam tx | Pool per kelas endpoint (uang vs baca); PgBouncer mode transaction (perhatian: `SET LOCAL` tetap aman, `SET` sesi tidak; prepared statement perlu `pgx` mode `QueryExecModeCacheDescribe`) |
| **Polling job** | Query claim berulang saat idle | Polling 500 ms × jumlah lane × replika | `pg_stat_statements` (calls claim query), CPU Postgres saat idle | Partial index claimable kecil; idle backoff hingga 2 dtk | `LISTEN/NOTIFY` (P2) untuk membangunkan lane |
| **Bloat `jobs`** | Index dan heap membengkak, claim melambat | Banyak UPDATE status + DELETE retensi | `pg_stat_user_tables.n_dead_tup`, ukuran index | Retensi 7 hari (batch), `fillfactor` (Step 4), autovacuum agresif per tabel | Partisi `jobs` per hari + `DROP PARTITION` (tanpa DELETE) |
| **JSON encoding** | Minor | Respons kecil, list dibatasi 100 | pprof (`encoding/json`) | Limit 100, field minimal | `goccy/go-json` / `jsoniter` hanya jika > 10% CPU |
| **Logging** | I/O stdout saat beban tinggi | Satu log per request + per job | pprof, throughput turun saat `level=debug` | `slog` JSON, `info` di perf env | Sampling log sukses (bukan error) |
| **Tracing** | Overhead span | Span per query pgx | Bandingkan p95 dengan dan tanpa OTel | Sampling `parentbased_traceidratio` (perf env 10%) | Matikan span per query, sisakan span per tx |

### 1.2 LedgerPay

| Area | Bottleneck yang diperkirakan | Kenapa | Deteksi | Sudah ada di desain | Jika terbukti |
|---|---|---|---|---|---|
| **Hot row wallet user** | Serialisasi per user | Disengaja (Demo B) | Lock wait per akun | Lock hanya ms | Tidak perlu: perilaku bisnis yang benar |
| **Hot row wallet merchant (O11)** | Throughput maksimal pembayaran ke satu merchant | Satu baris di-lock per pembayaran. Lock 2–5 ms → batas teoretis 200–500/dtk, dan antrean terasa saat utilisasi > ~70% | `pg_locks` + `pg_stat_activity` lock wait, p95/p99 confirm, eksperimen E-1 (§6) | `FOR NO KEY UPDATE`, tx pendek | `MERCHANT_PENDING` non-materialized + sweep (Step 7 O11 V2), **hanya** jika aturan keputusan E-1 terpenuhi |
| **Akun sistem** | Tidak ada (hipotesis) | Tidak materialized → hanya INSERT (ADR-L07) | Lock wait pada akun sistem harus 0 | ADR-L07 | — |
| **Insert `journal_entries`** | I/O + index maintenance (3 index) + deferred trigger keseimbangan (3× per journal 2 entry) | Tabel terbesar (20–25 juta baris di skenario) | `pg_stat_statements` (INSERT entries, fungsi trigger), WAL rate | Index minimal; trigger memakai index `journal_id` | Penanda per-tx untuk trigger (Step 4 catatan); partisi bulanan (V2) |
| **Riwayat wallet** | N+1 (detail polimorfik) dan offset scan | Lihat §2.1 | Jumlah query per request (log debug / `pg_stat_statements` calls), EXPLAIN | Cursor `(created_at, id)` + index `ix_journal_entries_account_created` INCLUDE | Read replica (V2) |
| **Verifikasi saldo (`ledger.verify`)** | Scan besar | Σ entries per akun untuk 100 ribu akun | Durasi job, buffers di EXPLAIN | Index-only scan `INCLUDE (direction, amount)`; RR-RO | Verifikasi inkremental sejak checkpoint (`account_balance_snapshots`, V2) |
| **Webhook inbox burst** | Lane inbox (4) dan verify-by-fetch ke provider | Setiap event = 1 panggilan HTTP ke provider + 1 tx | `webhook_inbox` RECEIVED count, umur event tertua, latency provider | Respons 200 cepat (insert saja), dedupe di DB | Naikkan lane inbox; batch verify jika provider mendukung |
| **Webhook keluar** | Merchant lambat menahan slot | Timeout 5 dtk × lane 8 | Durasi `webhook.deliver`, slot terpakai | Lane terpisah (bulkhead), timeout 5 dtk | Batas konkurensi per merchant |
| **Idempotency table** | Unique index besar | 1 baris per request uang × 7 hari | Ukuran tabel/index, latency INSERT | Retensi 7 hari | Partisi harian + drop |

### 1.3 PayRun

| Area | Bottleneck yang diperkirakan | Kenapa | Deteksi | Sudah ada di desain | Jika terbukti |
|---|---|---|---|---|---|
| **Kalkulasi payroll** | CPU engine **atau** input gathering | 10.000 karyawan × (prorata, komponen, lembur, pajak) | `payroll_chunk_duration_seconds`, pprof worker, breakdown txRead vs compute vs txWrite (span OTel) | Chunk 50, lane `payroll` 4, 7 query batch per chunk (§2.2) | Naikkan worker/lane; tuning chunk size (eksperimen PR-1) |
| **Upsert item/line** | Volume tulis | 10.000 item × ±12 line = 120 ribu baris per run | WAL, durasi txWrite | Satu txWrite per chunk; `INSERT … SELECT unnest(...)` batch, bukan INSERT per baris | `COPY` ke staging + merge (jarang perlu) |
| **Serialisasi chunk pada baris run** | Chunk paralel dari run yang sama antre di `payroll_runs FOR UPDATE` | Lock di txWrite (D4) | Lock wait pada `payroll_runs` | Lock hanya selama txWrite (ms), bukan selama compute | Counter `chunks_done` dipindah ke tabel `payroll_run_chunks` (satu baris per chunk), dengan penyelesaian dihitung via count |
| **RLS overhead** | Predikat tambahan di setiap query | `tenant_id = payrun.current_tenant()` | EXPLAIN: predikat masuk *Index Cond* (bagus) atau *Filter* (buruk) | Semua index diawali `tenant_id`; helper `current_tenant()` ditandai `STABLE` (dievaluasi sekali per query). Kalau perlu, `LEAKPROOF` untuk fungsi pembanding agar planner tidak menunda predikat | Bandingkan p95 dengan role `BYPASSRLS` di perf env sebagai *upper bound* overhead (hanya pengukuran) |
| **List employee (100 ribu)** | Pencarian nama | Tanpa `pg_trgm` di V1 (prefix search) | EXPLAIN, p95 PR-3 | Index `(tenant_id, lower(name) text_pattern_ops)`; cursor | `pg_trgm` GIN jika tenant besar butuh pencarian infix |
| **Approve (O12)** | Verifikasi fingerprint kandidat | O(perubahan sejak calculate) | Durasi approve, eksperimen E-2 | Change log + verifikasi kandidat saja | Hitung ulang hanya item dirty saat recalculate (V2) |
| **Lock period (O3)** | HR edit tertahan saat approve/finalize | `FOR UPDATE` period vs `FOR SHARE` penulis | Lock wait penulis input selama approve (PR-5) | Approve/finalize hanya ms (setelah O12) | Finalize besar dipecah: freeze via status, bukan UPDATE per item |
| **Render PDF** | **CPU** worker | Render PDF ±50–200 ms per slip (library Go) | `payslip_render_duration_seconds`, CPU worker | Lane `documents` (2) terpisah dari `payroll`; batch 20 | Instance worker documents terpisah; template lebih sederhana |
| **Import CSV 50 ribu baris** | Parsing + validasi + upsert | Validasi per baris (employee lookup) | Durasi job, query per baris | Lookup employee batch per N baris (map in-memory per batch) | Staging table + validasi set-based SQL |
| **Noisy neighbor** | Tenant 10 ribu memonopoli lane `payroll` | Chunk dari satu run membanjiri antrean | Umur job tertua per tenant kecil saat run besar berjalan (PR-2) | Lane terpisah | Batas konkurensi per tenant (P2, Step 7 `03` §9), diuji A/B di PR-2 |

---

## 2. Hotspot N+1

### 2.1 Riwayat wallet (LedgerPay)
**Naif:** ambil 50 entry riwayat, lalu per entry query `transfers` / `topups` / `payouts` / `payment_intents` untuk detail. Hasilnya 1 + 50 query.

**Desain:** 1 query halaman entries (cursor), lalu **satu query per tipe sumber** dengan `id = ANY($ids)`:
```sql
-- 1. halaman
SELECT je.id, je.created_at, je.direction, je.amount, j.source_type, j.source_id, j.purpose
FROM journal_entries je JOIN journals j ON j.id = je.journal_id
WHERE je.account_id = $acct AND (je.created_at, je.id) < ($c_ts, $c_id)
ORDER BY je.created_at DESC, je.id DESC LIMIT 51;
-- 2. detail, maksimal 4 query (hanya tipe yang muncul di halaman)
SELECT … FROM transfers       WHERE id = ANY($transfer_ids);
SELECT … FROM topups          WHERE id = ANY($topup_ids);
SELECT … FROM payouts         WHERE id = ANY($payout_ids);
SELECT … FROM payment_intents WHERE id = ANY($intent_ids);
```
Jumlah query **konstan (≤ 5)**, tidak tergantung ukuran halaman. Test: `wallet_history_query_count_constant` (hitung query lewat tracer pgx).

### 2.2 Input gathering chunk payroll (PayRun)
Untuk satu chunk berisi `$ids` (≤ 50 employee) dan periode `[$from, $to)`, txRead (RR-RO) menjalankan **7 query batch**:

| # | Query (ringkas) | Sumber |
|---|---|---|
| 1 | `employees` + employment span: `WHERE id = ANY($ids)` | status, tanggal masuk/keluar, PTKP |
| 2 | `salary_assignments WHERE employee_id = ANY($ids) AND valid_during && daterange($from,$to)` | gaji pokok per segmen |
| 3 | `employee_components … && daterange(...)` JOIN `component_definitions` | komponen per segmen |
| 4 | `attendance_records WHERE employee_id = ANY($ids) AND work_date >= $from AND work_date < $to` | hadir/absen |
| 5 | `leave_requests WHERE status='APPROVED' AND decided_at <= $cutoff AND employee_id = ANY($ids) AND dates && …` | cuti tidak dibayar |
| 6 | `overtime_requests WHERE status='APPROVED' AND decided_at <= $cutoff AND …` | lembur |
| 7 | `adjustments WHERE status='PENDING_APPLY' AND employee_id = ANY($ids) AND target_period = $p` | koreksi |

Ditambah data per run yang di-cache in-memory oleh job (sekali per job, bukan per employee): `tenant_settings`, kalender/libur periode, ruleset. **Konstan 7 query per chunk** → 10.000 karyawan = 200 chunk = 1.400 query, bukan 70.000. Test: `payroll_chunk_query_count_constant`.

### 2.3 Inbox approval & daftar slip
- **Inbox approval:** list `approval_requests` (cursor), lalu satu query batch per `subject_type` (leave/overtime/run) dan satu query batch nama employee. ≤ 4 query.
- **Daftar slip (HR, per run):** dari `payroll_items` JOIN `employees` (satu query, karena kolom yang ditampilkan ada di item + nama). Status PDF dari `payslips` di-JOIN di query yang sama, bukan per baris.

---

## 3. Lingkungan uji

| Aspek | Spesifikasi |
|---|---|
| **Target (angka resmi untuk README)** | VM 2 vCPU / 4 GB RAM / SSD, **sama** dengan target deployment (Step 9). Compose: api, worker, postgres, mock-provider, caddy, prometheus |
| Laptop pengembang | Dicatat **terpisah** dan tidak pernah dicampur dengan angka VM |
| Load generator | k6 di **mesin/container terpisah** (bukan di VM target), supaya CPU k6 tidak mencuri CPU sistem yang diuji |
| Postgres (perf env, VM 4 GB) | `shared_buffers = 1GB`, `effective_cache_size = 2.5GB`, `work_mem = 16MB`, `maintenance_work_mem = 256MB`, `max_connections = 100`, `wal_compression = on`, `random_page_cost = 1.1` (SSD), `shared_preload_libraries = 'pg_stat_statements,auto_explain'`, `auto_explain.log_min_duration = 100ms`, `track_io_timing = on` |
| Pool | api 20, worker 10, mock 5 (Step 7). Pool tidak diperbesar untuk "memenangkan" benchmark |
| Dicatat per run | git SHA, versi Go/Postgres/k6, spesifikasi mesin, konfigurasi Postgres, ukuran dataset, durasi warm-up, waktu mulai (UTC), environment variable yang relevan (pool, lane, chunk size) |
| Warm-up | 2 menit beban rendah sebelum pengukuran (cache, JIT pool koneksi) |

---

## 4. Rencana seeding data

| | LedgerPay | PayRun |
|---|---|---|
| Alat | `ledgerpay seed --profile=perf` (subcommand Go, deterministik dengan seed acak tetap) | `payrun seed --profile=perf` |
| Volume | 100.000 user · 1.000 merchant · **1 juta journal (Step 12: S7; 10 juta = opsional)** (≈ 2,2 juta entries) · 200 ribu payout · riwayat 12 bulan | 1.000 tenant · 100.000 employee · 12 bulan attendance (≈ 25 juta baris) · 11 run final per tenant (bulan ke-12 disisakan untuk diuji) |
| Distribusi | Aktivitas Pareto: 20% user menghasilkan 80% transaksi; 10 merchant "populer" | Ukuran tenant: 900 tenant 10–50 karyawan, 95 tenant 100–500, 4 tenant 2.000, **1 tenant 10.000** (target PR-1) |
| Cara tulis | `COPY` (pgx `CopyFrom`) per tabel, dalam batch 50.000 baris | `COPY` per tenant |
| Kecepatan (perkiraan) | COPY ±100–200 ribu baris/dtk → entries ±3–6 menit; total seed termasuk index ±20–40 menit | ±15–30 menit |

**Keputusan: seeding memakai role khusus non-production.**
- **PayRun:** `COPY FROM` **tidak didukung** untuk tabel yang terkena RLS bagi role non-bypass. Role aplikasi (`payrun_app`, `FORCE RLS`) tidak bisa dipakai, dan `INSERT` per baris lewat `WithTenantTx` untuk 25 juta baris terlalu lambat. Karena itu seeding memakai role **`payrun_seed` dengan `BYPASSRLS`**, yang **hanya dibuat di environment perf/dev** oleh skrip init perf. Role ini tidak pernah ada di migrasi production, dan test katalog (C-36) memastikan tidak ada role `BYPASSRLS` di environment production.
- **LedgerPay:** constraint trigger keseimbangan journal (`DEFERRABLE`) berjalan per baris walaupun dengan COPY: 22 juta eksekusi trigger. Seed perf menjalankan `SET session_replication_role = replica` (butuh superuser, **hanya** di container seed perf) sehingga trigger dilewati.
- **Gate wajib setelah seed:** karena constraint dilewati, data seed diverifikasi lewat role aplikasi sebelum test dimulai:
  1. Σ debit = Σ kredit per journal (query yang sama dengan trigger);
  2. `ledger.verify` → `ledgerpay_ledger_balance_drift_total = 0`;
  3. PayRun: sampel query lintas tenant lewat `payrun_app` → 0 baris tenant lain; `count` per tenant cocok dengan manifest seed.
  Seed yang gagal gate tidak boleh dipakai untuk mengukur apa pun.
- Setelah seed: `VACUUM (ANALYZE)` seluruh database, lalu `pg_stat_statements_reset()`.

---

## 5. Skenario load test (k6)

> **Amandemen Step 12 (S7/R4-13, otoritatif):** **subset V1** yang wajib dijalankan dan dilaporkan: **LedgerPay LP-2, LP-3, LP-4 (E-1), LP-9**; **PayRun PR-1, PR-3, PR-4 (E-2)**. Skenario lain (LP-1, LP-5–LP-8, PR-2, PR-5–PR-7) = opsional/V2, dijalankan hanya jika ada buffer. Skenario wajib master prompt tetap tercakup: 10.000 transaksi (LP-2), 100.000 karyawan (PR-3), run besar (PR-1). "1.000 concurrent requests" (LP-1) dan "1.000 payroll run" (PR-2) didokumentasikan sebagai rencana, dan baru dijalankan saat buffer tersedia.

**Model beban.** *Closed model* (VU tetap + think time) mensimulasikan populasi user yang menunggu respons, dan dipakai ketika master prompt menyebut "N concurrent requests". *Open model* (`constant-arrival-rate` / `ramping-arrival-rate`) mensimulasikan laju kedatangan yang tidak peduli sistem lambat, dan dipakai untuk mencari throughput maksimal tanpa *coordinated omission*.

**Kriteria lulus umum untuk semua skenario:**
- **Fungsional (wajib):** 0 pelanggaran invariant setelah run (cek SQL pasca-run dan metrik invariant = 0); 0 respons `500 INTERNAL_ERROR`.
- **Performa:** threshold SLO (§4 `00-index.md`) dicatat sebagai **baseline**. Gagal threshold berarti investigasi, bukan otomatis "desain salah".

### 5.1 LedgerPay

| ID | Tujuan | Model | Beban & data | Durasi | Kriteria lulus | Metrik utama |
|---|---|---|---|---|---|---|
| **LP-1** 1.000 concurrent requests | Perilaku sistem di bawah 1.000 user aktif dengan campuran realistis | Closed, **1.000 VU**, think time 1–3 dtk | Campuran: 60% `GET /wallet` + riwayat, 25% transfer, 10% confirm payment, 5% create payout. User dipilih dari 1.000 user aktif | Ramp 2 mnt → tahan 10 mnt | Invariant 0; error non-domain < 0,5%; p95 transfer dicatat vs SLO 200 ms; tidak ada `db_pool_acquire_timeout` berkelanjutan | p50/p95/p99 per route, RPS, `db_pool_in_use`, lock wait |
| **LP-2** 10.000 transaksi | Throughput transfer + correctness setelah 10 ribu transaksi | Open, `ramping-arrival-rate` 10 → 200 transfer/dtk | 1.000 user dengan saldo awal diketahui; pasangan acak; 10.000 transfer total | Sampai 10.000 iterasi | **Σ saldo sebelum = Σ saldo sesudah**; jumlah journal TRANSFER = jumlah 201; drift 0 | Laju maksimal dengan p95 < 200 ms, titik mulai jenuh |
| **LP-3** Hot wallet | Kontensi satu saldo (Demo B pada skala besar) | Open, 500 transfer/dtk dari **satu** wallet (saldo = 100 × amount) | 2.000 request | Hingga selesai | **Tepat 100 sukses**, sisanya `INSUFFICIENT_BALANCE`; saldo akhir 0; 0 deadlock | Lock wait p95/p99, `RESOURCE_BUSY` count |
| **LP-4** Hot merchant (**E-1**) | Uji klaim O11 | Open, step 25 → 50 → 100 → 200 → 400 pembayaran/dtk ke **satu** merchant, masing-masing 2 menit | 5.000 payer berbeda (tanpa kontensi sisi payer) | ±10 mnt | Lihat §6 E-1 | Laju maksimal berkelanjutan, lock wait merchant, p95 confirm |
| **LP-5** Idempotency storm | Retry agresif tidak menghasilkan efek ganda | Closed, 200 VU; setiap "niat" dikirim 5× dengan key sama (2 paralel, 3 berurutan) | 2.000 niat transfer | 5 mnt | **Tepat 2.000 journal**; replay ditandai `Idempotent-Replayed`; `REQUEST_IN_PROGRESS` < 5% | `idempotency_replay_total`, p95 replay vs eksekusi pertama |
| **LP-6** Webhook burst | Inbox menyerap lonjakan | Open, 500 webhook/dtk ke `/webhooks/provider` | 10.000 event, 20% duplikat, 5% out-of-order, untuk 8.000 top-up pending | Hingga inbox kosong | Semua respons 200 < 50 ms p95; tepat 8.000 kredit; duplikat → IGNORED; inbox kosong < 3 mnt | Umur event RECEIVED tertua, throughput lane inbox, latency verify-by-fetch |
| **LP-7** Payout pipeline | Throughput job + resolusi UNKNOWN | Open, 50 payout/dtk; mock: 10% `timeout_after_process`, 5% `decline` | 6.000 payout | 2 mnt beban + tunggu jadwal status check pertama (30 dtk, 2 mnt) | Setiap payout final atau UNKNOWN terjadwal; **transfer bank per payout ≤ 1** (cek `GET /admin/transfers`); ≥ 95% UNKNOWN terselesaikan di check ke-1/ke-2 | `ledgerpay_payout_unknown_total`, umur UNKNOWN tertua, job queue depth lane money |
| **LP-8** Soak | Kebocoran memori/koneksi, bloat | Open, 30% dari laju maksimal LP-2, campuran LP-1 | — | **1 jam** | RSS stabil (±10%); p95 di menit ke-60 ≤ 1,2× menit ke-5; goroutine stabil; dead tuple `jobs` terkendali | RSS, goroutines, `n_dead_tup`, p95 per 5 menit |
| **LP-9** Login | Membuktikan login CPU/memory-bound dan semaphore bekerja | Open, 5 → 50 login/dtk (rate limit dinonaktifkan **hanya** di skenario ini untuk mengukur kapasitas mentah) | 10.000 akun | 5 mnt | Tanpa OOM; saat jenuh: `503` cepat dari semaphore, bukan latency tak terbatas; endpoint lain tetap p95 < SLO selama login jenuh | CPU, RSS, p95 login, p95 `GET /wallet` paralel |

### 5.2 PayRun

| ID | Tujuan | Model | Beban & data | Durasi | Kriteria lulus | Metrik utama |
|---|---|---|---|---|---|---|
| **PR-1** Run 10.000 karyawan | Durasi kalkulasi + skala worker | Satu run; diulang dengan **1, 2, 4 worker** dan chunk size 25/50/100 | Tenant 10.000 | Hingga `CALCULATED` | 10.000 item; Σ item = total run; durasi dicatat (SLO 1.000 karyawan < 60 dtk → ekstrapolasi **tidak** diklaim, diukur langsung dengan sub-tenant 1.000) | `payroll_run_duration_seconds`, `payroll_chunk_duration_seconds` (txRead/compute/txWrite), lock wait `payroll_runs`, CPU worker vs DB |
| **PR-2** 1.000 payroll run | Antrean lintas tenant, noisy neighbor | 1.000 tenant memulai calculate dalam 5 menit (open, ±3,3 run/dtk), **termasuk** tenant 10.000 di menit pertama | Semua tenant perf | Hingga semua `CALCULATED` | Semua run selesai; invariant 0. **A/B:** tanpa vs dengan batas per tenant (P2) → bandingkan waktu tunggu run tenant kecil saat run besar berjalan | Umur job tertua lane payroll, durasi run per ukuran tenant (p50/p95), throughput chunk/dtk |
| **PR-3** 100.000 karyawan, API baca | Latency baca dengan RLS di dataset besar | Closed, 300 VU tersebar di 1.000 tenant (bobot sesuai ukuran) | `GET /employees` (filter departemen/status, prefix nama, cursor 5 halaman), `GET /attendance`, `GET /me/payslips` | 10 mnt | p95 < 300 ms; **0 baris lintas tenant** (VU memvalidasi `tenant` setiap item yang dikembalikan) | p95 per route, EXPLAIN query teratas (Index Cond vs Filter) |
| **PR-4** Approve (**E-2**) | Uji O12 | Tenant 10.000, run `PENDING_APPROVAL`; jumlah perubahan input 0 / 10 / 1.000 karyawan | Bandingkan implementasi change log vs recompute penuh (flag eksperimen di perf env) | Per variasi 20 pengulangan | Lihat §6 E-2 | Durasi approve p50/p95, lama memegang lock period |
| **PR-5** HR edit saat approve | Dampak O3 ke penulis input | Closed, 50 VU HR terus-menerus mengoreksi attendance di tenant 10.000 sambil approve/finalize dijalankan berulang | — | 5 mnt | Tidak ada `RESOURCE_BUSY` dari penulis selama approve normal; lock wait penulis p99 < 500 ms; hasil approve benar (`RUN_STALE` saat ada perubahan) | Lock wait `payroll_periods`, p99 PATCH attendance |
| **PR-6** Render 10.000 PDF | Throughput lane documents | Finalize tenant 10.000 | 10.000 slip (batch 20) | Hingga semua `RENDERED` | Semua RENDERED; object key unik; tidak memperlambat lane payroll tenant lain (cek PR-2 paralel) | `payslip_render_duration_seconds`, CPU worker, slip/dtk |
| **PR-7** Import 50.000 baris | Durasi + memori import CSV | 1 file 50.000 baris (≈ 4 MB), 2% baris invalid | Tenant 10.000 | Hingga `COMPLETED` | Baris valid masuk; 1.000 error terlapor; RSS worker < 300 MB; import ulang file sama → `IMPORT_DUPLICATE` | Durasi job, RSS, query per batch |

---

## 6. Eksperimen O11 / O12 (O20)

### E-1 — Throughput pembayaran ke satu merchant (O11)
| Aspek | Isi |
|---|---|
| Hipotesis | Lock wallet merchant ±2–5 ms per pembayaran. Throughput berkelanjutan ≥ 150 pembayaran/dtk dengan p95 confirm < 200 ms di VM 2 vCPU |
| Metode | LP-4 (step load). Per tahap: laju yang benar-benar tercapai, p95/p99 confirm, rata-rata dan p95 lock wait pada baris akun merchant (sampling `pg_locks` JOIN `pg_stat_activity` tiap 1 dtk), CPU Postgres |
| Hasil yang diharapkan | Kurva latency datar sampai ±60–70% batas serial, lalu naik tajam (perilaku antrean M/D/1) |
| **Aturan keputusan** | Laju berkelanjutan < **100/dtk** **atau** p95 lock wait > **50 ms** pada 100/dtk → implementasikan `MERCHANT_PENDING` (Step 7 O11 V2) dan ulangi E-1. Jika lulus → pertahankan V1 dan dokumentasikan angkanya sebagai baseline |
| Kontrol | Jalankan pembanding: pembayaran ke **100 merchant berbeda** dengan laju total sama, untuk memisahkan efek hot row dari batas sistem secara umum |

### E-2 — Biaya approve: change log vs recompute penuh (O12)
| Aspek | Isi |
|---|---|
| Hipotesis | Change log: durasi approve ~konstan (< 50 ms) untuk 0–10 perubahan, dan linear terhadap jumlah perubahan. Recompute penuh 10.000 karyawan: detik (input gathering + hash), dan **lock period dipegang selama itu** |
| Metode | PR-4. Ukur durasi approve dan lama lock `payroll_periods FOR UPDATE` (span OTel di dalam tx). Ukur juga dampak ke penulis (PR-5 paralel) |
| Hasil yang diharapkan | Change log: 0 perubahan ≈ 10–30 ms; 1.000 perubahan ≈ ratusan ms. Recompute: beberapa detik untuk semua variasi |
| **Aturan keputusan** | Change log dipertahankan (sudah desain). Kalau 1.000 perubahan > **2 dtk**, verifikasi kandidat dipindah ke job asinkron (`payroll.verify_stale`) dan approve menjadi dua langkah (`PENDING_VERIFICATION`) |

---

## 7. Prosedur profiling

| Alat | Cara | Kapan |
|---|---|---|
| **pprof** | `net/http/pprof` di port internal admin (`:6060`, tidak lewat Caddy, hanya `127.0.0.1`/network compose). `go tool pprof -http=: http://api:6060/debug/pprof/profile?seconds=30` saat beban puncak; `heap`, `goroutine`, `mutex`, `block` (aktifkan `runtime.SetMutexProfileFraction` di perf env) | Setiap skenario yang gagal threshold; selalu untuk LP-9, PR-1, PR-6 |
| **Flame graph** | Dari profil pprof (`-http` UI); simpan SVG ke `perf/results/<run>/` | Lampiran laporan |
| **pg_stat_statements** | `SELECT pg_stat_statements_reset();` sebelum run → setelah run: top 20 berdasarkan `total_exec_time`, `mean_exec_time`, `calls`, `shared_blks_read` | Setiap run |
| **EXPLAIN** | `EXPLAIN (ANALYZE, BUFFERS, SETTINGS)` untuk top 10 query. Checklist: (1) predikat `tenant_id` di *Index Cond*, bukan *Filter*; (2) tidak ada `Seq Scan` pada tabel > 10 ribu baris di jalur API; (3) `Rows Removed by Filter` kecil; (4) cursor memakai *Index Scan Backward*; (5) buffers `read` vs `hit` (cold vs warm); (6) tidak ada `Sort` di memori yang tumpah ke disk | Query baru di top 20 |
| **auto_explain** | Aktif di perf env (`log_min_duration = 100ms`, `log_analyze = on`, `log_buffers = on`, `log_nested_statements = on` agar query di dalam fungsi trigger terlihat) | Selalu di perf env |
| **Lock analysis** | Sampling tiap 1 dtk: `pg_locks` JOIN `pg_stat_activity` (`wait_event_type='Lock'`), dikelompokkan per relasi dan mode | LP-3, LP-4, PR-1, PR-5 |
| **Trace** | Jaeger: bandingkan durasi span txRead / compute / txWrite (PR-1), span di dalam tx approve (PR-4) | Investigasi |

---

## 8. Template laporan hasil

File: `perf/results/<YYYY-MM-DD>-<git sha>/REPORT.md`. Ringkasan yang diangkat ke README memakai tabel ini:

```markdown
### Load test baseline — <tanggal>, commit <sha>
Lingkungan: VM 2 vCPU / 4 GB RAM / SSD (<provider>), Postgres <ver>, Go <ver>, k6 dari mesin terpisah.
Dataset: <ringkas>. Pool: api 20 / worker 10. Ini baseline lingkungan tersebut, **bukan** klaim kapasitas production.

| Skenario | Beban | Hasil | p95 | Invariant | Catatan |
|---|---|---|---|---|---|
| LP-2 10.000 transfer | ramp 10→200/dtk | jenuh di ±<x>/dtk | <y> ms | ✅ drift 0 | bottleneck: <…> |
| LP-3 hot wallet | 2.000 req, 1 wallet | tepat 100 sukses | <y> ms | ✅ | 0 deadlock |
| PR-1 run 10.000 karyawan | 4 worker, chunk 50 | <z> dtk | — | ✅ Σ item = total | CPU worker <…>% |
| … | | | | | |

Temuan & tindakan: <bottleneck yang ditemukan → perubahan → hasil sesudahnya>
```

**Panduan wording yang jujur (untuk README, CV, interview):**

| ✅ Pakai | ❌ Hindari |
|---|---|
| "Pada VM 2 vCPU/4 GB, baseline transfer jenuh di ±X/dtk dengan p95 Y ms." | "Mendukung jutaan user" / "highly scalable" |
| "100 transfer paralel dari satu saldo: tepat N sukses, 0 saldo negatif, diuji di CI." | "Tidak pernah gagal" |
| "Run payroll 10.000 karyawan sintetis selesai dalam X dtk dengan 4 worker." | "Production-grade performance" |
| "Bottleneck pertama yang ditemukan: argon2 di login; diatasi dengan semaphore." | Angka laptop disajikan sebagai angka server |
| "Skenario uji: 1.000 concurrent request." | "Kapasitas 1.000 concurrent user" |

Menceritakan bottleneck yang ditemukan beserta perbaikannya justru sinyal senior. Angka besar tanpa konteks tidak.

---

## 9. CI perf smoke (regression guard)

| Aspek | Isi |
|---|---|
| Kapan | **(Step 12: S19) Ditunda sampai setelah M4 (LedgerPay) / M7 (PayRun).** Setelah itu: nightly (GitHub Actions `schedule`) + manual (`workflow_dispatch`). **Tidak** di setiap PR (lambat dan berisik di runner shared) |
| Lingkungan | Runner GitHub (2–4 vCPU), compose ringan, dataset kecil (`seed --profile=smoke`: 1.000 user, 10 tenant, 1 tenant 500 karyawan) |
| Skenario | LP-2 mini (1.000 transfer @ 50/dtk), LP-3 mini (200 req, 1 wallet), PR-1 mini (run 500 karyawan), PR-3 mini (50 VU, 2 mnt) |
| Threshold k6 | Fungsional **keras**: invariant 0, `500` = 0, LP-3 tepat N sukses → gagal = workflow merah. Performa **lunak**: p95 dibandingkan median 7 run terakhir; regresi > 30% → peringatan (anotasi + issue otomatis), bukan merah, karena runner shared berisik |
| Artefak | Ringkasan k6 JSON + top 10 `pg_stat_statements` diunggah sebagai artifact; tren p95 dicatat ke file CSV di branch `perf-history` |
| Tujuan | Menangkap regresi kasar (N+1 baru, index hilang, lock baru), bukan mengukur kapasitas |
