# Step 5.5 — PayRun Authorization Matrix

> Sumber endpoint: [`04-payrun-api.md`](04-payrun-api.md) (ID A1…Z2). Dasar keputusan: ADR-010 (RBAC + policy berbasis relasi di service layer), ADR-P01/P02 (RLS + `tid`), ADR-P09 (approval), [`00-index.md`](00-index.md) §5 (404 vs 403).
> Matriks ini adalah **input langsung untuk authorization matrix test** (PF-04 AC3). Mengubah matriks = mengubah test, dan sebaliknya.

---

## 1. Role dan scope

### 1.1 Role (per membership)
| Role | Tanggung jawab | Bukan tanggung jawabnya |
|---|---|---|
| `OWNER` | Pemilik akun tenant: setting, semua role, approve run | — (punya semua hak, tetapi tetap tunduk pada aturan self-approval) |
| `HR_ADMIN` | Data karyawan, organisasi, gaji & komponen, absensi, cuti, PII | Menjalankan/menyetujui payroll run |
| `PAYROLL_ADMIN` | Menjalankan payroll run, review item, submit, finalize, adjustment | Mengubah gaji pokok/komponen karyawan, melihat PII penuh |
| `MANAGER` | Approve cuti/lembur bawahan langsung, melihat data dasar bawahan | Data gaji, data karyawan di luar bawahannya |
| `EMPLOYEE` | Self-service: absensi, cuti, lembur, slip gaji sendiri | Data karyawan lain |

Satu membership bisa memegang beberapa role (contoh: `MANAGER` + `EMPLOYEE`). **Hak efektif = gabungan (union)** semua role. Scope SELF selalu tersedia bagi user yang tertaut ke employee, apa pun role-nya.

### 1.2 Scope berbasis relasi
| Scope | Simbol | Definisi | Cara ditegakkan |
|---|---|---|---|
| TENANT | ✅ | Semua resource di tenant aktif | RLS (`tid`) saja |
| SELF | 👤 | Resource milik employee yang tertaut ke user (`employees.user_id = principal.user_id`) | Predikat query `employee_id = $self` di service; `employee_id` dari request diabaikan atau dipaksa |
| DIRECT_REPORTS | 👥 | Employee yang `org_assignments.manager_employee_id = $self` **pada tanggal hari ini** (effective-dated) | Predikat query join `org_assignments` aktif |
| APPROVER | 🖊️ | Approval dengan `approver_id = principal.user_id` (snapshot saat submit) | Guard `WHERE approver_id = $actor` |
| Tidak diizinkan | ❌ | — | Lihat kolom "Ditolak →" |

### 1.3 Aturan status penolakan
| Jenis endpoint | Ditolak dengan | Alasan |
|---|---|---|
| Koleksi / aksi tingkat tenant (list, create) yang tidak boleh untuk role | `403 FORBIDDEN` | Keberadaan koleksi bukan rahasia |
| Resource tunggal yang **tidak terlihat** dalam scope role (karyawan lain bagi `E`, non-bawahan bagi `M`) | `404 NOT_FOUND` | Tidak membocorkan keberadaan resource |
| Resource tunggal yang **terlihat** oleh role, tetapi aksinya tidak diizinkan | `403 FORBIDDEN` | Resource sah diketahui; yang ditolak adalah aksinya |
| Resource milik tenant lain (role apa pun) | `404 NOT_FOUND` | RLS mengembalikan 0 baris |
| Token tanpa `tid` di endpoint tenant-scoped | `403 FORBIDDEN` | Belum memilih tenant |

---

## 2. Matriks

Kolom: **O** OWNER · **H** HR_ADMIN · **P** PAYROLL_ADMIN · **M** MANAGER · **E** EMPLOYEE. **Ditolak →** status untuk role ❌. **Audit?** ✅ = setiap panggilan sukses menulis `audit_log`.

### 2.1 Auth, session, tenancy, members

