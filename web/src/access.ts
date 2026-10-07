import type { Role } from './types'

// Hanya untuk menampilkan menu/tombol; server tetap menegakkan otorisasi (api/authz_matrix.yaml).
export const roleLabel: Record<Role, string> = {
  OWNER: 'Pemilik', HR_ADMIN: 'Admin HR', PAYROLL_ADMIN: 'Admin Payroll', MANAGER: 'Manajer', EMPLOYEE: 'Karyawan',
}

/** W1: OWNER/HR_ADMIN/PAYROLL_ADMIN (seluruh tenant), MANAGER (bawahan). EMPLOYEE → 403. */
export function canListEmployees(roles: Role[]): boolean {
  return roles.some((r) => r === 'OWNER' || r === 'HR_ADMIN' || r === 'PAYROLL_ADMIN' || r === 'MANAGER')
}

/** W2/W4/W5: hanya OWNER dan HR_ADMIN. */
export function canEditEmployees(roles: Role[]): boolean {
  return roles.includes('OWNER') || roles.includes('HR_ADMIN')
}

export const statusLabel: Record<string, string> = { ACTIVE: 'Aktif', RESIGNED: 'Resign', TERMINATED: 'Diberhentikan' }

/** Tanggal API (YYYY-MM-DD) → "5 Jan 2026" tanpa pergeseran zona waktu. */
export function formatDate(iso: string | null | undefined): string {
  if (!iso) return '—'
  const [y, m, d] = iso.split('-').map(Number)
  const months = ['Jan', 'Feb', 'Mar', 'Apr', 'Mei', 'Jun', 'Jul', 'Agu', 'Sep', 'Okt', 'Nov', 'Des']
  return `${d} ${months[m - 1]} ${y}`
}

/** "Admin HR · Karyawan" untuk daftar role membership. */
export function rolesText(roles: Role[]): string {
  return roles.map((r) => roleLabel[r]).join(' · ')
}

/** "6600000.00" → "Rp6.600.000" (string dari API; tidak pernah lewat float untuk aritmetika). */
export function formatMoney(v: string | null | undefined): string {
  if (!v) return '—'
  const [int, frac] = v.split('.')
  const grouped = int.replace(/\B(?=(\d{3})+(?!\d))/g, '.')
  return 'Rp' + grouped + (frac && /[1-9]/.test(frac) ? ',' + frac : '')
}

