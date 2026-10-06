# Step 5 — API Contract

> Input: Step 2 (feature spec, ADR-003 REST/OpenAPI, ADR-009 auth, ADR-010 authz), Step 3 (state machine, failure scenario), Step 4 (skema, constraint → error).
> Prinsip dari master prompt: **endpoint hanya untuk use case yang benar-benar diperlukan.** Setiap endpoint harus bisa ditelusuri ke fitur P0/P1. Tidak ada CRUD generik "karena biasanya ada".
> Sumber kebenaran saat coding adalah `api/openapi.yaml` per project (spec-first, ADR-003). Dokumen ini menjadi acuan untuk menulis spec tersebut.

## Isi

| File | Isi |
|---|---|
| [`01-error-catalogue.md`](01-error-catalogue.md) | Katalog error code kedua project: HTTP status, arti, retryable, tindakan client, sumber |
| [`02-ledgerpay-api.md`](02-ledgerpay-api.md) | Kontrak endpoint LedgerPay (user, merchant, webhook masuk, ops, health) |
| [`03-ledgerpay-external-contracts.md`](03-ledgerpay-external-contracts.md) | Kontrak ke luar: mock-provider API, payload webhook masuk, payload webhook keluar ke merchant |
| [`04-payrun-api.md`](04-payrun-api.md) | Kontrak endpoint PayRun |
| [`05-payrun-authorization-matrix.md`](05-payrun-authorization-matrix.md) | Matriks endpoint × role (input untuk authorization matrix test, PF-04) |

---

## Konvensi (mengikat untuk kedua project)

### 1. Dasar
| Aspek | Keputusan |
|---|---|
| Base URL | `https://<host>/api/v1` (Caddy meneruskan `/api/*` ke service api) |
| Versi | Di path (`/v1`). Perubahan *breaking* = `/v2`. Penambahan field atau endpoint bukan breaking. Client wajib mengabaikan field yang tidak dikenal |
| Format | `application/json; charset=utf-8`. Error: `application/problem+json` (RFC 9457) |
| Penamaan | Path `kebab-case` dan jamak (`/payment-intents`); field JSON `snake_case` |
| ID | UUIDv7 sebagai string. Prefix tipe tidak dipakai di V1 |
| Waktu | RFC 3339 UTC (`2026-03-15T08:30:00Z`). Tanggal bisnis `YYYY-MM-DD`. Periode `YYYY-MM` |
| Uang | **Selalu string.** LedgerPay: integer minor unit (`"amount": "5000000"` = Rp50.000,00) + `"currency": "IDR"`. PayRun: decimal 2 digit (`"4250000.00"`) |
| Aksi non-CRUD | Sub-resource kata kerja dengan `POST`: `POST /payroll-runs/{id}/calculate`, `POST /payment-intents/{id}/confirm`. Transisi state tidak dilakukan lewat `PATCH status` |
| Envelope | Resource tunggal dikembalikan langsung sebagai objek. List: `{ "data": [...], "page": { "next_cursor": "…", "has_more": true } }` |
| Ukuran body | JSON maksimal 64 KB → `413 PAYLOAD_TOO_LARGE`. Upload CSV (PayRun) maksimal 5 MB |

### 2. Authentication
| Skema | Dipakai oleh | Header |
|---|---|---|
| User JWT | Browser (kedua project) | `Authorization: Bearer <access_token>` (15 menit; ADR-009) |
| Refresh | Browser | Cookie `HttpOnly; Secure; SameSite=Strict; Path=/api/v1/auth` + header wajib `X-Requested-With: fetch` (mitigasi CSRF) |
| Merchant API key (LedgerPay) | Server merchant | `Authorization: Bearer lp_test_<prefix>_<secret>`, **hanya** di bawah `/v1/merchant/*` |
| Webhook signature (LedgerPay, masuk) | mock-provider | `Provider-Signature: t=<unix>,v1=<hex hmac-sha256>`; tanpa auth lain |
| Ops (LedgerPay) | Sari | User JWT dengan `role = OPS`, **hanya** di bawah `/v1/ops/*` |

Pemisahan prefix path per jenis principal (`/merchant`, `/ops`, `/webhooks`) membuat middleware auth per grup rute menjadi eksplisit. Endpoint user tidak pernah menerima API key, dan sebaliknya.

