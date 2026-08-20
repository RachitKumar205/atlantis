import { useCallback, useEffect, useState } from 'react'

import {
  ApiError,
  beginEnrolment,
  fetchAuthConfig,
  fetchPending,
  finishEnrolment,
  requestPasswordReset,
  signIn,
  signUp,
  verifySecondFactor,
} from '@/lib/api'
import {
  arrivalScreen,
  isSessionGone,
  retryAfterMessage,
  screenForNext,
  type Screen,
} from '@/lib/flow'
import { BackupCodes } from '@/screens/BackupCodes'
import { Enrol } from '@/screens/Enrol'
import { Message } from '@/screens/Message'
import { ResetRequest } from '@/screens/ResetRequest'
import { SignIn } from '@/screens/SignIn'
import { SignUp } from '@/screens/SignUp'
import { Verify } from '@/screens/Verify'

// The sign-in application.
//
// The server owns the sequence: POST /api/auth/login answers `next`, and
// GET /api/auth/pending answers it again after a reload. This component maps
// that onto a screen and does not decide for itself what follows what — the
// decisions that are its own live in lib/flow.ts, as functions with tests.

/** Where a completed sign-in goes. Replaced by the organisation landing in W5. */
const AFTER_SIGN_IN = '/organisations'

export function App() {
  const [screen, setScreen] = useState<Screen | null>(null)
  const [providers, setProviders] = useState<string[]>([])
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const [enrolment, setEnrolment] = useState<{ secret: string; uri: string } | null>(null)
  const [enrolRetried, setEnrolRetried] = useState(false)
  const [codes, setCodes] = useState<{ codes: string[]; signedIn: boolean } | null>(null)

  // One place that turns a failure into something the screens can show.
  //
  // A 401 anywhere means the pending login or the session is gone, and every
  // screen answers it the same way rather than each having its own branch.
  const handle = useCallback((e: unknown) => {
    if (e instanceof ApiError) {
      if (e.status === 429) {
        setError(retryAfterMessage(e.retryAfter))
        return
      }
      if (isSessionGone(e.status)) {
        setEnrolment(null)
        setScreen('signin')
        setError('Your sign-in expired. Please start again.')
        return
      }
      setError(e.message)
      return
    }
    setError(e instanceof Error ? e.message : String(e))
  }, [])

  // Boot: what is configured, and whether a sign-in is already in progress.
  useEffect(() => {
    let live = true
    Promise.all([fetchAuthConfig(), fetchPending()])
      .then(([config, pending]) => {
        if (!live) return
        setProviders(config.providers)
        const next = new URLSearchParams(window.location.search).get('next')
        setScreen(arrivalScreen(pending, next))
      })
      .catch((e: unknown) => {
        if (!live) return
        handle(e)
        setScreen('signin')
      })
    return () => {
      live = false
    }
  }, [handle])

  // Wraps an action so every one of them clears the last error, shows a busy
  // state, and routes failures through one place.
  const run = useCallback(
    async (fn: () => Promise<void>) => {
      setBusy(true)
      setError(null)
      try {
        await fn()
      } catch (e) {
        handle(e)
      } finally {
        setBusy(false)
      }
    },
    [handle],
  )

  const startEnrolment = useCallback(
    (retry: boolean) =>
      run(async () => {
        const started = await beginEnrolment()
        setEnrolment(started)
        setEnrolRetried(retry)
        setScreen('enrol')
      }),
    [run],
  )

  if (screen === null) {
    // The boot request has not answered. Deliberately blank rather than a
    // flash of the sign-in form for somebody who is mid-enrolment.
    return null
  }

  switch (screen) {
    case 'signin':
      return (
        <SignIn
          providers={providers}
          error={error}
          busy={busy}
          onSubmit={(email, password) =>
            run(async () => {
              const { next } = await signIn(email, password)
              const to = screenForNext(next)
              if (to === 'enrol') {
                await startEnrolment(false)
                return
              }
              if (to === null) {
                setError('This account is in a state the app does not understand.')
                return
              }
              setScreen(to)
            })
          }
          onSignUp={() => {
            setError(null)
            setScreen('signup')
          }}
          onForgot={() => {
            setError(null)
            setScreen('reset-request')
          }}
        />
      )

    case 'signup':
      return (
        <SignUp
          error={error}
          busy={busy}
          onSubmit={(email, password, name) =>
            run(async () => {
              await signUp(email, password, name)
              setScreen('check-email')
            })
          }
          onBack={() => {
            setError(null)
            setScreen('signin')
          }}
        />
      )

    case 'check-email':
      return (
        <Message
          title="Check your email"
          // The server answers identically whether or not the address was new,
          // so this must not imply an account was created.
          body="If that address can receive mail, a message is on its way. Open the link in it to finish."
          backLabel="Back to sign in"
          onBack={() => setScreen('signin')}
        />
      )

    case 'enrol':
      return (
        <Enrol
          secret={enrolment?.secret ?? ''}
          uri={enrolment?.uri ?? ''}
          error={error}
          busy={busy}
          retried={enrolRetried}
          onSubmit={(code) =>
            run(async () => {
              const result = await finishEnrolment(code)
              setCodes({ codes: result.backupCodes, signedIn: result.signedIn })
              setScreen('backup-codes')
            })
          }
          onRestart={() => startEnrolment(true)}
        />
      )

    case 'verify':
      return (
        <Verify
          error={error}
          busy={busy}
          onSubmit={(code) =>
            run(async () => {
              await verifySecondFactor(code)
              window.location.assign(AFTER_SIGN_IN)
            })
          }
          onRestart={() => {
            setError(null)
            setScreen('signin')
          }}
        />
      )

    case 'backup-codes':
      return (
        <BackupCodes
          codes={codes?.codes ?? []}
          signedIn={codes?.signedIn ?? false}
          onDone={() => {
            // signed_in comes from the server rather than from the message
            // text: this route has two exits, and which one it took decides
            // whether there is a session to go anywhere with.
            if (codes?.signedIn) {
              window.location.assign(AFTER_SIGN_IN)
              return
            }
            setScreen('signin')
          }}
        />
      )

    case 'reset-request':
      return (
        <ResetRequest
          error={error}
          busy={busy}
          onSubmit={(email) =>
            run(async () => {
              await requestPasswordReset(email)
              setScreen('reset-sent')
            })
          }
          onBack={() => {
            setError(null)
            setScreen('signin')
          }}
        />
      )

    case 'reset-sent':
      return (
        <Message
          title="Check your email"
          body="If that address has an account, a reset link is on its way. It is good for one hour."
          backLabel="Back to sign in"
          onBack={() => setScreen('signin')}
        />
      )
  }
}
