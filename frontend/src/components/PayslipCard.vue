<script setup lang="ts">
import { computed, ref } from 'vue'
import { api, ApiError } from '../api'
import AppIcon from './AppIcon.vue'
import { formatDate, formatMoney, formatPeriod, pdfStatusLabel, pdfStatusPill } from '../access'
import type { Payslip, PayslipDownload } from '../types'

// Slip web (PF-15): dibaca dari baris payroll, bukan dari PDF. Unduhan PDF (S5): URL bertanda tangan
// berlaku 5 menit, langsung dipakai lalu dibuang (tidak disimpan, tidak ditampilkan).
const props = defineProps<{ slip: Payslip }>()
const earnings = computed(() => props.slip.lines.filter((l) => l.category === 'EARNING'))
const deductions = computed(() => props.slip.lines.filter((l) => l.category === 'DEDUCTION'))
const error = ref('')
const busy = ref(false)

async function download() {
  busy.value = true
  error.value = ''
  try {
    const d = await api<PayslipDownload>(`/payslips/${props.slip.id}/download`)
    window.location.assign(d.url)
  } catch (e) {
    error.value = e instanceof ApiError && e.code === 'PAYSLIP_NOT_READY'
      ? 'PDF masih diproses. Coba lagi sebentar lagi.'
      : e instanceof ApiError ? e.message : 'Gagal menyiapkan unduhan'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <article class="card slip" aria-live="polite">
    <header class="slip-head">
      <div>
        <h2>Slip gaji {{ formatPeriod(slip.period) }}</h2>
        <p class="muted">
          {{ slip.employee.full_name }} · <span class="mono">{{ slip.employee.employee_no }}</span>
          <template v-if="slip.employee.position"> · {{ slip.employee.position }}</template>
          <template v-if="slip.employee.department"> · {{ slip.employee.department }}</template>
        </p>
        <p v-if="slip.bank_account_masked" class="muted"><AppIcon name="bank" :size="14" /> {{ slip.bank_account_masked }}</p>
      </div>
      <div class="slip-actions">
        <span class="pill" :class="pdfStatusPill[slip.pdf_status]">{{ pdfStatusLabel[slip.pdf_status] }}</span>
        <button type="button" class="btn sm" :disabled="busy || slip.pdf_status !== 'RENDERED'" @click="download">
          <AppIcon name="download" :size="16" />Unduh PDF
        </button>
      </div>
    </header>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>

    <section v-for="group in [{ title: 'Pendapatan', lines: earnings }, { title: 'Potongan', lines: deductions }]" :key="group.title">
      <h3>{{ group.title }}</h3>
      <table class="table lines">
        <tbody>
          <tr v-if="group.lines.length === 0"><td colspan="2" class="empty">—</td></tr>
          <tr v-for="l in group.lines" :key="l.sort_order">
            <td>
              <strong>{{ l.name }}</strong>
              <span v-if="l.adjustment_id" class="pill">koreksi</span>
              <div class="muted">{{ l.explanation }}</div>
            </td>
            <td class="right num" :class="l.category === 'DEDUCTION' ? 'out' : 'in'">
              {{ l.category === 'DEDUCTION' ? '−' : '' }}{{ formatMoney(l.amount) }}
            </td>
          </tr>
        </tbody>
      </table>
    </section>

    <dl class="totals">
      <div><dt>Total pendapatan</dt><dd class="num">{{ formatMoney(slip.totals.gross) }}</dd></div>
      <div><dt>Total potongan</dt><dd class="num">−{{ formatMoney(slip.totals.deductions) }}</dd></div>
      <div class="net"><dt>Gaji bersih</dt><dd class="num">{{ formatMoney(slip.totals.net) }}</dd></div>
    </dl>
    <p class="muted small">Ruleset {{ slip.ruleset.code }} {{ slip.ruleset.version }} · difinalisasi {{ formatDate(slip.finalized_at) }}</p>
  </article>
</template>

<style scoped>
.slip { display: flex; flex-direction: column; gap: 12px; }
.slip-head { display: flex; flex-wrap: wrap; justify-content: space-between; gap: 12px; }
.slip-head h2 { margin: 0 0 4px; }
.slip-head p { margin: 0; }
.slip-actions { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
h3 { font-size: 14px; margin: 4px 0; color: var(--muted); }
.lines { min-width: 0; }
.totals { display: grid; gap: 6px; margin: 0; border-top: 2px solid var(--border-strong); padding-top: 10px; }
.totals div { display: flex; justify-content: space-between; gap: 12px; }
.totals dt { color: var(--muted); }
.totals dd { margin: 0; }
.totals .net dt, .totals .net dd { font-weight: 800; color: inherit; font-size: 18px; }
.small { font-size: 12px; }
</style>