| ID | Endpoint | O | H | P | M | E | Ditolak → | Audit? |
|---|---|---|---|---|---|---|---|---|
| A1 | `POST /auth/login` | publik | | | | | — | ✅ (sukses & gagal) |
| A2 | `POST /auth/refresh` | cookie | | | | | `401` | |
| A3 | `POST /auth/logout` | cookie | | | | | — | |
| A4 | `GET /me` | user | | | | | `401` | |
| A5 | `GET /me/memberships` | user (milik sendiri) | | | | | `401` | |
| A6 | `POST /session/switch-tenant` | membership `ACTIVE` | | | | | `404` | ✅ |
| T1 | `POST /tenants/signup` | publik | | | | | — | ✅ |
| T2 | `GET /tenant` | ✅ | ✅ | ✅ | ✅ | ✅ | — | |
| T3 | `PATCH /tenant/settings` | ✅ | ❌ | ❌ | ❌ | ❌ | `403` | ✅ |
| T4 | `GET /holidays` | ✅ | ✅ | ✅ | ✅ | ✅ | — | |
| T5 | `POST /holidays` | ✅ | ✅ | ❌ | ❌ | ❌ | `403` | ✅ |
| T6 | `DELETE /holidays/{id}` | ✅ | ✅ | ❌ | ❌ | ❌ | `403` | ✅ |
| U1 | `GET /members` | ✅ | ✅ | ❌ | ❌ | ❌ | `403` | |
| U2 | `POST /invitations` | ✅ | ✅ᴿ | ❌ | ❌ | ❌ | `403` | ✅ |
| U3 | `POST /invitations/accept` | user (email cocok) | | | | | `410` | ✅ |
| U4 | `PATCH /members/{id}/roles` | ✅ | ✅ᴿ | ❌ | ❌ | ❌ | `403` | ✅ |
| U5 | `POST /members/{id}/revoke` | ✅ | ✅ᴿ | ❌ | ❌ | ❌ | `403` | ✅ |

ᴿ = terbatas: `HR_ADMIN` hanya boleh memberi/mencabut role `MANAGER`/`EMPLOYEE`, dan hanya pada member yang tidak memegang role admin (`OWNER`/`HR_ADMIN`/`PAYROLL_ADMIN`). Di luar itu → `403`.

### 2.2 Organisasi & employee

| ID | Endpoint | O | H | P | M | E | Ditolak → | Audit? |
|---|---|---|---|---|---|---|---|---|
| G1 | `GET /departments`, `/positions` | ✅ | ✅ | ✅ | ✅ | ✅ | — | |
| G2 | `POST /departments`, `/positions` | ✅ | ✅ | ❌ | ❌ | ❌ | `403` | ✅ |
| G3 | `PATCH /departments/{id}`, `/positions/{id}` | ✅ | ✅ | ❌ | ❌ | ❌ | `403` | ✅ |
| G4 | `POST …/{id}/deactivate` | ✅ | ✅ | ❌ | ❌ | ❌ | `403` | ✅ |
| W1 | `GET /employees` | ✅ | ✅ | ✅ | 👥 | ❌ | `403` | |
| W2 | `POST /employees` | ✅ | ✅ | ❌ | ❌ | ❌ | `403` | ✅ |
| W3 | `GET /employees/{id}` | ✅ | ✅ | ✅ | 👥ᴮ | 👤ᴮ | `404` | |
| W4 | `PATCH /employees/{id}` | ✅ | ✅ | ❌ | ❌ | ❌ | `403` (P), `404` (M non-bawahan, E bukan self), `403` (M bawahan, E self) | ✅ |
| W5 | `POST /employees/{id}/terminate` | ✅ | ✅ | ❌ | ❌ | ❌ | sama dengan W4 | ✅ |
| W6 | `GET /employees/{id}/pii` | ✅ | ✅ | ❌ | ❌ | 👤 | `403` (P; M bawahan), `404` (lainnya) | ✅ **setiap akses** |
| W7 | `POST /employees/{id}/org-assignments` | ✅ | ✅ | ❌ | ❌ | ❌ | sama dengan W4 | ✅ |
| W8 | `GET /employees/{id}/salary-history` | ✅ | ✅ | ✅ | ❌ | 👤 | `404` (M, E bukan self) | |
| W9 | `POST /employees/{id}/salary-changes` | ✅ | ✅ | ❌ | ❌ | ❌ | `403` (P), `404` (M/E di luar scope), `403` (M bawahan, E self) | ✅ |
| W10 | `POST /employees/{id}/component-assignments` | ✅ | ✅ | ❌ | ❌ | ❌ | sama dengan W9 | ✅ |
| C1 | `GET /component-definitions` | ✅ | ✅ | ✅ | ❌ | ❌ | `403` | |
| C2 | `POST /component-definitions` | ✅ | ✅ | ❌ | ❌ | ❌ | `403` | ✅ |
| C3 | `PATCH /component-definitions/{id}` | ✅ | ✅ | ❌ | ❌ | ❌ | `403` | ✅ |
| C4 | `POST /component-definitions/{id}/deactivate` | ✅ | ✅ | ❌ | ❌ | ❌ | `403` | ✅ |

