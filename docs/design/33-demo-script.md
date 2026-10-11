# PayRun: Demo Script (10 menit teknis + 3 menit HR)

> Bagian dari demo script. Struktur waktu dan aturan kejujuran klaim di `00-index.md` **mengikat**.
> Pola yang harus terasa sepanjang demo: **Problem → Solution → Engineering Challenge → Technical Solution → Evidence**.
> Sumber angka dan perilaku: `02-product-strategy.md` §8 (Demo A–C), `07-payroll-engine.md` §15 (contoh Tono), `09-failure-scenarios.md` (PFS-01/03/06), `16-api-endpoints.md` §17, `23-background-jobs.md` (demo faults), `24-observability.md` §9.

---

## 0. Kapan script ini bisa dipakai

| Kondisi | Versi demo |
|---|---|
| **Setelah PayRun Sprint 8** (demo toolbar + `demo_faults` tersedia, deploy publik) | Script lengkap di bawah, live di VM publik |
| **Sprint 5–7** (M6 tercapai, toolbar belum ada) | Demo B dibuktikan lewat **test subprocess** (`go test -run TestPayrollRun_CrashAndResume -v`), bukan toolbar. Lihat §6 |
| **Sebelum M5** | Jangan demo PayRun. Demo LedgerPay saja |

---

## 1. Persiapan (15 menit sebelum demo)

> **Token demo (final review R2-4):** `DEMO_TOKEN` hanya ada di environment terminal presenter (`export DEMO_TOKEN=…` dari password manager) dan ditempel manual ke demo toolbar saat dibutuhkan. Token disimpan di memori tab saja, **tidak pernah** di bundle SPA, `localStorage`, atau README. Endpoint demo yang tersisa hanya `faults`, `reset` (dan `fast-track` di PayRun); skenario provider diatur lewat admin mock via SSH tunnel.

### 1.1 Data & environment
| Item | Nilai / perintah | Cek |
|---|---|---|
| Reset data demo | `POST /api/v1/demo/reset` (dengan `DEMO_TOKEN`) atau `make demo-reset` | Tenant **PT Alpha** (100 karyawan aktif, termasuk Tono dan Arif sebagai manager), **PT Beta** (20 karyawan) |
| Chunk size demo | `PAYROLL_CHUNK_SIZE=10` di env worker demo (O5) | `GET /payroll-runs/{id}` nanti menampilkan `chunks_total: 10` |
| Lease demo | Lease job 10 dtk (mode demo) | Resume terlihat dalam ±10–15 dtk, bukan 30 dtk |
| Restart policy worker | `restart: unless-stopped` | Worker hidup lagi otomatis setelah `os.Exit(137)` |
| Data Tono (desain domain §15) | Gaji Rp6.000.000 → **Rp6.600.000 berlaku 15 Maret 2026**; lembur 6 jam (23 Mar 4j, 26 Mar 2j, status `PENDING` agar bisa di-approve saat demo); cuti tidak dibayar 25 Mar | Kenaikan gaji sengaja **belum** diinput, supaya diinput live |
| Periode | Maret 2026 belum punya run | `GET /payroll-runs?period=2026-03` kosong |
| Fault | Tidak ada fault aktif: `DELETE /api/v1/demo/faults` | — |
| Verifikasi menyeluruh | `make demo-verify PROJECT=payrun` hijau (Demo A–C headless, `25-testing-strategy.md`) | Jalankan **pagi hari demo**, bukan 1 menit sebelumnya |

### 1.2 Layar
| Jendela | Isi |
|---|---|
| Browser profile 1 | **Dewi, HR Admin PT Alpha** (sudah login, tenant Alpha aktif) |
| Browser profile 2 | **Arif, Manager PT Alpha** (inbox approval) |
| Browser profile 3 | **Tono, Employee PT Alpha** (halaman slip gaji) |
| Browser profile 4 | **HR PT Beta** (hanya untuk menyalin satu `employee_id` Beta ke clipboard) |
| Grafana | Dashboard **"PayRun — Payroll Runs"** dan **"PayRun — Tenancy & Security"** (`24-observability.md` §6), rentang waktu 15 menit |
| Jaeger | Service `payrun-worker`, siap dicari berdasarkan `run_id` |
| Terminal 1 | Siap: `curl` Demo A (§3.1), test isolation (§3.2) |
| Terminal 2 | `docker compose logs -f payrun-worker \| jq -c 'select(.job_kind!=null)'` |
| Slide 1 lembar | Diagram arsitektur (`04-architecture.md` §2.2) yang disederhanakan |

