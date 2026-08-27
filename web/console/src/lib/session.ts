import type { MeResult } from '@/api/client'

// Two decisions the chrome makes about a session, kept out of the components
// that make them.
//
// Not for tidiness. Both are one boolean wide, both are invisible when wrong —
// a switch that appears to do nothing, a menu that is missing — and neither can
// be reached from a test without a DOM, a router and a rendered tree. Here they
// are ordinary functions, and the tests beside them are the only thing standing
// between "the condition is still right" and "the condition still compiles".

// The name Cloud gives the assertion in the redirect fragment, and the flag
// that says which of the two arrivals it is.
//
// Fragment, not query string: it never reaches a server, so it stays out of
// access logs and out of the Referer header on the next navigation.
export const ASSERTION_PARAM = 'assertion'
export const MODE_PARAM = 'mode'

// RETURN_TO_PARAM carries the path the browser was trying to reach before it
// was sent to Cloud.
//
// Not `next`: Cloud reads that one already, and it names a screen there
// (`enrol`, `verify`) rather than a path. One name for two meanings ends with
// a sign-in landing on the enrolment screen.
export const RETURN_TO_PARAM = 'return_to'

// DEFAULT_PATH is where a sign-in carrying no path lands.
export const DEFAULT_PATH = '/schema'

// safeNext reduces an arriving return_to to a path on this console, or to
// DEFAULT_PATH.
//
// The value travels to Cloud and back in a fragment, so it arrives from
// outside. A scheme or an authority in it navigates the browser to another
// origin.
//
// `//host` and `/\host` both parse as an authority, so the character after the
// leading slash decides whether the path stays on this origin.
export function safeNext(raw: string | null | undefined): string {
  if (!raw || raw[0] !== '/') return DEFAULT_PATH
  if (raw[1] === '/' || raw[1] === '\\') return DEFAULT_PATH
  return raw
}

// carriesAssertion reports whether a URL fragment holds an assertion.
//
// The router's guard and the login page must agree about this, and before it
// was one function they did not have to: the guard tested for the substring
// `assertion=`, which also matches `#my_assertion=x`, while the page parsed the
// fragment properly. Two answers to one question is how a browser ends up
// bounced away from a page that was about to consume a credential.
export function carriesAssertion(hash: string): boolean {
  const raw = hash.replace(/^#/, '')
  if (!raw) return false
  return (new URLSearchParams(raw).get(ASSERTION_PARAM) ?? '') !== ''
}

// shouldRenderLogin decides whether /login renders or the guard bounces the
// browser to the console.
//
// An authenticated browser normally has no business at /login. Two arrivals are
// the exception, and both are authenticated by definition:
//
//   - Switching organisation. The old session is still live when Cloud
//     redirects back, so bouncing here drops the assertion in the fragment and
//     lands the user on /schema, still in the old organisation, with nothing
//     anywhere to say the switch did not happen.
//   - Stepping up. The popup shares the session cookie, so it is authenticated
//     the moment it opens. Bounced, it never runs the page that posts the
//     assertion back to the dialog waiting in the opener, and the dialog waits
//     for a message nobody will send.
//
// Reading the fragment is not trusting it. Login.tsx still spends the token at
// the server, which checks the signature, the issuer and the audience, and
// spends the jti once.
export function shouldRenderLogin(authed: boolean, hash: string): boolean {
  return !authed || carriesAssertion(hash)
}

// How the sidebar draws the organisation control.
export type SwitcherMode =
  // Nothing to draw — no session yet.
  | 'none'
  // The organisation, as text. One organisation, or a session opened before
  // the claim existed and carrying an empty list.
  | 'label'
  // The organisation, and the others behind it.
  | 'menu'

// switcherMode picks between them.
//
// 'menu' takes two organisations, not one. A control that always opens onto a
// single choice teaches people it does nothing, and they stop looking at it on
// the day it has two.
export function switcherMode(me: Pick<MeResult, 'org' | 'orgs'> | undefined): SwitcherMode {
  if (!me?.org) return 'none'
  return (me.orgs?.length ?? 0) > 1 ? 'menu' : 'label'
}

// How the Callers page draws the enrol control for one caller.
export type EnrolControl =
  // No signer is configured, so the control is inert and says why. Pressing an
  // enabled button here would return 503 — the exact failure enrolment was
  // built to remove, reappearing in the browser.
  | 'unconfigured'
  // This console has never enrolled this caller.
  | 'first'
  // It has. The warning names the date, because enrolling again supersedes it.
  | 'reenrol'

// enrolControlState picks between them.
//
// Lifted out of the page for the same reason switcherMode was: it is a decision
// rather than markup, it is one boolean wide, and it is invisible when wrong.
// A console with no signer that still offers the button looks completely normal
// until somebody presses it.
//
// `certs` being undefined is the loading state and the error state at once, and
// both must read as 'unconfigured' rather than as ready — offering the control
// before the answer arrives is the same defect as offering it when the answer
// is no.
export function enrolControlState(
  certs: { enrolment_enabled: boolean; certs: { caller: string }[] } | undefined,
  caller: string,
): EnrolControl {
  if (!certs?.enrolment_enabled) return 'unconfigured'
  return certs.certs.some(c => c.caller === caller) ? 'reenrol' : 'first'
}
