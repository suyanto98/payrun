# Step 10.4 — Definition of Done

> Bagian dari [Step 10](00-index.md). Milestone (M1–M7) mengikuti `00-index.md` §3.
> Dasar dari master prompt Phase 16: project baru dianggap selesai jika **core feature works, validation works, authorization works, tests pass, database migration works, Docker works, CI passes, logging exists, health check exists, critical failure scenario tested, README complete, demo scenario works**. Dokumen ini menurunkan daftar itu menjadi item yang **bisa diverifikasi**, lalu menambahkan requirement lain yang relevan untuk sistem keuangan dan multi-tenant.

**Prinsip:** setiap item DoD harus punya **bukti** yang bisa ditunjuk, berupa test hijau, output CI, URL, file, atau metrik. Item yang hanya bisa dijawab "sudah, percayalah" belum selesai.

---

## 1. DoD tingkat project (LedgerPay & PayRun)

Kolom **Milestone** menunjukkan kapan item itu wajib:
- **M3**: wajib untuk "siap melamar" (LedgerPay saja).
- **M4/M7**: wajib saat project selesai (M4 = LedgerPay, M7 = PayRun).
- **M5/M6**: milestone tengah PayRun.

### 1.1 Product
| # | Item | Bukti | Milestone |
|---|---|---|---|
| P1 | Semua fitur **P0** di feature matrix (Step 2 `01`/`03`) berfungsi end-to-end | Checklist LF/PF di README + E2E hijau | M3 (LedgerPay P0) · M7 |
| P2 | Semua fitur **P1** selesai | Checklist LF/PF | M4 · M7 |
| P3 | Fitur **P2/P3** yang tidak dikerjakan **didaftar eksplisit** di README ("Sengaja belum dibangun") beserta alasannya | Bagian README | M4 · M7 |
| P4 | Tidak ada fitur di luar matrix tanpa ADR/catatan scope | Review diff vs matrix | M4 · M7 |

### 1.2 Correctness
| # | Item | Bukti | Milestone |
|---|---|---|---|
| C1 | Setiap invariant **LI-01…LI-15** / **PI-01…PI-19** punya minimal satu test yang lulus | Matriks traceability Step 8 `02` §5 | M3 (LI P0) · M4 · M7 |
| C2 | Invariant checker berjalan di `t.Cleanup` setiap test integration/concurrency/chaos | Kode test helper | M3 · M7 |
| C3 | Metrik INVARIANT (`ledgerpay_ledger_unbalanced_journals`, `ledgerpay_ledger_balance_drift_accounts`, `payrun_tenant_isolation_violations_total`, `payrun_payroll_total_mismatch_runs`, dll.) **= 0 selama 7 hari berturut-turut** di environment demo | Panel Grafana + screenshot | M4 · M7 |
| C4 | Golden test payroll engine (≥ 30 kasus, Step 3 `06`) lulus | CI | M6 |
| C5 | Property test (journal seimbang, konservasi saldo, `net == Σ lines`, recompute-from-snapshot identik) lulus di nightly 1 juta iterasi | Log nightly | M4 · M7 |

### 1.3 Validation
| # | Item | Bukti | Milestone |
|---|---|---|---|
| V1 | Semua request divalidasi sesuai kontrak (decoder strict, `DisallowUnknownFields`, batas body) | Contract test + `mass_assignment_unknown_field_400` | M3 · M7 |
| V2 | Semua error memakai code dari katalog (Step 5 `01`); tidak ada error tak terpetakan selain `500 INTERNAL_ERROR` | `TestDBErrorMapping_AllCatalogueConstraints` + contract test | M3 · M7 |
| V3 | Uang selalu string di JSON | `TestContract_MoneyIsString` | M3 · M7 |

### 1.4 Authorization
| # | Item | Bukti | Milestone |
|---|---|---|---|
| A1 | LedgerPay: suite BOLA mencakup **setiap** resource (wallet, transfer, top-up, payout, payment intent, refund, ops) | `bola_*` hijau | M3 |
| A2 | PayRun: **authz matrix** (Step 5 `05`) mencakup 100% endpoint; endpoint tanpa baris matriks membuat CI gagal | `authz_matrix` hijau | M5 · M7 |
| A3 | PayRun: cross-tenant suite (Alpha ↔ Beta) untuk setiap endpoint tenant-scoped + RLS bypass test + pool leakage test | Suite isolasi | M5 |
| A4 | Ops endpoint LedgerPay hanya untuk role `OPS`; setiap aksi ops ter-audit dengan alasan | `ops_*` hijau | M4 |

