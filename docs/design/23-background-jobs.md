# Step 7.3 — Background Processing

> Bagian dari [Step 7](00-index.md) (Phase 8 master prompt). Mekanisme dasar job runner ada di ADR-006 dan Step 4 `01-shared-tables.md` §4 (klaim, lease, heartbeat, `CompleteInTx` dengan fencing, `dead`). Dokumen ini menetapkan **apa** yang dikerjakan secara asinkron, **kenapa**, dan **bagaimana setiap jenis job berperilaku** saat gagal.
> Aturan dasar dari `00-index.md` §4 tetap berlaku: side effect asinkron selalu di-enqueue lewat INSERT `jobs` di transaksi bisnis yang sama, tidak ada panggilan jaringan di dalam transaksi DB, dan setiap konsumer harus idempoten.

---

## 1. Sync vs Async

Prinsip keputusan:
- **Sync** kalau user butuh jawaban final sekarang **dan** operasinya bisa selesai di satu transaksi DB pendek tanpa pihak luar.
- **Async** kalau operasinya bergantung pada pihak luar yang tidak andal, lama atau berat, boleh selesai belakangan, atau dipicu jadwal.

### 1.1 LedgerPay

| Operasi | Keputusan | Alasan | Implikasi ke user |
|---|---|---|---|
| P2P transfer | **Sync** | Satu tx, dua row lock (< 50 ms), tanpa pihak luar. User butuh kepastian saat itu juga | `201 COMPLETED` atau `422` langsung |
| Konfirmasi payment intent | **Sync** | Sama dengan transfer. Merchant butuh status final untuk melepas barang | Checkout langsung sukses/gagal |
| Refund | **Sync** (efek ledger) + **async** (webhook ke merchant) | Efek uangnya cukup satu tx; notifikasinya ke pihak luar | `201`; merchant menerima `payment.refunded` belakangan |
| Buat top-up (sesi charge) | **Sync best-effort** + fallback **async** | Tx1 commit dulu, lalu API memanggil provider sekali (di luar tx). Kalau gagal/timeout, `topup.recover` melanjutkan tanpa menunggu client (Step 5 `POST /topups`) | `201` dengan `checkout_url`, atau `checkout_url: null` + polling |
| Penerapan webhook provider (top-up) | **Async** (inbox) | Provider butuh `200` cepat; verifikasi-by-fetch memanggil provider; retry tidak boleh bergantung pada provider | Saldo bertambah beberapa detik setelah bayar |
| Payout submit ke bank | **Async** | Pihak luar dengan *unknown outcome*; tidak boleh menahan request HTTP | `202 PENDING`, lalu `display_status` berubah |
| Status check payout | **Async** (terjadwal per payout) | Menunggu jawaban bank, dengan backoff berjam-jam | Status `PROCESSING` sampai pasti |
| Webhook keluar ke merchant | **Async** | Endpoint merchant bisa lambat atau mati; retry sampai 24 jam | Merchant bisa polling `GET` sebagai alternatif |
| Kedaluwarsa top-up / intent | **Async** (sweeper) | Dipicu waktu, bukan request | — |
| Reconciliation | **Async** (harian) | Berat, mengambil file dari luar, tidak ada user yang menunggu | Ops melihat hasilnya di console |
| Verifikasi saldo vs ledger | **Async** (per jam) | Pemeriksaan invariant, bukan bagian request | Metrik + alert |
| Notifikasi email (P3) | **Async** | Pihak luar, tidak kritis | — |

### 1.2 PayRun

| Operasi | Keputusan | Alasan | Implikasi ke user |
|---|---|---|---|
| Clock in/out, CRUD master data | **Sync** | Satu tx kecil | Langsung |
| Approve/reject cuti & lembur | **Sync** | Efek (saldo cuti) harus atomik dengan transisi approval (ADR-P09); satu tx | Langsung |
| Kalkulasi payroll | **Async** (plan + chunk) | 100–10.000 karyawan; satu tx raksasa berarti lock panjang dan nol progres (ADR-P05) | `202`, progres `chunks_done/chunks_total` |
| Approve run | **Sync** | Guard fingerprint berbasis change log, O(perubahan) (`00-index.md` O12) | Langsung `200` atau `409 RUN_STALE` |
| Finalize run | **Sync** (status + snapshot beku) + **async** (render slip) | Imutabilitas harus atomik; PDF berat dan bisa di-retry terpisah | `200 FINALIZED`; slip muncul bertahap |
| Render payslip PDF | **Async** | CPU + object storage; kegagalan satu slip tidak boleh menggagalkan finalize | `409 PAYSLIP_NOT_READY` + `Retry-After` |
| Import CSV absensi | **Async** | Ribuan baris, validasi per baris, file di object storage | `202`, laporan hasil di `GET /attendance-imports/{id}` |
| Accrual cuti (P2) | **Async** (harian per tenant) | Dipicu jadwal | — |
| Laporan / export (P3) | **Async** | Berat, read-only | Link unduh saat siap |

