# ADR-012 — Object Storage: S3-compatible, Hanya untuk PayRun

Status: Accepted · Tanggal: 2026-10-01 · Berlaku untuk: PayRun (LedgerPay sengaja tidak memakai)

## Context / Problem

PayRun menghasilkan dan menerima file: payslip PDF (P1), file CSV import absensi, export file transfer bank. File ini sensitif (gaji, rekening) dan tenant-scoped. LedgerPay hanya menerima settlement report dari mock provider, dalam bentuk data kecil yang ditarik lewat API.

## Options

| Opsi | Pros | Cons |
|---|---|---|
| **S3-compatible object storage** (MinIO lokal, S3/R2 saat deploy) | Murah, tahan lama, presigned URL, tidak membebani DB | Satu komponen lagi; konsistensi metadata DB ↔ objek harus dijaga |
| Simpan sebagai `bytea` di Postgres | Transaksional, satu backup | Bloat DB, backup lambat, memori aplikasi saat streaming |
| Filesystem lokal / volume | Sederhana | Tidak survive pergantian host; tidak bisa di-scale ke >1 instance; tanpa presigned URL |

## Decision

- **PayRun:** S3-compatible API melalui satu interface `platform/storage` (MinIO di Docker Compose, S3/R2 di deploy).
  - Bucket **private**, tanpa public read.
  - Key layout: `tenants/{tenant_id}/payslips/{run_id}/{employee_id}.pdf`, `tenants/{tenant_id}/imports/{import_id}.csv`.
  - Metadata (key, ukuran, checksum SHA-256, content type, pemilik) disimpan di tabel DB **ber-RLS**. Akses selalu melalui API yang memeriksa authorization, lalu menerbitkan **presigned GET URL dengan TTL 5 menit**.
  - Upload CSV: divalidasi ukuran dan content type, diproses async; file asli disimpan untuk audit.
  - Objek ditulis dulu, lalu metadata di-commit. Objek yatim (orphan) dibersihkan job berkala.
- **LedgerPay: tidak memakai object storage.** Settlement report ditarik dari mock provider API, baris-barisnya disimpan di tabel recon beserta hash file.

## Reason

- Payslip PDF yang di-generate async adalah alasan nyata (strategi produk PayRun §6.3). LedgerPay tidak punya kebutuhan itu, jadi tidak ditambahkan.
- `tenant_id` di prefix key membuat isolasi tenant berlaku juga untuk file (strategi produk PayRun C1: "file storage path").
- Presigned URL pendek membatasi dampak jika URL bocor (dibagikan, tercatat di log proxy).

## Trade-offs

- Tidak transaksional dengan DB: perlu urutan tulis + pembersihan orphan.
- Presigned URL tetap bisa dipakai siapa pun selama 5 menit (diterima; TTL pendek + audit penerbitan URL).
- MinIO menambah satu container di environment lokal.

## Future Migration

| Trigger | Perubahan |
|---|---|
| Kebutuhan crypto-shredding per tenant | Server-side encryption dengan kunci per tenant (SSE-KMS), selaras ADR-P08 V2 |
| Retensi dokumen regulasi | Lifecycle policy (pindah ke cold storage setelah N bulan, object lock untuk immutability) |
| LedgerPay butuh upload dokumen (KYC, dispute) | Pakai `platform/storage` yang sama (saat ini out of scope) |

## Interview angle

- *"Bagaimana mencegah employee mengunduh slip gaji orang lain lewat URL?"* → Bucket private, authorization di API, presigned TTL 5 menit, key tidak bisa ditebak tanpa akses.
- *"Bagaimana konsistensi antara file dan DB?"* → Urutan tulis + job orphan cleanup; DB adalah sumber kebenaran tentang file mana yang valid.

## Amandemen Sprint 0 (PayRun): SeaweedFS menggantikan MinIO di dev

Keputusan inti tidak berubah: S3-compatible API lewat `platform/storage`, bucket private, presigned URL pendek. Yang berubah hanya **server S3 di Docker Compose dev**.

| Aspek | Desain | Implementasi S0 | Alasan |
|---|---|---|---|
| Server S3 lokal | MinIO | SeaweedFS `chrislusf/seaweedfs:3.97` (`server -s3`, port 8333), profile `storage` di `deploy/compose.yml` | Image MinIO komunitas tidak lagi bisa di-pull (diverifikasi saat Sprint 0) |
| Akses bucket | Bucket private | `deploy/storage/s3.json`: satu identitas `payrun-app` dengan `Read`/`Write` hanya ke `payrun-private`; tidak ada identitas anonim, sehingga request tanpa tanda tangan ditolak | Sama dengan desain |
| Init bucket | `minio-init` | Service one-shot `storage-init` (`weed shell s3.bucket.create`), idempoten | Pengganti setara |

Konsekuensi: kode aplikasi hanya memakai API S3 standar (SigV4, presign), sehingga bisa berpindah ke S3/R2 saat deploy tanpa perubahan. Fitur khusus MinIO (admin API, `mc`) tidak boleh dipakai. Kalau ditemukan perbedaan perilaku presign antara SeaweedFS dan S3/R2 (Sprint 7), catat di sini.

**Catatan Sprint 7 (presign diverifikasi dengan SeaweedFS 3.97):** presigned GET SigV4 path-style (`X-Amz-Expires=300`) → `200 application/pdf`; GET tanpa tanda tangan → `403`; URL yang diubah → `403`; `response-content-disposition` dihormati. Tidak ada perbedaan yang memengaruhi kode. Dua catatan: (1) tanda tangan mengikat host, jadi aplikasi menandatangani dengan `S3_PUBLIC_ENDPOINT` (host yang dibuka browser) sementara PUT memakai `S3_ENDPOINT` internal; (2) izin `List` dicabut dari identitas `payrun-app` (sebelumnya ada di `s3.json`), sesuai least privilege T-PR-09. Kedaluwarsa 5 menit adalah perilaku SigV4 standar dan tidak diuji ulang di sini (`TestPayslipPresignedURLTTL` memeriksa parameter tanda tangan).

## Related

- [ADR-P01](ADR-P01-multi-tenancy-pooled-rls.md) · [ADR-P08](ADR-P08-pii-encryption.md) · [ADR-006](ADR-006-background-processing-postgres-queue.md)
