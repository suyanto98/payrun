# ADR-003 — REST + JSON, Spec-first OpenAPI 3.1

Status: Accepted · Tanggal: 2026-10-01 · Berlaku untuk: LedgerPay, PayRun

## Context / Problem

Ada tiga jenis client: SPA Vue (kedua project), server merchant (LedgerPay, machine-to-machine), dan curl/k6 untuk demo dan load test. API harus mudah didemokan, punya kontrak tertulis, dan mendukung semantik pembayaran (idempotency, error yang bisa diproses mesin).

## Options

| Opsi | Pros | Cons |
|---|---|---|
| **REST + JSON + OpenAPI** | Standar industri payment (Stripe, Xendit, Midtrans); mudah dipanggil dengan curl; caching/idempotency semantics HTTP jelas | Over/under-fetching; kontrak bisa drift kalau spec tidak ditegakkan |
| gRPC | Kontrak ketat (protobuf), performa tinggi, streaming | Tidak bisa dipanggil langsung dari browser tanpa proxy; merchant API publik jarang memakai gRPC; demo dengan curl lebih sulit |
| GraphQL | Fleksibel untuk UI yang kaya | Idempotency dan rate limit per operasi lebih rumit; query sewenang-wenang bisa jadi beban DB yang sulit diprediksi; otorisasi per field menambah permukaan BOLA |

## Decision

| Aspek | Keputusan |
|---|---|
| Style | REST resource-oriented, JSON, versi di path: `/v1/...` |
| Kontrak | **Spec-first**: `api/openapi.yaml` (OpenAPI 3.1) adalah sumber kebenaran; `oapi-codegen` menghasilkan types + server interface (chi); CI gagal jika kode tidak sesuai spec |
| Error | **RFC 9457 Problem Details** (`application/problem+json`) + `code` stabil yang bisa diproses mesin + `request_id` |
| Idempotency | Header `Idempotency-Key` wajib untuk endpoint yang mengubah uang/state penting. Semantik lengkap ada di [ADR-L02](ADR-L02-idempotency.md) (dan ADR-P05 untuk payroll run). Response replay memakai header `Idempotent-Replayed: true` |
| Pagination | **Cursor-based** (`?limit=50&cursor=<opaque>`), cursor = encode `(created_at, id)` |
| Tracing | Header `X-Request-ID` diterima/di-generate dan dikembalikan |
| Uang di JSON | **Selalu string**, tidak pernah JSON number: LedgerPay `"amount": "5000000"` (minor unit, [ADR-L04](ADR-L04-money-representation.md)); PayRun `"amount": "4250000.00"` (decimal, [ADR-P06](ADR-P06-decimal-rounding.md)). Alasannya: JavaScript number kehilangan presisi di atas 2^53, dan float tidak boleh menyentuh uang |
| 404 vs 403 | Resource yang tidak boleh *diketahui* user → 404; resource yang terlihat tetapi aksinya tidak diizinkan → 403 ([ADR-010](ADR-010-authorization.md)) |

Contoh error:

```json
{
  "type": "https://docs.ledgerpay.local/errors/insufficient-balance",
  "title": "Insufficient balance",
  "status": 422,
  "detail": "Wallet balance is lower than the requested amount.",
  "code": "INSUFFICIENT_BALANCE",
  "request_id": "01JA2Z8K6Q3M7V..."
}
```

## Reason

- Demo (Step 1 Demo A/B) dijalankan dengan curl/k6. REST membuatnya bisa dibaca di layar.
- Merchant persona (Budi) mengharapkan pola API seperti PG nyata.
- Spec-first memaksa desain kontrak terjadi **sebelum** coding (Step 5), sesuai keinginan user, dan mencegah drift.
- `code` yang stabil memisahkan pesan untuk manusia dari logika client.
- **Cursor vs offset:** riwayat transaksi terus bertambah. Offset menjadi lambat (`OFFSET 100000` tetap memindai baris) dan tidak stabil (baris baru menggeser halaman sehingga ada item dobel/terlewat). Cursor pada index `(created_at, id)` konstan dan stabil.

## Trade-offs

- Codegen menambah langkah build dan sedikit kode boilerplate.
- Cursor pagination tidak bisa "loncat ke halaman 37". Ini diterima untuk riwayat transaksi; tabel admin kecil boleh memakai offset.
- REST kurang efisien untuk dashboard yang butuh banyak resource sekaligus. Diterima, karena dashboard sederhana.

## Future Migration

| Trigger | Perubahan |
|---|---|
| Ada service internal yang saling memanggil dengan volume tinggi (setelah ekstraksi ADR-001) | gRPC untuk komunikasi internal; REST tetap untuk publik |
| Breaking change pada API merchant | `/v2` paralel dengan deprecation window; header `Sunset` |
| UI menjadi sangat kaya dengan banyak agregasi | BFF (backend-for-frontend) khusus SPA, bukan GraphQL publik |

## Interview angle

- *"Kenapa bukan GraphQL?"* → Endpoint finansial butuh semantik idempotency dan rate limit per operasi yang jelas. GraphQL membuat keduanya serta otorisasi per field lebih sulit.
- *"Kenapa cursor pagination?"* → Performa konstan dan hasil stabil saat data terus bertambah. Jelaskan bentuk cursor dan index-nya.
- *"Bagaimana menjaga dokumentasi API tidak basi?"* → Spec-first + codegen + CI gate.

## Related

- [ADR-L02](ADR-L02-idempotency.md) · [ADR-009](ADR-009-authentication.md) · [ADR-010](ADR-010-authorization.md)
