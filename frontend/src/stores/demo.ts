import { defineStore } from 'pinia'
import { api, ApiError } from '../api'
import { useAuth } from './auth'

// Sandbox pengunjung demo publik (hanya image demo). Build release: GET /demo = 404 → semua UI demo tersembunyi.
export type DemoPersona = { key: 'payroll' | 'owner' | 'employee'; name: string; title: string }
export type TourLinks = { last_month_run: string | null; current_run: string | null; raise_employee: string | null }
export type Sandbox = { expires_at: string; company: string; persona: DemoPersona['key'] | ''; personas: DemoPersona[]; links?: TourLinks }

const cookieHeaders = { 'X-Requested-With': 'fetch' }

/** Langkah tur demo. Tercentang otomatis saat benar-benar dilakukan (bukan saat diklik), lintas ganti peran. */
export type TourKey = 'final' | 'verify' | 'crash' | 'submit' | 'approve' | 'raise' | 'payslip'
export type TourStep = { key: TourKey; persona: DemoPersona['key']; to: string; title: string; hint: string; why: string }
export const tour: TourStep[] = [
  { key: 'final', persona: 'payroll', to: '/payroll', title: 'Buka payroll bulan lalu yang sudah final',
    hint: 'Klik langkah ini: payroll bulan lalu langsung terbuka.', why: 'Gaji yang sudah dibayar terkunci dan tercatat lengkap: siapa menghitung, siapa menyetujui, kapan.' },
  { key: 'verify', persona: 'payroll', to: '/payroll', title: 'Cek ulang semua slip',
    hint: 'Tekan tombol yang disorot: semua slip dihitung ulang.', why: 'Saat audit atau ada karyawan protes, HR bisa membuktikan setiap slip dihitung benar, bahkan berbulan-bulan kemudian.' },
  { key: 'crash', persona: 'payroll', to: '/payroll', title: 'Buat payroll bulan ini dengan "Pasang gangguan"',
    hint: 'Tekan "Buat payroll", lalu "Pasang gangguan" dan "Hitung" (keduanya akan disorot).', why: 'Server bisa mati kapan saja. Di sini tidak ada karyawan yang dibayar dua kali atau terlewat.' },
  { key: 'submit', persona: 'payroll', to: '/payroll', title: 'Minta persetujuan Pak Hendra',
    hint: 'Tekan "Minta persetujuan" yang disorot.', why: 'Orang yang menghitung gaji tidak bisa sekaligus menyetujuinya: mencegah salah hitung dan kecurangan.' },
  { key: 'approve', persona: 'owner', to: '/approvals', title: 'Setujui payroll atau cuti',
    hint: 'Tekan "Setujui" yang disorot (cuti Rizky atau payroll dari Dina).', why: 'Semua keputusan ada di satu kotak, dengan nama penyetuju tercatat.' },
  { key: 'raise', persona: 'owner', to: '/employees', title: 'Coba ubah gaji Bayu mulai bulan lalu',
    hint: 'Form ubah gaji Bayu langsung terbuka. Coba tanggal bulan lalu, lalu bulan ini.', why: 'Gaji yang sudah final tidak bisa diubah diam-diam. Perubahan berlaku ke depan dan riwayatnya tersimpan.' },
  { key: 'payslip', persona: 'employee', to: '/payslips', title: 'Buka slip gaji sendiri',
    hint: 'Buka slip yang disorot, lihat rincian BPJS, lalu unduh PDF.', why: 'Karyawan melihat sendiri rincian gajinya, jadi pertanyaan "kok gaji saya segini?" ke HR berkurang.' },
]

/** Tautan tur yang mendarat tepat di tempat aksinya (#penanda = elemen yang disorot, lihat spotlight.ts). */
export function tourLink(key: TourKey, l?: TourLinks): string {
  switch (key) {
    case 'final': return l?.last_month_run ? `/payroll/${l.last_month_run}#final` : '/payroll'
    case 'verify': return l?.last_month_run ? `/payroll/${l.last_month_run}#verify` : '/payroll'
    case 'crash': return l?.current_run ? `/payroll/${l.current_run}#crash` : '/payroll#create'
    case 'submit': return l?.current_run ? `/payroll/${l.current_run}#submit` : '/payroll#create'
    case 'approve': return '/approvals#approve'
    case 'raise': return l?.raise_employee ? `/employees/${l.raise_employee}?raise=1#raise` : '/employees'
    case 'payslip': return '/payslips#payslip'
  }
}

