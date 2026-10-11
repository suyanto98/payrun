# PayRun Roadmap (Sprint 0–10)

> Kerangka mengikat: `00-index.md`. Sprint 1 minggu, ± 14–16 jam. **PayRun dimulai setelah LedgerPay M4.**
> Nama sprint mengikuti tema master prompt sebagai *fokus utama* (§2 index). Test, keamanan dasar, log, dan CI dikerjakan **sejak Sprint 1**, tidak ditunda ke sprint bertema.
> Milestone: **M5** tenant isolation proven (akhir S2) · **M6** payroll correct & resumable (akhir S5) · **M7** PayRun selesai (akhir S10).


## ⚠️ Amandemen final review (otoritatif, menggantikan baris task yang bertentangan di bawah)

Sumber: `34-final-review.md` §3 dan §5. Jam per task diatur ulang saat sprint planning.

| Sprint | Perubahan | Jam |
|---|---|---:|
| S0 | **Repo `infra` + image `edge` dibuat di sini** (sebelumnya selama LedgerPay cukup `deploy/` di repo `ledgerpay`) | +2 (dipindah dari LP) |
| S1 | **Verifikasi email** (link sekali pakai; Mailpit di dev/demo) sebelum accept undangan dan aktivasi tenant (R2-8). **Mode eksklusif RLS + helper `current_tenant()`** dan catatan `nullif` (C2/C3); test `TestRLS_SelfPoliciesInactiveInTenantMode`, `TestCatalog_NoRawCurrentSettingInPolicies` | +1,5 |
| S2–S3 | UI master data (departemen/posisi/komponen) diganti **seed + API** (S12). Penugasan atasan **tidak effective-dated** (S14); effective dating didemokan lewat salary | −5,5 |
| S3 | Engine + golden **+2 jam**; golden bertahap: **12 kasus di S3**, sisanya di S5 (tetap 30 di M6). Lembur satu multiplier (S13) | +2 / −1 |
| S4–S6 | Kolom denormalisasi `payroll_items` (R3-4), ambang 200 kandidat saat approve (R1-7), run 0 karyawan (R1-3), job `payroll.cleanup_versions` (R3-2), `ix_payslips_item` (C4), audit `payroll.bulk_read` (R2-6), target adjustment `(year, month)` (R1-4) | dalam task terkait |
| S8 | **PF-21 (import CSV) → P2**; tidak dikerjakan kecuali ada buffer | −6 |
| S9 | Perf subset: **PR-1, PR-3, PR-4 (E-2)**; **PF-23 (diff) → P2** | −3 / −4 |
| Semua | UI payroll run + self-service **+5 jam** | +5 |
| | **Perkiraan netto:** ±12 jam dibebaskan → buffer PayRun | |

---

## 0. Apa yang lebih cepat dan apa yang baru dibanding LedgerPay

### 0.1 Dipakai ulang (menghemat ± 1,5 sprint)
| Aset dari LedgerPay | Bentuk reuse | Penghematan |
|---|---|---|
| Platform skeleton (`config`, `db.WithTx`, `httpx`, `auth`, `observability`, `clock`, `idgen`) | Disalin lalu di-rename (strategi produk §1.1) | Sprint 0 tidak perlu mendesain ulang fondasi |
| Job runner (`jobs` table, claim `SKIP LOCKED`, lease/heartbeat, fencing `CompleteInTx`, `jobs.Permanent`, OnDead) | Disalin utuh + ditambah `tenant_id` dan trigger `jobs_enforce_tenant` (C-43) | Async payroll bisa masuk **S4**, sebelum sprint bertema "Background" |
| Auth (argon2id + semaphore C-45, refresh rotation + reuse detection, CSRF header) | Disalin; ditambah `tid` claim, `X-Active-Tenant`, `auth_lookup_user` | ± 6 jam |
| CI workflows, lint config, Redocly ruleset, trivy/gitleaks | Disalin; path dan nama image disesuaikan | ± 4 jam |
| Repo `infra` (VM, Caddy `edge`, observability stack, backup timer, deploy-gate, sops) | Ditambah entri `payrun` | Deploy publik cukup ± 4 jam di S8 |
| Konvensi metrik/log, dashboard template, runbook | Disalin dengan prefix `payrun_` | ± 2 jam |
| Invariant checker di `t.Cleanup`, template DB per test, fixture builder | Disalin; fixture menjadi `fixture.Tenant("alpha")` | ± 3 jam |

