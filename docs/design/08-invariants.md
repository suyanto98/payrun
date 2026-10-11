# PayRun: Payroll Invariants

> ID dan ringkasan invariant: `00-index.md`. File ini merinci **pernyataan presisi, konsekuensi bisnis, penjaga, deteksi, dan test** untuk setiap invariant.
> Label penjaga: `DB-CONSTRAINT` · `DB-TX` · `LOCK` · `RLS` · `APP` · `IDEMPOTENCY` · `JOB` · `TEST` · `OBS`.
> **Aturan:** setiap invariant P0 punya minimal satu penjaga di level database/transaksi (`DB-CONSTRAINT`, `DB-TX`, `LOCK`, atau `RLS`).

---

## 1. Invariant per invariant

### PI-01 — Setiap baris bisnis milik tepat satu tenant; tidak ada read/write lintas tenant
- **Presisi:** setiap tabel bisnis punya `tenant_id NOT NULL`. Untuk setiap query yang dijalankan `payrun_app`, baris yang terbaca/tertulis memenuhi `tenant_id = app.tenant_id`.
- **Konsekuensi jika dilanggar:** kebocoran data gaji antar perusahaan; berakhirnya bisnis SaaS.
- **Penjaga:**
  - `RLS`: policy `USING` + `WITH CHECK`, `FORCE ROW LEVEL SECURITY`; `current_setting(…, true)` → fail closed.
  - `DB-CONSTRAINT`: `tenant_id NOT NULL`.
  - `APP`: tenant hanya dari JWT `tid`; `WithTenantTx` satu-satunya akses DB; query menulis `WHERE tenant_id` (lint).
  - `TEST`: suite isolasi (§3 di [`05`](06-multi-tenancy.md)), test katalog RLS.
- **Deteksi:** `tenant_isolation_violation_total` (dinaikkan oleh defensive check yang membandingkan `tenant_id` hasil load dengan `Principal.TenantID`); startup check role app bukan owner/BYPASSRLS.

### PI-02 — Referensi lintas tenant mustahil
- **Presisi:** setiap FK antar tabel bisnis berbentuk `(tenant_id, x_id) → parent(tenant_id, id)`, dengan `UNIQUE (tenant_id, id)` di parent.
- **Konsekuensi:** karyawan A menunjuk departemen/manager/komponen B, sehingga nama dan konfigurasi B bocor lewat join, dan laporan menjadi salah.
- **Penjaga:** `DB-CONSTRAINT` (composite FK); `RLS` (`WITH CHECK`); `APP` (load referensi lewat tenant aktif).
- **Deteksi:** error `23503` pada FK ber-`tenant_id` dicatat sebagai `tenant_isolation_violation_total` (seharusnya mustahil karena APP sudah memvalidasi).
- **Test:** INSERT langsung dengan referensi lintas tenant → ditolak; test katalog: setiap FK yang menunjuk tabel ber-`tenant_id` menyertakan `tenant_id`.

### PI-03 — Maksimal satu run aktif per (tenant, period)
- **Presisi:** untuk setiap `(tenant_id, period_id)`, jumlah run dengan `status <> 'CANCELLED'` ≤ 1.
- **Konsekuensi:** dua run untuk Maret → gaji dibayar dua kali, atau dua angka "final" yang berbeda.
- **Penjaga:**
  - `DB-CONSTRAINT`: `CREATE UNIQUE INDEX uq_active_run_per_period ON payroll_runs (tenant_id, period_id) WHERE status <> 'CANCELLED';`
  - `IDEMPOTENCY`: `Idempotency-Key` pada `POST /payroll-runs`; `23505` → kembalikan run yang ada.
- **Deteksi:** tidak perlu metrik terpisah karena DB menjamin; `409/200-replay` dicatat di log.
- **Test:** dua `POST` paralel (goroutine) → satu run; run `FINALIZED` + `POST` baru → ditolak (bukan `CANCELLED`).