ᴮ = versi dasar: tanpa bank, NIK, NPWP, dan PTKP (lihat `04` §16).

### 2.3 Attendance, leave, overtime, approval

| ID | Endpoint | O | H | P | M | E | Ditolak → | Audit? |
|---|---|---|---|---|---|---|---|---|
| D1 | `POST /attendance/clock-in` | 👤 | 👤 | 👤 | 👤 | 👤 | `422 EMPLOYEE_NOT_ACTIVE` (tanpa employee tertaut) | |
| D2 | `POST /attendance/clock-out` | 👤 | 👤 | 👤 | 👤 | 👤 | sama dengan D1 | |
| D3 | `GET /attendance` | ✅ | ✅ | ✅ | 👥 + 👤 | 👤 | `404` (`employee_id` di luar scope) | |
| D4 | `PATCH /attendance/{id}` | ✅ | ✅ | ❌ | ❌ | ❌ | `403` (resource terlihat), `404` (di luar scope) | ✅ |
| D5 | `POST /attendance-imports` | ✅ | ✅ | ❌ | ❌ | ❌ | `403` | ✅ |
| D6 | `GET /attendance-imports/{id}` | ✅ | ✅ | ❌ | ❌ | ❌ | `404` | |
| D7 | `GET /attendance-imports/{id}/errors` | ✅ | ✅ | ❌ | ❌ | ❌ | `404` | |
| L1 | `GET /leave-types` | ✅ | ✅ | ✅ | ✅ | ✅ | — | |
| L2 | `GET /me/leave-balances` | 👤 | 👤 | 👤 | 👤 | 👤 | — | |
| L3 | `POST /leave-requests` | 👤 | 👤 | 👤 | 👤 | 👤 | `422 EMPLOYEE_NOT_ACTIVE` | ✅ |
| L4 | `POST /leave-requests/{id}/cancel` | 👤 | 👤 | 👤 | 👤 | 👤 | `404` (bukan miliknya) | ✅ |
| L5 | `GET /leave-requests` | ✅ | ✅ | ❌ (👤) | 👥 + 👤 | 👤 | — (hasil dibatasi scope) | |
| V1 | `POST /overtime-requests` | 👤 | 👤 | 👤 | 👤 | 👤 | `422 EMPLOYEE_NOT_ACTIVE` | ✅ |
| V2 | `POST /overtime-requests/{id}/cancel` | 👤 | 👤 | 👤 | 👤 | 👤 | `404` | ✅ |
| V3 | `GET /overtime-requests` | ✅ | ✅ | ✅ | 👥 + 👤 | 👤 | — (hasil dibatasi scope) | |
| R1 | `GET /approvals/inbox` | 🖊️ | 🖊️ + `scope=all` | 🖊️ | 🖊️ | 🖊️ | — (hasil dibatasi) | |
| R2 | `POST /approvals/{id}/approve` | 🖊️ | 🖊️ + override (cuti/lembur) | 🖊️ | 🖊️ | 🖊️ | `404` (bukan approver) | ✅ |
| R3 | `POST /approvals/{id}/reject` | 🖊️ | 🖊️ + override (cuti/lembur) | 🖊️ | 🖊️ | 🖊️ | `404` | ✅ |

