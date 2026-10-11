# PayRun: Feature Matrix & Feature Specification

> Referensi: [strategi produk PayRun](02-product-strategy.md) (workflow W1–W5, business rules BR1–BR11, challenges C1–C11).
> Skala dan arti prioritas: lihat `00-index.md`.

---

## 1. Feature Matrix

| ID | Feature | Business Value | Technical Complexity | Production Relevance | Demo Impact | Priority |
|---|---|---:|---:|---:|---:|---|
| PF-01 | Tenant signup & onboarding | 4 | 2 | 4 | 2 | **P0** |
| PF-02 | IAM: user, membership, role, switch tenant | 5 | 3 | 5 | 2 | **P0** |
| PF-03 | Tenant isolation (context + RLS) | 5 | 4 | 5 | 5 | **P0** |
| PF-04 | Isolation & authorization test suite | 3 | 3 | 5 | 5 | **P0** |
| PF-05 | Org: departemen, posisi, atasan langsung | 4 | 2 | 3 | 2 | **P0** |
| PF-06 | Employee management + lifecycle (aktif, resign) | 5 | 2 | 4 | 3 | **P0** |
| PF-07 | Salary & komponen gaji (effective-dated) | 5 | 4 | 5 | 3 | **P0** |
| PF-08 | Attendance: clock in/out | 4 | 2 | 3 | 3 | **P0** |
| PF-09 | Leave: jenis, saldo, request | 4 | 3 | 4 | 3 | **P0** |
| PF-10 | Overtime request | 4 | 2 | 4 | 2 | **P0** |
| PF-11 | Approval satu tingkat (generik) | 5 | 3 | 4 | 3 | **P0** |
| PF-12 | Payroll engine (pure domain) | 5 | 4 | 5 | 4 | **P0** |
| PF-13 | Payroll run lifecycle + async calc + resume | 5 | 5 | 5 | 5 | **P0** |
| PF-14 | Finalize + snapshot + period lock | 5 | 4 | 5 | 5 | **P0** |
| PF-15 | Payslip (web) untuk employee | 5 | 2 | 3 | 4 | **P0** |
| PF-16 | Audit log (incl. akses slip gaji) | 4 | 2 | 5 | 3 | **P0** |
| PF-17 | Health, structured log (dengan tenant_id) | 3 | 2 | 5 | 2 | **P0** |
| PF-18 | UI: dashboard HR, payroll run, self-service employee | 4 | 3 | 2 | 4 | **P0** |
| PF-19 | Adjustment pasca-final (masuk periode berikutnya) | 4 | 3 | 4 | 4 | **P1** |
| PF-20 | PII encryption + masking | 4 | 3 | 5 | 3 | **P1** |
| PF-21 | Attendance CSV import (async, laporan validasi) | 4 | 3 | 4 | 3 | **P2** (diturunkan di final review S10) |
| PF-22 | Payslip PDF async + object storage | 4 | 3 | 3 | 4 | **P1** |
| PF-23 | Diff payroll vs bulan lalu + warning anomali | 4 | 2 | 4 | 4 | **P2** (diturunkan di final review S9) |
| PF-24 | Tenant-aware metrics & dashboard | 3 | 3 | 4 | 3 | **P1** |
| PF-25 | Export file transfer bank (CSV) | 3 | 1 | 3 | 2 | **P2** |
| PF-26 | Leave accrual terjadwal | 3 | 2 | 3 | 1 | **P2** |
| PF-27 | Fairness job per tenant (noisy neighbor) | 2 | 3 | 4 | 2 | **P2** |
| PF-28 | Break-glass support access (platform operator) | 3 | 3 | 4 | 3 | **P2** |
| PF-29 | Rate limiting | 3 | 2 | 4 | 1 | **P1** (otomatis dari skeleton LedgerPay LF-20 + tier `heavy` per tenant) |
| PF-30 | Notifikasi email | 2 | 2 | 2 | 1 | **P3** |
| PF-31 | Laporan (rekap departemen, BPJS, PPh) | 3 | 2 | 2 | 2 | **P3** |
| PF-32 | Approval multi-level | 2 | 3 | 3 | 1 | **P3** |
| PF-33 | Disbursement via LedgerPay payout API | 3 | 4 | 4 | 5 | **P3** (stretch, lintas project) |

