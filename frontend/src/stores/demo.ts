import { defineStore } from 'pinia'
import { api, ApiError } from '../api'
import { useAuth } from './auth'

// Sandbox pengunjung demo publik (hanya image demo). Build release: GET /demo = 404 → semua UI demo tersembunyi.
export type DemoPersona = { key: 'payroll' | 'owner' | 'employee'; name: string; title: string }
export type Sandbox = { expires_at: string; company: string; persona: DemoPersona['key'] | ''; personas: DemoPersona[] }

const cookieHeaders = { 'X-Requested-With': 'fetch' }

/** Halaman pertama setelah berganti peran: tempat persona itu punya sesuatu untuk dilakukan. */
export const personaLanding: Record<DemoPersona['key'], string> = { payroll: '/payroll', owner: '/approvals', employee: '/payslips' }

export const useDemo = defineStore('demo', {
  state: () => ({ available: null as boolean | null, ttlHours: 24, sandbox: null as Sandbox | null }),
  getters: {
    current: (s) => s.sandbox?.personas.find((p) => p.key === s.sandbox?.persona) ?? null,
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
      } catch (e) {
        this.sandbox = null
        if (!(e instanceof ApiError && e.status === 404)) throw e
      }
    },
    clear() {
      this.sandbox = null
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
