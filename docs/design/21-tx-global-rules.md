# Step 7 — Transaction, Concurrency & Background Processing

> Mencakup **Phase 7** (Transaction & Concurrency) dan **Phase 8** (Background Processing) dari master prompt.
> Input: ADR-006 (job runner + fencing), ADR-011 (transaction management), ADR-L02/L03/L06/L07/L08, ADR-P05/P07/P09; Step 3 (invariant, failure scenario); Step 4 (constraint, trigger, index); Step 5 (endpoint). Open items: **O3, O11, O12**.

## Isi

| File | Isi |
|---|---|
| [`01-ledgerpay-transactions-concurrency.md`](01-ledgerpay-transactions-concurrency.md) | Katalog operasi: transaction boundary, isolation, lock, race condition, retry, idempotency, rollback; timeline race konkret; analisis deadlock; hot row (O11) |
| [`02-payrun-transactions-concurrency.md`](02-payrun-transactions-concurrency.md) | Katalog operasi PayRun; race konkret; period lock (O3); stale detection berbasis change log (O12); interaksi RLS dengan transaksi |
| [`03-background-processing.md`](03-background-processing.md) | Sync vs async; katalog job (producer, consumer, retry, backoff, DLQ, idempotent consumer, status); scheduler; model proses worker; graceful shutdown; poison message; fairness |

---

## Keputusan global (mengikat)

### 1. Isolation level
| Konteks | Isolation | Alasan |
|---|---|---|
| **Default semua transaksi tulis** | **Read Committed** + explicit row lock (`FOR UPDATE` / `FOR SHARE`) + guard `UPDATE … WHERE status = $expected` | Perilaku bisa diprediksi, tanpa retry storm di baris panas; setiap titik konkurensi terlihat eksplisit di SQL (ADR-011) |
| Baca input kalkulasi payroll per chunk (PayRun) | **Repeatable Read, READ ONLY** (transaksi baca terpisah) | Snapshot konsisten untuk satu chunk tanpa memblok penulis. Tulis hasil chunk dilakukan di transaksi Read Committed terpisah (lihat §5) |
| Verifikasi saldo/laporan (job) | Repeatable Read, READ ONLY | Σ entries dan saldo dibaca dari satu snapshot |
| Serializable | **Tidak dipakai di V1** | Dipertimbangkan dan ditolak untuk jalur panas (ADR-011). Kandidat masa depan untuk invariant lintas baris yang sulit dikunci |

### 2. Urutan lock global (anti-deadlock)
Setiap transaksi yang mengambil lebih dari satu lock **wajib** mengikuti urutan ini. Urutan yang dilewati boleh, mundur tidak boleh.

**LedgerPay**
```text
1. idempotency_keys (INSERT → lock implisit pada unique index)
2. baris kontrol/bisnis, berurutan: recon_items → webhook_inbox → payment_intents / payouts / topups … FOR UPDATE
3. accounts … WHERE id = ANY($ids) ORDER BY id FOR NO KEY UPDATE   (selalu ascending; NO KEY agar
   cek FK dari journal_entries (KEY SHARE) tidak ikut terblok — lock implisit FK dihitung di posisi ini)
4. INSERT journals / journal_entries / jobs / audit_log          (baris baru, tanpa konflik)
```
**PayRun**
```text
1. idempotency_keys (INSERT)
1b. tenant_settings … FOR KEY SHARE (penulis input) / FOR UPDATE (pembuatan payroll_periods)  — menutup celah D1
2. payroll_periods … FOR UPDATE / FOR SHARE   (beberapa periode: urut (year, month) ascending)
3. payroll_runs … FOR UPDATE   (atau guard UPDATE … WHERE status)
4. approval_requests (guard UPDATE … WHERE status='PENDING')
5. leave_balances … FOR UPDATE
6. baris subjek (leave_requests / overtime_requests / attendance_records / salary_assignments)
7. INSERT/UPSERT payroll_items / lines / snapshots / jobs / audit_log
```

