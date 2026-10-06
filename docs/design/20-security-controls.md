# Step 6.4 — Security Controls, Testing & Operations

> Bagian dari [Step 6](00-index.md). Katalog kontrol ini direferensikan oleh threat di `01-shared-threats.md` (T-SH), `02-ledgerpay-threat-model.md` (T-LP), dan `03-payrun-threat-model.md` (T-PR).
> Setiap kontrol harus menunjuk **tempat implementasinya** dan **test yang membuktikannya**. Kontrol tanpa test dianggap belum ada.

---

## 1. Control catalogue

Tipe: **P** = preventive · **D** = detective · **C** = corrective.

### 1.1 Identitas & sesi
| ID | Kontrol | Tipe | Implementasi | Threat | Test |
|---|---|---|---|---|---|
| C-01 | Middleware auth per grup rute (user JWT / merchant API key / webhook signature / OPS) | P | `platform/httpx` router groups, `platform/auth` | T-SH-03, T-SH-14, T-LP (API key) | `route_group_auth_scheme_enforced` |
| C-02 | Password hashing argon2id + kebijakan password (min 10, daftar umum offline) | P | `platform/auth/password` | T-SH-02 | `password_hash_params`, `password_policy_rejects_common` |
| C-03 | Lockout bertahap + error generik + timing equalization; akun terkunci menjawab `401 INVALID_CREDENTIALS` + `Retry-After` (bukan `ACCOUNT_LOCKED`, Step 12 R2-5); limiter IP+email juga berlaku untuk email yang tidak ada | P | `identity` service, `users.locked_until` | T-SH-01 | `authn_bruteforce_lockout`, `authn_unknown_email_timing` |
| C-04 | Verifikasi JWT ketat (alg pinned HS256, kid, exp/nbf/iss/aud, skew 30s) | P | `platform/auth/jwt` | T-SH-03, T-SH-04 | `jwt_rejects_*` |
| C-05 | Refresh token opaque ter-hash, rotasi, reuse detection, grace 10s, batas family 30 hari | P/D | `identity`, `refresh_tokens` | T-SH-05, T-SH-06 | `refresh_*` |
| C-06 | Policy authorization di service layer + query scoping ke principal | P | `modules/*/service.go`, ADR-010 | T-SH-13, T-SH-14, T-LP, T-PR | `authz_matrix`, `bola_*` |
| C-07 | Rate limiter token bucket per tier (in-memory V1), trusted proxy | P | `platform/httpx/ratelimit` | T-SH-01, T-SH-15, T-SH-24 | `ratelimit_*` |
| C-08 | Proteksi CSRF endpoint cookie (SameSite=Strict + `X-Requested-With` + cek Origin) | P | `/auth/refresh`, `/auth/logout` handler | T-SH-07 | `csrf_refresh_*` |
| C-09 | Same-origin deployment, tanpa header CORS | P | Caddyfile, API tidak mengirim CORS | T-SH-08 | `cors_preflight_foreign_origin_denied` |

### 1.2 Aplikasi & input
| ID | Kontrol | Tipe | Implementasi | Threat | Test |
|---|---|---|---|---|---|
| C-10 | Security headers (CSP, HSTS, nosniff, dll.; §2) | P | Caddy + middleware API | T-SH-04, T-SH-09, T-SH-17 | `headers_*_present` |
| C-11 | Larangan `v-html` + rendering teks | P | ESLint `vue/no-v-html: error` | T-SH-09 | `lint_no_v_html`, `e2e_xss_payload_rendered_as_text` |
| C-12 | Query parameterized (sqlc), tanpa SQL dinamis berbasis string | P | `db/queries`, gosec G201/G202 | T-SH-10 | gosec CI, `sqli_filter_payloads_rejected` |
| C-13 | DTO eksplisit + decoder strict (`DisallowUnknownFields`) | P | `platform/httpx/decode`, oapi-codegen | T-SH-11 | `mass_assignment_unknown_field_rejected` |
| C-14 | Batas body (64 KB / CSV 5 MB), kedalaman JSON, media type | P | `platform/httpx`, Caddy | T-SH-12 | `body_limit_413`, `json_depth_limit_400`, `content_type_415` |
| C-15 | Least privilege DB: `<proj>_app` bukan owner, append-only = SELECT/INSERT | P | Migrasi role/grant (Step 4 §6) | T-SH-10, T-SH-20, T-PR (RLS) | `db_grants_catalogue_test` |
| C-16 | 404 untuk resource di luar scope; tidak ada code spesifik yang membocorkan jenis resource | P | Error mapping, service | T-SH-13, T-SH-21 | `bola_*`, `error_not_found_uniform` |
| C-17 | Perubahan role hanya lewat endpoint khusus + policy "hanya OWNER memberi OWNER" + audit | P/D | `iam`/`identity` | T-SH-14, T-PR (eskalasi) | `cannot_self_promote_owner`, `role_change_audited` |
| C-25 | Pemetaan error ke katalog RFC 9457; unmapped → 500 generik | P | `platform/httpx/problem`, `platform/db/errors` | T-SH-21 | `error_unmapped_returns_generic_500`, `error_no_pg_message_leak` |
| C-31 | Waktu bisnis dari server (`Clock`/`now()`), bukan dari klien | P | `platform/clock` | T-SH-26 | `clock_in_ignores_client_time` |

