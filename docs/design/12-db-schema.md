# PayRun: Spesifikasi Skema per Tabel

> Konvensi: `00-index.md`. ERD dan daftar tabel: [`11-db-erd.md`](11-db-erd.md). RLS, grant, index tambahan, volume: [`13-db-rls-consistency-indexing.md`](13-db-rls-consistency-indexing.md).
> Invariant (PI-xx): [`08-invariants.md`](08-invariants.md). State machine: desain domain`.
> Schema: `payrun`. Potongan SQL hanya untuk constraint, trigger, dan policy yang menjadi inti keputusan; DDL lengkap ditulis sebagai migrasi goose saat coding.

---

## 0. Blok kolom standar

Supaya setiap tabel tidak mengulang kolom yang sama, dipakai blok berikut. Tabel menyebut blok mana yang dipakai.

| Blok | Kolom | Tipe | Null | Default | Catatan |
|---|---|---|---|---|---|
| **[T] Tenant key** | `tenant_id` | `uuid` | NOT NULL | — | FK `→ tenants(id)`; kolom pertama di PK dan setiap index |
| | `id` | `uuid` | NOT NULL | — (UUIDv7 dari app) | `PRIMARY KEY (tenant_id, id)` → `pk_<tabel>` |
| **[M] Mutable** | `created_at` | `timestamptz` | NOT NULL | `now()` | |
| | `updated_at` | `timestamptz` | NOT NULL | `now()` | Trigger `set_updated_at` (BEFORE UPDATE) |
| **[H] Diubah manusia** | `created_by` | `uuid` | NULL | — | `users.id`; NULL bila dibuat sistem (seed/job) |
| | `updated_by` | `uuid` | NULL | — | |
| | `version` | `int` | NOT NULL | `1` | Optimistic concurrency: `UPDATE … WHERE version = $v` lalu `version = version + 1`; 0 baris → `409 VERSION_CONFLICT` |
| **[A] Append-only** | `created_at` | `timestamptz` | NOT NULL | `now()` | Tanpa `updated_at`; trigger `forbid_update_delete` + grant `SELECT, INSERT` |

**Aturan composite FK:** setiap FK ke tabel tenant-scoped ditulis `FOREIGN KEY (tenant_id, x_id) REFERENCES <parent>(tenant_id, id)`. Ini tidak diulang di setiap tabel. Daftar "FK" di bawah hanya menyebut kolom `x_id`-nya.

**Fungsi trigger generik** `set_updated_at` dan `forbid_update_delete` didefinisikan sekali per schema di [`10-db-shared-tables.md`](10-db-shared-tables.md) §6. `forbid_update_delete` memakai `ERRCODE = insufficient_privilege` (`42501`).

Kode error kustom PayRun: `P0001` = run final immutable (PI-04), `P0002` = period locked (PI-14), `P0004` = owner terakhir (PI-16), `P0005` = siklus manager (PI-19), `P0006` = invariant total (PI-05). Pemetaan ke kode API ada di `07` §7.

---

## 1. Tenancy

### 1.1 `tenants` (global)

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| `id` | `uuid` | NOT NULL | — | `PRIMARY KEY (id)`. Tabel ini adalah *akar* tenant, jadi tidak memakai blok [T] |
| `slug` | `text` | NOT NULL | — | Untuk tampilan/URL, **bukan** untuk isolasi |
| `name` | `text` | NOT NULL | — | Nama perusahaan |
| `status` | `text` | NOT NULL | `'ACTIVE'` | `ACTIVE`, `SUSPENDED` |
| `suspended_at` | `timestamptz` | NULL | — | |
| [M] | | | | |

- **Constraint:** `pk_tenants (id)`; `uq_tenants_slug UNIQUE (lower(slug))` (unique index); `ck_tenants_status CHECK (status IN ('ACTIVE','SUSPENDED'))`; `ck_tenants_slug_format CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$')`; `ck_tenants_suspended CHECK ((status = 'SUSPENDED') = (suspended_at IS NOT NULL))`.
- **Catatan:** status `DELETED` (offboarding) baru ditambahkan di V2. RLS khusus: lihat `07` §2.3.

### 1.2 `tenant_settings` (1:1 dengan tenant)

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| `tenant_id` | `uuid` | NOT NULL | — | `PRIMARY KEY (tenant_id)`, FK `→ tenants(id)`. Relasi 1:1, jadi tidak butuh `id` sendiri |
| `timezone` | `text` | NOT NULL | `'Asia/Jakarta'` | Nama IANA; divalidasi aplikasi (`time.LoadLocation`) |
| `work_days` | `smallint[]` | NOT NULL | `'{1,2,3,4,5}'` | Hari kerja mingguan (ISO: 1 = Senin … 7 = Minggu) |
| `proration_basis` | `text` | NOT NULL | `'WORKING_DAYS'` | `WORKING_DAYS`, `CALENDAR_DAYS` (`07-payroll-engine.md` §6) |
| `cutoff_day_of_next_month` | `smallint` | NOT NULL | `2` | Cut-off = tanggal ini di bulan berikutnya |
| `cutoff_time_local` | `time` | NOT NULL | `'23:59'` | Jam cut-off, waktu tenant |
| `overtime_basis` | `text` | NOT NULL | `'BASE'` | `BASE`, `BASE_PLUS_FIXED` |
| `overtime_max_hours_per_day` | `numeric(4,1)` | NULL | `4.0` | Warning `OVERTIME_OVER_LIMIT` |
| `incomplete_attendance_policy` | `text` | NOT NULL | `'COUNT_AS_PRESENT'` | `COUNT_AS_PRESENT`, `COUNT_AS_ABSENT` |
| `ruleset_code` | `text` | NOT NULL | `'ID-SIMPLIFIED'` | Kode ruleset; versi dipilih per periode berdasarkan `rulesets.valid_during` |
| `net_change_warning_pct` | `numeric(5,2)` | NOT NULL | `20.00` | Ambang warning diff vs bulan lalu (PF-23) |
| [M] [H] | | | | |

- **Constraint:** `ck_tenant_settings_work_days CHECK (work_days <@ '{1,2,3,4,5,6,7}' AND cardinality(work_days) BETWEEN 1 AND 7)`; `ck_tenant_settings_proration CHECK (proration_basis IN ('WORKING_DAYS','CALENDAR_DAYS'))`; `ck_tenant_settings_cutoff_day CHECK (cutoff_day_of_next_month BETWEEN 1 AND 28)`; `ck_tenant_settings_overtime_basis CHECK (overtime_basis IN ('BASE','BASE_PLUS_FIXED'))`; `ck_tenant_settings_incomplete CHECK (incomplete_attendance_policy IN ('COUNT_AS_PRESENT','COUNT_AS_ABSENT'))`.
- **Catatan:** pembagi lembur (173) **tidak** ada di sini. Nilai itu milik ruleset, agar ikut ter-versi dan ter-snapshot. `cutoff_day ≤ 28` mencegah tanggal yang tidak ada di Februari.

### 1.3 `holidays`

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `holiday_date` | `date` | NOT NULL | — | Tanggal libur (waktu tenant) |
| `name` | `text` | NOT NULL | — | "Hari Raya Nyepi" |
| [M] [H] | | | | |

- **Constraint:** `uq_holidays_date UNIQUE (tenant_id, holiday_date)`.
- **Trigger:** `holidays_period_guard` (fungsi `guard_period_date`, §6.5): menambah/menghapus libur di periode `FINALIZED` mengubah hari kerja, sehingga ditolak `P0002`.

---

## 2. IAM

### 2.1 `users`, `refresh_tokens` *(shared)*
Spesifikasi di [`10-db-shared-tables.md`](10-db-shared-tables.md). Dari sisi PayRun: global (tanpa `tenant_id`), dengan RLS khusus (`07` §2.3).

### 2.2 `memberships`

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `user_id` | `uuid` | NULL | — | FK `→ users(id)`. NULL selama `INVITED` bila email belum punya akun |
| `invited_email` | `text` | NULL | — | Email undangan (dinormalisasi lowercase) |
| `roles` | `text[]` | NOT NULL | — | Subset `{OWNER, HR_ADMIN, PAYROLL_ADMIN, MANAGER, EMPLOYEE}` |
| `status` | `text` | NOT NULL | `'INVITED'` | `INVITED`, `ACTIVE`, `REVOKED` |
| `accepted_at` | `timestamptz` | NULL | — | |
| `revoked_at` | `timestamptz` | NULL | — | |
| `revoked_by` | `uuid` | NULL | — | |
| [M] [H] | | | | |

- **Constraint:**
  - `uq_memberships_user UNIQUE (tenant_id, user_id)`. NULL tidak bentrok, sehingga beberapa undangan tanpa akun tetap boleh.
  - `uq_memberships_invited_email` unique index `(tenant_id, lower(invited_email)) WHERE status = 'INVITED'`.
  - `ck_memberships_status CHECK (status IN ('INVITED','ACTIVE','REVOKED'))`.
  - `ck_memberships_roles CHECK (cardinality(roles) >= 1 AND roles <@ ARRAY['OWNER','HR_ADMIN','PAYROLL_ADMIN','MANAGER','EMPLOYEE'])`.
  - `ck_memberships_user_when_active CHECK (status <> 'ACTIVE' OR user_id IS NOT NULL)` (migrasi 00008, S4: undangan yang dicabut menjadi REVOKED tanpa user).
  - `ck_memberships_invite_target CHECK (user_id IS NOT NULL OR invited_email IS NOT NULL)`.
- **Index:** `ix_memberships_user (user_id) WHERE status = 'ACTIVE'`, untuk login dan daftar tenant milik user (jalur `WithPlatformTx`).
- **Trigger:** `memberships_owner_guard`, constraint trigger deferred (PI-16):

