# Step 4.1 — Shared Tables

> Bagian dari [Step 4](00-index.md). Konvensi (penamaan, UUIDv7, tipe, audit field, role) mengikuti `00-index.md`.
> Tabel di sini punya **bentuk yang sama** di LedgerPay (schema `ledgerpay`) dan PayRun (schema `payrun`). Perbedaannya ditulis eksplisit di setiap tabel sebagai **Varian PayRun**.
> Tabel identitas khusus LedgerPay (`merchants`, `merchant_api_keys`, `merchant_webhook_secrets`) ada di `03-ledgerpay-schema.md`. Tabel khusus PayRun (`tenants`, `memberships`, dll.) ada di `06-payrun-schema.md`.

---

## 0. Ringkasan

| Tabel | LedgerPay | PayRun | Sifat | RLS di PayRun |
|---|---|---|---|---|
| `users` | ✓ | ✓ (global, bukan tenant-scoped) | Mutable | **Ya**, policy `self` + `tenant_member` |
| `refresh_tokens` | ✓ | ✓ (tidak terikat tenant) | Mutable (kolom status) | Tidak (diakses hanya oleh modul `iam`, lihat §2) |
| `idempotency_keys` | ✓ | ✓ (tenant-scoped) | Mutable sekali (response diisi) | **Ya** |
| `platform_idempotency_keys` | — | ✓ (khusus signup) | Mutable sekali | Tidak |
| `jobs` | ✓ | ✓ (`tenant_id` per baris) | Mutable | **Tidak**, dijaga trigger (lihat §5) |
| `audit_log` | ✓ | ✓ (`tenant_id` nullable) | Append-only | **Ya** |

---

## 1. `users`

**Tujuan:** identitas login. Di PayRun, identitas **dipisah dari keanggotaan tenant**: satu user bisa menjadi anggota banyak tenant lewat `memberships` (ADR-P02). Karena itu `users` tidak punya `tenant_id`.

| Kolom | Tipe | Null | Default | Keterangan |
|---|---|---|---|---|
| `id` | `uuid` | N | — | PK, UUIDv7 dari aplikasi |
| `email` | `text` | N | — | Disimpan apa adanya; unik case-insensitive |
| `password_hash` | `text` | N | — | String encoded argon2id (`$argon2id$v=19$m=65536,t=3,p=2$…`); parameter ikut tersimpan sehingga bisa di-upgrade saat login (rehash) |
| `display_name` | `text` | N | — | |
| `status` | `text` | N | `'ACTIVE'` | `ACTIVE`, `LOCKED`, `DISABLED` |
| `failed_login_count` | `int` | N | `0` | Reset saat login sukses |
| `locked_until` | `timestamptz` | Y | — | Lockout bertahap (ADR-009) |
| `last_login_at` | `timestamptz` | Y | — | |
| `created_at` / `updated_at` | `timestamptz` | N | `now()` | `updated_at` via trigger `set_updated_at` |
| `version` | `int` | N | `1` | Optimistic concurrency (ubah profil/password) |

**Constraint & index**
```sql
CONSTRAINT pk_users PRIMARY KEY (id),
CONSTRAINT ck_users_status CHECK (status IN ('ACTIVE','LOCKED','DISABLED')),
CONSTRAINT ck_users_email_format CHECK (position('@' in email) > 1 AND length(email) <= 254),
CONSTRAINT ck_users_failed_login_non_negative CHECK (failed_login_count >= 0)
CREATE UNIQUE INDEX uq_users_email_lower ON users (lower(email));
```

**Varian PayRun: RLS pada tabel global**

`users` memuat email dan nama dari user **semua** tenant. Tanpa RLS, bug di endpoint "daftar anggota tenant" bisa membocorkan identitas user tenant lain. Karena itu tabel ini tetap di bawah RLS, dengan dua policy:

```sql
ALTER TABLE users ENABLE ROW LEVEL SECURITY;
ALTER TABLE users FORCE  ROW LEVEL SECURITY;

-- Helper payrun.current_tenant() / payrun.current_user_id(): lihat 07 §2.1 (nullif '' → NULL; Step 12: C3)

-- 1) Diri sendiri: jalur WithPlatformTx (login lanjutan, profil, switch tenant, signup fase platform).
--    Hanya aktif di mode platform (tanpa tenant context) — Step 12: C2.
CREATE POLICY users_self ON users
  USING      ((SELECT payrun.current_tenant()) IS NULL AND id = (SELECT payrun.current_user_id()))
  WITH CHECK ((SELECT payrun.current_tenant()) IS NULL AND id = (SELECT payrun.current_user_id()));

-- 2) Anggota tenant AKTIF boleh dibaca (READ ONLY) dari dalam WithTenantTx (Step 12: R2-7 → status ACTIVE)
CREATE POLICY users_tenant_member_read ON users FOR SELECT
  USING (EXISTS (
    SELECT 1 FROM memberships m
    WHERE m.user_id = users.id
      AND m.status = 'ACTIVE'
      AND m.tenant_id = (SELECT payrun.current_tenant())));

-- 3) Kolom sensitif tidak pernah terbaca oleh role aplikasi (Step 12: R2-7).
--    password_hash hanya keluar lewat auth_lookup_user (SECURITY DEFINER, milik owner).
REVOKE SELECT ON users FROM payrun_app;
GRANT  SELECT (id, email, display_name, status, failed_login_count, locked_until,
               last_login_at, created_at, updated_at, version) ON users TO payrun_app;
GRANT  INSERT, UPDATE ON users TO payrun_app;   -- update password lewat kolom, tanpa bisa membacanya
```

**Mode eksklusif (Step 12: C2):** `WithTenantTx` hanya menyetel `app.tenant_id`, `WithPlatformTx` hanya `app.user_id`. Policy self mensyaratkan `current_tenant() IS NULL`, sehingga di dalam transaksi tenant user tidak "melihat dirinya" di tenant lain lewat jalur self.

Subquery ke `memberships` dievaluasi dengan RLS milik `memberships` juga. Hasilnya, hanya membership tenant aktif yang terlihat, sehingga yang terbaca hanya user yang benar-benar anggota tenant tersebut.

**Masalah login:** saat login belum ada `app.user_id` (user belum diketahui), sehingga policy `users_self` tidak bisa dipakai untuk mencari email. Solusinya adalah satu fungsi sempit:

```sql
CREATE FUNCTION payrun.auth_lookup_user(p_email text)
RETURNS TABLE (id uuid, password_hash text, status text, failed_login_count int, locked_until timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = payrun, pg_temp AS $$
  SELECT id, password_hash, status, failed_login_count, locked_until
  FROM users WHERE lower(email) = lower(p_email) LIMIT 1
$$;
REVOKE ALL ON FUNCTION payrun.auth_lookup_user(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION payrun.auth_lookup_user(text) TO payrun_app;
```

- Fungsi dimiliki `payrun_owner` dan hanya mengembalikan **satu baris berdasarkan email persis**, sehingga tidak bisa dipakai untuk enumerasi massal.
- `search_path` dikunci untuk mencegah serangan *search_path hijacking* pada fungsi `SECURITY DEFINER`.
- Setelah password terverifikasi, semua akses berikutnya memakai `WithPlatformTx(user_id)` di bawah policy biasa.
- Ini satu-satunya fungsi `SECURITY DEFINER` untuk identitas, dan sudah tercatat sebagai residual risk di Step 3 `05-payrun-multi-tenancy.md`. Code review wajib untuk setiap fungsi `SECURITY DEFINER` baru (aturan CI: grep di migrasi → wajib label review).

**Signup** membuat user baru di fase platform transaksi signup (`app.user_id` = id baru yang dibuat aplikasi, `app.tenant_id` kosong), sehingga `WITH CHECK` terpenuhi; baru setelah itu transaksi yang sama berpindah ke fase tenant (Step 4 `07` §2.3). Kalau email sudah terdaftar, `uq_users_email_lower` tetap terpicu (constraint tidak difilter RLS), dan alurnya meminta user login dulu lalu membuat tenant dari akun yang sudah ada.

**LedgerPay:** tanpa RLS (bukan multi-tenant). Akses dibatasi oleh scoping query di modul `identity` (LI-15).

---

## 2. `refresh_tokens`

**Tujuan:** refresh token opaque berotasi dengan *reuse detection* per family (ADR-009).

