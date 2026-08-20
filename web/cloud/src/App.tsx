import { useEffect, useState } from 'react'

import { fetchAuthConfig, type AuthConfig } from '@/lib/api'

// W3 is plumbing, not screens. This page exists to prove four things end to
// end, and it is replaced wholesale by the sign-in screens in W4:
//
//   1. The Go binary serves the built application (`-tags embedspa`).
//   2. A client route that is not a file reaches it rather than 404ing.
//   3. The API is reachable from the page — same origin in production, through
//      the Vite proxy in development.
//   4. The page renders under `style-src 'self'`, using no inline styles.
//
// Point 4 is the one a test cannot make. It only fails in a real browser
// against a tagged binary, so it is in the walkthrough.
export function App() {
  const [config, setConfig] = useState<AuthConfig | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    fetchAuthConfig().then(setConfig).catch((e: unknown) => {
      setError(e instanceof Error ? e.message : String(e))
    })
  }, [])

  return (
    <div className="shell">
      <main className="card">
        <h1 className="wordmark">atlantis cloud</h1>
        <p className="muted">Sign-in is not built yet — this is W3&rsquo;s plumbing check.</p>

        <p className={error ? 'status status--bad' : 'status'}>
          {error
            ? `api unreachable: ${error}`
            : config
              ? `api ok · providers: ${config.providers.length ? config.providers.join(', ') : 'none configured'}`
              : 'contacting the api…'}
        </p>
      </main>
    </div>
  )
}
