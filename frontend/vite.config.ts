/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// Dev: Vite mem-proxy /api ke API Go, sehingga SPA dan API SATU origin (http://localhost:5174; 5173
// dipakai LedgerPay). PR_WEB_PORT harus sama dengan yang dipakai `make up` (ALLOWED_ORIGINS /
// PUBLIC_BASE_URL di deploy/compose.yml), karena guard CSRF refresh memeriksa Origin.
// Cookie refresh (SameSite=Strict, Path=/api/v1/auth) ikut terkirim tanpa CORS. Produksi: image edge
// melayani SPA dan me-reverse-proxy /api dengan cara yang sama (S8, deploy ditunda).
const api = process.env.PR_API_URL ?? `http://localhost:${process.env.PR_HTTP_PORT ?? '8081'}`

export default defineConfig({
  plugins: [vue()],
  server: { port: Number(process.env.PR_WEB_PORT ?? 5174), strictPort: true, proxy: { '/api': { target: api, changeOrigin: false } } },
  test: { environment: 'node', include: ['src/**/*.test.ts'] },
})
