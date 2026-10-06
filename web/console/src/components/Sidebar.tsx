import { useEffect, useRef, useState } from 'react'
import { Link, useRouterState } from '@tanstack/react-router'
import {
  Archive,
  ArrowSquareOut,
  CaretUpDown,
  Check,
  ClockCounterClockwise,
  Cpu,
  Cube,
  Gear,
  GearSix,
  Pulse,
  ShieldChevron,
  SignOut,
  Stack,
  Users,
} from '@phosphor-icons/react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import { events } from '@/lib/analytics'
import { useMe } from '@/hooks/useAuth'
import { switcherMode } from '@/lib/session'
import { markSignInRedirect } from '@/lib/signin'

const NAV = [
  { to: '/schema',     icon: Stack,   tip: 'Schema' },
  { to: '/history',    icon: ClockCounterClockwise,  tip: 'History' },
  { to: '/sandbox',    icon: Cube,      tip: 'Sandbox' },
  { to: '/workers',    icon: Cpu,      tip: 'Workers' },
  { to: '/health',     icon: Pulse, tip: 'Health' },
  { to: '/callers',    icon: Users,    tip: 'Callers' },
  { to: '/approvals', icon: ShieldChevron, tip: 'Approvals' },
  { to: '/parked',     icon: Archive,  tip: 'Parked' },
  { to: '/operations', icon: GearSix,      tip: 'Operations' },
] as const

// Supabase-style hover-to-expand sidebar. Collapsed (icon-only, 56px) by
// default, expands to 220px while the cursor is over the rail. All state
// lives in CSS via `.app:has(.rail:hover)` — no toggle, no localStorage,
// no React state. Labels stay in the DOM so screen readers and tab order
// still pick them up; they're opacity-faded behind a CSS transition.
export function Sidebar() {
  const routerState = useRouterState()
  const path = routerState.location.pathname
  const qc = useQueryClient()
  const { data: me } = useMe()

  // Sign out of the console, then hand the browser to Cloud to end the session
  // there. Clearing the cookie here ends only this console: Cloud is a separate
  // origin, and its session is what mints the next assertion.
  //
  // onSettled, so a failed delete still reaches Cloud. Stopping on the error
  // leaves the browser signed in on both with nothing said.
  //
  // markSignInRedirect before leaving: a Cloud that cannot be reached drops the
  // browser back here with no session, and the mark is what makes /login render
  // its card in place of bouncing to Cloud again.
  const logoutMutation = useMutation({
    mutationFn: api.auth.logout,
    onSettled: () => {
      qc.clear()
      // Beside qc.clear(), and for the same reason. Whoever uses this browser
      // next is somebody else, and the id this person was reported under must
      // not follow them into the next session.
      events.reset()
      markSignInRedirect()
      window.location.href = me?.cloud_signout_url ?? '/login'
    },
  })

  const isActive = (to: string) => path.startsWith(to)
  const initials = avatarInitials(me?.name, me?.email)
  const displayName = displayNameFor(me)

  return (
    <nav className="rail">
      <div className="rail__head">
        <span className="rail__logo" aria-hidden>
          <svg width="26" height="26" viewBox="0 0 26 26" fill="none">
            <circle cx="13" cy="13" r="10"  stroke="var(--line-strong)" strokeWidth="1.3" />
            <circle cx="13" cy="13" r="5.5" stroke="var(--ink-2)"        strokeWidth="1.1" />
            <circle cx="13" cy="13" r="1.9" fill="var(--accent)" />
          </svg>
        </span>
        <span className="rail__brand">Atlantis</span>
      </div>

      <OrgSwitcher />

      <div className="rail__group">
        {NAV.map(({ to, icon: Icon, tip }) => (
          <Link
            key={to}
            to={to}
            className={`rail-btn ${isActive(to) ? 'is-active' : ''}`}
            aria-label={tip}
          >
            <Icon />
            <span className="rail-btn__label">{tip}</span>
          </Link>
        ))}
      </div>

      <div className="rail__spacer" />

      <div className="rail-role" title={me ? `${me.email} · ${me.role}` : 'signed-out'}>
        <span className="rail-role__badge">{initials}</span>
        <span className="rail-role__email">{displayName}</span>
      </div>

      <div className="rail__group">
        <Link
          to="/settings"
          className={`rail-btn ${isActive('/settings') ? 'is-active' : ''}`}
          aria-label="Settings"
        >
          <Gear />
          <span className="rail-btn__label">Settings</span>
        </Link>
        {/* Sits outside the organisation switcher, which renders as inert text
            for an account in one organisation and so would hide this from the
            accounts with the least reason to stay. */}
        {me?.cloud_url && (
          <a
            className="rail-btn"
            href={me.cloud_url}
            aria-label="Back to Atlantis Cloud"
          >
            <ArrowSquareOut />
            <span className="rail-btn__label">Atlantis Cloud</span>
          </a>
        )}
        <button
          type="button"
          className="rail-btn rail-btn--danger"
          aria-label="Sign out"
          onClick={() => logoutMutation.mutate()}
          disabled={logoutMutation.isPending}
        >
          <SignOut />
          <span className="rail-btn__label">Sign out</span>
        </button>
      </div>
    </nav>
  )
}

