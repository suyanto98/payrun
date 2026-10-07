import { defineStore } from 'pinia'
import type { ActiveTenant, Membership } from '../types'

// Endpoint cookie (refresh/logout) wajib X-Requested-With: fetch (CSRF guard, C-08).
const cookieHeaders: Record<string, string> = { 'X-Requested-With': 'fetch' }
// Tenant terakhir per perangkat: dikirim sebagai X-Active-Tenant saat refresh supaya token baru tetap
// ber-`tid` yang sama (ADR-P02). Bukan rahasia (server memeriksa membership), jadi boleh di storage.
const tenantKey = 'pr_active_tenant'

function storedTenant(): string {
  try { return localStorage.getItem(tenantKey) ?? '' } catch { return '' }
}
function storeTenant(id: string) {
  try { if (id) localStorage.setItem(tenantKey, id); else localStorage.removeItem(tenantKey) } catch { /* mode privat */ }
}

export class AuthError extends Error {}

export const useAuth = defineStore('auth', {
  state: () => ({
    token: '' as string,
    email: '' as string,
    displayName: '' as string,
    emailVerified: true,
    tenant: null as ActiveTenant | null,
    employeeId: null as string | null,
    ready: false,
  }),
  getters: {
    loggedIn: (s) => s.token !== '',
    roles: (s) => s.tenant?.roles ?? [],
  },
  actions: {
    async login(email: string, password: string) {
      const res = await fetch('/api/v1/auth/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email, password }),
      })
      const data = await res.json().catch(() => ({}))
      if (!res.ok) {
        throw new AuthError(data.code === 'RATE_LIMITED' ? 'Terlalu banyak percobaan, coba lagi nanti' : 'Email atau kata sandi salah')
      }
      await this.useSession(data.access_token)
      // Login memilih tenant otomatis hanya kalau user punya tepat satu; selain itu pakai yang terakhir.
      if (!this.tenant && storedTenant()) await this.switchTenant(storedTenant()).catch(() => storeTenant(''))
    },
    async useSession(token: string) {
      this.token = token
      await this.loadMe()
    },
    /** /me: identitas, tenant aktif (dari `tid`), dan employee milik user di tenant itu. */
    async loadMe() {
      const res = await fetch('/api/v1/me', { headers: { Authorization: `Bearer ${this.token}` } })
      if (!res.ok) return
      const me = await res.json()
      this.email = me.email
      this.displayName = me.display_name
      this.emailVerified = me.email_verified
      this.tenant = me.active_tenant
      this.employeeId = me.employee_id ?? null
      if (this.tenant) storeTenant(this.tenant.id)
    },
    async memberships(): Promise<Membership[]> {
      const res = await fetch('/api/v1/me/memberships', { headers: { Authorization: `Bearer ${this.token}` } })
      if (!res.ok) return []
      return (await res.json()).data
    },
    async switchTenant(tenantId: string) {
      const res = await fetch('/api/v1/session/switch-tenant', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${this.token}` },
        body: JSON.stringify({ tenant_id: tenantId }),
      })
      const data = await res.json().catch(() => ({}))
      if (!res.ok) throw new AuthError(data.detail ?? 'Tidak bisa masuk ke perusahaan ini')
      storeTenant(tenantId)
      await this.useSession(data.access_token)
    },
    /** Memulihkan sesi dari cookie refresh HttpOnly (saat aplikasi dibuka / token kedaluwarsa). */
    async refresh(): Promise<boolean> {
      const headers = { ...cookieHeaders }
      const tid = this.tenant?.id ?? storedTenant()
      if (tid) headers['X-Active-Tenant'] = tid
      const res = await fetch('/api/v1/auth/refresh', { method: 'POST', headers })
      if (!res.ok) {
        this.token = ''
        return false
      }
      await this.useSession((await res.json()).access_token)
      return true
    },
    async logout() {
      await fetch('/api/v1/auth/logout', { method: 'POST', headers: cookieHeaders })
      this.$reset()
      this.ready = true
    },
  },
})
