import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { KeyboardEvent as ReactKeyboardEvent } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { Activity, Box, Cog, History, Layers, Search, Settings, Users } from 'lucide-react'
import { queries, type MergedSchemaResponse } from '@/api/client'

// ── Item shape: group + icon + label + meta + run ──
type ItemGroup = 'Navigate' | 'Entities' | 'Versions' | 'Callers'

interface CmdItem {
  group: ItemGroup
  icon: React.ReactNode
  label: React.ReactNode
  meta?: string
  search: string
  run: () => void
}

// ── Entity extraction from merged schema (identical pattern to before) ──
interface EntityResult { entityId: string; name: string; namespace: string }

function extractEntities(schema: MergedSchemaResponse | undefined): EntityResult[] {
  if (!schema?.files) return []
  const out: EntityResult[] = []
  const seen = new Set<string>()
  for (const f of schema.files) {
    const re = /\bentity\s+(\w+)\s+in\s+([\w.]+)/g
    let m: RegExpExecArray | null
    while ((m = re.exec(f.content)) !== null) {
      const id = `${m[2]}.${m[1]}`
      if (!seen.has(id)) { seen.add(id); out.push({ entityId: id, name: m[1], namespace: m[2] }) }
    }
  }
  return out.sort((a, b) => a.entityId.localeCompare(b.entityId))
}

interface CommandBarProps {
  open: boolean
  onClose: () => void
}