### 3. Retry
| Kondisi | Tindakan | Batas |
|---|---|---|
| `40P01` deadlock, `40001` serialization | Ulangi **seluruh** fungsi transaksi (`WithTx` retry wrapper) | 3×, backoff 10 ms · 2ⁿ ± jitter 50% |
| `55P03` lock_timeout (2 dtk API) | **Tidak** di-retry di server → `409 RESOURCE_BUSY` / `REQUEST_IN_PROGRESS`; client retry dengan key sama | — |
| Error setelah efek eksternal (panggilan provider) | **Tidak pernah** retry transaksi secara buta. Lanjut lewat state machine + job | — |
| Error koneksi sebelum COMMIT | Aman di-retry (tidak ada yang tersimpan) | Sesuai wrapper |
| Error koneksi **saat** COMMIT (hasil tidak diketahui) | **Jangan retry di server.** Kembalikan 503; client retry dengan key yang sama → replay atau eksekusi pertama (FS-03) | — |

### 4. Aturan transaksi
1. **Tidak ada panggilan jaringan di dalam transaksi DB** (ADR-011). Panggilan eksternal selalu di antara dua transaksi, dan *intent*-nya di-commit lebih dulu.
2. Transaksi API target < 50 ms. Lock di-hold hanya selama itu.
3. Side effect asinkron **selalu** lewat INSERT `jobs` di transaksi yang sama (transactional outbox).
4. Job yang hasilnya ditulis ke DB menyelesaikan dirinya lewat `jobs.CompleteInTx` dengan fencing (ADR-006) di transaksi yang sama.
5. Semua transisi status memakai guard `WHERE status = $expected`. 0 baris → `409 INVALID_STATE_TRANSITION` (atau no-op untuk konsumer yang idempoten).

### 5. Pola chunk payroll (dipertegas)
```text
txRead  (REPEATABLE READ, READ ONLY, WithTenantTx): baca input semua employee di chunk
compute (di luar tx): engine.Calculate per employee → result + per-employee fingerprint
txWrite (READ COMMITTED, WithTenantTx):
   SELECT … FROM payroll_runs … FOR UPDATE; cek status = 'CALCULATING' AND calc_version = job.calc_version
   UPSERT items/lines/snapshots (calc_version) → UPDATE run SET chunks_done = chunks_done + 1 RETURNING …
   jobs.CompleteInTx (fencing) → jika chunk terakhir: total + fingerprint run + status CALCULATED
```
Kenapa baca dan tulis dipisah: menulis di Repeatable Read akan gagal `40001` setiap kali dua chunk paralel meng-update baris run yang sama (`chunks_done`). Input yang berubah di antara txRead dan txWrite tertangkap oleh mekanisme stale (§O12), jadi correctness tetap terjaga.

---

## Penyelesaian open items

### O3 — Race fingerprint di Read Committed (PI-13)
**Masalah:** approve membandingkan fingerprint. Di Read Committed, penulis input bisa commit tepat setelah approve membaca data tetapi sebelum approve commit, sehingga run disetujui dengan input yang sudah berubah.

**Keputusan:** serialisasi lewat baris `payroll_periods`.
- **Approve dan finalize** mengambil `SELECT … FROM payroll_periods WHERE … FOR UPDATE`.
- **Setiap penulis input** (attendance, leave/overtime approval, salary/komponen, employment) memanggil `PeriodGuard.Lock(tx, tenant, dates)`, yang mengambil `FOR SHARE` pada baris period yang tersentuh. Kalau baris period belum ada (belum ada run), tidak ada yang dikunci, dan itu aman karena belum ada fingerprint yang bisa basi.
- `FOR SHARE` vs `FOR SHARE` tidak saling memblok (penulis input tetap paralel satu sama lain). `FOR UPDATE` memblok keduanya, jadi approve melihat dunia yang stabil selama beberapa milidetik.