Kolom 👤 berarti "untuk dirinya sendiri": berlaku bagi role apa pun yang tertaut ke employee. Admin juga karyawan, sehingga juga clock-in dan mengajukan cuti. Kolom 🖊️ berarti siapa pun yang di-snapshot sebagai approver. Dalam praktik, approver adalah manager, HR (fallback), atau admin payroll (run).

### 2.4 Payroll, adjustment, payslip, audit

| ID | Endpoint | O | H | P | M | E | Ditolak → | Audit? |
|---|---|---|---|---|---|---|---|---|
| Y1 | `GET /payroll-periods` | ✅ | ✅ | ✅ | ❌ | ❌ | `403` | |
| Y2 | `POST /payroll-runs` | ✅ | ❌ | ✅ | ❌ | ❌ | `403` | ✅ |
| Y3 | `GET /payroll-runs` | ✅ | ✅ | ✅ | ❌ | ❌ | `403` | |
| Y4 | `GET /payroll-runs/{id}` | ✅ | ✅ | ✅ | ❌ | ❌ | `404` | |
| Y5 | `POST /payroll-runs/{id}/calculate` | ✅ | ❌ | ✅ | ❌ | ❌ | `403` (H), `404` (M/E) | ✅ |
| Y6 | `GET /payroll-runs/{id}/items` | ✅ | ✅ᴬ | ✅ | ❌ | ❌ | `404` | ✅ᴬ |
| Y7 | `GET /payroll-runs/{id}/items/{item_id}` | ✅ | ✅ᴬ | ✅ | ❌ | ❌ | `404` | ✅ᴬ |
| Y8 | `POST …/items/{item_id}/exclude` | ✅ | ❌ | ✅ | ❌ | ❌ | `403` (H), `404` (M/E) | ✅ |
| Y9 | `GET /payroll-runs/{id}/diff` | ✅ | ❌ | ✅ | ❌ | ❌ | `403` (H), `404` (M/E) | |
| Y10 | `POST /payroll-runs/{id}/submit` | ✅ | ❌ | ✅ | ❌ | ❌ | `403` (H), `404` (M/E) | ✅ |
| Y11 | `POST /payroll-runs/{id}/finalize` | ✅ | ❌ | ✅ | ❌ | ❌ | `403` (H), `404` (M/E) | ✅ |
| Y12 | `POST /payroll-runs/{id}/cancel` | ✅ | ❌ | ✅ | ❌ | ❌ | `403` (H), `404` (M/E) | ✅ |
| J1 | `POST /adjustments` | ✅ | ✅ | ✅ | ❌ | ❌ | `403` | ✅ |
| J2 | `GET /adjustments` | ✅ | ✅ | ✅ | ❌ | ❌ | `403` | |
| J3 | `POST /adjustments/{id}/cancel` | ✅ | ✅ | ✅ | ❌ | ❌ | `404` | ✅ |
| S1 | `GET /me/payslips` | 👤 | 👤 | 👤 | 👤 | 👤 | — | |
| S2 | `GET /me/payslips/{id}` | 👤 | 👤 | 👤 | 👤 | 👤 | `404` (bukan miliknya) | |
| S3 | `GET /payslips` | ✅ | ✅ | ✅ | ❌ | ❌ | `403` | |
| S4 | `GET /payslips/{id}` | ✅ᴬ | ✅ᴬ | ✅ᴬ | ❌ | ❌ | `404` | ✅ **setiap akses** |
| S5 | `GET /payslips/{id}/download` | ✅ᴬ + 👤 | ✅ᴬ + 👤 | ✅ᴬ + 👤 | 👤 | 👤 | `404` | ✅ (non-pemilik) |
| Z1 | `GET /audit-log` | ✅ | ✅ | ❌ | ❌ | ❌ | `403` | |
| Z2 | `GET /healthz`, `/readyz` | publik (internal) | | | | | — | |

ᴬ = diizinkan dengan audit. `HR_ADMIN` boleh **membaca** hasil payroll (untuk menjawab pertanyaan karyawan), tetapi setiap akses detail tercatat. `OWNER`/`PAYROLL_ADMIN` yang membuka slip **orang lain** lewat S4 juga diaudit (BR10 tidak membedakan role admin).