### PI-04 — Run `FINALIZED` immutable
- **Presisi:** setelah `payroll_runs.status = 'FINALIZED'`, tidak ada UPDATE/DELETE pada run (kecuali kolom non-bisnis yang di-whitelist, misalnya `payslips_rendered_count`), item, line, maupun snapshot-nya.
- **Konsekuensi:** angka yang sudah dibayar/dilaporkan berubah diam-diam; audit dan reproducibility hilang.
- **Penjaga:**
  - `DB-CONSTRAINT` (trigger):
    ```sql
    CREATE FUNCTION forbid_change_on_finalized() RETURNS trigger AS $$
    BEGIN
      IF EXISTS (SELECT 1 FROM payroll_runs r
                 WHERE r.id = OLD.run_id AND r.status = 'FINALIZED') THEN
        RAISE EXCEPTION 'payroll run % is finalized', OLD.run_id USING ERRCODE = 'P0001';
      END IF;
      RETURN COALESCE(NEW, OLD);
    END $$ LANGUAGE plpgsql;
    CREATE TRIGGER payroll_items_immutable BEFORE UPDATE OR DELETE ON payroll_items
      FOR EACH ROW EXECUTE FUNCTION forbid_change_on_finalized();
    -- sama untuk payroll_lines; trigger di payroll_runs menolak perubahan kolom bisnis jika OLD.status='FINALIZED'
    ```
  - `LOCK`: transisi status hanya lewat guard `WHERE status = $expected`; `FINALIZED` tidak punya transisi keluar.
  - `APP`: tidak ada endpoint yang mengubah item; koreksi lewat adjustment (PI-15).
- **Deteksi:** exception `P0001` dari trigger → log error + metrik `payroll_finalized_mutation_attempt_total` (alert: selalu indikasi bug).
- **Test:** UPDATE langsung pada item run final → gagal; walk-through §3.

### PI-05 — Total run = Σ net item; net item = Σ earning − Σ deduction
- **Presisi:** untuk `calc_version` terkini: `run.total_net = Σ item.net`, `run.total_gross = Σ item.gross`; untuk setiap item: `item.net = Σ line(EARNING).amount − Σ line(DEDUCTION).amount`, dengan setiap `line.amount` sudah dibulatkan (ADR-P06).
- **Konsekuensi:** file transfer bank berbeda dari slip gaji; selisih Rp1 yang membuat finance tidak percaya sistem.
- **Penjaga:**
  - `DB-TX`: total **dihitung** (`SELECT SUM(net) …`) dalam transaksi chunk terakhir, tidak diakumulasi.
  - `APP`: engine menghitung `net` dari line yang sudah dibulatkan (pembulatan sekali per line, bukan di total).
  - `DB-CONSTRAINT`: constraint trigger `DEFERRABLE INITIALLY DEFERRED` pada `payroll_items` yang memeriksa `net = Σ lines` saat commit (murah, per item yang berubah).
  - `TEST`: property test engine `net == Σ earnings − Σ deductions`; integration test total run.
- **Deteksi:** `JOB` verifikasi harian (`payrun verify-run`) untuk run final; metrik `payroll_total_mismatch_total` (target 0).

### PI-06 — Maksimal satu item per (run, employee, calc_version); hanya versi terkini yang berlaku
- **Presisi:** `UNIQUE (run_id, employee_id, calc_version)`; semua pembacaan (total, review, finalize, payslip) memakai `calc_version = run.calc_version`.
- **Konsekuensi:** karyawan muncul dua kali (dibayar dua kali) atau item dari kalkulasi lama ikut terhitung.
- **Penjaga:**
  - `DB-CONSTRAINT`: unique key; upsert `ON CONFLICT … DO UPDATE` saat chunk di-retry.
  - `LOCK`: chunk menulis hanya jika `run.status = 'CALCULATING' AND run.calc_version = $v` (dicek di tx yang sama), sehingga chunk versi lama menjadi no-op.
  - `IDEMPOTENCY`: chunk job unik `(run_id, calc_version, chunk_no)`.
  - `DB-TX`: penanda job selesai + `chunks_done++` + upsert dalam satu transaksi (ADR-006 `CompleteInTx`).
- **Deteksi:** saat transisi ke `CALCULATED`: `count(items@v) = count(eligible)`, kalau tidak → `FAILED` + alert.
- **Test:** kill worker setelah N item (Demo B); chunk versi lama yang dieksekusi setelah recalculate tidak menulis apa pun.