---

## 2. Script 10 menit (interviewer teknis)

### 0:00–1:00 — Problem

| SAY | DO | AUDIENCE SEES |
|---|---|---|
| "UMKM di Indonesia masih banyak menghitung gaji di spreadsheet. Rumus rusak, absensi beda versi, dan tidak ada jejak siapa mengubah angka. Setelah dikirim ke bank, tidak ada yang tahu angka final itu dihitung dari data versi mana." | Tampilkan slide masalah (3 poin) | Slide |
| "Kalau payroll dijadikan SaaS, risikonya bertambah: **satu kebocoran data gaji antar perusahaan bisa mematikan bisnis penyedianya.**" | — | — |
| *EN:* "PayRun is a multi-tenant payroll platform. The two guarantees I designed for: **tenant data can't leak even if my code has a bug**, and **a finalized payroll is reproducible and can't be silently changed.**" | — | — |

**Transisi:** "Saya tunjukkan dulu bagaimana sistemnya disusun untuk memberi dua jaminan itu."

### 1:00–2:00 — Architecture (Solution)

| SAY | DO | AUDIENCE SEES |
|---|---|---|
| "Modular monolith Go + PostgreSQL. Satu binary, dua proses: `api` untuk request, `worker` untuk kalkulasi payroll." | Slide diagram | api, worker, Postgres, MinIO |
| "Multi-tenancy: **pooled table + `tenant_id` + Postgres Row-Level Security.** Tenant diambil dari token, setiap transaksi menyetel `app.tenant_id` secara *transaction-local*, dan database sendiri yang memfilter baris. Jadi isolasi tidak bergantung pada developer yang tidak pernah lupa `WHERE tenant_id`." | Tunjuk lapisan di diagram: token → `WithTenantTx` → RLS → composite FK | Defense in depth |
| "Payroll dihitung **asinkron per chunk**, idempoten, dan bisa dilanjutkan setelah crash. Setiap item menyimpan **snapshot input + versi ruleset**, jadi hasil final bisa direproduksi." | Tunjuk worker + tabel snapshot | — |
| *EN:* "Isolation is enforced by the database, not by discipline." | — | — |

### 2:00–5:00 — Normal workflow (Solution bekerja)

| Waktu | SAY | DO | AUDIENCE SEES |
|---|---|---|---|
| 2:00 | "Ini Dewi, HR PT Alpha. 100 karyawan." | Profile 1 → Employees | List 100 karyawan, kolom NIK **ter-mask** (`****1234`) |
| 2:20 | "Tono naik gaji mulai 15 Maret. Riwayat lama **tidak ditimpa**: record lama ditutup, lalu record baru dibuat (*effective dating*)." | Tono → tab Gaji → "Ubah gaji" Rp6.600.000, berlaku 2026-03-15 | Riwayat: `[2026-01-01, 2026-03-15) Rp6.000.000`, `[2026-03-15, ∞) Rp6.600.000` |
| 2:45 | "Arif, manager Tono, menyetujui lembur. Approver di-*snapshot* saat request dibuat, dan dia tidak bisa menyetujui request miliknya sendiri." | Profile 2 → Inbox → approve lembur 23 & 26 Maret | Status `APPROVED`, tercatat di audit |
| 3:10 | "Kembali ke Dewi. Jalankan payroll Maret." | Profile 1 → Payroll → "Buat run Maret 2026" → "Hitung" | Run `CALCULATING`, progress bar `chunks 0/10 … 10/10` (±5–10 dtk), lalu `CALCULATED` |
| 3:40 | "Halaman review: total, jumlah karyawan, dan **warning**, misalnya lembur di atas batas. Warning tidak memblok, tapi error memblok submit." | Tunjuk ringkasan run | Total run, `counts.ok = 100`, jumlah warning |
| 4:00 | "Buka item Tono. Setiap baris punya penjelasan, jadi karyawan tahu potongan datang dari mana." | Klik Tono | Baris (angka dari desain domain §15): gaji pokok seg-1 **3.000.000** (10/20 hari kerja), seg-2 **3.300.000**, tunjangan jabatan **500.000**, transport **380.000** (19 hari hadir), lembur **419.653** ("6 jam = 11 jam berbobot × Rp38.150,29"), potongan cuti tidak dibayar **330.000**, BPJS 136.000 + 68.000 + 68.000, PPh21 **109.044** |
| 4:30 | "Gross 7.599.653, potongan 711.044, **net 6.888.609**. Net selalu sama persis dengan Σ baris, karena pembulatan dilakukan per komponen, bukan di total." | Tunjuk total item | Net **Rp6.888.609** |
| 4:50 | "Kenaikan gaji di tengah bulan menjadi **dua baris** gaji pokok, bukan satu angka rata-rata yang tidak bisa dijelaskan. Catatan: tarif pajak/BPJS di sini **contoh yang disederhanakan**, bukan tarif resmi." | — | — |