| Kolom | Tipe | Null | Default | Keterangan |
|---|---|---|---|---|
| `id` | `uuid` | N | — | PK |
| `user_id` | `uuid` | N | — | FK → `users(id)` |
| `family_id` | `uuid` | N | — | Satu family per login; semua rotasi berbagi family |
| `token_hash` | `bytea` | N | — | SHA-256 dari token (32 byte). Token mentah tidak pernah disimpan |
| `parent_id` | `uuid` | Y | — | Token sebelumnya di rantai rotasi (FK → `refresh_tokens(id)`) |
| `replaced_by` | `uuid` | Y | — | Token pengganti hasil rotasi. Dipakai untuk grace window |
| `issued_at` | `timestamptz` | N | `now()` | |
| `expires_at` | `timestamptz` | N | — | `issued_at + 7 hari` (sliding: token baru mendapat 7 hari baru, dengan batas absolut family 30 hari) |
| `family_expires_at` | `timestamptz` | N | — | Batas absolut family (dibawa dari token pertama) |
| `used_at` | `timestamptz` | Y | — | Waktu token dirotasi |
| `revoked_at` | `timestamptz` | Y | — | |
| `revoke_reason` | `text` | Y | — | `LOGOUT`, `REUSE_DETECTED`, `PASSWORD_CHANGED`, `ADMIN`, `MEMBERSHIP_REVOKED` (PayRun) |
| `user_agent` | `text` | Y | — | Untuk daftar sesi aktif |
| `ip` | `inet` | Y | — | |

```sql
CONSTRAINT pk_refresh_tokens PRIMARY KEY (id),
CONSTRAINT fk_refresh_tokens_user_id FOREIGN KEY (user_id) REFERENCES users(id),
CONSTRAINT fk_refresh_tokens_parent_id FOREIGN KEY (parent_id) REFERENCES refresh_tokens(id),
CONSTRAINT uq_refresh_tokens_token_hash UNIQUE (token_hash),
CONSTRAINT ck_refresh_tokens_hash_len CHECK (octet_length(token_hash) = 32),
CONSTRAINT ck_refresh_tokens_expiry CHECK (expires_at > issued_at AND expires_at <= family_expires_at),
CONSTRAINT ck_refresh_tokens_revoke_reason CHECK (revoke_reason IS NULL OR revoke_reason IN
  ('LOGOUT','REUSE_DETECTED','PASSWORD_CHANGED','ADMIN','MEMBERSHIP_REVOKED')),
CONSTRAINT ck_refresh_tokens_revoked_has_reason CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL))
CREATE INDEX ix_refresh_tokens_family_active ON refresh_tokens (family_id) WHERE revoked_at IS NULL;
CREATE INDEX ix_refresh_tokens_user_id     ON refresh_tokens (user_id);     -- Sprint 1: penuh, bukan partial (cek FK butuh index atas SEMUA baris; catalogue test C4)
CREATE INDEX ix_refresh_tokens_parent_id   ON refresh_tokens (parent_id);   -- Sprint 1: FK parent_id juga butuh index
CREATE INDEX ix_refresh_tokens_expires_at    ON refresh_tokens (expires_at);   -- cleanup
```

**Alur rotasi (satu transaksi):**
```sql
SELECT ... FROM refresh_tokens WHERE token_hash = $hash FOR UPDATE;
-- kasus:
-- a) tidak ada / revoked / expired            → 401
-- b) used_at IS NULL                           → rotasi: INSERT token baru (parent_id = lama),
--                                                UPDATE lama SET used_at = now(), replaced_by = baru
-- c) used_at IS NOT NULL AND now() - used_at <= interval '10 seconds'
--                                              → GRACE: dua tab refresh bersamaan. Jangan revoke family.
--                                                Terbitkan access token baru, tetapi JANGAN refresh token baru;
--                                                cookie di tab tersebut sudah ditimpa oleh respons tab pertama.
-- d) used_at IS NOT NULL AND lewat grace       → REUSE: UPDATE ... SET revoked_at = now(),
--                                                revoke_reason = 'REUSE_DETECTED' WHERE family_id = $f
--                                                AND revoked_at IS NULL; audit_log; 401
```
`FOR UPDATE` membuat dua refresh paralel dengan token yang sama terserialisasi: satu masuk kasus (b), yang lain kasus (c). Grace window 10 detik adalah kompromi. Pencuri yang memakai token dalam 10 detik setelah pemilik merotasinya tidak terdeteksi oleh request itu, tetapi tidak mendapat refresh token baru, sehingga sesinya mati ≤ 15 menit kemudian.

