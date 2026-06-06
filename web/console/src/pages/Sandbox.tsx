// Sandbox is the in-app surface for booting isolated, in-memory test
// databases. Each sandbox is a fresh, empty copy of the production
// schema; state lives only in this process and is destroyed when the
// sandbox is closed or the BFF restarts.
//
// Two backends: "sim" is the pure-Go in-memory simulator; "embedded"
// boots a Postgres process in-tree (~5 s cold start, full SQL). The
// wire value is "embedded" everywhere — the UI labels it "Postgres"
// for users.
//
// Layout: thin status strip across the top when a sandbox is active,
// then a two-column grid — left rail for boot controls + sandbox list
// + checkpoints, right pane for the workbench tabs.

import { useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import {
  Cpu,
  Database,
  Download,
  GitCompare,
  GitFork,
  Plus,
  RotateCcw,
  Trash2,
} from 'lucide-react'
import {
  api,
  queries,
  type SandboxBootRequest,
  type SandboxListEntry,
  type SandboxTableDiff,
} from '@/api/client'
import { PageShell } from '@/components/PageShell'
import { Sql } from '@/components/Sql'
import { RowsTable } from '@/components/RowsTable'

// ─────────────────────────── tabs + checkpoint store ───────────────────────────

type Tab = 'tables' | 'seed' | 'sql' | 'compare' | 'forks'

interface PerfCounters {
  bootMs?: number
  markUs?: number
  queryUs?: number
  bulkUs?: number
  forkUs?: number
}

// Checkpoint mirrors a server-side "mark" but holds enough metadata to
// re-render the timeline after a browser reload. The runtime exposes
// only the per-sandbox mark id; the SPA owns descriptors (capture time,
// latency) and persists them to localStorage so refreshing the tab
// doesn't black out the Compare tab.
interface Checkpoint {
  id: string
  capturedUs: number
  at: number // epoch ms
}

const ckptKey = (pubID: string) => `atl.sandbox.checkpoints.${pubID}`

function loadCheckpoints(pubID: string): Checkpoint[] {
  try {
    const raw = localStorage.getItem(ckptKey(pubID))
    if (!raw) return []
    const parsed = JSON.parse(raw)
    return Array.isArray(parsed) ? parsed : []
  } catch {
    return []
  }
}

function saveCheckpoints(pubID: string, list: Checkpoint[]) {
  try {
    localStorage.setItem(ckptKey(pubID), JSON.stringify(list))
  } catch {
    /* quota exceeded — drop silently */
  }
}

// ─────────────────────────── page ───────────────────────────

export function Sandbox() {
  // Contextual deep links: /sandbox?focus=ns.Entity opens the Tables
  // tab pre-filled; ?boot=sim auto-boots on first land. Used by the
  // Schema / History / Operations "Try in sandbox" entry points.
  const search = useSearch({ strict: false }) as {
    focus?: string
    boot?: 'sim' | 'embedded'
  }
  const navigate = useNavigate()

  const [activePub, setActivePub] = useState<string | null>(null)
  const [tab, setTab] = useState<Tab>('tables')
  // Perf counters are kept PER sandbox so switching between two booted
  // sandboxes doesn't show the other one's last query / checkpoint
  // latency in the strip.
  const [perfByPub, setPerfByPub] = useState<Record<string, PerfCounters>>({})
  const [checkpoints, setCheckpoints] = useState<Record<string, Checkpoint[]>>({})
  const [toast, setToast] = useState<string | null>(null)
  const [strict, setStrict] = useState(false)
  const [seedInput, setSeedInput] = useState('')
  const [autoBootTried, setAutoBootTried] = useState(false)
  const qc = useQueryClient()

  // Single timer for the toast — overlapping calls would otherwise race
  // and clobber each other, dismissing toasts before the user can read.
  const toastTimer = useRef<number | null>(null)
  const fire = (msg: string) => {
    if (toastTimer.current !== null) {
      clearTimeout(toastTimer.current)
    }
    setToast(msg)
    toastTimer.current = window.setTimeout(() => {
      setToast(null)
      toastTimer.current = null
    }, 2800)
  }

  // setPerf merges into the active sandbox's slot. Callers don't have to
  // pass the pubID because every event is scoped to whatever sandbox is
  // active at the moment it fires.
  const setPerf = (patch: PerfCounters) => {
    if (!activePub) return
    setPerfByPub(prev => ({ ...prev, [activePub]: { ...(prev[activePub] ?? {}), ...patch } }))
  }
  const perf = activePub ? (perfByPub[activePub] ?? {}) : {}

  // 30s tick so relative timestamp labels ("2m ago") re-render against
  // the current wall clock without waiting for some other state update.
  // Sub-minute precision isn't needed — the formatter floors to seconds
  // and we only render minute/hour buckets after 60s.
  const [, forceTick] = useState(0)
  useEffect(() => {
    const id = window.setInterval(() => forceTick(t => t + 1), 30_000)
    return () => window.clearInterval(id)
  }, [])

  const listQ = useQuery(queries.sandboxList())
  const sandboxes = listQ.data?.sandboxes ?? []
  useEffect(() => {
    if (activePub && sandboxes.some(s => s.pub_id === activePub)) return
    setActivePub(sandboxes.length > 0 ? sandboxes[0].pub_id : null)
  }, [activePub, sandboxes])

  // Hydrate checkpoints for the active sandbox from localStorage so a
  // reload doesn't wipe the timeline (server still holds the underlying
  // marks; the SPA just needs to remember their ids + metadata).
  useEffect(() => {
    if (!activePub) return
    if (checkpoints[activePub]) return
    const persisted = loadCheckpoints(activePub)
    if (persisted.length > 0) {
      setCheckpoints(prev => ({ ...prev, [activePub]: persisted }))
    }
  }, [activePub, checkpoints])

  const active = sandboxes.find(s => s.pub_id === activePub) ?? null
  const activeMarks = activePub ? (checkpoints[activePub] ?? []) : []

  const bootMut = useMutation({
    mutationFn: (req: SandboxBootRequest) => api.sandbox.boot(req),
    onSuccess: (resp) => {
      // Seed the new sandbox's perf slot with its boot time. Switching
      // back to this sandbox later still shows its real boot latency.
      setPerfByPub(prev => ({ ...prev, [resp.pub_id]: { bootMs: resp.boot_ms } }))
      setActivePub(resp.pub_id)
      qc.invalidateQueries({ queryKey: ['sandbox', 'list'] })
      fire(`Booted ${resp.backend === 'sim' ? 'in-memory' : 'Postgres'} sandbox in ${resp.boot_ms} ms`)
    },
    onError: (err: unknown) => fire(`Boot failed — ${(err as Error).message}`),
  })

  const boot = (backend: 'sim' | 'embedded') => {
    let seed: number | undefined
    if (seedInput.trim()) {
      const n = Number(seedInput)
      // Number("-") and similar partial inputs evaluate to NaN. Reject
      // those silently rather than sending seed: null to the BFF.
      if (Number.isFinite(n)) seed = n
    }
    bootMut.mutate({
      backend,
      determinism: strict ? 'strict' : undefined,
      seed,
    })
  }

  // Auto-boot from a ?boot= deep link arriving from Schema / History /
  // Operations. Guarded so a transient failure can't loop.
  useEffect(() => {
    if (autoBootTried) return
    if (!listQ.isSuccess) return
    if (sandboxes.length > 0) return
    if (search.boot !== 'sim' && search.boot !== 'embedded') return
    setAutoBootTried(true)
    bootMut.mutate({
      backend: search.boot,
      determinism: strict ? 'strict' : undefined,
    })
  }, [autoBootTried, listQ.isSuccess, sandboxes.length, search.boot, bootMut, strict])

  // Strip the ?boot= param after a successful auto-boot so a reload
  // doesn't re-trigger it.
  useEffect(() => {
    if (bootMut.isSuccess && search.boot) {
      navigate({
        to: '/sandbox',
        search: search.focus ? { focus: search.focus } : {},
        replace: true,
      })
    }
  }, [bootMut.isSuccess, search.boot, search.focus, navigate])

  const destroyMut = useMutation({
    mutationFn: (pubID: string) => api.sandbox.destroy(pubID),
    onSuccess: (_resp, pubID) => {
      // Clear the local checkpoint history + perf slot for the
      // destroyed sandbox so a future sandbox at a (very unlikely)
      // colliding pubID starts clean.
      try { localStorage.removeItem(ckptKey(pubID)) } catch { /* ignore */ }
      setCheckpoints(prev => {
        const next = { ...prev }
        delete next[pubID]
        return next
      })
      setPerfByPub(prev => {
        const next = { ...prev }
        delete next[pubID]
        return next
      })
      qc.invalidateQueries({ queryKey: ['sandbox', 'list'] })
      fire('Destroyed')
    },
    onError: (err: unknown) => fire(`Destroy failed — ${(err as Error).message}`),
  })

  const markMut = useMutation({
    mutationFn: () => api.sandbox.mark(activePub!),
    onSuccess: (resp) => {
      const us = resp.t_server_us ?? 0
      setPerf({ markUs: us })
      setCheckpoints(prev => {
        const list = [{ id: resp.mark_id, capturedUs: us, at: Date.now() }, ...(prev[activePub!] ?? [])]
        saveCheckpoints(activePub!, list)
        return { ...prev, [activePub!]: list }
      })
      fire(`Checkpoint captured in ${us} µs`)
    },
    onError: (err: unknown) => fire(`Checkpoint failed — ${(err as Error).message}`),
  })

  const restoreMut = useMutation({
    mutationFn: (markID: string) => api.sandbox.restore(activePub!, markID),
    onSuccess: () => fire('Restored to checkpoint'),
    onError: (err: unknown, markID: string) => {
      const msg = (err as Error).message
      // The server returns "no mark <id>" when the mark id is unknown,
      // which happens after a BFF restart (in-memory marks table is
      // gone but our localStorage still has the id). Prune the stale
      // entry so the user can't keep clicking a dead button.
      if (/no mark|404/i.test(msg) && activePub) {
        setCheckpoints(prev => {
          const list = (prev[activePub] ?? []).filter(m => m.id !== markID)
          saveCheckpoints(activePub, list)
          return { ...prev, [activePub]: list }
        })
        fire('Checkpoint expired (sandbox restarted)')
        return
      }
      fire(`Restore failed — ${msg}`)
    },
  })

  const downloadSnapshot = () => {
    if (!activePub) return
    // Same-origin GET carries the session cookie automatically. Use a
    // hidden anchor with `download` so the browser names the file.
    const a = document.createElement('a')
    a.href = `/api/sandbox/${encodeURIComponent(activePub)}/snapshot`
    a.download = `sandbox-${activePub.slice(0, 8)}.gob`
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
  }

  return (
    <PageShell
      title="Sandbox"
      sub="Isolated test databases · state lives in memory · destroyed on close"
    >
      <div className="sandbox-page">
        <ActiveStrip
          active={active}
          perf={perf}
          strict={strict}
          onDownloadSnapshot={downloadSnapshot}
          onDestroy={(id) => {
            if (confirm('Destroy this sandbox? Local state will be lost.')) destroyMut.mutate(id)
          }}
        />

        <div className="sandbox-grid">
          <aside className="sandbox-rail">
            <BootBlock
              pending={bootMut.isPending}
              error={bootMut.error as Error | null}
              strict={strict}
              onStrict={setStrict}
              seed={seedInput}
              onSeed={setSeedInput}
              onBoot={boot}
              hasActive={!!activePub}
            />

            <SandboxesList
              list={sandboxes}
              activePub={activePub}
              onSelect={setActivePub}
            />

            <CheckpointsList
              backend={active?.backend}
              marks={activeMarks}
              capturePending={markMut.isPending}
              restorePending={restoreMut.isPending}
              hasActive={!!activePub}
              onCapture={() => markMut.mutate()}
              onRestore={(id) => restoreMut.mutate(id)}
            />
          </aside>

          <main className="sandbox-workbench">
            <nav className="wb-tabs" role="tablist">
              <TabBtn id="tables" current={tab} disabled={!activePub} onClick={setTab}>Tables</TabBtn>
              <TabBtn id="seed" current={tab} disabled={!activePub} onClick={setTab}>Seed data</TabBtn>
              <TabBtn id="sql" current={tab} disabled={!activePub} onClick={setTab}>SQL</TabBtn>
              <TabBtn id="compare" current={tab} disabled={!activePub} onClick={setTab}>Compare</TabBtn>
              <TabBtn id="forks" current={tab} disabled={!activePub} onClick={setTab}>Forks</TabBtn>
            </nav>

            <div className="wb-body">
              {!activePub && <EmptyWorkbench />}
              {activePub && tab === 'tables' && (
                <TablesTab
                  pubID={activePub}
                  backend={active?.backend}
                  initialFocus={search.focus}
                />
              )}
              {activePub && tab === 'seed' && (
                <SeedTab
                  pubID={activePub}
                  backend={active?.backend}
                  onInsert={(n, us) => {
                    setPerf({ bulkUs: us })
                    fire(`Inserted ${n.toLocaleString()} rows in ${us} µs`)
                  }}
                />
              )}
              {activePub && tab === 'sql' && (
                <SqlTab
                  pubID={activePub}
                  onLatency={(us) => setPerf({ queryUs: us })}
                />
              )}
              {activePub && tab === 'compare' && (
                <CompareTab pubID={activePub} backend={active?.backend} marks={activeMarks} />
              )}
              {activePub && tab === 'forks' && (
                <ForksTab
                  pubID={activePub}
                  active={active}
                  sandboxes={sandboxes}
                  onSelect={setActivePub}
                  onForked={(us) => {
                    setPerf({ forkUs: us })
                    qc.invalidateQueries({ queryKey: ['sandbox', 'list'] })
                  }}
                />
              )}
            </div>
          </main>
        </div>
      </div>

      {toast && (
        <div className="toast-wrap">
          <div className="toast">{toast}</div>
        </div>
      )}
    </PageShell>
  )
}

// ─────────────────────────── tab button ───────────────────────────

function TabBtn({ id, current, onClick, disabled, children }: {
  id: Tab
  current: Tab
  onClick: (t: Tab) => void
  disabled?: boolean
  children: React.ReactNode
}) {
  return (
    <button
      className={`wb-tab ${id === current ? 'is-active' : ''}`}
      role="tab"
      aria-selected={id === current}
      disabled={disabled}
      onClick={() => onClick(id)}
    >
      {children}
    </button>
  )
}

// ─────────────────────────── active status strip ───────────────────────────

interface ActiveStripProps {
  active: SandboxListEntry | null
  perf: PerfCounters
  strict: boolean
  onDownloadSnapshot: () => void
  onDestroy: (pubID: string) => void
}
function ActiveStrip({ active, perf, strict, onDownloadSnapshot, onDestroy }: ActiveStripProps) {
  if (!active) {
    return (
      <div className="active-strip is-empty">
        No sandbox active. Boot one with <strong>+ In-memory</strong> or <strong>+ Postgres</strong> to begin.
      </div>
    )
  }
  const isSim = active.backend === 'sim'
  // Snapshot/Checkpoint/Seed/Compare all rely on sim-internal state
  // capture; the embedded backend exposes only SQL. Hide Snapshot here
  // so users don't get a "Snapshot is in-memory only" 400.
  const last = (label: string, val?: string) => (
    <span className="strip-stat">
      <span className="strip-stat-l">{label}</span>
      <span className="strip-stat-v mono">{val ?? '—'}</span>
    </span>
  )
  return (
    <div className="active-strip">
      <span className={`strip-chip ${isSim ? 'chip-sim' : 'chip-pg'}`}>
        {isSim ? <Cpu size={12} /> : <Database size={12} />}
        {isSim ? 'in-memory' : 'Postgres'}
      </span>
      <span className="strip-id mono" title={active.pub_id}>{active.pub_id.slice(0, 8)}</span>
      {active.schema_version && (
        <span className="strip-schema mono" title={active.schema_version}>
          schema {active.schema_version.slice(0, 7)}
        </span>
      )}
      {strict && <span className="strip-tag">deterministic</span>}

      <span className="strip-sep" />

      {last('boot', perf.bootMs !== undefined ? `${perf.bootMs} ms` : `${active.boot_ms} ms`)}
      {last('last query', perf.queryUs !== undefined ? `${perf.queryUs} µs` : undefined)}
      {last('last checkpoint', perf.markUs !== undefined ? `${perf.markUs} µs` : undefined)}
      {perf.forkUs !== undefined && last('last fork', `${perf.forkUs} µs`)}
      {perf.bulkUs !== undefined && last('last seed', `${perf.bulkUs} µs`)}

      <span className="strip-sep" />

      {isSim && (
        <button className="strip-btn" onClick={onDownloadSnapshot} title="Download a binary snapshot you can replay later">
          <Download size={12} /> Snapshot
        </button>
      )}
      <button
        className="strip-btn strip-btn-danger"
        onClick={() => onDestroy(active.pub_id)}
        title="Destroy this sandbox and free its memory"
      >
        <Trash2 size={12} /> Destroy
      </button>
    </div>
  )
}

// ─────────────────────────── boot block (left rail) ───────────────────────────

interface BootBlockProps {
  pending: boolean
  error: Error | null
  hasActive: boolean
  strict: boolean
  onStrict: (v: boolean) => void
  seed: string
  onSeed: (v: string) => void
  onBoot: (backend: 'sim' | 'embedded') => void
}
function BootBlock({ pending, error, hasActive, strict, onStrict, seed, onSeed, onBoot }: BootBlockProps) {
  return (
    <section className="rail-section rail-boot">
      <header className="rail-h">
        <span>Boot</span>
      </header>
      <div className="rail-body">
        <div className="boot-grid">
          <HoverInfo
            side="bottom"
            content={
              <>
                <p>A disposable schema you can fork, rewind, and seed in microseconds. Built for agent loops.</p>
                <p className="hi-foot">No joins, CTEs, or pgvector similarity search.</p>
              </>
            }
          >
            <button
              className="btn boot-btn"
              disabled={pending}
              onClick={() => onBoot('sim')}
            >
              <Cpu size={13} /> {pending ? 'Booting…' : 'In-memory'}
            </button>
          </HoverInfo>
          <HoverInfo
            side="bottom"
            content={
              <>
                <p>Real Postgres when you need joins, triggers, and plpgsql. ~5s to boot.</p>
                <p className="hi-foot">Checkpoint, seed, fork, and compare are in-memory only.</p>
              </>
            }
          >
            <button
              className="btn btn--ghost boot-btn"
              disabled={pending}
              onClick={() => onBoot('embedded')}
            >
              <Database size={13} /> {pending ? 'Booting…' : 'Postgres'}
            </button>
          </HoverInfo>
        </div>
        <div className="boot-opt">
          <label className="boot-strict">
            <input type="checkbox" checked={strict} onChange={(e) => onStrict(e.target.checked)} />
            <span>Deterministic</span>
          </label>
          <p className="boot-hint">
            Pins the clock for reproducible runs{hasActive ? '. Applies on next boot.' : '.'}
          </p>
          {strict && (
            <input
              className="input boot-seed mono"
              placeholder="seed (optional)"
              value={seed}
              onChange={(e) => onSeed(e.target.value.replace(/[^0-9-]/g, ''))}
            />
          )}
        </div>
        {error && (
          <div className="boot-error">
            {error.message}
          </div>
        )}
      </div>
    </section>
  )
}

// ─────────────────────────── sandboxes list ───────────────────────────

interface SandboxesListProps {
  list: SandboxListEntry[]
  activePub: string | null
  onSelect: (pubID: string) => void
}
function SandboxesList({ list, activePub, onSelect }: SandboxesListProps) {
  return (
    <section className="rail-section">
      <header className="rail-h">
        <span>Active sandboxes</span>
        <span className="rail-count">{list.length}</span>
      </header>
      {list.length === 0 ? (
        <div className="rail-empty">None yet. Boot one above to begin.</div>
      ) : (
        <ul className="sb-list">
          {list.map(s => {
            const isSim = s.backend === 'sim'
            return (
              <li key={s.pub_id} className={`sb-item ${s.pub_id === activePub ? 'is-active' : ''}`}>
                <button className="sb-item-body" onClick={() => onSelect(s.pub_id)}>
                  <div className="sb-item-row">
                    <span className={`sb-chip ${isSim ? 'chip-sim' : 'chip-pg'}`}>
                      {isSim ? 'in-memory' : 'postgres'}
                    </span>
                    <span className="mono sb-item-id">{s.pub_id.slice(0, 8)}</span>
                  </div>
                  <div className="sb-item-sub mono">
                    {s.schema_version ? `schema ${s.schema_version.slice(0, 7)}` : 'schema —'} · booted in {s.boot_ms} ms
                  </div>
                </button>
              </li>
            )
          })}
        </ul>
      )}
    </section>
  )
}

// ─────────────────────────── checkpoints (was Marks) ───────────────────────────

interface CheckpointsListProps {
  backend: string | undefined
  marks: Checkpoint[]
  capturePending: boolean
  restorePending: boolean
  hasActive: boolean
  onCapture: () => void
  onRestore: (id: string) => void
}
function CheckpointsList({ backend, marks, capturePending, restorePending, hasActive, onCapture, onRestore }: CheckpointsListProps) {
  // Checkpoints rely on the sim pool's CoW pointer capture — the
  // embedded backend has no equivalent (pg_dump is too slow for the
  // intended budget). Disable Capture + Restore for Postgres-backed
  // sandboxes and surface why so the user isn't left wondering.
  const isEmbedded = backend === 'embedded'
  // Capture and Restore both mutate sandbox state and share the marks
  // table — they must be mutually exclusive with each other AND with
  // any in-flight capture/restore.
  const busy = capturePending || restorePending
  const disabled = !hasActive || isEmbedded || busy

  return (
    <section className="rail-section">
      <header className="rail-h">
        <span>Checkpoints</span>
        <button
          className="rail-cta"
          disabled={disabled}
          onClick={onCapture}
          title={isEmbedded ? 'Checkpoints are in-memory only — boot a Sim sandbox' : 'Capture the current state'}
        >
          <Plus size={11} /> Capture
        </button>
      </header>
      <div className="rail-body rail-body-tight">
        {isEmbedded ? (
          <p className="rail-help">
            Checkpoints are <strong>in-memory only</strong>. Switch to a Sim sandbox to capture, restore, and compare states.
          </p>
        ) : (
          <p className="rail-help">
            Save the current state. Restore later with one click, or pick two checkpoints in the <strong>Compare</strong> tab to diff them.
          </p>
        )}
      </div>
      {!isEmbedded && marks.length === 0 ? (
        <div className="rail-empty">No checkpoints yet.</div>
      ) : isEmbedded ? null : (
        <ul className="ckpt-list">
          {marks.map(m => (
            <li key={m.id} className="ckpt-row">
              <button
                className="ckpt-restore"
                disabled={busy}
                onClick={() => onRestore(m.id)}
                title={busy ? 'Wait for the current capture/restore to finish' : 'Restore the sandbox to this checkpoint'}
              >
                <RotateCcw size={11} />
              </button>
              <div className="ckpt-body mono">
                <div className="ckpt-time">{formatRelative(m.at)}</div>
                <div className="ckpt-meta">captured in {m.capturedUs.toLocaleString()} µs</div>
              </div>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

function formatRelative(epochMs: number): string {
  const sec = Math.max(0, Math.floor((Date.now() - epochMs) / 1000))
  if (sec < 5) return 'just now'
  if (sec < 60) return `${sec}s ago`
  if (sec < 3600) return `${Math.floor(sec / 60)}m ago`
  return `${Math.floor(sec / 3600)}h ago`
}

// ─────────────────────────── empty workbench ───────────────────────────

function EmptyWorkbench() {
  return (
    <div className="wb-empty">
      <div className="wb-empty-title">No sandbox active</div>
      <p className="wb-empty-body">
        Boot one with <strong>+ In-memory</strong> or <strong>+ Postgres</strong> on the left to begin.
      </p>
      <dl className="wb-empty-tabs">
        <dt>Tables</dt>
        <dd>Browse the schema and sample rows.</dd>
        <dt>Seed data</dt>
        <dd>Populate tables with synthetic rows.</dd>
        <dt>SQL</dt>
        <dd>Run arbitrary queries against this sandbox.</dd>
        <dt>Compare</dt>
        <dd>Diff added, removed, and modified rows between two checkpoints.</dd>
        <dt>Forks</dt>
        <dd>Clone the sandbox into parallel copies, each with independent state.</dd>
      </dl>
    </div>
  )
}

// ─────────────────────────── hover info popover ───────────────────────────

// HoverInfo wraps an element with a hover popover. The open state is
// the parent's :hover/:focus-within (pure CSS), so there's no portal,
// no positioner library, no extra render on hover. Don't pass tall
// content — there's no viewport collision avoidance and a tall popover
// will clip behind the next rail section.
function HoverInfo({ children, content, side = 'bottom' }: {
  children: React.ReactNode
  content: React.ReactNode
  side?: 'top' | 'bottom' | 'right'
}) {
  return (
    <span className="hover-info">
      {children}
      <span className={`hover-info-pop hover-info-pop--${side}`} role="tooltip">
        {content}
      </span>
    </span>
  )
}

// SimOnlyPlaceholder renders inside an embedded sandbox's tab body
// when the underlying feature is sim-only. We keep the tab navigable
// so the user knows the surface exists, but the body explains the
// constraint instead of triggering a guaranteed 400 from the runtime.
function SimOnlyPlaceholder({ feature, what }: { feature: string, what: string }) {
  return (
    <div className="wb-tab-empty">
      <div className="wb-tab-empty-title">{feature} is in-memory only</div>
      <p className="wb-tab-empty-body">{what} Boot an in-memory sandbox to use this feature.</p>
    </div>
  )
}

// ─────────────────────────── SQL tab ───────────────────────────

const DEFAULT_SQL = `-- Pick an entity in the Tables tab to see its qualified name,
-- then adapt this query, e.g.:
--   SELECT * FROM "schema"."table" LIMIT 5
SELECT 1`

interface SqlTabProps {
  pubID: string
  onLatency: (us: number) => void
}
function SqlTab({ pubID, onLatency }: SqlTabProps) {
  const [sql, setSql] = useState<string>(DEFAULT_SQL)
  const [rows, setRows] = useState<Array<Record<string, unknown>> | null>(null)
  const [rowsAffected, setRowsAffected] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [running, setRunning] = useState(false)
  const [latencyUs, setLatencyUs] = useState<number | null>(null)

  // Monotonic request id — if a slower earlier call completes after a
  // faster newer one, the stale result is dropped. Otherwise rapid
  // Execute clicks (or a 10k query landing after a 1-row query) would
  // race and overwrite the freshest output.
  const reqRef = useRef(0)

  const run = async () => {
    setError(null)
    setRowsAffected(null)
    setRows(null)
    setRunning(true)
    const myReq = ++reqRef.current
    try {
      // Heuristic: anything with SELECT or RETURNING is a read; everything
      // else is an exec. Misclassifies SQL containing "SELECT" inside a
      // string literal — fine for an operator-facing surface.
      const upper = sql.toUpperCase()
      const isQuery = /\bSELECT\b/.test(upper) || /\bRETURNING\b/.test(upper)
      if (isQuery) {
        const resp = await api.sandbox.query(pubID, sql)
        if (myReq !== reqRef.current) return
        const us = resp.t_server_us ?? 0
        onLatency(us)
        setLatencyUs(us)
        setRows(resp.rows)
      } else {
        const resp = await api.sandbox.exec(pubID, sql)
        if (myReq !== reqRef.current) return
        const us = resp.t_server_us ?? 0
        onLatency(us)
        setLatencyUs(us)
        setRowsAffected(resp.rows_affected)
      }
    } catch (e) {
      if (myReq !== reqRef.current) return
      setError((e as Error).message)
    } finally {
      if (myReq === reqRef.current) setRunning(false)
    }
  }

  return (
    <div className="sql-tab">
      <div className="sql-editor">
        <textarea
          className="sql-editor-area mono"
          rows={12}
          value={sql}
          onChange={e => setSql(e.target.value)}
        />
        <div className="sql-editor-bar">
          <span className="sql-editor-hint">
            SELECT or RETURNING → rows. Anything else → rows-affected.
          </span>
          <button className="btn" disabled={running} onClick={run}>
            {running ? 'Running…' : 'Execute'}
          </button>
        </div>
      </div>

      {error && <div className="banner banner--error">{error}</div>}
      {rowsAffected !== null && (
        <div className="banner banner--ok">
          {rowsAffected} row(s) affected
          {latencyUs !== null && <span className="mono banner-meta"> · {latencyUs} µs</span>}
        </div>
      )}
      {rows && (
        <>
          <div className="rows-ribbon mono">
            {rows.length} row(s)
            {latencyUs !== null && <> · {latencyUs} µs</>}
          </div>
          <RowsTable rows={rows} empty="(no rows)" />
        </>
      )}
      {sql.trim() && (
        <details className="sql-preview">
          <summary>Parsed</summary>
          <Sql>{sql}</Sql>
        </details>
      )}
    </div>
  )
}

// ─────────────────────────── Tables tab (was Inspect) ───────────────────────────

interface TablesTabProps {
  pubID: string
  backend: string | undefined
  initialFocus?: string
}
function TablesTab({ pubID, backend, initialFocus }: TablesTabProps) {
  const [qualified, setQualified] = useState<string>(focusToQualified(initialFocus))
  const isEmbedded = backend === 'embedded'
  const catalogQ = useQuery({ ...queries.sandboxCatalog(pubID), enabled: !isEmbedded })
  const entities = catalogQ.data?.entities ?? []

  // First entity selection: when the catalog loads and the user hasn't
  // picked anything yet (or the prior pick isn't in this sandbox's
  // catalog), default to the first available entity.
  useEffect(() => {
    if (entities.length === 0) return
    if (!qualified || !entities.includes(qualified)) {
      setQualified(entities[0])
    }
  }, [entities, qualified])

  const describe = useQuery({
    ...queries.sandboxDescribe(pubID, qualified),
    retry: false,
    enabled: !isEmbedded && qualified.length > 0 && entities.includes(qualified),
  })
  const [sample, setSample] = useState<Array<Record<string, unknown>> | null>(null)
  const [loadingSample, setLoadingSample] = useState(false)
  const [sampleErr, setSampleErr] = useState<string | null>(null)
  const reqRef = useRef(0)

  // Clear stale sample + sample error whenever the user edits the
  // qualified name. Without this, a typo's error banner persists even
  // after the user types a valid name.
  useEffect(() => {
    setSampleErr(null)
    setSample(null)
  }, [qualified])

  const fetchSample = async () => {
    if (!qualified) return
    setLoadingSample(true)
    setSampleErr(null)
    const myReq = ++reqRef.current
    try {
      const resp = await api.sandbox.sample(pubID, qualified, 10)
      if (myReq !== reqRef.current) return
      setSample(resp.rows)
    } catch (e) {
      if (myReq !== reqRef.current) return
      setSample(null)
      setSampleErr((e as Error).message)
    } finally {
      if (myReq === reqRef.current) setLoadingSample(false)
    }
  }

  if (isEmbedded) {
    return (
      <SimOnlyPlaceholder
        feature="Tables"
        what="Describe and Sample read directly from the sim catalog — what columns each entity has, plus a quick row preview."
      />
    )
  }

  return (
    <div className="tab-stack">
      <div className="row tab-row">
        <select
          className="input mono"
          value={qualified}
          onChange={e => setQualified(e.target.value)}
          disabled={catalogQ.isLoading || entities.length === 0}
        >
          {catalogQ.isLoading && <option>Loading…</option>}
          {!catalogQ.isLoading && entities.length === 0 && (
            <option>No entities registered</option>
          )}
          {entities.map(name => (
            <option key={name} value={name}>{name}</option>
          ))}
        </select>
        <button className="btn" disabled={!qualified || loadingSample} onClick={fetchSample}>
          {loadingSample ? 'Loading…' : 'Sample 10 rows'}
        </button>
      </div>

      {describe.data && (
        <div className="card">
          <header className="card__head">
            <strong className="mono">{describe.data.qualified}</strong>
            <span className="card__sub mono">{describe.data.row_count.toLocaleString()} rows</span>
          </header>
          <div className="card__body" style={{ padding: 0 }}>
            <table className="rows-table mono">
              <thead>
                <tr>
                  <th>Column</th>
                  <th>Type</th>
                  <th>Nullable</th>
                </tr>
              </thead>
              <tbody>
                {describe.data.columns.map(c => (
                  <tr key={c.name}>
                    <td>{c.name}</td>
                    <td>{c.kind}</td>
                    <td>{c.nullable ? 'yes' : 'no'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
      {describe.error && qualified && (
        <div className="banner banner--error">{(describe.error as Error).message}</div>
      )}

      {sample && (
        <div className="card">
          <header className="card__head">
            <strong>Sample</strong>
            <span className="card__sub mono">{sample.length} rows</span>
          </header>
          <div className="card__body" style={{ padding: 0 }}>
            <RowsTable rows={sample} empty="(no rows yet — switch to Seed data to populate)" />
          </div>
        </div>
      )}
      {sampleErr && <div className="banner banner--error">{sampleErr}</div>}
    </div>
  )
}

// ─────────────────────────── Seed tab (was Fixtures) ───────────────────────────

interface SeedTabProps {
  pubID: string
  backend: string | undefined
  onInsert: (n: number, us: number) => void
}
function SeedTab({ pubID, backend, onInsert }: SeedTabProps) {
  const [qualified, setQualified] = useState<string>('')
  const [running, setRunning] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const reqRef = useRef(0)
  const isEmbedded = backend === 'embedded'
  const catalogQ = useQuery({ ...queries.sandboxCatalog(pubID), enabled: !isEmbedded })
  const entities = catalogQ.data?.entities ?? []

  useEffect(() => {
    if (entities.length === 0) return
    if (!qualified || !entities.includes(qualified)) {
      setQualified(entities[0])
    }
  }, [entities, qualified])

  const bulk = async (n: number) => {
    if (!qualified) return
    setErr(null)
    setRunning(true)
    const myReq = ++reqRef.current
    try {
      const resp = await api.sandbox.bulk(pubID, { qualified, n })
      if (myReq !== reqRef.current) return
      onInsert(resp.inserted, resp.t_server_us ?? 0)
    } catch (e) {
      if (myReq !== reqRef.current) return
      setErr((e as Error).message)
    } finally {
      if (myReq === reqRef.current) setRunning(false)
    }
  }

  if (isEmbedded) {
    return (
      <SimOnlyPlaceholder
        feature="Seed data"
        what="Synthetic row generation reads each column's declared type from the sim catalog to produce realistic values (email-shaped strings, monotonic timestamps, unit-norm vectors)."
      />
    )
  }

  return (
    <div className="tab-stack">
      <div className="row tab-row">
        <select
          className="input mono"
          value={qualified}
          onChange={e => setQualified(e.target.value)}
          disabled={catalogQ.isLoading || entities.length === 0}
        >
          {catalogQ.isLoading && <option>Loading…</option>}
          {!catalogQ.isLoading && entities.length === 0 && (
            <option>No entities registered</option>
          )}
          {entities.map(name => (
            <option key={name} value={name}>{name}</option>
          ))}
        </select>
      </div>

      <div className="seed-grid">
        <button className="btn btn--ghost seed-btn" disabled={!qualified || running} onClick={() => bulk(100)}>
          + 100 rows
        </button>
        <button className="btn btn--ghost seed-btn" disabled={!qualified || running} onClick={() => bulk(1000)}>
          + 1,000 rows
        </button>
        <button className="btn seed-btn" disabled={!qualified || running} onClick={() => bulk(10000)}>
          + 10,000 rows
        </button>
      </div>

      {err && <div className="banner banner--error">{err}</div>}
      <p className="tab-foot">
        Values are synthesized from each column's declared type — emails, timestamps, unit-norm vectors. Reproducible when Deterministic is on at boot.
      </p>
    </div>
  )
}

// ─────────────────────────── Compare tab (was Diff) ───────────────────────────

interface CompareTabProps {
  pubID: string
  backend: string | undefined
  marks: Checkpoint[]
}
function CompareTab({ pubID, backend, marks }: CompareTabProps) {
  // Chronological order in the dropdowns so "earlier" and "later" read
  // naturally. The checkpoints state is newest-first, so reverse here.
  const ordered = useMemo(() => [...marks].reverse(), [marks])
  const [earlier, setEarlier] = useState<string>('')
  const [later, setLater] = useState<string>('')
  // Track whether the user has manually picked a "later" mark. When
  // they haven't, auto-advance "later" to the newest checkpoint on
  // each new capture; when they have, leave their pick alone.
  const userTouched = useRef<{ earlier: boolean, later: boolean }>({ earlier: false, later: false })

  useEffect(() => {
    if (ordered.length === 0) return
    // Earlier: keep current pick if still valid; otherwise default to oldest.
    if (!ordered.some(m => m.id === earlier)) {
      setEarlier(ordered[0].id)
      userTouched.current.earlier = false
    }
    // Later: pin to newest UNLESS the user has manually picked something else
    // (and that pick still exists).
    const newest = ordered[ordered.length - 1].id
    if (!ordered.some(m => m.id === later)) {
      setLater(newest)
      userTouched.current.later = false
    } else if (!userTouched.current.later) {
      // User hasn't touched it — keep it pinned to the newest as new
      // checkpoints arrive.
      if (later !== newest) setLater(newest)
    }
  }, [ordered, earlier, later])

  const [result, setResult] = useState<Record<string, SandboxTableDiff> | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [running, setRunning] = useState(false)
  const [latencyUs, setLatencyUs] = useState<number | null>(null)
  const reqRef = useRef(0)

  const compare = async () => {
    setErr(null)
    setRunning(true)
    const myReq = ++reqRef.current
    try {
      const resp = await api.sandbox.diff(pubID, earlier, later)
      if (myReq !== reqRef.current) return
      setResult(resp.tables ?? {})
      setLatencyUs(resp.t_server_us ?? 0)
    } catch (e) {
      if (myReq !== reqRef.current) return
      setErr((e as Error).message)
    } finally {
      if (myReq === reqRef.current) setRunning(false)
    }
  }

  if (backend === 'embedded') {
    return (
      <SimOnlyPlaceholder
        feature="Compare"
        what="Comparing two checkpoints reads the sim's CoW row maps to compute added / removed / modified per table."
      />
    )
  }

  const tables = useMemo(() => result ? Object.keys(result).sort() : [], [result])

  if (marks.length < 2) {
    return (
      <div className="wb-tab-empty">
        <div className="wb-tab-empty-title">Need two checkpoints</div>
        <p className="wb-tab-empty-body">
          Capture two states in the left rail, then pick any two here to diff them. You currently have {marks.length} checkpoint{marks.length === 1 ? '' : 's'}.
        </p>
      </div>
    )
  }

  return (
    <div className="tab-stack">
      <div className="card">
        <div className="card__body cmp-controls">
          <label className="cmp-row">
            <span className="cmp-lab">Earlier</span>
            <select
              className="input mono"
              value={earlier}
              onChange={e => { userTouched.current.earlier = true; setEarlier(e.target.value) }}
            >
              {ordered.map(m => (
                <option key={m.id} value={m.id}>{formatRelative(m.at)} (captured in {m.capturedUs} µs)</option>
              ))}
            </select>
          </label>
          <label className="cmp-row">
            <span className="cmp-lab">Later</span>
            <select
              className="input mono"
              value={later}
              onChange={e => { userTouched.current.later = true; setLater(e.target.value) }}
            >
              {ordered.map(m => (
                <option key={m.id} value={m.id}>{formatRelative(m.at)} (captured in {m.capturedUs} µs)</option>
              ))}
            </select>
          </label>
          <div className="cmp-actions">
            <button className="btn" disabled={running || !earlier || !later || earlier === later} onClick={compare}>
              <GitCompare size={13} /> {running ? 'Comparing…' : 'Compare'}
            </button>
            {latencyUs !== null && (
              <span className="mono cmp-latency">computed in {latencyUs} µs</span>
            )}
          </div>
          {earlier && later && earlier === later && (
            <p className="cmp-warn">Pick two different checkpoints.</p>
          )}
          {err && <div className="banner banner--error">{err}</div>}
        </div>
      </div>

      {result && tables.length === 0 && (
        <div className="banner banner--ok">No differences between the two checkpoints.</div>
      )}

      {result && tables.length > 0 && (
        <div className="card">
          <header className="card__head">
            <strong>Changes by table</strong>
            <span className="card__sub mono">{tables.length} table(s)</span>
          </header>
          <div className="card__body" style={{ padding: 0 }}>
            <table className="rows-table mono cmp-table">
              <thead>
                <tr>
                  <th>Table</th>
                  <th>Added</th>
                  <th>Removed</th>
                  <th>Modified</th>
                </tr>
              </thead>
              <tbody>
                {tables.map(t => {
                  const td = result[t]
                  return (
                    <tr key={t}>
                      <td>{t}</td>
                      <td className="cmp-add">{td.added ? `+${td.added}` : '0'}</td>
                      <td className="cmp-rem">{td.removed ? `−${td.removed}` : '0'}</td>
                      <td className="cmp-mod">{td.modified ? `~${td.modified}` : '0'}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  )
}

// ─────────────────────────── Forks tab ───────────────────────────

interface ForksTabProps {
  pubID: string
  active: SandboxListEntry | null
  sandboxes: SandboxListEntry[]
  onSelect: (pubID: string) => void
  onForked: (us: number) => void
}
function ForksTab({ pubID, active, sandboxes, onSelect, onForked }: ForksTabProps) {
  const [n, setN] = useState<number>(2)
  const [running, setRunning] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [latencyUs, setLatencyUs] = useState<number | null>(null)
  const [childIDs, setChildIDs] = useState<string[]>([])
  const reqRef = useRef(0)

  // Only show "embedded blocks Fork" when we KNOW the backend is
  // embedded. While the list query is mid-refetch active is null —
  // don't claim that means Postgres.
  const isEmbedded = active?.backend === 'embedded'

  const fork = async () => {
    setErr(null)
    setRunning(true)
    const myReq = ++reqRef.current
    try {
      const resp = await api.sandbox.fork(pubID, n)
      if (myReq !== reqRef.current) return
      // Append rather than replace so repeated forks show all the
      // children that have been spawned this session, not just the
      // last batch.
      setChildIDs(prev => [...prev, ...resp.ids])
      const us = resp.t_server_us ?? 0
      setLatencyUs(us)
      onForked(us)
    } catch (e) {
      if (myReq !== reqRef.current) return
      setErr((e as Error).message)
    } finally {
      if (myReq === reqRef.current) setRunning(false)
    }
  }

  const parent = sandboxes.find(s => s.pub_id === pubID)
  const children = sandboxes.filter(s => childIDs.includes(s.pub_id))

  return (
    <div className="tab-stack">
      <div className="card">
        <div className="card__body fk-controls">
          <label className="fk-row">
            <span className="fk-lab">N</span>
            <input
              type="number"
              className="input mono fk-n"
              min={1}
              max={10}
              value={n}
              onChange={e => setN(Math.max(1, Math.min(10, Number(e.target.value) || 1)))}
            />
            <button className="btn" disabled={running || isEmbedded} onClick={fork} title={isEmbedded ? 'Forks are in-memory only' : 'Fork the active sandbox'}>
              <GitFork size={13} /> {running ? 'Forking…' : `Fork into ${n}`}
            </button>
            {latencyUs !== null && (
              <span className="mono fk-latency">forked in {latencyUs} µs</span>
            )}
          </label>
          {isEmbedded && (
            <p className="fk-warn">The active sandbox is Postgres-backed. Fork only works on in-memory sandboxes.</p>
          )}
          {err && <div className="banner banner--error">{err}</div>}
          <p className="tab-foot">
            Each fork has independent state, checkpoints, and destruction lifecycle. Useful for running N parallel strategies and comparing results.
          </p>
        </div>
      </div>

      {(parent || children.length > 0) && (
        <div className="forks-tree">
          {parent && (
            <div className="fork-node fork-parent">
              <div className="fork-node-label mono">{parent.pub_id.slice(0, 12)}</div>
              <span className="fork-node-tag">parent</span>
            </div>
          )}
          {children.length > 0 && <div className="fork-branches" />}
          <div className="fork-children-row">
            {children.map(c => (
              <button
                key={c.pub_id}
                className="fork-node fork-child"
                onClick={() => onSelect(c.pub_id)}
                title="Switch to this fork"
              >
                <div className="fork-node-label mono">{c.pub_id.slice(0, 12)}</div>
                <span className="fork-node-tag">child · {c.backend}</span>
              </button>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}

// ─────────────────────────── helpers ───────────────────────────

// focusToQualified accepts "ns.Entity" and renders the
// atlantis.<ns>_<snake(Entity)> form the sandbox catalog keys by. Used
// to pre-fill the Tables tab from the Schema page's deep link.
function focusToQualified(focus?: string): string {
  if (!focus) return ''
  const dot = focus.indexOf('.')
  if (dot < 0) return ''
  const ns = focus.slice(0, dot)
  const ent = focus.slice(dot + 1)
  const snake = ent.replace(/([A-Z])/g, (m, _c, i) => (i > 0 ? '_' : '') + m.toLowerCase())
  return `atlantis.${ns}_${snake}`
}
