# Step 5.4 — PayRun API Contract

> Konvensi, header, rate tier, dan format endpoint: [`00-index.md`](00-index.md). Error code: hanya dari [`01-error-catalogue.md`](01-error-catalogue.md).
> Fitur: Step 2 [`03-payrun-features.md`](../step-02-features-and-architecture/03-payrun-features.md) (PF-xx). State machine: Step 3 [`00-index.md`](../step-03-domain-design/00-index.md). Tabel: Step 4 [`06-payrun-schema.md`](../step-04-database-design/06-payrun-schema.md).
> Matriks role per endpoint: [`05-payrun-authorization-matrix.md`](05-payrun-authorization-matrix.md). Kolom "Authorization" di bawah hanya ringkasan.

---

## 1. Aturan tenant context (dibaca sebelum endpoint mana pun)

**Semua endpoint tenant-scoped mengambil tenant dari claim `tid` di access token.** Tenant tidak pernah diambil dari path, header, query, atau body. Karena itu path **tidak** diawali `/tenants/{tenant_id}`.

| Alasan | Penjelasan |
|---|---|
| Satu sumber kebenaran | `tid` diterbitkan server hanya setelah membership `ACTIVE` diverifikasi (ADR-P02). Token ditandatangani, sehingga tidak bisa dimanipulasi |
| Tidak ada IDOR lewat path | Kalau tenant ada di path, setiap handler wajib membandingkan `path.tenant_id == token.tid`. Satu handler yang lupa = kebocoran. Tanpa tenant di path, pembandingan itu tidak perlu ada |
| Selaras dengan RLS | `WithTenantTx` membaca tenant dari `Principal` di context. Tidak ada jalur bagi input request untuk memengaruhi `app.tenant_id` |
| Field `tenant_id` di body diabaikan | Body yang mengirim `tenant_id` → `400 VALIDATION_ERROR` (field tidak dikenal). Field ini sengaja tidak diterima agar tidak ada ilusi bahwa tenant bisa dipilih lewat body |

**Pengecualian (tanpa `tid`, memakai `WithPlatformTx` atau tanpa auth):** `POST /auth/login`, `POST /auth/refresh`, `POST /auth/logout`, `GET /me`, `GET /me/memberships`, `POST /session/switch-tenant`, `POST /tenants/signup`, `POST /invitations/accept`, `GET /healthz`, `GET /readyz`.

Token **tanpa** `tid` yang memanggil endpoint tenant-scoped → `403 FORBIDDEN` (`detail`: "Select a tenant first"). Client harus memanggil switch-tenant.

### Notasi role (ringkasan; detail di file 05)
`O` OWNER · `H` HR_ADMIN · `P` PAYROLL_ADMIN · `M` MANAGER · `E` EMPLOYEE · `self` = hanya data employee miliknya sendiri · `reports` = bawahan langsung (effective-dated, ADR-P04).

---

## 2. Ringkasan endpoint

| # | Method | Path | PF | Role (ringkas) | Idempotency | If-Match | Rate | Prio |
|---|---|---|---|---|---|---|---|---|
| A1 | POST | `/auth/login` | PF-02 | publik | n.a. | | auth-strict | P0 |
| A2 | POST | `/auth/refresh` | PF-02 | cookie | n.a. | | auth-strict | P0 |
| A3 | POST | `/auth/logout` | PF-02 | cookie | natural | | auth-ip | P0 |
| A4 | GET | `/me` | PF-02 | user | n.a. | | read | P0 |
| A5 | GET | `/me/memberships` | PF-02 | user | n.a. | | read | P0 |
| A6 | POST | `/session/switch-tenant` | PF-02 | user + membership | natural | | write | P0 |
| T1 | POST | `/tenants/signup` | PF-01 | publik | **required** (platform) | | auth-strict | P0 |
| T2 | GET | `/tenant` | PF-01 | semua role | n.a. | | read | P0 |
| T3 | PATCH | `/tenant/settings` | PF-01 | O | n.a. | ✅ | write | P0 |
| T4 | GET | `/holidays` | PF-08 | semua role | n.a. | | read | P0 |
| T5 | POST | `/holidays` | PF-08 | O, H | natural (unik per tanggal) | | write | P1 |
| T6 | DELETE | `/holidays/{id}` | PF-08 | O, H | natural | | write | P1 |
| U1 | GET | `/members` | PF-02 | O, H | n.a. | | read | P0 |
| U2 | POST | `/invitations` | PF-02 | O, H | natural (unik per email undangan) | | write | P0 |
| U3 | POST | `/invitations/accept` | PF-02 | user | natural | | auth-ip | P0 |
| U4 | PATCH | `/members/{id}/roles` | PF-02 | O (H terbatas) | n.a. | ✅ | write | P0 |
| U5 | POST | `/members/{id}/revoke` | PF-02 | O (H terbatas) | natural | | write | P0 |
| G1 | GET | `/departments` · `/positions` | PF-05 | semua role | n.a. | | read | P0 |
| G2 | POST | `/departments` · `/positions` | PF-05 | O, H | natural (kode unik) | | write | P0 |
| G3 | PATCH | `/departments/{id}` · `/positions/{id}` | PF-05 | O, H | n.a. | ✅ | write | P0 |
| G4 | POST | `/departments/{id}/deactivate` · `/positions/{id}/deactivate` | PF-05 | O, H | natural | | write | P0 |
| W1 | GET | `/employees` | PF-06 | O, H, P · M (reports) | n.a. | | read | P0 |
| W2 | POST | `/employees` | PF-06 | O, H | **required** | | write | P0 |
| W3 | GET | `/employees/{id}` | PF-06 | O, H, P · M (reports) · E (self) | n.a. | | read | P0 |
| W4 | PATCH | `/employees/{id}` | PF-06 | O, H | n.a. | ✅ | write | P0 |
| W5 | POST | `/employees/{id}/terminate` | PF-06 | O, H | natural | ✅ | write | P0 |
| W6 | GET | `/employees/{id}/pii` | PF-20 | O, H · E (self) | n.a. | | read | P1 |
| W7 | POST | `/employees/{id}/org-assignments` | PF-05 | O, H | **required** | | write | P0 |
| W8 | GET | `/employees/{id}/salary-history` | PF-07 | O, H, P · E (self) | n.a. | | read | P0 |
| W9 | POST | `/employees/{id}/salary-changes` | PF-07 | O, H | **required** | | write | P0 |
| W10 | POST | `/employees/{id}/component-assignments` | PF-07 | O, H | **required** | | write | P0 |
| C1 | GET | `/component-definitions` | PF-07 | O, H, P | n.a. | | read | P0 |
| C2 | POST | `/component-definitions` | PF-07 | O, H | natural (kode unik) | | write | P0 |
| C3 | PATCH | `/component-definitions/{id}` | PF-07 | O, H | n.a. | ✅ | write | P0 |
| C4 | POST | `/component-definitions/{id}/deactivate` | PF-07 | O, H | natural | | write | P0 |
| D1 | POST | `/attendance/clock-in` | PF-08 | E (self) | natural (1 record/hari) | | write | P0 |
| D2 | POST | `/attendance/clock-out` | PF-08 | E (self) | natural | | write | P0 |
| D3 | GET | `/attendance` | PF-08 | O, H, P · M (reports) · E (self) | n.a. | | read | P0 |
| D4 | PATCH | `/attendance/{id}` | PF-08 | O, H | n.a. | ✅ | write | P0 |
| D5 | POST | `/attendance-imports` | PF-21 | O, H | natural (file hash) + **required** | | heavy | P1 |
| D6 | GET | `/attendance-imports/{id}` | PF-21 | O, H | n.a. | | read | P1 |
| D7 | GET | `/attendance-imports/{id}/errors` | PF-21 | O, H | n.a. | | read | P1 |
| L1 | GET | `/leave-types` | PF-09 | semua role | n.a. | | read | P0 |
| L2 | GET | `/me/leave-balances` | PF-09 | E (self) | n.a. | | read | P0 |
| L3 | POST | `/leave-requests` | PF-09 | E (self) | **required** | | write | P0 |
| L4 | POST | `/leave-requests/{id}/cancel` | PF-09 | E (self, PENDING) | natural | | write | P0 |
| L5 | GET | `/leave-requests` | PF-09 | O, H · M (reports) · E (self) | n.a. | | read | P0 |
| V1 | POST | `/overtime-requests` | PF-10 | E (self) | **required** | | write | P0 |
| V2 | POST | `/overtime-requests/{id}/cancel` | PF-10 | E (self, PENDING) | natural | | write | P0 |
| V3 | GET | `/overtime-requests` | PF-10 | O, H, P · M (reports) · E (self) | n.a. | | read | P0 |
| R1 | GET | `/approvals/inbox` | PF-11 | approver (siapa pun) | n.a. | | read | P0 |
| R2 | POST | `/approvals/{id}/approve` | PF-11, PF-13 | approver ter-snapshot · H (override, bukan run) | natural (guard status) | | write | P0 |
| R3 | POST | `/approvals/{id}/reject` | PF-11, PF-13 | approver ter-snapshot · H (override, bukan run) | natural | | write | P0 |
| Y1 | GET | `/payroll-periods` | PF-13 | O, H, P | n.a. | | read | P0 |
| Y2 | POST | `/payroll-runs` | PF-13 | O, P | **required** | | write | P0 |
| Y3 | GET | `/payroll-runs` | PF-13 | O, H, P | n.a. | | read | P0 |
| Y4 | GET | `/payroll-runs/{id}` | PF-13 | O, H, P | n.a. | | read | P0 |
| Y5 | POST | `/payroll-runs/{id}/calculate` | PF-13 | O, P | **required** | | heavy | P0 |
| Y6 | GET | `/payroll-runs/{id}/items` | PF-13 | O, P · H (audit) | n.a. | | read | P0 |
| Y7 | GET | `/payroll-runs/{id}/items/{item_id}` | PF-12, PF-13 | O, P · H (audit) | n.a. | | read | P0 |
| Y8 | POST | `/payroll-runs/{id}/items/{item_id}/exclude` | PF-13 | O, P | natural | | write | P0 |
| Y9 | GET | `/payroll-runs/{id}/diff` | PF-23 | O, P | n.a. | | read | P1 |
| Y10 | POST | `/payroll-runs/{id}/submit` | PF-13 | O, P | natural (guard status) | | write | P0 |
| Y11 | POST | `/payroll-runs/{id}/finalize` | PF-14 | O, P | **required** | | heavy | P0 |
| Y12 | POST | `/payroll-runs/{id}/cancel` | PF-13 | O, P | natural | | write | P0 |
| J1 | POST | `/adjustments` | PF-19 | O, H, P | **required** | | write | P1 |
| J2 | GET | `/adjustments` | PF-19 | O, H, P | n.a. | | read | P1 |
| J3 | POST | `/adjustments/{id}/cancel` | PF-19 | O, H, P | natural | | write | P1 |
| S1 | GET | `/me/payslips` | PF-15 | E (self) | n.a. | | read | P0 |
| S2 | GET | `/me/payslips/{id}` | PF-15 | E (self) | n.a. | | read | P0 |
| S3 | GET | `/payslips` | PF-15 | O, P · H (audit per akses) | n.a. | | read | P0 |
| S4 | GET | `/payslips/{id}` | PF-15 | O, P · H (audit) | n.a. | | read | P0 |
| S5 | GET | `/payslips/{id}/download` | PF-22 | pemilik · O, P · H (audit) | n.a. | | read | P1 |
| Z1 | GET | `/audit-log` | PF-16 | O, H | n.a. | | read | P0 |
| Z2 | GET | `/healthz` · `/readyz` | PF-17 | publik (internal) | n.a. | | — | P0 |

