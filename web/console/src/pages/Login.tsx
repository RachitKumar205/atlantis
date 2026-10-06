import { useEffect, useRef, useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { api } from '@/api/client'
import {
  ASSERTION_PARAM,
  DEFAULT_PATH,
  RETURN_TO_PARAM,
  safeNext,
} from '@/lib/session'
import { clearSignInRedirect, startSignIn } from '@/lib/signin'
import { markOnboardingPending } from '@/lib/onboarding'

// Centered authcard: concentric-ring logo above lowercase "atlantis"
// wordmark. Multiple bolder redesigns (porthole + serif wordmark + depth
// ruler, instrument-plate labels) were tried and reverted — keep this
// baseline unless the whole auth surface is being rethought.
//
// There is no form any more. The console holds no credentials: a user signs
// in at Atlantis Cloud and arrives here carrying a signed assertion, which
// this page spends for a session cookie.
//
// The assertion travels in the URL *fragment*, not the query string. A
// fragment is never sent to the server, so it stays out of access logs, out
// of the Referer header on the next navigation, and out of anything sitting
// between the browser and here. It is also erased from the address bar below
// before the exchange resolves, so it does not survive into history or a
// bookmark.
//
// ASSERTION_PARAM comes from @/lib/session, which is also what the router's
// guard reads the fragment with. The guard has to let this page render when an
// assertion is present, so both must agree on what "present" means.

type Arrival = { assertion: string; next: string }

function takeAssertionFromURL(): Arrival | null {
  const raw = window.location.hash.replace(/^#/, '')
  if (!raw) return null

  const params = new URLSearchParams(raw)
  const assertion = params.get(ASSERTION_PARAM)
  if (!assertion) return null

  // Where the browser was headed before it was sent to Cloud. safeNext reduces
  // it to a path on this console; it arrived through Cloud, so it comes from
  // outside.
  const next = safeNext(params.get(RETURN_TO_PARAM))

  // Remove it before anything else runs. replaceState rather than assigning
  // location.hash, which would push a history entry that still contains it.
  params.delete(ASSERTION_PARAM)
  params.delete(RETURN_TO_PARAM)
  const rest = params.toString()
  window.history.replaceState(null, '', window.location.pathname + (rest ? `#${rest}` : ''))

  return { assertion, next }
}

export function Login() {
  const [arrival] = useState(takeAssertionFromURL)
  const assertion = arrival?.assertion ?? null

  // Cloud's address, for the card's link. Null until the request answers, and
  // null for good when the console cannot read its own config.
  const [cloudURL, setCloudURL] = useState<string | null>(null)

  const exchange = useMutation({
    mutationFn: (token: string) => api.auth.exchange(token),
    onSuccess: () => {
      clearSignInRedirect()

      // Opening the organisation from Cloud is what starts onboarding, and
      // this is the only place that knows it happened.
      markOnboardingPending()

      // A full load. The destination arrived from outside the route tree, so
      // the typed router cannot check it, and the load starts the console on
      // the session that now exists.
      window.location.replace(arrival?.next ?? DEFAULT_PATH)
    },
  })

  // Exchange once. React runs effects twice in StrictMode, and the assertion
  // is single-use at the server — a second POST would be refused as a replay
  // and the user would be shown a failure for a sign-in that worked.
  const started = useRef(false)
  useEffect(() => {
    if (!arrival || started.current) return
    started.current = true
    exchange.mutate(arrival.assertion)
  }, [arrival, exchange])

  // Reached with no assertion, so a sign-in starts here. startSignIn navigates
  // to Cloud unless the mark is set, which is the return leg of a trip that
  // produced none.
  //
  // DEFAULT_PATH as the return path: /login is this page, and sending it would
  // land the browser back here once the session exists.
  useEffect(() => {
    if (arrival) return
    void startSignIn(DEFAULT_PATH)
  }, [arrival])

  // The card links to Cloud whenever it renders — no assertion arrived, or the
  // exchange failed. Both leave the browser on this page with a session to
  // start.
  const needsLink = !arrival || exchange.isError
  useEffect(() => {
    if (!needsLink) return
    let live = true
    api.auth
      .config()
      .then(cfg => {
        if (live) setCloudURL(cfg.cloud_signin_url)
      })
      .catch(() => {
        // The console cannot reach its own API. The card renders without a
        // link, which is the state it is in.
      })
    return () => {
      live = false
    }
  }, [needsLink])

  return (
    <div className="auth">
      <div className="auth__grid" />
      <div className="auth__vignette" />

      <div className="authcard">
        <div className="auth__brand">
          <svg
            className="auth__logo"
            width="44"
            height="44"
            viewBox="0 0 26 26"
            fill="none"
            aria-hidden
          >
            <circle cx="13" cy="13" r="10"  stroke="var(--line-strong)" strokeWidth="1.1" />
            <circle cx="13" cy="13" r="5.5" stroke="var(--ink-2)"        strokeWidth="1" />
            <circle cx="13" cy="13" r="1.9" fill="var(--accent)" />
          </svg>
          <div className="wordmark">atlantis</div>
        </div>

        <div className="auth__title">Sign in</div>

        <div className="auth__fields">
          {assertion && exchange.isPending && (
            <p className="auth__note" aria-live="polite">Signing you in…</p>
          )}

          {exchange.isError && (
            <div className="banner banner--error" role="alert" aria-live="assertive">
              <span className="banner__icon" />
              <span>
                {exchange.error?.message ?? 'That sign-in link could not be used.'}
              </span>
            </div>
          )}

          {!assertion && !exchange.isPending && (
            <p className="auth__note">
              This console keeps no accounts. Sign in at Atlantis Cloud and
              choose it there.
            </p>
          )}

          {needsLink && !exchange.isPending && (
            cloudURL ? (
              <a className="btn btn--primary auth__cta" href={cloudURL}>
                Continue to Atlantis Cloud
              </a>
            ) : (
              <p className="auth__note">
                Atlantis Cloud could not be reached from this console.
              </p>
            )
          )}
        </div>
      </div>
    </div>
  )
}
