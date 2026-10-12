<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import AppIcon from '../components/AppIcon.vue'
import { useAuth } from '../stores/auth'
import { personaLanding, useDemo } from '../stores/demo'

// Landing publik: pengunjung tanpa sesi yang membuka "/" → halaman ini (router). Untuk HR/orang awam dulu, lalu
// satu bagian ringkas untuk tim teknis. Tautan repo hanya tampil kalau VITE_REPO_URL diisi saat build.
const auth = useAuth()
const demo = useDemo()
const router = useRouter()
const busy = ref(false)
const error = ref('')
const repoUrl = import.meta.env.VITE_REPO_URL as string | undefined
const videoUrl = import.meta.env.VITE_VIDEO_URL as string | undefined // video demo 2 menit (docs/demo-video-script.md)

onMounted(() => demo.detect())

async function tryDemo() {
  busy.value = true
  error.value = ''
  try {
    await demo.start()
    router.push(personaLanding.payroll)
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    busy.value = false
  }
}

const promises = [
  { icon: 'building', q: 'Data gaji perusahaan lain bocor?', a: 'Tidak bisa. Setiap perusahaan dipagari di tingkat database, bukan hanya di tampilan. Satu query yang lupa filter pun tetap hanya melihat perusahaannya sendiri.' },
  { icon: 'refresh', q: 'Server mati saat gaji dihitung?', a: 'Bagian yang terhenti dihitung ulang dari awal. Hasilnya tetap satu slip per karyawan, tidak dobel dan tidak hilang.' },
  { icon: 'lock', q: 'Gaji final diubah diam-diam?', a: 'Tidak bisa. Periode yang sudah final terkunci; koreksi tercatat sebagai penyesuaian di bulan berikutnya.' },
] as const

const roles = [
  { who: 'Dina · admin payroll', what: 'Menghitung dan mengajukan payroll. Tidak bisa menyetujui pengajuannya sendiri.' },
  { who: 'Pak Hendra · owner', what: 'Menyetujui payroll dan cuti dari kotak approval.' },
  { who: 'Rizky · karyawan', what: 'Melihat dan mengunduh slip gaji, mengajukan cuti.' },
]
</script>

<template>
  <div class="welcome">
    <header class="w-nav">
      <span class="brand"><span class="brand-mark"><AppIcon name="logo" :stroke="2.2" /></span>PayRun</span>
      <RouterLink :to="auth.loggedIn ? '/' : '/login'" class="w-login">{{ auth.loggedIn ? 'Buka aplikasi' : 'Masuk' }}</RouterLink>
    </header>

    <section class="w-hero">
      <div class="w-hero-text">
        <span class="w-badge"><AppIcon name="spark" :size="14" :stroke="2.2" />Proyek portofolio · bisa dicoba</span>
        <h1>Payroll yang bisa diaudit, untuk banyak perusahaan sekaligus.</h1>
        <p>Absensi, cuti, lembur, hitung gaji, persetujuan, sampai slip gaji. Coba sebagai HR sebuah kedai kopi dengan 15 karyawan: perusahaannya kami siapkan dalam beberapa detik, tanpa daftar.</p>
        <div class="w-cta">
          <button v-if="demo.available" type="button" class="btn" data-test="welcome-demo" :disabled="busy" @click="tryDemo">
            <AppIcon name="play" :size="18" :stroke="2" />{{ busy ? 'Menghitung gaji bulan lalu…' : 'Coba demo' }}
          </button>
          <RouterLink to="/login" class="btn ghost">Masuk</RouterLink>
        </div>
        <a v-if="videoUrl" :href="videoUrl" class="w-video" target="_blank" rel="noopener"><AppIcon name="play" :size="16" :stroke="2" />Tidak sempat mencoba? Tonton video 2 menit</a>
        <p v-if="error" class="alert error" role="alert">{{ error }}</p>
      </div>
      <div class="w-roles" aria-label="Tiga peran yang bisa dicoba bergantian">
        <span class="w-roles-title">Tiga peran, satu klik untuk berganti</span>
        <div v-for="r in roles" :key="r.who" class="w-role"><strong>{{ r.who }}</strong><span>{{ r.what }}</span></div>
      </div>
    </section>

    <section class="w-problems" aria-labelledby="w-problems-title">
      <h2 id="w-problems-title">Yang tidak boleh terjadi di urusan gaji</h2>
      <div class="w-grid">
        <article v-for="p in promises" :key="p.q" class="card w-card">
          <span class="w-icon"><AppIcon :name="p.icon" :size="20" :stroke="2" /></span>
          <h3>{{ p.q }}</h3>
          <p>{{ p.a }}</p>
        </article>
      </div>
    </section>

    <section class="w-problems" aria-labelledby="w-recruiter-title">
      <h2 id="w-recruiter-title">Untuk recruiter: apa yang ditunjukkan proyek ini tentang pembuatnya</h2>
      <div class="w-grid">
        <article class="card w-card"><h3>Memahami urusan HR, bukan hanya kode</h3><p>Persetujuan dua orang, periode gaji yang terkunci, koreksi di bulan berikutnya, slip yang bisa dilihat karyawan sendiri.</p></article>
        <article class="card w-card"><h3>Membuktikan, bukan sekadar mengklaim</h3><p>Setiap janji di halaman ini bisa kamu coba sendiri di demo, dan dijalankan ulang otomatis oleh ratusan test setiap kali kodenya berubah.</p></article>
        <article class="card w-card"><h3>Siap untuk banyak pelanggan sekaligus</h3><p>Satu sistem melayani banyak perusahaan dengan data yang terpisah ketat, dan sudah diuji untuk 10.000 karyawan dalam satu perusahaan.</p></article>
      </div>
    </section>

    <section class="card w-tech" aria-labelledby="w-tech-title">
      <h2 id="w-tech-title">Untuk tim teknis</h2>
      <ul>
        <li><strong>Multi-tenant pooled:</strong> PostgreSQL Row-Level Security dengan <code>FORCE</code>; tenant dari token, disetel per transaksi; role aplikasi tanpa <code>BYPASSRLS</code>.</li>
        <li><strong>Mesin payroll asinkron:</strong> run dibagi per chunk dengan lease dan fencing; chunk yang crash diulang tanpa menulis dua kali.</li>
        <li><strong>Final itu final:</strong> period guard + change log; setiap slip bisa dihitung ulang dari snapshot input dan ruleset-nya.</li>
        <li><strong>Maker-checker:</strong> approver dipilih server; matriks otorisasi per role diuji untuk setiap endpoint.</li>
        <li><strong>Diuji dan dapat diamati:</strong> test integrasi dengan PostgreSQL sungguhan, invariant checker di setiap test, metrik dan alert yang tertaut ke runbook.</li>
      </ul>
      <a v-if="repoUrl" :href="repoUrl" class="link-btn" target="_blank" rel="noopener">Lihat kode dan dokumen desainnya →</a>
    </section>

    <footer class="w-foot">PayRun adalah proyek portofolio. Perusahaan, karyawan, dan gaji di demo adalah data contoh; aturan pajak disederhanakan.</footer>
  </div>