---

## 3. Aturan khusus

### 3.1 Pemisahan tugas (separation of duties)
| Aturan | Ditegakkan oleh | Alasan |
|---|---|---|
| **`HR_ADMIN` mengubah gaji/komponen; `PAYROLL_ADMIN` tidak** (W9, W10, C2–C4) | Matriks + test | Orang yang menjalankan payroll tidak boleh sekaligus bisa menaikkan gaji, termasuk gajinya sendiri, lalu langsung memprosesnya. Ini kontrol fraud klasik di proses penggajian |
| **`PAYROLL_ADMIN` menjalankan run; `HR_ADMIN` tidak** (Y2, Y5, Y8, Y10–Y12) | Matriks + test | Pasangan dari aturan di atas: pengubah input ≠ pemroses output |
| **Approver run ≠ submitter** | ADR-P09, CHECK `ck_approval_requests_not_self`, validasi Y10 | Empat mata (*four-eyes*) sebelum uang keluar |
| **Pengecualian tenant satu admin** | Y10 `self_approval_reason` + `self_approved = true` + metrik `approval_self_approved_total` | Tanpa pengecualian ini, UMKM dengan satu admin tidak bisa menggaji karyawannya. Risikonya diterima, dibuat terlihat (audit + metrik), dan tidak disembunyikan |
| **`OWNER` memegang semua hak** | Matriks | Pemilik akun bertanggung jawab penuh. SoD di atas tetap berlaku untuk **pasangan aksi** (approve run sendiri tetap dilarang kecuali pengecualian satu admin) |

> Kalau satu orang diberi `HR_ADMIN` + `PAYROLL_ADMIN` sekaligus, union role menghapus pemisahan tugas. V1 **mengizinkan** kombinasi ini (UMKM kecil memang begitu) tetapi menampilkan peringatan di U4 dan mencatat audit `membership.sod_conflict`. Penegakan keras (tolak kombinasi) adalah opsi konfigurasi tenant di V2.

### 3.2 Self-approval
- Cuti/lembur: **tidak pernah** boleh (CHECK DB). Kalau atasan langsung = requester, atau tidak ada atasan, approver fallback ke `HR_ADMIN` (ADR-P09). Kalau `HR_ADMIN` satu-satunya juga requester → fallback ke `OWNER`.
- Payroll run: hanya lewat pengecualian satu admin (§3.1).

### 3.3 Override oleh HR_ADMIN
- Hanya untuk `LEAVE_REQUEST`/`OVERTIME_REQUEST`, misalnya ketika approver sedang cuti. `is_override = true`, `decided_by ≠ approver_id`, dengan audit `approval.overridden`.
- **Tidak** berlaku untuk `PAYROLL_RUN`. Run hanya bisa diputuskan oleh approver yang ditunjuk; untuk mengganti approver, run di-reject lalu di-submit ulang.
- HR tidak bisa meng-override request miliknya sendiri (aturan self-approval tetap berlaku).

### 3.4 PII dan data gaji
| Data | Siapa | Catatan |
|---|---|---|
| PII penuh (NIK, NPWP, no. rekening) | `O`, `H`, dan pemiliknya (👤) | Setiap akses diaudit (W6). `P` hanya melihat versi ter-mask. Transfer bank menggunakan file export (P2) yang dibuat sistem, bukan dibaca manusia |
| Gaji pokok & komponen | `O`, `H`, `P`, pemiliknya | `P` perlu membaca untuk review run, tetapi tidak bisa mengubah |
| Hasil payroll per karyawan (item, slip) | `O`, `P`, `H` (ter-audit), pemiliknya | `M` **tidak** melihat gaji bawahan |

### 3.5 MANAGER dan scope effective-dated
- 👥 dievaluasi **pada hari ini**. Manager yang baru dipindah kehilangan akses ke bawahan lama mulai tanggal `valid_from` penugasan baru.
- Approval yang sudah di-snapshot ke manager lama **tetap** di inbox manager lama (ADR-P09). Hak approve berasal dari snapshot, bukan dari relasi saat ini.

---

## 4. Matriks sebagai test (PF-04)