### Ringkasan per prioritas
| Prioritas | Jumlah | Tujuan |
|---|---|---|
| P0 | 18 | MVP yang bisa dipakai + Demo A, B, C (sebagian) |
| P1 | 5 | "Selesai": koreksi pasca-final, PII, import, PDF, review berkualitas |
| P2 | 6 | Hardening SaaS (+ PF-21, PF-23 sejak final review) |
| P3 | 4 | Diskusi interview / stretch |

> P0 PayRun lebih banyak daripada LedgerPay. Ini konsekuensi dari domain yang *lebar*. Mitigasinya: banyak fitur P0 PayRun adalah CRUD sederhana (PF-05, PF-06, PF-08, PF-10), sementara energi dikonsentrasikan pada PF-03, PF-07, PF-12, PF-13, dan PF-14.

### Fitur yang dipertimbangkan lalu ditolak
| Fitur | Kenapa ditolak |
|---|---|
| Shift scheduling / roster | Domain sendiri (constraint solving); tidak memperdalam payroll |
| Self-service signup employee | Employee selalu diundang HR (realistis untuk B2B) |
| Formula builder di UI | Lihat ADR-P03 (tanpa DSL) |
| Kalender libur nasional otomatis (API eksternal) | Cukup tabel libur per tenant yang diisi manual/seed |
| Chat/komentar di approval | Tidak ada sinyal engineering baru |

---

## 2. Feature Specification (P0 & P1)

Format: **Tujuan · User story · Acceptance criteria · Business rules · Edge cases · Dependency · Bukti**.

---

### PF-01 — Tenant Signup & Onboarding (P0)
**Persona:** Dewi.

**User story:** Sebagai pemilik/HR perusahaan, saya ingin mendaftarkan perusahaan saya dan langsung bisa mulai mengatur data.

**Acceptance criteria**
1. Signup membuat tenant, user (jika belum ada), dan membership `OWNER` dalam satu transaksi.
2. Tenant baru di-seed dengan konfigurasi default: jenis cuti standar, komponen gaji umum, aturan lembur default, ruleset pajak/BPJS versi *simplified*. Semua bisa diubah.
3. Slug tenant unik (untuk tampilan, bukan untuk isolasi).
4. Satu email bisa menjadi anggota beberapa tenant.

**Edge cases:** signup ganda (double-click) → satu tenant (idempotency key pada signup).
**Dependency:** skeleton.

---

### PF-02 — IAM: Membership, Role, Switch Tenant (P0)
**User story:** Sebagai user yang menjadi anggota lebih dari satu perusahaan, saya ingin memilih perusahaan yang sedang saya kelola, dan hanya melihat data perusahaan itu.

**Acceptance criteria**
1. Role per membership: `OWNER`, `HR_ADMIN`, `PAYROLL_ADMIN`, `MANAGER`, `EMPLOYEE`. Satu membership bisa punya lebih dari satu role (misalnya `MANAGER` + `EMPLOYEE`).
2. Undang user lewat email (token undangan sekali pakai, berlaku 7 hari). Di MVP, link undangan ditampilkan di UI/log, tanpa email.
3. `POST /v1/session/switch-tenant` memeriksa membership aktif → menerbitkan access token dengan claim `tid` + roles (ADR-P02).
4. Membership dicabut → user kehilangan akses paling lambat saat refresh berikutnya (≤ 15 menit). Operasi sensitif (finalize payroll, lihat slip gaji orang lain, ubah gaji) memeriksa ulang membership di DB.
5. Tenant harus selalu punya minimal satu `OWNER` (tidak bisa menghapus owner terakhir).
6. **(final review R2-8)** Email diverifikasi lewat link sekali pakai sebelum user bisa menerima undangan atau mengaktifkan tenant hasil signup. Di MVP tanpa SMTP, email ditangkap Mailpit (dev/demo).

