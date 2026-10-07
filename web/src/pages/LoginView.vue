<script setup lang="ts">
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppIcon from '../components/AppIcon.vue'
import AuthLayout from '../components/AuthLayout.vue'
import { useAuth } from '../stores/auth'
import { safeNext } from '../router'

const email = ref('')
const password = ref('')
const showPassword = ref(false)
const error = ref('')
const busy = ref(false)
const auth = useAuth()
const route = useRoute()
const router = useRouter()

async function submit() {
  busy.value = true
  error.value = ''
  try {
    await auth.login(email.value, password.value)
    // Belum ada tenant aktif (punya beberapa perusahaan, atau belum terverifikasi) → pilih dulu.
    if (auth.pendingInvite) router.push('/invite') // kembali ke undangan yang sedang dibuka
    else router.push(auth.tenant ? safeNext(route.query.next) : '/select-tenant')
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <AuthLayout>
    <form class="form" @submit.prevent="submit">
      <div>
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