### 1.5 Security
| # | Item | Bukti | Milestone |
|---|---|---|---|
| S1 | Semua kontrol berstatus **MVP** di Step 6 (C-01…C-46) terimplementasi, dan test-nya (Step 6 `04` §4) hijau | Checklist kontrol → test | M3 (kontrol LedgerPay P0) · M4 · M7 |
| S2 | CI scan bersih dari temuan **HIGH/CRITICAL yang bisa diperbaiki** (govulncheck, osv-scanner, trivy image) | Job CI | M3 · M7 |
| S3 | `gitleaks` atas **seluruh history** git bersih; tidak ada secret di repo (secret V1 di GitHub environment secrets; Step 12: R4-7) | Output gitleaks `--log-opts=--all` | M3 · M7 |
| S4 | Security header (C-10) hadir di semua respons | `headers_*_present` + E2E | M3 · M7 |
| S5 | Endpoint demo dan admin mock tidak ada di image release | `TestBuild_DemoEndpointsAbsentWithoutDemoMode` | M3 |
| S6 | SSRF egress guard (C-44) aktif dan teruji | `webhook_egress_*` | M4 |

### 1.6 Data & database
| # | Item | Bukti | Milestone |
|---|---|---|---|
| D1 | Migrasi jalan **dari nol** dan **sebagai upgrade** dari versi rilis sebelumnya | Job migration test | M1 · M5 |
| D2 | Migration catalogue test hijau: grant append-only, role app bukan owner, PayRun: RLS + FORCE di semua tabel tenant, tidak ada `SECURITY DEFINER` di luar allowlist, FK composite menyertakan `tenant_id` | CI | M1 (LedgerPay) · M5 |
| D3 | Backup harian berjalan + **restore drill sukses dalam 7 hari terakhir** (termasuk verifikasi jumlah baris PayRun > 0 dengan role `payrun_backup`) | Log drill + metrik `app_backup_last_success_timestamp_seconds` | M4 · M7 |
| D4 | Seed data demo deterministik (`make seed`) | Perintah + README | M3 · M7 |

### 1.7 Failure handling & concurrency
| # | Item | Bukti | Milestone |
|---|---|---|---|
| F1 | Test FS-01…FS-10 (LedgerPay) hijau | Matriks Step 8 `02` §5.3 | M3 (FS-01, 02, 04, 05, 08) · M4 (semua) |
| F2 | Test PFS-01…PFS-10 (PayRun) hijau | Idem | M6 (PFS-01, 02, 06) · M7 (semua) |
| F3 | Chaos suite (kill worker, toxiproxy untuk FS-03/PFS-04) lulus di nightly | Log nightly | M4 · M7 |
| F4 | Semua race test Step 7 (`01` §7, `02` §7) hijau dengan `-race` | CI | M3 (race LedgerPay inti) · M4 · M7 |

### 1.8 Testing
| # | Item | Bukti | Milestone |
|---|---|---|---|
| T1 | Matriks traceability **100%** untuk LI/PI/FS/PFS (setiap baris menunjuk test yang ada) | `TestTraceability_DocsReferenceExistingTests` | M4 · M7 |
| T2 | Karantina flaky **kosong** | Label `quarantine` = 0 | M4 · M7 |
| T3 | CI PR hijau di `main` untuk commit terakhir; waktu feedback PR < 10 menit | Riwayat Actions | M3 · M7 |
| T4 | Coverage package domain dilaporkan (indikatif ≥ 70%, bukan gate) | Artifact coverage | M4 · M7 |

### 1.9 Observability
| # | Item | Bukti | Milestone |
|---|---|---|---|
| O1 | Log JSON terstruktur dengan `request_id`, `correlation_id`, `trace_id`; redaction teruji | `log_redaction_*` + contoh log di README | M1 · M7 |
| O2 | Subset katalog metrik (Step 8 `01` §3) live: HTTP, DB pool, job, invariant, dan metrik bisnis inti | `/metrics` + Prometheus target UP | M3 (dasar) · M4 · M7 |
| O3 | Dashboard Grafana di-provision dari file di repo | Folder dashboards + screenshot | M4 · M7 |
| O4 | Aturan alert terdefinisi dan dimuat di Prometheus (minimal yang ada di Step 8 `01` §7) | `rules/*.yml` + `promtool check rules` di CI | M4 · M7 |
| O5 | Trace tersambung api → job → provider/merchant (span link via `jobs.trace_parent`) | Screenshot Jaeger Demo C / PFS-01 | M4 · M7 |

