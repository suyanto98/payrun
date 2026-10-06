# Step 1.2 — PayRun: Workforce & Payroll Platform (Multi-Tenant SaaS)

> *"A multi-tenant workforce and payroll platform where multiple companies can securely manage employees, attendance, approvals, and payroll while keeping tenant data isolated."*

---

## 1. Product Vision

**Payroll yang benar, bisa dijelaskan baris per baris, tidak bisa diubah diam-diam setelah final, dan data setiap perusahaan tidak pernah bisa bocor ke perusahaan lain, bahkan ketika ada bug di kode.**

PayRun adalah SaaS untuk UMKM (±10–300 karyawan). HR mengelola karyawan, absensi, cuti, dan lembur; manager menyetujui request; payroll admin menjalankan payroll bulanan; karyawan melihat slip gajinya sendiri.

Yang **bukan** visinya: HRIS lengkap seperti Talenta/Gadjian. Fokusnya hanya pada alur **data kehadiran → approval → kalkulasi payroll → finalisasi → slip gaji**, dengan dua jaminan: **isolasi tenant** dan **payroll yang reproducible & immutable**.

### Satu kalimat untuk HR
> "Aplikasi penggajian online untuk banyak perusahaan sekaligus. Gaji dihitung otomatis dari absensi dan lembur yang sudah disetujui, tidak bisa diubah sembarangan setelah final, dan data tiap perusahaan terkunci satu sama lain."

### Satu kalimat untuk technical interviewer
> "Modular monolith Go + PostgreSQL dengan pooled multi-tenancy yang ditegakkan Row-Level Security sebagai defense in depth. Payroll engine berbasis komponen dengan data effective-dated, payroll run sebagai state machine yang dieksekusi async secara idempoten dan resumable, serta snapshot input agar hasil final bisa direproduksi."

---

## 2. Target Users & Personas

| Persona | Siapa | Tujuan | Pain point yang relevan |
|---|---|---|---|
| **Dewi** (HR admin / tenant owner) | HR tunggal di perusahaan distribusi, 80 karyawan | Data karyawan rapi, payroll selesai tepat waktu | "Tiap bulan saya rekap absensi dari Excel, hitung lembur manual, lalu ada saja yang salah." |
| **Hendra** (payroll / finance admin) | Finance yang menjalankan dan menyetujui payroll | Hasil bisa dicek, dibandingkan dengan bulan lalu, dan dikunci | "Sudah saya kirim ke bank, ternyata ada absensi yang diubah. Sekarang angka mana yang benar?" |
| **Arif** (manager / approver) | Kepala gudang dengan 15 bawahan | Menyetujui cuti dan lembur dengan cepat | "Request lembur numpuk di WhatsApp, tidak ada jejaknya." |
| **Tono** (employee) | Staf gudang | Clock in/out, ajukan cuti, lihat slip gaji | "Saya tidak tahu potongan gaji saya dari mana." |
| **Platform operator** (internal, kita) | Tim penyedia SaaS | Onboarding tenant, support, menjaga sistem | Butuh akses untuk support **tanpa** melanggar isolasi. Ini titik serangan paling berbahaya |

> Persona *platform operator* penting untuk threat model. Akses lintas tenant hanya boleh lewat jalur *break-glass* yang eksplisit, terbatas waktu, dan ter-audit.

---

## 3. Business Problem