### 3. Header standar
| Header | Arah | Aturan |
|---|---|---|
| `X-Request-ID` | ↔ | Diterima (maks. 64 karakter, `[A-Za-z0-9-]`) atau dibuat server; selalu dikembalikan; muncul di body error sebagai `request_id` |
| `Idempotency-Key` | → | 8–255 karakter. **Wajib** pada endpoint bertanda *Idempotency: required*. Semantik: ADR-L02 |
| `Idempotent-Replayed: true` | ← | Pada respons replay |
| `If-Match: "<version>"` | → | **Wajib** pada `PATCH` resource ber-`version` (PayRun: employee, komponen, settings, departemen/posisi). Tanpa header → `428 PRECONDITION_REQUIRED`; tidak cocok → `412 VERSION_CONFLICT` |
| `ETag: "<version>"` | ← | Pada `GET`/`PATCH` resource ber-`version` |
| `RateLimit-Limit` / `RateLimit-Remaining` / `RateLimit-Reset` | ← | Pada endpoint ber-rate-limit; `429` + `Retry-After` |
| `Cache-Control: no-store` | ← | Semua respons API (data finansial dan PII; ADR-008) |
| `X-Active-Tenant` | → | PayRun, **hanya** pada `POST /auth/refresh`: tenant yang ingin dipertahankan di access token baru. Membership dicek ulang (O8) |

### 4. Error (RFC 9457)
```json
{
  "type": "https://docs.ledgerpay.dev/errors/insufficient-balance",
  "title": "Insufficient balance",
  "status": 422,
  "code": "INSUFFICIENT_BALANCE",
  "detail": "Wallet balance is lower than the requested amount.",
  "instance": "/api/v1/transfers",
  "request_id": "01J…",
  "errors": [ { "field": "amount", "code": "TOO_LARGE", "message": "…" } ]
}
```
- `code` stabil dan menjadi kontrak (client memprosesnya). `title`/`detail` untuk manusia dan bisa berubah.
- `errors[]` hanya untuk `400 VALIDATION_ERROR` (kesalahan per field).
- Extension tambahan didokumentasikan per code (contoh: `existing_resource_id`, `employees_changed`).
- Pesan error tidak pernah membocorkan keberadaan resource milik pihak lain, isi PII, atau detail internal (SQL, stack trace).

### 5. 404 vs 403 (ADR-010)
- Resource yang **tidak boleh diketahui** keberadaannya oleh pemanggil (milik user/merchant/tenant lain, atau di luar scope relasi) → `404 NOT_FOUND`.
- Resource yang **sah terlihat**, tetapi aksinya tidak diizinkan untuk role tersebut (contoh: employee melihat run tetapi mencoba finalize) → `403 FORBIDDEN`.

### 6. Pagination, filter, sort
- **Cursor:** `?limit=20&cursor=<opaque>`. Default 20, maksimal 100. Cursor = base64url dari `(sort_key, id)`, dan tidak boleh dirakit sendiri oleh client.
- **Sort tetap per endpoint** (biasanya `created_at DESC, id DESC`). Tidak ada `?sort=` bebas: setiap urutan butuh index yang sesuai (Step 4).
- **Filter eksplisit** per endpoint (contoh: `?status=PENDING&from=2026-03-01&to=2026-03-31`). Filter yang tidak dikenal → `400 VALIDATION_ERROR`.
- Rentang tanggal maksimal 366 hari pada endpoint riwayat.

### 7. Rate limit (V1: in-memory token bucket per instance, ADR-007)
| Tier | Kunci | Batas awal | Endpoint |
|---|---|---|---|
| `auth-strict` | IP + email | 5/menit, 20/jam | login, register, refresh |
| `auth-ip` | IP | 30/menit | semua `/auth/*` |
| `money-write` | user / merchant | 30/menit | transfer, top-up, payout, confirm, refund |
| `write` | user | 60/menit | tulis lainnya |
| `read` | user / merchant | 300/menit | `GET` |
| `merchant-api` | API key | 300/menit | `/merchant/*` |
| `heavy` (PayRun) | tenant | calculate 10/jam; import 10/jam; finalize 5/jam | `/payroll-runs/*/calculate`, `/attendance-imports`, finalize |
| `webhook-in` | — | Tidak dibatasi per provider (hanya ukuran body + signature) | `/webhooks/provider` |

Angka ini titik awal, ditinjau ulang setelah load test (Step 9). Rate limit adalah **perlindungan**, bukan mekanisme correctness. Idempotency dan lock tetap menjadi jaminan utama.

### 8. Format penulisan endpoint (dipakai di file 02 dan 04)
```text
### <METHOD> <path>
Use case / fitur      : LF-xx / PF-xx
Authentication        :
Authorization         :
Request               : (path, query, body)
Response              : (status + body)
Validation            :
Errors                : (code dari katalog)
Idempotency           : required / natural / n.a.
Pagination            : cursor / n.a.
Rate limit            : tier
Catatan               : (transaksi, async, failure scenario terkait)
```

---

## Penyelesaian open items

