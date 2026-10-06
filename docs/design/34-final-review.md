# Step 12.1 — Final Technical Review (terkonsolidasi)

> Sumber: empat review independen di [`_reviews/`](_reviews/) (R1 konsistensi/transaksi/race, R2 keamanan, R3 data/API/skalabilitas, R4 scope/test/observability/deploy) + temuan saat menyusun PRD (`03`, `04`).
> **Keempat temuan Critical diverifikasi ulang secara manual terhadap isi file sebelum diterima.**
> Kolom **Status**: ✅ diperbaiki di dokumen sumber pada Step 12 · 📝 dicatat sebagai keputusan di sini (dokumen ini otoritatif) dan diterapkan saat coding.

---

## 1. Ringkasan

| Severity | Jumlah | Tema dominan |
|---|---:|---|
| **Critical** | 4 | Claim inbox tanpa lease; dua celah di policy RLS PayRun; FK tanpa index di jalur finalize |
| **Important** | 33 | Edge case state machine, dedupe settlement, kebocoran kolom sensitif, audit bacaan gaji, jadwal yang terlalu optimistis, stack observability terlalu berat, test/alert yang belum terkatalog, pernyataan basi |
| **Nice to have** | 13 | Hardening tambahan (SSRF IPv6, MFA ops), optimasi kecil, penyeragaman format |

**Penilaian umum:** tidak ditemukan microservice atau teknologi besar yang tidak terjustifikasi di jalur V1 (R4). Arsitektur inti (modular monolith, Postgres sebagai sumber kebenaran, outbox, RLS) bertahan dari review. Masalah yang ditemukan ada di **detail implementasi** (predikat SQL, policy, index) dan **realisme jadwal**, bukan di arah desain.

---

## 2. Critical (harus diperbaiki sebelum coding bagian terkait)

### C1 — Claim inbox webhook mengabaikan lease (R1-1) · ✅
- **Bukti:** Step 7 `01` §2.6, claim `… WHERE status='RECEIVED' AND next_attempt_at <= now()` tidak memeriksa `locked_until`.
- **Dampak:** event yang sedang diproses (verify-by-fetch 3–10 dtk) diklaim ulang setiap polling 500 ms, sehingga `attempts` naik terus. Dalam ±4 dtk event mencapai batas 8, lalu `DEAD`. Top-up yang sah tidak terkredit sampai ops me-replay. Uang tidak dobel (fencing bekerja), tetapi fungsi intinya rusak.
- **Perbaikan:** predikat claim ditambah `AND (locked_until IS NULL OR locked_until < now())`. `attempts` hanya naik saat percobaan gagal atau lease kedaluwarsa. Test baru `TestInbox_SlowProcessingNotReclaimed`.

### C2 — Policy RLS "self" aktif di dalam transaksi tenant (R2-1) · ✅
- **Bukti:** Step 4 `07` §2.4: `WithTenantTx` juga menyetel `app.user_id`. Policy permisif (`memberships_self_read`, cabang self `tenants_current`, `users_self`) digabung dengan **OR**.
- **Dampak:** user yang menjadi anggota beberapa tenant melihat baris membership/tenant miliknya di tenant lain saat berada di konteks tenant A. Akibatnya ada kebocoran metadata lintas tenant, atau C-46 menembak 500 sebagai false positive dan memicu alert critical.
- **Perbaikan:** **mode eksklusif.** `WithTenantTx` hanya menyetel `app.tenant_id`; `WithPlatformTx` hanya menyetel `app.user_id`. Setiap policy self ditambah syarat `payrun.current_tenant() IS NULL`, sehingga policy itu hanya aktif di mode platform. Signup (butuh keduanya) berjalan dalam satu transaksi, berurutan: mode platform (buat user), lalu `set_config('app.tenant_id', …, true)` (buat tenant + membership). Test baru `TestRLS_SelfPoliciesInactiveInTenantMode`.

