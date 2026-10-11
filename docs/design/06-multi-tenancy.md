# PayRun: Multi-Tenancy Analysis & Threat Scenario

> Keputusan dasar: [ADR-P01](../adr/ADR-P01-multi-tenancy-pooled-rls.md) (pooled + RLS), [ADR-P02](../adr/ADR-P02-tenant-context.md) (tenant context), [ADR-010](../adr/ADR-010-authorization.md), [ADR-012](../adr/ADR-012-object-storage.md), [ADR-P08](../adr/ADR-P08-pii-encryption.md).
> Invariant terkait: PI-01, PI-02, PI-16, PI-17 ([`08-invariants.md`](08-invariants.md)).

---

## 1. Prinsip

1. **Isolasi tenant adalah properti sistem, bukan disiplin developer.** Setiap lapisan mengasumsikan lapisan di atasnya bisa salah.
2. **Fail closed.** Tanpa tenant context, hasilnya kosong atau error, tidak pernah "semua data".
3. **Tenant tidak pernah berasal dari input client.** Tenant berasal dari token yang ditandatangani server, yang diterbitkan setelah cek membership.
4. **RLS hanya melindungi Postgres.** Job, cache, log, metrik, dan object storage harus tenant-aware secara eksplisit.

---

## 2. Analisis per aspek

### 2.1 Tenant isolation
| Lapisan | Mekanisme | Melindungi dari |
|---|---|---|
| Identitas | JWT `tid` + membership (ADR-P02) | Client memilih tenant sendiri |
| Aplikasi | Policy di service + query scoping (ADR-010) | Akses ke resource di dalam tenant yang tidak berhak (role/relasi) |
| Database | RLS `USING` + `WITH CHECK`, `FORCE RLS` (ADR-P01) | Bug "lupa `WHERE tenant_id`" |
| Skema | Composite FK `(tenant_id, id)` | Referensi lintas tenant (employee A → department B) |
| Non-DB | Prefix `tenant_id` di job payload, object key, cache key, log | Kebocoran di luar Postgres |
| Deteksi | Audit + metrik `tenant_isolation_violation_total` | Pelanggaran yang lolos atau percobaan serangan |

### 2.2 Tenant context
- **Sumber:** claim `tid` di access token. Hanya diterbitkan oleh login (tenant default) dan `POST /v1/session/switch-tenant` setelah membership `ACTIVE` diverifikasi.
- **Propagasi:** middleware → `Principal{UserID, TenantID, Roles}` di `context.Context` → `db.WithTenantTx` → `set_config('app.tenant_id', $1, true)`.
- **Satu jalur akses DB:** package `db` tidak mengekspor pool mentah. Repository hanya menerima `*Queries` dari `WithTenantTx`/`WithTenantTxFor`.
- **Pengecualian eksplisit:** modul `tenancy` (signup) dan `iam` (login, daftar membership user, switch tenant) butuh query lintas tenant. Jalur ini memakai `db.WithPlatformTx` dengan **policy RLS terpisah** yang hanya mengizinkan baris milik `app.user_id` (contoh: `memberships USING (user_id = (SELECT payrun.current_user_id()))`). Ini bukan bypass.

### 2.3 Authorization
Tenant isolation menjawab *"tenant mana"*. Authorization menjawab *"siapa di dalam tenant boleh apa"*. Keduanya harus lolos.

| Role | Contoh izin | Contoh larangan |
|---|---|---|
| `OWNER` | Semua di tenant, termasuk membership & finalize | — |
| `HR_ADMIN` | Employee, org, salary, koreksi absensi, adjustment, override approval (ter-audit) | Approve payroll run yang di-submit sendiri |
| `PAYROLL_ADMIN` | Buat/hitung/submit/finalize run, lihat slip semua (ter-audit) | Ubah membership |
| `MANAGER` | Approve cuti/lembur **bawahan langsung** (relasi effective-dated) | Lihat gaji bawahan |
| `EMPLOYEE` | Data diri, clock in/out, request cuti/lembur, **slip sendiri** | Data karyawan lain |

