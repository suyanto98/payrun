# Step 4.7 — PayRun: Role DB, RLS, Konsistensi, Index, Volume & Seed

> Skema: [`06-payrun-schema.md`](06-payrun-schema.md). Tabel shared dan keputusan RLS-nya: [`01-shared-tables.md`](01-shared-tables.md). Analisis multi-tenancy: [Step 3 `05`](../step-03-domain-design/05-payrun-multi-tenancy.md). ADR-P01, ADR-P02.

---

## 1. Role database & grant

### 1.1 Role

| Role | Atribut | Dipakai oleh | Catatan |
|---|---|---|---|
| `payrun_owner` | `LOGIN`, owner schema `payrun` dan semua tabel | Job migrasi (goose) di CI/CD | Kredensial **tidak ada** di container api/worker |
| `payrun_app` | `LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB`, **bukan owner** | api + worker | Semua tabel bisnis `FORCE ROW LEVEL SECURITY` |
| `payrun_readonly` | `LOGIN NOBYPASSRLS` | Investigasi ops / laporan platform (opsional) | `SELECT` saja; tetap di bawah RLS kecuali tabel platform yang ditentukan |

**Startup check (fail fast):** api dan worker menjalankan query berikut saat boot dan menolak start kalau hasilnya tidak sesuai harapan (Step 3 `05` §5, residual risk "salah konfigurasi role"):

```sql
SELECT r.rolsuper, r.rolbypassrls,
       EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
               WHERE n.nspname = 'payrun' AND pg_get_userbyid(c.relowner) = current_user) AS owns_tables
FROM pg_roles r WHERE r.rolname = current_user;
-- harus: false, false, false
```

### 1.2 Matriks grant `payrun_app`

| Kelompok tabel | SELECT | INSERT | UPDATE | DELETE | Alasan |
|---|---|---|---|---|---|
| Mutable bisnis (`employees`, `memberships`, `org_assignments`, `salary_assignments`, `employee_components`, `attendance_records`, `leave_*`, `overtime_requests`, `approval_requests`, `payroll_periods`, `payroll_runs`, `adjustments`, `payslips`, `departments`, `positions`, `holidays`, `component_definitions`, `tenant_settings`, `invitations`, `attendance_imports`) | ✓ | ✓ | ✓ | ✗ | Strategi soft delete (`00-index.md` §5): tidak ada hard delete dari aplikasi |
| `payroll_items`, `payroll_lines`, `payroll_item_snapshots` | ✓ | ✓ | ✓ (items: exclude) | ✓ | DELETE dibutuhkan untuk membuang `calc_version` lama (ADR-P05). Setelah final diblokir trigger (PI-04) |
| Append-only (`audit_log`, `attendance_import_errors`) | ✓ | ✓ | ✗ | ✗ | Grant + trigger `forbid_update_delete` (dua lapis) |
| `rulesets` (global) | ✓ | ✗ | ✗ | ✗ | Hanya migrasi data (owner) |
| `tenants` | ✓ | ✓ (signup) | ✓ | ✗ | |
| `users`, `refresh_tokens` | ✓ | ✓ | ✓ | ✗ | `refresh_tokens` dibersihkan job retensi memakai owner |
| `idempotency_keys`, `platform_idempotency_keys`, `jobs` | ✓ | ✓ | ✓ | ✓ | Efemeral; job retensi menghapus baris kedaluwarsa |

Grant di atas diberikan per tabel di migrasi (bukan `GRANT ALL ON ALL TABLES`). Migration test membandingkan `information_schema.table_privileges` dengan matriks ini, sehingga tabel baru tanpa grant eksplisit membuat CI gagal.

---

## 2. Katalog policy RLS

### 2.1 Template tenant (tabel bisnis)

```sql
-- Helper (dibuat sekali oleh migrasi awal; LANGUAGE sql STABLE, BUKAN SECURITY DEFINER)  (Step 12: C3)
CREATE FUNCTION payrun.current_tenant() RETURNS uuid LANGUAGE sql STABLE AS
  $$ SELECT nullif(current_setting('app.tenant_id', true), '')::uuid $$;
CREATE FUNCTION payrun.current_user_id() RETURNS uuid LANGUAGE sql STABLE AS
  $$ SELECT nullif(current_setting('app.user_id', true), '')::uuid $$;

ALTER TABLE payrun.<t> ENABLE ROW LEVEL SECURITY;
ALTER TABLE payrun.<t> FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON payrun.<t>
  USING      (tenant_id = (SELECT payrun.current_tenant()))
  WITH CHECK (tenant_id = (SELECT payrun.current_tenant()));
```