1. **Payroll manual rawan salah dan tidak bisa diaudit.** UMKM menghitung gaji di spreadsheet: rumus rusak, data absensi beda versi, tidak ada jejak siapa mengubah apa. Gaji salah berarti karyawan kecewa, risiko sengketa, dan risiko denda pajak.
2. **Input berubah-ubah sampai detik terakhir.** Absensi dikoreksi, lembur baru disetujui, karyawan baru masuk di tengah bulan. Tanpa konsep *period lock* dan *snapshot*, tidak ada yang tahu angka final dihitung dari data versi mana.
3. **Data gaji adalah data paling sensitif di perusahaan.** Slip gaji yang dikirim lewat email/WhatsApp mudah bocor. Di model SaaS risikonya lebih besar: **satu kebocoran antar tenant bisa mematikan bisnis penyedia SaaS.**
4. **Approval tanpa jejak.** Cuti dan lembur disetujui lewat chat sehingga tidak ada bukti saat terjadi sengketa.
5. **Sisi penyedia SaaS:** satu codebase dan satu deployment melayani banyak perusahaan dengan konfigurasi berbeda (komponen gaji, aturan lembur), tanpa satu tenant memperlambat atau melihat tenant lain.

**Inti problem untuk portfolio:** **"Bagaimana menghitung uang yang menyangkut hidup orang secara benar dan reproducible, dari input yang terus berubah, untuk banyak perusahaan sekaligus, tanpa kemungkinan data saling bocor?"**

---

## 4. Core Workflows

### W1 — Tenant onboarding & setup
```text
Dewi → daftar perusahaan → tenant dibuat + Dewi jadi OWNER
     → buat departemen, posisi, komponen gaji (tunjangan, potongan), aturan lembur
     → undang user (HR, manager, employee) → membership (user ↔ tenant ↔ role)
```
**Tantangan:** tenant context sejak request pertama; satu user bisa menjadi anggota lebih dari satu tenant (contoh: konsultan payroll).

### W2 — Employee & salary (effective-dated)
```text
Dewi → tambah karyawan (data pribadi, rekening bank, NIK/NPWP terenkripsi)
     → set gaji pokok + komponen, berlaku mulai tanggal X
     → kenaikan gaji berlaku 15 Maret → record baru, record lama TIDAK ditimpa
```
**Tantangan:** riwayat perubahan, perhitungan proporsional (prorata) di tengah periode, PII.

### W3 — Attendance, leave, overtime + approval
```text
Tono  → clock in/out (atau HR import CSV absensi)
Tono  → ajukan cuti / lembur → status PENDING
Arif  → approve / reject (hanya untuk bawahannya) → status final, ter-audit
Saldo cuti berkurang hanya ketika approved
```
**Tantangan:** authorization berbasis relasi (manager ↔ bawahan), approval paralel/ganda, saldo cuti di bawah concurrency.

### W4 — Payroll run (inti produk)
```text
Hendra → buka periode Maret → POST /payroll-runs {period} (idempotent: 1 run aktif per tenant+periode)
       → Run[DRAFT] → job async: hitung per karyawan (chunk) → Run[CALCULATED]
       → review: total, perbandingan dengan bulan lalu, warning (gaji 0, lembur > batas, data kurang)
       → hitung ulang kalau ada koreksi input (selama belum di-approve)
       → submit → Run[PENDING_APPROVAL] → approver: approve / reject
       → finalize → Run[FINALIZED]: input di-snapshot, item dikunci, periode ditutup
       → job async: generate slip gaji (PDF) + export file transfer bank (CSV)
```
**Tantangan:** worker crash di tengah batch, run dobel, input berubah saat kalkulasi, immutability setelah final.

### W5 — Payslip & koreksi setelah final
```text
Tono → lihat slip gaji miliknya sendiri (akses tercatat di audit)
Ternyata lembur Tono kurang 2 jam di Maret → TIDAK mengedit run Maret
     → buat adjustment → otomatis masuk ke run April sebagai komponen "koreksi periode Maret"
```
**Tantangan:** finalized = immutable; koreksi tetap bisa ditelusuri.