**Penyesuaian dari daftar awal (dengan alasan):**
- **Tidak ada `POST /payroll-runs/{id}/approve` dan `/reject`.** Approval run memakai `POST /approvals/{id}/approve|reject` yang sama dengan cuti/lembur (ADR-P09: satu mesin approval). Service mendispatch berdasarkan `subject_type`, dan untuk `PAYROLL_RUN` menjalankan fingerprint guard. Dua endpoint untuk satu aksi berarti dua tempat menegakkan authorization dan audit, yang justru menjadi sumber bug. `PayrollRun` mengembalikan `approval_request_id` agar UI langsung tahu endpoint mana yang dipanggil.
- **Ditambah `POST /employees/{id}/component-assignments`** (W10). PF-07 butuh penugasan komponen per employee secara effective-dated, dan tanpa endpoint ini engine tidak punya input tunjangan.
- **Ditambah `/holidays`** (T4–T6). Hari kerja (prorata, `PER_DAY`, absensi) bergantung pada kalender libur tenant.
- **Ditambah `GET /payslips/{id}`** (S4) untuk HR/Payroll. Akses ini terpisah dari `/me/payslips/{id}` agar audit "akses oleh non-pemilik" (BR10, PI-17) terjadi di satu tempat yang eksplisit.
- **Periode tidak punya endpoint create.** `payroll_periods` dibuat otomatis oleh `POST /payroll-runs` (status `OPEN`, `cutoff_at` dibekukan dari `tenant_settings`).

---

## 3. Auth & session

### A1 · POST `/auth/login`
- **Use case:** PF-02
- **Authentication:** tidak ada. **Authorization:** —
- **Request:**
  ```json
  { "email": "dewi@alpha.co.id", "password": "••••••••••" }
  ```
- **Response `200`:** body + `Set-Cookie: refresh_token=…; HttpOnly; Secure; SameSite=Strict; Path=/api/v1/auth`
  ```json
  {
    "access_token": "eyJ…", "token_type": "Bearer", "expires_in": 900,
    "user": { "id": "0192…a1", "email": "dewi@alpha.co.id", "display_name": "Dewi" },
    "active_tenant": { "id": "0192…t1", "name": "PT Alpha", "roles": ["OWNER","HR_ADMIN"] }
  }
  ```
  `active_tenant` = tenant terakhir yang dipakai jika membership-nya masih `ACTIVE`. Kalau user hanya punya satu membership aktif, tenant itu. Selain itu `null`, dan token diterbitkan tanpa `tid`.
- **Validation:** email format valid (≤ 254); password 1–128 karakter (tidak divalidasi panjang minimum saat login).
- **Errors:** `401 INVALID_CREDENTIALS` (email tidak ada **atau** password salah, waktu respons disamakan), `403 ACCOUNT_LOCKED` (+ `Retry-After`), `429 RATE_LIMITED`.
- **Idempotency:** n.a. **Pagination:** n.a. **Rate limit:** `auth-strict`.
- **Catatan:** lookup lewat `payrun.auth_lookup_user(email)` (satu-satunya `SECURITY DEFINER`, Step 4). Gagal login menaikkan `failed_login_count`; ambang mengunci `locked_until` (nilai di Step 6). Audit `auth.login_succeeded` / `auth.login_failed` (tanpa password).

### A2 · POST `/auth/refresh`
- **Use case:** PF-02 · **Authentication:** cookie `refresh_token` + header `X-Requested-With: fetch` · **Authorization:** —
- **Request:** tanpa body. Header opsional `X-Active-Tenant: <tenant_id>`.
- **Response `200`:** sama dengan A1 (token baru, cookie dirotasi). Kalau `X-Active-Tenant` dikirim dan membership masih `ACTIVE` → `tid` dipertahankan, dan roles dibaca ulang dari DB (perubahan role berlaku di sini). Kalau membership sudah dicabut → `200` dengan `active_tenant: null` dan token tanpa `tid`; client menampilkan pemilih tenant.
- **Validation:** `X-Active-Tenant` harus UUID valid (selain itu `400 VALIDATION_ERROR`).
- **Errors:** `401 UNAUTHENTICATED` (cookie tidak ada, kedaluwarsa, atau melewati `family_expires_at`), `401 REFRESH_TOKEN_REUSED` (family dicabut), `429 RATE_LIMITED`.
- **Idempotency:** n.a. Dua refresh paralel dengan token yang sama dalam grace window 10 detik → keduanya sukses (`replaced_by`, Step 4 `01`). Di luar window → reuse detection.
- **Rate limit:** `auth-strict`.

### A3 · POST `/auth/logout`
- **Authentication:** cookie. **Response `204`**; refresh token dicabut (`revoke_reason = 'LOGOUT'`), cookie dihapus. Logout dengan token yang sudah dicabut → tetap `204` (natural idempotent). **Errors:** `429`. **Rate:** `auth-ip`.

### A4 · GET `/me`
- **Authentication:** Bearer (dengan/tanpa `tid`). **Response `200`:** `{ "id", "email", "display_name", "active_tenant": {…} | null, "employee_id": "…" | null }`. `employee_id` diisi kalau user tertaut ke employee di tenant aktif. **Errors:** `401 UNAUTHENTICATED`, `401 TOKEN_EXPIRED`. **Rate:** `read`.

### A5 · GET `/me/memberships`
- **Authentication:** Bearer. **Authorization:** hanya membership milik user (policy `app.user_id`, `WithPlatformTx`).
- **Response `200`:** `{ "data": [ { "tenant_id", "tenant_name", "roles": [...], "status": "ACTIVE" } ] }`. Tanpa pagination: jumlah tenant per user kecil. Respons dibatasi 100 entri; selebihnya ditolak saat membership dibuat.
- **Errors:** `401`. **Rate:** `read`.

### A6 · POST `/session/switch-tenant`
- **Use case:** PF-02 · **Authentication:** Bearer · **Authorization:** membership `ACTIVE` di tenant tujuan (dicek di DB lewat `WithPlatformTx`).
- **Request:** `{ "tenant_id": "0192…t2" }`
- **Response `200`:** `{ "access_token", "token_type", "expires_in", "active_tenant": { "id", "name", "roles" } }`. Refresh token tidak berubah, karena tidak terikat tenant.
- **Validation:** `tenant_id` UUID.
- **Errors:** `404 NOT_FOUND` (bukan anggota, membership `REVOKED`/`INVITED`, atau tenant tidak ada; ketiganya tidak dibedakan), `403 FORBIDDEN` (tenant `SUSPENDED`), `401`.
- **Idempotency:** natural (memanggil ulang menerbitkan token setara). **Rate:** `write`.
- **Catatan:** audit `session.tenant_switched`. Demo A: token dengan `tid` hasil manipulasi gagal di verifikasi signature (`401 UNAUTHENTICATED`), bukan di sini.

---

## 4. Tenancy

### T1 · POST `/tenants/signup`
- **Use case:** PF-01 · **Authentication:** tidak ada · **Authorization:** —
- **Request:**
  ```json
  {
    "company_name": "PT Alpha Distribusi",
    "slug": "alpha-distribusi",
    "owner": { "email": "dewi@alpha.co.id", "password": "••••••••••", "display_name": "Dewi" },
    "timezone": "Asia/Jakarta"
  }
  ```
  Kalau email sudah punya akun, password wajib cocok (kredensial diverifikasi). Tenant baru menjadi tambahan membership user tersebut.
- **Response `201`:** `{ "tenant": { "id", "name", "slug" }, "access_token", "expires_in", "active_tenant": {…} }` + cookie refresh.
- **Validation:** `company_name` 2–120; `slug` `^[a-z0-9](-?[a-z0-9]){2,39}$`; password ≥ 10 karakter (aturan lengkap di Step 6); timezone harus nama IANA valid.
- **Errors:** `400 VALIDATION_ERROR`, `409 DUPLICATE_CODE` (`field: "slug"`), `401 INVALID_CREDENTIALS` (email sudah terdaftar dengan password berbeda), `422 IDEMPOTENCY_KEY_REUSED`, `409 REQUEST_IN_PROGRESS`, `429`.
- **Idempotency:** **required**, disimpan di `platform_idempotency_keys` (Step 4 `01`), di-scope per email.
- **Rate limit:** `auth-strict`.
- **Catatan:** satu transaksi: `users` (jika baru) + `tenants` + `tenant_settings` + membership `OWNER` + seed default (jenis cuti, komponen sistem, kalender; Step 4 `07` §6). Audit `tenant.created`.

### T2 · GET `/tenant`
- **Authorization:** semua role di tenant aktif. **Response `200`:** `{ "id", "name", "slug", "status", "settings": { "timezone", "work_days", "proration_basis", "cutoff_day_of_next_month", "cutoff_time_local", "overtime_basis", "overtime_max_hours_per_day", "incomplete_attendance_policy", "ruleset_code", "net_change_warning_pct" }, "version": 3 }` + `ETag: "3"`. **Errors:** `401`, `403 FORBIDDEN` (tanpa `tid`). **Rate:** `read`.

### T3 · PATCH `/tenant/settings`
- **Use case:** PF-01 · **Authorization:** `O`
- **Request:** subset field `settings` (contoh `{ "proration_basis": "CALENDAR_DAYS", "cutoff_day_of_next_month": 3 }`) + `If-Match: "3"`.
- **Response `200`:** `Tenant` + `ETag` baru.
- **Validation:** sesuai CHECK Step 4 §1.2 (`cutoff_day_of_next_month` 1–28, enum, `work_days` subset 1–7, timezone IANA).
- **Errors:** `400 VALIDATION_ERROR`, `403 FORBIDDEN` (bukan OWNER), `412 VERSION_CONFLICT`, `428 PRECONDITION_REQUIRED`.
- **Catatan:** perubahan **tidak** memengaruhi periode yang sudah ada, karena `cutoff_at` dibekukan di `payroll_periods`. Run yang sedang `CALCULATED` menjadi stale **jika** setting tersebut masuk fingerprint (proration basis, kebijakan incomplete). Audit `tenant.settings_changed` (diff).

### T4–T6 · `/holidays`
- **T4 GET** `?year=2026` → `{ "data": [ { "id", "date": "2026-03-19", "name": "Libur perusahaan" } ] }` (tanpa pagination; maksimal 366 baris/tahun).
- **T5 POST** `{ "date": "2026-03-20", "name": "Cuti bersama" }` → `201`. Errors: `400 VALIDATION_ERROR`, `409 DUPLICATE_CODE` (`field: "date"`), `409 PERIOD_LOCKED` (tanggal di periode `FINALIZED`).
- **T6 DELETE** → `204`. Errors: `404`, `409 PERIOD_LOCKED`.
- **Authorization:** T5/T6 `O`, `H`. **Catatan:** libur memengaruhi hari kerja, sehingga menulis ke periode `PROCESSING` membuat run stale. Audit untuk T5/T6.

---

## 5. Members & invitations

### U1 · GET `/members`
- **Authorization:** `O`, `H`. **Query:** `?status=ACTIVE|INVITED|REVOKED&role=MANAGER&limit&cursor`.
- **Response `200`:** list `{ "id", "user": { "id", "email", "display_name" } | null, "invited_email", "roles", "status", "accepted_at", "version" }`.
- **Pagination:** cursor (`created_at DESC, id`). **Errors:** `403 FORBIDDEN`, `400 INVALID_CURSOR`. **Rate:** `read`.

### U2 · POST `/invitations`
- **Use case:** PF-02 · **Authorization:** `O` (semua role), `H` (hanya role `MANAGER`/`EMPLOYEE`; lihat file 05 §3)
- **Request:** `{ "email": "tono@alpha.co.id", "roles": ["EMPLOYEE"], "employee_id": "0192…e7" }` (`employee_id` opsional: menautkan user ke employee saat accept)
- **Response `201`:** `{ "membership_id", "status": "INVITED", "invite_url": "https://…/invite#<tenant_id>.<secret>", "expires_at" }`. `invite_url` hanya ditampilkan sekali; di MVP tanpa email (PF-02 AC2).
- **Validation:** email valid; `roles` non-empty subset enum; `employee_id` milik tenant (composite FK) dan belum tertaut ke user lain.
- **Errors:** `400 VALIDATION_ERROR`, `403 FORBIDDEN` (HR mengundang `OWNER`/`HR_ADMIN`/`PAYROLL_ADMIN`), `409 MEMBERSHIP_EXISTS` (user sudah `ACTIVE`, atau sudah ada undangan `INVITED` untuk email itu), `404 NOT_FOUND` (`employee_id`).
- **Idempotency:** natural, lewat partial unique undangan per email. **Rate:** `write`.
- **Catatan:** token undangan `<tenant_id>.<secret>`, disimpan ter-hash, berlaku 7 hari. Audit `membership.invited`.

