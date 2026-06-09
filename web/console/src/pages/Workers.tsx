// Workers — connected dispatched-worker sessions index.
//
// One row per session in the .tbl. Per-queue summary cards at the top
// of the body. 2s polling, paused while the tab is backgrounded.
// Empty state mirrors Operations.tsx "Queue is clear" pattern.

import { useEffect, useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { Cpu, RefreshCw } from 'lucide-react'
import { api, ApiError } from '@/api/client'
import type { WorkerSessionSummary } from '@/api/client'
import { PageShell } from '@/components/PageShell'

const POLL_MS = 2000

export function Workers() {
  const [sessions, setSessions] = useState<WorkerSessionSummary[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [tick, setTick] = useState(0)

  useEffect(() => {
    let cancelled = false
    let interval: ReturnType<typeof setInterval> | null = null

    const fetchOnce = async () => {
      if (document.visibilityState !== 'visible') return
      try {
        const res = await api.workers.list()
        if (cancelled) return
        setSessions(res.sessions ?? [])
        setError(null)
      } catch (e) {
        if (cancelled) return
        setError(e instanceof ApiError ? e.message : 'failed to load workers')
      } finally {
        if (!cancelled) setLoading(false)
      }
    }

    fetchOnce()
    interval = setInterval(fetchOnce, POLL_MS)

    const onVis = () => {
      if (document.visibilityState === 'visible') fetchOnce()
    }
    document.addEventListener('visibilitychange', onVis)

    return () => {
      cancelled = true
      if (interval) clearInterval(interval)
      document.removeEventListener('visibilitychange', onVis)
    }
  }, [tick])

  // Per-second tick refreshes the relative timestamps without a new fetch.
  const [, setRel] = useState(0)
  useEffect(() => {
    const id = setInterval(() => setRel(n => n + 1), 1000)
    return () => clearInterval(id)
  }, [])

  const queues = useMemo(() => groupByQueue(sessions), [sessions])
  const totals = useMemo(() => sumTotals(sessions), [sessions])

  const action = (
    <button
      className="btn btn--ghost btn--sm"
      onClick={() => setTick(t => t + 1)}
      title="Refresh"
      style={{ gap: 6 }}
    >
      <RefreshCw size={12} />
      <span>Refresh</span>
    </button>
  )

  return (
    <PageShell
      title="Workers"
      sub="dispatched-worker sessions · polled @2s"
      action={action}
    >
      <div className="page__bodyinner">
        {error && (
          <div className="banner banner--error" style={{ marginBottom: 18 }}>
            {error}
          </div>
        )}

        {/* Overview strip. Always rendered; collapses cleanly when empty. */}
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr 1fr', gap: 12, marginBottom: 24 }}>
          <StatCell label="Sessions" value={sessions.length} />
          <StatCell label="In-flight" value={totals.inflight} />
          <StatCell label="Dispatched" value={totals.dispatched} mono />
          <StatCell label="Failed" value={totals.failed} tone={totals.failed > 0 ? 'coral' : undefined} />
        </div>

        {queues.length > 0 && (
          <>
            <div className="section-label" style={{ marginBottom: 12 }}>BY QUEUE</div>
            <div style={{
              display: 'grid',
              gridTemplateColumns: `repeat(${Math.min(queues.length, 3)}, minmax(0, 1fr))`,
              gap: 12,
              marginBottom: 32,
            }}>
              {queues.map(q => (
                <div key={q.queue} className="card">
                  <div className="card__head">
                    <Cpu size={13} />
                    <span className="card__title mono">{q.queue}</span>
                    <span className="chip chip--count" style={{ marginLeft: 'auto' }}>
                      {q.sessions}
                    </span>
                  </div>
                  <div className="card__body" style={{ padding: '14px 18px' }}>
                    <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '14px 24px' }}>
                      <QueueRow label="In-flight" value={q.inflight} />
                      <QueueRow label="Dispatched" value={q.dispatched} />
                      <QueueRow label="Completed" value={q.completed} tone="sage" />
                      <QueueRow label="Failed" value={q.failed} tone={q.failed > 0 ? 'coral' : undefined} />
                    </div>
                  </div>
                </div>
              ))}
            </div>
          </>
        )}

        <div className="section-label" style={{ marginBottom: 12 }}>SESSIONS</div>

        {loading && sessions.length === 0 ? (
          <div className="card" style={{ padding: 12 }}>
            {[0, 1, 2].map(i => (
              <div key={i} className="sk" style={{ height: 44, margin: '4px 0' }} />
            ))}
          </div>
        ) : sessions.length === 0 ? (
          <div className="card">
            <div className="empty">
              <div className="empty__icon">
                <Cpu size={18} />
              </div>
              <div className="empty__title">No workers connected</div>
              <div className="empty__sub">
                Enable the dispatcher on atlantis with{' '}
                <span className="mono brass">ATL_JOBS_DISPATCHER_ENABLED=true</span>,
                then start a <span className="mono brass">NewDispatchedWorker</span> in your caller.
              </div>
            </div>
          </div>
        ) : (
          <div className="card" style={{ padding: 6 }}>
            <table className="tbl">
              <thead>
                <tr>
                  <th>Caller</th>
                  <th>Queue</th>
                  <th>Pod</th>
                  <th>Connected</th>
                  <th style={{ textAlign: 'right' }}>In-flight</th>
                  <th style={{ textAlign: 'right' }}>Dispatched</th>
                  <th style={{ textAlign: 'right' }}>Completed</th>
                  <th style={{ textAlign: 'right' }}>Failed</th>
                  <th>Heartbeat</th>
                  <th aria-label="actions" style={{ width: 32 }} />
                </tr>
              </thead>
              <tbody>
                {sessions.map(s => {
                  const stale = isStale(s.last_heartbeat_at)
                  return (
                    <tr key={s.session_id}>
                      <td className="mono">{s.caller}</td>
                      <td>{s.queue}</td>
                      <td className="mono faint">{s.pod_id || '—'}</td>
                      <td className="num">{relativeAgo(s.connected_at)}</td>
                      <td className="num" style={{ textAlign: 'right' }}>
                        <span style={{ color: 'var(--ink-0)' }}>{s.inflight_count}</span>
                        <span className="faint"> / {s.max_in_flight}</span>
                      </td>
                      <td className="num" style={{ textAlign: 'right' }}>{s.dispatched}</td>
                      <td className="num sage" style={{ textAlign: 'right' }}>{s.completed}</td>
                      <td className="num" style={{ textAlign: 'right', color: s.failed > 0 ? 'var(--coral)' : undefined }}>{s.failed}</td>
                      <td>
                        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 7 }}>
                          <span className={s.drained ? 'dot' : (stale ? 'dot dot--coral' : 'dot dot--brass')}
                            style={s.drained ? { background: 'var(--ink-3)' } : undefined} />
                          <span className={stale ? 'coral' : ''}>{relativeAgo(s.last_heartbeat_at)}</span>
                          {s.drained && <span className="badge badge--plain" style={{ marginLeft: 6 }}>draining</span>}
                        </span>
                      </td>
                      <td>
                        <Link
                          to="/workers/$id"
                          params={{ id: s.session_id }}
                          className="btn btn--ghost btn--sm"
                          style={{ height: 26, padding: '0 10px' }}
                        >
                          Open →
                        </Link>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </PageShell>
  )
}

function StatCell({ label, value, tone, mono }: { label: string; value: number; tone?: 'coral' | 'sage'; mono?: boolean }) {
  const color = tone === 'coral' ? 'var(--coral)' : tone === 'sage' ? 'var(--sage)' : 'var(--ink-0)'
  return (
    <div style={{
      background: 'var(--canvas-1)',
      border: '1px solid var(--line-soft)',
      borderRadius: 'var(--radius-lg)',
      padding: '14px 16px',
    }}>
      <div className="section-label" style={{ marginBottom: 8 }}>{label}</div>
      <div className={mono ? 'num mono' : 'num'} style={{
        fontSize: 24, fontWeight: 500, color, letterSpacing: '-0.01em',
      }}>
        {value.toLocaleString()}
      </div>
    </div>
  )
}

function QueueRow({ label, value, tone }: { label: string; value: number; tone?: 'sage' | 'coral' }) {
  const color = tone === 'sage' ? 'var(--sage)' : tone === 'coral' ? 'var(--coral)' : 'var(--ink-0)'
  return (
    <div>
      <div className="section-label" style={{ marginBottom: 4 }}>{label}</div>
      <div className="num" style={{ fontSize: 16, fontWeight: 500, color }}>{value.toLocaleString()}</div>
    </div>
  )
}

interface QueueSummary {
  queue: string
  sessions: number
  inflight: number
  dispatched: number
  completed: number
  failed: number
  revoked: number
}

function groupByQueue(sessions: WorkerSessionSummary[]): QueueSummary[] {
  const m = new Map<string, QueueSummary>()
  for (const s of sessions) {
    const cur = m.get(s.queue) ?? {
      queue: s.queue, sessions: 0, inflight: 0, dispatched: 0, completed: 0, failed: 0, revoked: 0,
    }
    cur.sessions++
    cur.inflight += s.inflight_count
    cur.dispatched += s.dispatched
    cur.completed += s.completed
    cur.failed += s.failed
    cur.revoked += s.revoked
    m.set(s.queue, cur)
  }
  return Array.from(m.values()).sort((a, b) => a.queue.localeCompare(b.queue))
}

function sumTotals(sessions: WorkerSessionSummary[]) {
  return sessions.reduce(
    (acc, s) => ({
      inflight: acc.inflight + s.inflight_count,
      dispatched: acc.dispatched + s.dispatched,
      completed: acc.completed + s.completed,
      failed: acc.failed + s.failed,
    }),
    { inflight: 0, dispatched: 0, completed: 0, failed: 0 },
  )
}

function isStale(rfc3339: string): boolean {
  const ts = new Date(rfc3339).getTime()
  if (!Number.isFinite(ts)) return false
  return Date.now() - ts > 10_000
}

function relativeAgo(rfc3339: string): string {
  const ts = new Date(rfc3339).getTime()
  if (!Number.isFinite(ts)) return '—'
  const delta = Math.max(0, Math.floor((Date.now() - ts) / 1000))
  if (delta < 60) return `${delta}s ago`
  if (delta < 3600) return `${Math.floor(delta / 60)}m ago`
  if (delta < 86400) return `${Math.floor(delta / 3600)}h ago`
  return `${Math.floor(delta / 86400)}d ago`
}
