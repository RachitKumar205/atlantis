import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Search, Pause, Play } from 'lucide-react'
import { api, queries, type LogLevel, type LogRecord } from '@/api/client'
import { PageShell } from '@/components/PageShell'

// Compact "14d 06:21" / "06h 12m" / "47s" formatting for the uptime
// chip. Mirrors the design's monospace tabular feel.
function fmtUptime(startedAt?: string): string {
  if (!startedAt) return '—'
  const start = new Date(startedAt).getTime()
  if (isNaN(start)) return '—'
  const secs = Math.max(0, Math.floor((Date.now() - start) / 1000))
  const d = Math.floor(secs / 86400)
  const h = Math.floor((secs % 86400) / 3600)
  const m = Math.floor((secs % 3600) / 60)
  const pad = (n: number) => String(n).padStart(2, '0')
  if (d > 0) return `${d}d ${pad(h)}:${pad(m)}`
  if (h > 0) return `${pad(h)}h ${pad(m)}m`
  return `${secs}s`
}

// Probe header + log toolbar (level chips, search, pause/live toggle)
// + log stream with expandable .logrow__expand. Classes come from
// pages.css.

const LV: Record<LogLevel, string> = { debug: 'D', info: 'I', warn: 'W', error: 'E' }
const POLL_MS = 1100
const SOFT_CAP = 5000