**Transisi:** "Itu alur normalnya. Sekarang bagian yang paling saya pikirkan: bagaimana kalau ada yang mencoba melihat data perusahaan lain?"

### 5:00–7:00 — Killer feature: Demo A, tenant isolation (Engineering Challenge → Technical Solution)

| Waktu | SAY | DO | AUDIENCE SEES |
|---|---|---|---|
| 5:00 | "Tantangannya: satu codebase dan satu database melayani banyak perusahaan. Satu query yang lupa filter berarti data gaji bocor." | — | — |
| 5:15 | "Dewi dari Alpha mencoba membuka karyawan milik PT Beta, dengan ID yang saya salin dari akun Beta." | Terminal 1: `curl … /employees/<BETA_ID>` dengan token Dewi (§3.1 A2) | **`404 NOT_FOUND`**, bukan 403. Sistem tidak membocorkan bahwa ID itu ada |
| 5:35 | "Bagaimana kalau token-nya diubah supaya mengaku tenant Beta?" | `curl` dengan JWT yang claim `tid`-nya diedit (§3.1 A3) | **`401 UNAUTHENTICATED`** (signature tidak valid) |
| 5:50 | "Atau mengirim `tenant_id` Beta di body, atau mereferensikan departemen Beta?" | §3.1 A4, A5 | `400 VALIDATION_ERROR` (field tidak dikenal); `404` (composite FK menolak referensi lintas tenant) |
| 6:10 | "Ini bagian yang paling penting. Semua di atas bisa dilakukan aplikasi biasa. Pertanyaannya: **bagaimana kalau kode saya sendiri yang salah?** Test ini sengaja menjalankan query **tanpa** `WHERE tenant_id`, seperti bug developer." | Terminal 1: `go test ./internal/... -run 'TestTenantIsolation\|TestRLS_BypassQuery' -v` (§3.2) | Output: `TestRLS_BypassQuery_ReturnsOnlyCurrentTenant PASS`, query mentah hanya mengembalikan 100 baris Alpha, 0 baris Beta |
| 6:35 | "Test isolation ini jalan untuk **setiap endpoint**. Endpoint baru tanpa baris di matriks otorisasi membuat CI gagal. Ada juga test bahwa koneksi pool yang dipakai ulang tidak mewarisi tenant sebelumnya." | Tunjuk ringkasan test (jumlah subtest, semua PASS) | `TestAuthzMatrix`, `TestRLS_PoolLeakage` PASS |
| 6:50 | "Di Grafana: penolakan 404 naik, dan metrik invariant **pelanggaran isolasi tetap 0**. Metrik itu dari pemeriksaan defensif di repository yang seharusnya tidak pernah menyala." | Grafana "Tenancy & Security" | `app_authz_denied_total{outcome="hidden_404"}` naik; `payrun_tenant_isolation_violations_total = 0` |
| | *EN:* "Even if I forget the tenant filter, Postgres returns only the current tenant's rows." | | |

**Transisi:** "Kalau isolasi menjawab 'siapa boleh melihat apa', bagian berikutnya menjawab 'apa yang terjadi kalau server mati di tengah payroll'."

### 7:00–9:00 — Failure scenario: Demo B crash & resume (+ Demo C singkat) (Evidence)

