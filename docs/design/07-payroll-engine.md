# PayRun: Payroll Engine sebagai Business-Rule Engine

> Referensi: [ADR-P03](../adr/ADR-P03-payroll-engine.md) (pure function + komponen ter-parameter), [ADR-P04](../adr/ADR-P04-effective-dating.md) (effective dating), [ADR-P05](../adr/ADR-P05-payroll-run-execution.md) (eksekusi run), [ADR-P06](../adr/ADR-P06-decimal-rounding.md) (decimal & pembulatan), [ADR-P07](../adr/ADR-P07-snapshot-and-period-lock.md) (fingerprint & snapshot), [PF-12](03-features.md).
> ID invariant (PI-xx) dan nama state mengikuti glosarium.

> ⚠️ **Semua tarif pajak, persentase BPJS, cap, dan pembagi lembur di dokumen ini adalah CONTOH (placeholder) untuk menjelaskan mekanisme engine.** Engine membaca semua nilai itu dari *ruleset* yang dapat dikonfigurasi. Ruleset bawaan PayRun berlabel **"simplified — not compliance-certified"**, dan angkanya wajib diverifikasi terhadap regulasi yang berlaku sebelum ditulis di README atau demo publik.

---

## 1. Posisi engine dalam sistem

Formula `Gross + Allowance + Overtime − Tax − Deduction = Net` hanya puncaknya. Yang membuat payroll sulit adalah pertanyaan di sekitarnya:

| Pertanyaan | Dijawab oleh |
|---|---|
| Nilai gaji mana yang berlaku, untuk hari yang mana? | Effective dating + prorata (§5, §6) |
| Hari apa yang dihitung sebagai hari kerja, hadir, atau absen? | Kalender tenant + attendance summary (§4, §8) |
| Lembur/cuti mana yang boleh masuk? | Status `APPROVED` + cut-off (§4, §7) |
| Di mana pembulatan terjadi, dan kenapa slip selalu bisa dijumlahkan? | Kebijakan pembulatan (§3) |
| Apa yang terjadi kalau konfigurasi salah atau net negatif? | Validasi error vs warning (§13, §14) |
| Bagaimana membuktikan angka bulan lalu masih benar? | Determinisme + snapshot + versi ruleset (§12) |

**Batas tanggung jawab:**

```text
            ┌──────────────── payroll/run (I/O, transaksi, job) ────────────────┐
            │  InputProvider: kumpulkan EmployeePeriodInput dari compensation,   │
            │  attendance, leave, overtime, adjustment (WithTenantTx)            │
            │                    │                                               │
            │                    ▼                                               │
            │   ┌────────── payroll/engine (PURE) ──────────┐                    │
            │   │ Calculate(input, ruleset) → PayrollResult │  tanpa DB, tanpa   │
            │   │ deterministik, tanpa time.Now / random    │  time.Now, tanpa   │
            │   └───────────────────────────────────────────┘  float            │
            │                    │                                               │
            │                    ▼                                               │
            │  upsert PayrollItem + PayrollLine + input_snapshot + fingerprint   │
            └────────────────────────────────────────────────────────────────────┘
```

---

## 2. Calculation rules — tipe komponen

### 2.1 Empat tipe kalkulasi

| Tipe | Rumus | Prorata? | Contoh |
|---|---|---|---|
| `FIXED` | `amount × employment_factor` | Ya, terhadap hari aktif bekerja (joiner/leaver), **bukan** terhadap absensi | Tunjangan jabatan Rp500.000 |
| `PERCENT_OF` | `rate × base(ref)`, base dibatasi `[cap_min, cap_max]` jika ada | Mengikuti base-nya | BPJS JHT karyawan 2% × upah BPJS |
| `PER_DAY` | `rate × days(source)`; `source ∈ {PRESENT, WORKING}` | Implisit (jumlah hari) | Uang transport Rp20.000 × hari hadir |
| `PER_HOUR_MULTIPLIER` | `hourly_rate × Σ(jam_tier × multiplier_tier)` | Tidak | Lembur |

### 2.2 Atribut setiap `ComponentDefinition`

