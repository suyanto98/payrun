# Naskah video demo PayRun (90 detik)

Untuk recruiter dan HR yang tidak sempat mencoba demo. Satu pesan: **payroll yang tidak bisa salah diam-diam**.
Semua yang ditampilkan memakai demo publik ("Coba demo"), jadi penonton bisa mengulangnya sendiri.

Setelah diunggah: isi `VITE_VIDEO_URL` saat build SPA (tautan muncul di landing) dan `[LINK]` video di README.
Tiga tautan opsional SPA adalah build arg image web: `VITE_VIDEO_URL`, `VITE_REPO_URL` ("Lihat kode"), dan
`VITE_CONTACT_URL` ("Hubungi pembuatnya" di rangkuman demo, contoh profil LinkedIn). Kosong = tautan disembunyikan.

## Persiapan
- Browser jendela 1280×800, zoom 110%, mode terang, tanpa bookmark bar dan notifikasi.
- Buka landing (`/welcome`) di tab baru; jangan klik apa pun sebelum mulai merekam.
- Rekam layar + suara (OBS / perekam bawaan). Narasi dibaca santai, ±130 kata per menit.
- Kalau salah, ulangi per segmen; potong di editor.

## Storyboard

| Waktu | Di layar | Narasi | Teks di layar |
|---|---|---|---|
| 0:00–0:08 | Landing PayRun | "Ini PayRun, aplikasi payroll yang saya bangun untuk banyak perusahaan sekaligus. Saya tunjukkan tiga hal yang tidak boleh terjadi di urusan gaji." | PayRun · proyek portofolio |
| 0:08–0:15 | Klik **Coba demo**; tunggu ±2 detik | "Satu klik: sistem menyiapkan perusahaan contoh dengan 15 karyawan, dan gaji bulan lalu benar-benar dihitung." | Perusahaan siap dalam ±2 detik |
| 0:15–0:28 | Buka payroll bulan lalu (Final); arahkan ke grafik departemen; klik **Cek ulang semua slip** | "Gaji bulan lalu sudah final. Saat audit, saya bisa menghitung ulang semua slip dari catatannya sendiri, dan hasilnya harus sama persis." | 15 dari 15 slip cocok persis |
| 0:28–0:45 | Kembali ke Payroll → **Buat payroll** → **Pasang gangguan** → **Hitung**; tunggu timeline | "Sekarang bulan ini, dengan simulasi server mati di tengah perhitungan. Satu bagian mati sebelum tersimpan, lalu diulang otomatis. Hasilnya: 15 karyawan, 15 slip. Tidak ada yang dibayar dua kali." | 1. Server mati di tengah jalan → tidak ada gaji dobel |
| 0:45–0:55 | **Minta persetujuan**; tunjuk bar kuning "Ganti peran" | "Dina, admin payroll, mengajukan. Dia tidak bisa menyetujui pengajuannya sendiri: harus orang kedua." | 2. Persetujuan dua orang |
| 0:55–1:10 | **Ganti peran → Hendra**; Persetujuan → **Setujui**; Karyawan → Bayu → **Ubah gaji** mulai bulan lalu → pesan ditolak | "Sebagai owner, saya setujui. Lalu saya coba menaikkan gaji Bayu mulai bulan lalu: ditolak, karena gaji yang sudah final tidak bisa diubah diam-diam. Perubahan hanya berlaku ke depan, dan riwayatnya tersimpan." | 3. Gaji final terkunci |
| 1:10–1:20 | **Ganti peran → Rizky**; Slip gaji; gulir ke rincian BPJS; klik **Unduh PDF** | "Karyawan melihat slipnya sendiri, lengkap dengan BPJS dan potongan. Pertanyaan ke HR berkurang." | Slip mandiri untuk karyawan |
| 1:20–1:30 | Kartu rangkuman di bar demo | "Data setiap perusahaan dipisah di tingkat database, dan semua janji ini diuji otomatis setiap kali kodenya berubah. Silakan coba sendiri lewat tautan di deskripsi." | Coba sendiri: [URL demo] |

## Catatan
- Jangan menyebut istilah teknis di narasi (RLS, chunk, idempotency). Untuk penonton teknis, tautkan README.
- Angka di video harus sama dengan yang tampil di layar; jangan menambahkan klaim yang tidak ditunjukkan.
- Simulasi gangguan memakai kegagalan per perusahaan (bukan mematikan server sungguhan), sama dengan yang dicoba pengunjung.
