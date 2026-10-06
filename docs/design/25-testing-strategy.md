# Step 8.2 — Testing Strategy

> Bagian dari [Step 8](00-index.md). Prinsip yang mengikat ada di `00-index.md` §5: Postgres sungguhan via testcontainers, **tanpa mock database**, dan traceability lebih penting dari coverage.
> Sumber nama test: Step 3 (`02`, `03`, `06`, `07`, `08`), Step 5 `05`, Step 6 `04` §4, Step 7 `01` §7, `02` §7, `03`. Dokumen ini **menyatukan** semuanya menjadi satu katalog dan satu matriks traceability.

---

## 0. Konvensi penamaan test (menyelesaikan beda gaya antar step)

Step 3 memakai nama fungsi Go (`TestTransfer_IdempotentReplay`), sedangkan Step 6/7 memakai ID `snake_case` (`refresh_reuse_revokes_family`). Keduanya dipertahankan dengan aturan berikut:

| Bentuk | Arti | Contoh |
|---|---|---|
| `Test<Subjek>_<Skenario>` | Fungsi test Go tingkat atas | `TestTransfer_IdempotentReplay` |
| `snake_case` | **Subtest** (`t.Run`) di dalam suite per kategori | `TestSecurity/refresh_reuse_revokes_family`, `TestConcurrency/leave_parallel_approve_respects_balance` |
| `golden/<kasus>` | Kasus golden payroll engine | `TestEngineGolden/tono_march_2026` |

Kalau Step 3 dan Step 7 memberi nama berbeda untuk test yang sama, **nama Step 7 menjadi kanonik**. Step 3 ditulis lebih dulu, dan Step 7 merinci tekniknya. Alias dicatat di kolom "Alias" pada §4 agar dokumen lama tetap bisa ditelusuri.

---

## 1. Test pyramid

```text
                         ┌─────────────┐
                         │  E2E (~12)  │  Playwright + compose: jalur kritis saja        < 5 menit
                     ┌───┴─────────────┴───┐
                     │ Chaos / failure (~25)│  kill proses, fault injection, testhooks     (nightly + PR berlabel)
                 ┌───┴──────────────────────┴───┐
                 │ Concurrency (~35)            │  barrier + test hook, Postgres sungguhan
             ┌───┴──────────────────────────────┴───┐
             │ Integration + security + contract     │  service + DB sungguhan, RLS, trigger,   < 5 menit (paralel)
             │ + migration catalogue (~300)          │  idempotency, job runner
         ┌───┴───────────────────────────────────────┴───┐
         │ Unit / domain + golden + property (~450)      │  pure function, tanpa I/O                < 30 detik
         └───────────────────────────────────────────────┘
```

| Layer | LedgerPay (perkiraan) | PayRun (perkiraan) | Budget waktu di CI |
|---|---:|---:|---|
| Unit / domain | 150 | 200 (termasuk 30 golden × varian) | < 30 dtk total |
| Property | 6 | 6 | 10.000 iterasi di PR (< 20 dtk); 1 juta di nightly |
| Integration + security + contract | 140 | 170 (termasuk authz matrix ±400 subtest) | < 5 menit (paralel per package) |
| Migration catalogue | 8 | 12 | < 30 dtk |
| Concurrency | 15 | 18 | Bagian dari integration (`-race`) |
| Chaos / failure | 12 | 10 | PR berlabel `chaos` + nightly, < 10 menit |
| E2E | 6 | 6 | < 5 menit per project |

Bentuknya sengaja **lebih tebal di tengah** daripada piramida klasik. Correctness kedua project ada di interaksi kode dengan Postgres (lock, constraint, trigger, RLS), dan bagian itu hanya bisa dibuktikan oleh integration test. Unit test tetap banyak, tetapi hanya untuk logika yang benar-benar murni.

---

## 2. Tooling

| Alat | Dipakai untuk | Catatan keputusan |
|---|---|---|
| Go `testing` + `testify` (`require`, bukan `assert`, untuk precondition) | Semua test Go | `require` menghentikan test di kegagalan pertama, sehingga tidak ada kegagalan berantai yang membingungkan |
| `testcontainers-go` (Postgres, versi sama dengan production) | Integration, concurrency, migration, chaos | **Satu container per package test**. Lihat §2.1 |
| `pgregory.net/rapid` | Property-based test | Shrinking otomatis menghasilkan contoh gagal minimal; seed dicatat di output |
| Golden file + flag `-update` | Payroll engine, PDF payslip, response contract tertentu | `go test ./payroll/engine -run Golden -update` menulis ulang golden; diff golden **wajib** di-review di PR |
| `net/http/httptest` | Handler + middleware tanpa jaringan | Router asli dipakai, sehingga middleware ikut teruji |
| Client hasil `oapi-codegen` + validator OpenAPI (`kin-openapi`) | Contract test | Request dan response divalidasi terhadap `api/openapi.yaml` |
| Playwright (TypeScript) | E2E browser | Berjalan terhadap `docker compose` profile `e2e` |
| k6 | Load test | Lihat file `03` |
| `go test -race` | Semua unit + integration | Selalu aktif di CI |
| Build tag `testhooks` | Titik interleaving (`testhook.Pause`) dan advisory-lock hook lintas proses (Step 7 `01` §7.1) | Kode hook menjadi **no-op** di build produksi; test `TestBuild_NoTesthooksInRelease` memeriksa binary rilis |
| `go.uber.org/goleak` | Deteksi goroutine bocor (worker loop, HTTP client) | Di `TestMain` setiap package yang menjalankan goroutine |
| ~~Toxiproxy (container)~~ **Dihapus (Step 12: S11).** FS-03/PFS-04 memakai hook `testhooks` yang menutup koneksi DB tepat setelah COMMIT terkirim / di tengah chunk | Memutus koneksi DB di titik tertentu (FS-03, PFS-04) | Semula opsional, hanya di suite chaos. Alasan: dua skenario "commit outcome unknown" tidak bisa disimulasikan dengan cara lain secara jujur |

### 2.1 Isolasi database: template DB per test

```text
TestMain (per package)
  └─ start container Postgres (sekali)
  └─ CREATE DATABASE app_template; jalankan semua migrasi (goose up) sebagai <proj>_owner
  └─ tandai app_template sebagai template

setiap test (t.Parallel() aman)
  └─ CREATE DATABASE t_<random> TEMPLATE app_template      (≈ 50–150 ms, copy di level file)
  └─ koneksi sebagai <proj>_app (role runtime, BUKAN owner) → test berjalan
  └─ t.Cleanup: DROP DATABASE t_<random>
```

| Alternatif | Kenapa tidak dipilih |
|---|---|
| Satu DB + rollback transaksi per test | Tidak bisa menguji commit, deferred trigger, `FOR UPDATE` lintas koneksi, atau job runner, padahal semuanya inti proyek ini |
| Satu DB + `TRUNCATE` antar test | Test tidak bisa paralel; urutan test memengaruhi hasil |
| Container per test | Terlalu lambat (2–5 dtk per test) |

**Aturan penting:** test terhubung sebagai **role aplikasi** (`<proj>_app`), bukan superuser atau owner. Kalau test berjalan sebagai superuser, RLS, grant append-only, dan `FORCE ROW LEVEL SECURITY` akan dilewati diam-diam, dan test keamanan menjadi bohong.

---

## 3. Definisi layer

