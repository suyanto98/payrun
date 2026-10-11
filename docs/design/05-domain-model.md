# PayRun: Domain Model

> Glosarium kanonik (nama aggregate, state, ID invariant): `00-index.md`. File ini **mengelaborasi**, tidak mengubah.
> Referensi: [feature spec](03-features.md), [arsitektur](04-architecture.md), ADR-P01…P09.

---

## 1. Subdomain Map

```text
┌──────────────────────────── CORE ────────────────────────────┐
│  Payroll: engine (pure) · run · item/line · adjustment ·     │
│           period · snapshot/fingerprint                      │
└───────────────▲───────────────▲───────────────▲──────────────┘
                │ input         │ input         │ keputusan
┌───────────────┴──┐ ┌──────────┴─────────┐ ┌───┴────────────┐ ┌──────────────────┐
│ Workforce        │ │ Time & Attendance  │ │ Approval       │ │ Compensation     │
│ employee, org    │ │ attendance, leave, │ │ approval req.  │ │ component def,   │
│ (effective-dated)│ │ overtime           │ │ (generik)      │ │ salary assign.,  │
└──────────────────┘ └────────────────────┘ └────────────────┘ │ ruleset          │
            SUPPORTING                                         └──────────────────┘
┌──────────────────────────── GENERIC ─────────────────────────┐
│  Tenancy & IAM · Audit · Documents (payslip PDF, CSV)        │
└──────────────────────────────────────────────────────────────┘
```

| Subdomain | Jenis | Kenapa jenis ini | Modul (desain fitur & arsitektur §2.3) |
|---|---|---|---|
| **Payroll** | Core | Pembeda produk: perhitungan yang benar, reproducible, immutable. Di sinilah energi engineering terbesar | `payroll` (`payroll/engine`, `payroll/run`) |
| **Compensation** | Supporting | Data yang dikonsumsi engine; rumit karena effective-dated, tetapi bukan pembeda | `compensation` |
| **Workforce** | Supporting | Subjek payroll; sebagian besar CRUD + effective dating untuk org | `employee`, `org` |
| **Time & Attendance** | Supporting | Penyedia input (hari hadir, cuti, lembur); punya aturan lokal (saldo cuti, tumpang tindih) | `attendance`, `leave`, `overtime` |
| **Approval** | Supporting | Workflow generik yang dipakai 3 subjek; kecil, tetapi menyentuh authorization relasi | `approval` |
| **Tenancy & IAM** | Generic | Setiap SaaS butuh; risikonya tinggi tetapi polanya standar | `tenancy`, `iam` |
| **Audit** | Generic | Append-only log; pola sama dengan LedgerPay | `audit` |
| **Documents** | Generic | Rendering PDF + storage; bisa diganti library/vendor tanpa mengubah domain | `payslip` |

**Arah dependency:** Supporting → *menyediakan* input untuk Core lewat interface yang dimiliki Core (§8). Core tidak mengenal tabel modul lain. Generic dipakai semua modul.

---

## 2. Ubiquitous Language

