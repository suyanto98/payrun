# Step 9.3 — Environments, Migrations, Rollback, Backup & Deployment

> Bagian dari [Step 9](00-index.md). Keputusan di `00-index.md` **mengikat**: satu VM untuk kedua project, tanpa staging di V1, *recreate* dengan health gate, secret dari sops + age, backup `pg_dump` terenkripsi ke offsite, RPO 24 jam / RTO 2 jam.
> Rujukan: ADR-013 (deployment), ADR-005 (goose, expand/contract), Step 4 §6 (role DB), Step 4 `07` §2.5 (katalog RLS), Step 6 `04` §6 (rotasi kunci), C-18, C-21, C-30, Step 8 `01` (alert/health), Step 8 `03` (perf env).

---

## 1. Environment

### 1.1 Matriks environment

| Aspek | `local-dev` | `ci` | `demo` (VM publik) | `perf` | `production` (hipotetis, V2) |
|---|---|---|---|---|---|
| Tujuan | Coding & debug | Membuktikan setiap PR/commit | Demo ke HR/interviewer, portfolio online | Load test & baseline angka | Pengguna nyata |
| Pengguna | Pemilik repo | GitHub Actions | Publik (read), pemilik (demo toolbar) | Pemilik | — |
| Data | Seed kecil (`seed --profile=dev`) | Fixture per test + seed E2E | Seed demo deterministik (`seed --profile=demo`), di-reset lewat `/demo/reset` | Seed besar (`seed --profile=perf`, Step 8 `03` §4) | Data nyata |
| Image | Build lokal (`make up`) | Build dari commit | `demo` build (tag `demo`) | `release` build (tanpa demo endpoint) | `release` build |
| `DEMO_MODE` | `true` | `true` (E2E demo-verify) | `true` + `DEMO_TOKEN` | `false` | `false` |
| Mock-provider admin | Terbuka di localhost | Terbuka di network CI | `MOCK_ADMIN_ENABLED=true` + token, **tidak** dipublish Caddy | Terbuka di network internal | Tidak ada (provider nyata) |
| Log level | `debug` | `info` (dump log saat gagal) | `info` | `info` (sesuai Step 8 `03`) | `info` |
| Rate limit | Longgar (×10) | Default | Default (Step 5 §7) | **Dinonaktifkan untuk tier yang diuji**, dicatat di laporan | Default, ditinjau dari data |
| Observability | Profile `obs` opsional | Tidak ada (log + artifact) | Profile `obs` aktif (Grafana via SSH tunnel / login) | Prometheus + pg_stat_statements + auto_explain | Terkelola |
| Sumber secret | `.env.local` (nilai dummy, di-commit sebagai `.env.example`) | GitHub Actions secrets / nilai acak per run | sops+age → `.env.<service>` di VM | sops+age (file `perf`) | Secret manager |
| Masa hidup | — | Per job | Permanen | Jam (dibuat → diuji → dihapus) | Permanen |
| Catatan biaya | Gratis | Gratis (kuota Actions) | Biaya VM bulanan (lihat `04`) | Per jam, hanya saat dipakai | Lihat `04` |

### 1.2 Kenapa tidak ada staging di V1
Staging permanen menggandakan biaya VM dan beban operasional. Untuk portfolio dengan satu kontributor dan data sintetis, nilainya rendah. Fungsinya digantikan oleh empat lapis:

| Fungsi staging | Pengganti di V1 |
|---|---|
| "Apakah build ini jalan end-to-end?" | E2E Playwright di atas Docker Compose lengkap di CI (Step 8 `02`) |
| "Apakah demo masih berfungsi?" | `make demo-verify` nightly di CI |
| "Apakah migrasi aman?" | Migrasi dari nol + migrasi dari dump demo terakhir di CI (§3.7) |
| "Bagaimana kalau rilis rusak di server?" | Health gate + rollback otomatis ke SHA sebelumnya (§4, §6) |

**Pemicu menambah staging:**
- ada data pengguna nyata;
- ada lebih dari satu kontributor yang merge paralel;
- ada migrasi data besar yang perlu gladi;
- ada integrasi provider sungguhan dengan sandbox.

### 1.3 Perf environment (efemeral)
- `make perf-up PROJECT=ledgerpay` membuat VM 2 vCPU / 4 GB lewat CLI penyedia (skrip `infra/perf/create.sh`), menjalankan provisioning yang sama (§7), deploy image `release` dengan SHA tertentu, lalu seed.
- k6 berjalan dari mesin terpisah (runner CI atau VM kecil kedua), sesuai Step 8 `03` §3.
- `make perf-down` menghapus VM. Hasil (laporan k6, dump `pg_stat_statements`, profil pprof) disimpan sebagai artifact.
- Role `payrun_seed` (`BYPASSRLS`) dan `session_replication_role = replica` untuk seed **hanya ada di env ini** (Step 8 `03`). Migrasi role seed berada di `migrations_perf/` dan tidak pernah dijalankan di `demo`.