**Yang sengaja tetap sync, beserta alasannya:** transfer, confirm, dan approval cuti. Menjadikannya async akan memindahkan masalah konsistensi ke client ("sudah terdebet atau belum?") tanpa keuntungan, karena semuanya selesai dalam satu transaksi milidetik.

---

## 2. Mekanisme antrean

```text
 PRODUCER (tx bisnis)                    TABEL                       WORKER LOOP (proses `worker`)         SIDE EFFECT
 ───────────────────────────             ─────────────────           ──────────────────────────────        ───────────────────────
 POST /payouts ───────────┐                                         ┌─ lane money ──────────────┐ ──────▶ mock-bank (HTTP)
 confirm / refund ────────┤ INSERT       ┌──────────────┐  claim    ├─ lane webhook-out ────────┤ ──────▶ merchant endpoint (C-44)
 calculate / finalize ────┼────────────▶ │    jobs      │ ◀──────── ├─ lane payroll ────────────┤ ──────▶ DB (items, run)
 import upload ───────────┤ (outbox)     └──────────────┘ SKIP LOCKED├─ lane documents ──────────┤ ──────▶ object storage
 scheduler loop ──────────┘ dedupe_key        ▲                     └─ lane maintenance ────────┘ ──────▶ DB (sweep, cleanup)
                                              │ CompleteInTx (fencing)
 POST /webhooks/provider ── INSERT ──▶ ┌──────────────┐ claim ──── inbox loop ─── verify-by-fetch ──▶ mock-provider GET
   (verifikasi HMAC dulu)              │webhook_inbox │ ◀───────── (LedgerPay)    ──▶ tx: topup + journal + jobs
                                       └──────────────┘
```

### 2.1 `jobs`: outbox sekaligus work queue
Satu tabel untuk dua peran: (1) **transactional outbox**, karena job ditulis di tx yang sama dengan perubahan bisnis, dan (2) **work queue**, karena worker mengklaimnya dengan `FOR UPDATE SKIP LOCKED`. Tidak ada relay ke broker (ADR-006).

### 2.2 `webhook_inbox` (LedgerPay): kenapa terpisah dari `jobs`
| Aspek | `webhook_inbox` | `jobs` |
|---|---|---|
| Penulis | Handler HTTP webhook (bukan tx bisnis) | Tx bisnis |
| Fungsi utama | **Bukti mentah** (raw body, signature ts) + dedupe `UNIQUE (provider, provider_event_id)` | Unit kerja |
| Status | `RECEIVED → PROCESSED/IGNORED/DEAD` + `outcome` (O2) | `queued/running/succeeded/dead` |
| Retensi | 90 hari (investigasi, replay ops) | 7 hari (`succeeded`) |
| Replay | Ops: `DEAD → RECEIVED` | Ops: `dead → queued` |

Menyimpan event ke `jobs` akan mencampur bukti audit dengan antrean yang dibersihkan mingguan. Unique key dedupe juga harus berada pada identitas event provider, bukan pada `dedupe_key` job yang hanya unik selama job aktif.

### 2.3 Scheduler
Loop di dalam proses `worker`, tanpa cron service terpisah (§8).

---

## 3. Katalog job

**Konvensi:**
- `dedupe_key` selalu berbentuk `<kind>:<identitas>`.
- `max` = `max_attempts`.
- Backoff "default" = ADR-006: `min(2s · 2^(attempt−1), 10m) ± 20% jitter`.
- Payload **hanya ID dan parameter**, tidak pernah PII atau nominal yang menjadi sumber kebenaran. Handler selalu membaca ulang state dari DB.

### 3.1 LedgerPay

