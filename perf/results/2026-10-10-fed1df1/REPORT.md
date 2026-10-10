# PayRun — laporan load test Sprint 9 (subset V1: PR-1, PR-3, PR-4/E-2)

### Load test baseline — 2026-10-09/10, commit fed1df1 + working tree Sprint 7–9 (belum di-commit)

Lingkungan: **laptop pengembang**, bukan VM target. Intel i3-1315U / 8 thread / 14 GB RAM, dipakai bersama aplikasi
desktop, dev stack, dan **k6 di mesin yang sama** (melanggar §3 "k6 dari mesin terpisah"; CPU k6 ikut mencuri CPU
sistem yang diuji). Postgres 17.11 (`shared_buffers=1GB`, `work_mem=16MB`, `pg_stat_statements`), Go 1.26.6, k6 1.2.3.
Pool: api 20 / worker default. Dataset: 1.000 tenant, 100.000 karyawan (1 tenant 10.000), 300.000 baris absensi
(`make perf-seed`, gate invariant lolos). **Ini baseline laptop tersebut, bukan klaim kapasitas production.** Angka VM
2 vCPU / 4 GB diukur ulang setelah VM ada (deploy publik ditunda, keputusan pemilik 2026-10-04).

| Skenario | Beban | Hasil | p95 | Invariant | Catatan |
|---|---|---|---|---|---|
| PR-1 run 10.000 karyawan | 4 worker, chunk 50 | **±10 dtk** (≈ 1.000 karyawan/dtk) | — | ✅ Σ item = total, 1 item per karyawan | 1 → 2 → 4 worker: 13,8 → 10,3 → 10,0 dtk |
| PR-1 tenant 492 karyawan | 1 worker, chunk 50 | ±1,0 dtk | — | ✅ | SLO < 60 dtk / 1.000 karyawan, diukur langsung (tenant 1.000 tidak ada di seed; 492 yang terdekat) |
| PR-3 baca 100.000 karyawan (RLS) | open model 36 iterasi/dtk ≈ **227 req/dtk** (±50% titik jenuh), 5 mnt | lulus | **10–20 ms** per route | ✅ **0 baris lintas tenant** dari 74.164 respons | Threshold p95 < 300 ms lulus |
| PR-3 saturasi | closed 300 VU tanpa think time, 10 mnt | jenuh di **±450 req/dtk** | ±820 ms | ✅ 0 baris lintas tenant dari 285.358 | p95 = antrean (Little: 300 VU / 453 req/dtk ≈ 660 ms rata-rata) |
| PR-4 / E-2 approve | tenant 10.000, 20 pengulangan per variasi | change log ≤ 55 ms (p95) | lihat tabel E-2 | ✅ `RUN_STALE` tepat setiap kali ada perubahan | Keputusan: tetap sinkron |

## PR-1 — run 10.000 karyawan (`../2026-10-09-fed1df1/pr1-*`)

| Worker | Chunk | Rep 1 / 2 / 3 (dtk) |
|---|---|---|
| 1 | 50 | 14,1 / 14,5 / 12,9 |
| 2 | 50 | 10,4 / 10,3 / 10,4 |
| 4 | 25 | 13,5 / 12,8 / 12,1 |
| 4 | 50 | 10,1 / 9,7 / 10,2 |
| 4 | 100 | 9,0 / 10,6 / 9,7 |

**Temuan & tindakan (urut waktu, seri `pr1-t0-w4-c50`):**
1. 35 → 26 dtk: penulisan chunk menjadi tiga `INSERT … SELECT unnest(...)` (item, line, snapshot), bukan ±7 INSERT per
   karyawan. `COPY` tidak bisa dipakai: Postgres menolak `COPY FROM` ke tabel ber-RLS untuk role tanpa `BYPASSRLS`.
2. 26 → 10 dtk: bug job runner. `Runner.Run` tidur selama interval polling setiap kali semua slot penuh, sehingga
   throughput dibatasi ±concurrency job per interval berapa pun cepatnya job. Sekarang goroutine yang selesai memberi
   sinyal slot kosong (`TestJobRunner_FullCapacity_ClaimsAsSoonAsSlotFrees`; skeleton 0.0.11, **bug yang sama ada
   di LedgerPay**).
