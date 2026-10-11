# PayRun: Payroll Failure Scenarios

> Format setiap skenario: **Setup → Request → Failure → Detection → Handling → Recovery → Final State**, lalu invariant yang dijaga, test otomatis, dan demo terkait.
> Nama state, job, dan ID invariant mengikuti glosarium. Mekanisme: [ADR-P05](../adr/ADR-P05-payroll-run-execution.md) (run + chunk), [ADR-P07](../adr/ADR-P07-snapshot-and-period-lock.md) (fingerprint), [ADR-P09](../adr/ADR-P09-approval-workflow.md) (approval), [ADR-006](../adr/ADR-006-background-processing-postgres-queue.md) (job runner), [ADR-011](../adr/ADR-011-transaction-management.md) (transaksi), [ADR-P01](../adr/ADR-P01-multi-tenancy-pooled-rls.md)/[ADR-P02](../adr/ADR-P02-tenant-context.md) (tenancy).

## Setup bersama

| Item | Nilai |
|---|---|
| Tenant | **PT Alpha** (100 karyawan aktif), **PT Beta** (40 karyawan) |
| Periode | Maret 2026 (`PayrollPeriod` status awal `OPEN`) |
| User PT Alpha | Dewi (`HR_ADMIN`), Hendra (`PAYROLL_ADMIN`), Bu Ratna (`OWNER`) |
| User PT Beta | Sinta (`HR_ADMIN`, `PAYROLL_ADMIN`) |
| Chunk size | **10** (konfigurasi demo agar progres terlihat; default production 50) → 100 karyawan = 10 chunk |
| Job runner | Lease 30 detik, `max_attempts = 5`, backoff `min(2s · 2^attempt, 10m) ± jitter` |
| Fencing | `jobs.CompleteInTx(tx, job)` menjalankan `UPDATE jobs SET status='succeeded' WHERE id=$id AND locked_by=$worker AND status='running'`. Jika 0 baris, lease sudah diambil worker lain, sehingga seluruh transaksi chunk di-rollback |

---

## PFS-01 — Payroll Job Crash (worker mati di tengah chunk)

**Request**
1. Hendra: `POST /v1/payroll-runs/{run}/calculate` → tx: `DRAFT → CALCULATING`, `calc_version = 1`, `chunks_total = 10`, `chunks_done = 0`, enqueue `payroll.plan` → `202 Accepted`.
2. `payroll.plan` membuat 10 job `payroll.calc_chunk` (unique key `run+v1+chunk_no`).
3. Chunk 1–3 commit: 30 item, `chunks_done = 3`.

**Failure**
4. Worker W1 mengambil chunk 4 (karyawan ke-31 s.d. 40) dan sedang menghitung karyawan ke-37 ketika container dimatikan (`docker kill payrun-worker`). Transaksi chunk 4 belum commit.

**Detection**
5. Postgres mendeteksi koneksi putus → transaksi chunk 4 di-rollback otomatis (0 item chunk 4 tersimpan).
6. Job chunk 4 tetap `running` dengan `locked_until` = T+30s. Setelah lewat, query claim memperlakukannya sebagai lease kedaluwarsa.
7. Metrik `job_lease_expired_total{kind="payroll.calc_chunk"}` naik. UI menampilkan progres berhenti di 3/10.

**Handling**
8. Worker W2 (atau W1 setelah restart) meng-claim chunk 4: `attempts = 2`, `locked_by = W2`.
9. Guard di awal chunk: run masih `CALCULATING` dan `calc_version = 1` → lanjut.
10. Chunk 4 dihitung ulang dari awal. Engine deterministik, jadi hasilnya sama dengan yang akan dihasilkan W1.

**Recovery** — otomatis, tanpa intervensi.
11. Chunk 5–10 berjalan normal. Chunk terakhir yang commit membuat `chunks_done = 10 = chunks_total`, memverifikasi `count(items v1) = 100`, menghitung total + fingerprint, lalu run → `CALCULATED`.

**Final State**
| Item | Nilai |
|---|---|
| Run | `CALCULATED`, `calc_version = 1`, `chunks_done = 10/10` |
| Item | **100** (tidak ada duplikat; unique `(run_id, employee_id, calc_version)`) |
| Total | `Σ net` 100 item |
| Job chunk 4 | `succeeded`, `attempts = 2` |

