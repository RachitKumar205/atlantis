import { useEffect, useRef, useState, type FormEvent } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery } from '@tanstack/react-query'
import { Check, Server, RotateCw, AlertCircle } from 'lucide-react'
import { api } from '@/api/client'
import type { ConnectivityProbe } from '@/api/client'

type Step = 1 | 2
type ProbeStatus = 'wait' | 'run' | 'ok' | 'err'

// Local row type widens the server's three-state enum with a "run"
// placeholder used while the connectivity query is in flight.
interface ProbeRow {
  label: string
  status: ProbeStatus
  meta?: string
}

// The five probe labels the server returns, in order. Used to render
// placeholder rows during the first load so the layout doesn't shift
// when the response arrives.
const PROBE_LABELS = [
  'TCP reachable',
  'TLS 1.3 handshake',
  'Server cert chain',
  'Client cert accepted',
  'gRPC reflection',
]

// Wizard steps. Two now: operator credentials, then live connectivity
// probes. The user is created on the connectivity step's "Finish setup"
// button so a half-completed wizard never leaves an orphaned admin
// user in the DB.
const STEPS: { num: Step; name: string }[] = [
  { num: 1, name: 'Operator' },
  { num: 2, name: 'Connectivity' },
]

// Matches `--med` in tokens.css. Used to delay the active-track gradient
// when the user goes BACK in the wizard so the previously-active
// segment finishes draining before the new one lights up. Reduced-motion
// users skip the delay entirely.
const TRACK_TRANSITION_MS = 220