### Payroll engine (ringkas; spesifikasi di Step 3)
```text
Gross = Gaji pokok (prorata) + Tunjangan tetap + Tunjangan tidak tetap + Lembur + Adjustment(+)
Potongan = Absen tanpa keterangan + Cuti tidak dibayar + BPJS karyawan + PPh21 + Adjustment(−)
Net = Gross − Potongan
```
- Komponen bertipe **data**, bukan kode: `FIXED`, `PERCENT_OF(base)`, `PER_DAY`, `PER_HOUR × multiplier`, dengan batas atas/bawah.
- PPh21 dan BPJS diimplementasikan sebagai **versi disederhanakan yang dapat dikonfigurasi** (tabel tarif/persentase sebagai data ber-versi), dan **ditandai tegas tidak tersertifikasi untuk compliance**.
- Semua perhitungan memakai decimal dengan aturan pembulatan eksplisit per komponen. Pembulatan dilakukan sekali di titik yang ditentukan, bukan di setiap langkah.

---

## 5. Core Business Rules (awal; diformalkan menjadi invariant di Step 3)

| # | Rule |
|---|---|
| BR1 | Setiap data bisnis milik tepat satu tenant. Tidak ada query yang bisa mengembalikan data tenant lain |
| BR2 | Maksimal satu payroll run aktif (non-cancelled) per tenant per periode |
| BR3 | Run `FINALIZED` immutable: item, total, dan snapshot input tidak bisa diubah. Koreksi lewat adjustment di periode berikutnya |
| BR4 | Total run = Σ net semua payroll item; Σ komponen item = net item |
| BR5 | Karyawan hanya masuk payroll jika aktif pada setidaknya satu hari di periode tersebut; prorata untuk joiner/leaver |
| BR6 | Hanya cuti/lembur berstatus `APPROVED` sebelum cut-off yang dihitung |
| BR7 | Approver tidak boleh menyetujui request miliknya sendiri; manager hanya untuk bawahan langsungnya. (Pengecualian payroll run untuk tenant dengan satu admin: lihat ADR-P09) |
| BR8 | Saldo cuti tidak boleh negatif (kecuali jenis cuti yang mengizinkan) |
| BR9 | Data salary effective-dated: perubahan tidak menimpa riwayat; tepat satu record berlaku per tanggal |
| BR10 | Employee hanya bisa melihat slip gajinya sendiri; setiap akses slip gaji oleh HR tercatat di audit |
| BR11 | Kalkulasi deterministik: input snapshot yang sama + versi aturan yang sama menghasilkan output yang sama persis |

---

## 6. MVP Scope

### 6.1 MVP: minimum agar produk benar-benar bisa dipakai
| Fitur | Alasan masuk MVP |
|---|---|
| Signup tenant, login, membership user ↔ tenant, role (OWNER, HR_ADMIN, PAYROLL_ADMIN, MANAGER, EMPLOYEE) | Fondasi SaaS multi-tenant |
| Departemen, posisi, relasi atasan | Diperlukan untuk approval dan laporan |
| Employee CRUD + status (aktif, resign) + rekening bank | Subjek payroll |
| Salary & komponen gaji effective-dated | Input payroll |
| Attendance: clock in/out sederhana + import CSV | Input payroll; import CSV adalah cara nyata UMKM memasukkan absensi |
| Leave & overtime request + approval satu tingkat | Input payroll yang membutuhkan persetujuan |
| Payroll run: calculate → review → approve → finalize | Inti produk |
| Payslip (tampilan web) untuk employee | Nilai yang langsung dirasakan karyawan |
| Audit log untuk aksi sensitif | Wajib untuk data gaji |
| UI minimal: dashboard HR, halaman payroll run, self-service employee | Demo ke HR |