### 3.1 Unit / domain
**Masuk:** fungsi murni tanpa I/O.
- `ledger`: validasi journal (seimbang, ≥ 2 entry, amount > 0, satu currency), template purpose → arah DR/CR yang sah.
- `Money` (LedgerPay, int64 minor unit): overflow, beda currency, parsing string. `Decimal` (PayRun): pembulatan per mode.
- Tabel transisi state machine (setiap pasangan from × to) untuk TopUp, PaymentIntent, Payout, InboxEvent, PayrollRun, ApprovalRequest, Adjustment.
- Payroll engine (`Calculate`), golden + property.
- Fingerprint (kanonikalisasi input → hash stabil).
- Policy authorization (fungsi `CanX(principal, resource)`).
- Encoder CSV (escape formula), masking PII, redaction logger, verifikasi JWT, HMAC signature.

**Tidak boleh di sini (anti-pattern):** mock repository untuk "menguji service". Service yang logikanya bergantung pada transaksi DB diuji di integration. Mock database menguji asumsi penulis test, bukan perilaku Postgres.

### 3.2 Integration (service + Postgres sungguhan)
**Masuk:** service layer + HTTP handler dengan DB sungguhan: transaksi, constraint, trigger (append-only, deferred balance, PI-04, PeriodGuard, PI-16/19), RLS, idempotency, job runner (claim, lease, fencing, backoff, dead), inbox, recon, audit di tx yang sama, mapping error DB → code katalog.
**Tidak boleh di sini:** memanggil provider sungguhan (selalu mock-provider), `time.Sleep` untuk menunggu job. Gunakan helper `jobs.DrainUntilIdle(ctx)` atau clock palsu.

### 3.3 Contract
**Masuk:** setiap endpoint di Step 5. Request contoh dan response nyata divalidasi terhadap `openapi.yaml`. Uang harus string. Error harus `application/problem+json` dengan `code` dari katalog. Kontrak mock-provider (Step 5 `03`) diuji dua arah: LedgerPay client vs mock server.
**Tidak boleh di sini:** logika bisnis. Contract test hanya memeriksa bentuk.

### 3.4 Migration & catalogue
**Masuk:**
- `goose up` dari nol dan `down` satu langkah untuk migrasi terbaru (expand/contract, ADR-013).
- Query katalog `pg_catalog`:
  - setiap tabel ber-`tenant_id` punya `relrowsecurity` + `relforcerowsecurity` (PayRun);
  - tidak ada fungsi `SECURITY DEFINER` selain allowlist `payrun.auth_lookup_user`;
  - `<proj>_app` bukan owner dan tidak punya `BYPASSRLS`;
  - tabel append-only tidak punya grant UPDATE/DELETE untuk `<proj>_app`;
  - setiap FK ke tabel ber-`tenant_id` menyertakan `tenant_id` (PI-02);
  - setiap kolom status punya CHECK;
  - semua view memakai `security_invoker = true`.

### 3.5 Concurrency
**Masuk:** test yang membuktikan race di Step 7 benar-benar dijaga. Teknik (Step 7 `01` §7.1): barrier goroutine, `testhook.Pause` di titik bernama, advisory-lock hook untuk worker di proses lain, pembanding naif (hanya di test code), dan **invariant checker** yang dijalankan setelah setiap test (Σ debit = Σ kredit, `balance = Σ entries`, tidak ada saldo negatif, jumlah journal per source sesuai; PayRun: Σ item = total, satu run aktif per period).
**Tidak boleh di sini:** "jalankan 1.000 kali dan berharap race muncul" tanpa hook. Race yang tidak deterministik tidak membuktikan apa-apa ketika test-nya hijau.

### 3.6 Failure / chaos
**Masuk:** kill worker di titik tertentu (`demo_faults` / `faults.Check` → `os.Exit(137)` di subprocess), skenario mock-provider (`timeout_after_process`, `error_5xx`, `webhook_duplicate:n`, `webhook_delay`, `webhook_out_of_order`, `settlement_mismatch`), dan pemutusan koneksi DB via hook `testhooks` (FS-03, PFS-04; Toxiproxy dihapus di Step 12). Setelah recovery: invariant checker + jumlah efek eksternal (mock bank mencatat tepat 1 transfer).

### 3.7 Security
**Masuk:** seluruh katalog Step 6 `04` §4: authn, refresh reuse, CSRF/CORS, BOLA, authz matrix, cross-tenant, RLS bypass, pool leakage, SSRF egress, webhook signature/replay, idempotency abuse, upload, CSV injection, PII, redaction, audit append-only, security headers, error leakage.

### 3.8 E2E
**Masuk:** hanya *critical user journey* yang membuktikan semua lapisan tersambung (browser → Caddy → api → DB → worker → mock-provider).
**Tidak boleh di sini:** variasi validasi atau edge case. Semua itu sudah di integration; E2E yang banyak hanya menambah flaky.

---

## 4. Test catalogue

Kolom **CI**: `U` unit · `I` integration · `C` contract · `M` migration · `X` chaos (PR berlabel + nightly) · `E` E2E · `N` nightly saja.

### 4.1 LedgerPay — unit / domain / property

| Test | Membuktikan | Trace | CI |
|---|---|---|---|
| `TestJournal_Validate` (tabel kasus) | Journal tidak seimbang / 1 entry / amount ≤ 0 / campur currency ditolak di APP | LI-01, LI-11 | U |
| `TestJournalPurpose_Templates` | Kombinasi purpose × arah akun yang tidak sah ditolak | LI-01, LI-06 | U |
| `TestMoney_*` (overflow, currency mismatch, parse) | Aritmetika uang aman; tidak ada float | LI-11 | U |
| `TestStateMachine_{TopUp,PaymentIntent,Payout,Inbox}` | Semua pasangan from × to: hanya transisi sah yang diterima | LI-07 | U |
| `TestPolicy_*` | Principal hanya pada resource miliknya | LI-15 | U |
| `TestWebhookSignature_*` | HMAC benar/salah, constant-time, window timestamp | T-LP, C-32 | U |
| `TestProperty_JournalAlwaysBalanced` | Journal acak hasil builder selalu seimbang; journal tidak seimbang selalu ditolak | LI-01 | U/N |
| `TestProperty_RandomTransfersConserveTotal` | Urutan transfer/top-up/payout acak (di DB, iterasi kecil) → Σ semua saldo user + sistem konstan, `balance == Σ entries` di setiap langkah | LI-01, LI-03 | I/N |

### 4.2 LedgerPay — integration & concurrency