**Invariant yang dijaga:** PI-05, PI-06, PI-12.
**Test:** `TestPayrollRun_WorkerKilledMidChunk_ResumesWithoutDuplicates`. Implementasinya memakai failure hook `PAYRUN_FAIL_AFTER_EMPLOYEE=37` yang memanggil `os.Exit` (hanya aktif di build test/demo).
**Demo:** **Demo B**.

---

## PFS-02 — Duplicate Payroll Execution

### 2a. Tombol "Calculate" ditekan dua kali

**Request → Failure**
1. Hendra menekan "Calculate" dua kali dalam 200 ms. Request A dan B membawa `Idempotency-Key` yang sama (dibuat sekali per aksi oleh UI).

**Detection → Handling**
2. A: `UPDATE payroll_runs SET status='CALCULATING' … WHERE status IN ('DRAFT','CALCULATED','FAILED')` → 1 baris → `202`.
3. B: key yang sama → menunggu unique index idempotency, lalu me-*replay* response A (`202`, `Idempotent-Replayed: true`).
4. Tanpa key (client lain / curl): guard state mengembalikan 0 baris → `409 INVALID_STATE_TRANSITION {current_status: "CALCULATING"}`. UI menampilkan progres run yang sedang berjalan.

### 2b. Job dikirim dua kali (at-least-once delivery)

**Failure**
1. W1 mengambil chunk 6, tetapi terhenti karena GC pause / jaringan lambat selama 40 detik (melewati lease 30 detik).
2. W2 meng-claim chunk 6 (lease kedaluwarsa): `locked_by = W2`, `attempts = 2`.
3. Keduanya menghitung chunk 6 secara bersamaan.

**Detection → Handling**
4. W2 commit lebih dulu: upsert 10 item, `CompleteInTx` → 1 baris (locked_by = W2), `chunks_done` +1.
5. W1 bangun dan mencoba commit: `CompleteInTx` → **0 baris** (locked_by sudah W2) → transaksi W1 di-rollback, termasuk upsert item dan increment `chunks_done`. Metrik `job_fenced_total` naik.
6. Kalaupun fencing gagal (bug), upsert item menulis nilai identik (deterministik) dan verifikasi `count(items) = eligible` sebelum `CALCULATED` menangkap counter yang tidak konsisten.

**Final State**
| Item | Nilai |
|---|---|
| Run | Satu run, `CALCULATED` |
| Item | 100, satu per karyawan |
| `chunks_done` | Tepat 10 (tidak 11) |

**Invariant yang dijaga:** PI-03, PI-06.
**Test:** `TestCalculate_DoubleSubmit_SameIdempotencyKey_Replays`, `TestCalcChunk_LeaseStolen_FencedCommitRollsBack`.
**Demo:** Bonus Demo B ("klik Run dua kali").

---

## PFS-03 — Employee Changed During Payroll

**Request**
1. Run Maret `CALCULATED` (`calc_version = 1`, `input_fingerprint = F1`).

**Failure** (tiga variasi, dan semuanya terjadi setelah kalkulasi, sebelum approve)
2. Dewi mengoreksi attendance Tono (E012) tanggal 10 Maret: `ABSENT_UNEXCUSED → PRESENT`.
3. Dewi menginput kenaikan gaji Andi (E045) berlaku 20 Maret.
4. Dewi mengisi tanggal keluar Budi (E077) = 25 Maret.

Semua edit **diizinkan**, karena periode berstatus `PROCESSING` (ADR-P07).

**Detection**
5. Hendra: `POST /v1/payroll-runs/{run}/submit` → server menghitung ulang `run_fingerprint` dari data saat ini (tanpa engine) = F2 ≠ F1.
6. Response `409 RUN_STALE {employees_changed: ["E012","E045","E077"]}`.

**Handling**
7. UI menampilkan daftar karyawan yang berubah beserta jenis perubahannya; tombol "Recalculate".
8. Recalculate → `calc_version = 2`, 10 chunk baru. Untuk Budi, `payroll.plan` versi 2 tetap memasukkannya (masih aktif 1–24 Maret) dengan prorata span baru.

**Catatan konsistensi baca:** chunk membaca input dengan Read Committed. Kalau edit commit *di tengah* pembacaan satu karyawan, `input_fingerprint` item mencerminkan input yang **benar-benar dipakai**, sehingga guard submit tetap mendeteksi selisih. Tidak perlu `REPEATABLE READ` untuk correctness.

