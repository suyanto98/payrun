# PRD — PayRun: Workforce & Payroll Platform (Multi-Tenant SaaS)

> PRD terpadu, disusun dari keadaan **final** dokumen desain, setelah semua amandemen dan penyelarasan. Kalau isinya berbeda dengan dokumen step lama, yang berlaku adalah PRD ini dan dokumen yang dirujuknya.
> Status: Final draft untuk coding · Tanggal: 2026-10-02 · Pemilik: (penulis portfolio) · Project kedua, dikerjakan setelah LedgerPay M4.

---

## 1. Overview

**Pitch:** *A multi-tenant workforce and payroll platform where multiple companies can securely manage employees, attendance, approvals, and payroll while keeping tenant data isolated.*

**Vision:** payroll yang benar dan bisa dijelaskan baris per baris. Hasil yang sudah final tidak bisa diubah diam-diam, dan data setiap perusahaan tidak bisa bocor ke perusahaan lain, bahkan ketika ada bug di kode.

**Posisi dalam portfolio:** PayRun menunjukkan **breadth**: domain bisnis yang luas, isolasi tenant, rule engine yang reproducible, dan batch processing. Ini melengkapi LedgerPay yang menunjukkan **depth** (correctness di bawah concurrency dan failure).

**Bentuk sistem:** modular monolith Go + PostgreSQL (pooled tables + `tenant_id` + Row-Level Security), worker async berbasis Postgres job queue, object storage S3-compatible untuk dokumen, dan SPA Vue 3. Detail di `04-architecture.md` dan ADR-P01…P09.

---

## 2. Problem, Goals & Non-Goals

### 2.1 Problem
1. Payroll UMKM dihitung di spreadsheet: rawan salah, tanpa audit trail, dan versi data yang dipakai tidak jelas.
2. Input (absensi, lembur, kenaikan gaji) terus berubah sampai detik terakhir. Tanpa *stale detection*, *period lock*, dan *snapshot*, tidak ada yang tahu angka final dihitung dari data versi mana.
3. Data gaji adalah data paling sensitif di perusahaan. Di model SaaS, satu kebocoran antar tenant bisa mematikan bisnis penyedia.
4. Approval cuti dan lembur lewat chat tidak meninggalkan jejak.

### 2.2 Goals
| # | Goal | Ukuran keberhasilan |
|---|---|---|
| G1 | Isolasi tenant yang tetap bertahan ketika ada bug aplikasi | 100% endpoint tenant-scoped lolos cross-tenant test; RLS bypass test hijau; metrik `payrun_tenant_isolation_violations_total` = 0 |
| G2 | Payroll benar, deterministik, dan bisa dijelaskan | Golden test 30 kasus hijau; recompute-from-snapshot identik; setiap baris slip punya *explanation* |
| G3 | Hasil final immutable, koreksi tetap bisa ditelusuri | 0 perubahan pada run `FINALIZED`; koreksi lewat adjustment ke periode berikutnya |
| G4 | Payroll tahan crash dan eksekusi ganda | PFS-01…10 punya test otomatis yang hijau; tidak pernah ada item/run ganda |
| G5 | Approval dengan jejak dan aturan relasi | Authorization matrix test 100% endpoint; tidak ada self-approval di luar pengecualian terdokumentasi |

### 2.3 Non-Goals
- Bukan HRIS lengkap (tidak ada recruitment, performance review, training, KPI).
- **Tidak mengklaim kepatuhan pajak/BPJS.** Aturan PPh21 dan BPJS adalah versi *simplified* yang dapat dikonfigurasi, **tidak tersertifikasi** (demo script §2).
- **Tidak mengklaim kepatuhan UU PDP.** PayRun "menerapkan praktik perlindungan data": enkripsi PII, masking, audit akses (O17).
- Tidak ada disbursement ke bank sungguhan di V1.
- Tidak ada klaim kapasitas production. Angka performa adalah baseline lingkungan uji.

---

## 3. Personas & Key Journeys