**Dependency:** PF-01.
**Bukti:** test token dengan `tid` tenant lain hasil manipulasi → ditolak (signature); test membership dicabut.

---

### PF-03 — Tenant Isolation (P0)
**Tujuan:** BR1, yaitu tidak ada jalur untuk membaca atau menulis data tenant lain.

**Acceptance criteria**
1. Semua tabel bisnis punya `tenant_id NOT NULL` + policy RLS (`USING` dan `WITH CHECK`) + `FORCE ROW LEVEL SECURITY`.
2. Aplikasi terhubung dengan role yang **bukan owner tabel** dan tidak punya `BYPASSRLS`. Migrasi memakai role owner terpisah.
3. Semua akses DB lewat `WithTenantTx(ctx, tenantID, fn)`, yang menjalankan `set_config('app.tenant_id', …, true)` (transaction-local).
4. Tanpa tenant context → query mengembalikan 0 baris (*fail closed*), bukan error yang membocorkan data.
5. FK antar tabel bisnis menyertakan `tenant_id` (composite FK), sehingga referensi lintas tenant mustahil di level DB.
6. Job background membawa `tenant_id` dan memakai `WithTenantTx`. Job tanpa tenant (misalnya scheduler global) hanya boleh mengiterasi daftar tenant, lalu bekerja per tenant.
7. Object key storage, cache key (jika ada), dan label log/metrik menyertakan `tenant_id`.

**Edge cases:** koneksi pool dipakai ulang setelah transaksi tenant A → tenant B tidak mewarisi setting (dibuktikan dengan test); ID resource ditebak (UUIDv7) → tetap 404.
**Dependency:** skeleton (`WithTenantTx`).
**Bukti:** **Demo A**.

---

### PF-04 — Isolation & Authorization Test Suite (P0)
**Tujuan:** mengubah klaim keamanan menjadi bukti otomatis. Fitur ini engineering, tetapi diperlakukan sebagai fitur produk.

**Acceptance criteria**
1. Fixture standar: Tenant Alpha dan Beta, masing-masing dengan user semua role.
2. **Cross-tenant test** untuk setiap endpoint tenant-scoped: user Alpha mengakses ID milik Beta → 404, dan list tidak pernah mengandung data Beta.
3. **Authorization matrix test**: tabel (endpoint × role → diizinkan/ditolak) dijalankan otomatis. Endpoint baru tanpa entri di matrix membuat test gagal.
4. **RLS bypass test**: query mentah tanpa filter `tenant_id`, dijalankan dengan role aplikasi di dalam `WithTenantTx(Alpha)`, hanya mengembalikan data Alpha.
5. **Pool leakage test**: transaksi Alpha diikuti transaksi tanpa tenant di koneksi yang sama → 0 baris.
6. Berjalan di CI pada setiap PR.

**Dependency:** PF-02, PF-03.
**Bukti:** **Demo A** (output test di layar).

---

### PF-05 — Org: Departemen, Posisi, Atasan (P0)
**Acceptance criteria**
1. CRUD departemen dan posisi (soft-deactivate, bukan hard delete jika masih dipakai).
2. Setiap employee punya atasan langsung (opsional; `NULL` untuk pimpinan tertinggi). Siklus atasan (A atasan B, B atasan A) ditolak.
3. Penugasan departemen/posisi/atasan effective-dated (ADR-P04), agar approver dan laporan historis tetap benar.

**Dependency:** PF-03.

---

### PF-06 — Employee Management (P0)
**Persona:** Dewi.

**Acceptance criteria**
1. Data: nomor karyawan (unik per tenant), nama, email (opsional, untuk menautkan ke user), tanggal masuk, tanggal keluar, status (`ACTIVE`, `RESIGNED`, `TERMINATED`), PTKP status (untuk pajak simplified), rekening bank, NIK, NPWP.
2. Menautkan employee ke user (membership `EMPLOYEE`) agar karyawan bisa self-service.
3. Resign: tanggal keluar diisi. Employee tetap masuk payroll periode terakhirnya secara prorata, lalu tidak lagi.
4. Riwayat perubahan data penting tercatat di audit (PF-16).
5. List dengan filter (departemen, status) + cursor pagination + pencarian nama.