### C3 — Cast `::uuid` mentah di policy → error acak di koneksi pool (R2-2, mencakup R3-9) · ✅
- **Bukti:** Step 4 `01` (policy `users_*`, `audit_tenant`, `idempotency_keys`) dan template di Step 4 `07` §2 masih `current_setting('app.x', true)::uuid`.
- **Dampak:** setelah GUC kustom pernah disetel di suatu session (termasuk `SET LOCAL`), `current_setting(..., true)` mengembalikan `''`, bukan `NULL`. Cast `''::uuid` gagal, sehingga login dan audit gagal secara acak tergantung koneksi pool mana yang didapat.
- **Perbaikan:** semua policy memakai helper `payrun.current_tenant()` / `payrun.current_user_id()` (`STABLE`, `nullif(…, '')::uuid`), dibungkus subselect `(SELECT payrun.current_tenant())` agar dievaluasi sekali per query (sekaligus R3-9). Test catalogue: tidak boleh ada `current_setting(` mentah di definisi policy (`pg_policies`).

### C4 — FK `payslips.item_id` tanpa index di jalur DELETE finalize (R3-1) · ✅
- **Bukti:** Step 4 `06` §payslips: `item_id` FK → `payroll_items` tanpa index. Step 7 `02`: finalize men-`DELETE` item `calc_version` lama.
- **Dampak:** setiap baris parent yang dihapus memicu cek FK berupa seq scan atas seluruh `payslips` (±1 juta baris/tahun dalam skenario volume), sehingga finalize melewati `statement_timeout` dan tidak pernah berhasil.
- **Perbaikan:** index `ix_payslips_item (tenant_id, item_id)`. Pembersihan versi lama dipindah keluar dari transaksi finalize (R3-2). Test catalogue baru: **setiap FK harus punya index yang diawali kolom FK-nya** (query `pg_constraint` × `pg_index`) di kedua project.

---

## 3. Important

### 3.1 Konsistensi, transaksi, race (R1)
| ID | Temuan | Keputusan | Status |
|---|---|---|---|
| R1-2 (O19) | Fencing inbox menulis status final di awal tx | Fencing = `SELECT … WHERE id=$ev AND locked_by=$w AND status='RECEIVED' FOR UPDATE` (0 baris → rollback), lalu **satu** `UPDATE` final di akhir tx. **O19 ditutup** | ✅ |
| R1-3 | Run 0 karyawan macet di `CALCULATING` | `payroll.plan`: k = 0 → di tx yang sama total 0, status `CALCULATED`, warning `NO_ELIGIBLE_EMPLOYEES`. Test `TestPayrollRun_ZeroEligibleCompletes` | ✅ |
| R1-4 | `adjustments.target_period_id` merujuk periode yang belum ada | Simpan target sebagai `(target_year, target_month)`, bukan FK. Diresolusi saat run periode itu dibuat | ✅ |
| R1-5 | `PROVIDER_SETTLEMENT` dobel jika recon diulang dengan file berbeda | Source journal = `('provider_settlement', <topup_id>)`, bukan `recon_item`, sehingga `UNIQUE (source_type, source_id, purpose)` menjaga per top-up | ✅ |
| R1-6 | Tabel constraint → error di Step 4 basi | **Katalog Step 5 `01` otoritatif.** Tabel di Step 4 `04` §8 dan `07` §7 diganti rujukan ke katalog | ✅ |
| R1-7 | Approve memegang `payroll_periods FOR UPDATE` selama verifikasi ribuan kandidat | Jika kandidat > **200** → langsung `409 RUN_STALE` (recalculate lebih murah daripada verifikasi di bawah lock). Di bawah ambang: verifikasi seperti O12 | ✅ |
| R1-8 | TOCTOU pembuatan periode saat create run → `23505` tak terpetakan | `tenant_settings FOR UPDATE` → `INSERT … ON CONFLICT DO NOTHING` → `SELECT`. `uq_payroll_periods_month` ditambahkan ke pemetaan error | ✅ |

