import { useState, type FormEvent } from 'react'

import { Button, Card, Field, LinkButton, Notice } from '@/components/ui'

export function Verify({ error, busy, onSubmit, onRestart }: {
  error: string | null
  busy: boolean
  onSubmit: (code: string) => void
  onRestart: () => void
}) {
  const [code, setCode] = useState('')

  function submit(e: FormEvent) {
    e.preventDefault()
    onSubmit(code)
  }

  return (
    <Card
      title="Enter your code"
      subtitle="From your authenticator app, or one of your backup codes."
    >
      {error ? <Notice kind="error">{error}</Notice> : null}

      <form onSubmit={submit}>
        {/*
          One field for both kinds. The server tells them apart by shape — six
          digits is a TOTP code, anything else is tried as a backup code — so a
          chooser here would be a way to pick the wrong one.
        */}
        <Field
          label="Code"
          name="code"
          inputMode="text"
          autoComplete="one-time-code"
          value={code}
          onChange={(e) => setCode(e.target.value)}
          required
          autoFocus
        />
        <Button type="submit" busy={busy}>Sign in</Button>
      </form>

      <p className="foot">
        <LinkButton onClick={onRestart}>Start again</LinkButton>
      </p>
    </Card>
  )
}