| Kind | Producer · trigger | Payload | `dedupe_key` | Aksi konsumer | Strategi idempoten | Eksternal? | Timeout | Max · backoff | Saat `dead` | Lane |
|---|---|---|---|---|---|---|---|---|---|---|
| `payout.submit` | Tx `POST /payouts` | `payout_id` | `payout.submit:{id}` | `PENDING` → tx: `SUBMITTED` (commit) → POST bank → tx: `SUCCEEDED`+settle / `FAILED`+reversal / `UNKNOWN` + enqueue `payout.status_check` | Guard status. Kalau diklaim ulang dan payout sudah `SUBMITTED` → **tidak submit ulang**: jadi `UNKNOWN` + enqueue status check (ADR-L06). Idempotency key ke bank = `payout_id` | Ya | 15 dtk (HTTP 10 dtk) | 5 · 5s, 30s, 2m, 10m | Payout tetap `PENDING`/`SUBMITTED` → alert `job_dead_total{kind="payout.submit"}`; ops: `POST /ops/jobs/{id}/retry` atau resolve | money (4) |
| `payout.status_check` | `payout.submit` (UNKNOWN) atau check sebelumnya | `payout_id`, `check_no` | `payout.status_check:{id}:{n}` | GET status bank → `SUCCEEDED`/`FAILED` (tx + journal), `NOT_FOUND` + lewat safe window → resubmit dengan key yang sama, belum pasti → enqueue check `n+1` di tx yang sama | Guard `WHERE status = 'UNKNOWN'`; journal unik per `(source, purpose)` | Ya | 15 dtk | **Jadwal tetap** antar check: 30s, 2m, 10m, 1h, 6h. Error infra di satu check: 3 · default | Check ke-5 tak pasti → `needs_reconciliation = true` (bukan dead). Dead infra → alert, ops resolve | money |
| `webhook.deliver` | Tx bisnis yang membuat `webhook_deliveries` (confirm, refund, expire) | `delivery_id` | `webhook.deliver:{delivery_id}` | POST ke `webhook_url` lewat egress guard (C-44), tanda tangan HMAC, catat `webhook_delivery_attempts` | `event_id` stabil (merchant dedupe); guard `status = 'PENDING'` | Ya | 8 dtk (HTTP 5 dtk) | **7** · 1m, 5m, 30m, 2h, 6h, 24h ± 10% (Step 5 `03` §3) | OnDead: `webhook_deliveries → DEAD`; ops replay (`/ops/webhook-deliveries/{id}/replay`) | webhook-out (8) |
| `topup.recover` | Scheduler, setiap 1 menit | — (sweep) | `topup.recover:{menit}` | Ambil top-up `recovery_point = 'CREATED'` berumur > 30 dtk (batch 100) → POST charge (key = `topup_id`) → tx2 `PROVIDER_CREATED` + simpan response idempotency | Provider idempoten per `topup_id`; guard `recovery_point = 'CREATED'` | Ya | 60 dtk | 3 · default | Alert; sweep berikutnya mencoba lagi | maintenance (2) |
| `topup.expire_sweep` | Scheduler, setiap 5 menit | — | `topup.expire_sweep:{slot}` | `PENDING` lewat `expires_at` (batch 50) → **cek status charge ke provider dulu** → sukses: terapkan kredit (jalur yang sama dengan inbox); selain itu `EXPIRED` | Guard status; journal unik `(topup, TOPUP_CREDIT)` | Ya | 120 dtk | 3 · default | Alert; sweep berikutnya | maintenance |
| `intent.expire_sweep` | Scheduler, setiap 1 menit | — | `intent.expire_sweep:{menit}` | `UPDATE … SET EXPIRED WHERE status = 'REQUIRES_CONFIRMATION' AND expires_at < now()` (batch) + `webhook_deliveries` + `webhook.deliver` di tx yang sama | Guard status | Tidak | 30 dtk | 3 · default | Alert | maintenance |
| `ledger.verify` | Scheduler, per jam | — | `ledger.verify:{jam}` | RR read-only: bandingkan `accounts.balance` vs Σ entries; Σ debit = Σ kredit global | Read-only | Tidak | 10 menit + heartbeat | 2 · 5m | Alert (verifikasi tidak berjalan = buta) | maintenance |
| `recon.run` | Scheduler 02:30 (D−1) atau `POST /ops/recon-runs` | `business_date` | `recon.run:{date}` | GET settlement CSV → `recon_runs` (unik `file_hash`) → matching → `PROVIDER_SETTLEMENT` per item `MATCHED` → `recon_items` mismatch | `uq_recon_runs_file`; journal unik per `recon_item`; `recon_items` unik per referensi | Ya | 15 menit + heartbeat | 5 · 5m, 15m, 1h, 3h | `recon_runs.status = FAILED` (OnDead); alert ke ops | maintenance |
| `maintenance.idempotency_cleanup` | Scheduler 02:00 | — | `…:{date}` | DELETE batch 5.000 | DELETE idempoten | Tidak | 10 menit | 3 · default | Alert (tabel tumbuh) | maintenance |
| `maintenance.jobs_cleanup` | Scheduler per jam | — | `…:{jam}` | DELETE `succeeded` > 7 hari, `dead` > 30 hari | Idempoten | Tidak | 10 menit | 3 · default | Alert | maintenance |
| `maintenance.refresh_token_cleanup` | Scheduler harian | — | `…:{date}` | DELETE family kedaluwarsa > 7 hari | Idempoten | Tidak | 10 menit | 3 | Alert | maintenance |
| `maintenance.inbox_cleanup` | Scheduler harian | — | `…:{date}` | DELETE inbox non-`RECEIVED` > 90 hari (diizinkan trigger) | Idempoten | Tidak | 10 menit | 3 | Alert | maintenance |
| `notify.send` (P3) | Tx bisnis | `template`, `user_id`, `ref_id` | `notify.send:{ref}:{template}` | Kirim email | Dedupe key | Ya | 10 dtk | 5 · default | Dibuang + log (tidak kritis) | maintenance |

**Inbox (bukan job kind, tetapi loop sendiri):**

