# Step 6.1 — Shared Threats (LedgerPay & PayRun)

> Bagian dari [Step 6](00-index.md). Format, skala risiko, dan skema ID mengikuti `00-index.md`.
> Threat di sini berlaku untuk **kedua** project karena keduanya memakai skeleton yang sama (ADR-001, ADR-009, ADR-013). Threat yang spesifik domain ada di `02-ledgerpay-threat-model.md` (T-LP) dan `03-payrun-threat-model.md` (T-PR). Kontrol `C-xx` didefinisikan di `04-security-controls-and-testing.md`.

---

## 1. Aset bersama

| Aset | Kenapa berharga | Lokasi |
|---|---|---|
| Kredensial user (password) | Pengambilalihan akun → akses uang / data gaji | `users.password_hash` (argon2id) |
| Access token (JWT) | Bearer: siapa pun yang memegangnya bertindak sebagai user selama ≤ 15 menit | Memori SPA |
| Refresh token | Bisa menerbitkan access token baru hingga 30 hari (batas family) | Cookie HttpOnly; DB menyimpan SHA-256 |
| Kunci penandatanganan JWT | Bisa memalsukan token **siapa pun**, termasuk OPS/OWNER | Env/secret di VM |
| Secret lain (DB password, webhook secret, keyring PII) | Akses langsung ke data atau pemalsuan pesan | Env/secret di VM |
| Database | Sumber kebenaran semua data | Container Postgres, volume, backup |
| Audit log | Bukti non-repudiation | `audit_log` (append-only) |
| Ketersediaan layanan | Tanpa layanan, tidak ada transaksi/payroll | VM tunggal (V1) |
| Pipeline build & dependency | Kode berbahaya bisa masuk ke image production | GitHub Actions, GHCR, go.sum, package-lock |

## 2. Trust boundary bersama

```text
  Internet (tidak tepercaya)
  ─────────────────────────────────────────────────────────────── B1: TLS (Caddy)
   Browser SPA ──HTTPS──▶ Caddy :443 ──HTTP (network compose internal)──▶ api :8080
   (XSS di sini = token di memori dicuri)                               │
                                                                        │ B2: app ↔ DB
  ─────────────────────────────────────────────────────────────────────┼─ role <proj>_app
                                                                        ▼  (bukan owner)
                                                         Postgres (tidak dipublish ke host)
                                                                        ▲
   worker ──────────────────────────────────────────────────────────────┘
      │ B3: egress ke pihak luar (mock-provider / object storage)
      ▼
  ─────────────────────────────────────────────────────────────── B4: operator
   Developer laptop ──SSH key──▶ VM (docker compose, .env 0600)
   GitHub Actions ──SSH deploy key / GHCR token──▶ VM                B5: supply chain
```

| Boundary | Yang melintas | Ancaman utama |
|---|---|---|
| B1 Internet ↔ Caddy/api | Request user, merchant, webhook | Spoofing identitas, injection, CSRF, DoS |
| B2 api/worker ↔ DB | Query SQL | SQL injection, privilege terlalu luas, eskalasi lewat owner |
| B3 worker ↔ pihak luar | Panggilan HTTP keluar | Lihat T-LP / T-PR |
| B4 Operator ↔ VM | SSH, file `.env` | Kompromi host = semua secret |
| B5 Kode ↔ image | Dependency, base image, CI | Supply chain |

---

## 3. Threats

### T-SH-01 — Credential stuffing & brute force login              [Inherent: High → Residual: Low]
```text
STRIDE        : S
Aset          : Kredensial user, akun
Threat        : Penyerang menebak password atau mencoba daftar email:password bocor dari situs lain.
Attack scenario: Botnet mengirim ribuan POST /auth/login per menit dengan kombinasi dari dump bocor,
                 tersebar di banyak IP; atau satu IP menebak password satu akun target.
Mitigation    : (1) Rate limit tier auth-strict: 5/menit dan 20/jam per (IP + email), tier auth-ip 30/menit per IP (C-07).
                (Step 12 R2-5) Akun terkunci dijawab persis seperti kredensial salah (`401 INVALID_CREDENTIALS` + `Retry-After`), tidak pernah `ACCOUNT_LOCKED`, agar status lock tidak membocorkan email yang terdaftar.
                (2) Lockout bertahap per akun: setelah 5 gagal → users.locked_until = now()+1 menit, berlipat
                    sampai maksimal 15 menit; reset saat login sukses (C-03).
                (3) Pesan generik 401 INVALID_CREDENTIALS untuk email tidak ada maupun password salah.
                (4) Timing equalization: untuk email yang tidak ada, tetap jalankan argon2id terhadap hash dummy
                    sehingga waktu respons tidak membocorkan keberadaan email.
                (5) Akun terkunci → 403 ACCOUNT_LOCKED + Retry-After (tidak menyebut alasan detail).
Detection     : Metrik auth_login_failed_total{reason}, auth_account_locked_total; alert jika rasio gagal > 50%
                selama 5 menit; log berisi IP + email ter-hash (bukan email mentah).
Test          : authn_bruteforce_lockout (6 percobaan → 403 ACCOUNT_LOCKED); authn_unknown_email_timing
                (selisih median waktu respons email ada vs tidak ada < 20%); authn_generic_error.
Status        : MVP. Residual: lockout bisa disalahgunakan untuk mengunci akun korban (DoS per akun). Diterima,
                karena lock singkat (≤ 15 menit) dan per-IP limiter membatasi skala. Production: MFA, CAPTCHA adaptif.
```