| Istilah | Arti | Jangan dikacaukan dengan |
|---|---|---|
| **Period** (`PayrollPeriod`) | Bulan kalender gaji untuk satu tenant (`2026-03`), dengan tanggal *cut-off* | **Run**: proses perhitungan untuk sebuah period. Satu period bisa punya banyak run yang di-cancel, tetapi maksimal satu yang aktif (PI-03) |
| **Run** (`PayrollRun`) | Satu eksekusi payroll untuk satu period; memiliki lifecycle sampai `FINALIZED` | **calc_version**: nomor kalkulasi *di dalam* satu run. Recalculate menaikkan versi, bukan membuat run baru |
| **calc_version** | Penanda generasi hasil kalkulasi dalam satu run. Hanya versi terkini yang berlaku (PI-06) | Versi **ruleset** (tarif pajak/BPJS) |
| **Item** (`PayrollItem`) | Hasil per karyawan dalam run + `calc_version`: gross, deductions, net, snapshot, fingerprint | **Line**: satu baris komponen di dalam item ("Lembur 6 jam × 1,5 × …") |
| **Line** (`PayrollLine`) | Satu komponen terhitung + *explanation*; sudah dibulatkan | Component **definition** (konfigurasi tenant) |
| **Cut-off** | Batas waktu persetujuan agar cuti/lembur ikut dihitung di period ini (PI-08) | Akhir period. Cut-off bisa jatuh sebelum akhir bulan |
| **Proration** | Faktor `hari aktif / basis hari` per *span* (joiner, leaver, perubahan gaji tengah period) | Potongan absen: absen adalah *deduction* terpisah, bukan prorata |
| **Effective dating** | Record berlaku di rentang `[valid_from, valid_to)`. Perubahan menutup record lama dan membuat record baru | Audit log: audit mencatat *siapa mengubah*; effective dating mencatat *nilai yang berlaku kapan* |
| **Adjustment** | Koreksi atas period yang **sudah final**, diterapkan di run period berikutnya sebagai line terpisah | **Recalculation**: menghitung ulang run yang **belum final** (calc_version++) |
| **Fingerprint** | Hash input per karyawan (+ ruleset version) untuk mendeteksi *apakah input berubah* (PI-13) | **Snapshot**: salinan input lengkap untuk *mereproduksi* hasil (PI-12). Fingerprint = deteksi; snapshot = bukti |
| **Approve** | Keputusan manusia bahwa hasil kalkulasi benar | **Finalize**: langkah teknis eksplisit yang mengunci run + period dan memicu payslip. Approve dapat dibatalkan dengan recalc (lewat reject); finalize tidak |
| **User** | Identitas login global (email + password) | **Membership**: hubungan user ↔ tenant + role. **Employee**: subjek payroll di sebuah tenant; boleh tanpa user (karyawan yang tidak memakai self-service) |
| **Stale** | Run yang input-nya berubah setelah kalkulasi; tidak bisa di-approve | `FAILED`: kalkulasi gagal secara teknis/data |

---

## 3. Entities vs Value Objects

**Entity** punya identitas dan lifecycle: Tenant, Membership, Employee, OrgAssignment, SalaryAssignment, ComponentDefinition, Ruleset, AttendanceRecord, LeaveBalance, LeaveRequest, OvertimeRequest, ApprovalRequest, PayrollPeriod, PayrollRun, PayrollItem, Adjustment, Payslip, AuditEntry.

**Value object** tidak punya identitas, immutable, dan divalidasi saat dibuat. Tujuannya mencegah kelas bug, bukan formalitas.

| Value Object | Representasi | Bug yang dicegah |
|---|---|---|
| `Money` | `decimal.Decimal` + currency `IDR`; di JSON **string** (`"4250000.00"`), di DB `NUMERIC(18,2)` (ADR-P06) | Float, campur currency, pembulatan sembarangan |
| `Rate` / `Percentage` | `decimal.Decimal` dengan skala lebih tinggi (`NUMERIC(9,6)`); konstruktor menolak < 0 | `2` vs `0.02` tertukar (konstruktor eksplisit `PercentFromBasisPoints`) |
| `DateRange` | `[From, To)` half-open, `To` opsional (∞); method `Contains`, `Overlaps`, `Intersect`, `Days` | Off-by-one di batas bulan, double-count hari perubahan gaji |
| `Period` | `(Year, Month)` + `Range() DateRange` + `CutOff time.Time` | Period invalid (bulan 13), salah hitung jumlah hari |
| `Fingerprint` | `[32]byte` (SHA-256), dibandingkan constant-time tidak diperlukan (bukan rahasia), di-hex saat serialisasi | Membandingkan hash dari serialisasi non-kanonik |
| `EncryptedField` | `v1:<nonce>:<ct>` + AAD `tenant_id:table.column` (ADR-P08) | Menyimpan plaintext PII; ciphertext swapping |
| `MaskedValue` | String hasil masking (`****1234`); tipe berbeda dari nilai asli | Mengembalikan nilai penuh ke API secara tidak sengaja |
| `Sensitive[T]` | Wrapper yang `String()`/`MarshalJSON()`-nya `[REDACTED]` | PII bocor di log |
| `TenantID`, `EmployeeID`, `RunID`, … | Go **named types** di atas `uuid.UUID` (`type TenantID uuid.UUID`) | Tertukar argumen: `GetEmployee(ctx, tenantID, employeeID)` dipanggil terbalik tetap ter-compile kalau keduanya `uuid.UUID` |