```sql
CREATE FUNCTION payrun.ensure_tenant_has_owner() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM payrun.memberships m
                 WHERE m.tenant_id = OLD.tenant_id AND m.status = 'ACTIVE'
                   AND 'OWNER' = ANY (m.roles)) THEN
    RAISE EXCEPTION 'tenant % must keep at least one active OWNER', OLD.tenant_id
      USING ERRCODE = 'P0004';
  END IF;
  RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER memberships_owner_guard
  AFTER UPDATE OR DELETE ON payrun.memberships
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW
  WHEN (('OWNER' = ANY (OLD.roles)) AND OLD.status = 'ACTIVE')
  EXECUTE FUNCTION payrun.ensure_tenant_has_owner();
```
  Karena deferred, transfer kepemilikan dalam satu transaksi (tambah owner baru, cabut owner lama) tetap lolos. Race antara dua pencabutan owner yang paralel diserialkan oleh aplikasi dengan `pg_advisory_xact_lock(hashtextextended('owners:'||tenant_id, 0))` sebelum mengubah role owner (PI-16; desain transaksi & concurrency). `tenants FOR UPDATE` tidak dipakai karena akan memblok cek FK `KEY SHARE` dari setiap INSERT di tenant tersebut.

**Keputusan: `roles text[]`, bukan tabel `membership_roles`.**

| Opsi | Kelebihan | Kekurangan |
|---|---|---|
| **`text[]` + CHECK subset** (dipilih) | Satu baris per membership; cek PI-16 cukup `'OWNER' = ANY(roles)`; switch-tenant membaca satu baris; set role kecil dan didefinisikan di kode | Tidak ada FK ke tabel role; query "semua HR_ADMIN" memakai `= ANY` (cukup cepat per tenant) |
| Tabel `membership_roles (membership_id, role)` | Normal, bisa diberi FK/atribut per role | Join di setiap request; trigger PI-16 lebih rumit; tidak ada atribut per role yang dibutuhkan |
| Tabel `roles` dinamis (custom role per tenant) | Fleksibel | Di luar cakupan (RBAC kustom = fitur enterprise) |

### 2.3 `invitations`

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `membership_id` | `uuid` | NOT NULL | — | FK → `memberships` |
| `token_hash` | `bytea` | NOT NULL | — | SHA-256 dari bagian rahasia token |
| `email` | `text` | NOT NULL | — | Tujuan undangan |
| `expires_at` | `timestamptz` | NOT NULL | — | Default `now() + 7 days` (diisi app) |
| `accepted_at` | `timestamptz` | NULL | — | Sekali pakai |
| `revoked_at` | `timestamptz` | NULL | — | Undangan dikirim ulang → token lama di-revoke |
| [A-ish] `created_at`, `created_by` | | | | |

- **Constraint:** `uq_invitations_token_hash UNIQUE (token_hash)`; `ck_invitations_single_use CHECK (accepted_at IS NULL OR revoked_at IS NULL)`; `ck_invitations_sha256 CHECK (octet_length(token_hash) = 32)`.
- **Index:** `ix_invitations_membership (tenant_id, membership_id)`.
- **Catatan desain token:** format token `<tenant_id>.<secret 32 byte base64url>`. Endpoint accept memisahkan `tenant_id`, membuka `WithTenantTx(tenant_id)`, lalu mencari `token_hash = sha256(secret)` di bawah RLS tenant tersebut. Tenant context berasal dari token rahasia yang diverifikasi di dalam transaksi, bukan dari input bebas. Menebak `tenant_id` tanpa secret tidak berguna, dan menyalin secret ke `tenant_id` lain tidak cocok karena hash dicari di bawah RLS tenant itu.
- Kolom yang berubah hanya `accepted_at`/`revoked_at` (sekali). Tabel ini mutable dengan pola "set sekali", tanpa trigger append-only.

---

## 3. Org

### 3.1 `departments` / 3.2 `positions`

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `code` | `text` | NOT NULL | — | Kode pendek unik per tenant (`WH`, `FIN`) |
| `name` | `text` | NOT NULL | — | |
| `status` | `text` | NOT NULL | `'ACTIVE'` | `ACTIVE`, `INACTIVE` (deaktivasi, bukan hapus) |
| [M] [H] | | | | |

- **Constraint:** `uq_departments_code UNIQUE (tenant_id, code)`; `ck_departments_status CHECK (status IN ('ACTIVE','INACTIVE'))`. Hal yang sama berlaku untuk `positions`.
- **Catatan:** deaktivasi ditolak aplikasi jika masih ada `org_assignments` yang berlaku hari ini. Riwayat lama tetap valid, karena FK menunjuk baris yang tetap ada.

### 3.3 `org_assignments` (effective-dated)

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `employee_id` | `uuid` | NOT NULL | — | FK → `employees` |
| `department_id` | `uuid` | NOT NULL | — | FK → `departments` |
| `position_id` | `uuid` | NOT NULL | — | FK → `positions` |
| `manager_employee_id` | `uuid` | NULL | — | FK → `employees`; NULL = pimpinan tertinggi |
| `valid_during` | `daterange` | NOT NULL | — | `[from, to)`, `to` boleh tak hingga |
| [M] [H] | | | | |

- **Constraint:**
  - `ex_org_assignments_no_overlap EXCLUDE USING gist (tenant_id WITH =, employee_id WITH =, valid_during WITH &&)` (PI-09).
  - `ck_org_assignments_not_self CHECK (manager_employee_id IS DISTINCT FROM employee_id)` (PI-19, siklus langsung).
  - `ck_org_assignments_range CHECK (NOT isempty(valid_during) AND lower_inc(valid_during) AND NOT upper_inc(valid_during) AND lower(valid_during) IS NOT NULL)`.
  - FK `manager_employee_id` composite `(tenant_id, manager_employee_id)` dengan `MATCH SIMPLE`: jika manager NULL, FK tidak dicek.
- **Index:** GiST dari exclusion constraint melayani "assignment per tanggal". Tambahan: `ix_org_assignments_manager (tenant_id, manager_employee_id) WHERE manager_employee_id IS NOT NULL` untuk "bawahan langsung saya" (approval inbox, authz manager-of).
- **Trigger:** `org_assignments_no_cycle` (PI-19), BEFORE INSERT OR UPDATE OF `manager_employee_id`, `valid_during`:

```sql
CREATE FUNCTION payrun.forbid_manager_cycle() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE d date := lower(NEW.valid_during);
BEGIN
  IF NEW.manager_employee_id IS NULL THEN RETURN NEW; END IF;
  -- serialisasi perubahan struktur org per tenant (jarang terjadi, jadi murah)
  PERFORM pg_advisory_xact_lock(hashtextextended('org:' || NEW.tenant_id::text, 0));
  IF EXISTS (
    WITH RECURSIVE chain(emp) AS (
      SELECT NEW.manager_employee_id
      UNION
      SELECT o.manager_employee_id
      FROM payrun.org_assignments o JOIN chain c ON o.employee_id = c.emp
      WHERE o.tenant_id = NEW.tenant_id AND o.valid_during @> d
        AND o.id <> NEW.id AND o.manager_employee_id IS NOT NULL
    ) SELECT 1 FROM chain WHERE emp = NEW.employee_id
  ) THEN
    RAISE EXCEPTION 'manager cycle for employee % at %', NEW.employee_id, d USING ERRCODE = 'P0005';
  END IF;
  RETURN NEW;
END $$;
```
  Advisory lock per tenant menutup race "A set manager B" bersamaan dengan "B set manager A", yang masing-masing tidak melihat perubahan yang lain di bawah Read Committed. Keterbatasan lintas tanggal efektif (desain domain PI-19) tetap berlaku dan ditangkap job validasi harian.

---

## 4. Employee

### 4.1 `employees`

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `employee_no` | `text` | NOT NULL | — | Nomor karyawan, unik per tenant (dipakai import CSV) |
| `full_name` | `text` | NOT NULL | — | Tidak dienkripsi (pencarian/sorting; ADR-P08) |
| `work_email` | `text` | NULL | — | |
| `user_id` | `uuid` | NULL | — | FK `→ users(id)` untuk self-service |
| `status` | `text` | NOT NULL | `'ACTIVE'` | `ACTIVE`, `RESIGNED`, `TERMINATED` |
| `ptkp_status` | `text` | NOT NULL | `'TK/0'` | Status PTKP (simplified): `TK/0..TK/3`, `K/0..K/3` |
| `nik_enc` | `bytea` | NULL | — | Ciphertext `v1:nonce:ct`, AAD `tenant_id:employees.nik` |
| `nik_bidx` | `bytea` | NULL | — | HMAC-SHA256 blind index (PI-18) |
| `nik_last4` | `text` | NULL | — | Untuk masking tanpa dekripsi |
| `npwp_enc` | `bytea` | NULL | — | |
| `npwp_bidx` | `bytea` | NULL | — | |
| `npwp_last4` | `text` | NULL | — | |
| `bank_name` | `text` | NULL | — | |
| `bank_account_enc` | `bytea` | NULL | — | Tanpa blind index (ADR-P08) |
| `bank_account_last4` | `text` | NULL | — | |
| `bank_account_holder` | `text` | NULL | — | |
| `pii_key_version` | `smallint` | NULL | — | Versi kunci aktif saat terakhir dienkripsi; mempermudah job rotasi (`WHERE pii_key_version < $active`) |
| [M] [H] | | | | |