### T-SH-02 — Kebocoran database password hash                    [Inherent: High → Residual: Low]
```text
STRIDE        : I
Aset          : users.password_hash
Threat        : Dump DB/backup bocor dan penyerang melakukan cracking offline.
Attack scenario: Backup pg_dump yang tersimpan tanpa enkripsi bocor; penyerang menjalankan GPU cracking.
Mitigation    : argon2id m=64 MiB, t=3, p=2, salt 16 byte per user (ADR-009), dituning ±100–250 ms di server target.
                Panjang minimal 10 karakter; cek terhadap daftar offline ±100.000 password paling umum yang
                di-embed di binary (tanpa panggilan HIBP, agar tidak ada dependency jaringan dan kebocoran
                prefix hash ke pihak ketiga). Backup dienkripsi (T-SH-19).
                Keputusan: TANPA pepper di V1. Pepper menambah secret yang kalau hilang membuat semua password
                tidak bisa diverifikasi, dan kalau bocor bersama DB tidak memberi apa-apa. Nilai tambahnya nyata
                hanya jika disimpan terpisah (KMS/HSM), dan itu production enhancement.
Detection     : Tidak bisa dideteksi saat cracking; deteksi kebocoran backup lewat akses log storage.
Test          : password_hash_params (hash yang dihasilkan memakai parameter terkonfigurasi);
                password_policy_rejects_common.
Status        : MVP. Production: pepper di KMS, rehash otomatis saat parameter dinaikkan (rehash-on-login).
```

### T-SH-03 — Pemalsuan JWT / algorithm confusion                  [Inherent: Critical → Residual: Low]
```text
STRIDE        : S, E
Aset          : Kunci JWT, semua akun
Threat        : Penyerang membuat token valid tanpa login, atau mengubah claim (roles, tid, sub).
Attack scenario: Mengirim token dengan header alg=none; mengganti HS256 menjadi RS256 dengan public key
                 sebagai secret; mengedit payload roles=["OPS"] dan berharap signature tidak dicek.
Mitigation    : Verifier mem-pin algoritma HS256 (library dikonfigurasi dengan daftar alg yang diizinkan,
                bukan membaca alg dari header). Token tanpa kid atau kid tidak dikenal ditolak.
                Validasi exp, nbf, iat, iss, aud dengan clock skew maksimal 30 detik. Secret ≥ 256 bit acak per
                environment. Roles dan tid hanya dipercaya karena token ditandatangani server dan diterbitkan
                dari state DB (ADR-009, ADR-P02); operasi sensitif tetap memeriksa ulang DB (T-SH-13).
Detection     : Metrik auth_token_invalid_total{reason=signature|alg|expired|kid}; lonjakan reason=signature → alert.
Test          : jwt_rejects_alg_none, jwt_rejects_alg_swap, jwt_rejects_tampered_claims, jwt_rejects_unknown_kid,
                jwt_rejects_expired_with_skew.
Status        : MVP.
```

### T-SH-04 — Pencurian access token lewat XSS                      [Inherent: High → Residual: Medium]
```text
STRIDE        : S, I
Aset          : Access token
Threat        : Skrip berbahaya di halaman membaca token dan mengirimnya ke penyerang.
Attack scenario: Nama merchant/karyawan berisi <img src=x onerror=…> yang dirender sebagai HTML; skrip membaca
                 token dari localStorage lalu mengirimnya.
Mitigation    : Access token hanya di memori (Pinia), bukan localStorage/sessionStorage; TTL 15 menit.
                Refresh token di cookie HttpOnly sehingga tidak terbaca JavaScript. Pencegahan XSS utama di T-SH-09.
Detection     : Laporan CSP violation (report-to) dicatat sebagai metrik csp_violation_total.
Test          : e2e_token_not_in_web_storage (Playwright memeriksa localStorage/sessionStorage kosong dari token).
Status        : MVP. Residual Medium: XSS yang berhasil tetap bisa memakai token selama sesi halaman aktif
                (memanggil API atas nama user). Memori hanya mencegah pencurian jangka panjang, bukan pemakaian
                langsung. Karena itu pencegahan XSS (T-SH-09) adalah kontrol utama.
```