**Varian PayRun:** refresh token **tidak terikat tenant**. Tenant aktif ditentukan saat menerbitkan access token (claim `tid`, ADR-P02). Saat refresh, server membaca `tid` terakhir dari request (header `X-Active-Tenant` yang dikirim SPA) dan **memeriksa ulang membership di DB** sebelum menerbitkannya kembali. Inilah mekanisme yang membatasi akses setelah membership dicabut ≤ 15 menit. Kalau membership sudah dicabut, refresh tetap berhasil tetapi access token baru tidak membawa `tid` itu.

**RLS:** `refresh_tokens` **tidak** di bawah RLS di PayRun. Alasannya:
1. Endpoint refresh tidak terautentikasi (hanya cookie), sehingga belum ada `app.user_id` untuk policy.
2. Tabel ini tidak berisi data bisnis tenant maupun PII selain IP/user agent.
3. Akses dibatasi ke modul `iam` oleh depguard, dan lookup selalu berdasarkan `token_hash` (32 byte acak, tidak bisa ditebak).

Ini pengecualian sadar. Alternatifnya, fungsi `SECURITY DEFINER` kedua, ditolak karena menambah permukaan `SECURITY DEFINER` tanpa melindungi data yang sensitif.

---

## 3. `idempotency_keys`

### 3.1 LedgerPay (ADR-L02)

> Varian `users` LedgerPay menambah kolom `role text NOT NULL DEFAULT 'USER' CHECK (role IN ('USER','OPS'))` (lihat `03-ledgerpay-schema.md`). PayRun tidak punya kolom role di `users`, karena role melekat pada membership.

| Kolom | Tipe | Null | Keterangan |
|---|---|---|---|
| `id` | `uuid` | N | PK |
| `client_id` | `uuid` | N | `users.id` (JWT) atau `merchants.id` (API key) |
| `client_type` | `text` | N | `USER`, `MERCHANT`. Mencegah tabrakan UUID antar tabel secara konseptual |
| `endpoint` | `text` | N | Template rute, contoh `POST /v1/transfers` (bukan path dengan ID) |
| `key` | `text` | N | Nilai header `Idempotency-Key` |
| `request_hash` | `bytea` | N | SHA-256(method + path + canonical JSON body) |
| `response_status` | `smallint` | Y | NULL = request masih berjalan (pola b belum selesai); terisi = siap di-replay. Progres bisnis **tidak** disimpan di sini, tetapi di objek bisnisnya (`topups.recovery_point`) |
| `response_body` | `jsonb` | Y | Body respons untuk replay |
| `resource_type` / `resource_id` | `text` / `uuid` | Y | Objek bisnis yang dibuat (untuk investigasi ops) |
| `created_at` | `timestamptz` | N | |
| `expires_at` | `timestamptz` | N | `created_at + 7 hari` |
| `updated_at` | `timestamptz` | N | Trigger |

```sql
CONSTRAINT pk_idempotency_keys PRIMARY KEY (id),
CONSTRAINT uq_idempotency_keys_scope UNIQUE (client_type, client_id, endpoint, key),
CONSTRAINT ck_idempotency_keys_key_len CHECK (length(key) BETWEEN 8 AND 255),
CONSTRAINT ck_idempotency_keys_hash_len CHECK (octet_length(request_hash) = 32),
CONSTRAINT ck_idempotency_keys_client_type CHECK (client_type IN ('USER','MERCHANT')),
CONSTRAINT ck_idempotency_keys_response_pair
  CHECK ((response_status IS NULL) = (response_body IS NULL))
CREATE INDEX ix_idempotency_keys_expires_at ON idempotency_keys (expires_at);
```
> Unique scope memakai `client_type` + `client_id` (penyesuaian kecil dari ADR-L02 yang menulis `(client_id, endpoint, key)`). Maknanya sama, tetapi eksplisit untuk dua jenis client.

### 3.2 Varian PayRun (tenant-scoped)

Dipakai untuk endpoint state penting di dalam tenant: buat run, calculate, finalize, buat adjustment, import CSV.

- Tambah `tenant_id uuid NOT NULL`; **PK `(tenant_id, id)`**; `client_type` selalu `USER`.
- Unique: `uq_idempotency_keys_scope UNIQUE (tenant_id, client_id, endpoint, key)`. Key yang sama di tenant berbeda tidak bentrok.
- `response_status` selalu terisi saat commit (PayRun tidak memanggil pihak luar secara sinkron, jadi tidak ada pola b).
- RLS standar: `tenant_isolation USING / WITH CHECK (tenant_id = (SELECT payrun.current_tenant()))` (Step 12: C3).