---

## 2. Configuration management

### 2.1 Prinsip
- **12-factor:** semua konfigurasi lewat environment variable. Tidak ada file config per environment di dalam image.
- **Fail fast:** `config.Load()` saat startup mem-parse ke struct (`caarlos0/env`), lalu menjalankan `Validate()`: wajib ada, format URL/DSN, panjang kunci minimum (contoh: kunci JWT ≥ 32 byte), dan kombinasi terlarang (`DEMO_MODE=true` pada image `release` → panic). Proses tidak pernah berjalan dengan konfigurasi setengah valid.
- **Prioritas:** default di kode < `.env.<service>` (dimuat oleh Compose `env_file`) < `environment:` eksplisit di Compose < env proses (override manual saat debug).
- Konfigurasi sensitif tidak pernah dicetak. Dump konfigurasi saat startup me-redact field bertag `secret:"true"` (C-18).

### 2.2 Alur secret ke VM
> **Amandemen Step 12 (R4-7/S2):** di V1 sumber secret adalah **GitHub environment secrets** (environment `demo`), bukan file sops. Job deploy merender `.env.<service>` dari secret tersebut lalu mengirimnya ke VM (`0600`). Alur sops + age di bawah ini menjadi **Production Enhancement** (lihat `04`).

```text
[V1]  GitHub environment secrets "demo"  ──render .env per service──┐
[V2]  infra/secrets/demo/*.env.sops ── sops -d (key age) ───────────┤
        ▼
CI deploy job ──scp──▶ VM: /srv/app/env/<service>.env   (owner deploy:deploy, mode 0600)
        │
        ▼
docker compose (env_file per service)
```
- Setiap service hanya menerima secret yang dibutuhkannya. **DSN owner hanya diberikan ke container `migrate`.** `api` dan `worker` hanya memegang DSN `<proj>_app`, sehingga kompromi api tidak memberi hak DDL atau bypass RLS.
- V1: mengubah secret = ubah GitHub environment secret lalu deploy ulang. (V2 dengan sops: PR ke `infra/secrets/*.sops`, riwayat perubahan terenkripsi di git.)

### 2.3 Inventaris secret

| Secret | Dipakai oleh | Disimpan di | Rotasi (Step 6 `04` §6) | Pemilik |
|---|---|---|---|---|
| `JWT_SIGNING_KEYS` (per `kid`) | api (kedua project) | sops → `api.env` | §6.1 overlap kid | Pemilik repo |
| `PROVIDER_WEBHOOK_SECRET` | ledgerpay-api, mock-provider | sops | §6.2 dual secret | Pemilik repo |
| `MERCHANT_WEBHOOK_KEK` (enkripsi `webhook_secret_enc`) | ledgerpay api/worker | sops | §6.3 (pola keyring) | Pemilik repo |
| `PII_KEYRING`, `PII_BLIND_INDEX_KEY` | payrun api/worker | sops | §6.3 | Pemilik repo |
| `DATABASE_URL_APP` (per project) | api, worker | sops | §6.4 | Pemilik repo |
| `DATABASE_URL_OWNER` (per project) | **hanya** `migrate` | sops | §6.4 | Pemilik repo |
| `POSTGRES_SUPERUSER_PASSWORD` | init Postgres, backup | sops | §6.4 | Pemilik repo |
| `MINIO_ROOT_*`, `S3_ACCESS_KEY/SECRET` (app) | minio, payrun api/worker | sops | 180 hari, pola §6.4 | Pemilik repo |
| `DEMO_TOKEN`, `MOCK_ADMIN_TOKEN` | api (demo), mock-provider | sops | Setiap kali dibagikan ke pihak lain | Pemilik repo |
| `GRAFANA_ADMIN_PASSWORD` | grafana | sops | 180 hari | Pemilik repo |
| `BACKUP_AGE_RECIPIENTS` (public key) | backup | sops (bukan rahasia, tetapi dijaga integritasnya) | Tahunan | Pemilik repo |
| `OFFSITE_S3_*` (R2/B2, write-only untuk bucket backup) | backup | sops | 180 hari | Pemilik repo |
| `SOPS_AGE_KEY` | CI deploy | GitHub env secret `demo` | Tahunan / darurat | Pemilik repo |
| `DEPLOY_SSH_KEY`, `GHCR_TOKEN` | CI | GitHub secrets | 180 hari | Pemilik repo |
| `RESTORE_DRILL_AGE_KEY` (private) | CI restore drill | GitHub env secret `restore-drill` | Tahunan | Pemilik repo |

> Kredensial offsite untuk VM dibuat **write-only** (boleh `PutObject`, tidak boleh `DeleteObject`/`GetObject`). Kalau VM dikompromikan, penyerang tidak bisa menghapus atau membaca backup lama. Retensi dihapus oleh lifecycle rule bucket, bukan oleh VM.

---

## 3. Database migrations

