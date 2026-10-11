<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { api } from '../api'
import { formatMoney } from '../access'

// Biaya gaji (net) per departemen untuk versi terkini run (Y6s). Satu seri besaran → bar horizontal satu warna,
// label nilai langsung (tanpa legenda), tooltip saat hover/fokus, dan tampilan tabel. Nama departemen dari
// GET /departments: endpoint ringkasan sengaja tidak meng-join tabel milik modul org.
const props = defineProps<{ runId: string; calcVersion: number }>()

type Row = { department_id: string | null; employees: number; gross: string; deductions: string; net: string }
type Unit = { id: string; name: string }
const rows = ref<Row[]>([])
const names = ref<Record<string, string>>({})
const asTable = ref(false)
const hover = ref<number | null>(null)

async function load() {
  try {
    // Departemen nonaktif ikut dibaca: run lama tetap menampilkan nama departemen saat perhitungan.
    const [sum, active, inactive] = await Promise.all([
      api<{ by_department: Row[] }>(`/payroll-runs/${props.runId}/summary`),
      api<{ data: Unit[] }>('/departments?status=ACTIVE'),
      api<{ data: Unit[] }>('/departments?status=INACTIVE'),
    ])
    rows.value = sum.by_department
    names.value = Object.fromEntries([...active.data, ...inactive.data].map((d) => [d.id, d.name]))
  } catch {
    rows.value = []
  }
}
watch(() => [props.runId, props.calcVersion], load, { immediate: true })

const num = (v: string) => Number(v) // tampilan saja (lebar bar); angka uang tetap ditampilkan dari string
const max = computed(() => Math.max(1, ...rows.value.map((r) => num(r.net))))
const label = (r: Row) => (r.department_id ? (names.value[r.department_id] ?? 'Departemen') : 'Tanpa departemen')
const share = (r: Row) => {
  const total = rows.value.reduce((a, x) => a + num(x.net), 0)
  return total > 0 ? Math.round((num(r.net) / total) * 100) : 0
}
</script>

<template>
  <section v-if="rows.length" class="card stack dept-chart" aria-labelledby="dept-chart-title" data-test="dept-chart">
    <div class="card-head flush">
      <h2 id="dept-chart-title">Biaya gaji per departemen</h2>
      <button type="button" class="link-btn" @click="asTable = !asTable">{{ asTable ? 'Lihat grafik' : 'Lihat tabel' }}</button>
    </div>

    <ul v-if="!asTable" class="bars" role="list">
      <li v-for="(r, i) in rows" :key="r.department_id ?? 'none'" class="bar-row" tabindex="0"
        :aria-label="`${label(r)}: ${formatMoney(r.net)} untuk ${r.employees} karyawan, ${share(r)} persen dari total`"
        @pointerenter="hover = i" @pointerleave="hover = null" @focus="hover = i" @blur="hover = null">
        <span class="bar-name">{{ label(r) }}<span class="bar-sub">{{ r.employees }} karyawan</span></span>
        <span class="bar-track">
          <span class="bar" :style="{ '--f': Math.max(0.02, num(r.net) / max) }" />
          <span class="bar-value">{{ formatMoney(r.net) }}</span>
        </span>
        <span v-if="hover === i" class="tip" role="tooltip">
          <strong>{{ formatMoney(r.net) }}</strong> net · {{ share(r) }}% dari total<br />
          Bruto {{ formatMoney(r.gross) }} · potongan {{ formatMoney(r.deductions) }}
        </span>
      </li>
    </ul>

    <div v-else class="table-scroll">
      <table class="table">
        <thead><tr><th scope="col">Departemen</th><th scope="col" class="right">Karyawan</th><th scope="col" class="right">Bruto</th><th scope="col" class="right">Potongan</th><th scope="col" class="right">Net</th></tr></thead>
        <tbody>
          <tr v-for="r in rows" :key="r.department_id ?? 'none'">
            <td>{{ label(r) }}</td><td class="right">{{ r.employees }}</td><td class="right">{{ formatMoney(r.gross) }}</td>
            <td class="right">{{ formatMoney(r.deductions) }}</td><td class="right"><strong>{{ formatMoney(r.net) }}</strong></td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>

<style scoped>
.card-head.flush { padding: 0; }
.bars { list-style: none; padding: 0; margin: 0; display: flex; flex-direction: column; gap: 14px; }
.bar-row { position: relative; display: grid; grid-template-columns: minmax(110px, 180px) 1fr; gap: 16px; align-items: center; border-radius: 10px; outline-offset: 4px; }
.bar-name { font-size: 14px; font-weight: 600; color: var(--text); display: flex; flex-direction: column; }
.bar-sub { font-size: 12px; font-weight: 500; color: var(--muted); }
.bar-track { display: flex; align-items: center; gap: 10px; min-width: 0; }
/* Mark tipis, ujung data membulat 4px, menempel di baseline (kiri). Lebar = porsi dari ruang SETELAH label
   nilai (--label), supaya label bar terbesar tidak terdorong keluar kartu. */
.bar-track { --label: 112px; }
.bar { display: block; height: 18px; width: calc((100% - var(--label)) * var(--f)); background: var(--blue); border-radius: 0 4px 4px 0; transition: width .4s ease; flex-shrink: 0; }
.bar-row:hover .bar, .bar-row:focus .bar { background: var(--blue-dark); }
.bar-value { font-size: 13px; font-weight: 700; color: var(--text); white-space: nowrap; font-variant-numeric: tabular-nums; }
.tip { position: absolute; left: min(196px, 40%); top: calc(100% + 6px); z-index: 5; padding: 8px 10px; border-radius: 10px; background: var(--card);
  border: 1px solid var(--border-strong); box-shadow: 0 6px 20px rgba(15, 23, 42, .12); font-size: 12px; color: var(--text-2); line-height: 1.5; pointer-events: none; }
.tip strong { color: var(--text); }
@media (max-width: 768px) {
  .bar-row { grid-template-columns: 1fr; gap: 6px; }
  .tip { left: 0; }
}
</style>