- Relasi `manager-of` dievaluasi pada tanggal request (ADR-P04) dan approver di-snapshot (ADR-P09).
- Resource yang tidak boleh diketahui → **404**; aksi tidak diizinkan pada resource yang terlihat → **403** (ADR-010).
- Operasi sensitif memeriksa ulang membership + role di DB (ADR-P02): finalize, ubah gaji, lihat slip orang lain, lihat PII penuh.

### 2.4 Database strategy
Pooled tables + `tenant_id uuid NOT NULL` di setiap tabel bisnis. Dua role: `payrun_owner` (migrasi) dan `payrun_app` (runtime, non-owner, tanpa `BYPASSRLS`). Index tabel bisnis selalu diawali `tenant_id`. Detail dan perbandingan opsi di ADR-P01.

### 2.5 Query filtering
- Query tetap menulis `WHERE tenant_id = $tenant` **secara eksplisit** walaupun RLS aktif. Alasannya:
  1. planner mendapat predikat yang jelas untuk index `(tenant_id, …)`;
  2. kode bisa dibaca tanpa harus tahu ada RLS;
  3. RLS menjadi jaring kedua, bukan satu-satunya.
- Lint di CI (custom check atas file `db/queries/*.sql`): setiap query pada tabel ber-`tenant_id` harus menyebut `tenant_id` di `WHERE`/`JOIN`, kecuali diberi anotasi `-- tenant: rls-only` dengan alasan.

### 2.6 Row-level security
```sql
ALTER TABLE <t> ENABLE ROW LEVEL SECURITY;
ALTER TABLE <t> FORCE  ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON <t>
  USING      (tenant_id = (SELECT payrun.current_tenant()))
  WITH CHECK (tenant_id = (SELECT payrun.current_tenant()));
```
- **Migration test:** query katalog memastikan *setiap* tabel yang punya kolom `tenant_id` memiliki `relrowsecurity = true`, `relforcerowsecurity = true`, dan policy `tenant_isolation`. Tabel baru tanpa RLS membuat CI gagal.
- **Mode eksklusif (final review C2/C3):** `WithTenantTx` hanya menyetel `app.tenant_id`, `WithPlatformTx` hanya `app.user_id`. Policy self hanya aktif saat tidak ada tenant context. Semua policy memakai helper `current_tenant()`/`current_user_id()` dengan `nullif(…,'')` (lihat ADR-P02 amandemen).
- **Deteksi defensif (desain observability & testing, C-46):** helper decode repository membandingkan `row.tenant_id` dengan tenant di context. Kalau berbeda (yang seharusnya mustahil karena RLS), request digagalkan dan metrik INVARIANT `payrun_tenant_isolation_violations_total` naik. Dengan begitu, kegagalan RLS yang tak terduga terdeteksi, bukan diam-diam.
- Fungsi `SECURITY DEFINER` **dilarang** di skema aplikasi (dicek oleh migration test), karena fungsi itu berjalan dengan hak owner dan bisa melewati RLS. **Satu pengecualian ber-allowlist:** `payrun.auth_lookup_user(email)` untuk login (sebelum identitas user diketahui, policy `users_self` belum bisa dipakai). Fungsi ini hanya mengembalikan `id, password_hash, status` untuk satu email, punya `SET search_path` eksplisit, dan menjadi satu-satunya entri di allowlist test katalog (`10-db-shared-tables.md`, `13-db-rls-consistency-indexing.md`).

### 2.7 Cross-tenant access prevention
Ringkasan jalur serangan dan pertahanannya ada di §4 (threat scenario). Prinsipnya: setiap jalur harus melewati **minimal dua lapisan independen**.

### 2.8 Tenant-aware cache
V1 tidak memakai cache (ADR-008). Aturan untuk saat cache ditambahkan:
- Satu-satunya pembuat key: `cache.Key(tenantID, namespace, id)` → `t:{tenant_id}:{ns}:{id}`.
- Tidak boleh meng-cache response HTTP yang berisi data tenant di proxy/CDN (`Cache-Control: private, no-store` untuk endpoint ber-tenant).
- Invalidasi saat menulis, di dalam use case yang sama (setelah commit).
- Test: dua tenant dengan ID resource yang sama (seed sengaja) tidak saling mendapat nilai cache.