| Persona | Peran | Kebutuhan utama |
|---|---|---|
| **Dewi** | HR admin / OWNER tenant | Data karyawan rapi, payroll tepat waktu |
| **Hendra** | Payroll / finance admin (`PAYROLL_ADMIN`) | Hasil bisa dicek, dibandingkan dengan bulan lalu, lalu dikunci |
| **Arif** | Manager (`MANAGER`) | Menyetujui cuti/lembur bawahan langsung dengan cepat dan ter-audit |
| **Tono** | Employee (`EMPLOYEE`) | Clock in/out, mengajukan cuti/lembur, melihat slip gaji sendiri |
| **Platform operator** | Internal SaaS | Support **tanpa** backdoor lintas tenant (break-glass = P2) |

| Journey | Alur ringkas | Fitur |
|---|---|---|
| **W1** Onboarding tenant | Signup → tenant + OWNER → seed default (settings, jenis cuti, komponen, kalender) → undang user | PF-01, PF-02 |
| **W2** Employee & salary | Tambah karyawan (PII terenkripsi) → gaji + komponen *effective-dated* → kenaikan gaji = record baru | PF-05, PF-06, PF-07, PF-20 |
| **W3** Attendance, leave, overtime | Clock in/out atau import CSV → request cuti/lembur → approval atasan langsung (saldo cuti berkurang di transaksi yang sama) | PF-08…PF-11, PF-21 |
| **W4** Payroll run | Create run (periode dibuat otomatis) → calculate (async, chunk) → review + warning + diff → submit → approve (lewat `/approvals`, dengan stale guard) → finalize (snapshot dibekukan, periode `FINALIZED`) → slip + export | PF-12…PF-14, PF-19, PF-22, PF-23 |
| **W5** Payslip & koreksi | Employee melihat slip sendiri (akses HR ter-audit) → koreksi pasca-final lewat adjustment ke run berikutnya | PF-15, PF-19 |

---

## 4. Scope

| Prioritas | Fitur |
|---|---|
| **P0 (MVP + demo)** | PF-01 signup · PF-02 IAM/membership/switch tenant · PF-03 tenant isolation (context + RLS) · PF-04 isolation & authz test suite · PF-05 org · PF-06 employee · PF-07 salary effective-dated · PF-08 clock in/out · PF-09 leave · PF-10 overtime · PF-11 approval satu tingkat · PF-12 payroll engine · PF-13 run lifecycle + async + resume · PF-14 finalize + snapshot + period lock · PF-15 payslip web · PF-16 audit log · PF-17 health + log dengan `tenant_id` · PF-18 UI minimal |
| **P1 (wajib sebelum "selesai")** | PF-19 adjustment pasca-final · PF-20 PII encryption + masking · PF-21 import CSV absensi · PF-22 payslip PDF + object storage · PF-23 diff vs bulan lalu + warning · PF-24 metrik tenant-aware · PF-29 rate limiting (otomatis dari skeleton + tier `heavy` per tenant) |
| **P2 (jika ada waktu)** | PF-25 export file transfer bank · PF-26 leave accrual terjadwal · PF-27 fairness job per tenant · PF-28 break-glass support access |
| **P3 (diskusi / stretch)** | PF-30 email · PF-31 laporan · PF-32 approval multi-level · PF-33 disbursement via LedgerPay |
| **Out of scope** | Kepatuhan pajak/BPJS tersertifikasi & integrasi DJP/BPJS · disbursement bank nyata · formula DSL buatan user · face recognition/GPS/mesin fingerprint · recruitment/performance/KPI · multi-country/currency · SSO/SAML/SCIM (V2/V3) · schema/DB per tenant (sengaja pooled + RLS) · microservices · mobile app native · billing SaaS · shift scheduling · periode non-kalender (mis. 21–20) |

---

## 5. Functional Requirements (ringkas)

> Spesifikasi lengkap beserta acceptance criteria ada di `03-features.md`. Kontrak endpoint ada di `16-api-endpoints.md`, dan matriks role di `17-api-authorization-matrix.md`.

