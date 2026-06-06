import { useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  AlertTriangle,
  Building2,
  Check,
  Copy,
  Fingerprint,
  Github,
  GitBranch,
  Link as LinkIcon,
  Lock,
  LogOut,
  Mail,
  Monitor,
  Plus,
  Server,
  Shield,
  Trash2,
  Users,
} from 'lucide-react'
import { api, queries, type CallerRepo, type OperatorUser, type UserRole } from '@/api/client'
import { useMe } from '@/hooks/useAuth'
import { PageShell } from '@/components/PageShell'

// Sectioned IA: left sub-nav (General / Members / Security / Danger
// zone) drives the right panel. Integrations is wired but absent from
// SECTIONS — see the dormancy note below. Every panel uses the .setrow
// pattern: label + help text on the left, control on the right.

// 'integrations' is intentionally absent — the panel exists below
// (IntegrationsPanel + MappingDialog) but isn't shown in the nav or
// rendered yet. Reintroduce by adding `'integrations'` back to the
// SectionId union, the SECTIONS array, the panel render in Settings(),
// and re-enabling the callerRepos query / upsert mutation.
type SectionId = 'general' | 'members' | 'security' | 'danger'

interface Section {
  id: SectionId
  label: string
  icon: React.ReactNode
  danger?: boolean
}

const SECTIONS: Section[] = [
  { id: 'general',  label: 'General',     icon: <Building2 /> },
  { id: 'members',  label: 'Members',     icon: <Users /> },
  { id: 'security', label: 'Security',    icon: <Shield /> },
  { id: 'danger',   label: 'Danger zone', icon: <AlertTriangle />, danger: true },
]

