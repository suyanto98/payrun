<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api, ApiError, newIdempotencyKey } from '../api'
import AppIcon from '../components/AppIcon.vue'
import { adjustmentComponentLabel, adjustmentStatusLabel, adjustmentStatusPill, formatMoney, formatPeriod } from '../access'
import type { Adjustment, AdjustmentComponent, EmployeeListItem, Page } from '../types'

// J1–J3 (PF-19): koreksi periode yang sudah FINAL, masuk ke run periode target sebagai baris "Koreksi".
// Menunggu run (PENDING_APPLY) masih bisa dibatalkan; setelah run target final → Diterapkan, tidak bisa diubah.
const employees = ref<EmployeeListItem[]>([])
const list = ref<Adjustment[]>([])
const page = ref<Page>({ next_cursor: null, has_more: false })
const status = ref('')
const error = ref('')
const notice = ref('')
const busy = ref(false)

const now = new Date()
const month = (d: Date) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}`
const form = ref({
  employee_id: '', origin_period: month(new Date(now.getFullYear(), now.getMonth() - 1, 1)), target_period: month(now),
  component_code: 'OVERTIME_CORRECTION' as AdjustmentComponent, amount: '', deduct: false, taxable: true, reason: '',
})
let submitKey = newIdempotencyKey()
watch(form, () => { submitKey = newIdempotencyKey() }, { deep: true })

const empName = computed(() => Object.fromEntries(employees.value.map((e) => [e.id, `${e.full_name} (${e.employee_no})`])))

async function load(append = false) {
  error.value = ''
  try {
    const params = new URLSearchParams({ limit: '20' })
    if (status.value) params.set('status', status.value)
    if (append && page.value.next_cursor) params.set('cursor', page.value.next_cursor)
    const res = await api<{ data: Adjustment[]; page: Page }>('/adjustments?' + params)
    list.value = append ? [...list.value, ...res.data] : res.data
    page.value = res.page
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Gagal memuat adjustment'
  }
}

async function loadEmployees() {
  try {
    employees.value = (await api<{ data: EmployeeListItem[] }>('/employees?limit=100')).data
  } catch { /* daftar karyawan opsional untuk label */ }
}

async function submit() {
  busy.value = true
  error.value = ''
  notice.value = ''
  const f = form.value
  const amount = (f.deduct ? '-' : '') + f.amount.trim().replace(',', '.')
  try {
    const a = await api<Adjustment>('/adjustments', { method: 'POST', idempotencyKey: submitKey, body: {
      employee_id: f.employee_id, origin_period: f.origin_period, target_period: f.target_period,
      component_code: f.component_code, amount, taxable: f.taxable, reason: f.reason.trim() } })
    notice.value = `Tercatat: ${formatMoney(a.amount.replace('-', ''))} ${a.amount.startsWith('-') ? 'potongan' : 'tambahan'} di payroll ${formatPeriod(a.target_period)}.`
    form.value = { ...form.value, amount: '', reason: '' }
    await load()
  } catch (e) {
    if (e instanceof ApiError && e.code === 'PERIOD_LOCKED') error.value = `Payroll ${formatPeriod(f.target_period)} sudah final. Pilih periode target berikutnya.`
    else if (e instanceof ApiError && e.code === 'EMPLOYEE_NOT_ACTIVE') error.value = 'Karyawan tidak bekerja di periode target.'
    else error.value = e instanceof ApiError ? (e.fields[0] ? `${e.fields[0].field}: ${e.fields[0].message ?? e.fields[0].code}` : e.message) : 'Gagal menyimpan'
  } finally {
    busy.value = false
  }
}

const cancelling = ref<string | null>(null)
const cancelReason = ref('')
async function cancel(a: Adjustment) {
  busy.value = true
  error.value = ''
  try {
    await api(`/adjustments/${a.id}/cancel`, { method: 'POST', body: { reason: cancelReason.value.trim() } })
    cancelling.value = null
    cancelReason.value = ''
    await load()
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Gagal membatalkan'
  } finally {
    busy.value = false
  }
}

watch(status, () => load())
onMounted(() => Promise.all([load(), loadEmployees()]))
</script>

<template>
  <div class="stack">
    <div class="page-head">
      <div>
        <h1>Adjustment</h1>
        <p class="sub">Payroll yang sudah final tidak diubah. Kekurangan atau kelebihan bayar dikoreksi di payroll bulan berikutnya.</p>
      </div>
    </div>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <p v-if="notice" class="note" role="status"><AppIcon name="check" />{{ notice }}</p>

    <form class="card form" @submit.prevent="submit">
      <h2>Catat koreksi</h2>
      <div class="row-wrap">
        <div class="field wide">
          <label for="emp">Karyawan</label>
          <select id="emp" v-model="form.employee_id" class="select" required>
            <option value="" disabled>Pilih karyawan</option>
            <option v-for="e in employees" :key="e.id" :value="e.id">{{ e.full_name }} ({{ e.employee_no }})</option>
          </select>
        </div>
        <div class="field">
          <label for="comp">Jenis</label>
          <select id="comp" v-model="form.component_code" class="select">
            <option v-for="(label, code) in adjustmentComponentLabel" :key="code" :value="code">{{ label }}</option>
          </select>
        </div>
      </div>
      <div class="row-wrap">
        <div class="field"><label for="origin">Periode yang dikoreksi (sudah final)</label><input id="origin" v-model="form.origin_period" class="input" type="month" required /></div>
        <div class="field"><label for="target">Dibayar/dipotong di payroll</label><input id="target" v-model="form.target_period" class="input" type="month" :min="form.origin_period" required /></div>
      </div>
      <div class="row-wrap">
        <div class="field">
          <label for="amount">Jumlah (Rp)</label>
          <input id="amount" v-model="form.amount" class="input num" inputmode="decimal" pattern="[0-9]{1,10}([.,][0-9]{1,2})?" required placeholder="76300.58" />
        </div>
        <fieldset class="field choice">
          <legend>Arah</legend>
          <label><input v-model="form.deduct" type="radio" :value="false" /> Tambahan</label>
          <label><input v-model="form.deduct" type="radio" :value="true" /> Potongan</label>
        </fieldset>
        <label class="field check"><input v-model="form.taxable" type="checkbox" /> Kena pajak (masuk bruto kena PPh)</label>
      </div>
      <div class="field"><label for="reason">Alasan</label><input id="reason" v-model="form.reason" class="input" maxlength="500" required placeholder="contoh: lembur 2 jam belum tercatat" /></div>
      <button type="submit" class="btn" :disabled="busy">Simpan</button>
    </form>

    <div class="card card-flush">
      <div class="card-head">
        <h2>Daftar adjustment</h2>
        <select v-model="status" class="select sm" aria-label="Filter status">
          <option value="">Semua</option>
          <option v-for="(label, code) in adjustmentStatusLabel" :key="code" :value="code">{{ label }}</option>
        </select>
      </div>
      <div class="table-scroll">
        <table class="table">
          <thead>
            <tr><th scope="col">Karyawan</th><th scope="col">Koreksi</th><th scope="col" class="right">Jumlah</th><th scope="col">Status</th><th scope="col"><span class="sr-only">Aksi</span></th></tr>
          </thead>
          <tbody>
            <tr v-if="list.length === 0"><td colspan="5" class="empty">Belum ada adjustment.</td></tr>
            <template v-for="a in list" :key="a.id">
              <tr>
                <td>{{ empName[a.employee_id] ?? a.employee_id.slice(0, 8) }}</td>
                <td>
                  {{ adjustmentComponentLabel[a.component_code] }} {{ formatPeriod(a.origin_period) }} → {{ formatPeriod(a.target_period) }}
                  <div class="muted">{{ a.reason }}<template v-if="!a.taxable"> · tidak kena pajak</template></div>
                </td>
                <td class="right num" :class="a.amount.startsWith('-') ? 'out' : 'in'">{{ a.amount.startsWith('-') ? '−' : '' }}{{ formatMoney(a.amount.replace('-', '')) }}</td>
                <td>
                  <span class="pill" :class="adjustmentStatusPill[a.status]">{{ adjustmentStatusLabel[a.status] }}</span>
                  <RouterLink v-if="a.applied_run_id" :to="`/payroll/${a.applied_run_id}`" class="muted"> run</RouterLink>
                </td>
                <td class="right">
                  <button v-if="a.status === 'PENDING_APPLY'" type="button" class="btn-row" @click="cancelling = cancelling === a.id ? null : a.id">Batalkan</button>
                </td>
              </tr>
              <tr v-if="cancelling === a.id">
                <td colspan="5">
                  <form class="inline" @submit.prevent="cancel(a)">
                    <input v-model="cancelReason" class="input sm" maxlength="500" required placeholder="Alasan pembatalan" aria-label="Alasan pembatalan" />
                    <button type="submit" class="btn sm" :disabled="busy">Batalkan adjustment</button>
                  </form>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
      </div>
      <div v-if="page.has_more" class="table-foot"><button type="button" class="btn sm ghost" @click="load(true)">Muat lagi</button></div>
    </div>
  </div>
</template>

<style scoped>
.row-wrap .field { flex: 1 1 200px; }
.row-wrap .field.wide { flex: 2 1 280px; }
.choice { border: 0; padding: 0; margin: 0; display: flex; gap: 12px; align-items: center; flex-wrap: wrap; }
.choice legend { font-size: 13px; font-weight: 600; margin-bottom: 6px; width: 100%; }
.check { display: flex; gap: 8px; align-items: center; flex-direction: row; }
.inline { display: flex; flex-wrap: wrap; gap: 8px; }
.inline .input { flex: 1 1 260px; }
</style>
