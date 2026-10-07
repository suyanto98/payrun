import { describe, expect, it, vi } from 'vitest'

vi.mock('./pages/LoginView.vue', () => ({ default: {} }))
vi.mock('./pages/SelectTenantView.vue', () => ({ default: {} }))
vi.mock('./pages/EmployeesView.vue', () => ({ default: {} }))
vi.mock('./pages/EmployeeDetailView.vue', () => ({ default: {} }))
vi.mock('./pages/InviteView.vue', () => ({ default: {} }))
vi.mock('vue-router', () => ({ createRouter: () => ({ beforeEach: () => {} }), createWebHistory: () => ({}) }))

const { safeNext } = await import('./router')

describe('safeNext', () => {
  it('accepts internal paths only', () => {
    expect(safeNext('/employees/1')).toBe('/employees/1')
    expect(safeNext('//evil.example/x')).toBe('/')
    expect(safeNext('https://evil.example')).toBe('/')
    expect(safeNext('/\\evil.example')).toBe('/')
    expect(safeNext(undefined)).toBe('/')
  })
})
