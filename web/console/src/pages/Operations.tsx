import { Component, useEffect, useState, type ErrorInfo, type ReactNode } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { AlertTriangle, Box, Check, ChevronRight, Inbox, RefreshCw, Search, Shield, Undo2 } from 'lucide-react'
import { api, queries, type AuditEntry, type JobStatus } from '@/api/client'
import { useIsAdmin } from '@/hooks/useAuth'
import { PageShell } from '@/components/PageShell'
import { Sql } from '@/components/Sql'

// Use the design's :root[data-ops="tabs"] variant — switch between tabbed
// and stacked layouts based on viewport. The design defaults to tabbed for
// dense screens; keep that default.
function useOpsTabs() {
  useEffect(() => {
    document.documentElement.setAttribute('data-ops', 'tabs')
    return () => { document.documentElement.removeAttribute('data-ops') }
  }, [])
}

type Tab = 'rollback' | 'dead' | 'audit'

export function Operations() {
  useOpsTabs()
  const [tab, setTab] = useState<Tab>('rollback')
  const [toast, setToast] = useState<string | null>(null)
  const fire = (msg: string) => { setToast(msg); setTimeout(() => setToast(null), 2400) }

  return (
    <PageShell title="Operations" sub="rollback · dead jobs · audit">
        <div className="page__bodyinner">
          <div className="ops-tabbar">
            <div className="ops-tabs">
              <button className={`ops-tab ${tab === 'rollback' ? 'is-active' : ''}`} onClick={() => setTab('rollback')}>Rollback</button>
              <button className={`ops-tab ${tab === 'dead' ? 'is-active' : ''}`} onClick={() => setTab('dead')}>Dead jobs</button>
              <button className={`ops-tab ${tab === 'audit' ? 'is-active' : ''}`} onClick={() => setTab('audit')}>Audit log</button>
            </div>
          </div>

          <div className="ops-stack stack-24">
            <section className={`ops-panel ${tab === 'rollback' ? 'is-tabactive' : ''}`} data-panel="rollback">
              <PanelErrorBoundary>
                <RollbackPanel onToast={fire} />
              </PanelErrorBoundary>
            </section>
            <section className={`ops-panel ${tab === 'dead' ? 'is-tabactive' : ''}`} data-panel="dead">
              <DeadJobsPanel onToast={fire} />
            </section>
            <section className={`ops-panel ${tab === 'audit' ? 'is-tabactive' : ''}`} data-panel="audit">
              <AuditPanel />
            </section>
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

// PanelErrorBoundary catches a render error in the active panel and
// surfaces it inline, instead of letting it unmount the whole
// Operations tree. Per-panel boundary so a broken rollback preview
// doesn't blank the rest of the page; the root boundary in __root.tsx
// is the last-resort catch.
class PanelErrorBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state = { error: null as Error | null }

  static getDerivedStateFromError(error: Error) {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('[PanelErrorBoundary] caught:', error, info.componentStack)
  }

  render() {
    if (this.state.error) {
      return (
        <div className="banner banner--error" style={{ margin: 16, whiteSpace: 'pre-wrap', fontFamily: 'var(--mono)', fontSize: 12, lineHeight: 1.5 }}>
          <div style={{ fontWeight: 600, marginBottom: 6 }}>Render error in this panel:</div>
          <div>{this.state.error.message}</div>
          {this.state.error.stack && (
            <details style={{ marginTop: 8 }}>
              <summary style={{ cursor: 'pointer' }}>stack</summary>
              <pre style={{ marginTop: 6, fontSize: 11 }}>{this.state.error.stack}</pre>
            </details>
          )}
        </div>
      )
    }
    return this.props.children
  }
}

// ── Rollback ─────────────────────────────────────────────────────────────
function RollbackPanel({ onToast }: { onToast: (msg: string) => void }) {
  const isAdmin = useIsAdmin()
  const [target, setTarget] = useState('')
  const [confirmed, setConfirmed] = useState(false)
  const [result, setResult] = useState<{ new_version: number; up_sql: string } | null>(null)

  const targetVersion = () => Number(target.replace(/^v0*/, ''))

  // Preview: read-only RPC that returns the SQL the rollback would
  // actually execute. Replaces the previous placeholder line "SQL preview
  // is computed server-side at execution" which was useless.
  const preview = useMutation({
    mutationFn: () => api.schema.previewRollback(targetVersion()),
    onSuccess: () => setConfirmed(false),   // re-prime the confirmation gate
  })

  // Execute: actual mutation. The BFF auto-fills the actor identity from
  // the logged-in operator's email; the SPA doesn't need to ask.
  const m = useMutation({
    mutationFn: () => api.schema.rollback(targetVersion()),
    onSuccess: (res) => {
      setResult(res)
      preview.reset()
      onToast(`Rolled back to v${String(res.new_version).padStart(4, '0')}`)
    },
  })

  return (
    <div className="card">
      <div className="card__head">
        <Undo2 size={14} />
        <span className="card__title">Schema rollback</span>
        <span className="badge badge--break" style={{ marginLeft: 'auto' }}>destructive</span>
      </div>
      <div className="card__body">
        <div className="banner banner--warn" style={{ marginBottom: 18 }}>
          <AlertTriangle size={14} className="banner__icon" />
          <span>
            Rollback re-points the active schema and runs <b>down</b> migrations.
            Callers on the newer version will fail until they re-apply.
          </span>
        </div>

        <div className="row" style={{ gap: 20, alignItems: 'flex-end', flexWrap: 'wrap' }}>
          <div className="field" style={{ minWidth: 180 }}>
            <label className="field__label">Roll back to</label>
            <input
              className="input mono"
              placeholder="v0047"
              value={target}
              onChange={e => setTarget(e.target.value)}
            />
          </div>
          <button
            type="button"
            className="btn"
            onClick={() => preview.mutate()}
            disabled={!target || !isAdmin || preview.isPending}
          >
            {preview.isPending ? 'Computing…' : 'Preview plan'}
          </button>
        </div>

        {preview.isError && (
          <div className="banner banner--error" style={{ marginTop: 14 }}>
            {(preview.error as Error).message}
          </div>
        )}

        {/* See PreviewBlock for why this is a typed prop, not a closure capture. */}
        {preview.data ? (
          <PreviewBlock
            data={preview.data}
            confirmed={confirmed}
            onConfirm={setConfirmed}
            onExecute={() => m.mutate()}
            executePending={m.isPending}
            executeError={m.isError ? (m.error as Error).message : null}
            isAdmin={isAdmin}
            result={result}
          />
        ) : null}
      </div>
    </div>
  )
}

// PreviewBlock renders everything that depends on the preview result.
// Pulled out as its own component so the type narrowing on `data` is
// explicit (a typed prop, not a closure capture under a `&&` guard) —
// the previous in-JSX IIFE pattern lost the narrowing across React's
// reconciliation re-renders and caused a null-deref blank-page on
// state updates like checkbox toggling.
function PreviewBlock({
  data, confirmed, onConfirm, onExecute, executePending, executeError, isAdmin, result,
}: {
  data: {
    target_version: number
    current_version: number
    up_sql: string
    plan_class: string
    change_count: number
  }
  confirmed: boolean
  onConfirm: (v: boolean) => void
  onExecute: () => void
  executePending: boolean
  executeError: string | null
  isAdmin: boolean
  result: { new_version: number; up_sql: string } | null
}) {
  const planClassKey =
    data.plan_class === 'cross_caller_breaking' ? 'break' :
    data.plan_class === 'backfill_required'     ? 'back'  :
    data.plan_class === 'additive'              ? 'add'   :
                                                  'plain'

  return (
    <div style={{ marginTop: 20 }}>
      <div className="row" style={{ gap: 12, marginBottom: 14, alignItems: 'center', flexWrap: 'wrap' }}>
        <span className="section-label">tide plan</span>
        <span className={`badge badge--${planClassKey}`}>
          {data.plan_class} · {data.change_count} change{data.change_count === 1 ? '' : 's'}
        </span>
        <span className="mono faint" style={{ fontSize: 11.5 }}>
          v{String(data.current_version).padStart(4, '0')}
          {' → '}
          v{String(data.target_version).padStart(4, '0')}
        </span>
      </div>

      <details className="sqlblock" open>
        <summary>
          <ChevronRight size={12} /> down migration SQL
        </summary>
        <Sql>{data.up_sql || '-- no SQL emitted (target IR identical to current)'}</Sql>
      </details>

      <div className="row" style={{ marginTop: 16, gap: 10, alignItems: 'center' }}>
        <label className="checkbox">
          <input
            type="checkbox"
            checked={confirmed}
            onChange={e => onConfirm(e.target.checked)}
          />
          <span className="checkbox__box"><Check size={11} /></span>
          <span>I understand this rewrites the active schema pointer.</span>
        </label>
        <span className="spacer" style={{ flex: 1 }} />
        <SandboxPreviewButton />
        <button
          type="button"
          className="btn btn--danger"
          onClick={onExecute}
          disabled={!confirmed || executePending || !isAdmin}
        >
          {executePending ? 'Rolling back…' : 'Execute rollback'}
        </button>
      </div>

      {executeError && (
        <div className="banner banner--error" style={{ marginTop: 14 }}>
          {executeError}
        </div>
      )}

      {result && (
        <details className="sqlblock" open style={{ marginTop: 18 }}>
          <summary>
            <ChevronRight size={12} /> executed SQL — now v{result.new_version}
          </summary>
          <Sql>{result.up_sql || '(no SQL emitted)'}</Sql>
        </details>
      )}
    </div>
  )
}

// ── Dead jobs ────────────────────────────────────────────────────────────
function DeadJobsPanel({ onToast }: { onToast: (msg: string) => void }) {
  const qc = useQueryClient()
  const isAdmin = useIsAdmin()
  const { data, isLoading, error } = useQuery(queries.deadJobs())
  const [open, setOpen] = useState<string | null>(null)

  const retryM = useMutation({
    mutationFn: (id: string) => api.jobs.retry(id),
    onSuccess: (_, id) => {
      qc.invalidateQueries({ queryKey: ['jobs', 'dead'] })
      onToast(`Re-queued ${id.slice(0, 12)}`)
    },
  })

  const jobs = data?.jobs ?? []

  return (
    <div className="card">
      <div className="card__head">
        <Inbox size={14} />
        <span className="card__title">Dead job queue</span>
        <span className="chip" style={{ marginLeft: 'auto' }}>{jobs.length} stuck</span>
      </div>
      <div className="card__body" style={{ padding: 8 }}>
        {error && (
          <div className="banner banner--error" style={{ margin: 6 }}>
            {(error as Error).message}
          </div>
        )}

        {isLoading ? (
          <>
            {[0, 1, 2].map(i => (
              <div key={i} className="sk" style={{ height: 46, margin: '4px 0' }} />
            ))}
          </>
        ) : jobs.length === 0 ? (
          <div className="empty">
            <div className="empty__title">Queue is clear</div>
            <div className="empty__sub">No jobs have exhausted their retry budget.</div>
          </div>
        ) : (
          jobs.map(j => (
            <DeadJob
              key={j.job_id}
              job={j}
              isOpen={open === j.job_id}
              onToggle={() => setOpen(o => o === j.job_id ? null : j.job_id)}
              onRetry={() => retryM.mutate(j.job_id)}
              onDiscard={() => onToast(`Discarded ${j.job_id.slice(0, 12)}`)}
              canAdmin={isAdmin}
              retrying={retryM.isPending && retryM.variables === j.job_id}
            />
          ))
        )}
      </div>
    </div>
  )
}

function DeadJob({
  job, isOpen, onToggle, onRetry, onDiscard, canAdmin, retrying,
}: {
  job: JobStatus
  isOpen: boolean
  onToggle: () => void
  onRetry: () => void
  onDiscard: () => void
  canAdmin: boolean
  retrying: boolean
}) {
  return (
    <div className="deadjob">
      <div
        className="row"
        onClick={onToggle}
        style={{
          height: 46, padding: '0 12px', cursor: 'pointer',
          borderRadius: 'var(--radius)', gap: 12, transition: 'background var(--fast) var(--ease)',
        }}
        onMouseEnter={e => (e.currentTarget.style.background = 'var(--canvas-2)')}
        onMouseLeave={e => (e.currentTarget.style.background = 'transparent')}
      >
        <span className="lvl lvl--error">E</span>
        <span className="mono" style={{ color: 'var(--ink-0)', fontSize: 12.5 }}>
          {job.job_id.slice(0, 10)}
        </span>
        <span className="badge badge--plain">{job.job_name}</span>
        <span className="mono muted" style={{ fontSize: 12 }}>{job.entity_id ?? '—'}</span>
        <span className="spacer" style={{ flex: 1 }} />
        <span className="mono faint" style={{ fontSize: 11.5, whiteSpace: 'nowrap' }}>
          {job.attempts}/{job.max_attempts} attempts
        </span>
        <span className="brass" style={{ display: 'flex', transform: isOpen ? 'rotate(90deg)' : '', transition: 'transform var(--fast) var(--ease)' }}>
          <ChevronRight size={14} />
        </span>
      </div>

      {isOpen && (
        <div style={{ padding: '4px 12px 14px 40px' }}>
          {job.last_error && (
            <div className="banner banner--error" style={{ marginBottom: 12 }}>
              <AlertTriangle size={13} className="banner__icon" />
              <span className="mono" style={{ fontSize: 12 }}>{job.last_error}</span>
            </div>
          )}
          {job.payload && (
            <>
              <div className="section-label" style={{ marginBottom: 6 }}>payload</div>
              <pre style={{
                margin: '0 0 14px', padding: '11px 13px', background: 'var(--canvas-0)',
                border: '1px solid var(--line-soft)', borderRadius: 'var(--radius)',
                fontFamily: 'var(--mono)', fontSize: 11.5, color: 'var(--ink-2)',
              }}>{JSON.stringify(job.payload, null, 2)}</pre>
            </>
          )}
          <div className="row" style={{ gap: 8 }}>
            <button className="btn btn--sm" onClick={onRetry} disabled={!canAdmin || retrying}>
              <RefreshCw size={12} />
              <span>{retrying ? 'Retrying…' : 'Retry job'}</span>
            </button>
            <button className="btn btn--sm btn--ghost" onClick={onDiscard} disabled={!canAdmin}>
              Discard
            </button>
          </div>
        </div>
      )}
    </div>
  )
}

// ── Audit log ────────────────────────────────────────────────────────────
function fmtTime(ts: string) {
  const d = new Date(ts)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

const ACTION_LABEL: Record<string, string> = {
  revoke_caller: 'revoked caller',
  register_caller: 'registered caller',
  rollback_schema: 'rolled back',
  retry_dead_job: 'retried job',
  issue_caller_cert: 'issued cert',
  setup_complete: 'completed setup',
}

function AuditPanel() {
  const { data, isLoading, error } = useQuery(queries.auditLog())
  const [q, setQ] = useState('')
  const entries = data?.entries ?? []

  const filtered = entries.filter(e => {
    if (!q) return true
    const hay = `${e.user_email} ${e.action} ${JSON.stringify(e.detail || {})}`.toLowerCase()
    return hay.includes(q.toLowerCase())
  })

  return (
    <div className="card">
      <div className="card__head">
        <Shield size={14} />
        <span className="card__title">Audit log</span>
        <div className="logsearch" style={{ marginLeft: 'auto', height: 26, minWidth: 160 }}>
          <Search />
          <input placeholder="filter…" value={q} onChange={e => setQ(e.target.value)} />
        </div>
      </div>
      <div className="card__body" style={{ padding: 0 }}>
        {isLoading ? (
          <div style={{ padding: 18 }}>
            {[0, 1, 2].map(i => (
              <div key={i} className="sk" style={{ height: 18, marginBottom: 8 }} />
            ))}
          </div>
        ) : error ? (
          <div className="banner banner--error" style={{ margin: 14 }}>{(error as Error).message}</div>
        ) : filtered.length === 0 ? (
          <div className="empty">
            <div className="empty__title">{q ? 'No matching audit events' : 'No audit events yet'}</div>
            <div className="empty__sub">
              Operator-initiated mutations — rollbacks, revocations, cert issuance — appear here.
            </div>
          </div>
        ) : (
          filtered.map(e => <AuditRow key={e.id} entry={e} />)
        )}
      </div>
    </div>
  )
}

function AuditRow({ entry }: { entry: AuditEntry }) {
  const target = entry.detail && typeof entry.detail === 'object'
    ? Object.values(entry.detail as Record<string, unknown>)[0]
    : undefined
  const meta = entry.detail && typeof entry.detail === 'object'
    ? Object.entries(entry.detail as Record<string, unknown>)
        .slice(1)
        .map(([k, v]) => `${k}=${v}`)
        .join(' ')
    : ''

  return (
    <div
      className="auditrow"
      style={{
        display: 'grid',
        gridTemplateColumns: '88px 150px 170px 1fr',
        gap: 14,
        alignItems: 'center',
        height: 34,
        padding: '0 18px',
        borderBottom: '1px solid var(--line-soft)',
        fontFamily: 'var(--mono)',
        fontSize: 12,
      }}
    >
      <span className="faint num">{fmtTime(entry.created_at)}</span>
      <span className="slate">{entry.user_email}</span>
      <span style={{ color: 'var(--ink-0)' }}>{ACTION_LABEL[entry.action] ?? entry.action}</span>
      <span className="muted">
        {String(target ?? '')}
        {meta && <span className="faint"> · {meta}</span>}
      </span>
    </div>
  )
}

// SandboxPreviewButton is the Operations-page contextual entry. Sends
// the user to /sandbox with ?boot=sim — the auto-boot from current IR
// gives them an isolated mirror of production state where they can
// experiment with the SQL the rollback would execute, without risk.
function SandboxPreviewButton() {
  const navigate = useNavigate()
  return (
    <button
      type="button"
      className="btn btn--ghost"
      onClick={() => navigate({ to: '/sandbox', search: { boot: 'sim' } })}
      title="Open a sandbox booted from the current schema — exec the rollback SQL there first."
    >
      <Box size={13} />
      <span>Preview in sandbox</span>
    </button>
  )
}