// Two-step wizard: operator credentials, then live connectivity probes
// against the configured atlantis endpoint. The user row is created
// only on the connectivity step's submit, so a half-completed wizard
// never leaves an orphaned admin user in the DB. Step UI uses .gauge /
// .card / .probe-test from pages.css.
export function Setup() {
  const [step, setStep] = useState<Step>(1)

  // activeStep drives the `.is-active` gradient on the gauge. On forward
  // navigation it snaps to `step` immediately. On backward navigation it
  // briefly drops to null so the higher segment drains, then catches up
  // to the new lower step. This sequences the two animations.
  const [activeStep, setActiveStep] = useState<Step | null>(1)
  const prevStepRef = useRef<Step>(1)
  useEffect(() => {
    const prev = prevStepRef.current
    prevStepRef.current = step
    if (step > prev) {
      setActiveStep(step)
      return
    }
    if (step < prev) {
      setActiveStep(null)
      const reduced = window.matchMedia?.('(prefers-reduced-motion: reduce)').matches ?? false
      const delay = reduced ? 0 : TRACK_TRANSITION_MS
      const t = window.setTimeout(() => setActiveStep(step), delay)
      return () => window.clearTimeout(t)
    }
  }, [step])

  const [firstName, setFirstName] = useState('')
  const [lastName, setLastName] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [endpoint, setEndpoint] = useState('')
  const [validation, setValidation] = useState('')
  const navigate = useNavigate()

  // Step 2 lazy-loads the connectivity probes. The endpoint, overall
  // status, and per-probe results all come from the BFF — no client-
  // side guessing.
  const healthQ = useQuery({
    queryKey: ['setup', 'connectivity'] as const,
    queryFn: () => api.setup.connectivity(),
    enabled: step === 2,
    retry: 0,
    staleTime: 0,
  })

  // setupM creates the admin user. Deferred to step 2's "Finish setup"
  // button so a half-completed wizard never leaves the system with an
  // orphaned admin user. Closing the browser before the final click
  // means the database stays empty and the wizard re-appears next visit.
  const setupM = useMutation({
    mutationFn: () => api.setup.configure(firstName, lastName, email, password),
    onSuccess: () => navigate({ to: '/login' }),
  })

  const submitStep1 = (e: FormEvent) => {
    e.preventDefault()
    setValidation('')
    if (!email) { setValidation('Email is required'); return }
    if (password.length < 8) { setValidation('Password must be at least 8 characters'); return }
    if (password !== confirm) { setValidation('Passwords do not match'); return }
    // Credentials stay in component state until step 2 commits them.
    setStep(2)
  }

  const serverHealthy = healthQ.data?.overall === 'ok'

  // Sync the endpoint input with what the BFF reports it dials —
  // the input is a display aid in self-host mode, not a configuration
  // surface (the address comes from ATL_ENDPOINT, not the form).
  useEffect(() => {
    if (healthQ.data?.endpoint) setEndpoint(healthQ.data.endpoint)
  }, [healthQ.data?.endpoint])

  // Render rows from the server's probe array when available; otherwise
  // render placeholder rows in the canonical order so the layout doesn't
  // shift while the probes are in flight. On a hard network failure
  // (BFF unreachable), surface a single "TCP reachable" error so the
  // user still gets actionable signal.
  const probes: ProbeRow[] = (() => {
    if (healthQ.isLoading) {
      const [first, ...rest] = PROBE_LABELS
      return [
        { label: first, status: 'run' as const },
        ...rest.map(label => ({ label, status: 'wait' as const })),
      ]
    }
    if (healthQ.data) {
      return healthQ.data.probes.map((p: ConnectivityProbe) => ({
        label: p.label,
        status: p.status as ProbeStatus,
        meta: p.meta,
      }))
    }
    // Fetch itself failed — wizard can't even reach its own BFF.
    return [
      { label: PROBE_LABELS[0], status: 'err', meta: healthQ.error?.message ?? 'unreachable' },
      ...PROBE_LABELS.slice(1).map(label => ({ label, status: 'wait' as ProbeStatus })),
    ]
  })()

  return (
    <div className="auth" style={{ alignItems: 'flex-start', paddingTop: '9vh' }}>
      <div className="auth__grid" />
      <div className="setup" style={{ position: 'relative', zIndex: 1, width: 620 }}>
        <div className="wordmark" style={{ fontSize: 18, marginBottom: 6 }}>
          atlant
          <span style={{ position: 'relative' }}>
            i
            <span
              className="i-dot"
              style={{ position: 'absolute', left: '50%', top: -1, transform: 'translateX(-50%)' }}
            />
          </span>
          s
          <span className="muted" style={{ fontWeight: 400, fontSize: 13, marginLeft: 6 }}>
            setup
          </span>
        </div>

        <div className="gauge">
          {STEPS.map(s => {
            const isActive = activeStep === s.num
            // is-done covers the segments we've moved past AND the
            // current segment WHILE the back-transition is in flight
            // (current step yet, but its gradient hasn't appeared
            // yet — keep it as the solid done state until the drain
            // above finishes).
            const isDone = s.num < step || (s.num === step && !isActive)
            return (
              <div
                key={s.num}
                className={`gstep ${isDone ? 'is-done' : ''} ${isActive ? 'is-active' : ''}`}
              >
                <div className="gstep__track" />
                <div className="gstep__meta">
                  <span className="gstep__name">{s.name}</span>
                </div>
              </div>
            )
          })}
        </div>

        {/* Step 1 ── operator account */}
        {step === 1 && (
          <>
            <div className="card">
              <div className="card__head">
                <Server />
                <span className="card__title">Create the first operator</span>
                <span className="badge badge--plain" style={{ marginLeft: 'auto' }}>step 1 / {STEPS.length}</span>
              </div>
              <div className="card__body">
                <form onSubmit={submitStep1} noValidate>
                  <div className="row" style={{ gap: 12, marginBottom: 18 }}>
                    <div className="field" style={{ flex: 1 }}>
                      <label className="field__label">First name <span style={{ color: 'var(--ink-3)', fontWeight: 400 }}>(optional)</span></label>
                      <input
                        className="input"
                        type="text"
                        placeholder="Ada"
                        value={firstName}
                        onChange={e => setFirstName(e.target.value)}
                        autoComplete="given-name"
                        disabled={setupM.isPending}
                      />
                    </div>
                    <div className="field" style={{ flex: 1 }}>
                      <label className="field__label">Last name <span style={{ color: 'var(--ink-3)', fontWeight: 400 }}>(optional)</span></label>
                      <input
                        className="input"
                        type="text"
                        placeholder="Lovelace"
                        value={lastName}
                        onChange={e => setLastName(e.target.value)}
                        autoComplete="family-name"
                        disabled={setupM.isPending}
                      />
                    </div>
                  </div>
                  <div className="field" style={{ marginBottom: 18 }}>
                    <label className="field__label">Email</label>
                    <input
                      className="input"
                      type="email"
                      placeholder="operator@company.dev"
                      value={email}
                      onChange={e => setEmail(e.target.value)}
                      autoComplete="email"
                      autoFocus
                      disabled={setupM.isPending}
                    />
                  </div>
                  <div className="field" style={{ marginBottom: 18 }}>
                    <label className="field__label">Password</label>
                    <input
                      className="input"
                      type="password"
                      placeholder="Min. 8 characters"
                      value={password}
                      onChange={e => setPassword(e.target.value)}
                      autoComplete="new-password"
                      disabled={setupM.isPending}
                    />
                  </div>
                  <div className="field" style={{ marginBottom: 18 }}>
                    <label className="field__label">Confirm password</label>
                    <input
                      className="input"
                      type="password"
                      placeholder="Repeat password"
                      value={confirm}
                      onChange={e => setConfirm(e.target.value)}
                      autoComplete="new-password"
                      disabled={setupM.isPending}
                    />
                  </div>

                  {validation && (
                    <div className="banner banner--error" style={{ marginBottom: 16 }}>
                      <AlertCircle size={16} />
                      <span>{validation}</span>
                    </div>
                  )}

                  <div className="row" style={{ justifyContent: 'flex-end' }}>
                    <button type="submit" className="btn btn--brass">
                      Continue → Connectivity
                    </button>
                  </div>
                </form>
              </div>
            </div>
          </>
        )}

        {/* Step 2 ── connectivity */}
        {step === 2 && (
          <>
            <div className="card">
              <div className="card__head">
                <Server />
                <span className="card__title">Verify atlantis endpoint &amp; mTLS</span>
                <span className="badge badge--plain" style={{ marginLeft: 'auto' }}>step 2 / {STEPS.length}</span>
              </div>
              <div className="card__body">
                <div className="row" style={{ gap: 16, alignItems: 'flex-end', marginBottom: 6 }}>
                  <div className="field" style={{ flex: 1 }}>
                    <label className="field__label">Endpoint</label>
                    <input
                      className="input mono"
                      value={endpoint || (healthQ.isLoading ? 'loading…' : '')}
                      readOnly
                      title="Set via the ATL_ENDPOINT environment variable on the console process."
                    />
                  </div>
                  <button
                    className="btn"
                    onClick={() => healthQ.refetch()}
                    disabled={healthQ.isFetching}
                  >
                    <RotateCw size={13} />
                    <span>Re-check</span>
                  </button>
                </div>
                <p style={{ marginBottom: 22, fontSize: 11.5, color: 'var(--ink-3)' }}>
                  Configured via <code className="mono" style={{ color: 'var(--ink-2)' }}>ATL_ENDPOINT</code> on the console process. To change it, update your deployment env and restart.
                </p>

                <div className="probe-test">
                  {probes.map((p, i) => (
                    <div key={i} className="probe-test__row" data-st={p.status}>
                      <span className="probe-test__icon">
                        {p.status === 'ok' && (
                          <span className="sage"><Check size={13} /></span>
                        )}
                        {p.status === 'run' && <span className="spin" />}
                        {p.status === 'err' && (
                          <span className="coral"><AlertCircle size={13} /></span>
                        )}
                        {p.status === 'wait' && <span className="faint">·</span>}
                      </span>
                      <span className="probe-test__label">{p.label}</span>
                      <span
                        className={
                          'probe-test__status ' +
                          (p.status === 'ok'  ? 'sage'  :
                           p.status === 'run' ? 'brass' :
                           p.status === 'err' ? 'coral' : 'faint')
                        }
                      >
                        {p.status === 'ok'  ? (p.meta || 'ok') :
                         p.status === 'run' ? 'checking…' :
                         p.status === 'err' ? (p.meta || 'failed') : 'pending'}
                      </span>
                    </div>
                  ))}
                </div>
              </div>
            </div>

            {setupM.isError && (
              <div className="banner banner--error" style={{ marginTop: 16 }}>
                <AlertCircle size={16} />
                <span>{setupM.error?.message ?? 'Could not create operator account.'}</span>
              </div>
            )}

            <div className="row" style={{ marginTop: 22, justifyContent: 'space-between' }}>
              <button
                className="btn btn--ghost"
                onClick={() => setStep(1)}
                disabled={setupM.isPending}
              >
                ← Operator
              </button>
              <button
                className="btn btn--brass"
                onClick={() => setupM.mutate()}
                disabled={!serverHealthy || setupM.isPending}
              >
                {setupM.isPending ? 'Creating account…' : 'Finish setup'}
              </button>
            </div>
          </>
        )}
      </div>
    </div>
  )
}