### O12 — Approve tanpa menghitung ulang fingerprint seluruh karyawan
**Keputusan:** tabel **`payroll_input_changes (tenant_id, period_id, employee_id, changed_at)`** (UPSERT, satu baris per employee per period) sebagai *penyaring kandidat*, ditambah verifikasi fingerprint hanya untuk kandidat.

**Mekanisme (urutan lock yang membuatnya benar):**
1. `POST /payroll-runs/{id}/calculate` mengambil `payroll_periods FOR UPDATE`, lalu mencatat `run.calc_started_at = clock_timestamp()` dan commit. Karena `FOR UPDATE`, semua penulis input yang sedang memegang `FOR SHARE` harus commit lebih dulu, sehingga perubahan mereka **pasti terlihat** oleh semua chunk (chunk baru membaca setelah calculate commit).
2. Setiap penulis input, lewat `PeriodGuard`, mengambil `FOR SHARE` pada period, **lalu** menulis `changed_at = clock_timestamp()` (diambil setelah lock didapat) di transaksi yang sama dengan perubahan. Penulis yang mendapat lock setelah calculate commit pasti punya `changed_at > calc_started_at`.
3. Approve (memegang `payroll_periods FOR UPDATE`, sehingga dunia stabil):
   - kandidat = `SELECT employee_id FROM payroll_input_changes WHERE period_id = $p AND changed_at > $run.calc_started_at`;
   - untuk setiap kandidat saja: hitung ulang fingerprint input saat ini, lalu bandingkan dengan `payroll_items.input_fingerprint`;
   - ada yang berbeda → `409 RUN_STALE {employees_changed}`.

**Celah tambahan yang ditutup (D1, ditemukan di file `02`):** kalau baris period **belum ada** saat penulis berjalan, penulis tidak punya baris untuk dikunci dan tidak menulis change log, sementara run bisa dibuat dan dikalkulasi sebelum penulis commit. Penutupnya: setiap penulis input mengambil `tenant_settings FOR KEY SHARE` lebih dulu, dan pembuatan `payroll_periods` mengambil `tenant_settings FOR UPDATE`. Dengan begitu, pembuatan period menunggu semua penulis yang sedang berjalan, dan penulis berikutnya pasti melihat period dan menguncinya. **Eligibility (D6):** cek stale juga mendeteksi karyawan yang sekarang eligible tetapi belum punya item, dan item untuk karyawan yang sudah tidak eligible.

**Kenapa benar:** perubahan yang tidak terlihat oleh kalkulasi pasti di-commit setelah chunk membaca, artinya setelah calculate commit, sehingga pasti masuk kandidat (langkah 1–2). Kandidat bisa juga berisi perubahan yang sebenarnya *sudah* terlihat oleh chunk (chunk yang berjalan belakangan). Verifikasi fingerprint di langkah 3 menyaring false positive tersebut. Biayanya O(jumlah perubahan sejak calculate), bukan O(jumlah karyawan).

- Fingerprint per item tetap disimpan (bukti reproducibility PI-12 dan audit).
- Recalculate hanya untuk karyawan yang berubah adalah optimasi V2. V1 tetap menghitung ulang seluruh run.
- Tabel ini dan kolom `payroll_runs.calc_started_at` ditambahkan ke skema (Step 4 diperbarui di file `02`).

### O11 — Wallet merchant sebagai hot row
**Keputusan V1:** tetap satu akun wallet merchant yang di-lock per pembayaran. Lock hanya di-hold ±2–5 ms (tanpa network di dalam tx), jadi batas teoretisnya ±200–500 pembayaran/detik untuk satu merchant. Angka ini jauh di atas kebutuhan portfolio, dan **dibuktikan dengan load test** (Step 9) sebelum dioptimasi.
**Jalur V2 (didokumentasikan, tidak dibangun):** pembayaran mengkredit satu akun `MERCHANT_PENDING:<merchant>` yang **non-materialized** (seperti akun sistem, ADR-L07), sehingga tidak ada row lock per pembayaran. Job sweep berkala memindahkan saldo ke wallet merchant dengan purpose journal baru `MERCHANT_SETTLEMENT`. Sharding tidak diperlukan karena akun non-materialized tidak di-lock. Trade-off: saldo merchant menjadi *eventually* tersedia, dan refund harus memperhitungkan dana pending. Detail di file `01` §5.