- **Constraint:**
  - `uq_employees_employee_no UNIQUE (tenant_id, employee_no)` (PI-18).
  - `uq_employees_nik_bidx UNIQUE (tenant_id, nik_bidx)`. NULL tidak bentrok.
  - `uq_employees_npwp_bidx UNIQUE (tenant_id, npwp_bidx)`.
  - `uq_employees_user UNIQUE (tenant_id, user_id)`: satu user = maksimal satu employee per tenant.
  - `ck_employees_status CHECK (status IN ('ACTIVE','RESIGNED','TERMINATED'))`.
  - `ck_employees_ptkp CHECK (ptkp_status IN ('TK/0','TK/1','TK/2','TK/3','K/0','K/1','K/2','K/3'))`.
  - `ck_employees_nik_pair CHECK ((nik_enc IS NULL) = (nik_bidx IS NULL))`, begitu juga untuk NPWP. Ciphertext tanpa blind index (atau sebaliknya) berarti bug.
  - `ck_employees_last4 CHECK (nik_last4 IS NULL OR nik_last4 ~ '^[0-9]{4}$')` (dan sejenisnya).
- **Index:** `ix_employees_name_prefix (tenant_id, lower(full_name) text_pattern_ops)` untuk pencarian prefix nama (`07` §4); `ix_employees_status (tenant_id, status)`.
- **Catatan:** `status` adalah ringkasan untuk filter UI. Sumber kebenaran eligibility payroll adalah `employment_spans`. Konsistensinya (status `RESIGNED` ⇔ span terakhir tertutup) dijaga di use case "resign" yang menulis keduanya dalam satu transaksi.

### 4.2 `employment_spans` (effective-dated)

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `employee_id` | `uuid` | NOT NULL | — | FK → `employees` |
| `span` | `daterange` | NOT NULL | — | `[hire_date, end_date)`; `end_date` = hari pertama tidak aktif |
| `end_reason` | `text` | NULL | — | `RESIGNED`, `TERMINATED`, `CONTRACT_END` |
| [M] [H] | | | | |

- **Constraint:** `ex_employment_spans_no_overlap EXCLUDE USING gist (tenant_id WITH =, employee_id WITH =, span WITH &&)` (PI-07, PI-09); `ck_employment_spans_range` (sama dengan §3.3); `ck_employment_spans_end_reason CHECK ((upper_inf(span)) = (end_reason IS NULL))`: span terbuka tidak punya alasan selesai, span tertutup wajib punya.
- **Trigger:** `employment_spans_period_guard` (fungsi `guard_period_range`, §6.5).
- **Catatan:** rehire = span baru. Query eligibility: `span && period_range`.

---

## 5. Compensation

### 5.1 `rulesets` (GLOBAL, read-only bagi app)

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| `id` | `uuid` | NOT NULL | — | `PRIMARY KEY (id)` (tanpa tenant) |
| `code` | `text` | NOT NULL | — | `ID-SIMPLIFIED` |
| `version` | `text` | NOT NULL | — | `2026.1` |
| `valid_during` | `daterange` | NOT NULL | — | Rentang berlaku |
| `label` | `text` | NOT NULL | — | Contoh: `"simplified — not compliance-certified"`. Wajib tampil di UI |
| `payload` | `jsonb` | NOT NULL | — | Divisor lembur, tier, statutory (BPJS), tax brackets per kategori, rounding (struktur `Ruleset` di `07-payroll-engine.md` §14) |
| `payload_sha256` | `bytea` | NOT NULL | — | Hash kanonik payload; ikut masuk fingerprint |
| [A] `created_at` | | | | |

- **Constraint:** `uq_rulesets_code_version UNIQUE (code, version)`; `ex_rulesets_no_overlap EXCLUDE USING gist (code WITH =, valid_during WITH &&)`: untuk satu kode, tepat satu versi berlaku per tanggal; `ck_rulesets_sha256 CHECK (octet_length(payload_sha256) = 32)`.
- **Trigger:** `rulesets_immutable BEFORE UPDATE OR DELETE` → `forbid_update_delete` (PI-12).
- **Grant:** `payrun_app`: `SELECT` saja. Baris baru hanya lewat migrasi data (role owner). Tidak memakai RLS karena bukan data tenant.
- **Catatan:** menutup versi lama (`valid_during` diubah) tidak mungkin karena tabel immutable. Konsekuensinya, versi baru dibuat dengan rentang yang **tidak beririsan** dengan versi lama. Versi lama dibuat dengan `upper` yang sudah diketahui, atau versi baru dipublikasikan dengan kode baru (`ID-SIMPLIFIED-2027`). Aturan ini didokumentasikan di runbook ruleset.

### 5.2 `component_definitions`

**Keputusan: kolom ber-tipe + CHECK per `calc_type`, bukan `params jsonb`.** Hanya ada 4 tipe dengan parameter yang tetap. Kolom ber-tipe memberi CHECK di DB, query, dan diff audit yang jelas. `jsonb` baru masuk akal kalau tipe kalkulasi bertambah liar (dan itu justru sinyal untuk menolak DSL, ADR-P03).

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `code` | `text` | NOT NULL | — | Unik per tenant; direferensikan `PERCENT_OF` dan baris slip |
| `name` | `text` | NOT NULL | — | Label slip |
| `category` | `text` | NOT NULL | — | `EARNING`, `DEDUCTION` |
| `stage` | `text` | NOT NULL | — | `EARNING`, `OVERTIME`, `ABSENCE`, `STATUTORY`, `TAX`, `ADJUSTMENT` |
| `calc_type` | `text` | NOT NULL | — | `FIXED`, `PERCENT_OF`, `PER_DAY`, `PER_HOUR_MULTIPLIER` |
| `default_amount` | `numeric(18,2)` | NULL | — | `FIXED`/`PER_DAY`: nilai default (bisa di-override per employee) |
| `rate` | `numeric(12,6)` | NULL | — | `PERCENT_OF`: 0.020000 = 2% |
| `ruleset_ref` | `text` | NULL | — | `PERCENT_OF` statutory: rate/cap diambil dari ruleset (`BPJS_JHT_EE`) |
| `base_ref` | `text` | NULL | — | `PERCENT_OF`: `BASE_SALARY`, `FIXED_ALLOWANCES`, `BPJS_WAGE`, `TAXABLE_GROSS`, atau `code` komponen lain |
| `day_source` | `text` | NULL | — | `PER_DAY`: `PRESENT`, `WORKING` |
| `cap_min` | `numeric(18,2)` | NULL | — | |
| `cap_max` | `numeric(18,2)` | NULL | — | |
| `taxable` | `boolean` | NOT NULL | `true` | |
| `bpjs_base` | `boolean` | NOT NULL | `false` | |
| `rounding` | `text` | NOT NULL | `'HALF_UP'` | `HALF_UP`, `DOWN`, `UP` |
| `is_system` | `boolean` | NOT NULL | `false` | Komponen sistem (BASE_SALARY, OVERTIME, ABSENCE_*, BPJS_*, PPH21, ADJUSTMENT): diisi seed, tidak bisa dihapus/diubah tipenya oleh tenant |
| `status` | `text` | NOT NULL | `'ACTIVE'` | `ACTIVE`, `INACTIVE` |
| [M] [H] | | | | |

- **Constraint:**
  - `uq_component_definitions_code UNIQUE (tenant_id, code)`; `ck_component_definitions_code_format CHECK (code ~ '^[A-Z][A-Z0-9_]{1,40}$')`.
  - Enum CHECK untuk `category`, `stage`, `calc_type`, `day_source`, `rounding`, `status`.
  - `ck_component_definitions_params`:
    ```sql
    CHECK (CASE calc_type
      WHEN 'FIXED'      THEN rate IS NULL AND base_ref IS NULL AND ruleset_ref IS NULL AND day_source IS NULL
      WHEN 'PER_DAY'    THEN day_source IS NOT NULL AND rate IS NULL AND base_ref IS NULL AND ruleset_ref IS NULL
      WHEN 'PERCENT_OF' THEN base_ref IS NOT NULL AND day_source IS NULL AND default_amount IS NULL
                             AND ((rate IS NULL) <> (ruleset_ref IS NULL))      -- rate sendiri XOR dari ruleset
      WHEN 'PER_HOUR_MULTIPLIER' THEN is_system AND stage = 'OVERTIME'          -- tier dari ruleset
    END)
    ```
  - `ck_component_definitions_caps CHECK (cap_min IS NULL OR cap_max IS NULL OR cap_min <= cap_max)`.
  - `ck_component_definitions_rate CHECK (rate IS NULL OR (rate >= 0 AND rate <= 1))`.
  - `ck_component_definitions_tenant_stage CHECK (is_system OR stage = 'EARNING')`. Komponen tenant hanya berada di stage `EARNING`, dengan kategori `EARNING` atau `DEDUCTION` biasa. Stage lain milik sistem (`07-payroll-engine.md` §2.2).
- **Catatan:** validasi graf referensi (`base_ref` ke komponen yang ada/aktif, tanpa siklus, tanpa referensi ke stage yang lebih akhir) dilakukan aplikasi saat simpan (`422 COMPONENT_CYCLE`, dll.). Graf tidak bisa dinyatakan sebagai constraint. Engine juga memvalidasi secara defensif saat kalkulasi.

### 5.3 `employee_components` (effective-dated)

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `employee_id` | `uuid` | NOT NULL | — | FK → `employees` |
| `component_id` | `uuid` | NOT NULL | — | FK → `component_definitions` |
| `amount` | `numeric(18,2)` | NULL | — | Override `default_amount` (`FIXED`/`PER_DAY`) |
| `rate` | `numeric(12,6)` | NULL | — | Override `rate` (`PERCENT_OF` non-statutory) |
| `valid_during` | `daterange` | NOT NULL | — | |
| [M] [H] | | | | |

- **Constraint:** `ex_employee_components_no_overlap EXCLUDE USING gist (tenant_id WITH =, employee_id WITH =, component_id WITH =, valid_during WITH &&)` (PI-09); `ck_employee_components_range`; `ck_employee_components_amount CHECK (amount IS NULL OR amount >= 0)`; `ck_employee_components_rate CHECK (rate IS NULL OR rate BETWEEN 0 AND 1)`.
- **Index:** `ix_employee_components_employee (tenant_id, employee_id)` (batch chunk, `07` §4).
- **Trigger:** `employee_components_period_guard` (`guard_period_range`).

