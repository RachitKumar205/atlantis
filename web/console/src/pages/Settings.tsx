import { useEffect, useRef, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Buildings,
  Copy,
  Fingerprint,
  HardDrives,
  Lock,
  Monitor,
  Shield,
  ShieldCheck,
  SignOut,
  Snowflake,
  Users,
  Warning,
} from '@phosphor-icons/react'
import {
  api,
  queries,
  type ChangePolicyEntry,
  type FreezeWindow,
  type MeResult,
} from '@/api/client'
import { useMe } from '@/hooks/useAuth'
import { STEP_UP_MESSAGE } from '@/pages/Login'
import { PageShell } from '@/components/PageShell'

// Sectioned IA: left sub-nav (General / Members / Security / Danger
// zone) drives the right panel. Every panel uses the .setrow pattern:
// label + help text on the left, control on the right.
//
// There was a fifth section, Integrations, which mapped each caller to a
// GitHub owner/repo/branch for the console's "Open PR" button. It was written,
// never shown in the nav, and is now deleted along with the flow it served —
// the console does not author schema, so it has no reason to hold a repository
// address. Schema files live in the customer's own git repo.
type SectionId = 'general' | 'members' | 'security' | 'policy' | 'danger'

interface Section {
  id: SectionId
  label: string
  icon: React.ReactNode
  danger?: boolean
}

const SECTIONS: Section[] = [
  { id: 'general',  label: 'General',     icon: <Buildings /> },
  { id: 'members',  label: 'Members',     icon: <Users /> },
  { id: 'security', label: 'Security',    icon: <Shield /> },
  { id: 'policy',   label: 'Change policy', icon: <ShieldCheck /> },
  { id: 'danger',   label: 'Danger zone', icon: <Warning />, danger: true },
]

