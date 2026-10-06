# ADR-006 — Background Processing: Postgres-backed Outbox + Job Queue

Status: Accepted · Tanggal: 2026-10-01 · Berlaku untuk: LedgerPay, PayRun

## Context / Problem

Banyak pekerjaan tidak boleh (atau tidak perlu) dijalankan di request HTTP:

- LedgerPay: memanggil bank untuk payout, mengirim webhook ke merchant, memproses webhook masuk, reconciliation, notifikasi.
- PayRun: kalkulasi payroll per chunk, generate PDF payslip, import CSV absensi, job terjadwal.

Masalah utamanya adalah **dual-write** (Step 1 LedgerPay C4): jika kode melakukan `COMMIT` ke DB lalu `publish` ke broker, salah satu bisa gagal. Hasilnya, pesan hilang (uang sudah di-hold tetapi payout tidak pernah dikirim) atau pesan terkirim untuk transaksi yang di-rollback.

## Options

| Opsi | Pros | Cons |
|---|---|---|
| **Postgres outbox + job table, worker `SKIP LOCKED`** (hand-rolled) | Job ditulis **dalam transaksi yang sama** dengan efek bisnis, sehingga dual-write tidak ada; tanpa infrastruktur tambahan; bisa di-query dengan SQL untuk observability | Throughput terbatas oleh Postgres; bloat tabel; fitur (cron, uniqueness, UI) harus dibuat sendiri |
| **River** (library queue Go di atas Postgres) | Matang: unique jobs, retry, cron, UI; tetap transaksional dengan pgx | Kedalaman implementasi tersembunyi di library; kurang bahan untuk interview |
| **RabbitMQ** (dipakai di pekerjaan) | Routing fleksibel, ack/nack, DLX, throughput tinggi | Tetap butuh outbox untuk menghindari dual-write; satu komponen infra lagi untuk dioperasikan & dimonitor |
| **Kafka** | Log terdistribusi, replay, fan-out masif | Sangat berlebihan untuk volume ini; operasional berat |
| Redis Streams / BullMQ-like | Cepat | Persistence/durability perlu konfigurasi hati-hati; tetap dual-write |

## Decision

**Postgres-backed queue yang ditulis sendiri satu kali**, sebagai package `platform/jobs`, dipakai oleh kedua project.

Bentuk tabel (sketsa, DDL final di Step 4):

```text
jobs(id, kind, payload jsonb, tenant_id NULL, status[queued|running|succeeded|dead],
     attempts, max_attempts, run_at, locked_by, locked_until, last_error,
     dedupe_key NULL UNIQUE, created_at, updated_at)
```

Outbox = INSERT ke `jobs` **di dalam transaksi bisnis**. Worker mengambil job:

```sql
UPDATE jobs SET status='running', locked_by=$worker, locked_until=now()+interval '30 seconds',
               attempts = attempts + 1
WHERE id IN (
  SELECT id FROM jobs
  WHERE (status='queued' AND run_at <= now())
     OR (status='running' AND locked_until < now())     -- lease kedaluwarsa = worker mati
  ORDER BY run_at
  LIMIT $batch
  FOR UPDATE SKIP LOCKED
)
RETURNING *;
```

| Mekanisme | Aturan |
|---|---|
| Lease + heartbeat | Job panjang memperpanjang `locked_until` secara berkala. Jika worker mati, lease habis dan job diambil worker lain |
| Retry | Gagal → `status='queued'`, `run_at = now() + backoff` dengan `backoff = min(base · 2^attempts, cap) ± jitter` (awal: base 2s, cap 10 menit) |
| DLQ | `attempts >= max_attempts` → `status='dead'`; alert + tombol replay manual di ops console |
| Delivery | **At-least-once.** Setiap handler **wajib idempoten** (upsert, transisi state kondisional, dedupe key) |
| Latensi | Polling interval pendek (±500 ms); `LISTEN/NOTIFY` opsional untuk membangunkan worker segera |
| Penyelesaian transaksional | Runner menyediakan `jobs.CompleteInTx(tx, job)` dengan **fencing**: `UPDATE jobs SET status='succeeded' WHERE id=$1 AND locked_by=$worker AND status='running'`. Kalau hasilnya 0 baris, lease worker ini sudah diambil worker lain (*zombie worker*), sehingga seluruh transaksi bisnis di-rollback. Tanpa fencing, worker yang lambat bisa, misalnya, menambah `chunks_done` dua kali (Step 3 PFS-02). Dengan begitu, handler bisa menandai job `succeeded` **di transaksi yang sama** dengan kerja bisnisnya (dipakai oleh chunk payroll, ADR-P05, dan inbox webhook, ADR-L08). Handler yang memanggil pihak luar tetap memakai state machine + idempotency key, karena panggilan jaringan tidak bisa ikut transaksi |
| Ordering | Tidak dijamin global. Handler yang butuh urutan memakai transisi state monoton, bukan urutan job |