### 3.1 Struktur & penamaan
```text
db/
├── migrations/          # goose SQL, semua environment
│   ├── 00001_init_roles_schema.sql
│   ├── 00002_ledger.sql
│   └── 00017_add_payout_needs_reconciliation.sql
├── migrations_demo/     # hanya demo/ci: demo_faults, seed reference demo (tabel versi goose terpisah)
└── migrations_perf/     # hanya perf: role payrun_seed, setting perf
```
- Nama file `NNNNN_<kata_kerja>_<objek>.sql` dengan nomor berurutan (bukan timestamp). Dengan satu kontributor, urutan linear lebih mudah dibaca dan konflik nomor tertangkap saat merge.
- `migrations_demo` dan `migrations_perf` memakai tabel versi goose terpisah (`goose_db_version_demo`, `_perf`), sehingga tidak pernah tercampur dengan riwayat utama.

### 3.2 Cara dijalankan
```text
deploy: docker compose run --rm ledgerpay-migrate   # image yang sama: `ledgerpay migrate up`
        env: DATABASE_URL_OWNER (role <proj>_owner)
        exit 0  → lanjut restart worker/api
        exit ≠0 → deploy berhenti, app lama tetap berjalan (skema masih kompatibel, §3.3)
```
- Migrasi selalu berjalan **sebelum** app baru dinyalakan, dan selalu sebagai `<proj>_owner`. App tidak pernah menjalankan migrasi saat startup: dua replika yang start bersamaan bisa saling berlomba, dan app tidak boleh memegang hak DDL.
- `readyz` membandingkan versi skema dengan versi minimum yang dibutuhkan binary. Lihat §3.8 untuk aturannya.

### 3.3 Aturan wajib
| # | Aturan | Alasan |
|---|---|---|
| M1 | **Kompatibel dengan app N-1** (expand/contract, ADR-005). Satu rilis hanya boleh *menambah* (expand) **atau** *menghapus yang sudah tidak dipakai rilis sebelumnya* (contract), tidak keduanya untuk objek yang sama | Rollback app = deploy SHA lama tanpa down-migration |
| M2 | Migrasi yang sudah ter-apply **tidak pernah diedit**. Koreksi = migrasi baru | Riwayat skema identik di semua environment |
| M3 | Satu migrasi = satu transaksi (default goose), **kecuali** `-- +goose NO TRANSACTION` untuk `CREATE INDEX CONCURRENTLY` / `DROP INDEX CONCURRENTLY` (satu statement per file) | DDL Postgres transaksional; index concurrently tidak boleh di dalam tx |
| M4 | Setiap file diawali `SET lock_timeout = '5s'; SET statement_timeout = '60s';` | DDL yang menunggu lock memblok semua query di belakangnya (lock queue). Lebih baik migrasi gagal cepat dan diulang daripada menahan traffic |
| M5 | **Backfill data besar bukan di migrasi.** Pakai job batch idempoten (`maintenance.backfill_<nama>`, 1.000 baris per tx) | Migrasi panjang = lock panjang + deploy macet |
| M6 | Constraint baru di tabel besar: `ADD CONSTRAINT … NOT VALID` (cepat), lalu `VALIDATE CONSTRAINT` di migrasi terpisah (hanya `SHARE UPDATE EXCLUSIVE`) | Validasi tidak memblok tulis |
| M7 | `ADD COLUMN … NOT NULL DEFAULT <konstanta>` boleh (cepat sejak PG 11). Default volatil atau NOT NULL tanpa default → pola expand/contract | Menghindari rewrite tabel |
| M8 | **PayRun:** tabel tenant-scoped baru wajib di migrasi yang sama: `tenant_id NOT NULL`, PK `(tenant_id, id)`, FK composite, `ENABLE` + `FORCE ROW LEVEL SECURITY`, policy `tenant_isolation` (`USING` + `WITH CHECK`), grant ke `payrun_app`. Test katalog CI (Step 4 `07` §2.5) gagal kalau satu saja terlewat | Tabel tanpa RLS = kebocoran lintas tenant yang diam |
| M9 | **LedgerPay:** tabel append-only baru wajib trigger `forbid_update_delete` + grant `SELECT, INSERT` saja | Invariant LI-04 |
| M10 | Tidak ada `DROP` di rilis yang sama dengan berhentinya pemakaian kolom/tabel | M1 |

### 3.4 Contoh expand/contract

**(a) Rename kolom:** `payment_intents.description` → `statement_descriptor`
| Rilis | Migrasi | Kode |
|---|---|---|
| R1 (expand) | `ADD COLUMN statement_descriptor text` | Menulis ke **kedua** kolom, membaca `description` |
| — | Job `maintenance.backfill_statement_descriptor` (batch, idempoten: `WHERE statement_descriptor IS NULL`) | — |
| R2 | — | Membaca `statement_descriptor` (fallback `description` jika NULL), tetap menulis keduanya |
| R3 | `ALTER … SET NOT NULL` (setelah verifikasi `count(*) WHERE … IS NULL = 0`; untuk tabel besar: `CHECK (… IS NOT NULL) NOT VALID` → `VALIDATE` → `SET NOT NULL`) | Hanya menulis `statement_descriptor` |
| R4 (contract) | `DROP COLUMN description` | — |