| Test (kanonik) | Alias Step 3 | Membuktikan | Trace | CI |
|---|---|---|---|---|
| `TestTransfer_IdempotentReplay` | — | Replay identik, payload beda → 422, key per client | LI-05, FS-01 | I |
| `TestTransfer_RejectedResultIsReplayed` | — | 422 domain disimpan dan di-replay | LI-05, FS-01 | I |
| `TestIdempotency_ConcurrentSameKey_OneEffect` | — | Dua request bersamaan dengan key sama → satu efek | LI-05, race (c) | I |
| `TestIdempotency_FirstTxRolledBack_SecondProceeds` | — | Insert pertama rollback → request kedua diproses normal | LI-05 | I |
| `TestTransfer_ConcurrentDoubleSpend_100x` | `TestTransfer_100ParallelFromSameWallet` | Tepat 10 dari 100 berhasil, saldo 0, ledger seimbang | LI-02, FS-02, race (a), **Demo B** | I |
| `TestTransfer_NaiveImplementation_Overdraws` | — | Test di atas sensitif terhadap race (pembanding naif harus gagal) | LI-02 | I |
| `TestTransfer_OppositeDirections_NoDeadlock` | `TestTransfer_OppositeDirectionNoDeadlock` | 50 pasang A→B/B→A, `db_deadlock_total = 0` | race (b) | I |
| `TestLedger_AppValidationRemoved_DBStillRejects` | — | Defense in depth: validasi saldo dimatikan via hook → `CHECK` menolak | LI-02 | I |
| `TestJournal_UnbalancedRejectedAtCommit` | — | Deferred trigger menolak journal tidak seimbang / 1 entry | LI-01 | I |
| `TestLedger_AppendOnly` | — | UPDATE/DELETE `journals`/`journal_entries` sebagai role app → gagal | LI-04 | I |
| `TestLedger_DoubleReversalRejected` | — | Reversal kedua → `23505` → `409 ALREADY_REVERSED` | LI-04 | I |
| `TestLedgerVerify_DetectsInjectedDrift` | — | Job verifikasi mendeteksi drift yang disuntikkan (sebagai owner) dan menaikkan metrik | LI-03 | I |
| `TestRefund_ConcurrentExceedingAmount` | `TestRefund_ParallelExceedingAmount` | 5 × 30.000 atas 100.000 → tepat 3 sukses | LI-08, race (d) | I |
| `TestConfirm_ParallelTabs_OnePayment` | — | Dua konfirmasi dengan key berbeda → satu sukses, satu `409 INVALID_STATE_TRANSITION` | LI-13 | I |
| `TestConfirm_VsExpirySweeper` | — | Konfirmasi vs sweeper, kedua urutan | race (e) | I |
| `TestInbox_LeaseExpired_ProcessedOnce` | — | Event diproses dua worker setelah lease habis → satu journal | LI-09, race (f) | I |
| `TestWebhook_DuplicateEvents_CreditOnce` | — | 3 event paralel + 1 ulangan → satu kredit | LI-09, FS-06 | I |
| `TestWebhook_BadSignature_401` / `TestWebhook_StaleTimestamp_400` | — | Webhook palsu / replay ditolak tanpa efek | T-LP, FS-06 | I |
| `TestWebhook_AmountMismatch_Dead` | — | Amount/currency event ≠ top-up → `DEAD` + alert | T-LP | I |
| `TestWebhook_VerifyByFetch_MismatchNotCredited` | — | Provider menyatakan charge belum sukses → tidak dikredit (amandemen ADR-L08) | T-LP | I |
| `TestTopup_LateWebhookAfterExpiry_Credited` | `TestWebhook_LateSuccessAfterExpired_Credits` | `EXPIRED` status lunak | FS-07, race (h) | I |
| `TestWebhook_OutOfOrder_NoRegression` | — | Event lama setelah final → no-op | FS-07 | I |
| `TestWebhook_FinalStateConflict_FlagsRecon` | — | Konflik final → `PROCESSED/CONFLICT` + recon item | FS-07, O2 | I |
| `TestTopUp_ProviderTimeout_RetryResumesFromRecoveryPoint` | — | Retry melanjutkan dari `topups.recovery_point` | FS-05, FS-10 | I |
| `TestTopupRecover_StuckCreated_Completed` | — | Sweep `topup.recover` menyelesaikan top-up tanpa client retry | FS-10 | I |
| `TestPayout_TimeoutAfterProcess_ResolvesViaStatusQuery` | — | Timeout → `UNKNOWN` → status query → `SUCCEEDED`, satu settle | LI-10, FS-05 | I |
| `TestPayoutStatusCheck_VsOpsResolve` | — | Status check vs resolve manual → satu journal settle | LI-07, race (g) | I |
| `TestPayout_MismatchedProviderResponse_Unknown` | — | Respons sukses dengan amount/id berbeda → `UNKNOWN` + reconciliation | amandemen ADR-L06 | I |
| `TestPaymentIntent_RetryAfterClientTimeout` | — | Merchant retry dengan key sama → satu intent | FS-09 | I |
| `TestPaymentIntent_DuplicateMerchantOrder_409` | — | `merchant_order_id` ganda → `409 DUPLICATE_MERCHANT_ORDER` | LI-13 | I |
| `TestMerchantWebhook_RetryBackoffThenDead` / `TestMerchantWebhook_StableEventID` | — | Retry 7 attempt sesuai jadwal lalu dead; event id stabil | FS-09 | I |
| `TestJobRunner_LeaseExpiry_Reclaims` | — | Lease habis → job diambil ulang | FS-08, ADR-006 | I |
| `TestJobRunner_FencedCompletion_ZombieRollsBack` | — | `CompleteInTx` 0 baris → tx bisnis rollback | ADR-006 | I |
| `TestJobRunner_PermanentError_DeadImmediately` | — | `jobs.Permanent` → dead tanpa menghabiskan attempt | Step 7 `03` | I |
| `TestScheduler_DedupeKey_NoDoubleSchedule` | — | Dua replika worker → satu job per bucket waktu | Step 7 `03` | I |
| `TestRecon_ClassifiesAllCategories` / `TestRecon_Rerun_Idempotent` | — | Klasifikasi 5 kategori; recon ulang tidak menduplikasi; `PROVIDER_SETTLEMENT` tidak dobel | LI-06, LI-12 | I |
| `TestAccountsLock_TopupInsertNotBlockedByTransfer` | — | `FOR NO KEY UPDATE`: INSERT entry tidak terblok (< 100 ms) | Step 7 §4 | I |
| `TestPool_Exhaustion_Returns503` | — | Pool habis → `503 SERVICE_UNAVAILABLE` | Step 7 §6 | I |
| `TestTrace_RequestToJournalToAudit` | — | Dari `request_id` bisa ditelusuri journal → entries → audit | LI-14 | I |
| `TestBOLA/<resource>` | — | User/merchant lain → 404 untuk wallet, transfer, top-up, payout, intent, refund | LI-15, T-LP | I |

### 4.3 LedgerPay — chaos & E2E

| Test | Membuktikan | Trace | CI |
|---|---|---|---|
| `TestTransfer_ConnectionDropBeforeCommit` | Koneksi putus sebelum COMMIT → tidak ada efek, retry aman | FS-03 | X |
| `TestTransfer_CommitOutcomeUnknown` | Hook `testhooks` (Step 12: pengganti Toxiproxy) memutus koneksi tepat setelah COMMIT → client 503 → retry key sama → replay, 1 journal | FS-03 | X |
| `TestConfirm_CrashAfterCommit` | API panic setelah commit sebelum response → retry → replay | FS-04 | X |
| `TestPayoutSubmit_CrashAfterBankAccepted_NoDoubleSend` | Alias `TestPayout_WorkerKilledAfterBankAccepted`. Mock bank mencatat tepat 1 transfer | LI-10, FS-08, **Demo C** | X |
| `TestPayoutSubmit_CrashBeforeBankCall_ResubmitAfterSafeWindow` | Jalur `NOT_FOUND` + safe window, key sama | LI-10 | X |
| `TestPartial/<checkpoint>` | Kill di setiap checkpoint bernomor (top-up, payout, payment) → semua objek berakhir terminal/dapat dilanjutkan + invariant checker hijau | FS-10 | X |
| `E2E_RegisterTopupTransfer` | Register → top-up (checkout mock) → transfer → saldo & riwayat benar | LF-01/02/04/07 | E |
| `E2E_MerchantCheckout` | Merchant membuat intent → user bayar → status & webhook merchant palsu menerima event | LF-10, LF-15 | E |
| `E2E_PayoutUnknownResolved` | Payout `0408` → "Sedang diproses" → selesai otomatis | LF-11 | E |
| `E2E_OpsTraceAndResolve` | Ops mencari transaksi dari request_id, menyelesaikan mismatch dengan bukti | LF-17/18, LI-14 | E |
| `e2e_xss_payload_rendered_as_text`, `e2e_token_not_in_web_storage` | XSS & penyimpanan token | T-SH | E |

