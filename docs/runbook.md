# Runbook PayRun

Satu bagian per alert di `deploy/prometheus/rules/` (annotation `runbook` setiap alert menunjuk ke sini;
`TestObservability_AlertsLinkToRunbook` memastikan heading-nya ada), lalu prosedur operasional.
Pola setiap alert: **dampak → lihat dulu → investigasi → pulihkan → setelahnya**.

Konvensi perintah (VM demo atau lokal; nama container lokal: `payrun-api-1`, `payrun-postgres-1`):
- `payrun verify-run <tenant> <run>` di container api: hitung ulang run dari snapshot tersimpan (role
  aplikasi, di bawah RLS, read-only). Keluaran JSON; `mismatches` kosong = hasil tersimpan bisa direproduksi.
- Investigasi data tenant: `psql` sebagai **`payrun_readonly`** (SELECT saja, `NOBYPASSRLS`) setelah
  `SELECT set_config('app.tenant_id', '<tenant>', false);`. Tanpa tenant context hasilnya 0 baris (fail closed).
- Tabel `jobs` (lintas tenant) hanya dibaca sebagai superuser/owner di VM, tidak pernah dari aplikasi.
- Kill switch (env worker, restart worker): `PR_PAYROLL_CALC_ENABLED=false` (job payroll tetap antre, tidak
  dead), `PR_PAYSLIP_RENDER_ENABLED=false` (lane documents berhenti).

## Integritas

### PayRunInvariantViolated
- **Dampak:** insiden tertinggi. Salah satu dari: baris tenant lain terlihat (isolasi), percobaan mengubah run
  final, penulisan input ke periode terkunci melewati guard aplikasi (ditolak trigger DB), total run ≠ Σ item,
  atau recompute dari snapshot berbeda.
- **Lihat dulu:** counter mana yang naik (`payrun_tenant_isolation_violations_total`,
  `payrun_finalized_mutation_attempts_total{table}`, `payrun_period_lock_bypass_total{table}`,
  `payrun_payroll_total_mismatch_runs`, `payrun_reproducibility_failures_total`) dan sejak kapan;
  bandingkan dengan deploy terakhir (`app_build_info`).
- **Investigasi:**
  - *Isolasi:* log `tenant.isolation_violation` (field `table`, `want_tenant`, `row_tenant`, plus `request_id` dari middleware). Guard
    C-46 sudah mengembalikan 500 alih-alih data, jadi tidak ada baris yang keluar; cari query yang tidak
    lewat `WithTenantTx` atau policy yang berubah (`TestCatalogue_*` harus tetap hijau di commit itu).
  - *Finalized / period lock:* trigger menolak (`RUN_FINALIZED` / `PERIOD_LOCKED`); label `table` menunjuk
    penulisnya. Kenaikan dari jalur aplikasi = bug guard aplikasi (penulis yang tidak memanggil PeriodGuard).
  - *Total mismatch / reproducibility:* `payrun verify-run <tenant> <run>` untuk run yang disebut log;
    `mismatches` menyebut karyawan dan field.
- **Pulihkan:** isolasi → rollback ke SHA sebelumnya (`rollback.yml`) bila terkait rilis. Run yang tidak bisa
  direproduksi jangan difinalisasi: cancel lalu recalculate. Run yang sudah final tidak diubah; koreksi lewat
  adjustment ke periode berikutnya.
- **Setelahnya:** postmortem; test yang mereproduksi (suite isolasi / `TestFinalizedRun_DirectUpdateRejected`).

## Proses latar

### BatchJobFailed
- **Dampak:** run payroll `FAILED` (job chunk dead setelah 5 percobaan, atau jumlah item ≠ eligible). HR tidak
  bisa submit run itu sampai di-recalculate.
- **Lihat dulu:** `failure_reason` run (`GET /api/v1/payroll-runs/{id}`); alert `JobsDeadLettered` biasanya
  ikut menyala untuk kind `payroll.calc_chunk`.
- **Investigasi:** `SELECT id, attempts, last_error FROM jobs WHERE kind = 'payroll.calc_chunk' AND status =
  'dead' AND payload->>'run_id' = '<run>';` (stack trace panic ada di `last_error`). Penyebab umum: data
  karyawan tidak konsisten (komponen menunjuk komponen yang dihapus), karyawan hilang di antara plan dan chunk.
- **Pulihkan:** perbaiki datanya, lalu recalculate dari UI/API (FAILED → CALCULATING diizinkan). Tidak ada
  item setengah jadi: setiap percobaan chunk di-rollback (`TestCalcChunk_PanicExhaustsAttempts_RunFailed`).

