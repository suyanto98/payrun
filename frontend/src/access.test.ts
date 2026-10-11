import { describe, expect, it } from 'vitest'
import { canEditEmployees, canListEmployees, canReadPayroll, canRunPayroll, formatDate, formatMoney, formatPeriod, runActions } from './access'

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
    // Timestamp (finalized_at): tanggal WIB. 20:30 UTC = 03:30 WIB hari berikutnya.
    expect(formatDate('2026-10-11T03:12:45.123Z')).toBe('11 Okt 2026')
    expect(formatDate('2026-10-11T20:30:00Z')).toBe('12 Okt 2026')
  })
  it('formats money strings without floats', () => {
    expect(formatMoney('6600000.00')).toBe('Rp6.600.000')
    expect(formatMoney('20000.50')).toBe('Rp20.000,50')
    expect(formatMoney(null)).toBe('—')
  })
  it('mirrors Y3/Y5 of the authorization matrix (HR reads payroll, does not run it)', () => {
    expect(canReadPayroll(['HR_ADMIN'])).toBe(true)
    expect(canRunPayroll(['HR_ADMIN'])).toBe(false)
    expect(canRunPayroll(['PAYROLL_ADMIN'])).toBe(true)
    expect(canReadPayroll(['MANAGER', 'EMPLOYEE'])).toBe(false)
  })
  it('offers only the transitions the run state machine allows', () => {
    expect(runActions('CALCULATED')).toEqual({ calculate: true, submit: true, cancel: true })
    expect(runActions('PENDING_APPROVAL')).toEqual({ calculate: false, submit: false, cancel: false })
    expect(runActions('CALCULATING')).toEqual({ calculate: false, submit: false, cancel: false })
    expect(runActions('DRAFT')).toEqual({ calculate: true, submit: false, cancel: true })
  })
  it('formats payroll periods', () => {
    expect(formatPeriod('2026-03')).toBe('Maret 2026')
  })
})