- `current_setting(…, true)` mengembalikan NULL jika setting tidak ada, sehingga `tenant_id = NULL` → tidak ada baris (**fail closed**).
- **Pitfall string kosong (Step 12: C3):** setelah GUC kustom pernah disetel di suatu sesi (termasuk lewat `set_config(…, true)` yang sudah berakhir), `current_setting(…, true)` mengembalikan `''`, bukan NULL. Cast mentah `''::uuid` → error `invalid input syntax`, sehingga login/audit gagal **acak** tergantung koneksi pool mana yang didapat. Karena itu **semua** policy wajib memakai helper `payrun.current_tenant()` / `payrun.current_user_id()` (yang membungkus `nullif(…, '')`), tidak pernah `current_setting(…)::uuid` mentah. Dibungkus subselect `(SELECT …)` agar dievaluasi sekali per query (initplan), bukan per baris. Dicek oleh query katalog (e) di §2.5.

**Diterapkan ke:** `tenant_settings`, `memberships`¹, `invitations`, `departments`, `positions`, `holidays`, `employees`, `employment_spans`, `org_assignments`, `component_definitions`, `employee_components`, `salary_assignments`, `attendance_records`, `attendance_imports`, `attendance_import_errors`, `leave_types`, `leave_balances`, `leave_requests`, `overtime_requests`, `approval_requests`, `payroll_periods`, `payroll_runs`, `payroll_items`, `payroll_lines`, `payroll_item_snapshots`, `payroll_input_changes` *(Step 7)*, `adjustments`, `payslips`, `idempotency_keys`, `audit_log`² (+ `tenants`³, `users`⁴ dengan policy khusus).

¹ ditambah policy self (§2.3) · ² policy WITH CHECK khusus (lihat `01-shared-tables.md` §5) · ³ ⁴ policy khusus (§2.3).

> `tenant_settings` memakai PK `tenant_id` (tanpa `id`). Template tetap berlaku karena kolomnya bernama `tenant_id`.

### 2.2 Tabel tanpa RLS (pengecualian yang disengaja)

| Tabel | Kenapa tanpa RLS | Penjaga pengganti |
|---|---|---|
| `rulesets` | Data global, bukan milik tenant | Grant `SELECT` saja + trigger immutable |
| ~~`jobs`~~ | ~~Diklaim worker lintas tenant~~ | Sejak migrasi `00004` (review S1) `jobs` memakai RLS + FORCE; lihat catatan implementasi Sprint 1 di bawah |
| `refresh_tokens` | Tidak berisi data tenant; dicari berdasarkan hash token | Akses hanya modul `iam` (depguard) |
| `platform_idempotency_keys` | Signup terjadi sebelum tenant ada | Akses hanya modul `tenancy` (depguard); payload tanpa PII |

Daftar ini adalah **allowlist** di test katalog (§2.5). Tabel lain tanpa RLS membuat CI gagal.

### 2.3 Policy khusus

**`tenants`** (tabel akar, kolom `id`, bukan `tenant_id`):
```sql
CREATE POLICY tenants_current ON payrun.tenants
  USING (id = (SELECT payrun.current_tenant())
         OR ((SELECT payrun.current_tenant()) IS NULL             -- cabang self HANYA di mode platform (Step 12: C2)
             AND EXISTS (SELECT 1 FROM payrun.memberships m      -- daftar tenant milik user (WithPlatformTx)
                         WHERE m.tenant_id = tenants.id AND m.status = 'ACTIVE'
                           AND m.user_id = (SELECT payrun.current_user_id()))))
  WITH CHECK (id = (SELECT payrun.current_tenant()));
```
Signup (satu transaksi, dua fase berurutan; Step 12: C2): **fase platform** (`app.user_id` saja) → buat/temukan `users`; lalu app membuat `tenant_id` baru (UUIDv7) → `set_config('app.tenant_id', new_id, true)` **dan** `set_config('app.user_id', '', true)` → **fase tenant**: INSERT `tenants`, `tenant_settings`, `memberships` (OWNER), dan seed (§6), semuanya lolos `WITH CHECK` tenant biasa. Tidak ada policy "boleh INSERT tenant apa pun", dan tidak pernah ada momen kedua setting aktif bersamaan.