**Edge cases:** nomor karyawan sama di tenant berbeda → boleh; NIK sama di tenant yang sama → ditolak (blind index, PF-20).
**Dependency:** PF-03, PF-05.

---

### PF-07 — Salary & Komponen Gaji (Effective-Dated) (P0)
**Persona:** Dewi, Hendra.

**User story:** Sebagai HR, saya ingin mencatat kenaikan gaji yang berlaku mulai tanggal tertentu tanpa kehilangan riwayat, agar payroll periode mana pun dihitung dengan nilai yang benar.

**Acceptance criteria**
1. Definisi komponen per tenant: nama, kategori (`EARNING`/`DEDUCTION`), tipe kalkulasi (`FIXED`, `PERCENT_OF`, `PER_DAY`, `PER_HOUR_MULTIPLIER`), parameter, cap, taxable (ya/tidak), aturan pembulatan (ADR-P03, ADR-P06).
2. Assignment per employee: gaji pokok + komponen, dengan `valid_from`/`valid_to` (half-open). Periode yang tumpang tindih ditolak oleh DB (exclusion constraint, ADR-P04).
3. Mengubah gaji = menutup record lama di `valid_from` baru, lalu membuat record baru. Riwayat tidak pernah ditimpa.
4. Query "kondisi gaji per tanggal X" tersedia untuk engine dan UI.
5. Perubahan yang berlaku surut ke periode yang sudah `FINALIZED` → ditolak. Koreksi dilakukan lewat adjustment (PF-19).

**Edge cases:** dua HR mengubah gaji karyawan yang sama bersamaan → satu berhasil, satu ditolak (exclusion constraint / optimistic version); kenaikan di tengah periode → prorata dua segmen.
**Dependency:** PF-06.
**Bukti:** test exclusion constraint; golden test prorata dua segmen.

---

### PF-08 — Attendance: Clock In/Out (P0)
**Persona:** Tono.

**Acceptance criteria**
1. Employee clock in/out (timestamp server, timezone tenant). Satu catatan harian per employee.
2. Clock in dua kali → idempoten (mengembalikan catatan yang ada). Clock out tanpa clock in → 422.
3. HR bisa mengoreksi catatan (dengan alasan, ter-audit) selama periodenya belum `FINALIZED`.
4. Status harian diturunkan: `PRESENT`, `ABSENT`, `LEAVE`, `HOLIDAY`, `WEEKEND`, berdasarkan kalender kerja tenant + cuti yang approved.

**Edge cases:** shift melewati tengah malam; lupa clock out → ditandai `INCOMPLETE` untuk ditinjau HR.
**Dependency:** PF-06.

---

### PF-09 — Leave (P0)
**Acceptance criteria**
1. Jenis cuti per tenant: nama, dibayar/tidak, kuota tahunan, boleh minus atau tidak.
2. Request cuti (rentang tanggal) → `PENDING` → approval (PF-11).
3. Saldo cuti berkurang **di transaksi yang sama** dengan approval. Saldo tidak cukup saat approve → approval gagal (bukan saldo negatif).
4. Request yang tumpang tindih dengan cuti `PENDING`/`APPROVED` milik employee yang sama → ditolak.
5. Pembatalan: oleh requester saat `PENDING`; setelah `APPROVED` hanya oleh HR, dan saldo dikembalikan.
6. Cuti tidak dibayar yang approved menjadi input potongan payroll.

**Edge cases:** dua request yang bersama-sama menghabiskan sisa saldo di-approve paralel → hanya yang muat yang lolos (row lock pada saldo).
**Dependency:** PF-06, PF-11.

---

### PF-10 — Overtime Request (P0)
**Acceptance criteria**
1. Request lembur: tanggal, jam mulai/selesai, alasan → `PENDING` → approval.
2. Hanya lembur `APPROVED` dengan tanggal di dalam periode dan disetujui sebelum *cut-off* run yang dihitung (BR6).
3. Batas lembur per hari/minggu dari konfigurasi tenant (warning, bukan blokir, kecuali dikonfigurasi keras).
4. Multiplier lembur dari aturan tenant (versi *simplified*, contoh: jam pertama 1,5×, berikutnya 2×).

