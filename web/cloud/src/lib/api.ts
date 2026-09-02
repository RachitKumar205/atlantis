// The Cloud API, as this application sees it.
//
// Same origin in production — the Go binary serves both the page and the
// routes — and through the Vite dev proxy in development. Either way the paths
// are relative, so nothing here needs to know which it is.

/**
 * ApiError carries what the screens have to decide on.
 *
 * `status` because one rule covers the whole flow: a 401 anywhere means the
 * pending login or the session is gone and the person starts again (see
 * isSessionGone). `retryAfter` because the sign-in limiter is shared across
 * every route and five per minute is reachable by an honest person, so "wait
 * this long" is worth saying precisely.
 */
import { normaliseMe, normaliseOrg, type Me, type Org } from './orgs'

export class ApiError extends Error {
  readonly status: number
  readonly retryAfter: string | null

  constructor(message: string, status: number, retryAfter: string | null = null) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.retryAfter = retryAfter
  }
}

export type AuthConfig = {
  /** OAuth providers this deployment has credentials for. May be empty. */
  providers: string[]
}

export type PendingState = {
  pending: boolean
  next?: string
}

export type LoginResult = {
  /** "enrol" or "verify" — what this account has to do next. */
  next: string
}

export type EnrolStart = {
  /** The shared secret, for typing in by hand. */
  secret: string
  /** The otpauth:// URI, for the QR code. */
  uri: string
}

export type EnrolResult = {
  backupCodes: string[]
  /** True when finishing enrolment also completed a sign-in. */
  signedIn: boolean
}

/**
 * What the sign-in screen needs before anybody has signed in.
 *
 * Unauthenticated: the page must decide whether to draw a "Continue with
 * GitHub" button before there is a session to ask about.
 */
export async function fetchAuthConfig(): Promise<AuthConfig> {
  const res = await request('GET', '/api/auth/config')
  return { providers: Array.isArray(res.providers) ? (res.providers as string[]) : [] }
}

/**
 * Whether a half-finished sign-in is in progress.
 *
 * The pending cookie is HttpOnly, so a reloaded page has no other way to ask.
 * Answers 200 either way — "nobody is signing in" is a normal state, and a 401
 * would make a first visit look like a failure.
 */
export async function fetchPending(): Promise<PendingState> {
  const res = await request('GET', '/api/auth/pending')
  return {
    pending: res.pending === true,
    next: typeof res.next === 'string' ? res.next : undefined,
  }
}

/** Creates an account. Answers the same way whether or not the address is new. */
export async function signUp(
  email: string,
  password: string,
  firstName: string,
  lastName: string,
): Promise<string> {
  const res = await request('POST', '/api/auth/signup', {
    email,
    password,
    first_name: firstName,
    last_name: lastName,
  })
  return typeof res.message === 'string' ? res.message : 'Check your email.'
}

/**
 * Verifies a password. Never returns a session — only a second factor does.
 */
export async function signIn(email: string, password: string): Promise<LoginResult> {
  const res = await request('POST', '/api/auth/login', { email, password })
  return { next: typeof res.next === 'string' ? res.next : '' }
}

/** Mints an unconfirmed TOTP secret. Safe to call again; it replaces the last one. */
export async function beginEnrolment(): Promise<EnrolStart> {
  const res = await request('POST', '/api/auth/2fa/enrol/begin', {})
  return {
    secret: typeof res.secret === 'string' ? res.secret : '',
    uri: typeof res.uri === 'string' ? res.uri : '',
  }
}

/**
 * Confirms enrolment and returns the backup codes.
 *
 * Whether this also completes a sign-in depends on how it was reached — during
 * a sign-in it does, from an existing session it does not — and the server
 * decides. The message is what says so, which is why it is read rather than
 * assumed.
 */
export async function finishEnrolment(code: string): Promise<EnrolResult> {
  const res = await request('POST', '/api/auth/2fa/enrol/finish', { code })
  return {
    backupCodes: Array.isArray(res.backup_codes) ? (res.backup_codes as string[]) : [],
    signedIn: res.signed_in === true,
  }
}

/** Presents a TOTP code or a backup code, completing the sign-in. */
export async function verifySecondFactor(code: string): Promise<void> {
  await request('POST', '/api/auth/2fa/verify', { code })
}

