# ADR-P08 — PII: Application-Level Field Encryption, Blind Index, Masking

> Status: **Accepted** · Tanggal: 2026-10-01 · Berlaku untuk: **PayRun**

## Context / Problem

PayRun menyimpan data identitas karyawan (NIK, NPWP, nomor rekening bank) dan data gaji. Kebocoran bisa terjadi lewat jalur yang **tidak** melewati authorization aplikasi: dump/backup database yang bocor, akses read-only analis, log, atau error message. Disk encryption (at rest) di provider tidak melindungi dari dump logis.

Kebutuhan tambahan:
- NIK harus unik per tenant dan bisa dicari (cek duplikat saat import);
- HR perlu melihat nomor rekening untuk ekspor bank, tetapi UI umumnya cukup menampilkan versi termasking.

## Options

| Opsi | Melindungi dump DB | Bisa dicari / unik | Kompleksitas |
|---|---|---|---|
| A. Hanya disk encryption provider | ❌ | ✅ | Rendah |
| B. `pgcrypto` di DB (`pgp_sym_encrypt`) | Sebagian (key lewat query/parameter, bisa muncul di log statement) | Sulit | Sedang |
| **C. Enkripsi di aplikasi (AES-256-GCM) + blind index HMAC** | ✅ | ✅ (equality via blind index) | Sedang |
| D. KMS envelope per tenant (DEK per tenant dienkripsi KMS) | ✅ + crypto-shredding | ✅ | Tinggi; butuh KMS |

## Decision

**C untuk V1**, dengan desain yang siap berpindah ke D.

### Field yang dienkripsi

| Field | Dienkripsi | Blind index | Masking default |
|---|---|---|---|
| NIK | ✅ | ✅ (uniqueness per tenant, pencarian) | `************1234` |
| NPWP | ✅ | ✅ | `**.***.***.*-***.567` (4 digit terakhir) |
| Nomor rekening bank | ✅ | ❌ | `****1234` |
| Nama, email kerja | ❌ (dibutuhkan untuk pencarian/sorting, dilindungi RLS + RBAC) | — | — |
| **Jumlah gaji & item payroll** | ❌ | — | Disembunyikan berdasarkan role |

**Kenapa gaji tidak dienkripsi per field:** gaji dipakai untuk kalkulasi, agregasi (total run, laporan per departemen), dan validasi constraint di DB. Mengenkripsinya memindahkan semua agregasi ke aplikasi dan merusak invariant yang ditegakkan DB. Gaji dilindungi oleh **RLS (tenant) + RBAC (hanya HR/payroll/karyawan ybs) + audit log akses slip**. Trade-off ini diterima dan dijelaskan.

### Format ciphertext

```text
v1:<base64(nonce 12 byte)>:<base64(ciphertext||tag)>
AAD = tenant_id || ":" || table.column      // contoh "3f1c…:employees.nik"
```
AAD mencegah *ciphertext swapping*: ciphertext NIK karyawan tenant A yang disalin ke baris tenant B, atau ke kolom lain, gagal didekripsi.

### Blind index

```text
nik_bidx = HMAC-SHA256(key_bidx, tenant_id || ":" || normalize(nik))
UNIQUE (tenant_id, nik_bidx)
```
Key blind index **terpisah** dari key enkripsi. Normalisasi (hapus spasi/titik) sebelum HMAC.

### Keyring & rotasi

- Keyring dari secret (env/secret manager): `PII_KEYS="v1:<base64>,v2:<base64>"`, `PII_ACTIVE=v2`.
- Enkripsi selalu memakai key aktif; dekripsi memilih key berdasarkan prefix versi.
- Rotasi: tambah `v2`, jadikan aktif, job background melakukan re-encrypt *lazy/batch*; `v1` dihapus setelah tidak ada ciphertext `v1` tersisa (diverifikasi dengan query prefix).
- Rotasi key blind index membutuhkan rebuild index penuh (jarang dilakukan).

### Masking & redaction

- API mengembalikan nilai termasking secara default; nilai penuh hanya lewat endpoint/flag khusus untuk role yang berhak, dan setiap akses **tercatat di audit log**.
- Logger memiliki redactor berbasis nama field (`nik`, `npwp`, `account_number`, `password`, `token`) dan tipe `Sensitive[T]` yang `String()`-nya selalu `[REDACTED]`.
- Error message tidak pernah menyertakan nilai field.

### Model ancaman

| Ancaman | Dilindungi? |
|---|---|
| Dump/backup DB bocor | ✅ (tanpa key aplikasi, field PII tidak terbaca) |
| Akses read-only DB oleh analis/support | ✅ untuk PII terenkripsi; gaji tetap terlihat (mitigasi: role DB terbatas) |
| Ciphertext dipindah antar tenant/kolom | ✅ (AAD) |
| Server aplikasi atau secret-nya diretas | ❌ (penyerang punya key) |
| User sah dengan role HR menyalahgunakan akses | ❌ secara teknis; terdeteksi lewat audit log |

## Reason

- Melindungi skenario kebocoran paling umum (backup/dump) tanpa infrastruktur KMS di V1.
- Blind index menjaga kebutuhan bisnis (cek duplikat NIK) tetap ditegakkan DB.
- Format berversi dan AAD membuat migrasi ke KMS envelope tidak memerlukan perubahan skema.

## Trade-offs

- Tidak bisa query `LIKE`/range pada field terenkripsi.
- Kehilangan key = kehilangan data; backup key wajib dan terpisah dari backup DB.
- Blind index membocorkan kesamaan nilai (dua baris dengan NIK sama terlihat sama) di dalam tenant yang sama; diterima karena NIK memang harus unik.

## Future Migration

| Pemicu | Langkah |
|---|---|
| Tenant enterprise / compliance (UU PDP) menuntut penghapusan terverifikasi | **KMS envelope per tenant**: DEK per tenant dienkripsi KMS; hapus DEK = crypto-shredding seluruh PII tenant |
| Audit menuntut pemisahan akses key | Pindahkan keyring ke secret manager/KMS dengan IAM terpisah |

## Interview angle

- **"Kenapa gaji tidak dienkripsi?"** → Dibutuhkan untuk agregasi dan constraint; dilindungi RLS, RBAC, dan audit. Enkripsi bukan pengganti authorization.
- **"Bagaimana mengecek NIK duplikat kalau terenkripsi?"** → Blind index HMAC dengan key terpisah dan unique constraint per tenant.
- **"Apa yang tidak dilindungi desain ini?"** → Aplikasi yang diretas. Enkripsi di aplikasi melindungi data di luar aplikasi, bukan dari aplikasinya sendiri.

## Related

ADR-P01 (RLS), ADR-P02 (redaction di log), ADR-010 (RBAC), Step 6 (threat model, nanti).
