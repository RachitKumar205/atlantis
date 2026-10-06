import { useState, type FormEvent } from 'react'

import { OtpField } from '@atlantis/shared/otp'

import { Button, Card, Field, LinkButton, Notice } from '@/components/ui'

export function Verify({ error, busy, onSubmit, onRestart }: {
  error: string | null
  busy: boolean
  onSubmit: (code: string) => void
  onRestart: () => void
}) {
  const [code, setCode] = useState('')

  // The six-box field is the default because a six-digit authenticator code is
  // what nearly everybody arrives with. A backup code is ten characters in
  // XXXXX-XXXXX form and does not fit in it, so it gets a plain field — reached
  // by the link below, or automatically when one is pasted into the boxes.
  //
  // Which one is showing decides nothing on the server: it still tells them
  // apart by shape, so a wrong turn here costs a click rather than a refusal.
  const [backup, setBackup] = useState(false)

  function submit(e: FormEvent) {
    e.preventDefault()
    onSubmit(code)
  }

  function useBackup(pasted?: string) {
    setBackup(true)
    if (pasted !== undefined) setCode(pasted)
  }

  return (
    <Card
      title="Enter your code"
      subtitle="From your authenticator app, or one of your backup codes."
    >
      {error ? <Notice kind="error">{error}</Notice> : null}

      <form onSubmit={submit}>
        {backup ? (
          <Field
            label="Backup code"
            name="code"
            inputMode="text"
            autoComplete="one-time-code"
            autoCapitalize="characters"
            spellCheck={false}
            placeholder="XXXXX-XXXXX"
            value={code}
            onChange={(e) => setCode(e.target.value)}
            required
            autoFocus
          />
        ) : (
          <div className="field">
            <span className="label">Code</span>
            <OtpField value={code} onChange={setCode} onOverflow={useBackup} />
          </div>
        )}

        {/* Enabled until the request starts, so validation happens on submit
            rather than by refusing to let the button be pressed. */}
        <Button type="submit" busy={busy}>Sign in</Button>
      </form>

      <p className="foot">
        {backup ? (
          <LinkButton onClick={() => { setBackup(false); setCode('') }}>
            Use your authenticator
          </LinkButton>
        ) : (
          <LinkButton onClick={() => useBackup('')}>Use a backup code</LinkButton>
        )}
        <span className="foot__sep">·</span>
        <LinkButton onClick={onRestart}>Start again</LinkButton>
      </p>
    </Card>
  )
}