**Recovery**
9. Run `CALCULATED` v2 dengan fingerprint F2 → submit berhasil → `PENDING_APPROVAL`.
10. Variasi setelah final: edit attendance Maret setelah run `FINALIZED` → `409 PERIOD_LOCKED`. Koreksi lewat adjustment ke April.

**Final State**
| Item | Nilai |
|---|---|
| Run | `PENDING_APPROVAL`, `calc_version = 2`, fingerprint F2 |
| Item v2 | 100 (item v1 dihapus setelah v2 `CALCULATED`, atau disimpan untuk diff) |
| Net E012 | Naik (potongan absen hilang); E045: dua segmen gaji; E077: prorata 1–24 Maret |

**Invariant yang dijaga:** PI-07, PI-13, PI-14.
**Test:** `TestSubmit_InputChangedAfterCalc_ReturnsRunStaleWithEmployees`, `TestAttendanceEdit_FinalizedPeriod_PeriodLocked`.

---

## PFS-04 — Database Failure

### 4a. Koneksi putus di tengah chunk
1. Chunk 8 sedang upsert ketika jaringan worker → DB putus.
2. **Detection:** error `pgconn` (connection reset) → transaksi gagal; Postgres me-rollback transaksi di sisi server.
3. **Handling:** job runner menandai attempt gagal → job `queued`, `run_at = now() + backoff`. Kalau bahkan update status job gagal (DB tidak terjangkau), lease akan kedaluwarsa sendiri.
4. **Recovery:** DB terjangkau lagi → chunk 8 diulang → sama seperti PFS-01.

### 4b. Database mati total selama 2 menit
1. **Detection:** `GET /readyz` api dan worker → `503` (dependency `postgres` down); metrik `db_up = 0`; alert.
2. **Handling:** api mengembalikan `503 SERVICE_UNAVAILABLE` (Problem Details) tanpa efek parsial. Worker berhenti meng-claim dan mencoba lagi dengan backoff.
3. **Recovery:** setelah DB kembali, job yang `queued` atau dengan lease kedaluwarsa dilanjutkan. Tidak ada state yang hidup di memori yang hilang, karena semua state run ada di DB.

### 4c. Koneksi putus tepat saat `COMMIT` finalize
1. Bu Ratna: `POST /v1/payroll-runs/{run}/finalize` (`Idempotency-Key: K`). Transaksi finalize (guard fingerprint → `FINALIZED` → periode `FINALIZED` → enqueue `payslip.render` → simpan respons idempotency) mengirim `COMMIT`, lalu koneksi putus sebelum acknowledgment.
2. **Detection:** API tidak tahu apakah commit berhasil → `500` ke client (outcome unknown).
3. **Handling:** UI melakukan retry dengan key K.
   - Jika commit **berhasil**: record idempotency K ada → replay `200` (run `FINALIZED`).
   - Jika commit **gagal**: tidak ada efek apa pun → finalize dijalankan ulang secara normal.
4. **Final State:** run `FINALIZED` tepat sekali. Job `payslip.render` ada tepat sekali, karena di-enqueue di transaksi yang sama (outbox).

**Invariant yang dijaga:** PI-04, PI-06, PI-12.
**Test:** `TestCalcChunk_ConnectionDropped_RetriedSafely` (toxiproxy/pemutusan koneksi di testcontainers), `TestFinalize_RetryAfterUnknownCommit_Idempotent`.
**Di luar V1:** failover ke replica asinkron yang kehilangan transaksi terakhir (RPO > 0). Dibahas di arsitektur V2.

---

## PFS-05 — Worker Retry (transient vs permanent)

### 5a. Gagal sementara, lalu berhasil
1. Chunk 7 attempt 1 gagal dengan `57014 statement_timeout` karena DB sedang sibuk (autovacuum besar).
2. **Handling:** error diklasifikasikan *transient* → job `queued`, `run_at = +2s ± jitter`. Attempt 2 gagal lagi → `+4s`. Attempt 3 berhasil.
3. **Final State:** run `CALCULATED`, 100 item, chunk 7 `attempts = 3`. Metrik `job_retry_total` naik 2.