### 4.4 PayRun — unit / domain / golden / property

| Test | Membuktikan | Trace | CI |
|---|---|---|---|
| `TestEngineGolden/<30 kasus>` | Lihat kelompok di bawah | PI-05, PI-07, PI-08, PI-12 | U |
| `TestEngine_Recompute_FromCanonicalJSON` | Serialisasi input → deserialisasi → `Calculate` identik untuk semua golden | PI-12 | U |
| `TestProperty_NetEqualsSumOfLines` | `Net == Σ earning − Σ deduction` untuk input acak | PI-05 | U/N |
| `TestProperty_Deterministic` | `Calculate(x) == Calculate(x)`; urutan map/slice input tidak memengaruhi hasil | PI-12 | U/N |
| `TestProperty_ProrationBounds` | Faktor prorata ∈ [0, 1]; Σ faktor segmen ≤ 1 | PI-07 | U/N |
| `TestFingerprint_StableAndSensitive` | Input sama → hash sama; ubah satu field relevan → hash berubah | PI-13 | U |
| `TestComponentGraph_CycleDetected` | Alias `TestComponentConfig_Cycle_Rejected` | PFS-07 | U |
| `TestStateMachine_{PayrollRun,Approval,Adjustment,Period}` | Pasangan from × to | PI-04, PI-11, PI-15 | U |
| `TestPolicy_*` (relasi manager, SELF, SoD) | Policy relasional | PI-11, PI-17 | U |
| `csv_export_escapes_formula`, `pii_roundtrip`, `pii_aad_swap_fails`, `log_redaction_pii_types` | Encoder CSV, enkripsi PII, redaction | T-PR | U |

**Kelompok golden (30 kasus, Step 3 `06` §17):**

| Kelompok | Kasus | Trace |
|---|---|---|
| Dasar | `full_month_basic`, `tono_march_2026`, `february_working_days` | PI-05 |
| Prorata & employment | `joiner_mid_month_working_days`, `joiner_mid_month_calendar_days`, `joiner_on_weekend`, `leaver_mid_month`, `join_and_leave_same_month`, `rehire_two_spans` | PI-07 |
| Effective dating | `raise_mid_month_two_segments`, `two_raises_three_segments`, `overtime_across_raise` | PI-09 |
| Lembur | `overtime_tiers_single_day`, `overtime_over_limit_warning` | PI-08 |
| Absensi & cuti | `unpaid_leave_deduction`, `unexcused_absence_deduction`, `paid_leave_reduces_per_day_present`, `incomplete_attendance_policy_present`, `incomplete_attendance_policy_absent` | PI-08 |
| BPJS, pajak, pembulatan | `bpjs_cap_applied`, `bpjs_rounding_line_vs_total`, `tax_bracket_boundary`, `tax_rounding_down` | PI-05 |
| Komponen | `percent_of_component_chain` | PFS-07 |
| Adjustment | `adjustment_positive_taxable`, `adjustment_negative` | PI-15 |
| Error & warning | `negative_net_error`, `no_salary_error`, `missing_component_ref_error`, `zero_base_salary_warning` | PFS-07 |

### 4.5 PayRun — integration & concurrency

| Test (kanonik) | Alias Step 3 | Membuktikan | Trace | CI |
|---|---|---|---|---|
| `TestCrossTenant/<endpoint>` | `TestCrossTenant_PayrollRunEndpoints_Return404` | Semua role Tenant Beta → 404 di setiap endpoint Alpha | PI-01, PFS-09, **Demo A** | I |
| `TestAuthzMatrix` (table-driven dari Step 5 `05`) | — | Endpoint × role sesuai matriks; endpoint tanpa baris → fail | PI-11, PI-17, T-PR | I |
| `rls_bypass_query_test` | — | Query mentah tanpa filter tenant di `WithTenantTx(Alpha)` → hanya data Alpha | PI-01 | I |
| `rls_no_context_returns_zero_rows` | — | Tanpa tenant context → 0 baris (*fail closed*) | PI-01 | I |
| `pool_leakage_after_commit` | `pool_leakage_test` | Koneksi bekas tenant A tidak mewarisi tenant | PI-01 | I |
| `with_tenant_tx_retry_resets_tenant` | — | Retry `40001` tetap memakai tenant yang benar | PI-01 | I |
| `TestRLS_CompositeFK_RejectsCrossTenantItem` | `cross_tenant_fk_rejected` | Referensi lintas tenant ditolak FK | PI-02 | I |
| `run_create_two_admins_one_wins` | `TestCreateRun_ConcurrentSamePeriod_SingleRun` | Paralel, key berbeda → satu run + `409 ACTIVE_RUN_EXISTS` | PI-03, PFS-06, race (e) | I |
| `run_create_double_click_replays` | `TestCalculate_DoubleSubmit_SameIdempotencyKey_Replays` | Key sama → replay | PI-03, PFS-02 | I |
| `TestFinalizedRun_DirectUpdateRejected` | — | UPDATE/DELETE item/line/snapshot run final → `RUN_FINALIZED` | PI-04 | I |
| `zombie_chunk_vs_finalize` | — | INSERT item tertahan vs finalize → ditolak (trigger `FOR SHARE`) | PI-04, race (g) | I |
| `TestRunTotals_EqualSumOfItems` | — | Total run = Σ item setelah kalkulasi; trigger PI-05 menolak item yang tidak konsisten | PI-05 | I |
| `calc_chunk_old_version_noop` | `TestCalcChunk_StaleCalcVersion_NoOp` | Chunk versi lama no-op setelah recalculate | PI-06, PFS-06, race (f) | I |
| `calc_chunk_zombie_fenced` | `TestCalcChunk_LeaseStolen_FencedCommitRollsBack` | Fencing mencegah `chunks_done` dobel | PI-06, PFS-02 | I |
| `TestEligibility_JoinerLeaverIncludedExactly` | — | Employee masuk run ⇔ aktif ≥ 1 hari | PI-07 | I |
| `TestCutoff_ApprovedAfterCutoff_NextPeriod` | — | Lembur di-approve setelah cut-off tidak masuk run ini | PI-08 | I |
| `salary_change_parallel_one_wins` | — | `23P01` / `412` pada perubahan gaji paralel | PI-09, race (h) | I |
| `leave_parallel_approve_respects_balance` | — | Saldo cuti tidak negatif di bawah approve paralel | PI-10, race (a) | I |
| `approval_approve_vs_reject_single_winner` | `TestApproval_ApproveVsRejectRace_SingleDecision` | Satu keputusan, lainnya `409` | PI-11, PFS-08, race (b) | I |
| `TestApproval_SelfApprovalRejectedByDB` | — | Self-approval cuti ditolak CHECK; pengecualian run hanya untuk tenant satu admin (dihitung server) | PI-11 | I |
| `TestReject_RequiresNote` / `TestRunApprove_BySubmitter_Forbidden` | — | `422 DECISION_NOTE_REQUIRED`; SoD | PFS-08 | I |
| `TestEngine_RecomputeFromStoredSnapshot` | — | Untuk run final di DB: `Calculate(snapshot, ruleset_version)` = item tersimpan | PI-12 | I |
| `approve_blocks_concurrent_input_write` | — | Approve memegang period `FOR UPDATE`, penulis input menunggu | PI-13, O3, race (c) | I |
| `stale_detected_for_writer_committed_after_chunk_read` | `TestSubmit_InputChangedAfterCalc_ReturnsRunStaleWithEmployees` | Penulis commit setelah chunk membaca → `RUN_STALE` | PI-13, O12, PFS-03 | I |
| `stale_detected_when_period_created_concurrently` | — | Celah D1 tertutup | PI-13, D1 | I |
| `stale_false_positive_filtered` | — | Ubah lalu kembalikan nilai → approve sukses | PI-13, O12 | I |
| `stale_detects_new_hire_and_termination` | — | Eligibility berubah setelah kalkulasi | PI-07, PI-13, D6 | I |
| `TestPeriodGuard_FinalizedPeriodRejected` | `TestAttendanceEdit_FinalizedPeriod_PeriodLocked` | Absensi, gaji surut, import CSV ke period final → `PERIOD_LOCKED` | PI-14, PFS-03 | I |
| `TestPeriodGuard_RaiseAfterFinalizedPeriodsAllowed` | — | Hanya rentang yang berubah diperiksa | PI-14 | I |
| `TestAdjustment_AppliedOnce` | — | Adjustment masuk tepat satu run final; batal setelah kalkulasi → run stale | PI-15 | I |
| `owner_parallel_revoke_keeps_one` | — | Advisory lock: dua owner saling cabut → satu gagal `409 LAST_OWNER` | PI-16 | I |
| `TestPayslipAccess_Matrix` | — | Pemilik / karyawan lain / manager / HR; HR ter-audit | PI-17 | I |
| `TestEmployee_NikUniquePerTenant` | — | NIK format berbeda → ditolak; tenant lain → boleh | PI-18 | I |
| `manager_cycle_parallel_rejected` | — | Siklus manager (termasuk paralel) → `422 MANAGER_CYCLE` | PI-19, race (i) | I |
| `TestCalcChunk_TransientError_RetriesWithBackoff` / `TestCalcChunk_PanicExhaustsAttempts_RunFailed` | — | Retry transient; permanent → dead → run `FAILED` | PFS-05 | I |
| `TestSubmit_RunWithErrorItems_Rejected` | — | `422 RUN_HAS_ERRORS` | PFS-07 | I |
| `TestJob_TenantRunMismatch_NoOpAndAlert` | — | Payload job yang tidak cocok dengan tenant → no-op + alert | PFS-09, C-43 | I |
| `TestInvitation_EmailMismatchRejected` | — | Accept dengan email berbeda → `410 INVITATION_INVALID` | T-PR | I |
| `upload_validation_*` | — | Ukuran, MIME + magic bytes, baris, encoding | T-PR, C-38 | I |

