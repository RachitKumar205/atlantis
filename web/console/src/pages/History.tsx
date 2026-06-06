import { useState, useMemo, useCallback } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { Box, ChevronRight } from 'lucide-react'
import { queries, type SchemaVersionSummary, type DiffPayload, type SchemaVersionDetail } from '@/api/client'
import { PageShell } from '@/components/PageShell'
import { Sql } from '@/components/Sql'

// ── helpers ───────────────────────────────────────────────────────────────
function relativeTime(ts: string): string {
  const then = new Date(ts).getTime()
  if (isNaN(then)) return ts
  const diffMs = Date.now() - then
  const sec = Math.floor(diffMs / 1000)
  if (sec < 60) return `${sec}s ago`
  const min = Math.floor(sec / 60)
  if (min < 60) return `${min}m ago`
  const hr = Math.floor(min / 60)
  if (hr < 24) return `${hr}h ago`
  const day = Math.floor(hr / 24)
  if (day < 30) return `${day}d ago`
  return new Date(ts).toLocaleDateString()
}

function dayKey(ts: string): string {
  const d = new Date(ts)
  if (isNaN(d.getTime())) return 'unknown'
  return d.toLocaleDateString(undefined, { weekday: 'long', month: 'short', day: 'numeric' })
}

function parseRawDiff(raw: unknown): DiffPayload {
  if (!raw || typeof raw !== 'object') return { additive: [], backfill_required: [], breaking: [] }
  const d = raw as Record<string, unknown>
  return {
    additive: Array.isArray(d.additive) ? d.additive : [],
    backfill_required: Array.isArray(d.backfill_required) ? d.backfill_required : [],
    breaking: Array.isArray(d.breaking) ? d.breaking : [],
  }
}

// One badge per non-additive change class in a version. Mirrors the
// design's badge--add / badge--back / badge--break classes.
function ChangeBadges({ summary }: { summary: SchemaVersionSummary }) {
  const cls = summary.plan_class
  const count = summary.change_count
  if (count === 0) return null
  const k =
    cls === 'cross_caller_breaking' ? 'break' :
    cls === 'backfill_required'     ? 'back'  :
    cls === 'additive'              ? 'add'   :
                                      'plain'
  return <span className={`badge badge--${k}`}>{cls} · {count}</span>
}

// ── node ──────────────────────────────────────────────────────────────────
function VersionNode({ version }: { version: SchemaVersionSummary }) {
  const [open, setOpen] = useState(false)
  const detailQ = useQuery({
    ...queries.historyVersion(version.version),
    enabled: open,
  })
  const toggle = useCallback(() => setOpen(v => !v), [])

  const versionStr = `v${String(version.version).padStart(4, '0')}`

  return (
    <div className={`tlnode ${open ? 'is-open' : ''}`} data-ver={versionStr}>
      <div className="tlnode__dot" />
      <div className="tlnode__card">
        <div className="tlnode__bar" onClick={toggle}>
          <span className="tlnode__ver">{versionStr}</span>
          <span className="tlnode__caller mono">{version.caller}</span>
          {version.ir_hash && (
            <span
              className="tlnode__hash mono"
              title={version.ir_hash}
            >
              {version.ir_hash.slice(0, 7)}
            </span>
          )}
          <span className="tlnode__time">{relativeTime(version.created_at)}</span>
          <span className="tlnode__badges">
            <ChangeBadges summary={version} />
            {version.event_type === 'rollback' && (
              <span className="badge badge--back">rollback</span>
            )}
          </span>
        </div>

        <div className="tlnode__detail">
          {detailQ.isLoading && (
            <div style={{ padding: '12px 16px' }}>
              <div className="sk" style={{ height: 60 }} />
            </div>
          )}
          {detailQ.isError && (
            <div style={{ padding: '12px 16px', color: 'var(--coral)', fontFamily: 'var(--mono)', fontSize: 12 }}>
              {detailQ.error?.message}
            </div>
          )}
          {detailQ.data && <DiffSection detail={detailQ.data} />}
        </div>
      </div>
    </div>
  )
}