### 5b. Gagal permanen (bug)
1. Engine `panic` (nil map) untuk karyawan dengan konfigurasi komponen yang langka, berada di chunk 9.
2. **Handling:** job runner me-*recover* panic → attempt gagal dengan stack trace di `last_error` → retry 5× → `dead`.
3. **Detection:** handler `dead` untuk `payroll.calc_chunk` menjalankan transisi `CALCULATING → FAILED` dan menyimpan `failure_reason` + nomor chunk; metrik `job_dead_total{kind}` → alert.
4. **Recovery (manual):** perbaikan kode di-deploy → Hendra `recalculate` dari `FAILED` → `calc_version = 2` → semua 10 chunk dihitung ulang (bukan hanya chunk 9, agar seluruh item memakai kode yang sama).
5. **Final State:** run `CALCULATED` v2, 100 item; item v1 parsial (90) dihapus.

**Pembeda penting:** masalah *data* (konfigurasi salah, gaji tidak ada) **bukan** kegagalan job. Masalah data menjadi item `ERROR` tanpa retry (PFS-07). Retry hanya untuk kegagalan *sistem* (DB, timeout, panic), karena me-retry data yang salah hanya membuang waktu dan menunda deteksi.

**Invariant yang dijaga:** PI-05, PI-06.
**Test:** `TestCalcChunk_TransientError_RetriesWithBackoff`, `TestCalcChunk_PanicExhaustsAttempts_RunFailed`.

---

## PFS-06 — Concurrent Payroll Execution

### 6a. Dua admin membuat run periode yang sama bersamaan
1. Hendra dan Bu Ratna (dua browser) menekan "Buat run Maret" pada detik yang sama, dengan key berbeda.
2. Keduanya `INSERT payroll_runs (tenant, period, 'DRAFT')`. Partial unique index `(tenant_id, period_id) WHERE status <> 'CANCELLED'` → satu berhasil, satu `23505`.
3. **Handling:** API memetakan `23505` pada index ini menjadi `200 OK` dengan run yang sudah ada (create idempoten secara semantik), dengan header `X-Existing-Resource: true`.
4. **Final State:** **1 run** untuk Maret.

### 6b. Recalculate ketika chunk versi lama masih di antrean
1. Run v1 `CALCULATING` dengan chunk 1–4 selesai, lalu Hendra menyadari ada setting yang salah. Recalculate maupun cancel dari `CALCULATING` ditolak (`409 INVALID_STATE_TRANSITION`, ADR-P05). **Keputusan V1:** dari `CALCULATING` Hendra hanya bisa menunggu sampai `CALCULATED`/`FAILED`, lalu recalculate → v2.
2. Kasus yang tetap mungkin: job chunk v1 yang tertunda (backoff) baru dieksekusi setelah v2 dimulai.
3. **Handling:** guard awal chunk: `calc_version` job (1) ≠ run (2) → **no-op**, job `succeeded`, tidak menyentuh item maupun counter. `UPDATE … WHERE calc_version = $v` juga memastikan counter v2 tidak tersentuh.
4. **Final State:** run `CALCULATED` v2, `chunks_done` v2 = 10, item v2 = 100.

### 6c. Approve berbarengan dengan recalculate
1. Run `PENDING_APPROVAL`. Hendra mencoba recalculate sementara Bu Ratna approve.
2. Recalculate dari `PENDING_APPROVAL` tidak diizinkan → `409 INVALID_STATE_TRANSITION`. Approve berhasil → `APPROVED`.

**Invariant yang dijaga:** PI-03, PI-06, PI-11.
**Test:** `TestCreateRun_ConcurrentSamePeriod_SingleRun` (2 goroutine + barrier), `TestCalcChunk_StaleCalcVersion_NoOp`.

---

## PFS-07 — Invalid Salary Configuration

**Setup:** dari 100 karyawan PT Alpha, tiga punya masalah konfigurasi.

| Karyawan | Masalah |
|---|---|
| E020 | Komponen `BONUS_KPI` (`PERCENT_OF TUNJ_KHUSUS`), padahal `TUNJ_KHUSUS` dinonaktifkan setelah assignment dibuat |
| E033 | Tidak punya salary assignment yang beririsan dengan Maret (HR lupa input) |
| E061 | Adjustment −Rp9.000.000 (koreksi kelebihan bayar) lebih besar dari earning bulan ini |

**Pencegahan di depan:** membuat siklus (`A = % × B`, `B = % × A`) atau referensi ke komponen yang tidak ada **saat menyimpan konfigurasi** → `422 COMPONENT_CYCLE` / `422 INVALID_COMPONENT_REFERENCE`. Kasus di atas adalah yang lolos dari validasi itu (data berubah setelahnya).