### T-SH-05 — Pencurian & replay refresh token                      [Inherent: High → Residual: Low]
```text
STRIDE        : S
Aset          : Refresh token, sesi
Threat        : Penyerang yang memperoleh refresh token bisa menerbitkan access token baru berulang kali.
Attack scenario: Malware di perangkat korban menyalin cookie; atau token bocor lewat log proxy yang salah konfigurasi.
Mitigation    : Opaque 256-bit, DB hanya menyimpan SHA-256 (dump DB tidak memberi token yang bisa dipakai).
                Rotasi setiap refresh; token lama ditandai used_at + replaced_by.
                Reuse detection: token yang sudah dipakai dipresentasikan lagi di luar grace window →
                revoke seluruh family (revoked_at, revoke_reason='REUSE') + audit + 401 REFRESH_TOKEN_REUSED.
                Grace window 10 detik: token lama yang dipakai lagi dalam 10 detik DAN replaced_by masih
                aktif mengembalikan token pengganti yang sama (bukan menerbitkan baru), untuk menangani dua tab
                yang refresh bersamaan tanpa false positive.
                Batas absolut family_expires_at = 30 hari; expires_at sliding 7 hari.
                Keputusan: refresh token TIDAK diikat ke User-Agent/IP. UA mudah dipalsukan dan berubah saat
                browser update; IP berubah di jaringan seluler. Keduanya menghasilkan revokasi palsu tanpa
                menghentikan penyerang yang serius. Reuse detection adalah kontrol yang sebenarnya.
Detection     : auth_refresh_reuse_total (alert jika > 0 per user dalam 1 jam, atau lonjakan global);
                audit refresh_token.family_revoked.
Test          : refresh_rotation_issues_new_token, refresh_reuse_revokes_family, refresh_parallel_within_grace_ok,
                refresh_reuse_after_grace_revokes, refresh_family_absolute_expiry.
Status        : MVP. Residual: dalam grace window 10 detik, penyerang yang memakai token curian tepat bersamaan
                dengan korban bisa memperoleh token pengganti yang sama. Jendelanya sempit, dan begitu salah satu
                pihak melakukan refresh berikutnya, reuse terdeteksi. Diterima.
```

### T-SH-06 — Sesi tidak bisa dicabut (logout, akun dinonaktifkan)  [Inherent: Medium → Residual: Low]
```text
STRIDE        : E
Aset          : Sesi
Threat        : User logout atau dinonaktifkan, tetapi token lama masih berlaku.
Attack scenario: Laptop bersama: user logout, penyerang memakai access token yang masih ada di memori tab lain.
Mitigation    : Logout merevoke family refresh token. Access token kedaluwarsa ≤ 15 menit (stateless, tanpa denylist
                di V1). users.status = DISABLED → refresh ditolak, dan endpoint sensitif memeriksa status user di DB.
                PayRun: pencabutan membership berlaku paling lambat saat refresh (ADR-P02), dan operasi sensitif
                memeriksa ulang membership.
Detection     : Audit auth.logout, user.disabled.
Test          : logout_revokes_family, disabled_user_cannot_refresh, sensitive_op_rechecks_user_status.
Status        : MVP. Residual: jendela ≤ 15 menit untuk endpoint non-sensitif. Production: denylist jti di Redis.
```

### T-SH-07 — CSRF                                                    [Inherent: Medium → Residual: Low]
```text
STRIDE        : T, E
Aset          : Sesi, aksi atas nama user
Threat        : Situs lain memicu request ber-cookie ke API.
Attack scenario: Halaman evil.example berisi form auto-submit ke /api/v1/auth/refresh untuk memperoleh token,
                 atau ke endpoint transfer.
Mitigation    : Hanya endpoint refresh/logout yang memakai cookie, dan cookie itu SameSite=Strict, Secure, HttpOnly,
                Path=/api/v1/auth. Endpoint tersebut juga mewajibkan header X-Requested-With: fetch (form HTML
                tidak bisa mengirim header kustom) dan memvalidasi header Origin terhadap origin aplikasi.
                Semua endpoint lain memakai Authorization: Bearer yang tidak dikirim otomatis oleh browser,
                sehingga tidak rentan CSRF. CORS tidak dibuka (T-SH-08), jadi preflight dari origin lain gagal.
Detection     : auth_csrf_rejected_total.
Test          : csrf_refresh_without_header_rejected, csrf_refresh_bad_origin_rejected.
Status        : MVP.
```