| Langkah | Detail |
|---|---|
| Klaim | `UPDATE webhook_inbox SET next_attempt_at = now() + interval '60 s', attempts = attempts + 1 WHERE id IN (SELECT id … WHERE status = 'RECEIVED' AND next_attempt_at <= now() ORDER BY received_at LIMIT 20 FOR UPDATE SKIP LOCKED) RETURNING …`. `next_attempt_at` di masa depan berfungsi sebagai lease |
| Verifikasi | `GET /v1/charges/{id}` ke provider, di luar tx (amandemen ADR-L08) |
| Terapkan | Tx: transisi monoton top-up (guard status) + journal + jobs → inbox `PROCESSED/IGNORED` + `outcome` |
| Gagal | Backoff default, `next_attempt_at` diset; **8** percobaan → `DEAD` + alert `webhook_inbox_dead_total`; ops replay `DEAD → RECEIVED` |
| Payload tidak valid (amount/currency tidak cocok) | Langsung `DEAD` (permanent) + alert |

### 3.2 PayRun

Semua job tenant di-enqueue di dalam `WithTenantTx` (trigger `jobs_enforce_tenant`, C-43) dan dijalankan **hanya** lewat `jobs.RunTenantJob`, yang membuka `WithTenantTxFor(jobs.tenant_id)`.

| Kind | Producer · trigger | Payload | `dedupe_key` | Aksi konsumer | Strategi idempoten | Eksternal? | Timeout | Max · backoff | Saat `dead` | Lane |
|---|---|---|---|---|---|---|---|---|---|---|
| `payroll.plan` | Tx `POST /payroll-runs/{id}/calculate` | `run_id`, `calc_version` | `payroll.plan:{run}:{v}` | Guard `CALCULATING` + versi → tentukan employee eligible (urut `employee_id`) → `chunks_total` → INSERT chunk job, **semua + `CompleteInTx` dalam satu tx** | Guard versi; dedupe chunk; satu tx | Tidak | 60 dtk | 5 · default | OnDead: run → `FAILED` (`failure_reason = plan`); alert | payroll (4) |
| `payroll.calc_chunk` | `payroll.plan` | `run_id`, `calc_version`, `chunk_no`, `employee_ids[]` (≤ 50; ditetapkan saat plan agar deterministik) | `payroll.calc_chunk:{run}:{v}:{n}` | Pola txRead (RR, read-only) → compute → txWrite (RC) dari `00-index.md` §5; chunk terakhir menutup run | Versi tidak cocok → no-op + complete; UPSERT `(run, employee, calc_version)`; `chunks_done` + `CompleteInTx` di tx yang sama; trigger PI-04 `FOR SHARE` | Tidak | 120 dtk + heartbeat | 5 · 2s, 4s, 8s, 16s (cap 1m) | OnDead: run `CALCULATING → FAILED` + nomor chunk (PFS-05); alert. HR memperbaiki data → recalculate | payroll |
| `payroll.cleanup_versions` (Step 12: R3-2) | Tx finalize / run menjadi `CALCULATED` | `run_id`, `keep_version` | `payroll.cleanup_versions:{run}:{v}` | Hapus item `calc_version < keep_version` per batch 1.000 (cascade line/snapshot), di luar lock periode | DELETE yang sudah kosong = no-op | Tidak | 60 dtk | 5 · default | Alert; data lama hanya memakan ruang | `payroll` |
| `payslip.render` | Tx finalize (satu job per **batch 20 payslip**) | `run_id`, `payslip_ids[]` | `payslip.render:{run}:{batch}` | Per payslip: render PDF di memori → PUT object (key deterministik) → tx: `RENDERED` + checksum. Gagal satu payslip → payslip itu `FAILED`, batch lanjut | Object key deterministik (overwrite = aman); guard `status IN ('PENDING','FAILED')` | Ya (object storage) | 5 menit + heartbeat | 5 · default | Payslip tetap `PENDING`/`FAILED` → `PAYSLIP_NOT_READY`; alert; re-enqueue lewat CLI operator `worker jobs retry` | documents (2) |
| `attendance.import` | Tx `POST /attendance-imports` | `import_id` | `attendance.import:{id}` | `UPLOADED → PROCESSING` → stream CSV dari storage → batch 500 baris per tx (UPSERT `(tenant, employee, work_date)`, error `ON CONFLICT DO NOTHING`) → `COMPLETED` + counts | UPSERT natural key; error unik per baris; mengulang dari awal aman | Ya (storage) | 10 menit + heartbeat | 3 · default | OnDead: import `FAILED` (file boleh diunggah ulang, karena `uq_attendance_imports_file` mengecualikan `FAILED`) | documents |
| `leave.accrual_fanout` (P2) | Scheduler 00:30 | `date` | `leave.accrual_fanout:{date}` | Iterasi tenant aktif → enqueue `leave.accrual` per tenant **di dalam `WithTenantTxFor(tenant)`** | Dedupe per tenant + tanggal | Tidak | 5 menit | 3 · default | Alert | maintenance (1) |
| `leave.accrual` (P2) | `leave.accrual_fanout` | `date` (tenant dari kolom) | `leave.accrual:{tenant}:{date}` | Tambah saldo sesuai aturan jenis cuti | **Butuh catatan accrual unik `(tenant, employee, leave_type, accrual_date)`**, karena saldo bukan upsert alami (lihat catatan di akhir) | Tidak | 5 menit | 3 · default | Alert | maintenance |
| `maintenance.*` | Scheduler | — | `…:{slot}` | Cleanup `idempotency_keys`, `platform_idempotency_keys`, `jobs`, `refresh_tokens`, `invitations` (> 90 hari) | DELETE idempoten | Tidak | 10 menit | 3 | Alert | maintenance |
| `maintenance.audit_partitions` (V2) | Scheduler bulanan | `month` | `…:{month}` | Buat partisi bulan depan `audit_log` / `attendance_records` | `CREATE … IF NOT EXISTS` | Tidak | 5 menit | 3 | Alert (insert gagal kalau partisi tidak ada) | maintenance |
| `report.generate` (P3) | Tx request laporan | `report_id` | `report.generate:{id}` | Query replica → CSV (encoder pusat, C-39) → storage | Key deterministik | Ya (storage) | 10 menit | 3 | Laporan `FAILED` | documents |

