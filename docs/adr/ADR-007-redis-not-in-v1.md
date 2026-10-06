# ADR-007 — Redis Tidak Dipakai di V1

Status: Accepted · Tanggal: 2026-10-01 · Berlaku untuk: LedgerPay, PayRun

## Context / Problem

Redis sering dimasukkan ke stack secara default (cache, session, rate limit, idempotency, lock terdistribusi). Setiap komponen infrastruktur menambah biaya, failure mode, dan pekerjaan operasional. Pertanyaannya: masalah apa di V1 yang **hanya** bisa diselesaikan Redis? (Berubah dari v1, yang memakai Redis di LedgerPay.)

## Options

| Opsi | Pros | Cons |
|---|---|---|
| **Tanpa Redis di V1** | Infra lebih sedikit; semua state transaksional di Postgres | Rate limit hanya per-instance / di Postgres; tidak ada cache cepat |
| Redis untuk idempotency | Cepat | **Tidak transaksional dengan efek finansial.** Kalau Redis menyimpan "selesai" tetapi DB rollback (atau sebaliknya), jaminan idempotency rusak |
| Redis untuk rate limit + cache | Pola standar | Belum ada bukti kebutuhan; menambah dependency di readiness check |
| Redis lock terdistribusi (Redlock) | Mutual exclusion lintas instance | Row lock Postgres sudah memberi mutual exclusion **yang terikat ke transaksi**; Redlock punya kelemahan yang dikenal (clock drift, GC pause) |

## Decision

**Redis tidak dipakai di V1.**

- Idempotency: Postgres, di transaksi yang sama ([ADR-L02](ADR-L02-idempotency.md)). Ini **tidak akan pernah** dipindah ke Redis saja.
- Rate limiting V1: middleware in-memory per instance (token bucket) untuk login/OTP-like endpoints. Cukup karena V1 hanya punya satu instance API.
- Lock: row lock Postgres.
- Session: tidak ada server session (JWT + refresh token di Postgres, [ADR-009](ADR-009-authentication.md)).

## Reason

Tidak ada masalah V1 yang membutuhkan Redis. Menambahkannya hanya membuat project terlihat lebih kompleks, dan itu dilarang oleh prinsip Step 1.

## Trade-offs

- Rate limit per-instance tidak akurat jika API di-scale ke >1 instance.
- Tidak ada cache cepat untuk read-heavy endpoint (lihat [ADR-008](ADR-008-caching.md)).

## Future Migration

| Trigger | Perubahan |
|---|---|
| API berjalan > 1 instance dan butuh rate limit global (brute force login, API key merchant) | Redis untuk rate limiting (sliding window / GCRA) |
| Profiling menunjukkan reference data dibaca berulang dan menjadi bottleneck | Redis sebagai cache read-through dengan key tenant-aware |
| Butuh dedupe cepat untuk webhook masuk volume sangat tinggi | Redis sebagai *pre-filter* saja; dedupe autoritatif tetap di Postgres |

## Interview angle

- *"Kenapa tidak simpan idempotency key di Redis?"* → Karena tidak atomik dengan efek finansial. Jelaskan skenario crash di antara keduanya.
- *"Kapan kamu menambah Redis?"* → Saat ada >1 instance dan butuh rate limit global, atau ada bottleneck read yang terukur.

## Related

- [ADR-008](ADR-008-caching.md) · [ADR-L02](ADR-L02-idempotency.md) · [ADR-L03](ADR-L03-concurrency-pessimistic-locking.md)