</template>

<style scoped>
.welcome { min-height: 100dvh; background: var(--bg); padding: 0 24px 40px; display: flex; flex-direction: column; gap: 32px; }
.w-nav { max-width: 1120px; width: 100%; margin: 0 auto; display: flex; justify-content: space-between; align-items: center; padding: 20px 0 0; }
.w-nav .brand { color: var(--navy); }
.w-nav .brand-mark { background: var(--navy); color: #fff; }
.w-login { font-weight: 700; text-decoration: none; }
.w-hero { max-width: 1120px; width: 100%; margin: 0 auto; background: var(--navy); color: #fff; border-radius: 28px; padding: 48px; display: grid; grid-template-columns: 1.4fr 1fr; gap: 40px; align-items: center; }
.w-hero h1 { font-size: 42px; line-height: 1.12; letter-spacing: -1px; font-weight: 800; margin: 14px 0 12px; }
.w-hero p { color: var(--on-navy-muted); font-size: 16px; line-height: 1.6; max-width: 560px; }
.w-badge { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; font-weight: 700; padding: 5px 12px; border-radius: 999px; background: rgba(255, 255, 255, .12); }
.w-cta { display: flex; gap: 12px; flex-wrap: wrap; margin-top: 24px; }
.w-video { display: inline-flex; align-items: center; gap: 8px; margin-top: 16px; color: #fff; font-size: 14px; font-weight: 700; }
.w-cta .btn.ghost { background: transparent; color: #fff; border-color: rgba(255, 255, 255, .35); }
.w-roles { padding: 20px; border-radius: 20px; background: rgba(255, 255, 255, .08); display: flex; flex-direction: column; gap: 14px; }
.w-roles-title { font-size: 12px; font-weight: 700; text-transform: uppercase; letter-spacing: .5px; color: var(--on-navy-muted); }
.w-role { display: flex; flex-direction: column; gap: 2px; font-size: 14px; line-height: 1.45; }
.w-role span { color: var(--on-navy-muted); }
.w-problems, .w-tech, .w-foot { max-width: 1120px; width: 100%; margin: 0 auto; }
.w-problems h2, .w-tech h2 { font-size: 22px; font-weight: 800; letter-spacing: -.3px; margin-bottom: 16px; }
.w-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 16px; }
.w-card { display: flex; flex-direction: column; gap: 10px; }
.w-card h3 { font-size: 16px; font-weight: 700; }
.w-card p { font-size: 14px; color: var(--text-2); line-height: 1.55; }
.w-icon { width: 40px; height: 40px; border-radius: 12px; background: var(--blue-tint); color: var(--blue); display: flex; align-items: center; justify-content: center; }
.w-tech ul { margin: 0 0 12px; padding-left: 20px; display: flex; flex-direction: column; gap: 8px; font-size: 14px; line-height: 1.55; color: var(--text-2); }
.w-tech code { font-size: 13px; }
.w-foot { font-size: 12px; color: var(--muted); text-align: center; }
@media (max-width: 860px) {
  .welcome { padding: 0 16px 32px; gap: 24px; }
  .w-hero { grid-template-columns: 1fr; padding: 28px 22px; border-radius: 22px; gap: 24px; }
  .w-hero h1 { font-size: 30px; }
  .w-grid { grid-template-columns: 1fr; }
}
</style>
