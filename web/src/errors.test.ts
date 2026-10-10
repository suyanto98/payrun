import { describe, expect, it } from 'vitest'
import { ApiError } from './api'
import { errorText } from './errors'

describe('errorText', () => {
  it('maps a catalogue code to the Indonesian message, not the server detail', () => {
    expect(errorText(new ApiError(409, 'PERIOD_LOCKED', 'Payroll period is finalized'), 'x')).toBe('Periode payroll ini sudah final dan terkunci.')
  })
  it('names the first invalid field for VALIDATION_ERROR', () => {
    const e = new ApiError(400, 'VALIDATION_ERROR', 'Validation failed', [{ field: 'limit', code: 'OUT_OF_RANGE', message: 'must be 1..100' }])
    expect(errorText(e, 'x')).toBe('Periksa kembali isian form. (limit: must be 1..100)')
  })
  it('falls back to the server message for an unknown code, and to the fallback for non-API errors', () => {
    expect(errorText(new ApiError(418, 'BRAND_NEW_CODE', 'server says hi'), 'x')).toBe('server says hi')
    expect(errorText(new TypeError('Failed to fetch'), 'Gagal memuat')).toBe('Gagal memuat')
  })
})