function fmtTime(iso: string): string {
  const d = new Date(iso)
  const p = (n: number, w = 2) => String(n).padStart(w, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`
}

function attrPreview(attrs: Record<string, string>) {
  return Object.keys(attrs).slice(0, 3).map(k => (
    <span key={k}>
      <span className="ak">{k}=</span>
      <span className="av">{String(attrs[k]).slice(0, 28)}</span>
    </span>
  ))
}

// Probe header — six live chips wired to /api/health and /api/jobs/dead.
// /readyz + /healthz come from the upstream HTTP status codes; uptime
// is computed client-side from the server's started_at (re-render every
// second via a tick state). version is the schema version (matches the
// Schema page's sub).
function ProbeHeader() {
  const healthQ = useQuery({ ...queries.health(), refetchInterval: 5000 })
  const deadQ = useQuery({ ...queries.deadJobs(), refetchInterval: 10_000 })

  // Re-render once per second so uptime ticks forward without a fetch.
  const [, setTick] = useState(0)
  useEffect(() => {
    const id = setInterval(() => setTick(n => n + 1), 1000)
    return () => clearInterval(id)
  }, [])

  const atl = healthQ.data?.atlantis
  const deadJobs = deadQ.data?.jobs?.length ?? 0

  const readyzCode = atl?.readyz_code
  const healthzCode = atl?.healthz_code
  const versionStr = atl?.schema_version
    ? `v${String(atl.schema_version).padStart(4, '0')}`
    : '—'
  const uptimeStr = fmtUptime(atl?.started_at)
  const metricsCount = atl?.metrics_series

  const codeClass = (c?: number): string =>
    c == null ? '' : c === 200 ? 'is-ok' : 'is-err'

  return (
    <div className="probe">
      <div className="probe__inner">
        <div className="probe-chip-row">
          <span className={`probe-chip ${codeClass(readyzCode)}`}>
            <span className="probe-chip__status" />
            <span className="k">/readyz</span>
            <span className="v">{readyzCode ?? '—'}</span>
          </span>
          <span className={`probe-chip ${codeClass(healthzCode)}`}>
            <span className="probe-chip__status" />
            <span className="k">/healthz</span>
            <span className="v">{healthzCode ?? '—'}</span>
          </span>
          <span className="probe-chip">
            <span className="k">uptime</span>
            <span className="v">{uptimeStr}</span>
          </span>
          <span className="probe-chip">
            <span className="k">version</span>
            <span className="v">{versionStr}</span>
          </span>
          <span className={`probe-chip ${deadJobs > 0 ? 'is-warn' : ''}`}>
            {deadJobs > 0 && <span className="probe-chip__status" />}
            <span className="k">dead jobs</span>
            <span className="v">{deadJobs}</span>
          </span>
          <span className="spacer" style={{ flex: 1 }} />
          <span className="probe-chip">
            <span className="k">:8081/metrics</span>
            <span className="v">
              {metricsCount != null ? `${metricsCount} series` : '—'}
            </span>
          </span>
        </div>
      </div>
    </div>
  )
}

function LogRow({
  rec,
  isOpen,
  onToggle,
}: {
  rec: LogRecord
  isOpen: boolean
  onToggle: (seq: number) => void
}) {
  const raw = JSON.stringify({ time: rec.time, level: rec.level.toUpperCase(), msg: rec.msg, ...rec.attrs })
  return (
    <div
      className={`logrow lv-${rec.level} ${isOpen ? 'is-open' : ''}`}
      data-seq={rec.seq}
      onClick={() => onToggle(rec.seq)}
    >
      <span className="logrow__time">{fmtTime(rec.time)}</span>
      <span className={`lvl lvl--${rec.level}`}>{LV[rec.level]}</span>
      <span className="logrow__msg">{rec.msg}</span>
      <span className="logrow__attrs">{attrPreview(rec.attrs)}</span>
      <div className="logrow__expand">
        <dl className="kv">
          {Object.entries(rec.attrs).map(([k, v]) => (
            <div key={k} style={{ display: 'contents' }}>
              <dt>{k}</dt>
              <dd>
                {String(v).startsWith('http') ? (
                  <a href="#" onClick={e => e.preventDefault()}>{v}</a>
                ) : v}
              </dd>
            </div>
          ))}
        </dl>
        <div className="lograw">{raw}</div>
      </div>
    </div>
  )
}

export function Health() {
  const [records, setRecords] = useState<LogRecord[]>([])
  const [since, setSince] = useState(0)
  const [cold, setCold] = useState(true)
  const [paused, setPaused] = useState(false)
  const [level, setLevel] = useState<LogLevel | null>(null)
  const [q, setQ] = useState('')
  const [openSeq, setOpenSeq] = useState<number | null>(null)
  const [anchored, setAnchored] = useState(true)

  const streamRef = useRef<HTMLDivElement>(null)
  const sinceRef = useRef(since)
  sinceRef.current = since
  const pausedRef = useRef(paused)
  pausedRef.current = paused

  useEffect(() => {
    let cancelled = false
    let coldTimeout: ReturnType<typeof setTimeout> | null = null
    let interval: ReturnType<typeof setInterval> | null = null

    const tick = async () => {
      if (pausedRef.current) return
      try {
        const res = await api.logs.since(sinceRef.current)
        if (cancelled) return
        if (res.records.length) {
          setRecords(prev => {
            const merged = [...prev, ...res.records]
            return merged.length > SOFT_CAP ? merged.slice(merged.length - SOFT_CAP) : merged
          })
          setSince(res.last_seq)
        } else if (res.last_seq > sinceRef.current) {
          setSince(res.last_seq)
        }
      } catch {/* transient — retry next tick */}
    }

    coldTimeout = setTimeout(async () => {
      await tick()
      setCold(false)
      interval = setInterval(tick, POLL_MS)
    }, 850)

    return () => {
      cancelled = true
      if (coldTimeout) clearTimeout(coldTimeout)
      if (interval) clearInterval(interval)
    }
  }, [])

  // Auto-scroll while anchored to bottom; release on manual scroll-up.
  useEffect(() => {
    if (!anchored) return
    const el = streamRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [records, anchored])

  const onStreamScroll = () => {
    const el = streamRef.current
    if (!el) return
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40
    setAnchored(atBottom)
  }

  const counts = useMemo(() => {
    const c: Record<LogLevel, number> = { debug: 0, info: 0, warn: 0, error: 0 }
    records.forEach(r => { c[r.level] += 1 })
    return c
  }, [records])

  const filtered = useMemo(() => {
    if (!level && !q) return records
    const needle = q.toLowerCase()
    return records.filter(r => {
      if (level && r.level !== level) return false
      if (q) {
        const hay = (r.msg + ' ' + JSON.stringify(r.attrs)).toLowerCase()
        if (!hay.includes(needle)) return false
      }
      return true
    })
  }, [records, level, q])

  const toggle = (seq: number) => setOpenSeq(curr => (curr === seq ? null : seq))
  const clearFilters = () => { setLevel(null); setQ('') }

  // Cold-load skeleton — 16 dummy rows matching the design's skeleton().
  const skeleton = Array.from({ length: 16 }).map((_, i) => (
    <div key={i} className="logrow" style={{ cursor: 'default' }}>
      <span className="sk" style={{ height: 11, width: 72 }} />
      <span className="sk" style={{ height: 14, width: 14, borderRadius: 3 }} />
      <span className="sk" style={{ height: 11, width: `${30 + (i * 7) % 50}%` }} />
      <span />
    </div>
  ))

  const filterChip = (lvl: LogLevel | null, label: string) => (
    <button
      key={label}
      className={`filterchip ${level === lvl ? 'is-active' : ''}`}
      onClick={() => setLevel(lvl)}
    >
      {label}
      <span className="ct">{lvl ? counts[lvl] : records.length}</span>
    </button>
  )

  return (
    <PageShell title="Health" sub="live activity · ring buffer (last 5,000 lines) · polled @1s" flush>
    <div className="health">
      <ProbeHeader />

      <div className="logbar">
        <div className="logbar__inner">
          <div className="row" style={{ gap: 6 }}>
            {filterChip(null, 'all')}
            {filterChip('info', 'info')}
            {filterChip('warn', 'warn')}
            {filterChip('error', 'error')}
            {filterChip('debug', 'debug')}
          </div>
          <div className="logsearch">
            <Search />
            <input
              type="text"
              placeholder="filter substring…"
              value={q}
              onChange={e => setQ(e.target.value)}
            />
          </div>
          <span className="spacer" style={{ flex: 1 }} />
          <button
            className={`tailbtn ${paused ? 'is-paused' : 'is-live'}`}
            onClick={() => setPaused(p => !p)}
          >
            <span className="live-dot" />
            {paused ? 'Paused' : 'Live'}
            {paused ? <Play size={12} /> : <Pause size={12} />}
          </button>
        </div>
      </div>

      <div style={{ flex: 1, display: 'flex', minHeight: 0 }}>
        <div ref={streamRef} className="logstream" onScroll={onStreamScroll}>
          <div className="logstream__inner">
            {cold ? skeleton : filtered.length === 0 ? (
              <div className="empty">
                <div className="empty__icon">
                  <Search size={18} />
                </div>
                <div className="empty__title">
                  {level || q ? 'No lines match this filter' : 'No log activity yet'}
                </div>
                <div className="empty__sub">
                  {level || q
                    ? 'Clear the level or substring filter to see the full tail. The buffer holds the last 5,000 lines.'
                    : 'The server has not emitted any log lines since the ring buffer started. Activity will appear here as callers connect and schema changes flow through CI.'}
                </div>
                {(level || q) && (
                  <button className="btn btn--sm" onClick={clearFilters}>Clear filters</button>
                )}
              </div>
            ) : (
              filtered.map(r => (
                <LogRow key={r.seq} rec={r} isOpen={openSeq === r.seq} onToggle={toggle} />
              ))
            )}
          </div>
        </div>
      </div>
    </div>
    </PageShell>
  )
}
