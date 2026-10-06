# ADR-P01 — Multi-Tenancy: Pooled Tables + `tenant_id` + Postgres RLS

> Status: **Accepted** · Tanggal: 2026-10-01 · Berlaku untuk: **PayRun**

## Context / Problem

PayRun melayani banyak perusahaan dari satu codebase dan satu deployment. Kebocoran data gaji antar tenant adalah satu-satunya kegagalan yang bisa mematikan bisnis SaaS (Step 1, R2). Isolasi tidak boleh bergantung pada "developer selalu ingat menulis `WHERE tenant_id = ?`".

Pengalaman nyata dari pekerjaan sehari-hari: pernah mengoperasikan **schema-per-tenant** dengan biaya yang terasa langsung:
- migrasi harus diiterasi ke N schema (lambat, bisa gagal di tengah sehingga sebagian schema tertinggal versi);
- `search_path` harus diset benar di setiap koneksi; salah set berarti query ke schema yang salah;
- reporting lintas tenant butuh mekanisme tambahan (contoh: FDW ke DB report).

ADR ini memilih pendekatan yang **sengaja berbeda** agar perbandingan trade-off-nya bisa dijelaskan dari pengalaman, bukan teori. Tidak ada skema, nama, atau kode dari pekerjaan kantor yang dipakai.

## Options

| Kriteria | A. Pooled + filter di aplikasi saja | **B. Pooled + RLS** | C. Schema-per-tenant | D. DB-per-tenant |
|---|---|---|---|---|
| Kekuatan isolasi | Lemah: satu query lupa filter → bocor | **Kuat**: DB menolak baris tenant lain walau kode salah | Kuat jika `search_path` benar; salah set → bocor | Sangat kuat (batas fisik) |
| Biaya migrasi | 1× | **1×** | N× (iterasi per schema, drift versi) | N× + orkestrasi per DB |
| Connection pooling | Satu pool | **Satu pool** (konteks via `SET LOCAL`) | Satu pool, tapi `search_path` per transaksi | Pool per DB, cepat habis |
| Noisy neighbor | Shared | Shared (mitigasi: index, limit job per tenant) | Shared (DB sama) | Terisolasi |
| Backup/restore per tenant | Sulit (export filter) | Sulit (export filter) | Mudah (`pg_dump -n`) | Paling mudah |
| Biaya infra | Rendah | **Rendah** | Rendah–sedang (katalog membengkak di ribuan schema) | Tinggi |
| Kecepatan onboarding | Instan (INSERT) | **Instan (INSERT)** | Perlu create schema + jalankan migrasi (async) | Provision DB |
| Reporting lintas tenant (platform) | Mudah | Mudah (role khusus) | Sulit (UNION/FDW) | Sangat sulit |

## Decision

**B. Pooled tables, setiap tabel bisnis punya `tenant_id uuid NOT NULL`, ditegakkan oleh Postgres Row-Level Security** sebagai lapisan terakhir. Filter di aplikasi tetap ditulis (lapisan pertama); RLS menangkap yang terlewat.

```sql
ALTER TABLE employees ENABLE ROW LEVEL SECURITY;
ALTER TABLE employees FORCE ROW LEVEL SECURITY;   -- berlaku juga untuk owner tabel

CREATE POLICY tenant_isolation ON employees
  USING      (tenant_id = (SELECT payrun.current_tenant()))
  WITH CHECK (tenant_id = (SELECT payrun.current_tenant()));
```
> Helper `payrun.current_tenant()` = `nullif(current_setting('app.tenant_id', true), '')::uuid` (STABLE). Cast mentah gagal untuk `''`, yaitu nilai yang muncul setelah GUC pernah disetel di session yang sama, dan helper ini mencegahnya (Step 12: C3).

- `current_setting(..., true)` mengembalikan `NULL` jika belum diset, sehingga `tenant_id = NULL` → tidak ada baris (**fail closed**).
- `WITH CHECK` mencegah INSERT/UPDATE yang menulis baris ke tenant lain.

**Dua role database:**

| Role | Dipakai oleh | Hak |
|---|---|---|
| `payrun_owner` | `migrate` | Owner tabel, DDL. Tidak pernah dipakai oleh `api`/`worker` |
| `payrun_app` | `api`, `worker` | DML saja, **bukan owner, tanpa `BYPASSRLS`** |

