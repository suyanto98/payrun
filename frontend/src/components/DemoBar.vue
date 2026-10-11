<script setup lang="ts">
import { computed, ref } from 'vue'
import AppIcon from './AppIcon.vue'
import { personaLanding, useDemo, type DemoPersona } from '../stores/demo'

// Bar mode demo: siapa aku sekarang, ganti peran, dan langkah berikutnya untuk peran ini. Tujuannya membuat
// pengunjung awam (HR, recruiter) melihat alur payroll lengkap tanpa membaca dokumentasi.
const demo = useDemo()
const busy = ref('')
const error = ref('')
// Mobile: panduan tertutup dulu supaya isi halaman langsung terlihat.
const open = ref(typeof window === 'undefined' || window.innerWidth > 768)

type Step = { to: string; title: string; hint: string }
const steps: Record<DemoPersona['key'], Step[]> = {
  payroll: [
    { to: '/payroll', title: 'Lihat payroll bulan lalu', hint: 'Sudah final: dihitung, diajukan, disetujui Pak Hendra.' },
    { to: '/payroll', title: 'Buat payroll bulan ini, lalu Hitung', hint: 'Coba "Pasang gangguan" dulu: satu bagian mati di tengah, lalu diulang otomatis.' },
    { to: '/payroll', title: 'Ajukan ke Pak Hendra', hint: 'Kamu tidak bisa menyetujui pengajuanmu sendiri. Ganti peran ke owner.' },
  ],
  owner: [
    { to: '/approvals', title: 'Buka kotak approval', hint: 'Cuti Rizky (dan payroll yang diajukan Dina) menunggu keputusanmu.' },
    { to: '/employees', title: 'Lihat data karyawan', hint: 'Data perusahaan lain tidak pernah terlihat di sini.' },
  ],
  employee: [
    { to: '/payslips', title: 'Lihat slip gaji bulan lalu', hint: 'Bisa diunduh sebagai PDF.' },
    { to: '/leave', title: 'Ajukan cuti', hint: 'Masuk ke kotak approval Pak Hendra.' },
  ],
}

const rtf = new Intl.RelativeTimeFormat('id-ID', { numeric: 'auto' })
const expiresIn = computed(() => {
  if (!demo.sandbox) return ''
  const mins = Math.max(0, Math.round((new Date(demo.sandbox.expires_at).getTime() - Date.now()) / 60000))
  return mins >= 60 ? rtf.format(Math.round(mins / 60), 'hour') : rtf.format(mins, 'minute')
})
const currentSteps = computed(() => (demo.current ? steps[demo.current.key] : []))

// Muat ulang penuh: menu, hak akses, dan semua data halaman mengikuti peran baru.
async function switchTo(key: DemoPersona['key']) {
  busy.value = key
  error.value = ''
  try {
    await demo.switchTo(key)
    window.location.assign(personaLanding[key])
  } catch (e) {
    error.value = (e as Error).message
    busy.value = ''
  }
}

async function restart() {
  busy.value = 'restart'
  error.value = ''
  try {
    await demo.start()
    window.location.assign(personaLanding.payroll)
  } catch (e) {
    error.value = (e as Error).message
    busy.value = ''
  }
}
</script>

<template>
  <section v-if="demo.sandbox && demo.current" class="demo-bar" aria-label="Mode demo" data-test="demo-bar">
    <div class="demo-bar-top">
      <AppIcon name="spark" :size="18" :stroke="2" />
      <span class="demo-bar-text">
        <strong>Mode demo · {{ demo.sandbox.company }}</strong>
        <span>Kamu sebagai <strong>{{ demo.current.name }}</strong> ({{ demo.current.title }}). Data contoh terhapus {{ expiresIn }}.</span>
      </span>
      <button type="button" class="link-btn" :disabled="busy !== ''" @click="restart">{{ busy === 'restart' ? 'Menyiapkan…' : 'Mulai ulang' }}</button>
    </div>
    <div class="persona-switch" role="group" aria-label="Ganti peran">
      <span class="persona-label">Ganti peran:</span>
      <button v-for="p in demo.sandbox.personas" :key="p.key" type="button" class="persona-btn" :class="{ on: p.key === demo.current.key }"
        :aria-pressed="p.key === demo.current.key" :disabled="busy !== ''" :data-test="`persona-${p.key}`" @click="p.key !== demo.current.key && switchTo(p.key)">
        <AppIcon name="user" :size="14" :stroke="2.2" />{{ busy === p.key ? 'Beralih…' : p.name.split(' ')[0] }}
        <span class="persona-role">{{ p.title.split(' · ')[0] }}</span>
      </button>
      <button type="button" class="link-btn guide-toggle" :aria-expanded="open" @click="open = !open">{{ open ? 'Sembunyikan panduan' : 'Tampilkan panduan' }}</button>
    </div>
    <ol v-if="open" class="demo-steps">
      <li v-for="(s, i) in currentSteps" :key="s.title">
        <RouterLink :to="s.to"><span class="demo-step-no">{{ i + 1 }}</span><span><strong>{{ s.title }}</strong><span class="demo-step-hint">{{ s.hint }}</span></span></RouterLink>
      </li>
    </ol>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>
  </section>
</template>