```go
type TenantID   uuid.UUID
type EmployeeID uuid.UUID

func (r *EmployeeRepo) Get(ctx context.Context, q *Queries, id EmployeeID) (Employee, error)
// GetEmployee(ctx, q, tenantID) → compile error, bukan bug di production
```

---

## 4. Aggregates

Aggregate = **batas konsistensi**: semua yang di dalamnya diubah dalam satu transaksi dan invariant-nya dijaga bersama. Aggregate dibuat sekecil mungkin. Referensi antar aggregate memakai ID.

| Aggregate (root) | Isi | Invariant yang dijaga | Guard / locking | Kenapa batasnya di sini |
|---|---|---|---|---|
| `Tenant` | settings, default ruleset ref | PI-16 (bersama Membership) | Deferred trigger hitung OWNER | Konfigurasi tenant berubah jarang, independen dari data karyawan |
| `Membership` | user_id, tenant_id, roles, status | PI-16, PI-01 | `UNIQUE (tenant_id, user_id)`; transisi `WHERE status=…` | Hak akses berubah tanpa menyentuh Employee |
| `Employee` | identitas, PII terenkripsi, employment spans | PI-18, PI-07 (span) | `UNIQUE (tenant_id, employee_no)`, `UNIQUE (tenant_id, nik_bidx)` | Data pribadi berubah independen dari gaji/org |
| `OrgAssignment` | department, position, manager, `valid_during` | PI-09, PI-19 | EXCLUDE gist; cek siklus | Dipisah dari Employee agar riwayat org tidak membengkakkan Employee |
| `SalaryAssignment` | base salary + komponen, `valid_during` | PI-09, PI-14 | EXCLUDE gist; PeriodGuard | Unit perubahan "kenaikan gaji" |
| `ComponentDefinition` | tipe, parameter, cap, rounding | Validitas referensi `PERCENT_OF` | APP (validasi graf referensi) | Konfigurasi tenant; dipakai banyak karyawan |
| `Ruleset` | tabel pajak/BPJS, versi | PI-12 | Immutable per versi (INSERT only) | Reproducibility butuh versi yang tidak pernah berubah |
| `AttendanceRecord` | per (employee, tanggal) | PI-14 | `UNIQUE (tenant_id, employee_id, work_date)`; PeriodGuard + trigger | Unit kerja harian; upsert idempoten untuk import |
| `LeaveBalance` | (employee, leave_type, tahun) → saldo | PI-10 | `CHECK (balance >= 0)` (atau tipe yang mengizinkan minus); `SELECT … FOR UPDATE` | Titik contention saat approval paralel; dipisah dari LeaveRequest agar lock-nya kecil |
| `LeaveRequest` / `OvertimeRequest` | rentang/jam, status | PI-08, tumpang tindih cuti | EXCLUDE gist pada `(employee_id, daterange)` untuk status aktif; guard status | Satu permintaan = satu unit keputusan |
| `ApprovalRequest` | subject, requester, approver snapshot, status | PI-11 | Partial unique `WHERE status='PENDING'`; `CHECK (approver_id <> requester_id)` kecuali flag `self_approved`; guard `WHERE status='PENDING'` | Generik untuk 3 subjek (ADR-P09) |
| `PayrollPeriod` | (tenant, year, month), cut-off, status | PI-14 | `UNIQUE (tenant_id, year, month)`; guard status | Period adalah kunci untuk lock input |
| `PayrollRun` (+ Item, Line) | status, calc_version, chunks, fingerprint, totals | PI-03, PI-04, PI-05, PI-06, PI-13 | Partial unique index; guard status; trigger immutability; upsert per `(run_id, employee_id, calc_version)` | Lihat catatan di bawah |
| `Adjustment` | employee, origin period, target run, amount, status | PI-15 | `applied_run_id` diisi sekali; guard status | Koreksi adalah fakta bisnis tersendiri, bukan edit run lama |
| `Payslip` | run, employee, object key, render status | PI-17 | Key deterministik; RLS + policy | Dokumen turunan; bisa di-render ulang tanpa menyentuh item |
| `AuditEntry` | actor, action, target, diff | Append-only | Trigger tolak UPDATE/DELETE | — |

### Catatan pragmatis: ukuran aggregate `PayrollRun`