### 5.4 `salary_assignments` (effective-dated)

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `employee_id` | `uuid` | NOT NULL | — | FK → `employees` |
| `base_salary` | `numeric(18,2)` | NOT NULL | — | Gaji pokok bulanan |
| `valid_during` | `daterange` | NOT NULL | — | |
| `change_reason` | `text` | NULL | — | "Kenaikan tahunan" |
| [M] [H] | | | | |

- **Constraint:** `ex_salary_assignments_no_overlap EXCLUDE USING gist (tenant_id WITH =, employee_id WITH =, valid_during WITH &&)` (PI-09); `ck_salary_assignments_range`; `ck_salary_assignments_non_negative CHECK (base_salary >= 0)`.
- **Index:** `ix_salary_assignments_employee (tenant_id, employee_id)`.
- **Trigger:** `salary_assignments_period_guard` (`guard_period_range`).
- **Pola perubahan gaji** (satu transaksi): `UPDATE … SET valid_during = daterange(lower(valid_during), $new_from)` pada record yang berlaku, lalu `INSERT` record baru `[$new_from, ∞)`. EXCLUDE menjamin tidak ada irisan. Celah (coverage) dijaga aplikasi + warning engine `NO_SALARY`.

---

## 6. Time & Attendance

### 6.1 `attendance_records`

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `employee_id` | `uuid` | NOT NULL | — | FK → `employees` |
| `work_date` | `date` | NOT NULL | — | Tanggal kerja lokal tenant |
| `clock_in_at` | `timestamptz` | NULL | — | Waktu server (UTC) |
| `clock_out_at` | `timestamptz` | NULL | — | |
| `status` | `text` | NOT NULL | `'INCOMPLETE'` | `INCOMPLETE`, `COMPLETE` |
| `source` | `text` | NOT NULL | — | `CLOCK`, `IMPORT`, `CORRECTION` |
| `import_id` | `uuid` | NULL | — | FK → `attendance_imports` (jika dari import) |
| `correction_reason` | `text` | NULL | — | Wajib jika `source = 'CORRECTION'` |
| [M] [H] | | | | |

- **Constraint:** `uq_attendance_records_day UNIQUE (tenant_id, employee_id, work_date)`: kunci idempotensi clock-in dan upsert import; `ck_attendance_records_order CHECK (clock_out_at IS NULL OR clock_in_at IS NULL OR clock_out_at > clock_in_at)`; `ck_attendance_records_complete CHECK ((status = 'COMPLETE') = (clock_in_at IS NOT NULL AND clock_out_at IS NOT NULL))`; `ck_attendance_records_correction CHECK (source <> 'CORRECTION' OR correction_reason IS NOT NULL)`; enum CHECK `status`, `source`.
- **Index:** unique di atas melayani "absensi employee X di periode" (range scan `work_date`). Tambahan: `ix_attendance_records_date (tenant_id, work_date)` untuk rekap harian HR.
- **Trigger:** `attendance_records_period_guard` (`guard_period_date` pada `work_date`; untuk UPDATE juga mengecek `OLD.work_date`).
- **Catatan shift malam:** `work_date` = tanggal clock-in lokal. Clock-out boleh jatuh di tanggal berikutnya.

### 6.2 `attendance_imports` (O7)

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `status` | `text` | NOT NULL | `'UPLOADED'` | `UPLOADED`, `PROCESSING`, `COMPLETED`, `FAILED` |
| `mode` | `text` | NOT NULL | `'PARTIAL'` | `PARTIAL` (baris valid tetap masuk), `ALL_OR_NOTHING` |
| `file_hash` | `bytea` | NOT NULL | — | SHA-256 isi file |
| `object_key` | `text` | NOT NULL | — | `tenants/{tenant_id}/imports/{id}.csv` (ADR-012) |
| `original_filename` | `text` | NOT NULL | — | Hanya untuk tampilan; tidak pernah dipakai sebagai path |
| `size_bytes` | `int` | NOT NULL | — | |
| `rows_total` / `rows_ok` / `rows_failed` | `int` | NULL | — | Diisi saat selesai |
| `failure_reason` | `text` | NULL | — | Kegagalan level file (format, ukuran) |
| `started_at` / `completed_at` | `timestamptz` | NULL | — | |
| [M] [H] | | | | |

- **Constraint:**
  - `uq_attendance_imports_file` unique index `(tenant_id, file_hash) WHERE status <> 'FAILED'`. Upload ulang file identik → `409 IMPORT_DUPLICATE` berisi ID import sebelumnya (ini bentuk "peringatan" di PF-21). Karena upsert idempoten, menjalankan ulang file yang sama tidak ada gunanya. File yang import-nya `FAILED` boleh diunggah ulang.
  - `ck_attendance_imports_size CHECK (size_bytes BETWEEN 1 AND 5242880)` (5 MB, threat model menetapkan final); `ck_attendance_imports_counts CHECK (rows_ok + rows_failed <= rows_total)`; `ck_attendance_imports_object_key CHECK (object_key LIKE 'tenants/' || tenant_id::text || '/imports/%')`. Object key wajib ber-prefix tenant (ditegakkan DB).
- **Index:** `ix_attendance_imports_recent (tenant_id, created_at DESC)`.

### 6.3 `attendance_import_errors`

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `import_id` | `uuid` | NOT NULL | — | FK → `attendance_imports` |
| `row_number` | `int` | NOT NULL | — | Baris CSV (1 = baris data pertama) |
| `employee_no` | `text` | NULL | — | Nilai mentah (untuk laporan) |
| `error_code` | `text` | NOT NULL | — | `EMPLOYEE_NOT_FOUND`, `INVALID_DATE`, `PERIOD_LOCKED`, `INVALID_TIME_ORDER`, … |
| `message` | `text` | NOT NULL | — | Pesan ramah, tanpa PII |
| [A] | | | | |

- **Constraint:** `uq_attendance_import_errors_row UNIQUE (tenant_id, import_id, row_number, error_code)`. Retry batch import tidak menggandakan error (`ON CONFLICT DO NOTHING`).
- **Trigger:** append-only (`forbid_update_delete`), kecuali penghapusan oleh job retensi yang memakai role owner.

### 6.4 `leave_types`, `leave_balances`, `leave_requests`, `overtime_requests`

**`leave_types`**

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `code` | `text` | NOT NULL | — | `ANNUAL`, `SICK`, `UNPAID` |
| `name` | `text` | NOT NULL | — | |
| `is_paid` | `boolean` | NOT NULL | `true` | `false` → potongan `ABSENCE_UNPAID_LEAVE` |
| `annual_quota_days` | `numeric(5,1)` | NULL | — | NULL = tanpa kuota (contoh: unpaid) |
| `max_negative_days` | `numeric(5,1)` | NOT NULL | `0` | Batas saldo minus yang diizinkan |
| `status` | `text` | NOT NULL | `'ACTIVE'` | |
| [M] [H] | | | | |

Constraint: `uq_leave_types_code UNIQUE (tenant_id, code)`; `ck_leave_types_quota CHECK (annual_quota_days IS NULL OR annual_quota_days >= 0)`; `ck_leave_types_negative CHECK (max_negative_days >= 0)`.

**`leave_balances`** (PI-10)

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `employee_id` | `uuid` | NOT NULL | — | FK → `employees` |
| `leave_type_id` | `uuid` | NOT NULL | — | FK → `leave_types` |
| `year` | `smallint` | NOT NULL | — | Tahun kuota |
| `balance_days` | `numeric(6,1)` | NOT NULL | — | Saldo saat ini |
| `min_allowed_days` | `numeric(6,1)` | NOT NULL | `0` | Denormalisasi `−leave_types.max_negative_days` |
| [M] | | | | |

- **Constraint:** `uq_leave_balances_key UNIQUE (tenant_id, employee_id, leave_type_id, year)`; `ck_leave_balances_min CHECK (balance_days >= min_allowed_days)` (PI-10); `ck_leave_balances_min_non_positive CHECK (min_allowed_days <= 0)`.
- **Catatan:** CHECK tidak bisa mereferensikan tabel lain, sehingga batas minus didenormalisasi. Saat `leave_types.max_negative_days` diubah, use case yang sama meng-update `min_allowed_days` semua saldo tipe itu dalam satu transaksi. Approve cuti: `SELECT … FOR UPDATE` pada baris saldo, lalu kurangi saldo. Pelanggaran CHECK berarti saldo tidak cukup → rollback → `422 INSUFFICIENT_LEAVE_BALANCE`.

**`leave_requests`**

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `employee_id` | `uuid` | NOT NULL | — | FK → `employees` |
| `leave_type_id` | `uuid` | NOT NULL | — | FK → `leave_types` |
| `dates` | `daterange` | NOT NULL | — | `[mulai, selesai+1)` |
| `days` | `numeric(5,1)` | NOT NULL | — | Hari kerja dalam rentang (dihitung saat submit dari kalender tenant) |
| `reason` | `text` | NULL | — | |
| `status` | `text` | NOT NULL | `'PENDING'` | `PENDING`, `APPROVED`, `REJECTED`, `CANCELLED` |
| `approved_at` | `timestamptz` | NULL | — | Denormalisasi `approval_requests.decided_at` untuk filter cut-off (PI-08) |
| `cancels_request_id` | `uuid` | NULL | — | Untuk "pembatalan cuti yang sudah APPROVED" sebagai request baru (ADR-P09); FK → `leave_requests` |
| [M] [H] | | | | |

