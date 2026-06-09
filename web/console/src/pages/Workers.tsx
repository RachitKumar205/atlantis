// Workers — connected dispatched-worker sessions index.
//
// One row per session. Sums + per-queue summary cards at the top.
// 2s polling, paused while the tab is backgrounded.
//
// Read-only here; per-row drilling links to /workers/$id where the
// admin actions (Drain / Evict) live behind sudo confirmation.

import { useEffect, useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { api, ApiError } from '@/api/client'
import type { WorkerSessionSummary } from '@/api/client'

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
        const msg = e instanceof ApiError ? e.message : 'failed to load workers'
        setError(msg)
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

  // 1s timer to refresh relative timestamps without re-fetching.
  const [, setRelTick] = useState(0)
  useEffect(() => {
    const id = setInterval(() => setRelTick(n => n + 1), 1000)
    return () => clearInterval(id)
  }, [])

  const byQueue = useMemo(() => groupByQueue(sessions), [sessions])

  return (
    <div className="page">
      <header className="page__head">
        <div>
          <h1 className="page__title">Workers</h1>
          <div className="page__subtitle">Connected dispatched-worker sessions. Live, refreshes every 2s.</div>
        </div>
        <button
          className="btn btn--ghost"
          onClick={() => setTick(t => t + 1)}
          title="Refresh now"
        >
          Refresh
        </button>
      </header>

      {error && <div className="banner banner--error">{error}</div>}

      <section className="cards" style={{ marginBottom: 24 }}>
        {byQueue.length === 0 && !loading && !error && (
          <div className="empty">
            No workers connected. Enable the dispatcher with
            <code className="mono"> ATL_JOBS_DISPATCHER_ENABLED=true </code>
            on atlantis and start a <code className="mono">NewDispatchedWorker</code> in your caller.
          </div>
        )}
        {byQueue.map(q => (
          <div key={q.queue} className="card">
            <div className="card__head">
              <div className="card__title">{q.queue}</div>
              <div className="card__sub">{q.sessions} session{q.sessions === 1 ? '' : 's'}</div>
            </div>
            <div className="row" style={{ gap: 20 }}>
              <Metric label="In-flight" value={q.inflight} />
              <Metric label="Dispatched" value={q.dispatched} />
              <Metric label="Completed" value={q.completed} />
              <Metric label="Failed" value={q.failed} />
              <Metric label="Revoked" value={q.revoked} />
            </div>
          </div>
        ))}
      </section>

      <section>
        <h2 className="section__title">Sessions</h2>
        <table className="table">
          <thead>
            <tr>
              <th>Caller</th>
              <th>Queue</th>
              <th>Pod</th>
              <th>Connected</th>
              <th>In-flight</th>
              <th>Dispatched</th>
              <th>Completed</th>
              <th>Failed</th>
              <th>Heartbeat</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {sessions.map(s => {
              const stale = isStale(s.last_heartbeat_at)
              return (
                <tr key={s.session_id}>
                  <td className="mono">{s.caller}</td>
                  <td>{s.queue}</td>
                  <td className="mono" style={{ color: 'var(--ink-2)' }}>{s.pod_id || '—'}</td>
                  <td>{relativeAgo(s.connected_at)}</td>
                  <td>
                    <span style={{ fontWeight: 600 }}>{s.inflight_count}</span>
                    <span style={{ color: 'var(--ink-3)' }}> / {s.max_in_flight}</span>
                  </td>
                  <td>{s.dispatched}</td>
                  <td>{s.completed}</td>
                  <td style={{ color: s.failed > 0 ? 'var(--coral)' : undefined }}>{s.failed}</td>
                  <td>
                    <span className={stale ? 'pill pill--warn' : 'pill pill--ok'}>
                      {relativeAgo(s.last_heartbeat_at)}
                    </span>
                    {s.drained && <span className="pill pill--muted" style={{ marginLeft: 6 }}>draining</span>}
                  </td>
                  <td>
                    <Link to="/workers/$id" params={{ id: s.session_id }} className="link">
                      details →
                    </Link>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
        {loading && <div className="muted">Loading…</div>}
      </section>
    </div>
  )
}

function Metric({ label, value }: { label: string; value: number }) {
  return (
    <div>
      <div style={{ color: 'var(--ink-3)', fontSize: 11, textTransform: 'uppercase', letterSpacing: 0.5 }}>{label}</div>
      <div style={{ fontSize: 20, fontWeight: 600 }}>{value.toLocaleString()}</div>
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
      queue: s.queue,
      sessions: 0,
      inflight: 0,
      dispatched: 0,
      completed: 0,
      failed: 0,
      revoked: 0,
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

// isStale flags a heartbeat that's older than 2× the typical
// HeartbeatMS (~5s server default × 2 = 10s). The exact threshold
// isn't critical — the pill is a visual nudge for the operator.
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
