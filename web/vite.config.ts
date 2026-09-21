import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8081',
        changeOrigin: true,
      },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    globals: true,
    // 强制钉死 NODE_ENV=test，不继承宿主环境。
    //
    // 原因: React 19 的 production 构建不导出 act，而 @testing-library/react 的
    // act 回退链最终会调用 React.act。若宿主预设 NODE_ENV=production(不少 CI 与
    // 构建机都会)，vitest 会解析到 react.production.js，整套测试将以
    // "React.act is not a function" 集体失败(实测 82 个测试挂掉 63 个)，
    // 并让带 set -e 的 build.sh 在第一步就中断。
    env: { NODE_ENV: 'test' },
  },
})