| Atribut | Nilai | Fungsi |
|---|---|---|
| `code` | `TUNJ_JABATAN`, `TRANSPORT`, … (unik per tenant) | Referensi oleh `PERCENT_OF` dan baris slip |
| `category` | `EARNING` / `DEDUCTION` | Tanda di net |
| `stage` | `EARNING`, `OVERTIME`, `ABSENCE`, `STATUTORY`, `TAX`, `ADJUSTMENT` | Posisi di pipeline (§2.4). Untuk komponen tenant hanya `EARNING`/`DEDUCTION` biasa; stage lain milik sistem |
| `taxable` | bool | Masuk ke `TAXABLE_GROSS` atau tidak |
| `bpjs_base` | bool | Masuk ke upah dasar BPJS atau tidak (umumnya gaji pokok + tunjangan tetap) |
| `rounding` | `HALF_UP` (default), `DOWN`, `UP` | §3 |
| `cap_min`, `cap_max` | decimal, opsional | Batas base (`PERCENT_OF`) atau hasil |

### 2.3 Base virtual yang bisa direferensikan `PERCENT_OF`

| Base | Isi |
|---|---|
| `BASE_SALARY` | Gaji pokok setelah prorata (hasil stage 2) |
| `FIXED_ALLOWANCES` | Σ komponen `FIXED` earning |
| `BPJS_WAGE` | Σ baris dengan `bpjs_base = true` |
| `TAXABLE_GROSS` | Σ earning taxable − potongan absensi (§10) |
| `<component code>` | Hasil komponen lain di stage yang **lebih awal** |

### 2.4 Urutan evaluasi & validasi dependensi

Pipeline tetap (ADR-P03). Di dalam satu stage, komponen `PERCENT_OF` yang mereferensikan komponen lain diurutkan dengan **topological sort**.

| Validasi | Kapan | Hasil |
|---|---|---|
| Referensi ke komponen yang tidak ada / `INACTIVE` | Saat simpan konfigurasi | 422 `INVALID_COMPONENT_REFERENCE` |
| Siklus (`A = 10% × B`, `B = 5% × A`) | Saat simpan (deteksi siklus di graf) | 422 `COMPONENT_CYCLE` |
| Referensi ke stage yang lebih akhir (misalnya earning = % dari pajak) | Saat simpan | 422 `INVALID_COMPONENT_REFERENCE` (katalog kontrak API; final review) |
| Kondisi yang sama tetapi lolos (misalnya komponen dinonaktifkan setelah di-assign) | Saat kalkulasi, defensif | Item `ERROR` dengan kode yang sama (§13) |

Validasi ganda ini disengaja: validasi saat simpan memberi UX yang baik, sedangkan validasi defensif di engine memastikan data lama/korup tidak menghasilkan angka diam-diam.

---

## 3. Rounding

| Aturan | Keputusan |
|---|---|
| Titik pembulatan | **Sekali per baris komponen**, tepat sebelum baris ditambahkan ke hasil |
| Unit | IDR, 0 desimal |
| Mode default | `HALF_UP`; per komponen bisa `DOWN` (contoh: potongan pajak di contoh §15) atau `UP` |
| Hasil antara | **Tidak** dibulatkan: prorata, tarif per jam, dan base `PERCENT_OF` memakai presisi penuh |
| Base dari baris lain | `PERCENT_OF` atas komponen lain memakai **nilai baris yang sudah dibulatkan** (yang tampil di slip), agar karyawan bisa memverifikasi sendiri |
| Net | `Σ earning lines − Σ deduction lines` atas nilai yang sudah dibulatkan, sehingga eksak dan tanpa pembulatan lagi |
| Total run | `Σ net` item (PI-05) |

**Konsekuensi yang diterima:** total bisa berbeda ±Rp1 per baris dari nilai "eksak teoretis" (lihat §16). Sebagai gantinya, setiap slip **dapat dijumlahkan sendiri oleh karyawan**, dan invariant Σ selalu eksak.

---

## 4. Payroll period, cut-off, kalender, timezone

| Konsep | Definisi |
|---|---|
| Periode | Bulan kalender, half-open `[tanggal 1, tanggal 1 bulan berikutnya)`. Periode non-kalender (21–20) **di luar cakupan V1** |
| Timezone | Timezone tenant (default `Asia/Jakarta`). Semua "tanggal" di engine adalah **tanggal lokal**; timestamp disimpan UTC dan dikonversi oleh InputProvider, bukan engine |
| Hari kerja | Dari **kalender kerja tenant**: hari kerja mingguan (default Senin–Jumat) dikurangi hari libur yang dicatat tenant (tabel libur per tenant, diisi manual/seed) |
| Cut-off | `period.cutoff_at` (default: hari ke-2 bulan berikutnya, 23:59 waktu tenant; dapat dikonfigurasi). Cuti/lembur masuk jika `APPROVED` dengan `decided_at ≤ cutoff_at` **dan** tanggalnya di dalam periode (PI-08) |
| Lewat cut-off | Lembur Maret yang di-approve setelah cut-off **tidak** masuk run Maret. HR membuat adjustment ke April (§11) |