### JobsDeadLettered
- **Dampak:** pekerjaan latar berhenti untuk satu kind. `payroll.*` → run FAILED (lihat di atas);
  `payslip.render` → slip tetap `PAYSLIP_NOT_READY`; kind maintenance → pembersihan tertunda.
- **Investigasi:** `SELECT kind, tenant_id, attempts, left(last_error, 200) FROM jobs WHERE status = 'dead'
  AND updated_at > now() - interval '1 hour' ORDER BY updated_at DESC;`. Job dead dengan `run ... not found` dan
  tenant yang tidak cocok dengan run = payload lintas tenant (PFS-09, `TestJob_TenantRunMismatch_NoOpAndAlert`):
  tidak ada data yang berubah, tetapi jalur enqueue-nya bug.
- **Pulihkan:** perbaiki penyebab, lalu antrekan ulang: `UPDATE jobs SET status = 'queued', attempts = 0,
  run_at = now() WHERE id = '<job>';` (payroll: lebih aman recalculate run daripada menghidupkan chunk lama;
  chunk versi lama no-op karena `calc_version`).

### JobBacklogOld
- **Dampak:** job tertua menunggu > 5 menit: run "CALCULATING" lama, slip belum muncul.
- **Lihat dulu:** worker hidup? `/readyz` port ops worker (per lane); kill switch aktif? (`PR_*_ENABLED`).
- **Investigasi:** `SELECT kind, count(*), min(run_at) FROM jobs WHERE status = 'queued' GROUP BY kind;` dan
  `SELECT kind, locked_by, locked_until FROM jobs WHERE status = 'running';`. Lease kedaluwarsa tanpa worker
  aktif = worker mati di tengah job; worker lain mengambil alih setelah `JOB_LEASE_TTL`.
- **Pulihkan:** restart / tambah worker (`PAYROLL_WORKER_CONCURRENCY`, maksimal 32). Run besar tenant lain
  memonopoli lane `payroll` (noisy neighbor, batas per tenant = P2): tunggu atau tunda run besar.

### OutboxLagHigh
- **Dampak:** efek samping (email verifikasi/undangan, job lanjutan) tertunda > 30 detik setelah commit.
- **Investigasi:** sama dengan `JobBacklogOld` (outbox PayRun = tabel `jobs`); cek koneksi SMTP (Mailpit di
  dev/demo) untuk job email.
- **Pulihkan:** restart worker; SMTP mati → job di-retry dengan backoff, tidak perlu tindakan selain memulihkan SMTP.

### PayslipRenderFailing
- **Dampak:** PDF slip gagal dibuat 15 menit berturut-turut; karyawan melihat slip web, unduhan PDF
  `PAYSLIP_NOT_READY` (409).
- **Investigasi:** log worker `payslip.render` (biasanya object storage: endpoint, kredensial, bucket
  `payrun-private` belum dibuat); `SELECT status, attempts, last_error FROM payslips WHERE status = 'FAILED'`
  (sebagai `payrun_readonly` + tenant context).
- **Pulihkan:** pulihkan storage (`make up-storage` lokal); job di-retry sendiri. Render idempoten: object key
  deterministik, PUT ulang menimpa byte yang sama (`TestPayslipRender_CrashMidBatch_ResumesIdempotently`).
  Darurat: `PR_PAYSLIP_RENDER_ENABLED=false` supaya lane payroll tidak ikut terganggu.

## Platform

### High5xxRatio
- **Dampak:** > 2% respons 5xx selama 5 menit (per service).
- **Investigasi:** log `http.request` dengan `status >= 500` → `request_id` → stack/`error`; route mana
  (label `route` di `app_http_requests_total`). Lonjakan setelah deploy → rollback.
- **Pulihkan:** `rollback.yml` ke SHA terakhir yang sehat; DB penuh/lambat → lihat `DBPoolSaturated`.

### DBPoolSaturated
- **Dampak:** request menunggu koneksi; latency naik untuk semua tenant.
- **Investigasi:** `SELECT state, wait_event_type, left(query, 120), now() - xact_start FROM pg_stat_activity
  WHERE datname = 'payrun' ORDER BY xact_start;`. Penyebab yang pernah terlihat: query daftar karyawan memakai
  generic plan yang salah (PR-3, `perf/results/2026-10-10-fed1df1/REPORT.md`); transaksi panjang memegang lock
  periode (approve/finalize normalnya milidetik).
- **Pulihkan:** hentikan sumber beban (kill switch untuk job berat); `pg_cancel_backend(pid)` untuk query liar.
  Jangan memperbesar pool tanpa melihat CPU Postgres.