| ID | Requirement inti | Acceptance (ringkas) |
|---|---|---|
| PF-01 | Signup membuat tenant + user + membership OWNER dalam satu transaksi; seed default dapat diubah | Signup ganda (key sama) → satu tenant (`platform_idempotency_keys`) |
| PF-02 | Role per membership (OWNER, HR_ADMIN, PAYROLL_ADMIN, MANAGER, EMPLOYEE); undangan sekali pakai 7 hari, email harus cocok; `POST /v1/session/switch-tenant` menerbitkan token ber-`tid` | Token dengan `tid` hasil manipulasi ditolak; membership dicabut → akses hilang ≤ 15 menit, operasi sensitif cek ulang; owner terakhir tidak bisa dicabut (`LAST_OWNER`) |
| PF-03 | Semua tabel bisnis `tenant_id` + RLS `FORCE`; akses DB hanya lewat `WithTenantTx`; tenant **hanya** dari claim `tid`, bukan dari path/header/body | Tanpa context → 0 baris (*fail closed*); ID tenant lain → 404 |
| PF-04 | Suite cross-tenant + authorization matrix + RLS bypass + pool leakage, berjalan di CI | Endpoint tanpa baris matriks → CI gagal |
| PF-05 | Departemen, posisi, atasan langsung (effective-dated, asiklik) | Siklus atasan → `MANAGER_CYCLE` |
| PF-06 | Employee + lifecycle (ACTIVE/RESIGNED/TERMINATED), tautan ke user, filter + cursor | `EMPLOYEE_NO_TAKEN`, `NIK_TAKEN` per tenant |
| PF-07 | Komponen bertipe data + salary/komponen *effective-dated*; perubahan = tutup record lama + buka record baru | Tumpang tindih → `OVERLAPPING_PERIOD`; perubahan ke periode final → `PERIOD_LOCKED` |
| PF-08 | Clock in/out idempoten, koreksi HR ber-alasan selama periode belum final | `CLOCK_OUT_WITHOUT_CLOCK_IN`; `INCOMPLETE` ditinjau HR |
| PF-09 | Jenis cuti, saldo, request; saldo berkurang saat approve | Saldo tidak cukup → `INSUFFICIENT_LEAVE_BALANCE`; tumpang tindih → `OVERLAPPING_LEAVE` |
| PF-10 | Request lembur; dihitung jika `APPROVED` sebelum cut-off | Lewat cut-off → koreksi lewat adjustment bulan berikutnya |
| PF-11 | Approval generik satu tingkat; approver di-snapshot saat submit (atasan → HR_ADMIN → OWNER) | Approve vs reject bersamaan → satu menang (`APPROVAL_ALREADY_DECIDED`); self-approval → `SELF_APPROVAL_FORBIDDEN` |
| PF-12 | Engine pure function, pipeline tetap, baris ber-*explanation*, ruleset ber-versi | Golden 30 kasus; property `net = Σ lines` |
| PF-13 | Run state machine; satu run aktif per (tenant, periode); calculate async dalam chunk; resume setelah crash; recalculate = `calc_version` baru; approve via `/approvals` dengan stale guard | Run ganda admin lain → `ACTIVE_RUN_EXISTS`; input berubah → `RUN_STALE {employees_changed}`; item ERROR → `RUN_HAS_ERRORS` sampai di-exclude |
| PF-14 | Snapshot ditulis saat kalkulasi, **dibekukan** saat finalize; periode `OPEN → PROCESSING → FINALIZED` | Ubah run final → `RUN_FINALIZED`; ubah input periode final → `PERIOD_LOCKED`; `verify-run` identik |
| PF-15 | Employee melihat slip sendiri; HR melihat dengan audit; nilai sensitif ter-mask | Employee lain → 404 |
| PF-16 | Audit append-only di bawah RLS untuk aksi sensitif | Ditulis di transaksi yang sama |
| PF-17 | `/healthz`, `/readyz` (versi DB ≥ versi minimum binary), log JSON dengan `tenant_id` + redaction PII | Test redaction |
| PF-18 | UI: signup, pilih tenant, dashboard HR, employee, approval inbox, payroll run, self-service, demo toolbar (mode demo) | Fokus pada halaman run + slip |
| PF-19 | Adjustment ± ke run berikutnya, mereferensikan periode asal | Diterapkan tepat sekali ke satu run final |
| PF-20 | NIK/NPWP/rekening dienkripsi AES-256-GCM (AAD tenant+field) + blind index; full view ter-audit | Default response ter-mask |
| PF-21 | Upload CSV ≤ 5 MB / ±50.000 baris → job validasi → laporan per baris | File sama → `IMPORT_DUPLICATE`; baris periode final ditolak |
| PF-22 | PDF slip async (batch 20), bucket private, presigned URL 5 menit setelah otorisasi + audit | Belum siap → `PAYSLIP_NOT_READY` |
| PF-23 | Diff net vs bulan lalu + flag > ambang + ringkasan warning | Tampil di review run |
| PF-24 | Metrik payroll & tenancy (kardinalitas aman) | Dashboard "Payroll Runs", "Tenancy & Security" |