> Step 12 (R1-3): `payroll.plan` dengan 0 karyawan eligible langsung menyelesaikan run (`CALCULATED`, total 0, warning `NO_ELIGIBLE_EMPLOYEES`) di tx yang sama.

**Bukan job:**
- **Adjustment** diterapkan di dalam tx kalkulasi/finalize (status `PENDING_APPLY → APPLIED` saat run target final).
- **Kedaluwarsa undangan** dievaluasi *lazy* saat accept (`expires_at`); tidak perlu sweeper.
- **Status "semua slip selesai"** dihitung lewat query (`count(*) FILTER (WHERE status <> 'RENDERED')`), bukan lewat job penutup.

---

## 4. Model proses worker

| Aspek | Keputusan |
|---|---|
| Binary | Satu binary, subcommand `worker` (ADR-001). Bisa dijalankan N replika |
| Lane | Satu loop klaim per lane, dengan filter `kind = ANY($lane_kinds)` dan pool goroutine sendiri (*bulkhead*). LedgerPay: `money` 4, `webhook-out` 8, `maintenance` 2, + loop `inbox` 4. PayRun: `payroll` 4, `documents` 2, `maintenance` 1 |
| Batch klaim | `min(slot kosong di pool, 10)`. Worker tidak mengklaim melebihi kapasitas, supaya lease tidak habis selagi job masih mengantre di memori |
| Polling | 500 ms saat ada pekerjaan; *idle backoff* hingga 2 dtk saat kosong. **Opsional (P2):** trigger enqueue `pg_notify('jobs', kind)` (terkirim saat commit) membangunkan lane yang sesuai seketika; polling tetap ada sebagai jaring pengaman |
| Lease & heartbeat | Lease default 30 dtk. Kind berdurasi panjang (`recon.run`, `attendance.import`, `payslip.render`, `ledger.verify`, `payroll.calc_chunk`) mengirim heartbeat setiap 10 dtk. Heartbeat 0 baris → lease hilang → `ctx` handler dibatalkan, dan `CompleteInTx` juga akan gagal (fencing) |
| Deadline | Setiap kind punya timeout (kolom katalog). `context.WithTimeout` membungkus handler; panggilan HTTP keluar punya timeout yang lebih pendek |
| Panic | `recover()` per job → percobaan gagal; stack trace (dipotong 2 KB) masuk `last_error`; worker tetap hidup |
| Graceful shutdown | `SIGTERM` → (1) berhenti mengklaim; (2) tunggu job in-flight hingga **25 dtk**; (3) batalkan `ctx` sisa job dan beri 3 dtk untuk kembali; (4) job yang kembali dengan `context.Canceled` dilepas (`queued`, `run_at = now()`); (5) handler yang tidak kembali ditinggal, lalu lease-nya habis. Compose: `stop_grace_period: 30s` (default Docker 10 dtk terlalu pendek) |
| Identitas | `locked_by = hostname:pid:uuid-per-start`, sehingga worker yang restart tidak pernah dianggap pemilik lease lamanya (fencing tetap efektif) |

---

## 5. Retry & backoff

| Klasifikasi error | Contoh | Perlakuan |
|---|---|---|
| **Retryable** | Timeout/5xx dari pihak luar, `55P03`, koneksi DB putus, `ErrLeaseLost` | `status = queued`, `run_at = now() + backoff` |
| **Permanent** (`jobs.Permanent(err)`) | Payload tidak bisa di-decode, referensi tidak ada (0 baris di bawah RLS), amount event provider tidak cocok, CSV bukan UTF-8 | Langsung `dead` (`attempts = max_attempts`) + OnDead + alert. Tidak membuang 5–7 percobaan untuk data yang pasti rusak |
| **Hasil bisnis "belum pasti"** | Status payout belum final | **Bukan error.** Handler menyelesaikan job dan meng-enqueue check berikutnya dengan jadwal tetap (`payout.status_check:{id}:{n+1}`) |
| **No-op sah** | Chunk dengan `calc_version` lama, event duplikat, status sudah berubah | Selesai sukses (`CompleteInTx`), tanpa efek |