Cut-off sengaja ditentukan oleh data periode, bukan oleh "waktu job berjalan", agar kalkulasi ulang pada hari berbeda tetap menghasilkan input yang sama (determinisme, PI-12).

---

## 5. Employee status & eligibility (PI-07)

| Kasus | Aturan |
|---|---|
| Aktif penuh | Masuk run, `employment_factor = 1` |
| Joiner (masuk tanggal 10) | Masuk run. Span kerja `[10, akhir periode)`; gaji pokok dan `FIXED` diprorata |
| Leaver (tanggal keluar 20 → aktif sampai 19) | Masuk run terakhirnya dengan span `[awal periode, 20)`. Tidak masuk run setelahnya |
| Masuk dan keluar di periode yang sama | Satu span `[masuk, keluar)` |
| Rehire (keluar Januari, masuk lagi Maret) | `EmploymentSpan` lebih dari satu; engine menjumlahkan semua span yang beririsan dengan periode |
| Tidak aktif satu hari pun di periode | **Tidak** masuk run (bukan item 0) |
| Aktif tetapi tanpa salary assignment yang berlaku | Masuk run sebagai item `ERROR NO_SALARY` (§13), bukan dilewati diam-diam |

Eligibility ditentukan oleh job `payroll.plan` (daftar employee per chunk) **dan** diverifikasi ulang oleh engine dari `Employment` di input. Jika keduanya tidak sepakat (misalnya tanggal keluar diubah setelah plan), item menjadi `ERROR NOT_ELIGIBLE` dan fingerprint run menjadi stale (lihat PFS-03).

---

## 6. Proration

### 6.1 Metode

| Metode | Rumus faktor | Kelebihan | Kekurangan |
|---|---|---|---|
| **Hari kerja (default)** | `hari kerja aktif / hari kerja periode` | Sejalan dengan potongan absensi per hari kerja; joiner di akhir pekan tidak "dibayar" untuk hari libur | Nilai per hari berbeda antar bulan (Feb vs Mar) |
| Hari kalender | `hari kalender aktif / hari kalender periode` | Sederhana, mudah dipahami | Joiner Senin vs Sabtu menghasilkan angka yang terasa aneh |
| Basis tetap 30 hari | `hari aktif / 30` | Nilai per hari konstan | Februari bisa > 100%; butuh aturan cap |

**Keputusan:** default **hari kerja**, sebagai parameter `proration_basis` di ruleset tenant. Hari kalender juga didukung (contoh di ADR-P04 memakai hari kalender), sedangkan basis 30 hari di luar cakupan V1. Metode tersimpan di snapshot, jadi run lama tetap reproducible walaupun tenant mengganti metode.

### 6.2 Multi-segmen (perubahan gaji di tengah periode)

```text
segments = irisan( EmploymentSpan × SalarySpan × periode )
BASE_SALARY = Σ_segment  salary_segment × (hari_kerja_segment / hari_kerja_periode)
```
Setiap segmen menghasilkan **satu baris** (`BASE_SALARY` segmen 1, segmen 2) dengan explanation masing-masing, dan dibulatkan per baris. Lihat contoh lengkap di §15.

---

## 7. Overtime

| Aturan | Nilai (V1) |
|---|---|
| Sumber | `OvertimeRequest` berstatus `APPROVED`, tanggal di periode, `decided_at ≤ cutoff_at` |
| Tarif per jam | `overtime_basis / overtime_divisor`. Default `overtime_basis = BASE` (gaji pokok yang berlaku **pada tanggal lembur**), `overtime_divisor = 173`. **Simplified; verify against regulation.** Basis alternatif `BASE_PLUS_FIXED` tersedia di ruleset |
| Tier | Per hari lembur: jam ke-1 × 1,5; jam ke-2 dst × 2 (contoh, dari ruleset). Tier hari libur di luar cakupan V1 |
| Batas | `max_hours_per_day` / `max_hours_per_week` dari konfigurasi → **warning** `OVERTIME_OVER_LIMIT`, bukan error (kecuali tenant memilih mode keras di level request) |
| Baris slip | Satu baris `OVERTIME`, explanation memuat rincian per hari |

---

## 8. Absence

