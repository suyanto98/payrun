<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { api, ApiError, newIdempotencyKey } from '../api'
import AppIcon from '../components/AppIcon.vue'
import { canRunPayroll, formatMoney, formatPeriod, runActions, runStatusLabel, runStatusPill } from '../access'
import { useAuth } from '../stores/auth'
import type { Page, PayrollItem, PayrollItemDetail, PayrollRun } from '../types'

// Y4 status + progres (polling 2 dtk selama CALCULATING), Y5 hitung, Y6/Y7 item + baris, Y8 exclude,
// Y10 submit, Y12 batal. Tombol hanya petunjuk; server menegakkan role dan state machine.
const auth = useAuth()
const route = useRoute()
const id = computed(() => String(route.params.id))
const canRun = computed(() => canRunPayroll(auth.roles))

const run = ref<PayrollRun | null>(null)
const error = ref('')
const notice = ref('')
const busy = ref(false)
const actions = computed(() => runActions(run.value?.status ?? ''))

let poll: ReturnType<typeof setTimeout> | undefined
async function loadRun() {
  clearTimeout(poll)
  try {
    run.value = await api<PayrollRun>(`/payroll-runs/${id.value}`)
    if (run.value.status === 'CALCULATING') poll = setTimeout(loadRun, 2000)
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Gagal memuat run'
  }
}
onBeforeUnmount(() => clearTimeout(poll))

// ---------------------------------------------------------------- item
const items = ref<PayrollItem[]>([])
const itemsPage = ref<Page>({ next_cursor: null, has_more: false })
const itemStatus = ref('')
const itemsLoading = ref(false)
const showItems = computed(() => !!run.value && run.value.calc_version > 0 && run.value.status !== 'CALCULATING' && run.value.status !== 'CANCELLED')

async function loadItems(append = false) {
  itemsLoading.value = true
  const params = new URLSearchParams({ limit: '50' })
  if (itemStatus.value) params.set('status', itemStatus.value)
  if (append && itemsPage.value.next_cursor) params.set('cursor', itemsPage.value.next_cursor)
  try {
    const res = await api<{ data: PayrollItem[]; page: Page }>(`/payroll-runs/${id.value}/items?` + params)
    items.value = append ? [...items.value, ...res.data] : res.data
    itemsPage.value = res.page
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Gagal memuat item'
  } finally {
    itemsLoading.value = false
  }
}
watch(itemStatus, () => loadItems())
watch(() => [run.value?.status, run.value?.calc_version], () => { if (showItems.value) loadItems() })

const detail = ref<PayrollItemDetail | null>(null)
async function openItem(it: PayrollItem) {
  try {
    detail.value = await api<PayrollItemDetail>(`/payroll-runs/${id.value}/items/${it.id}`)
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Gagal memuat item'
  }
}

const excluding = ref<string | null>(null)
const excludeReason = ref('')
async function exclude(it: PayrollItem) {
  busy.value = true
  error.value = ''
  try {
    await api(`/payroll-runs/${id.value}/items/${it.id}/exclude`, { method: 'POST', body: { reason: excludeReason.value } })
    excluding.value = null
    excludeReason.value = ''
    await Promise.all([loadRun(), loadItems()])
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Gagal mengecualikan item'
  } finally {
    busy.value = false
  }
}

// ---------------------------------------------------------------- aksi run
let calcKey = newIdempotencyKey() // klik ganda / retry = satu kalkulasi (replay 202)
async function calculate() {
  busy.value = true
  error.value = ''
  notice.value = ''
  try {
    run.value = await api<PayrollRun>(`/payroll-runs/${id.value}/calculate`, { method: 'POST', idempotencyKey: calcKey })
    calcKey = newIdempotencyKey()
    detail.value = null
    loadRun()
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Gagal memulai kalkulasi'
  } finally {
    busy.value = false
  }
}