**Request → Failure**
1. Calculate → semua chunk sukses (masalah data bukan kegagalan job).
2. Engine menghasilkan item `ERROR`: E020 `COMPONENT_REF_MISSING`, E033 `NO_SALARY`, E061 `NEGATIVE_NET` (baris tetap dihitung untuk diagnosis).

**Detection**
3. Run `CALCULATED` dengan ringkasan `errors: 3, warnings: n`. Halaman review memfilter item error di bagian atas.
4. Metrik `payroll_item_errors_total{code}` naik.

**Handling**
5. Submit → `422 RUN_HAS_ERRORS {items: ["E020","E033","E061"]}`.
6. Perbaikan: aktifkan kembali / ganti referensi komponen E020; input salary E033; pecah adjustment E061 menjadi 3 × −Rp3.000.000 ke Maret–Mei. Alternatifnya, **exclude** E061 dari run dengan alasan (ter-audit) dan selesaikan di periode berikutnya.

**Recovery**
7. Recalculate → v2: 0 error → submit berhasil.

**Final State**
| Item | Nilai |
|---|---|
| Run | `PENDING_APPROVAL` v2 |
| Item | 100 `OK` (atau 99 + 1 excluded dengan alasan) |
| Total | Σ net item yang tidak di-exclude |

**Invariant yang dijaga:** PI-05 (item error tidak ikut total final), PI-07.
**Test:** golden `negative_net_error`, `no_salary_error`, `missing_component_ref_error`; `TestSubmit_RunWithErrorItems_Rejected`; `TestComponentConfig_Cycle_Rejected`.

---

## PFS-08 — Approval Rejection

**Request**
1. Hendra submit run v2 → `PENDING_APPROVAL`; `approval_requests` (`subject_type = PAYROLL_RUN`, `approver_id = Bu Ratna`, `PENDING`).

**Failure (bisnis)**
2. Bu Ratna menemukan lembur tim gudang belum masuk. Ia menekan **Reject** tanpa catatan → `422 DECISION_NOTE_REQUIRED`.
3. Reject dengan catatan "Lembur gudang 20–25 Maret belum masuk".

**Handling** (satu transaksi)
4. `UPDATE approval_requests SET status='REJECTED' … WHERE id=$1 AND status='PENDING' AND approver_id=$actor` → 1 baris.
5. Run `PENDING_APPROVAL → CALCULATED` + audit entry.

**Race**
6. Bu Ratna sempat membuka tab lain dan menekan **Approve** pada detik yang sama. `UPDATE … WHERE status='PENDING'` → 0 baris → `409 APPROVAL_ALREADY_DECIDED`. Tepat satu keputusan menang, dan tidak pernah terjadi `APPROVED` dan `REJECTED` sekaligus.
7. Hendra mencoba approve sendiri → `403` (bukan approver; tenant punya lebih dari satu admin, jadi pengecualian ADR-P09 tidak berlaku).

**Recovery**
8. Manager gudang meng-approve lembur (sebelum cut-off) → fingerprint berubah → Hendra recalculate (v3) → submit → **approval_request baru** (partial unique index mengizinkan, karena yang lama sudah `REJECTED`) → Bu Ratna approve → guard fingerprint lolos → run `APPROVED`.

**Final State**
| Item | Nilai |
|---|---|
| Run | `APPROVED`, `calc_version = 3` |
| Approval | 2 record: `REJECTED` (dengan catatan), `APPROVED` |
| Audit | submit, reject, recalculate, submit, approve |

**Invariant yang dijaga:** PI-11, PI-13.
**Test:** `TestApproval_ApproveVsRejectRace_SingleDecision`, `TestReject_RequiresNote`, `TestRunApprove_BySubmitter_Forbidden`.

---

## PFS-09 — Tenant Access Violation

**Setup:** run Maret PT Beta = `run_B`, milik Sinta.

**Request → Failure (percobaan)**
1. Dewi (PT Alpha) mendapatkan `run_B` (misalnya dari URL yang dibagikan sembarangan) dan memanggil `GET /v1/payroll-runs/{run_B}` dan `POST /v1/payroll-runs/{run_B}/calculate`.
2. Dewi memanipulasi JWT: mengganti `tid` menjadi PT Beta.
3. Bug hipotetis: kode internal meng-enqueue `payroll.calc_chunk` dengan `tenant_id = Alpha` tetapi `run_id = run_B`.