3. **Sisa bottleneck (tidak diperbaiki, sudah di bawah SLO):** skala worker datar setelah 2 karena setiap chunk
   memegang `LockRun … FOR UPDATE` selama txWrite (±28 ms tulis per chunk). Di 4 worker `LockRun` rata-rata 110 ms
   (hampir semuanya menunggu lock), di 2 worker 22 ms, di 1 worker tidak muncul di top query. Bagian serial ± 200 chunk
   × ±30–50 ms = batas Amdahl ±6–10 dtk. Opsi V2 bila diperlukan: tulis item tanpa lock run lalu fencing dengan
   `UPDATE … SET chunks_done = chunks_done + 1 WHERE status = 'CALCULATING' AND calc_version = $v` di akhir tx (lock
   hanya dari UPDATE sampai COMMIT). Ini mengubah argumen fencing (ADR-P05), jadi butuh ulang DoD S4 (zombie chunk,
   lease dicuri, versi usang).

Chunk 25 lebih lambat (400 chunk = 400× overhead claim + tx + lock); chunk 100 setara 50. Default tetap 50
(granularitas resume lebih halus saat worker crash, PFS-01).

## PR-3 — baca 100.000 karyawan dengan RLS

Campuran per iterasi (k6 `perf/k6/pr3_read.js`): 5 halaman cursor `GET /employees`, filter status + departemen,
prefix nama, `GET /attendance?date=` (limit 200). Setiap karyawan yang dikembalikan divalidasi: departemennya harus
milik tenant VU itu.

| Putaran | Kode | Throughput (closed 300 VU) | p95 | Top query (rata-rata di bawah beban) | CPU Postgres |
|---|---|---|---|---|---|
| `pr3-before` | asli | **37 req/dtk** | 11,4 dtk | `ListEmployeesByNo` 731 ms | ±700% |
| `pr3-page-first` | halaman dulu, LATERAL sesudahnya | 433 req/dtk | 840 ms | `ListEmployeesByNo` 35 ms | ±490% |
| `pr3-custom-plan` | + custom plan | 453 req/dtk | 820 ms | `ListEmployeesByNo` 16 ms | ±460% |
| `pr3-rate36` | + open model 227 req/dtk | 227 req/dtk (sesuai target) | **10–20 ms** | — | — |

**Temuan 1 — top-N sort setelah join semua karyawan (perbaikan terbesar, ±12× throughput).** `EXPLAIN (ANALYZE,
BUFFERS)` sebagai `payrun_app` (RLS aktif) dengan `PREPARE` + 6 eksekusi (pgx memakai prepared statement; mulai
eksekusi ke-6 Postgres boleh memakai generic plan): Postgres meng-join **10.000** karyawan tenant dengan LATERAL span
+ org + departemen + posisi, baru kemudian `top-N heapsort` untuk 51 baris. Hasilnya 110.194 buffer dan 240 ms per
halaman. Estimasi baris karyawan = 1 (pola `($n IS NULL OR kolom = $n)` di generic plan), sehingga planner tidak
memilih "index `uq_employees_employee_no`, berhenti di baris ke-51".
*Tindakan:* halaman dipilih dulu di subquery (`ORDER BY … LIMIT`, filter departemen menjadi `EXISTS`), LATERAL hanya
untuk ≤ 51 baris itu. Percobaan kedua menemukan jebakan lanjutan: LATERAL org tanpa `LIMIT` di-*pull up* planner
menjadi join biasa + `Materialize` seluruh 10.000 assignment (510.000 pasangan dibuang oleh join filter). `LIMIT 1`
(aman karena `ex_org_assignments_no_overlap`) membuat subquery tidak bisa diratakan. Hasil: 240 → 8 ms (generic plan).
Test regresi: filter departemen diterapkan sebelum LIMIT halaman (`TestSalary_EffectiveDatedChange`).

**Temuan 2 — generic plan tetap salah memilih index.** Generic plan memindai seluruh tenant lewat
`ix_employees_manager` lalu sort (8 ms); custom plan memakai `uq_employees_employee_no` dan berhenti di baris ke-52
(1,5 ms termasuk planning 0,8 ms). *Tindakan:* `db.CustomPlan(tx)` untuk daftar karyawan saja (unnamed statement,
`pgx.QueryExecModeCacheDescribe`), bukan `plan_cache_mode = force_custom_plan` untuk seluruh role (memaksa replanning
semua query). Test `TestCustomPlan_NeverLeavesNamedPreparedStatement` (lolos; gagal kalau wrapper dirusak). Efek di
bawah beban: waktu DB query ini turun separuh, throughput hanya +5%, karena laptop sudah jenuh secara keseluruhan
(Postgres + API + k6 berbagi 8 thread).