Rollback aman di setiap titik: R2 → R1 tetap jalan karena R1 masih menulis kedua kolom.

**(b) Kolom NOT NULL tanpa default konstan:** `payroll_runs.eligible_count`
R1: tambah nullable + kode mengisinya untuk run baru → backfill job menghitung dari items → R2: `NOT VALID` check + `VALIDATE` + `SET NOT NULL`.

**(c) Memecah tabel:** konfigurasi webhook dari `merchants` ke `merchant_webhook_endpoints`
R1: buat tabel baru + kode menulis ke keduanya, membaca dari lama → backfill → R2: membaca dari baru → R3: berhenti menulis lama → R4: drop kolom lama. Kalau ada data yang ditulis di jendela R1–R2, dual-write menjaga keduanya tetap sinkron.

### 3.5 Index besar
```sql
-- 00031_ix_journal_entries_created.sql
-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
CREATE INDEX CONCURRENTLY IF NOT EXISTS ix_journal_entries_created ON ledgerpay.journal_entries (created_at);
```
Kalau `CONCURRENTLY` gagal di tengah, index ditinggalkan dalam status `INVALID`. Migrasi berikutnya (atau runbook) menjalankan `DROP INDEX CONCURRENTLY` lalu membuatnya ulang. `IF NOT EXISTS` saja tidak cukup, karena index INVALID sudah ada namanya.

### 3.6 Verifikasi setelah migrasi (otomatis di deploy)
1. `goose status` = versi target.
2. **PayRun:** query katalog RLS (Step 4 `07` §2.5) dijalankan terhadap DB demo; hasil harus kosong (tidak ada tabel tenant tanpa RLS/FORCE, tidak ada `SECURITY DEFINER` di luar allowlist, `payrun_app` bukan owner).
3. **LedgerPay:** semua tabel append-only punya trigger + grant yang benar (query katalog serupa).
4. Tidak ada index `INVALID` (`pg_index.indisvalid = false`).
Kalau gagal → deploy berhenti **sebelum** app baru dinyalakan.

### 3.7 Migrasi di CI
- Migrasi dari nol → katalog check → integration test (Step 8 `02`).
- **Migrasi dari snapshot demo:** job mingguan me-restore backup demo terbaru ke container (§5.5), lalu menjalankan migrasi branch. Ini gladi migrasi terhadap data berbentuk nyata, pengganti staging.
- Lint migrasi: larang `CREATE INDEX` tanpa `CONCURRENTLY` pada tabel yang ditandai besar (`journal_entries`, `jobs`, `attendance_records`, `payroll_lines`, `audit_log`), larang `DROP` tanpa label `contract` di nama file, dan wajibkan `SET lock_timeout`.

### 3.8 Kegagalan migrasi
| Situasi | Akibat | Tindakan |
|---|---|---|
| Gagal di dalam tx (constraint, lock_timeout) | Rollback penuh; versi tidak naik | App lama tetap berjalan. Perbaiki atau ulangi (lock_timeout biasanya cukup diulang di jam sepi) |
| Gagal di `NO TRANSACTION` (index concurrently) | Index `INVALID` tertinggal | Runbook §8.2 |
| Berhasil, tetapi app baru gagal health gate | Skema baru + app lama | Rollback app (§4). Aman karena M1 |

> **Aturan readiness (penting):** `readyz` harus memeriksa `versi_db >= versi_minimum_binary`, **bukan** `versi_db == versi_binary`. Dengan `==`, app lama menjadi *not ready* begitu migrasi baru ter-apply. Akibatnya jendela deploy berubah menjadi downtime, dan rollback app tidak pernah bisa *ready*. Step 8 `01` §2 perlu diselaraskan dengan aturan ini.

---

## 4. Rollback strategy

### 4.1 Filosofi
- **App:** rollback = deploy ulang **SHA sebelumnya** yang tercatat di VM (`/srv/app/releases/previous`). Selalu aman karena skema hanya di-*expand* dalam satu rilis (M1).
- **Database:** **tidak ada down-migration di demo/production.** Down-migration yang menghapus kolom berarti menghapus data yang sudah ditulis setelah deploy. Kesalahan skema diperbaiki dengan **roll forward** (migrasi baru). File `-- +goose Down` tetap ditulis untuk `local-dev` saja.
- **Restore dari backup** hanya untuk korupsi atau kehilangan data, bukan untuk "membatalkan rilis". Restore membuang semua data setelah backup (RPO 24 jam).