### PI-07 — Employee masuk run ⇔ aktif ≥ 1 hari di period; prorata sesuai hari aktif
- **Presisi:** himpunan item = karyawan dengan `employment_span ∩ period.range ≠ ∅`. Faktor prorata per span = `hari(span ∩ period) / basis` (basis dari ruleset: hari kalender atau hari kerja).
- **Konsekuensi:** karyawan resign tetap digaji penuh; joiner baru tidak digaji; prorata salah.
- **Penjaga:** `APP` (query eligible + engine prorata dengan `DateRange.Intersect`); `DB-CONSTRAINT` (employment span: `CHECK (NOT isempty(span))`, EXCLUDE overlap per employee); `TEST` (golden: joiner tanggal 15, leaver tanggal 10, keduanya dalam satu bulan, kenaikan gaji tengah bulan).
- **Deteksi:** warning engine untuk karyawan aktif dengan net = 0; diff vs bulan lalu (PF-23).

### PI-08 — Hanya cuti/lembur `APPROVED` sebelum cut-off yang dihitung
- **Presisi:** input lembur/cuti tidak dibayar = request dengan `status = 'APPROVED'`, tanggal di period, dan `approval.decided_at < period.cut_off`. Yang disetujui setelah cut-off masuk period berikutnya sebagai adjustment/input bulan depan (keputusan detail di [`06`](07-payroll-engine.md)).
- **Konsekuensi:** lembur yang belum disetujui ikut dibayar; atau dibayar dua kali (bulan ini dan bulan depan).
- **Penjaga:** `APP` (filter di `InputProvider.Overtime/UnpaidLeave`); `DB-TX` (fingerprint mencakup daftar request yang dipakai, sehingga approval baru sebelum approve run membuat run stale, PI-13); `TEST` (lembur disetujui tepat setelah cut-off).
- **Deteksi:** fingerprint mismatch saat approve (`RUN_STALE`).
- **Catatan:** tidak ada penjaga DB murni karena ini aturan *seleksi input*. Penjaga level transaksinya adalah fingerprint guard (PI-13), sehingga aturan tetap P0-compliant.

### PI-09 — Record effective-dated tidak tumpang tindih; tepat satu yang berlaku per tanggal
- **Presisi:** untuk setiap `(tenant_id, employee_id)` di `salary_assignments`, `org_assignments`, `employee_components` (per komponen), `employment_spans`: tidak ada dua `valid_during` yang beririsan. Selama karyawan aktif, setiap tanggal tercakup tepat satu record salary.
- **Konsekuensi:** dua gaji pokok berlaku bersamaan → dihitung dobel; celah → gaji 0.
- **Penjaga:**
  - `DB-CONSTRAINT`: `EXCLUDE USING gist (tenant_id WITH =, employee_id WITH =, valid_during WITH &&)` (btree_gist).
  - `DB-TX`: "tutup lama + buka baru" dalam satu transaksi.
  - `APP`: validasi *coverage* (tidak ada celah dalam employment span) saat menyimpan.
- **Deteksi:** warning engine `SALARY_GAP` (ada hari aktif tanpa salary).
- **Test:** dua perubahan gaji paralel dengan tanggal beririsan → satu gagal (`23P01`).

### PI-10 — Saldo cuti ≥ 0 (kecuali jenis cuti yang mengizinkan minus)
- **Presisi:** `leave_balances.balance >= -leave_types.max_negative` (default `max_negative = 0`).
- **Konsekuensi:** karyawan mengambil cuti berbayar melebihi hak.
- **Penjaga:** `DB-CONSTRAINT` (`CHECK` lewat kolom denormalisasi `min_allowed` di `leave_balances`, karena CHECK tidak bisa mereferensikan tabel lain); `LOCK` (`SELECT … FOR UPDATE` pada baris saldo saat approve); `DB-TX` (pengurangan saldo + approval dalam satu tx).
- **Deteksi:** pelanggaran CHECK → approval rollback dengan error `INSUFFICIENT_LEAVE_BALANCE`.
- **Test:** dua approval paralel yang bersama-sama melebihi saldo → hanya satu sukses.