### 4.6 PayRun — chaos & E2E

| Test | Membuktikan | Trace | CI |
|---|---|---|---|
| `crash_resume_identical_result` | Alias `TestPayrollRun_WorkerKilledMidChunk_ResumesWithoutDuplicates`. Kill di chunk ke-4, bandingkan dengan run tanpa crash → identik | PI-06, PFS-01, **Demo B** | X |
| `TestCalcChunk_ConnectionDropped_RetriedSafely` | Hook `testhooks` (Step 12: pengganti Toxiproxy) memutus koneksi di tengah chunk | PFS-04 | X |
| `TestFinalize_RetryAfterUnknownCommit_Idempotent` | Hasil commit finalize tidak diketahui → retry → replay / state sudah final | PFS-04 | X |
| `TestPayslipRender_CrashMidBatch_ResumesIdempotently` | Object key deterministik; tidak ada PDF dobel | PFS-10 | X |
| `TestPayslipPDF_DeterministicBytes` | Input sama → byte PDF sama (golden) | PFS-10, PI-12 | U |
| `E2E_TenantIsolation` | Login Alpha, buka URL employee Beta → halaman 404 | **Demo A** | E |
| `E2E_PayrollRunHappyPath` | Run → kalkulasi → review → approve → finalize → slip terlihat oleh employee | PF-13/14/15 | E |
| `E2E_LeaveApprovalFlow` | Employee ajukan cuti → manager approve → saldo berkurang | PF-09, PF-11 | E |
| `E2E_FinalizedImmutability` | Edit absensi period final ditolak; adjustment muncul di run berikutnya | **Demo C** | E |

### 4.7 Shared (kedua project)

| Kelompok | Test | CI |
|---|---|---|
| Authn | `authn_generic_error`, `authn_unknown_email_timing`, `authn_bruteforce_lockout`, `password_policy_rejects_common` | I |
| JWT | `jwt_rejects_alg_none`, `jwt_rejects_alg_swap`, `jwt_rejects_tampered_claims`, `jwt_rejects_unknown_kid`, `jwt_rejects_expired_with_skew` | U |
| Refresh | `refresh_rotation_issues_new_token`, `refresh_reuse_revokes_family`, `refresh_parallel_within_grace_ok`, `refresh_reuse_after_grace_revokes`, `refresh_family_absolute_expiry`, `logout_revokes_family` | I |
| CSRF / CORS | `csrf_refresh_without_header_rejected`, `csrf_refresh_bad_origin_rejected`, `cors_preflight_foreign_origin_denied` | I |
| Input | `mass_assignment_unknown_field_rejected`, `body_limit_413`, `json_depth_limit_400`, `sqli_filter_payloads_rejected` | I |
| Audit | `audit_log_update_rejected`, `audit_log_delete_rejected`, `audit_written_in_same_tx` | I |
| Error | `error_unmapped_returns_generic_500`, `error_no_pg_message_leak`, `error_not_found_uniform`, `TestDBErrorMapping_AllCatalogueConstraints` (setiap constraint di katalog Step 5 `01` §4 menghasilkan code yang benar) | I |
| Headers | `headers_csp_present`, `headers_hsts_present`, `headers_nosniff_present`, `headers_no_store_api` | E (compose) |
| Logging | `log_redaction_auth_body`, `log_redaction_headers` | U |
| Build | `TestBuild_NoTesthooksInRelease`, `TestBuild_DemoEndpointsAbsentWithoutDemoMode` | I |
| Migration | `TestMigrations_UpFromZero`, `TestMigrations_LatestDownUp`, `rls_catalogue_test` (PayRun), `TestCatalogue_AppendOnlyGrants`, `TestCatalogue_StatusColumnsHaveCheck`, `TestCatalogue_TenantFKsComposite` (PayRun), `TestCatalogue_NoSecurityDefinerOutsideAllowlist` (PayRun) | M |

### 4.9 Tambahan dari Step 12 (final review) dan kontrol yang ditambahkan setelah Step 8

