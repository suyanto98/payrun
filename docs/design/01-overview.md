# Step 1.0 — Portfolio Strategy Overview

> Bagian dari: Step 1 — Product & System Strategy
> Baca dulu file ini, lalu [`01-ledgerpay.md`](01-ledgerpay.md), [`02-payrun.md`](02-payrun.md), dan [`03-cross-project.md`](03-cross-project.md).

---

## 1. Apa yang sebenarnya ingin dibuktikan

Interviewer tidak menilai jumlah halaman atau fitur. Yang mereka cari adalah bukti bahwa kandidat bisa menjawab pertanyaan seperti ini:

1. *Apa yang terjadi kalau request ini dikirim dua kali?*
2. *Apa yang terjadi kalau dua request ini datang di milidetik yang sama?*
3. *Apa yang terjadi kalau server mati di tengah proses ini?*
4. *Bagaimana kamu tahu sistem ini sedang bermasalah?*
5. *Bagaimana kamu membuktikan data user A tidak pernah bisa dibaca user B?*
6. *Kenapa kamu memilih X, bukan Y, dan kapan pilihan itu harus diganti?*

Kedua project dipilih karena masing-masing **memaksa** pertanyaan-pertanyaan di atas muncul dari domain bisnisnya sendiri. Jadi kompleksitasnya tidak ditambahkan secara artifisial.

| Pertanyaan | Dibuktikan paling kuat di |
|---|---|
| Duplicate request / idempotency | LedgerPay (uang), PayRun (payroll run) |
| Concurrency / race condition | LedgerPay (saldo yang sama, hot account) |
| Crash di tengah proses / recovery | LedgerPay (payout unknown outcome), PayRun (payroll batch resume) |
| Integrasi sistem eksternal yang tidak andal | LedgerPay (payment provider, webhook) |
| Isolasi data / authorization | PayRun (multi-tenant, RLS), LedgerPay (BOLA pada wallet) |
| Aturan bisnis kompleks yang harus reproducible | PayRun (payroll engine, effective dating) |
| Workflow + state machine | Keduanya (status transaksi; status payroll run + approval) |
| Batch / background processing | PayRun (kalkulasi ribuan karyawan), LedgerPay (outbox, reconciliation) |
| Auditability & immutability | LedgerPay (ledger append-only), PayRun (finalized payroll + adjustment) |

---

## 2. Profil kedua project (bukan ranking)

| Dimensi | LedgerPay | PayRun |
|---|---|---|
| Karakter kompleksitas | **Dalam** (depth). Fitur sedikit, tetapi setiap fitur punya banyak edge case dan konsekuensinya fatal | **Lebar** (breadth). Banyak entitas, aturan, dan workflow yang saling bergantung |
| Inti engineering | Correctness di bawah concurrency dan partial failure | Isolasi tenant, rule engine yang reproducible, batch processing |
| Kalau gagal, kerugiannya | Uang hilang atau tercipta dari udara | Gaji salah, atau data gaji perusahaan A bocor ke perusahaan B |
| Model konsistensi | Strong, tanpa kompromi, untuk ledger dan saldo | Strong untuk payroll final; eventual untuk laporan dan dokumen |
| Interaksi dengan pihak luar | Tinggi (payment provider, bank, merchant webhook) | Rendah (export file bank; bisa terhubung ke LedgerPay sebagai stretch goal) |
| Multi-tenancy | Ringan (merchant ≈ "tenant" untuk API key) | **Inti dari produk** |
| Demo yang paling terasa | Angka saldo yang tetap benar saat diserang 100 request paralel | User perusahaan A tidak bisa melihat data B, walaupun ada bug di query |
| Topik interview yang dibuka | Isolation level, locking, idempotency, outbox, exactly-once vs at-least-once, reconciliation | RLS, tenant context, effective dating, decimal/rounding, job idempotency, approval workflow, PII |

**Gabungan keduanya:** LedgerPay membuktikan *"saya bisa membuat sistem yang benar ketika semuanya salah"*. PayRun membuktikan *"saya bisa memodelkan domain bisnis yang luas dan membangun SaaS yang aman"*. Ini dua sinyal berbeda yang dicari untuk level mid-to-senior.

---

## 3. Asumsi kerja

Asumsi berikut dipakai di seluruh dokumen. Kalau ada yang salah, koreksi dulu sebelum Step 2.

| # | Asumsi | Dampak kalau salah |
|---|---|---|
| A1 | Waktu tersedia ≥ 14 jam/minggu (dikonfirmasi 2026-10-01) | Estimasi roadmap di `03-cross-project.md` berubah |
| A2 | Backend Go: bahasa yang dipakai sehari-hari, jadi produktif dan bisa dipertahankan di interview | Kalau target lowongan Java/Node, stack perlu ditinjau ulang di ADR Step 2 |
| A3 | Frontend Vue 3 (sudah familiar), dengan UI **minimal tetapi rapi** | UI hanya alat demo. Nilai utamanya ada di backend dan sistemnya |
| A4 | Domain Indonesia (IDR, aturan e-money, PPh21/BPJS versi **disederhanakan**) | Angka regulasi wajib diverifikasi dan ditandai "simplified" di README |
| A5 | Dikerjakan solo, tetapi dengan proses tim: ADR, PR, CI, conventional commit | — |
| A6 | Tidak ada kode, skema, atau penamaan yang disalin dari pekerjaan kantor (ERP) | Risiko IP dan etika. Lihat `03-cross-project.md` §4 |

---

## 4. Prinsip penyaringan (dipakai di setiap keputusan)

Setiap fitur atau teknologi harus lolos **minimal 2 dari 3** gerbang berikut:

1. **Business gate:** apakah menyelesaikan masalah nyata seorang persona?
2. **Engineering gate:** apakah memaksa munculnya masalah production yang nyata (konsistensi, failure, keamanan, skala)?
3. **Demo gate:** apakah bisa ditunjukkan secara langsung dalam waktu ≤ 2 menit, dengan bukti yang terlihat (angka, log, metrik, test)?

Ditambah satu **gerbang veto**: *apakah bisa diselesaikan dan dipertahankan sendiri di interview?* Kalau tidak bisa, fitur itu dibuang walaupun lolos tiga gerbang di atas.

Contoh penerapan:

| Kandidat | Business | Engineering | Demo | Veto | Hasil |
|---|---|---|---|---|---|
| Idempotency key | ✅ | ✅ | ✅ | ✅ | **Masuk (P0)** |
| Microservices per domain | ❌ | ⚠️ (kompleksitas jaringan, bukan masalah domain) | ❌ | ❌ | **Dibuang** |
| Kafka | ❌ | ⚠️ | ❌ | ❌ | **Dibuang**; Postgres-backed queue sudah cukup |
| Dark mode UI | ❌ | ❌ | ⚠️ | ✅ | **Dibuang** |
| Reconciliation | ✅ | ✅ | ✅ | ✅ | **Masuk** |
| Payroll formula DSL (user bisa menulis rumus) | ⚠️ | ✅ | ⚠️ | ❌ (parser + sandbox terlalu besar) | **Dibuang**; diganti tipe komponen yang ter-parameter |