Secara konsep, run, item, dan line adalah satu aggregate: total run harus konsisten dengan item (PI-05), dan item final tidak boleh berubah (PI-04). Tetapi **memuat 5.000 item ke memori untuk setiap perubahan run tidak masuk akal.**

Keputusan:
- **Tulis per chunk.** Setiap chunk job meng-upsert ±50 item dalam transaksinya sendiri, lalu menaikkan `chunks_done` pada root dengan guard `calc_version` (ADR-P05).
- **Root menjaga state, bukan seluruh isi.** Transisi status selalu lewat root (`UPDATE payroll_runs … WHERE status = $expected`). Item hanya bisa ditulis jika root `CALCULATING` dengan `calc_version` yang cocok. Kondisi ini dicek di transaksi chunk yang sama.
- **Total dihitung, bukan diakumulasi.** Saat chunk terakhir selesai, total = `SUM(net)` dari item versi terkini (PI-05). Tidak ada `total += x` yang bisa drift saat retry.
- **Immutability di DB.** Trigger menolak perubahan item ketika root `FINALIZED` (PI-04). Jadi invariant tetap terjaga walaupun item tidak di-load lewat root.

Artinya: batas *konsistensi* tetap run, tetapi batas *transaksi* untuk kalkulasi adalah chunk. Dalam DDD klasik ini adalah pelanggaran yang disengaja, dan alasannya bisa dijelaskan di interview.

---

## 5. State Machines

Semua transisi memakai pola `UPDATE … SET status = $to WHERE id = $id AND status = $from`. Hasil 0 baris → `409 INVALID_STATE_TRANSITION` (atau 404 jika actor tidak berhak melihat). Setiap transisi menulis `audit_log` di transaksi yang sama.

### 5.1 PayrollPeriod
```text
OPEN ──(run pertama dibuat)──▶ PROCESSING ──(run FINALIZED)──▶ FINALIZED
  ▲                                │
  └──(semua run CANCELLED)─────────┘
```
| Dari → Ke | Pemicu | Guard | Side effect |
|---|---|---|---|
| — → `OPEN` | Dibuat lazily saat run pertama / HR membuka period | `UNIQUE (tenant_id, year, month)` | — |
| `OPEN → PROCESSING` | Run dibuat | Tx yang sama dengan INSERT run | — |
| `PROCESSING → OPEN` | Run aktif di-cancel | Tidak ada run aktif lain | Audit |
| `PROCESSING → FINALIZED` | Finalize run | Tx yang sama dengan finalize | Input period terkunci (PI-14) |

### 5.2 PayrollRun
```text
DRAFT ─calculate─▶ CALCULATING ─chunks selesai─▶ CALCULATED ─submit─▶ PENDING_APPROVAL ─approve─▶ APPROVED ─finalize─▶ FINALIZED
                    │      ▲                       │    ▲                  │
                    │      └──── recalculate ──────┘    └──── reject ──────┘
                    └─chunk dead─▶ FAILED ─recalculate─▶ CALCULATING
CANCELLED ◀── DRAFT | CALCULATED | FAILED
```
| Dari → Ke | Actor | Guard | Side effect (tx yang sama) |
|---|---|---|---|
| — → `DRAFT` | PAYROLL_ADMIN / OWNER | PI-03 partial unique; idempotency key | Period `OPEN → PROCESSING` |
| `DRAFT/CALCULATED/FAILED → CALCULATING` | PAYROLL_ADMIN | — | `calc_version++`, reset chunks, outbox `payroll_run.calculation_requested` (job `payroll.plan`) |
| `CALCULATING → CALCULATED` | Worker (chunk terakhir) | `chunks_done = chunks_total` dan `count(items) = eligible` | Total = Σ item; simpan run fingerprint; outbox `payroll_run.calculated` |
| `CALCULATING → FAILED` | Worker | Chunk job `dead` | Daftar karyawan bermasalah; outbox `payroll_run.failed` |
| `CALCULATED → PENDING_APPROVAL` | PAYROLL_ADMIN | Tidak ada item `ERROR` (atau dikecualikan eksplisit); fingerprint masih cocok | Buat `ApprovalRequest(PAYROLL_RUN)` |
| `PENDING_APPROVAL → APPROVED` | Approver (bukan submitter; pengecualian ADR-P09) | Approval guard + **fingerprint guard (PI-13)** + cek ulang membership | Outbox `payroll_run.approved` |
| `PENDING_APPROVAL → CALCULATED` | Approver (reject) | Catatan wajib | Approval `REJECTED` |
| `APPROVED → FINALIZED` | PAYROLL_ADMIN / OWNER | Fingerprint guard sekali lagi | Period `FINALIZED`; adjustment terkait `APPLIED`; outbox `payroll_run.finalized` (job payslip + export) |
| `DRAFT/CALCULATED/FAILED → CANCELLED` | PAYROLL_ADMIN | — | Period kembali `OPEN` jika tidak ada run aktif lain |