---

## 6. Business Rules & Invariants

| Business rule (strategi produk) | Invariant penjaga (`08-invariants.md`) |
|---|---|
| BR1 Data milik tepat satu tenant | PI-01 (RLS + context), PI-02 (composite FK) |
| BR2 Satu run aktif per periode | PI-03 (partial unique index) |
| BR3 Run final immutable; koreksi via adjustment | PI-04 (trigger + `FOR SHARE`), PI-15 |
| BR4 Total = Σ item; item = Σ komponen | PI-05, PI-06 |
| BR5 Eligibility + prorata | PI-07 |
| BR6 Hanya cuti/lembur APPROVED sebelum cut-off | PI-08 |
| BR7 Tanpa self-approval; manager hanya untuk bawahan langsung (pengecualian: run di tenant satu admin, alasan wajib, dihitung server) | PI-11 |
| BR8 Saldo cuti tidak negatif | PI-10 |
| BR9 Effective dating tanpa menimpa riwayat | PI-09 |
| BR10 Slip hanya untuk pemilik + role berwenang, akses ter-audit | PI-17 |
| BR11 Determinisme | PI-12, PI-13 (stale guard), PI-14 (period lock) |
| — | PI-16 (≥ 1 OWNER, advisory lock), PI-18 (keunikan per tenant), PI-19 (atasan asiklik) |

Setiap invariant P0 punya minimal satu penjaga di level database atau transaksi. Validasi aplikasi saja tidak dianggap cukup (desain domain §mekanisme).

---

## 7. Multi-Tenancy & Data Protection Requirements

| Requirement | Penjaga |
|---|---|
| Tenant context tunggal dari access token (`tid`), divalidasi terhadap membership saat diterbitkan | ADR-P02 |
| RLS `ENABLE` + `FORCE` di semua tabel tenant; `payrun_app` bukan owner dan `NOBYPASSRLS`; CI catalogue test (allowlist `SECURITY DEFINER` = `auth_lookup_user` saja) | ADR-P01, C-36 |
| `set_config('app.tenant_id', …, true)` transaction-local (anti kebocoran via pool) | ADR-P02, pool leakage test |
| Composite FK `(tenant_id, id)` | C-37 |
| Job membawa `tenant_id` (trigger `jobs_enforce_tenant`); handler hanya memakai kolom itu | C-43 |
| Deteksi defensif `row.tenant_id == ctx.Tenant` → 500 + metrik INVARIANT | C-46 |
| Object key diawali `tenants/{tenant_id}/`; bucket private; presigned 5 menit | ADR-012, C-40 |
| PII: enkripsi aplikasi + blind index + masking + audit full view + redaction log | ADR-P08, C-22, C-23 |
| Undangan: hash SHA-256 secret 32 byte, single use, email cocok, secret di fragment URL | Threat model T-PR |
| Separation of duties: HR mengelola gaji, PAYROLL_ADMIN menjalankan payroll; gabungan role diizinkan tetapi diaudit | ADR-010 amandemen |
| Backup memakai role `payrun_backup` (`BYPASSRLS`, SELECT saja), karena `FORCE RLS` juga berlaku untuk owner | Desain DevOps |

Threat scenario "user Company A mengakses employee Company B" beserta lapisan yang menghentikannya ada di `06-multi-tenancy.md`. Threat model lengkap (20 threat) ada di `19-security-threat-model.md`.

---

## 8. Payroll Calculation Requirements