/** Halaman pertama setelah berganti peran: tempat persona itu punya sesuatu untuk dilakukan. */
export const personaLanding: Record<DemoPersona['key'], string> = { payroll: '/payroll', owner: '/approvals', employee: '/payslips' }

export const useDemo = defineStore('demo', {
  state: () => ({ available: null as boolean | null, ttlHours: 24, sandbox: null as Sandbox | null, done: [] as TourKey[], showSummary: false }),
  getters: {
    current: (s) => s.sandbox?.personas.find((p) => p.key === s.sandbox?.persona) ?? null,
    finished: (s) => tour.every((t) => s.done.includes(t.key)),
  },
  actions: {
    async detect(): Promise<boolean> {
      if (this.available === null) {
        try {
          const res = await fetch('/api/v1/demo')
          this.available = res.ok
          if (res.ok) this.ttlHours = Math.round(((await res.json()).ttl_seconds ?? 86400) / 3600)
        } catch {
          this.available = false
        }
      }
      return this.available
    },
    async load() {
      try {
        this.sandbox = await api<Sandbox>('/demo/sandbox')
        this.done = readTour(this.sandbox.expires_at)
      } catch (e) {
        this.sandbox = null
        if (!(e instanceof ApiError && e.status === 404)) throw e
      }
    },
    clear() {
      this.sandbox = null
      this.done = []
      this.showSummary = false
    },
    /** Tandai langkah tur selesai (hanya di sandbox). Selesai semua → rangkuman tampil sekali. */
    mark(key: TourKey) {
      if (!this.sandbox || this.done.includes(key)) return
      this.done = [...this.done, key]
      writeTour(this.sandbox.expires_at, this.done)
      if (this.finished) this.showSummary = true
    },
    /** Satu klik: server membuat perusahaan contoh (±5 detik: payroll bulan lalu benar-benar dihitung) lalu login. */
    async start() {
      const res = await fetch('/api/v1/demo/sessions', { method: 'POST', headers: cookieHeaders })
      const data = await res.json().catch(() => ({}))
      if (!res.ok) {
        if (data.code === 'RATE_LIMITED') throw new Error('Terlalu sering memulai demo. Tunggu sebentar, lalu coba lagi.')
        if (res.status === 503) throw new Error(data.detail?.includes('quota') ? 'Kuota demo hari ini sudah habis. Silakan coba lagi besok.' : 'Server sedang sibuk menyiapkan demo. Coba lagi sebentar lagi.')
        throw new Error('Demo belum bisa dimulai. Coba lagi sebentar lagi.')
      }
      await useAuth().useSession(data.access_token)
    },
    /** Ganti peran di sandbox yang sama (server memeriksa keanggotaan sandbox, bukan klaim browser). */
    async switchTo(persona: DemoPersona['key']) {
      const auth = useAuth()
      const res = await fetch('/api/v1/demo/persona', {
        method: 'POST',
        headers: { ...cookieHeaders, 'Content-Type': 'application/json', Authorization: `Bearer ${auth.token}` },
        body: JSON.stringify({ persona }),
      })
      const data = await res.json().catch(() => ({}))
      if (!res.ok) throw new Error('Tidak bisa berganti peran. Muat ulang halaman, lalu coba lagi.')
      await auth.useSession(data.access_token)
    },
  },
})

// Progres tur per sandbox (expires_at unik per sandbox) di localStorage: kenyamanan per penonton, bukan data
// yang harus awet. Mode privat / storage diblokir → tur tetap jalan, hanya tidak diingat setelah muat ulang.
function tourKey(sandboxExpiry: string) {
  return 'pr_demo_tour:' + sandboxExpiry
}
function readTour(sandboxExpiry: string): TourKey[] {
  try {
    const v = JSON.parse(localStorage.getItem(tourKey(sandboxExpiry)) ?? '[]')
    return Array.isArray(v) ? v.filter((k) => tour.some((t) => t.key === k)) : []
  } catch {
    return []
  }
}
function writeTour(sandboxExpiry: string, done: TourKey[]) {
  try {
    localStorage.setItem(tourKey(sandboxExpiry), JSON.stringify(done))
  } catch {
    /* mode privat */
  }
}