- **Constraint:** `ex_leave_requests_no_overlap EXCLUDE USING gist (tenant_id WITH =, employee_id WITH =, dates WITH &&) WHERE (status IN ('PENDING','APPROVED') AND cancels_request_id IS NULL)`; `ck_leave_requests_range`; `ck_leave_requests_days CHECK (days > 0)`; `ck_leave_requests_approved CHECK ((status = 'APPROVED') = (approved_at IS NOT NULL))`; enum CHECK `status`.
- **Index:** `ix_leave_requests_employee (tenant_id, employee_id, status)`; `ix_leave_requests_created (tenant_id, created_at DESC, id)` untuk list tenant-wide `GET /leave-requests` (final review R3-3); input payroll memakai `(tenant_id, employee_id)` + filter `dates && period` + `status = 'APPROVED'`.
- **Trigger:** `leave_requests_period_guard` (`guard_period_range` pada `dates`).

**`overtime_requests`**

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `employee_id` | `uuid` | NOT NULL | — | FK → `employees` |
| `work_date` | `date` | NOT NULL | — | Tanggal lembur (lokal) |
| `start_at` / `end_at` | `timestamptz` | NOT NULL | — | |
| `hours` | `numeric(6,2)` | NOT NULL | — | Dihitung aplikasi dari start/end, dibulatkan ke 0,25 jam (kebijakan tenant di V2) |
| `reason` | `text` | NOT NULL | — | |
| `status` | `text` | NOT NULL | `'PENDING'` | Sama dengan cuti |
| `approved_at` | `timestamptz` | NULL | — | Untuk cut-off (PI-08) |
| [M] [H] | | | | |

- **Constraint:** `ck_overtime_requests_order CHECK (end_at > start_at)`; `ck_overtime_requests_hours CHECK (hours > 0 AND hours <= 24)`; `ex_overtime_requests_no_overlap EXCLUDE USING gist (tenant_id WITH =, employee_id WITH =, tstzrange(start_at, end_at) WITH &&) WHERE (status IN ('PENDING','APPROVED'))`; `ck_overtime_requests_approved` (sama dengan cuti).
- **Index:** `ix_overtime_requests_employee_date (tenant_id, employee_id, work_date)`; `ix_overtime_requests_created (tenant_id, created_at DESC, id)` untuk list tenant-wide (final review R3-3).
- **Trigger:** `overtime_requests_period_guard` (`guard_period_date` pada `work_date`).

### 6.5 Fungsi PeriodGuard di DB (PI-14)

`PeriodGuard.EnsureWritable` di aplikasi memberi error yang ramah (`409 PERIOD_LOCKED`). Trigger berikut adalah **jaring pengaman** untuk jalur yang lupa memanggil guard (desain domain PI-14).

```sql
-- Tanggal tunggal (attendance, overtime, holidays)
CREATE FUNCTION payrun.guard_period_date() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE col text := TG_ARGV[0];  -- nama kolom tanggal, mis. 'work_date'
        d_new date; d_old date;
BEGIN
  IF TG_OP <> 'DELETE' THEN EXECUTE format('SELECT ($1).%I', col) INTO d_new USING NEW; END IF;
  IF TG_OP <> 'INSERT' THEN EXECUTE format('SELECT ($1).%I', col) INTO d_old USING OLD; END IF;
  IF EXISTS (SELECT 1 FROM payrun.payroll_periods p
             WHERE p.tenant_id = COALESCE(NEW.tenant_id, OLD.tenant_id)
               AND p.status = 'FINALIZED'
               AND (p.period_range @> d_new OR p.period_range @> d_old)) THEN
    RAISE EXCEPTION 'period locked' USING ERRCODE = 'P0002';
  END IF;
  RETURN COALESCE(NEW, OLD);
END $$;
```

```sql
-- Rentang (salary, components, employment, leave): hanya tanggal yang NILAINYA berubah yang dicek
CREATE FUNCTION payrun.guard_period_range() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE col text := TG_ARGV[0]; r_new daterange; r_old daterange; changed daterange[];
BEGIN
  IF TG_OP <> 'DELETE' THEN EXECUTE format('SELECT ($1).%I', col) INTO r_new USING NEW; END IF;
  IF TG_OP <> 'INSERT' THEN EXECUTE format('SELECT ($1).%I', col) INTO r_old USING OLD; END IF;

  IF TG_OP = 'INSERT' THEN changed := ARRAY[r_new];
  ELSIF TG_OP = 'DELETE' THEN changed := ARRAY[r_old];
  ELSIF (to_jsonb(NEW) - col - 'updated_at' - 'updated_by' - 'version')
        IS DISTINCT FROM (to_jsonb(OLD) - col - 'updated_at' - 'updated_by' - 'version') THEN
    changed := ARRAY[r_old, r_new];                       -- nilai lain berubah: seluruh rentang terdampak
  ELSIF lower(r_old) = lower(r_new) THEN                  -- hanya ujung kanan bergeser (kasus "tutup record")
    changed := ARRAY[daterange(least(upper(r_old), upper(r_new)), greatest(upper(r_old), upper(r_new)))];
  ELSIF upper(r_old) IS NOT DISTINCT FROM upper(r_new) THEN
    changed := ARRAY[daterange(least(lower(r_old), lower(r_new)), greatest(lower(r_old), lower(r_new)))];
  ELSE changed := ARRAY[r_old, r_new];
  END IF;

  IF EXISTS (SELECT 1 FROM payrun.payroll_periods p, unnest(changed) c
             WHERE p.tenant_id = COALESCE(NEW.tenant_id, OLD.tenant_id)
               AND p.status = 'FINALIZED' AND p.period_range && c) THEN
    RAISE EXCEPTION 'period locked' USING ERRCODE = 'P0002';
  END IF;
  RETURN COALESCE(NEW, OLD);
END $$;
```

**Kenapa hanya rentang yang berubah:** gaji `[2026-01-01, ∞)` berlaku untuk Maret yang sudah `FINALIZED`. Menutupnya di 15 April (kenaikan gaji April) mengubah rentang menjadi `[2026-01-01, 2026-04-15)`. Rentang lama dan baru sama-sama beririsan dengan Maret, tetapi tanggal yang *nilainya berubah* hanya `[2026-04-15, ∞)`. Pengecekan naif akan memblokir setiap kenaikan gaji setelah periode pertama difinalisasi. Ini contoh bagus untuk interview: aturan bisnis "jangan ubah periode final" harus diterjemahkan ke "jangan ubah *nilai yang berlaku* di tanggal periode final".

| Tabel | Trigger | Argumen |
|---|---|---|
| `attendance_records` | `guard_period_date` | `'work_date'` |
| `overtime_requests` | `guard_period_date` | `'work_date'` |
| `holidays` | `guard_period_date` | `'holiday_date'` |
| `leave_requests` | `guard_period_range` | `'dates'` |
| `salary_assignments`, `employee_components` | `guard_period_range` | `'valid_during'` |
| `employment_spans` | `guard_period_range` | `'span'` |

Index `ix_payroll_periods_finalized (tenant_id, period_range) USING gist WHERE status = 'FINALIZED'` membuat pengecekan trigger murah.

### 6.6 `approval_requests`

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `subject_type` | `text` | NOT NULL | — | `LEAVE_REQUEST`, `OVERTIME_REQUEST`, `PAYROLL_RUN` |
| `subject_id` | `uuid` | NOT NULL | — | ID subjek (polimorfik, tanpa FK fisik) |
| `requester_id` | `uuid` | NOT NULL | — | FK `→ users(id)` |
| `approver_id` | `uuid` | NOT NULL | — | FK `→ users(id)`; di-snapshot saat submit |
| `status` | `text` | NOT NULL | `'PENDING'` | `PENDING`, `APPROVED`, `REJECTED`, `CANCELLED` |
| `decision_note` | `text` | NULL | — | Wajib untuk `REJECTED` |
| `decided_by` | `uuid` | NULL | — | Biasanya = `approver_id`; berbeda jika HR override |
| `is_override` | `boolean` | NOT NULL | `false` | HR_ADMIN memutuskan menggantikan approver (ter-audit) |
| `self_approved` | `boolean` | NOT NULL | `false` | Pengecualian satu admin (ADR-P09) |
| `self_approval_reason` | `text` | NULL | — | |
| `decided_at` | `timestamptz` | NULL | — | |
| [M] | | | | |

- **Constraint:**
  - `uq_approval_requests_pending` unique index `(tenant_id, subject_type, subject_id) WHERE status = 'PENDING'` (PI-11).
  - `ck_approval_requests_not_self CHECK (approver_id <> requester_id OR (self_approved AND subject_type = 'PAYROLL_RUN' AND self_approval_reason IS NOT NULL))` (PI-11, versi terkoreksi desain domain).
  - `ck_approval_requests_self_flag CHECK (NOT self_approved OR approver_id = requester_id)`.
  - `ck_approval_requests_decided CHECK ((status = 'PENDING') = (decided_at IS NULL))`.
  - `ck_approval_requests_reject_note CHECK (status <> 'REJECTED' OR decision_note IS NOT NULL)`.
  - `ck_approval_requests_override CHECK (NOT is_override OR decided_by IS DISTINCT FROM approver_id)`.
  - Enum CHECK `subject_type`, `status`.
- **Index:** `ix_approval_requests_inbox (tenant_id, approver_id, created_at DESC) WHERE status = 'PENDING'` (approval inbox manager); `ix_approval_requests_subject (tenant_id, subject_type, subject_id)` (riwayat keputusan per subjek).
- **Catatan polimorfik:** tidak ada FK fisik ke subjek. Integritasnya dijaga karena approval **hanya** dibuat oleh use case submit subjek di transaksi yang sama, dan subjek tidak pernah dihapus (strategi soft delete). Ini satu-satunya referensi tanpa FK di skema bisnis, dan pengecualian ini diterima secara sadar. Alternatif tiga kolom FK nullable (`leave_request_id`, `overtime_request_id`, `payroll_run_id`) + CHECK "tepat satu terisi" dicatat sebagai opsi kalau integritas ini terbukti bermasalah.