**`memberships`** (template tenant + akses diri sendiri):
```sql
CREATE POLICY memberships_self_read ON payrun.memberships FOR SELECT
  USING ((SELECT payrun.current_tenant()) IS NULL                -- hanya mode platform (Step 12: C2)
         AND user_id = (SELECT payrun.current_user_id()));
```
Policy permisif digabung dengan OR terhadap `tenant_isolation`. **Mode eksklusif (Step 12: C2):** tanpa syarat `current_tenant() IS NULL`, user yang anggota beberapa tenant akan melihat baris membership miliknya di tenant lain saat berada di konteks tenant A (kebocoran metadata, dan C-46 menembak 500 sebagai false positive). Jalur `WithPlatformTx` (login, daftar tenant, switch tenant) hanya bisa **membaca** membership milik user itu sendiri. Policy ini sengaja `FOR SELECT` saja, sehingga user tidak bisa membuat atau mengubah membership di tenant lain lewat jalur self. Semua penulisan membership memakai `WithTenantTx` (undangan, accept via token ber-tenant, revoke).

**`users`**: dua policy (`users_self`, `users_tenant_member_read`) + fungsi `auth_lookup_user` untuk login. Didefinisikan di `01-shared-tables.md` §1.

**`audit_log`**: `USING` tenant aktif; `WITH CHECK` mengizinkan baris platform (`tenant_id IS NULL`) hanya jika tidak ada tenant context. Didefinisikan di `01-shared-tables.md` §5. Ditambah (Step 12: R2-3) policy baca baris platform khusus role investigasi:
```sql
CREATE POLICY audit_platform_read ON payrun.audit_log FOR SELECT TO payrun_readonly
  USING (tenant_id IS NULL);
```
Tanpa policy ini baris audit platform (login gagal, signup) tidak terbaca role mana pun, karena `payrun_readonly` juga tunduk pada RLS.

**`idempotency_keys`**: template tenant standar (signup dipindah ke `platform_idempotency_keys`).

### 2.4 Jalur akses (ringkasan)

| Helper | Setting yang diset (transaction-local) | Tabel yang terlihat | Dipakai oleh |
|---|---|---|---|
| `WithTenantTx(ctx, tid)` / `WithTenantTxFor(tid)` | `app.tenant_id` **saja** (Step 12: C2; identitas principal untuk audit diteruskan sebagai parameter aplikasi, bukan GUC) | Semua baris tenant `tid` | Semua modul bisnis, job tenant |
| `WithPlatformTx(ctx, uid)` | `app.user_id` saja | `users` (self), `memberships` (self, read), `tenants` (yang user ikuti) | `iam` (login, list tenant, switch), `tenancy` (signup sebelum tenant dibuat) |
| Tanpa helper (pool mentah) | — | Hanya tabel tanpa RLS (`jobs`, `rulesets`, …) | `platform/jobs` (claim), `platform` startup check |

**Mode eksklusif:** satu transaksi tidak pernah memiliki `app.tenant_id` dan `app.user_id` aktif bersamaan. Policy self (`users_self`, `memberships_self_read`, cabang self `tenants_current`) mensyaratkan `current_tenant() IS NULL`. Test: `TestRLS_SelfPoliciesInactiveInTenantMode`.

Package `db` tidak mengekspor pool mentah ke modul bisnis. `platform/jobs` mendapat akses khusus lewat depguard.

### 2.5 Verifikasi katalog di CI

Dijalankan setelah `goose up` di pipeline integration test:

```sql
-- (a) Tabel di schema payrun yang punya kolom tenant_id tetapi RLS/FORCE tidak aktif atau tanpa policy
SELECT c.relname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'payrun'
WHERE c.relkind IN ('r','p')
  AND c.relname NOT IN ('jobs','rulesets','refresh_tokens','platform_idempotency_keys')   -- allowlist §2.2
  AND (NOT c.relrowsecurity OR NOT c.relforcerowsecurity
       OR NOT EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid));
-- harus 0 baris

-- (b) FK ke tabel tenant-scoped yang tidak menyertakan tenant_id (PI-02)
SELECT con.conname
FROM pg_constraint con
JOIN pg_class ref ON ref.oid = con.confrelid
JOIN pg_namespace n ON n.oid = ref.relnamespace AND n.nspname = 'payrun'
WHERE con.contype = 'f'
  AND EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = ref.oid AND a.attname = 'tenant_id')
  AND NOT EXISTS (SELECT 1 FROM unnest(con.confkey) k
                  JOIN pg_attribute a ON a.attrelid = ref.oid AND a.attnum = k
                  WHERE a.attname = 'tenant_id');
-- harus 0 baris (pengecualian: FK ke tenants(id) sendiri, karena tenants tidak punya kolom tenant_id)

-- (c) Fungsi SECURITY DEFINER di schema payrun di luar allowlist
SELECT p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE n.nspname = 'payrun' AND p.prosecdef AND p.proname NOT IN ('auth_lookup_user');
-- harus 0 baris

-- (d) View tanpa security_invoker
SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'payrun' AND c.relkind = 'v'
  AND NOT coalesce('security_invoker=true' = ANY (c.reloptions), false);
-- harus 0 baris

-- (e) (Step 12: C3) Policy yang memakai current_setting( mentah, bukan helper current_tenant()/current_user_id()
SELECT schemaname, tablename, policyname FROM pg_policies
WHERE schemaname = 'payrun'
  AND (coalesce(qual, '') LIKE '%current_setting(%' OR coalesce(with_check, '') LIKE '%current_setting(%');
-- harus 0 baris

-- (f) (Step 12: C4) FK tanpa index yang diawali kolom FK-nya — berlaku untuk KEDUA project (ganti nama schema)
SELECT con.conrelid::regclass AS tbl, con.conname
FROM pg_constraint con
JOIN pg_namespace n ON n.oid = con.connamespace AND n.nspname = 'payrun'
WHERE con.contype = 'f'
  AND NOT EXISTS (
    SELECT 1 FROM pg_index i
    WHERE i.indrelid = con.conrelid
      AND (i.indkey::int2[])[0:cardinality(con.conkey)-1] = con.conkey);
-- harus 0 baris. Tanpa index, setiap DELETE/UPDATE key di tabel parent memicu seq scan
-- di tabel anak untuk cek FK (contoh nyata: payslips.item_id saat cleanup calc_version).
```