### T-SH-08 — CORS misconfiguration                                   [Inherent: Medium → Residual: Low]
```text
STRIDE        : I
Aset          : Data user
Threat        : Origin lain bisa membaca respons API dengan kredensial user.
Attack scenario: Server memantulkan header Origin apa pun ke Access-Control-Allow-Origin plus
                 Allow-Credentials: true; situs penyerang membaca saldo atau data gaji.
Mitigation    : SPA dan API disajikan dari origin yang sama lewat Caddy (/ dan /api/*), sehingga CORS tidak
                diperlukan. API tidak mengirim header CORS sama sekali; preflight OPTIONS dari origin lain
                tidak mendapat izin. Kalau kelak dibutuhkan, gunakan allowlist eksplisit, bukan refleksi atau wildcard.
                Merchant API (LedgerPay) dipanggil server-to-server, jadi juga tidak butuh CORS.
Detection     : —
Test          : cors_preflight_foreign_origin_denied (tidak ada Access-Control-Allow-Origin di respons).
Status        : MVP.
```

### T-SH-09 — Stored / reflected XSS                                  [Inherent: High → Residual: Low]
```text
STRIDE        : T, I, E
Aset          : Sesi user di browser
Threat        : Input user dirender sebagai kode di browser user lain.
Attack scenario: Merchant menyimpan display name berisi skrip; ops membuka ops console dan skrip berjalan dengan
                 hak OPS. Di PayRun: nama karyawan atau catatan cuti berisi skrip yang dibuka HR.
Mitigation    : Vue template auto-escaping; larangan v-html (aturan ESLint vue/no-v-html = error); teks dari server
                (nama, catatan, explanation line payroll) selalu dirender sebagai teks.
                CSP ketat: default-src 'self'; script-src 'self' (tanpa inline script, tanpa 'unsafe-eval');
                object-src 'none'; frame-ancestors 'none'; base-uri 'self' (detail header di 04 §2). Build Vite
                tidak memerlukan inline script, jadi nonce tidak dibutuhkan.
                API selalu Content-Type: application/json + X-Content-Type-Options: nosniff, sehingga respons
                JSON tidak bisa diinterpretasi sebagai HTML.
                Trusted Types: opsional (P2); dicatat sebagai hardening.
Detection     : CSP report-to → csp_violation_total.
Test          : lint_no_v_html (CI), e2e_xss_payload_rendered_as_text (payload di nama → tampil literal, tidak ada
                dialog), headers_csp_present.
Status        : MVP.
```

### T-SH-10 — SQL injection                                           [Inherent: Critical → Residual: Low]
```text
STRIDE        : T, I, E
Aset          : Database
Threat        : Input user mengubah struktur query.
Attack scenario: Parameter filter ?status=PENDING' OR '1'='1 dirangkai ke string SQL.
Mitigation    : Semua query lewat sqlc (parameterized; ADR-005). Tidak ada fmt.Sprintf/konkatenasi untuk SQL;
                filter dinamis diimplementasikan sebagai varian query tetap atau parameter nullable
                (WHERE ($1::text IS NULL OR status = $1)), bukan string building. Kolom sort tidak bisa dipilih
                bebas (Step 5 §6). gosec G201/G202 di CI. Role DB aplikasi bukan owner, sehingga injection pun tidak
                bisa DROP/ALTER, dan di PayRun tetap di bawah RLS.
Detection     : Error SQL syntax tak terduga (42601) → metrik db_unexpected_error_total + alert.
Test          : gosec di CI (fail), sqli_filter_payloads_rejected (payload umum di setiap filter → 400 atau hasil
                kosong, tidak pernah 500).
Status        : MVP.
```

### T-SH-11 — Mass assignment & field tak dikenal                     [Inherent: Medium → Residual: Low]
```text
STRIDE        : T, E
Aset          : Integritas data, privilege
Threat        : Client menyisipkan field yang tidak seharusnya bisa diubah.
Attack scenario: PATCH /employees/{id} dengan {"status":"ACTIVE","tenant_id":"<lain>"}; POST /auth/register dengan
                 {"role":"OPS"}; POST /transfers dengan {"from_account_id":"<akun korban>"}.
Mitigation    : DTO request eksplisit per endpoint (oapi-codegen dari OpenAPI), bukan bind langsung ke model DB.
                Decoder JSON strict (DisallowUnknownFields) → 400 VALIDATION_ERROR untuk field tidak dikenal
                (Step 5: body berisi tenant_id ditolak). Field turunan server (role, tenant_id, owner, status) tidak
                pernah ada di DTO request.
Detection     : validation_error_total{code=UNKNOWN_FIELD}.
Test          : mass_assignment_unknown_field_rejected (untuk setiap endpoint tulis, tambah field "role"/"tenant_id" → 400).
Status        : MVP.
```

