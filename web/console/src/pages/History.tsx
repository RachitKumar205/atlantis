import { useState, useMemo, useCallback } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { CaretRight, Cube } from '@phosphor-icons/react'
import {
  queries,
  parseRawDiff,
  orderedBuckets,
  planClassBadge,
  type SchemaVersionSummary,
  type SchemaVersionDetail,
} from '@/api/client'
import { PageShell } from '@/components/PageShell'
import { Timestamp } from '@/components/Timestamp'
import { Actor } from '@/components/Actor'
import { Sql } from '@/components/Sql'

// ── helpers ───────────────────────────────────────────────────────────────
function dayKey(ts: string): string {
  const d = new Date(ts)
  if (isNaN(d.getTime())) return 'unknown'
  return d.toLocaleDateString(undefined, { weekday: 'long', month: 'short', day: 'numeric' })
}

// One badge per change class in a version.
//
// Looked up rather than matched arm by arm. A class with no arm fell through to
// 'plain', the grey "nothing notable" badge, so the one version in the list
// that destroyed data was the one styled to be ignored.
//
// planClassBadge, not a direct index: these summaries come from /api/history,
// which carries the hyphenated spelling of the class. Indexing the map with it
// misses — which is why breaking and backfill-required versions have been grey
// here too, not only destructive ones.
// Who a version is attributed to.
//
// A person and a machine are different answers and have to read apart without
// colour: a named person is prose, a caller keeps the monospace this page
// gives every identifier. Same rule the schema browser uses.
// What a version did, for the events whose diff is empty by construction.
//
// An adopt records a database that already existed, so it has no structural
// change to list and never will. "No structural changes" is true and tells a
// reader nothing; the number of tables it took responsibility for is the
// thing that happened.
function eventSummary(v: SchemaVersionSummary): string | null {
  if (v.change_count > 0) return null
  const n = v.entity_count ?? 0
  if (v.event_type === 'adopt') {
    return n > 0 ? `Baselined ${n} ${n === 1 ? 'table' : 'tables'} that already existed.` : null
  }
  if (v.event_type === 'seed') {
    return n > 0 ? `Seeded ${n} ${n === 1 ? 'table' : 'tables'}.` : null
  }
  return null
}

function ChangeBadges({ summary }: { summary: SchemaVersionSummary }) {
  const cls = summary.plan_class
  const count = summary.change_count
  // An adopt carries no changes and still happened. Falling through to null
  // left the row with no badge at all, which read as an apply that did nothing.
  if (count === 0) {
    if (summary.event_type === 'adopt' || summary.event_type === 'seed') {
      return <span className="badge badge--back">{summary.event_type}</span>
    }
    return null
  }
  return <span className={`badge badge--${planClassBadge(cls)}`}>{cls} · {count}</span>
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
          {(() => {
            return (
              <span className="tlnode__actor">
                <Actor
                  of={{
                    caller: version.caller,
                    actor: version.actor ?? '',
                    actor_email: version.actor_email ?? '',
                    actor_name: version.actor_name ?? '',
                  }}
                  fallbackCaller={version.caller}
                />
              </span>
            )
          })()}
          {version.ir_hash && (
            <span
              className="tlnode__hash mono"
              title={version.ir_hash}
            >
              {version.ir_hash.slice(0, 7)}
            </span>
          )}
          <Timestamp at={version.created_at} className="tlnode__time" />
          <span className="tlnode__badges">
            <ChangeBadges summary={version} />
            {version.event_type === 'rollback' && (
              <span className="badge badge--back">rollback</span>
            )}
            {version.applied_under_policy && (
              <span
                className="badge"
                title={`Applied unattended under the ${version.applied_under_policy} tier${
                  version.applied_verdict ? `, on a ${version.applied_verdict} rehearsal` : ''}`}
              >
                auto · {version.applied_under_policy.replaceAll('_', ' ')}
                {version.applied_verdict ? ` · ${version.applied_verdict.replaceAll('_', ' ')}` : ''}
              </span>
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
          {detailQ.data && <DiffSection detail={detailQ.data} summary={version} />}
        </div>
      </div>
    </div>
  )
}

function DiffSection({ detail, summary }: { detail: SchemaVersionDetail; summary?: SchemaVersionSummary }) {
  const diff = parseRawDiff(detail.diff)
  // Built from whichever buckets the payload actually carries. Listing three
  // by name is what hid destructive changes here for a release.
  const rows = orderedBuckets(diff).flatMap(({ bucket, glyph, badge }) =>
    diff[bucket].map(change => ({ glyph, badge, change })))

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
          <div className="diffrow">
            <span className="diffrow__sign">·</span>
            <span>{summary ? eventSummary(summary) ?? 'No structural changes.' : 'No structural changes.'}</span>
          </div>
        ) : (
          rows.map(({ glyph, badge, change: c }, i) => (
            <div key={i} className={`diffrow d-${badge}`}>
              <span className="diffrow__sign">{glyph}</span>
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
            <CaretRight size={12} /> up migration SQL
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
      <Cube size={13} />
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