| Aspek | Requirement |
|---|---|
| Tipe komponen | `FIXED`, `PERCENT_OF` (dengan cap), `PER_DAY` (`PRESENT`/`WORKING`), `PER_HOUR_MULTIPLIER`. **Tanpa DSL** (ADR-P03) |
| Pipeline | Proration → earnings → overtime → absence → BPJS → tax → adjustments → net |
| Validasi konfigurasi | Referensi tidak ada / stage salah → `INVALID_COMPONENT_REFERENCE`; siklus → `COMPONENT_CYCLE`; defensif di engine → item `ERROR` |
| Pembulatan | Sekali per baris komponen (IDR 0 desimal, default `HALF_UP`, dapat dikonfigurasi). Net = Σ baris yang sudah dibulatkan, sehingga eksak dan bisa dijumlahkan sendiri oleh karyawan (ADR-P06) |
| Presisi | `NUMERIC` di DB + `shopspring/decimal`; uang di JSON selalu string; tanpa float |
| Periode | Bulan kalender, timezone tenant; cut-off default hari ke-2 bulan berikutnya 23:59 (konfigurasi) |
| Prorata | Default **hari kerja** (kalender kerja + libur tenant); hari kalender sebagai opsi; multi-segmen untuk perubahan gaji di tengah periode |
| Lembur | Hanya `APPROVED` ≤ cut-off; tarif per jam = gaji pokok / 173 (**simplified, konfigurasi**); multiplier bertingkat |
| Absensi | Cuti tidak dibayar dan absen tanpa keterangan dipotong per hari kerja memakai tarif harian segmen yang berlaku |
| BPJS & PPh21 | **Simplified — not compliance-certified.** Ruleset ber-versi dan effective-dated (`ID-SIMPLIFIED-2026.1`). Tarif contoh adalah placeholder |
| Net negatif | Item `ERROR NEGATIVE_NET`; carry-over tidak didukung di V1; submit diblok sampai diperbaiki atau di-exclude dengan alasan |
| Reproducibility | Input snapshot + versi ruleset per item; `verify-run` (recompute) identik; fingerprint per item untuk audit |
| Contoh acuan | Tono, Maret 2026 → net Rp6.888.609 (`07-payroll-engine.md` §15) |

---

## 9. Non-Functional Requirements

| Kategori | Requirement |
|---|---|
| **Konsistensi** | Read Committed + lock eksplisit + guard status; urutan lock global (idempotency → `tenant_settings` → `payroll_periods` → `payroll_runs` → approval → saldo → subjek); approve/finalize mengunci periode `FOR UPDATE`, penulis input `FOR SHARE`; stale detection berbasis change log yang dibuktikan dengan argumen lock (desain transaksi & concurrency O3/O12/D1) |
| **Reliability** | Kalkulasi chunked, idempoten, resumable (fencing `CompleteInTx`, `calc_version` guard); 10 failure scenario PFS-01…10 (crash, eksekusi ganda, input berubah, DB failure, retry, concurrent run, konfigurasi invalid, rejection, akses lintas tenant, partial processing) wajib punya test |
| **Security** | Kontrol MVP threat model (C-01…C-46 yang relevan): JWT pendek + refresh rotation, argon2id + semaphore, rate limit (tier `heavy` per tenant), header keamanan, CSV formula escape, validasi upload |
| **Performa (target desain, bukan klaim)** | Kalkulasi 1.000 karyawan < 60 dtk; p95 endpoint baca < 300 ms; ketersediaan 99,5%/30 hari di lingkungan demo. Diukur dengan skenario PR-1…PR-7 (termasuk tenant 10.000 karyawan, 100.000 karyawan total, 1.000 run). Hasil dilaporkan sebagai baseline VM 2 vCPU/4 GB |
| **Observability** | Log JSON dengan `tenant_id`/`correlation_id`/`trace_id`; metrik payroll & tenancy; trace request → job chunk; alert run gagal dan pelanggaran isolasi (critical) |
| **Operability** | Deploy via CI dengan health gate + auto rollback; migrasi expand/contract (tabel tenant baru wajib RLS di migrasi yang sama); kill switch `PR_PAYROLL_CALC_ENABLED`, `PR_PAYSLIP_RENDER_ENABLED`; backup harian terenkripsi + restore drill yang memverifikasi jumlah baris > 0; RPO 24 jam / RTO 2 jam (V1) |
| **Maintainability** | Engine package pure (tanpa DB/waktu/jaringan, ditegakkan linter); akses DB hanya lewat `platform/db`; ADR untuk setiap perubahan keputusan |

---

## 10. Success Metrics

| Jenis | Metrik | Target |
|---|---|---|
| Engineering | Endpoint tenant-scoped yang tercakup cross-tenant test | 100% |
| Engineering | Pelanggaran invariant payroll di test dan di metrik runtime | 0 |
| Engineering | Golden test engine | ≥ 30 kasus hijau |
| Engineering | Hasil akhir run dengan crash = hasil tanpa crash | Selalu identik |
| Engineering | Traceability PI/PFS → test | 100% |
| Demo | Demo A–C dari nol (`make demo-verify`) | ≤ 10 menit, hijau nightly 7 hari |
| Produk (hipotetis) | Waktu periode dibuka → final; % run di-reject/recalc; adjustment per 100 karyawan | Diamati, bukan target |

