// defineConfig comes from vitest/config, not vite, so the `test` block below
// type-checks. It is vite's own defineConfig widened with vitest's options —
// everything else in this file behaves exactly as before.
import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'node:url'

// Route tree is maintained manually in src/routeTree.gen.ts.

export default defineConfig({
  // One config rather than a separate vitest.config.ts, so tests resolve the
  // `@` alias below through the same rules the app builds with. A second
  // config file is a second place for that alias to drift.
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
  },
  plugins: [react(), tailwindcss()],
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
