import { useEffect, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Check, Copy, Key, Link2, Plus, Trash2, X } from 'lucide-react'
import { api, ApiError, queries, type CallerInfo, type EnrollTokenResponse } from '@/api/client'
import { useIsAdmin } from '@/hooks/useAuth'
import { enrolControlState, type EnrolControl } from '@/lib/session'
import { PageShell } from '@/components/PageShell'
import { HoverInfo } from '@/components/HoverInfo'
import { SudoConfirmDialog } from './Settings'

function fmtDateShort(iso?: string) {
  if (!iso) return '—'
  const d = new Date(iso)
  if (isNaN(d.getTime())) return '—'
  return d.toISOString().slice(0, 10)
}

const CALLER_NAME_RE = /^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$|^[a-z0-9]$/

// Until the BFF exposes per-caller cert expiry, the meter renders an
// "unknown" state — same chrome, no fill. The day classes still work
// when expiry data arrives (additive change).
function certClass(days?: number) {
  if (days == null) return ''
  return days < 14 ? 'is-exp' : days < 60 ? 'is-soon' : ''
}
function certLabel(days?: number) {
  if (days == null) return 'unknown'
  return days < 14 ? 'expiring' : days < 60 ? 'renew soon' : 'valid'
}

// ── Page ──────────────────────────────────────────────────────────────────
export function Callers() {
  const qc = useQueryClient()
  const isAdmin = useIsAdmin()
  const { data, isLoading, error } = useQuery(queries.callers())
  const { data: instance } = useQuery(queries.instance())
  const [addOpen, setAddOpen] = useState(false)
  const [revoking, setRevoking] = useState<string | null>(null)
  const [enrolment, setEnrolment] = useState<EnrollTokenResponse | null>(null)
  const [enrollingCaller, setEnrollingCaller] = useState<string | null>(null)
  const { data: certs } = useQuery(queries.callerCerts())
  const [aliasEditing, setAliasEditing] = useState<string | null>(null)
  const [toast, setToast] = useState<string | null>(null)

  // The design's pages.css gates .callergrid and .callertable behind a
  // :root[data-callers="cards"] attribute. Cards is the canonical view
  // here; setting the attribute reveals the grid layout and hides the
  // table fallback.
  useEffect(() => {
    document.documentElement.setAttribute('data-callers', 'cards')
    return () => { document.documentElement.removeAttribute('data-callers') }
  }, [])

  const showToast = (msg: string) => {
    setToast(msg)
    setTimeout(() => setToast(null), 2400)
  }

  const enrolM = useMutation({
    mutationFn: (name: string) => api.callers.enroll(name),
    onSuccess: (tok) => {
      setEnrollingCaller(null)
      setEnrolment(tok)
    },
    onError: (err) => {
      setEnrollingCaller(null)
      showToast(err instanceof Error ? err.message : 'Could not mint an enrolment token')
    },
  })

  const revokeM = useMutation({
    mutationFn: (name: string) => api.callers.revoke(name),
    onSuccess: (_, name) => {
      setRevoking(null)
      qc.invalidateQueries({ queryKey: ['callers'] })
      showToast(`Caller revoked: ${name}`)
    },
  })

  const registerM = useMutation({
    mutationFn: ({ name, canMutate }: { name: string; canMutate: boolean }) =>
      api.callers.register(name, canMutate),
    onSuccess: (_, vars) => {
      qc.invalidateQueries({ queryKey: ['callers'] })
      setAddOpen(false)
      showToast(`Registered caller: ${vars.name}`)
    },
  })

  return (
    <PageShell
      title="Callers"
      sub="registered identities · mTLS"
      action={isAdmin && (
        <button className="btn btn--brass" onClick={() => setAddOpen(true)}>
          <Plus size={14} />
          <span>Add caller</span>
        </button>
      )}
    >
        <div className="page__bodyinner">
          {error && (
            <div className="banner banner--error" style={{ marginBottom: 16 }}>
              {(error as Error).message}
            </div>
          )}

          {isLoading ? (
            <div
              className="callergrid"
              style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(300px, 1fr))', gap: 14 }}
            >
              {[0, 1, 2, 3, 4, 5].map(i => (
                <div key={i} className="sk" style={{ height: 184 }} />
              ))}
            </div>
          ) : !data?.callers?.length ? (
            <div className="empty">
              <div className="empty__title">No callers registered</div>
              <div className="empty__sub">
                Run <span className="mono">tide apply</span> from a caller repo, or click
                <span className="brass" style={{ marginLeft: 4 }}>Add caller</span> above to register one.
              </div>
            </div>
          ) : (
            <div className="callergrid">
              {data.callers.map(c => (
                <CallerCard
                  key={c.caller}
                  caller={c}
                  canAdmin={isAdmin}
                  onEnrol={() => { setEnrollingCaller(c.caller); enrolM.mutate(c.caller) }}
                  isEnrolling={enrollingCaller === c.caller}
                  enrolState={enrolControlState(certs, c.caller)}
                  enrolledAt={certs?.certs.find(x => x.caller === c.caller)?.issued_at}
                  onRevoke={() => setRevoking(c.caller)}
                  onManageAliases={() => setAliasEditing(c.caller)}
                />
              ))}
            </div>
          )}
        </div>

      {addOpen && (
        <AddDialog
          onCancel={() => setAddOpen(false)}
          onRegister={(name, canMutate) => registerM.mutate({ name, canMutate })}
          isPending={registerM.isPending}
          errorMsg={registerM.error ? (registerM.error as Error).message : null}
        />
      )}

      {revoking && (
        <RevokeDialog
          name={revoking}
          isPending={revokeM.isPending}
          onCancel={() => setRevoking(null)}
          onConfirm={() => revokeM.mutate(revoking)}
        />
      )}

      {enrolment && (
        <EnrolDialog
          token={enrolment}
          endpoint={instance?.endpoint}
          onClose={() => { setEnrolment(null); qc.invalidateQueries({ queryKey: ['caller-certs'] }) }}
          showToast={showToast}
        />
      )}

      {aliasEditing && (
        <AliasesDialog
          caller={aliasEditing}
          onClose={(saved) => {
            setAliasEditing(null)
            if (saved) showToast(`Aliases updated: ${aliasEditing}`)
          }}
        />
      )}

      {toast && (
        <div className="toast-wrap">
          <div className="toast">
            <span className="dot" />
            <span>{toast}</span>
          </div>
        </div>
      )}
    </PageShell>
  )
}