### U3 · POST `/invitations/accept`
- **Authentication:** Bearer (user sudah login; user baru register dulu lewat alur signup-akun yang sama dengan login form) · **Authorization:** email user = `invited_email`.
- **Request:** `{ "token": "<tenant_id>.<secret>" }`
- **Response `200`:** `{ "membership": { "tenant_id", "roles", "status": "ACTIVE" } }`. Client lalu memanggil switch-tenant.
- **Errors:** `410 INVITATION_INVALID` (kedaluwarsa, sudah dipakai, token salah, **atau** email tidak cocok; semuanya satu code), `409 MEMBERSHIP_EXISTS`, `401`.
- **Idempotency:** natural (`UPDATE … WHERE status = 'INVITED'`). Accept kedua → `410 INVITATION_INVALID`. **Rate:** `auth-ip`.
- **Catatan:** `tenant_id` pada token hanya dipakai untuk mencari baris undangan lewat jalur platform. Otorisasi tetap berasal dari hash secret + email. Audit `membership.accepted`.

### U4 · PATCH `/members/{id}/roles`
- **Authorization:** `O`; `H` hanya untuk menambah/menghapus `MANAGER`/`EMPLOYEE` pada member yang tidak memegang role admin.
- **Request:** `{ "roles": ["MANAGER","EMPLOYEE"] }` + `If-Match`.
- **Response `200`:** `Membership` + `ETag`.
- **Errors:** `400 VALIDATION_ERROR`, `403 FORBIDDEN`, `404 NOT_FOUND`, `409 LAST_OWNER` (menghapus `OWNER` terakhir), `412 VERSION_CONFLICT`, `428`.
- **Catatan:** role baru berlaku di access token berikutnya (refresh ≤ 15 menit). Operasi sensitif sudah memeriksa ulang membership di DB (ADR-P02). Trigger deferred PI-16 adalah lapisan terakhir. Audit `membership.roles_changed`.

### U5 · POST `/members/{id}/revoke`
- **Authorization:** `O`; `H` hanya untuk member non-admin. **Request:** `{ "reason": "Resign" }`. **Response `200`:** `Membership` (`REVOKED`).
- **Errors:** `403`, `404`, `409 LAST_OWNER`, `409 INVALID_STATE_TRANSITION` (sudah `REVOKED`). **Idempotency:** natural. **Rate:** `write`.
- **Catatan:** semua refresh token user untuk sesi aktif **tidak** dicabut (refresh tidak terikat tenant). Refresh berikutnya dengan `X-Active-Tenant` tenant ini akan menghasilkan token tanpa `tid` (A2). Self-revoke diizinkan kecuali untuk owner terakhir. Audit `membership.revoked`.

---

## 6. Organisasi

### G1 · GET `/departments` · `/positions`
- **Authorization:** semua role. **Query:** `?status=ACTIVE|INACTIVE` (default `ACTIVE`). **Response `200`:** `{ "data": [ { "id", "code", "name", "status", "version" } ] }`. Tanpa pagination (master data kecil; dibatasi 500 per tenant). **Rate:** `read`.

### G2 · POST `/departments` · `/positions`
- **Authorization:** `O`, `H`. **Request:** `{ "code": "WH", "name": "Gudang" }`. **Response `201`** + `ETag`.
- **Validation:** `code` `^[A-Z0-9_]{1,20}$`; `name` 1–100.
- **Errors:** `400 VALIDATION_ERROR`, `409 DUPLICATE_CODE` (`field: "code"`), `403`. **Idempotency:** natural (kode unik). **Rate:** `write`.

### G3 · PATCH `/departments/{id}` · `/positions/{id}`
- **Request:** `{ "name": "Gudang Utama" }` + `If-Match`. `code` immutable. **Errors:** `400`, `403`, `404`, `412`, `428`.

### G4 · POST `…/{id}/deactivate`
- **Response `200`** (`INACTIVE`). Masih dipakai di `org_assignments` yang berlaku hari ini → `409 INVALID_STATE_TRANSITION` (`detail` menyebut jumlah employee). **Idempotency:** natural. Audit.

---

## 7. Employees

### W1 · GET `/employees`
- **Use case:** PF-06 · **Authorization:** `O`, `H`, `P` (seluruh tenant); `M` (hanya bawahan langsung pada hari ini); `E` → `403 FORBIDDEN`
- **Query:** `?status=ACTIVE|RESIGNED|TERMINATED&department_id=…&q=ton&limit=20&cursor=…`
- **Response `200`:**
  ```json
  {
    "data": [
      { "id": "0192…e7", "employee_no": "EMP-0037", "full_name": "Tono Saputra",
        "status": "ACTIVE", "department": { "id": "…", "name": "Gudang" },
        "position": { "id": "…", "name": "Staf Gudang" }, "hire_date": "2024-02-01" }
    ],
    "page": { "next_cursor": "eyJr…", "has_more": true }
  }
  ```
- **Validation:** `q` 2–50 karakter, **prefix** match pada `lower(full_name)` atau `employee_no` (Step 4 `07` §4: tanpa `pg_trgm` di V1); filter tidak dikenal → `400`.
- **Errors:** `400 VALIDATION_ERROR`, `400 INVALID_CURSOR`, `403 FORBIDDEN`.
- **Pagination:** cursor (`lower(full_name), id` ASC jika `q` diisi; selain itu `employee_no, id`). **Rate:** `read`.
- **Catatan:** list **tidak** memuat PII apa pun, termasuk yang ter-mask.

### W2 · POST `/employees`
- **Use case:** PF-06 · **Authorization:** `O`, `H`
- **Request:**
  ```json
  {
    "employee_no": "EMP-0101", "full_name": "Rudi Hartono", "work_email": "rudi@alpha.co.id",
    "hire_date": "2026-03-16", "ptkp_status": "K/1",
    "nik": "3174012304950001", "npwp": "012345678901234",
    "bank": { "name": "BCA", "account_number": "1234567890", "holder": "Rudi Hartono" },
    "org": { "department_id": "…", "position_id": "…", "manager_employee_id": "0192…m1" },
    "salary": { "base_salary": "6000000.00" }
  }
  ```
- **Response `201`:** `Employee` (PII ter-mask) + `ETag: "1"`.
- **Validation:** `employee_no` `^[A-Za-z0-9-]{1,30}$`; `nik` 16 digit; `npwp` 15–16 digit; `account_number` 6–20 digit; `hire_date` ≤ hari ini + 90 hari; `base_salary` decimal string ≥ 0, maksimal 2 desimal; `ptkp_status` enum; referensi org milik tenant dan `ACTIVE`.
- **Errors:** `400 VALIDATION_ERROR`, `403`, `404 NOT_FOUND` (department/position/manager), `409 EMPLOYEE_NO_TAKEN`, `409 NIK_TAKEN`, `409 PERIOD_LOCKED` (`hire_date` di periode `FINALIZED`; pakai adjustment), `422 IDEMPOTENCY_KEY_REUSED`, `409 REQUEST_IN_PROGRESS`.
- **Idempotency:** **required**. **Rate:** `write`.
- **Catatan:** satu transaksi: `employees` (PII dienkripsi AES-GCM + blind index, ADR-P08) + `employment_spans [hire_date, ∞)` + `org_assignments` + `salary_assignments`. Audit `employee.created` (diff tanpa nilai PII mentah).

### W3 · GET `/employees/{id}`
- **Authorization:** `O`, `H`, `P`; `M` (bawahan langsung); `E` (self). Selain itu `404 NOT_FOUND`.
- **Response `200`:** `Employee` (§16) + `ETag`. `M` dan `E` menerima versi tanpa field bank (hanya nama, posisi, departemen, status).
- **Errors:** `404`, `401`. **Rate:** `read`.

### W4 · PATCH `/employees/{id}`
- **Authorization:** `O`, `H`. **Request:** subset `{ "full_name", "work_email", "ptkp_status", "nik", "npwp", "bank": {…} }` + `If-Match`. `employee_no`, `hire_date`, dan status **tidak** bisa diubah di sini.
- **Errors:** `400`, `403`, `404`, `409 NIK_TAKEN`, `412 VERSION_CONFLICT`, `428`.
- **Catatan:** `ptkp_status` masuk fingerprint, sehingga mengubahnya saat run `CALCULATED` membuat run stale (PFS-03). Field PII baru dienkripsi ulang dengan kunci aktif. Audit `employee.updated` (field yang berubah, tanpa nilai PII).

### W5 · POST `/employees/{id}/terminate`
- **Use case:** PF-06 · **Authorization:** `O`, `H`
- **Request:** `{ "type": "RESIGNED", "end_date": "2026-03-20", "reason": "Pindah kota" }` + `If-Match`
- **Response `200`:** `Employee` (`status: RESIGNED`, `end_date`).
- **Validation:** `type` ∈ `RESIGNED`, `TERMINATED`; `end_date` ≥ `hire_date`; `end_date` adalah hari kerja **terakhir** (inklusif; disimpan sebagai batas atas span `end_date + 1`).
- **Errors:** `400`, `403`, `404`, `409 INVALID_STATE_TRANSITION` (sudah bukan `ACTIVE`), `409 PERIOD_LOCKED` (`end_date` di periode final), `412`, `428`.
- **Idempotency:** natural (guard status `ACTIVE`). **Rate:** `write`.
- **Catatan:** satu transaksi: tutup `employment_spans` + status + tutup assignment efektif. Employee tetap masuk payroll periode terakhir secara prorata (BR5). Rehire = span baru, P2 (belum ada endpoint di V1). Audit.

### W6 · GET `/employees/{id}/pii`
- **Use case:** PF-20 · **Authorization:** `O`, `H`; `E` (self). `P` dan `M` → `403 FORBIDDEN` (lihat file 05 §3).
- **Response `200`:** `{ "nik": "3174012304950001", "npwp": "…", "bank": { "name", "account_number", "holder" } }` + `Cache-Control: no-store`.
- **Errors:** `403`, `404`. **Rate:** `read` (ditambah limit khusus 30/jam per user; dicatat di Step 6).
- **Catatan:** **setiap** panggilan menulis `audit_log` `employee.pii_viewed` (actor, target, field yang dibaca) di transaksi yang sama dengan dekripsi. Kalau audit gagal ditulis, respons gagal (`503`), bukan PII terkirim tanpa jejak.

### W7 · POST `/employees/{id}/org-assignments`
- **Use case:** PF-05 · **Authorization:** `O`, `H`
- **Request:** `{ "valid_from": "2026-04-01", "department_id": "…", "position_id": "…", "manager_employee_id": "…" | null }`
- **Response `201`:** `OrgAssignment { id, valid_from, valid_to: null, department, position, manager }`.
- **Validation:** `valid_from` ≥ `hire_date`; referensi milik tenant dan `ACTIVE`; manager ≠ diri sendiri.
- **Errors:** `400`, `403`, `404`, `409 OVERLAPPING_PERIOD` (bentrok dengan assignment masa depan yang sudah ada), `409 PERIOD_LOCKED`, `422 MANAGER_CYCLE`, `422 IDEMPOTENCY_KEY_REUSED`.
- **Idempotency:** **required**. **Rate:** `write`.
- **Catatan:** pola effective-dated (ADR-P04): tutup record yang berlaku di `valid_from`, lalu insert record baru. Approver request yang sudah `PENDING` **tidak** berpindah (snapshot, ADR-P09). Audit.

### W8 · GET `/employees/{id}/salary-history`
- **Use case:** PF-07 · **Authorization:** `O`, `H`, `P`; `E` (self). `M` → `404`.
- **Query:** `?as_of=2026-03-15` (opsional: hanya record yang berlaku di tanggal itu).
- **Response `200`:**
  ```json
  {
    "salary": [
      { "id": "…", "base_salary": "6000000.00", "valid_from": "2026-01-01", "valid_to": "2026-03-15", "change_reason": null },
      { "id": "…", "base_salary": "6600000.00", "valid_from": "2026-03-15", "valid_to": null, "change_reason": "Kenaikan tahunan" }
    ],
    "components": [
      { "id": "…", "component": { "code": "TUNJ_JABATAN", "name": "Tunjangan jabatan", "calc_type": "FIXED" },
        "amount": "500000.00", "rate": null, "valid_from": "2026-01-01", "valid_to": null }
    ]
  }
  ```
  `valid_to` eksklusif (half-open).
- **Errors:** `404`, `400 VALIDATION_ERROR` (`as_of`). **Pagination:** n.a. (riwayat per employee kecil). **Rate:** `read`.