### 4.2 Tabel keputusan
| Yang rusak | Gejala | Tindakan | Siapa / bagaimana |
|---|---|---|---|
| Bug di app baru, data aman | Health gate gagal / error rate naik | Rollback app ke SHA sebelumnya | Otomatis (health gate) atau workflow `rollback` manual |
| Bug di app baru, data salah ditulis | Invariant metric > 0, laporan user | Matikan fitur (kill switch §4.3) → rollback app → perbaiki data dengan journal/adjustment (bukan UPDATE) | Manual, ada audit |
| Migrasi salah tetapi tidak merusak data | Query lambat, constraint salah | Roll forward dengan migrasi koreksi | PR biasa |
| Migrasi merusak data (contoh: backfill salah) | Invariant/verification gagal | Kill switch → hentikan job backfill → perbaiki dengan migrasi/job koreksi. Restore hanya jika tidak bisa direkonstruksi | Manual |
| DB korup / terhapus | Postgres tidak bisa start / data hilang | Restore backup terakhir (§5.4) | Manual, RTO 2 jam |

### 4.3 Kill switch (minimal, env-based)
Flag dibaca saat startup dan bisa diubah lewat deploy konfigurasi saja (tanpa build). V1 tidak memakai sistem feature flag: lima flag tidak membutuhkan infrastruktur.

| Flag | Default | Efek saat `false` |
|---|---|---|
| `LP_PAYOUT_SUBMIT_ENABLED` | `true` | Lane `payout.submit` berhenti claim; job tetap `queued`. `POST /payouts` tetap menerima (dana di-hold), sehingga tidak ada request yang hilang |
| `LP_WEBHOOK_DELIVERY_ENABLED` | `true` | Lane `webhook.deliver` berhenti; event menumpuk aman di jobs |
| `LP_TOPUP_ENABLED` | `true` | `POST /topups` → `503 SERVICE_UNAVAILABLE`; inbox tetap memproses webhook yang masuk |
| `PR_PAYROLL_CALC_ENABLED` | `true` | Lane `payroll.*` berhenti; `POST …/calculate` → `503` |
| `PR_PAYSLIP_RENDER_ENABLED` | `true` | Lane `payslip.render` berhenti; slip web tetap tersedia |

Prinsipnya: **jeda di sisi konsumer, bukan buang di sisi produser**. Pekerjaan yang sudah di-commit tetap di antrean dan dilanjutkan setelah flag dinyalakan lagi.

---

## 5. Backup & restore

### 5.1 Apa yang di-backup
| Aset | Di-backup? | Cara | Alasan |
|---|---|---|---|
| DB `ledgerpay` | Ya | `pg_dump -Fc` | Sumber kebenaran uang |
| DB `payrun` | Ya | `pg_dump -Fc` | Sumber kebenaran payroll |
| DB `mockprovider` | Ya | `pg_dump -Fc` | Diperlukan agar recon setelah restore tetap konsisten dengan "provider" |
| MinIO `payslips/` | **Tidak** | — | Bisa dibangun ulang dari snapshot payroll final (job `payslip.render` untuk run `FINALIZED`). Ini properti desain reproducibility (PI-12) |
| MinIO `imports/` (CSV asli) | Ya | `tar` prefix `imports/` → age → offsite | Bukti sumber data absensi; tidak bisa dibangun ulang |
| Konfigurasi & secret | Tidak (sudah di git, terenkripsi sops) | — | Repo `infra` adalah backup-nya |
| Volume Prometheus/Loki/Grafana | Tidak | — | Data observability demo bisa hilang; dashboard di-provision dari repo |

### 5.2 Cara
- Service **`backup`** di Compose (image `postgres` + `age` + `rclone` + `supercronic`) menjalankan `backup.sh` pukul **02:00 UTC**:
  ```text
  for db in ledgerpay payrun mockprovider:
      pg_dump -Fc -d $db -U <role> | age -r $OPERATOR_PUB -r $DRILL_PUB > $db-YYYYMMDD.dump.age
      # role: backup (ledgerpay, mockprovider), payrun_backup (payrun; BYPASSRLS + SELECT saja,
      # karena FORCE RLS juga berlaku untuk owner — lihat 01)
  tar imports/ | age … > imports-YYYYMMDD.tar.age
  rclone copy → offsite:backups/daily/   (hari Minggu juga → backups/weekly/)
  sha256 manifest + upload
  tulis /var/lib/node_exporter/textfile/backup.prom:
      app_backup_last_success_timestamp_seconds{db="…"} <unix>
      app_backup_size_bytes{db="…"} <n>
  ```
- Backup dienkripsi untuk **dua recipient**: key operator (offline) dan key *restore drill* (GitHub env `restore-drill`). Lihat §5.5 tentang trade-off-nya.
- Offsite: bucket S3-compatible (Cloudflare R2 atau Backblaze B2; pilihan di `04`) dengan **lifecycle rule**: `daily/` dihapus setelah 7 hari, `weekly/` setelah 28 hari. Kredensial VM write-only (§2.3).
- `pg_dump` mengambil snapshot konsisten per database (satu transaksi `REPEATABLE READ`). Antar database tidak konsisten secara atomik. Konsekuensinya: setelah restore, recon LedgerPay ↔ mockprovider bisa menemukan selisih di sekitar waktu dump, dan itu justru tugas recon.

