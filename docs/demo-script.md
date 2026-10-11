# Demo script PayRun (10 menit)

Semua langkah di sini dijalankan dengan asersi oleh `make demo-live` terhadap stack demo lokal; versi headless
(tanpa stack, test integrasi) adalah `make demo-verify` (nightly). Kalau satu janji tidak terpenuhi, skripnya berhenti
dengan `DEMO FAILED`. Pola setiap segmen: **masalah → mekanisme → bukti**.

Disesuaikan dari rencana demo script (`docs/design/33-demo-script.md`) dengan yang benar-benar dibangun: tenant demo dibuat
oleh `make seed-demo` (seed-0 = **Alpha**, 100 karyawan; seed-1 = **Beta**, 10–50 karyawan; password
`correct horse battery`), tidak ada toolbar demo di UI, fault dipasang lewat `POST /api/v1/demo/faults` dengan
`X-Demo-Token` (OWNER saja).

## Persiapan (10 menit sebelumnya)
| # | Langkah | Siap jika |
|---|---|---|
| 1 | `make demo-down && make up-demo && make seed-demo` (data demo dipakai sekali) | `seed demo OK: 2 tenants, 128 employees …` |
| 2 | `PR_HTTP_PORT=8082 PR_WEB_PORT=5176 make web-dev` → `http://localhost:5176` | Login `owner-0@seed.payrun.test` tampil |
| 3 | Dua jendela browser: Alpha (`owner-0@…`) dan Beta (`owner-1@…`); tab ketiga `payroll-0@…` | Daftar karyawan tampil di masing-masing |
| 4 | Terminal font besar: satu tab per demo (`scripts/demo-live.sh a`, `b`, `c`) | — |
| 5 | Cadangan: video (`[LINK]`) dan `make demo-verify` hijau | Bisa pindah < 10 detik |

## Alur

| Waktu | Segmen | Lakukan | Bukti yang ditunjuk |
|---|---|---|---|
| 0:00 | **Masalah** | "Satu database untuk ratusan perusahaan. Satu query yang lupa `WHERE tenant_id` = data gaji perusahaan lain bocor. Worker mati di tengah payroll 100 orang: ada yang dibayar dua kali?" | — |
| 1:00 | **Arsitektur** | Diagram di README / `docs/architecture.md` §2 | Tenant dari token, `set_config(..., true)` per transaksi, RLS `FORCE`, role aplikasi tanpa `BYPASSRLS` |
| 2:00 | **Alur normal** | UI Alpha: daftar karyawan, tambah satu karyawan dengan NIK (seed sengaja tanpa PII), buka detailnya | NIK tampil ter-mask (`************1234`); nilai lengkap hanya lewat API full view (OWNER/HR atau karyawan itu sendiri, rate limit, tercatat di audit log; tidak ada tombolnya di UI); data hanya milik Alpha |
| 3:00 | **Demo A — isolasi** | `scripts/demo-live.sh a` | A1 OWNER Beta minta karyawan Alpha → **404** (bukan 403); A2 daftar Beta 0 baris Alpha; A3 `tid` diedit tanpa re-sign → 401; A4 `tenant_id` di body → 400. Tunjukkan `TestRLS_UnfilteredQuerySeesOnlyActiveTenant`: query tanpa filter pun hanya melihat tenant aktif |
| 5:00 | **Demo B — crash & resume** | `scripts/demo-live.sh b` | B2 run ganda → `409 ACTIVE_RUN_EXISTS` menunjuk run pertama; fault: worker **exit tepat sebelum COMMIT chunk ke-4**; progres berhenti di 6/10 ±10 dtk (lease), worker di-restart, lanjut ke 10/10; `verify-run`: 100 item dihitung ulang dari snapshot, 0 selisih |
| 7:00 | **Demo C — final itu final** | `scripts/demo-live.sh c` | Pengaju tidak bisa approve sendiri (403); OWNER approve (cek stale); finalize; kenaikan gaji surut ke September → `409 PERIOD_LOCKED`, mulai Oktober → 201; `verify-run` setelah final tetap 0 selisih |
| 9:00 | **Penutup** | Grafana *PayRun — Payroll & Tenancy* (`make up-obs`), `perf/results/…/REPORT.md` | Metrik INV = 0 (isolasi, mutasi final, period bypass, total mismatch, reproducibility); baseline jujur (laptop); temuan PR-3 (generic plan). Tawarkan diskusi: RLS vs schema-per-tenant, kenapa change log bukan lock input, fencing |

## Versi 3 menit (HR / non-teknis)
1. "Setiap perusahaan hanya melihat datanya sendiri" → Demo A1 di dua browser.
2. "Sistem mati di tengah hitung gaji tidak membuat gaji dobel" → Demo B: 100 karyawan = 100 hasil, bukan 110, walaupun worker mati di tengah jalan.
3. "Gaji yang sudah final tidak bisa diubah diam-diam; koreksi tercatat di bulan berikutnya" → Demo C5.