### 0.2 Baru (tempat risiko PayRun berada)
| Hal baru | Kenapa berisiko | Sprint |
|---|---|---|
| RLS + `FORCE` + `WithTenantTx` / `WithPlatformTx` + catalogue test | Isolasi palsu (owner bypass, `SET` session bocor) | S1 |
| Tenant guard defensif C-46 + composite FK | Kebocoran yang tidak terdeteksi | S2 |
| Effective dating (`daterange` + `EXCLUDE`, PeriodGuard yang memeriksa rentang berubah) | Off-by-one tanggal, kenaikan gaji terblokir | S3, S5 |
| Payroll engine pure + golden test + decimal/rounding | Selisih Rp1, hasil tidak deterministik | S3 |
| Run state machine + chunk txRead/txWrite + `calc_version` | Item dobel, zombie chunk | S4 |
| Approval engine generik + stale detection berbasis change log (O3/O12/D1) | Race halus di Read Committed | S5 |
| PII encryption (AES-GCM + AAD + blind index) | Ciphertext swapping, kebocoran lewat log | S2, S4 |
| Object storage + presigned URL + upload CSV | Akses lintas tenant lewat URL/file | S7, S8 |

---

## 1. Ringkasan sprint

| Sprint | Tema (fokus) | Isi utama | Milestone |
|---|---|---|---|
| S0 | Product + Architecture | Port skeleton, repo `payrun`, infra (DB `payrun` + role, MinIO), OpenAPI skeleton, review ADR | — |
| S1 | Foundation | Tenancy + IAM inti, `WithTenantTx`/`WithPlatformTx`, RLS catalogue test, isolation harness | — |
| S2 | Core Domain | Undangan, org master, employee + PII minimal, tenant guard, cross-tenant + authz matrix test, UI awal | **M5** |
| S3 | Critical Business Logic | Org assignment effective-dated, komponen + salary effective-dated, **payroll engine + golden** | — |
| S4 | Security *(+ critical logic II)* | **Run lifecycle + async chunk + resume**, role grant + owner lock, PII masking/full-view | — |
| S5 | Testing | Approval engine, submit/approve + stale (O3/O12/D1), suite concurrency, golden 30, Demo B | **M6** |
| S6 | Background Processing *(+ input domain)* | Attendance, leave, overtime → input engine, finalize + period lock, UI self-service | — |
| S7 | Observability | Metrik/dashboard/alert/tracing PayRun, payslip web + PDF + MinIO, adjustment, Demo C penuh | — |
| S8 | DevOps | Deploy publik bersama LedgerPay, `payrun_backup` + restore drill, rollback, demo faults, CSV import | — |
| S9 | Performance | PR-1…PR-7, eksperimen E-2, tuning chunk, cek N+1, diff vs bulan lalu (PF-23) | — |
| S10 | Documentation + Demo | README, ADR final, demo script + video, UI polish, audit DoD, interview pass | **M7** |

> **Kenapa S4 bukan "security murni":** M6 (payroll benar dan resumable) harus tercapai di akhir S5, dan itu butuh run + async chunk. Karena job runner sudah ada dari LedgerPay, async calc bisa dikerjakan di S4. Item security PayRun-spesifik (role grant, owner lock, PII masking) tetap masuk S4 dengan porsi ± 4 jam. Security dasar (auth, scoping, RLS, idempotency) sudah dikerjakan sejak S1.
> **Kenapa sprint "Background" (S6) berisi domain input:** async inti sudah selesai di S4. Pekerjaan background yang tersisa (payslip PDF, CSV import) baru bermakna setelah ada data input dan finalize, jadi digeser ke S7–S8.

---

## 2. Detail per sprint

### Sprint 0 — Product + Architecture (port & persiapan)
**Objective:** repo `payrun` berdiri di atas skeleton LedgerPay, dengan infra siap menampung database dan storage PayRun. Belum ada fitur domain.

**Deliverables:** repo `payrun` (struktur `32-repository-structure.md`), skeleton ter-rename, CI hijau, `api/openapi.yaml` berisi skeleton endpoint P0 (tanpa body lengkap), migrasi role (`payrun_owner`, `payrun_app NOBYPASSRLS`, `payrun_readonly`, `payrun_backup BYPASSRLS SELECT-only`), MinIO di compose dev, `docs/` berisi dokumen dokumen desain + ADR-P01…P09.

| Task | Jam |
|---|---|
| Buat repo, salin skeleton, rename module/prefix metrik (`payrun_`) | 3 |
| Port CI (lint, unit, integration, Redocly, trivy, gitleaks); hijau di commit kosong | 2,5 |
| Migrasi role + `btree_gist` + helper `payrun.current_tenant()` (`nullif(current_setting(...),'')`) | 2 |
| Compose dev: Postgres (DB `payrun`), MinIO + init bucket private, mock tidak dipakai | 1,5 |
| OpenAPI skeleton P0 + generate types (oapi-codegen) | 2 |
| Review ulang ADR-P01…P09 + "lessons from LedgerPay" (§5) → catat perubahan sebagai amandemen | 2 |
| Seed tenant default (fungsi seed, belum dipanggil endpoint) — rancang data O6 | 1,5 |
| **Total** | **14,5** |