| Jenis | Sumber | Efek |
|---|---|---|
| Cuti dibayar | `LeaveRequest APPROVED`, jenis `paid = true` | Tidak ada potongan; dihitung sebagai hari kerja **tidak hadir** untuk `PER_DAY(PRESENT)` |
| Cuti tidak dibayar | `LeaveRequest APPROVED`, jenis `paid = false` | Baris `ABSENCE_UNPAID_LEAVE` = `hari × daily_rate` |
| Absen tanpa keterangan | Hari kerja tanpa attendance, tanpa cuti, setelah HR meninjau (`INCOMPLETE` yang tidak dikoreksi dihitung sesuai kebijakan tenant) | Baris `ABSENCE_UNEXCUSED` = `hari × daily_rate` |
| Libur tenant / akhir pekan | Kalender | Tidak dihitung |

`daily_rate = salary_segment_hari_itu / hari_kerja_periode`, sehingga konsisten dengan prorata hari kerja: satu hari absen memotong tepat satu hari dari gaji pokok segmen tersebut.

Kebijakan untuk catatan `INCOMPLETE` (lupa clock out) adalah parameter tenant: `COUNT_AS_PRESENT` (default) atau `COUNT_AS_ABSENT`. Catatan yang masih `INCOMPLETE` saat kalkulasi menghasilkan warning `INCOMPLETE_ATTENDANCE`.

---

## 9. Deductions — BPJS (statutory, contoh)

| Komponen (contoh) | Tipe | Base | Rate karyawan (CONTOH) | Cap base (CONTOH) |
|---|---|---|---|---|
| `BPJS_JHT_EE` | `PERCENT_OF` | `BPJS_WAGE` | 2% | — |
| `BPJS_JP_EE` | `PERCENT_OF` | `BPJS_WAGE` | 1% | Rp10.000.000 |
| `BPJS_KES_EE` | `PERCENT_OF` | `BPJS_WAGE` | 1% | Rp12.000.000 |

- Rate dan cap berasal dari **ruleset ber-versi** (effective-dated), bukan dari komponen tenant. Tenant hanya mengaktifkan/menonaktifkan.
- Porsi perusahaan (employer share) **tidak** memotong net. Di V1 tidak dihitung; di V2 bisa ditambahkan sebagai baris informasi `EMPLOYER_COST`.
- Pinjaman/kasbon karyawan **di luar cakupan V1**. Potongan ad-hoc memakai adjustment negatif.

---

## 10. Tax — PPh21 versi simplified

> **Simplified — not compliance-certified.** PayRun tidak mengklaim menghitung PPh21 yang sah. Tujuannya mendemonstrasikan *mekanisme* ruleset ber-versi; tarif di bawah ini placeholder.

| Aspek | Pendekatan V1 |
|---|---|
| Model | Tarif efektif bulanan per kategori `TaxProfile` (diturunkan dari status PTKP karyawan) |
| Base | `TAXABLE_GROSS = Σ earning lines dengan taxable = true − Σ baris ABSENCE` |
| Tabel | `rulesets.tax_brackets[category]`: daftar `(lower, upper, rate)`, satu rate berlaku untuk seluruh base (bukan progresif per lapis) |
| Pembulatan | Dikonfigurasi di ruleset (contoh di §15: `DOWN`) |
| Di luar cakupan | Rekonsiliasi tahunan (masa pajak Desember), tunjangan pajak (gross-up), NPWP vs non-NPWP, pajak bonus/THR terpisah |

Contoh tabel kategori `A` (**PLACEHOLDER, bukan angka resmi**):

| Base dari | Base sampai | Rate |
|---|---|---|
| 0 | 5.400.000 | 0% |
| 5.400.000,01 | 7.000.000 | 1% |
| 7.000.000,01 | 7.500.000 | 1,5% |
| 7.500.000,01 | 9.000.000 | 2% |
| … | … | … |

---

## 11. Adjustment

| Aturan | Nilai |
|---|---|
| Asal | Koreksi untuk periode yang sudah `FINALIZED` (lembur kurang, potongan salah, bonus manual) |
| Bentuk | `amount` ± (atau jam/hari yang dikonversi oleh InputProvider dengan tarif **periode asal**, sehingga hasilnya tetap jumlah rupiah yang eksplisit) |
| Masuk ke | Run berikutnya yang belum final untuk employee tersebut (`target run`) |
| Baris slip | `ADJUSTMENT` dengan explanation "Koreksi lembur periode Maret 2026 (ADJ-…)" |
| Pajak | Flag `taxable` per adjustment; jika taxable, ikut `TAXABLE_GROSS` periode target (simplified) |
| Status | `PENDING_APPLY → APPLIED` saat run target `FINALIZED`; `CANCELLED` sebelum itu (PI-15) |
| Efek pada fingerprint | Adjustment baru/batal untuk run yang sudah dikalkulasi → run *stale* (ADR-P07) |

