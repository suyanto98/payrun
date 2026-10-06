# ADR-008 — Caching Strategy: Tidak Ada Application Cache di V1

Status: Accepted · Tanggal: 2026-10-01 · Berlaku untuk: LedgerPay, PayRun

## Context / Problem

Cache mempercepat read, tetapi memperkenalkan data basi dan bug invalidasi. Untuk data finansial (saldo, status pembayaran, payroll final), data basi bisa membuat user mengambil keputusan dari angka yang salah. Di sistem multi-tenant, cache key yang salah bisa **membocorkan data antar tenant** (Step 1 PayRun C1).

## Options

| Opsi | Pros | Cons |
|---|---|---|
| **Tanpa cache, baca dari primary** | Selalu benar; tidak ada invalidasi | Semua beban read ke DB |
| In-process cache (LRU) | Sangat cepat, tanpa infra | Tidak konsisten antar instance; invalidasi sulit |
| Redis cache | Shared antar instance | Infra tambahan; invalidasi; risiko key tanpa tenant |
| HTTP caching (ETag / Cache-Control) | Murah | Tidak cocok untuk data finansial pribadi |

## Decision

- **Tidak ada application cache di V1.**
- Data finansial (saldo, transaksi, payroll, payslip) selalu dibaca dari **primary** dengan response header `Cache-Control: no-store`.
- Asset statis SPA boleh di-cache agresif (nama file dengan hash).

**Aturan untuk cache di masa depan** (wajib jika cache ditambahkan):
1. Key **wajib** diawali tenant: `t:{tenant_id}:...`. Helper cache tidak menerima key tanpa tenant context.
2. Hanya untuk **reference data** yang jarang berubah: konfigurasi tenant, definisi komponen gaji, ruleset pajak ber-versi (immutable per versi, sehingga aman di-cache tanpa invalidasi).
3. Invalidasi saat write lewat outbox event, dengan TTL sebagai jaring pengaman.
4. Tidak pernah meng-cache saldo atau status transaksi.

## Reason

Belum ada bottleneck yang terukur. Index yang tepat + query sederhana cukup untuk volume portfolio. Cache yang ditambahkan tanpa bukti adalah over-engineering dan sumber bug.

## Trade-offs

- Beban read seluruhnya ke Postgres.
- Dashboard dengan agregasi bisa lambat pada data besar. Ini akan dideteksi lewat load test di Step 8.

## Future Migration

| Trigger | Perubahan |
|---|---|
| p95 endpoint reference data > target dan profiling menunjukkan DB sebagai penyebab | Cache read-through untuk reference data (aturan di atas) |
| Riwayat/laporan membebani primary | Read replica atau read model, bukan cache |

## Interview angle

- *"Bagaimana kamu meng-cache data multi-tenant dengan aman?"* → Tenant di key secara wajib lewat helper, hanya reference data, ruleset immutable per versi.
- *"Kenapa saldo tidak di-cache?"* → Saldo basi berarti keputusan salah. Satu query dengan index PK sudah sub-milidetik.

## Related

- [ADR-007](ADR-007-redis-not-in-v1.md) · [ADR-P01](ADR-P01-multi-tenancy-pooled-rls.md) · [ADR-P03](ADR-P03-payroll-engine.md)