### 6.2 Production Signal: technical depth tinggi
| Fitur | Sinyal yang ditunjukkan |
|---|---|
| **Tenant isolation berlapis**: tenant context di middleware → `SET LOCAL app.tenant_id` per transaksi → Postgres RLS | Defense in depth; paham pitfall connection pool |
| **Tenant isolation test suite** (otomatis untuk setiap endpoint) | Security dibuktikan, bukan diklaim |
| **Payroll run state machine** + unique constraint run aktif | Workflow yang benar di bawah concurrency |
| **Async, idempotent, resumable calculation** (chunk + checkpoint per karyawan) | Batch processing yang tahan crash |
| **Input snapshot saat finalize** + versi aturan | Reproducibility dan audit |
| **Effective dating** untuk salary dan komponen | Temporal data modelling |
| **Golden-file tests** untuk payroll engine (kasus prorata, lembur, resign, pembulatan) | Testing domain yang serius |
| **Period lock**: input di periode yang sedang diproses / sudah final tidak bisa diubah | Konsistensi antara data operasional dan kalkulasi |
| **Field-level encryption** untuk NIK/NPWP/rekening + masking di API | Penanganan PII |
| **Tenant-aware jobs, logs, metrics** (tenant_id di setiap job, log, dan label metrik yang aman) | Observability multi-tenant |

### 6.3 Nice to Have
| Fitur | Kenapa tidak di MVP |
|---|---|
| Payslip PDF async + object storage (MinIO/S3) + signed URL | Demo impact tinggi; kandidat P1 setelah inti stabil |
| Export file transfer bank (CSV) | Murah dibuat, menutup alur bisnis |
| Diff payroll vs bulan lalu + warning anomali | Sangat berguna untuk Hendra; P1 |
| Approval multi-level | Satu tingkat sudah membuktikan pola; multi-level menambah kombinasi tanpa sinyal baru |
| Notifikasi email (request approval, slip siap) | Consumer tambahan dari job queue |
| Shift scheduling | Domain sendiri yang cukup besar |
| Laporan (rekap per departemen, rekap BPJS/PPh) | Read model / eventual consistency; bisa ditambah setelah inti selesai |
| Fairness antar tenant di job queue (noisy neighbor) | Diskusi bagus; implementasi sederhana (limit per tenant) bisa menyusul |
| **Integrasi dengan LedgerPay** (disbursement gaji via payout API) | Cerita portfolio yang kuat (PayRun sebagai *client* yang memakai idempotency key), tetapi menambah coupling. **Stretch goal setelah kedua project selesai** |

### 6.4 Out of Scope (sengaja tidak dibuat)
Lihat §10.

---

## 7. High-Impact Features (urut berdasarkan nilai engineering)

| # | Fitur | Kenapa high-impact | Yang dibuktikan |
|---|---|---|---|
| 1 | **Tenant isolation dengan RLS sebagai defense in depth** | Satu-satunya risiko yang bisa mematikan bisnis SaaS | Security engineering, pemahaman Postgres yang dalam, berpikir "bagaimana kalau kode saya salah" |
| 2 | **Payroll run lifecycle + immutability** | Inti nilai bisnis: angka final yang bisa dipercaya | State machine, invariants, audit |
| 3 | **Async resumable payroll calculation** | Batch yang crash di tengah jalan adalah kejadian production yang nyata | Idempotent job, checkpoint, retry, at-least-once |
| 4 | **Payroll engine berbasis komponen + effective dating** | Domain modelling yang serius, bukan formula hard-coded | Rule engine sederhana, decimal, temporal data |
| 5 | **Snapshot & reproducibility** | Menjawab "angka ini dihitung dari data mana?" | Data versioning, determinisme |
| 6 | **Approval workflow dengan authorization berbasis relasi** | RBAC saja tidak cukup; butuh aturan "atasan langsung" | ABAC/ReBAC sederhana, BOLA prevention |
| 7 | **Isolation test suite** | Mengubah klaim keamanan menjadi bukti otomatis | Security testing |

---

## 8. Killer Demo Features

### Demo A — Tenant isolation: "bahkan ketika kode saya salah"
```text
1. Login sebagai HR PT Alpha → GET /employees/{id milik PT Beta}
   → 404 (bukan 403; tidak membocorkan keberadaan resource)
2. Ganti header / tenant di token secara manual → ditolak (membership divalidasi di server)
3. Jalankan test yang sengaja memakai query TANPA filter tenant_id
   (mensimulasikan bug developer)
   → tetap hanya mengembalikan data PT Alpha, karena RLS
4. Tampilkan isolation test suite: setiap endpoint diuji otomatis lintas tenant → hijau
```
**Bukti:** response API, hasil test, policy RLS di migration, log dengan `tenant_id`.

