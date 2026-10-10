// Pesan error untuk user, per `code` katalog RFC 9457 (docs/design/15-api-error-catalogue.md). Server mengirim
// `detail` berbahasa Inggris untuk developer; UI menampilkan teks ini. Setiap code di internal/platform/apperr
// wajib punya entri (TestSPA_ErrorMessagesCoverCatalogue). Halaman boleh menimpa pesan untuk konteksnya sendiri.
import { ApiError } from './api'

export const errorMessages: Record<string, string> = {
  ACTIVE_RUN_EXISTS: 'Sudah ada payroll run aktif untuk periode ini.',
  APPROVAL_ALREADY_DECIDED: 'Permintaan ini sudah diputuskan orang lain.',
  CLOCK_OUT_WITHOUT_CLOCK_IN: 'Belum ada clock in untuk hari ini.',
  COMPONENT_CYCLE: 'Komponen saling merujuk (siklus). Periksa referensi persentase.',
  DECISION_NOTE_REQUIRED: 'Catatan wajib diisi saat menolak.',
  DUPLICATE_CODE: 'Kode ini sudah dipakai.',
  EMAIL_ALREADY_REGISTERED: 'Email ini sudah terdaftar. Silakan masuk.',
  EMAIL_VERIFICATION_INVALID: 'Tautan verifikasi tidak berlaku atau sudah kedaluwarsa.',
  EMPLOYEE_NOT_ACTIVE: 'Karyawan tidak aktif pada tanggal tersebut.',
  EMPLOYEE_NO_TAKEN: 'Nomor karyawan sudah dipakai.',
  FORBIDDEN: 'Anda tidak punya akses untuk tindakan ini.',
  IDEMPOTENCY_KEY_INVALID: 'Permintaan tidak valid. Muat ulang halaman lalu coba lagi.',
  IDEMPOTENCY_KEY_REQUIRED: 'Permintaan tidak valid. Muat ulang halaman lalu coba lagi.',
  IDEMPOTENCY_KEY_REUSED: 'Isi form berubah sejak percobaan sebelumnya. Kirim ulang form.',
  INSUFFICIENT_LEAVE_BALANCE: 'Saldo cuti tidak cukup.',
  INTERNAL_ERROR: 'Terjadi kesalahan di server. Coba lagi beberapa saat lagi.',
  INVALID_COMPONENT_REFERENCE: 'Komponen yang dirujuk tidak ada atau tidak bisa dipakai.',
  INVALID_CREDENTIALS: 'Email atau password salah.',
  INVALID_CURSOR: 'Halaman tidak valid. Muat ulang daftar.',
  INVALID_STATE_TRANSITION: 'Tindakan ini tidak bisa dilakukan pada status saat ini. Muat ulang halaman.',
  INVITATION_INVALID: 'Undangan tidak berlaku, sudah dipakai, atau untuk email lain.',
  LAST_OWNER: 'Tenant harus tetap punya minimal satu OWNER.',
  MALFORMED_JSON: 'Permintaan tidak valid. Muat ulang halaman lalu coba lagi.',
  MANAGER_CYCLE: 'Atasan ini akan membuat rantai atasan berputar.',
  MEMBERSHIP_EXISTS: 'Orang ini sudah menjadi anggota tenant.',
  NIK_TAKEN: 'NIK sudah dipakai karyawan lain.',
  NO_APPROVER_AVAILABLE: 'Tidak ada approver yang tersedia. Hubungi OWNER.',
  NOT_FOUND: 'Data tidak ditemukan.',
  NPWP_TAKEN: 'NPWP sudah dipakai karyawan lain.',
  OVERLAPPING_LEAVE: 'Sudah ada cuti pada tanggal yang beririsan.',
  OVERLAPPING_PERIOD: 'Rentang tanggal beririsan dengan data yang sudah ada.',
  PAYLOAD_TOO_LARGE: 'Data terlalu besar.',
  PAYSLIP_NOT_READY: 'PDF slip gaji sedang dibuat. Coba lagi sebentar lagi.',
  PERIOD_LOCKED: 'Periode payroll ini sudah final dan terkunci.',
  PRECONDITION_REQUIRED: 'Data perlu dimuat ulang sebelum diubah.',
  RATE_LIMITED: 'Terlalu banyak percobaan. Tunggu sebentar lalu coba lagi.',
  REFRESH_TOKEN_REUSED: 'Sesi berakhir demi keamanan. Silakan masuk lagi.',
  REQUEST_IN_PROGRESS: 'Permintaan yang sama sedang diproses.',
  RESOURCE_BUSY: 'Data sedang diproses. Coba lagi sebentar lagi.',
  RUN_FINALIZED: 'Payroll run ini sudah final dan tidak bisa diubah.',
  RUN_HAS_ERRORS: 'Masih ada karyawan berstatus ERROR. Perbaiki atau kecualikan dulu.',
  RUN_STALE: 'Data input berubah sejak kalkulasi. Hitung ulang payroll run.',
  SELF_APPROVAL_FORBIDDEN: 'Pengaju tidak boleh menyetujui permintaannya sendiri.',
  SELF_APPROVAL_REASON_REQUIRED: 'Alasan wajib diisi karena Anda satu-satunya admin.',
  SERVICE_UNAVAILABLE: 'Layanan sedang sibuk. Coba lagi beberapa saat lagi.',
  TOKEN_EXPIRED: 'Sesi berakhir. Silakan masuk lagi.',
  UNAUTHENTICATED: 'Sesi berakhir. Silakan masuk lagi.',
  UNSUPPORTED_MEDIA_TYPE: 'Format data tidak didukung.',
  VALIDATION_ERROR: 'Periksa kembali isian form.',
  VERSION_CONFLICT: 'Data sudah diubah orang lain. Muat ulang lalu ulangi perubahan.',
}

/** Teks untuk user dari error apa pun. Code tak dikenal → pesan server; bukan ApiError → fallback. */
export function errorText(e: unknown, fallback: string): string {
  if (!(e instanceof ApiError)) return fallback
  const base = errorMessages[e.code] ?? e.message ?? fallback
  if (e.code === 'VALIDATION_ERROR' && e.fields.length > 0) {
    const f = e.fields[0]
    return `${base} (${f.field}${f.message ? ': ' + f.message : ''})`
  }
  return base
}