---

## 7. Payroll

### 7.1 `payroll_periods`

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `year` | `smallint` | NOT NULL | — | |
| `month` | `smallint` | NOT NULL | — | |
| `period_range` | `daterange` | NOT NULL | *generated* | `GENERATED ALWAYS AS (daterange(make_date(year, month, 1), (make_date(year, month, 1) + interval '1 month')::date)) STORED` |
| `cutoff_at` | `timestamptz` | NOT NULL | — | Dihitung dari `tenant_settings` saat periode dibuat, lalu **dibekukan** di sini (determinisme) |
| `status` | `text` | NOT NULL | `'OPEN'` | `OPEN`, `PROCESSING`, `FINALIZED` |
| `finalized_run_id` | `uuid` | NULL | — | FK → `payroll_runs` |
| `finalized_at` | `timestamptz` | NULL | — | |
| [M] | | | | |

- **Constraint:** `uq_payroll_periods_month UNIQUE (tenant_id, year, month)`; `ck_payroll_periods_month CHECK (month BETWEEN 1 AND 12)`; `ck_payroll_periods_year CHECK (year BETWEEN 2000 AND 2100)`; `ck_payroll_periods_status CHECK (status IN ('OPEN','PROCESSING','FINALIZED'))`; `ck_payroll_periods_finalized CHECK ((status = 'FINALIZED') = (finalized_run_id IS NOT NULL AND finalized_at IS NOT NULL))`; `ck_payroll_periods_cutoff CHECK (cutoff_at >= lower(period_range)::timestamptz)` (kasar; zona waktu ditangani aplikasi).
- **Index:** `ix_payroll_periods_finalized` (GiST, §6.5).
- **Trigger:** `payroll_periods_final_immutable`: BEFORE UPDATE/DELETE jika `OLD.status = 'FINALIZED'` → `P0001`. `FINALIZED` adalah status terminal.
- **Catatan:** `make_date` dan cast `timestamp → date` immutable, sehingga boleh dipakai di generated column. FK `finalized_run_id` dan FK `payroll_runs.period_id` membentuk siklus referensi. Keduanya dibuat sebagai FK biasa karena periode dibuat lebih dulu dan `finalized_run_id` diisi belakangan; tidak perlu `DEFERRABLE`.

### 7.2 `payroll_runs`

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `period_id` | `uuid` | NOT NULL | — | FK → `payroll_periods` |
| `status` | `text` | NOT NULL | `'DRAFT'` | `DRAFT`, `CALCULATING`, `CALCULATED`, `PENDING_APPROVAL`, `APPROVED`, `FINALIZED`, `FAILED`, `CANCELLED` |
| `calc_version` | `int` | NOT NULL | `0` | Naik setiap calculate/recalculate |
| `chunks_total` | `int` | NOT NULL | `0` | |
| `chunks_done` | `int` | NOT NULL | `0` | |
| `eligible_count` | `int` | NULL | — | Diisi `payroll.plan`; dipakai verifikasi `count(items) = eligible_count` |
| `input_fingerprint` | `bytea` | NULL | — | Run fingerprint untuk `calc_version` terkini (ADR-P07) |
| `ruleset_code` / `ruleset_version` | `text` | NULL | — | Versi yang dipakai kalkulasi terkini |
| `total_gross` / `total_deductions` / `total_net` | `numeric(18,2)` | NULL | — | Σ item `OK` @ `calc_version` (PI-05) |
| `item_ok_count` / `item_error_count` / `item_excluded_count` / `warning_count` | `int` | NULL | — | Ringkasan review |
| `failure_reason` | `text` | NULL | — | Untuk `FAILED` |
| `calc_started_at` / `calculated_at` | `timestamptz` | NULL | — | **(desain transaksi & concurrency)** `calc_started_at` (sebelumnya `calculation_started_at`) = `clock_timestamp()` yang dicatat calculate **sambil memegang `payroll_periods FOR UPDATE`**; batas bawah kandidat stale O12. Juga dipakai untuk metrik durasi |
| `submitted_by` / `submitted_at` | `uuid` / `timestamptz` | NULL | — | |
| `approved_by` / `approved_at` | `uuid` / `timestamptz` | NULL | — | |
| `finalized_by` / `finalized_at` | `uuid` / `timestamptz` | NULL | — | |
| `cancelled_by` / `cancelled_at` | `uuid` / `timestamptz` | NULL | — | |
| [M] `created_by` | | | | Tanpa `version`: transisi dijaga guard `WHERE status = $expected` |

- **Constraint:**
  - `uq_payroll_runs_active_per_period` unique index `(tenant_id, period_id) WHERE status <> 'CANCELLED'` (PI-03).
  - `ck_payroll_runs_status` (8 nilai).
  - `ck_payroll_runs_chunks CHECK (chunks_done BETWEEN 0 AND chunks_total)`.
  - `ck_payroll_runs_calculated_totals CHECK (status IN ('DRAFT','CALCULATING','FAILED','CANCELLED') OR (total_net IS NOT NULL AND input_fingerprint IS NOT NULL AND total_net = total_gross - total_deductions))`.
  - `ck_payroll_runs_finalized CHECK ((status = 'FINALIZED') = (finalized_at IS NOT NULL))`.
  - `ck_payroll_runs_fingerprint CHECK (input_fingerprint IS NULL OR octet_length(input_fingerprint) = 32)`.
  - **(desain transaksi & concurrency)** `ck_payroll_runs_calc_started CHECK (status IN ('DRAFT','CANCELLED') OR calc_started_at IS NOT NULL)`: setiap run yang pernah dikalkulasi punya batas bawah O12. `CANCELLED` dikecualikan karena run bisa dibatalkan dari `DRAFT`.
- **Index:** `ix_payroll_runs_period (tenant_id, period_id, created_at DESC)` (riwayat run per periode, termasuk yang dibatalkan).
- **Trigger:**
  - `payroll_runs_terminal_immutable` BEFORE UPDATE OR DELETE: `OLD.status IN ('FINALIZED','CANCELLED')` → `P0001` (PI-04).
  - `payroll_runs_totals_check` BEFORE UPDATE OF `status` ketika `NEW.status IN ('CALCULATED','FINALIZED')` (PI-05):
    ```sql
    SELECT coalesce(sum(gross),0), coalesce(sum(deductions),0), coalesce(sum(net),0), count(*)
      INTO g, d, n, c
    FROM payrun.payroll_items
    WHERE tenant_id = NEW.tenant_id AND run_id = NEW.id
      AND calc_version = NEW.calc_version AND status = 'OK';
    IF (g, d, n, c) IS DISTINCT FROM (NEW.total_gross, NEW.total_deductions, NEW.total_net, NEW.item_ok_count) THEN
      RAISE EXCEPTION 'run totals mismatch' USING ERRCODE = 'P0006';
    END IF;
    ```

**Keputusan PI-05 (konsistensi total):** total **dihitung** dengan `SUM` di transaksi chunk terakhir, bukan diakumulasi per chunk, lalu **diverifikasi ulang oleh trigger** setiap kali run masuk `CALCULATED` atau `FINALIZED`. Konsistensi item ↔ line dijaga deferred trigger di `payroll_items` (§7.3). Ditambah job `payrun verify-run` harian sebagai deteksi. Alasan memilih trigger saat transisi daripada trigger di setiap INSERT item: run belum punya total yang bermakna selama `CALCULATING`. Satu agregasi saat transisi lebih murah (sekali per run, terindeks) dan tetap menjamin tidak ada total salah yang pernah terlihat di status `CALCULATED` atau sesudahnya.

### 7.3 `payroll_items`

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `run_id` | `uuid` | NOT NULL | — | FK → `payroll_runs` |
| `employee_id` | `uuid` | NOT NULL | — | FK → `employees` |
| `calc_version` | `int` | NOT NULL | — | |
| `status` | `text` | NOT NULL | — | `OK`, `ERROR`, `EXCLUDED` |
| `gross` / `deductions` / `net` | `numeric(18,2)` | NULL | — | NULL untuk `ERROR` tanpa baris (contoh: `NO_SALARY`) |
| `error_codes` | `text[]` | NOT NULL | `'{}'` | `NO_SALARY`, `NEGATIVE_NET`, … (`07-payroll-engine.md` §13) |
| `warning_codes` | `text[]` | NOT NULL | `'{}'` | `OVERTIME_OVER_LIMIT`, … |
| `issues` | `jsonb` | NOT NULL | `'[]'` | Detail error/warning `{code, message, data}` untuk UI; tidak di-query |
| `input_fingerprint` | `bytea` | NOT NULL | — | Fingerprint per karyawan |
| `employee_no` / `employee_name` / `department_id` | `text` / `text` / `uuid` | NOT NULL / NOT NULL / NULL | — | **(final review R3-4)** Denormalisasi saat kalkulasi (diambil dari snapshot input), sehingga list/filter item run historis memakai data *pada saat itu*, bukan data live. Bukan PII sensitif (nama tetap di bawah RLS) |
| `excluded_reason` / `excluded_by` / `excluded_at` | `text` / `uuid` / `timestamptz` | NULL | — | Exclude eksplisit (ter-audit) |
| `created_at` | `timestamptz` | NOT NULL | `now()` | |