**Temuan 3 — metodologi.** Closed model 300 VU **tanpa think time** adalah uji saturasi: latency yang terukur adalah
waktu antre (hukum Little), bukan latency layanan. p95 < 300 ms dievaluasi dengan open model di ±50% titik jenuh
(`RATE=36 DURATION=5m make perf-run S=pr3`): p95 10–20 ms. Rencana §5.2 menyebut closed model; script tidak punya think
time, sehingga kedua angka dilaporkan dan diberi label.

**Belum diperbaiki (dicatat):** `ListEmployeesByName` (prefix) 22 ms rata-rata di bawah beban. `OR` antara
`lower(full_name) LIKE` dan `lower(employee_no) LIKE` mencegah pemakaian `ix_employees_name_prefix`; selain itu index
`text_pattern_ops` tidak bisa melayani `ORDER BY lower(full_name)` di collation non-C. Opsi V2: dua query + `UNION`,
atau pg_trgm (ditolak di V1). `ListRecords` absensi 16 ms (limit 200 baris).

## PR-4 / E-2 — biaya approve: change log vs recompute penuh (O12)

Tenant 10.000, run `PENDING_APPROVAL`; perubahan gaji lewat API (PeriodGuard mencatat change log), lalu approve diulang
20×. Approve yang menemukan perubahan di-rollback penuh dengan `409 RUN_STALE`, sehingga state tidak berubah dan bisa
diulang. Durasi = waktu respons HTTP `curl` (termasuk HTTP + auth), bukan span di dalam tx.

| Variasi | n | p50 | p95 | max | Respons |
|---|---|---|---|---|---|
| 0 perubahan | 1 | 11,3 ms | — | — | 200 APPROVED (state berubah → sekali saja) |
| 10 perubahan (change log) | 20 | 25,0 ms | 30,7 ms | 58,5 ms | 20× 409 RUN_STALE |
| 200 perubahan (batas verifikasi) | 20 | 44,3 ms | 54,7 ms | 60,9 ms | 20× 409 RUN_STALE |
| 1.000 perubahan (> 200 → stale tanpa verifikasi, Step 12 R1-7) | 20 | 6,4 ms | 10,5 ms | 22,2 ms | 20× 409 RUN_STALE |
| Recompute penuh 10.000 karyawan (`payrun perf-recheck`) | 3 | 1,18 dtk | — | 1,20 dtk | 1.000 berubah terdeteksi |

**Keputusan (aturan §6 E-2: pindah ke job async bila 1.000 perubahan > 2 dtk):** change log **dipertahankan sinkron**,
approve tetap satu langkah, tidak ada `PENDING_VERIFICATION`, tanpa amandemen arsitektur ADR-P07. Biaya approve linear
terhadap jumlah kandidat sampai 200 (±0,1 ms per kandidat di atas ±20 ms dasar), lalu konstan karena batas 200.
Recompute penuh ±25× lebih lambat daripada 200 kandidat dan **memegang lock periode `FOR UPDATE` selama itu** (di
produksi; alat ukur `perf-recheck` sendiri tanpa lock), sehingga semua penulis input PeriodGuard di tenant itu
tertahan ±1,2 dtk. Hipotesis "1.000 perubahan ≈ ratusan ms" tidak berlaku karena batas 200 sudah memotongnya.
PR-5 (dampak lock ke penulis) tidak dijalankan (opsional V1).

## Tidak dijalankan (opsional V1, amandemen Step 12 S7/R4-13)
PR-2 (1.000 run lintas tenant), PR-5, PR-6 (10.000 PDF), PR-7 (import 50.000 baris). PF-23 (diff vs bulan lalu) → P2.

## Cara mengulang
```bash
make perf-up && make perf-seed                       # sekali; data tetap di volume payrun-perf_pgdata
PR_PERF_WORKERS=4 PR_PERF_CHUNK=50 make perf-run S=pr1
make perf-run S=pr3                                  # saturasi, closed 300 VU, 10 mnt
RATE=36 DURATION=5m make perf-run S=pr3              # open model ±50% titik jenuh
make perf-run S=pr4                                  # E-2 (mengubah gaji 1.000 karyawan tenant seed-0)
make perf-down                                       # hapus env + data
```
Setiap direktori skenario berisi `env.txt` (mesin, versi, dataset), `run.log`, `docker-stats.csv` (CPU/RAM ±2 dtk),
`top-queries.txt` (`pg_stat_statements`), dan untuk PR-3 `k6.txt` + `k6-summary.json` (token dihapus).