function DiffSection({ detail }: { detail: SchemaVersionDetail }) {
  const diff = parseRawDiff(detail.diff)
  const rows = [
    ...diff.additive.map(c => ['+', 'add', c]),
    ...diff.backfill_required.map(c => ['~', 'back', c]),
    ...diff.breaking.map(c => ['!', 'break', c]),
  ] as [string, 'add' | 'back' | 'break', DiffPayload['additive'][number]][]

  return (
    <>
      {detail.ir_hash && (
        <div
          className="tlnode__hash-row mono"
          style={{ padding: '8px 16px 0', fontSize: 11, color: 'var(--ink-3)' }}
        >
          <span style={{ marginRight: 8 }}>content</span>
          <span style={{ color: 'var(--ink-1)' }}>{detail.ir_hash}</span>
        </div>
      )}

      <div className="diff">
        {rows.length === 0 ? (
          <div className="diffrow"><span className="diffrow__sign">·</span><span>No structural changes.</span></div>
        ) : (
          rows.map(([sign, k, c], i) => (
            <div key={i} className={`diffrow d-${k}`}>
              <span className="diffrow__sign">{sign}</span>
              <span>
                <strong style={{ color: 'var(--ink-0)' }}>{c.entity_id}</strong>
                {c.field && <> · {c.field}</>}
                {c.detail && <> — {c.detail}</>}
              </span>
            </div>
          ))
        )}
      </div>

      {detail.up_sql && (
        <details className="sqlblock">
          <summary>
            <ChevronRight size={12} /> up migration SQL
          </summary>
          <Sql>{detail.up_sql}</Sql>
        </details>
      )}

      <div className="tlnode__sandbox-cta">
        <SandboxLaunchButton />
      </div>
    </>
  )
}

// SandboxLaunchButton is the History-page contextual entry. We pass
// ?boot=sim so /sandbox auto-boots from the current IR on landing.
// Phase 2 doesn't yet support booting a sandbox at a specific
// historical version — the current IR is good enough for "I want to
// explore this entity's data shape."
function SandboxLaunchButton() {
  const navigate = useNavigate()
  return (
    <button
      className="btn btn--ghost"
      style={{ marginTop: 4 }}
      onClick={() => navigate({ to: '/sandbox', search: { boot: 'sim' } })}
      title="Open a sandbox booted from the current schema."
    >
      <Box size={13} />
      <span>Open in sandbox</span>
    </button>
  )
}

// ── page ───────────────────────────────────────────────────────────────────
export function History() {
  const [before, setBefore] = useState<number | undefined>()
  const [caller, setCaller] = useState<string | null>(null)
  const { data, isLoading, error } = useQuery(queries.historyList({ before, caller: caller ?? undefined }))
  const versions = data?.versions ?? []

  const callers = useMemo(() => {
    const s = new Set<string>()
    versions.forEach(v => s.add(v.caller))
    return Array.from(s).sort()
  }, [versions])

  const grouped = useMemo(() => {
    const groups: { day: string; items: SchemaVersionSummary[] }[] = []
    for (const v of versions) {
      const day = dayKey(v.created_at)
      const last = groups[groups.length - 1]
      if (last && last.day === day) last.items.push(v)
      else groups.push({ day, items: [v] })
    }
    return groups
  }, [versions])

  const loadMore = () => {
    const last = versions[versions.length - 1]
    if (last) setBefore(last.version)
  }

  return (
    <PageShell title="History" sub={`${versions.length} versions · schema changes · newest first`}>
        <div className="page__bodyinner">
          {/* page__head is now rendered by PageShell. */}
          {callers.length > 1 && (
            <div className="row" style={{ marginBottom: 18, gap: 8, flexWrap: 'wrap' }}>
              <span className="section-label">Filter caller</span>
              <div className="row" style={{ gap: 6, flexWrap: 'wrap' }}>
                <button
                  className={`filterchip ${!caller ? 'is-active' : ''}`}
                  onClick={() => setCaller(null)}
                >
                  all
                </button>
                {callers.map(c => (
                  <button
                    key={c}
                    className={`filterchip ${caller === c ? 'is-active' : ''}`}
                    onClick={() => setCaller(caller === c ? null : c)}
                    style={{ fontFamily: 'var(--mono)' }}
                  >
                    {c}
                  </button>
                ))}
              </div>
            </div>
          )}

          {isLoading ? (
            <div>
              {[0, 1, 2].map(i => (
                <div key={i} className="sk" style={{ height: 60, marginBottom: 8, borderRadius: 'var(--radius-lg)' }} />
              ))}
            </div>
          ) : error ? (
            <div className="banner banner--error">{(error as Error).message}</div>
          ) : versions.length === 0 ? (
            <div className="empty">
              <div className="empty__title">
                No schema versions {caller ? `from ${caller}` : 'yet'}
              </div>
              <div className="empty__sub">
                {caller
                  ? 'This caller has not pushed a schema change in the visible window.'
                  : 'Schema versions appear here as callers register and run tide apply.'}
              </div>
            </div>
          ) : (
            <div className="timeline">
              {grouped.map((g, gi) => (
                <div key={`${g.day}-${gi}`} className="tl-daygroup">
                  <div className="tl-dayhead">
                    <h4>{g.day}</h4>
                    <div className="line" />
                  </div>
                  {g.items.map(v => <VersionNode key={v.version} version={v} />)}
                </div>
              ))}
              {data?.has_more && (
                <div style={{ textAlign: 'center', padding: 20 }}>
                  <button className="btn" onClick={loadMore}>Load older versions</button>
                </div>
              )}
            </div>
          )}
        </div>
    </PageShell>
  )
}