// ── Single card (design's .callercard) ──────────────────────────────────
function CallerCard({
  caller,
  canAdmin,
  onEnrol,
  isEnrolling,
  enrolState,
  enrolledAt,
  onRevoke,
  onManageAliases,
}: {
  caller: CallerInfo
  canAdmin: boolean
  onEnrol: () => void
  isEnrolling: boolean
  enrolState: EnrolControl
  /** When this console last enrolled this caller, if it ever did. Absent is the
   * normal case: enrolment is new, and the issuance path it replaces answered
   * 503 in every deployment it ever ran in. */
  enrolledAt?: string
  onRevoke: () => void
  onManageAliases: () => void
}) {
  // cert_expires_at is populated whenever a cert is issued through the
  // console — see `RecordCallerCertExpiry` on the server. Callers whose cert
  // was minted out-of-band (`make dev-caller-cert`, or straight from the
  // signer) have an absent value and the meter renders in its "unknown" state.
  const certDays = (() => {
    if (!caller.cert_expires_at) return undefined
    const exp = new Date(caller.cert_expires_at).getTime()
    if (isNaN(exp)) return undefined
    return Math.max(0, Math.round((exp - Date.now()) / 86_400_000))
  })()
  const certTotal = 365
  const pct = certDays != null
    ? Math.max(0, Math.min(100, Math.round((certDays / certTotal) * 100)))
    : 0

  const versionStr = caller.schema_version
    ? `v${String(caller.schema_version).padStart(4, '0')}`
    : '—'

  return (
    <div className="callercard" data-caller={caller.caller}>
      <div className="callercard__top">
        <span className="dot dot--brass" />
        <span className="callercard__name">{caller.caller}</span>
        {!caller.can_mutate && (
          <span className="badge badge--plain">read-only</span>
        )}
        {caller.registered === false && (
          <span className="badge badge--plain">implicit</span>
        )}
        <span className="spacer" style={{ flex: 1 }} />
        {canAdmin && (
          <>
            <HoverInfo
              side="bottom"
              inline
              content={
                <>
                  <p>Configure identity aliases — let this cert satisfy <code className="mono">visible_to</code> for other names.</p>
                  <p className="hi-foot">Operator-controlled. Auth still uses the cert CN; aliases only widen what visible_to predicates match.</p>
                </>
              }
            >
              <button
                className="btn btn--sm btn--ghost btn--icon"
                onClick={onManageAliases}
                aria-label="Manage aliases"
              >
                <Link2 size={13} />
              </button>
            </HoverInfo>
            <HoverInfo
              side="bottom"
              inline
              content={
                enrolState === 'unconfigured' ? (
                  <>
                    <p>Certificate enrolment is not configured on this console.</p>
                    <p className="hi-foot">An operator sets a signer address and the enrolment listener.</p>
                  </>
                ) : (
                  <>
                    <p>Mint a single-use enrolment token. The machine generates its own key and sends only a certificate request — no private key leaves it.</p>
                    <p className="hi-foot">
                      {enrolState === 'reenrol' && enrolledAt
                        ? `Last enrolled ${enrolledAt.slice(0, 10)}. Enrolling again supersedes that certificate — atlantis stops authenticating it.`
                        : 'Enrolling supersedes whatever certificate this caller is using now — atlantis stops authenticating it.'}
                    </p>
                  </>
                )
              }
            >
              <button
                className="btn btn--sm btn--ghost btn--icon"
                onClick={onEnrol}
                disabled={isEnrolling || enrolState === 'unconfigured'}
                aria-label="Enrol a machine"
              >
                {isEnrolling ? <span className="spin" /> : <Key size={13} />}
              </button>
            </HoverInfo>
            <HoverInfo
              side="bottom"
              inline
              content={
                <>
                  <p>Drops this caller's identity and any files they registered.</p>
                  <p className="hi-foot">Every cert for this caller stops authenticating — reads and writes both fail.</p>
                </>
              }
            >
              <button
                className="btn btn--sm btn--danger btn--icon"
                onClick={onRevoke}
                aria-label="Revoke caller"
              >
                <Trash2 size={13} />
              </button>
            </HoverInfo>
          </>
        )}
      </div>

      <div className="callercard__grid">
        <div className="cstat">
          <span className="cstat__l">files</span>
          <span className="cstat__v">{caller.file_count || '—'}</span>
        </div>
        <div className="cstat">
          <span className="cstat__l">schema</span>
          <span className="cstat__v">{versionStr}</span>
        </div>
        <div className="cstat">
          <span className="cstat__l">last applied</span>
          <span className="cstat__v">{fmtDateShort(caller.last_applied_at)}</span>
        </div>
        <div className="cstat">
          <span className="cstat__l">can apply</span>
          <span className="cstat__v">{caller.can_mutate ? 'yes' : 'no'}</span>
        </div>
      </div>

      <div className={`cert-meter ${certClass(certDays)}`}>
        <div className="row" style={{ justifyContent: 'space-between' }}>
          <span className="cstat__l" style={{ whiteSpace: 'nowrap' }}>
            cert · {certLabel(certDays)}
          </span>
          <span
            className="mono num"
            style={{ fontSize: 11.5, color: 'var(--ink-2)', whiteSpace: 'nowrap' }}
          >
            {certDays != null ? `${certDays}d left` : '—'}
          </span>
        </div>
        <div className="cert-meter__bar">
          <div className="cert-meter__fill" style={{ width: `${pct}%` }} />
        </div>
      </div>
    </div>
  )
}