export function Settings() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { data: me } = useMe()
  const { data: ops, isLoading: oLoading } = useQuery(queries.operators())

  const [active, setActive] = useState<SectionId>('general')
  const [toast, setToast] = useState<string | null>(null)
  const fire = (msg: string) => { setToast(msg); setTimeout(() => setToast(null), 2200) }

  // ── dormant Integrations wiring (restore alongside the panel render) ──
  // const { data: repos, isLoading: rLoading } = useQuery(queries.callerRepos())
  // const upsert = useMutation({
  //   mutationFn: ({ caller, data }: { caller: string; data: Omit<CallerRepo, 'caller'> }) =>
  //     api.callerRepos.upsert(caller, data),
  //   onSuccess: (_, vars) => {
  //     qc.invalidateQueries({ queryKey: ['callerRepos'] })
  //     fire(`Mapping updated → ${vars.caller}`)
  //   },
  // })

  const setRole = useMutation({
    mutationFn: ({ id, role }: { id: number; role: UserRole }) => api.users.setRole(id, role),
    onSuccess: (_, vars) => {
      qc.invalidateQueries({ queryKey: ['users', 'operators'] })
      fire(`Role updated → ${vars.role}`)
    },
    onError: (err: Error) => fire(err.message),
  })
  const createUser = useMutation({
    mutationFn: ({ firstName, lastName, email, password, role }: {
      firstName: string
      lastName: string
      email: string
      password: string
      role: UserRole
    }) => api.users.create(firstName, lastName, email, password, role),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['users', 'operators'] })
      fire('Invite sent')
    },
    onError: (err: Error) => fire(err.message),
  })
  const deleteUser = useMutation({
    mutationFn: (id: number) => api.users.delete(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['users', 'operators'] })
      fire('Operator removed')
    },
    onError: (err: Error) => fire(err.message),
  })

  return (
    <PageShell title="Settings" sub="general · members · security">
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
              <MembersPanel
                users={ops?.users ?? []}
                loading={oLoading}
                currentUserId={me?.id ?? ''}
                onRoleChange={(id, role) => setRole.mutate({ id, role })}
                onInvite={(firstName, lastName, email, password, role) =>
                  createUser.mutate({ firstName, lastName, email, password, role })
                }
                onDelete={(id) => deleteUser.mutate(id)}
                saving={setRole.isPending || createUser.isPending || deleteUser.isPending}
              />
            </div>
            {/* Integrations panel is currently dormant — see SECTIONS comment.
                Restore by re-enabling repos/upsert above and reinstating:
                <div className={`set-panel ${active === 'integrations' ? 'is-active' : ''}`}>
                  <IntegrationsPanel ... />
                </div> */}
            <div className={`set-panel ${active === 'security' ? 'is-active' : ''}`}>
              <SecurityPanel
                onToast={fire}
                onAfterPasswordChange={() => navigate({ to: '/login' })}
              />
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
  // Server version comes from the live health probe — same source the
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
          <Building2 size={14} />
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
                <Server />
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
// avatarFor derives initials from the user record. Prefers real name
// when present; falls back to email parsing for legacy users created
// before the name fields were added.
function avatarFor(u: OperatorUser, i: number): { initials: string; tone: string } {
  const f = (u.first_name ?? '').trim()
  const l = (u.last_name ?? '').trim()
  let initials: string
  if (f && l) {
    initials = (f.charAt(0) + l.charAt(0)).toUpperCase()
  } else if (f) {
    initials = f.slice(0, 2).toUpperCase()
  } else {
    initials = u.email.split('@')[0].slice(0, 2).toUpperCase()
  }
  const tones = ['', 'av-slate', 'av-sage', 'av-coral']
  return { initials, tone: tones[i % tones.length] }
}

// nameFor returns the user's full name when set; falls back to a
// readable derivation of the email local part otherwise.
function nameFor(u: OperatorUser): string {
  const f = (u.first_name ?? '').trim()
  const l = (u.last_name ?? '').trim()
  if (f && l) return `${f} ${l}`
  if (f) return f
  const local = u.email.split('@')[0].replace(/[._-]/g, ' ')
  return local.charAt(0).toUpperCase() + local.slice(1)
}

function MembersPanel({
  users, loading, currentUserId, onRoleChange, onInvite, onDelete, saving,
}: {
  users: OperatorUser[]
  loading: boolean
  currentUserId: string
  onRoleChange: (id: number, role: UserRole) => void
  onInvite: (firstName: string, lastName: string, email: string, password: string, role: UserRole) => void
  onDelete: (id: number) => void
  saving: boolean
}) {
  const [inviting, setInviting] = useState(false)
  const [confirmRemove, setConfirmRemove] = useState<OperatorUser | null>(null)

  return (
    <>
      <div className="set-head">
        <h2>Members</h2>
        <p>People with console access. Roles control who can apply schema changes, issue caller certificates, and manage operators.</p>
      </div>
      <section className="card">
        <div className="card__head">
          <Users size={14} />
          <span className="card__title">Operators</span>
          <span className="chip" style={{ marginLeft: 10 }}>{users.length}</span>
          <button
            className="btn btn--sm"
            style={{ marginLeft: 'auto' }}
            onClick={() => setInviting(true)}
          >
            <Plus size={12} />
            <span>Invite operator</span>
          </button>
        </div>
        <div className="card__body" style={{ padding: 0 }}>
          {loading ? (
            <div style={{ padding: 18 }}>
              {[0, 1].map(i => (
                <div key={i} className="sk" style={{ height: 40, marginBottom: 8 }} />
              ))}
            </div>
          ) : (
            <div className="memberlist">
              {users.map((u, i) => {
                const isSelf = String(u.id) === currentUserId
                const { initials, tone } = avatarFor(u, i)
                return (
                  <div key={u.id} className="member">
                    <span className={`avatar ${tone}`}>{initials}</span>
                    <div className="member__id">
                      <div className="member__name">
                        {nameFor(u)}
                        {isSelf && <span className="badge badge--plain">you</span>}
                      </div>
                      <div className="member__email">{u.email}</div>
                    </div>
                    <div className="member__control">
                      <select
                        className="input--boxed"
                        value={u.role}
                        disabled={isSelf || saving}
                        title={isSelf ? 'You cannot change your own role' : undefined}
                        onChange={e => onRoleChange(u.id, e.target.value as UserRole)}
                      >
                        <option value="admin">admin</option>
                        <option value="viewer">viewer</option>
                      </select>
                      <button
                        className="iconbtn"
                        title="Remove"
                        disabled={isSelf}
                        onClick={() => setConfirmRemove(u)}
                      >
                        <Trash2 />
                      </button>
                    </div>
                  </div>
                )
              })}
            </div>
          )}
        </div>
      </section>

      {inviting && (
        <InviteDialog
          onCancel={() => setInviting(false)}
          onInvite={(f, l, e, p, r) => { onInvite(f, l, e, p, r); setInviting(false) }}
          saving={saving}
        />
      )}

      {confirmRemove && (
        <ConfirmDialog
          title="Remove operator"
          icon={<Trash2 />}
          body={
            <>
              Revoke console access for <b>{confirmRemove.email}</b>? Their active sessions end immediately.
            </>
          }
          confirmLabel="Remove"
          danger
          onCancel={() => setConfirmRemove(null)}
          onConfirm={() => {
            onDelete(confirmRemove.id)
            setConfirmRemove(null)
          }}
        />
      )}
    </>
  )
}

// ── Integrations ─────────────────────────────────────────────────────────
function IntegrationsPanel({
  repos, loading, onSave, saving, onToast,
}: {
  repos: CallerRepo[]
  loading: boolean
  onSave: (caller: string, data: Omit<CallerRepo, 'caller'>) => void
  saving: boolean
  onToast: (msg: string) => void
}) {
  const [editing, setEditing] = useState<CallerRepo | null>(null)
  const [adding, setAdding] = useState(false)

  return (
    <>
      <div className="set-head">
        <h2>Integrations</h2>
        <p>Connect Atlantis to your source of truth. Each caller is mapped to the repository and path Atlantis watches for schema files.</p>
      </div>

      <section className="card">
        <div className="connstrip">
          <span className="connstrip__logo"><Github /></span>
          <div className="connstrip__main">
            <div className="connstrip__title">GitHub</div>
            <div className="connstrip__status">
              <span className="dot" />
              Connected to <span className="mono" style={{ color: 'var(--ink-1)' }}>acme</span> · {repos.length} repositor{repos.length === 1 ? 'y' : 'ies'}
            </div>
          </div>
          <button className="btn btn--sm" onClick={() => onToast('Opening GitHub app settings…')}>Manage</button>
        </div>
      </section>

      <section className="card">
        <div className="card__head">
          <LinkIcon size={14} />
          <span className="card__title">Caller → repository mappings</span>
          <button
            className="btn btn--sm btn--ghost"
            style={{ marginLeft: 'auto' }}
            onClick={() => setAdding(true)}
          >
            <Plus size={12} />
            <span>Add mapping</span>
          </button>
        </div>
        <div className="card__body" style={{ padding: 0 }}>
          {loading ? (
            <div style={{ padding: 18 }}>
              {[0, 1].map(i => (
                <div key={i} className="sk" style={{ height: 40, marginBottom: 8 }} />
              ))}
            </div>
          ) : repos.length === 0 ? (
            <div className="empty">
              <div className="empty__title">No mappings yet</div>
              <div className="empty__sub">
                Click <span className="brass">Add mapping</span> to point a caller at a repository.
              </div>
            </div>
          ) : (
            repos.map(m => (
              <div key={m.caller} className="maprow">
                <span className="maprow__caller">{m.caller}</span>
                <span className="maprow__repo">
                  <Github />
                  {m.owner}/{m.repo}
                  {m.schema_path_prefix && (
                    <span className="faint">/{m.schema_path_prefix}</span>
                  )}
                </span>
                <span className="maprow__meta">
                  <GitBranch />
                  {m.default_branch}
                </span>
                <button
                  className="btn btn--sm btn--ghost maprow__edit"
                  onClick={() => setEditing(m)}
                >
                  <span>Edit</span>
                </button>
              </div>
            ))
          )}
        </div>
      </section>

      {(editing || adding) && (
        <MappingDialog
          repo={editing}
          onCancel={() => { setEditing(null); setAdding(false) }}
          onSave={(caller, data) => {
            onSave(caller, data)
            setEditing(null)
            setAdding(false)
          }}
          saving={saving}
        />
      )}
    </>
  )
}

// ── Security ─────────────────────────────────────────────────────────────
function SecurityPanel({
  onToast,
  onAfterPasswordChange,
}: {
  onToast: (msg: string) => void
  onAfterPasswordChange: () => void
}) {
  // mTLS toggle is dormant — mTLS is always required at the gRPC layer
  // today, so a UI toggle would either be a no-op or introduce a real
  // permissive mode that weakens trust. State + JSX are preserved below
  // (commented) so we can re-enable cleanly once a permissive mode is
  // implemented end-to-end.
  // const [mtlsRequired, setMtlsRequired] = useState(true)
  const [pwOpen, setPwOpen] = useState(false)
  const [pwError, setPwError] = useState<string | null>(null)

  const changePw = useMutation({
    mutationFn: ({ current, next }: { current: string; next: string }) =>
      api.auth.changePassword(current, next),
    onSuccess: () => {
      setPwOpen(false)
      setPwError(null)
      onToast('Password updated — sign in again')
      // The BFF invalidates every session for this user on success;
      // bounce to /login so the next request doesn't 401 mid-render.
      onAfterPasswordChange()
    },
    onError: (err: Error) => setPwError(err.message),
  })

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
        <p>Console credentials and the transport policy callers must satisfy. mTLS settings here apply to every registered caller.</p>
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
              <div className="setrow__help">Operator credentials, rotated independently of mTLS certificates.</div>
            </div>
            <div className="setrow__control">
              <button className="btn btn--sm" onClick={() => setPwOpen(true)}>Change password</button>
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

      {pwOpen && (
        <PasswordDialog
          error={pwError}
          pending={changePw.isPending}
          onCancel={() => { setPwOpen(false); setPwError(null) }}
          onSubmit={(current, next) => changePw.mutate({ current, next })}
        />
      )}
    </>
  )
}

