import { useState, type FormEvent } from 'react'

import { Button, Card, Field, LinkButton, Notice } from '@/components/ui'

export function SignIn({ providers, error, busy, onSubmit, onSignUp, onForgot }: {
  providers: string[]
  error: string | null
  busy: boolean
  onSubmit: (email: string, password: string) => void
  onSignUp: () => void
  onForgot: () => void
}) {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')

  function submit(e: FormEvent) {
    e.preventDefault()
    onSubmit(email, password)
  }

  return (
    <Card title="Sign in">
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
          label="Password"
          name="password"
          type="password"
          autoComplete="current-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required
        />
        <Button type="submit" busy={busy}>Continue</Button>
      </form>

      {/*
        Drawn from GET /api/auth/config, which exists because this decision has
        to be made before there is a session to ask about. A provider with no
        credentials is absent from that list, and its route is a 404 — so a
        hardcoded button would send people to a dead end.
      */}
      {providers.length > 0 ? (
        <div className="providers">
          <p className="divider"><span>or</span></p>
          {providers.map((p) => (
            <a key={p} className="btn btn--ghost" href={`/auth/${p}`}>
              Continue with {label(p)}
            </a>
          ))}
        </div>
      ) : null}

      <p className="foot">
        <LinkButton onClick={onSignUp}>Create an account</LinkButton>
        <span className="foot__sep">·</span>
        <LinkButton onClick={onForgot}>Forgot password</LinkButton>
      </p>
    </Card>
  )
}

function label(provider: string): string {
  switch (provider) {
    case 'github':
      return 'GitHub'
    case 'google':
      return 'Google'
    default:
      return provider
  }
}