### 5.3 ApprovalRequest
```text
PENDING ─approve─▶ APPROVED
   ├────reject───▶ REJECTED
   └────cancel───▶ CANCELLED   (oleh requester)
```
| Dari → Ke | Actor | Guard | Side effect |
|---|---|---|---|
| — → `PENDING` | Requester (via submit subjek) | Partial unique per subjek; approver di-resolve & di-snapshot | Outbox `leave.requested` / `overtime.requested` / `payroll_run.submitted` |
| `PENDING → APPROVED` | `approver_id` (atau HR_ADMIN override, ter-audit) | `WHERE status='PENDING' AND approver_id=$actor`; bukan self (PI-11) | Efek subjek (saldo cuti, status request/run); outbox `approval.decided` |
| `PENDING → REJECTED` | Approver | Catatan wajib | Subjek → REJECTED / run → CALCULATED; outbox `approval.decided` |
| `PENDING → CANCELLED` | Requester | `requester_id=$actor` | Subjek → CANCELLED |

### 5.4 LeaveRequest / OvertimeRequest
```text
PENDING ─▶ APPROVED      (efek: saldo cuti berkurang / lembur masuk input payroll)
   ├────▶ REJECTED
   └────▶ CANCELLED
```
Status request bergerak **hanya** sebagai efek dari ApprovalRequest (atau cancel oleh requester), sehingga keduanya tidak pernah tidak sinkron. Membatalkan cuti `APPROVED` = request pembatalan baru yang mengembalikan saldo (ADR-P09). Semua perubahan ditolak jika tanggalnya jatuh di period `FINALIZED` (PI-14).

### 5.5 Adjustment
```text
PENDING_APPLY ─(run target FINALIZED)─▶ APPLIED
      └────────(dibatalkan HR)────────▶ CANCELLED
```
| Dari → Ke | Pemicu | Guard | Side effect |
|---|---|---|---|
| — → `PENDING_APPLY` | HR_ADMIN / PAYROLL_ADMIN | `origin_period` harus `FINALIZED`; target = period `OPEN/PROCESSING` berikutnya | Run target (jika sudah dikalkulasi) menjadi stale (fingerprint berubah); outbox `adjustment.created` |
| `PENDING_APPLY → APPLIED` | Finalize run target | Tx yang sama dengan finalize; `applied_run_id IS NULL` (PI-15) | — |
| `PENDING_APPLY → CANCELLED` | HR | Belum `APPLIED` | Run target stale |

### 5.6 Payslip
```text
PENDING ─render ok─▶ RENDERED
   └──render gagal (max attempts)─▶ FAILED ─(retry manual)─▶ PENDING
```
Dibuat (PENDING) untuk setiap item saat finalize. Render idempoten: object key deterministik `tenants/{tenant_id}/payslips/{run_id}/{employee_id}.pdf`, sehingga render ulang menimpa file yang sama.

### 5.7 Membership
```text
INVITED ─accept─▶ ACTIVE ─revoke─▶ REVOKED
   └──expired (7 hari) / revoke──▶ REVOKED
```
| Dari → Ke | Actor | Guard | Side effect |
|---|---|---|---|
| — → `INVITED` | OWNER / HR_ADMIN | `UNIQUE (tenant_id, email)` untuk undangan aktif | Token undangan (hash) |
| `INVITED → ACTIVE` | User yang diundang | Token valid, belum kedaluwarsa, sekali pakai | Audit |
| `ACTIVE → REVOKED` | OWNER / HR_ADMIN | PI-16 (bukan OWNER terakhir) | Revoke refresh token family untuk tenant ini; outbox `membership.revoked` |

---

## 6. Domain Events