- **Constraint:**
  - `uq_payroll_items_version UNIQUE (tenant_id, run_id, employee_id, calc_version)` (PI-06). Target `ON CONFLICT` saat chunk di-retry.
  - `ck_payroll_items_status CHECK (status IN ('OK','ERROR','EXCLUDED'))`.
  - `ck_payroll_items_ok_amounts CHECK (status <> 'OK' OR (gross IS NOT NULL AND deductions IS NOT NULL AND net IS NOT NULL AND net = gross - deductions AND net >= 0))`. `net >= 0` untuk item `OK` adalah konsekuensi keputusan `NEGATIVE_NET = ERROR`.
  - `ck_payroll_items_ok_no_errors CHECK (status <> 'OK' OR cardinality(error_codes) = 0)`.
  - `ck_payroll_items_error_has_code CHECK (status <> 'ERROR' OR cardinality(error_codes) > 0)`.
  - `ck_payroll_items_excluded CHECK ((status = 'EXCLUDED') = (excluded_reason IS NOT NULL AND excluded_by IS NOT NULL))`.
- **Index:** unique di atas melayani "item per run per versi" (review, total, finalize) dan upsert. Tambahan: `ix_payroll_items_employee (tenant_id, employee_id, created_at DESC)` untuk riwayat gaji karyawan dan diff bulan lalu (PF-23). **(final review R3-4)** `ix_payroll_items_review (tenant_id, run_id, calc_version, employee_no, id)` untuk cursor list item urut nomor karyawan; `ix_payroll_items_dept (tenant_id, run_id, department_id)` untuk filter departemen.
- **Trigger:**
  - `payroll_items_immutable` (PI-04), BEFORE INSERT OR UPDATE OR DELETE:
    ```sql
    CREATE FUNCTION payrun.forbid_change_on_finalized_run() RETURNS trigger LANGUAGE plpgsql AS $$
    DECLARE t uuid := COALESCE(NEW.tenant_id, OLD.tenant_id);
            r uuid := COALESCE(NEW.run_id, OLD.run_id);
    BEGIN
      -- FOR SHARE: mengunci baris run agar terserialisasi dengan transaksi finalize
      -- (yang meng-UPDATE baris yang sama). Tanpa lock, di Read Committed sebuah chunk
      -- zombie bisa lolos saat finalize belum commit, lalu keduanya commit.
      IF EXISTS (SELECT 1 FROM payrun.payroll_runs pr
                 WHERE pr.tenant_id = t AND pr.id = r AND pr.status = 'FINALIZED'
                   -- final review (R3-2): cleanup batch boleh menghapus baris versi lama
                   AND NOT (TG_OP = 'DELETE' AND OLD.calc_version < pr.calc_version)
                 FOR SHARE) THEN
        RAISE EXCEPTION 'payroll run % is finalized', r USING ERRCODE = 'P0001';
      END IF;
      RETURN COALESCE(NEW, OLD);
    END $$;
    -- dipasang di payroll_items, payroll_lines, payroll_item_snapshots
    ```
    Mencakup INSERT, bukan hanya UPDATE/DELETE: chunk zombie atau bug yang menambah item ke run final juga ditolak. **Race yang ditutup:** lock `FOR SHARE` bertabrakan dengan `UPDATE payroll_runs SET status='FINALIZED'`. Kalau finalize commit lebih dulu, INSERT menunggu, lalu melihat `FINALIZED` dan ditolak. Kalau chunk commit lebih dulu, finalize menunggu, lalu guard fingerprint/`chunks_done` menolaknya. Lapis lain yang sudah ada: fencing `CompleteInTx` (ADR-006) dan guard `calc_version`.
  - `payroll_items_sum_check` (PI-05, item ↔ line), constraint trigger `AFTER INSERT OR UPDATE … DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.status = 'OK')`: memeriksa `gross = Σ line EARNING`, `deductions = Σ line DEDUCTION`. Deferred karena line di-INSERT setelah item dalam transaksi yang sama.
- **Catatan versi lama (final review R3-2):** item `calc_version` lama **tidak** dihapus di transaksi finalize/calculate. Job `payroll.cleanup_versions` menghapusnya secara batch (1.000 baris per tx) setelah run `CALCULATED`/`FINALIZED`, dengan cascade ke line dan snapshot (FK `ON DELETE CASCADE`). Semua query baca selalu memfilter `calc_version = run.calc_version`, jadi baris lama yang belum dibersihkan tidak pernah terlihat. Trigger PI-04 mengizinkan **hanya** DELETE baris versi lama (`OLD.calc_version < run.calc_version`) pada run final, karena baris itu bukan bagian dari hasil final; INSERT/UPDATE/DELETE versi terkini tetap ditolak.

### 7.4 `payroll_lines`

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `run_id` | `uuid` | NOT NULL | — | Denormalisasi untuk trigger immutability dan kandidat kunci partisi |
| `calc_version` | `int` | NOT NULL | — | **(final review)** Denormalisasi dari item, dipakai trigger PI-04 (izin DELETE versi lama) dan unique per versi |
| `item_id` | `uuid` | NOT NULL | — | FK → `payroll_items` `ON DELETE CASCADE` |
| `sort_order` | `smallint` | NOT NULL | — | Urutan tampil di slip (= urutan pipeline) |
| `code` | `text` | NOT NULL | — | `BASE_SALARY`, `OVERTIME`, `BPJS_JHT_EE`, `ADJUSTMENT`, … |
| `category` | `text` | NOT NULL | — | `EARNING`, `DEDUCTION` |
| `stage` | `text` | NOT NULL | — | |
| `amount` | `numeric(18,2)` | NOT NULL | — | Sudah dibulatkan |
| `basis` | `jsonb` | NOT NULL | `'{}'` | Data terstruktur (`{"rate":"0.02","base":"6800000"}`) |
| `explanation` | `text` | NOT NULL | — | Kalimat yang tampil di slip |
| `taxable` | `boolean` | NOT NULL | — | |
| `rounding` | `text` | NOT NULL | — | Mode yang dipakai |
| `adjustment_id` | `uuid` | NULL | — | FK → `adjustments` untuk baris koreksi |
| [A] `created_at` | | | | |

- **Constraint:** `uq_payroll_lines_order UNIQUE (tenant_id, item_id, sort_order)`; `ck_payroll_lines_amount CHECK (amount >= 0)` (arah ditentukan `category`, bukan tanda, sehingga adjustment negatif menjadi line `DEDUCTION`); `ck_payroll_lines_adjustment CHECK ((code = 'ADJUSTMENT') = (adjustment_id IS NOT NULL))`; enum CHECK `category`, `rounding`.
- **Index:** unique di atas melayani "line per item" (slip, review). `uq_payroll_lines_adjustment_per_run` unique index `(tenant_id, run_id, calc_version, adjustment_id) WHERE adjustment_id IS NOT NULL`: satu adjustment hanya muncul sekali per versi kalkulasi run (PI-15, lapis kedua). **(final review)** `calc_version` wajib ada di index ini, karena cleanup versi lama sekarang asinkron (R3-2): saat recalculation, line versi lama dan baru bisa hidup bersamaan.
- **Trigger:** `payroll_lines_immutable` (`forbid_change_on_finalized_run`) + `payroll_lines_no_update` BEFORE UPDATE → `forbid_update_delete`. Line tidak pernah di-UPDATE. **DELETE tidak** diblokir trigger generik, karena line versi lama memang dihapus lewat cascade sebelum run final. Setelah final, DELETE diblokir oleh `forbid_change_on_finalized_run`.

### 7.5 `payroll_item_snapshots`

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| `tenant_id` | `uuid` | NOT NULL | — | |
| `item_id` | `uuid` | NOT NULL | — | `PRIMARY KEY (tenant_id, item_id)`, FK → `payroll_items` `ON DELETE CASCADE` (1:1) |
| `run_id` | `uuid` | NOT NULL | — | Untuk trigger immutability |
| `calc_version` | `int` | NOT NULL | — | **(final review)** Denormalisasi dari item (trigger PI-04) |
| `snapshot_version` | `smallint` | NOT NULL | — | Versi skema `EmployeePeriodInput` |
| `input` | `jsonb` | NOT NULL | — | `EmployeePeriodInput` kanonik lengkap (kalender, cut-off, span, komponen, absensi, lembur, adjustment, policy) |
| `ruleset_code` / `ruleset_version` | `text` | NOT NULL | — | |
| `input_fingerprint` | `bytea` | NOT NULL | — | = `payroll_items.input_fingerprint` |
| [A] `created_at` | | | | |

- **Keputusan: snapshot ditulis saat item dihitung (di transaksi chunk), bukan saat finalize** (mengikuti ADR-P07). Finalize tidak perlu membaca ulang dan menyalin data, sehingga transaksinya pendek. Snapshot otomatis ikut membeku lewat trigger saat run `FINALIZED`.
- **Kenapa tabel terpisah dari `payroll_items`:** `input` berukuran beberapa KB per karyawan (kalender + absensi harian). Memisahkannya membuat scan review/total atas `payroll_items` tetap sempit, dan snapshot hanya dibaca saat verify/sengketa. PostgreSQL akan men-TOAST kolom besar, tetapi tabel terpisah juga memisahkan pola akses dan retensi.
- **Constraint:** `ck_payroll_item_snapshots_sha256 CHECK (octet_length(input_fingerprint) = 32)`.
- **Trigger:** `payroll_item_snapshots_immutable` (`forbid_change_on_finalized_run`) + BEFORE UPDATE → `forbid_update_delete`. Snapshot tidak pernah di-UPDATE; versi baru = item baru.