### 1.10 Operations
| # | Item | Bukti | Milestone |
|---|---|---|---|
| OP1 | `/healthz` dan `/readyz` sesuai Step 8 `01` §2 (readiness: versi DB ≥ versi minimum binary) | Test + curl | M1 |
| OP2 | Docker image release + Compose bisa menjalankan seluruh sistem dari clone bersih (`make up`) | Langkah README diuji di mesin bersih | M1 · M5 |
| OP3 | M3: deploy ter-script yang bisa diulang ke VM publik. M4: deploy lewat CI dengan health gate + **rollback otomatis pernah diuji minimal sekali** (deploy SHA rusak secara sengaja) | Skrip deploy / log workflow | M3 (deploy ter-script) · M4 (CD + rollback teruji) |
| OP4 | Runbook untuk alert teratas (Step 8 `01` §8) + operasional (Step 9 `03` §8) tersedia | `docs/runbooks/` | M4 · M7 |
| OP5 | RPO/RTO V1 terdokumentasi secara jujur (24 jam / 2 jam) | README "Limitations" | M4 · M7 |
| OP6 | Kill switch env (Step 9 `03`) berfungsi | Test manual tercatat | M4 · M7 |

### 1.11 Performance
| # | Item | Bukti | Milestone |
|---|---|---|---|
| PF1 | Laporan baseline load test dipublikasikan dengan **spesifikasi environment** (VM, versi, git SHA) dan wording jujur (Step 8 `03` §8) | `docs/performance.md` | M4 · M7 |
| PF2 | Keputusan eksperimen E-1 (hot merchant) / E-2 (approve stale) **dicatat** beserta datanya | ADR baru / catatan di laporan | M4 (E-1) · M7 (E-2) |
| PF3 | CI perf smoke nightly berjalan | Log nightly | M4 · M7 |

### 1.12 Documentation
| # | Item | Bukti | Milestone |
|---|---|---|---|
| DC1 | README lengkap sesuai struktur Step 11 (problem, arsitektur, demo, keputusan, cara menjalankan, limitations) | README | M3 (versi ringkas) · M4 · M7 |
| DC2 | ADR mutakhir: setiap perubahan perilaku dari desain punya ADR baru/amandemen | `docs/adr/` | M4 · M7 |
| DC3 | Diagram arsitektur (context + container) mutakhir | `docs/architecture.md` | M3 · M7 |
| DC4 | API docs dari `api/openapi.yaml` (Redocly/Swagger UI, statis) | URL / file | M4 · M7 |
| DC5 | Demo script (Step 11) | `docs/demo-script.md` | M3 · M7 |
| DC6 | Bagian **"Limitations & what I'd do next"** yang jujur (single VM, simplified tax, tanpa MFA, dll.) | README | M3 · M7 |

### 1.13 Demo
| # | Item | Bukti | Milestone |
|---|---|---|---|
| DM1 | `make demo-verify` hijau di nightly: **3 malam berturut-turut untuk M3**, **7 malam untuk M4/M7** (Step 12: R4-3) | Riwayat nightly | M3 (Demo A–C) · M4 (A–D) · M7 (A–C PayRun) |
| DM2 | Video demo (5–10 menit, idealnya ≤ 8) direkam dan ditautkan di README | URL video | M3 · M7 |
| DM3 | URL publik bisa diakses; data demo ter-reset ke kondisi awal | Cek manual | M3 · M7 |
| DM4 | Token demo (`DEMO_TOKEN`) dan admin mock aman (tidak di README, dirotasi setelah dibagikan) | GitHub environment secret + catatan rotasi (Step 12: R4-7) | M3 · M7 |

### 1.14 Hygiene
| # | Item | Bukti | Milestone |
|---|---|---|---|
| H1 | Lisensi (contoh: MIT) | `LICENSE` | M3 · M7 |
| H2 | Tidak ada `TODO`/`FIXME` di jalur kritis (ledger, idempotency, payout, tenancy, payroll run) tanpa nomor issue | `grep` di CI | M4 · M7 |
| H3 | Dependency dan base image di-pin; Dependabot aktif | `go.sum`, digest, config | M3 · M7 |
| H4 | Tidak ada kode, skema, atau penamaan dari pekerjaan kantor | Review diri + Step 1 `03` §4 | M3 · M7 |
| H5 | Conventional commits; riwayat git bisa dibaca (bukan "fix", "wip" beruntun di `main`) | Riwayat git | M3 · M7 |