- **Formula default (ADR-006):** `delay = min(2s · 2^(attempt−1), 10m)`, lalu dikalikan faktor acak `[0.8, 1.2]`. Jitter mencegah *thundering herd* ketika banyak job gagal bersamaan, misalnya saat provider down.
- **Override per kind:** lihat kolom katalog (`webhook.deliver`, `payout.status_check`, `payroll.calc_chunk`, `recon.run`).
- `max_attempts` diisi saat enqueue dari registry kind, bukan default tabel (10).

---

## 6. Dead Letter Queue

| Aspek | Keputusan |
|---|---|
| Representasi | `jobs.status = 'dead'` **adalah** DLQ (tanpa tabel atau antrean terpisah). Inbox: `webhook_inbox.status = 'DEAD'`; webhook keluar: `webhook_deliveries.status = 'DEAD'` |
| OnDead hook | Registry kind boleh mendaftarkan fungsi yang dijalankan **di tx yang sama** dengan penandaan `dead`, untuk membawa state bisnis ke kondisi terminal yang jujur: run `FAILED`, import `FAILED`, `webhook_deliveries` `DEAD`, `recon_runs` `FAILED` |
| Alert | `increase(job_dead_total[5m]) > 0` untuk kind kritis (`payout.*`, `payroll.*`, `recon.run`, `ledger.verify`) → alert segera. Kind lain: ringkasan harian |
| Tindakan manual | LedgerPay: `GET /ops/jobs?status=dead`, `POST /ops/jobs/{id}/retry` (`dead → queued`, `attempts = 0`, ter-audit), `POST /ops/webhook-deliveries/{id}/replay`, replay inbox. PayRun: status run/import/slip terlihat oleh HR (recalculate / unggah ulang); retry job oleh operator platform lewat CLI `worker jobs retry <id>` (tidak ada ops console PayRun di V1) |
| Retensi | 30 hari, lalu dihapus `maintenance.jobs_cleanup`. Retry/replay manual tercatat di `audit_log`, sehingga jejaknya tetap ada |

---

## 7. Pola konsumer idempoten

Delivery bersifat **at-least-once**: setiap handler harus aman dijalankan dua kali, termasuk kalau dijalankan bersamaan oleh worker zombie.

| Pola | Cara kerja | Dipakai di |
|---|---|---|
| **State-guarded transition** | `UPDATE … SET status = $new WHERE id = $id AND status = $expected`; 0 baris = sudah diproses → no-op | `payout.*`, sweeper, inbox, `payslip.render`, `attendance.import` |
| **Upsert dengan natural key** | `INSERT … ON CONFLICT (natural key) DO UPDATE/NOTHING` | `payroll.calc_chunk` (item per run+employee+versi), `attendance.import` (per employee+tanggal) |
| **Unique journal source** | `UNIQUE (source_type, source_id, purpose)`: posting kedua gagal → dianggap sudah | Inbox (TOPUP_CREDIT), `payout.*` (SETTLE/REVERSAL), `recon.run` (PROVIDER_SETTLEMENT) |
| **Idempotency key ke provider** | Kunci = ID lokal yang stabil (`topup_id`, `payout_id`) | `topup.recover`, `payout.submit`, resubmit dari status check |
| **Key objek deterministik** | Key object storage = fungsi dari identitas (`tenants/{t}/payslips/{run}/{emp}.pdf`); render ulang menimpa objek yang sama | `payslip.render`, `report.generate` |
| **Dedupe saat enqueue** | `uq_jobs_dedupe_key` (selama `queued/running`); scheduler dan fan-out tidak bisa membuat job ganda | Semua job terjadwal, chunk, `payout.status_check:{id}:{n}` |
| **Versioning** | Job membawa `calc_version`; versi lama → no-op | `payroll.calc_chunk`, `payroll.plan` |
| **Fencing saat selesai** | `CompleteInTx … WHERE locked_by = $me AND status = 'running'`; gagal → rollback seluruh kerja bisnis | Semua job yang menulis DB |
| **Event ID stabil** | ID event dibuat sekali di outbox; penerima melakukan dedupe | `webhook.deliver` |

---

## 8. Penjadwalan tanpa cron service