### 3.2 Keamanan (R2)
| ID | Temuan | Keputusan | Status |
|---|---|---|---|
| R2-3 | Audit platform (`tenant_id` NULL) tidak terbaca siapa pun | Policy tambahan `audit_platform_read` untuk role `payrun_readonly` saja (investigasi) | ✅ |
| R2-4 | Cara toolbar demo mendapat `DEMO_TOKEN` tidak didefinisikan | Token **tidak pernah** ada di bundle SPA. Toolbar meminta presenter menempelkan token; disimpan di memori tab saja (bukan storage). Endpoint demo juga dibatasi rate limit `heavy` | ✅ |
| R2-5 | `403 ACCOUNT_LOCKED` membocorkan keberadaan akun | Login terkunci → respons **sama** dengan kredensial salah (`401 INVALID_CREDENTIALS` + `Retry-After`). Limiter IP+email juga berlaku untuk email yang tidak ada. `ACCOUNT_LOCKED` dihapus dari respons publik | ✅ |
| R2-6 | List slip/item gaji tanpa audit per akses | Setiap request list yang memuat nominal gaji orang lain menghasilkan **satu** baris audit (`payroll.bulk_read`, run_id, jumlah baris). Ini juga masuk daftar operasi sensitif yang memeriksa ulang membership (sekaligus R2-12) | ✅ |
| R2-7 | `users_tenant_member_read` membuka semua kolom `users` termasuk `password_hash` | `REVOKE SELECT (password_hash) ON users FROM payrun_app` (column privilege; hash hanya lewat `auth_lookup_user`), dan policy hanya untuk membership `ACTIVE` | ✅ |
| R2-8 | Tanpa verifikasi email, guard "email undangan harus cocok" lemah | Verifikasi email (link sekali pakai) **wajib sebelum accept undangan dan sebelum signup tenant aktif** (PayRun). Di MVP tanpa SMTP: Mailpit di dev/demo | 📝 (PF-02, roadmap PR S1 +1,5 jam) |

### 3.3 Data, API, skalabilitas (R3)
| ID | Temuan | Keputusan | Status |
|---|---|---|---|
| R3-2 | Penghapusan versi lama di dalam tx finalize memblok penulis input | Job `payroll.cleanup_versions` (batch 1.000 baris) setelah `CALCULATED`/`FINALIZED`. Query membaca hanya `calc_version` terkini | ✅ |
| R3-3 | List leave/overtime/adjustments/audit `created_at DESC` tanpa index | Tambah `(tenant_id, created_at, id)` pada keempat tabel | ✅ |
| R3-4 | Cursor/filter item payroll memakai kolom yang tidak ada | Denormalisasi `employee_no`, `employee_name`, `department_id` ke `payroll_items` saat kalkulasi (benar untuk run historis) + index review | ✅ |
| R3-5 | Diff vs bulan lalu berisiko N+1 | Cari run pembanding sekali, lalu satu join per halaman. PF-23 kini P2 (S9) | 📝 |
| R3-6 | `app_job_queue_depth` memindai seluruh `jobs` setiap 15 dtk | Hitung hanya `status IN ('queued','running')` lewat partial index; interval 30 dtk | 📝 |
| R3-7 | `ledger.verify` per jam = full scan dalam snapshot panjang | Inkremental per jam (akun yang punya entry baru sejak checkpoint) + full scan harian di luar jam sibuk | 📝 |
| R3-8 | Pernyataan basi (readiness `=`, `FOR UPDATE` akun) | Diperbarui di Step 5 `02`/`04`, Step 2 `02`, Step 4 `04` | ✅ |