### Amandemen (Step 11): verifikasi rencana query claim
Kondisi `(status='queued' AND run_at <= now()) OR (status='running' AND locked_until < now())` bisa berakhir dengan BitmapOr + sort, bukan index scan yang rapi pada partial index. Kalau `EXPLAIN (ANALYZE, BUFFERS)` saat coding menunjukkan itu bermasalah, pakai alternatif yang lebih sederhana: **lease reaper** terpisah (`UPDATE jobs SET status='queued', run_at=now() WHERE status='running' AND locked_until < now()`, dijalankan tiap beberapa detik), sehingga query claim hanya memakai partial index `ix_jobs_claimable (run_at) WHERE status='queued'`. Fencing tetap aman, karena `CompleteInTx` mensyaratkan `status='running' AND locked_by=$worker`. Job yang sudah di-reap tidak bisa diselesaikan oleh worker zombie.

### Catatan implementasi (Sprint 6)
- **Satu sumber waktu:** `run_at` untuk backoff, delay status check, dan claim semuanya dihitung dengan `now()` database (`make_interval(secs => …)`), bukan jam aplikasi. Jam aplikasi (fake clock di test, drift di produksi) yang berbeda dari jam DB membuat job jatuh tempo terlalu cepat atau terlambat.
- **Scheduler:** `EnqueueScheduledJob` memakai `INSERT … SELECT … WHERE NOT EXISTS (dedupe_key)` + `ON CONFLICT` partial unique. Celah "bucket sudah succeeded lalu replika lain meng-enqueue lagi" (yang diterima desain) tertutup selama retensi job; `Tick` boleh dipanggil berulang setiap 15 dtk tanpa logika `Due()`.
- **Lane:** tiga `Runner` (money 4, webhook-out 8, maintenance 2) + loop inbox. `ReleaseJob` saat shutdown mengembalikan job yang dibatalkan tanpa menghitung percobaan.
- Query claim memakai bentuk satu-query (queued OR lease expired); verifikasi `EXPLAIN` dibawa ke Sprint 9.

## Reason

- Menghapus dual-write dengan cara paling sederhana: satu `COMMIT` mencakup efek bisnis dan pekerjaan lanjutannya.
- Volume portfolio (ratusan–ribuan job/menit di load test) jauh di bawah batas praktis Postgres queue.
- Menulisnya sendiri memberi bahan interview yang dalam (lease, visibility, idempotent consumer, poison message). Dibangun sekali di LedgerPay lalu dipakai ulang PayRun (Step 1 `03-cross-project.md` §1.1).
- **Kenapa bukan RabbitMQ**, padahal sudah familiar dari pekerjaan: RabbitMQ tidak menghapus dual-write. Kita tetap butuh outbox + relay ke broker, ditambah broker yang harus dioperasikan. Tanpa kebutuhan fan-out atau throughput, broker hanya menambah failure mode.
- **Kenapa bukan River:** River adalah pilihan production yang sangat masuk akal dan menjadi jalur migrasi utama. Hand-rolled dipilih karena tujuan portfolio adalah *memahami* mekanismenya.

## Trade-offs

- Polling menambah query konstan ke DB (kecil, tetapi ada).
- Tabel `jobs` cepat bloat; butuh autovacuum yang dituning + pembersihan job selesai (retensi 7 hari).
- Fitur seperti cron, unique job, dan dashboard harus dibuat sendiri (minimal: query SQL + halaman ops sederhana).
- Risiko bug di job runner buatan sendiri. Mitigasi: integration test untuk lease kedaluwarsa, crash, dan concurrent workers.

## Future Migration

| Trigger | Perubahan |
|---|---|
| Fitur job runner yang dibutuhkan makin banyak (cron kompleks, rate limit per kind, UI) | Ganti `platform/jobs` dengan **River** (sama-sama Postgres + pgx, tetap transaksional) |
| Throughput job > beberapa ribu/detik atau polling membebani primary | Outbox tetap di Postgres, relay ke **RabbitMQ/Kafka** (atau CDC via logical replication/Debezium) |
| Banyak consumer independen untuk event yang sama (analytics, notifikasi, fraud) | Event streaming (Kafka) dengan outbox sebagai sumber |
| Service diekstrak (ADR-001) | Broker menjadi kontrak antar service |

## Interview angle

- *"Bagaimana kamu menjamin pesan tidak hilang setelah commit?"* → Transactional outbox: job ditulis di transaksi yang sama.
- *"Exactly-once?"* → Tidak ada exactly-once delivery. Yang ada at-least-once delivery + idempotent processing, sehingga **efeknya** terjadi tepat sekali.
- *"Apa yang terjadi kalau worker mati di tengah job?"* → Lease habis → job diambil ulang → handler idempoten. Tunjukkan Demo C LedgerPay / Demo B PayRun.
- *"Kapan kamu pindah ke Kafka/RabbitMQ?"* → Sebutkan trigger konkret di tabel atas, bukan "kalau sudah besar".

## Related

- [ADR-004](ADR-004-postgresql.md) · [ADR-011](ADR-011-transaction-management.md) · [ADR-L06](ADR-L06-payout-unknown-outcome.md) · [ADR-L08](ADR-L08-inbound-webhook-inbox.md) · [ADR-P05](ADR-P05-payroll-run-execution.md)
