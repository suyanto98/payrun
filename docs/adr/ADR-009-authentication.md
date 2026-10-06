# ADR-009 — Authentication: JWT Pendek + Refresh Token Rotasi, API Key untuk Merchant

Status: Accepted · Tanggal: 2026-10-01 · Berlaku untuk: LedgerPay, PayRun

## Context / Problem

Ada dua jenis client:
1. **Manusia lewat SPA Vue** (Rina, Sari, Dewi, Tono): butuh login dengan password, sesi yang bisa dicabut, dan perlindungan dari XSS/CSRF.
2. **Server merchant** (LedgerPay, Budi): machine-to-machine, tanpa browser.

Ancamannya meliputi pencurian token (XSS), CSRF, brute force password, token replay, dan kebocoran API key.

## Options

| Opsi | Pros | Cons |
|---|---|---|
| Server-side session (cookie session ID + tabel/Redis session) | Pencabutan instan; tidak ada token di JS | Lookup setiap request; CSRF harus ditangani; kurang cocok untuk API merchant |
| **JWT access pendek + refresh token opaque berotasi** | Verifikasi stateless untuk access; refresh dapat dicabut di DB; standar SPA modern | Access token tidak bisa dicabut instan (window = TTL); lebih banyak bagian |
| JWT panjang tanpa refresh | Sederhana | Tidak bisa dicabut; dicuri berarti akses lama |
| OAuth/OIDC provider eksternal (Auth0, Keycloak) | Fitur lengkap | Menyembunyikan mekanisme yang ingin ditunjukkan; vendor/infra tambahan |

## Decision

**User (SPA):**

| Aspek | Keputusan |
|---|---|
| Password | **argon2id**, parameter awal: memory 64 MiB, iterations 3, parallelism 2, salt 16 byte (dituning agar ±100–250 ms di server target) |
| Access token | JWT **15 menit**, **HS256** dengan header `kid` untuk rotasi kunci; claim `sub`, `exp`, `iat`, `jti`, `roles` (+ `tid` di PayRun, ADR-P02) |
| Penyimpanan access token | **Memori SPA** (variabel Pinia), **bukan** localStorage |
| Refresh token | Opaque random 256-bit, disimpan **ter-hash (SHA-256)** di DB dengan `family_id`, `expires_at` (7 hari, sliding), `family_expires_at` (batas absolut 30 hari sejak login), `replaced_by` (grace 10 detik untuk refresh paralel dari dua tab), `revoked_at` (Step 4 `01-shared-tables.md`) |
| Pengiriman refresh token | Cookie `HttpOnly; Secure; SameSite=Strict; Path=/api/v1/auth` (dibutuhkan oleh refresh **dan** logout) |
| CSRF di endpoint refresh | `SameSite=Strict` + wajib header kustom (`X-Requested-With: fetch`, nilai yang sama di kedua project) + validasi `Origin`. Endpoint lain memakai `Authorization: Bearer`, sehingga tidak rentan CSRF |
| Rotasi + reuse detection | Setiap refresh: token lama ditandai `used`, token baru diterbitkan di family yang sama. Kalau token yang **sudah dipakai** dipresentasikan lagi, berarti ada pencurian → **revoke seluruh family** + audit log + paksa login ulang |
| Logout | Revoke family refresh token; access token kedaluwarsa sendiri ≤ 15 menit |
| Brute force | Rate limit per IP + per akun, lockout bertahap (ADR-007 untuk versi terdistribusi) |

**Merchant (LedgerPay):**

| Aspek | Keputusan |
|---|---|
| Format | `lp_live_<prefix8>_<secret32>` (dan `lp_test_...`); prefix untuk lookup & identifikasi di log, secret ≥ 192 bit entropi |
| Penyimpanan | **SHA-256(secret)**, bukan argon2. Secret acak berentropi tinggi tidak bisa di-brute-force, sehingga hash lambat tidak memberi keamanan tambahan dan hanya menambah latensi di setiap request. Argon2 diperlukan untuk password karena entropinya rendah |
| Siklus hidup | Ditampilkan **sekali** saat dibuat; bisa dicabut; mendukung 2 key aktif untuk rotasi tanpa downtime; `last_used_at` dicatat |