/** Asks for a password-reset link. Answers identically whether or not the address exists. */
export async function requestPasswordReset(email: string): Promise<string> {
  const res = await request('POST', '/api/auth/reset/request', { email })
  return typeof res.message === 'string' ? res.message : 'Check your email.'
}

/**
 * The signed-in account and its organisations.
 *
 * Also how the application discovers it is signed in at all. Everything else it
 * boots on answers for a pre-session state, so before this route existed there
 * was no way to ask — which is why /organisations rendered the sign-in form.
 *
 * A 401 here is not an error to show. It means "not signed in", which is an
 * answer, and the caller turns it into the sign-in screen.
 */
export async function fetchMe(): Promise<Me> {
  return normaliseMe(await request('GET', '/api/account/me'))
}

/** Creates an organisation and queues it for provisioning. */
export async function createOrg(name: string, displayName: string): Promise<Org> {
  return normaliseOrg(await request('POST', '/api/orgs', { name, display_name: displayName }))
}

/**
 * Deletes an organisation, with the retention window the server decides.
 *
 * confirm carries the organisation's own name and the server compares it there.
 * Sending it is not a formality the client could skip: without a match the
 * route answers 400 and nothing is deleted.
 */
export async function deleteOrg(name: string, confirm: string): Promise<Org> {
  return normaliseOrg(
    await request('POST', `/api/orgs/${encodeURIComponent(name)}/delete`, { confirm }),
  )
}

/** Brings a deleted organisation back, while it still can be. */
export async function restoreOrg(name: string): Promise<Org> {
  return normaliseOrg(await request('POST', `/api/orgs/${encodeURIComponent(name)}/restore`))
}

/** Reads one organisation, for a screen waiting on it to come up. */
export async function fetchOrg(name: string): Promise<Org> {
  return normaliseOrg(await request('GET', `/api/orgs/${encodeURIComponent(name)}`))
}

/**
 * Ends the session.
 *
 * The organisations screen is the first page in this application somebody stays
 * on, so it is the first that needs a way out. Without it, arriving there is a
 * room with no door.
 */
export async function signOut(): Promise<void> {
  await request('POST', '/api/auth/logout')
}

/**
 * request performs a same-origin JSON call and normalises the failure.
 *
 * The API answers errors as `{"error": "..."}` with a non-2xx status, and those
 * messages are written to be shown to a person — so they are surfaced rather
 * than replaced with something generic.
 */
async function request(
  method: string,
  path: string,
  body?: unknown,
): Promise<Record<string, unknown>> {
  const res = await fetch(path, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
    // Cookies are how both the session and the half-finished login travel, and
    // both are HttpOnly. same-origin rather than include: this app never calls
    // anywhere else, and include would send them if it ever did.
    credentials: 'same-origin',
  })

  const retryAfter = res.headers?.get ? res.headers.get('Retry-After') : null

  let parsed: Record<string, unknown> = {}
  try {
    parsed = (await res.json()) as Record<string, unknown>
  } catch {
    // A non-JSON body from a JSON route means something upstream answered
    // instead — a proxy error page, or the SPA itself if a route went missing.
    if (!res.ok) throw new ApiError(`unexpected ${res.status} response`, res.status, retryAfter)
    throw new ApiError('unreadable response', res.status, retryAfter)
  }

  if (!res.ok) {
    const message =
      typeof parsed.error === 'string' ? parsed.error : `request failed (${res.status})`
    throw new ApiError(message, res.status, retryAfter)
  }
  return parsed
}

/**
 * Looks up a `tide login` code for the approval page. A miss costs the grant
 * one of its five attempts, which is what makes the short code unguessable in
 * practice.
 */
export async function cliLookup(userCode: string): Promise<{
  caller: string
  hostname: string
  address: string
  agent: string
  expires_at: string
}> {
  const res = await request('POST', '/api/cli/lookup', { user_code: userCode })
  return {
    caller: typeof res.caller === 'string' ? res.caller : '',
    hostname: typeof res.hostname === 'string' ? res.hostname : '',
    address: typeof res.address === 'string' ? res.address : '',
    agent: typeof res.agent === 'string' ? res.agent : '',
    expires_at: typeof res.expires_at === 'string' ? res.expires_at : '',
  }
}

/** Records the decision on a `tide login` code. */
export async function cliDecide(userCode: string, org: string, approve: boolean): Promise<void> {
  await request('POST', '/api/cli/decide', { user_code: userCode, org, approve })
}
