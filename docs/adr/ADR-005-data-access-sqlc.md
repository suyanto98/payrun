# ADR-005 — Data Access: sqlc + pgx, Migrasi goose

Status: Accepted · Tanggal: 2026-10-01 · Berlaku untuk: LedgerPay, PayRun

## Context / Problem

Nilai inti kedua project ada pada **query yang tepat**: lock (`FOR UPDATE`), queue (`SKIP LOCKED`), upsert idempoten (`ON CONFLICT`), conditional transition (`UPDATE … WHERE status = $expected`), dan `SET LOCAL` untuk tenant. Semua itu harus terlihat jelas di kode agar bisa ditunjukkan ke interviewer dan di-review. Di pekerjaan sehari-hari kandidat memakai ORM (GORM). Project ini sengaja memilih pendekatan yang berbeda dan harus bisa menjelaskan alasannya.

## Options

| Opsi | Pros | Cons |
|---|---|---|
| **GORM** (ORM) | Cepat untuk CRUD; familiar dari pekerjaan | SQL tersembunyi; lock & `SKIP LOCKED` lewat `clause.Locking` kurang terlihat; N+1 mudah terjadi tanpa disadari; error baru ketahuan saat runtime |
| **sqlc + pgx/v5** | SQL ditulis eksplisit, Go code di-generate dan **diperiksa saat compile** terhadap skema; tanpa reflection; pgx memberi akses penuh ke fitur Postgres | Query dinamis (filter opsional) lebih repot; tetap menulis SQL untuk CRUD sederhana |
| Query builder (squirrel / goqu) | Fleksibel untuk query dinamis | Tidak ada pemeriksaan terhadap skema; kesalahan kolom baru terlihat saat runtime |
| `database/sql` mentah | Tanpa dependensi | Boilerplate scan; rawan typo; tidak ada type safety |

## Decision

- **sqlc** (engine postgresql, driver `pgx/v5`) untuk semua query. SQL ada di `db/queries/<module>.sql`.
- **pgx/v5 `pgxpool`** sebagai driver/pool.
- Query dinamis yang benar-benar perlu (filter list admin) memakai pola `($1::text IS NULL OR col = $1)` di sqlc. Builder kecil boleh dipakai di satu tempat dengan test.
- **goose** untuk migrasi berbasis **file SQL** (`db/migrations/NNNNN_name.sql`), dijalankan lewat subcommand `migrate` (bukan otomatis saat startup `api`).
- Migrasi wajib **expand/contract**: tambah kolom nullable/default → deploy kode yang menulis dua-duanya → backfill → deploy kode yang membaca kolom baru → hapus kolom lama di rilis berikutnya.

## Reason

- Saat interviewer bertanya *"tunjukkan di mana kamu mengunci wallet"*, jawabannya adalah satu baris SQL yang terbaca, bukan rantai method ORM.
- Compile-time check menangkap kolom yang salah ketik atau tipe yang tidak cocok sebelum test berjalan.
- RLS dan `SET LOCAL` (PayRun) berjalan di koneksi yang sama dengan query. pgx memberi kontrol eksplisit atas transaksi dan koneksi.
- Kontras dengan ORM di pekerjaan memberi cerita interview yang jujur: *"ORM produktif untuk CRUD ERP. Untuk sistem yang nilainya ada di locking dan constraint, saya memilih SQL eksplisit."*

## Trade-offs

- Lebih banyak SQL yang ditulis untuk CRUD sederhana (PayRun punya banyak entitas).
- Query dinamis kurang ergonomis.
- `sqlc generate` menjadi langkah build yang wajib (dicek di CI: hasil generate harus sama dengan yang di-commit).

## Future Migration

| Trigger | Perubahan |
|---|---|
| Laporan dengan filter sangat dinamis | Query builder terisolasi di modul reporting, atau read model khusus |
| Migrasi butuh backfill besar | Backfill sebagai job batch terpisah (chunked, idempoten), bukan di dalam file migrasi |

## Interview angle

- *"Kenapa tidak pakai ORM?"* → Bandingkan secara jujur: ORM lebih cepat untuk CRUD, tetapi menyembunyikan lock dan query plan. Untuk project ini visibilitas lebih penting.
- *"Bagaimana menjalankan migrasi tanpa downtime?"* → Expand/contract + migrasi sebagai langkah deploy terpisah + kode kompatibel dengan skema N dan N+1.
- *"Bagaimana mencegah N+1?"* → Query eksplisit per use case; prefetch per chunk (payroll). N+1 terlihat langsung di kode karena tidak ada lazy loading.

## Related

- [ADR-004](ADR-004-postgresql.md) · [ADR-011](ADR-011-transaction-management.md) · [ADR-013](ADR-013-deployment.md)