### 2.9 Tenant-aware background job
| Aturan | Penegakan |
|---|---|
| `tenant_id` wajib di setiap job payload (kolom `jobs.tenant_id NOT NULL` untuk job berkategori tenant) | Constraint + konstruktor job bertipe |
| Worker membuka `WithTenantTxFor(job.TenantID)` | Handler menerima `*Queries` yang sudah ter-scope, bukan pool |
| Job memvalidasi status tenant dan objek saat dieksekusi (tenant `SUSPENDED` → skip; run sudah `CANCELLED` → no-op) | Handler |
| Job platform (scheduler accrual, cleanup) mengiterasi tenant lalu meng-enqueue job per tenant | Tidak ada job yang memproses banyak tenant dalam satu transaksi |
| Fairness (P2): batas job berjalan per tenant | Kolom `tenant_id` di query claim (ADR-006) |

### 2.10 Logging, metrics, tracing
- Middleware menambahkan `tenant_id`, `user_id`, `request_id` ke logger context. Job menambahkan `tenant_id`, `job_id`, `run_id`.
- PII di-redact (ADR-P08). Jumlah gaji tidak boleh muncul di log.
- Metrik: **tidak** memakai label `tenant_id` secara default (kardinalitas). Per-tenant insight diambil dari log/trace, atau label `tenant_tier`. Pengecualian: top-N tenant via recording rule.

### 2.11 Object storage
- Key: `tenants/{tenant_id}/payslips/{run_id}/{employee_id}.pdf`, `tenants/{tenant_id}/imports/{import_id}.csv` (ADR-012).
- Metadata objek ada di tabel ber-RLS. URL tidak pernah dibangun dari input client; URL **dibuat server** dari metadata yang lolos RLS + policy.
- Presigned GET dengan TTL 5 menit; bucket private.

### 2.12 Platform operator & break-glass (P2)
- Tidak ada role DB yang bypass RLS untuk runtime.
- Break-glass: request akses (alasan, durasi ≤ 1 jam, read-only) → token dengan `tid` target + claim `impersonation{operator_id, grant_id}` → semua aksi tercatat di audit **tenant tersebut** dan terlihat oleh OWNER.
- V2: approval kedua untuk grant + notifikasi email ke OWNER.

### 2.13 Tenant lifecycle
| Tahap | Perilaku |
|---|---|
| Signup | INSERT tenant + membership OWNER + seed konfigurasi (satu transaksi). Tidak ada DDL, sehingga onboarding instan |
| Suspend (tagihan/abuse) | `tenants.status = SUSPENDED`: login ke tenant ditolak, job di-skip, data tetap ada |
| Offboarding / delete (V2) | Export data tenant (job, `COPY … WHERE tenant_id`), soft-delete + periode retensi, lalu hard delete per tabel mengikuti urutan FK. V2: crypto-shredding dengan menghapus DEK tenant (ADR-P08) |

### 2.14 Pengalaman schema-per-tenant vs pooled + RLS
| Aspek operasional | Schema-per-tenant (pernah dioperasikan) | Pooled + RLS (PayRun) |
|---|---|---|
| Migrasi | Iterasi N schema; gagal di tengah → sebagian schema tertinggal versi; perlu tooling "migrasi semua schema" | Satu migrasi; satu versi skema |
| Onboarding tenant | Create schema + jalankan semua migrasi (biasanya async via queue) | Satu INSERT dalam transaksi |
| Salah konteks | `search_path` salah → query ke schema tenant lain | `app.tenant_id` tidak diset → 0 baris (fail closed) |
| Reporting lintas tenant | UNION banyak schema atau mekanisme tambahan seperti FDW ke DB report | Query biasa oleh role platform yang terkontrol |
| Backup/restore satu tenant | Mudah (`pg_dump -n`) | Sulit (export terfilter) |
| Noisy neighbor | Tetap berbagi instance DB | Sama |

**Pelajaran yang dibawa:** biaya terbesar schema-per-tenant ada di *operasi* (migrasi, drift, konteks koneksi), bukan di performa. Untuk target UMKM dengan banyak tenant kecil, pooled + RLS lebih murah dioperasikan. Schema/DB terpisah tetap menjadi jalur V3 untuk tenant yang butuh isolasi fisik.