export function Settings() {
  const navigate = useNavigate()
  const { data: me } = useMe()

  const [active, setActive] = useState<SectionId>('general')
  const [toast, setToast] = useState<string | null>(null)
  const fire = (msg: string) => { setToast(msg); setTimeout(() => setToast(null), 2200) }

  return (
    <PageShell title="Settings" sub="general · members · security · change policy">
      <div className="page__bodyinner">
        <div className="settings">
          <nav className="set-nav">
            {SECTIONS.map(s => (
              <button
                key={s.id}
                className={`set-nav__item ${s.danger ? 'is-danger' : ''} ${active === s.id ? 'is-active' : ''}`}
                onClick={() => setActive(s.id)}
                type="button"
              >
                {s.icon}
                <span>{s.label}</span>
              </button>
            ))}
          </nav>

          <div className="set-content">
            <div className={`set-panel ${active === 'general' ? 'is-active' : ''}`}>
              <GeneralPanel onToast={fire} />
            </div>
            <div className={`set-panel ${active === 'members' ? 'is-active' : ''}`}>
              <MembersPanel me={me} />
            </div>
            <div className={`set-panel ${active === 'security' ? 'is-active' : ''}`}>
              <SecurityPanel onToast={fire} me={me} />
            </div>
            <div className={`set-panel ${active === 'policy' ? 'is-active' : ''}`}>
              <ChangePolicyPanel onToast={fire} isAdmin={me?.role === 'admin'} />
              <ProtectedEntitiesPanel onToast={fire} isAdmin={me?.role === 'admin'} />
              <FreezeWindowsPanel onToast={fire} isAdmin={me?.role === 'admin'} />
            </div>
            <div className={`set-panel ${active === 'danger' ? 'is-active' : ''}`}>
              <DangerPanel
                onToast={fire}
                onAfterSignOutAll={() => navigate({ to: '/login' })}
              />
            </div>
          </div>
        </div>
      </div>

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

// ── General ──────────────────────────────────────────────────────────────
function GeneralPanel({ onToast }: { onToast: (msg: string) => void }) {
  // Endpoint comes from the BFF's /api/instance (which reads ATL_ENDPOINT).
  // HardDrives version comes from the live health probe — same source the
  // Health page's "version" chip uses, so the two surfaces always agree.
  const instanceQ = useQuery(queries.instance())
  const healthQ = useQuery({ ...queries.health(), refetchInterval: 30_000 })

  const endpoint = instanceQ.data?.endpoint ?? '—'
  const schemaVer = healthQ.data?.atlantis?.schema_version
  const versionStr = schemaVer ? `v${String(schemaVer).padStart(4, '0')}` : '—'
  const status = healthQ.data?.atlantis?.status
  const isHealthy = status === 'healthy'

  return (
    <>
      <div className="set-head">
        <h2>General</h2>
        <p>Identity and connection details for this Atlantis instance. Used by every caller that applies a schema.</p>
      </div>
      <section className="card">
        <div className="card__head">
          <Buildings size={14} />
          <span className="card__title">Instance</span>
        </div>
        <div className="card__body" style={{ padding: 0 }}>
          <div className="setrow">
            <div className="setrow__main">
              <div className="setrow__label">Endpoint</div>
              <div className="setrow__help">gRPC address callers connect to over mTLS.</div>
            </div>
            <div className="setrow__control">
              <span className="set-readout">
                <HardDrives />
                {endpoint}
              </span>
              <button
                className="btn btn--sm btn--ghost btn--icon"
                title="Copy"
                onClick={() => {
                  if (endpoint === '—') return
                  navigator.clipboard.writeText(endpoint)
                  onToast('Endpoint copied')
                }}
                disabled={endpoint === '—'}
              >
                <Copy size={13} />
              </button>
            </div>
          </div>

          <div className="setrow">
            <div className="setrow__main">
              <div className="setrow__label">Schema version</div>
              <div className="setrow__help">Latest applied schema. Advances each time a caller runs <span className="mono">tide apply</span>.</div>
            </div>
            <div className="setrow__control">
              <span className="set-readout mono">{versionStr}</span>
              {isHealthy && (
                <span className="row" style={{ gap: 6, color: 'var(--sage)', fontSize: 12 }}>
                  <span className="dot dot--sage" />
                  Healthy
                </span>
              )}
              {status && status !== 'healthy' && (
                <span className="row" style={{ gap: 6, color: 'var(--coral)', fontSize: 12 }}>
                  <span className="dot dot--coral" />
                  {status}
                </span>
              )}
            </div>
          </div>
        </div>
      </section>
    </>
  )
}

// ── Members ──────────────────────────────────────────────────────────────

// Read-only, and that is the design rather than a gap.
//
// This console used to invite operators, set their roles and delete them. It
// cannot any more, because it holds no accounts: membership and roles belong
// to the organisation at Atlantis Cloud, and arrive here inside each
// assertion. Keeping an editable copy would mean two systems disagreeing about
// who may do what, with the console's copy winning by virtue of being the one
// enforcing it.
function MembersPanel({ me }: { me?: MeResult }) {
  return (
    <>
      <div className="set-head">
        <h2>Members</h2>
        <p>Who can reach this console is decided by your organisation at Atlantis Cloud. This page shows what your current session was granted.</p>
      </div>

      <section className="card">
        <div className="card__head">
          <Users size={14} />
          <span className="card__title">This session</span>
        </div>
        <div className="card__body" style={{ padding: 0 }}>
          <div className="setrow">
            <div className="setrow__main">
              <div className="setrow__label">Signed in as</div>
              <div className="setrow__help">{me?.email ?? '—'}</div>
            </div>
            <div className="setrow__control">
              <span className="set-readout">{me?.name || '—'}</span>
            </div>
          </div>

          <div className="setrow">
            <div className="setrow__main">
              <div className="setrow__label">Organisation</div>
              <div className="setrow__help">
                Every request from this session is scoped to it. This console serves this
                organisation only;{' '}
                {(me?.orgs?.length ?? 0) > 1
                  ? 'use the switcher in the sidebar to sign in to another one you belong to.'
                  : 'an account in more than one gets a switcher in the sidebar.'}
              </div>
            </div>
            <div className="setrow__control">
              <span className="set-readout">{me?.org ?? '—'}</span>
            </div>
          </div>

          <div className="setrow">
            <div className="setrow__main">
              <div className="setrow__label">Role</div>
              <div className="setrow__help">
                Granted by Atlantis Cloud and fixed for this session. A change there takes effect the next time you sign in.
              </div>
            </div>
            <div className="setrow__control">
              <span className={`badge ${me?.role === 'admin' ? 'badge--add' : ''}`}>{me?.role ?? '—'}</span>
            </div>
          </div>
        </div>
      </section>
    </>
  )
}

// ── Security ─────────────────────────────────────────────────────────────
function SecurityPanel({
  onToast,
  me,
}: {
  onToast: (msg: string) => void
  // Carries step_up_url, which is Cloud's address with this session's
  // organisation on it. Passed rather than fetched again, matching
  // MembersPanel: two useMe() calls in one page is two cache reads that can
  // disagree mid-render.
  me?: MeResult
}) {
  // mTLS toggle is dormant — mTLS is always required at the gRPC layer
  // today, so a UI toggle would either be a no-op or introduce a real
  // permissive mode that weakens trust. State + JSX are preserved below
  // (commented) so we can re-enable cleanly once a permissive mode is
  // implemented end-to-end.
  // const [mtlsRequired, setMtlsRequired] = useState(true)

  const signOutOthers = useMutation({
    mutationFn: () => api.auth.signOutOthers(),
    onSuccess: (res) =>
      onToast(
        res.sessions_removed === 0
          ? 'No other sessions to sign out'
          : `Signed out ${res.sessions_removed} other session${res.sessions_removed === 1 ? '' : 's'}`,
      ),
    onError: (err: Error) => onToast(err.message),
  })

  return (
    <>
      <div className="set-head">
        <h2>Security</h2>
        <p>Sessions on this console, and the transport policy callers must satisfy. mTLS settings here apply to every registered caller.</p>
      </div>

      <section className="card">
        <div className="card__head">
          <Lock size={14} />
          <span className="card__title">Authentication</span>
        </div>
        <div className="card__body" style={{ padding: 0 }}>
          <div className="setrow">
            <div className="setrow__main">
              <div className="setrow__label">Password</div>
              <div className="setrow__help">
                Held by Atlantis Cloud, not by this console. Change it where you sign in.
              </div>
            </div>
            <div className="setrow__control">
              {/* step_up_url is Cloud's address with this session's org on it,
                  which is the account the password belongs to. */}
              {me?.step_up_url ? (
                <a className="set-readout" href={me.step_up_url}>Manage at Atlantis Cloud</a>
              ) : (
                <span className="set-readout">Managed by Atlantis Cloud</span>
              )}
            </div>
          </div>

          <div className="setrow">
            <div className="setrow__main">
              <div className="setrow__label">Session lifetime</div>
              <div className="setrow__help">How long a console session stays valid before re-authentication.</div>
            </div>
            <div className="setrow__control">
              <select
                className="input--boxed"
                defaultValue="12 hours"
                onChange={e => onToast(`Session lifetime → ${e.target.value}`)}
              >
                <option>4 hours</option>
                <option>12 hours</option>
                <option>24 hours</option>
                <option>7 days</option>
              </select>
            </div>
          </div>

          {/* mTLS toggle dormant — see SecurityPanel comment. Restore:
          <div className="setrow">
            <div className="setrow__main">
              <div className="setrow__label">Require mTLS for all callers</div>
              <div className="setrow__help">Reject any caller that connects without a valid client certificate.</div>
            </div>
            <div className="setrow__control">
              <label className="switch">
                <input
                  type="checkbox"
                  checked={mtlsRequired}
                  onChange={e => {
                    setMtlsRequired(e.target.checked)
                    onToast(e.target.checked ? 'mTLS now required for all callers' : 'mTLS requirement disabled')
                  }}
                />
                <span className="switch__track" />
              </label>
            </div>
          </div> */}
        </div>
      </section>

      <section className="card">
        <div className="card__head">
          <Monitor size={14} />
          <span className="card__title">Sessions</span>
        </div>
        <div className="card__body" style={{ padding: 0 }}>
          <div className="setrow">
            <div className="setrow__main">
              <div className="setrow__label">
                This device <span className="badge badge--add" style={{ marginLeft: 6 }}>current</span>
              </div>
              <div className="setrow__help">macOS · last active just now</div>
            </div>
            <div className="setrow__control">
              <span className="set-readout">
                <Fingerprint />
                mTLS verified
              </span>
            </div>
          </div>

          <div className="setrow">
            <div className="setrow__main">
              <div className="setrow__label">Other sessions</div>
              <div className="setrow__help">Sign out of every console session except this one.</div>
            </div>
            <div className="setrow__control">
              <button
                className="btn btn--sm"
                onClick={() => signOutOthers.mutate()}
                disabled={signOutOthers.isPending}
              >
                {signOutOthers.isPending ? 'Signing out…' : 'Sign out others'}
              </button>
            </div>
          </div>
        </div>
      </section>

    </>
  )
}

// ── Danger ───────────────────────────────────────────────────────────────
// CLASS_LABELS names each class the way `tide plan` does, so an operator
// reading a refused plan and an operator reading this page see the same word.
// The keys are the proto enum names the server stores and returns.
const CLASS_LABELS: Record<string, { name: string; help: string }> = {
  PLAN_CLASS_ADDITIVE: {
    name: 'Additive',
    help: 'New entities, new nullable fields, new indexes. Nothing existing changes shape.',
  },
  PLAN_CLASS_BACKFILL_REQUIRED: {
    name: 'Backfill required',
    help: 'A new NOT NULL column without a default, or a new composite unique. Existing rows have to be filled in or deduplicated first.',
  },
  PLAN_CLASS_CROSS_CALLER_BREAKING: {
    name: 'Cross-caller breaking',
    help: 'Removes or renames something another team reads. Their next deploy fails, not this one.',
  },
  PLAN_CLASS_DESTRUCTIVE: {
    name: 'Destructive',
    help: 'Drops something that holds rows. Parked rather than deleted, and recoverable until the retention window closes.',
  },
}

// ChangePolicyPanel — which classes of schema change may apply unattended.
//
// The rule is stored by the admin server, not the console, because the thing
// that enforces it is `tide apply`. This page is a view onto that table.
//
// It IS enforced. The admin server's gateOnChangePolicy reads this table inside
// the transaction that would run the DDL, on both ApplyMigration and
// BeginBackfillPlan, and refuses — recording a plan for a reviewer.
//
// The row below says so on the page rather than leaving it implied. This panel
// shipped one release before enforcement did, saying "not yet enforced" for
// exactly the same reason: an operator toggling a control that decides whether
// production DDL runs unattended should be able to read what it does now, not
// infer it from a release note.
function ChangePolicyPanel({ onToast, isAdmin }: { onToast: (msg: string) => void; isAdmin: boolean }) {
  const qc = useQueryClient()
  const [pending, setPending] = useState<ChangePolicyEntry | null>(null)

  const { data, isLoading } = useQuery({
    queryKey: ['change-policy'],
    queryFn: api.policy.get,
  })

  // Sudo first, then the write — the same two-step the danger zone uses. This
  // is the control that decides whether production DDL runs without a human,
  // so a stolen session cookie alone must not be enough to relax it.
  const save = useMutation({
    mutationFn: async ({ entry, assertion }: { entry: ChangePolicyEntry; assertion: string }) => {
      await api.auth.sudo(assertion)
      return api.policy.set([entry])
    },
    onSuccess: (_res, { entry }) => {
      qc.invalidateQueries({ queryKey: ['change-policy'] })
      const label = CLASS_LABELS[entry.change_class]?.name ?? entry.change_class
      onToast(entry.require_approval
        ? `${label} changes now need approval`
        : `${label} changes now apply unattended`)
      setPending(null)
    },
  })

  const entries = data?.entries ?? []

  return (
    <>
      <div className="set-head">
        <h2>Change policy</h2>
        <p>
          Which classes of schema change may apply without a human. `tide plan` reports the
          rule for the class it produced, so an engineer sees it before they run `tide apply`.
        </p>
      </div>

      <div className="setrow">
        <div className="setrow__main">
          <div className="setrow__label">Enforced on apply</div>
          <div className="setrow__help">
            A change in a class that needs approval is refused before its DDL runs, and
            recorded. `tide apply` exits 2 — the gate working, not a failure — and the
            request appears under Approvals for a reviewer to decide.
          </div>
        </div>
      </div>

      {isLoading && <div className="setrow"><div className="setrow__main">Loading…</div></div>}

      {entries.map(e => {
        const meta = CLASS_LABELS[e.change_class]
        return (
          <div className="setrow" key={e.change_class}>
            <div className="setrow__main">
              <div className="setrow__label">{meta?.name ?? e.change_class}</div>
              <div className="setrow__help">
                {meta?.help ?? 'A class this console does not have a description for.'}
                {e.updated_by && (
                  <> {' '}<span className="muted">Last changed by {e.updated_by}.</span></>
                )}
              </div>
            </div>
            <div className="setrow__control">
              {/* Coral marks "applies unattended", not "needs approval".
                  Requiring a human is the safe setting; the one worth catching
                  an operator's eye as they scan the page is the class that
                  runs production DDL with nobody watching. */}
              <button
                className={`btn btn--sm ${e.require_approval ? '' : 'btn--danger'}`}
                disabled={!isAdmin || save.isPending}
                title={isAdmin ? undefined : 'Only admins can change this'}
                onClick={() => setPending({ ...e, require_approval: !e.require_approval })}
              >
                {e.require_approval ? `Needs ${e.approver_role}` : 'Applies unattended'}
              </button>
            </div>
          </div>
        )
      })}

      {pending && (
        <SudoConfirmDialog
          title={pending.require_approval ? 'Require approval' : 'Allow unattended applies'}
          icon={<ShieldCheck />}
          body={
            <p>
              {pending.require_approval
                ? <>Plans classed <strong>{CLASS_LABELS[pending.change_class]?.name ?? pending.change_class}</strong> will
                   wait for a <strong>{pending.approver_role}</strong> before they can apply.</>
                : <>Plans classed <strong>{CLASS_LABELS[pending.change_class]?.name ?? pending.change_class}</strong> will
                   apply with no human involved.</>}
            </p>
          }
          confirmLabel={pending.require_approval ? 'Require approval' : 'Allow unattended'}
          pending={save.isPending}
          error={save.error ? (save.error as Error).message : null}
          onCancel={() => { save.reset(); setPending(null) }}
          onConfirm={(assertion) => save.mutate({ entry: pending, assertion })}
        />
      )}
    </>
  )
}

function ProtectedEntitiesPanel({ onToast, isAdmin }: { onToast: (msg: string) => void; isAdmin: boolean }) {
  const qc = useQueryClient()
  const { data, isLoading } = useQuery({
    queryKey: ['protected-entities'],
    queryFn: api.protection.list,
  })
  const [pattern, setPattern] = useState('')
  const [floor, setFloor] = useState('require_approval')
  const [reason, setReason] = useState('')
  const [confirm, setConfirm] = useState<{ kind: 'put' | 'remove'; pattern: string } | null>(null)

  const act = useMutation({
    mutationFn: async ({ kind, pattern: p, assertion }: { kind: 'put' | 'remove'; pattern: string; assertion: string }) => {
      await api.auth.sudo(assertion)
      if (kind === 'put') return api.protection.put({ pattern: p, floor, reason })
      return api.protection.remove(p)
    },
    onSuccess: (_res, v) => {
      qc.invalidateQueries({ queryKey: ['protected-entities'] })
      onToast(v.kind === 'put' ? `Protected ${v.pattern}` : `Protection removed: ${v.pattern}`)
      setConfirm(null)
      if (v.kind === 'put') { setPattern(''); setReason('') }
    },
  })

  const entities = data?.entities ?? []

  return (
    <>
      <div className="set-head" style={{ marginTop: 32 }}>
        <h2>Protected entities</h2>
        <p>
          Per-entity approval floors. A change touching a protected entity waits for a human
          whatever the caller's apply policy and the change policy say, and an{' '}
          <span className="mono">admin_only</span> floor demands an admin's decision.
        </p>
      </div>

      {isLoading && <div className="setrow"><div className="setrow__main">Loading…</div></div>}
      {!isLoading && entities.length === 0 && (
        <div className="setrow"><div className="setrow__main setrow__help">
          Nothing is protected. Patterns are exact (<span className="mono">payments.Invoice</span>)
          or a whole namespace (<span className="mono">payments.*</span>).
        </div></div>
      )}

      {entities.map(e => (
        <div className="setrow" key={e.pattern}>
          <div className="setrow__main">
            <div className="setrow__label mono">{e.pattern}</div>
            <div className="setrow__help">
              {e.floor === 'admin_only' ? 'Needs an admin' : 'Needs approval'}
              {e.reason ? ` — ${e.reason}` : ''}
            </div>
          </div>
          <div className="setrow__control">
            <button
              className="btn btn--sm btn--ghost"
              disabled={!isAdmin || act.isPending}
              title={isAdmin ? undefined : 'Only admins can change this'}
              onClick={() => setConfirm({ kind: 'remove', pattern: e.pattern })}
            >
              Remove
            </button>
          </div>
        </div>
      ))}

      <div className="setrow">
        <div className="setrow__main" style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
          <input
            className="input mono"
            style={{ width: 200 }}
            placeholder="payments.*"
            value={pattern}
            onChange={e => setPattern(e.target.value)}
            disabled={!isAdmin}
          />
          <select className="input" value={floor} onChange={e => setFloor(e.target.value)} disabled={!isAdmin}>
            <option value="require_approval">needs approval</option>
            <option value="admin_only">needs an admin</option>
          </select>
          <input
            className="input"
            style={{ flex: 1, minWidth: 160 }}
            placeholder="why (shown in refusals)"
            value={reason}
            onChange={e => setReason(e.target.value)}
            disabled={!isAdmin}
          />
          <button
            className="btn btn--sm"
            disabled={!isAdmin || pattern.trim() === '' || act.isPending}
            onClick={() => setConfirm({ kind: 'put', pattern: pattern.trim() })}
          >
            Protect
          </button>
        </div>
      </div>

      {confirm && (
        <SudoConfirmDialog
          title={confirm.kind === 'put' ? 'Protect an entity' : 'Remove a protection'}
          icon={<ShieldCheck />}
          body={
            confirm.kind === 'put'
              ? <p>Changes touching <span className="mono">{confirm.pattern}</span> will wait for
                  {floor === 'admin_only' ? ' an admin' : ' an approval'}, at every tier.</p>
              : <p><span className="mono">{confirm.pattern}</span> loses its floor; the change
                  policy and each caller's apply policy still apply.</p>
          }
          confirmLabel={confirm.kind === 'put' ? 'Protect' : 'Remove'}
          pending={act.isPending}
          error={act.error ? (act.error as Error).message : null}
          onCancel={() => { act.reset(); setConfirm(null) }}
          onConfirm={(assertion) => act.mutate({ kind: confirm.kind, pattern: confirm.pattern, assertion })}
        />
      )}
    </>
  )
}

function FreezeWindowsPanel({ onToast, isAdmin }: { onToast: (msg: string) => void; isAdmin: boolean }) {
  const qc = useQueryClient()
  const { data, isLoading } = useQuery({
    queryKey: ['freeze-windows'],
    queryFn: api.freezes.list,
  })
  const [starts, setStarts] = useState('')
  const [ends, setEnds] = useState('')
  const [reason, setReason] = useState('')
  const [confirm, setConfirm] = useState<{ kind: 'create' } | { kind: 'remove'; window: FreezeWindow } | null>(null)

  const toRFC3339 = (local: string) => (local ? new Date(local).toISOString() : '')

  const act = useMutation({
    mutationFn: async ({ assertion }: { assertion: string }) => {
      await api.auth.sudo(assertion)
      if (confirm?.kind === 'remove') return api.freezes.remove(confirm.window.id)
      return api.freezes.create({
        starts_at: toRFC3339(starts),
        ends_at: toRFC3339(ends),
        display_tz: Intl.DateTimeFormat().resolvedOptions().timeZone,
        reason,
      })
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['freeze-windows'] })
      onToast(confirm?.kind === 'remove' ? 'Freeze window removed' : 'Freeze window created')
      setConfirm(null)
      setStarts(''); setEnds(''); setReason('')
    },
  })

  const windows = data?.windows ?? []
  const fmt = (iso: string) => new Date(iso).toLocaleString()

  return (
    <>
      <div className="set-head" style={{ marginTop: 32 }}>
        <h2>Freeze windows</h2>
        <p>
          Intervals during which nothing applies. Approvals stay open during a freeze — the
          decision is not the action — and an approved change queues behind the window.
          An admin can override a plan through it.
        </p>
      </div>

      {isLoading && <div className="setrow"><div className="setrow__main">Loading…</div></div>}
      {!isLoading && windows.length === 0 && (
        <div className="setrow"><div className="setrow__main setrow__help">
          No freezes. Applies run whenever a caller submits them, subject to the policies above.
        </div></div>
      )}

      {windows.map(w => {
        const active = new Date(w.starts_at) <= new Date() && new Date() < new Date(w.ends_at)
        return (
          <div className="setrow" key={w.id}>
            <div className="setrow__main">
              <div className="setrow__label">
                {fmt(w.starts_at)} — {fmt(w.ends_at)}
                {active && <span className="badge badge--destroy" style={{ marginLeft: 8 }}>active</span>}
              </div>
              <div className="setrow__help">
                {(w.classes?.length ?? 0) === 0 ? 'Every class' : w.classes!.join(', ')}
                {w.reason ? ` — ${w.reason}` : ''}
              </div>
            </div>
            <div className="setrow__control">
              <button
                className="btn btn--sm btn--ghost"
                disabled={!isAdmin || act.isPending}
                title={isAdmin ? undefined : 'Only admins can change this'}
                onClick={() => setConfirm({ kind: 'remove', window: w })}
              >
                Lift
              </button>
            </div>
          </div>
        )
      })}

      <div className="setrow">
        <div className="setrow__main" style={{ display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
          <input
            className="input"
            type="datetime-local"
            value={starts}
            onChange={e => setStarts(e.target.value)}
            disabled={!isAdmin}
            aria-label="Freeze starts"
          />
          <span className="muted">to</span>
          <input
            className="input"
            type="datetime-local"
            value={ends}
            onChange={e => setEnds(e.target.value)}
            disabled={!isAdmin}
            aria-label="Freeze ends"
          />
          <input
            className="input"
            style={{ flex: 1, minWidth: 140 }}
            placeholder="why (shown in refusals)"
            value={reason}
            onChange={e => setReason(e.target.value)}
            disabled={!isAdmin}
          />
          <button
            className="btn btn--sm"
            disabled={!isAdmin || !starts || !ends || act.isPending}
            onClick={() => setConfirm({ kind: 'create' })}
          >
            Freeze
          </button>
        </div>
      </div>

      {confirm && (
        <SudoConfirmDialog
          title={confirm.kind === 'create' ? 'Create a freeze window' : 'Lift a freeze window'}
          icon={<Snowflake />}
          body={
            confirm.kind === 'create'
              ? <p>Nothing will apply between <strong>{starts && fmt(toRFC3339(starts))}</strong> and{' '}
                  <strong>{ends && fmt(toRFC3339(ends))}</strong>. Approvals stay open; applies queue.</p>
              : <p>Applies resume immediately; anything queued behind the window can run on its
                  next retry.</p>
          }
          confirmLabel={confirm.kind === 'create' ? 'Freeze' : 'Lift'}
          pending={act.isPending}
          error={act.error ? (act.error as Error).message : null}
          onCancel={() => { act.reset(); setConfirm(null) }}
          onConfirm={(assertion) => act.mutate({ assertion })}
        />
      )}
    </>
  )
}

function DangerPanel({
  onToast,
  onAfterSignOutAll,
}: {
  onToast: (msg: string) => void
  onAfterSignOutAll: () => void
}) {
  const qc = useQueryClient()
  const [confirm, setConfirm] = useState<'signoutall' | 'revokeall' | null>(null)

  // Danger-zone mutations call /api/auth/sudo first to elevate the
  // session, then the actual action. The two-step is so a stolen
  // session cookie alone isn't enough — the operator must produce
  // a fresh sign-in code from Cloud too, within sudoTTL of the action.
  const signOutAll = useMutation({
    mutationFn: async (assertion: string) => {
      await api.auth.sudo(assertion)
      return api.auth.signOutAll()
    },
    onSuccess: (res) => {
      onToast(`Signed out ${res.sessions_removed} session${res.sessions_removed === 1 ? '' : 's'}`)
      setConfirm(null)
      onAfterSignOutAll()
    },
    // Don't auto-dismiss on error — the dialog surfaces the message
    // inline so the user can correct (expired code, rate-limited, etc).
  })

  const revokeAll = useMutation({
    mutationFn: async ({ assertion }: { assertion: string }) => {
      await api.auth.sudo(assertion)
      return api.callers.revokeAll()
    },
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ['callers'] })
      const failureNote = res.failures.length > 0 ? ` (${res.failures.length} failed)` : ''
      onToast(`Revoked ${res.revoked} caller${res.revoked === 1 ? '' : 's'}${failureNote}`)
      setConfirm(null)
    },
  })

  return (
    <>
      <div className="set-head">
        <h2>Danger zone</h2>
        <p>Irreversible and high-impact actions. These affect every operator and caller — proceed with care.</p>
      </div>

      <div className="danger-card">
        <div className="danger-card__head">
          <Warning />
          <span className="danger-card__title">Destructive actions</span>
        </div>
        <div className="setrow">
          <div className="setrow__main">
            <div className="setrow__label">Sign out all sessions</div>
            <div className="setrow__help">
              Invalidate every active console session, including yours. Everyone must sign in again.
            </div>
          </div>
          <div className="setrow__control">
            <button
              className="btn btn--sm btn--danger"
              onClick={() => setConfirm('signoutall')}
              disabled={signOutAll.isPending}
            >
              <SignOut size={12} />
              <span>Sign out all</span>
            </button>
          </div>
        </div>
        <div className="setrow">
          <div className="setrow__main">
            <div className="setrow__label">Revoke all caller certificates</div>
            <div className="setrow__help">
              Revoke every caller at once. <b>All callers will fail</b> until each is restored from the Callers page; their registered files re-register on the next apply.
            </div>
          </div>
          <div className="setrow__control">
            <button
              className="btn btn--sm btn--danger"
              onClick={() => setConfirm('revokeall')}
              disabled={revokeAll.isPending}
            >
              Revoke all
            </button>
          </div>
        </div>
      </div>

      {confirm === 'signoutall' && (
        <SudoConfirmDialog
          title="Sign out all sessions"
          icon={<SignOut />}
          body={<>End <b>every</b> active console session, including this one. You will be returned to the login screen.</>}
          confirmLabel="Sign out all"
          pending={signOutAll.isPending}
          error={signOutAll.error ? (signOutAll.error as Error).message : null}
          onCancel={() => { setConfirm(null); signOutAll.reset() }}
          onConfirm={(assertion) => signOutAll.mutate(assertion)}
        />
      )}

      {confirm === 'revokeall' && (
        <SudoConfirmDialog
          title="Revoke all caller certificates"
          icon={<Warning />}
          body={
            <>
              Drop <b>every</b> caller from the allowlist and clear their schema registrations.
              All callers fail until each is re-registered and re-issued from the Callers page.
            </>
          }
          requiredText="revoke all"
          confirmLabel="Revoke all"
          pending={revokeAll.isPending}
          error={revokeAll.error ? (revokeAll.error as Error).message : null}
          onCancel={() => { setConfirm(null); revokeAll.reset() }}
          onConfirm={(assertion) => revokeAll.mutate({ assertion })}
        />
      )}
    </>
  )
}

