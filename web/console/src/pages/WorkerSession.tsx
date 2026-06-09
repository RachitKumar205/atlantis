// WorkerSession — per-session drill-in.
//
// Header pill: connected | stale heartbeat | disconnected.
// Counter cards: dispatched / completed / failed / revoked.
// In-flight panel: live list of jobs the worker is running.
// Job names panel: what this session declared it can handle.
// Recent events: last 50 state transitions from the server-side ring.
//
// Actions (admin role only, gated by sudo confirm via the shared
// SudoConfirmDialog reused from Settings):
//   - Drain: graceful stop; let in-flight finish; close session.
//   - Evict: force-close; release in-flight rows; immediate.

import { useEffect, useState } from 'react'
import { Link, useNavigate, useParams } from '@tanstack/react-router'
import { AlertTriangle, ShieldOff } from 'lucide-react'
import { api, ApiError } from '@/api/client'
import type { WorkerSessionDetail } from '@/api/client'
import { SudoConfirmDialog } from './Settings'

const POLL_MS = 2000

export function WorkerSession() {
  const { id } = useParams({ from: '/workers/$id' })
  const navigate = useNavigate()

  const [detail, setDetail] = useState<WorkerSessionDetail | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [notFound, setNotFound] = useState(false)
  const [tick, setTick] = useState(0)

  const [showDrain, setShowDrain] = useState(false)
  const [showEvict, setShowEvict] = useState(false)
  const [actionPending, setActionPending] = useState(false)
  const [actionError, setActionError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    let interval: ReturnType<typeof setInterval> | null = null

    const fetchOnce = async () => {
      if (document.visibilityState !== 'visible') return
      try {
        const res = await api.workers.get(id)
        if (cancelled) return
        setDetail(res.session)
        setNotFound(false)
        setError(null)
      } catch (e) {
        if (cancelled) return
        if (e instanceof ApiError && e.status === 404) {
          setNotFound(true)
        } else {
          setError(e instanceof ApiError ? e.message : 'failed to load session')
        }
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
  }, [id, tick])

  const drain = async (password: string) => {
    setActionPending(true)
    setActionError(null)
    try {
      await api.auth.sudo(password)
      await api.workers.drain(id)
      setShowDrain(false)
      setTick(t => t + 1)
    } catch (e) {
      setActionError(e instanceof ApiError ? e.message : 'drain failed')
    } finally {
      setActionPending(false)
    }
  }

  const evict = async (password: string) => {
    setActionPending(true)
    setActionError(null)
    try {
      await api.auth.sudo(password)
      await api.workers.evict(id)
      navigate({ to: '/workers' })
    } catch (e) {
      setActionError(e instanceof ApiError ? e.message : 'evict failed')
      setActionPending(false)
    }
  }

  if (notFound) {
    return (
      <div className="page">
        <header className="page__head">
          <div>
            <h1 className="page__title">Worker session not found</h1>
            <div className="page__subtitle">
              The session disconnected or was never registered.
              <Link to="/workers" className="link" style={{ marginLeft: 8 }}>← back to Workers</Link>
            </div>
          </div>
        </header>
      </div>
    )
  }

  if (loading && !detail) {
    return <div className="page"><div className="muted">Loading…</div></div>
  }

  if (!detail) {
    return (
      <div className="page">
        <div className="banner banner--error">{error || 'failed to load'}</div>
        <Link to="/workers" className="link">← back</Link>
      </div>
    )
  }

  const stale = isStale(detail.last_heartbeat_at)
  const heartbeatPill =
    detail.drained ? 'pill pill--muted' :
    stale ? 'pill pill--warn' :
    'pill pill--ok'
  const heartbeatLabel =
    detail.drained ? 'draining' :
    stale ? 'stale heartbeat' :
    'connected'

  return (
    <div className="page">
      <header className="page__head">
        <div>
          <div className="row" style={{ gap: 12, alignItems: 'center' }}>
            <Link to="/workers" className="link">← workers</Link>
          </div>
          <h1 className="page__title">
            <span className="mono">{detail.caller}</span>
            <span style={{ color: 'var(--ink-3)', margin: '0 8px' }}>·</span>
            <span>{detail.queue}</span>
          </h1>
          <div className="page__subtitle">
            <span className={heartbeatPill}>{heartbeatLabel}</span>
            <span style={{ margin: '0 8px', color: 'var(--ink-3)' }}>·</span>
            <span className="mono" style={{ fontSize: 11 }}>{detail.session_id}</span>
            <span style={{ margin: '0 8px', color: 'var(--ink-3)' }}>·</span>
            <span>pod {detail.pod_id || '—'}</span>
            <span style={{ margin: '0 8px', color: 'var(--ink-3)' }}>·</span>
            <span>connected {relativeAgo(detail.connected_at)}</span>
            {detail.sdk_version && (
              <>
                <span style={{ margin: '0 8px', color: 'var(--ink-3)' }}>·</span>
                <span>SDK {detail.sdk_version}</span>
              </>
            )}
          </div>
        </div>
        <div className="row" style={{ gap: 8 }}>
          {!detail.drained && (
            <button className="btn btn--ghost" onClick={() => { setActionError(null); setShowDrain(true) }}>
              <AlertTriangle size={14} style={{ marginRight: 6 }} />
              Drain
            </button>
          )}
          <button className="btn btn--danger" onClick={() => { setActionError(null); setShowEvict(true) }}>
            <ShieldOff size={14} style={{ marginRight: 6 }} />
            Evict
          </button>
        </div>
      </header>

      {error && <div className="banner banner--error">{error}</div>}

      <section className="cards" style={{ marginBottom: 24 }}>
        <CounterCard label="Dispatched" value={detail.dispatched} />
        <CounterCard label="Completed" value={detail.completed} />
        <CounterCard label="Failed" value={detail.failed} tone={detail.failed > 0 ? 'warn' : undefined} />
        <CounterCard label="Revoked" value={detail.revoked} tone={detail.revoked > 0 ? 'warn' : undefined} />
        <CounterCard label="In-flight" value={detail.inflight_count} sub={`of ${detail.max_in_flight} max`} />
      </section>

      <div className="grid" style={{ gridTemplateColumns: '2fr 1fr', gap: 24 }}>
        <section>
          <h2 className="section__title">In-flight jobs</h2>
          {detail.inflight.length === 0 ? (
            <div className="empty">No jobs currently in-flight on this session.</div>
          ) : (
            <table className="table">
              <thead>
                <tr>
                  <th>Job ID</th>
                  <th>Name</th>
                  <th>Dispatched</th>
                  <th>Ack</th>
                </tr>
              </thead>
              <tbody>
                {detail.inflight.map(r => (
                  <tr key={r.job_id}>
                    <td className="mono">{r.job_id}</td>
                    <td className="mono">{r.job_name}</td>
                    <td>{relativeAgo(r.dispatched_at)}</td>
                    <td>{r.ack_received ? <span className="pill pill--ok">acked</span> : <span className="pill pill--warn">pending</span>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </section>

        <section>
          <h2 className="section__title">Handles</h2>
          {detail.job_names.length === 0 ? (
            <div className="empty">No declared job names.</div>
          ) : (
            <ul className="list">
              {detail.job_names.map(n => (
                <li key={n} className="mono" style={{ fontSize: 13 }}>{n}</li>
              ))}
            </ul>
          )}
        </section>
      </div>

      <section style={{ marginTop: 32 }}>
        <h2 className="section__title">Recent events</h2>
        {detail.events.length === 0 ? (
          <div className="empty">No recorded events yet.</div>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>When</th>
                <th>Event</th>
                <th>Job</th>
                <th>Note</th>
              </tr>
            </thead>
            <tbody>
              {[...detail.events].reverse().map((e, i) => (
                <tr key={i}>
                  <td className="mono" style={{ fontSize: 12 }}>{relativeAgo(e.at)}</td>
                  <td>{e.kind}</td>
                  <td className="mono" style={{ fontSize: 12 }}>
                    {e.job_id ? `${e.job_id}${e.job_name ? ' ' + e.job_name : ''}` : '—'}
                  </td>
                  <td style={{ color: 'var(--ink-2)' }}>{e.note || ''}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      {showDrain && (
        <SudoConfirmDialog
          title="Drain worker"
          icon={<AlertTriangle size={18} />}
          body={
            <>
              <p>
                Dispatcher will stop pushing new jobs to this session. The {detail.inflight_count}
                {' '}in-flight job{detail.inflight_count === 1 ? '' : 's'} will finish normally; the
                session disconnects once in-flight reaches zero.
              </p>
              <p style={{ marginTop: 8, color: 'var(--ink-2)' }}>
                Safe choice for rolling out a worker change or shutting one down cleanly.
              </p>
            </>
          }
          confirmLabel="Drain"
          pending={actionPending}
          error={actionError}
          onCancel={() => setShowDrain(false)}
          onConfirm={drain}
        />
      )}

      {showEvict && (
        <SudoConfirmDialog
          title="Evict worker"
          icon={<ShieldOff size={18} />}
          body={
            <>
              <p>
                Force-close the stream immediately. The {detail.inflight_count}
                {' '}in-flight job{detail.inflight_count === 1 ? '' : 's'} will be returned to
                the queue and re-attempted by another worker.
              </p>
              <p style={{ marginTop: 8, color: 'var(--coral)' }}>
                Destructive. Use only for stuck or misbehaving workers.
              </p>
            </>
          }
          requiredText="evict"
          confirmLabel="Evict"
          pending={actionPending}
          error={actionError}
          onCancel={() => setShowEvict(false)}
          onConfirm={evict}
        />
      )}
    </div>
  )
}

function CounterCard({ label, value, sub, tone }: { label: string; value: number; sub?: string; tone?: 'warn' }) {
  return (
    <div className="card">
      <div className="card__head">
        <div className="card__title">{label}</div>
      </div>
      <div style={{ fontSize: 28, fontWeight: 600, color: tone === 'warn' ? 'var(--coral)' : undefined }}>
        {value.toLocaleString()}
      </div>
      {sub && <div style={{ color: 'var(--ink-3)', fontSize: 11 }}>{sub}</div>}
    </div>
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
