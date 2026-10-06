# ADR-P02 — Tenant Context Propagation

> Status: **Accepted** · Tanggal: 2026-10-01 · Berlaku untuk: **PayRun**

## Context / Problem

RLS (ADR-P01) hanya seaman nilai `app.tenant_id` yang diset. Pertanyaannya:
1. Dari mana server tahu tenant aktif sebuah request, dan bagaimana mencegah user memalsukannya?
2. Bagaimana nilai itu sampai ke DB tanpa bocor ke request lain yang memakai koneksi pool yang sama?
3. Bagaimana konteks tenant ikut ke tempat yang **tidak** dilindungi RLS: background job, cache, log, object storage?

Satu user bisa menjadi anggota lebih dari satu tenant (contoh: konsultan payroll), jadi tenant tidak bisa diturunkan dari user saja.

## Options

| Opsi | Cara | Kelebihan | Kekurangan |
|---|---|---|---|
| A. Header `X-Tenant-ID` per request, cek membership di DB setiap request | Client kirim tenant, server validasi | Revocation instan | +1 query per request; mudah lupa validasi di satu jalur |
| B. Tenant di path `/v1/tenants/{id}/...` + cek membership per request | Eksplisit di URL | Jelas di log/route | Sama dengan A; URL panjang; tetap butuh cek per request |
| **C. Claim `tid` di access token, diterbitkan setelah cek membership** | Endpoint *switch tenant* menerbitkan token baru | Tanpa query membership per request; tenant tidak bisa dipalsukan (token ditandatangani) | Revocation tertunda sampai token habis (≤ 15 menit) |
| D. Subdomain per tenant (`alpha.payrun.app`) | Tenant dari host | UX branding | DNS/TLS wildcard, cookie per subdomain; tetap butuh C/A untuk validasi |

## Decision

**C.** Access token (ADR-009) memuat `sub` (user_id), `tid` (tenant aktif), dan `roles` (role di tenant itu).

```text
Login            → token dengan tid = tenant default (atau tanpa tid jika belum memilih)
POST /v1/session/switch-tenant {tenant_id}
                 → cek membership aktif di DB → terbitkan access token baru dengan tid
Request          → middleware: verify JWT → Principal{UserID, TenantID, Roles} di context
Service          → db.WithTenantTx(ctx, fn)  // satu-satunya cara repository mendapat koneksi
```

```go
func (d *DB) WithTenantTx(ctx context.Context, fn func(q *Queries) error) error {
    p, ok := auth.PrincipalFrom(ctx)
    if !ok || p.TenantID == uuid.Nil {
        return ErrNoTenantContext // fail closed, tidak pernah fallback
    }
    return pgx.BeginTxFunc(ctx, d.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
        // is_local = true → hanya berlaku sampai COMMIT/ROLLBACK
        if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, p.TenantID.String()); err != nil {
            return err
        }
        return fn(New(tx))
    })
}
```

Worker memakai varian `WithTenantTxFor(ctx, tenantID, fn)`; `tenant_id` wajib ada di setiap payload job.

### Pitfall dan penanganannya

| Pitfall | Akibat | Penanganan |
|---|---|---|
| `SET app.tenant_id` (session-level) | Nilai menempel di koneksi; request berikutnya dari tenant lain mewarisi tenant lama | Hanya `set_config(..., true)` di dalam transaksi; lint/grep CI melarang `SET app.` tanpa `LOCAL`; test: dua transaksi berurutan di koneksi yang sama, transaksi kedua tanpa setting → 0 baris |
| Setting tidak diset | Bisa jadi "lihat semua" kalau policy salah tulis | `current_setting('app.tenant_id', true)` → `NULL` → 0 baris (**fail closed**); `WithTenantTx` menolak context tanpa tenant |
| Query di luar `WithTenantTx` | RLS mengembalikan 0 baris, bug tersembunyi | Repository hanya menerima `*Queries` dari `WithTenantTx`; pool mentah tidak diekspor dari package `db` |
| Background job | Worker tidak punya HTTP context | `tenant_id` field wajib di payload; job tanpa tenant ditolak (kecuali job platform yang eksplisit) |
| Cache key (jika nanti ada, ADR-008) | Data tenant A disajikan ke B | Helper `cache.Key(tenantID, ...)` adalah satu-satunya pembuat key |
| Log | Sulit investigasi insiden per tenant; risiko PII | `tenant_id` dan `user_id` ditambahkan ke logger context oleh middleware; PII di-redact (ADR-P08) |
| Object storage key | File tenant A bisa diakses lewat key tebakan | Key `tenants/{tenant_id}/...`, akses hanya lewat presigned URL setelah cek authz (ADR-012) |
| Metrik | Label `tenant_id` meledakkan kardinalitas | `tenant_id` hanya di log/trace; metrik per tenant hanya untuk top-N atau via log-based query |