### PI-11 — Approval diputuskan maksimal sekali; tanpa self-approval; approver = snapshot
- **Presisi:** transisi keluar dari `PENDING` terjadi tepat sekali; `approver_id <> requester_id` kecuali `self_approved = true` (hanya `subject_type = 'PAYROLL_RUN'` di tenant dengan satu admin, alasan wajib); hanya `approver_id` yang di-snapshot (atau override HR ter-audit) yang boleh memutuskan.
- **Konsekuensi:** cuti disetujui dan ditolak sekaligus; manager menyetujui lemburnya sendiri.
- **Penjaga:**
  - `LOCK`: `UPDATE approval_requests SET status=$s … WHERE id=$1 AND status='PENDING' AND approver_id=$actor`.
  - `DB-CONSTRAINT`: partial unique `(tenant_id, subject_type, subject_id) WHERE status='PENDING'`; `CHECK (approver_id <> requester_id OR (self_approved AND subject_type = 'PAYROLL_RUN' AND self_approval_reason IS NOT NULL))`.
  - `APP`: resolusi approver saat submit; syarat "satu admin" dicek di service.
- **Deteksi:** audit log setiap keputusan; metrik `approval_self_approved_total` (dipantau).
- **Test:** approve vs reject paralel → satu menang, yang lain 409; self-approval cuti → ditolak DB.

### PI-12 — Determinisme
- **Presisi:** `engine.Calculate(item.input_snapshot, ruleset(item.ruleset_version))` menghasilkan line, gross, deduction, net yang identik dengan yang tersimpan, kapan pun dijalankan.
- **Konsekuensi:** tidak bisa membuktikan angka lama benar; sengketa tidak bisa diselesaikan.
- **Penjaga:** `APP` (engine pure: tanpa `time.Now`, DB, random, iterasi map tanpa urutan; package diperiksa depguard); `DB-CONSTRAINT` (ruleset immutable per versi: trigger tolak UPDATE); `DB-TX` (snapshot ditulis bersama item); `TEST` (recompute-from-snapshot untuk setiap fixture + golden files).
- **Deteksi:** `JOB` `payrun verify-run` (sampling run final) → `payroll_reproducibility_failure_total` (target 0, alert).

### PI-13 — Run hanya bisa di-approve jika fingerprint input saat ini = fingerprint kalkulasi
- **Presisi:** pada transisi `PENDING_APPROVAL → APPROVED` dan `APPROVED → FINALIZED`, `run_fingerprint(data saat ini) = run.input_fingerprint@calc_version`.
- **Konsekuensi:** run yang disetujui tidak mencerminkan data terbaru (absensi dikoreksi, adjustment baru) → gaji salah tetapi tampak "approved".
- **Penjaga (mekanisme final, desain transaksi & concurrency O12 + final review):** `LOCK` approve/finalize memegang `payroll_periods FOR UPDATE`, sementara penulis input memegang `FOR SHARE` dan menulis `payroll_input_changes` (+ `tenant_settings` lock untuk celah D1); `DB-TX` kandidat = perubahan sejak `calc_started_at`, lalu fingerprint **hanya kandidat** dihitung ulang di transaksi yang sama dengan transisi (lebih dari 200 kandidat → langsung `RUN_STALE`); `APP` `InputProvider` yang sama untuk kalkulasi dan fingerprint (§8 di [`04`](05-domain-model.md)).
- **Deteksi:** `409 RUN_STALE {employees_changed}`; metrik `payroll_run_stale_total`.
- **Test:** koreksi absensi setelah `CALCULATED` → approve ditolak; recalculate → approve sukses.
- **Keterbatasan:** ~~window race di Read Committed~~ **ditutup** di desain transaksi & concurrency (O3/O12): lock periode membuat penulis input menunggu selama approve, dan argumen berbasis lock menjamin semua perubahan yang tidak terlihat oleh kalkulasi masuk kandidat.