// ── Danger ───────────────────────────────────────────────────────────────
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
  // their password too, within sudoTTL of the action.
  const signOutAll = useMutation({
    mutationFn: async (password: string) => {
      await api.auth.sudo(password)
      return api.auth.signOutAll()
    },
    onSuccess: (res) => {
      onToast(`Signed out ${res.sessions_removed} session${res.sessions_removed === 1 ? '' : 's'}`)
      setConfirm(null)
      onAfterSignOutAll()
    },
    // Don't auto-dismiss on error — the dialog surfaces the message
    // inline so the user can correct (wrong password, rate-limited, etc).
  })

  const revokeAll = useMutation({
    mutationFn: async ({ password }: { password: string }) => {
      await api.auth.sudo(password)
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
          <AlertTriangle />
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
              <LogOut size={12} />
              <span>Sign out all</span>
            </button>
          </div>
        </div>
        <div className="setrow">
          <div className="setrow__main">
            <div className="setrow__label">Revoke all caller certificates</div>
            <div className="setrow__help">
              Drop every caller from the allowlist. <b>All callers will fail</b> until they're re-registered and re-issued from the Callers page.
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
          icon={<LogOut />}
          body={<>End <b>every</b> active console session, including this one. You will be returned to the login screen.</>}
          confirmLabel="Sign out all"
          pending={signOutAll.isPending}
          error={signOutAll.error ? (signOutAll.error as Error).message : null}
          onCancel={() => { setConfirm(null); signOutAll.reset() }}
          onConfirm={(password) => signOutAll.mutate(password)}
        />
      )}

      {confirm === 'revokeall' && (
        <SudoConfirmDialog
          title="Revoke all caller certificates"
          icon={<AlertTriangle />}
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
          onConfirm={(password) => revokeAll.mutate({ password })}
        />
      )}
    </>
  )
}