### T-SH-12 — Input berukuran/berstruktur ekstrem                    [Inherent: Medium → Residual: Low]
```text
STRIDE        : D
Aset          : Ketersediaan api
Threat        : Body sangat besar atau JSON sangat dalam menghabiskan memori/CPU.
Attack scenario: POST 50 MB JSON; JSON dengan nesting 100.000 level; string 10 MB di field note.
Mitigation    : http.MaxBytesReader 64 KB (413 PAYLOAD_TOO_LARGE), CSV 5 MB khusus upload PayRun; batas kedalaman
                JSON (decoder dengan limit depth 32); batas panjang per field di validator (contoh note ≤ 140);
                Caddy request_body max_size sebagai lapisan pertama. Hanya Content-Type yang diharapkan
                (415 UNSUPPORTED_MEDIA_TYPE).
Detection     : http_request_rejected_total{reason=too_large|depth|media_type}.
Test          : body_limit_413, json_depth_limit_400, content_type_415.
Status        : MVP.
```

### T-SH-13 — IDOR / BOLA (pola umum)                                 [Inherent: Critical → Residual: Low]
```text
STRIDE        : I, T, E
Aset          : Resource milik user/merchant/tenant lain
Threat        : Mengakses objek orang lain dengan mengganti ID di path/body.
Attack scenario: GET /payouts/{id milik orang lain}; POST /leave-requests dengan employee_id orang lain.
Mitigation    : Setiap query di-scope ke principal (WHERE owner = $principal) di service layer, bukan dicek setelah
                fetch (ADR-010). Resource bukan milik principal → 404 NOT_FOUND (bukan 403), agar keberadaan tidak bocor.
                UUIDv7 BUKAN kontrol keamanan: ID bisa bocor lewat log/URL/screenshot, dan bagian awalnya
                berbasis waktu, sehingga tidak boleh diandalkan untuk kerahasiaan.
                PayRun menambah RLS + composite FK sebagai lapisan DB (T-PR). Detail per resource ada di T-LP/T-PR.
Detection     : authz_denied_total{resource}; lonjakan 404 per principal pada resource sensitif → alert (indikasi enumerasi).
Test          : bola_<resource> untuk setiap resource (user A mengakses resource B → 404), dijalankan generik dari
                daftar endpoint.
Status        : MVP.
```

### T-SH-14 — Privilege escalation (pola umum)                       [Inherent: Critical → Residual: Low]
```text
STRIDE        : E
Aset          : Role OPS (LedgerPay), OWNER/HR_ADMIN/PAYROLL_ADMIN (PayRun)
Threat        : User biasa memperoleh hak lebih tinggi.
Attack scenario: Mengirim header X-Role: OPS; mengubah claim roles di JWT; memanggil endpoint /ops/* dengan token
                 user biasa; HR_ADMIN memberi dirinya OWNER.
Mitigation    : Role selalu berasal dari state server (users.role / memberships.roles) dan dibawa di JWT yang
                ditandatangani (T-SH-03). Tidak ada header/parameter role dari client. Middleware per grup rute
                (/ops/* wajib role OPS). Perubahan role hanya lewat endpoint khusus dengan policy eksplisit
                (contoh: hanya OWNER yang bisa memberi role OWNER) dan selalu diaudit. Role OPS di LedgerPay hanya
                dibuat lewat seed/CLI, tidak ada endpoint untuk membuatnya.
Detection     : Audit membership.roles_changed / user.role_changed; alert untuk setiap pemberian role OPS/OWNER.
Test          : authz_matrix (Step 5 file 05), ops_route_rejects_user_role, cannot_self_promote_owner.
Status        : MVP.
```

### T-SH-15 — Bypass rate limit                                       [Inherent: Medium → Residual: Medium]
```text
STRIDE        : D, S
Aset          : Efektivitas T-SH-01, ketersediaan
Threat        : Penyerang melewati limiter dengan memalsukan IP atau memanfaatkan banyak instance.
Attack scenario: Mengirim X-Forwarded-For acak agar setiap request tampak dari IP berbeda.
Mitigation    : api hanya memercayai X-Forwarded-For dari Caddy (trusted proxy = alamat network compose);
                IP klien diambil dari hop yang ditambahkan Caddy, bukan dari nilai yang dikirim klien.
                Kunci limiter gabungan per user/API key + per IP (Step 5 §7).
                Keterbatasan V1: limiter in-memory per instance (ADR-007). Dengan satu instance hal ini akurat;
                restart mereset counter.
Detection     : rate_limited_total{tier}.
Test          : ratelimit_ignores_spoofed_xff, ratelimit_tier_<x>_429.
Status        : MVP (single instance). Production: Redis/limiter terdistribusi + rate limit di edge/WAF.
                Residual Medium: botnet dengan banyak IP asli tetap bisa mencoba credential stuffing secara lambat.
```

