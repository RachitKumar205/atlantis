import { afterEach, describe, expect, it, vi } from 'vitest'

import { fetchAuthConfig } from './api'

// Stubs global fetch with one canned response.
function stubFetch(status: number, body: unknown, opts: { json?: boolean } = {}) {
  const json = opts.json ?? true
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => ({
      ok: status >= 200 && status < 300,
      status,
      json: async () => {
        if (!json) throw new SyntaxError('not json')
        return body
      },
    })),
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('fetchAuthConfig', () => {
  it('returns the providers the deployment configured', async () => {
    stubFetch(200, { providers: ['github', 'google'] })
    await expect(fetchAuthConfig()).resolves.toEqual({ providers: ['github', 'google'] })
  })

  // The server sends `[]` and never `null` for an unconfigured deployment, but
  // the page branches on length and a wrong shape here is a blank sign-in
  // screen with no error — so it is normalised rather than trusted.
  it('treats a missing or malformed list as no providers', async () => {
    for (const body of [{}, { providers: null }, { providers: 'github' }]) {
      stubFetch(200, body)
      await expect(fetchAuthConfig()).resolves.toEqual({ providers: [] })
    }
  })

  // The API's error strings are written to be shown to a person, so they are
  // surfaced rather than replaced with something generic.
  it('surfaces the API error message', async () => {
    stubFetch(429, { error: 'too many requests — try again shortly' })
    await expect(fetchAuthConfig()).rejects.toThrow('too many requests — try again shortly')
  })

  it('falls back to the status when there is no message', async () => {
    stubFetch(500, {})
    await expect(fetchAuthConfig()).rejects.toThrow('500')
  })

  // A JSON route answering with something that is not JSON means something else
  // replied — a proxy error page, or the SPA itself if a route went missing.
  // That must not surface as "undefined is not an object".
  it('reports a non-JSON body as an unexpected response', async () => {
    stubFetch(502, null, { json: false })
    await expect(fetchAuthConfig()).rejects.toThrow('unexpected 502')
  })
})