**Dependency:** LedgerPay M4 (skeleton stabil, infra berjalan).
**DoD:** `make up` → `/readyz` hijau; CI hijau; `TestBuild_NoTesthooksInRelease` lolos; role DB terverifikasi dengan query katalog (app role bukan owner, `NOBYPASSRLS`).
**Risk:** menyalin bug skeleton LedgerPay. *Mitigasi:* tinjau issue/catatan "lessons" LedgerPay sebelum menyalin.
**Demo-able:** belum ada (internal).

---

### Sprint 1 — Foundation (tenancy + IAM inti)
**Objective:** user bisa signup tenant, login, dan memilih tenant. Setiap query berjalan di bawah RLS, dan isolasi dibuktikan otomatis sejak tabel pertama.

**Deliverables:** PF-01, PF-02 (inti), PF-03, PF-17 (field `tenant_id` di setiap log request/job lewat middleware tenant context + redaction PII di logger; health/readiness sudah ada dari skeleton S0; dikerjakan di dalam task middleware tenant, tanpa jam tambahan). Tabel `tenants`, `tenant_settings`, `users` (policy `users_self`, `users_tenant_member_read`), `memberships`, `refresh_tokens`, `idempotency_keys` (tenant), `platform_idempotency_keys`, `audit_log`, `jobs` (+ `jobs_enforce_tenant`). Endpoint: `POST /tenants/signup`, `POST /auth/login`, `POST /auth/refresh` (+ `X-Active-Tenant`), `POST /auth/logout`, `GET /me`, `GET /me/memberships`, `POST /v1/session/switch-tenant`, `GET /tenant`.

| Task | Jam |
|---|---|
| Migrasi tabel di atas + policy RLS (`USING` + `WITH CHECK`) + `FORCE` | 3 |
| `WithTenantTx` (`set_config(..., true)`) + `WithPlatformTx` (`app.user_id`) + depguard: hanya `tenancy`/`iam` boleh pakai platform tx | 2,5 |
| `auth_lookup_user` (`SECURITY DEFINER`, `SET search_path`) + login/refresh/logout dengan `tid` | 3 |
| Signup (platform idempotency, seed default O6) + switch-tenant (cek membership) | 2,5 |
| **RLS catalogue test** di CI (semua tabel ber-`tenant_id` punya RLS + FORCE; allowlist SECURITY DEFINER = 1) | 1,5 |
| Isolation harness: fixture Alpha/Beta semua role; `rls_no_context_returns_zero_rows`, `pool_leakage_after_commit`, `rls_bypass_query_test` | 2,5 |
| **Total** | **15** |

**Dependency:** S0.
**DoD:** `rls_catalogue_test`, `rls_bypass_query_test`, `rls_no_context_returns_zero_rows`, `pool_leakage_after_commit`, `TestCatalogue_TenantFKsComposite` hijau; refresh reuse test (`refresh_reuse_revokes_family`) lolos di PayRun; login memakai semaphore C-45.
**Risk:** RLS terlihat jalan padahal test berjalan sebagai superuser. *Mitigasi:* test terhubung sebagai `payrun_app` (`25-testing-strategy.md`); catalogue test memeriksa `relforcerowsecurity`.
**Demo-able:** signup dua tenant, login, switch tenant; query mentah tanpa filter hanya mengembalikan data tenant aktif.

---

### Sprint 2 — Core Domain (org + employee) → **M5**
**Objective:** HR mengelola departemen, posisi, dan karyawan. Isolasi lintas tenant dan matriks otorisasi terbukti untuk semua endpoint yang ada.

**Deliverables:** PF-02 (undangan), PF-04, PF-05 (master), PF-06, PF-20 (enkripsi minimal), PF-18 (UI awal). Tabel `invitations`, `departments`, `positions`, `employees` (PII `bytea` + `nik_bidx`). Endpoint: `POST /invitations`, `POST /invitations/accept`, `GET /members`, departments/positions CRUD + deactivate, `GET/POST /employees`, `GET/PATCH /employees/{id}` (`If-Match`), `POST /employees/{id}/terminate`.

| Task | Jam |
|---|---|
| Undangan: token `<tenant_id>.<secret>` di fragment URL, SHA-256, single use, email wajib cocok (T-PR-04) | 3 |
| Departments/positions CRUD + `DUPLICATE_CODE` | 1,5 |
| Employees CRUD + `If-Match`/`ETag` + `EMPLOYEE_NO_TAKEN` | 3 |
| PII minimal: keyring v1 AES-256-GCM (AAD = tenant_id + field) + blind index HMAC (`NIK_TAKEN`); `pii_roundtrip`, `pii_aad_swap_fails` | 2 |
| Tenant guard C-46 di decode repository + metrik `payrun_tenant_isolation_violations_total`; `tenant_guard_detects_foreign_row` | 1 |
| Cross-tenant suite untuk semua endpoint + **`authz_matrix`** table-driven (endpoint tanpa baris matriks = fail) | 3 |
| UI: login + pemilih tenant + daftar/detail employee (komponen library) | 2,5 |
| **Total** | **16** |