---

## 3. Isolation test suite (ringkas; detail di desain observability & testing)

| Test | Membuktikan |
|---|---|
| Cross-tenant per endpoint (generated dari router) | Setiap endpoint tenant-scoped mengembalikan 404/kosong untuk ID tenant lain |
| Authorization matrix (endpoint × role) | Role tidak bisa melebihi izinnya; endpoint baru tanpa entri → test gagal |
| RLS bypass (query mentah tanpa `WHERE tenant_id`) | DB tetap hanya mengembalikan tenant aktif |
| Tanpa context | 0 baris / `ErrNoTenantContext` |
| Pool leakage (`MaxConns = 1`, tx tenant A lalu tx tanpa setting) | Setting tidak menempel di koneksi |
| `WITH CHECK` (INSERT baris dengan `tenant_id` lain) | Ditolak oleh DB |
| Katalog (semua tabel ber-`tenant_id` punya RLS + FORCE; tidak ada `SECURITY DEFINER`; `payrun_app` bukan owner & tanpa `BYPASSRLS`) | Konfigurasi tidak bisa mundur diam-diam |
| Job tanpa `tenant_id` | Ditolak saat enqueue |

---

## 4. Threat Scenario — "User dari Company A mencoba mengakses Employee Company B"

**Setup:** Tenant **Alpha** (Company A) dan **Beta** (Company B). Penyerang **Mallory** adalah `HR_ADMIN` sah di Alpha. Mallory tidak punya membership di Beta. Target: data karyawan Beta, terutama gaji dan rekening.

### A1 — Menebak / enumerasi UUID employee
| | |
|---|---|
| **Serangan** | `GET /v1/employees/{uuid-beta}`, dengan UUID didapat dari URL bocor atau dicoba massal |
| **Dihentikan oleh** | (1) Query scoping `WHERE tenant_id = $alpha AND id = $1` → tidak ada baris; (2) RLS (jika filter lupa ditulis) → tidak ada baris |
| **Response** | `404 NOT_FOUND`, identik dengan ID yang benar-benar tidak ada (tidak ada oracle keberadaan) |
| **Bukti** | Log `status=404 tenant_id=alpha`; enumerasi massal memicu rate limit (P2) + metrik `http_404_by_route` anomali. UUIDv7 tetap tidak dianggap sebagai rahasia |

### A2 — Memalsukan claim `tid` di JWT
| | |
|---|---|
| **Serangan** | Decode token, ganti `tid` menjadi Beta, encode ulang (atau `alg: none`) |
| **Dihentikan oleh** | Verifikasi signature (HS256 + `kid`), whitelist algoritma, cek `exp`/`iss`/`aud` (ADR-009) |
| **Response** | `401 UNAUTHORIZED` |
| **Bukti** | Log `auth_failure reason=invalid_signature`; metrik `auth_token_invalid_total` |

### A3 — Menyelipkan tenant lewat header / body / query
| | |
|---|---|
| **Serangan** | `X-Tenant-ID: beta`, `{"tenant_id": "beta", …}` di body, `?tenant_id=beta` |
| **Dihentikan oleh** | Server tidak pernah membaca tenant dari input. DTO request tidak punya field `tenant_id`; nilai tenant diambil dari `Principal`. OpenAPI dengan `additionalProperties: false` menolak field asing |
| **Response** | Field diabaikan / `400 VALIDATION_ERROR` (field tidak dikenal); aksi tetap di Alpha |
| **Bukti** | Test kontrak; log validasi |

### A4 — IDOR lewat resource bertingkat
| | |
|---|---|
| **Serangan** | `POST /v1/leave-requests {employee_id: <beta>}` atau `POST /v1/adjustments {employee_id: <beta>, amount: …}` untuk menulis ke karyawan Beta |
| **Dihentikan oleh** | (1) Service me-load employee lewat `WithTenantTx(alpha)` → tidak ditemukan; (2) jika lolos, INSERT dengan `tenant_id = alpha` + FK `(tenant_id, employee_id) → employees(tenant_id, id)` gagal karena pasangan (alpha, beta-employee) tidak ada; (3) `WITH CHECK` menolak INSERT dengan `tenant_id = beta` |
| **Response** | `404 NOT_FOUND` (employee tidak ditemukan) |
| **Bukti** | Error FK (`23503`) jika lapisan 1 bocor → dicatat sebagai `tenant_isolation_violation_total` (seharusnya tidak pernah terjadi, jadi alert) |