---

## 12. Approval, recalculation, locking & finalized payroll

### 12.1 Siapa melakukan apa

| Aksi | Role | Guard |
|---|---|---|
| Create run, calculate, recalculate, submit | `PAYROLL_ADMIN`, `OWNER` | State run (ADR-P05) |
| Approve / reject | `PAYROLL_ADMIN`/`OWNER` **selain** submitter (pengecualian satu admin: ADR-P09) | `approval_requests.status = 'PENDING'` + fingerprint (PI-11, PI-13) |
| Finalize | `PAYROLL_ADMIN`, `OWNER`; membership dicek ulang di DB | `APPROVED` + fingerprint sekali lagi |
| Exclude employee dari run | `PAYROLL_ADMIN`, `OWNER`, alasan wajib, ter-audit | Hanya saat `CALCULATED` |

### 12.2 Recalculation

| Kondisi | Boleh recalculate? |
|---|---|
| `DRAFT` | Ya (kalkulasi pertama) |
| `CALCULATED` / `FAILED` | Ya, `calc_version += 1` |
| `PENDING_APPROVAL` | Tidak; reject atau tarik submit dulu |
| `APPROVED` | Tidak; finalize, atau (V1) cancel lalu buat run baru |
| `FINALIZED` / `CANCELLED` | Tidak pernah |

Kapan recalculation **wajib**: ketika guard fingerprint mengembalikan `409 RUN_STALE` saat submit/approve/finalize.

### 12.3 Apa yang membeku saat `FINALIZED`

| Yang membeku | Mekanisme |
|---|---|
| `PayrollItem`, `PayrollLine`, total run | Trigger menolak UPDATE/DELETE untuk run `FINALIZED` (PI-04) |
| `input_snapshot` (EmployeePeriodInput lengkap, termasuk kalender, cut-off, metode prorata) + `ruleset_version` + `snapshot_version` | Ditulis saat item dihitung, ikut membeku |
| Input operasional periode (attendance, leave, overtime, salary span yang beririsan) | Periode `FINALIZED` → `PERIOD_LOCKED` (PI-14) |
| Adjustment yang diterapkan | `APPLIED`, immutable |

**Reproducibility (PI-12):** `Calculate(item.input_snapshot, ruleset(item.ruleset_version)) == item.result` untuk setiap item, dibuktikan oleh test dan perintah `payrun verify-run <id>`.

---

## 13. Validation errors vs warnings

| Kode | Jenis | Contoh penyebab | Efek |
|---|---|---|---|
| `NO_SALARY` | ERROR | Employee aktif tanpa salary assignment di periode | Item `ERROR`, tanpa baris |
| `COMPONENT_REF_MISSING` | ERROR | `PERCENT_OF` ke komponen nonaktif | Item `ERROR` |
| `COMPONENT_CYCLE` | ERROR | Data lama dengan siklus | Item `ERROR` |
| `NEGATIVE_NET` | ERROR | Potongan + adjustment negatif > earning | Item `ERROR`, baris tetap dihitung untuk diagnosis |
| `NOT_ELIGIBLE` | ERROR | Plan dan input tidak sepakat soal eligibility | Item `ERROR` |
| `RULESET_MISSING` | ERROR | Tidak ada ruleset yang berlaku di periode | Item `ERROR` |
| `ZERO_BASE_SALARY` | WARNING | Gaji pokok 0 | Item `OK` + warning |
| `OVERTIME_OVER_LIMIT` | WARNING | Lembur > batas konfigurasi | Item `OK` + warning |
| `INCOMPLETE_ATTENDANCE` | WARNING | Ada catatan `INCOMPLETE` | Item `OK` + warning |
| `NET_CHANGE_ABOVE_THRESHOLD` | WARNING | Net berubah > 20% dari bulan lalu (PF-23) | Item `OK` + warning |

**Aturan run:** item `ERROR` **tidak** menggagalkan run (run tetap `CALCULATED`), tetapi `submit` ditolak `422 RUN_HAS_ERRORS` sampai setiap item error diperbaiki (lalu recalculate) atau dikecualikan secara eksplisit. Warning tidak memblokir apa pun, tetapi ditampilkan di review dan dicatat di item.