### W9 · POST `/employees/{id}/salary-changes`
- **Use case:** PF-07 · **Authorization:** `O`, `H` (**bukan** `P`, karena pemisahan tugas; file 05 §3)
- **Request:** `{ "base_salary": "6600000.00", "valid_from": "2026-03-15", "change_reason": "Kenaikan tahunan" }`
- **Response `201`:** `SalaryAssignment`.
- **Validation:** `base_salary` ≥ 0, 2 desimal; `valid_from` ≥ `hire_date` dan ≤ hari ini + 366 hari.
- **Errors:** `400`, `403`, `404`, `409 OVERLAPPING_PERIOD` (bentrok dengan perubahan masa depan; batalkan dulu), `409 PERIOD_LOCKED` (`valid_from` jatuh di periode final; pakai adjustment, PF-07 AC5), `422 IDEMPOTENCY_KEY_REUSED`, `409 REQUEST_IN_PROGRESS`.
- **Idempotency:** **required**. **Rate:** `write`.
- **Catatan:** satu transaksi: `UPDATE` record berlaku (tutup di `valid_from`) + `INSERT` record baru. PeriodGuard hanya memeriksa rentang yang nilainya berubah (`[valid_from, ∞)`), Step 3 PI-14. Dua HR bersamaan → satu kena `409 OVERLAPPING_PERIOD` (EXCLUDE). Run `CALCULATED` di periode yang tersentuh menjadi stale (PFS-03). Audit `salary.changed`.

### W10 · POST `/employees/{id}/component-assignments`
- **Use case:** PF-07 · **Authorization:** `O`, `H`
- **Request:** `{ "component_id": "…", "amount": "500000.00", "rate": null, "valid_from": "2026-04-01", "valid_to": null }`
- **Response `201`:** `EmployeeComponent`.
- **Validation:** komponen `ACTIVE`, bukan `is_system` (komponen sistem tidak ditugaskan manual); `amount` hanya untuk `FIXED`/`PER_DAY`, `rate` hanya untuk `PERCENT_OF` non-statutory; `valid_to` > `valid_from` jika ada.
- **Errors:** `400`, `403`, `404`, `409 OVERLAPPING_PERIOD`, `409 PERIOD_LOCKED`, `422 IDEMPOTENCY_KEY_REUSED`.
- **Idempotency:** **required**. **Catatan:** untuk mengakhiri penugasan, kirim assignment yang sama dengan `valid_to` (menutup record yang berlaku). Audit.

---

## 8. Komponen gaji

### C1 · GET `/component-definitions`
- **Authorization:** `O`, `H`, `P`. **Query:** `?status=ACTIVE|INACTIVE&category=EARNING|DEDUCTION`. **Response `200`:** `{ "data": [ComponentDefinition] }` (tanpa pagination; dibatasi 200 per tenant). **Rate:** `read`.

### C2 · POST `/component-definitions`
- **Use case:** PF-07 · **Authorization:** `O`, `H`
- **Request:**
  ```json
  { "code": "TRANSPORT", "name": "Uang transport", "category": "EARNING", "calc_type": "PER_DAY",
    "default_amount": "20000.00", "day_source": "PRESENT", "taxable": true, "bpjs_base": false,
    "rounding": "HALF_UP" }
  ```
- **Response `201`:** `ComponentDefinition` + `ETag`.
- **Validation:** `code` `^[A-Z][A-Z0-9_]{1,40}$`; parameter sesuai `calc_type` (CHECK Step 4 §5.2: `PER_HOUR_MULTIPLIER` dan stage selain `EARNING` hanya untuk komponen sistem); `cap_min ≤ cap_max`; `rate` 0–1.
- **Errors:** `400 VALIDATION_ERROR`, `409 DUPLICATE_CODE` (`field: "code"`), `403`, `422 INVALID_COMPONENT_REFERENCE` (`base_ref` tidak ada/tidak aktif/stage lebih akhir), `422 COMPONENT_CYCLE` (+ `cycle[]`).
- **Idempotency:** natural (kode unik). **Rate:** `write`.

### C3 · PATCH `/component-definitions/{id}`
- **Request:** subset `{ "name", "default_amount", "rate", "base_ref", "cap_min", "cap_max", "taxable", "bpjs_base", "rounding" }` + `If-Match`. `code`, `category`, dan `calc_type` immutable. Komponen `is_system`: hanya `name` yang boleh diubah.
- **Errors:** `400`, `403` (mengubah field terlarang pada komponen sistem), `404`, `412`, `428`, `422 INVALID_COMPONENT_REFERENCE`, `422 COMPONENT_CYCLE`.
- **Catatan:** definisi tidak effective-dated. Perubahan `default_amount`/`rate` berlaku untuk kalkulasi **berikutnya**, sehingga run `CALCULATED` menjadi stale (definisi masuk input fingerprint). Run final tidak terpengaruh karena memakai snapshot. Audit (diff).

### C4 · POST `/component-definitions/{id}/deactivate`
- **Response `200`.** Komponen sistem → `403 FORBIDDEN`. Masih menjadi `base_ref` komponen aktif lain → `422 INVALID_COMPONENT_REFERENCE`. Assignment aktif tetap ada tetapi diabaikan engine (warning `INACTIVE_COMPONENT`). **Idempotency:** natural.

---

## 9. Attendance

### D1 · POST `/attendance/clock-in`
- **Use case:** PF-08 · **Authorization:** user dengan `employee_id` tertaut di tenant aktif (role `E`)
- **Request:** tanpa body (waktu = jam server; tanggal kerja = tanggal lokal tenant).
- **Response `201`** (record baru) atau **`200`** (sudah clock-in hari ini; mengembalikan record yang ada): `AttendanceRecord`.
- **Errors:** `422 EMPLOYEE_NOT_ACTIVE` (user tanpa employee, atau employee tidak aktif hari ini), `409 PERIOD_LOCKED` (hampir mustahil, hanya jika periode berjalan sudah final), `401`, `403`.
- **Idempotency:** natural, lewat `uq_attendance_records_day` (`INSERT … ON CONFLICT DO NOTHING RETURNING`, lalu `SELECT`). **Rate:** `write`.

### D2 · POST `/attendance/clock-out`
- **Response `200`:** `AttendanceRecord` (`COMPLETE`). Clock-out kedua → `200` dengan record yang sama (tidak menimpa jam pertama).
- **Errors:** `422 CLOCK_OUT_WITHOUT_CLOCK_IN`, `422 EMPLOYEE_NOT_ACTIVE`. **Catatan:** shift lewat tengah malam ditangani dengan mencari record `INCOMPLETE` terbaru ≤ 24 jam ke belakang (Step 4 §6.1).

### D3 · GET `/attendance`
- **Authorization:** `O`, `H`, `P` (semua); `M` (bawahan); `E` (self, `employee_id` diabaikan dan dipaksa = self).
- **Query:** `?employee_id=…&period=2026-03` **atau** `?employee_id=…&from=2026-03-01&to=2026-03-31` (maksimal 62 hari); HR boleh tanpa `employee_id` dengan `?date=2026-03-16` (rekap harian).
- **Response `200`:** list `AttendanceRecord` + `summary` per employee jika `period` dipakai (`present_days`, `incomplete_days`, `working_days`).
- **Pagination:** cursor (`work_date, id`). **Errors:** `400`, `403`, `404` (`employee_id` di luar scope). **Rate:** `read`.

### D4 · PATCH `/attendance/{id}`
- **Use case:** PF-08 AC3 · **Authorization:** `O`, `H`
- **Request:** `{ "clock_in_at": "2026-03-16T01:02:00Z", "clock_out_at": "2026-03-16T10:05:00Z", "correction_reason": "Lupa clock out" }` + `If-Match`
- **Response `200`:** `AttendanceRecord` (`source: CORRECTION`).
- **Validation:** `clock_out_at` > `clock_in_at`; `correction_reason` wajib (1–500).
- **Errors:** `400`, `403`, `404`, `409 PERIOD_LOCKED`, `412`, `428`.
- **Catatan:** koreksi untuk hari yang belum punya record memakai import (D5) atau clock oleh employee. V1 tidak menyediakan "create manual" terpisah, untuk menjaga jumlah endpoint. Run `CALCULATED` menjadi stale. Audit (before/after).

### D5 · POST `/attendance-imports`
- **Use case:** PF-21 · **Authorization:** `O`, `H`
- **Request:** `multipart/form-data`: `file` (text/csv, ≤ 5 MB), `mode` = `PARTIAL` | `ALL_OR_NOTHING`. Kolom CSV: `employee_no,work_date,clock_in_local,clock_out_local`.
- **Response `202`:** `AttendanceImport { id, status: "UPLOADED", mode, original_filename, size_bytes, created_at }` + `Location: /api/v1/attendance-imports/{id}`.
- **Validation (sinkron):** Content-Type multipart; file part ada; ukuran 1 B–5 MB; header CSV sesuai template; encoding UTF-8. Validasi per baris dilakukan async.
- **Errors:** `400 VALIDATION_ERROR`, `413 PAYLOAD_TOO_LARGE`, `415 UNSUPPORTED_MEDIA_TYPE`, `409 IMPORT_DUPLICATE` (+ `existing_resource_id`), `422 IDEMPOTENCY_KEY_REUSED`, `429 RATE_LIMITED`.
- **Idempotency:** **required** (`Idempotency-Key`) **dan** natural (`file_hash`).
- **Rate limit:** `heavy` (10/jam per tenant).
- **Catatan:** file disimpan ke `tenants/{tenant_id}/imports/{id}.csv` (ADR-012), lalu row `attendance_imports` + job `attendance.import` dalam satu transaksi. Upload ke object storage terjadi **sebelum** transaksi; objek yatim dibersihkan job retensi. Baris untuk periode final → error baris `PERIOD_LOCKED`. Upsert per `(employee, work_date)` membuat retry job aman.

### D6 · GET `/attendance-imports/{id}`
- **Response `200`:** `{ "id", "status": "COMPLETED", "mode", "rows_total": 412, "rows_ok": 405, "rows_failed": 7, "failure_reason": null, "started_at", "completed_at" }`. Client melakukan polling. **Errors:** `404`.

### D7 · GET `/attendance-imports/{id}/errors`
- **Response `200`:** list `{ "row_number": 37, "employee_no": "EMP-9999", "error_code": "EMPLOYEE_NOT_FOUND", "message": "…" }` (`error_code` baris adalah kode validasi import, bukan HTTP error code). **Pagination:** cursor (`row_number`). **Errors:** `404`.

---

## 10. Leave & overtime

### L1 · GET `/leave-types`
- **Response `200`:** `{ "data": [ { "id", "code": "ANNUAL", "name", "is_paid": true, "annual_quota_days": "12.0", "max_negative_days": "0.0", "status" } ] }`. Pengelolaan jenis cuti di V1 hanya lewat seed (P2 untuk CRUD).

### L2 · GET `/me/leave-balances`
- **Authorization:** `E` (self). **Query:** `?year=2026`. **Response `200`:** `{ "data": [ { "leave_type": {…}, "year": 2026, "balance_days": "9.0", "pending_days": "2.0" } ] }`. `pending_days` informatif (request `PENDING`), tidak mengurangi saldo (PF-09 AC3).

### L3 · POST `/leave-requests`
- **Use case:** PF-09 · **Authorization:** `E` (untuk dirinya sendiri; tidak ada field `employee_id`)
- **Request:** `{ "leave_type_id": "…", "start_date": "2026-03-25", "end_date": "2026-03-25", "reason": "Urusan keluarga" }` (`end_date` inklusif)
- **Response `201`:**
  ```json
  { "id": "…", "status": "PENDING", "leave_type": { "code": "UNPAID", "is_paid": false },
    "start_date": "2026-03-25", "end_date": "2026-03-25", "days": "1.0",
    "approval": { "id": "0192…ap", "approver": { "id": "…", "display_name": "Arif" }, "status": "PENDING" } }
  ```
- **Validation:** `start_date` ≤ `end_date`; rentang ≤ 60 hari; `days` (hari kerja dalam rentang) > 0; jenis cuti `ACTIVE`.
- **Errors:** `400`, `422 EMPLOYEE_NOT_ACTIVE`, `409 OVERLAPPING_LEAVE`, `409 PERIOD_LOCKED`, `422 IDEMPOTENCY_KEY_REUSED`, `409 REQUEST_IN_PROGRESS`.
- **Idempotency:** **required**. **Rate:** `write`.
- **Catatan:** satu transaksi: `leave_requests` + `approval_requests` (approver di-snapshot: atasan langsung pada tanggal submit; fallback `HR_ADMIN`, ADR-P09). Saldo **belum** dikurangi. Audit + outbox `leave.requested`.

