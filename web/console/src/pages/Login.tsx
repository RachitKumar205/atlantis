import { useState, type FormEvent } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { ArrowRight } from 'lucide-react'
import { api } from '@/api/client'

// Centered authcard: concentric-ring logo above lowercase "atlantis"
// wordmark, "Sign in" title, two inputs, full-width brass submit.
// Multiple bolder redesigns (porthole + serif wordmark + depth ruler,
// instrument-plate labels) were tried and reverted — keep this baseline
// unless the whole auth surface is being rethought.
export function Login() {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const navigate = useNavigate()
  const qc = useQueryClient()

  const loginMutation = useMutation({
    mutationFn: () => api.auth.login(email, password),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['auth', 'me'] })
      navigate({ to: '/schema' })
    },
  })

  const submit = (e: FormEvent) => {
    e.preventDefault()
    if (!email || !password) return
    loginMutation.mutate()
  }

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

        <form className="auth__fields" onSubmit={submit} noValidate>
          <div className="field">
            <label className="field__label" htmlFor="auth-email">Email</label>
            <input
              id="auth-email"
              className="input"
              type="email"
              placeholder="you@example.com"
              value={email}
              onChange={e => setEmail(e.target.value)}
              autoComplete="email"
              autoFocus
              disabled={loginMutation.isPending}
            />
          </div>
          <div className="field">
            <label className="field__label" htmlFor="auth-password">Password</label>
            <input
              id="auth-password"
              className="input"
              type="password"
              placeholder="••••••••••"
              value={password}
              onChange={e => setPassword(e.target.value)}
              autoComplete="current-password"
              disabled={loginMutation.isPending}
            />
          </div>

          {loginMutation.isError && (
            <div className="banner banner--error" role="alert" aria-live="assertive">
              <span className="banner__icon" />
              <span>{loginMutation.error?.message ?? 'Invalid credentials'}</span>
            </div>
          )}

          <button
            type="submit"
            className="btn btn--brass"
            style={{ width: '100%', justifyContent: 'center', height: 38, marginTop: 4 }}
            disabled={loginMutation.isPending || !email || !password}
          >
            <span>{loginMutation.isPending ? 'Signing in…' : 'Sign in'}</span>
            <ArrowRight size={14} />
          </button>
        </form>
      </div>
    </div>
  )
}