**Dependency:** S1.
**DoD (M5):** Demo A desain fitur & arsitektur (`02-product-strategy.md` §8) jalan sebagai test otomatis: GET employee Beta dari Alpha → 404; manipulasi `tid` → 401; query tanpa filter → hanya Alpha; `authz_matrix` hijau; `cross_tenant_fk_rejected`; `TestEmployee_NikUniquePerTenant`.
**Risk:** undangan memakan waktu lebih (UI accept + email-match). *Mitigasi:* di MVP link undangan ditampilkan di UI/log (PF-02), tanpa email.
**Demo-able:** **Demo A** lengkap di depan layar (output test + response API).

---

### Sprint 3 — Critical Business Logic (kompensasi + payroll engine)
**Objective:** data gaji effective-dated tersimpan benar, dan payroll engine menghitung satu karyawan secara deterministik dan terjelaskan.

**Deliverables:** PF-05 (org assignment), PF-07, PF-12. Tabel `org_assignments` (EXCLUDE + trigger siklus + advisory lock `org:<tenant>`), `component_definitions`, `salary_assignments`, `employee_components`, `rulesets` (global, read-only, `ID-SIMPLIFIED-2026.1`), `work_calendars`/`holidays`. Endpoint: `POST /employees/{id}/org-assignments`, component definitions CRUD, `POST /employees/{id}/salary-changes`, `GET /employees/{id}/salary-history`, `POST /employees/{id}/component-assignments`, `/holidays`.

| Task | Jam |
|---|---|
| Org assignment effective-dated + `MANAGER_CYCLE` (advisory lock per tenant) | 2,5 |
| Component definitions + validasi graf (`COMPONENT_CYCLE`, `INVALID_COMPONENT_REFERENCE`) | 2,5 |
| Salary assignment + employee components: close+open dalam satu tx, `OVERLAPPING_PERIOD` (23P01) | 3 |
| **Payroll engine pure** (`Calculate(input, ruleset)`): prorata hari kerja multi-segmen, 4 tipe komponen, BPJS, pajak simplified, pembulatan per komponen, explanation line | 5 |
| Golden test ≥ 20 kasus (termasuk worked example Tono = 6.888.609) + property `net == Σ lines` | 3 |
| **Total** | **16** |

**Dependency:** S2 (employee). Engine sendiri tidak bergantung DB, jadi bisa dimulai di hari pertama sprint.
**DoD:** golden ≥ 20 hijau; worked example `07-payroll-engine.md` §15 tepat; property test 10.000 iterasi; `TestEligibility_JoinerLeaverIncludedExactly`; engine tidak meng-import `platform/db` maupun `time.Now` (depguard).
**Risk:** detail pajak/BPJS menyedot waktu (R4 strategi produk). *Mitigasi:* ruleset contoh berlabel *simplified*; tidak ada riset regulasi di sprint ini.
**Demo-able:** slip satu karyawan dari golden file, baris per baris dengan penjelasan.

---

### Sprint 4 — Security *(+ critical logic II: run lifecycle & async)*
**Objective:** payroll run untuk seluruh tenant bisa dihitung asinkron, resumable, dan aman terhadap run ganda. Hardening keamanan khusus PayRun diselesaikan.

**Deliverables:** PF-13 (inti), PF-02 (role change), PF-20 (masking + full view). Tabel `payroll_periods`, `payroll_runs` (+ `calc_started_at`), `payroll_items`, `payroll_lines`, `payroll_item_snapshots` (trigger PI-04 dengan `FOR SHARE`, PI-05 deferred). Endpoint: `POST /payroll-runs` (`ACTIVE_RUN_EXISTS`), `GET /payroll-runs[/{id}]` (progres), `POST /payroll-runs/{id}/calculate`, `GET /payroll-runs/{id}/items[/{item_id}]`, `POST …/items/{item_id}/exclude`, `POST /payroll-runs/{id}/cancel`, `PATCH /members/{id}/roles`, `POST /members/{id}/revoke`, `GET /employees/{id}/pii`.

| Task | Jam |
|---|---|
| Migrasi run/period/item/line/snapshot + trigger PI-04 (`FOR SHARE`) + PI-05 | 2 |
| Create run (period auto-create dengan `tenant_settings FOR UPDATE`, partial unique, idempotency) + calculate (`payroll_periods FOR UPDATE`, `calc_started_at`) | 2 |
| Job `payroll.plan` (eligible → chunk `employee_ids[]`, dedupe `payroll.calc_chunk:{run}:{v}:{n}`) | 1,5 |
| Job `payroll.calc_chunk`: txRead RR READ ONLY → compute → txWrite RC (`payroll_runs FOR UPDATE`, guard `calc_version`, upsert, `chunks_done`, `CompleteInTx`), snapshot + fingerprint per item | 3,5 |
| Recalculate (`calc_version++`), cancel, exclude item (alasan + audit) | 1 |
| Role change + aturan grant (OWNER/HR_ADMIN) + advisory lock `owners:<tenant>` + `LAST_OWNER`; audit `membership.sod_conflict` | 2,5 |
| PII masking default + `GET /employees/{id}/pii` ter-audit; `pii_masked_by_default`, `pii_full_view_audited` | 1,5 |
| **Total** | **14** |