### 5.3 Monitoring
| Alert | Ekspresi | Severity |
|---|---|---|
| `BackupStale` | `time() - app_backup_last_success_timestamp_seconds > 26*3600` | warning |
| `BackupMissing` | `absent(app_backup_last_success_timestamp_seconds)` selama 1 jam | warning |
| `BackupSizeDrop` | ukuran turun > 50% vs kemarin | warning (indikasi dump kosong) |
| `RestoreDrillFailed` | Workflow CI gagal → notifikasi GitHub | warning |

### 5.4 Prosedur restore (manual)
1. Matikan api dan worker project terkait (`docker compose stop <proj>-api <proj>-worker`). Caddy menjawab 503.
2. Ambil file dari offsite dengan kredensial **read** (disimpan offline, bukan di VM), lalu verifikasi sha256 dengan manifest.
3. Dekripsi dengan key operator: `age -d -i operator.key db.dump.age > db.dump`.
4. `DROP DATABASE … WITH (FORCE)` → `CREATE DATABASE …` (owner `<proj>_owner`) → `pg_restore --no-owner --role=<proj>_owner -d … db.dump`. Role dibuat oleh migrasi init, jadi `GRANT` ikut ter-restore.
5. Jalankan `migrate up` (kalau backup lebih tua dari versi skema app saat ini).
6. **Verifikasi:** `ledgerpay verify` (journal seimbang, saldo = Σ entries, LI-03) dan `payrun verify` (Σ item = total run, katalog RLS). Kalau gagal → **jangan** start app, investigasi dulu.
7. Start worker lalu api. Recon LedgerPay untuk tanggal sekitar backup dijalankan ulang (mismatch masuk antrean ops).
8. PayRun: jalankan job `payslip.rebuild` untuk run `FINALIZED` yang PDF-nya hilang (kalau MinIO juga hilang).
9. Catat insiden (tanggal backup yang dipakai, data yang hilang = jendela RPO).

### 5.5 Restore drill otomatis
- Workflow **mingguan** `restore-drill.yml` (setiap Senin): unduh backup terbaru → dekripsi dengan `RESTORE_DRILL_AGE_KEY` → restore ke Postgres di runner → `migrate up` dengan SHA `main` → `verify` + jalankan subset test invariant → laporkan durasi restore (indikator RTO).
- Workflow yang sama dipakai untuk "migrasi dari snapshot demo" (§3.7).
- Ditambah **drill manual bulanan** dengan key operator (memastikan key offline masih ada dan bisa dipakai).
- **Trade-off yang diterima:** key drill di GitHub berarti kompromi akun GitHub bisa mendekripsi backup. Untuk data demo sintetis, risiko ini diterima demi drill yang benar-benar berjalan. Di production, drill dijalankan di environment terisolasi dengan akses terbatas, dan key-nya tidak berada di CI publik.

### 5.6 RPO / RTO
| | V1 (demo) | V2 (production) |
|---|---|---|
| Mekanisme | `pg_dump` harian | Managed Postgres dengan PITR (atau self-hosted `pgBackRest`/`wal-g` + WAL archiving ke object storage) |
| **RPO** | **24 jam** | ≤ 5 menit (WAL archiving) |
| **RTO** | **2 jam** (VM baru + restore, §8.7) | < 30 menit (failover standby / restore PITR) |
| Diuji | Drill mingguan otomatis + bulanan manual | Drill + chaos game day per kuartal |

---

## 6. Deployment strategy

### 6.1 V1: recreate per service dengan health gate
```text
CI (main) → image :<sha> di GHCR → job deploy (SSH sebagai `deploy`):
 0. simpan SHA aktif → /srv/app/releases/previous
 1. docker compose pull <proj>-migrate <proj>-worker <proj>-api
 2. docker compose run --rm <proj>-migrate            (§3.2; gagal → stop, app lama tetap jalan)
 3. verifikasi pasca-migrasi (§3.6)
 4. docker compose up -d --no-deps <proj>-worker
      SIGTERM ke worker lama: berhenti claim, selesaikan in-flight ≤ 25 dtk (stop_grace_period 30s);
      job yang belum selesai → lease habis → diambil worker baru (at-least-once, consumer idempoten)
 5. docker compose up -d --no-deps <proj>-api
      SIGTERM: http.Server.Shutdown(ctx 10s) — tolak koneksi baru, tunggu request in-flight
 6. health gate: /readyz (port internal) 200 untuk api DAN worker dalam 60 dtk
 7. smoke test: login + satu operasi baca + (demo) satu transfer kecil dengan Idempotency-Key acak
 8. sukses → catat SHA → /srv/app/releases/current
    gagal  → deploy ulang SHA previous (langkah 4–6) + pipeline merah + notifikasi
```