### 7.6 `adjustments`

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `employee_id` | `uuid` | NOT NULL | — | FK → `employees` |
| `origin_period_id` | `uuid` | NOT NULL | — | FK → `payroll_periods` (harus `FINALIZED`, dicek app) |
| `target_year` / `target_month` | `smallint` / `smallint` | NOT NULL | — | **(final review R1-4)** Periode target sebagai nilai, **bukan FK**: periode target biasanya belum ada saat adjustment dibuat. Diresolusi saat run periode itu dibuat/dikalkulasi (InputProvider mencari `PENDING_APPLY` dengan tahun-bulan yang sama) |
| `component_code` | `text` | NOT NULL | — | `OVERTIME_CORRECTION`, `DEDUCTION_CORRECTION`, `MANUAL` |
| `amount` | `numeric(18,2)` | NOT NULL | — | Positif = tambahan, negatif = potongan |
| `taxable` | `boolean` | NOT NULL | `true` | |
| `reason` | `text` | NOT NULL | — | |
| `status` | `text` | NOT NULL | `'PENDING_APPLY'` | `PENDING_APPLY`, `APPLIED`, `CANCELLED` |
| `applied_run_id` | `uuid` | NULL | — | FK → `payroll_runs` |
| `applied_at` | `timestamptz` | NULL | — | |
| `cancelled_by` / `cancelled_at` | `uuid` / `timestamptz` | NULL | — | |
| [M] [H] | | | | |

- **Constraint:** `ck_adjustments_amount CHECK (amount <> 0)`; `ck_adjustments_target_month CHECK (target_month BETWEEN 1 AND 12)`; kondisi "target harus setelah origin" dicek APP (origin adalah FK ke periode final); `ck_adjustments_applied CHECK ((status = 'APPLIED') = (applied_run_id IS NOT NULL AND applied_at IS NOT NULL))` (PI-15); `ck_adjustments_cancelled CHECK ((status = 'CANCELLED') = (cancelled_at IS NOT NULL))`; enum CHECK.
- **Index:** `ix_adjustments_target (tenant_id, target_year, target_month, employee_id) WHERE status = 'PENDING_APPLY'` (InputProvider.Adjustments per chunk); `ix_adjustments_created (tenant_id, created_at DESC, id)` untuk `GET /adjustments` (final review R3-3).
- **Trigger:** `adjustments_applied_immutable` BEFORE UPDATE OR DELETE `WHEN (OLD.status IN ('APPLIED','CANCELLED'))` → `P0001`.
- **Catatan:** "diterapkan maksimal sekali" dijaga oleh tiga lapis: (1) target periode tunggal (tahun-bulan) + `uq_payroll_runs_active_per_period` → hanya satu run yang bisa memuatnya; (2) `uq_payroll_lines_adjustment_per_run`; (3) guard `WHERE status = 'PENDING_APPLY' AND applied_run_id IS NULL` di transaksi finalize.

### 7.6b `payroll_input_changes` (ditambahkan di desain transaksi & concurrency, O12)

Log perubahan input per (periode, karyawan). Dipakai approve/finalize sebagai **penyaring kandidat** stale, sehingga approve tidak perlu menghitung ulang fingerprint seluruh karyawan (`21-tx-global-rules.md` O12, `22-tx-concurrency.md` §3.3).

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| `tenant_id` | `uuid` | NOT NULL | — | |
| `period_id` | `uuid` | NOT NULL | — | FK `(tenant_id, period_id) → payroll_periods` |
| `employee_id` | `uuid` | NOT NULL | — | FK `(tenant_id, employee_id) → employees` |
| `changed_at` | `timestamptz` | NOT NULL | — | `clock_timestamp()` yang diambil **setelah** `PeriodGuard` mendapat `FOR SHARE` pada periode (bukan `now()`, yang adalah waktu mulai transaksi) |
| `change_count` | `int` | NOT NULL | `1` | Jumlah perubahan sejak baris dibuat (informasi untuk UI/audit) |

- **Constraint:** `PRIMARY KEY (tenant_id, period_id, employee_id)` (satu baris per karyawan per periode; ditulis dengan `INSERT … ON CONFLICT DO UPDATE SET changed_at = EXCLUDED.changed_at, change_count = change_count + 1`); `ck_payroll_input_changes_count CHECK (change_count >= 1)`.
- **Index:** `ix_payroll_input_changes_period_changed (tenant_id, period_id, changed_at)` untuk query approve `WHERE period_id = $p AND changed_at > $calc_started_at`.
- **Ditulis oleh:** `PeriodGuard` untuk setiap periode **non-final yang sudah ada** dan beririsan dengan rentang yang berubah, di transaksi yang sama dengan perubahan input (attendance, leave/overtime approve, salary/komponen, employment, adjustment target).
- **RLS:** policy standar `tenant_isolation` + `FORCE`. Tidak ada trigger immutability (baris di-upsert).
- **Retensi:** baris untuk periode `FINALIZED` tidak lagi dibutuhkan untuk stale detection; dihapus job cleanup > 90 hari setelah finalize (disimpan sementara untuk investigasi).

### 7.7 `payslips`

| Kolom | Tipe | Null | Default | Kegunaan |
|---|---|---|---|---|
| [T] | | | | |
| `run_id` | `uuid` | NOT NULL | — | FK → `payroll_runs` |
| `employee_id` | `uuid` | NOT NULL | — | FK → `employees` |
| `item_id` | `uuid` | NOT NULL | — | FK → `payroll_items` |
| `status` | `text` | NOT NULL | `'PENDING'` | `PENDING`, `RENDERED`, `FAILED` |
| `object_key` | `text` | NOT NULL | — | Deterministik: `tenants/{tenant_id}/payslips/{run_id}/{employee_id}.pdf` |
| `checksum_sha256` | `bytea` | NULL | — | Diisi setelah render |
| `size_bytes` | `int` | NULL | — | |
| `rendered_at` | `timestamptz` | NULL | — | |
| `attempts` | `smallint` | NOT NULL | `0` | |
| `last_error` | `text` | NULL | — | Tanpa PII |
| [M] | | | | |

- **Constraint:** `uq_payslips_run_employee UNIQUE (tenant_id, run_id, employee_id)`; `uq_payslips_object_key UNIQUE (object_key)`; `ck_payslips_object_key CHECK (object_key = 'tenants/' || tenant_id::text || '/payslips/' || run_id::text || '/' || employee_id::text || '.pdf')`. Key **ditegakkan** deterministik oleh DB, sehingga render ulang selalu menimpa objek yang sama (idempoten, PFS-10). Juga `ck_payslips_rendered CHECK ((status = 'RENDERED') = (rendered_at IS NOT NULL AND checksum_sha256 IS NOT NULL))`.
- **Index:** `ix_payslips_employee (tenant_id, employee_id, created_at DESC)` (daftar slip saya); `ix_payslips_pending (tenant_id, run_id) WHERE status <> 'RENDERED'` (progres render); **`ix_payslips_item (tenant_id, item_id)`** (final review C4). Tanpa index ini, setiap DELETE item `calc_version` lama memicu cek FK berupa seq scan atas seluruh `payslips` (±1 juta baris/tahun), sehingga cleanup/finalize melewati `statement_timeout`. Aturan umum dijaga query katalog (f) di `07` §2.5: **setiap FK wajib punya index yang diawali kolom FK-nya** (kedua project).
- **Catatan:** payslip **eventual**. Baris dibuat `PENDING` di transaksi finalize, sedangkan PDF dirender job. Slip web (PF-15) dirender dari `payroll_lines` dan tidak bergantung pada PDF.

---

## 8. Shared tables, sisi PayRun

| Tabel | Hal khusus PayRun (spesifikasi kolom di `10-db-shared-tables.md`) |
|---|---|
| `idempotency_keys` | Tenant-scoped (`tenant_id NOT NULL`, PK `(tenant_id, id)`, RLS standar). Signup tenant memakai tabel terpisah `platform_idempotency_keys` (tanpa RLS, hanya modul `tenancy`) |
| `jobs` | `tenant_id` wajib untuk job tenant (`payroll.plan`, `payroll.calc_chunk`, `payslip.render`, `attendance.import`); NULL untuk job platform (scheduler, cleanup). Kunci unik chunk: `dedupe_key = 'calc_chunk:' || run_id || ':' || calc_version || ':' || chunk_no` |
| `audit_log` | `tenant_id` NOT NULL untuk aksi dalam tenant; NULL untuk aksi platform (login gagal, signup sebelum tenant ada). `target_type` mencakup `employee`, `salary_assignment`, `payroll_run`, `payslip_access`, `membership`, … |

---

## 9. Ringkasan trigger

| Trigger | Tabel | Invariant | Kode error |
|---|---|---|---|
| `set_updated_at` | Semua tabel [M] | — | — |
| `forbid_update_delete` (shared) | `rulesets`, `attendance_import_errors`, `payroll_item_snapshots` (UPDATE saja), `audit_log` | PI-12, audit | `42501` |
| `memberships_owner_guard` (deferred) | `memberships` | PI-16 | `P0004` |
| `org_assignments_no_cycle` | `org_assignments` | PI-19 | `P0005` |
| `guard_period_date` / `guard_period_range` | 7 tabel input (§6.5) | PI-14 | `P0002` |
| `payroll_periods_final_immutable` | `payroll_periods` | PI-14 | `P0001` |
| `payroll_runs_terminal_immutable` | `payroll_runs` | PI-04 | `P0001` |
| `payroll_runs_totals_check` | `payroll_runs` | PI-05 | `P0006` |
| `forbid_change_on_finalized_run` | `payroll_items`, `payroll_lines`, `payroll_item_snapshots` | PI-04 | `P0001` |
| `payroll_items_sum_check` (deferred) | `payroll_items` | PI-05 | `P0006` |
| `adjustments_applied_immutable` | `adjustments` | PI-15 | `P0001` |

> **Biaya trigger:** semua trigger di jalur panas (`payroll_items`, `payroll_lines`) berupa satu lookup PK ke `payroll_runs`, yang hampir pasti ada di buffer cache. Sekitar 15 baris line per item × 50 item per chunk berarti ±800 lookup per transaksi chunk. Angka ini diukur di load test desain observability & testing. Kalau terbukti mahal, opsi pertama adalah trigger `FOR EACH STATEMENT` dengan transition table (`REFERENCING NEW TABLE`), bukan menghapus trigger.