### ServiceNotReady
- **Dampak:** api atau lane worker tidak siap; edge mengembalikan 502/503.
- **Investigasi:** `/readyz` (port ops) menyebut komponen yang gagal: DB, versi skema (`MinSchemaVersion` binary >
  versi DB = migrasi belum jalan), lane worker.
- **Pulihkan:** jalankan migrasi (`payrun migrate up` dengan DSN owner) atau rollback binary ke SHA yang
  cocok dengan skema; migrasi bersifat expand/contract sehingga binary lama tetap jalan di skema baru.

## Keamanan

### RefreshTokenReuseSpike
- **Dampak:** refresh token yang sudah dirotasi dipakai lagi (> 5 kali / 15 menit): kemungkinan token dicuri,
  atau bug klien yang mengirim refresh paralel.
- **Investigasi:** log `auth.refresh_reuse` (user, family, IP, user agent). Satu user banyak kejadian dari IP
  berbeda = indikasi pencurian; banyak user dari satu versi SPA = bug klien.
- **Pulihkan:** family token sudah dicabut otomatis (user harus login ulang). Bug klien → perbaiki SPA
  (refresh tunggal dengan lock). Pencurian → beri tahu user, reset password.

## Prosedur

### Deploy dan rollback
`deploy/prod/deploy.sh <host|local> <sha>` (build/pull, migrate, health gate, smoke, rollback otomatis bila gagal).
Rollback manual: workflow `rollback.yml` dengan SHA yang pernah sukses. Uji lokal: `deploy/prod/deploy.sh local <sha>`.
Deploy publik belum dilakukan (ditunda sampai aplikasi siap dipakai).

### Restore dari backup
`scripts/restore-drill.sh <file.dump.age> <identity>`: restore ke Postgres sekali pakai, migrate, cek tabel tenant
tidak kosong (dump dengan role ber-RLS menghasilkan tabel kosong tanpa error), lalu `verify-run` setiap run final.
Restore sungguhan memakai langkah yang sama ke DB target. RPO 24 jam (backup harian), RTO ±2 jam.

### Demo (image build tag `demo` saja)
`make up-demo && make seed-demo && make demo-live` (stack terpisah `payrun-demo`, API `:8082`). Fault tertinggal dari
latihan: `DELETE /api/v1/demo/faults` dengan `X-Demo-Token` sebagai OWNER tenant itu, atau `make demo-down`.

Sandbox pengunjung ("Coba demo" di halaman login): `POST /api/v1/demo/sessions` membuat tenant baru (PT Kopi
Nusantara, 15 karyawan, cuti menunggu approval) dan menjalankan payroll bulan lalu sampai final lewat pipeline asli.
Request **menunggu worker** menghitung (normal 1–3 detik, batas 45 detik → 503); worker mati = tombol demo gagal,
jadi cek `docker logs <project>-worker-1` dulu. Baris `demo_sandboxes` ditulis di transaksi pertama, sehingga sandbox
yang gagal di tengah tetap ikut dibersihkan. Job `demo.sandbox_expire` (5 menit): tenant lewat `DEMO_SANDBOX_TTL`
(24 jam) → `SUSPENDED` + refresh token anggota dicabut; data payroll final tidak dihapus (PI-05). Batas: 3/menit +
20/jam per IP, `DEMO_SANDBOX_DAILY_MAX` (500) per 24 jam → 503. Pantau:
`SELECT count(*) FROM demo_sandboxes WHERE created_at > now() - interval '1 day'`. Disk menipis: turunkan kuota atau
reset volume demo di luar jam ramai.

Simulasi gangguan pengunjung: `POST /api/v1/demo/crash-test` (3/menit per user) memasang SATU fault `panic` di
`payroll.calc_chunk.before_commit` untuk tenant sandbox itu saja (berlaku 15 menit, pemasangan ulang mengganti
yang lama). Worker tidak mati; chunk itu rollback dan diulang. `GET /api/v1/demo/runs/{id}/timeline` membaca
status chunk dari `jobs` (tanpa pesan error mentah). Fault `exit` tetap hanya untuk presenter (`X-Demo-Token`).
Cek ulang hasil pengunjung: `POST /api/v1/demo/runs/{id}/verify` menjalankan `VerifyRun` (sama dengan `payrun verify-run`)
untuk tenant sandbox pemanggil saja, dibatasi 3/menit per user; respons hanya jumlah item dan jumlah selisih.
`ok=false` di run final demo = perlakukan seperti alert reproducibility (PI-12).
