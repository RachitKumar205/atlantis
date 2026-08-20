import { useState, type FormEvent } from 'react'

import { Button, Card, Field, LinkButton, Notice } from '@/components/ui'

export function ResetRequest({ error, busy, onSubmit, onBack }: {
  error: string | null
  busy: boolean
  onSubmit: (email: string) => void
  onBack: () => void
}) {
  const [email, setEmail] = useState('')

  function submit(e: FormEvent) {
    e.preventDefault()
    onSubmit(email)
  }

  return (
    <Card
      title="Reset your password"
      subtitle="We will send a link if that address has an account."
    >
      {error ? <Notice kind="error">{error}</Notice> : null}

      <form onSubmit={submit}>
        <Field
          label="Email"
          name="email"
          type="email"
          autoComplete="username"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          required
          autoFocus
        />
        <Button type="submit" busy={busy}>Send the link</Button>
      </form>

      <p className="foot">
        <LinkButton onClick={onBack}>Back to sign in</LinkButton>
      </p>
    </Card>
  )
}