### Demo B — Payroll crash & resume: "worker mati di tengah chunk ke-4"
```text
1. Tenant demo dengan 100 karyawan → jalankan payroll run
2. Failure injection: worker crash saat chunk ke-4 (karyawan 31–40, chunk size demo 10) belum di-commit (atau docker kill manual)
3. Run tetap CALCULATING; job di-lease ulang setelah timeout
4. Worker hidup lagi → melanjutkan dari checkpoint → tidak ada item dobel
5. Run → CALCULATED, 100 item, total = Σ item

Bonus: klik "Run payroll" dua kali / dua admin bersamaan → tepat satu run (unique constraint + idempotency)
```
**Bukti:** progress bar run, query `count(*)` item = 100, log job dengan attempt ke-2, metrik durasi run.

### Demo C — Immutability & koreksi: "angka final tidak bisa diubah diam-diam"
```text
1. Finalize run Maret → snapshot tersimpan
2. Coba ubah absensi Maret → ditolak (PERIOD_LOCKED)
3. Coba ubah item payroll Maret lewat API → ditolak (RUN_FINALIZED)
4. Buat adjustment "lembur Maret kurang 2 jam" → muncul di run April sebagai baris koreksi
5. Hitung ulang dari snapshot Maret → hasil identik byte-per-byte (reproducible)
```
**Bukti:** error response, slip gaji April dengan baris koreksi yang mereferensikan Maret, audit trail.

---

## 9. Production Engineering Challenges

| # | Challenge | Kenapa sulit | Arah solusi (detail di Step 3/6/7) |
|---|---|---|---|
| C1 | **Propagasi tenant context** ke semua tempat | HTTP, background job, cache key, log, file storage path; satu tempat lupa berarti bocor | Tenant context sebagai nilai wajib di setiap boundary; job payload membawa tenant_id; helper `WithTenantTx` satu-satunya cara akses DB |
| C2 | **RLS + connection pool** | `SET` di level session bocor ke request berikutnya yang memakai koneksi yang sama | `SET LOCAL` di dalam transaksi; role aplikasi tanpa `BYPASSRLS`; test khusus untuk kebocoran via pool |
| C3 | **Effective dating** | Query "gaji yang berlaku tanggal X", tumpang tindih periode, prorata | Exclusion constraint (`tstzrange`/`daterange` + `EXCLUDE USING gist`) agar tidak ada periode tumpang tindih |
| C4 | **Decimal & pembulatan** | Σ dari nilai yang dibulatkan ≠ nilai Σ yang dibulatkan | `NUMERIC` di DB, library decimal di Go, kebijakan pembulatan per komponen, test golden |
| C5 | **Batch idempotent & resumable** | Crash di tengah, retry, at-least-once delivery dari queue | Unique (run_id, employee_id) pada item; upsert; checkpoint; job lease + heartbeat |
| C6 | **Input berubah saat kalkulasi** | Absensi diedit ketika run sedang dihitung | **Diputuskan di ADR-P07:** input fingerprint + *stale detection* (approve ditolak jika input berubah), lock keras hanya saat finalize, plus snapshot saat finalize |
| C7 | **Concurrent run / approval** | Dua admin menekan tombol bersamaan; approve dan reject bersamaan | Partial unique index untuk run aktif; transisi state dengan `UPDATE … WHERE status = expected` (optimistic) |
| C8 | **Authorization berbasis relasi** | Manager hanya untuk bawahan; employee hanya data sendiri; HR untuk semua di tenant | Policy di service layer + scope query; diuji di isolation/authorization suite |
| C9 | **PII & data gaji** | Bocor lewat log, export, error message | Field-level encryption, masking, redaction di logger, audit akses |
| C10 | **Migrasi skema multi-tenant** | Pooled = satu migrasi untuk semua tenant; schema-per-tenant = N migrasi | Pooled dipilih. Trade-off ini dibandingkan secara eksplisit di ADR (lihat `03-cross-project.md` §4) |
| C11 | **Noisy neighbor** | Tenant 5.000 karyawan memblokir antrean tenant 20 karyawan | Chunking + batas konkurensi per tenant di job queue (P2) |