### PI-14 — Input untuk period `FINALIZED` tidak bisa diubah
- **Presisi:** tidak ada INSERT/UPDATE/DELETE pada attendance, leave/overtime request, salary/component assignment, atau employment span yang menyentuh tanggal di period berstatus `FINALIZED` milik tenant tersebut.
- **Konsekuensi:** snapshot dan data operasional berbeda; slip tidak bisa dijelaskan dari data sumber.
- **Nuansa effective dating (dari desain database):** yang diperiksa adalah rentang tanggal yang **nilainya berubah**, bukan seluruh rentang lama dan baru. Contoh: menutup `salary_assignments` lama `[Jan, ∞)` menjadi `[Jan, Jun)` untuk kenaikan gaji Juni hanya mengubah rentang `[Jun, ∞)`, jadi tetap diizinkan walaupun Januari–Mei sudah final. Pemeriksaan naif atas seluruh rentang akan memblokir setiap kenaikan gaji (detail di `12-db-schema.md`).
- **Penjaga:**
  - `APP`: `PeriodGuard.EnsureWritable` di semua modul input → `409 PERIOD_LOCKED`.
  - `DB-CONSTRAINT`: trigger di tabel input:
    ```sql
    -- contoh attendance_records
    IF EXISTS (SELECT 1 FROM payroll_periods p
               WHERE p.tenant_id = NEW.tenant_id AND p.status = 'FINALIZED'
                 AND p.range @> NEW.work_date) THEN
      RAISE EXCEPTION 'period locked' USING ERRCODE = 'P0002';
    END IF;
    ```
    Untuk rentang (salary `valid_during`): menolak jika rentang lama **atau** baru beririsan dengan period final.
  - `DB-TX`: finalize mengubah status period di transaksi yang sama dengan run.
- **Deteksi:** `P0002` yang lolos dari PeriodGuard → `period_lock_bypass_total` (indikasi jalur yang lupa memanggil guard).
- **Test:** koreksi absensi, perubahan gaji berlaku surut, dan import CSV ke period final → semua ditolak.

### PI-15 — Adjustment diterapkan tepat ke satu run final, maksimal sekali
- **Presisi:** `adjustments.applied_run_id` diisi sekali (dari `NULL`) saat run target `FINALIZED`; adjustment `APPLIED` immutable; satu adjustment tidak muncul di dua item final.
- **Konsekuensi:** koreksi dibayar dua kali atau tidak pernah dibayar.
- **Penjaga:** `LOCK` (`UPDATE adjustments SET status='APPLIED', applied_run_id=$run WHERE id = ANY($ids) AND status='PENDING_APPLY' AND applied_run_id IS NULL` dalam tx finalize; jumlah baris harus = jumlah adjustment di item, kalau tidak → rollback); `DB-CONSTRAINT` (`CHECK ((status='APPLIED') = (applied_run_id IS NOT NULL))`; trigger immutability untuk `APPLIED`); `DB-TX` (bagian dari tx finalize); `APP` (fingerprint mencakup adjustment, PI-13).
- **Deteksi:** rollback finalize dengan error `ADJUSTMENT_CONFLICT`.
- **Test:** adjustment dibatalkan setelah run target dikalkulasi → run stale; finalize dua run berbeda yang sama-sama memuat adjustment yang sama → mustahil karena target period tunggal + guard.

### PI-16 — Tenant selalu punya ≥ 1 `OWNER` aktif
- **Presisi:** untuk setiap tenant `ACTIVE`, `count(memberships WHERE status='ACTIVE' AND 'OWNER' = ANY(roles)) ≥ 1`.
- **Konsekuensi:** tenant tanpa admin; tidak ada yang bisa mengelola membership atau finalize.
- **Penjaga:** `DB-CONSTRAINT` (constraint trigger `DEFERRABLE INITIALLY DEFERRED` pada `memberships` AFTER UPDATE/DELETE: hitung owner tersisa untuk `OLD.tenant_id`; 0 → exception), sehingga transfer kepemilikan dalam satu tx (tambah owner baru, cabut lama) tetap lolos; `LOCK` (`pg_advisory_xact_lock(hashtextextended('owners:'||tenant_id, 0))` sebelum mengubah role owner (desain transaksi & concurrency; bukan `tenants FOR UPDATE`, yang akan memblok cek FK `KEY SHARE` dari semua INSERT di tenant itu), untuk menserialkan dua pencabutan owner paralel yang masing-masing melihat "masih ada satu lagi"); `APP` (pesan error ramah).
- **Test:** dua owner saling mencabut secara paralel → satu gagal.