### L4 · POST `/leave-requests/{id}/cancel`
- **Authorization:** requester (self), hanya saat `PENDING`. **Response `200`** (`CANCELLED`; approval ikut `CANCELLED`).
- **Errors:** `404`, `409 INVALID_STATE_TRANSITION` (sudah diputuskan; cuti `APPROVED` dibatalkan lewat request pembatalan, yang di V1 dilakukan HR lewat endpoint yang sama dengan `cancels_request_id`, P1). **Idempotency:** natural.

### L5 · GET `/leave-requests`
- **Authorization:** `O`, `H` (semua); `M` (bawahan); `E` (self). **Query:** `?status=…&employee_id=…&from&to`. **Pagination:** cursor (`created_at DESC`). **Errors:** `400`, `404`.

### V1 · POST `/overtime-requests`
- **Use case:** PF-10 · **Authorization:** `E` (self)
- **Request:** `{ "work_date": "2026-03-23", "start_at": "2026-03-23T10:00:00Z", "end_at": "2026-03-23T14:00:00Z", "reason": "Stock opname" }`
- **Response `201`:** `OvertimeRequest { id, status: "PENDING", hours: "4.00", approval: {…}, warnings: ["OVERTIME_OVER_LIMIT"]? }`. Melebihi `overtime_max_hours_per_day` hanya menghasilkan warning, bukan blokir (PF-10 AC3).
- **Validation:** `end_at` > `start_at`; durasi ≤ 12 jam; `work_date` = tanggal lokal `start_at`; `reason` 1–500.
- **Errors:** `400`, `422 EMPLOYEE_NOT_ACTIVE`, `409 OVERLAPPING_PERIOD` (bentrok dengan lembur lain), `409 PERIOD_LOCKED`, `422 IDEMPOTENCY_KEY_REUSED`.
- **Idempotency:** **required**. **Rate:** `write`.

### V2 · POST `/overtime-requests/{id}/cancel` — sama dengan L4.
### V3 · GET `/overtime-requests` — sama dengan L5, ditambah `P` boleh melihat semua (review input payroll).

---

## 11. Approvals

### R1 · GET `/approvals/inbox`
- **Use case:** PF-11 · **Authorization:** setiap user di tenant; hanya mengembalikan approval dengan `approver_id = user`. `H` dengan `?scope=all` melihat semua `PENDING` untuk keperluan override.
- **Query:** `?status=PENDING` (default) `&subject_type=LEAVE_REQUEST|OVERTIME_REQUEST|PAYROLL_RUN&limit&cursor`
- **Response `200`:** list `ApprovalRequest` dengan ringkasan subjek (`subject.summary`: contoh "Cuti tidak dibayar 25 Mar 2026 · 1 hari · Tono").
- **Pagination:** cursor (`created_at DESC`; index `ix_approval_requests_inbox`). **Rate:** `read`.

### R2 · POST `/approvals/{id}/approve`
- **Use case:** PF-11, PF-13 (subject `PAYROLL_RUN`) · **Authorization:** `approver_id` yang di-snapshot. `H` boleh override **hanya** untuk `LEAVE_REQUEST`/`OVERTIME_REQUEST` (ter-audit, `is_override = true`). Selain itu `404 NOT_FOUND`.
- **Request:** `{ "note": "OK" }` (opsional)
- **Response `200`:** `ApprovalRequest` (`APPROVED`) + subjek terbaru (`LeaveRequest` / `OvertimeRequest` / `PayrollRun`).
- **Errors (umum):** `404 NOT_FOUND`, `409 APPROVAL_ALREADY_DECIDED` (+ `status`), `403 SELF_APPROVAL_FORBIDDEN` (lapisan defensif; CHECK DB juga menolak).
- **Errors per subject:**
  - `LEAVE_REQUEST`: `422 INSUFFICIENT_LEAVE_BALANCE` (+ `balance`; approval tetap `PENDING`), `409 PERIOD_LOCKED`.
  - `PAYROLL_RUN`: `409 RUN_STALE` (+ `employees_changed[]`; approval tetap `PENDING`, run tetap `PENDING_APPROVAL`; jika kandidat perubahan > 200, langsung `RUN_STALE` tanpa verifikasi per karyawan, Step 12: R1-7), `409 INVALID_STATE_TRANSITION` (run tidak `PENDING_APPROVAL`).
- **Idempotency:** natural (`UPDATE … WHERE status='PENDING' AND approver_id=$actor`). Approve kedua → `409 APPROVAL_ALREADY_DECIDED`. **Rate:** `write`.
- **Transaksi `PAYROLL_RUN`** (ADR-P07 + penyelesaian O3):
  ```text
  BEGIN
    SELECT … FROM payroll_periods WHERE id = $period FOR UPDATE     -- serialisasi dengan penulis input
    hitung ulang input fingerprint run (InputProvider, sama dengan saat kalkulasi)
    ≠ payroll_runs.input_fingerprint → ROLLBACK → 409 RUN_STALE {employees_changed}
    UPDATE approval_requests … WHERE status='PENDING'               -- 0 baris → 409
    UPDATE payroll_runs SET status='APPROVED' WHERE status='PENDING_APPROVAL'
    audit + outbox payroll_run.approved
  COMMIT
  ```
  Penulis input (melalui `PeriodGuard`) mengambil `FOR SHARE` pada baris periode yang sama, sehingga tidak ada perubahan input yang lolos di antara hitung fingerprint dan commit. Biaya hitung ulang fingerprint sebanding dengan jumlah karyawan. Untuk tenant 10.000 karyawan, ini beberapa detik dan ditinjau di Step 9. Alternatif V2: penanda `input_changed_at` per periode.
- **Catatan PFS-08:** approve dan reject bersamaan → tepat satu yang menang (guard status), yang lain `409 APPROVAL_ALREADY_DECIDED`.

### R3 · POST `/approvals/{id}/reject`
- **Authorization:** sama dengan R2. **Request:** `{ "note": "Data lembur Maret belum lengkap" }` (**wajib**)
- **Response `200`:** `ApprovalRequest` (`REJECTED`). Subjek: cuti/lembur → `REJECTED`; run → kembali ke `CALCULATED` (ADR-P05; alasan tersimpan di approval).
- **Errors:** `422 DECISION_NOTE_REQUIRED`, `404`, `409 APPROVAL_ALREADY_DECIDED`.

---

## 12. Payroll

### Y1 · GET `/payroll-periods`
- **Authorization:** `O`, `H`, `P`. **Query:** `?year=2026`. **Response `200`:** `{ "data": [ { "id", "period": "2026-03", "status": "FINALIZED", "cutoff_at": "2026-04-02T16:59:00Z", "finalized_run_id", "finalized_at", "active_run": { "id", "status" } | null } ] }`. Tanpa pagination (≤ 12 per tahun).

### Y2 · POST `/payroll-runs`
- **Use case:** PF-13 · **Authorization:** `O`, `P`
- **Request:** `{ "period": "2026-03" }`
- **Response `201`:** `PayrollRun` (`status: DRAFT`, `calc_version: 0`).
- **Validation:** `period` `YYYY-MM`; tidak lebih dari 1 bulan ke depan dari bulan berjalan (zona waktu tenant); tidak sebelum `hire_date` employee pertama tenant.
- **Errors:**
  - `409 ACTIVE_RUN_EXISTS` (+ `existing_resource_id`): admin lain sudah membuat run untuk periode ini (`uq_payroll_runs_active_per_period`).
  - `409 PERIOD_LOCKED`: periode sudah `FINALIZED`.
  - `422 IDEMPOTENCY_KEY_REUSED`, `409 REQUEST_IN_PROGRESS`, `400 VALIDATION_ERROR`, `403`.
- **Idempotency:** **required**. Klik ganda oleh admin yang sama → replay `201` + `Idempotent-Replayed: true` (PFS-06).
- **Rate limit:** `write`.
- **Catatan:** satu transaksi: buat `payroll_periods` (jika belum ada; `cutoff_at` dibekukan dari `tenant_settings`) + run `DRAFT`. Periode → `PROCESSING`. Audit `payroll_run.created`.

### Y3 · GET `/payroll-runs`
- **Query:** `?period=2026-03&status=…&limit&cursor` (termasuk run `CANCELLED` untuk riwayat). **Pagination:** cursor (`created_at DESC`). **Authorization:** `O`, `H`, `P`.

### Y4 · GET `/payroll-runs/{id}`
- **Response `200`:**
  ```json
  {
    "id": "0192…r3", "period": "2026-03", "status": "CALCULATING", "calc_version": 2,
    "progress": { "chunks_done": 4, "chunks_total": 10, "eligible_count": 100 },
    "totals": null,
    "counts": { "ok": null, "error": null, "excluded": null, "warnings": null },
    "ruleset": null, "approval_request_id": null,
    "created_by": "…", "calc_started_at": "2026-04-03T02:10:00Z",
    "stale": false
  }
  ```
  Setelah `CALCULATED`: `totals: { "gross": "759965300.00", "deductions": "71104400.00", "net": "688860900.00" }`, `counts`, `ruleset: { "code": "ID-SIMPLIFIED", "version": "2026.1" }`. `stale` diisi `true` bila **terdeteksi murah** (contoh: ada audit perubahan input setelah `calculated_at`). Ini hanya petunjuk UI. Penentu otoritatifnya tetap guard di R2.
- **Errors:** `404`. **Rate:** `read` (UI polling tiap 2 detik selama `CALCULATING`).

### Y5 · POST `/payroll-runs/{id}/calculate`
- **Use case:** PF-13 · **Authorization:** `O`, `P`
- **Request:** tanpa body.
- **Response `202`:** `PayrollRun` (`status: CALCULATING`, `calc_version` naik) + `Location: /api/v1/payroll-runs/{id}`.
- **Errors:** `409 INVALID_STATE_TRANSITION` (status bukan `DRAFT`/`CALCULATED`/`FAILED`; termasuk saat sedang `CALCULATING`, karena recalc/cancel dari `CALCULATING` ditolak, Step 3 `08`), `404`, `422 IDEMPOTENCY_KEY_REUSED`, `409 REQUEST_IN_PROGRESS`, `429 RATE_LIMITED`.
- **Idempotency:** **required**. Replay mengembalikan `202` yang sama dan tidak menaikkan `calc_version` lagi.
- **Rate limit:** `heavy` (10/jam per tenant).
- **Catatan:** transaksi: `payroll_periods FOR UPDATE` → `UPDATE payroll_runs SET status='CALCULATING', calc_version = calc_version + 1, calc_started_at = clock_timestamp() WHERE status IN (...)` + job `payroll.plan` (outbox). Lock period inilah yang membuat deteksi stale berbasis change log benar (Step 7 O12). Worker memecah ke chunk job (default 50 karyawan; demo 10, O5). Chunk lama dengan `calc_version` usang menjadi no-op. Crash → resume (PFS-01). Semua chunk gagal permanen → run `FAILED` + `failure_reason` (PFS-05).

### Y6 · GET `/payroll-runs/{id}/items`
- **Authorization:** `O`, `P`; `H`. **Setiap request** menulis satu baris audit `payroll.bulk_read` (run_id, jumlah baris) dan memeriksa ulang membership di DB (operasi sensitif; Step 12: R2-6, menggantikan audit "sekali per hari")
- **Query:** `?status=OK|ERROR|EXCLUDED&has_warning=true&warning=OVERTIME_OVER_LIMIT&department_id=…&limit=50&cursor`
- **Response `200`:** list `PayrollItem` (tanpa lines) untuk `calc_version` terkini.
- **Pagination:** cursor (`employee_no, id`). `employee_no`, `employee_name`, dan `department_id` adalah kolom **denormalisasi di `payroll_items`** yang diisi saat kalkulasi, jadi filter departemen benar untuk run historis (Step 12: R3-4). **Errors:** `404`, `400`. **Rate:** `read`.

