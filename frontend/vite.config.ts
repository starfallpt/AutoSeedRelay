import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// https://vite.dev/config/
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: {
      // 后端 dev 服务（Go Gin）默认监听 9020
      '/api': {
        target: 'http://localhost:9020',
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: 'dist',
  },
})