// ── Add caller modal (.modal pattern from console.css) ────────────────────
function AddDialog({
  onCancel,
  onRegister,
  isPending,
  errorMsg,
}: {
  onCancel: () => void
  onRegister: (name: string, canMutate: boolean) => void
  isPending: boolean
  errorMsg: string | null
}) {
  const [name, setName] = useState('')
  const [canMutate, setCanMutate] = useState(true)
  const valid = !name || CALLER_NAME_RE.test(name.trim())

  return (
    <div className="overlay is-open" onMouseDown={e => { if (e.target === e.currentTarget) onCancel() }}>
      <div className="modal" role="dialog" aria-modal>
        <div className="modal__head">
          <div className="modal__title">Register caller</div>
          <div className="modal__sub">A caller owns a namespace and connects over mTLS.</div>
        </div>
        <div className="modal__body">
          <div className="field">
            <label className="field__label">Caller name</label>
            <input
              className={`input mono ${!valid ? '' : ''}`}
              placeholder="service-name"
              value={name}
              onChange={e => setName(e.target.value.toLowerCase())}
              autoFocus
            />
            {!valid && (
              <span className="coral" style={{ fontSize: 11.5, marginTop: 4 }}>
                Lowercase letters, digits, interior hyphens only.
              </span>
            )}
          </div>

          <label className="checkbox">
            <input type="checkbox" checked={canMutate} onChange={e => setCanMutate(e.target.checked)} />
            <span className="checkbox__box"><Check size={11} /></span>
            <span>
              Can apply schema
              <span className="faint" style={{ fontSize: 11.5, marginLeft: 4 }}>
                — grants <span className="mono">tide apply</span>
              </span>
            </span>
          </label>

          {errorMsg && (
            <div className="banner banner--error" style={{ marginTop: 2 }}>
              <span className="banner__icon" />
              <span>{errorMsg}</span>
            </div>
          )}
        </div>
        <div className="modal__foot">
          <button className="btn" onClick={onCancel}>Cancel</button>
          <button
            className="btn btn--brass"
            disabled={!name || !valid || isPending}
            onClick={() => onRegister(name.trim(), canMutate)}
          >
            <Plus size={14} />
            <span>{isPending ? 'Registering…' : 'Register'}</span>
          </button>
        </div>
      </div>
    </div>
  )
}