**Detection → Handling**
4. (1) `WithTenantTx(Alpha)` → RLS menyembunyikan `run_B` → service menerima "not found" → **`404 RUN_NOT_FOUND`** (tidak membocorkan bahwa run itu ada). Tidak ada state yang berubah.
5. (2) Signature JWT tidak valid → `401`. Tidak pernah sampai ke DB.
6. (3) Worker `WithTenantTxFor(Alpha)` → run tidak ditemukan → job diselesaikan sebagai no-op dengan log `ERROR` dan metrik `tenant_isolation_violation_total` +1 → alert. Kalaupun kode mencoba menulis `payroll_items (tenant_id=Alpha, run_id=run_B)`, composite FK `(tenant_id, run_id) → payroll_runs(tenant_id, id)` menolaknya, dan policy RLS `WITH CHECK` juga menolak.
7. Varian: membership Dewi dicabut saat run Alpha sedang `CALCULATING`. Job tetap berjalan (run milik tenant, bukan milik user). Dewi kehilangan akses ≤ 15 menit, dan aksi sensitif (finalize) memeriksa ulang membership → `403`.

**Recovery:** tidak diperlukan (tidak ada efek). Untuk (3), bug diperbaiki dan alert ditutup.

**Final State**
| Item | Nilai |
|---|---|
| Data PT Beta | Tidak terbaca dan tidak berubah |
| Audit | Percobaan akses tercatat dengan `user_id`, `tenant_id` aktif, resource ID |
| Metrik | `tenant_isolation_violation_total` = 1 untuk kasus (3); kasus (1) dan (2) tercatat sebagai 404/401 biasa |

**Invariant yang dijaga:** PI-01, PI-02.
**Test:** isolation suite PF-04: `TestCrossTenant_PayrollRunEndpoints_Return404`, `TestJob_TenantRunMismatch_NoOpAndAlert`, `TestRLS_CompositeFK_RejectsCrossTenantItem`.
**Demo:** **Demo A**.

---

## PFS-10 — Partial Payroll Processing (setelah finalize)

**Request**
1. Bu Ratna finalize run Maret (satu transaksi: run & periode `FINALIZED`, 100 `Payslip` `PENDING`, enqueue `payslip.render` per 20 payslip = 5 job).

**Failure**
2. Job render ke-4 (payslip 61–80) sedang berjalan. 8 PDF sudah di-upload ke object storage dan status `RENDERED` sudah di-commit untuk 6 di antaranya (commit per payslip). Lalu worker crash, sehingga PDF ke-7 dan ke-8 sudah ada di storage tetapi DB masih `PENDING`.

**Detection**
3. Lease job 4 kedaluwarsa. UI HR menampilkan "Slip gaji: 66/100 siap". Metrik `payslip_pending_total` > 0 lebih dari X menit → warning.

**Handling**
4. Job 4 diambil ulang. Untuk setiap payslip: status `RENDERED` → skip; `PENDING` → render ulang.
5. Object key deterministik: `tenants/{tenant_id}/payslips/{run_id}/{employee_id}.pdf`. Render ulang menimpa objek yang sama (PUT idempoten). Isi PDF deterministik (timestamp di dokumen = `run.finalized_at`, bukan waktu render), sehingga objek yang ditimpa identik.
6. Upload sukses lalu `UPDATE payslips SET status='RENDERED', object_key=…, sha256=… WHERE id=$1 AND status='PENDING'`.

**Recovery** — otomatis. Job 5 berjalan normal.

**Final State**
| Item | Nilai |
|---|---|
| Run | `FINALIZED` (tidak pernah tersentuh oleh kegagalan render) |
| Payslip | 100 `RENDERED`, masing-masing dengan `sha256` |
| Objek storage | Tepat 100, tanpa duplikat dan tanpa yatim |
| Akses karyawan | Presigned URL hanya setelah status `RENDERED` + cek kepemilikan |

**Kenapa finalize tidak bisa "setengah jadi":** finalize adalah satu transaksi DB. Bagian yang bersifat parsial hanyalah efek turunan (PDF, export bank), dan setiap efek turunan idempoten per item. Kegagalan rendering permanen untuk satu karyawan (setelah `max_attempts`) → payslip `FAILED` + alert, tanpa memengaruhi 99 lainnya; ops bisa me-retry dari console.

