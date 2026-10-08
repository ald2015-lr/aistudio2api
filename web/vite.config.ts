import tailwindcss from '@tailwindcss/vite'
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'
import { fileURLToPath, URL } from 'node:url'

const serviceTarget = 'http://127.0.0.1:2048'

// defineConfig 定义前端构建和本地联调入口
export default defineConfig({
  plugins: [vue(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    outDir: fileURLToPath(new URL('../internal/webui/dist', import.meta.url)),
    emptyOutDir: true,
    rolldownOptions: {
      output: {
        // 图表库单独成块：只被懒加载的用量页引用，不进入主包；用量页代码更新后浏览器仍可复用缓存的图表库
        codeSplitting: {
          groups: [{ name: 'echarts', test: /[\\/]node_modules[\\/](echarts|zrender)[\\/]/ }],
        },
      },
    },
    // 按需引入的图表库单块约 560 kB，只在打开用量页时下载；主包远低于此上限，超过时仍会提示
    chunkSizeWarningLimit: 600,
  },
  server: {
    proxy: {
      '/api': serviceTarget,
      '/health': serviceTarget,
      '/v1': serviceTarget,
      '/v1beta': serviceTarget,
    },
  },
})
