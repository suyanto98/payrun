# ADR-P04 — Effective Dating dengan `daterange` + Exclusion Constraint

> Status: **Accepted** · Tanggal: 2026-10-01 · Berlaku untuk: **PayRun**

## Context / Problem

Gaji naik per 15 Maret, karyawan pindah departemen per 1 April, tunjangan dihentikan per 1 Mei. Payroll Maret harus dihitung dengan **gaji yang berlaku per tanggal**: setengah bulan dengan gaji lama, setengah bulan dengan gaji baru. Menimpa nilai lama menghapus riwayat dan membuat payroll periode lalu tidak bisa dihitung ulang atau diaudit (BR9).

Masalah turunannya:
- dua record yang tumpang tindih membuat "gaji yang berlaku" ambigu;
- celah (gap) antar record membuat karyawan tanpa gaji di tanggal tertentu;
- konvensi batas akhir inklusif vs eksklusif menimbulkan bug off-by-one.

## Options

| Opsi | Cara | Kelebihan | Kekurangan |
|---|---|---|---|
| A. Kolom nilai saat ini + tabel history (audit log) | Update di tempat, history disalin | Query "saat ini" sederhana | Query "per tanggal" sulit; history bisa tidak sinkron |
| B. `valid_from` / `valid_to` kolom date + validasi di aplikasi | Record baru per perubahan | Mudah dipahami | Overlap hanya dicegah aplikasi; race dua request bisa lolos |
| **C. `daterange` half-open + `EXCLUDE USING gist`** | DB menolak overlap | Invariant ditegakkan DB, aman dari race | Butuh extension `btree_gist`; konsep lebih asing |
| D. Bitemporal penuh (valid time + transaction time) | Dua sumbu waktu | Bisa menjawab "apa yang kita ketahui pada tanggal X" | Kompleks; kebutuhan "known-as-of" sudah ditutup snapshot (ADR-P07) |

## Decision

**C.** Rentang **half-open `[valid_from, valid_to)`**, dengan `valid_to = NULL` (tak terbatas) untuk record yang masih berlaku.

```sql
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE employee_salaries (
  id           uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL,
  employee_id  uuid NOT NULL,
  base_salary  numeric(18,2) NOT NULL CHECK (base_salary >= 0),
  valid_during daterange NOT NULL CHECK (NOT isempty(valid_during)),
  created_by   uuid NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, employee_id) REFERENCES employees (tenant_id, id),
  EXCLUDE USING gist (
    tenant_id WITH =, employee_id WITH =, valid_during WITH &&
  )
);
```

**Query "berlaku per tanggal":**
```sql
SELECT base_salary FROM employee_salaries
WHERE employee_id = $1 AND valid_during @> $2::date;
```

**Query untuk periode (semua span yang beririsan):**
```sql
SELECT base_salary, valid_during * daterange($start, $end) AS span
FROM employee_salaries
WHERE employee_id = $1 AND valid_during && daterange($start, $end);
```

**Perubahan** dilakukan dalam satu transaksi: tutup record lama (`valid_during = daterange(lower, $new_from)`) lalu INSERT record baru `[$new_from, NULL)`. Exclusion constraint memastikan tidak ada dua request paralel yang sama-sama berhasil membuat overlap.

### Contoh prorata: kenaikan gaji 15 Maret (basis **hari kalender**; default engine = hari kerja, lihat `07-payroll-engine.md` §6)

```text
Record 1: Rp 6.000.000, [2026-01-01, 2026-03-15)
Record 2: Rp 7.000.000, [2026-03-15, ∞)
Periode Maret = [2026-03-01, 2026-04-01), 31 hari kalender (basis prorata = konfigurasi tenant)

Span 1: 14 hari × 6.000.000/31 = 2.709.677,419…
Span 2: 17 hari × 7.000.000/31 = 3.838.709,677…
Gaji pokok Maret = 6.548.387,096… → dibulatkan per komponen (ADR-P06) → Rp 6.548.387
```
Basis prorata (hari kalender vs hari kerja) adalah parameter ruleset tenant, bukan hard-code.

### Apa yang effective-dated

| Entitas | Effective-dated? | Alasan |
|---|---|---|
| Gaji pokok | ✅ | Input payroll, berubah di tengah periode |
| Assignment komponen ke karyawan (tunjangan, potongan tetap) | ✅ | Bisa mulai/berhenti di tengah periode |
| Posisi & departemen karyawan | ✅ | Laporan per departemen dan approver historis |
| Atasan langsung | ✅ | Approver per tanggal request |
| Ruleset pajak/BPJS | ✅ (level tenant/platform) | Regulasi berubah per tanggal |
| Status kerja (aktif, resign) | ✅ sebagai `employment_spans` | Joiner/leaver |
| Data pribadi (nama, alamat, rekening) | ❌ (cukup audit log) | Tidak memengaruhi perhitungan historis |
| Request cuti/lembur, absensi | ❌ | Sudah merupakan event bertanggal |

## Reason

- Invariant "tepat satu record berlaku per tanggal" ditegakkan **database**, termasuk di bawah concurrency.
- Half-open menghilangkan bug off-by-one: span berurutan bersambung tanpa celah atau overlap (`[a,b)` + `[b,c)`).
- Operator range Postgres (`@>`, `&&`, `*`) membuat query per tanggal dan per periode ringkas.

## Trade-offs

- Gap tidak dicegah oleh exclusion constraint; dicegah dengan validasi aplikasi + warning saat kalkulasi ("tidak ada gaji berlaku pada tanggal X").
- Koreksi retroaktif (gaji Februari ternyata salah setelah Februari final) tidak mengubah run final; hasilnya adalah adjustment (ADR-P07).
- Developer perlu memahami tipe range Postgres; mapping di Go (pgtype.Range) sedikit verbose.

## Future Migration

| Pemicu | Langkah |
|---|---|
| Kebutuhan audit "apa yang diketahui sistem pada tanggal X" di luar payroll | Tambah sumbu transaction time (`recorded_during tstzrange`) → bitemporal |
| Volume history besar | Index GiST sudah mendukung; partisi bukan prioritas |

## Interview angle

- **"Kenapa half-open?"** → Span berurutan bisa disambung tanpa celah/overlap, dan durasi = `upper − lower` tanpa ±1.
- **"Dua HR mengubah gaji karyawan yang sama bersamaan?"** → Salah satu gagal di exclusion constraint (`23P01`), di-map ke 409 CONFLICT.

## Related

ADR-P03 (proration di engine), ADR-P06 (pembulatan), ADR-P07 (koreksi via adjustment), ADR-004.