**Dependency:** S3 (engine, salary), job runner dari skeleton.
**DoD:** `TestPayrollRun_WorkerKilledMidChunk_ResumesWithoutDuplicates`, `TestCalcChunk_LeaseStolen_FencedCommitRollsBack`, `TestCalcChunk_StaleCalcVersion_NoOp`, `TestCreateRun_ConcurrentSamePeriod_SingleRun`, `run_create_double_click_replays`, `TestRunTotals_EqualSumOfItems`, `owner_parallel_revoke_keeps_one`, `role_grant_rules_enforced`.
**Risk:** split txRead/txWrite salah sehingga `40001` di chunk paralel. *Mitigasi:* ikuti `21-tx-global-rules.md` §5 secara harfiah; test 4 worker paralel di CI.
**Demo-able:** run 100 karyawan dengan progres chunk; kill worker (test subprocess) → resume tanpa duplikat.

---

### Sprint 5 — Testing *(+ approval & stale detection)* → **M6**
**Objective:** run bisa di-submit dan di-approve dengan jaminan input tidak berubah sejak kalkulasi. Suite concurrency PayRun lengkap. M6 tercapai.

**Deliverables:** PF-11 (engine approval), PF-13 (submit/approve/reject), PeriodGuard + change log untuk penulis input yang sudah ada (salary, komponen, org, employment). Tabel `approval_requests`, `payroll_input_changes`. Endpoint: `POST /payroll-runs/{id}/submit` (alasan self-approval jika satu admin, dihitung server), `GET /approvals/inbox`, `POST /approvals/{id}/approve|reject`.

| Task | Jam |
|---|---|
| Approval engine generik (dispatch per `subject_type`, guard `WHERE status='PENDING'`, CHECK self-approval terkoreksi) | 3 |
| `PeriodGuard.Lock`: `tenant_settings FOR KEY SHARE` → periode `FOR SHARE` (urut year, month) → UPSERT `payroll_input_changes` (`changed_at = clock_timestamp()` setelah lock); dipasang di penulis salary/komponen/org/employment | 2,5 |
| Submit + approve `PAYROLL_RUN` (period `FOR UPDATE` → kandidat change log → verifikasi fingerprint → `RUN_STALE {employees_changed}`; D6 eligibility) | 3 |
| Suite concurrency: approve vs reject, edit saat approve (O3), penulis sebelum period ada (D1), zombie chunk vs finalize-ready, owners revoke | 3 |
| Golden diperluas ke 30 kasus + test recompute-from-snapshot (PI-12) | 1,5 |
| UI halaman payroll run: progres, item/warning, submit, approve | 2,5 |
| **Total** | **15,5** |

**Dependency:** S4.
**DoD (M6):** golden 30 + recompute-from-snapshot hijau; Demo B (crash & resume + run ganda) lolos otomatis; `TestApproval_ApproveVsRejectRace_SingleDecision`, `TestSubmit_InputChangedAfterCalc_ReturnsRunStaleWithEmployees`, `TestApproval_SelfApprovalRejectedByDB`, `TestRunApprove_BySubmitter_Forbidden`, `TestSubmit_RunWithErrorItems_Rejected`, test D1 dan O3 hijau; matriks traceability PayRun untuk PI-03/04/05/06/11/12/13 terisi.
**Risk:** argumen O12/D1 salah diimplementasikan (urutan lock). *Mitigasi:* test interleaving dengan hook `testhooks` (advisory lock) untuk setiap titik di `22-tx-concurrency.md` race (c)(d).
**Demo-able:** **Demo B** di UI; approve ditolak `RUN_STALE` setelah HR mengubah gaji.

---

### Sprint 6 — Background Processing *(+ input domain & finalize)*
**Objective:** data kehadiran, cuti, dan lembur yang sudah disetujui mengalir ke engine. Run bisa difinalisasi, dan periodenya terkunci.

**Deliverables:** PF-08, PF-09, PF-10, PF-14, PF-15 (parsial), PF-18 (self-service). Tabel `attendance_records`, `leave_types`, `leave_balances`, `leave_requests`, `overtime_requests`. Endpoint: clock in/out, `GET /attendance`, `PATCH /attendance/{id}`, leave types/balances/requests (+ cancel), overtime requests (+ cancel), `POST /payroll-runs/{id}/finalize`.