### T-SH-16 — Kebocoran secret                                        [Inherent: Critical → Residual: Medium]
```text
STRIDE        : I, S
Aset          : Kunci JWT, DB password, webhook secret, keyring PII
Threat        : Secret ter-commit ke git, tercetak di log, atau terbaca dari server.
Attack scenario: Developer meng-commit .env; secret dicetak saat config dump di startup; image Docker
                 menyertakan .env.
Mitigation    : .env tidak pernah di-commit (.gitignore + gitleaks di CI dan pre-commit). .env di VM dengan
                permission 0600 milik user deploy. .dockerignore mengecualikan .env.
                Config loader me-redact field bertag secret saat logging. Secret berbeda per environment
                (dev/demo). Setiap kunci punya identifier versi (kid / v1) sehingga bisa dirotasi tanpa downtime
                (runbook 04 §6).
Detection     : gitleaks (CI fail); audit akses SSH ke VM.
Test          : config_dump_redacts_secrets; gitleaks job.
Status        : MVP. Residual Medium: siapa pun dengan akses root VM membaca semua secret (T-SH-25).
                Production: secret manager + KMS dengan IAM terpisah.
```

### T-SH-17 — Penyadapan lalu lintas (encryption in transit)         [Inherent: High → Residual: Low]
```text
STRIDE        : I, T
Aset          : Token, data finansial, PII
Threat        : Lalu lintas dibaca atau dimodifikasi di jaringan.
Attack scenario: Wi-Fi publik; downgrade ke HTTP.
Mitigation    : Caddy TLS otomatis (Let's Encrypt), redirect HTTP→HTTPS, HSTS max-age=31536000; includeSubDomains.
                Lalu lintas Caddy ↔ api dan api ↔ Postgres berjalan di network compose internal yang tidak
                dipublish ke host. Port Postgres tidak di-expose. Panggilan keluar ke provider/object storage
                memakai HTTPS dengan verifikasi sertifikat (tidak ada InsecureSkipVerify).
Detection     : —
Test          : headers_hsts_present, compose_no_db_port_published (cek konfigurasi di CI).
Status        : MVP. Production: TLS ke managed DB (sslmode=verify-full), mTLS internal jika jaringan dibagi.
```

### T-SH-18 — Kebocoran data at rest (disk, backup)                   [Inherent: High → Residual: Medium]
```text
STRIDE        : I
Aset          : Seluruh data DB
Threat        : Disk VM, snapshot, atau file backup diakses pihak tidak berwenang.
Attack scenario: Backup pg_dump diunggah ke bucket publik karena salah konfigurasi.
Mitigation    : Enkripsi disk dari penyedia VM. Backup dienkripsi sebelum diunggah (age/gpg dengan public key;
                private key tidak disimpan di VM) ke bucket private. PayRun: PII sensitif dienkripsi di level
                aplikasi (ADR-P08), sehingga dump DB saja tidak membuka NIK/NPWP/rekening.
Detection     : Log akses bucket backup.
Test          : backup_artifact_is_encrypted (script restore drill memastikan file tidak bisa dibaca tanpa kunci).
Status        : MVP. Residual Medium: data non-PII (saldo, nominal gaji) tetap terbaca dari dump yang didekripsi.
```

### T-SH-19 — Data sensitif di log                                    [Inherent: High → Residual: Low]
```text
STRIDE        : I
Aset          : Password, token, API key, PII, nominal gaji
Threat        : Log menjadi tempat kebocoran karena aksesnya lebih longgar dibanding DB.
Attack scenario: Middleware mencatat body request login (password) atau header Authorization; error PayRun
                 mencetak struct employee lengkap dengan NIK.
Mitigation    : Logging berbasis allowlist field: middleware request log hanya mencatat method, route template,
                status, durasi, request_id, principal id, tenant_id. Body tidak pernah dicatat untuk /auth/* dan
                endpoint PII. Header Authorization, Cookie, Provider-Signature selalu di-redact. Tipe Go untuk
                secret/PII (Secret, EncryptedField) mengimplementasikan LogValuer yang mengembalikan "[REDACTED]".
                Email di log di-hash.
Detection     : Test otomatis + review.
Test          : log_redaction_auth_body, log_redaction_headers, log_redaction_pii_types (menangkap output slog di test
                dan memastikan tidak ada pola password/token/NIK).
Status        : MVP.
```

