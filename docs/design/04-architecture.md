# Step 2.4 — PayRun: System Architecture V1 → V2 → V3

> Prinsip sama dengan LedgerPay: mulai paling sederhana, lalu berubah hanya karena *pemicu terukur*.
> Keputusan rinci: lihat [ADR register](00-index.md#adr-register).

---

## 1. Requirement yang menggerakkan arsitektur

| Requirement | Konsekuensi arsitektural |
|---|---|
| Isolasi tenant tidak boleh bergantung pada developer yang tidak pernah lupa `WHERE tenant_id` | Isolasi ditegakkan di **database** (RLS), dengan tenant context yang wajib di setiap transaksi |
| Payroll banyak karyawan bisa lama dan bisa crash | Kalkulasi **asinkron**, chunked, idempoten, resumable |
| Hasil final harus reproducible dan immutable | Engine sebagai pure function + snapshot input + versi ruleset |
| Banyak entitas saling bergantung (employee, salary, attendance, leave, approval, run) | Satu database relasional dengan constraint kuat; modular monolith |
| Dokumen (slip PDF, CSV) bukan data relasional | Object storage, dengan metadata di DB |
| Biaya rendah, dikerjakan solo | Skeleton yang sama dengan LedgerPay, satu VM |

---

## 2. Version 1 — Portfolio MVP (yang akan dibangun)

### 2.1 Context diagram
```text
  ┌──────┐  ┌────────┐  ┌──────┐  ┌──────┐
  │ Dewi │  │ Hendra │  │ Arif │  │ Tono │      (user dari Tenant A, B, C, …)
  │  HR  │  │Payroll │  │ Mgr  │  │ Emp  │
  └──┬───┘  └───┬────┘  └──┬───┘  └──┬───┘
     └──────────┴──── browser ───────┘
                       ▼
        ┌───────────────────────────────┐        ┌─────────────────────┐
        │            PayRun             │───────▶│ Object storage      │
        │ (multi-tenant workforce &     │        │ (payslip PDF, CSV)  │
        │  payroll SaaS)                │        └─────────────────────┘
        └───────────────────────────────┘
                       │  (stretch P3)
                       ▼
                 LedgerPay payout API
```

### 2.2 Container diagram
```text
                       ┌──────────────────────────────┐
  Browser ──HTTPS──▶   │ Caddy (reverse proxy, TLS)   │
                       └──────┬───────────────┬───────┘
                              ▼               ▼ /api/*
                  ┌──────────────────┐  ┌────────────────────────────────────┐
                  │ web (Vue SPA)    │  │ payrun api (Go)                    │
                  └──────────────────┘  │ middleware: request_id → auth (JWT)│
                                        │ → tenant (claim tid) → handler     │
                                        └──────────────┬─────────────────────┘
                                                       │ WithTenantTx:
                                                       │ BEGIN; set_config('app.tenant_id', $1, true)
                                        ┌──────────────▼─────────────────────┐
                                        │ PostgreSQL                         │
                                        │ pooled tables + tenant_id + RLS    │
                                        │ role: payrun_owner (migrasi)       │
                                        │       payrun_app (runtime, no RLS  │
                                        │       bypass)                      │
                                        └──────────────▲─────────────────────┘
                                                       │
                                        ┌──────────────┴─────────────────────┐
                                        │ payrun worker (Go)                 │
                                        │ - payroll.plan / calc_chunk │
                                        │ - payslip.render (P1)              │
                                        │ - attendance.import (P1)           │
                                        │ - schedulers: accrual (P2), cleanup│
                                        └──────────────┬─────────────────────┘
                                                       ▼
                                        ┌────────────────────────────────────┐
                                        │ MinIO (lokal) / S3-compatible      │
                                        │ hanya setelah PF-21/PF-22 (P1)     │
                                        └────────────────────────────────────┘

  Observability: sama dengan LedgerPay (Prometheus, Grafana, log JSON) + tenant_id di log
```

### 2.3 Module view
```text
internal/
├── platform/        (disalin dari LedgerPay) + db.WithTenantTx + tenancy context + storage client
└── modules/
    ├── tenancy/     tenant, signup, konfigurasi default
    ├── iam/         user, membership, role, invitation, switch-tenant
    ├── org/         department, position, reporting line (effective-dated)
    ├── employee/    employee, PII (encrypt/mask), status lifecycle
    ├── compensation/ component definition, salary assignment (effective-dated), ruleset
    ├── attendance/  daily record, import, work calendar
    ├── leave/       leave type, balance, request
    ├── overtime/    overtime request
    ├── approval/    approval request generik + inbox
    ├── payroll/     engine (pure, tanpa dependency), run, item, adjustment, snapshot
    ├── payslip/     view, PDF render
    └── audit/       audit log
```

**Aturan dependency tambahan (selain aturan LedgerPay):**
1. `payroll/engine` adalah package **pure**: tidak boleh meng-import `platform/db`, `time.Now`, atau modul lain. Input dikumpulkan oleh `payroll/run` dari modul lain lewat interface *input provider*.
2. Modul yang menyediakan input payroll (`attendance`, `leave`, `overtime`, `compensation`) wajib memeriksa **period lock** sebelum menulis. Pemeriksaan ini lewat interface `payroll.PeriodGuard`, agar logika lock tidak tersebar.
3. Tidak ada kode yang membuka koneksi DB tanpa `WithTenantTx`, kecuali modul `tenancy`/`iam` untuk operasi lintas tenant yang eksplisit (signup, membership lookup), yang memakai role/policy terpisah. Detailnya di ADR-P02.

### 2.4 Alur kunci V1

**Request tenant-scoped**
```text
JWT (sub=user, tid=tenant, roles) → middleware memvalidasi signature + exp
→ ctx.Tenant = tid → handler → service → WithTenantTx(ctx, tid, fn)
   BEGIN; SELECT set_config('app.tenant_id', tid, true); …query… ; COMMIT
→ RLS: setiap baris difilter USING (tenant_id = (SELECT payrun.current_tenant()))
```

**Payroll run**
```text
API  : POST /payroll-runs/{id}/calculate
       tx: run DRAFT→CALCULATING, calc_version++, job payroll.plan → 202
Worker orchestrate (WithTenantTx):
       ambil daftar employee aktif di periode → buat K chunk job (≈50 employee/job) + set chunks_total
Worker calc_chunk (paralel, at-least-once):
       baca input chunk (snapshot konsisten) → engine.Calculate per employee (in-memory)
       tx: upsert item (run_id, employee_id, calc_version) + tandai chunk selesai
           + UPDATE run SET chunks_done = chunks_done + 1 RETURNING …
       chunk terakhir → hitung total + fingerprint → CALCULATED
Crash → lease habis → chunk diulang → upsert menimpa dengan hasil identik (deterministik)
```

**Finalize**
```text
tx: cek status APPROVED + fingerprint input saat ini == fingerprint kalkulasi
    → simpan snapshot per item → run FINALIZED → period FINALIZED (terkunci) → job payslip.render
```

### 2.5 Deployment V1
```text
1 VM ── docker compose:
  caddy · web · api · worker · postgres · minio (saat P1) · [prometheus · grafana]
Postgres init: buat role payrun_owner & payrun_app; migrasi dijalankan sebagai owner
CI/CD: sama dengan LedgerPay (ADR-013)
```

### 2.6 Batas V1 yang diakui
| Batas | Dampak | Kenapa diterima |
|---|---|---|
| Semua tenant berbagi satu DB dan satu worker pool | Tenant besar bisa memperlambat tenant kecil (noisy neighbor) | Belum ada tenant besar; fairness masuk P2 (PF-27) |
| Backup/restore per tenant tidak bisa granular | Restore satu tenant = restore selektif dari dump (sulit) | Trade-off pooled yang diterima (ADR-P01) |
| Pencabutan membership berlaku ≤ 15 menit | Jendela akses kecil setelah dicabut | Operasi sensitif tetap memeriksa ulang (ADR-P02) |
| Kunci enkripsi PII di env | Kunci bocor bersama server | V2 memakai KMS (ADR-P08) |

---

## 3. Version 2 — Production Readiness

**Pemicu:** tenant berbayar pertama, kewajiban perlindungan data pribadi (UU PDP), dan payroll yang harus selesai di tanggal gajian (SLA).

```text
  LB/WAF → api×N (stateless) ─┬─ Redis (rate limit; cache config tenant dengan key tenant-aware)
                              ▼
            Managed Postgres HA + PITR ── read replica (laporan, export)
                              ▲
   worker pool terpisah: [payroll-calc] [documents] [default]
        + batas konkurensi per tenant (fairness)
                              │
            S3 (SSE + bucket policy) · KMS (envelope key per tenant)
  Observability: OTel traces, log terpusat (PII-redacted), SLO "payroll selesai < X menit untuk N karyawan"
```

| Perubahan | Pemicu | Kenapa |
|---|---|---|
| Worker pool per jenis job + limit per tenant | Run tenant besar menunda slip/run tenant lain | Bulkhead + fairness |
| KMS envelope encryption per tenant | UU PDP, permintaan penghapusan data tenant | *Crypto-shredding*: hapus DEK tenant = data tidak bisa dibaca lagi |
| Read replica untuk laporan/export | Query laporan berat mengganggu transaksi | Laporan boleh eventual |
| Redis (rate limit, cache konfigurasi) | > 1 instance api; konfigurasi tenant sering dibaca | Key cache wajib diawali tenant_id; invalidasi saat konfigurasi ditulis |
| SSO (OIDC) untuk tenant korporat | Permintaan pelanggan | IAM sudah memisahkan identitas dari membership |
| Data retention & export per tenant | Offboarding tenant, hak subjek data | Job export/hapus per tenant, tervalidasi |
| Break-glass support access dengan approval + TTL | Tim support bertambah | Akses lintas tenant yang eksplisit dan ter-audit (PF-28) |

---

## 4. Version 3 — Scaling Architecture (hanya didiskusikan)

**Pemicu terukur:**
- Satu atau beberapa tenant sangat besar (misalnya puluhan ribu karyawan) dengan kebutuhan isolasi/kinerja/kontrak khusus.
- Jumlah tenant dan data membuat satu cluster Postgres sulit dioperasikan (ukuran, vacuum, waktu restore).
- Kebutuhan data residency per negara/pelanggan.

```text
                         Tenant catalog (tenant_id → cell/DB, tier)
                                   │
          ┌────────────────────────┼─────────────────────────┐
          ▼                        ▼                         ▼
   Cell 1: pooled DB        Cell 2: pooled DB         Silo: dedicated DB
   (ribuan tenant kecil)    (ribuan tenant kecil)     (enterprise tenant)
          │                        │                         │
          └──── kode aplikasi yang sama; routing koneksi berdasarkan tenant catalog ────┘

   Payroll compute: worker yang di-scale horizontal per cell; run tenant besar dipecah lebih halus
   Reporting: CDC → warehouse terpisah (bukan query ke DB transaksional)
```

| Perubahan | Kenapa | Harga yang dibayar |
|---|---|---|
| **Cell-based / hybrid pool–silo** | Membatasi *blast radius*; tenant besar mendapat DB sendiri tanpa mengubah kode | Routing koneksi, migrasi ke banyak DB (ironisnya kembali ke masalah "migrasi N schema", tetapi hanya untuk segelintir silo) |
| Tenant catalog | Satu sumber kebenaran untuk lokasi tenant | Komponen kritis baru; harus HA |
| Pindah tenant antar cell | Rebalancing | Butuh prosedur migrasi online per tenant; composite key `tenant_id` dari V1 memudahkannya |
| Warehouse untuk reporting | Laporan lintas periode yang berat | Pipeline data, eventual consistency |

> **Poin interview:** keputusan V1 yang membuat V3 mungkin: `tenant_id` di setiap tabel dan setiap composite key, akses DB hanya lewat `WithTenantTx`, dan job yang selalu membawa `tenant_id`. Dengan begitu, memindahkan satu tenant ke DB lain adalah soal **routing**, bukan rewrite.

---

## 5. Ringkasan evolusi

| Aspek | V1 (dibangun) | V2 (production) | V3 (scale) |
|---|---|---|---|
| Tenancy | Pooled + RLS | Pooled + RLS + fairness | Cell/hybrid pool–silo + tenant catalog |
| Proses | api + worker | api×N + worker pool per jenis | Per cell |
| Database | 1 Postgres | Managed HA + PITR + replica | Banyak cluster + warehouse |
| Queue | Postgres job (shared runner) | + limit per tenant | Per cell |
| Storage | MinIO/S3 (P1) | S3 + SSE + KMS | Per region/residency |
| Encryption | AES-GCM, kunci dari env | KMS envelope per tenant | + per-region key |
| Auth | JWT + membership | + SSO OIDC, break-glass | + SCIM provisioning |