| Waktu | SAY | DO | AUDIENCE SEES |
|---|---|---|---|
| 7:00 | "Saya pasang *fault*: worker mati tepat sebelum chunk ke-4 di-commit. Chunk size demo 10, jadi chunk ke-4 berisi karyawan 31–40. Di production chunk size 50." | Toolbar demo → "Crash worker" → `POST /api/v1/demo/faults {"point":"payroll.calc_chunk.before_commit","after_n":3}` | Fault aktif |
| 7:15 | "Hitung ulang run Maret. `calc_version` naik ke 2, jadi hasil versi lama otomatis diabaikan." | Profile 1 → "Hitung ulang" | Progress `chunks 3/10`, lalu berhenti |
| 7:25 | "Worker-nya mati. `os.Exit(137)`, persis seperti dibunuh OOM killer." | Terminal 2 | Log terakhir `payroll.calc_chunk chunk=4 attempt=1`, proses keluar; compose me-restart worker |
| 7:40 | "Run tetap `CALCULATING`. Chunk 4 tidak pernah setengah tersimpan, karena item, line, snapshot, counter progres, dan penanda job selesai ada di **satu transaksi**." | Refresh run | `chunks 3/10`, status `CALCULATING` |
| 7:55 | "Setelah lease 10 detik habis, worker baru mengambil chunk 4 lagi dan melanjutkan." | Tunggu ±10 dtk | `chunks 10/10` → **`CALCULATED`**, `counts.ok = 100` |
| 8:10 | "Buktinya: 100 item, bukan 110. Total run = Σ item. Item ditulis dengan *upsert* per `(run, employee, calc_version)`, jadi ulangan tidak bisa menghasilkan duplikat." | Panel Grafana + tombol "Verify" (atau query §3.4) | `items = 100`, `total_net = Σ item`; `app_job_lease_expired_total{kind="payroll.calc_chunk"}` +1 |
| 8:25 | "Bonus: dua admin menekan 'Buat run' untuk periode yang sama." | §3.3: dua `POST /payroll-runs` dengan key berbeda | Satu `201`, satu **`409 ACTIVE_RUN_EXISTS`** + `existing_resource_id` (partial unique index) |
| 8:40 | "Terakhir, setelah final: angka tidak bisa diubah diam-diam." *(Demo C versi singkat; run sudah disubmit, di-approve, dan difinalize sebelumnya lewat tombol "Fast-track demo")* | Edit absensi Tono 25 Maret | **`409 PERIOD_LOCKED`**. Koreksi dilakukan lewat **adjustment** yang muncul di slip April sebagai "Koreksi lembur periode Maret 2026" |
| | *EN:* "Crash mid-batch, resume from the last committed chunk, no duplicate items, and finalized payroll is immutable." | | |

> **Pilihan Demo C:** `PERIOD_LOCKED` dipilih karena paling cepat dipahami. Kalau interviewer tertarik pada concurrency, ganti dengan **`RUN_STALE`**: ubah absensi *setelah* calculate tetapi *sebelum* approve → approve ditolak `409 RUN_STALE {employees_changed:[Tono]}` (PFS-03, desain transaksi & concurrency O12). Jangan tampilkan keduanya di 10 menit.

### 9:00–10:00 — Observability & discussion hooks

| SAY | DO | AUDIENCE SEES |
|---|---|---|
| "Di Jaeger ada dua span untuk chunk 4: attempt pertama terputus, attempt kedua sukses, dan keduanya ter-link ke request 'Hitung ulang'." | Jaeger, cari `run_id` | Dua span `job payroll.calc_chunk` (chunk 4) |
| "Dashboard Payroll Runs: durasi run, chunk per detik, item error per kode." | Grafana | `payrun_payroll_run_duration_seconds`, progres |
| "Beberapa hal yang dengan senang hati saya bahas lebih dalam:" | Slide topik | 1) **Schema-per-tenant vs pooled + RLS**: di pekerjaan saya sebelumnya saya mengoperasikan schema per perusahaan dan merasakan biaya migrasi ke banyak schema; di sini saya sengaja memilih RLS, dan ada trade-off-nya. 2) **Noisy neighbor**: tenant 10.000 karyawan vs tenant 20 karyawan di antrean yang sama. 3) **Determinism & snapshot**: bagaimana membuktikan payroll Januari tahun lalu benar. 4) **Race saat approve**: kenapa approve mengunci baris periode |
| *EN:* "Happy to go deeper into any of these; the design decisions are written up as ADRs." | — | — |