| Test | Membuktikan | Telusur ke | Project | Layer |
|---|---|---|---|---|
| `TestInbox_SlowProcessingNotReclaimed` | Event yang sedang diproses (lease aktif) tidak diklaim ulang; `attempts` tidak naik selama verify-by-fetch lambat | Step 12 C1 (R1-1), ADR-L08 | LedgerPay | I |
| `TestRLS_SelfPoliciesInactiveInTenantMode` | Di `WithTenantTx`, policy self (`memberships`, `tenants`, `users`) tidak membuka baris tenant lain milik user multi-tenant | Step 12 C2 (R2-1), PI-01 | PayRun | I |
| `TestCatalog_NoRawCurrentSettingInPolicies` | Tidak ada `current_setting(` mentah di `pg_policies`; semua lewat helper `current_tenant()` / `current_user_id()` | Step 12 C3 (R2-2) | PayRun | M |
| `TestCatalog_EveryFKHasIndex` | Setiap FK punya index yang diawali kolom FK-nya (`pg_constraint` × `pg_index`) | Step 12 C4 (R3-1) | Keduanya | M |
| `TestPayrollRun_ZeroEligibleCompletes` | Run tanpa karyawan eligible langsung `CALCULATED` (total 0, warning `NO_ELIGIBLE_EMPLOYEES`) | Step 12 R1-3, PF-13 | PayRun | I |
| `password_hash_concurrency_bounded` | Paling banyak ±4 hash argon2id bersamaan; antrean > 2 dtk → `503` | C-45 | Keduanya | I |
| `tenant_guard_detects_foreign_row` | Decode repository menolak baris dengan `tenant_id` ≠ context → 500 + metrik INVARIANT | C-46 | PayRun | I |
| `restore_drill_row_counts_positive` | Restore drill (`payrun_backup` BYPASSRLS) menghasilkan jumlah baris tabel tenant > 0; dump kosong = gagal | C-21, Step 9 `03` | Keduanya | Nightly/mingguan (infra) |
| `TestAuth_LockedLoginIndistinguishable` | Login akun terkunci dan email tidak terdaftar menghasilkan respons identik (`401 INVALID_CREDENTIALS` + `Retry-After`) | Step 12 R2-5 | Keduanya | I |
| `TestAudit_BulkSalaryReadAudited` | Request list slip/item gaji milik orang lain menulis satu baris audit `payroll.bulk_read` (+ recheck membership) | Step 12 R2-6, PI-17 | PayRun | I |

**Alias nama test demo PayRun (Step 11 `02`) → nama kanonik katalog:**

| Dipakai di demo script | Nama kanonik |
|---|---|
| `TestTenantIsolation` | Suite `TestCrossTenant_*` / `TestAuthzMatrix` (§4.5) |
| `TestRLS_BypassQuery_ReturnsOnlyCurrentTenant` | `TestRLS_RawQueryWithoutTenantFilter_ReturnsOnlyCurrentTenant` (PI-01) |
| `TestRLS_PoolLeakage` | `TestRLS_PoolConnectionReuse_NoTenantLeak` (PI-01) |
| `TestPayrollRun_CrashAndResume` | `TestPayrollRun_WorkerCrashMidChunk_ResumesWithoutDuplicates` (PFS-01) |

> Kalau nama kanonik di §4.5/§4.6 berbeda dari kolom kanan, nama di §4.5/§4.6 yang berlaku (O23); demo script mengikuti katalog saat coding.

---

## 5. Traceability matrix

Status semua baris: **planned**. Matriks ini wajib diperbarui di PR yang menambah atau mengubah invariant/skenario. Test `TestTraceability_DocsReferenceExistingTests` (nightly; **Step 12: S17, mulai M4**) mem-parse dokumen ini dan memastikan setiap nama test benar-benar ada di kode.

### 5.1 LedgerPay invariants

| Invariant | Test |
|---|---|
| LI-01 Journal seimbang | `TestJournal_Validate`, `TestProperty_JournalAlwaysBalanced`, `TestJournal_UnbalancedRejectedAtCommit` |
| LI-02 Saldo ≥ 0 | `TestTransfer_ConcurrentDoubleSpend_100x`, `TestTransfer_NaiveImplementation_Overdraws`, `TestLedger_AppValidationRemoved_DBStillRejects` |
| LI-03 Saldo = Σ entries | `TestProperty_RandomTransfersConserveTotal`, `TestLedgerVerify_DetectsInjectedDrift` |
| LI-04 Append-only, reversal sekali | `TestLedger_AppendOnly`, `TestLedger_DoubleReversalRejected` |
| LI-05 Satu key satu efek | `TestTransfer_IdempotentReplay`, `TestTransfer_RejectedResultIsReplayed`, `TestIdempotency_ConcurrentSameKey_OneEffect`, `TestIdempotency_FirstTxRolledBack_SecondProceeds`, `idempotency_cross_client_isolated` |
| LI-06 Satu journal per (source, purpose) | `TestJournalPurpose_Templates`, `TestInbox_LeaseExpired_ProcessedOnce`, `TestRecon_Rerun_Idempotent` |
| LI-07 Transisi sah | `TestStateMachine_{TopUp,PaymentIntent,Payout,Inbox}`, `TestPayoutStatusCheck_VsOpsResolve` |
| LI-08 Σ refund ≤ amount | `TestRefund_ConcurrentExceedingAmount` |
| LI-09 Event webhook sekali | `TestWebhook_DuplicateEvents_CreditOnce`, `TestInbox_LeaseExpired_ProcessedOnce` |
| LI-10 Payout ke bank sekali | `TestPayoutSubmit_CrashAfterBankAccepted_NoDoubleSend`, `TestPayoutSubmit_CrashBeforeBankCall_ResubmitAfterSafeWindow`, `TestPayout_TimeoutAfterProcess_ResolvesViaStatusQuery` |
| LI-11 Integer minor unit | `TestMoney_*`, contract test uang-sebagai-string (`TestContract_MoneyIsString`) |
| LI-12 Liability cocok aset (eventual) | `TestRecon_ClassifiesAllCategories`, `TestRecon_Rerun_Idempotent` |
| LI-13 Intent dibayar sekali | `TestConfirm_ParallelTabs_OnePayment`, `TestPaymentIntent_DuplicateMerchantOrder_409` |
| LI-14 Traceability | `TestTrace_RequestToJournalToAudit`, `E2E_OpsTraceAndResolve` |
| LI-15 Ownership | `TestBOLA/<resource>`, `TestPolicy_*` |

### 5.2 PayRun invariants

