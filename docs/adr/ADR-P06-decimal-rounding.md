# ADR-P06 — Decimal & Rounding Policy

> Status: **Accepted** · Tanggal: 2026-10-01 · Berlaku untuk: **PayRun**

## Context / Problem

Payroll penuh dengan pecahan: prorata (`14/31`), persentase (2%, 1%, 4%), tarif per jam (`gaji/173`), multiplier lembur (1,5×), tarif pajak. Float (`float64`) tidak bisa merepresentasikan `0,1` secara eksak. Selisih Rp1 di satu karyawan merusak invariant "Σ item = total run" (BR4) dan kepercayaan HR.

Masalah yang lebih halus: **di mana pembulatan dilakukan**. Jumlah dari nilai yang dibulatkan tidak selalu sama dengan pembulatan dari jumlah.

### Kenapa berbeda dari LedgerPay (`BIGINT`, ADR-L04)

| | LedgerPay | PayRun |
|---|---|---|
| Operasi dominan | Penjumlahan/pengurangan nominal yang sudah eksak | Perkalian dan pembagian dengan rate, persentase, pecahan hari |
| Hasil antara | Selalu bilangan bulat minor unit | Pecahan tak terbatas (`6.000.000 × 14/31`) |
| Representasi tepat | Integer | Decimal presisi tinggi, dibulatkan di titik yang ditentukan |

Ledger hanya mencatat hasil akhir; payroll **menghasilkan** angka dari perhitungan. Karena itu tipe datanya berbeda.

## Options

| Opsi | Kelebihan | Kekurangan |
|---|---|---|
| A. `float64` | Cepat, bawaan | Tidak eksak; selisih pembulatan acak; **ditolak** |
| B. Integer sen + aritmetika manual | Eksak untuk penjumlahan | Pembagian dan persentase butuh aturan manual di setiap tempat; rawan bug |
| **C. `NUMERIC` di DB + `shopspring/decimal` di Go** | Eksak, mendukung pembagian presisi tinggi | Lebih lambat dari integer; perlu disiplin agar tidak tercampur float |
| D. `big.Rat` | Pecahan eksak tanpa batas | Ukuran tumbuh; konversi ke tampilan rumit; tidak ada di DB |

## Decision

**C**, dengan aturan berikut:

1. **Perhitungan antara dengan presisi penuh** (`decimal.Decimal`, pembagian dengan presisi ≥ 16 digit).
2. **Pembulatan sekali per baris komponen**, ke unit mata uang (IDR: 0 desimal), mode default **half-up**. Mode bisa dikonfigurasi per komponen (`HALF_UP`, `DOWN`, `UP`), misalnya potongan dibulatkan ke bawah demi karyawan.
3. **Net = Σ baris yang sudah dibulatkan**. Total run = Σ net. Invariant penjumlahan dengan demikian selalu eksak, karena semua yang dijumlahkan sudah dibulatkan.
4. **Penyimpanan:** jumlah uang `NUMERIC(18,2)` (desimal disediakan untuk kebutuhan masa depan, walau IDR dibulatkan ke 0); rate/persentase `NUMERIC(12,6)`; jam/hari `NUMERIC(8,2)`.
5. **Tidak ada float di jalur uang**: struct Go memakai `decimal.Decimal`; JSON API mengirim jumlah sebagai **string** (`"6548387.00"`) agar JavaScript tidak mengubahnya menjadi float. Linter (custom/grep CI) melarang `float64` di package `payroll`.

### Contoh: Σ dibulatkan ≠ dibulatkan Σ

```text
Tiga komponen dengan hasil antara:
  Tunjangan A (prorata)  = 333.333,33
  Tunjangan B (prorata)  = 333.333,33
  Tunjangan C (prorata)  = 333.333,34
  Σ eksak                = 1.000.000,00

Cara 1 — bulatkan tiap baris (half-up, 0 desimal):
  333.333 + 333.333 + 333.333 = 999.999   → yang tampil di slip, dan Σ cocok dengan net

Cara 2 — bulatkan totalnya saja:
  net = 1.000.000, tetapi baris di slip menampilkan 333.333 ×3 = 999.999
  → slip tidak bisa dijumlahkan oleh karyawan (selisih Rp1)
```

**Pilihan: Cara 1.** Konsistensi *slip dapat dijumlahkan sendiri oleh karyawan* lebih penting daripada selisih ±Rp1 terhadap nilai eksak teoretis. Selisih per baris dibatasi < Rp1 dan terdokumentasi.

## Reason

- Persentase dan prorata membutuhkan pembagian eksak dengan presisi tinggi, yang integer tidak memberikan secara alami.
- Pembulatan per baris membuat setiap angka di slip gaji bisa diverifikasi manual dan invariant Σ selalu benar.
- String di JSON menghilangkan kelas bug presisi di frontend.

## Trade-offs

- `shopspring/decimal` lebih lambat dari integer (tidak signifikan untuk ribuan karyawan; diukur di load test).
- Pembulatan per baris menghasilkan selisih kecil terhadap total eksak; perlu dijelaskan di dokumentasi.
- Disiplin tipe dibutuhkan di seluruh codebase (konversi ke/dari float dilarang).

## Future Migration

| Pemicu | Langkah |
|---|---|
| Multi-currency | Exponent per currency dari tabel ISO 4217; pembulatan ke exponent masing-masing |
| Aturan pajak mewajibkan pembulatan khusus (misalnya ke ribuan) | Mode pembulatan per komponen ditambah `NEAREST_1000` di ruleset |
| Integrasi dengan LedgerPay | Konversi net (dibulatkan) ke `BIGINT` minor unit di boundary, dengan assert tidak ada sisa pecahan |

## Interview angle

- **"Kenapa LedgerPay integer dan PayRun decimal?"** → Ledger hanya menjumlahkan nominal eksak; payroll menghasilkan nominal dari pembagian dan persentase.
- **"Total payroll selisih Rp1 dari kalkulasi Excel HR. Mana yang benar?"** → Tunjukkan aturan pembulatan per baris dan bahwa setiap slip dapat dijumlahkan; Excel biasanya membulatkan di akhir.
- **"Kenapa jumlah di JSON berupa string?"** → `Number` di JavaScript adalah float64; nilai besar dan desimal bisa berubah.

## Related

ADR-P03 (engine), ADR-P04 (prorata), ADR-L04 (representasi uang LedgerPay), ADR-003 (format API).