**Net negatif (keputusan V1):** `NEGATIVE_NET` adalah ERROR. Carry-over sisa potongan ke bulan berikutnya **tidak didukung** di V1. Alasannya: carry-over adalah utang karyawan kepada perusahaan, butuh persetujuan dan pelacakan saldo, dan itu di luar domain V1. HR memperbaiki penyebabnya (misalnya memecah adjustment ke beberapa periode) atau mengecualikan karyawan.

---

## 14. Data structures (Go-ish)

```go
// package payroll/engine — pure: tidak mengimpor db, http, time.Now, math/rand
func Calculate(in EmployeePeriodInput, rs Ruleset) PayrollResult

type EmployeePeriodInput struct {
    SnapshotVersion int
    EmployeeID      uuid.UUID
    Period          DateRange            // [2026-03-01, 2026-04-01)
    Calendar        WorkCalendar         // hari kerja per tanggal (sudah dikurangi libur tenant)
    CutoffAt        time.Time            // informasi; filter dilakukan InputProvider
    Employment      []DateRange          // satu atau lebih span (rehire)
    Salary          []SalarySpan         // {Range, BaseSalary decimal}
    Components      []ComponentAssignment // {Def ComponentDefinition, Amount/Rate decimal, Range}
    Attendance      []DayStatus          // per tanggal: PRESENT|ABSENT_UNEXCUSED|PAID_LEAVE|UNPAID_LEAVE|INCOMPLETE
    Overtime        []OvertimeEntry      // {Date, Hours decimal} — hanya APPROVED ≤ cut-off
    Adjustments     []AdjustmentInput    // {ID, OriginPeriod, Amount decimal, Taxable bool, Reason}
    TaxProfile      TaxProfile           // {Category string} — simplified
    Policy          TenantPolicy         // proration_basis, overtime_basis, incomplete_policy
}

type Ruleset struct {
    Version         string               // "ID-SIMPLIFIED-2026.1"
    OvertimeDivisor decimal.Decimal      // 173 (contoh)
    OvertimeTiers   []Tier               // {FromHour, Multiplier}
    Statutory       []StatutoryRule      // BPJS: {Code, Rate, CapMax, Rounding}
    TaxBrackets     map[string][]Bracket // per kategori
    TaxRounding     RoundingMode
}

type PayrollResult struct {
    Status     ItemStatus     // OK | ERROR
    Lines      []Line
    Gross      decimal.Decimal // Σ EARNING lines
    Deductions decimal.Decimal // Σ DEDUCTION lines
    Net        decimal.Decimal // Gross − Deductions
    Errors     []Issue
    Warnings   []Issue
}

type Line struct {
    Code        string          // "BASE_SALARY", "OVERTIME", "BPJS_JHT_EE", …
    Category    Category        // EARNING | DEDUCTION
    Stage       Stage
    Amount      decimal.Decimal // sudah dibulatkan
    Basis       map[string]string // data terstruktur: {"rate":"0.02","base":"6800000"} — untuk audit/UI
    Explanation string          // "2% × upah BPJS Rp6.800.000"
    Taxable     bool
}
```

Catatan desain:
- `Basis` dan `Explanation` adalah bagian dari hasil, bukan dekorasi UI. Slip gaji dirender langsung dari `Lines`, sehingga yang dilihat karyawan sama persis dengan yang dihitung.
- Engine tidak mengembalikan `error` Go untuk masalah data; masalah data menjadi `Errors` di item. `panic` (bug) ditangkap oleh job runner dan diperlakukan sebagai kegagalan sistem yang di-retry (lihat PFS-05).
- Linter CI melarang `float64` dan `time.Now` di package `payroll/engine`.

---

## 15. Worked example lengkap — Tono, Maret 2026

### 15.1 Asumsi

| Item | Nilai |
|---|---|
| Periode | Maret 2026 = `[2026-03-01, 2026-04-01)`; 1 Maret 2026 adalah hari Minggu |
| Kalender tenant (contoh) | Senin–Jumat = 22 hari; libur tenant (contoh) 19 dan 20 Maret → **20 hari kerja** |
| Metode prorata | Hari kerja |
| Status | Aktif sepanjang periode |
| Gaji pokok | Rp6.000.000 `[2026-01-01, 2026-03-15)`; Rp6.600.000 `[2026-03-15, ∞)` |
| Hari kerja per segmen | Segmen 1 `[1, 15)`: 2–6 & 9–13 Maret = **10**; segmen 2 `[15, 1 Apr)`: 16–18, 23–27, 30–31 Maret = **10** (19–20 libur) |
| Tunjangan jabatan | `FIXED` Rp500.000, taxable, `bpjs_base = true` |
| Uang transport | `PER_DAY(PRESENT)` Rp20.000, taxable, `bpjs_base = false` |
| Cuti tidak dibayar | 1 hari: Rabu 25 Maret (segmen 2), `APPROVED` sebelum cut-off |
| Hadir | 20 hari kerja − 1 cuti tidak dibayar = **19 hari** (tidak ada absen lain) |
| Lembur (`APPROVED` ≤ cut-off) | Senin 23 Maret 4 jam; Kamis 26 Maret 2 jam; total 6 jam |
| Ruleset (CONTOH) | Divisor 173; tier jam ke-1 × 1,5, jam ke-2+ × 2; BPJS JHT 2%, JP 1%, Kes 1% (cap tidak tercapai); pajak kategori A tabel §10, pembulatan pajak `DOWN` |

