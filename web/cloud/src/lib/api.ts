// The Cloud API, as this application sees it.
//
// Same origin in production — the Go binary serves both the page and the
// routes — and through the Vite dev proxy in development. Either way the paths
// are relative, so nothing here needs to know which it is.

export type AuthConfig = {
  /** OAuth providers this deployment has credentials for. May be empty. */
  providers: string[]
}

/**
 * What the sign-in screen needs before anybody has signed in.
 *
 * Unauthenticated: the page must decide whether to draw a "Continue with
 * GitHub" button before there is a session to ask about.
 */
export async function fetchAuthConfig(): Promise<AuthConfig> {
  const res = await request('/api/auth/config')
  return { providers: Array.isArray(res.providers) ? (res.providers as string[]) : [] }
}

/**
 * request performs a same-origin JSON call and normalises the failure.
 *
 * The API answers errors as `{"error": "..."}` with a non-2xx status, and those
 * messages are written to be shown to a person — so they are surfaced rather
 * than replaced with something generic.
 */
async function request(path: string, init?: RequestInit): Promise<Record<string, unknown>> {
  const res = await fetch(path, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) },
    // Cookies are how both the session and the half-finished login travel, and
    // both are HttpOnly. same-origin rather than include: this app never calls
    // anywhere else, and include would send them if it ever did.
    credentials: 'same-origin',
  })

  let body: Record<string, unknown> = {}
  try {
    body = (await res.json()) as Record<string, unknown>
  } catch {
    // A non-JSON body from a JSON route means something upstream answered
    // instead — a proxy error page, or the SPA itself if a route went missing.
    if (!res.ok) throw new Error(`unexpected ${res.status} response`)
    throw new Error('unreadable response')
  }

  if (!res.ok) {
    throw new Error(typeof body.error === 'string' ? body.error : `request failed (${res.status})`)
  }
  return body
}