Ditambah test perilaku (Step 3 `05` §3): RLS bypass test, pool leakage test (`MaxConns = 1`), dan `WITH CHECK` test.

---

## 3. Strong vs eventually consistent

| Data | Model | Alasan |
|---|---|---|
| Membership & role | **Strong** | Dasar authorization. Pencabutan berlaku di DB seketika; token ≤ 15 menit adalah satu-satunya jendela, dan operasi sensitif tetap memeriksa DB (ADR-P02) |
| Salary, komponen, employment span, org assignment | **Strong** | Input payroll; EXCLUDE dan PeriodGuard harus dievaluasi terhadap data terbaru dalam transaksi |
| Attendance, leave/overtime request | **Strong** | Input payroll + PeriodGuard; clock-in idempoten via unique constraint |
| Approval & saldo cuti | **Strong** | Keputusan dan efek (saldo) dalam satu transaksi; race diselesaikan oleh guard/lock (PI-10, PI-11) |
| State run, item, line, total | **Strong** | Uang. Transisi dijaga guard + trigger; total diverifikasi saat transisi (PI-05) |
| Snapshot & fingerprint | **Strong** | Ditulis di transaksi chunk yang sama dengan item; bukti reproducibility |
| Progres kalkulasi (`chunks_done`) | **Strong per chunk**, eventual untuk UI | Counter atomik; UI polling melihat progres yang sedikit tertinggal, dan itu tidak masalah |
| PDF payslip | **Eventual** | Dirender job setelah finalize. Slip web dari `payroll_lines` sudah tersedia seketika |
| Hasil import CSV | **Eventual** | Job async; status + laporan error terlihat setelah selesai |
| Diff vs bulan lalu, warning anomali | **Eventual (dihitung saat dibaca / saat kalkulasi)** | Bantuan review, bukan keputusan |
| Laporan (P3), metrik, dashboard | **Eventual** | Boleh dihitung dari replica (V2) |
| Audit log | **Strong** (ditulis di transaksi yang sama) | Jejak perubahan tidak boleh hilang ketika perubahannya commit |
| Notifikasi | **Eventual** (outbox → job) | Tidak boleh menahan transaksi bisnis |

**Aturan praktis:** apa pun yang *dicek oleh constraint, guard, atau fingerprint* harus strong. Apa pun yang *turunan untuk dibaca manusia* boleh eventual. Data eventual tidak boleh menjadi input keputusan yang kemudian ditulis.

---

## 4. Pola query → index

Prinsip: **setiap index diawali `tenant_id`**, karena (1) predikat RLS dan query eksplisit selalu memuat `tenant_id = $t`, dan (2) index tetap selektif per tenant di tabel pooled. PK `(tenant_id, id)` sudah melayani semua lookup berdasarkan ID.

