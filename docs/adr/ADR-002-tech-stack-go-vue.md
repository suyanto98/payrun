# ADR-002 — Tech Stack: Go + Vue 3

Status: Accepted (dikonfirmasi user 2026-10-01) · Tanggal: 2026-10-01 · Berlaku untuk: LedgerPay, PayRun

## Context / Problem

Bahasa backend menentukan dua hal: seberapa cepat project selesai, dan seberapa dalam kandidat bisa membela setiap baris kode saat interview. Kandidat sudah ±2 tahun memakai Go (backend) dan Vue 3 (frontend) di pekerjaan sehari-hari. Lowongan target meliputi fintech, product company, dan SaaS B2B di Jakarta, yang banyak memakai Go, Java/Kotlin, dan Node.

## Options

| Opsi | Pros | Cons |
|---|---|---|
| **Go + Vue 3 (TS)** | Sudah produktif; concurrency primitive kuat untuk test race condition; binary tunggal mudah di-deploy; banyak dipakai fintech Indonesia | Ekosistem domain-modelling lebih sedikit dibanding JVM; generics masih terbatas |
| Java/Kotlin + Spring | Dominan di bank/enterprise; ekosistem transaksi matang | Kurva belajar; kecepatan delivery turun; "magic" annotation menyembunyikan boundary transaksi yang justru ingin ditunjukkan |
| Node.js (TypeScript) + React | Satu bahasa full-stack; ekosistem besar | Concurrency model single-thread kurang cocok untuk mendemokan locking; perlu belajar ulang praktik production |

## Decision

| Layer | Pilihan |
|---|---|
| Backend | **Go** (versi stabil terbaru), `net/http` + router `chi`, `pgx/v5`, `sqlc`, `goose`, `log/slog` |
| Frontend | **Vue 3 + TypeScript + Vite**, Pinia, `@tanstack/vue-query`, satu component library (PrimeVue atau setara), validasi form dengan zod |
| Testing | `testing` + testify, testcontainers-go, `pgregory.net/rapid` (property test), Playwright (E2E), k6 (load) |

Detail library final dikunci di Step 10 (project structure). ADR ini hanya mengunci bahasa dan framework.

## Reason

- **Gerbang veto** (Step 1 `00-overview.md` §4): semua kode harus bisa dipertahankan sendiri di interview. Bahasa yang sudah dikuasai memaksimalkan waktu untuk *masalah engineering*, bukan untuk sintaks.
- Goroutine + `sync.WaitGroup` membuat concurrency test (Demo B LedgerPay) singkat dan mudah dibaca.
- Go membuat boundary transaksi eksplisit (tidak ada `@Transactional`), dan itu yang ingin ditunjukkan.
- Vue: UI hanya alat demo. Framework yang sudah dikuasai menekan waktu UI (Step 1 PayRun R7).

## Trade-offs

- Lowongan Java-heavy (bank tradisional) mungkin menilai lebih rendah. Mitigasinya: konsep (isolation level, outbox, RLS) bersifat lintas bahasa dan dijelaskan di README.
- Sebagian pola DDD lebih verbose di Go (tidak ada sum type; error handling eksplisit).

## Future Migration

Tidak direncanakan. Kalau target lowongan berubah ke JVM, yang ditulis ulang cukup satu modul kecil (contoh: payroll engine di Kotlin) sebagai pembanding, bukan seluruh project.

## Interview angle

- *"Kenapa Go?"* → Produktivitas + concurrency yang eksplisit + binary tunggal. Saya ingin boundary transaksi terlihat di kode, bukan disembunyikan annotation.
- *"Bagaimana Go menangani race condition di kode kamu?"* → Race di level memori dicegah `-race` di CI. Race di level data dicegah DB lock/constraint, karena itulah yang benar-benar penting di multi-instance.

## Related

- [ADR-001](ADR-001-modular-monolith.md) · [ADR-005](ADR-005-data-access-sqlc.md) · [ADR-003](ADR-003-rest-api-openapi.md)
