import { describe, expect, it } from 'vitest'
import { canEditEmployees, canListEmployees, formatDate } from './access'

describe('access', () => {
  it('mirrors W1/W2 of the authorization matrix', () => {
    expect(canListEmployees(['EMPLOYEE'])).toBe(false)
    expect(canListEmployees(['MANAGER', 'EMPLOYEE'])).toBe(true)
    expect(canListEmployees(['PAYROLL_ADMIN'])).toBe(true)
    expect(canEditEmployees(['PAYROLL_ADMIN'])).toBe(false)
    expect(canEditEmployees(['HR_ADMIN'])).toBe(true)
  })
  it('formats API dates without timezone shifts', () => {
    expect(formatDate('2026-01-05')).toBe('5 Jan 2026')
    expect(formatDate('2026-12-31')).toBe('31 Des 2026')
    expect(formatDate(null)).toBe('—')
  })
})