const needReason = ref(false)
const needApprover = ref(false)
const selfReason = ref('')
const approverId = ref('')
const admins = ref<{ id: string; label: string }[]>([])
async function submit() {
  busy.value = true
  error.value = ''
  const body: Record<string, string> = {}
  if (needReason.value) body.self_approval_reason = selfReason.value
  if (needApprover.value && approverId.value) body.approver_user_id = approverId.value
  try {
    run.value = await api<PayrollRun>(`/payroll-runs/${id.value}/submit`, { method: 'POST', body })
    needReason.value = needApprover.value = false
    notice.value = 'Run diajukan. Approver melihatnya di menu Approval.'
  } catch (e) {
    if (!(e instanceof ApiError)) { error.value = 'Gagal mengajukan run'; return }
    if (e.code === 'SELF_APPROVAL_REASON_REQUIRED') {
      needReason.value = true
      error.value = 'Tidak ada admin lain yang bisa menyetujui. Tulis alasan untuk menyetujui sendiri (tercatat di audit).'
    } else if (e.code === 'VALIDATION_ERROR' && e.fields.some((f) => f.field === 'approver_user_id')) {
      needApprover.value = true
      await loadAdmins()
      error.value = 'Ada beberapa admin. Pilih siapa yang menyetujui.'
    } else if (e.code === 'RUN_HAS_ERRORS') {
      error.value = `${e.extra.error_count} item masih ERROR. Perbaiki datanya lalu hitung ulang, atau kecualikan item tersebut.`
    } else {
      error.value = e.message
    }
  } finally {
    busy.value = false
  }
}
// Daftar admin hanya bisa dibaca OWNER/HR_ADMIN (U1); PAYROLL_ADMIN mengisi lewat bantuan OWNER.
async function loadAdmins() {
  try {
    const res = await api<{ data: { user: { id: string; display_name: string } | null; roles: string[] }[] }>('/members?status=ACTIVE&limit=100')
    admins.value = res.data
      .filter((m) => m.user && m.user.id !== auth.userId && m.roles.some((r) => r === 'OWNER' || r === 'PAYROLL_ADMIN'))
      .map((m) => ({ id: m.user!.id, label: m.user!.display_name }))
  } catch {
    admins.value = []
  }
}

// Y11: satu niat = satu key, retry (respons hilang) = replay 200 yang sama.
let finalizeKey = newIdempotencyKey()
async function finalize() {
  if (!window.confirm('Finalisasi mengunci periode dan hasil payroll. Lanjutkan?')) return
  busy.value = true
  error.value = ''
  try {
    run.value = await api<PayrollRun>(`/payroll-runs/${id.value}/finalize`, { method: 'POST', idempotencyKey: finalizeKey })
    finalizeKey = newIdempotencyKey()
    notice.value = 'Payroll difinalisasi. Periode terkunci; koreksi berikutnya lewat adjustment.'
  } catch (e) {
    if (e instanceof ApiError && e.code === 'RUN_STALE') {
      const n = (e.extra.employees_changed as unknown[] | undefined)?.length ?? 0
      error.value = `Input berubah setelah approve (${n} karyawan). Tolak approval lalu hitung ulang.`
    } else {
      error.value = e instanceof ApiError ? e.message : 'Gagal memfinalisasi'
    }
  } finally {
    busy.value = false
  }
}

const cancelling = ref(false)
const cancelReason = ref('')
async function cancel() {
  busy.value = true
  error.value = ''
  try {
    run.value = await api<PayrollRun>(`/payroll-runs/${id.value}/cancel`, { method: 'POST', body: { reason: cancelReason.value } })
    cancelling.value = false
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Gagal membatalkan run'
  } finally {
    busy.value = false
  }
}

onMounted(loadRun)

const progressPct = computed(() => {
  const p = run.value?.progress
  return p && p.chunks_total > 0 ? Math.round((p.chunks_done / p.chunks_total) * 100) : 0
})
const itemPill: Record<string, string> = { OK: 'ok', ERROR: 'bad', EXCLUDED: '' }
</script>

