# ADR-001 — Modular Monolith

Status: Accepted · Tanggal: 2026-10-01 · Berlaku untuk: LedgerPay, PayRun

## Context / Problem

Kedua project harus selesai dikerjakan oleh satu orang, tetapi tetap menunjukkan batas domain yang jelas. Masalah inti keduanya adalah **konsistensi**:

- LedgerPay: journal, saldo, idempotency key, dan outbox harus ter-commit **atomik** (Step 1 LedgerPay C1, C3, C4).
- PayRun: status payroll run, item, dan audit harus konsisten; setiap akses DB harus melewati tenant context (Step 1 PayRun C1, C5).

Pertanyaannya: bagaimana memecah kode agar modular tanpa memecah transaksi?

## Options

| Opsi | Pros | Cons |
|---|---|---|
| **Layered monolith** (`handlers/`, `services/`, `repositories/` global) | Paling sederhana, familiar | Batas domain kabur; setiap service bisa membaca tabel mana saja; sulit dipecah kelak |
| **Modular monolith** (package per modul, satu binary, satu DB) | Satu DB transaction lintas modul; batas domain eksplisit; deployment sederhana | Disiplin dependency harus ditegakkan oleh tooling; satu unit deploy |
| **Microservices** (wallet, ledger, payout, … sebagai service terpisah) | Scaling & deployment independen; isolasi kegagalan | Transfer lintas service butuh saga/2PC; network failure di setiap call; observability terdistribusi; infra berlipat; tidak ada tim yang membutuhkan otonomi |

## Decision

**Modular monolith.** Satu binary Go dengan subcommand `api`, `worker`, `migrate`, `seed`; satu database PostgreSQL per project.

Aturan dependency (ditegakkan dengan `depguard` di golangci-lint + struktur `internal/`):

| Aturan | Contoh |
|---|---|
| `internal/platform/*` **tidak boleh** import `internal/modules/*` | `platform/db` tidak tahu ada modul `ledger` |
| Modul berkomunikasi lewat **interface service** yang diekspor, bukan lewat tabel modul lain | `payout` memanggil `ledger.Service.Post(ctx, tx, journal)`, bukan `INSERT INTO journal_entries` |
| Modul boleh ikut dalam transaksi pemanggil (`tx` diteruskan lewat context/parameter) | `transfer` membuka tx, lalu `ledger` dan `idempotency` menulis di tx yang sama |
| Tidak ada siklus import antar modul | dicek otomatis oleh compiler Go + depguard |

**Satu-satunya proses terpisah adalah `mock-provider`** (LedgerPay). Proses ini merepresentasikan **sistem eksternal** (payment gateway, bank), jadi batas jaringannya memang nyata: timeout, signature, retry. Lihat ADR-L05.

## Reason

- Invariant finansial dan workflow payroll butuh **satu transaksi ACID**. Microservices akan mengubah satu `COMMIT` menjadi saga dengan compensating action, dan itu kompleksitas yang tidak diminta domain.
- Modularitas tetap terbukti: batas modul terlihat di kode dan diperiksa di CI.
- Proses `api` dan `worker` dipisah saat runtime sehingga bisa di-scale terpisah, walaupun berasal dari binary yang sama.

## Trade-offs

- Satu bug fatal (panic tanpa recover, memory leak) memengaruhi seluruh `api`.
- Semua modul ter-deploy bersamaan; tidak bisa merilis `payout` tanpa merilis `transfer`.
- Disiplin batas modul bergantung pada linter. Kalau linter dimatikan, monolith bisa membusuk menjadi "big ball of mud".

## Future Migration

| Trigger | Perubahan |
|---|---|
| Lebih dari satu tim mengubah modul yang sama dan saling menghambat release | Ekstrak modul dengan batas paling bersih (contoh: notification, merchant webhook dispatcher) menjadi service |
| Write throughput ledger menjadi bottleneck yang tidak bisa diselesaikan di satu Postgres (setelah tuning & partitioning) | Ledger menjadi service tersendiri dengan API `Post(journal)` yang idempoten; saga + suspense account untuk operasi lintas service (V3 di arsitektur) |
| Worker butuh resource yang sangat berbeda (contoh: PDF rendering CPU-heavy) | Binary worker terpisah per jenis job, tetap satu codebase |

Karena modul hanya berinteraksi lewat interface, ekstraksi berarti mengganti implementasi interface dengan client HTTP/gRPC, bukan menulis ulang domain.

## Interview angle

- *"Kenapa bukan microservices?"* → Karena invariant saya butuh satu transaksi. Microservices tanpa kebutuhan organisasi atau scaling yang berbeda hanya menukar `COMMIT` dengan saga.
- *"Bagaimana kamu mencegah modul saling membaca tabel?"* → depguard + package `internal` + review. Contohnya: modul payout hanya bisa memposting journal lewat `ledger.Service`.
- *"Modul mana yang pertama kamu pecah, dan kenapa?"* → Dispatcher webhook/notifikasi: async, tidak butuh transaksi bersama, dan sudah berkomunikasi lewat outbox.

## Related

- [ADR-006](ADR-006-background-processing-postgres-queue.md) — outbox sebagai jahitan antar modul
- [ADR-011](ADR-011-transaction-management.md) — boundary transaksi
- [ADR-L05](ADR-L05-mock-provider.md) — mock-provider sebagai proses terpisah