### 3.3 `platform_idempotency_keys` (PayRun, khusus signup)

Signup terjadi **sebelum tenant ada** dan bisa sebelum user ada, sehingga tidak bisa memakai tabel tenant-scoped. Opsi yang dipertimbangkan:

| Opsi | Masalah |
|---|---|
| `idempotency_keys.tenant_id` nullable + policy khusus | Merusak PK `(tenant_id, id)` dan membuat policy tabel inti punya cabang khusus |
| Mengandalkan unique email/slug saja | Double-click → request kedua mendapat 409, bukan respons yang sama (bukan idempotency sungguhan) |
| **Tabel terpisah tanpa tenant** | Dipilih |

Kolom: `id`, `endpoint`, `key`, `request_hash`, `response_status`, `response_body`, `created_at`, `expires_at` (24 jam). `UNIQUE (endpoint, key)`. **Tanpa RLS**, diakses hanya oleh modul `tenancy` (depguard).

Keamanan: `request_hash` mencakup email, jadi key yang dipakai ulang dengan email lain → 422. Key adalah UUID acak dari client (≥ 122 bit), dan `response_body` hanya berisi `tenant_id` + `slug` (tanpa token, tanpa PII). Token login diterbitkan ulang oleh endpoint login biasa, bukan di-replay dari tabel ini.

---

## 4. `jobs` (outbox + job queue, ADR-006)

| Kolom | Tipe | Null | Default | Keterangan |
|---|---|---|---|---|
| `id` | `uuid` | N | — | PK |
| `kind` | `text` | N | — | Contoh `payout.submit`, `payroll.calc_chunk` |
| `payload` | `jsonb` | N | `'{}'` | **Hanya ID dan parameter**, tidak ada PII atau data uang yang menjadi sumber kebenaran |
| `tenant_id` | `uuid` | Y | — | LedgerPay: selalu NULL. PayRun: wajib untuk job tenant, NULL hanya untuk job platform |
| `status` | `text` | N | `'queued'` | `queued`, `running`, `succeeded`, `dead` |
| `attempts` | `int` | N | `0` | Naik saat klaim |
| `max_attempts` | `int` | N | `10` | Per kind (contoh: webhook keluar 7 = 1 percobaan + 6 retry sesuai kontrak Step 5, payroll chunk 5; katalog di Step 7 `03`) |
| `run_at` | `timestamptz` | N | `now()` | Jadwal / backoff |
| `locked_by` | `text` | Y | — | ID worker (`hostname:pid:uuid`) |
| `locked_until` | `timestamptz` | Y | — | Lease |
| `last_error` | `text` | Y | — | Dipotong 2 KB, tanpa PII |
| `dedupe_key` | `text` | Y | — | Mencegah job ganda (contoh `payroll.calc_chunk:{run}:{ver}:{chunk}`) |
| `correlation_id` | `text` | Y | — | Diteruskan ke log handler |
| `trace_parent` | `text` | Y | — | W3C `traceparent` milik request producer (Step 8). Span job dibuat sebagai root baru dengan *link* ke span ini |
| `created_at` / `updated_at` | `timestamptz` | N | `now()` | |
| `completed_at` | `timestamptz` | Y | — | Saat `succeeded`/`dead` |

```sql
CONSTRAINT pk_jobs PRIMARY KEY (id),
CONSTRAINT ck_jobs_status CHECK (status IN ('queued','running','succeeded','dead')),
CONSTRAINT ck_jobs_attempts CHECK (attempts >= 0 AND max_attempts > 0),
CONSTRAINT ck_jobs_running_has_lease CHECK (status <> 'running' OR (locked_by IS NOT NULL AND locked_until IS NOT NULL)),
CONSTRAINT ck_jobs_terminal_has_completed CHECK (status NOT IN ('succeeded','dead') OR completed_at IS NOT NULL)
CREATE UNIQUE INDEX uq_jobs_dedupe_key ON jobs (dedupe_key) WHERE dedupe_key IS NOT NULL AND status IN ('queued','running');
CREATE INDEX ix_jobs_claimable     ON jobs (run_at)        WHERE status = 'queued';
CREATE INDEX ix_jobs_lease_expired ON jobs (locked_until)  WHERE status = 'running';
CREATE INDEX ix_jobs_dead          ON jobs (kind, completed_at) WHERE status = 'dead';
CREATE INDEX ix_jobs_succeeded_cleanup ON jobs (completed_at) WHERE status = 'succeeded';
```
PK tetap `(id)` di PayRun (bukan `(tenant_id, id)`), karena `tenant_id` boleh NULL untuk job platform dan klaim dilakukan lintas tenant.

