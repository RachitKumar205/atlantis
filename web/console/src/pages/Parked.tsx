import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Archive, Check, Info, Warning } from '@phosphor-icons/react'
import { queries, type ParkedObject } from '@/api/client'
import { PageShell } from '@/components/PageShell'
import { HoverInfo } from '@/components/HoverInfo'

// Removing an entity or a field does not drop anything. The object is parked —
// a table moved into the tombstone schema, a column renamed out of the way —
// and stays recoverable for 30 days.
//
// This page exists because that window was invisible. The register was
// reachable only by hand-written SQL against a table no command, page or doc
// mentioned, which makes a recoverable delete indistinguishable from a
// permanent one at exactly the moment somebody needs to tell them apart.

function fmtWindow(reapAfter: string): { label: string; urgent: boolean } {
  const ms = new Date(reapAfter).getTime() - Date.now()
  if (isNaN(ms)) return { label: '—', urgent: false }
  if (ms <= 0) return { label: 'due now', urgent: true }
  const hours = ms / 3_600_000
  if (hours < 1) return { label: `${Math.floor(ms / 60_000)}m left`, urgent: true }
  if (hours < 48) return { label: `${Math.floor(hours)}h left`, urgent: true }
  const days = Math.floor(hours / 24)
  // Under a week is roughly the window in which somebody back from leave can
  // still change their mind, so it is worth marking.
  return { label: `${days}d left`, urgent: days < 7 }
}

// A single failed attempt is usually not a problem: the reaper declines a lock
// it would otherwise wait on after 3s, so one ordinary long transaction against
// a live table bumps attempts to 1 and the next run succeeds. Nothing resets
// the counter, so warning at 1 would leave a permanent banner on a healthy
// system — and a warning that is always on is not a warning.
const STUCK_ATTEMPTS = 2

function displayName(o: ParkedObject) {
  return o.kind === 'column'
    ? `${o.original_schema}.${o.parent_table}.${o.original_name}`
    : `${o.original_schema}.${o.original_name}`
}

export function Parked() {
  const [showReaped, setShowReaped] = useState(false)
  const { data, isLoading, error } = useQuery(queries.parkedObjects(showReaped))

  const objects = data?.objects ?? []
  const stuck = objects.filter((o) => !o.reaped_at && o.attempts >= STUCK_ATTEMPTS)

  return (
    <PageShell
      title="Parked"
      sub="Objects a destructive migration kept instead of dropping. Revert the schema change to restore one."
    >
      <div className="page__bodyinner">
      <div className="row" style={{ gap: 12, alignItems: 'center', marginBottom: 14 }}>
        <label className="checkbox">
          <input
            type="checkbox"
            checked={showReaped}
            onChange={(e) => setShowReaped(e.target.checked)}
          />
          <span className="checkbox__box"><Check size={11} /></span>
          <span>Include already reaped</span>
        </label>
        <HoverInfo
          inline
          content="A reaped object is an audit record: it existed and was dropped on that date. Its rows are gone and only a backup restores them."
        >
          <Info size={13} style={{ color: 'var(--ink-3)' }} />
        </HoverInfo>
      </div>

      {isLoading && (
        <div className="card">
          <div className="empty">
            <div className="empty__title">Loading…</div>
          </div>
        </div>
      )}

      {error && (
        <div className="banner banner--error">
          <Warning size={14} className="banner__icon" />
          <span>Could not read the register: {(error as Error).message}</span>
        </div>
      )}

      {!isLoading && !error && (
        <>
          {stuck.length > 0 && (
            <div className="banner banner--warn" style={{ marginBottom: 14 }}>
              <Warning size={14} className="banner__icon" />
              <span>
                {stuck.length} object{stuck.length === 1 ? '' : 's'} have failed to drop
                repeatedly. Usually something was created against them after they were
                parked — the reaper declines rather than cascading. Hover the state chip
                for the last error.
              </span>
            </div>
          )}

          {objects.length === 0 ? (
            <div className="card">
              <div className="empty">
                <div className="empty__icon"><Archive size={18} /></div>
                <div className="empty__title">
                  {showReaped ? 'Nothing has ever been parked' : 'Nothing is parked'}
                </div>
                <div className="empty__sub">
                  Removing an entity or a field parks it here, recoverable, before
                  anything is dropped.
                </div>
              </div>
            </div>
          ) : (
            <div className="card" style={{ padding: 6 }}>
            <table className="tbl">
              <caption className="sr-only">
                Objects a destructive migration parked instead of dropping, with
                how long each one remains recoverable.
              </caption>
              <thead>
                <tr>
                  <th>Kind</th>
                  <th>Object</th>
                  <th>Parked</th>
                  <th>Recoverable for</th>
                  <th>State</th>
                </tr>
              </thead>
              <tbody>
                {objects.map((o) => {
                  const w = fmtWindow(o.reap_after)
                  return (
                    <tr key={o.id}>
                      <td className="mono">{o.kind}</td>
                      <td className="mono">{displayName(o)}</td>
                      <td className="mono">{o.parked_at ? o.parked_at.slice(0, 10) : '—'}</td>
                      <td className="mono">
                        {o.reaped_at ? (
                          <span aria-label="Already reaped; no longer recoverable">—</span>
                        ) : (
                          <span className={w.urgent ? 'chip is-warn' : 'chip'}>{w.label}</span>
                        )}
                      </td>
                      <td>
                        {o.reaped_at ? (
                          <span className="chip">reaped {o.reaped_at.slice(0, 10)}</span>
                        ) : o.attempts >= STUCK_ATTEMPTS ? (
                          <HoverInfo
                            inline
                            content={
                              <span className="mono" style={{ fontSize: 11.5 }}>
                                {o.last_error || 'No error was recorded.'}
                              </span>
                            }
                          >
                            <button
                              type="button"
                              className="chip is-warn"
                              style={{ cursor: 'help', font: 'inherit' }}
                              aria-label={`Failed to drop ${o.attempts} times. ${
                                o.last_error || 'No error was recorded.'
                              }`}
                            >
                              stuck ({o.attempts})
                            </button>
                          </HoverInfo>
                        ) : (
                          <span className="chip is-ok">recoverable</span>
                        )}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
            </div>
          )}

          {data?.has_more && (
            <div className="page__sub" style={{ marginTop: 12, whiteSpace: 'normal' }}>
              More objects are parked than are shown here. This list is capped
              server-side; use <code>tide parked</code> to page through the rest.
            </div>
          )}

          <div className="page__sub" style={{ marginTop: 18, whiteSpace: 'normal' }}>
            The retention window is fixed on each object when it is parked, so a later
            change to the default never shortens one already promised. To keep something
            longer, push out <code>reap_after</code> in{' '}
            <code>atlantis.parked_objects</code>. To reclaim the space now, clear{' '}
            <code>next_attempt_after</code> as well — a failed attempt backs an object
            off for up to a day, and backdating <code>reap_after</code> alone will not
            move it.
          </div>
        </>
      )}
      </div>
    </PageShell>
  )
}