```go
// di setiap replika worker, setiap 15 detik:
for _, s := range schedules {                // {kind, every/cron, bucket func}
    bucket := s.Bucket(now)                  // "2026-03-15" | "2026-03-15T10" | "2026-03-15T10:05"
    if s.Due(now) {
        jobs.Enqueue(ctx, s.Kind, payload, jobs.Dedupe(s.Kind+":"+bucket))   // ON CONFLICT DO NOTHING
    }
}
```
- **Kenapa tanpa leader election:** semua replika boleh mencoba meng-enqueue. `uq_jobs_dedupe_key` menjamin hanya satu job per *bucket* waktu yang ada selama `queued/running`. Yang kalah cukup mendapat `ON CONFLICT DO NOTHING`.
- **Celah yang diterima:** setelah job bucket itu `succeeded`, unique partial index tidak lagi berlaku, sehingga replika yang sangat terlambat (jam sistem melenceng) bisa meng-enqueue ulang bucket yang sama. Ini aman karena semua job terjadwal idempoten (sweep berbasis state, cleanup berbasis kondisi, `recon.run` unik per `file_hash`), dan `Due()` hanya bernilai true di awal jendela bucket.
- **Cron yang terlewat:** kalau semua worker mati saat jadwal, loop berikutnya yang hidup melihat bucket saat ini. Untuk job harian kritis (`recon.run`), scheduler juga memeriksa `recon_runs` untuk D−1 dan meng-enqueue kalau belum ada (*catch-up*).

---

## 9. Fairness & isolasi

| Mekanisme | Detail | Status |
|---|---|---|
| **Lane per jenis kerja** (bulkhead) | Webhook merchant yang lambat tidak menahan payout; render PDF tidak menahan kalkulasi payroll | MVP |
| **Prioritas** | Dalam satu lane, urutan klaim `ORDER BY priority DESC, run_at` (kolom `priority smallint DEFAULT 0`). Contoh: `payslip.render` untuk run yang baru final (user sedang menunggu) > `report.generate` | P2 (kolom ditambahkan saat dibutuhkan) |
| **Batas konkurensi per tenant** (PayRun) | Mencegah satu tenant 10.000 karyawan memonopoli lane `payroll` | P2 |

Sketsa klaim dengan batas per tenant:
```sql
SELECT j.id FROM jobs j
WHERE j.status = 'queued' AND j.run_at <= now() AND j.kind = ANY($lane_kinds)
  AND (j.tenant_id IS NULL OR
       (SELECT count(*) FROM jobs r
         WHERE r.tenant_id = j.tenant_id AND r.status = 'running'
           AND r.kind = ANY($lane_kinds)) < $per_tenant_limit)       -- mis. 2 chunk paralel per tenant
ORDER BY j.run_at
LIMIT $n
FOR UPDATE SKIP LOCKED;
-- index pendukung: (tenant_id, kind) WHERE status = 'running'
```
Batas ini **lunak**: dua worker yang mengklaim bersamaan bisa sama-sama melihat `count < limit` dan melampauinya sebanyak jumlah replika. Ini diterima, karena tujuannya fairness, bukan correctness. Kalau perlu ketat: `pg_try_advisory_xact_lock(hash(tenant_id, slot))` per slot.

---

## 10. Status job & observability

| Transisi | Log (JSON, dengan `job_id`, `kind`, `tenant_id`, `correlation_id`, `attempt`) | Metrik |
|---|---|---|
| enqueue | `job.enqueued` | `job_enqueued_total{kind}` |
| `queued → running` | `job.claimed` (+ `was_lease_expired`) | `job_lease_expired_total{kind}` jika klaim mengambil lease kedaluwarsa (CTE klaim mengembalikan status lama) |
| `running → succeeded` | `job.succeeded` (+ `duration_ms`) | `job_duration_seconds{kind,outcome="ok"}` |
| `running → queued` (retry) | `job.failed` (+ `retry_in`, `error_class`) | `job_failures_total{kind,class}` |
| `running → dead` | `job.dead` (level ERROR) | `job_dead_total{kind}` → alert (§6) |
| Lease hilang | `job.lease_lost` (WARN) | `job_lease_lost_total{kind}` |

**Gauge periodik** (query setiap 15 dtk oleh satu goroutine di tiap worker; aman ganda karena hanya membaca):
- `job_queue_depth{kind,status}`
- `job_oldest_age_seconds{kind}` = `now() − min(run_at)` untuk `queued` yang sudah jatuh tempo
- `outbox_lag_seconds` = `now() − min(created_at)` untuk job `queued` yang belum pernah diklaim (`attempts = 0`) dan sudah jatuh tempo: berapa lama side effect tertunda sejak commit bisnis
- LedgerPay tambahan: `webhook_inbox_pending`, `webhook_inbox_dead_total`, `payout_unknown_total`, `payout_unknown_oldest_seconds`, `webhook_delivery_blocked_total` (C-44)

**Propagasi correlation ID:**
```text
X-Request-ID (HTTP) → ctx → jobs.correlation_id (saat enqueue) → logger handler
   → header pada panggilan keluar (provider, merchant webhook): X-Request-ID baru per panggilan
     + X-Correlation-ID milik alur (Step 8 `01-observability.md` §1)
   → job turunan (status_check, chunk, payslip.render) mewarisi correlation_id induknya
```
Dengan begitu satu ID bisa melacak `POST /payouts` → `payout.submit` → panggilan bank → 5 `payout.status_check` → journal settle (dipakai di demo observability, Step 11).

---

## 11. Endpoint demo (O15)