**Klaim (worker):**
```sql
UPDATE jobs SET status = 'running', locked_by = $1,
       locked_until = now() + $2::interval, attempts = attempts + 1, updated_at = now()
WHERE id IN (
  SELECT id FROM jobs
  WHERE (status = 'queued'  AND run_at <= now())
     OR (status = 'running' AND locked_until < now())
  ORDER BY run_at
  LIMIT $3
  FOR UPDATE SKIP LOCKED)
RETURNING id, kind, payload, tenant_id, attempts, max_attempts, correlation_id;
```
Planner menggabungkan dua partial index lewat BitmapOr. Kalau profil menunjukkan masalah, query dipecah menjadi dua klaim (queued, lalu lease expired).

**`CompleteInTx` (fencing):**
```sql
UPDATE jobs SET status = 'succeeded', completed_at = now(), locked_until = NULL, updated_at = now()
WHERE id = $1 AND locked_by = $2 AND status = 'running';
-- 0 baris → lease sudah diambil worker lain → aplikasi mengembalikan ErrLeaseLost → ROLLBACK seluruh tx bisnis
```

**Gagal / retry:** `UPDATE … SET status = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'queued' END, run_at = now() + backoff, last_error = $e, locked_by = NULL, locked_until = NULL, completed_at = CASE WHEN … 'dead' THEN now() END WHERE id = $1 AND locked_by = $2`. Fencing yang sama berlaku.

**Heartbeat:** `UPDATE jobs SET locked_until = now() + $ttl WHERE id = $1 AND locked_by = $2 AND status = 'running'`. 0 baris berarti lease hilang dan handler harus berhenti.

### 4.1 PayRun: kenapa `jobs` tidak di bawah RLS tenant

Worker mengklaim job **lintas tenant** dalam satu query. Dengan RLS tenant, worker tidak akan melihat apa pun, karena tidak ada satu `app.tenant_id` yang tepat. Opsi:

| Opsi | Penilaian |
|---|---|
| RLS + role worker khusus `BYPASSRLS` | Worker menjadi role dengan bypass untuk **semua** tabel. Terlalu luas |
| Satu queue/tabel per tenant | Operasional rumit, klaim jadi N query |
| **Tanpa RLS, `tenant_id` dijaga trigger saat INSERT** | **Dipilih** |

Trigger memastikan `tenant_id` di baris job **sama** dengan tenant transaksi yang membuatnya, sehingga request tenant A tidak bisa membuat job untuk tenant B:

```sql
CREATE FUNCTION payrun.jobs_enforce_tenant() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE ctx uuid := nullif(current_setting('app.tenant_id', true), '')::uuid;
BEGIN
  IF ctx IS NOT NULL AND NEW.tenant_id IS DISTINCT FROM ctx THEN
    RAISE EXCEPTION 'jobs.tenant_id (%) must match app.tenant_id (%)', NEW.tenant_id, ctx
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ck_jobs_tenant_matches_context';
  END IF;
  IF ctx IS NULL AND NEW.tenant_id IS NOT NULL THEN
    RAISE EXCEPTION 'tenant job must be enqueued inside a tenant transaction'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'ck_jobs_tenant_matches_context';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER trg_jobs_enforce_tenant BEFORE INSERT ON jobs
  FOR EACH ROW EXECUTE FUNCTION payrun.jobs_enforce_tenant();
```

Aturan turunan:
1. Scheduler platform (contoh: accrual cuti harian) berjalan tanpa tenant, mengiterasi daftar tenant, lalu meng-enqueue job per tenant **di dalam `WithTenantTxFor(tenant)`**.
2. Handler **hanya** memakai kolom `jobs.tenant_id` untuk membuka `WithTenantTxFor`. `tenant_id` di dalam `payload` (kalau ada) diabaikan. Satu sumber kebenaran, dijaga trigger.
3. Setelah tenant transaction dibuka, semua data dibaca di bawah RLS. Job yang mereferensikan ID milik tenant lain hanya akan menemukan 0 baris (Step 3 PFS-09).
4. Payload tidak boleh berisi PII. Kalau seseorang bisa membaca `jobs`, yang terlihat hanya ID.