| Task | Jam |
|---|---|
| Attendance clock in/out (idempoten), koreksi ter-audit, status harian, kebijakan `INCOMPLETE` | 3 |
| Leave: jenis, saldo (`CHECK` min), request (`EXCLUDE` overlap), efek approval dengan lock saldo | 3,5 |
| Overtime request + cut-off (`TestCutoff_ApprovedAfterCutoff_NextPeriod`) | 1,5 |
| Input provider engine untuk attendance/leave/overtime + PeriodGuard di semua penulis ini | 2 |
| Finalize: freeze item/snapshot, run `FINALIZED`, period `FINALIZED`, enqueue job payslip | 2 |
| UI self-service: clock in/out, request cuti/lembur, approval inbox manager | 2,5 |
| Demo C bagian 1–3 (ubah absensi Maret → `PERIOD_LOCKED`; ubah item → `RUN_FINALIZED`) | 0,5 |
| **Total** | **15** |

**Dependency:** S5 (approval engine, PeriodGuard).
**DoD:** `leave_parallel_approve_respects_balance`, `TestAttendanceEdit_FinalizedPeriod_PeriodLocked`, `TestPeriodGuard_FinalizedPeriodRejected`, `TestPeriodGuard_RaiseAfterFinalizedPeriodsAllowed`, `TestFinalizedRun_DirectUpdateRejected`, `TestFinalize_RetryAfterUnknownCommit_Idempotent`; golden kasus cuti tidak dibayar dan lembur memakai data dari input provider (integration).
**Risk:** sprint paling padat di sisi domain. *Mitigasi:* overtime tier tetap minimal (2 tier dari ruleset); koreksi attendance tanpa UI khusus (form sederhana).
**Demo-able:** alur lengkap: karyawan clock in → cuti disetujui manager → run → approve → finalize.

---

### Sprint 7 — Observability *(+ payslip & adjustment)*
**Objective:** PayRun bisa dipantau per tenant dan per run. Slip gaji tersedia di web dan PDF. Koreksi pasca-final lewat adjustment, sehingga Demo C lengkap.

**Deliverables:** PF-24, PF-15, PF-22, PF-19, PF-16 (akses slip). Tabel `adjustments`, `payslips`. Endpoint: `GET /me/payslips[/{id}]`, `GET /payslips?run_id`, `GET /payslips/{id}` (admin, ter-audit), `GET /payslips/{id}/download` (`PAYSLIP_NOT_READY`), adjustments CRUD + cancel.

| Task | Jam |
|---|---|
| Adjustment: create/cancel, masuk run target sebagai line, `APPLIED` saat finalize (PI-15), membuat run target *stale* | 2,5 |
| Payslip web (owner + admin ter-audit, PI-17) + job `payslip.render` batch 20, object key deterministik, MinIO presigned 5 menit (C-40) | 4,5 |
| Metrik PayRun (katalog `24-observability.md` §3), tracing span chunk dengan link ke request calculate (`jobs.trace_parent`) | 2,5 |
| Dashboard "PayRun — Payroll Runs" dan "PayRun — Tenancy & Security" + alert (run failed, isolation violation, dead job) | 2,5 |
| UI halaman slip gaji (self-service + admin) | 1,5 |
| Demo C bagian 4–5 (adjustment April + recompute Maret identik) | 0,5 |
| **Total** | **14** |

**Dependency:** S6 (finalize).
**DoD:** `TestPayslipAccess_Matrix`, `payslip_presigned_url_ttl`, `payslip_download_requires_authz`, `TestPayslipPDF_DeterministicBytes`, `TestPayslipRender_CrashMidBatch_ResumesIdempotently`; dashboard ter-provision dari file; alert isolation violation memicu di test sintetis.
**Risk:** library PDF di Go memakan waktu. *Mitigasi:* template HTML sederhana → PDF dengan library pure-Go yang dipilih di awal sprint (time-box 1 jam evaluasi); kalau gagal, PF-22 masuk cut list dan slip tetap web.
**Demo-able:** **Demo C** penuh; dashboard menampilkan durasi run dan progres chunk saat demo.

---

### Sprint 8 — DevOps *(+ CSV import)*
**Objective:** PayRun berjalan di VM publik bersama LedgerPay dengan backup yang benar, rollback, dan fault injection untuk demo live. Import CSV melengkapi alur nyata UMKM.

**Deliverables:** deployment `payrun` di repo `infra`, `payrun_backup` + restore drill, demo faults PayRun, PF-21. Tabel `attendance_imports`, `attendance_import_errors`, `demo_faults` (`migrations_demo/`). Endpoint: `POST /attendance-imports` (multipart), `GET /attendance-imports/{id}[/errors]`, `/api/v1/demo/*` (build tag `demo`).