---

## 10. Out-of-Scope Features

| Fitur | Alasan dikeluarkan |
|---|---|
| Kepatuhan pajak/BPJS yang tersertifikasi, pelaporan e-SPT, integrasi DJP/BPJS | Regulasi berubah dan butuh keahlian khusus. Engine hanya mendukung aturan sebagai data yang dapat dikonfigurasi, dan ditandai *simplified* |
| Disbursement gaji ke bank sungguhan | Cukup export file transfer bank; integrasi dengan LedgerPay hanya stretch goal |
| Formula DSL yang bisa ditulis user | Parser, sandbox, dan keamanan evaluasi terlalu besar. Tipe komponen ter-parameter sudah menutup >90% kebutuhan UMKM |
| Face recognition, GPS/geofencing absensi, integrasi mesin fingerprint | Hardware/ML, bukan inti engineering backend. Import CSV mewakili integrasi mesin absensi |
| Recruitment, performance review, training, KPI | Modul HRIS lain, tidak menyentuh payroll |
| Multi-country / multi-currency | Kompleksitas regulasi tanpa sinyal tambahan |
| SSO/SAML/SCIM | Relevan untuk enterprise, tetapi implementasinya besar. Disebut sebagai V3 di Step 2 |
| Database/schema per tenant | Sengaja dipilih pooled + RLS (lihat ADR Step 2); schema-per-tenant dibahas sebagai alternatif |
| Microservices (employee service, payroll service, …) | Payroll butuh konsistensi lintas entitas dalam satu transaksi; tidak ada alasan scaling independen |
| Mobile app native | Web responsif cukup untuk clock in/out demo |
| Billing SaaS (langganan, invoice ke tenant) | Domain lain; bisa disebut sebagai future work |

---

## 11. Initial Architecture Direction

> Ini **arah**, belum keputusan final. ADR lengkap dan versi V1→V3 dikerjakan di Step 2.

```text
                  ┌────────────────────────────────────────────────┐
  Browser (Vue) ─▶│  payrun api  (Go, modular monolith)            │
                  │  middleware: auth → tenant resolve → membership │
                  │  modules: tenancy, iam, org, employee, salary,  │
                  │  attendance, leave, overtime, approval, payroll,│
                  │  payslip, audit                                 │
                  └───────────────┬────────────────────────────────┘
                                  │ WithTenantTx: BEGIN; SET LOCAL app.tenant_id; …
                           ┌──────▼──────────────────────────┐
                           │ PostgreSQL (pooled, tenant_id + │◀── payrun worker (binary yang sama)
                           │ Row-Level Security)             │     - payroll calculation (chunked)
                           └─────────────────────────────────┘     - payslip generation (P1)
                                                                   - scheduled: leave accrual, reminders
                                          ┌──────────────────┐
                                          │ Object storage   │◀── payslip PDF, CSV import/export (P1)
                                          │ (MinIO lokal/S3) │
                                          └──────────────────┘
```

