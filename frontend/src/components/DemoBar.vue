<script setup lang="ts">
import { computed, ref } from 'vue'
import AppIcon from './AppIcon.vue'
import { personaLanding, tour, tourLink, useDemo, type DemoPersona } from '../stores/demo'

// Bar mode demo: siapa aku sekarang, ganti peran, langkah tur untuk peran ini (dengan "kenapa penting" dalam
// bahasa HR), progres lintas peran, dan rangkuman di akhir. Tujuannya: pengunjung awam memahami APA yang hebat,
// bukan hanya bisa mengklik.
const demo = useDemo()
const busy = ref('')
const error = ref('')
// Mobile: panduan tertutup dulu supaya isi halaman langsung terlihat.
const open = ref(typeof window === 'undefined' || window.innerWidth > 768)
const contactUrl = import.meta.env.VITE_CONTACT_URL as string | undefined
const repoUrl = import.meta.env.VITE_REPO_URL as string | undefined

const rtf = new Intl.RelativeTimeFormat('id-ID', { numeric: 'auto' })
const expiresIn = computed(() => {
  if (!demo.sandbox) return ''
  const mins = Math.max(0, Math.round((new Date(demo.sandbox.expires_at).getTime() - Date.now()) / 60000))
  return mins >= 60 ? rtf.format(Math.round(mins / 60), 'hour') : rtf.format(mins, 'minute')
})
const mine = computed(() => tour.filter((t) => t.persona === demo.current?.key))
const others = computed(() =>
  (demo.sandbox?.personas ?? [])
    .filter((p) => p.key !== demo.current?.key)
    .map((p) => ({ ...p, left: tour.filter((t) => t.persona === p.key && !demo.done.includes(t.key)).length }))
    .filter((p) => p.left > 0),
)
const doneCount = computed(() => demo.done.length)
const isDone = (k: (typeof tour)[number]['key']) => demo.done.includes(k)

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

    <template v-if="open">
      <div class="tour-progress" data-test="tour-progress">
        <span><strong>{{ doneCount }} dari {{ tour.length }}</strong> langkah dicoba</span>
        <span class="tour-track" role="progressbar" :aria-valuenow="doneCount" aria-valuemin="0" :aria-valuemax="tour.length"><span :style="{ width: (doneCount / tour.length) * 100 + '%' }" /></span>
        <button type="button" class="link-btn" data-test="show-summary" @click="demo.showSummary = true">Lihat rangkuman</button>
      </div>
      <ol class="demo-steps">
        <li v-for="s in mine" :key="s.key" :class="{ done: isDone(s.key) }" :data-test="`tour-${s.key}`">
          <RouterLink :to="tourLink(s.key, demo.sandbox.links)">
            <span class="demo-step-no"><AppIcon v-if="isDone(s.key)" name="check" :size="14" :stroke="3" /><template v-else>{{ tour.indexOf(s) + 1 }}</template></span>
            <span>
              <strong>{{ s.title }}</strong>
              <span class="demo-step-hint">{{ s.hint }}</span>
              <span class="demo-step-why"><b>Kenapa penting:</b> {{ s.why }}</span>
            </span>
          </RouterLink>
        </li>
      </ol>
      <p v-if="others.length" class="next-roles">
        Lanjutkan sebagai
        <template v-for="(o, i) in others" :key="o.key">
          <span v-if="i > 0"> atau </span>
          <button type="button" class="link-btn" :disabled="busy !== ''" @click="switchTo(o.key)">{{ o.name.split(' ')[0] }} ({{ o.left }} langkah)</button>
        </template>
      </p>
    </template>
    <p v-if="error" class="alert error" role="alert">{{ error }}</p>

    <!-- Rangkuman: muncul otomatis saat semua langkah dicoba, atau lewat "Lihat rangkuman". -->
    <div v-if="demo.showSummary" class="summary" role="region" aria-labelledby="summary-title" data-test="demo-summary">
      <h2 id="summary-title">{{ demo.finished ? 'Kamu sudah mencoba semuanya.' : 'Yang ditunjukkan demo ini' }}</h2>
      <ol class="summary-points">
        <li><strong>Gaji dihitung otomatis, bisa dicek ulang, dan harus disetujui orang kedua</strong> sebelum final.</li>
        <li><strong>Server mati di tengah perhitungan pun tidak membuat gaji dobel atau hilang.</strong></li>
        <li><strong>Gaji yang sudah final terkunci.</strong> Setiap perubahan berlaku ke depan dan tercatat, siap diaudit.</li>
      </ol>
      <p class="summary-note">Di balik layar: data setiap perusahaan dipisah di tingkat database, dan setiap janji di atas diuji otomatis setiap kali kodenya berubah.</p>
      <div class="row-wrap">
        <a v-if="contactUrl" :href="contactUrl" class="btn sm" target="_blank" rel="noopener" data-test="summary-contact">Hubungi pembuatnya</a>
        <a v-if="repoUrl" :href="repoUrl" class="btn sm ghost" target="_blank" rel="noopener">Lihat kode</a>
        <button type="button" class="btn sm ghost" @click="demo.showSummary = false">Tutup</button>
      </div>
    </div>
  </section>
</template>
