// Which screen the sign-in application is on, and what moves it.
//
// Every decision here is a plain function with a unit test, following the
// precedent web/console/src/lib/session.ts set: the same choices lived as
// inline booleans in components first, where they were invisible when wrong and
// unreachable from a test without a DOM and a router.
//
// The server owns the sequence. `POST /api/auth/login` answers `next`, and so
// does `GET /api/auth/pending` after a reload — this module maps that onto a
// screen rather than deciding for itself what comes after what. Anything it
// decided independently would be a second copy of the flow, free to disagree
// with the one that actually gates access.

export type Screen =
  | 'organisations'
  | 'signin'
  | 'signup'
  | 'check-email'
  | 'enrol'
  | 'verify'
  | 'backup-codes'
  | 'reset-request'
  | 'reset-sent'

/** The `next` values the server sends. Anything else is not understood. */
export type Next = 'enrol' | 'verify'

/**
 * screenForNext maps the server's `next` onto a screen.
 *
 * An unrecognised value returns null rather than guessing. Guessing would mean
 * showing somebody the enrolment screen when the server wanted a code, which
 * looks like the app is broken in a way that has nothing to do with the cause;
 * a null lets the caller say so plainly.
 */
export function screenForNext(next: string): Screen | null {
  switch (next) {
    case 'enrol':
      return 'enrol'
    case 'verify':
      return 'verify'
    default:
      return null
  }
}

/**
 * arrivalScreen decides where a fresh page load starts.
 *
 * `pending` comes from GET /api/auth/pending, which exists because the pending
 * cookie is HttpOnly and a reloaded page cannot read it. Without this a
 * half-finished sign-in shows a sign-in form as though nothing had happened —
 * survivable on the password path, where a password can be retyped, and not on
 * the OAuth path, where the whole provider round trip has to be repeated.
 *
 * `next` from the query string is how an OAuth callback hands over
 * (/signin?next=enrol). It is only trusted when the server also says a pending
 * login exists: the query string is attacker-supplied and a pending login is
 * not, so the cookie decides whether there is a flow at all and the query
 * string only ever refines a flow the server already confirmed.
 */
export function arrivalScreen(pending: { pending: boolean; next?: string }, fromQuery?: string | null): Screen {
  if (!pending.pending) return 'signin'

  const fromServer = pending.next ? screenForNext(pending.next) : null
  if (fromServer) return fromServer

  // The server said a login is in progress but not what it needs. The query
  // string is the only remaining hint, and it is safe to use here precisely
  // because the pending login has already been confirmed.
  const hinted = fromQuery ? screenForNext(fromQuery) : null
  return hinted ?? 'verify'
}

/**
 * canLeaveBackupCodes gates the one screen that shows a secret exactly once.
 *
 * The codes are generated, hashed, stored and returned in a single response;
 * there is no route that shows them again. Navigating away without reading them
 * leaves an account whose backup codes nobody has — recoverable only by
 * regenerating, which is a route W6 adds and W4 does not have.
 *
 * So leaving is deliberate: the person has to say they have saved them.
 */
export function canLeaveBackupCodes(acknowledged: boolean): boolean {
  return acknowledged
}

/**
 * retryAfterMessage turns a 429 into something a person can act on.
 *
 * The limiter is five requests per minute per IP across the whole of sign-in
 * (ratelimit.go), which one honest person can reach by mistyping a six-digit
 * code twice and then clicking through — and a whole office shares behind one
 * NAT. "Too many requests" with no number reads as a fault; a wait reads as a
 * wait.
 */
export function retryAfterMessage(retryAfter: string | null): string {
  const seconds = Number(retryAfter)
  if (!Number.isFinite(seconds) || seconds <= 0) {
    return 'Too many attempts. Wait a moment and try again.'
  }
  if (seconds < 60) {
    return `Too many attempts. Try again in ${Math.ceil(seconds)} seconds.`
  }
  const minutes = Math.ceil(seconds / 60)
  return `Too many attempts. Try again in ${minutes} minute${minutes === 1 ? '' : 's'}.`
}

/**
 * isSessionGone reports whether a failure means the sign-in has to restart.
 *
 * One rule rather than a branch per screen. A 401 from anywhere in the flow
 * means the pending login or the session is gone — expired, spent, or replaced
 * — and every screen answers it the same way: back to the start, saying why.
 */
export function isSessionGone(status: number): boolean {
  return status === 401
}