### A5 — Mereferensikan objek Beta saat membuat objek Alpha
| | |
|---|---|
| **Serangan** | `POST /v1/employees {department_id: <beta-dept>, manager_id: <beta-emp>}`, agar karyawan Alpha "menempel" ke struktur Beta dan kemudian membocorkan nama departemen/manager lewat response |
| **Dihentikan oleh** | Composite FK `(tenant_id, department_id) → departments(tenant_id, id)` (PI-02); validasi service yang me-load department di tenant aktif |
| **Response** | `422 VALIDATION_ERROR` (`department_id` tidak valid) |
| **Bukti** | Test skema: INSERT langsung dengan referensi lintas tenant gagal di DB |

### A6 — Manipulasi filter pada list endpoint
| | |
|---|---|
| **Serangan** | `GET /v1/employees?department_id=<beta>`, `?cursor=<cursor buatan yang meng-encode id Beta>`, `?search=…` dengan payload SQL |
| **Dihentikan oleh** | Filter selalu ditambahkan **di atas** `tenant_id = $alpha` (AND, tidak pernah OR); cursor opaque ditandatangani/divalidasi dan hanya menentukan posisi, bukan scope; query parametrized (sqlc), tanpa string concatenation; RLS |
| **Response** | `200` dengan list kosong / `400` untuk cursor invalid |
| **Bukti** | Cross-tenant test untuk list: hasil tidak pernah mengandung ID Beta |

### A7 — Kebocoran lewat job / import / export
| | |
|---|---|
| **Serangan** | Upload CSV absensi Alpha berisi nomor karyawan milik Beta (`EMP-0001` ada di kedua tenant), berharap import menulis ke karyawan Beta; atau memicu export yang "lupa" scope |
| **Dihentikan oleh** | Job `attendance.import` membawa `tenant_id = alpha` dan berjalan di `WithTenantTxFor(alpha)` → lookup `employee_no` hanya menemukan karyawan Alpha (nomor karyawan unik **per tenant**, PI-18); export berjalan dengan RLS yang sama; file disimpan di prefix `tenants/alpha/` |
| **Response** | Baris menulis ke `EMP-0001` milik Alpha (memang milik Alpha), atau gagal `EMPLOYEE_NOT_FOUND` |
| **Bukti** | Test import dengan nomor karyawan yang sama di dua tenant |

### A8 — Bug developer: lupa `WHERE tenant_id`
| | |
|---|---|
| **Serangan** | Bukan serangan aktif. Endpoint baru `GET /v1/reports/headcount` ditulis dengan `SELECT department_id, count(*) FROM employees GROUP BY 1` tanpa filter tenant |
| **Dihentikan oleh** | RLS: query dijalankan oleh `payrun_app` di dalam `WithTenantTx(alpha)` → hanya baris Alpha yang terlihat; lint query (§2.5) menandai query tanpa `tenant_id` saat PR |
| **Response** | Hasil benar (hanya Alpha) |
| **Bukti** | **Killer Demo A step 3**: test yang sengaja menjalankan query ini dan memverifikasi hasilnya |

### A9 — Kebocoran lewat connection pool
| | |
|---|---|
| **Serangan** | Bukan serangan aktif. Kode memakai `SET app.tenant_id` (session) sehingga koneksi yang dipakai ulang oleh request Mallory masih membawa tenant Beta dari request sebelumnya |
| **Dihentikan oleh** | Hanya `set_config(…, true)` di dalam transaksi (ADR-P02); grep CI melarang `SET app.` tanpa `LOCAL`; `WithTenantTx` selalu men-set nilai di awal setiap transaksi; test pool leakage |
| **Response** | — |
| **Bukti** | Test `MaxConns = 1` |