// ── Revoke confirmation ──────────────────────────────────────────────────
function RevokeDialog({
  name,
  isPending,
  onCancel,
  onConfirm,
}: {
  name: string
  isPending: boolean
  onCancel: () => void
  onConfirm: () => void
}) {
  return (
    <div className="overlay is-open" onMouseDown={e => { if (e.target === e.currentTarget) onCancel() }}>
      <div className="modal" role="dialog" aria-modal>
        <div className="modal__head">
          <div className="modal__title">Revoke caller?</div>
          <div className="modal__sub">
            Removes <span className="mono brass">{name}</span> from the identity table and clears
            all of its registered files. Every cert minted for this caller stops
            authenticating immediately — both reads and writes fail.
          </div>
        </div>
        <div className="modal__foot">
          <button className="btn" onClick={onCancel} disabled={isPending}>Cancel</button>
          <button className="btn btn--danger" onClick={onConfirm} disabled={isPending}>
            {isPending ? 'Revoking…' : 'Revoke caller'}
          </button>
        </div>
      </div>
    </div>
  )
}

// ── Enrolment dialog ────────────────────────────────────────────────────
//
// What this shows is a token and a command. What it does NOT show — and what
// the dialog it replaces did show — is a private key.
//
// That dialog offered three downloads: certificate, key, CA bundle. The key had
// been generated inside the console, sent across the network, and held in this
// tab's memory until somebody clicked. It never needed to be here: the signer
// has only ever accepted a certificate request, so the keypair can be born on
// the machine that will use it and stay there.
function EnrolDialog({
  token,
  endpoint,
  onClose,
  showToast,
}: {
  token: EnrollTokenResponse
  endpoint?: string
  onClose: () => void
  showToast: (msg: string) => void
}) {
  const exp = new Date(token.expires_at)
  const expStr = isNaN(exp.getTime())
    ? token.expires_at
    : exp.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })

  const cmd = `tide login \\
  --org ${token.org} \\
  --caller ${token.caller} \\
  --token ${token.token}`

  return (
    <div className="overlay is-open" onMouseDown={e => { if (e.target === e.currentTarget) onClose() }}>
      <div className="modal" role="dialog" aria-modal style={{ width: 560 }}>
        <div className="modal__head">
          <div className="modal__title">Enrol a machine — {token.caller}</div>
          <div className="modal__sub">
            Run this on the machine that will hold the certificate. It generates its own
            key, which never leaves it. The token works once and expires at {expStr}.
          </div>
        </div>
        <div className="modal__body">
          {/* Unconditional, and it has to be. Enrolling supersedes whatever
              certificate this caller is using — and the console cannot see
              atlantis's fingerprint, so it cannot know whether there is one to
              supersede. Saying "this may replace something" only when we happen
              to know would be silent in exactly the case that hurts. */}
          <div className="banner banner--warn" style={{ marginBottom: 14 }}>
            Completing this enrolment replaces the certificate <span className="mono">{token.caller}</span> is
            using now. atlantis stops authenticating the old one as soon as the new one is recorded — any
            machine still holding it starts failing.
          </div>

          <div className="section-label" style={{ marginBottom: 8 }}>On the target machine</div>
          <pre style={{
            margin: 0, padding: '13px 15px', background: 'var(--canvas-0)',
            border: '1px solid var(--line-soft)', borderRadius: 'var(--radius)',
            fontFamily: 'var(--mono)', fontSize: 12, color: 'var(--ink-1)', lineHeight: 1.7,
            position: 'relative', whiteSpace: 'pre-wrap', wordBreak: 'break-all',
          }}>
            {cmd}
            <button
              className="btn btn--sm btn--ghost btn--icon"
              style={{ position: 'absolute', top: 9, right: 9 }}
              onClick={() => { navigator.clipboard.writeText(cmd); showToast('Copied the enrolment command') }}
              aria-label="Copy the enrolment command"
            >
              <Copy size={13} />
            </button>
          </pre>

          <p className="faint" style={{ fontSize: 12, marginTop: 12, lineHeight: 1.6 }}>
            The token is shown once and is not recoverable — the console stores only its hash.
            {endpoint && <> This organisation's atlantis is <span className="mono">{endpoint}</span>.</>}
          </p>
        </div>
        <div className="modal__foot">
          <button className="btn btn--brass" onClick={onClose}>Done</button>
        </div>
      </div>
    </div>
  )
}