Semua event ditulis ke outbox (`jobs`, ADR-006) **di transaksi yang sama** dengan perubahan state. Delivery at-least-once, jadi setiap consumer wajib idempoten.

| Event | Producer (use case) | Consumer | Kunci idempotensi consumer |
|---|---|---|---|
| `leave.requested` / `overtime.requested` | Submit request | Notifikasi approver (inbox UI; email P3) | `approval_request_id` |
| `approval.decided` | Approve/reject | Notifikasi requester | `approval_request_id + status` |
| `payroll_run.calculation_requested` | Calculate | `payroll.plan` (membuat chunk job) | `run_id + calc_version` (chunk job unik `run_id+calc_version+chunk_no`) |
| `payroll_run.calculated` | Chunk terakhir | Notifikasi payroll admin; metrik durasi | `run_id + calc_version` |
| `payroll_run.failed` | Chunk dead | Notifikasi + alert | `run_id + calc_version` |
| `payroll_run.submitted` | Submit | Notifikasi approver | `approval_request_id` |
| `payroll_run.approved` | Approve | Notifikasi admin (siap finalize) | `run_id` |
| `payroll_run.finalized` | Finalize | `payslip.render` (per chunk), export bank (P2) | `run_id` → payslip per `(run_id, employee_id)` |
| `payslip.rendered` | Render job | Notifikasi karyawan (P3) | `payslip_id` |
| `adjustment.created` | Create adjustment | Penanda stale di run target (sebenarnya terdeteksi oleh fingerprint; event untuk UI) | `adjustment_id` |
| `membership.revoked` | Revoke | Pencabutan sesi; audit | `membership_id` |
| `attendance.import_requested` | Upload CSV | `attendance.import` | `import_id` (+ upsert per `(employee, work_date)`) |

> Event di PayRun adalah mekanisme **side effect asinkron**, bukan sumber kebenaran. State tetap ada di tabel. Tidak ada event sourcing.

---

## 7. Transaction Boundaries

| Use case | Ditulis dalam satu transaksi | Guard / lock | Di luar transaksi / job |
|---|---|---|---|
| Tenant signup | Tenant + user (jika baru) + membership OWNER + konfigurasi default (jenis cuti, komponen, ruleset ref) | Idempotency key; unique slug/email | — |
| Invite / accept | Invite: membership `INVITED` + token hash. Accept: `INVITED → ACTIVE` | Token sekali pakai (guard status) | Email (P3) |
| Switch tenant | **Read-only**: cek membership `ACTIVE` | — | Terbitkan access token baru (tanpa tulis DB, kecuali audit) |
| Ubah gaji | Tutup record lama + INSERT record baru + audit | EXCLUDE gist (PI-09); PeriodGuard (tidak boleh menyentuh period `FINALIZED`) | — |
| Koreksi absensi | Update record + audit (alasan wajib) | PeriodGuard + trigger (PI-14) | Run aktif menjadi stale (terdeteksi saat approve) |
| Approve cuti | Approval `PENDING → APPROVED` + `SELECT … FOR UPDATE` leave_balance + kurangi saldo + request `APPROVED` + audit + outbox | Guard status; `CHECK balance >= 0` (PI-10, PI-11) | Notifikasi |
| Buat run | INSERT run `DRAFT` + period `OPEN → PROCESSING` | Partial unique (PI-03); idempotency key | — |
| Calculate (orchestrate) | Tx1: run → `CALCULATING`, `calc_version++`, enqueue `payroll.plan`. Tx2 (`payroll.plan`): INSERT K chunk job + `chunks_total` | Guard status; unique chunk job key | Engine dijalankan di worker |
| Calculate (chunk) | Upsert ±50 item + line + snapshot + fingerprint, `chunks_done++`, tandai job selesai (`CompleteInTx`), dan jika terakhir: total + `CALCULATED` | Guard `status='CALCULATING' AND calc_version=$v` (PI-06) | Load input + `engine.Calculate` boleh di dalam tx (tanpa network call), dengan `statement_timeout` |
| Submit | Run → `PENDING_APPROVAL` + ApprovalRequest | Fingerprint check; tidak ada item `ERROR` | Notifikasi |
| Approve run | Approval → `APPROVED` + run → `APPROVED` + audit | Guard status + **fingerprint (PI-13)** + cek ulang membership di DB | Notifikasi |
| Reject run | Approval → `REJECTED` + run → `CALCULATED` | Catatan wajib | — |
| Finalize | Fingerprint guard + run → `FINALIZED` + period → `FINALIZED` + adjustment → `APPLIED` + payslip rows `PENDING` + outbox | Guard status; trigger immutability aktif sejak status berubah (PI-04, PI-14, PI-15) | Render PDF, export bank |
| Buat adjustment | INSERT adjustment `PENDING_APPLY` + audit | `origin_period` `FINALIZED`; target `OPEN/PROCESSING` | — |
| Render payslip | Per chunk: tulis object **lalu** update payslip `RENDERED` + key + checksum | Key deterministik (idempoten) | Upload ke object storage terjadi sebelum commit metadata; orphan dibersihkan job (ADR-012) |
| CSV import | Per batch (±500 baris): upsert attendance per `(employee_id, work_date)` + catat hasil baris | PeriodGuard per baris; `import_id` | Simpan file ke storage sebelum job; laporan akhir |

