import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 开发模式将 API 与 WS 代理到本地 dashboard(端口见 dashboard/manifest/config/config.yaml,默认 5678)。
// 若对着容器(8000)开发,把下方 5678 改为 8000。
export default defineConfig({
  plugins: [vue()],
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:5678',
      '/ws': { target: 'ws://127.0.0.1:5678', ws: true },
    },
  },
})