// ── Aliases dialog ──────────────────────────────────────────────────────
//
// Aliases let a registered caller's cert CN satisfy `visible_to`
// predicates declared for other identity names. PostgreSQL-roles /
// AD-SID / DNS-CNAME pattern: schemas reference roles, deploy-time
// config maps physical identities to those roles. Identity rename
// doesn't require schema edits.
//
// Aliases are operator-controlled (sudo-gated on save) and never
// substitute for authentication — they only widen the authz match set.
function AliasesDialog({
  caller,
  onClose,
}: {
  caller: string
  onClose: (saved: boolean) => void
}) {
  const qc = useQueryClient()
  const { data, isLoading, error } = useQuery({
    queryKey: ['caller-aliases', caller],
    queryFn: () => api.callers.aliases(caller),
  })

  const [draft, setDraft] = useState<string[] | null>(null)
  const [newAlias, setNewAlias] = useState('')
  const [showSudo, setShowSudo] = useState(false)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)

  // Hydrate draft once data arrives. Subsequent loads (e.g. background
  // refetch) shouldn't clobber unsaved edits.
  useEffect(() => {
    if (data && draft === null) setDraft([...data.aliases])
  }, [data, draft])

  const current = draft ?? data?.aliases ?? []
  const dirty = data ? !arraysEqual(current, data.aliases) : false

  const onAdd = () => {
    const a = newAlias.trim()
    if (!a) return
    if (a === caller) {
      setSaveError(`Alias cannot equal the caller name (${caller}).`)
      return
    }
    if (current.includes(a)) {
      setNewAlias('')
      return
    }
    setDraft([...current, a])
    setNewAlias('')
    setSaveError(null)
  }

  const onRemove = (a: string) => {
    setDraft(current.filter(x => x !== a))
    setSaveError(null)
  }

  const save = async (password: string) => {
    setSaving(true)
    setSaveError(null)
    try {
      await api.auth.sudo(password)
      await api.callers.setAliases(caller, current)
      qc.invalidateQueries({ queryKey: ['caller-aliases', caller] })
      setShowSudo(false)
      onClose(true)
    } catch (e) {
      setSaveError(e instanceof ApiError ? e.message : 'save failed')
    } finally {
      setSaving(false)
    }
  }

  // Render sudo gate alone (not stacked) when the operator hits Save —
  // mirrors the Settings page's "one modal at a time" pattern. Aliases
  // dialog state is kept in this closure so cancel-from-sudo returns
  // the operator to the same editing state, not a fresh load.
  if (showSudo) {
    return (
      <SudoConfirmDialog
        title="Save aliases"
        icon={<Link2 size={18} />}
        body={
          <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
            <p>
              Replacing the alias set for <span className="mono">{caller}</span>.
              {' '}This widens (or narrows) the <code className="mono">visible_to</code> match set
              immediately for all future authz checks.
            </p>
            <p className="muted" style={{ fontSize: 12 }}>
              Cert authentication is unchanged. Aliases only affect what visible_to predicates this
              cert satisfies at job-dispatch authz time.
            </p>
          </div>
        }
        confirmLabel="Save"
        pending={saving}
        error={saveError}
        onCancel={() => setShowSudo(false)}
        onConfirm={save}
      />
    )
  }

  return (
    <div
      className="overlay is-open"
      onMouseDown={e => { if (e.target === e.currentTarget) onClose(false) }}
    >
      <div className="modal" style={{ width: 520 }} role="dialog" aria-modal>
          <div className="modal__head">
            <div className="row" style={{ gap: 10, alignItems: 'center' }}>
              <Link2 size={16} />
              <span className="modal__title">Identity aliases</span>
            </div>
            <div className="modal__sub">
              <span className="mono">{caller}</span> — additional names this cert satisfies for{' '}
              <code className="mono">visible_to</code>
            </div>
          </div>
          <div className="modal__body">
            {isLoading && <div className="muted">Loading…</div>}
            {error && (
              <div className="banner banner--error">
                {(error as Error).message}
              </div>
            )}

            {!isLoading && !error && (
              <>
                <div className="field">
                  <label className="field__label">Current aliases</label>
                  {current.length === 0 ? (
                    <div className="faint" style={{ fontSize: 12, padding: '6px 0' }}>
                      No aliases. Only <span className="mono">visible_to "{caller}"</span> and{' '}
                      <span className="mono">visible_to "*"</span> match this cert today.
                    </div>
                  ) : (
                    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
                      {current.map(a => (
                        <span
                          key={a}
                          className="badge badge--plain mono"
                          style={{
                            display: 'inline-flex',
                            alignItems: 'center',
                            gap: 6,
                            padding: '4px 4px 4px 10px',
                            background: 'var(--canvas-3)',
                            border: '1px solid var(--line-soft)',
                            borderRadius: 14,
                          }}
                        >
                          {a}
                          <button
                            type="button"
                            onClick={() => onRemove(a)}
                            className="btn btn--ghost btn--icon"
                            style={{ width: 18, height: 18, padding: 0, color: 'var(--ink-3)' }}
                            aria-label={`Remove alias ${a}`}
                          >
                            <X size={11} />
                          </button>
                        </span>
                      ))}
                    </div>
                  )}
                </div>

                <div className="field">
                  <label className="field__label">Add alias</label>
                  <div className="row" style={{ gap: 6 }}>
                    <input
                      className="input mono"
                      value={newAlias}
                      onChange={e => setNewAlias(e.target.value)}
                      onKeyDown={e => { if (e.key === 'Enter') onAdd() }}
                      placeholder="e.g. vendor"
                      autoFocus
                      spellCheck={false}
                      autoComplete="off"
                      style={{ flex: 1 }}
                    />
                    <button
                      className="btn btn--ghost btn--sm"
                      onClick={onAdd}
                      disabled={!newAlias.trim()}
                    >
                      <Plus size={12} /> Add
                    </button>
                  </div>
                  <div className="faint" style={{ fontSize: 11, marginTop: 6, lineHeight: 1.5 }}>
                    A schema declaration of <span className="mono">visible_to "X"</span> matches when
                    the cert CN is X or X appears in this alias list. Reserved names (atlantis*,
                    anonymous, *) are rejected.
                  </div>
                </div>

                {saveError && (
                  <div className="banner banner--error">{saveError}</div>
                )}
              </>
            )}
          </div>
          <div className="modal__foot">
            <button
              className="btn btn--ghost"
              onClick={() => onClose(false)}
              disabled={saving}
            >
              Cancel
            </button>
            <button
              className="btn btn--brass"
              onClick={() => { setSaveError(null); setShowSudo(true) }}
              disabled={!dirty || isLoading}
            >
              {dirty ? 'Save changes' : 'No changes'}
            </button>
          </div>
        </div>
      </div>
  )
}

function arraysEqual(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false
  const as = [...a].sort()
  const bs = [...b].sort()
  for (let i = 0; i < as.length; i++) {
    if (as[i] !== bs[i]) return false
  }
  return true
}