| # | Keputusan |
|---|---|
| O1 | Katalog error lengkap di [`01-error-catalogue.md`](01-error-catalogue.md) |
| O8 | `X-Active-Tenant` hanya di `POST /auth/refresh` PayRun (§3) |
| O9 | `IMPORT_DUPLICATE` masuk katalog. Pemetaan error trigger DB: `42501` (append-only/forbidden change) → `409 IMMUTABLE_RECORD`; `P0001` dari trigger run final → `409 RUN_FINALIZED`; trigger PeriodGuard → `409 PERIOD_LOCKED` |

## Perubahan terhadap step sebelumnya

| Item | Perubahan | Alasan |
|---|---|---|
| Run ganda dari admin berbeda (PFS-06) | `200 + X-Existing-Resource` diganti **`409 ACTIVE_RUN_EXISTS`** + extension `existing_resource_id`. Klik ganda oleh admin yang sama tetap di-replay lewat `Idempotency-Key` (`201` + `Idempotent-Replayed`) | Admin B tidak boleh mengira dirinya membuat run baru. Konflik yang eksplisit lebih jujur daripada "create-or-get" diam-diam |
| `VERSION_CONFLICT` | `409` → **`412`** dengan `If-Match` | Semantik HTTP yang benar untuk precondition |
| `INVALID_STATE` (422) | Dilebur ke **`409 INVALID_STATE_TRANSITION`** | Satu code untuk satu arti |
| `RUN_NOT_FOUND` | Diganti `404 NOT_FOUND` generik | Code spesifik membocorkan jenis resource yang dicari |

---

## Keputusan selama Step 5

| Item | Keputusan | Di mana |
|---|---|---|
| Manajemen API key merchant | **Hanya lewat ops** (`/ops/merchants/{id}/api-keys`, `/ops/api-keys/{id}/revoke`). Key yang bocor tidak bisa dipakai untuk membuat key pengganti bagi dirinya sendiri | `02-ledgerpay-api.md` |
| Visibilitas checkout | Sebelum dibayar: siapa pun yang punya link intent (*capability URL*). Setelah `SUCCEEDED`: hanya payer | `02` |
| Riwayat wallet | Berbasis ledger: top-up `PENDING` tidak muncul, payout yang di-hold muncul sebagai "dana ditahan" | `02` |
| Status payout untuk user | Field `display_status`. `UNKNOWN` ditampilkan sebagai `PROCESSING`, karena detail internal tidak relevan bagi user | `02` |
| Webhook payout dari provider | Tidak ada di V1. Status query adalah jalur utama (ADR-L06) | `03` |
| Header signature webhook masuk | `Provider-Signature` (ADR-L05/L08 diselaraskan) | `03` |
| Nama skenario mock | Kanonik dari ADR-L05; LF-06 diselaraskan | `03` §1.7 |
| Tenant context PayRun | **Hanya** dari claim `tid`. Path tidak memakai `/tenants/{id}`; body yang mengirim `tenant_id` → `400` | `04-payrun-api.md` |
| Approval payroll run | Lewat `/approvals/{id}/approve\|reject` (satu mesin approval); alasan self-approval diisi saat submit | `04`, amandemen ADR-P09 |
| Periode payroll | Dibuat otomatis oleh `POST /payroll-runs`; tidak ada endpoint create period | `04` |
| Separation of duties | `HR_ADMIN` + `PAYROLL_ADMIN` pada satu orang diizinkan, tetapi diaudit (`membership.sod_conflict`) | `05`, amandemen ADR-010 |
| Code baru | `409 DUPLICATE_CODE`, `409 PAYSLIP_NOT_READY` | `01-error-catalogue.md` |
| Nama constraint di katalog | Dicocokkan satu per satu dengan skema Step 4 (7 nama diperbaiki) | `01` |

## Open items untuk step berikutnya

Masih berlaku dari step sebelumnya: **O3** (sebagian selesai: approve sudah mengunci `payroll_periods FOR UPDATE`; penulis input mengambil `FOR SHARE` lewat `PeriodGuard`, diformalkan di Step 7), **O4** (metrik, Step 8), **O5** (chunk size demo, Step 11), **O10** (refresh token/undangan → threat model, Step 6), **O11** (hot row wallet merchant, Step 7/9).

| # | Item | Step |
|---|---|---|
| O12 | Approve run menghitung ulang fingerprint seluruh karyawan secara sinkron. Untuk tenant 10.000 karyawan ini bisa memakan beberapa detik. Opsi: tandai item *dirty* saat input berubah (via `PeriodGuard`), lalu hitung ulang hanya yang dirty. Ukur dulu | Step 7 / Step 9 |
| O13 | Threat model untuk capability URL checkout (entropi ID, TTL intent, tidak membocorkan data merchant sensitif) | Step 6 |