### 1.3 Data, secret, logging
| ID | Kontrol | Tipe | Implementasi | Threat | Test |
|---|---|---|---|---|---|
| C-18 | Secret di env 0600, redaction config, kunci per environment ber-versi (kid/v1) | P | `platform/config`, VM | T-SH-16 | `config_dump_redacts_secrets` |
| C-19 | gitleaks (CI + pre-commit) | D | CI | T-SH-16 | Job `gitleaks` |
| C-20 | TLS (Caddy, Let's Encrypt), HSTS, network internal compose, DB tidak dipublish | P | Caddyfile, `docker-compose.yml` | T-SH-17 | `headers_hsts_present`, `compose_no_db_port_published` |
| C-21 | Enkripsi disk penyedia + backup terenkripsi (age, public key di VM; dua recipient: key operator offline + key drill di GitHub env `restore-drill`, pengecualian yang diterima karena data demo sintetis, Step 9 `03`) | P | Skrip backup (Step 9) | T-SH-18 | `backup_artifact_is_encrypted` (restore drill) |
| C-22 | Enkripsi PII level aplikasi (AES-256-GCM, AAD, keyring) + blind index | P | PayRun `employee`, ADR-P08 | T-SH-18, T-PR (PII) | `pii_roundtrip`, `pii_aad_swap_fails`, `pii_masked_by_default` |
| C-23 | Logging berbasis allowlist + redaction header/tipe sensitif | P | `platform/observability` | T-SH-19 | `log_redaction_*` |
| C-24 | Audit log append-only (trigger + grant), ditulis dalam transaksi bisnis | P/D | `audit_log` | T-SH-20, T-LP (insider), T-PR | `audit_log_update_rejected`, `audit_written_in_same_tx` |

### 1.4 Supply chain & infrastruktur
| ID | Kontrol | Tipe | Implementasi | Threat | Test |
|---|---|---|---|---|---|
| C-26 | Scanning dependency (govulncheck, osv-scanner/npm audit) + trivy image | D | CI (§3) | T-SH-22 | Job CI |
| C-27 | Pin base image by digest + Actions by commit SHA + Dependabot | P | Dockerfile, workflow | T-SH-22 | Lint workflow (`pinned_actions_check`) |
| C-28 | Hardening container (distroless, non-root, read-only, no-new-privileges, cap_drop ALL, limit) | P | Dockerfile, compose | T-SH-23 | `compose_hardening_check` |
| C-29 | Timeout server/DB, pool tetap, batas list/rentang, tier `heavy` | P | `cmd/*`, ADR-011, Step 5 §6–7 | T-SH-24 | `server_timeouts_configured`, load test |
| C-30 | Hardening host (SSH key-only, ufw 22/80/443, fail2ban, unattended-upgrades, Grafana tidak dipublish) | P | Provisioning (Step 9) | T-SH-25 | Skrip cek host |

### 1.5 Kontrol domain (dirinci di file 02/03)
| ID | Kontrol | Tipe | Implementasi | Threat | Test |
|---|---|---|---|---|---|
| C-32 | Verifikasi HMAC webhook masuk (constant-time) + window timestamp 5 menit | P | LedgerPay `webhook` (ADR-L08) | T-LP (forgery, replay) | `webhook_bad_signature_401`, `webhook_stale_400` |
| C-33 | Inbox dedupe `UNIQUE (provider, provider_event_id)` + transisi monoton | P | `webhook_inbox` | T-LP (replay, duplikat) | `webhook_duplicate_no_double_credit` |
| C-34 | Idempotency transaksional (ADR-L02) + `request_hash` | P | `idempotency_keys` | T-LP (double spend), T-PR (run ganda) | `idempotency_*` |
| C-35 | Row lock berurutan + `CHECK (balance >= 0)` + deferred trigger keseimbangan journal | P | LedgerPay `ledger` | T-LP (race exploitation) | `concurrency_transfer_100`, `journal_unbalanced_rejected` |
| C-36 | RLS + `FORCE ROW LEVEL SECURITY` + role tanpa `BYPASSRLS` + catalogue test (allowlist SECURITY DEFINER) | P/D | PayRun migrasi, CI | T-PR (cross-tenant, misconfig) | `rls_catalogue_test`, `rls_bypass_query_test`, `pool_leakage_test` |
| C-37 | Composite FK `(tenant_id, id)` | P | PayRun skema | T-PR (referensi lintas tenant) | `cross_tenant_fk_rejected` |
| C-38 | Validasi upload (ukuran, MIME + magic bytes, parser CSV ketat, batas baris, encoding UTF-8) | P | PayRun `attendance` import | T-PR (upload) | `upload_validation_*` |
| C-39 | Escape formula pada export CSV (`=`, `+`, `-`, `@`, tab, CR → diawali `'`) | P | PayRun export | T-PR (CSV injection) | `csv_export_escapes_formula` |
| C-40 | Bucket private + presigned URL TTL 5 menit, dibuat setelah otorisasi + audit | P | PayRun `payslip` (ADR-012) | T-PR (payslip URL) | `payslip_download_requires_authz`, `presigned_ttl` |
| C-41 | Guard approval (`WHERE status='PENDING'`, larangan self-approval kecuali pengecualian, SoD) | P | PayRun `approval` | T-PR (SoD, bypass approval) | `approval_*` |
| C-42 | API key: SHA-256 at rest, prefix untuk lookup, 2 slot aktif, manajemen hanya via ops | P | LedgerPay `identity` | T-LP (API key) | `api_key_*` |
| C-43 | Penegakan `tenant_id` pada INSERT job (`jobs_enforce_tenant`) + handler hanya memakai `jobs.tenant_id` | P | PayRun `jobs` | T-PR (job tampering) | `job_tenant_mismatch_rejected` |
| C-44 | Egress guard untuk webhook keluar: resolve DNS sekali, tolak IP private/loopback/link-local/CGNAT/ULA/metadata, connect ke IP yang sudah divalidasi, HTTPS-only di mode live, tanpa redirect, body respons maksimal 64 KB lalu dibuang | P | LedgerPay `webhook` delivery client | T-LP (SSRF via `webhook_url`) | `webhook_egress_blocks_private_ip`, `webhook_egress_no_redirect` |
| C-45 | Semaphore hashing password: maksimal ±4 hash argon2id bersamaan (64 MiB per hash); antre maksimal 2 dtk lalu `503 SERVICE_UNAVAILABLE`. Mencegah login dipakai sebagai vektor DoS memori | P | `platform/auth/password` | T-SH-24, T-SH-01 | `password_hash_concurrency_bounded` |
| C-46 | Pemeriksaan defensif `row.tenant_id == ctx.Tenant` saat repository men-decode baris tenant-scoped; jika berbeda → request gagal 500 + metrik INVARIANT `payrun_tenant_isolation_violations_total` + log error. Lapis deteksi di atas RLS | D | PayRun `platform/db` decode helper | T-PR (cross-tenant) | `tenant_guard_detects_foreign_row` |

---

## 2. HTTP security headers

| Header | Nilai | Di-set oleh | Catatan |
|---|---|---|---|
| `Strict-Transport-Security` | `max-age=31536000; includeSubDomains` | Caddy | `preload` hanya setelah domain permanen |
| `Content-Security-Policy` (SPA) | `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; report-to csp` | Caddy (untuk `/`) | Build Vite tanpa inline script. Kalau library UI butuh inline style, pakai hash, bukan `'unsafe-inline'` untuk script |
| `Content-Security-Policy` (API) | `default-src 'none'; frame-ancestors 'none'` | Middleware API | Respons JSON tidak pernah perlu memuat resource |
| `X-Content-Type-Options` | `nosniff` | Caddy + API | |
| `Referrer-Policy` | `no-referrer` (API), `strict-origin-when-cross-origin` (SPA) | Caddy / API | Mencegah ID resource bocor lewat Referer |
| `Permissions-Policy` | `camera=(), microphone=(), geolocation=(), payment=()` | Caddy | |
| `X-Frame-Options` | `DENY` | Caddy | Cadangan untuk browser lama (`frame-ancestors` adalah kontrol utama) |
| `Cache-Control` | `no-store` | Middleware API | Semua respons `/api/*` (ADR-008) |
| `Server` / `X-Powered-By` | dihapus | Caddy (`-Server`) | |
| `Cross-Origin-Opener-Policy` | `same-origin` | Caddy | |

Test: `headers_*_present` (integration test terhadap Caddy + api di compose) memeriksa setiap header di tabel ini.

---

## 3. CI security pipeline

| Job | Alat | Kapan | Kebijakan gagal |
|---|---|---|---|
| Static analysis Go | `golangci-lint` dengan `gosec`, `errcheck`, `staticcheck` | Setiap PR | Fail pada temuan apa pun (dengan `//nolint:gosec // alasan` yang ditinjau) |
| Vulnerability Go | `govulncheck ./...` | Setiap PR + terjadwal mingguan | Fail jika ada vuln yang **terjangkau** dari kode |
| Vulnerability JS | `osv-scanner` (atau `npm audit --audit-level=high`) | Setiap PR + mingguan | Fail pada HIGH/CRITICAL |
| Secret scan | `gitleaks` | Setiap PR + pre-commit | Fail pada temuan |
| Image scan | `trivy image` | Setelah build image | Fail pada CRITICAL/HIGH yang **punya fix** |
| Lint frontend | ESLint (`vue/no-v-html`) | Setiap PR | Fail |
| Konfigurasi | `compose_hardening_check`, `pinned_actions_check` | Setiap PR | Fail |
| Katalog DB (PayRun) | Test migrasi: setiap tabel ber-`tenant_id` punya RLS + FORCE; `payrun_app` bukan owner & tanpa BYPASSRLS; tidak ada `SECURITY DEFINER` selain allowlist (`payrun.auth_lookup_user`) | Setiap PR | Fail |
| Katalog DB (keduanya) | Grant: tabel append-only tidak punya UPDATE/DELETE untuk `<proj>_app` | Setiap PR | Fail |
| Security test suite | Test di §4 (bagian dari integration/e2e) | Setiap PR | Fail |
| DAST baseline (P2) | OWASP ZAP baseline terhadap environment demo | Terjadwal mingguan | Laporan; fail hanya untuk HIGH |

---

## 4. Security test catalogue (input untuk Step 8)

| Kategori | Test | Project | Tipe |
|---|---|---|---|
| Authentication | `authn_generic_error`, `authn_unknown_email_timing`, `authn_bruteforce_lockout`, `password_policy_rejects_common` | Keduanya | Integration |
| JWT | `jwt_rejects_alg_none`, `jwt_rejects_alg_swap`, `jwt_rejects_tampered_claims`, `jwt_rejects_unknown_kid`, `jwt_rejects_expired_with_skew` | Keduanya | Unit |
| Refresh | `refresh_rotation_issues_new_token`, `refresh_reuse_revokes_family`, `refresh_parallel_within_grace_ok`, `refresh_reuse_after_grace_revokes`, `refresh_family_absolute_expiry`, `logout_revokes_family` | Keduanya | Integration |
| CSRF / CORS | `csrf_refresh_without_header_rejected`, `csrf_refresh_bad_origin_rejected`, `cors_preflight_foreign_origin_denied` | Keduanya | Integration |
| BOLA | `bola_<resource>` untuk wallet, transfer, top-up, payout, payment intent (merchant ↔ merchant), refund | LedgerPay | Integration |
| Authz matrix | `authz_matrix` (Step 5 file 05; endpoint tanpa baris matriks = fail) | PayRun | Integration (table-driven) |
| SSRF / egress | `webhook_egress_blocks_private_ip`, `webhook_egress_blocks_metadata_ip`, `webhook_egress_no_redirect`, `webhook_egress_dns_rebinding_pinned_ip` | LedgerPay | Integration |
| Ops | `ops_route_rejects_user_role`, `ops_resolution_requires_evidence`, `ops_action_audited` | LedgerPay | Integration |
| Cross-tenant | `cross_tenant_<endpoint>` (semua role dari Beta → 404), `cross_tenant_fk_rejected` | PayRun | Integration |
| RLS | `rls_catalogue_test`, `rls_bypass_query_test` (query tanpa filter tenant tetap hanya mengembalikan tenant aktif), `pool_leakage_test`, `rls_no_context_returns_zero_rows` | PayRun | Integration |
| Webhook | `webhook_bad_signature_401`, `webhook_stale_400`, `webhook_duplicate_no_double_credit`, `webhook_amount_mismatch_dead` | LedgerPay | Integration |
| Idempotency abuse | `idempotency_payload_mismatch_422`, `idempotency_cross_client_isolated` (key sama dari user lain tidak replay respons milik orang lain) | LedgerPay (+ PayRun run) | Integration |
| Mass assignment / input | `mass_assignment_unknown_field_rejected`, `body_limit_413`, `json_depth_limit_400`, `sqli_filter_payloads_rejected` | Keduanya | Integration |
| Upload | `upload_validation_size`, `upload_validation_mime_magic`, `upload_validation_row_limit`, `upload_validation_encoding` | PayRun | Integration |
| CSV injection | `csv_export_escapes_formula` | PayRun | Unit |
| PII | `pii_roundtrip`, `pii_aad_swap_fails`, `pii_masked_by_default`, `pii_full_view_audited` | PayRun | Unit / integration |
| Logging | `log_redaction_auth_body`, `log_redaction_headers`, `log_redaction_pii_types` | Keduanya | Unit |
| Audit | `audit_log_update_rejected`, `audit_log_delete_rejected`, `audit_written_in_same_tx` | Keduanya | Integration |
| Headers | `headers_csp_present`, `headers_hsts_present`, `headers_nosniff_present`, `headers_no_store_api` | Keduanya | Integration (compose) |
| XSS | `lint_no_v_html`, `e2e_xss_payload_rendered_as_text` | Keduanya | Lint / E2E |
| Token storage | `e2e_token_not_in_web_storage` | Keduanya | E2E |
| Error | `error_unmapped_returns_generic_500`, `error_no_pg_message_leak`, `error_not_found_uniform` | Keduanya | Integration |

---

## 5. Portfolio MVP vs Production Enhancement

| Area | Portfolio MVP (dibangun) | Production enhancement (didiskusikan) |
|---|---|---|
| Autentikasi | Password argon2id, JWT + refresh rotasi | **MFA/TOTP** wajib untuk OPS (LedgerPay) dan OWNER/PAYROLL_ADMIN (PayRun); WebAuthn; step-up auth untuk payout/finalize |
| Brute force | Limiter in-memory + lockout | Limiter terdistribusi (Redis), CAPTCHA adaptif, reputasi IP |
| Sesi | Revoke family, access 15 menit | Denylist `jti`, device management ("keluar dari semua perangkat"), device binding |
| Secret | `.env` 0600, rotasi via kid | **Secret manager + KMS**, rotasi otomatis, IAM per service |
| PII | AES-GCM keyring dari env | KMS envelope per tenant, crypto-shredding |
| Audit | Append-only di DB | Kirim ke **WORM/object lock** + hash chain, SIEM |
| Edge | Caddy TLS | **WAF**, CDN, **DDoS protection** |
| Deteksi | Metrik + alert dasar | SIEM, **anomaly detection** (velocity payout, login dari negara baru), UEBA untuk ops |
| Verifikasi | Security test suite + scanner CI | **Penetration test** eksternal, **bug bounty**, threat modeling ulang per fitur |
| Host | SSH key-only, ufw, fail2ban | Bastion/SSM, immutable infra, CIS benchmark, EDR |
| Supply chain | Scanner + pin digest | SBOM + signing image (cosign), SLSA provenance, admission policy |

---

## 6. Key & secret inventory + rotation runbook

| Secret | Pemilik | Lokasi V1 | Rotasi rutin | Rotasi darurat |
|---|---|---|---|---|
| Kunci JWT (HS256, per kid) | api | `.env` | 90 hari | Segera (lihat 6.1) |
| Webhook secret provider (masuk) | api | `.env` + mock-provider | 180 hari | Segera |
| Webhook secret per merchant (keluar) | LedgerPay DB (terenkripsi) | `merchants.webhook_secret_enc` | Atas permintaan merchant | Segera |
| API key merchant | Merchant | Hash di DB | Atas permintaan (2 slot) | Revoke segera (playbook 7.1) |
| Keyring PII (`v1`, `v2`, …) | PayRun | `.env` | Tahunan | Segera + re-encrypt |
| Kunci blind index PII | PayRun | `.env` | Jarang (butuh rebuild) | Rebuild index penuh |
| Password DB (`_owner`, `_app`) | Postgres | `.env` | 180 hari | Segera |
| Deploy key SSH / token GHCR | CI | GitHub secrets | 180 hari | Segera |
| Kunci publik backup (age) | Operator | VM (public), offline (private) | Tahunan | Ganti + re-encrypt backup yang dipertahankan |

### 6.1 Kunci JWT (overlap kid)
1. Buat kunci baru `k2`; tambahkan ke daftar kunci **verifikasi** (`k1`, `k2`); deploy.
2. Ubah kunci **penandatanganan** aktif ke `k2`; deploy. Token baru ditandatangani `k2`, token lama `k1` tetap valid.
3. Tunggu ≥ 15 menit (TTL access token).
4. Hapus `k1` dari daftar verifikasi; deploy.
5. **Darurat (kunci bocor):** lompat langsung ke langkah 4 (semua sesi aktif harus refresh) **dan** revoke semua refresh family jika ada indikasi token dipalsukan.

### 6.2 Webhook secret (dual-secret window)
1. Tambahkan secret baru sebagai secret **kedua yang diterima** di verifier (terima `s1` atau `s2`).
2. Ganti secret penandatanganan di sisi pengirim ke `s2`.
3. Pantau `webhook_signature_valid_total{secret="s1"}` sampai 0 selama 24 jam.
4. Hapus `s1`.
   Untuk webhook keluar ke merchant, urutannya dibalik: LedgerPay mengirim **dua** signature (`v1=` untuk s1 dan s2) selama masa transisi, dan merchant memperbarui verifier-nya.

### 6.3 Keyring PII (version bump + lazy re-encrypt)
1. Tambahkan `v2` ke keyring; set `active = v2`; deploy. Data baru ditulis `v2:…`, data lama `v1:…` tetap terbaca.
2. Job `pii.reencrypt` (batch per tenant, idempoten) membaca `v1`, menulis `v2`, dengan AAD yang sama.
3. Pantau `pii_ciphertext_version_total{version="v1"}` sampai 0.
4. Hapus `v1` dari keyring.
   Kalau kunci bocor, job ini menjadi prioritas, dan `v1` dihapus setelah selesai. Kebocoran sebelumnya tetap dianggap insiden (backup lama).

### 6.4 Password DB
1. `ALTER ROLE <proj>_app PASSWORD '<baru>'` (oleh owner).
2. Perbarui `.env`; restart api/worker secara berurutan. Pool membuat koneksi baru dengan password baru; koneksi lama tetap hidup sampai ditutup.
3. Verifikasi `/readyz`.

### 6.5 API key merchant
Ops membuat key di slot kosong → merchant mengganti konfigurasi → ops merevoke key lama → `last_used_at` key lama berhenti bertambah. Kalau kedua slot terisi, salah satu harus direvoke dulu.

---

## 7. Incident response

Siklus: **Detect → Contain → Eradicate → Recover → Postmortem** (blameless, ditulis dalam 5 hari kerja, dengan action item ber-owner).

### 7.1 Playbook: API key merchant bocor (LedgerPay)
| Fase | Tindakan |
|---|---|
| Detect | Laporan merchant; `merchant_api_requests_total` dari IP/ASN baru; lonjakan `POST /merchant/payment-intents` atau refund |
| Contain | Ops merevoke key (`POST /ops/api-keys/{id}/revoke`), efektif seketika karena lookup per request ke DB. Bekukan akun merchant (`FROZEN`) bila ada refund mencurigakan |
| Eradicate | Terbitkan key baru di slot lain; minta merchant merotasi webhook secret juga bila servernya dikompromikan |
| Recover | Tinjau semua aksi key bocor via `audit_log` + `request_id`. Refund tidak sah dikoreksi lewat journal (bukan edit saldo); hubungi user terdampak |
| Postmortem | Dari mana bocornya (repo, log, server)? Apakah perlu IP allowlist per merchant? |

### 7.2 Playbook: dugaan kebocoran lintas tenant (PayRun)
| Fase | Tindakan |
|---|---|
| Detect | `tenant_isolation_violation_total` > 0 (defensive check), laporan pelanggan, test isolasi gagal di main |
| Contain | Matikan endpoint/fitur terkait (feature flag atau rollback ke SHA sebelumnya). Jika penyebabnya belum jelas, mode read-only per tenant |
| Eradicate | Temukan jalur (RLS terlewat? jalur `WithPlatformTx`? cache/file?); perbaiki; tambahkan test regresi di suite isolasi |
| Recover | Tentukan cakupan dari `audit_log` (siapa membaca apa); notifikasi tenant terdampak dan, bila memenuhi kriteria, otoritas sesuai UU PDP (§8) |
| Postmortem | Kenapa lapisan RLS/composite FK tidak menahan? Tambahkan pengecekan katalog atau lint baru |

### 7.3 Playbook: lonjakan refresh token reuse (keduanya)
| Fase | Tindakan |
|---|---|
| Detect | `auth_refresh_reuse_total` melonjak (banyak user dalam waktu singkat) |
| Contain | Reuse detection sudah merevoke family terdampak. Jika lonjakan global (indikasi kebocoran massal, misalnya log proxy), revoke **semua** refresh family dan rotasi kunci JWT (6.1 darurat) |
| Eradicate | Cari sumber kebocoran (XSS? ekstensi browser? log?); cek CSP violation report |
| Recover | User login ulang; komunikasikan |
| Postmortem | Apakah perlu device binding atau MFA? |

---

## 8. Catatan kepatuhan

### 8.1 UU Pelindungan Data Pribadi (UU PDP) — relevan untuk PayRun
> **Verifikasi teks UU sebelum dikutip.** Poin berikut ringkasan tingkat tinggi untuk desain, bukan nasihat hukum, dan angka/pasal harus dicek ke sumber resmi sebelum ditulis di README.

| Kewajiban (ringkas) | Dampak ke desain PayRun |
|---|---|
| Dasar pemrosesan yang sah (contoh: pelaksanaan kontrak kerja, kewajiban hukum) | Tenant (pemberi kerja) adalah pengendali data; PayRun bertindak sebagai **prosesor**. Ini dicatat di ToS/DPA (di luar cakupan portfolio) |
| Minimisasi data | Hanya field yang dibutuhkan payroll; tidak ada data biometrik/foto di V1 |
| Keamanan pemrosesan | RLS, enkripsi PII (C-22), audit akses (C-24), least privilege (C-15) |
| Hak subjek data (akses, koreksi, penghapusan) | Export data per employee; penghapusan = **anonimisasi** kolom PII (Step 4 §5), karena data payroll historis tetap wajib disimpan untuk kewajiban lain |
| Notifikasi kegagalan pelindungan data dalam batas waktu tertentu | Playbook 7.2 mencakup penentuan cakupan dari audit log. **Batas waktu notifikasi harus diverifikasi dari teks UU** |
| Retensi | Kebijakan retensi per tenant (V2); PDF slip dan audit mengikuti kebutuhan hukum ketenagakerjaan/pajak |

### 8.2 Di luar cakupan
- **PCI DSS:** tidak berlaku. LedgerPay tidak memproses, menyimpan, atau mentransmisikan data kartu (top-up lewat provider hosted checkout).
- Regulasi e-money / penyelenggara jasa pembayaran: LedgerPay adalah sistem portfolio, bukan layanan berizin. Batas saldo hanya "terinspirasi" aturan (BR9) dan ditandai untuk diverifikasi.