| Aspek | Arah awal | Alasan singkat |
|---|---|---|
| Gaya arsitektur | **Modular monolith**, binary `api` / `worker` / `migrate` | Konsistensi lintas entitas; skeleton yang sama dengan LedgerPay |
| Multi-tenancy | **Pooled tables + `tenant_id` + Postgres RLS** | Satu migrasi untuk semua tenant, operasional sederhana, isolasi ditegakkan DB. Sengaja **berbeda** dari pendekatan schema-per-company di pekerjaan kantor, sehingga ada perbandingan trade-off dari pengalaman nyata |
| Database | **PostgreSQL** | RLS, exclusion constraint, `NUMERIC`, `SKIP LOCKED` |
| Job queue | **Postgres-backed queue** (hand-rolled dari skeleton LedgerPay *atau* River; diputuskan di ADR Step 2) | Transaksional dengan perubahan state run; tanpa broker tambahan |
| Object storage | MinIO (lokal) / S3-compatible (deploy); hanya saat payslip PDF masuk | Jangan simpan blob di DB; signed URL untuk akses |
| Cache | **Tidak ada di V1** | Belum ada bottleneck terbukti. Kalau ditambah, wajib tenant-aware key |
| Auth | JWT access + refresh rotasi; token memuat `user_id`, tenant aktif divalidasi terhadap membership di DB | User multi-tenant; mencegah token tampering |
| Deployment | Docker Compose, satu VM | Sama dengan LedgerPay |

---

## 12. Key Technical Risks

| # | Risiko | Dampak | Kemungkinan | Mitigasi |
|---|---|---|---|---|
| R1 | **Domain terlalu lebar**: HRIS bisa tak terbatas | Tidak selesai | **Tinggi** | MVP dikunci pada alur absensi → approval → payroll → slip; modul lain masuk Out of Scope |
| R2 | **Kebocoran tenant** lewat jalur yang terlupakan (job, export, cache, file path, error message) | Fatal untuk kredibilitas | Sedang | RLS sebagai lapisan terakhir + isolation test otomatis untuk setiap endpoint dan job |
| R3 | **Pitfall RLS** (session `SET` bocor via pool, owner tabel bypass RLS, `FORCE ROW LEVEL SECURITY` lupa) | Isolasi palsu, merasa aman padahal tidak | Sedang | Role aplikasi terpisah dari owner; `FORCE RLS`; test khusus pool leakage |
| R4 | **Kerumitan regulasi pajak/BPJS** menghabiskan waktu | Terjebak di detail compliance | Tinggi | Aturan sebagai data yang disederhanakan; label tegas "not compliance-certified" |
| R5 | **Bug pembulatan / decimal** | Angka berselisih Rp1 dan merusak invariant total | Sedang | Golden tests + property test "Σ item = total" |
| R6 | **Terlihat seperti "mengulang kerjaan kantor"** (ERP) atau risiko IP | Nilai portfolio turun / masalah etika | Sedang | Pendekatan tenancy sengaja berbeda; tidak ada kode, skema, atau penamaan dari kantor |
| R7 | **UI menyedot waktu** (banyak form CRUD) | Backend inti terlambat | Tinggi | UI minimal dengan komponen library; prioritaskan halaman payroll run dan self-service |
| R8 | **Performa batch** buruk untuk tenant besar (N+1 saat kalkulasi) | Demo 100 karyawan lambat | Sedang | Prefetch input per chunk; load test dengan 10k karyawan sintetis |

---

## 13. Success Metrics

### Product (seandainya produk nyata)
- Waktu dari "periode dibuka" sampai "payroll final" per tenant.
- Persentase run yang harus di-reject / dihitung ulang.
- Jumlah adjustment pasca-final per 100 karyawan (indikator kualitas input).

### Engineering (dapat diverifikasi di portfolio)
| Metrik | Target |
|---|---|
| Endpoint yang tercakup tenant isolation test | **100%** endpoint tenant-scoped |
| Pelanggaran invariant payroll (run dobel, item dobel, Σ item ≠ total, perubahan setelah final) di test | **0** |
| Golden test payroll engine | Mencakup prorata, joiner/leaver, lembur, cuti tidak dibayar, adjustment, pembulatan |
| Crash di tengah run → hasil akhir identik dengan run tanpa crash | Selalu |
| Durasi kalkulasi 1.000 / 10.000 karyawan sintetis di load test lokal | Dicatat sebagai baseline. **Bukan klaim kapasitas production** |
| Demo A–C bisa dijalankan dari nol (`make demo`) | ≤ 10 menit, deterministik |