---

## 3. Lampiran: perintah dan output yang diharapkan

> Variabel: `$API=https://payrun.<domain>/api/v1`, `$T_DEWI` = access token Dewi (tenant Alpha), `$BETA_EMP` = ID karyawan Beta, `$BETA_DEPT` = ID departemen Beta.

### 3.1 Demo A — isolasi lewat API
```bash
# A2 — membaca karyawan tenant lain
curl -s -o /dev/null -w "%{http_code}\n" -H "Authorization: Bearer $T_DEWI" $API/employees/$BETA_EMP
# → 404   (body: {"code":"NOT_FOUND", ...})

# A3 — claim tid diedit (payload diubah tanpa re-sign)
curl -s -H "Authorization: Bearer $T_TAMPERED" $API/employees | jq .code
# → "UNAUTHENTICATED"   (401)

# A4 — tenant_id di body
curl -s -X POST -H "Authorization: Bearer $T_DEWI" -H "Idempotency-Key: demo-a4-$(date +%s)" \
  -d '{"tenant_id":"<BETA>","employee_no":"X1","full_name":"X"}' $API/employees | jq .code
# → "VALIDATION_ERROR"  (400, field tidak dikenal)

# A5 — referensi departemen tenant lain
curl -s -X POST -H "Authorization: Bearer $T_DEWI" -H "Idempotency-Key: demo-a5-$(date +%s)" \
  -d '{"employee_no":"X2","full_name":"X","org":{"department_id":"'$BETA_DEPT'"}}' $API/employees | jq .code
# → "NOT_FOUND"         (404; composite FK (tenant_id, department_id))
```

### 3.2 Demo A — test isolasi
```bash
go test ./internal/... -run 'TestTenantIsolation|TestRLS_BypassQuery|TestRLS_PoolLeakage|TestAuthzMatrix' -count=1 -v | grep -E '^(=== RUN|--- (PASS|FAIL)|ok)'
# --- PASS: TestRLS_BypassQuery_ReturnsOnlyCurrentTenant
#     rows returned: 100 (tenant alpha), foreign rows: 0
# --- PASS: TestRLS_PoolLeakage
# --- PASS: TestAuthzMatrix (… subtests)
# ok
```

### 3.3 Demo B — run ganda
```bash
curl -s -X POST -H "Authorization: Bearer $T_DEWI" -H "Idempotency-Key: K1" -d '{"period":"2026-04"}' $API/payroll-runs -o /dev/null -w "%{http_code}\n"   # 201
curl -s -X POST -H "Authorization: Bearer $T_DEWI" -H "Idempotency-Key: K1" -d '{"period":"2026-04"}' $API/payroll-runs -D - -o /dev/null | grep -i idempotent-replayed   # Idempotent-Replayed: true
curl -s -X POST -H "Authorization: Bearer $T_HENDRA" -H "Idempotency-Key: K2" -d '{"period":"2026-04"}' $API/payroll-runs | jq '{code, existing_resource_id}'
# → {"code":"ACTIVE_RUN_EXISTS","existing_resource_id":"…"}   (409)
```
> Run ganda didemokan di periode **April** agar tidak mengganggu run Maret yang sedang dipakai Demo B.

### 3.4 Demo B — fault, crash, verifikasi
```bash
curl -s -X POST -H "X-Demo-Token: $DEMO_TOKEN" -d '{"point":"payroll.calc_chunk.before_commit","after_n":3}' $API/demo/faults
curl -s -X POST -H "Authorization: Bearer $T_DEWI" -H "Idempotency-Key: calc-$(date +%s)" $API/payroll-runs/$RUN/calculate   # 202
watch -n1 "curl -s -H 'Authorization: Bearer $T_DEWI' $API/payroll-runs/$RUN | jq '{status, calc_version, progress}'"
# {"status":"CALCULATING","calc_version":2,"progress":{"chunks_done":3,"chunks_total":10}}  … ±10 dtk …
# {"status":"CALCULATED","calc_version":2,"progress":{"chunks_done":10,"chunks_total":10}}

# Verifikasi (read-only, role payrun_readonly, tetap di bawah RLS)
SELECT count(*) AS items, sum(net) AS sum_net FROM payroll_items WHERE run_id = :run AND calc_version = 2;
-- items = 100 ; sum_net = payroll_runs.total_net
```