| Invariant | Test |
|---|---|
| PI-01 Satu tenant per baris | `TestCrossTenant/<endpoint>`, `rls_bypass_query_test`, `rls_no_context_returns_zero_rows`, `pool_leakage_after_commit`, `with_tenant_tx_retry_resets_tenant`, `rls_catalogue_test` |
| PI-02 Tanpa referensi lintas tenant | `TestRLS_CompositeFK_RejectsCrossTenantItem`, `TestCatalogue_TenantFKsComposite` |
| PI-03 Satu run aktif per period | `run_create_two_admins_one_wins`, `run_create_double_click_replays` |
| PI-04 Run final immutable | `TestFinalizedRun_DirectUpdateRejected`, `zombie_chunk_vs_finalize` |
| PI-05 Total = Σ | `TestRunTotals_EqualSumOfItems`, `TestProperty_NetEqualsSumOfLines`, golden "Dasar" + "BPJS, pajak, pembulatan" |
| PI-06 Satu item per (run, employee, version) | `calc_chunk_old_version_noop`, `calc_chunk_zombie_fenced`, `crash_resume_identical_result` |
| PI-07 Eligibility & prorata | `TestEligibility_JoinerLeaverIncludedExactly`, `TestProperty_ProrationBounds`, golden "Prorata & employment", `stale_detects_new_hire_and_termination` |
| PI-08 Hanya approved ≤ cut-off | `TestCutoff_ApprovedAfterCutoff_NextPeriod`, golden "Lembur" + "Absensi & cuti" |
| PI-09 Effective dating tanpa overlap | `salary_change_parallel_one_wins`, golden "Effective dating" |
| PI-10 Saldo cuti ≥ 0 | `leave_parallel_approve_respects_balance` |
| PI-11 Approval sekali, tanpa self-approval | `approval_approve_vs_reject_single_winner`, `TestApproval_SelfApprovalRejectedByDB`, `TestRunApprove_BySubmitter_Forbidden`, `TestAuthzMatrix` |
| PI-12 Determinisme | `TestProperty_Deterministic`, `TestEngine_Recompute_FromCanonicalJSON`, `TestEngine_RecomputeFromStoredSnapshot`, `TestPayslipPDF_DeterministicBytes` |
| PI-13 Approve hanya jika input tidak berubah | `approve_blocks_concurrent_input_write`, `stale_detected_for_writer_committed_after_chunk_read`, `stale_detected_when_period_created_concurrently`, `stale_false_positive_filtered`, `TestFingerprint_StableAndSensitive` |
| PI-14 Period final terkunci | `TestPeriodGuard_FinalizedPeriodRejected`, `TestPeriodGuard_RaiseAfterFinalizedPeriodsAllowed` |
| PI-15 Adjustment sekali | `TestAdjustment_AppliedOnce`, golden "Adjustment" |
| PI-16 ≥ 1 owner | `owner_parallel_revoke_keeps_one` |
| PI-17 Akses slip | `TestPayslipAccess_Matrix`, `TestAuthzMatrix` |
| PI-18 Unik per tenant | `TestEmployee_NikUniquePerTenant` |
| PI-19 Manager asiklik | `manager_cycle_parallel_rejected` |

### 5.3 Failure scenarios

| Skenario | Test |
|---|---|
| FS-01 Duplicate request | `TestTransfer_IdempotentReplay`, `TestTransfer_RejectedResultIsReplayed` |
| FS-02 Concurrent payment | `TestTransfer_ConcurrentDoubleSpend_100x`, `TestTransfer_OppositeDirections_NoDeadlock`, `TestRefund_ConcurrentExceedingAmount` |
| FS-03 Database failure | `TestTransfer_ConnectionDropBeforeCommit`, `TestTransfer_CommitOutcomeUnknown` |
| FS-04 Server crash | `TestConfirm_CrashAfterCommit` |
| FS-05 Provider timeout | `TestPayout_TimeoutAfterProcess_ResolvesViaStatusQuery`, `TestTopUp_ProviderTimeout_RetryResumesFromRecoveryPoint` |
| FS-06 Webhook duplicate | `TestWebhook_DuplicateEvents_CreditOnce`, `TestWebhook_BadSignature_401`, `TestWebhook_StaleTimestamp_400` |
| FS-07 Webhook terlambat / tidak berurutan | `TestTopup_LateWebhookAfterExpiry_Credited`, `TestWebhook_OutOfOrder_NoRegression`, `TestWebhook_FinalStateConflict_FlagsRecon` |
| FS-08 Worker crash | `TestPayoutSubmit_CrashAfterBankAccepted_NoDoubleSend`, `TestJobRunner_LeaseExpiry_Reclaims` |
| FS-09 Network timeout | `TestPaymentIntent_RetryAfterClientTimeout`, `TestMerchantWebhook_RetryBackoffThenDead`, `TestMerchantWebhook_StableEventID` |
| FS-10 Partial processing | `TestPartial/<checkpoint>`, `TestTopupRecover_StuckCreated_Completed` |
| PFS-01 Job crash | `crash_resume_identical_result` |
| PFS-02 Duplicate execution | `run_create_double_click_replays`, `calc_chunk_zombie_fenced` |
| PFS-03 Employee berubah | `stale_detected_for_writer_committed_after_chunk_read`, `TestPeriodGuard_FinalizedPeriodRejected` |
| PFS-04 Database failure | `TestCalcChunk_ConnectionDropped_RetriedSafely`, `TestFinalize_RetryAfterUnknownCommit_Idempotent` |
| PFS-05 Worker retry | `TestCalcChunk_TransientError_RetriesWithBackoff`, `TestCalcChunk_PanicExhaustsAttempts_RunFailed` |
| PFS-06 Concurrent execution | `run_create_two_admins_one_wins`, `calc_chunk_old_version_noop` |
| PFS-07 Konfigurasi salah | golden "Error & warning", `TestSubmit_RunWithErrorItems_Rejected`, `TestComponentGraph_CycleDetected` |
| PFS-08 Approval rejection | `approval_approve_vs_reject_single_winner`, `TestReject_RequiresNote`, `TestRunApprove_BySubmitter_Forbidden` |
| PFS-09 Tenant access violation | `TestCrossTenant/<endpoint>`, `TestJob_TenantRunMismatch_NoOpAndAlert`, `TestRLS_CompositeFK_RejectsCrossTenantItem` |
| PFS-10 Partial processing | `TestPayslipRender_CrashMidBatch_ResumesIdempotently`, `TestPayslipPDF_DeterministicBytes` |

### 5.4 Threat MVP (Step 6) per kategori

| Kategori threat | Test |
|---|---|
| T-SH authn / password / brute force | §4.7 Authn |
| T-SH JWT / refresh / sesi | §4.7 JWT, Refresh |
| T-SH CSRF / CORS / XSS | §4.7 CSRF / CORS, `e2e_xss_payload_rendered_as_text`, `lint_no_v_html` |
| T-SH injection / mass assignment / body | §4.7 Input |
| T-SH error / logging / audit | §4.7 Error, Logging, Audit |
| T-SH headers / TLS | §4.7 Headers |
| T-SH supply chain / container | Job CI (gosec, govulncheck, osv-scanner, trivy, gitleaks, `compose_hardening_check`, `pinned_actions_check`) |
| T-LP double spend / idempotency / amount | §4.2 (LI-02, LI-05, LI-11) |
| T-LP BOLA / enumerasi | `TestBOLA/<resource>`, `TestTransfer_InvalidRecipientGeneric` |
| T-LP webhook forgery / replay / tampering | `TestWebhook_BadSignature_401`, `TestWebhook_StaleTimestamp_400`, `TestWebhook_AmountMismatch_Dead`, `TestWebhook_VerifyByFetch_MismatchNotCredited` |
| T-LP SSRF | `webhook_egress_blocks_private_ip`, `webhook_egress_blocks_metadata_ip`, `webhook_egress_no_redirect`, `webhook_egress_dns_rebinding_pinned_ip` |
| T-LP API key / ops insider | `api_key_hashed_at_rest`, `api_key_slot_limit`, `ops_route_rejects_user_role`, `ops_resolution_requires_evidence`, `ops_action_audited` |
| T-LP demo/admin endpoint terekspos | `TestBuild_DemoEndpointsAbsentWithoutDemoMode`, `mock_admin_requires_token` |
| T-PR cross-tenant / RLS misconfiguration | §4.5 PI-01/PI-02, §4.7 Migration |
| T-PR privilege escalation / SoD / undangan | `TestAuthzMatrix`, `TestRunApprove_BySubmitter_Forbidden`, `TestInvitation_EmailMismatchRejected`, `role_grant_rules_enforced` |
| T-PR PII / payslip URL | `pii_*`, `TestPayslipAccess_Matrix`, `payslip_presigned_url_ttl` |
| T-PR upload / CSV injection | `upload_validation_*`, `csv_export_escapes_formula` |
| T-PR job tenant confusion | `TestJob_TenantRunMismatch_NoOpAndAlert`, `jobs_enforce_tenant_trigger` |
| Step 12 Critical C1–C4 | `TestInbox_SlowProcessingNotReclaimed`, `TestRLS_SelfPoliciesInactiveInTenantMode`, `TestCatalog_NoRawCurrentSettingInPolicies`, `TestCatalog_EveryFKHasIndex` |
| C-45 / C-46 / C-21 | `password_hash_concurrency_bounded`, `tenant_guard_detects_foreign_row`, `restore_drill_row_counts_positive` |
| Step 12 R1-3 / R2-5 / R2-6 | `TestPayrollRun_ZeroEligibleCompletes`, `TestAuth_LockedLoginIndistinguishable`, `TestAudit_BulkSalaryReadAudited` |