### 15.2 Perhitungan per baris

| # | Code | Kategori | Perhitungan (presisi penuh) | Hasil antara | Dibulatkan |
|---|---|---|---|---|---|
| 1 | `BASE_SALARY` seg-1 | EARNING | 6.000.000 × 10/20 | 3.000.000 | **3.000.000** |
| 2 | `BASE_SALARY` seg-2 | EARNING | 6.600.000 × 10/20 | 3.300.000 | **3.300.000** |
| 3 | `TUNJ_JABATAN` | EARNING | 500.000 × 1 (aktif penuh) | 500.000 | **500.000** |
| 4 | `TRANSPORT` | EARNING | 20.000 × 19 hari hadir | 380.000 | **380.000** |
| 5 | `OVERTIME` | EARNING | tarif/jam = 6.600.000 / 173 = 38.150,289017…; jam berbobot = (1×1,5 + 3×2) + (1×1,5 + 1×2) = 7,5 + 3,5 = 11; 11 × 38.150,289017… | 419.653,179… | **419.653** (HALF_UP) |
| 6 | `ABSENCE_UNPAID_LEAVE` | DEDUCTION | 1 hari × (6.600.000 / 20) | 330.000 | **330.000** |
| 7 | `BPJS_JHT_EE` | DEDUCTION | 2% × BPJS_WAGE; BPJS_WAGE = baris 1+2+3 = 6.800.000 | 136.000 | **136.000** |
| 8 | `BPJS_JP_EE` | DEDUCTION | 1% × 6.800.000 | 68.000 | **68.000** |
| 9 | `BPJS_KES_EE` | DEDUCTION | 1% × 6.800.000 | 68.000 | **68.000** |
| 10 | `PPH21` | DEDUCTION | TAXABLE_GROSS = (1+2+3+4+5) − 6 = 7.599.653 − 330.000 = 7.269.653 → bracket 7.000.000,01–7.500.000 → 1,5% (CONTOH); 7.269.653 × 0,015 | 109.044,795 | **109.044** (DOWN) |

Explanation yang tersimpan, contoh baris 5: *"Lembur 6 jam (23 Mar 4j: 1×1,5 + 3×2; 26 Mar 2j: 1×1,5 + 1×2) = 11 jam berbobot × Rp38.150,29/jam (Rp6.600.000 / 173)"*.

### 15.3 Total dan pemeriksaan Σ

```text
Gross       = 3.000.000 + 3.300.000 + 500.000 + 380.000 + 419.653            = 7.599.653
Deductions  = 330.000 + 136.000 + 68.000 + 68.000 + 109.044                   =   711.044
Net         = 7.599.653 − 711.044                                             = 6.888.609

Cek PI-05:   Σ EARNING lines − Σ DEDUCTION lines = Net    ✔ (eksak, semua baris sudah bulat)
Cek determinisme: input_snapshot + "ID-SIMPLIFIED-2026.1" → selalu 6.888.609
```

### 15.4 Hal yang ditunjukkan contoh ini

- **Multi-segmen:** kenaikan gaji 15 Maret menghasilkan dua baris gaji pokok, bukan satu angka rata-rata yang tidak bisa dijelaskan.
- **Tarif lembur memakai gaji yang berlaku pada tanggal lembur** (segmen 2), bukan gaji awal bulan.
- **Potongan absensi memakai `daily_rate` segmen yang berlaku pada tanggal absen.**
- **`FIXED` tidak dipotong oleh absensi**, sedangkan `PER_DAY(PRESENT)` otomatis berkurang (19, bukan 20).
- **Mode pembulatan per komponen:** lembur `HALF_UP`, pajak `DOWN`.

---