**Integritas referensial lintas tenant** dicegah di level skema, bukan hanya RLS:

```sql
CREATE TABLE departments (
  id uuid NOT NULL, tenant_id uuid NOT NULL REFERENCES tenants(id),
  name text NOT NULL,
  PRIMARY KEY (tenant_id, id)   -- konvensi final Step 4: PK composite, bukan id + UNIQUE
);
CREATE TABLE employees (
  id uuid NOT NULL, tenant_id uuid NOT NULL,
  department_id uuid,
  PRIMARY KEY (tenant_id, id)   -- konvensi final Step 4: PK composite, bukan id + UNIQUE,
  FOREIGN KEY (tenant_id, department_id) REFERENCES departments (tenant_id, id)
);
```
Employee tenant A tidak mungkin menunjuk department tenant B, karena FK menyertakan `tenant_id`.

**Performa:** setiap index tabel bisnis diawali `tenant_id` (contoh `(tenant_id, status)`, `(tenant_id, employee_id, period)`). Policy RLS menjadi predikat tambahan yang dimanfaatkan planner. Hindari fungsi non-`STABLE`/`LEAKPROOF` di policy.

## Reason

1. **Defense in depth:** bug "lupa WHERE" di kode tidak berubah menjadi kebocoran data (Killer Demo A).
2. **Satu migrasi untuk semua tenant**: menghilangkan masalah drift versi yang dialami di schema-per-tenant.
3. **Operasional sederhana untuk solo developer:** satu pool, satu skema, onboarding tenant = INSERT.
4. **Nilai interview:** perbandingan B vs C didukung pengalaman nyata mengoperasikan C.

## Trade-offs

- **RLS mudah salah konfigurasi**: owner bypass tanpa `FORCE`, role dengan `BYPASSRLS`, setting yang bocor lewat pool (ditangani ADR-P02). Wajib ada test khusus.
- **Noisy neighbor** tetap ada: tenant besar berbagi tabel dan I/O yang sama.
- **Backup/restore per tenant** sulit; restore satu tenant butuh export terfilter, bukan `pg_restore` biasa.
- **Debugging** lebih membingungkan: query manual sebagai `payrun_app` tanpa setting → hasil kosong.
- Kolom `tenant_id` di setiap tabel dan composite FK menambah verbosity skema.

## Future Migration

| Pemicu | Langkah |
|---|---|
| Tenant enterprise meminta isolasi fisik / data residency | **Hybrid pool/silo (V3)**: tabel `tenant_catalog` menyimpan `placement` (pool / dedicated DB); router koneksi memilih DB berdasarkan tenant. Skema identik, sehingga RLS tetap berlaku di silo |
| Satu tenant > ~20% beban tulis | Pindahkan ke silo, atau partisi tabel besar (`payroll_items`, `attendance`) berdasarkan `tenant_id` (hash/list) |
| Kebutuhan restore per tenant rutin | Tooling export/import per tenant berbasis `COPY … WHERE tenant_id = $1` dengan urutan FK |

## Interview angle

- **"Kenapa tidak schema-per-tenant?"** → Saya pernah mengoperasikannya: migrasi N schema, risiko drift, `search_path`. Untuk UMKM berukuran kecil-menengah, pooled + RLS lebih murah dan isolasinya ditegakkan DB. Saya akan beralih ke silo untuk tenant yang butuh isolasi fisik.
- **"Bagaimana kamu tahu RLS benar-benar aktif?"** → `FORCE RLS`, role app bukan owner, test yang sengaja menjalankan query tanpa filter dan memverifikasi hasilnya hanya tenant aktif, plus test tanpa setting → 0 baris.
- **"Apa yang tidak dilindungi RLS?"** → Object storage, cache, log, dan job payload. Semuanya harus tenant-aware sendiri (ADR-P02, ADR-012).

## Related

ADR-P02 (tenant context), ADR-004 (PostgreSQL), ADR-010 (authorization), ADR-012 (object storage key prefix), Step 1 `02-payrun.md` §9 C1–C2, C10; `03-cross-project.md` §4.