**Perkiraan downtime:** jendela antara api lama berhenti dan api baru *ready*, sekitar 2–5 detik. Caddy dikonfigurasi `lb_try_duration 10s` + `lb_try_interval 250ms`. Selama jendela itu, request baru **ditahan** dan dicoba ulang ke upstream, bukan langsung dijawab 502. Caddy hanya mengulang ketika koneksi ke upstream gagal dibuka (request belum terkirim), jadi aman juga untuk `POST`. Hasilnya, user kebanyakan hanya merasakan latensi tambahan, bukan error.

**Kenapa jendela ini aman untuk uang:** request yang terputus di tengah (api lama mati setelah commit tetapi sebelum response sampai) di-retry client dengan `Idempotency-Key` yang sama, lalu mendapat replay (FS-04). Tidak ada efek ganda.

### 6.2 Evolusi
| Versi | Strategi | Pemicu |
|---|---|---|
| V1 | Recreate + health gate + `lb_try_duration` | Demo, satu instance |
| V2 | **Blue/green**: dua service api (`api-blue`, `api-green`), Caddy dengan dua upstream + `health_uri /readyz`. Deploy menyalakan warna baru, menunggu ready, lalu menghentikan warna lama. Atau **rolling** dengan 2 replika | Downtime beberapa detik tidak lagi bisa diterima; SLO ketersediaan > 99,9% |
| V3 | Orchestrator (Kubernetes/ECS/Nomad): readiness/liveness probe, rolling update `maxUnavailable=0`, worker sebagai Deployment terpisah, migrasi sebagai Job pre-deploy (Helm hook / Argo sync wave) | Banyak service, autoscaling, beberapa host (ADR-013 Future Migration) |

> Pada V2 dan V3, aturan M1 (kompatibel N-1) dan readiness `versi_db >= versi_minimum` menjadi **wajib**, karena dua versi app berjalan bersamaan di atas skema yang sama.

---

## 7. Host provisioning & hardening (C-30)

### 7.1 Keputusan alat: **cloud-init + satu skrip `bootstrap.sh` yang idempoten**
| Opsi | Kelebihan | Kekurangan |
|---|---|---|
| **cloud-init + bash idempoten** | Nol dependensi; satu host; bisa dibaca dari atas ke bawah; dijalankan ulang dengan aman | Tidak skalabel ke banyak host; idempotensi harus ditulis manual |
| Ansible playbook | Idempoten bawaan, modul siap pakai, diakui industri | Satu alat lagi untuk satu host; inventory + Python di controller |
| Terraform + Ansible | IaC penuh termasuk VM/DNS | Berlebihan untuk satu VM; state file perlu dikelola |

**Dipilih: cloud-init + bash** untuk V1. Pemicu migrasi ke Terraform (VM, DNS, bucket) + Ansible: lebih dari satu host atau environment permanen kedua. Ini dicatat sebagai Production Enhancement di `04`.

### 7.2 Isi `bootstrap.sh`
| Langkah | Detail |
|---|---|
| OS | Ubuntu LTS / Debian stable; `timedatectl set-timezone UTC` |
| User | `deploy` (tanpa sudo penuh; hanya grup `docker` + sudoers terbatas untuk `systemctl restart docker`) |
| SSH | `PasswordAuthentication no`, `PermitRootLogin no`, key-only. Key CI dibatasi `from="<rentang IP Actions>"` (opsional) |
| Firewall | `ufw default deny incoming`; allow 22, 80, 443. Port Postgres/metrics **tidak pernah** dibuka; Docker tidak dipublish selain Caddy (catatan: Docker melewati ufw untuk port yang dipublish, jadi hanya Caddy yang punya `ports:`) |
| fail2ban | Jail `sshd` |
| Update | `unattended-upgrades` (security only), reboot otomatis 04:00 jika diperlukan |
| Docker | Engine + compose plugin dari repo resmi; `daemon.json`: `log-driver json-file`, `max-size 10m`, `max-file 5`; `live-restore true` |
| Swap | 2 GB swapfile (VM 8 GB) / 4 GB (VM 4 GB) dengan `vm.swappiness=10`, sebagai jaring pengaman OOM, bukan memori kerja |
| Direktori | `/srv/app/{compose,env,releases,backups-tmp}`, owner `deploy`, `env/` mode 0700 |
| node_exporter | **Ya**, di profile `obs` (container, network internal) + textfile collector untuk metrik backup |
| Disk alert | `node_filesystem_avail_bytes / size < 15%` → warning, `< 5%` → critical |

### 7.3 DNS & TLS
- Domain murah dengan record A `ledgerpay`, `payrun`, `pay` (mock checkout), `grafana` (opsional) → IP VM, TTL 300 (memudahkan pindah VM saat DR).
- Caddy mengelola sertifikat Let's Encrypt (ACME HTTP-01) secara otomatis. Volume `caddy_data` di-backup? **Tidak**: sertifikat bisa diterbitkan ulang (perhatikan rate limit Let's Encrypt saat rebuild berulang; gunakan staging ACME saat menguji provisioning).