### 4.1 Fixture
```text
Tenant Alpha:  owner_a (O), hr_a (H), payroll_a (P), manager_a (M+E, punya bawahan emp_a1),
               emp_a1 (E, bawahan manager_a), emp_a2 (E, bukan bawahan manager_a)
               + data: department, komponen, run Maret (FINALIZED), run April (CALCULATED), slip, approval PENDING
Tenant Beta:   owner_b, hr_b, payroll_b, manager_b, emp_b1 + data setara
```
Fixture dibangun sekali per paket test (testcontainers Postgres + migrasi + seed), lalu setiap kasus berjalan di transaksi yang di-rollback.

### 4.2 Bentuk test (table-driven)
```go
type authzCase struct {
    ID       string       // "W3"
    Method   string
    Path     string       // template: "/employees/{emp_a2}"
    Actor    string       // "manager_a"
    Want     int          // 404
    WantCode string       // "NOT_FOUND"
    Audit    bool         // harus ada baris audit_log baru?
}
```
- Kasus dibangkitkan dari file data `authz_matrix.yaml` yang isinya adalah tabel §2 (satu baris per endpoint × aktor × target). Dokumen dan test **berasal dari sumber yang sama**.
- Untuk setiap baris dijalankan tiga varian target bila relevan: **SELF** (resource milik aktor), **SCOPE** (bawahan/tenant), dan **OUT** (karyawan lain / non-bawahan).
- Assert: status HTTP, `code`, dan (jika `Audit`) jumlah baris `audit_log` bertambah tepat 1 dengan `action` yang benar.

### 4.3 Kolom lintas tenant
Untuk **setiap** endpoint tenant-scoped yang menerima ID: semua aktor Beta memanggil ID milik Alpha → **`404 NOT_FOUND`**. Untuk endpoint list: hasil tidak pernah mengandung ID Alpha. Kasus ini dibangkitkan otomatis dari daftar rute, tanpa ditulis per endpoint.

### 4.4 Aturan CI
1. **Endpoint tanpa baris matriks = test gagal.** Test membaca daftar rute dari router chi (`chi.Walk`) dan membandingkannya dengan `authz_matrix.yaml`. Rute baru yang belum didaftarkan menggagalkan build.
2. **Baris matriks tanpa rute = test gagal** (matriks basi).
3. Test berjalan di setiap PR (Step 10 CI), bersama tenant isolation test (RLS bypass, pool leakage; Step 3 `05`).

### 4.5 Yang **tidak** dicakup matriks (dan di mana dicakup)
| Aspek | Dicakup oleh |
|---|---|
| Kebocoran lewat RLS saat query lupa filter | RLS bypass test (PF-04 AC4) |
| Kebocoran setting tenant via connection pool | Pool leakage test (PF-04 AC5) |
| Job background lintas tenant | Test job dengan `tenant_id` palsu di payload (PFS-09) |
| Presigned URL ditebak / kedaluwarsa | Test S5 (TTL, signature) + Step 6 |
| Manipulasi `tid` di JWT | Test middleware auth (signature) |

---

## Catatan implementasi (Sprint 2)

- Matriks sebagai data ada di **`api/authz_matrix.yaml`** (bukan `authz_matrix.yaml` di root). `TestAuthzMatrix_CoversEveryRoute` membandingkannya dengan `chi.Walk` dua arah (§4.4 aturan 1–2) dan mewajibkan setiap baris tenant punya case. Baris S3+ ditambahkan bersama rutenya.
- Endpoint yang mengubah data mendapat resource **baru** per aktor (`If-Match: "1"` selalu valid), sehingga urutan eksekusi tidak memengaruhi hasil.
- §4.3 lintas tenant: role yang **diizinkan** endpoint wajib `404`. Role yang **ditolak** endpoint boleh `403` (penolakan tingkat aksi oleh `RequireAnyRole`, dijawab sebelum id dibaca sehingga tidak bergantung pada keberadaan resource) atau `404`. Respons 2xx apa pun = kebocoran.
- Fixture §4.1 di S2 belum memuat bawahan `MANAGER` dan data payroll; kasus 👥 dan Y/S ditambahkan di S3–S7.