---

## 6. Fixture & test data

| Komponen | Keputusan |
|---|---|
| Builder | `fixture.NewUser(t, db, opts...)`, `fixture.FundedWallet(t, db, amount)`, `fixture.Merchant(t, db)`. PayRun: `fixture.Tenant(t, db, "alpha")` menghasilkan tenant + satu user per role (`OWNER`, `HR_ADMIN`, `PAYROLL_ADMIN`, `MANAGER` dengan 3 bawahan, `EMPLOYEE`) + kalender kerja + komponen default; `fixture.Tenant(t, db, "beta")` untuk test lintas tenant |
| Nilai default | Builder mengisi field wajib dengan nilai valid; test hanya menimpa yang relevan dengan skenarionya |
| Waktu | Interface `Clock` (Step 2 skeleton) → `clock.Fake` di test. Waktu DB (`now()`, `clock_timestamp()`) **tidak** di-fake; test yang bergantung waktu DB memakai jarak relatif, bukan tanggal absolut |
| ID | Interface `idgen` → generator deterministik di unit test (golden stabil); UUIDv7 asli di integration |
| Random | Seed property test dicatat di output (`rapid` otomatis). Data acak di integration memakai `rand.New(rand.NewPCG(seed, …))` dengan seed dari `-seed` atau waktu, dan dicetak saat gagal |
| Mock-provider | **Integration:** paket `mockprovider` dijalankan **in-process** sebagai `httptest.Server` dengan kode server yang **sama** dengan service mock-provider (satu package Go, dua cara menjalankan). Lebih cepat dan bisa dikendalikan per test. **E2E/chaos:** container mock-provider sungguhan |
| Golden payroll | `payroll/engine/testdata/cases/<nama>.json` (input + ruleset) → `<nama>.golden.json` (hasil lengkap + explanation). Ruleset contoh `ID-SIMPLIFIED-2026.1` disalin ke `testdata/rulesets/` agar golden tidak berubah ketika ruleset production berubah |
| Data E2E | `make seed-e2e`: dua tenant (Alpha 20 karyawan, Beta 5), satu merchant, beberapa user dengan saldo. Deterministik |
| Invariant checker | `invariants.CheckLedgerPay(t, db)` / `invariants.CheckPayRun(t, db)` dipanggil di `t.Cleanup` semua test integration/concurrency/chaos. Pelanggaran = test gagal walaupun assertion test itu sendiri lolos |

---

## 7. CI gating

```text
PR ──► lint (golangci-lint+gosec, ESLint, workflow lint)           ─┐
   ──► unit + golden + property(10k)  -race                          │ paralel
   ──► security static (govulncheck, osv-scanner, gitleaks)         ─┘
        │
        ▼
   integration (paralel per package, -race, testcontainers)
   + contract + migration catalogue
        │
        ▼
   build image → trivy → compose up → E2E (Playwright) + headers check
        │
        ▼  (label "chaos" atau perubahan di jobs/, payout/, payroll/run/)
   chaos suite (testhooks putus koneksi, kill worker)

nightly: property 1 juta iterasi · chaos penuh · demo-verify · load smoke (k6, file 03) · ZAP baseline (P2)
         · TestTraceability_DocsReferenceExistingTests (Step 12: S17, ditunda ke M4)
```

| Aturan | Keputusan |
|---|---|
| Memblok merge | Lint, unit, integration, contract, migration catalogue, security static, image scan, E2E. Chaos memblok hanya jika dijalankan (label/path) |
| `-race` | Selalu aktif untuk unit dan integration |
| Timeout | Unit 2 menit, integration 10 menit, E2E 10 menit (`go test -timeout`, job timeout GitHub Actions) |
| Kebijakan flaky | Test flaky **tidak** di-retry sampai hijau. Ditandai `t.Skip("quarantine: #<issue>")` lewat label `quarantine`, issue dibuka, dan SLA perbaikan **7 hari**. Test karantina tetap berjalan di nightly agar datanya terkumpul. Lebih dari 3 test di karantina berarti fitur baru dibekukan sampai jumlahnya turun |
| Laporan | JUnit XML (`gotestsum --junitfile`) sebagai artifact + ringkasan di PR; coverage `go test -coverprofile` per package domain (indikatif ≥ 70%, tidak memblok); laporan Playwright (trace + video untuk test gagal) |
| Cache | Image Postgres di-pull sekali per job; cache modul Go dan npm |

---

## 8. Demo verification (`make demo-verify`)

Demo live adalah momen paling berisiko di proses interview, jadi demo diperlakukan sebagai test.

| Demo | Skrip | Assertion otomatis |
|---|---|---|
| LedgerPay A: Idempotency | `scripts/demo/a_idempotency.sh` (curl) | Request #1 = #2 (body sama + `Idempotent-Replayed`); payload beda → 422; saldo berubah sekali; `count(journals WHERE source=…) = 1` |
| LedgerPay B: Concurrency | `scripts/demo/b_concurrency.js` (k6) | 10 sukses, 90 `INSUFFICIENT_BALANCE`, saldo 0, invariant checker hijau |
| LedgerPay C: Failure recovery | `scripts/demo/c_payout_crash.sh` (skenario `timeout_after_process` + `demo_faults` kill worker) | `GET /admin/transfers` mock = 1 transfer; payout `SUCCEEDED`; satu journal settle |
| LedgerPay D: Reconciliation | `scripts/demo/d_recon.sh` (`payout_status_query: disabled` + `settlement_mismatch:status`) | Recon item `STATUS_MISMATCH` muncul; resolusi ops tercatat di audit |
| PayRun A: Isolation | `scripts/demo/pr_a_isolation.sh` + `go test ./... -run 'TestCrossTenant|rls_'` | 404 lintas tenant; test RLS bypass hijau |
| PayRun B: Crash & resume | `scripts/demo/pr_b_crash.sh` (chunk size 10, kill di karyawan ke-37) | Run `CALCULATED`, 100 item, total = Σ item, sama dengan run tanpa crash |
| PayRun C: Immutability | `scripts/demo/pr_c_immutable.sh` | Edit absensi → `PERIOD_LOCKED`; adjustment muncul di run berikutnya; recompute snapshot identik |

- `make demo-verify` menjalankan semua skrip **headless** terhadap compose profile `demo` yang di-seed ulang (`make demo-reset`), dan gagal kalau satu assertion gagal.
- Dijalankan **nightly** di CI dan **wajib** dijalankan manual sebelum setiap sesi interview. Skrip yang sama dipakai saat demo live, sehingga yang ditunjukkan ke interviewer adalah hal yang sudah teruji setiap malam.
