# ADR-011 — Transaction Management: Read Committed + Explicit Lock, Tanpa Network Call di dalam Transaksi

Status: Accepted · Tanggal: 2026-10-01 · Berlaku untuk: LedgerPay, PayRun

## Context / Problem

Hampir semua invariant di Step 1 bergantung pada transaksi yang benar: journal + saldo + idempotency + outbox harus atomik (LedgerPay C1, C3, C4); transisi run + item + audit harus atomik dan berjalan di bawah tenant context (PayRun C2, C5, C7). Ada empat pertanyaan:

1. Siapa yang membuka dan menutup transaksi?
2. Isolation level apa yang dipakai?
3. Bagaimana menangani error konkurensi (serialization failure, deadlock)?
4. Apa yang boleh terjadi di dalam transaksi?

## Options (isolation level)

| Level (Postgres) | Mencegah | Masih mungkin | Biaya |
|---|---|---|---|
| **Read Committed** (default) | Dirty read | Non-repeatable read, phantom, **lost update** (jika read-modify-write tanpa lock), write skew | Paling murah; lost update dicegah dengan `FOR UPDATE` / conditional UPDATE |
| Repeatable Read (snapshot isolation) | + non-repeatable read, phantom (di Postgres) | **Write skew**; update konflik menghasilkan error `40001` yang harus di-retry | Retry di bawah contention |
| Serializable (SSI) | Semua anomali | — | Error `40001` lebih sering; retry storm di hot row; overhead predicate lock |

Opsi lain untuk boundary transaksi:

| Opsi | Pros | Cons |
|---|---|---|
| Transaksi di handler HTTP | Sederhana | Logika bisnis bocor ke HTTP; worker tidak bisa memakai ulang |
| **Transaksi di service layer (`WithTx`)** | Satu use case = satu boundary; dipakai oleh API dan worker | Service harus meneruskan `tx` ke modul lain |
| Unit-of-work / `@Transactional`-like magic | Ringkas | Boundary tersembunyi, justru yang ingin ditunjukkan |

## Decision

| Aturan | Detail |
|---|---|
| Boundary | Service layer memiliki transaksi lewat `WithTx`. Repository menerima `pgx.Tx` / `Querier`; tidak pernah membuka transaksi sendiri |
| Isolation default | **Read Committed** + **explicit row lock** (`SELECT … FOR UPDATE`) atau **conditional UPDATE** (`UPDATE … WHERE status = $expected` lalu cek `RowsAffected`) di setiap read-modify-write |
| Level lebih tinggi | Boleh **per use case** dengan ADR/komentar (contoh: `REPEATABLE READ READ ONLY` untuk membaca input satu chunk payroll secara konsisten) |
| Network call | **Dilarang** di dalam transaksi DB (HTTP ke provider, email, S3). Gunakan outbox/job (ADR-006) atau pola recovery point (ADR-L02) |
| Retry | Hanya **seluruh transaksi** yang di-retry, untuk SQLSTATE `40001` (serialization) dan `40P01` (deadlock): maksimal 3×, backoff 10–50 ms + jitter. Error lain tidak di-retry |
| Timeout (titik awal) | API: `lock_timeout = 2s`, `statement_timeout = 5s`, `idle_in_transaction_session_timeout = 10s`. Worker: `statement_timeout` lebih panjang per kind |
| Tenant (PayRun) | `WithTenantTx` = `WithTx` + `SET LOCAL app.tenant_id` di awal (ADR-P02) |

```go
func (db *DB) WithTx(ctx context.Context, fn func(q *sqlc.Queries) error) error {
    return retry(ctx, 3, isRetryable /*40001,40P01*/, func() error {
        tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
        if err != nil { return err }
        defer tx.Rollback(ctx) // no-op setelah commit
        if err := fn(sqlc.New(tx)); err != nil { return err }
        return tx.Commit(ctx)
    })
}
```

## Reason

- Read Committed + lock eksplisit membuat **titik serialisasi terlihat di kode** dan tidak menghasilkan retry storm pada wallet yang ramai (Demo B: 100 request ke satu saldo). SERIALIZABLE akan menghasilkan banyak `40001` pada skenario itu.
- **Kenapa tidak ada network call di dalam transaksi:** (1) lock ditahan selama latensi jaringan, sehingga throughput turun dan deadlock/timeout naik; (2) jika HTTP sukses lalu `COMMIT` gagal, efek eksternal terjadi tanpa catatan internal, yaitu dual-write; (3) koneksi pool habis saat provider lambat. Ini akar masalah payout unknown outcome (ADR-L06).
- Me-retry seluruh transaksi (bukan satu statement) menjamin tidak ada setengah efek yang diulang.
- Timeout membatasi dampak satu request lambat terhadap seluruh pool.

## Trade-offs

- Developer harus ingat menambahkan lock pada setiap read-modify-write. Mitigasi: concurrency test untuk setiap use case keuangan + review checklist.
- Retry otomatis bisa menutupi masalah contention. Mitigasi: metrik `db_tx_retries_total{reason}`.
- Pola "tanpa network call di transaksi" membuat beberapa alur jadi asinkron dan state machine lebih panjang (diterima; itu justru realistis).

## Future Migration

| Trigger | Perubahan |
|---|---|
| Ditemukan invariant lintas baris yang sulit dikunci (write skew) | SERIALIZABLE untuk use case tersebut saja, dengan retry |
| Contention tinggi di baris tertentu (hot row) | Lihat ADR-L07 (sharding sub-akun, agregasi async) |
| Operasi lintas service (setelah ekstraksi) | Saga + outbox; tidak memakai 2PC |

## Interview angle

- *"Isolation level apa yang kamu pakai, dan kenapa bukan Serializable?"* → Jelaskan tabel anomali dan retry storm di hot row.
- *"Kenapa tidak memanggil bank di dalam transaksi?"* → Lock held + dual-write + pool exhaustion.
- *"Apa yang terjadi saat deadlock?"* → Postgres membatalkan salah satu → `40P01` → retry seluruh transaksi. Lock ordering mencegahnya terjadi sejak awal (ADR-L03).

## Related

- [ADR-L03](ADR-L03-concurrency-pessimistic-locking.md) · [ADR-L02](ADR-L02-idempotency.md) · [ADR-006](ADR-006-background-processing-postgres-queue.md) · [ADR-P02](ADR-P02-tenant-context.md) · [ADR-P05](ADR-P05-payroll-run-execution.md)