// OrgSwitcher shows which organisation this console is serving, and offers the
// others when there are others.
//
// Every entry is a plain link to Cloud. The console does not know — and must
// not decide — where a switch lands: `/authorize?org=<name>` re-reads the
// membership, mints for that organisation, and redirects to *its* registered
// console, which may be this deployment or an entirely different one. So a
// switch is a full-page navigation and never a fetch. `me.orgs` carries a
// server-built URL per organisation for the same reason `step_up_endpoint` does:
// nothing here assembles a Cloud URL out of a name it happens to hold.
//
// With one organisation this renders a label and no control. A menu that
// always opens onto a single choice teaches people that it does nothing, and
// they stop looking at it on the day it has two.
function OrgSwitcher() {
  const { data: me } = useMe()
  const [open, setOpen] = useState(false)
  const wrapRef = useRef<HTMLDivElement>(null)

  // Dismiss on an outside press or Escape. `mousedown` rather than `click` so
  // the menu is gone before whatever was underneath it reacts, and Escape
  // because a popover you can only close by aiming at it is a trap for anyone
  // driving from the keyboard.
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => {
      if (!wrapRef.current?.contains(e.target as Node)) setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  const mode = switcherMode(me)
  if (mode === 'none' || !me) return null

  const targets = me.orgs ?? []
  if (mode === 'label') {
    return (
      <div className="rail-org" title={me.org}>
        <div className="rail-org__face" data-testid="org-label">
          <OrgFace org={me.org} display={me.org_display_name} />
        </div>
      </div>
    )
  }

  return (
    <div className="rail-org" ref={wrapRef}>
      <button
        type="button"
        className={`rail-org__face rail-org__face--btn ${open ? 'is-open' : ''}`}
        // aria-haspopup="true" and not "menu". role="menu" is a promise of
        // arrow-key navigation and typeahead; this is a short list of links,
        // which the browser already moves between with Tab. A role that
        // announces a keyboard model the component does not implement is worse
        // than no role.
        aria-haspopup="true"
        aria-expanded={open}
        aria-label={`Organisation: ${me.org}. Switch organisation`}
        title={me.org}
        data-testid="org-switcher"
        onClick={() => setOpen(v => !v)}
      >
          <OrgFace org={me.org} display={me.org_display_name} />
        <CaretUpDown className="rail-org__chev" />
      </button>

      <div
        className={`screens-pop screens-pop--org ${open ? 'is-open' : ''}`}
        aria-label="Organisations"
      >
        <div className="screens-pop__group">Organisations</div>
        {targets.map(o =>
          o.name === me.org ? (
            <div key={o.name} className="screens-item is-active" aria-current="true">
              <OrgMonogram label={o.display_name || o.name} />
              <span>{o.display_name || o.name}</span>
              <Check className="screens-item__tick" />
            </div>
          ) : (
            <a key={o.name} className="screens-item" href={o.url}>
              <OrgMonogram label={o.display_name || o.name} />
              <span>{o.display_name || o.name}</span>
            </a>
          ),
        )}
      </div>
    </div>
  )
}

// displayNameFor renders the user's preferred label in the sidebar.
//
// Cloud asserts one `name`, not a first/last pair, because a great many people
// do not have a name that splits into two that way. It is optional, so the
// email is the fallback and the slot stays informative either way.
function displayNameFor(me?: { name?: string; email?: string }): string {
  const n = (me?.name ?? '').trim()
  if (n) return n
  return me?.email ?? ''
}

// avatarInitials renders the two-letter badge. Priority:
//   1. Initials from the asserted name — first and last word when there are
//      several, otherwise its first two letters.
//   2. Two letters from the email local part, split on common separators.
//   3. "?" while auth is loading, so the badge slot keeps its size.
function avatarInitials(name?: string, email?: string): string {
  const words = (name ?? '').trim().split(/\s+/).filter(Boolean)
  if (words.length >= 2) {
    return (words[0].charAt(0) + words[words.length - 1].charAt(0)).toUpperCase()
  }
  if (words.length === 1) return words[0].slice(0, 2).toUpperCase()

  if (!email) return '?'
  const local = email.split('@')[0]
  const parts = local.split(/[._-]/).filter(Boolean)
  if (parts.length >= 2) {
    return (parts[0].charAt(0) + parts[1].charAt(0)).toUpperCase()
  }
  return local.slice(0, 2).toUpperCase()
}

// OrgFace is the monogram and name block in the rail, matching the shape the
// organisation rows in Cloud use.
//
// The display name arrives on the assertion's org_names claim, which carries
// only the organisations whose display name differs from their name — so this
// draws one line for an organisation that never set one, and two for one that
// did, without deciding anything itself.
function OrgMonogram({ label }: { label: string }) {
  return (
    <span className="rail-org__avatar" aria-hidden="true">{label.trim().charAt(0)}</span>
  )
}

function OrgFace({ org, display }: { org: string; display?: string }) {
  const label = display || org
  return (
    <>
      <OrgMonogram label={label} />
      <span className="rail-org__block">
        <span className="rail-org__name">{label}</span>
        {display && display !== org ? (
          <span className="rail-org__slug">{org}</span>
        ) : null}
      </span>
    </>
  )
}