### 3.5 Demo C — immutability & koreksi
```bash
curl -s -X PATCH -H "Authorization: Bearer $T_DEWI" -H "If-Match: \"3\"" -d '{"clock_out":"2026-03-25T10:00:00Z","reason":"koreksi"}' $API/attendance/$ATT_TONO_0325 | jq .code
# → "PERIOD_LOCKED"   (409)
curl -s -X POST -H "Authorization: Bearer $T_DEWI" -H "Idempotency-Key: adj-1" \
  -d '{"employee_id":"'$TONO'","origin_period":"2026-03","target_period":"2026-04","component_code":"OVERTIME_CORRECTION","amount":"76300.58","reason":"lembur kurang 2 jam"}' $API/adjustments -o /dev/null -w "%{http_code}\n"   # 201
payrun verify-run --run $RUN_MARET     # recompute dari snapshot → "identical: 100/100 items"
```
> Nominal adjustment mengikuti contoh di kontrak API (`16-api-endpoints.md` §17) dan hanya ilustrasi.

---

## 4. Versi 3 menit (HR / non-teknis)

| Waktu | SAY | DO / SEES |
|---|---|---|
| 0:00–0:40 | "Banyak perusahaan kecil menghitung gaji di Excel. Sering salah, tidak ada jejak, dan file gaji gampang bocor. Saya membuat aplikasi payroll online yang dipakai banyak perusahaan sekaligus." | Slide 1 |
| 0:40–1:40 | "HR memasukkan kenaikan gaji, manager menyetujui lembur, lalu HR menekan satu tombol untuk menghitung gaji 100 karyawan. Di slip, karyawan bisa melihat dari mana setiap potongan berasal." | Kenaikan gaji → approve lembur → hitung → slip Tono (net Rp6.888.609, baris dengan penjelasan) |
| 1:40–2:20 | **Momen "wow" 1:** "PT A tidak bisa melihat gaji karyawan PT B. **Bahkan kalau programmernya membuat kesalahan**, database tetap menolak." | `404` ditampilkan dengan bahasa awam: "data tidak ditemukan" |
| 2:20–2:50 | **Momen "wow" 2:** "Kalau server mati di tengah perhitungan gaji, sistem melanjutkan sendiri, dan **tidak ada karyawan yang dihitung dua kali**." | Progress bar berhenti → lanjut → 100 karyawan |
| 2:50–3:00 | "Dan setelah gaji final, angkanya tidak bisa diubah diam-diam. Koreksi selalu tercatat di bulan berikutnya." | Pesan "Periode sudah dikunci" |

Hindari istilah RLS, chunk, idempotent, dan fingerprint. Kalau ditanya "pakai teknologi apa", jawab singkat: "Go dan PostgreSQL, dengan fitur keamanan database untuk memisahkan data tiap perusahaan."

---

## 5. Interupsi yang mungkin & jawaban singkat

