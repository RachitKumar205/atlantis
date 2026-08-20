import { useState, type FormEvent } from 'react'

import { Button, Card, Field, LinkButton, Notice } from '@/components/ui'

export function SignUp({ error, busy, onSubmit, onBack }: {
  error: string | null
  busy: boolean
  onSubmit: (email: string, password: string, name: string) => void
  onBack: () => void
}) {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [name, setName] = useState('')

  function submit(e: FormEvent) {
    e.preventDefault()
    onSubmit(email, password, name)
  }

  return (
    <Card
      title="Create an account"
      subtitle="You will set up two-factor authentication before you can sign in."
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
        <Field
          label="Name"
          name="name"
          autoComplete="name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          hint="Optional."
        />
        {/*
          No strength meter here. The server checks with zxcvbn and against the
          breach corpus, and refuses below score 3 with a message written to be
          shown — a second opinion in the page could disagree with the one that
          decides, which is worse than none.
        */}
        <Field
          label="Password"
          name="password"
          type="password"
          autoComplete="new-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required
          hint="At least 12 characters. Checked against known breached passwords."
        />
        <Button type="submit" busy={busy}>Create account</Button>
      </form>

      <p className="foot">
        <LinkButton onClick={onBack}>Back to sign in</LinkButton>
      </p>
    </Card>
  )
}