**Dependency:** PF-06, PF-11.

---

### PF-11 — Approval Satu Tingkat (P0)
**Persona:** Arif.

**User story:** Sebagai manager, saya ingin melihat dan memproses request bawahan langsung saya di satu tempat, dengan jejak yang jelas.

**Acceptance criteria**
1. Request (cuti, lembur) membuat `approval_request` dengan approver yang **di-snapshot saat submit** (atasan langsung pada tanggal itu; fallback `HR_ADMIN` jika tidak ada atasan) (ADR-P09).
2. Hanya approver yang ditunjuk (atau `HR_ADMIN` sebagai override, ter-audit) yang bisa approve/reject.
3. Self-approval ditolak, termasuk ketika user memegang role `HR_ADMIN`.
4. Transisi status memakai guard `WHERE status = 'PENDING'`. Approve dan reject yang bersamaan → tepat satu yang menang.
5. Efek approval (misalnya pengurangan saldo cuti) terjadi di transaksi yang sama dengan transisi.
6. Inbox approval untuk manager (list + filter).

**Dependency:** PF-05.
**Bukti:** test race approve/reject; authorization matrix (manager bukan atasan → 404).

---

### PF-12 — Payroll Engine (P0)
**Tujuan:** perhitungan yang benar, deterministik, dan bisa dijelaskan baris per baris.

**Acceptance criteria**
1. Pure function: `Calculate(EmployeePeriodInput, Ruleset) → PayrollResult`, tanpa akses DB, waktu, atau random.
2. Urutan pipeline tetap (ADR-P03): prorata → earnings → overtime → potongan absensi/cuti tidak dibayar → BPJS → pajak → adjustment → net.
3. Hasil berisi baris per komponen dengan *explanation* (dasar perhitungan, contoh: "Lembur 6 jam × 1,5 × Rp 28.902"), dan baris inilah yang tampil di slip gaji.
4. Pembulatan per komponen. `net = Σ baris`, eksak (ADR-P06).
5. Ruleset pajak/BPJS ber-versi dan effective-dated; hasil mencatat versi ruleset yang dipakai.
6. Warning (bukan error) untuk kondisi mencurigakan: net ≤ 0, gaji pokok 0, lembur melebihi batas.
7. Error validasi (konfigurasi tidak valid, contoh: `PERCENT_OF` mereferensikan komponen yang tidak ada) → item ditandai `ERROR` dengan pesan, tidak menggagalkan seluruh run.

**Business rules:** BR4, BR5, BR11.
**Dependency:** tidak ada (dibangun paling awal, lihat strategi produk §5.2).
**Bukti:** golden-file test (≥ 20 kasus: joiner, leaver, kenaikan tengah periode, lembur, cuti tidak dibayar, absen, cap BPJS, PTKP berbeda, adjustment ±, pembulatan); property test `net == Σ lines`.

---

### PF-13 — Payroll Run Lifecycle + Async Calc + Resume (P0)
**Persona:** Hendra.

**User story:** Sebagai payroll admin, saya ingin menjalankan payroll untuk seluruh karyawan, melihat progresnya, dan yakin hasilnya benar walaupun server bermasalah di tengah proses.

**Acceptance criteria**
1. `POST /payroll-runs {period}` (idempoten) → run `DRAFT`. Maksimal satu run aktif per (tenant, periode) (partial unique index; ADR-P05).
2. `POST /payroll-runs/{id}/calculate` → `CALCULATING` → job `payroll.plan` memecah karyawan aktif menjadi chunk job.
3. Progres terlihat (chunk selesai / total) lewat polling.
4. Crash worker → chunk yang belum selesai diambil ulang. Item di-upsert (unique per run + employee + calc_version), sehingga tidak ada duplikat.
5. Semua chunk selesai → `CALCULATED`, total run dihitung dari item (BR4).
6. Recalculate saat `CALCULATED` → `calc_version` bertambah, item versi lama diabaikan/dihapus, lalu kalkulasi ulang.
7. Submit → `PENDING_APPROVAL`. Approver (`OWNER`/`PAYROLL_ADMIN`, bukan pembuat run) → approve (`APPROVED`) / reject (kembali ke `CALCULATED` dengan catatan). **Pengecualian:** tenant yang hanya punya satu admin boleh *self-approve* dengan alasan wajib + audit (ADR-P09). Tanpa pengecualian ini, UMKM kecil tidak bisa menggaji karyawannya.
8. Approve ditolak jika input berubah sejak kalkulasi terakhir (*stale fingerprint*, ADR-P07).
9. Cancel diizinkan sebelum `FINALIZED`.

