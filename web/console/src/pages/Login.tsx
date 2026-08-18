import { useEffect, useRef, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'

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
const ASSERTION_PARAM = 'assertion'
const MODE_PARAM = 'mode'

// STEP_UP_MESSAGE is the postMessage type this page sends to its opener.
//
// Exported so the listener and the sender name the same string. Two string
// literals in two files is one rename away from a dialog that waits forever for
// a message nobody sends.
export const STEP_UP_MESSAGE = 'atlantis:step-up'

type Arrival = { assertion: string; stepUp: boolean }

function takeAssertionFromURL(): Arrival | null {
  const raw = window.location.hash.replace(/^#/, '')
  if (!raw) return null

  const params = new URLSearchParams(raw)
  const assertion = params.get(ASSERTION_PARAM)
  if (!assertion) return null

  // mode=reauth means Cloud sent this to a popup the console opened to confirm
  // a destructive action, not to a tab signing somebody in. The assertion says
  // a second factor was just presented; it belongs to the dialog waiting in the
  // opener, and exchanging it here would spend it for a second session instead.
  const stepUp = params.get(MODE_PARAM) === 'reauth'

  // Remove it before anything else runs. replaceState rather than assigning
  // location.hash, which would push a history entry that still contains it.
  params.delete(ASSERTION_PARAM)
  params.delete(MODE_PARAM)
  const rest = params.toString()
  window.history.replaceState(null, '', window.location.pathname + (rest ? `#${rest}` : ''))

  return { assertion, stepUp }
}

export function Login() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const [arrival] = useState(takeAssertionFromURL)
  const assertion = arrival?.assertion ?? null

  const exchange = useMutation({
    mutationFn: (token: string) => api.auth.exchange(token),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['auth', 'me'] })
      navigate({ to: '/schema', search: { namespace: undefined, entity: undefined } })
    },
  })

  // Exchange once. React runs effects twice in StrictMode, and the assertion
  // is single-use at the server — a second POST would be refused as a replay
  // and the user would be shown a failure for a sign-in that worked.
  const started = useRef(false)
  useEffect(() => {
    if (!arrival || started.current) return
    started.current = true

    if (arrival.stepUp) {
      // Hand it back and close. targetOrigin is this page's own origin rather
      // than '*': the assertion is a credential, and '*' would deliver it to
      // whatever document happened to open this window — including one on
      // another site.
      window.opener?.postMessage(
        { type: STEP_UP_MESSAGE, assertion: arrival.assertion },
        window.location.origin,
      )
      window.close()
      return
    }

    exchange.mutate(arrival.assertion)
  }, [arrival, exchange])

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
                {' '}Start again from Atlantis Cloud.
              </span>
            </div>
          )}

          {!assertion && !exchange.isPending && (
            <p className="auth__note">
              Sign in from Atlantis Cloud, then choose this console. Opening this
              page directly cannot sign you in — the console keeps no accounts of
              its own.
            </p>
          )}
        </div>
      </div>
    </div>
  )
}