**Residual risk:** bug di kode worker yang membuka transaksi dengan tenant yang salah. Mitigasinya: helper `jobs.RunTenantJob(job, fn)` adalah satu-satunya cara handler tenant dijalankan, dan diuji di isolation suite (PF-04).

---

## 5. `audit_log`

| Kolom | Tipe | Null | Keterangan |
|---|---|---|---|
| `id` | `uuid` | N | PK |
| `tenant_id` | `uuid` | Y | **Hanya PayRun.** NULL untuk aksi platform (signup, login gagal, break-glass grant) |
| `actor_type` | `text` | N | `USER`, `MERCHANT`, `SYSTEM`, `OPS` (LedgerPay); PayRun: `USER`, `SYSTEM`, `PLATFORM_OPERATOR` |
| `actor_id` | `uuid` | Y | NULL untuk `SYSTEM` |
| `action` | `text` | N | Format `domain.verb`, contoh `payout.manual_resolve`, `salary.changed`, `payslip.viewed` |
| `target_type` / `target_id` | `text` / `uuid` | Y | Objek yang dikenai aksi |
| `request_id` / `correlation_id` | `text` | Y | Menghubungkan ke log dan trace |
| `ip` | `inet` | Y | |
| `user_agent` | `text` | Y | Dipotong 512 karakter |
| `diff` | `jsonb` | Y | `{field: {from, to}}`, **sudah di-redact** (PII → `"[REDACTED]"` atau nilai ter-mask) |
| `reason` | `text` | Y | Wajib untuk aksi ops/override (dijaga di APP per action) |
| `created_at` | `timestamptz` | N | |

```sql
CONSTRAINT pk_audit_log PRIMARY KEY (id),
CONSTRAINT ck_audit_log_action_format CHECK (action ~ '^[a-z_]+\.[a-z_]+$'),
CONSTRAINT ck_audit_log_actor CHECK (actor_type = 'SYSTEM' OR actor_id IS NOT NULL)
CREATE INDEX ix_audit_log_target ON audit_log (target_type, target_id, created_at DESC);
CREATE INDEX ix_audit_log_actor  ON audit_log (actor_id, created_at DESC);
CREATE INDEX ix_audit_log_created ON audit_log (created_at DESC, id);   -- list kronologis (Step 12: R3-3)
CREATE TRIGGER trg_audit_log_append_only BEFORE UPDATE OR DELETE ON audit_log
  FOR EACH ROW EXECUTE FUNCTION forbid_update_delete();
GRANT SELECT, INSERT ON audit_log TO <proj>_app;   -- tanpa UPDATE/DELETE
```

**Varian PayRun:**
- Index diawali tenant: `ix_audit_log_tenant_target (tenant_id, target_type, target_id, created_at DESC)`, `ix_audit_log_tenant_actor (tenant_id, actor_id, created_at DESC)`, `ix_audit_log_tenant_action (tenant_id, action, created_at DESC)` (contoh: semua `payslip.viewed`), `ix_audit_log_tenant_created (tenant_id, created_at DESC, id)` untuk `GET /audit-log` tanpa filter (Step 12: R3-3).
- PK tetap `(id)`, karena `tenant_id` nullable. Ini pengecualian terhadap konvensi PK `(tenant_id, id)`.
- RLS:
```sql
CREATE POLICY audit_tenant ON audit_log
  USING      (tenant_id = (SELECT payrun.current_tenant()))
  WITH CHECK (tenant_id = (SELECT payrun.current_tenant())
              OR (tenant_id IS NULL AND (SELECT payrun.current_tenant()) IS NULL));
-- Step 12 (R2-3): baris platform dibaca role investigasi saja
CREATE POLICY audit_platform_read ON audit_log FOR SELECT TO payrun_readonly
  USING (tenant_id IS NULL);
```
  Baris platform (`tenant_id IS NULL`) tidak pernah terlihat oleh user tenant. Baris ini hanya dibaca oleh `payrun_readonly` lewat policy `audit_platform_read`. (Step 12: C3 — versi lama memakai cast mentah; `current_setting(…) IS NULL` juga keliru karena setelah GUC pernah disetel nilainya `''`, bukan NULL, sehingga baris platform seperti login gagal tertolak.)
- Tidak ada FK dari `audit_log` ke tabel lain, karena log tetap harus bisa ditulis walaupun target sedang dideaktivasi, dan tidak boleh menghalangi apa pun.