### Revocation

Membership dicabut → refresh token user untuk tenant itu tidak bisa lagi menerbitkan `tid` tersebut, sehingga akses berakhir **paling lambat 15 menit**. Operasi sensitif (finalize payroll, ubah gaji, ekspor rekening bank, lihat slip karyawan lain) **memeriksa ulang membership dan role di DB** dalam transaksi yang sama.

### Platform operator (P2)

Tidak ada role "super admin yang bypass RLS" di `payrun_app`. Akses support memakai **break-glass**: grant ber-durasi (misalnya 1 jam), alasan wajib diisi, read-only, tercatat di audit log tenant, dan terlihat oleh owner tenant. Implementasinya menerbitkan token dengan `tid` target plus claim `impersonation`.

### Jalur lintas tenant yang eksplisit: `WithPlatformTx`
Modul `tenancy` dan `iam` perlu membaca data **lintas tenant milik satu user**, misalnya daftar membership saat login atau saat memilih tenant. Untuk itu ada `db.WithPlatformTx(ctx, userID, fn)` yang menyetel `app.user_id` (transaction-local). Tabel `memberships` punya policy kedua `USING (user_id = (SELECT payrun.current_user_id()))`. Jadi jalur ini **tetap di bawah RLS** (bukan bypass), hanya dengan kunci yang berbeda. Pemakaiannya dibatasi lewat depguard ke modul `tenancy`/`iam` saja (detail di Step 3 `05-payrun-multi-tenancy.md`).

### Amandemen (Step 12: C2): mode eksklusif
`WithTenantTx` menyetel **hanya** `app.tenant_id`; `WithPlatformTx` menyetel **hanya** `app.user_id`. Policy self (`users_self`, `memberships_self_read`, cabang self `tenants_current`) mensyaratkan `current_tenant() IS NULL`. Tanpa aturan ini, policy permisif yang digabung dengan OR membuat user multi-tenant melihat barisnya di tenant lain dari dalam konteks tenant A, dan C-46 menembak sebagai false positive. Semua policy memakai helper `payrun.current_tenant()` / `current_user_id()` (`nullif(…,'')`), karena setelah GUC pernah disetel di session, `current_setting(…, true)` mengembalikan `''`, bukan NULL (Step 12: C3).

## Reason

- Tenant tidak bisa dipalsukan karena `tid` ditandatangani server dan hanya diterbitkan setelah cek membership.
- Tidak ada query membership di hot path, dan jendela 15 menit sesuai dengan masa berlaku access token.
- Satu jalur akses DB (`WithTenantTx`) membuat lupa set konteks menjadi error yang terlihat, bukan kebocoran diam-diam.

## Trade-offs

- Revocation tidak instan untuk operasi non-sensitif (maksimal 15 menit).
- Berpindah tenant butuh token baru; SPA harus menangani refresh state.
- Satu transaksi per unit kerja wajib, termasuk untuk read sederhana (overhead `set_config` kecil, tetapi ada).

## Future Migration

| Pemicu | Langkah |
|---|---|
| Butuh revocation instan (enterprise, compliance) | Denylist `jti`/membership-version di Redis dengan cek per request (ADR-007) |
| SSO enterprise | Tenant ditentukan dari IdP/domain email; claim `tid` tetap jadi mekanisme internal |
| Hybrid silo (ADR-P01 V3) | `WithTenantTx` memilih pool berdasarkan `tenant_catalog.placement` sebelum `BEGIN` |

## Interview angle

- **"Kenapa `SET LOCAL`, bukan `SET`?"** → Connection pool menggunakan ulang koneksi; `SET` session akan bocor ke request tenant lain. `set_config(..., true)` hilang saat transaksi selesai.
- **"User mengganti `tid` di token?"** → Signature tidak valid. Token baru hanya terbit setelah cek membership.
- **"Bagaimana dengan job yang berjalan setelah user dikeluarkan?"** → Job membawa `tenant_id` dan memvalidasi status tenant/run; tidak bergantung pada user yang memicunya.

## Related

ADR-P01, ADR-009 (authentication), ADR-010 (authorization), ADR-006 (job payload), ADR-008 (cache key), ADR-012, ADR-P08 (redaction).