**Guard (berlapis, Step 6 T-LP-21):** rute hanya dikompilasi dengan build tag `demo` **dan** didaftarkan jika `DEMO_MODE=true`; wajib header `X-Demo-Token`; rate limit `write`. Build produksi tidak berisi kode ini sama sekali (titik injeksi fault di build non-demo adalah fungsi no-op). Pola yang sama berlaku di PayRun.

| Endpoint | Fungsi |
|---|---|
| `POST /api/v1/demo/scenarios` | Proxy ke `PUT /admin/scenario` mock-provider. Token admin mock dipegang server, tidak pernah dikirim ke browser |
| `POST /api/v1/demo/faults` `{ "point": "payout.after_bank_call", "times": 1 }` | Memasang *fault* sekali pakai di tabel demo `demo_faults` (hanya ada di migrasi demo) |
| `DELETE /api/v1/demo/faults` | Bersihkan semua fault |
| `GET /api/v1/demo/evidence/payouts/{id}` | Bukti Demo C: jumlah journal untuk payout + jumlah transfer di mock-bank (`GET /admin/transfers`) + timeline status |
| `POST /api/v1/demo/reset` | Hapus data demo + seed ulang + `POST /admin/reset` mock |
| PayRun: `POST /api/v1/demo/payroll-runs/{id}/fast-track` | Submit → approve (self-approval demo) → finalize dalam satu panggilan, untuk Demo C yang waktunya sempit. Tetap melewati semua guard (fingerprint, period lock) |

**Autentikasi semua endpoint demo:** header **`X-Demo-Token: <DEMO_TOKEN>`** (dibandingkan constant-time), selain JWT user biasa. Tanpa token yang cocok → `404` (rute disamarkan).
| PayRun: `POST /api/v1/demo/faults` `{ "point": "payroll.calc_chunk.before_commit", "after_n": 3 }` | Worker mati saat chunk ke-4 (PFS-01, Demo B; chunk size demo 10, O5) |

**Titik injeksi** (dipanggil `faults.Check(ctx, point)` di kode handler):

| Point | Lokasi | Skenario |
|---|---|---|
| `payout.after_bank_call` | Setelah response bank diterima, sebelum tx3 | FS-08, Demo C |
| `payout.before_submit_commit` | Sebelum commit `SUBMITTED` | Crash sebelum intent tercatat → aman submit |
| `inbox.before_apply` | Setelah verify-by-fetch, sebelum tx terapkan | Event diproses ulang tanpa kredit ganda |
| `payroll.calc_chunk.before_commit` | Sebelum commit txWrite chunk | PFS-01 |
| `payslip.render.after_put` | Setelah PUT object, sebelum tx `RENDERED` | PFS-10 |

**Cara "membunuh" worker: `os.Exit(137)` di titik injeksi vs `docker kill`**

| | `os.Exit` di titik injeksi | `docker kill` manual |
|---|---|---|
| Ketepatan waktu | **Deterministik**: mati tepat di antara panggilan bank dan pencatatan hasil | Tergantung kecepatan tangan; sering meleset |
| Realisme | Tanpa `defer`, tanpa graceful shutdown, sama seperti `kill -9` | Paling realistis secara infrastruktur |
| Dipakai untuk | Demo live dan test otomatis (chaos test di CI memakai titik yang sama) | Ditunjukkan sekali sebagai pembuktian bahwa titik injeksi tidak "curang" |

Setelah exit, compose (`restart: unless-stopped`) menyalakan worker lagi. Mode demo memakai lease 10 dtk (env `JOB_LEASE_TTL`) agar pemulihan terlihat dalam hitungan detik, bukan 30 dtk.

---

## Catatan untuk diselaraskan

1. **Nama kind:** Step 4 `03-ledgerpay-schema.md` menyebut `retention.cleanup` dan `notify.*`; Step 4 `01-shared-tables.md` §8 menyebut `maintenance.*`. Dokumen ini menetapkan **`maintenance.*`** dan **`notify.send`** sebagai kanonik.
2. **Kind payroll:** dokumen ini memakai `payroll.plan` (ADR-P05, Step 4) sebagai nama kanonik, bukan `payroll.orchestrate`.
3. **Format `dedupe_key` chunk:** Step 4 `06-payrun-schema.md` menulis `calc_chunk:{run}:{v}:{n}`. Dokumen ini menetapkan prefix kind lengkap: `payroll.calc_chunk:{run}:{v}:{n}`.
4. **`max_attempts` webhook keluar:** Step 4 menyebut 8 sebagai contoh; kontrak Step 5 (1 + 6 retry) berarti **7**.
5. **Skema yang perlu ditambah:**
   - `leave_accruals (tenant_id, employee_id, leave_type_id, accrual_date, amount)` dengan unique key, untuk idempotensi `leave.accrual` (P2).
   - Tabel demo `demo_faults` (hanya di migrasi demo).
   - Kolom opsional `jobs.priority` (P2).
6. **Compose:** worker butuh `stop_grace_period: 30s`. Dicatat untuk Step 9.