**V2:** partisi bulanan (`PARTITION BY RANGE (created_at)`) + arsip partisi lama ke object storage, ketika tabel melewati puluhan juta baris.

---

## 6. Trigger function bersama

```sql
CREATE FUNCTION set_updated_at() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END $$;
-- dipasang di setiap tabel mutable:
-- CREATE TRIGGER trg_<tabel>_updated_at BEFORE UPDATE ON <tabel>
--   FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE FUNCTION forbid_update_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% is append-only: % is not allowed', TG_TABLE_NAME, TG_OP
    USING ERRCODE = 'insufficient_privilege', HINT = 'Create a correcting record instead';
END $$;
```

Kedua fungsi dibuat sekali per schema oleh migrasi awal (`<proj>_owner`). `forbid_update_delete` juga dipasang di `journals`, `journal_entries` (LedgerPay). Di PayRun, `payroll_lines` / `payroll_item_snapshots` memakai trigger khusus: UPDATE selalu ditolak, DELETE hanya ditolak setelah run `FINALIZED`, karena line `calc_version` lama dihapus saat recalculation (ADR-P05; lihat `06-payrun-schema.md`). Detailnya ada di file schema masing-masing.

---

## 7. Pemetaan constraint → error domain

| Constraint / sumber | SQLSTATE | Error domain | HTTP |
|---|---|---|---|
| `uq_users_email_lower` | 23505 | `EMAIL_ALREADY_REGISTERED` | 409 |
| `uq_refresh_tokens_token_hash` | 23505 | (bug: tabrakan hash) → error internal | 500 |
| `uq_idempotency_keys_scope` | 23505 | Tidak muncul ke client; ditangani via `ON CONFLICT DO NOTHING` → replay / `IDEMPOTENCY_KEY_REUSED` | 200/201 replay atau 422 |
| `ck_idempotency_keys_key_len` | 23514 | `IDEMPOTENCY_KEY_INVALID` | 400 |
| `uq_jobs_dedupe_key` | 23505 | Job sudah antre → no-op (sukses) | — |
| `ck_jobs_tenant_matches_context` (trigger) | 23514 | Bug → `INTERNAL`, metrik `tenant_isolation_violation_total` + alert | 500 |
| `trg_*_append_only` (`forbid_update_delete`) | 42501 | Bug → `INTERNAL` + alert | 500 |
| `lock_timeout` saat menunggu key idempotency | 55P03 | `REQUEST_IN_PROGRESS` | 409 |
| RLS `WITH CHECK` gagal | 42501 | Bug → `INTERNAL` + metrik isolasi | 500 |

Pelanggaran yang seharusnya mustahil (trigger isolasi, append-only, RLS `WITH CHECK`) **tidak pernah** diterjemahkan menjadi 4xx yang ramah. Pelanggaran ini adalah bug, jadi harus berbunyi di alert.

---

## 8. Retensi & cleanup

| Tabel | Aturan | Job kind | Batch | Jadwal |
|---|---|---|---|---|
| `idempotency_keys` | `DELETE WHERE expires_at < now()` | `maintenance.idempotency_cleanup` | 5.000 baris per batch, loop sampai habis | Harian 02:00 |
| `platform_idempotency_keys` (PayRun) | `expires_at < now()` (24 jam) | `maintenance.idempotency_cleanup` | 1.000 | Harian |
| `refresh_tokens` | `family_expires_at < now() - interval '7 days'` (sisakan seminggu untuk investigasi reuse) | `maintenance.refresh_token_cleanup` | 5.000 | Harian |
| `jobs` (`succeeded`) | `completed_at < now() - interval '7 days'` | `maintenance.jobs_cleanup` | 10.000 | Per jam |
| `jobs` (`dead`) | `completed_at < now() - interval '30 days'`, setelah dicatat di audit / sudah di-replay | `maintenance.jobs_cleanup` | 1.000 | Harian |
| `audit_log` | **Tidak dihapus** di V1. V2: arsip partisi > 13 bulan | — | — | — |

Batch delete memakai pola `DELETE FROM t WHERE id IN (SELECT id FROM t WHERE <aturan> LIMIT $n)` agar transaksi pendek, lock tidak menumpuk, dan autovacuum bisa mengejar. Job cleanup sendiri berjalan lewat job runner (dengan `dedupe_key` per jadwal), sehingga dua worker tidak menjalankan cleanup yang sama bersamaan.