### PI-17 — Slip gaji hanya untuk pemilik + role berwenang; akses non-pemilik ter-audit
- **Presisi:** `GET payslip` sukses ⇔ (`employee.user_id = actor`) ∨ (actor punya `PAYROLL_ADMIN`/`HR_ADMIN`/`OWNER` di tenant). Setiap akses oleh non-pemilik menghasilkan `audit_log` di transaksi yang sama dengan penerbitan presigned URL/response.
- **Konsekuensi:** gaji rekan kerja bocor (konflik internal, pelanggaran UU PDP).
- **Penjaga:** `RLS` (batas tenant); `APP` (policy + query scoping `WHERE employee.user_id = $actor` untuk role EMPLOYEE); `DB-TX` (audit + pembuatan URL atomik: tanpa audit, tanpa URL); object storage private + presigned TTL 5 menit.
- **Deteksi:** laporan audit akses slip per tenant; alert pola tak wajar (V2).
- **Test:** matrix role × slip (pemilik, karyawan lain, manager, HR); audit tercatat untuk HR.

### PI-18 — Nomor karyawan dan NIK unik per tenant
- **Presisi:** `UNIQUE (tenant_id, employee_no)`, `UNIQUE (tenant_id, nik_bidx)` (blind index HMAC atas NIK ternormalisasi, ADR-P08).
- **Konsekuensi:** import CSV menulis ke karyawan yang salah; karyawan ganda dibayar dua kali.
- **Penjaga:** `DB-CONSTRAINT`; `APP` (normalisasi NIK sebelum HMAC).
- **Test:** NIK sama dengan format berbeda (`32.01…` vs `3201…`) → ditolak; NIK sama di tenant berbeda → boleh.

### PI-19 — Garis pelaporan asiklik
- **Presisi:** untuk setiap tanggal, graf `employee → manager` dari `org_assignments` yang berlaku tidak memiliki siklus.
- **Konsekuensi:** resolusi approver tak berujung; A menyetujui B yang menyetujui A (bypass self-approval tidak langsung).
- **Penjaga:** `DB-TX` + `LOCK` (saat set manager: kunci baris org assignment karyawan terkait, lalu cek dengan recursive CTE di tx yang sama):
  ```sql
  WITH RECURSIVE chain AS (
    SELECT manager_id FROM org_assignments
     WHERE tenant_id = $t AND employee_id = $new_manager AND valid_during @> $date
    UNION
    SELECT o.manager_id FROM org_assignments o JOIN chain c ON o.employee_id = c.manager_id
     WHERE o.tenant_id = $t AND o.valid_during @> $date
  )
  SELECT EXISTS (SELECT 1 FROM chain WHERE manager_id = $employee);  -- true → siklus
  ```
  `APP` (validasi dan error ramah); `DB-CONSTRAINT` (`CHECK (manager_id <> employee_id)` untuk siklus langsung).
- **Keterbatasan:** cek dilakukan per tanggal efektif perubahan. Perubahan effective-dated di masa depan dicek terhadap tanggal mulainya; kombinasi perubahan bertahap yang membentuk siklus di tanggal lain dideteksi oleh `JOB` validasi harian.
- **Test:** A→B, B→C, set C→A → ditolak.

---

## 2. Matriks ringkas