**Invariant yang dijaga:** PI-04, PI-17.
**Test:** `TestPayslipRender_CrashMidBatch_ResumesIdempotently`, `TestPayslipPDF_DeterministicBytes`.

---

## Ringkasan

| # | Skenario | Deteksi | Mekanisme kunci | Pemulihan | PI |
|---|---|---|---|---|---|
| PFS-01 | Worker crash di tengah chunk | Lease kedaluwarsa | Chunk = 1 tx; upsert unik; engine deterministik | Otomatis | 05, 06, 12 |
| PFS-02 | Eksekusi ganda | Idempotency key / guard state / fencing | Replay; `locked_by` fencing di `CompleteInTx` | Otomatis | 03, 06 |
| PFS-03 | Input berubah saat payroll | Fingerprint saat submit | `RUN_STALE` + daftar karyawan; `PERIOD_LOCKED` setelah final | Recalculate | 07, 13, 14 |
| PFS-04 | Database failure | Error koneksi, `readyz` | Rollback; state hanya di DB; idempotency pada commit tidak pasti | Otomatis / retry client | 04, 06, 12 |
| PFS-05 | Worker retry | `attempts`, `dead` | Transient → backoff; permanen → `FAILED` | Otomatis / deploy + recalculate | 05, 06 |
| PFS-06 | Eksekusi bersamaan | `23505`, guard state, versi | Partial unique index; `calc_version` | Otomatis | 03, 06, 11 |
| PFS-07 | Konfigurasi gaji tidak valid | Item `ERROR` | Validasi saat simpan + defensif di engine; submit diblokir | Perbaiki data / exclude | 05, 07 |
| PFS-08 | Approval ditolak | Keputusan approver | Guard `WHERE status='PENDING'`; catatan wajib | Recalculate + submit ulang | 11, 13 |
| PFS-09 | Pelanggaran akses tenant | 404 / 401 / metrik | RLS + composite FK + JWT signature | Tidak diperlukan | 01, 02 |
| PFS-10 | Pemrosesan parsial (payslip) | Status per payslip | Key objek deterministik; PDF deterministik; status per item | Otomatis | 04, 17 |

## Di luar cakupan V1

| Skenario | Alasan / arah |
|---|---|
| Failover DB dengan kehilangan transaksi (replica async) | V1 single DB; V2 memakai managed HA + PITR. Idempotency key dan fingerprint tetap membuat replay aman |
| Kerusakan object storage / bucket terhapus | Backup storage dan regenerasi dari snapshot (PDF deterministik) dibahas di desain DevOps |
| Noisy neighbor: run tenant besar menunda tenant lain | Fairness per tenant di job runner (PF-27, P2) |
| Membuka ulang periode final (kasus legal) | Workflow *reopen* dengan approval ganda (ADR-P07, future) |
| Clock skew antar worker | Tidak relevan: semua lease dan timestamp memakai `now()` milik database, bukan jam worker |

## Prinsip yang muncul berulang

1. **Unique constraint adalah idempotency yang paling bisa dipercaya.** `(run_id, employee_id, calc_version)`, partial unique run aktif, dan unique approval `PENDING` bekerja walaupun job dikirim dua kali.
2. **Setiap transisi state memakai guard `UPDATE … WHERE status = $expected`.** 0 baris berarti pihak lain sudah menang, sehingga transisi ditolak tanpa efek.
3. **Unit kerja = unit transaksi = unit retry.** Chunk dan payslip bisa diulang secara independen tanpa mengulang seluruh run.
4. **Komputasi deterministik membuat retry aman.** Menghitung ulang chunk atau me-render ulang PDF menghasilkan nilai identik.
5. **Fencing token (`locked_by`) mencegah worker "zombie" menulis setelah lease-nya diambil.**
6. **Deteksi, bukan penguncian, untuk input yang berubah.** Fingerprint membuat HR tetap bisa memperbaiki data, tetapi tidak bisa meng-approve angka basi.
7. **Fail closed.** Tanpa tenant context → 0 baris; resource tenant lain → 404; job dengan tenant yang tidak cocok → no-op + alert.
8. **Masalah data ≠ kegagalan sistem.** Data salah menjadi item `ERROR` yang terlihat dan tanpa retry; kegagalan sistem di-retry dengan backoff.
