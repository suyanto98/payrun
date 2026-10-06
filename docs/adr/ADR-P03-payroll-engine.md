# ADR-P03 — Payroll Engine: Pure Function + Parameterized Components + Versioned Rulesets

> Status: **Accepted** · Tanggal: 2026-10-01 · Berlaku untuk: **PayRun**

## Context / Problem

Setiap tenant punya struktur gaji berbeda: tunjangan tetap, tunjangan per hari hadir, uang makan, lembur dengan multiplier, potongan absen. Aturan pajak dan BPJS berubah dari waktu ke waktu. Engine harus:
- cukup fleksibel untuk mayoritas UMKM tanpa perubahan kode;
- **deterministik** (input + ruleset sama → output sama, BR11) agar hasil final bisa direproduksi (ADR-P07);
- bisa diuji tuntas tanpa database;
- bisa dijelaskan baris per baris di slip gaji.

## Options

| Opsi | Fleksibilitas | Keamanan / testability | Biaya implementasi |
|---|---|---|---|
| A. Formula hard-coded di Go per tenant | Rendah; perubahan butuh deploy | Mudah diuji | Rendah, tetapi tidak scale ke banyak tenant |
| **B. Tipe komponen ter-parameter (data) + pure function** | Menutup sebagian besar kebutuhan UMKM | Deterministik, diuji dengan golden file | Sedang |
| C. Formula DSL yang ditulis user (parser sendiri) | Tinggi | Butuh parser, sandbox, batas waktu eksekusi, validasi siklus dependensi | Tinggi |
| D. Embedded expression language (CEL / expr) | Tinggi | Sandbox sudah ada, tetapi error formula jadi masalah support | Sedang–tinggi |

## Decision

**B.** Engine adalah pure function tanpa I/O, tanpa jam sistem, dan tanpa akses DB:

```go
// package payroll/engine — tidak mengimpor db, http, time.Now
func Calculate(in EmployeeInput, rs Ruleset) (Result, error)

type EmployeeInput struct {
    EmployeeID   uuid.UUID
    Period       Period            // [start, end)
    Employment   []EmploymentSpan  // aktif dari–sampai (joiner/leaver)
    Salary       []SalarySpan      // effective-dated (ADR-P04)
    Components   []ComponentAssignment
    Attendance   AttendanceSummary // hari kerja, hadir, absen tanpa keterangan
    UnpaidLeave  decimal.Decimal   // hari
    Overtime     []OvertimeEntry   // jam APPROVED sebelum cut-off
    Adjustments  []Adjustment      // koreksi periode sebelumnya
    TaxProfile   TaxProfile        // status PTKP (disederhanakan)
}
type Result struct {
    Lines    []Line          // komponen, kode, jumlah, dasar perhitungan
    Gross, Deductions, Net decimal.Decimal
    Warnings []Warning       // gaji 0, lembur > batas, data kurang
}
```

### Tipe komponen

| Tipe | Rumus | Contoh |
|---|---|---|
| `FIXED` | `amount × proration` | Tunjangan jabatan Rp1.000.000 |
| `PERCENT_OF` | `pct × base(component_ref)`, opsional `cap_min`/`cap_max` | BPJS JHT karyawan 2% × gaji pokok |
| `PER_DAY` | `rate × days(source)`; `source` = hadir / hari kerja | Uang makan Rp25.000 × 20 hari hadir |
| `PER_HOUR_MULTIPLIER` | `hourly_rate × hours × multiplier(tier)` | Lembur 1,5× jam pertama, 2× jam berikutnya |

Setiap komponen punya `kind` (EARNING / DEDUCTION), `taxable`, `rounding` (ADR-P06), dan urutan evaluasi yang ditentukan oleh pipeline, bukan user.

### Pipeline (urutan tetap)

```text
1. Proration      → faktor aktif per span (joiner/leaver, perubahan gaji tengah periode)
2. Earnings       → gaji pokok + FIXED + PER_DAY earning
3. Overtime       → PER_HOUR_MULTIPLIER dari lembur APPROVED
4. Absence        → potongan absen tanpa keterangan + cuti tidak dibayar
5. BPJS           → PERCENT_OF dengan cap dari ruleset
6. Tax (PPh21)    → dari penghasilan bruto kena pajak, tarif dari ruleset
7. Adjustments    → koreksi periode lalu (+/−)
8. Net            → Σ earnings − Σ deductions (dari baris yang sudah dibulatkan)
```

### Ruleset ber-versi

Tarif pajak, persentase dan cap BPJS, serta multiplier lembur default disimpan sebagai **data effective-dated** (`rulesets` dengan `version`, `valid_from`). Run mencatat versi ruleset yang dipakai. Semua ruleset diberi label **"simplified — not compliance-certified"** di UI dan README. Angka diverifikasi sebelum ditulis di dokumentasi publik.

### Testing

Golden-file tests: `testdata/cases/*.json` (input + ruleset) → `*.golden.json` (hasil), mencakup prorata joiner/leaver, kenaikan gaji tengah periode, lembur bertingkat, cuti tidak dibayar, cap BPJS, adjustment negatif, dan pembulatan. Property test: `Net == Σ earnings − Σ deductions` untuk input acak. Detail di Step 8.

## Reason

- Pure function berarti bagian paling berisiko dari sisi correctness bisa diselesaikan dan diuji **sebelum** ada DB atau UI (urutan slice di Step 1 §5.2).
- Determinisme adalah syarat reproducibility dan snapshot (ADR-P07).
- Komponen sebagai data memberi fleksibilitas per tenant tanpa membuka permukaan serangan berupa evaluasi kode buatan user.

## Trade-offs

- Struktur gaji yang tidak bisa diekspresikan dengan 4 tipe (contoh: bonus berbasis target penjualan) harus diinput sebagai adjustment manual.
- Urutan pipeline tetap; tenant tidak bisa membuat komponen yang bergantung pada hasil pajak.
- Aturan pajak yang disederhanakan bukan representasi lengkap regulasi.

## Future Migration

| Pemicu | Langkah |
|---|---|
| Banyak tenant meminta formula kustom | Tambah tipe `EXPRESSION` memakai **CEL** (sandboxed, non-Turing-complete, ada cost limit), dengan validasi saat simpan dan preview hasil. Bukan parser buatan sendiri |
| Kepatuhan pajak sungguhan diperlukan | Modul tax terpisah dengan test vector resmi, atau integrasi layanan pihak ketiga |
| Kalkulasi > 100k karyawan per run | Engine sudah pure, tinggal diparalelkan per chunk (ADR-P05) |

## Interview angle

- **"Kenapa tidak membiarkan user menulis formula?"** → Butuh parser, sandbox, batas eksekusi, deteksi siklus, dan support untuk formula yang salah. Empat tipe ter-parameter sudah menutup kebutuhan target user. Kalau formula benar-benar dibutuhkan, saya memakai CEL, bukan membuat bahasa sendiri.
- **"Bagaimana memastikan hasil hari ini sama dengan hasil 6 bulan lagi?"** → Pure function, versi ruleset dicatat, input di-snapshot, dan ada test recompute-from-snapshot.

## Related

ADR-P04 (effective dating), ADR-P05 (eksekusi run), ADR-P06 (pembulatan), ADR-P07 (snapshot), Step 1 `02-payrun.md` §4.