| Invariant | DB-CONSTRAINT | DB-TX | LOCK | RLS | APP | IDEMPOTENCY | JOB | TEST | OBS |
|---|---|---|---|---|---|---|---|---|---|
| PI-01 Tenant ownership | ✅ NOT NULL | | | ✅ | ✅ | | | ✅ | ✅ |
| PI-02 No cross-tenant ref | ✅ composite FK | | | ✅ | ✅ | | | ✅ | ✅ |
| PI-03 1 run aktif | ✅ partial unique | | | | | ✅ | | ✅ | |
| PI-04 Final immutable | ✅ trigger | | ✅ | | ✅ | | | ✅ | ✅ |
| PI-05 Σ total | ✅ deferred trigger | ✅ | | | ✅ | | ✅ | ✅ | ✅ |
| PI-06 1 item/versi | ✅ unique | ✅ | ✅ | | | ✅ | | ✅ | ✅ |
| PI-07 Eligibility/prorata | ✅ span EXCLUDE | | | | ✅ | | | ✅ | ✅ |
| PI-08 Approved + cut-off | | ✅ (via PI-13) | | | ✅ | | | ✅ | ✅ |
| PI-09 No overlap | ✅ EXCLUDE gist | ✅ | | | ✅ | | | ✅ | ✅ |
| PI-10 Saldo cuti | ✅ CHECK | ✅ | ✅ | | | | | ✅ | |
| PI-11 Approval sekali | ✅ partial unique + CHECK | | ✅ | | ✅ | | | ✅ | ✅ |
| PI-12 Determinisme | ✅ ruleset immutable | ✅ | | | ✅ | | ✅ | ✅ | ✅ |
| PI-13 Fingerprint guard | | ✅ | ✅ | | ✅ | | | ✅ | ✅ |
| PI-14 Period lock | ✅ trigger | ✅ | | | ✅ | | | ✅ | ✅ |
| PI-15 Adjustment sekali | ✅ CHECK + trigger | ✅ | ✅ | | ✅ | | | ✅ | |
| PI-16 ≥ 1 owner | ✅ deferred trigger | | ✅ | | ✅ | | | ✅ | |
| PI-17 Akses slip | | ✅ (audit atomik) | | ✅ | ✅ | | | ✅ | ✅ |
| PI-18 Unik per tenant | ✅ unique | | | | ✅ | | | ✅ | |
| PI-19 Asiklik | ✅ CHECK (langsung) | ✅ | ✅ | | ✅ | | ✅ | ✅ | |

---

## 3. Defense in depth: PI-04 menghadapi endpoint yang salah

**Skenario:** developer menambah endpoint `PATCH /v1/payroll-runs/{id}/items/{item}` untuk "memperbaiki catatan" item, dan lupa mengecek status run. Hendra memanggilnya pada run Maret yang sudah `FINALIZED` dan mengubah `net`.

```text
1. Router / authz          → Hendra PAYROLL_ADMIN di tenant yang benar → lolos (memang berhak di tenant ini)
2. Service (bug)           → tidak mengecek run.status → lolos
3. WithTenantTx + RLS      → baris milik tenant aktif → lolos (RLS menjaga tenant, bukan status)
4. Trigger payroll_items_immutable
                           → run.status = 'FINALIZED' → RAISE EXCEPTION P0001 → tx ROLLBACK
5. Error mapping (db)      → P0001 → 409 RUN_FINALIZED (bukan 500)
6. Observability           → log error + payroll_finalized_mutation_attempt_total++ → alert
7. CI (seharusnya sebelum merge)
                           → test "mutasi item run final ditolak" dijalankan untuk semua endpoint item → PR gagal
```

**Hasil:** angka final tidak berubah. Bug tetap terdeteksi lewat alert, dan endpoint bisa diperbaiki. Tanpa lapisan 4, bug yang sama akan mengubah gaji yang sudah dibayar tanpa jejak, kecuali audit log ditulis (yang juga bergantung pada kode yang sama).

---

## 4. Invariant yang belum ditegakkan secara ketat di V1

| Invariant / aspek | Celah di V1 | Kenapa diterima | Jalur penguatan |
|---|---|---|---|
| PI-13 window race (Read Committed) | Input bisa berubah di antara hitung fingerprint dan commit approve | Window milidetik; finalize memeriksa ulang | Tx approve/finalize memakai `REPEATABLE READ` atau lock baris period (`FOR UPDATE`) yang juga diambil oleh PeriodGuard saat menulis input |
| PI-19 siklus lintas tanggal efektif | Cek hanya pada tanggal mulai perubahan | Perubahan org jarang dan sederhana di UMKM | Validasi semua titik perubahan di masa depan; job harian sudah mendeteksi |
| PI-09 coverage (tanpa celah) | Overlap dijaga DB, celah hanya oleh APP + warning engine | Celah tidak membuat gaji dobel, dan terlihat sebagai warning saat review | Constraint trigger coverage per employment span |
| PI-05 untuk run non-final | Deferred trigger memeriksa per item, total run dihitung saat transisi | Run non-final memang boleh berubah | — |
| PI-17 deteksi penyalahgunaan insider | Tercatat, tetapi tidak di-alert | Butuh baseline perilaku | V2: alert akses massal slip |
| PI-08 cut-off untuk approval terlambat | Aturan "masuk bulan depan" bergantung APP | Tidak ada representasi DB yang sederhana | Kolom `payable_period_id` yang di-set saat approval (deterministik) |