### 3.4 Scope, test, observability, deploy (R4)
| ID | Temuan | Keputusan | Status |
|---|---|---|---|
| R4-1 | Waktu UI kedua SPA kurang 2–3× | Reinvestasi **+10 jam** UI (dari penyederhanaan §5); master data PayRun lewat seed + API (S12) | 📝 roadmap |
| R4-2 | Deploy publik pertama hanya 3,5 jam | **+3 jam** deploy awal + **1,5 jam** video di LP S7 | 📝 roadmap |
| R4-3 | DM1 "7 malam hijau" bertentangan dengan M3 | M3: `demo-verify` hijau **3 malam berturut-turut**; 7 malam untuk M4/M7 | ✅ (DoD) |
| R4-4 | DoD M3 menuntut kontrol S8 (backup, sops) | Backup/restore drill pindah ke M4; sops ditunda (S2) | ✅ (DoD) |
| R4-5 | CD + rollback + drill terlalu sedikit jam | **+3,5 jam** di LP S8 | 📝 roadmap |
| R4-6 | Stack observability terlalu berat untuk satu VM | **V1 = Prometheus + Grafana + node_exporter.** Loki, Promtail, Jaeger, dan Alertmanager di profile opsional `obs-full` (dinyalakan saat demo tracing/log). Rule alert tetap dievaluasi Prometheus + `promtool` di CI | ✅ |
| R4-7 | sops + age tidak sepadan di V1 | Secret runtime dari **GitHub environment secrets** → ditulis ke `.env` (0600) saat deploy. sops = Production Enhancement | ✅ |
| R4-8 | Repo `infra` + image `edge` dua SPA = coupling | Selama LedgerPay: folder `deploy/` di repo `ledgerpay`, SPA dibawa image project. Repo `infra` dibuat di **PayRun S0** saat VM mulai dibagi | ✅ |
| R4-9 | Image release di-push tetapi tidak di-deploy | Image release dibangun dan dipindai, **tidak di-push** di V1 | ✅ |
| R4-10 | Test yang ditambahkan setelah Step 8 belum terkatalog | Ditambahkan ke katalog/traceability Step 8 `02` (C-45, C-46, C-21, C1–C4 di atas, test demo PayRun) | ✅ |
| R4-11 | Alert backup basi, disk, dan TLS tidak ada | Tambah A-14 backup basi (> 26 jam), A-15 disk > 85%, A-16 sertifikat < 14 hari | ✅ |
| R4-12 | Satu Postgres untuk kedua project tanpa freeze | **Freeze policy:** tidak ada deploy/migrasi dalam 24 jam sebelum interview; reset data demo H-1; checklist kesiapan demo (Step 10 `04`) | ✅ (DoD) |
| R4-13 | Sprint performa LedgerPay terlalu penuh | Subset skenario (S7 §5): LP-2, LP-3, LP-4 (E-1), LP-9; seed 1 juta journal | 📝 roadmap |
| R4-14 | PayRun S3: engine + 20 golden dalam 8 jam | **+2 jam**; golden bertahap: 12 kasus di S3, sisanya di S5 (tetap 30 di M6) | 📝 roadmap |

### 3.5 Inkonsistensi dari penyusunan PRD
| Temuan | Perbaikan | Status |
|---|---|---|
| LI-05 dan ringkasan FS-01 masih `(client_id, endpoint, key)` | → `(client_type, client_id, endpoint, key)` | ✅ |
| Endpoint ops API key berlabel P0, sedangkan ops console P1 | Role `OPS` + rute `/ops/*` minimal (API key saja) masuk P0 di LP S4; ops console lengkap tetap P1 | 📝 roadmap |
| Step 1 §6.3 masih menaruh rate limiting di Nice to have (Redis) | Catatan "lihat Step 2 final: P1, in-memory" | ✅ |
| Diagram W1 Step 1 belum memakai inbox + verify-by-fetch | Catatan rujukan ke ADR-L08 | ✅ |
| PI-13 Step 3 masih "bandingkan fingerprint seluruh run" | Diperbarui ke mekanisme Step 7 O12 (change log + verifikasi kandidat) | ✅ |
| `INVALID_STAGE_REFERENCE` di Step 3 `06` tidak ada di katalog | → `INVALID_COMPONENT_REFERENCE` | ✅ |
| Toxiproxy di ringkasan FS Step 3 | Opsional, lalu dihapus (S11) | ✅ |

---

## 4. Nice to have
| ID | Temuan | Keputusan |
|---|---|---|
| R1-9 | Churn MultiXact dari shared lock tiap clock-in | Ukur di load test; alternatif: clock-in pada tanggal dalam periode tanpa run aktif tidak perlu `FOR SHARE` period |
| R1-10 | Change log hanya ditulis aplikasi | Test arsitektur: query tulis ke tabel input di luar `PeriodGuard` gagal di CI |
| R1-11 | Format dedupe & ambang `topup.recover` tidak seragam | Kanonik = Step 7 `03` (`payroll.calc_chunk:{run}:{v}:{n}`, `payroll.plan:{run}:{v}`, recover > 30 dtk) |
| R2-9 | Blocklist SSRF belum lengkap (IPv4-mapped IPv6, NAT64, 6to4) | Tambahkan ke C-44 saat implementasi |
| R2-10 | `jobs` tanpa pembatasan UPDATE/SELECT lintas tenant | Diterima (worker memang lintas tenant); handler hanya memakai `jobs.tenant_id` (C-43) |
| R2-11 | Ops: pencarian email tidak diaudit, tanpa MFA | Audit `ops.search`; MFA = Production Enhancement |
| R2-12 | Penurunan role berlaku ≤ 15 menit | Bacaan gaji massal masuk daftar recheck (lihat R2-6) |
| R3-10 | Filter `type` riwayat wallet untuk tipe jarang | Index parsial jika load test membuktikan |
| R3-11 | Churn snapshot JSONB per recalculation | `ALTER … SET COMPRESSION lz4` + cleanup batch (R3-2) |
| R4-15 | `lb_try_duration` bisa menyamarkan crash loop | Alert `app_ready == 0` > 1 menit tetap menembak |
| R4-16 | OTel "sejak V1" vs tracing di S7 | Instrumentasi kode sejak S1 (murah); backend Jaeger dinyalakan di S7 (profile `obs-full`) |
| R4-17 | Endpoint demo terlalu banyak | Hanya `faults`, `reset`, `fast-track` (S6) |
| O16 | Rotasi secret webhook merchant dengan dual-signature | **Production Enhancement**; V1 merotasi lewat ops dengan window singkat (didokumentasikan di runbook Step 6 §6.2). **O16 ditutup** |