| # | Pola query | Index | Catatan |
|---|---|---|---|
| Q1 | List employee + filter status, cursor | `ix_employees_status (tenant_id, status)` + PK | Cursor `(created_at, id)` → V1 cukup sort di memori per tenant (≤ 10k baris). Kalau lambat: `(tenant_id, status, created_at, id)` |
| Q2 | Cari employee berdasarkan nama | `ix_employees_name_prefix (tenant_id, lower(full_name) text_pattern_ops)` | **Tanpa `pg_trgm` di V1.** Prefix (`LIKE 'bud%'`) memakai index. Substring (`ILIKE '%udi%'`) jatuh ke scan dalam satu tenant (≤ 10k baris, kurang dari beberapa ms). `pg_trgm` + GIN ditambahkan kalau tenant > 50k karyawan atau pencarian substring jadi keluhan nyata. Menghemat satu extension dan index GIN yang mahal ditulis |
| Q3 | Gaji berlaku pada tanggal X (1 employee) | GiST `ex_salary_assignments_no_overlap` | `valid_during @> $date` memakai index exclusion |
| Q4 | Input chunk: salary/komponen/span untuk 50 employee di periode | **B-tree** `ix_salary_assignments_employee (tenant_id, employee_id)` (dan sejenisnya) | GiST **tidak** mendukung `employee_id = ANY($ids)` sebagai index condition, sedangkan B-tree mendukungnya. Baris per employee sedikit (1–3), jadi filter `valid_during && $period` setelah index scan murah. Detail kecil yang menentukan tidak adanya N+1 |
| Q5 | Absensi employee di periode | `uq_attendance_records_day (tenant_id, employee_id, work_date)` | Range scan `work_date >= $from AND work_date < $to`; batch `employee_id = ANY($ids)` |
| Q6 | Rekap absensi harian HR | `ix_attendance_records_date (tenant_id, work_date)` | |
| Q7 | Approval inbox manager | `ix_approval_requests_inbox (tenant_id, approver_id, created_at DESC) WHERE status = 'PENDING'` | Partial index: hanya yang pending, jadi tetap kecil walaupun riwayat jutaan |
| Q8 | Bawahan langsung saya (authz manager-of) | `ix_org_assignments_manager (tenant_id, manager_employee_id)` + filter `valid_during @> today` | |
| Q9 | Cek overlap cuti | GiST `ex_leave_requests_no_overlap` | Constraint sekaligus index |
| Q10 | Cuti/lembur approved untuk input payroll | `ix_leave_requests_employee (tenant_id, employee_id, status)`, `ix_overtime_requests_employee_date (tenant_id, employee_id, work_date)` | Filter `approved_at <= cutoff_at` setelah index |
| Q11 | Run per periode | `uq_payroll_runs_active_per_period` (partial) + `ix_payroll_runs_period` | |
| Q12 | Item per run per versi (review, total, finalize, upsert chunk) | `uq_payroll_items_version (tenant_id, run_id, employee_id, calc_version)` | Total run: `WHERE tenant_id, run_id, calc_version, status='OK'` → index prefix `(tenant_id, run_id)` + filter. Kalau run 10k item terasa lambat: `(tenant_id, run_id, calc_version, status) INCLUDE (gross, deductions, net)` untuk index-only scan |
| Q13 | Line per item (slip, review) | `uq_payroll_lines_order (tenant_id, item_id, sort_order)` | Sudah terurut untuk render |
| Q14 | Slip saya | `ix_payslips_employee (tenant_id, employee_id, created_at DESC)` | |
| Q15 | Diff vs bulan lalu | `ix_payroll_items_employee (tenant_id, employee_id, created_at DESC)` | Ambil item final terakhir sebelum run ini |
| Q16 | Audit per objek / per aksi | `ix_audit_log_tenant_target`, `ix_audit_log_tenant_action` (shared §5) | Contoh: "siapa melihat slip Tono" |
| Q17 | PeriodGuard trigger | GiST `ix_payroll_periods_finalized (tenant_id, period_range) WHERE status = 'FINALIZED'` | Dipanggil di setiap tulis input; harus murah |
| Q18 | Adjustment pending untuk run target | `ix_adjustments_target (tenant_id, target_period_id, employee_id) WHERE status = 'PENDING_APPLY'` | |
| Q19 | Membership user (login / list tenant) | `ix_memberships_user (user_id) WHERE status = 'ACTIVE'` | Satu-satunya index yang **tidak** diawali `tenant_id`, karena jalur ini memang lintas tenant (self) |

**Index yang sengaja tidak dibuat:** GIN pada `jsonb` (`issues`, `basis`, `input`, `payload`), karena kolom itu tidak pernah di-query. Index pada `audit_log.diff`. Index terpisah `(tenant_id)` saja, karena sudah tercakup prefix index lain.

---

## 5. Volume, pertumbuhan, dan partisi

Skenario uji (`00-index.md` §8): 1.000 tenant, 100.000 employee, tenant terbesar 10.000.