---

## 2. M3 — "Siap melamar" (LedgerPay), subset minimal

Cukup untuk mulai melamar. Semua item harus ✅:

- [ ] P0 LedgerPay berjalan end-to-end: register/login, wallet, transfer, top-up + webhook, merchant payment intent, payout dengan `UNKNOWN`, audit log, health, UI minimal
- [ ] **Demo A** (idempotency), **Demo B** (100 transfer paralel), **Demo C** (worker mati di tengah payout) lulus sebagai test otomatis **dan** jalan di URL publik
- [ ] Test inti hijau di CI dengan `-race`: invariant LI P0, FS-01/02/04/05/08, race double spend & deadlock, BOLA suite
- [ ] Migrasi dari nol + catalogue test (grant append-only)
- [ ] Docker + Compose: `make up` dari clone bersih berjalan < 10 menit
- [ ] CI/CD: PR pipeline + deploy ke VM dengan health gate
- [ ] Log terstruktur + correlation ID; metrik dasar (HTTP, job, invariant) + satu dashboard
- [ ] Security dasar: argon2id + semaphore (C-45), JWT/refresh rotation, header, gitleaks history bersih, image release tanpa endpoint demo
- [ ] README versi ringkas: masalah, arsitektur (diagram), cara menjalankan, tiga demo, keputusan kunci (link ADR), limitations
- [ ] Video demo 5–10 menit
- [ ] `make demo-verify` hijau 7 malam berturut-turut

**Boleh belum ada di M3:** Demo D (recon), refund, webhook keluar, ops console lengkap, load test, tracing, backup + restore drill (D3/C-21 → M4; Step 12: R4-4), sops (ditunda), alert lengkap. Semuanya masuk sisa Sprint 8–10.

---

## 3. DoD tingkat fitur (per PR yang membawa fitur)

Sebuah fitur selesai jika:

1. **Spec**: bagian feature spec (Step 2) dan dokumen terkait diperbarui kalau perilakunya berbeda dari desain.
2. **Kontrak**: `api/openapi.yaml` diperbarui; generated code mutakhir; contract test hijau.
3. **Validasi & error**: validasi input lengkap; semua error memakai code dari katalog; constraint baru dipetakan di `platform/db`.
4. **Authorization**: policy di service layer, query di-scope ke principal/tenant; test BOLA/authz matrix/cross-tenant ditambahkan.
5. **Idempotency**: endpoint yang mengubah uang/state penting mewajibkan `Idempotency-Key`; test replay + payload berbeda.
6. **Transaksi**: boundary, lock order, dan isolation sesuai Step 7; tanpa panggilan jaringan di dalam transaksi.
7. **Migrasi**: expand-only (contract di deploy terpisah); PayRun: tabel baru langsung RLS + FORCE + policy.
8. **Test**: unit (domain) + integration (Postgres sungguhan, role app) + race test jika ada konkurensi; invariant checker lulus.
9. **Observability**: log event dengan nama stabil; metrik ditambahkan ke katalog jika relevan; tidak ada PII di log.
10. **Dokumentasi**: ADR baru/amandemen jika keputusan berubah; README/demo script diperbarui jika terlihat oleh user.

---

## 4. DoD tingkat sprint (template)

```markdown
## Sprint N — <tema> (YYYY-MM-DD → YYYY-MM-DD)

### Objective
<satu kalimat>

### Done jika
- [ ] Semua deliverable sprint ter-merge ke `main` dan CI hijau
- [ ] Setiap fitur memenuhi DoD fitur (§3)
- [ ] Test baru tercatat di matriks traceability
- [ ] Demo kecil sprint ini bisa dijalankan (`make demo-sprint-N` atau langkah manual tercatat)
- [ ] Open items/keputusan baru dicatat (ADR/amandemen)
- [ ] Deploy ke VM demo berhasil (mulai Sprint 7 LedgerPay / Sprint 1 PayRun)
- [ ] Retrospektif singkat: apa yang meleset dari estimasi dan kenapa

### Dipindah ke sprint berikutnya (dengan alasan)
- …
```

---

## 5. PR checklist (salin ke `.github/pull_request_template.md`)

