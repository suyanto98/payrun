# ADR-010 — Authorization: RBAC + Resource/Relationship Policy di Service Layer

Status: Accepted · Tanggal: 2026-10-01 · Berlaku untuk: LedgerPay, PayRun

## Context / Problem

Authentication menjawab *siapa kamu*. Authorization menjawab *boleh melakukan apa terhadap resource yang mana*. Kerentanan API paling umum (OWASP API Top 10 #1) adalah **BOLA/IDOR**: user yang sah mengganti ID di URL lalu mengakses resource milik orang lain.

- LedgerPay: Rina hanya boleh melihat wallet & transaksinya; merchant Budi hanya pembayaran miliknya; Sari (ops) boleh melihat semua tetapi aksinya ter-audit.
- PayRun: role saja tidak cukup. "Manager boleh approve cuti" harus menjadi "manager boleh approve cuti **bawahan langsungnya**". Employee hanya boleh melihat slip gajinya sendiri.

## Options

| Opsi | Pros | Cons |
|---|---|---|
| RBAC murni (cek role di middleware route) | Sederhana | Tidak menangani kepemilikan/relasi, sehingga BOLA tetap terbuka |
| **RBAC + policy function per use case di service layer + query scoping** | Eksplisit, mudah di-test, cukup untuk aturan relasi sederhana | Policy tersebar per modul; disiplin dibutuhkan agar tidak ada use case yang lupa |
| Policy engine (OPA/Rego, Casbin, Cedar) | Policy terpusat, bisa diaudit, deklaratif | Bahasa & infra tambahan; aturan relasi (manager-of) butuh data dari DB anyway; berlebihan untuk jumlah aturan saat ini |
| Zanzibar-style ReBAC (SpiceDB/OpenFGA) | Sangat kuat untuk relasi kompleks | Infra besar; overkill |

## Decision

1. **Middleware**: memastikan terautentikasi dan memuat `Principal` (user_id, roles, tenant_id) ke context.
2. **Service layer**: setiap use case memanggil policy eksplisit sebelum bertindak.
3. **Query scoping**: repository untuk resource milik user **selalu** menyertakan pemilik di `WHERE`. Resource yang bukan milik user tidak pernah ter-load.
4. **Resource yang tidak boleh diakses → 404 Not Found**, bukan 403.
5. PayRun menambah **RLS** sebagai lapisan terakhir untuk batas tenant ([ADR-P01](ADR-P01-multi-tenancy-pooled-rls.md)).

```go
// platform/authz
type Principal struct { UserID, TenantID uuid.UUID; Roles []Role }

// LedgerPay
func CanViewWallet(p Principal, w Wallet) bool { return w.OwnerID == p.UserID || p.Has(RoleOps) }

// PayRun
func CanApproveLeave(p Principal, req LeaveRequest, approverOf func(emp uuid.UUID) uuid.UUID) error {
    if req.EmployeeUserID == p.UserID { return ErrSelfApproval }
    if approverOf(req.EmployeeID) != p.UserID && !p.Has(RoleHRAdmin) { return ErrNotFound }
    return nil
}

// repository: kepemilikan di query, bukan filter setelah load
// SELECT ... FROM wallets WHERE id = $1 AND owner_id = $2
```

## Reason

- **404 vs 403:** 403 membocorkan bahwa resource dengan ID tersebut **ada**, sehingga penyerang bisa melakukan enumerasi. 404 memperlakukan "tidak ada" dan "bukan milikmu" secara identik. (Untuk aksi pada resource yang memang terlihat tetapi tidak boleh diubah, misalnya employee melihat run payroll tapi tidak boleh finalize, dipakai 403.)
- Query scoping membuat kebocoran butuh **dua** kesalahan, bukan satu: policy lupa **dan** query lupa.
- Policy sebagai fungsi biasa bisa diuji dengan table-driven test dan menjadi dasar *authorization matrix test* (Step 8).
- **Kenapa bukan OPA/Casbin/Cedar sekarang:** jumlah aturan masih puluhan, dan aturan relasi butuh data dari DB. Policy engine menambah bahasa kedua tanpa mengurangi kode.

## Trade-offs

- Policy tersebar di modul; perlu checklist review "use case baru = policy + test".
- Tidak ada UI admin untuk mengubah policy (diterima: role statis).

## Future Migration

| Trigger | Perubahan |
|---|---|
| Tenant butuh custom role / permission granular | Tabel permission + role kustom per tenant; policy membaca permission, bukan nama role |
| Aturan menjadi ratusan atau perlu diaudit oleh pihak non-engineer | Cedar atau OPA dengan data relasi disuplai aplikasi |
| Relasi kompleks (delegasi, hierarki multi-level, sharing) | ReBAC (OpenFGA/SpiceDB) |

## Interview angle

- *"Bagaimana kamu mencegah IDOR/BOLA?"* → Query scoping + policy + 404 + test otomatis lintas user/tenant.
- *"403 atau 404?"* → Jelaskan enumerasi dan kapan 403 tetap tepat.
- *"Bagaimana kamu tahu tidak ada endpoint yang lupa dicek?"* → Authorization matrix test yang memanggil setiap endpoint dengan principal yang salah.

## Amandemen (Step 5, 2026-10-01)

**Separation of duties (PayRun):** `HR_ADMIN` mengelola gaji, `PAYROLL_ADMIN` menjalankan payroll, dan approver run harus berbeda dari pembuatnya (kecuali pengecualian satu admin, ADR-P09). Satu orang dengan role `HR_ADMIN` + `PAYROLL_ADMIN` **diizinkan** di V1, karena UMKM sering hanya punya satu orang HR/finance. Kombinasi ini menghasilkan audit `membership.sod_conflict` dan ditampilkan sebagai peringatan di halaman member. Matriks lengkap ada di Step 5 `05-payrun-authorization-matrix.md`.

## Related

- [ADR-009](ADR-009-authentication.md) · [ADR-P01](ADR-P01-multi-tenancy-pooled-rls.md) · [ADR-P02](ADR-P02-tenant-context.md) · [ADR-P09](ADR-P09-approval-workflow.md)