| Tabel | Baris/tahun | Ukuran/baris (perkiraan) | Ukuran/tahun (+ index) | Catatan |
|---|---|---|---|---|
| `attendance_records` | ~25 juta | ~120 B | ~5–6 GB | Terbesar; hampir selalu dibaca per (tenant, employee, rentang tanggal) |
| `payroll_lines` | ~15 juta | ~250 B (explanation + basis) | ~5 GB | Dibaca per item; immutable setelah final |
| `audit_log` | ~10 juta | ~400 B | ~5 GB | Append-only; dibaca jarang, per objek |
| `payroll_item_snapshots` | ~1,2 juta | ~3–6 KB (TOAST) | ~5–7 GB | Dibaca hanya saat verify/sengketa |
| `approval_requests` | ~1,6 juta | ~200 B | ~0,5 GB | |
| `payroll_items` | ~1,2 juta | ~200 B | ~0,3 GB | |
| Lainnya | < 1 juta | | < 1 GB total | |

**Kesimpulan V1:** total ±25–30 GB/tahun pada skenario uji. Satu instance Postgres menangani ini **tanpa partisi**. Load test (Step 8) memvalidasi query Q4, Q5, Q12, dan Q17 pada data sintetis 1 tahun.

**Kandidat partisi (V2/V3):**

| Tabel | Kunci partisi | Kelebihan | Kekurangan / syarat |
|---|---|---|---|
| `audit_log` | `RANGE (created_at)` bulanan | Retensi = `DROP PARTITION` (murah); arsip ke object storage | PK harus memuat `created_at` → `(id, created_at)` |
| `attendance_records` | `RANGE (work_date)` tahunan | Query selalu ber-rentang tanggal → partition pruning; data lama jarang disentuh | PK/unique harus memuat `work_date`: `(tenant_id, id, work_date)`; unique `(tenant_id, employee_id, work_date)` sudah memuatnya |
| `payroll_lines`, `payroll_item_snapshots` | `RANGE` per tahun via `created_at`, **atau** `HASH (tenant_id)` | Time: arsip run lama. Hash: menyebar tenant besar | Time: lookup per item butuh kunci partisi (denormalisasi `period`). Hash: tidak membantu retensi |
| Semua (V3 cell) | Bukan partisi, tetapi **cell** per kelompok tenant (Step 2 arsitektur V3) | Blast radius, ukuran per cluster | Routing koneksi |

**Kenapa tidak `HASH (tenant_id)` sejak awal:** hash per tenant tidak membantu masalah yang benar-benar akan muncul lebih dulu (retensi data lama, vacuum tabel besar). Hash juga membuat setiap query tanpa `tenant_id`, misalnya job platform, memindai semua partisi. Partisi waktu cocok dengan pola akses dan retensi. Isolasi tenant besar lebih baik diselesaikan di level cell/silo (V3).

**Retensi (job harian, role owner):**

| Data | Retensi | Mekanisme |
|---|---|---|
| `jobs` `succeeded` | 7 hari | DELETE batch |
| `idempotency_keys`, `platform_idempotency_keys` | 24 jam – 7 hari (`expires_at`) | DELETE batch |
| `invitations` kedaluwarsa/terpakai | 90 hari | DELETE batch |
| `refresh_tokens` kedaluwarsa/revoked | 30 hari | DELETE batch |
| `attendance_import_errors` | 1 tahun | DELETE batch (V2: drop partisi) |
| Payroll, attendance, audit | **Tidak dihapus** di V1 (kewajiban penyimpanan dokumen ketenagakerjaan/pajak; durasi pasti diverifikasi) | V2: arsip, bukan hapus |

---

## 6. Seed default tenant (O6)

Dijalankan oleh use case signup, **di transaksi yang sama** dengan INSERT `tenants` (tenant context sudah diset ke tenant baru, §2.3). Seed adalah kode aplikasi (bukan migrasi), sehingga ter-versi bersama kode dan teruji.

