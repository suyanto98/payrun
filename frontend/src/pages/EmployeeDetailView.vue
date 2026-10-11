<script setup lang="ts">
import { errorText } from '../errors'
import { computed, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { api, ApiError, newIdempotencyKey } from '../api'
import { canEditEmployees, canListEmployees, formatDate, formatMoney, statusLabel } from '../access'
import { useAuth } from '../stores/auth'
import { useDemo } from '../stores/demo'
import type { Employee, SalaryHistory } from '../types'

const route = useRoute()
const auth = useAuth()
const emp = ref<Employee | null>(null)
const error = ref('')
const notFound = ref(false)

const salary = ref<SalaryHistory | null>(null)

async function load(id: string) {
  salary.value = null
  emp.value = null
  error.value = ''
  notFound.value = false
  try {
    emp.value = await api<Employee>(`/employees/${id}`)
    // W8: OWNER/HR/Payroll dan karyawan ybs; manager mendapat 404 (tidak melihat gaji bawahan) → disembunyikan.
    salary.value = await api<SalaryHistory>(`/employees/${id}/salary-history`).catch(() => null)
  } catch (e) {
    // 404 juga untuk karyawan perusahaan lain atau di luar scope (tidak dibedakan, file 17 §1.3).
    if (e instanceof ApiError && e.status === 404) notFound.value = true
    else error.value = errorText(e, 'Gagal memuat karyawan')
  }
}
watch(() => route.params.id as string, load, { immediate: true })

// Versi lengkap (OWNER/HR_ADMIN/PAYROLL_ADMIN) memuat PTKP dan PII ter-mask; versi dasar tidak.
const full = computed(() => emp.value !== null && emp.value.ptkp_status !== undefined)
const isSelf = computed(() => emp.value !== null && emp.value.id === auth.employeeId)

// W9 ubah gaji (OWNER/HR_ADMIN): berlaku mulai tanggal tertentu. Tanggal di periode yang sudah final ditolak server
// (PERIOD_LOCKED): gaji final tidak berubah diam-diam; selisih bulan lalu dicatat sebagai koreksi gaji.
const raise = ref({ open: false, rupiah: '', from: '', reason: '' })
const raiseBusy = ref(false)
const raiseError = ref('')
const raiseNotice = ref('')
let raiseKey = newIdempotencyKey()
watch(raise, () => (raiseKey = newIdempotencyKey()), { deep: true })
function openRaise() {
  const d = new Date()
  raise.value = { open: true, rupiah: '', from: `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-01`, reason: '' }
  raiseError.value = ''
  raiseNotice.value = ''
}
async function submitRaise() {
  const digits = raise.value.rupiah.replace(/\D/g, '')
  if (!digits || !emp.value) {
    raiseError.value = 'Isi gaji pokok baru dalam rupiah.'
    return
  }
  raiseBusy.value = true
  raiseError.value = ''
  try {
    await api(`/employees/${emp.value.id}/salary-changes`, {
      method: 'POST', idempotencyKey: raiseKey,
      body: { base_salary: `${digits}.00`, valid_from: raise.value.from, change_reason: raise.value.reason.trim() || undefined },
    })
    raiseNotice.value = `Gaji baru berlaku mulai ${formatDate(raise.value.from)}. Riwayat di bawah sudah diperbarui.`
    useDemo().mark('raise')
    raise.value.open = false
    salary.value = await api<SalaryHistory>(`/employees/${emp.value.id}/salary-history`).catch(() => salary.value)
  } catch (e) {
    raiseError.value = errorText(e, 'Gagal mengubah gaji')
    if (e instanceof ApiError && e.code === 'PERIOD_LOCKED') useDemo().mark('raise') // penolakan itu sendiri adalah buktinya
  } finally {
    raiseBusy.value = false
  }
}
</script>

<template>
  <div class="stack narrow">
    <p v-if="canListEmployees(auth.roles)" class="crumb"><RouterLink to="/employees">Karyawan</RouterLink> / Detail</p>
    <p v-if="notFound" class="card empty">Karyawan tidak ditemukan.</p>
    <p v-else-if="error" class="alert error" role="alert">{{ error }}</p>
    <template v-else-if="emp">
      <div class="page-head">
        <div>
          <h1>{{ emp.full_name }}</h1>
          <p class="sub"><span class="mono">{{ emp.employee_no }}</span><span v-if="isSelf"> · Profil saya</span></p>
        </div>
        <span class="pill" :class="{ ok: emp.status === 'ACTIVE', warn: emp.status === 'RESIGNED', bad: emp.status === 'TERMINATED' }">{{ statusLabel[emp.status] ?? emp.status }}</span>
      </div>
      <section class="card">
        <h2>Kepegawaian</h2>
        <dl class="dl">
          <div><dt>Email kerja</dt><dd>{{ emp.work_email ?? '—' }}</dd></div>
          <div><dt>Mulai kerja</dt><dd>{{ formatDate(emp.hire_date) }}</dd></div>
          <div v-if="emp.end_date"><dt>Hari kerja terakhir</dt><dd>{{ formatDate(emp.end_date) }}</dd></div>
          <div v-if="full"><dt>Status PTKP</dt><dd>{{ emp.ptkp_status }}</dd></div>
          <div><dt>Departemen</dt><dd>{{ emp.current_org?.department?.name ?? '—' }}</dd></div>
          <div><dt>Posisi</dt><dd>{{ emp.current_org?.position?.name ?? '—' }}</dd></div>
          <div><dt>Atasan</dt><dd>
            <RouterLink v-if="emp.current_org?.manager" :to="`/employees/${emp.current_org.manager.id}`">{{ emp.current_org.manager.full_name }}</RouterLink>
            <span v-else>—</span>
          </dd></div>
        </dl>
      </section>
      <section v-if="salary" class="card">
        <div class="card-head flush">
          <h2>Gaji</h2>
          <button v-if="canEditEmployees(auth.roles) && !raise.open" type="button" class="btn sm ghost" data-test="open-raise" @click="openRaise">Ubah gaji</button>
        </div>
        <p class="muted">Riwayat berlaku per tanggal; tanggal akhir tidak termasuk. Setiap perubahan tercatat, tidak menimpa yang lama.</p>
        <p v-if="raiseNotice" class="alert ok" role="status" data-test="raise-notice">{{ raiseNotice }}</p>
        <form v-if="raise.open" class="form raise-form" data-test="raise-form" @submit.prevent="submitRaise">
          <div class="field">
            <label for="raise-amount">Gaji pokok baru (Rp)</label>
            <input id="raise-amount" v-model="raise.rupiah" class="input" inputmode="numeric" placeholder="8.500.000" required />
          </div>
          <div class="field">
            <label for="raise-from">Berlaku mulai</label>
            <input id="raise-from" v-model="raise.from" class="input" type="date" required />
            <span class="hint">Coba tanggal di bulan yang gajinya sudah final: sistem akan menolak.</span>
          </div>
          <div class="field">
            <label for="raise-reason">Alasan (opsional)</label>
            <input id="raise-reason" v-model="raise.reason" class="input" maxlength="200" placeholder="Promosi, kenaikan tahunan, …" />
          </div>
          <p v-if="raiseError" class="alert error" role="alert" data-test="raise-error">{{ raiseError }}</p>
          <div class="row-wrap">
            <button type="submit" class="btn sm" :disabled="raiseBusy">Simpan</button>
            <button type="button" class="btn sm ghost" @click="raise.open = false">Batal</button>
          </div>
        </form>
        <p v-if="salary.salary.length === 0" class="muted">Belum ada data gaji.</p>
        <dl v-else class="dl">
          <div v-for="s in salary.salary" :key="s.id">
            <dt>{{ formatDate(s.valid_from) }} – {{ s.valid_to ? formatDate(s.valid_to) : 'sekarang' }}<span v-if="s.change_reason"> · {{ s.change_reason }}</span></dt>
            <dd class="num">{{ formatMoney(s.base_salary) }}</dd>
          </div>
        </dl>
        <template v-if="salary.components.length">
          <h2 class="sub-head">Komponen</h2>
          <dl class="dl">
            <div v-for="c in salary.components" :key="c.id">
              <dt>{{ c.component.name }} · {{ formatDate(c.valid_from) }}{{ c.valid_to ? ' – ' + formatDate(c.valid_to) : '' }}</dt>
              <dd class="num">{{ c.amount ? formatMoney(c.amount) : c.rate ?? 'nilai default' }}</dd>
            </div>
          </dl>
        </template>
      </section>
      <section v-if="full" class="card">
        <h2>Data pribadi</h2>
        <p class="muted">Disimpan terenkripsi dan selalu ditampilkan ter-mask.</p>
        <dl class="dl">
          <div><dt>NIK</dt><dd class="mono">{{ emp.nik_masked ?? '—' }}</dd></div>
          <div><dt>NPWP</dt><dd class="mono">{{ emp.npwp_masked ?? '—' }}</dd></div>
          <div><dt>Bank</dt><dd>{{ emp.bank?.name ?? '—' }}</dd></div>
          <div><dt>Nomor rekening</dt><dd class="mono">{{ emp.bank?.account_masked ?? '—' }}</dd></div>
          <div><dt>Atas nama</dt><dd>{{ emp.bank?.holder ?? '—' }}</dd></div>
        </dl>
      </section>
    </template>
    <p v-else class="muted">Memuat…</p>
  </div>
</template>

<style scoped>
.card-head.flush { padding: 0; }
.raise-form { margin: 12px 0 8px; padding: 16px; border-radius: 14px; background: var(--bg); gap: 14px; }
.sub-head { margin-top: 18px; }
</style>

