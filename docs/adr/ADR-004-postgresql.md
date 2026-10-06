# ADR-004 — PostgreSQL sebagai Satu-satunya Source of Truth

Status: Accepted · Tanggal: 2026-10-01 · Berlaku untuk: LedgerPay, PayRun

## Context / Problem

Kedua project menyimpan data yang menuntut **strong consistency**: ledger dan saldo (LedgerPay), payroll final dan isolasi tenant (PayRun). Banyak invariant di Step 1 sebaiknya ditegakkan oleh database, bukan hanya kode aplikasi, karena kode aplikasi bisa salah, sedangkan constraint tidak bisa dilewati. Database juga dipakai sebagai queue (ADR-006) agar tidak menambah infrastruktur.

## Options

| Opsi | Pros | Cons |
|---|---|---|
| **PostgreSQL** | ACID matang; constraint kaya (CHECK, EXCLUDE, partial unique); RLS; `SKIP LOCKED`; NUMERIC; JSONB; range types | Single-writer; scaling write butuh partitioning/sharding manual |
| MySQL (InnoDB) | Populer, ACID, replikasi matang | Tidak ada RLS native; tidak ada EXCLUDE constraint/range type; CHECK baru di 8.0.16; gap-lock behavior di REPEATABLE READ sering mengejutkan |
| MongoDB | Skema fleksibel; multi-document transaction tersedia | Constraint relasional (FK, CHECK lintas field) lemah; double-entry dan tenant isolation harus ditegakkan aplikasi; transaksi multi-dokumen punya batas dan biaya |
| CockroachDB / distributed SQL | Scaling horizontal, Serializable default | Biaya & kompleksitas operasional; retry transaksi sering di bawah contention; berlebihan untuk volume portfolio |

## Decision

**PostgreSQL** (versi stabil terbaru yang didukung managed provider pilihan, minimal 16) untuk kedua project, masing-masing dengan database sendiri.

Fitur yang **sengaja dipakai** (setiap fitur menegakkan invariant tertentu):

| Fitur | Dipakai untuk |
|---|---|
| Row lock `SELECT … FOR UPDATE` | Saldo wallet (ADR-L03) |
| `CHECK` constraint | `balance >= 0` untuk akun user; `amount > 0`; status valid |
| `EXCLUDE USING gist` + `daterange` | Effective dating tanpa overlap (ADR-P04) |
| Partial unique index | Satu payroll run aktif per (tenant, period) (ADR-P05); key idempotency |
| `FOR UPDATE SKIP LOCKED` | Job queue / outbox (ADR-006) |
| Row-Level Security | Isolasi tenant PayRun (ADR-P01) |
| `NUMERIC` | Perhitungan payroll (ADR-P06) |
| `JSONB` | Snapshot input payroll (ADR-P07), response idempotency, payload outbox/webhook |
| Trigger | Menolak UPDATE/DELETE pada tabel ledger (ADR-L01) dan audit log |

## Reason

- Invariant yang ditegakkan DB adalah **defense in depth**. Contohnya: kalaupun ada bug di lock ordering, `CHECK (balance >= 0)` tetap menolak saldo negatif.
- Satu teknologi data untuk OLTP, queue, dan audit berarti satu backup, satu monitoring, dan satu transaksi.
- Postgres sudah dipakai di pekerjaan sehari-hari, sehingga fitur lanjutannya (RLS, EXCLUDE, SKIP LOCKED) adalah area pendalaman yang realistis.

## Trade-offs

- Queue di Postgres menambah write load ke DB yang sama (diterima di volume ini; lihat ADR-006).
- Fitur khusus Postgres (RLS, EXCLUDE) mengunci kita ke Postgres. Migrasi ke DB lain akan mahal, dan ini diterima secara sadar.
- Scaling write vertikal dulu; sharding manual kalau benar-benar diperlukan.

## Future Migration

| Trigger | Perubahan |
|---|---|
| CPU/IO primary > 70% secara konsisten, atau query riwayat mengganggu OLTP | Read replica untuk riwayat/laporan; app tetap baca saldo dari primary |
| Tabel `journal_entries` / `payroll_items` > ratusan juta baris | Declarative partitioning per bulan (LedgerPay) / per tenant-hash (PayRun) |
| Write throughput melebihi kapasitas satu primary setelah tuning | Sharding berdasarkan `account_id`/`tenant_id` (V3 di dokumen arsitektur), atau evaluasi distributed SQL |
| Kebutuhan analitik berat | CDC (logical replication) ke warehouse, bukan query di primary |

## Interview angle

- *"Kenapa tidak cukup validasi di aplikasi?"* → Aplikasi bisa punya bug, punya banyak instance, atau ada script manual. Constraint DB adalah lapisan terakhir yang tidak bisa dilewati.
- *"Apa kelemahan memakai Postgres sebagai queue?"* → Bloat dari dead tuple, beban write tambahan, dan batas throughput. Jelaskan kapan pindah ke broker (ADR-006).
- *"Kenapa bukan MongoDB?"* → Masalah saya relasional dan butuh constraint lintas baris/tabel.

## Related

- [ADR-005](ADR-005-data-access-sqlc.md) · [ADR-006](ADR-006-background-processing-postgres-queue.md) · [ADR-011](ADR-011-transaction-management.md) · [ADR-P01](ADR-P01-multi-tenancy-pooled-rls.md) · [ADR-L01](ADR-L01-double-entry-ledger.md)