// ── Dialogs ──────────────────────────────────────────────────────────────
// SudoConfirmDialog — destructive-action gate that combines the
// optional typed-phrase challenge with a required re-authentication at Cloud.
// The submit calls /api/auth/sudo first (via the mutation wired up
// in DangerPanel) so a stolen session cookie alone isn't enough to
// trigger sign-out-all or revoke-all.
//
// Exported so other pages with destructive admin actions can reuse
// the same gate (PR 3 Workers tab uses it for Drain / Evict).
export function SudoConfirmDialog({
  title, icon, body, requiredText, confirmLabel, pending, error, onCancel, onConfirm,
}: {
  title: string
  icon: React.ReactNode
  body: React.ReactNode
  requiredText?: string
  confirmLabel: string
  pending?: boolean
  error: string | null
  onCancel: () => void
  onConfirm: (assertion: string) => void
}) {
  const [typed, setTyped] = useState('')
  const [assertion, setAssertion] = useState('')
  const [waiting, setWaiting] = useState(false)
  const [popupBlocked, setPopupBlocked] = useState(false)
  const popup = useRef<Window | null>(null)
  const { data: me } = useMe()

  const phraseOK = !requiredText || typed.trim().toLowerCase() === requiredText.toLowerCase()
  const canSubmit = phraseOK && assertion.length > 0 && !pending

  // Listen for the assertion the popup hands back.
  //
  // Three checks before believing a message, and each closes a different door:
  // the origin must be this console (another site can postMessage to us), the
  // source must be the window we opened (this page may have other children),
  // and the type must match (extensions and dev tooling post here too).
  useEffect(() => {
    function onMessage(e: MessageEvent) {
      if (e.origin !== window.location.origin) return
      if (popup.current && e.source !== popup.current) return
      if (e.data?.type !== STEP_UP_MESSAGE || typeof e.data.assertion !== 'string') return

      setWaiting(false)
      popup.current = null
      // Straight through, without waiting for another click. The user has just
      // confirmed at Cloud and the assertion is good for two minutes; asking
      // them to press the button again would be a second confirmation of the
      // thing they came back from confirming.
      if (phraseOK) onConfirm(e.data.assertion)
      else setAssertion(e.data.assertion)
    }
    window.addEventListener('message', onMessage)
    return () => window.removeEventListener('message', onMessage)
  }, [onConfirm, phraseOK])

  function confirmAtCloud() {
    if (!me?.step_up_url) return
    setPopupBlocked(false)
    const w = window.open(me.step_up_url, 'atlantis-step-up', 'width=460,height=560')
    if (!w) {
      // Blocked. The paste field below is the way through, and it has to stay
      // for exactly this — a dialog whose only path is a popup is a dialog a
      // blocked popup turns into a dead end.
      setPopupBlocked(true)
      return
    }
    popup.current = w
    setWaiting(true)
  }

  return (
    <div className="overlay is-open" onMouseDown={e => { if (e.target === e.currentTarget) onCancel() }}>
      <div className="modal" style={{ width: 440 }} role="dialog" aria-modal>
        <div className="modal__head">
          <div className="row" style={{ gap: 10, alignItems: 'center' }}>
            {icon}
            <span className="modal__title">{title}</span>
          </div>
        </div>
        <div className="modal__body">
          <div style={{ fontSize: 13, color: 'var(--ink-1)', lineHeight: 1.55, marginBottom: 16 }}>{body}</div>

          {requiredText && (
            <div className="field">
              <label className="field__label">
                Type <span className="mono" style={{ color: 'var(--coral)' }}>{requiredText}</span> to confirm
              </label>
              <input
                className="input mono"
                value={typed}
                onChange={e => setTyped(e.target.value)}
                autoFocus
                spellCheck={false}
                autoComplete="off"
                placeholder={requiredText}
              />
            </div>
          )}

          {/*
            Step-up sends the user back to Atlantis Cloud to present a second
            factor — this console holds no credential to re-check. What comes
            back is an assertion saying a factor was presented, which is a
            stronger statement than "this token is fresh": Cloud will mint a
            fresh one for anybody holding a twelve-hour session, and only the
            reauth path sets the claim the server requires here.

            A popup rather than a redirect. A full navigation would discard
            this dialog, and with it the action the user is partway through
            confirming; the popup returns through postMessage and leaves the
            page standing.
          */}
          <div className="field">
            <label className="field__label">Confirm with Atlantis Cloud</label>
            <button
              className="btn"
              type="button"
              onClick={confirmAtCloud}
              disabled={waiting || pending || !me?.step_up_url}
            >
              {waiting ? 'Waiting for Atlantis Cloud…' : 'Confirm with Atlantis Cloud'}
            </button>
            {waiting && (
              <div className="hint" style={{ marginTop: 6 }}>
                A window opened for you to enter your code. Close it to cancel.
              </div>
            )}
          </div>

          {/*
            The fallback, and it stays. A browser that blocks the popup would
            otherwise leave this dialog with no way forward at all. Shown only
            when that happens, so the ordinary path is one button.
          */}
          {popupBlocked && (
            <div className="field">
              <label className="field__label" htmlFor="sudo-assertion">
                Your browser blocked the window. Open{' '}
                {/*
                  rel="opener", NOT the reflexive noreferrer.

                  noreferrer implies noopener, so a tab opened that way has
                  window.opener === null — and the page it lands on hands the
                  assertion back with `window.opener?.postMessage(...)`, which
                  then silently does nothing and closes. The documented way out
                  of a blocked popup discarded the credential without a word,
                  which is the exact dead end this field exists to prevent.

                  Nothing is leaked by allowing the opener: both windows are
                  this console's own origin by the time the handover happens,
                  Cloud sets Referrer-Policy: no-referrer itself, and the
                  postMessage names its target origin explicitly.
                */}
                <a href={me?.step_up_url} target="_blank" rel="opener">this link</a>{' '}
                and paste the code it gives you.
              </label>
              <input
                id="sudo-assertion"
                className="input mono"
                type="text"
                autoComplete="off"
                spellCheck={false}
                value={assertion}
                onChange={e => setAssertion(e.target.value.trim())}
                placeholder="eyJhbGciOi…"
                onKeyDown={e => { if (e.key === 'Enter' && canSubmit) onConfirm(assertion) }}
              />
            </div>
          )}

          {error && (
            <div className="banner banner--error" style={{ marginTop: 4 }}>{error}</div>
          )}
        </div>
        <div className="modal__foot">
          <button className="btn btn--ghost" onClick={onCancel} disabled={pending}>Cancel</button>
          <button
            className="btn btn--danger"
            onClick={() => onConfirm(assertion)}
            disabled={!canSubmit}
          >
            {pending ? 'Working…' : confirmLabel}
          </button>
        </div>
      </div>
    </div>
  )
}