// ⌘K modal: scrim + box + input row + grouped results + foot with
// keyboard hints. Result list uses .cmdk__group / .cmdk__item with
// .ic / .lbl / .meta slots; .is-sel marks the keyboard-selected row.
export function CommandBar({ open, onClose }: CommandBarProps) {
  const navigate = useNavigate()
  const [q, setQ] = useState('')
  const [sel, setSel] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)
  const resultsRef = useRef<HTMLDivElement>(null)

  const { data: schema } = useQuery({ ...queries.schemaMerged(), enabled: open })
  const { data: history } = useQuery({ ...queries.historyList({ limit: 12 }), enabled: open })
  const { data: callers } = useQuery({ ...queries.callers(), enabled: open })

  // Build the searchable index: nav routes + entities from the merged
  // schema + recent version IDs + registered caller CNs.
  const index = useMemo<CmdItem[]>(() => {
    const items: CmdItem[] = []

    const navs: { id: string; tip: string; icon: React.ReactNode }[] = [
      { id: '/schema',     tip: 'Schema',     icon: <Layers size={14} /> },
      { id: '/history',    tip: 'History',    icon: <History size={14} /> },
      { id: '/sandbox',    tip: 'Sandbox',    icon: <Box size={14} /> },
      { id: '/health',     tip: 'Health',     icon: <Activity size={14} /> },
      { id: '/callers',    tip: 'Callers',    icon: <Users size={14} /> },
      { id: '/operations', tip: 'Operations', icon: <Cog size={14} /> },
      { id: '/settings',   tip: 'Settings',   icon: <Settings size={14} /> },
    ]
    navs.forEach(n => items.push({
      group: 'Navigate', icon: n.icon, label: n.tip,
      meta: 'page', search: n.tip,
      run: () => navigate({ to: n.id }),
    }))

    extractEntities(schema).forEach(e => items.push({
      group: 'Entities',
      icon: <Layers size={14} />,
      label: (
        <span className="mono">
          <span className="faint">{e.namespace}.</span>{e.name}
        </span>
      ),
      meta: 'entity',
      search: `${e.namespace}.${e.name}`,
      run: () => navigate({ to: '/schema', search: { namespace: e.namespace, entity: e.entityId } }),
    }))

    history?.versions?.forEach(v => {
      const verStr = `v${String(v.version).padStart(4, '0')}`
      items.push({
        group: 'Versions',
        icon: <History size={14} />,
        label: <span className="mono">{verStr}</span>,
        meta: v.caller,
        search: `${verStr} ${v.caller}`,
        run: () => navigate({ to: '/history' }),
      })
    })

    callers?.callers?.forEach(c => items.push({
      group: 'Callers',
      icon: <Users size={14} />,
      label: <span className="mono">{c.caller}</span>,
      meta: c.schema_version ? `v${String(c.schema_version).padStart(4, '0')}` : '',
      search: c.caller,
      run: () => navigate({ to: '/callers' }),
    }))

    return items
  }, [schema, history, callers, navigate])

  // Filter (design: substring match on search OR label; cap 40)
  const filtered = useMemo(() => {
    const ql = q.trim().toLowerCase()
    const matches = ql
      ? index.filter(it => it.search.toLowerCase().includes(ql))
      : index.slice(0, 8)
    return matches.slice(0, 40)
  }, [index, q])

  // Group items in the same order the design does — preserve insertion order.
  const grouped = useMemo(() => {
    const groups: { name: ItemGroup; items: { it: CmdItem; i: number }[] }[] = []
    filtered.forEach((it, i) => {
      const last = groups[groups.length - 1]
      if (last && last.name === it.group) last.items.push({ it, i })
      else groups.push({ name: it.group, items: [{ it, i }] })
    })
    return groups
  }, [filtered])

  // Reset on open
  useEffect(() => {
    if (open) {
      setQ('')
      setSel(0)
      setTimeout(() => inputRef.current?.focus(), 30)
    }
  }, [open])

  // Reset selection when results shape changes
  useEffect(() => { setSel(0) }, [filtered.length])

  // Scroll the selected row into view on every selection change.
  useEffect(() => {
    const box = resultsRef.current
    if (!box) return
    const el = box.querySelector(`[data-i="${sel}"]`) as HTMLElement | null
    if (!el) return
    const r = el.getBoundingClientRect()
    const br = box.getBoundingClientRect()
    if (r.bottom > br.bottom) box.scrollTop += r.bottom - br.bottom
    if (r.top    < br.top)    box.scrollTop -= br.top - r.top
  }, [sel])

  const handleKeyDown = useCallback((e: ReactKeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Escape') { e.preventDefault(); onClose(); return }
    if (e.key === 'ArrowDown') { e.preventDefault(); setSel(i => Math.min(i + 1, filtered.length - 1)); return }
    if (e.key === 'ArrowUp')   { e.preventDefault(); setSel(i => Math.max(i - 1, 0)); return }
    if (e.key === 'Enter')     { e.preventDefault(); filtered[sel]?.run(); onClose(); return }
  }, [filtered, sel, onClose])

  return (
    <div className={`cmdk ${open ? 'is-open' : ''}`} aria-hidden={!open}>
      <div className="cmdk__scrim" onClick={onClose} />
      <div className="cmdk__box" role="dialog" aria-modal aria-label="Command palette">
        <div className="cmdk__input">
          <Search />
          <input
            ref={inputRef}
            type="text"
            placeholder="Jump to entity, version, or caller…"
            spellCheck={false}
            autoComplete="off"
            value={q}
            onChange={e => setQ(e.target.value)}
            onKeyDown={handleKeyDown}
          />
        </div>

        <div className="cmdk__results" ref={resultsRef}>
          {grouped.length === 0 ? (
            <div className="cmdk__group">No results</div>
          ) : (
            grouped.map(g => (
              <div key={g.name}>
                <div className="cmdk__group">{g.name}</div>
                {g.items.map(({ it, i }) => (
                  <div
                    key={i}
                    className={`cmdk__item ${i === sel ? 'is-sel' : ''}`}
                    data-i={i}
                    onMouseEnter={() => setSel(i)}
                    onClick={() => { it.run(); onClose() }}
                  >
                    <span className="ic">{it.icon}</span>
                    <span className="lbl">{it.label}</span>
                    {it.meta && <span className="meta">{it.meta}</span>}
                  </div>
                ))}
              </div>
            ))
          )}
        </div>

        <div className="cmdk__foot">
          <span className="k">
            <span className="kbd">↑</span>
            <span className="kbd">↓</span>
            navigate
          </span>
          <span className="k">
            <span className="kbd">↵</span>
            open
          </span>
          <span className="k">
            <span className="kbd">esc</span>
            close
          </span>
          <span className="spacer" style={{ flex: 1 }} />
          <span className="k muted">atlantis · console</span>
        </div>
      </div>
    </div>
  )
}

// ── Global ⌘K hook (unchanged API for callers) ────────────────────────────
// Module-level pointer to the live setOpen so child components anywhere in
// the tree can call openCommandBar() without prop-drilling or Context.
let externalSetOpen: ((next: boolean) => void) | null = null
export function openCommandBar() { externalSetOpen?.(true) }

export function useCommandBar() {
  const [open, setOpen] = useState(false)

  useEffect(() => {
    externalSetOpen = setOpen
    const handler = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setOpen(v => !v)
      }
    }
    window.addEventListener('keydown', handler)
    return () => {
      externalSetOpen = null
      window.removeEventListener('keydown', handler)
    }
  }, [])

  return { open, setOpen, close: () => setOpen(false) }
}