**Aturan umum (ADR-011):** tidak ada network call di dalam transaksi DB. Engine murni CPU dan dijalankan di dalam tx chunk agar input yang dibaca konsisten (snapshot `REPEATABLE READ` per chunk boleh dipakai bila dibutuhkan; default Read Committed + guard fingerprint sudah cukup karena perubahan input terdeteksi saat approve).

---

## 8. Input Provider & PeriodGuard

Payroll (core) **mendefinisikan** interface yang dibutuhkannya. Modul supporting **mengimplementasikannya**. Arah dependency: `attendance → payroll` (implementasi interface), bukan `payroll → attendance` (*dependency inversion*).

```go
// package payroll/run — dimiliki core
type InputProvider interface {
    // Semua method menerima *Queries dari WithTenantTx (RLS aktif) dan batch employee (tanpa N+1).
    Employment(ctx context.Context, q *db.Queries, p Period, ids []EmployeeID) (map[EmployeeID][]engine.EmploymentSpan, error)
    Salary(ctx context.Context, q *db.Queries, p Period, ids []EmployeeID) (map[EmployeeID][]engine.SalarySpan, error)
    Components(ctx context.Context, q *db.Queries, p Period, ids []EmployeeID) (map[EmployeeID][]engine.ComponentAssignment, error)
    Attendance(ctx context.Context, q *db.Queries, p Period, ids []EmployeeID) (map[EmployeeID]engine.AttendanceSummary, error)
    UnpaidLeave(ctx context.Context, q *db.Queries, p Period, ids []EmployeeID) (map[EmployeeID]decimal.Decimal, error)
    Overtime(ctx context.Context, q *db.Queries, p Period, cutOff time.Time, ids []EmployeeID) (map[EmployeeID][]engine.OvertimeEntry, error)
    Adjustments(ctx context.Context, q *db.Queries, target RunID, ids []EmployeeID) (map[EmployeeID][]engine.Adjustment, error)
}
// Implementasi komposit dirakit di bootstrap: compensation.Provider, attendance.Provider, leave.Provider, …
```

- Fungsi yang sama dipakai untuk **kalkulasi** (input → engine) dan **fingerprint** (input → hash tanpa engine). Ini memastikan definisi "input" hanya ada di satu tempat; kalau tidak, fingerprint bisa tidak mendeteksi perubahan pada input yang dilupakan.
- Batch per chunk (50 karyawan) → setiap method = satu query `WHERE employee_id = ANY($1)`.

```go
// package payroll — dimiliki core, dipanggil modul input sebelum menulis
type PeriodGuard interface {
    // Mengembalikan ErrPeriodLocked jika ada tanggal di [from, to) yang jatuh di period FINALIZED.
    EnsureWritable(ctx context.Context, q *db.Queries, emp EmployeeID, r DateRange) error
}
```

- Dipanggil oleh `attendance`, `leave`, `overtime`, `compensation` sebelum INSERT/UPDATE, sehingga aturan lock tidak tersebar di empat modul.
- **Jaring pengaman:** trigger DB pada tabel input menolak perubahan yang menyentuh period `FINALIZED` (PI-14). PeriodGuard memberi error yang ramah (`409 PERIOD_LOCKED`); trigger menangkap jalur yang lupa memanggil guard.
