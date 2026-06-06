import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { fileURLToPath, URL } from 'node:url'

// Route tree is maintained manually in src/routeTree.gen.ts.

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    // The Go binary (cmd/console) embeds the SPA from this directory.
    outDir: '../../cmd/console/dist',
    emptyOutDir: true,
  },
  server: {
    // In dev, proxy API calls to the Go BFF.
    proxy: {
      '/api': {
        target: 'http://localhost:3000',
        changeOrigin: true,
      },
    },
  },
})