---

## 8. Runbook operasional (singkat)

### 8.1 Deploy gagal di health gate
1. Pipeline sudah otomatis rollback ke SHA previous. Pastikan `/readyz` 200 dan smoke test lolos.
2. Ambil log container baru dari artifact deploy (`docker compose logs --since 10m`).
3. Penyebab umum: env var baru belum ada di sops (fail fast `config.Validate`), versi migrasi minimum binary > versi DB (migrasi belum jalan), port/healthcheck salah.
4. Perbaiki lewat PR. Jangan deploy manual dari laptop.

### 8.2 Migrasi gagal di tengah
1. Migrasi transaksional → tidak ada perubahan. Ulangi deploy, atau jalankan di jam sepi jika penyebabnya `lock_timeout`.
2. `CREATE INDEX CONCURRENTLY` gagal → `SELECT indexrelid::regclass FROM pg_index WHERE NOT indisvalid;` → `DROP INDEX CONCURRENTLY <nama>;` → jalankan ulang migrasi.
3. App lama tetap berjalan sepanjang proses (M1).

### 8.3 Disk penuh
1. `df -h`, `docker system df`.
2. Pembersih aman: `docker image prune -a --filter "until=168h"` (simpan SHA current + previous), Loki retention 7 hari, Prometheus retention 15 hari, file sementara backup.
3. Cek bloat tabel `jobs`/`webhook_inbox` (job `maintenance.*` gagal?) → `VACUUM` + jalankan cleanup.
4. Postgres yang kehabisan disk berhenti menerima tulis. Api menjawab 503, dan idempotency membuat retry client aman.

### 8.4 Postgres down
1. `docker compose ps postgres`, lalu log. Penyebab umum: disk penuh (8.3), OOM (cek `dmesg`), korupsi.
2. `restart: unless-stopped` biasanya memulihkan crash biasa. Api/worker otomatis tersambung lagi (pool reconnect), dan readiness kembali hijau.
3. Kalau data korup → §5.4.

### 8.5 Restore dari backup
Lihat §5.4. Umumkan jendela data yang hilang (RPO).

### 8.6 Rotasi secret
Ikuti Step 6 `04` §6 per jenis secret. Perubahan nilai = PR ke `infra/secrets/*.sops` → deploy. Pastikan urutan overlap (kid / dual secret) diikuti agar tidak memutus sesi atau webhook.

### 8.7 VM hilang total
| Langkah | Perkiraan waktu |
|---|---|
| Buat VM baru (CLI penyedia + cloud-init + `bootstrap.sh`) | 10–15 menit |
| Ubah DNS A record (TTL 300) | 5–10 menit propagasi |
| Deploy dari CI (`workflow_dispatch` deploy SHA current) — membuat Postgres kosong + migrasi init | 10 menit |
| Restore 3 database + `imports/` (§5.4) + verify | 15–30 menit (ukuran demo) |
| Rebuild payslip PDF, recon ulang, smoke test | 10–20 menit |
| **Total** | **±1–1,5 jam → di bawah RTO 2 jam** |

---

## 9. Ringkasan disaster recovery

| Skenario | Dampak | Pemulihan | RPO | RTO |
|---|---|---|---|---|
| Rilis app rusak | Error/5xx, health gate merah | Rollback otomatis ke SHA previous | 0 | < 2 menit |
| Migrasi gagal | Deploy berhenti, app lama jalan | Perbaiki / ulangi (§8.2) | 0 | 0 (tidak ada downtime) |
| Data salah ditulis bug | Invariant > 0 | Kill switch + koreksi via journal/adjustment | 0 | Jam (manual) |
| Container crash | Restart otomatis | `restart: unless-stopped` | 0 | Detik |
| Postgres crash (data utuh) | 503 selama restart | Restart otomatis | 0 | < 1 menit |
| Disk penuh | Tulis gagal, 503 | §8.3 | 0 | 15–30 menit |
| DB korup / terhapus | Layanan mati | Restore backup (§5.4) | ≤ 24 jam | ≤ 2 jam |
| VM hilang | Semua mati | Rebuild + restore (§8.7) | ≤ 24 jam | ≤ 2 jam |
| Akun penyedia VM dikunci | Semua mati | VM di penyedia lain + restore dari offsite (bucket di penyedia **berbeda** dari VM) | ≤ 24 jam | Beberapa jam |
| Kredensial VM bocor | Risiko data | Rotasi semua secret (Step 6 §6), rebuild VM, audit. Backup lama aman karena kredensial offsite write-only | — | Jam |
| Backup offsite hilang/rusak | Tidak ada jalur restore | Terdeteksi oleh drill mingguan; buat backup baru segera | — | — |