| Pertanyaan | Jawaban singkat (≤ 30 detik) | Rujukan |
|---|---|---|
| "Kenapa tidak schema per tenant?" | "Saya pernah mengoperasikan schema per perusahaan. Isolasinya kuat, tapi migrasi harus dijalankan ke banyak schema dan laporan lintas tenant jadi rumit. Untuk UMKM yang jumlahnya banyak dan kecil-kecil, pooled + RLS lebih murah dioperasikan. Untuk tenant sangat besar, jalur V3-nya hybrid: tenant besar mendapat DB sendiri lewat tenant catalog." | ADR-P01, `04-architecture.md` §4 |
| "Apakah pajaknya sudah sesuai aturan PPh21/BPJS?" | "Belum, dan saya tidak mengklaimnya. Tarifnya disimpan sebagai ruleset ber-versi yang disederhanakan dan bisa dikonfigurasi. Engine-nya didesain supaya aturan resmi bisa dimasukkan sebagai data tanpa mengubah kode." | ADR-P03, `07-payroll-engine.md` §10 |
| "Bagaimana kalau RLS salah konfigurasi, misalnya lupa `FORCE`?" | "Ada test katalog di CI: setiap tabel ber-`tenant_id` wajib RLS + FORCE, role aplikasi bukan owner dan tanpa BYPASSRLS, dan `SECURITY DEFINER` hanya satu yang di-allowlist. Di runtime ada pemeriksaan defensif: kalau baris dari tenant lain sampai ke aplikasi, request digagalkan dan metrik invariant naik." | `13-db-rls-consistency-indexing.md`, threat model C-36, C-46 |
| "Kenapa 404, bukan 403?" | "403 membocorkan bahwa resource itu ada. Untuk data tenant lain, pemanggil tidak boleh tahu keberadaannya." | ADR-010 |
| "Kalau HR mengubah absensi saat run sedang dihitung?" | "Diizinkan, tapi approve akan menolak dengan RUN_STALE dan menampilkan karyawan yang berubah. Approve mengunci baris periode, dan penulis input mengambil share lock, sehingga tidak ada celah race." | ADR-P07, desain transaksi & concurrency O3/O12 |
| "Satu tenant besar memperlambat yang lain?" | "Di V1 ya, itu batas yang saya akui. Mitigasinya: chunking, rate limit `heavy` per tenant, dan batas konkurensi job per tenant (P2)." | `04-architecture.md` §2.6, `23-background-jobs.md` |
| "Worker zombie menulis setelah run final?" | "Ditolak tiga lapis: fencing saat menyelesaikan job, guard `calc_version`, dan trigger yang mengunci baris run dengan `FOR SHARE` sehingga terserialisasi dengan finalize." | ADR-006, `12-db-schema.md` PI-04 |

---

## 6. Rencana cadangan & urutan pemotongan

### 6.1 Kalau ada yang tidak berjalan
| Masalah | Cadangan |
|---|---|
| VM publik down / jaringan lambat | `make demo` di laptop (compose lokal, data seed yang sama) |
| Toolbar demo belum ada (sebelum PayRun Sprint 8) atau fault tidak menyala | Demo B lewat test subprocess: `go test -tags testhooks ./internal/app/ -run TestPayrollRun_CrashAndResume -count=1 -v` (atau `make demo-verify`: Demo A + B + recompute). Worker subprocess keluar sebelum commit chunk 4 (run `CALCULATING`, 3/10 chunk, 30 item), worker kedua melanjutkan setelah lease habis, `items=100`, tanpa duplikat, total = Σ item, lalu run ganda → `201` + `409 ACTIVE_RUN_EXISTS` |
| Resume lebih lama dari perkiraan | Lanjutkan bicara tentang fencing dan lease (±30 dtk), lalu kembali ke layar |
| Grafana/Jaeger tidak memuat | Tunjukkan log terminal 2 + query SQL §3.4 |
| Semua gagal | Video rekaman (§7), diputar dengan narasi langsung |

### 6.2 Urutan pemotongan kalau waktu habis
1. Potong **Demo C** (cukup sebut satu kalimat: "finalized = immutable, koreksi lewat adjustment").
2. Potong **run ganda** (bonus 8:25).
3. Potong **A4/A5** di Demo A (sisakan 404 + test RLS bypass).
4. **Jangan pernah potong:** test RLS bypass (6:10) dan crash & resume (7:00–8:10). Keduanya adalah inti nilai teknis PayRun.

---

## 7. Latihan & video cadangan

| Kegiatan | Kapan | Kriteria |
|---|---|---|
| Latihan penuh dengan timer | 3× sebelum interview pertama, 1× sehari sebelum setiap interview | Selesai ≤ 10:00; setiap segmen dalam ±15 dtk dari jadwal |
| Latihan versi HR | 2× | ≤ 3:00, tanpa istilah teknis |
| `make demo-verify PROJECT=payrun` | Nightly (CI) + pagi hari demo | Hijau |
| Rekam video cadangan | Setelah PayRun Sprint 8 (toolbar tersedia); rekam ulang setiap ada perubahan UI besar | 1080p, ≤ 10 menit, layar terbagi (app kiri, Grafana/terminal kanan), tanpa data sensitif atau token terlihat |
| Checklist kesiapan demo | Sebelum setiap interview | `31-definition-of-done.md` §6 |
| Rotasi `DEMO_TOKEN` | Setelah demo yang dibagikan layarnya ke pihak luar | Token lama tidak berlaku |