## 16. Mini example — Σ dibulatkan ≠ dibulatkan Σ

Karyawan dengan `BPJS_WAGE` = 6.548.387 (gaji pokok prorata dari ADR-P04):

```text
                         eksak         dibulatkan (HALF_UP)
BPJS_JHT_EE  2%     130.967,74    →    130.968
BPJS_JP_EE   1%      65.483,87    →     65.484
BPJS_KES_EE  1%      65.483,87    →     65.484
                                       -------
Σ baris dibulatkan                     261.936
Dibulatkan dari Σ (4% × 6.548.387 = 261.935,48)  →  261.935     selisih Rp1
```

**Aturan yang dipakai:** potongan BPJS = **261.936**, yaitu jumlah baris yang tampil di slip. Kalau total dibulatkan terpisah (261.935), karyawan yang menjumlahkan tiga baris di slip akan menemukan selisih Rp1, dan invariant `net = Σ lines` (PI-05) rusak. Selisih terhadap nilai eksak dibatasi < Rp1 per baris dan didokumentasikan di halaman bantuan.

---

## 17. Katalog golden test (≥ 20 kasus)

File: `payroll/engine/testdata/cases/<nama>.json` → `<nama>.golden.json`. Setiap golden menyimpan baris lengkap, termasuk explanation.

| # | Nama kasus | Yang diuji |
|---|---|---|
| 1 | `full_month_basic` | Gaji pokok penuh, tanpa komponen lain |
| 2 | `tono_march_2026` | Contoh lengkap §15 |
| 3 | `joiner_mid_month_working_days` | Prorata joiner, basis hari kerja |
| 4 | `joiner_mid_month_calendar_days` | Prorata joiner, basis hari kalender |
| 5 | `joiner_on_weekend` | Joiner hari Sabtu: faktor dihitung dari Senin berikutnya |
| 6 | `leaver_mid_month` | Leaver, span berakhir sebelum tanggal keluar |
| 7 | `join_and_leave_same_month` | Satu span pendek |
| 8 | `rehire_two_spans` | Dua employment span dalam satu periode |
| 9 | `raise_mid_month_two_segments` | Dua baris gaji pokok |
| 10 | `two_raises_three_segments` | Tiga segmen |
| 11 | `overtime_tiers_single_day` | Tier 1,5× / 2× |
| 12 | `overtime_across_raise` | Lembur sebelum dan sesudah kenaikan memakai tarif berbeda |
| 13 | `overtime_over_limit_warning` | Warning, item tetap `OK` |
| 14 | `unpaid_leave_deduction` | Potongan per hari, segmen yang benar |
| 15 | `unexcused_absence_deduction` | Absen tanpa keterangan |
| 16 | `paid_leave_reduces_per_day_present` | `PER_DAY(PRESENT)` berkurang, gaji pokok tidak |
| 17 | `incomplete_attendance_policy_present` | Kebijakan `COUNT_AS_PRESENT` + warning |
| 18 | `incomplete_attendance_policy_absent` | Kebijakan `COUNT_AS_ABSENT` |
| 19 | `bpjs_cap_applied` | Base di atas cap JP |
| 20 | `bpjs_rounding_line_vs_total` | Contoh §16 |
| 21 | `tax_bracket_boundary` | Base tepat di batas bracket |
| 22 | `tax_rounding_down` | Mode `DOWN` |
| 23 | `percent_of_component_chain` | `PERCENT_OF` mereferensikan komponen lain (topological order) |
| 24 | `adjustment_positive_taxable` | Adjustment + masuk taxable gross |
| 25 | `adjustment_negative` | Adjustment negatif |
| 26 | `negative_net_error` | Item `ERROR NEGATIVE_NET` |
| 27 | `no_salary_error` | Item `ERROR NO_SALARY` |
| 28 | `missing_component_ref_error` | Item `ERROR COMPONENT_REF_MISSING` |
| 29 | `zero_base_salary_warning` | Warning `ZERO_BASE_SALARY` |
| 30 | `february_working_days` | Bulan pendek |

**Property test** (input acak, `rapid`): `Net == Σ earning − Σ deduction`; `Calculate(x) == Calculate(x)` (determinisme); prorata ∈ [0, 1]; tidak ada baris dengan lebih dari 0 desimal untuk IDR.

**Recompute test:** untuk setiap golden, serialisasi `EmployeePeriodInput` → JSON kanonik → deserialisasi → `Calculate` harus menghasilkan hasil identik. Ini membuktikan snapshot benar-benar cukup untuk reproduksi.