// ── Dialogs ──────────────────────────────────────────────────────────────
function ConfirmDialog({
  title, icon, body, confirmLabel, danger, pending, onCancel, onConfirm,
}: {
  title: string
  icon: React.ReactNode
  body: React.ReactNode
  confirmLabel: string
  danger?: boolean
  pending?: boolean
  onCancel: () => void
  onConfirm: () => void
}) {
  return (
    <div className="overlay is-open" onMouseDown={e => { if (e.target === e.currentTarget) onCancel() }}>
      <div className="modal" style={{ width: 420 }} role="dialog" aria-modal>
        <div className="modal__head">
          <div className="row" style={{ gap: 10, alignItems: 'center' }}>
            {icon}
            <span className="modal__title">{title}</span>
          </div>
        </div>
        <div className="modal__body">
          <div style={{ fontSize: 13, color: 'var(--ink-1)', lineHeight: 1.55 }}>{body}</div>
        </div>
        <div className="modal__foot">
          <button className="btn btn--ghost" onClick={onCancel} disabled={pending}>Cancel</button>
          <button
            className={`btn ${danger ? 'btn--danger' : 'btn--brass'}`}
            onClick={onConfirm}
            disabled={pending}
          >
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  )
}

// SudoConfirmDialog — destructive-action gate that combines the
// optional typed-phrase challenge with a required password re-auth.
// The submit calls /api/auth/sudo first (via the mutation wired up
// in DangerPanel) so a stolen session cookie alone isn't enough to
// trigger sign-out-all or revoke-all.
function SudoConfirmDialog({
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
  onConfirm: (password: string) => void
}) {
  const [typed, setTyped] = useState('')
  const [password, setPassword] = useState('')

  const phraseOK = !requiredText || typed.trim().toLowerCase() === requiredText.toLowerCase()
  const canSubmit = phraseOK && password.length > 0 && !pending

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

          <div className="field">
            <label className="field__label">Confirm with your password</label>
            <input
              className="input"
              type="password"
              autoFocus={!requiredText}
              autoComplete="current-password"
              value={password}
              onChange={e => setPassword(e.target.value)}
              placeholder="••••••••"
              onKeyDown={e => { if (e.key === 'Enter' && canSubmit) onConfirm(password) }}
            />
          </div>

          {error && (
            <div className="banner banner--error" style={{ marginTop: 4 }}>{error}</div>
          )}
        </div>
        <div className="modal__foot">
          <button className="btn btn--ghost" onClick={onCancel} disabled={pending}>Cancel</button>
          <button
            className="btn btn--danger"
            onClick={() => onConfirm(password)}
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
// above which composes typed-phrase + password re-auth.
function TypedConfirmDialog({
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

function InviteDialog({
  onCancel, onInvite, saving,
}: {
  onCancel: () => void
  onInvite: (firstName: string, lastName: string, email: string, password: string, role: UserRole) => void
  saving: boolean
}) {
  const [firstName, setFirstName] = useState('')
  const [lastName, setLastName] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [role, setRole] = useState<UserRole>('viewer')

  return (
    <div className="overlay is-open" onMouseDown={e => { if (e.target === e.currentTarget) onCancel() }}>
      <div className="modal" role="dialog" aria-modal>
        <div className="modal__head">
          <div className="modal__title">Invite operator</div>
          <div className="modal__sub">They receive an email to set credentials and enroll an mTLS certificate.</div>
        </div>
        <div className="modal__body">
          <div className="row" style={{ gap: 12 }}>
            <div className="field" style={{ flex: 1 }}>
              <label className="field__label">First name <span style={{ color: 'var(--ink-3)', fontWeight: 400 }}>(optional)</span></label>
              <input
                className="input"
                type="text"
                placeholder="Ada"
                value={firstName}
                onChange={e => setFirstName(e.target.value)}
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
              />
            </div>
          </div>
          <div className="field">
            <label className="field__label">Email address</label>
            <input
              className="input"
              type="email"
              placeholder="name@your.org"
              autoFocus
              value={email}
              onChange={e => setEmail(e.target.value)}
            />
          </div>
          <div className="field">
            <label className="field__label">Initial password</label>
            <input
              className="input"
              type="password"
              placeholder="min 8 characters"
              value={password}
              onChange={e => setPassword(e.target.value)}
            />
          </div>
          <div className="field">
            <label className="field__label">Role</label>
            <select
              className="input--boxed"
              value={role}
              onChange={e => setRole(e.target.value as UserRole)}
            >
              <option value="viewer">viewer</option>
              <option value="admin">admin</option>
            </select>
          </div>
        </div>
        <div className="modal__foot">
          <button className="btn btn--ghost" onClick={onCancel}>Cancel</button>
          <button
            className="btn btn--brass"
            disabled={!email || password.length < 8 || saving}
            onClick={() => onInvite(firstName.trim(), lastName.trim(), email, password, role)}
          >
            <Mail size={13} />
            <span>{saving ? 'Inviting…' : 'Send invite'}</span>
          </button>
        </div>
      </div>
    </div>
  )
}

function MappingDialog({
  repo, onCancel, onSave, saving,
}: {
  repo: CallerRepo | null
  onCancel: () => void
  onSave: (caller: string, data: Omit<CallerRepo, 'caller'>) => void
  saving: boolean
}) {
  const [caller, setCaller] = useState(repo?.caller ?? '')
  const [ownerRepo, setOwnerRepo] = useState(repo ? `${repo.owner}/${repo.repo}` : '')
  const [path, setPath] = useState(repo?.schema_path_prefix ?? '')
  const [branch, setBranch] = useState(repo?.default_branch ?? 'main')

  const handleSave = () => {
    const [owner, repoName] = ownerRepo.split('/')
    if (!owner || !repoName) return
    onSave(caller, {
      owner,
      repo: repoName,
      default_branch: branch,
      schema_path_prefix: path,
    })
  }

  return (
    <div className="overlay is-open" onMouseDown={e => { if (e.target === e.currentTarget) onCancel() }}>
      <div className="modal" role="dialog" aria-modal>
        <div className="modal__head">
          <div className="modal__title">{repo ? 'Edit mapping' : 'Add mapping'}</div>
          <div className="modal__sub">
            {repo
              ? `Update where Atlantis watches for ${repo.caller}'s schema.`
              : 'Point a caller at a repository path.'}
          </div>
        </div>
        <div className="modal__body">
          <div className="field">
            <label className="field__label">Caller</label>
            <input
              className="input mono"
              value={caller}
              placeholder="backend"
              disabled={!!repo}
              onChange={e => setCaller(e.target.value)}
            />
          </div>
          <div className="field">
            <label className="field__label">Repository</label>
            <input
              className="input mono"
              value={ownerRepo}
              placeholder="acme/service"
              onChange={e => setOwnerRepo(e.target.value)}
            />
          </div>
          <div className="row" style={{ gap: 18 }}>
            <div className="field" style={{ flex: 1 }}>
              <label className="field__label">Path</label>
              <input
                className="input mono"
                value={path}
                placeholder="schema/*.atl"
                onChange={e => setPath(e.target.value)}
              />
            </div>
            <div className="field" style={{ flex: 1 }}>
              <label className="field__label">Branch</label>
              <input
                className="input mono"
                value={branch}
                placeholder="main"
                onChange={e => setBranch(e.target.value)}
              />
            </div>
          </div>
        </div>
        <div className="modal__foot">
          <button className="btn btn--ghost" onClick={onCancel}>Cancel</button>
          <button
            className="btn btn--brass"
            disabled={!caller || !ownerRepo.includes('/') || saving}
            onClick={handleSave}
          >
            <Check size={13} />
            <span>{repo ? 'Save mapping' : 'Add mapping'}</span>
          </button>
        </div>
      </div>
    </div>
  )
}

function PasswordDialog({
  onCancel, onSubmit, error, pending,
}: {
  onCancel: () => void
  onSubmit: (current: string, next: string) => void
  error: string | null
  pending: boolean
}) {
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [localError, setLocalError] = useState<string | null>(null)

  const handle = () => {
    setLocalError(null)
    if (next !== confirm) { setLocalError('New password and confirmation do not match'); return }
    if (next.length < 8)  { setLocalError('New password must be at least 8 characters'); return }
    onSubmit(current, next)
  }

  const errMsg = localError ?? error

  return (
    <div className="overlay is-open" onMouseDown={e => { if (e.target === e.currentTarget) onCancel() }}>
      <div className="modal" role="dialog" aria-modal>
        <div className="modal__head">
          <div className="modal__title">Change password</div>
          <div className="modal__sub">Re-enter your current password to set a new one. You'll be signed out everywhere.</div>
        </div>
        <div className="modal__body">
          <div className="field">
            <label className="field__label">Current password</label>
            <input
              className="input"
              type="password"
              placeholder="••••••••"
              autoFocus
              value={current}
              onChange={e => setCurrent(e.target.value)}
            />
          </div>
          <div className="field">
            <label className="field__label">New password</label>
            <input
              className="input"
              type="password"
              placeholder="min 8 characters"
              value={next}
              onChange={e => setNext(e.target.value)}
            />
          </div>
          <div className="field">
            <label className="field__label">Confirm new password</label>
            <input
              className="input"
              type="password"
              placeholder="••••••••"
              value={confirm}
              onChange={e => setConfirm(e.target.value)}
            />
          </div>
          {errMsg && (
            <div className="banner banner--error" style={{ marginTop: 4 }}>{errMsg}</div>
          )}
        </div>
        <div className="modal__foot">
          <button className="btn btn--ghost" onClick={onCancel} disabled={pending}>Cancel</button>
          <button
            className="btn btn--brass"
            onClick={handle}
            disabled={pending || !current || !next || !confirm}
          >
            <Lock size={13} />
            <span>{pending ? 'Updating…' : 'Update password'}</span>
          </button>
        </div>
      </div>
    </div>
  )
}