| Task | Jam |
|---|---|
| Infra: service `payrun-api`/`worker` di compose gabungan, site Caddy `payrun.<domain>`, route `files.<domain>` untuk presigned GET, env sops | 4 |
| Backup `pg_dump` dengan `payrun_backup` + restore drill memverifikasi jumlah baris tabel tenant > 0 | 2,5 |
| Rollback + deploy-gate bersama (`flock`), smoke test pasca-deploy read-only | 2 |
| Demo faults: `faults.Check` di `payroll.calc_chunk` (crash setelah N karyawan, chunk size demo 10, O5) | 1,5 |
| CSV import: upload ke bucket, job `attendance.import` per batch, laporan error, `IMPORT_DUPLICATE`, C-38 (5 MB, ± 50.000 baris), encoder CSV pusat C-39 | 4 |
| **Total** | **14** |

**Dependency:** S7; repo `infra` dari LedgerPay.
**DoD:** deploy dari `main` hijau dengan health gate; rollback ke SHA sebelumnya diuji; restore drill mingguan lulus (baris > 0); `TestBuild_DemoEndpointsAbsentWithoutDemoMode`; `csv_export_escapes_formula`; import 50.000 baris lolos di integration.
**Risk:** memori VM 8 GB saat dua project + obs hidup. *Mitigasi:* anggaran memori `27-docker-compose.md`; matikan Loki/Jaeger di luar demo.
**Demo-able:** Demo B live di VM publik lewat demo toolbar (worker crash di karyawan ke-37, chunk 4).

---

### Sprint 9 — Performance
**Objective:** baseline performa PayRun tercatat jujur, eksperimen O12 diputuskan, dan bottleneck yang terbukti diperbaiki.

**Deliverables:** laporan load test (template `26-performance-plan.md` §8), hasil E-2 + keputusan, PF-23.

| Task | Jam |
|---|---|
| Seed perf (`payrun_seed` `BYPASSRLS` + `COPY`, 1.000 tenant / 100.000 employee) + gate verifikasi invariant setelah seed | 2,5 |
| PR-1 (run 10.000 karyawan, 1→2→4 worker), PR-2 (1.000 run lintas tenant), PR-3 (read 100.000 employee dengan RLS) | 4 |
| **E-2 / PR-4** (approve dengan 0/10/1.000 perubahan) + PR-5 (edit HR saat approve, lock wait) | 2,5 |
| PR-6 (render 10.000 PDF), PR-7 (import 50.000 baris) | 2 |
| Perbaikan yang terbukti: N+1 input chunk (konstan query per chunk), tuning chunk size, index tambahan dari `pg_stat_statements` | 3 |
| PF-23 diff vs bulan lalu + warning anomali | 2 |
| **Total** | **16** |

**Dependency:** S8 (VM perf efemeral, seed).
**DoD:** semua PR-x punya hasil + spesifikasi mesin; 0 pelanggaran invariant di semua run; keputusan E-2 tertulis (amandemen ADR-P07 jika approve dipindah ke job); README memakai wording "baseline pada VM … " (bukan klaim kapasitas).
**Risk:** run 10.000 karyawan jauh di atas SLO 60 detik/1.000. *Mitigasi:* profil dulu (pprof + `EXPLAIN`); yang paling mungkin adalah N+1 input atau ukuran chunk.
**Demo-able:** grafik durasi run vs jumlah worker.

---

### Sprint 10 — Documentation + Demo → **M7**
**Objective:** PayRun siap dibedah interviewer: dokumentasi lengkap, demo terlatih, DoD terpenuhi.

**Deliverables:** README (struktur demo script), ADR final + amandemen, demo script PayRun A–C, video demo, UI polish, audit DoD, `make demo-verify` hijau nightly.

| Task | Jam |
|---|---|
| README + diagram arsitektur + "how to run" + "design decisions" + hasil load test | 4 |
| Finalisasi ADR & dokumen `docs/` (sinkron dengan implementasi; tandai yang berubah) | 2 |
| Demo script A–C + rehearsal ≥ 3× + rekam video | 3 |
| UI polish (empty state, error dari `code` RFC 9457, responsif) | 3 |
| Audit DoD project (`31-definition-of-done.md`) + `TestTraceability_DocsReferenceExistingTests` hijau | 2 |
| Interview pass: jawab pertanyaan demo script dengan menunjuk kode/test | 1,5 |
| **Total** | **15,5** |

**Dependency:** S9.
**DoD (M7):** seluruh DoD project (`04`) tercentang; demo-verify nightly hijau 3 malam berturut-turut; video ≤ 10 menit.
**Risk:** dokumentasi tertinggal dari implementasi. *Mitigasi:* pembaruan ADR/amandemen dilakukan di setiap sprint (DoD sprint), sprint ini hanya merapikan.
**Demo-able:** seluruh demo A–C + observability.

---

## 3. Workstream (Gantt sederhana)