### Tanpa presenter: tur 7 langkah di bar demo
Bar demo menampilkan tur lintas peran (Dina 4, Hendra 2, Rizky 1), masing-masing dengan "Kenapa penting" dalam
bahasa HR. Langkah tercentang saat benar-benar dilakukan (payroll final dibuka, cek ulang lolos, kesimpulan simulasi
gangguan tampil, payroll diajukan, persetujuan, ubah gaji ditolak/berhasil, slip dibuka); selesai semua → rangkuman
3 poin + "Hubungi pembuatnya" (`VITE_CONTACT_URL`). Kenaikan gaji Bayu mulai bulan ini sudah ada di sandbox, jadi
riwayat gaji dan penolakan `PERIOD_LOCKED` bisa dicoba dari profil karyawan. Naskah video: `docs/demo-video-script.md`.

### Tanpa presenter: "Coba demo" di halaman login
Pengunjung link demo publik mencoba sendiri. Tombol **Coba demo** membuat perusahaan pribadi **PT Kopi Nusantara**
(15 karyawan, 3 departemen) dengan payroll bulan lalu yang sudah **final**: benar-benar dihitung worker, diajukan
Dina, disetujui Pak Hendra, lalu slip gaji dirender. Bar demo di atas setiap halaman menunjukkan peran aktif,
tombol **Ganti peran**, dan langkah berikutnya. Diuji `TestDemoSandbox_OneClickCompanyWithPersonas`.
1. **Dina (admin payroll):** buka payroll bulan lalu (Final), buat payroll bulan ini → Hitung → Ajukan.
2. **Ganti peran ke Hendra (owner):** kotak approval berisi cuti Rizky (dan payroll yang baru diajukan). "Dina tidak bisa menyetujui pengajuannya sendiri."
3. **Ganti peran ke Rizky (karyawan):** slip gaji bulan lalu + unduh PDF; ajukan cuti. Menu payroll tidak ada (otorisasi server, bukan sekadar UI).
4. **Simulasi gangguan** (halaman run, sebelum Hitung): "Pasang gangguan" → satu chunk panic tepat sebelum COMMIT,
   rollback, diulang otomatis (backoff ±2 dtk). Timeline: "Bagian N: mati di tengah → diulang → selesai"; hasil
   "15 karyawan, 15 hasil". Versi aman dari Demo B: `panic` (satu percobaan chunk tenant pengunjung), bukan `exit`
   yang mematikan worker bersama di VM publik (`TestDemoSandbox_CrashTestChunkRetriesWithoutDuplicates`).
5. **Biaya gaji per departemen** (halaman run): grafik dari `GET /payroll-runs/{id}/summary` (Y6s), totalnya sama persis
   dengan Total net run (`TestDemoSandbox_DepartmentSummaryAddsUpToRunTotal`).
6. **Di balik layar: cek ulang hasil** (run final): `POST /api/v1/demo/runs/{id}/verify` = `VerifyRun` (PI-12), "15 dari 15
   slip cocok persis". Pengunjung tanpa sesi yang membuka `/` melihat landing `/welcome`.

## Kalau ada yang salah saat live
| Gejala | Tindakan |
|---|---|
| `seed: database already has employees` | Data demo sudah terpakai: `make demo-down && make up-demo && make seed-demo` |
| Demo B tidak bergerak dari 6/10 | Normal ±10 dtk (lease). Lebih dari 30 dtk: `docker ps` (worker restart?), `docker logs payrun-demo-worker-1` |
| `run not CALCULATED after 90 s` | Fault tertinggal dari latihan: `make demo-down` lalu ulang persiapan |
| Port 8082 terpakai | Ubah port di `DEMO` (Makefile) dan `PR_DEMO_API` untuk skrip |
| Stack belum healthy | `docker compose -p payrun-demo ps`; pindah ke video |

## Pertanyaan yang biasanya muncul
- *"Kalau developer lupa `WHERE tenant_id`?"* → Demo A + `TestRLS_UnfilteredQuerySeesOnlyActiveTenant`; tenant guard C-46 sebagai lapis kedua; test katalog memastikan setiap tabel tenant `FORCE RLS`.
- *"Kenapa 404, bukan 403?"* → 403 membocorkan bahwa resource itu ada di tenant lain (enumeration).
- *"Bagaimana tahu worker tidak menulis dua kali?"* → Fencing: `CompleteInTx` di transaksi yang sama dengan tulisan chunk; lease dicuri → commit gagal (`TestJobRunner_FencedCompletion_ZombieRollsBack`).
- *"HR mengubah data saat run menunggu approval?"* → change log + fingerprint, `RUN_STALE` dengan daftar karyawan (`TestSubmit_InputChangedAfterCalc_ReturnsRunStaleWithEmployees`).