<template>
  <div class="stack">
    <div class="page-head">
      <div>
        <p class="crumb"><RouterLink to="/payroll">Payroll</RouterLink></p>
        <h1 v-if="run">{{ formatPeriod(run.period) }}</h1>
        <p v-if="run" class="sub">
          <span class="pill" :class="runStatusPill[run.status]">{{ runStatusLabel[run.status] ?? run.status }}</span>
          · versi {{ run.calc_version }}<template v-if="run.ruleset"> · ruleset {{ run.ruleset.code }} {{ run.ruleset.version }}</template>
        </p>
      </div>
      <div v-if="run && canRun" class="btn-group">
        <button v-if="actions.calculate" type="button" class="btn sm" :disabled="busy" @click="calculate">
          <AppIcon name="refresh" />{{ run.calc_version > 0 ? 'Hitung ulang' : 'Hitung' }}
        </button>
        <button v-if="actions.submit" type="button" class="btn sm" :disabled="busy" data-test="submit" @click="submit">
          <AppIcon name="check" />Ajukan approval
        </button>
        <button v-if="run.status === 'APPROVED'" type="button" class="btn sm" :disabled="busy" data-test="finalize" @click="finalize">
          <AppIcon name="lock" />Finalisasi
        </button>
        <button v-if="actions.cancel" type="button" class="btn sm ghost" :disabled="busy" @click="cancelling = !cancelling">Batalkan</button>
      </div>
    </div>

    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
    <p v-if="notice" class="note" role="status"><AppIcon name="info" />{{ notice }}</p>

    <form v-if="needReason || needApprover" class="card form" @submit.prevent="submit">
      <div v-if="needApprover" class="field">
        <label for="approver">Approver</label>
        <select id="approver" v-model="approverId" class="select" required>
          <option value="" disabled>Pilih admin</option>
          <option v-for="a in admins" :key="a.id" :value="a.id">{{ a.label }}</option>
        </select>
        <span v-if="admins.length === 0" class="hint">Daftar admin hanya bisa dibuka Pemilik/Admin HR. Minta Pemilik mengajukan run ini.</span>
      </div>
      <div v-if="needReason" class="field">
        <label for="reason">Alasan menyetujui sendiri</label>
        <textarea id="reason" v-model="selfReason" class="textarea" rows="2" maxlength="500" required />
      </div>
      <button type="submit" class="btn sm" :disabled="busy">Ajukan</button>
    </form>

    <form v-if="cancelling" class="card form" @submit.prevent="cancel">
      <div class="field">
        <label for="cancel-reason">Alasan pembatalan</label>
        <input id="cancel-reason" v-model="cancelReason" class="input" minlength="3" maxlength="500" required />
      </div>
      <button type="submit" class="btn sm" :disabled="busy">Batalkan run</button>
    </form>

    <template v-if="run">
      <div v-if="run.status === 'CALCULATING'" class="card" aria-live="polite">
        <p class="muted">Menghitung chunk {{ run.progress.chunks_done }} dari {{ run.progress.chunks_total || '…' }}
          <template v-if="run.progress.eligible_count !== null"> · {{ run.progress.eligible_count }} karyawan</template></p>
        <div class="progress" role="progressbar" :aria-valuenow="progressPct" aria-valuemin="0" aria-valuemax="100">
          <span :style="{ width: progressPct + '%' }" />
        </div>
      </div>
      <p v-if="run.status === 'FAILED'" class="alert error">{{ run.failure_reason }}</p>
      <p v-if="run.status === 'CANCELLED'" class="note"><AppIcon name="info" />Dibatalkan: {{ run.cancel_reason }}</p>
      <p v-if="run.status === 'FINALIZED'" class="note"><AppIcon name="lock" />Final. Periode terkunci; hasil tidak bisa diubah dan bisa dihitung ulang dari snapshot.</p>
      <p v-if="run.status === 'PENDING_APPROVAL'" class="note"><AppIcon name="clock" />Menunggu keputusan approver. Hitung ulang dan pembatalan dikunci sampai diputuskan.</p>

      <div v-if="run.totals" class="row-wrap">
        <div class="balance-card stat">
          <span class="balance-label">Total net</span>
          <span class="balance">{{ formatMoney(run.totals.net) }}</span>
          <span class="balance-meta">Bruto {{ formatMoney(run.totals.gross) }} · Potongan {{ formatMoney(run.totals.deductions) }}</span>
        </div>
        <dl class="card counts">
          <div><dt>OK</dt><dd class="num">{{ run.counts.ok ?? 0 }}</dd></div>
          <div><dt>Error</dt><dd class="num">{{ run.counts.error ?? 0 }}</dd></div>
          <div><dt>Dikecualikan</dt><dd class="num">{{ run.counts.excluded ?? 0 }}</dd></div>
          <div><dt>Peringatan</dt><dd class="num">{{ run.counts.warnings ?? 0 }}</dd></div>
        </dl>
      </div>

      <div v-if="showItems" class="card card-flush">
        <div class="card-head">
          <h2>Item</h2>
          <select v-model="itemStatus" class="select sm" aria-label="Filter status item">
            <option value="">Semua</option>
            <option value="OK">OK</option>
            <option value="ERROR">Error</option>
            <option value="EXCLUDED">Dikecualikan</option>
          </select>
        </div>
        <div class="table-scroll">
          <table class="table">
            <thead>
              <tr>
                <th scope="col">Nomor</th>
                <th scope="col">Nama</th>
                <th scope="col">Status</th>
                <th scope="col" class="right">Net</th>
                <th scope="col">Catatan</th>
                <th scope="col"><span class="sr-only">Aksi</span></th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!itemsLoading && items.length === 0"><td colspan="6" class="empty">Tidak ada item.</td></tr>
              <template v-for="it in items" :key="it.id">
                <tr>
                  <td class="mono">{{ it.employee_no }}</td>
                  <td><button type="button" class="link-btn" @click="openItem(it)">{{ it.employee_name }}</button></td>
                  <td><span class="pill" :class="itemPill[it.status]">{{ it.status }}</span></td>
                  <td class="right num">{{ formatMoney(it.net) }}</td>
                  <td class="muted">{{ [...it.error_codes, ...it.warning_codes].join(', ') || it.excluded_reason || '—' }}</td>
                  <td class="right">
                    <button v-if="canRun && it.status === 'ERROR' && run.status === 'CALCULATED'" type="button" class="btn-row"
                      @click="excluding = excluding === it.id ? null : it.id">Kecualikan</button>
                  </td>
                </tr>
                <tr v-if="excluding === it.id">
                  <td colspan="6">
                    <form class="exclude" @submit.prevent="exclude(it)">
                      <input v-model="excludeReason" class="input sm" minlength="10" maxlength="500" required
                        placeholder="Alasan (min. 10 karakter), contoh: dibayar manual bulan ini" aria-label="Alasan mengecualikan" />
                      <button type="submit" class="btn sm" :disabled="busy">Kecualikan</button>
                    </form>
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>
        <div v-if="itemsPage.has_more" class="table-foot">
          <span class="muted">{{ items.length }} item dimuat</span>
          <button type="button" class="btn sm ghost" :disabled="itemsLoading" @click="loadItems(true)">Muat lagi</button>
        </div>
      </div>

      <div v-if="detail" class="card" aria-live="polite">
        <div class="card-head flush">
          <h2>{{ detail.employee_name }} <span class="muted mono">{{ detail.employee_no }}</span></h2>
          <button type="button" class="icon-btn bare" aria-label="Tutup detail" @click="detail = null"><AppIcon name="x" /></button>
        </div>
        <p v-for="is in detail.issues" :key="is.code" class="alert" :class="is.kind === 'error' ? 'error' : 'warn'">{{ is.code }}: {{ is.detail }}</p>
        <table v-if="detail.lines.length" class="table lines">
          <tbody>
            <tr v-for="l in detail.lines" :key="l.sort_order">
              <td>
                <strong>{{ l.name }}</strong>
                <div class="muted">{{ l.explanation }}</div>
              </td>
              <td class="right num" :class="l.category === 'DEDUCTION' ? 'out' : 'in'">
                {{ l.category === 'DEDUCTION' ? '−' : '' }}{{ formatMoney(l.amount) }}
              </td>
            </tr>
            <tr class="total"><td>Net</td><td class="right num">{{ formatMoney(detail.net) }}</td></tr>
          </tbody>
        </table>
      </div>
    </template>
  </div>
</template>

<style scoped>
.progress { height: 10px; border-radius: 999px; background: var(--border); overflow: hidden; margin-top: 10px; }
.progress span { display: block; height: 100%; background: var(--blue); transition: width .4s ease; }
.stat { flex: 1 1 320px; }
.counts { flex: 1 1 280px; display: grid; grid-template-columns: repeat(2, 1fr); gap: 12px; margin: 0; }
.counts dt { font-size: 12px; color: var(--muted); }
.counts dd { margin: 0; font-size: 22px; font-weight: 800; }
.exclude { display: flex; flex-wrap: wrap; gap: 8px; }
.exclude .input { flex: 1 1 280px; }
.card-head.flush { padding: 0 0 8px; }
.lines { min-width: 0; }
.lines .total td { font-weight: 800; border-top: 2px solid var(--border-strong); }
.alert.warn { background: var(--warn-tint, #fff4e0); color: var(--warn, #8a5300); }
</style>