**Edge cases:** dua admin menekan "Run" bersamaan → satu run; tenant dengan 0 karyawan aktif → run `CALCULATED` dengan total 0 + warning; item `ERROR` → run tidak bisa di-submit sampai diperbaiki/dikecualikan secara eksplisit.
**Dependency:** PF-07, PF-12, job runner shared.
**Bukti:** **Demo B**.

---

### PF-14 — Finalize + Snapshot + Period Lock (P0)
**User story:** Sebagai finance, saya ingin payroll yang sudah final tidak bisa berubah, dan bisa dibuktikan dihitung dari data apa.

**Acceptance criteria**
1. Snapshot input JSONB per item + versi ruleset + fingerprint **ditulis saat kalkulasi** (di transaksi chunk, ADR-P07), sehingga snapshot persis sama dengan yang dihitung. Finalize (dari `APPROVED`) dalam satu transaksi **membekukan** snapshot dan item tersebut: run `FINALIZED`, periode `FINALIZED` (terkunci; status periode: `OPEN` → `PROCESSING` → `FINALIZED`, ADR-P07).
2. Setelah `FINALIZED`: item, total, dan snapshot tidak bisa diubah (trigger + tidak ada endpoint ubah).
3. Periode `FINALIZED`: perubahan attendance/leave/overtime/salary yang mempengaruhi periode itu → 409 `PERIOD_LOCKED`.
4. **Reproducibility:** `Calculate(snapshot, ruleset_version)` menghasilkan item yang identik dengan yang tersimpan (diuji otomatis).
5. Finalize memicu job: generate payslip (PF-15/PF-22), lalu export bank (PF-25, P2).

**Business rules:** BR3, BR11.
**Bukti:** **Demo C**.

---

### PF-15 — Payslip Web (P0)
**Persona:** Tono.

**Acceptance criteria**
1. Employee melihat daftar dan detail slip gajinya sendiri untuk run `FINALIZED`. Baris komponen berikut *explanation*-nya.
2. Employee lain atau manager → 404. HR/Payroll admin bisa melihat, dan **setiap akses tercatat** di audit (BR10).
3. Nilai rekening ditampilkan ter-mask.

**Dependency:** PF-14.

---

### PF-16 — Audit Log (P0)
**Acceptance criteria**
1. Dicatat: perubahan gaji/komponen, data employee sensitif, approval, transisi payroll run, finalize, akses slip gaji oleh non-pemilik, perubahan membership/role, switch tenant, aksi break-glass (P2).
2. Field: tenant_id, actor, action, target, diff ringkas (tanpa nilai PII mentah), request_id, timestamp.
3. Append-only, dan juga di bawah RLS (HR tenant hanya melihat audit tenant-nya).
4. Ditulis di transaksi yang sama dengan perubahan.

---

### PF-17 — Health & Structured Log (P0)
Sama seperti LedgerPay LF-13, ditambah `tenant_id` di setiap log request/job dan redaction PII (NIK, NPWP, rekening, gaji di pesan log).

---

### PF-18 — UI Minimal (P0)
**Halaman:** signup tenant · login + pilih tenant · dashboard HR (ringkasan) · employee list/detail (tab gaji dengan riwayat effective-dated) · approval inbox · payroll run (list, progres, review item, warning, approve, finalize) · self-service employee (clock in/out, request cuti/lembur, slip gaji) · demo toolbar (crash worker, mode demo saja).

