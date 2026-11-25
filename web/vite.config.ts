import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// Served under /ui/ by remote-client serve (web/embed.go); `GET /` on the
// local endpoint redirects here for browsers. Dev proxies to a running
// serve on the default port; the /ui/api surfaces (settings, login,
// doctor) answer with the CSRF token the real server injects, so against
// `vite dev` only /v1 and /api/v1/models work fully.
export default defineConfig({
  base: '/ui/',
  plugins: [vue()],
  server: {
    port: 5174,
    proxy: {
      '/v1': 'http://127.0.0.1:4141',
      '/api': 'http://127.0.0.1:4141',
    },
  },
  build: { outDir: 'dist', emptyOutDir: true },
})
