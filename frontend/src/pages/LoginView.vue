<script setup lang="ts">
import { errorText } from '../errors'
import { onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppIcon from '../components/AppIcon.vue'
import AuthLayout from '../components/AuthLayout.vue'
import { useAuth } from '../stores/auth'
import { safeNext } from '../router'
import { personaLanding, useDemo } from '../stores/demo'

const email = ref('')
const password = ref('')
const showPassword = ref(false)
const error = ref('')
const busy = ref(false)
const auth = useAuth()
const route = useRoute()
const router = useRouter()
const demo = useDemo()
const demoBusy = ref(false)
const demoError = ref('')

onMounted(() => demo.detect())

async function startDemo() {
  demoBusy.value = true
  demoError.value = ''
  try {
    await demo.start()
    router.push(personaLanding.payroll)
  } catch (e) {
    demoError.value = (e as Error).message
  } finally {
    demoBusy.value = false
  }
}

async function submit() {
  busy.value = true
  error.value = ''
  try {
    await auth.login(email.value, password.value)
    // Belum ada tenant aktif (punya beberapa perusahaan, atau belum terverifikasi) → pilih dulu.
    if (auth.pendingInvite) router.push('/invite') // kembali ke undangan yang sedang dibuka
    else router.push(auth.tenant ? safeNext(route.query.next) : '/select-tenant')
  } catch (e) {
    error.value = errorText(e, 'Terjadi kesalahan. Coba lagi.')
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <AuthLayout>
    <form class="form" @submit.prevent="submit">
      <section v-if="demo.available" class="demo-cta" aria-labelledby="demo-title">
        <span class="demo-cta-badge"><AppIcon name="spark" :size="14" :stroke="2.2" />Tanpa daftar</span>
        <h2 id="demo-title">Coba PayRun sekarang</h2>
        <p>Kami siapkan <strong>PT Kopi Nusantara</strong> untukmu: 15 karyawan, payroll bulan lalu yang sudah final, dan tiga peran yang bisa kamu coba bergantian. Terhapus sendiri dalam {{ demo.ttlHours }} jam.</p>
        <button type="button" class="btn block" data-test="start-demo" :disabled="demoBusy" @click="startDemo">
          <AppIcon name="play" :size="18" :stroke="2" />{{ demoBusy ? 'Menghitung gaji bulan lalu…' : 'Coba demo' }}
        </button>
        <p v-if="demoError" class="alert error" role="alert">{{ demoError }}</p>
      </section>
      <p v-if="demo.available" class="or-divider"><span>atau masuk dengan akunmu</span></p>
      <div v-else>
        <h2>Masuk</h2>
        <p class="lead">Masukkan email dan kata sandi akun PayRun-mu.</p>
      </div>
      <div class="field">
        <label for="email">Email</label>
        <input id="email" v-model="email" class="input" type="email" autocomplete="username" required />
      </div>
      <div class="field">
        <label for="password">Kata sandi</label>
        <div class="input-wrap">
          <input id="password" v-model="password" class="input" :type="showPassword ? 'text' : 'password'" autocomplete="current-password" required />
          <button type="button" class="icon-btn bare" :aria-label="showPassword ? 'Sembunyikan sandi' : 'Tampilkan sandi'" @click="showPassword = !showPassword">
            <AppIcon :name="showPassword ? 'eyeOff' : 'eye'" />
          </button>
        </div>
      </div>
      <p v-if="error" class="alert error" role="alert">{{ error }}</p>
      <button type="submit" class="btn block" :disabled="busy">Masuk</button>
    </form>
  </AuthLayout>
</template>
