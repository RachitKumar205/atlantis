import { Link, useRouterState } from '@tanstack/react-router'
import {
  Activity,
  Box,
  Cog,
  History,
  Layers,
  LogOut,
  Settings,
  Users,
} from 'lucide-react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import { useMe } from '@/hooks/useAuth'

const NAV = [
  { to: '/schema',     icon: Layers,   tip: 'Schema' },
  { to: '/history',    icon: History,  tip: 'History' },
  { to: '/sandbox',    icon: Box,      tip: 'Sandbox' },
  { to: '/health',     icon: Activity, tip: 'Health' },
  { to: '/callers',    icon: Users,    tip: 'Callers' },
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
  const initials = avatarInitials(me?.first_name, me?.last_name, me?.email)
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
// Returns the full name when both first and last are set, the first
// name alone when that's all there is, and falls back to email so the
// slot stays informative for users who skipped the name fields.
function displayNameFor(me?: { first_name?: string; last_name?: string; email?: string }): string {
  const f = (me?.first_name ?? '').trim()
  const l = (me?.last_name ?? '').trim()
  if (f && l) return `${f} ${l}`
  if (f) return f
  return me?.email ?? ''
}

// avatarInitials renders the two-letter badge. Priority:
//   1. First initial + last initial when both names are present.
//   2. Two letters derived from the email local part (split on common
//      separators) when names aren't set — covers users created before
//      the name fields were added.
//   3. "?" while auth is loading so the badge slot keeps its size.
function avatarInitials(first?: string, last?: string, email?: string): string {
  const f = (first ?? '').trim()
  const l = (last ?? '').trim()
  if (f && l) return (f.charAt(0) + l.charAt(0)).toUpperCase()
  if (f) return f.slice(0, 2).toUpperCase()
  if (!email) return '?'
  const local = email.split('@')[0]
  const parts = local.split(/[._-]/).filter(Boolean)
  if (parts.length >= 2) {
    return (parts[0].charAt(0) + parts[1].charAt(0)).toUpperCase()
  }
  return local.slice(0, 2).toUpperCase()
}