| Tabel | Baris yang dibuat | Nilai |
|---|---|---|
| `tenant_settings` | 1 | `timezone = Asia/Jakarta`, `work_days = {1..5}`, `proration_basis = WORKING_DAYS`, `cutoff_day_of_next_month = 2`, `cutoff_time_local = 23:59`, `overtime_basis = BASE`, `overtime_max_hours_per_day = 4`, `incomplete_attendance_policy = COUNT_AS_PRESENT`, `ruleset_code = ID-SIMPLIFIED`, `net_change_warning_pct = 20` |
| `memberships` | 1 | User pendaftar, `roles = {OWNER}`, `status = ACTIVE` |
| `leave_types` | 3 | `ANNUAL` (dibayar, kuota 12, minus 0), `SICK` (dibayar, tanpa kuota), `UNPAID` (tidak dibayar, tanpa kuota) |
| `component_definitions` (sistem, `is_system = true`) | 9 | `BASE_SALARY` (EARNING), `OVERTIME` (`PER_HOUR_MULTIPLIER`), `ABSENCE_UNPAID_LEAVE`, `ABSENCE_UNEXCUSED` (DEDUCTION, stage ABSENCE), `BPJS_JHT_EE`, `BPJS_JP_EE`, `BPJS_KES_EE` (`PERCENT_OF`, `ruleset_ref`, base `BPJS_WAGE`), `PPH21` (stage TAX), `ADJUSTMENT` |
| `component_definitions` (contoh tenant) | 2 | `TUNJ_JABATAN` (`FIXED`, taxable, `bpjs_base`), `TRANSPORT` (`PER_DAY`, `PRESENT`, taxable). Bisa dihapus/diubah tenant |
| `departments` / `positions` | 1 / 1 | `GEN` "Umum" / `STAFF` "Staff", agar employee pertama bisa langsung dibuat |
| `holidays` | 0 | Diisi tenant sendiri. Opsi "impor libur nasional tahun ini" dari daftar statis di kode adalah P2 |
| `audit_log` | 1 | `tenant.created` |

`rulesets` **tidak** di-seed per tenant. Ruleset bersifat global, diisi migrasi data (`ID-SIMPLIFIED` versi `2026.1`, berlabel *simplified — not compliance-certified*, dengan nilai placeholder dari Step 3 `06`).

---

## 7. Pemetaan constraint → error domain (PayRun)

> **(Step 12: R1-6)** Tabel pemetaan versi Step 4 di sini sudah **basi** (memakai kode seperti `RUN_ALREADY_EXISTS` + `200`, `SELF_APPROVAL_NOT_ALLOWED`, `EFFECTIVE_PERIOD_OVERLAP` yang kemudian diganti/dilebur). **Sumber otoritatif adalah [Step 5 `01-error-catalogue.md`](../step-05-api-contract/01-error-catalogue.md) §3 (code PayRun) dan §4 (pemetaan error database → code).** Lapisan `platform/db` memetakan `(SQLSTATE, constraint name)` sesuai katalog itu; nama constraint di [`06`](06-payrun-schema.md) tetap kontrak.

Catatan khusus yang tidak ada di katalog:

| SQLSTATE / constraint | Perilaku |
|---|---|
| `23505` `uq_payroll_items_version` | Tidak muncul ke API (`ON CONFLICT` di chunk) |
| `23505` `uq_attendance_records_day` (clock-in) | Bukan error: idempoten, kembalikan record yang ada |
| `23505` `uq_payroll_periods_month` | Tidak seharusnya muncul: create run memakai `INSERT … ON CONFLICT DO NOTHING` + `SELECT` di bawah `tenant_settings FOR UPDATE` (Step 12: R1-8). Jika tetap muncul → `INTERNAL` + alert (bug) |
| `23514` `ck_payroll_items_ok_amounts`, `P0006`, `42501` | Bug → `INTERNAL` (500) + alert |
| `23514` `ck_jobs_tenant_matches_context` | Bug → `INTERNAL` + `payrun_tenant_isolation_violations_total` |

---

## Catatan implementasi (Sprint 1)

Migrasi `db/migrations/00002_tenancy_iam.sql`. Deviasi dari desain di atas, masing-masing dengan alasannya:

| Aspek | Desain | Implementasi S1 | Alasan |
|---|---|---|---|
| `auth_lookup_user` di bawah `FORCE RLS` | Fungsi `SECURITY DEFINER` milik `payrun_owner` | Sama, **ditambah** policy `users_owner_auth_lookup ON users FOR SELECT TO payrun_owner USING (true)` | `FORCE ROW LEVEL SECURITY` juga berlaku untuk owner. Tanpa policy ini fungsi berjalan sebagai owner tanpa tenant/user context dan selalu melihat 0 baris: login tidak akan pernah berhasil. Policy hanya untuk role migrasi (kredensialnya tidak ada di container api/worker), SELECT saja |
| `ix_memberships_user` | Partial `(user_id) WHERE status = 'ACTIVE'` | Penuh `(user_id)` | Index ini sekaligus index FK `fk_memberships_user_id`; Step 12 C4 mensyaratkan index FK mencakup semua baris (`TestCatalog_EveryFKHasIndex`) |
| `jobs_enforce_tenant` | `nullif(current_setting(...), '')::uuid` | `payrun.current_tenant()` | Satu definisi helper (C3) |
| Batas penulisan jalur platform | depguard (`tenancy`/`iam` saja) | forbidigo `db.(WithPlatformTx\|SetPlatformUser\|SetTenant)` di luar `internal/modules/(iam\|tenancy)` | depguard membatasi import package, bukan pemakaian fungsi |
| Verifikasi email (R2-8, migrasi `00003`) | Tidak dirinci | `users.email_verified_at`; status tenant `PENDING_VERIFICATION`; tabel `email_verifications` (hash token + `user_id`, TTL 24 jam) **tanpa RLS**, masuk allowlist §2.2 | Pola `refresh_tokens`: endpoint verifikasi tidak terautentikasi (link di email), lookup berdasarkan hash 256-bit, tabel tanpa data tenant dan tanpa PII. Hanya modul `iam` |
| `payrun_readonly` | Tidak dirinci | `SELECT` pada `tenants`, `tenant_settings`, `memberships`, `audit_log`, kolom `jobs` tanpa `payload`/`last_error` (migrasi `00004`), dan kolom `users` tanpa `password_hash` | Investigasi tetap di bawah RLS; hash password dan token tidak terbaca |
| `jobs` (review keamanan S1, migrasi `00004`) | Tanpa RLS; dijaga trigger `jobs_enforce_tenant` saat INSERT | **RLS + FORCE** policy `jobs_tenant`: `current_tenant() IS NULL OR tenant_id = current_tenant()`; trigger `jobs_forbid_tenant_change` (`ck_jobs_tenant_immutable`) menolak UPDATE `tenant_id` di mode apa pun; `jobs` keluar dari allowlist §2.2 | Trigger INSERT saja tidak mencegah transaksi tenant A membaca payload job tenant B, atau memindahkan job ke tenant lain lewat UPDATE (worker lalu menjalankannya di `WithTenantTxFor(jobs.tenant_id)` = tenant korban). Tanpa tenant context (claim worker lintas tenant) semua baris tetap terlihat |

Test katalog §2.5 (a)–(f) dan matriks grant §1.2 ada di `db/migrations/catalog_test.go`, masing-masing dengan test negatif. Test perilaku (tanpa context = 0 baris, query tanpa filter, `WITH CHECK`, kebocoran pool setelah COMMIT/ROLLBACK, mode eksklusif C2, `password_hash`, `jobs_enforce_tenant`, isolasi `jobs` setelah INSERT, PI-16, audit platform) ada di `internal/platform/db/rls_test.go` dan berjalan sebagai `payrun_app`.

## Catatan implementasi (Sprint 2)

Migrasi `00005`: `departments`, `positions`, `employees`, `employment_spans`, `invitations`, semuanya RLS + FORCE + `tenant_isolation`, grant tanpa `DELETE`.

| Aspek | Desain | Implementasi S2 | Alasan |
|---|---|---|---|
| `employees.pii_key_version` | `smallint` | `text` (contoh `v1`) | Versi keyring berupa string (`PII_KEYRING` `{"v1": …}`), sama dengan prefix ciphertext |
| Format ciphertext | `v1:<base64 nonce>:<base64 ct>` | `bytea`: `"v1:"` + nonce 12 byte + ct‖tag | Disimpan sebagai `bytea`, jadi base64 tidak perlu; prefix versi tetap ada untuk rotasi |
| AAD / blind index | AAD `tenant_id:employees.nik`; HMAC `tenant_id || ":" || nik` | AAD dan scope blind index sama: `"<tenant_id>:employees.<field>"`; blind index HMAC-SHA256 dengan kunci terpisah `PII_BLIND_INDEX_KEY` | NIK yang sama di dua tenant menghasilkan index berbeda (tidak bisa dikorelasikan dari dump) |
| `ck_employees_*_pair` | `enc` ⇔ `bidx` | `enc` ⇔ `bidx` ⇔ `last4` (NPWP sama); rekening `enc` ⇔ `last4` | Ketiga kolom selalu ditulis bersamaan |
| `invitations.employee_id` | Tidak ada di tabel (hanya di request U2) | Kolom `employee_id` + composite FK ke `employees` | Penautan dilakukan saat accept, jadi harus tersimpan di undangan |
| `ix_invitations_employee` | — | Index penuh `(tenant_id, employee_id)` | Index FK harus mencakup semua baris (C4; partial ditolak `TestCatalog_EveryFKHasIndex`) |
| `payrun_readonly` | — | Tanpa kolom `*_enc`/`*_bidx` employees dan `token_hash` undangan | Dump analis tidak membawa ciphertext/hash |
| Tenant guard C-46 | Helper decode repository | `db.GuardTenant(ctx, table, row.tenant_id)` dipanggil setiap decode baris tenant di modul S2; mismatch → `500` + `payrun_tenant_isolation_violations_total{table}` + log error | `TestTenantGuard_DetectsForeignRow` |