---

## Keputusan & penyelarasan selama Step 7

| Item | Keputusan | Diterapkan di |
|---|---|---|
| Lock akun LedgerPay | **`FOR NO KEY UPDATE`** (bukan `FOR UPDATE`), agar cek FK `KEY SHARE` dari `journal_entries` tidak terblok | §2, amandemen ADR-L03, file `01` |
| Celah D1 (period belum ada) | `tenant_settings FOR KEY SHARE` oleh penulis input; `FOR UPDATE` oleh pembuatan period | §2, O12, file `02` |
| Chunk txWrite | `payroll_runs FOR UPDATE` (bukan `FOR SHARE`), karena baris run di-UPDATE di tx yang sama | §5 |
| PI-16 (dua owner saling mencabut) | **Advisory lock** `owners:<tenant>`, bukan `tenants FOR UPDATE` (yang memblok cek FK semua INSERT tenant) | Step 3 PI-16, Step 4 `06`, file `02` |
| Hot row merchant V2 (O11) | Satu akun `MERCHANT_PENDING` non-materialized + sweep + purpose `MERCHANT_SETTLEMENT`; tanpa sharding | O11, file `01` §5 |
| Kolom & tabel baru | `payroll_runs.calc_started_at`, `payroll_input_changes` | Step 4 `06`, `07` |
| Nama job kanonik | `payroll.plan` (bukan `orchestrate`), `maintenance.*`, `notify.send`; dedupe chunk `payroll.calc_chunk:{run}:{v}:{n}` | file `03`, Step 2/4/5 diselaraskan |
| Webhook keluar `max_attempts` | 7 (1 + 6 retry sesuai kontrak Step 5) | Step 4 `01` |
| Top-up | Charge dibuat *best-effort* sinkron setelah tx1; fallback sweep `topup.recover` tiap 1 menit | file `03` |
| Payout status check | Satu job per check (`payout.status_check:{id}:{n}`), jadwal 30s/2m/10m/1h/6h, lalu `needs_reconciliation` | file `03` |
| Payslip render | Batch 20 slip per job; status "semua selesai" dihitung lewat query | file `03` |
| Scheduler | Tanpa leader election: `dedupe_key = <kind>:<time-bucket>` | file `03` |
| Job runner | `jobs.Permanent` → langsung dead; OnDead hook di tx yang sama; `locked_by` = uuid baru per start proses | file `03` |
| Pool & timeout | api 20, worker 10, mock 5; acquire timeout 2 dtk → `503`; worker `lock_timeout` 5 dtk; `statement_timeout` per kind; compose `stop_grace_period: 30s` | file `01`, `03` |
| Demo fault injection | Tabel `demo_faults` + titik `faults.Check`; worker keluar dengan `os.Exit(137)` di titik yang ditentukan (O15) | file `03` |

## Open items untuk step berikutnya

Masih berlaku: **O4 + O14** (metrik, Step 8), **O5** (chunk size demo, Step 11), **O16** (Step 12), **O17** (Step 11).

| # | Item | Step |
|---|---|---|
| O18 | Skema yang belum dispesifikasikan: `demo_faults` (demo only), `leave_accruals` (P2), kolom opsional `jobs.priority` (P2), purpose `MERCHANT_SETTLEMENT` (V2 saja) | Step 9 / Step 12 |
| O19 | Inbox: transisi status/outcome sementara di dalam satu tx (fencing UPDATE di awal, final di akhir). Pastikan CHECK kombinasi status × outcome di Step 4 tidak menolak keadaan antara, atau gunakan satu UPDATE di akhir | Coding / Step 12 |
| O20 | Load test harus membuktikan klaim O11 (±200–500 pembayaran/detik per merchant) dan biaya approve O12 | Step 8 / Step 9 |