### Y7 · GET `/payroll-runs/{id}/items/{item_id}`
- **Response `200`:** `PayrollItem` + `lines[]` (berurutan sesuai pipeline, lengkap dengan `explanation`) + `issues[]`. Contoh line:
  ```json
  { "code": "OVERTIME", "category": "EARNING", "amount": "419653.00",
    "basis": { "hours": "6.00", "weighted_hours": "11.00", "hourly_rate": "38150.289017", "divisor": "173" },
    "explanation": "Lembur 6 jam (23 Mar 4j: 1×1,5 + 3×2; 26 Mar 2j: 1×1,5 + 1×2) = 11 jam berbobot × Rp38.150,29/jam",
    "rounding": "HALF_UP", "taxable": true }
  ```
- **Errors:** `404`. **Catatan:** item dari `calc_version` lama → `404` (tidak relevan lagi).

### Y8 · POST `/payroll-runs/{id}/items/{item_id}/exclude`
- **Use case:** PF-13 (item `ERROR` tidak bisa di-submit kecuali dikecualikan) · **Authorization:** `O`, `P`
- **Request:** `{ "reason": "Karyawan cuti di luar tanggungan; dibayar manual" }` (wajib, 10–500)
- **Response `200`:** `PayrollItem` (`EXCLUDED`) + total run yang diperbarui.
- **Errors:** `404`, `409 INVALID_STATE_TRANSITION` (run bukan `CALCULATED`, atau item bukan `ERROR`), `400 VALIDATION_ERROR` (alasan).
- **Idempotency:** natural (`WHERE status='ERROR'`). **Catatan:** V1 hanya mengizinkan exclude item `ERROR`; mengecualikan item `OK` di luar cakupan. Audit `payroll_item.excluded`. Item excluded tidak dibayar, tidak masuk total, dan tidak mendapat payslip.

### Y9 · GET `/payroll-runs/{id}/diff` (P1)
- **Use case:** PF-23 · **Authorization:** `O`, `P`
- **Query:** `?only_flagged=true&limit&cursor`
- **Response `200`:**
  ```json
  {
    "summary": { "total_net": "688860900.00", "previous_total_net": "671250000.00", "change_pct": "2.62",
                 "employees": 100, "new_employees": 2, "leavers": 1, "flagged": 3 },
    "data": [ { "employee_id": "…", "employee_no": "EMP-0037", "net": "6888609.00",
                "previous_net": "5612000.00", "change_pct": "22.75", "flagged": true } ],
    "page": { "next_cursor": null, "has_more": false }
  }
  ```
  Pembanding = run `FINALIZED` periode sebelumnya. Ambang flag = `tenant_settings.net_change_warning_pct`.
- **Errors:** `404`, `409 INVALID_STATE_TRANSITION` (run belum `CALCULATED`). **Catatan:** read model yang dihitung on-the-fly dari `payroll_items` (eventual tidak diperlukan pada skala V1).

### Y10 · POST `/payroll-runs/{id}/submit`
- **Use case:** PF-13 · **Authorization:** `O`, `P`
- **Request:**
  ```json
  { "approver_user_id": "0192…u9", "self_approval_reason": null }
  ```
  - `approver_user_id` opsional. Kalau kosong: dipilih otomatis bila tepat ada satu user lain dengan role `OWNER`/`PAYROLL_ADMIN`. Kalau ada beberapa → `400 VALIDATION_ERROR` (pilih salah satu).
  - `self_approval_reason` **hanya** untuk tenant yang tidak punya admin lain (ADR-P09). Approver = submitter, `self_approved = true`.
- **Response `200`:** `PayrollRun` (`PENDING_APPROVAL`, `approval_request_id`).
- **Errors:** `422 RUN_HAS_ERRORS` (+ `error_count`), `409 INVALID_STATE_TRANSITION` (bukan `CALCULATED`), `422 SELF_APPROVAL_REASON_REQUIRED` (tidak ada admin lain dan alasan kosong), `403 SELF_APPROVAL_FORBIDDEN` (mengirim alasan padahal ada admin lain, atau memilih diri sendiri sebagai approver), `404 NOT_FOUND` (`approver_user_id` bukan admin aktif di tenant), `400 VALIDATION_ERROR`.
- **Idempotency:** natural (guard `WHERE status='CALCULATED'`). **Rate:** `write`.
- **Catatan:** satu transaksi: run → `PENDING_APPROVAL` + `approval_requests` (`PAYROLL_RUN`). Fingerprint **tidak** dicek di sini; pemeriksaan otoritatif terjadi saat approve (R2) dan finalize (Y11). Audit + outbox `payroll_run.submitted`.

### Y11 · POST `/payroll-runs/{id}/finalize`
- **Use case:** PF-14 · **Authorization:** `O`, `P` (boleh oleh approver atau submitter; pemisahan tugas sudah terjadi di approve)
- **Request:** tanpa body.
- **Response `200`:** `PayrollRun` (`FINALIZED`, `finalized_at`) + `payslips: { "pending": 98 }`.
- **Errors:** `409 INVALID_STATE_TRANSITION` (bukan `APPROVED`), `409 RUN_STALE` (+ `employees_changed[]`; input berubah setelah approve, sehingga run harus dihitung ulang dan diajukan lagi), `404`, `422 IDEMPOTENCY_KEY_REUSED`, `409 REQUEST_IN_PROGRESS`.
- **Idempotency:** **required**. Finalize yang di-replay → `200` yang sama. **Rate limit:** `heavy` (5/jam per tenant).
- **Catatan:** satu transaksi (ADR-P07, Step 4 §7):
  ```text
  SELECT payroll_periods … FOR UPDATE → fingerprint guard
  UPDATE payroll_runs SET status='FINALIZED' WHERE status='APPROVED'   -- trigger totals check (PI-05)
  UPDATE payroll_periods SET status='FINALIZED', finalized_run_id      -- PeriodGuard aktif penuh
  UPDATE adjustments SET status='APPLIED' WHERE … AND status='PENDING_APPLY' (PI-15)
  INSERT payslips (PENDING) per item OK + job payslip.render (chunked)
  audit + outbox payroll_run.finalized
  ```
  Snapshot sudah ditulis saat kalkulasi dan sekarang dibekukan trigger (PI-04). Crash di tengah transaksi → rollback penuh; retry dengan key yang sama aman (PFS-04/PFS-10).

### Y12 · POST `/payroll-runs/{id}/cancel`
- **Authorization:** `O`, `P`. **Request:** `{ "reason": "Salah periode" }` (wajib). **Response `200`:** `PayrollRun` (`CANCELLED`).
- **Errors:** `409 INVALID_STATE_TRANSITION` (hanya dari `DRAFT`/`CALCULATED`/`FAILED`; `PENDING_APPROVAL` harus di-reject dulu; `CALCULATING` ditolak), `404`.
- **Catatan:** periode kembali `OPEN` jika tidak ada run aktif lain. Run `CANCELLED` tetap tersimpan untuk riwayat dan membebaskan slot unique, sehingga run baru untuk periode yang sama bisa dibuat.

---

## 13. Adjustments (P1)

### J1 · POST `/adjustments`
- **Use case:** PF-19 · **Authorization:** `O`, `H`, `P`
- **Request:**
  ```json
  { "employee_id": "0192…e7", "origin_period": "2026-03", "target_period": "2026-04",
    "component_code": "OVERTIME_CORRECTION", "amount": "76300.58",
    "taxable": true, "reason": "Lembur 26 Maret kurang 2 jam" }
  ```
  `amount` boleh negatif (potongan). Untuk koreksi lembur, client menghitung nominal dari tampilan bantu. V1 tidak menghitung ulang otomatis dari jam.
- **Response `201`:** `Adjustment` (`PENDING_APPLY`).
- **Validation:** `origin_period` harus `FINALIZED` (selain itu `400 VALIDATION_ERROR` field `origin_period`); `target_period` > `origin_period` dan belum `FINALIZED`; `amount` ≠ 0, 2 desimal, |amount| ≤ 1.000.000.000; `component_code` ∈ enum; employee aktif di periode target.
- **Errors:** `400`, `404` (employee), `409 PERIOD_LOCKED` (target sudah final), `422 EMPLOYEE_NOT_ACTIVE`, `422 IDEMPOTENCY_KEY_REUSED`.
- **Idempotency:** **required**. **Rate:** `write`.
- **Catatan:** kalau run target sudah `CALCULATED`, run tersebut menjadi stale (PF-19 AC4). Audit `adjustment.created`.

### J2 · GET `/adjustments`
- **Query:** `?employee_id&target_period&status&limit&cursor`. **Pagination:** cursor (`created_at DESC`).

### J3 · POST `/adjustments/{id}/cancel`
- **Request:** `{ "reason": "…" }`. **Response `200`** (`CANCELLED`). **Errors:** `409 INVALID_STATE_TRANSITION` (sudah `APPLIED`/`CANCELLED`), `404`. Membatalkan adjustment yang sudah masuk run `CALCULATED` membuat run tersebut stale.

---

## 14. Payslips

### S1 · GET `/me/payslips`
- **Authorization:** `E` (self). **Query:** `?year=2026`. **Response `200`:** list `{ "id", "period": "2026-03", "net": "6888609.00", "finalized_at", "pdf_status": "RENDERED" }` (run `FINALIZED` saja). **Pagination:** cursor (periode DESC).

### S2 · GET `/me/payslips/{id}`
- **Response `200`:** `Payslip` (§16): header (periode, employee, posisi), `lines[]` dengan `explanation`, total, rekening ter-mask. Dirender dari `payroll_lines`, sehingga tersedia walaupun PDF belum jadi (PF-15). **Errors:** `404` (bukan miliknya). Akses oleh pemilik tidak diaudit per akses.

### S3 · GET `/payslips`
- **Authorization:** `O`, `P`; `H` (ter-audit). **Query:** `?run_id=…` (wajib) `&employee_id&limit&cursor`. **Response:** list ringkas tanpa lines. **Catatan:** setiap request list menulis satu baris audit `payroll.bulk_read` (run_id, jumlah baris) + recheck membership (Step 12: R2-6); detail (S4) diaudit per slip.

### S4 · GET `/payslips/{id}`
- **Authorization:** `O`, `P`, `H`. Setiap akses menulis `audit_log` `payslip.viewed_by_admin` di transaksi yang sama (BR10, PI-17). **Response:** sama dengan S2. **Errors:** `404`.

### S5 · GET `/payslips/{id}/download` (P1)
- **Use case:** PF-22 · **Authorization:** pemilik slip, atau `O`/`P`/`H` (ter-audit untuk non-pemilik)
- **Response `200`:** `{ "url": "https://storage…/tenants/{tenant_id}/payslips/{run_id}/{employee_id}.pdf?X-Amz-Signature=…", "expires_at": "2026-04-03T03:05:00Z" }`. Presigned GET, TTL 5 menit (ADR-012).
- **Errors:** `404`, `409 PAYSLIP_NOT_READY` (PDF masih `PENDING`/`FAILED`; + `Retry-After`).
- **Catatan:** URL dibuat setelah otorisasi dan audit. URL tidak di-cache dan tidak pernah dimasukkan ke log.

---

## 15. Audit & health

### Z1 · GET `/audit-log`
- **Authorization:** `O`, `H`. **Query:** `?target_type=employee&target_id=…&actor_id=…&action=salary.changed&from&to&limit&cursor` (rentang ≤ 366 hari).
- **Response `200`:** list `{ "id", "actor": { "type": "USER", "id", "display_name" }, "action", "target": { "type", "id" }, "diff", "reason", "request_id", "created_at" }`. `diff` sudah ter-redaksi saat ditulis (tanpa nilai PII).
- **Pagination:** cursor (`created_at DESC`). **Errors:** `400`, `403`.

### Z2 · GET `/healthz` · GET `/readyz`
- **Authentication:** tidak ada. Hanya diekspos ke jaringan internal/LB (Caddy tidak meneruskan dari publik, Step 9).
- **`/healthz` → `200 {"status":"ok"}`** selama proses hidup.
- **`/readyz` → `200`** jika DB terjangkau (`SELECT 1` dengan timeout 1 detik) dan versi migrasi = versi yang diharapkan binary; selain itu **`503`** `{"status":"unavailable","checks":{"db":"fail"}}`. Object storage **tidak** masuk readiness, karena hanya fitur P1 yang bergantung padanya.

---

## 16. Response models

