import { useState, type FormEvent } from 'react'

import { Button, Card, Field, LinkButton, Notice, QRCode } from '@/components/ui'

export function Enrol({ secret, uri, error, busy, retried, onSubmit, onRestart }: {
  secret: string
  uri: string
  error: string | null
  busy: boolean
  /** True when a fresh secret was minted after an earlier attempt. */
  retried: boolean
  onSubmit: (code: string) => void
  onRestart: () => void
}) {
  const [code, setCode] = useState('')
  const [showSecret, setShowSecret] = useState(false)

  function submit(e: FormEvent) {
    e.preventDefault()
    onSubmit(code)
  }

  return (
    <Card
      title="Set up two-factor authentication"
      subtitle="Scan this with an authenticator app, then enter the code it shows."
    >
      {error ? <Notice kind="error">{error}</Notice> : null}

      {/*
        Said plainly, because the window is short and the failure is confusing.
        A pending login lives ten minutes (store.PendingTTL), which is not long
        if the app still has to be installed.
      */}
      <Notice kind="info">
        Finish within ten minutes, or you will need to sign in again.
      </Notice>

      {/*
        handleEnrolBegin writes a fresh unconfirmed secret every time it is
        called, so a restart leaves the previous entry in the authenticator app
        pointing at a secret the server has forgotten. Two "Atlantis Cloud"
        entries, one of which silently never works.
      */}
      {retried ? (
        <Notice kind="info">
          This is a new code. Delete any earlier &ldquo;Atlantis Cloud&rdquo; entry in your
          authenticator app — it will not work.
        </Notice>
      ) : null}

      <div className="qrwrap">
        <QRCode uri={uri} />
      </div>

      <p className="foot">
        <LinkButton onClick={() => setShowSecret((v) => !v)}>
          {showSecret ? 'Hide the code' : 'Can’t scan it?'}
        </LinkButton>
      </p>
      {/*
        The accessible path to the same secret, and the one that works when a
        camera will not focus. Selectable text rather than a copy button: this
        is a secret, and a clipboard write is a side effect somebody should
        choose.
      */}
      {showSecret ? (
        <p className="secret">
          <span className="label">Enter this key by hand</span>
          <code>{secret}</code>
        </p>
      ) : null}

      <form onSubmit={submit}>
        <Field
          label="Code from your authenticator"
          name="code"
          inputMode="numeric"
          autoComplete="one-time-code"
          value={code}
          onChange={(e) => setCode(e.target.value)}
          required
          autoFocus
        />
        <Button type="submit" busy={busy}>Confirm</Button>
      </form>

      <p className="foot">
        <LinkButton onClick={onRestart}>Start again</LinkButton>
      </p>
    </Card>
  )
}
