import type { ReactNode } from 'react'
import { MagnifyingGlass } from '@phosphor-icons/react'
import { openCommandBar } from './CommandBar'

interface PageShellProps {
  // ReactNode (not just string) so pages can stamp inline badges
  // alongside the title — see Sandbox.tsx using a SANDBOX wordmark.
  title: ReactNode
  sub?: string
  // path-style mono title for entity detail screens, like the design's
  // `.page__title.is-path` variant.
  pathTitle?: boolean
  // flush pages (.schema, .health) zero out body padding so 3-pane grids
  // and the log stream can run edge-to-edge.
  flush?: boolean
  // fill pages keep their padding but stop the body scrolling, so a child
  // sized to 100% takes the height that is left and scrolls its own panes.
  fill?: boolean
  // optional brass action button rendered on the right of the head
  // (e.g. "Add caller" / "Issue cert").
  action?: ReactNode
  children: ReactNode
}

// Page chrome wrapper: .page > .page__head (title + sub + ⌘K + optional
// action) + .page__body. flush=true zeroes the body padding for pages
// that own their full-bleed layout (Schema's 3-pane grid, Health's log
// stream).
export function PageShell({ title, sub, pathTitle, flush, fill, action, children }: PageShellProps) {
  return (
    <div className={`page ${flush ? 'page--flush' : ''}`}>
      <header className="page__head">
        <div className="page__headinner">
          <div>
            <div className={`page__title ${pathTitle ? 'is-path mono' : ''}`}>{title}</div>
            {sub && <div className="page__sub">{sub}</div>}
          </div>
          <div className="page__head-actions">
            <button
              className="btn btn--ghost"
              style={{ gap: 9 }}
              onClick={() => openCommandBar()}
              type="button"
            >
              <MagnifyingGlass size={14} />
              <span className="muted" style={{ fontSize: 12 }}>Search</span>
              <span className="kbd">⌘K</span>
            </button>
            {action}
          </div>
        </div>
      </header>
      <div className={`page__body ${fill ? 'page__body--fill' : ''}`}>{children}</div>
    </div>
  )
}