### A10 — Menebak URL slip gaji
| | |
|---|---|
| **Serangan** | Mallory menyusun `https://storage/…/tenants/beta/payslips/{run}/{emp}.pdf` atau memakai ulang presigned URL lama |
| **Dihentikan oleh** | Bucket private (tanpa signature → 403 dari storage); presigned URL hanya dibuat server setelah metadata lolos RLS + policy (PI-17); signature mengikat key + expiry; TTL 5 menit |
| **Response** | `403` dari object storage / URL kedaluwarsa |
| **Bukti** | Audit log pembuatan presigned URL (siapa, slip siapa); access log storage |

### Layer diagram

```text
 Request Mallory (HR_ADMIN @ Alpha)
   │
   ▼
 [1] Edge / Authentication ──────── signature, alg whitelist, exp, rate limit      ⟵ A2, (A1 massal)
   │
   ▼
 [2] Tenant context ─────────────── tid dari token saja; DTO tanpa tenant_id       ⟵ A3
   │
   ▼
 [3] Authorization policy ───────── role + relasi (manager-of, self), 404/403      ⟵ akses intra-tenant
   │
   ▼
 [4] Query scoping ──────────────── WHERE tenant_id = $tid (+ lint)                 ⟵ A1, A4, A6
   │
   ▼
 [5] Row-Level Security ─────────── USING / WITH CHECK, FORCE, fail closed          ⟵ A8, A9, cadangan A1/A4/A6/A7
   │
   ▼
 [6] Composite FK ───────────────── (tenant_id, id) di setiap referensi            ⟵ A4, A5
   │
   ▼
 [7] Non-DB scoping ─────────────── job tenant_id, object key prefix, presigned    ⟵ A7, A10
   │
   ▼
 [8] Audit & detection ──────────── audit log, tenant_isolation_violation_total,   ⟵ semua (deteksi)
                                    auth_token_invalid_total, alert
```

---

## 5. Apa yang masih bisa menembusnya (residual risk)

| Risiko | Kenapa bisa menembus | Mitigasi |
|---|---|---|
| Role `payrun_owner` / kredensial migrasi bocor | Owner bisa `ALTER TABLE … DISABLE ROW LEVEL SECURITY` | Kredensial owner hanya di job migrasi CI/CD, tidak ada di container runtime; secret terpisah; V2: secret manager + audit akses |
| `payrun_app` diberi `BYPASSRLS` atau dijadikan owner karena salah konfigurasi | RLS diam-diam tidak berlaku | Test katalog di CI dan **readiness check saat startup**: api menolak start jika role-nya owner/BYPASSRLS |
| Fungsi `SECURITY DEFINER` / view milik owner | Berjalan dengan hak owner dan bisa melewati RLS | Dilarang di skema aplikasi (test katalog); view memakai `security_invoker = true` |
| Bug di jalur lintas tenant yang sah (`tenancy`, `iam`: login, daftar membership, switch tenant) | Jalur ini memang melihat lebih dari satu tenant | Policy RLS khusus berbasis `app.user_id`; endpoint minimal; test khusus (user tidak melihat membership orang lain) |
| Kunci JWT bocor | Penyerang bisa mencetak token dengan `tid` apa pun | Rotasi `kid`; secret di secret manager (V2); token pendek; operasi sensitif tetap cek membership di DB, jadi token palsu tanpa membership gagal di operasi tersebut |
| Penyalahgunaan oleh insider sah (HR Alpha membocorkan data Alpha) | Bukan masalah isolasi tenant | Audit akses slip/PII + alert pola tak wajar (V2) |
| Side channel (timing, ukuran response, error berbeda) | 404 lebih cepat untuk ID tenant lain? | Jalur 404 identik (query sama, hanya tanpa hasil); tidak ada error yang membedakan "ada tapi bukan milikmu" |
| Backup / dump DB bocor | Semua tenant dalam satu dump | PII terenkripsi (ADR-P08); backup terenkripsi at rest; V2: KMS per tenant |

> **Poin interview:** *"RLS bukan pengganti authorization. Ia adalah lapisan terakhir yang mengubah bug saya dari kebocoran data menjadi hasil kosong. Saya juga menguji bahwa RLS benar-benar aktif, karena RLS yang salah konfigurasi lebih berbahaya daripada tidak ada RLS: tim merasa aman padahal tidak."*