**Fokus energi UI:** halaman payroll run dan slip gaji. Sisanya cukup memakai komponen tabel/form dari library.

---

### PF-19 — Adjustment Pasca-Final (P1)
**Acceptance criteria**
1. HR membuat adjustment untuk employee: jumlah (±), alasan, periode asal (referensi), periode target (run berikutnya yang belum final).
2. Adjustment masuk ke kalkulasi run target sebagai baris komponen "Koreksi periode {asal}".
3. Adjustment yang sudah dipakai di run `FINALIZED` immutable. Adjustment yang belum dipakai bisa dibatalkan (ter-audit).
4. Adjustment yang dibuat setelah run target dikalkulasi membuat fingerprint run tersebut *stale* (ADR-P07).

---

### PF-20 — PII Encryption + Masking (P1)
**Acceptance criteria**
1. NIK, NPWP, dan nomor rekening dienkripsi di level aplikasi (AES-256-GCM, AAD = tenant_id + nama field) (ADR-P08).
2. Blind index HMAC untuk uniqueness/pencarian NIK.
3. API mengembalikan nilai ter-mask secara default. Nilai penuh hanya untuk role berwenang lewat endpoint khusus yang ter-audit.
4. Rotasi kunci: ciphertext membawa versi kunci. Re-encrypt dilakukan lazy/batch.

---

### PF-21 — Attendance CSV Import (P1)
**Acceptance criteria**
1. HR mengunggah CSV (format template) → file disimpan (object storage) → job async memvalidasi per baris.
2. Laporan hasil: baris sukses, baris gagal + alasan. Import parsial diizinkan (baris valid tetap masuk), atau mode all-or-nothing (dipilih HR).
3. Import ulang file yang sama (hash sama) → diperingatkan; upsert per (employee, tanggal) agar idempoten.
4. Baris untuk periode `FINALIZED` → gagal dengan alasan `PERIOD_LOCKED`.
5. Batas ukuran file dan validasi tipe (lihat threat model).

---

### PF-22 — Payslip PDF + Object Storage (P1)
**Acceptance criteria**
1. Setelah finalize, job membuat PDF per employee (chunked, idempoten: key object deterministik).
2. Disimpan di bucket private dengan key `tenants/{tenant_id}/payslips/{run_id}/{employee_id}.pdf`.
3. Unduh lewat presigned URL (TTL 5 menit), setelah otorisasi + audit.
4. Gagal generate → retry. Status per payslip terlihat oleh HR.

---

### PF-23 — Diff vs Bulan Lalu + Warning (P1)
**Acceptance criteria:** di halaman review run, tampilkan per employee: net bulan ini vs bulan lalu, selisih %, flag jika > ambang (misalnya 20%); ringkasan run: total, jumlah karyawan, karyawan baru/keluar, dan jumlah warning per jenis.

---

### PF-24 — Tenant-Aware Metrics (P1)
**Acceptance criteria:** metrik `payroll_run_duration_seconds`, `payroll_items_calculated_total`, `payroll_item_errors_total`, `job_queue_depth`, `job_dead_total`, `tenant_isolation_violation_total` (dari defensive check; harus 0). Label `tenant_id` hanya pada metrik dengan kardinalitas yang aman, atau diganti dengan *tenant tier*. Detail di desain observability & testing.

---

## 3. Traceability: fitur → demo → failure scenario

| Demo / Scenario | Fitur |
|---|---|
| Demo A: Tenant isolation | PF-02, PF-03, PF-04 |
| Demo B: Crash & resume, run ganda | PF-13 |
| Demo C: Immutability & koreksi | PF-14, PF-19 |
| Payroll job crash / worker retry / partial processing | PF-13 |
| Duplicate / concurrent payroll execution | PF-13 |
| Employee berubah saat payroll | PF-07, PF-14 (ADR-P07) |
| Invalid salary configuration | PF-07, PF-12 |
| Approval rejection | PF-11, PF-13 |
| Tenant access violation | PF-03, PF-04 |

> 10 payroll failure scenario lengkap dikerjakan di **Desain domain**.