| Workstream | S0 | S1 | S2 | S3 | S4 | S5 | S6 | S7 | S8 | S9 | S10 |
|---|---|---|---|---|---|---|---|---|---|---|---|
| Fondasi & infra | ██ | █ | | | | | | | ██ | | |
| Tenancy / IAM / security | | ██ | ██ | | █ | | | | | | |
| Org & employee | | | ██ | █ | | | | | | | |
| Kompensasi & engine | | | | ██ | | █ | | | | | |
| Run & async | | | | | ██ | █ | █ | | █ | | |
| Approval & stale | | | | | | ██ | █ | | | | |
| Input (attendance/leave/OT) | | | | | | | ██ | | █ | | |
| Payslip & adjustment | | | | | | | | ██ | | | |
| Observability | | █ | | | | | | ██ | | | |
| Test suites (berjalan terus) | █ | █ | █ | █ | █ | ██ | █ | █ | █ | █ | █ |
| UI | | | █ | | | █ | █ | █ | | | █ |
| Performance | | | | | | | | | | ██ | |
| Docs & demo | █ | | | | | | | | | | ██ |

---

## 4. Ditunda (P2/P3) beserta alasannya

| Fitur | Prioritas | Alasan ditunda |
|---|---|---|
| PF-25 Export file transfer bank | P2 | Murah, tetapi tidak menambah sinyal baru (encoder CSV sudah dibuktikan di import) |
| PF-26 Leave accrual terjadwal | P2 | Pola scheduler sudah dibuktikan di LedgerPay (recon) |
| PF-27 Fairness per tenant | P2 | Dibahas lewat hasil PR-2; diimplementasikan hanya jika noisy neighbor terbukti |
| PF-28 Break-glass support | P2 | Desain sudah ada (threat model); implementasi butuh UI + approval tambahan |
| PF-29 Rate limiting PayRun | P2 | Middleware ikut skeleton jika LedgerPay sudah membangun LF-20; kalau belum, tetap P2 |
| PF-30 Notifikasi email | P3 | Hanya consumer job tambahan |
| PF-31 Laporan | P3 | Read model eventual; tidak memperdalam inti |
| PF-32 Approval multi-level | P3 | Satu tingkat sudah membuktikan pola; jalur migrasi ada di ADR-P09 |
| PF-33 Disbursement via LedgerPay | P3 | Stretch lintas project; hanya jika M7 tercapai lebih awal |
| KMS envelope per tenant | V2 | Production enhancement (ADR-P08) |

---

## 5. Cut list jika terlambat (urutan pemotongan)

Prinsip: **M5 dan M6 dilindungi penuh** (Demo A dan Demo B adalah inti nilai PayRun). Demo C dipertahankan minimal tanpa adjustment.

| Urutan | Dipotong / disederhanakan | Dampak | Yang tetap |
|---|---|---|---|
| 1 | PF-23 diff vs bulan lalu | Review run kurang kaya | Warning dasar dari engine |
| 2 | PF-22 PDF + MinIO | Tidak ada unduhan PDF | Payslip web (PF-15) |
| 3 | PF-21 CSV import | Absensi hanya lewat clock in/out | Clock in/out + koreksi |
| 4 | Jaeger/Loki di profile obs | Tidak ada trace visual saat demo | Log JSON + Prometheus/Grafana |
| 5 | PF-19 adjustment | Demo C berhenti di immutability | `PERIOD_LOCKED` + `RUN_FINALIZED` + recompute identik |
| 6 | Overtime tier | Lembur flat multiplier | Lembur dari request yang disetujui |
| **Tidak boleh dipotong** | RLS + catalogue test, isolation suite, authz matrix, engine + golden, run async + resume, stale detection, finalize immutability | — | — |

Sprint buffer cadangan (§1 index) dipakai **sebelum** memotong item 4 ke bawah.

---

## 6. Pelajaran dari LedgerPay yang diterapkan

- [ ] Tulis invariant checker dan test concurrency **sebelum** handler HTTP, bukan sesudah.
- [ ] Setiap migrasi tabel tenant baru memuat policy RLS + `FORCE` di file yang sama (catalogue test menolak jika tidak).
- [ ] Hindari ORM-style helper yang menyembunyikan `FOR UPDATE`/`FOR SHARE`; urutan lock ditulis eksplisit dan sesuai desain transaksi & concurrency §2.
- [ ] Error code baru masuk katalog kontrak API lebih dulu, lalu dipakai di handler (meta test `TestDBErrorMapping_AllCatalogueConstraints`).
- [ ] Demo dirancang sebagai test (`make demo-verify`) sejak sprint fitur itu dibuat, bukan di sprint dokumentasi.
- [ ] ADR yang berubah saat coding langsung diberi amandemen di sprint yang sama.
- [ ] Ukur dulu (pprof, `pg_stat_statements`), baru optimasi.
- [ ] UI dibatasi time-box per sprint. Halaman payroll run dan slip gaji mendapat prioritas.