### Catatan implementasi (Sprint 4)
- **Grace window refresh (10 dtk):** refresh kedua dengan token yang baru saja dirotasi mendapat access token baru **tanpa** refresh token baru. Server tidak bisa mengirim ulang penerusnya karena yang disimpan hanya hash; browser sudah menerima penerus dari respons pertama di cookie jar yang sama. Refresh paralel diserialkan dengan `SELECT … FOR UPDATE` pada baris token.
- **Lockout:** 5 password salah → `locked_until` 1 menit, berlipat sampai 15 menit; percobaan selama terkunci tidak memperpanjang lock. Respons akun terkunci = `401 INVALID_CREDENTIALS` **tanpa `Retry-After`** (lihat backlog Sprint 4: header itu sendiri akan membocorkan email terdaftar). Argon2 tetap dijalankan untuk akun terkunci (timing).
- **Semaphore argon2:** maksimal 4 hash paralel; antre > 2 dtk → `503 SERVICE_UNAVAILABLE` (C-45).
- **API key:** `lp_test_<prefix 8 hex>_<secret 32 base64url>`; lookup lewat prefix, bandingkan SHA-256(secret) secara constant-time; `last_used_at` di-update maksimal 1×/menit.

## Reason

- Access token di memori tidak bisa dicuri lewat `localStorage` oleh script XSS. Refresh token `HttpOnly` tidak bisa dibaca JS sama sekali.
- Reuse detection mengubah pencurian refresh token dari "akses diam-diam selamanya" menjadi kejadian yang **terdeteksi** dan menghentikan sesi.
- **HS256 vs EdDSA:** hanya satu service (monolith) yang menerbitkan dan memverifikasi token, jadi secret simetris cukup dan sederhana. EdDSA (Ed25519) unggul ketika pihak lain perlu memverifikasi tanpa bisa menerbitkan (microservices, client pihak ketiga). `kid` menyiapkan rotasi dan migrasi ke asimetris.

## Trade-offs

- Pencabutan akses tidak instan (maksimal 15 menit) untuk access token yang sudah terbit. Operasi sangat sensitif (payout, ubah rekening, finalize payroll) dapat memeriksa ulang status user di DB.
- Refresh setelah reload halaman memerlukan satu request tambahan (access token di memori hilang).
- Membuat auth sendiri berisiko bug keamanan. Mitigasi: library standar (`golang-jwt`, `argon2id`), test negatif (token kedaluwarsa, `alg=none`, `kid` salah, reuse).

## Future Migration

| Trigger | Perubahan |
|---|---|
| Ada service lain yang perlu memverifikasi token | EdDSA + endpoint JWKS |
| Klien enterprise butuh SSO | OIDC/SAML (PayRun V3) lewat provider; token internal tetap sama |
| Butuh pencabutan instan | Denylist `jti` jangka pendek (Redis) atau session server-side |
| Payout bernilai besar | Step-up auth (PIN/OTP) — nice to have LedgerPay |

## Interview angle

- *"Di mana kamu simpan JWT di browser, dan kenapa?"* → Access di memori, refresh di cookie HttpOnly. Jelaskan trade-off XSS vs CSRF dan mitigasi masing-masing.
- *"Bagaimana mendeteksi refresh token dicuri?"* → Rotasi + reuse detection → revoke family.
- *"Kenapa API key di-hash SHA-256 tapi password argon2id?"* → Perbedaan entropi.
- *"Bagaimana mencabut akses user yang dipecat?"* → Revoke refresh family + TTL access 15 menit + pemeriksaan ulang pada operasi sensitif.

## Related

- [ADR-010](ADR-010-authorization.md) · [ADR-007](ADR-007-redis-not-in-v1.md) · [ADR-P02](ADR-P02-tenant-context.md) · [ADR-003](ADR-003-rest-api-openapi.md)
