import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import { fileURLToPath, URL } from 'node:url'

// The Atlantis Cloud sign-in application.
//
// Deliberately NOT a copy of web/console's config. Two settings differ because
// this origin holds every password, TOTP secret and the assertion signing key,
// and its Content-Security-Policy is correspondingly tighter — see spaCSP in
// internal/cloud/server/server.go.
export default defineConfig({
  // One config rather than a separate vitest.config.ts, so tests resolve the
  // `@` alias below through the same rules the app builds with.
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
  },
  plugins: [react()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    // The Go binary (cmd/cloud) embeds the SPA from this directory, but only
    // when built with `-tags embedspa`. See cmd/cloud/spa_none.go.
    outDir: '../../cmd/cloud/dist',
    emptyOutDir: true,

    // No data: URIs, at any size.
    //
    // Vite inlines any imported asset under 4 KB as a base64 data: URI by
    // default (DEFAULT_ASSETS_INLINE_LIMIT). The policy here is `img-src
    // 'self'` and `font-src 'self'` with no `data:`, so a small SVG or a
    // subsetted woff2 would be silently blocked by the browser — and would work
    // perfectly in `vite dev`, which never sees the policy at all.
    //
    // Zero, so every asset is a file with a URL.
    assetsInlineLimit: 0,
  },
  server: {
    // In dev, proxy API calls to `make dev-auth`. The console's equivalent
    // points at :3000; Cloud listens on :9500.
    //
    // NOTE: nothing in this dev server enforces the production CSP. A style
    // that `style-src 'self'` will reject renders perfectly here. The policy is
    // only exercised by a tagged binary serving the built app.
    proxy: {
      '/api': { target: 'http://localhost:9500', changeOrigin: true },
      '/auth': { target: 'http://localhost:9500', changeOrigin: true },
      '/authorize': { target: 'http://localhost:9500', changeOrigin: true },
    },
  },
})
