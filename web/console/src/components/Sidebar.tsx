import { Link, useRouterState } from '@tanstack/react-router'
import {
  Activity,
  Box,
  Cog,
  Cpu,
  History,
  Archive,
  Layers,
  LogOut,
  Settings,
  Users,
  ShieldQuestion,
} from 'lucide-react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import { useMe } from '@/hooks/useAuth'

const NAV = [
  { to: '/schema',     icon: Layers,   tip: 'Schema' },
  { to: '/history',    icon: History,  tip: 'History' },
  { to: '/sandbox',    icon: Box,      tip: 'Sandbox' },
  { to: '/workers',    icon: Cpu,      tip: 'Workers' },
  { to: '/health',     icon: Activity, tip: 'Health' },
  { to: '/callers',    icon: Users,    tip: 'Callers' },
  { to: '/approvals', icon: ShieldQuestion, tip: 'Approvals' },
  { to: '/parked',     icon: Archive,  tip: 'Parked' },
  { to: '/operations', icon: Cog,      tip: 'Operations' },
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

  const logoutMutation = useMutation({
    mutationFn: api.auth.logout,
    onSuccess: () => {
      qc.clear()
      window.location.href = '/login'
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
        <span className="rail__brand">atlantis</span>
      </div>

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
          <Settings />
          <span className="rail-btn__label">Settings</span>
        </Link>
        <button
          type="button"
          className="rail-btn"
          aria-label="Sign out"
          onClick={() => logoutMutation.mutate()}
          disabled={logoutMutation.isPending}
        >
          <LogOut />
          <span className="rail-btn__label">Sign out</span>
        </button>
      </div>
    </nav>
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