---

## 5. Keputusan penyederhanaan (R4, diterima seluruhnya)

Basis roadmap 337,5 jam. Penyederhanaan menghemat **±51 jam (15,1%)**, sekitar 20 jam diinvestasikan ulang untuk menutup kekurangan anggaran (R4-1/2/5/14), dan sisanya **±31 jam menjadi buffer** (± 2 sprint).

| # | Penyederhanaan | Jam |
|---|---|---:|
| S1 | Alertmanager tidak di V1 (rule tetap dievaluasi + `promtool`) | 1,5 |
| S2 | Secret dari GitHub environment secrets; sops ditunda | 3 |
| S3 | Repo `infra` + image `edge` baru di PayRun S0 | 4 |
| S4 | Image release tidak di-push | 1,5 |
| S5 | `deploy-gate` forced command → Production Enhancement (V1: user `deploy` dengan key khusus) | 2 |
| S6 | Endpoint demo hanya `faults`, `reset`, `fast-track` | 3 |
| S7 | Perf: subset skenario, seed 1 juta journal, perf smoke ditunda | 8 |
| S8 | Recon untuk Demo D lewat API/curl (tanpa UI ops recon) | 2 |
| S9 | PF-23 (diff vs bulan lalu) P1 → P2 | 3 |
| S10 | PF-21 (import CSV) P1 → P2 | 6 |
| S11 | Tanpa toxiproxy | 2 |
| S12 | UI master data PayRun → seed + API | 3 |
| S13 | Lembur satu multiplier (tier ditunda) | 1 |
| S14 | Penugasan atasan tidak effective-dated (kolom atasan saat ini + audit; approver tetap di-snapshot). Effective dating tetap didemokan lewat salary | 2,5 |
| S15 | Dashboard 6 → 3 | 2 |
| S16 | Ruleset Redocly kustom ditunda | 1,5 |
| S17 | Meta test traceability ditunda ke M4 | 2 |
| S18 | Skenario `settlement_mismatch` satu varian (status) | 1 |
| S19 | CI perf smoke ditunda sampai setelah M4/M7 | 2 |
| | **Total** | **51** |

> **Status:** keputusan di §5 dan baris 📝 **otoritatif mulai sekarang** dan menggantikan teks lama di dokumen sumber. Roadmap (Step 10 `02`/`03`) diperbarui dengan catatan amandemen di bagian atasnya (lihat §6), bukan ditulis ulang per baris. Detail jam per task disesuaikan saat sprint planning.

---

## 6. Dokumen yang diubah pada Step 12
Daftar lengkap ada di `00-index.md` (bagian "Perubahan yang diterapkan"). Semua baris ✅ sudah diterapkan dan diverifikasi.

## 7. Sisa untuk saat coding
| # | Item |
|---|---|
| O23 | Nama test kanonik (beberapa nama Step 3/7/11 berbeda; pakai katalog Step 8 `02`) |
| O25 | Sintaks Caddyfile untuk menghapus query string dari access log |
| — | Verifikasi `EXPLAIN` untuk query claim job (amandemen ADR-006) dan query panas lain (Step 8 `03` §7) |
| — | Hasil eksperimen E-1/E-2 menentukan apakah `MERCHANT_PENDING` / verifikasi stale async dibangun |