```markdown
## Ringkasan
<apa dan kenapa; link LF/PF/ADR/FS>

## Checklist
- [ ] Spec/ADR diperbarui (atau tidak relevan)
- [ ] OpenAPI diperbarui + contract test
- [ ] Error memakai code katalog; constraint baru dipetakan
- [ ] Authorization: policy + test (BOLA / authz matrix / cross-tenant)
- [ ] Idempotency (jika mengubah uang/state penting)
- [ ] Transaksi sesuai Step 7 (lock order, tanpa network di dalam tx)
- [ ] Migrasi expand-only; PayRun: RLS + FORCE + policy untuk tabel baru
- [ ] Unit + integration test; race test jika konkuren; invariant checker lulus
- [ ] Log/metrik ditambahkan; tidak ada PII/secret di log
- [ ] Tidak ada TODO di jalur kritis tanpa issue
- [ ] Screenshot/log bukti (untuk perubahan UI atau perilaku demo)
```

> Karena proyek solo, branch protection memakai "conversation resolution" dan checklist ini (Step 9 `02`). Checklist berfungsi sebagai review diri yang eksplisit.

---

## 6. Checklist kesiapan demo (sebelum interview)

| # | Item | Cara cek |
|---|---|---|
| R1 | Data demo di-reset ke kondisi awal (saldo, merchant, tenant Alpha/Beta) | `POST /api/v1/demo/reset` lalu verifikasi saldo awal |
| R2 | Semua skenario failure **mati** (mock scenario `ok`, `demo_faults` kosong) | Admin mock + query |
| R3 | `DEMO_TOKEN` masih rahasia (tidak pernah ada di bundle SPA; ditempel presenter saat demo); rotasi kalau pernah dibagikan | GitHub environment secret |
| R4 | `make demo-verify` hijau tadi malam | Riwayat nightly |
| R5 | Latency dari lokasi interview wajar (< 300 ms ke VM) | `curl -w` |
| R6 | Dashboard Grafana dan Jaeger sudah terbuka di tab, rentang waktu "last 15 minutes" | Browser |
| R7 | Terminal siap dengan script curl/k6 per demo; font besar | Latihan |
| R8 | **Video cadangan** siap diputar offline | File lokal |
| R9 | **Fallback jika VM mati:** `make up demo` lokal sudah di-pull dan teruji di laptop | Uji H-1 |
| R10 | Penjelasan "limitations" siap diucapkan (single VM, simplified tax, tanpa MFA) | Demo script Step 11 |
| R11 | **Freeze policy (Step 12: R4-12):** tidak ada deploy atau migrasi dalam 24 jam sebelum interview; data demo di-reset H-1 dan diverifikasi | Riwayat deploy + checklist H-1 |
| R12 | Jaeger/Loki (`obs-full`) dinyalakan **hanya** jika demo memerlukan trace/log terpusat; cek memori VM setelahnya | `docker stats` |

---

## 7. "Belum selesai" yang terlihat selesai (anti-contoh)

| Terlihat selesai | Kenapa belum selesai |
|---|---|
| Test ada, tetapi di-`Skip` atau hanya jalan di laptop | Test yang tidak berjalan di CI tidak membuktikan apa-apa |
| Demo jalan di lokal, tetapi URL publik error atau datanya kotor | Interviewer melihat URL publik, bukan laptop Anda |
| Metrik ada, tetapi tidak ada alert dan dashboard | Tidak ada yang tahu ketika sistem rusak |
| README menyebut "handles 1,000 RPS" tanpa spesifikasi environment | Klaim tanpa konteks = merah di mata interviewer senior (Step 8 `03` §8) |
| Fitur berfungsi, tetapi tanpa authz test | BOLA adalah bug yang paling sering ditanyakan, dan paling mudah ditemukan |
| Idempotency "sudah ada", tetapi key disimpan di luar transaksi efek | Melanggar ADR-L02; replay bisa menghasilkan efek ganda |
| Migrasi jalan dari nol, tetapi belum pernah diuji sebagai upgrade | Deploy kedua yang akan gagal |
| Backup berjalan, tetapi restore belum pernah dicoba | Backup yang belum pernah di-restore belum terbukti bisa dipakai |
| Payroll menghasilkan angka, tetapi tanpa golden test | Tidak ada yang tahu apakah angkanya benar |
| CI hijau karena test flaky di-retry sampai lulus | Melanggar kebijakan flaky (Step 8 `02` §7) |
| Dokumen desain tidak lagi cocok dengan kode | Interviewer membaca keduanya; inkonsistensi menurunkan kepercayaan |