```text
Employee {
  id, employee_no, full_name, work_email, status, hire_date, end_date|null, ptkp_status,
  nik_masked: "************0001", npwp_masked: "***********1234",
  bank: { name, account_masked: "******7890", holder },
  current_org: { department{id,name}, position{id,name}, manager{id,full_name}|null, valid_from },
  user_id|null, version
}                                    // M/E menerima tanpa bank, nik, npwp, ptkp

SalaryAssignment   { id, employee_id, base_salary, valid_from, valid_to|null, change_reason, created_by, created_at }
EmployeeComponent  { id, employee_id, component{id,code,name,calc_type}, amount|null, rate|null, valid_from, valid_to|null }
ComponentDefinition{ id, code, name, category, stage, calc_type, default_amount|null, rate|null, ruleset_ref|null,
                     base_ref|null, day_source|null, cap_min|null, cap_max|null, taxable, bpjs_base, rounding,
                     is_system, status, version }
AttendanceRecord   { id, employee_id, work_date, clock_in_at|null, clock_out_at|null, status, source,
                     correction_reason|null, version }
LeaveRequest       { id, employee_id, leave_type{id,code,name,is_paid}, start_date, end_date, days, reason,
                     status, approved_at|null, approval{id,approver{id,display_name},status} }
OvertimeRequest    { id, employee_id, work_date, start_at, end_at, hours, reason, status, approved_at|null,
                     approval{…}, warnings[] }
ApprovalRequest    { id, subject_type, subject_id, subject{summary}, requester{id,display_name},
                     approver{id,display_name}, status, decision_note|null, decided_by|null, is_override,
                     self_approved, self_approval_reason|null, decided_at|null, created_at }
PayrollRun         { id, period, status, calc_version, progress{chunks_done,chunks_total,eligible_count},
                     totals{gross,deductions,net}|null, counts{ok,error,excluded,warnings},
                     ruleset{code,version}|null, approval_request_id|null, stale,
                     created_by, submitted_by|null, approved_by|null, finalized_by|null,
                     calc_started_at|null, calculated_at|null, finalized_at|null, failure_reason|null }
PayrollItem        { id, run_id, calc_version, employee{id,employee_no,full_name,department},
                     status, gross|null, deductions|null, net|null, error_codes[], warning_codes[],
                     excluded_reason|null, lines[]?, issues[]? }
PayrollLine        { sort_order, code, category, stage, amount, basis{}, explanation, taxable, rounding,
                     adjustment_id|null }
Payslip            { id, period, employee{employee_no,full_name,position,department}, bank_account_masked,
                     lines[PayrollLine], totals{gross,deductions,net}, ruleset{code,version},
                     pdf_status, finalized_at }
Adjustment         { id, employee_id, origin_period, target_period, component_code, amount, taxable, reason,
                     status, applied_run_id|null, applied_at|null, created_by, created_at }
AttendanceImport   { id, status, mode, original_filename, size_bytes, rows_total|null, rows_ok|null,
                     rows_failed|null, failure_reason|null, started_at|null, completed_at|null }
Problem            { type, title, status, code, detail, instance, request_id, errors[]?, …extensions }
```

Semua nilai uang berupa string decimal 2 digit. Rate berupa string decimal 6 digit. Tanggal `YYYY-MM-DD`, periode `YYYY-MM`.

---

## 17. Demo A–C sebagai urutan panggilan

**Demo A: tenant isolation**
1. Login sebagai Dewi (PT Alpha) → `A1`, lalu `A6 switch-tenant` ke Alpha.
2. `GET /employees/{id milik PT Beta}` → **`404 NOT_FOUND`** (RLS + scoping; Step 3 `05`).
3. Ubah claim `tid` di JWT secara manual → **`401 UNAUTHENTICATED`** (signature tidak valid).
4. `POST /employees` dengan `org.department_id` milik Beta → **`404 NOT_FOUND`** (composite FK `23503`).
5. Kirim `"tenant_id": "<Beta>"` di body → **`400 VALIDATION_ERROR`** (field tidak dikenal).
6. Tampilkan hasil `go test ./... -run TestTenantIsolation` dan matriks file 05 → hijau.

**Demo B: crash & resume, run ganda**
1. `POST /payroll-runs {period:"2026-03"}` dengan `Idempotency-Key: K1`, dua kali → `201` lalu `201 Idempotent-Replayed`.
2. Admin kedua `POST /payroll-runs` (key lain) → **`409 ACTIVE_RUN_EXISTS`** + `existing_resource_id`.
3. `POST /payroll-runs/{id}/calculate` → `202`. Polling `GET /payroll-runs/{id}`: `chunks_done 3/10`.
4. Toolbar demo membunuh worker saat chunk ke-4 (karyawan 31–40; chunk size 10) belum di-commit. Polling tetap `CALCULATING`.
5. Worker hidup lagi → `chunks_done` naik ke `10/10` → `CALCULATED`, `counts.ok = 100`, totals = Σ item.

**Demo C: immutability & koreksi**
1. `POST /payroll-runs/{id}/submit` → `POST /approvals/{approval_id}/approve` (approver lain) → `POST /payroll-runs/{id}/finalize`.
2. `PATCH /attendance/{id Maret}` → **`409 PERIOD_LOCKED`**.
3. `POST /employees/{id}/salary-changes {valid_from:"2026-03-10"}` → **`409 PERIOD_LOCKED`**.
4. `POST /adjustments {origin:"2026-03", target:"2026-04", component_code:"OVERTIME_CORRECTION", amount:"76300.58"}` → `201`.
5. Run April: `GET /payroll-runs/{april}/items/{tono}` → line `ADJUSTMENT` "Koreksi lembur periode Maret 2026".
6. (CLI) `payrun verify-run --run <maret>` → recompute dari snapshot = hasil tersimpan.

---

## 18. Catatan untuk katalog error

Dua code yang diusulkan saat menulis file ini sudah **diadopsi** ke [`01-error-catalogue.md`](01-error-catalogue.md): `409 DUPLICATE_CODE` (+ `field`) untuk duplikat kode/slug/tanggal master data, dan `409 PAYSLIP_NOT_READY` (+ `Retry-After`) untuk PDF slip yang belum siap. Endpoint di atas sudah diperbarui untuk memakai kedua code ini.

---

## Catatan implementasi (Sprint 1)

Kontrak yang berlaku ada di `api/openapi.yaml` (dicek `TestContract_S1ResponsesMatchSpec`). Perbedaan dan tambahan terhadap A1–A6, T1–T2:

| Endpoint | Desain | Implementasi S1 | Alasan |
|---|---|---|---|
| A1 login, `active_tenant` | Tenant terakhir yang dipakai, atau satu-satunya membership | Hanya "satu-satunya membership ACTIVE di tenant ACTIVE"; selain itu `null` | Tidak ada kolom untuk "tenant terakhir". SPA mengirim `X-Active-Tenant` saat refresh, yang sudah memulihkan tenant terakhir per perangkat |
| A1 akun terkunci | `401 INVALID_CREDENTIALS` + `Retry-After` (R2-5) | `401 INVALID_CREDENTIALS` **tanpa** `Retry-After` | Header itu hanya muncul untuk akun yang ada dan terkunci, sehingga sendirinya menjadi oracle enumerasi (temuan LedgerPay S4). Status lock ada di log/metrik `app_authn_lockouts_total` |
| A7 `POST /auth/verify-email` | — (R2-8 hanya menyebut "link sekali pakai") | Baru. `{token, password}` → `204`; token tidak dikenal/kedaluwarsa/terpakai → `410 EMAIL_VERIFICATION_INVALID`; password akun saat ini salah → `401 INVALID_CREDENTIALS` (menaikkan counter lockout, link tidak terpakai; review S1: pemilik inbox tidak bisa mengaktifkan akun yang didaftarkan orang lain dengan password yang tidak ia ketahui). Mengaktifkan semua tenant `PENDING_VERIFICATION` tempat user OWNER | Keputusan S1: tenant hasil signup berstatus `PENDING_VERIFICATION` sampai email diverifikasi |
| A8 `POST /auth/resend-verification` | — | Baru. Bearer (tanpa `tid`), `204`; link lama mati; `503` kalau SMTP gagal | Email dikirim langsung setelah commit (worker/outbox baru ada S4), sehingga harus ada jalan pemulihan |
| T1 signup | `201` dengan token + `active_tenant` | Email baru: tenant `PENDING_VERIFICATION`, `active_tenant: null`, `email_verification_required: true`, email verifikasi dikirim. Akun lama terverifikasi (password cocok): tenant `ACTIVE` dan langsung terpilih. Akun lama **belum** terverifikasi dengan password berbeda: kredensial di-*reclaim* (password + nama diganti, semua sesi dan link lama dicabut, audit `auth.unverified_account_reclaimed`), lalu alur email baru | R2-8; review S1 (pre-hijacking) |
| T1 idempotency | `request_hash` mencakup email | `request_hash` = SHA-256 atas field permintaan **tanpa password**; replay hanya `{tenant, email_verification_required}` + `Idempotent-Replayed: true` | SHA-256 atas JSON berisi password yang tersimpan di DB bisa di-crack offline |
| T1 seed | `tenant_settings`, membership, jenis cuti, komponen, departemen/posisi | `tenant_settings` (timezone dari input), membership OWNER, audit `tenant.created` | Tabel jenis cuti, komponen, departemen, dan posisi baru ada di S2–S3; seed-nya ditambahkan bersama tabelnya |
| A6 switch ke tenant `PENDING_VERIFICATION` | — | `403 FORBIDDEN` ("Email verification required…") | Sama dengan tenant `SUSPENDED`: tenant ada, tetapi belum boleh dipakai |
| A4 `/me` | `{id, email, display_name, active_tenant, employee_id}` | Ditambah `email_verified` | SPA menampilkan banner "verifikasi email" |

## Catatan implementasi (Sprint 2)

Kontrak yang berlaku: `api/openapi.yaml` (dicek `TestContract_S2ResponsesMatchSpec`); otorisasi: `api/authz_matrix.yaml` (`TestAuthzMatrix`, `TestCrossTenant`). Perbedaan terhadap U1–U3, G1–G4, W1–W5:

| Endpoint | Desain | Implementasi S2 | Alasan |
|---|---|---|---|
| W2 request | `org` + `salary` di body | Belum ada; field tak dikenal → `400` | `org_assignments` dan `salary_assignments` dibuat di S3; field-nya ditambahkan bersama tabelnya |
| W1/W3 respons | `department`, `position`, `current_org` terisi | Selalu `null` | Sama, S3 |
| W1/W3 `MANAGER` | Bawahan langsung hari ini | Himpunan bawahan kosong (list kosong, detail `404`) | Relasi atasan ada di `org_assignments` (S3) |
| W2/W5 `PERIOD_LOCKED` | Tolak tanggal di periode `FINALIZED` | Belum dicek | `payroll_periods` baru ada di S4; trigger PeriodGuard `employment_spans` ditambahkan saat itu |
| G4 deactivate | `409` kalau masih dipakai assignment hari ini | Selalu boleh | Sama, S3 |
| W4/W5 urutan error | — | Otorisasi dicek **sebelum** `If-Match`: role tanpa hak mendapat `403`/`404`, bukan `428` | File 17: status penolakan ditentukan otorisasi, bukan kelengkapan header |
| W4 `0 baris` | — | Dibedakan lewat lookup: tidak ada / tenant lain → `404`, versi basi → `412` | Menjawab `412` untuk id tenant lain membocorkan keberadaannya (ditemukan `TestCrossTenant`) |
| U2 respons | `invite_url` | `invite_url` = `PUBLIC_BASE_URL/invite#<tenant_id>.<secret>` + `Cache-Control: no-store`; tidak ada email undangan | MVP (PF-02 AC2): link ditampilkan sekali |
| U3 accept | Email cocok | Ditambah: email user wajib **terverifikasi** (`403` "Verify your email…"), karena verifikasi kini butuh password akun (review S1). Token tidak valid / tenant salah / kedaluwarsa / sudah dipakai / email beda → satu code `410 INVITATION_INVALID`. User yang pernah `REVOKED` di tenant itu → `409 MEMBERSHIP_EXISTS` (re-invite menyusul bersama revoke, S4) | R2-8 |
| U2/U3 `employee_id` | Tautkan user ke employee | `iam` memanggil modul `employee` lewat interface `EmployeeLinker` di composition root (cek belum tertaut saat undang, tautkan saat accept; sudah tertaut → `409 INVALID_STATE_TRANSITION`) | ADR-001: modul tidak menyentuh tabel modul lain |
| A4 `/me` `employee_id` | Terisi | Terisi kalau token punya `tid` dan user tertaut ke employee di tenant itu | — |
| U2 re-invite | `409 MEMBERSHIP_EXISTS` bila sudah ada undangan `INVITED` | Undangan baru untuk email yang masih `INVITED` **mengganti** link lama pada membership yang sama (link lama `revoked_at`, role diperbarui, audit `membership.reinvited`). `HR_ADMIN` tidak boleh menimpa undangan ber-role admin (`403`). Satu employee hanya boleh dicadangkan satu undangan yang masih berlaku (`409 INVALID_STATE_TRANSITION`) | Review S2: link hilang/kedaluwarsa tidak boleh mengunci email sampai revoke (S4) |
| U3 tenant tidak aktif | — | Undangan ke tenant `SUSPENDED`/`PENDING_VERIFICATION` → `410 INVITATION_INVALID` | Review S2 |
| SPA `/invite` | — | Rahasia di fragment dipindah ke memori oleh guard router dan dihapus dari URL; `?next=` tidak pernah memuat hash dan hanya menerima path internal | Review S2: rahasia sempat berpindah ke query string lewat redirect login |
| Kode baru | — | `409 NPWP_TAKEN` (`uq_employees_npwp_bidx`), dimasukkan ke katalog lebih dulu | Pola `NIK_TAKEN` |