---

## 11. Milestones

| Milestone | Target | Kriteria |
|---|---|---|
| **M5 — Tenant isolation proven** | Akhir Sprint 2 PayRun | Demo A (isolation suite + RLS bypass test) hijau di CI |
| **M6 — Payroll correct & resumable** | Akhir Sprint 5 | Golden 30 kasus + recompute-from-snapshot + Demo B (via test subprocess); PFS-01/02/06 hijau |
| **M7 — PayRun selesai** | Akhir Sprint 10 | Seluruh DoD project (`31-definition-of-done.md`), termasuk semua PFS, baseline load test, dan README |

Roadmap per sprint ada di `30-roadmap.md`, termasuk *cut list* (urutan pemotongan: PF-23 → PF-22 → PF-21 → Jaeger/Loki → PF-19 → tier lembur). RLS, isolation, engine, async, stale detection, dan finalize tidak boleh dipotong.

---

## 12. Risks & Assumptions

| Risiko | Mitigasi |
|---|---|
| Domain terlalu lebar | Scope dikunci pada alur absensi → approval → payroll → slip; cut list |
| Kebocoran tenant lewat jalur yang terlupakan (job, export, cache, file) | RLS sebagai lapisan terakhir + catalogue test + deteksi defensif C-46 |
| Pitfall RLS (pool, owner, `FORCE`, `SECURITY DEFINER`) | Role terpisah, `FORCE`, allowlist, pool leakage test |
| Detail regulasi pajak menghabiskan waktu | Aturan sebagai data *simplified* + label tegas |
| Bug pembulatan | Golden test + property test |
| UI menyedot waktu (tidak ada sprint UI khusus) | Library komponen yang sama dengan LedgerPay; fokus pada halaman run + slip |
| Terlihat "mengulang kerjaan kantor" / IP | Pendekatan tenancy sengaja berbeda; tidak ada kode/skema/nama dari kantor |

**Asumsi:** ≥ 14 jam/minggu; skeleton, infra, dan job runner dipakai ulang dari LedgerPay; satu VM demo bersama; data demo sepenuhnya sintetis.

---

## 13. Open Questions & Future Work

- **V2:** KMS envelope encryption per tenant (crypto-shredding), worker pool per jenis job + limit per tenant, SSO OIDC, break-glass dengan approval + TTL, read replica untuk laporan, retensi/export data per tenant.
- **V3:** cell-based / hybrid pool–silo dengan tenant catalog; warehouse untuk reporting.
- **Optimasi yang menunggu data:** recalculate hanya karyawan yang berubah; verifikasi stale asinkron jika approve dengan 1.000 perubahan > 2 detik (eksperimen E-2).
- **Perlu kajian hukum sebelum diklaim:** konflik retensi data pajak vs hak hapus data pribadi (O17).
- **Stretch lintas project:** disbursement gaji via payout API LedgerPay (PF-33).

---

## 14. Traceability Index

| Topik | Dokumen |
|---|---|
| Vision, persona, workflow, BR | `02-product-strategy.md` |
| Feature matrix & spec | `03-features.md` |
| Arsitektur V1→V3 | `04-architecture.md` |
| ADR | Desain fitur & arsitektur `adr/ADR-P01…P09` (+ shared ADR-001…013) |
| Domain model, multi-tenancy, engine, invariant, failure | `05-domain-model.md`–`08` |
| Skema, RLS, index | `11-db-erd.md`–`07` (+ `01` shared) |
| API & authorization matrix | `16-api-endpoints.md`, `05`, `01` (error catalogue) |
| Threat model | `19-security-threat-model.md` (+ `01`, `04`) |
| Transaksi, concurrency, job | `22-tx-concurrency.md`, `03` |
| Observability, testing, performa | `24-observability.md`–`03` |
| DevOps, biaya | Desain DevOps |
| Repo, roadmap, DoD | `32-repository-structure.md`, `03`, `04` |
| Demo, interview, positioning | `33-demo-script.md`–`05` |