// TypedConfirmDialog — Slack-style "type the exact phrase to confirm"
// gate for the most destructive actions. The confirm button stays
// disabled until the user types the required phrase verbatim. Kept for
// non-sudo-required typed gates; the danger-zone uses SudoConfirmDialog
// above which composes typed-phrase + re-authentication.
// Exported so it is not an unused local: the component is deliberately
// dormant, not dead, and the typecheck gate has to be able to run.
export function TypedConfirmDialog({
  title, icon, body, requiredText, confirmLabel, pending, onCancel, onConfirm,
}: {
  title: string
  icon: React.ReactNode
  body: React.ReactNode
  requiredText: string
  confirmLabel: string
  pending?: boolean
  onCancel: () => void
  onConfirm: () => void
}) {
  const [typed, setTyped] = useState('')
  const matches = typed.trim().toLowerCase() === requiredText.toLowerCase()

  return (
    <div className="overlay is-open" onMouseDown={e => { if (e.target === e.currentTarget) onCancel() }}>
      <div className="modal" style={{ width: 440 }} role="dialog" aria-modal>
        <div className="modal__head">
          <div className="row" style={{ gap: 10, alignItems: 'center' }}>
            {icon}
            <span className="modal__title">{title}</span>
          </div>
        </div>
        <div className="modal__body">
          <div style={{ fontSize: 13, color: 'var(--ink-1)', lineHeight: 1.55, marginBottom: 16 }}>{body}</div>
          <div className="field">
            <label className="field__label">
              Type <span className="mono" style={{ color: 'var(--coral)' }}>{requiredText}</span> to confirm
            </label>
            <input
              className="input mono"
              value={typed}
              onChange={e => setTyped(e.target.value)}
              autoFocus
              spellCheck={false}
              autoComplete="off"
              placeholder={requiredText}
            />
          </div>
        </div>
        <div className="modal__foot">
          <button className="btn btn--ghost" onClick={onCancel} disabled={pending}>Cancel</button>
          <button
            className="btn btn--danger"
            onClick={onConfirm}
            disabled={!matches || pending}
          >
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  )
}