## Catatan implementasi (Sprint 3)

Kontrak: `api/openapi.yaml` (`TestContract_S3ResponsesMatchSpec`); otorisasi: `api/authz_matrix.yaml` (151 kasus peran × target, termasuk 👥 dan 👤).

| Endpoint | Desain | Implementasi S3 | Alasan |
|---|---|---|---|
| W7 atasan | `manager_employee_id` effective-dated di `org_assignments` | Departemen/posisi effective-dated di `org_assignments`; atasan = **kolom saat ini** `employees.manager_employee_id` (berlaku segera, ter-audit `employee.org_assigned`), siklus ditolak trigger PI-19 (`422 MANAGER_CYCLE`, advisory lock per tenant) | Amandemen Step 12 S14 |
| W2 | `org` + `salary` | Diterima; keduanya mulai `hire_date` di transaksi pembuatan employee. Departemen/posisi tenant lain → `404` (Demo A langkah 4) | — |
| W1 | Filter `department_id` | Ada; `department`/`position` = penempatan yang berlaku hari ini (UTC; zona tenant masuk bersama kalender run S4) | — |
| W3 `current_org` | `{department, position, manager, valid_from}` | Sama; `department`/`position`/`valid_from` null bila hanya atasan yang diset | Atasan tidak effective-dated (S14) |
| W8 `M` | `404` | `404` juga untuk bawahan sendiri: manager tidak melihat gaji (file 17 §3.4) | — |
| W9/W10/W7 | `409 PERIOD_LOCKED` | Belum dicek | `payroll_periods` dibuat di S4 |
| W10 | `amount`/`rate` | Assignment tanpa `amount` memakai `default_amount` definisi saat kalkulasi; komponen sistem atau `INACTIVE` → `400` (`NOT_ASSIGNABLE`) | — |
| C2/C3 | `422 COMPONENT_CYCLE` / `INVALID_COMPONENT_REFERENCE` | Divalidasi dengan `engine.ValidateGraph` (fungsi yang sama dipakai engine saat kalkulasi); referensi ke base virtual yang baru lengkap di stage lebih akhir (contoh `TAXABLE_GROSS` dari komponen EARNING) ditolak | §2.4 engine |
| C3/C4 sistem | Hanya `name` | Sama; parameter lain → `403`; nonaktifkan → `403` (termasuk BPJS; mengaktifkan/menonaktifkan BPJS per tenant menyusul bila dibutuhkan) | — |
| T4 | `?year` | Wajib; tanpa pagination | — |
| Seed O6 | Komponen, jenis cuti, master | 9 komponen sistem lewat `seed_system_components()` saat signup + backfill migrasi `00006` untuk tenant lama. Jenis cuti di S6 | — |
| Lembur | Amandemen S13: satu multiplier | **Tidak diterapkan**: engine tetap mendukung tier dari ruleset (ruleset bisa berisi satu tier). Satu multiplier akan mengubah angka acuan Tono 6.888.609 yang dipakai PRD, demo script, contoh API, dan DoD S3 | Konsistensi angka acuan |
| Idempotency (review S3) | Hash = method + path + body | Hash memakai path **konkret** (dengan `{id}`); key + body yang sama untuk resource lain → `422 IDEMPOTENCY_KEY_REUSED`, bukan replay respons resource pertama | Bulk raise dengan key yang sama sempat "sukses" tanpa mengubah gaji karyawan kedua |
| W10 `valid_to` (review S3) | Menutup record berlaku | Override sementara `[from, to)` di tengah record berlaku mempertahankan ekor `[to, akhir lama)` dengan nilai lama; penutupan record lama ikut tercatat di audit (`closed_previous`) | Sebelumnya ekor terhapus |
| C2/C3 (review S3) | — | Validasi graf diserialkan per tenant (advisory lock `comp:<tenant>`) dan memuat SEMUA definisi; kode base virtual/sistem dicadangkan (`400 RESERVED`); komponen `DEDUCTION` tidak boleh `bpjs_base` | Race siklus; definisi > 200; kode yang membayangi base virtual |
| Koreksi record effective-dated | "Batalkan perubahan masa depan dulu" | **Belum ada** endpoint batal/koreksi record gaji/komponen/penempatan yang salah (dan app tidak punya grant `DELETE`). Koreksi = record baru dengan `valid_from` lebih akhir | Celah yang diketahui; endpoint koreksi ter-audit direncanakan bersama PeriodGuard (S5/S6) |
| Modul | ADR-001: modul hanya meng-query tabelnya | Penyimpangan disengaja: query baca employee JOIN `departments`/`positions` (nama untuk tampilan). Semua PENULISAN lintas modul lewat interface di composition root (`OrgUnits`, `Salaries`, `Assignments`, `Employees`) | Menghindari N+1 untuk tampilan |

## Catatan implementasi (Sprint 4)

Kontrak: `api/openapi.yaml` (`TestContract_S4ResponsesMatchSpec`); otorisasi: `api/authz_matrix.yaml` (Y2–Y8, Y12 dengan target `run` / `run_item`, termasuk lintas tenant).

| Endpoint | Desain | Implementasi S4 | Alasan |
|---|---|---|---|
| `PayrollRun` | §16: `approval_request_id`, `stale`, `submitted_by`, `approved_by`, `finalized_by`, `finalized_at` | Belum ada di respons; ditambahkan di S5 bersama approval (perubahan aditif, tidak memecah client). Ditambah `created_at` dan `cancel_reason` | Field approval tanpa approval engine hanya akan selalu `null`/`false`; `stale: false` yang selalu false menyesatkan UI |
| `PayrollItem` | `employee{id, employee_no, full_name, department}`, `run_id`, `calc_version` | Datar: `employee_id`, `employee_no`, `employee_name`, `department_id` (kolom denormalisasi saat kalkulasi, R3-4). `calc_version` hanya di detail (Y7/Y8); `run_id` sudah ada di path | Nested `employee{}` mengesankan data employee terkini, padahal nilainya dibekukan saat run dihitung |
| `PayrollLine` | `adjustment_id` | Belum ada; ditambahkan di S7 bersama adjustment. Ditambah `name` | — |
| Y7 `issues[]` | — | `{kind: error\|warning, code, detail}` | — |
| Y3 | `?period&status&limit&cursor` | Sama (cursor `created_at DESC, id`) | — |
| Y6 | `?warning=` | Belum ada; `?status`, `?department_id`, `?has_warning` ada. Limit 1..200 (default 50) | Filter per kode warning bersama warning lembur (S6) |
| Y5/Y11 tanpa body | "tanpa body" | Body kosong diterima: hash idempotency memperlakukan body kosong = `{}` (skeleton 0.0.6). `{}` juga diterima | Sebelumnya body kosong → `400 MALFORMED_JSON` |
| Y8 respons | `PayrollItem` + total run | Hanya `PayrollItem` (`EXCLUDED`); `counts` run diperbarui di transaksi yang sama, client membaca Y4 | Satu bentuk respons per resource |
| Y5 rate limit | `heavy` 10/jam per tenant | Sama, tetapi dihitung **setelah** cek role: `MANAGER`/`EMPLOYEE` yang ditolak tidak menghabiskan kuota tenant | Penolakan oleh role rendah tidak boleh memblokir payroll tenant |
| Y1, Y9–Y11 | — | Belum ada rute (skema tetap `Pending`) | Y1/Y9 di S5–S6, Y10/Y11 bersama approval dan finalize |
| U4/U5 hak pemanggil | Role dari token + cek ulang di DB untuk operasi sensitif | Role pemanggil **selalu** dibaca ulang dari DB di dalam transaksi, sesudah advisory lock `owners:<tenant>`; token OWNER/HR basi (sudah diturunkan) → `403` | ADR-P02; pemanggil yang baru dicabut tidak boleh menang balapan |
| U4/U5 advisory lock | `owners:<tenant>` sebelum mengubah role OWNER | Diambil untuk **setiap** U4/U5 (bukan hanya bila OWNER terlibat), sebelum membaca baris apa pun | Urutan lock tetap sama (advisory → membership); operasi jarang. Tanpa lock, dua OWNER yang saling menurunkan berakhir dengan 0 owner walaupun ada trigger deferred (dibuktikan `TestMembership_OwnerParallelRevokeKeepsOne` dengan lock dimatikan) |
| U4 | `Membership` | `Member` (bentuk sama dengan U1) + `ETag`; roles absolut; roles sama → `200` tanpa versi baru dan tanpa audit; hanya member `ACTIVE` (undangan `INVITED` diubah lewat undang ulang U2) → selain itu `409 INVALID_STATE_TRANSITION`. `If-Match` dicek sesudah otorisasi | — |
| U4 SoD | Peringatan di U4 + audit | Audit `membership.sod_conflict` saat kombinasi `HR_ADMIN` + `PAYROLL_ADMIN` baru terbentuk; peringatan di UI dihitung dari `roles` | Tidak ada field `warnings` di respons |
| U5 | `{reason}` | `reason` wajib 3–200. `INVITED` juga bisa dicabut (link undangan dimatikan, accept → `410`); butuh migrasi `00008` (CHECK `user_id` hanya untuk `ACTIVE`). `user` di respons `null` setelah dicabut (sama dengan U1) | Membatalkan undangan sebelumnya hanya bisa dengan undang ulang |
| U2/U3 re-invite `REVOKED` | — | Undangan ke email member yang pernah dicabut diterima dengan **mengaktifkan ulang baris membership lama** (role dari undangan, `revoked_at` dikosongkan); baris undangan ditutup `REVOKED`. Audit `membership.reactivated` (target = baris lama, diff berisi id baris undangan) | Satu baris per `(tenant_id, user_id)`: riwayat (dibuat, dicabut, aktif lagi) tetap di satu baris (keputusan pemilik, S4) |
| W6 | `O`, `H`; `E` (self) | Sama; "self" berlaku untuk role apa pun yang tertaut ke employee (file 17 👤). `P` → `403`, `M` atas bawahan langsung → `403`, lainnya `404`. Role pemanggil dibaca ulang dari DB (interface `Memberships` dari iam) | ADR-P02 |
| W6 audit | `employee.pii_viewed` di transaksi dekripsi; gagal → `503` | Sama; diff `fields` = field yang benar-benar didekripsi (`nik`, `npwp`, `bank_account`). COMMIT gagal → `5xx` tanpa PII (`TestPII_AuditNotCommittedSendsNothing`) | — |
| W6 rate limit | 30/jam per user | Tier `pii-view` (skeleton 0.0.6), di atas tier `read` | — |
| Y6/Y7 cek ulang membership | Y6: "memeriksa ulang membership di DB" (R2-6) | Y6 dan Y7: role dari DB; bukan lagi pembaca → `404`. Audit baca detail HR (`payroll_item.read`) juga diputuskan dari role DB | Token PAYROLL_ADMIN basi tidak boleh membaca hasil payroll |
