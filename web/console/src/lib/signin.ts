import { api } from '@/api/client'
import { RETURN_TO_PARAM } from '@/lib/session'

// REDIRECTED marks that this browser has been sent to Cloud once.
//
// The trip to Cloud is a full page load, so the mark lives in sessionStorage: a
// module variable does not survive it, and every return reads as a first
// attempt.
//
// A console that cannot open a session ping-pongs without the mark — the guard
// sends the browser to Cloud, Cloud sends it back with no assertion, the guard
// sends it again.
const REDIRECTED = 'atlantis:signin-redirected'

// redirectAttempted reports whether this browser has been sent to Cloud.
export function redirectAttempted(): boolean {
  return sessionStorage.getItem(REDIRECTED) !== null
}

// clearSignInRedirect drops the mark, freeing a later sign-in to redirect.
// Called once a session exists.
export function clearSignInRedirect(): void {
  sessionStorage.removeItem(REDIRECTED)
}

// markSignInRedirect sets the mark without navigating.
//
// Sign-out leaves for Cloud on its own, so the trip has happened as far as the
// loop guard is concerned. A Cloud that cannot be reached returns the browser
// here with no session, and the mark is what renders the card.
export function markSignInRedirect(): void {
  sessionStorage.setItem(REDIRECTED, '1')
}

// startSignIn sends the browser to Cloud carrying the path it was trying to
// reach, and reports whether it navigated.
//
// False means the mark was already set, or the console could not read its own
// config. The caller renders the sign-in card.
export async function startSignIn(nextPath: string): Promise<boolean> {
  if (redirectAttempted()) return false

  let url = ''
  try {
    url = (await api.auth.config()).cloud_signin_url
  } catch {
    return false
  }
  if (!url) return false

  sessionStorage.setItem(REDIRECTED, '1')
  const sep = url.includes('?') ? '&' : '?'
  window.location.assign(`${url}${sep}${RETURN_TO_PARAM}=${encodeURIComponent(nextPath)}`)
  return true
}