### T-SH-20 — Repudiation & audit log tampering                      [Inherent: Medium → Residual: Low]
```text
STRIDE        : R, T
Aset          : audit_log, journals
Threat        : Pelaku menyangkal aksinya, atau menghapus/mengubah jejaknya.
Attack scenario: Ops yang curang meresolusi payout sebagai SUCCEEDED lalu menghapus baris audit; developer dengan
                 akses DB meng-UPDATE audit_log.
Mitigation    : audit_log append-only: trigger forbid_update_delete + grant role app hanya SELECT, INSERT
                (Step 4 §6). Entri berisi actor_type, actor_id, request_id, correlation_id, IP, reason. Audit
                ditulis di transaksi yang sama dengan perubahan bisnis, sehingga tidak ada perubahan tanpa jejak.
                Role owner (yang bisa men-DROP trigger) hanya dipakai migrasi.
Detection     : Migrasi yang menyentuh trigger audit ditinjau di PR (CODEOWNERS); job verifikasi jumlah audit vs aksi.
Test          : audit_log_update_rejected, audit_log_delete_rejected, audit_written_in_same_tx (rollback → tidak ada audit).
Status        : MVP. Residual: pemegang role owner/akses root tetap bisa mengubah. Production: kirim audit ke
                penyimpanan WORM/eksternal (object lock) secara near real-time, dengan hash chain.
```

### T-SH-21 — Information disclosure lewat error                      [Inherent: Medium → Residual: Low]
```text
STRIDE        : I
Aset          : Detail internal (skema, versi, query)
Threat        : Error message membocorkan struktur internal atau keberadaan data.
Attack scenario: Mengirim input rusak dan membaca pesan Postgres mentah berisi nama tabel/constraint.
Mitigation    : Semua error dipetakan ke katalog RFC 9457 (Step 5 file 01). Error tak terpetakan → 500 INTERNAL_ERROR
                generik; detail hanya di log yang ditautkan request_id. Tidak ada stack trace di respons.
                Header Server/X-Powered-By dihapus. Pesan untuk NIK_TAKEN, INVALID_RECIPIENT, dan sejenisnya
                sengaja tidak menyebut pemilik data.
Detection     : —
Test          : error_unmapped_returns_generic_500, error_no_pg_message_leak.
Status        : MVP.
```

### T-SH-22 — Supply chain (dependency & base image)                  [Inherent: High → Residual: Medium]
```text
STRIDE        : T, E
Aset          : Image production
Threat        : Dependency atau base image berbahaya/rentan masuk ke build.
Attack scenario: Paket npm typosquat; library Go dengan CVE kritis; base image tag :latest berubah diam-diam.
Mitigation    : go.sum + package-lock.json di-commit dan diverifikasi; govulncheck dan osv-scanner/npm audit di CI;
                base image di-pin dengan digest (@sha256:…); Dependabot/Renovate untuk pembaruan terkontrol;
                trivy scan image (fail pada CRITICAL/HIGH yang punya fix). GitHub Actions di-pin ke commit SHA.
                SBOM via syft: opsional (P2).
Detection     : Laporan scan CI terjadwal mingguan (bukan hanya saat push).
Test          : Job CI (04 §3).
Status        : MVP. Residual Medium: zero-day atau maintainer yang dikompromikan tidak tertangkap scanner.
```

### T-SH-23 — Container escape / runtime compromise                   [Inherent: Medium → Residual: Low]
```text
STRIDE        : E
Aset          : Host VM
Threat        : Celah di aplikasi dimanfaatkan untuk mengeksekusi perintah dan keluar dari container.
Attack scenario: RCE di parser → shell di container → menulis ke filesystem → eskalasi ke host.
Mitigation    : Image distroless static (tanpa shell/package manager), user non-root (UID 65532), read_only: true
                rootfs (tmpfs untuk /tmp bila perlu), security_opt no-new-privileges:true, cap_drop: [ALL],
                tidak ada privileged/mount docker.sock. Resource limit (mem/cpu) per container.
Detection     : Restart container tak terduga → alert.
Test          : compose_hardening_check (lint konfigurasi compose di CI: non-root, read_only, cap_drop).
Status        : MVP.
```

### T-SH-24 — DoS tingkat aplikasi                                     [Inherent: High → Residual: Medium]
```text
STRIDE        : D
Aset          : Ketersediaan
Threat        : Request lambat atau mahal menghabiskan koneksi, goroutine, atau pool DB.
Attack scenario: Slowloris (header dikirim sangat lambat); banyak request ke endpoint list dengan rentang tanggal
                 besar; request yang menunggu lock menahan koneksi pool.
Mitigation    : http.Server: ReadHeaderTimeout 5s, ReadTimeout 15s, WriteTimeout 30s, IdleTimeout 60s; Caddy timeouts
                serupa. Postgres: statement_timeout 5s (API), lock_timeout 2s, idle_in_transaction_session_timeout 10s
                (ADR-011). Pool berukuran tetap (api 10–20 koneksi). Batas rentang tanggal 366 hari, limit list ≤ 100
                (Step 5 §6). Endpoint mahal (calculate, import, finalize) punya tier heavy per tenant. Body limit (T-SH-12).
Detection     : http_in_flight_requests, db_pool_wait_seconds, p99 latency → alert.
Test          : server_timeouts_configured, list_range_limit_enforced, load test Step 9.
Status        : MVP. Residual Medium: DDoS volumetrik di luar kemampuan satu VM. Production: CDN/WAF/DDoS protection.
```

### T-SH-25 — Kompromi host VM / akses SSH                            [Inherent: Critical → Residual: Medium]
```text
STRIDE        : E, I, T
Aset          : Semua (secret, DB, image)
Threat        : Penyerang memperoleh akses shell ke VM.
Attack scenario: Brute force password SSH; kunci SSH developer dicuri; port database/Grafana terbuka ke internet.
Mitigation    : SSH key-only (PasswordAuthentication no, PermitRootLogin no); ufw hanya membuka 22/80/443;
                fail2ban untuk SSH; unattended-upgrades untuk patch keamanan OS. Grafana/Prometheus tidak dipublish
                (akses lewat SSH tunnel). Deploy key CI terbatas pada user deploy (bukan root), dengan perintah dibatasi bila memungkinkan.
Detection     : auth.log dipantau; alert login SSH dari IP baru (P2).
Test          : Skrip cek konfigurasi host (port terbuka, sshd_config) dijalankan saat provisioning.
Status        : MVP (hardening dasar). Production: bastion/SSM, tanpa SSH langsung; immutable infra; CIS benchmark.
                Residual Medium: satu VM berarti satu titik kompromi untuk semua komponen.
```

### T-SH-26 — Manipulasi waktu / kepercayaan pada clock klien       [Inherent: Medium → Residual: Low]
```text
STRIDE        : T
Aset          : Expiry, absensi, cut-off, timestamp audit
Threat        : Klien mengirim timestamp palsu untuk memperpanjang validitas atau memalsukan waktu kejadian.
Attack scenario: Client mengirim "clock_in_at" mundur 2 jam; merchant mengirim expires_at sangat jauh.
Mitigation    : Semua timestamp bisnis ditentukan server (now() di DB / Clock di aplikasi). API tidak menerima
                timestamp kejadian dari klien kecuali sebagai input yang divalidasi dan dibatasi (contoh: rentang
                tanggal lembur dalam periode berjalan). Expiry dihitung server dari konfigurasi. NTP aktif di VM.
Detection     : —
Test          : clock_in_ignores_client_time, expiry_server_computed.
Status        : MVP.
```

---

## 4. Ringkasan

| ID | Threat | Inherent | Residual | Status | Kontrol utama |
|---|---|---|---|---|---|
| T-SH-01 | Credential stuffing / brute force | High | Low | MVP | C-03, C-07 |
| T-SH-02 | Kebocoran hash password | High | Low | MVP | C-02 |
| T-SH-03 | Pemalsuan JWT / alg confusion | Critical | Low | MVP | C-04 |
| T-SH-04 | Pencurian access token via XSS | High | Medium | MVP | C-04, C-10 |
| T-SH-05 | Pencurian/replay refresh token | High | Low | MVP | C-05 |
| T-SH-06 | Sesi tidak bisa dicabut | Medium | Low | MVP | C-05, C-06 |
| T-SH-07 | CSRF | Medium | Low | MVP | C-08 |
| T-SH-08 | CORS misconfiguration | Medium | Low | MVP | C-09 |
| T-SH-09 | XSS | High | Low | MVP | C-10, C-11 |
| T-SH-10 | SQL injection | Critical | Low | MVP | C-12, C-15 |
| T-SH-11 | Mass assignment | Medium | Low | MVP | C-13 |
| T-SH-12 | Input ekstrem | Medium | Low | MVP | C-14 |
| T-SH-13 | IDOR / BOLA | Critical | Low | MVP | C-06, C-16 |
| T-SH-14 | Privilege escalation | Critical | Low | MVP | C-06, C-17 |
| T-SH-15 | Bypass rate limit | Medium | Medium | MVP | C-07 |
| T-SH-16 | Kebocoran secret | Critical | Medium | MVP | C-18, C-19 |
| T-SH-17 | Penyadapan in transit | High | Low | MVP | C-20 |
| T-SH-18 | Data at rest | High | Medium | MVP | C-21, C-22 |
| T-SH-19 | Data sensitif di log | High | Low | MVP | C-23 |
| T-SH-20 | Repudiation / audit tampering | Medium | Low | MVP | C-24 |
| T-SH-21 | Disclosure lewat error | Medium | Low | MVP | C-25 |
| T-SH-22 | Supply chain | High | Medium | MVP | C-26, C-27 |
| T-SH-23 | Container escape | Medium | Low | MVP | C-28 |
| T-SH-24 | DoS aplikasi | High | Medium | MVP | C-29, C-45 |
| T-SH-25 | Kompromi host / SSH | Critical | Medium | MVP | C-30 |
| T-SH-26 | Manipulasi waktu | Medium | Low | MVP | C-31 |
