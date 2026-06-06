import { Component, type ErrorInfo, type ReactNode } from 'react'
import {
  createRootRouteWithContext,
  Outlet,
  redirect,
  useRouterState,
} from '@tanstack/react-router'
import type { QueryClient } from '@tanstack/react-query'
import { Sidebar } from '@/components/Sidebar'
import { CommandBar, useCommandBar } from '@/components/CommandBar'
import { queries, ApiError } from '@/api/client'

// Last-resort error boundary. Without this, an uncaught throw blanks
// the whole console; with it, the user sees the actual message + stack
// and the cookie/session survives a reload. Per-page boundaries (e.g.
// Operations.PanelErrorBoundary) still catch panel-scoped failures
// first so a single broken page doesn't pull the chrome down with it.
class RootErrorBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state = { error: null as Error | null }
  static getDerivedStateFromError(error: Error) { return { error } }
  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('[RootErrorBoundary] caught:', error, info.componentStack)
  }
  render() {
    if (this.state.error) {
      return (
        <div style={{ position: 'fixed', inset: 0, background: '#0C0C0E', color: '#F5F5F7', padding: 32, fontFamily: 'monospace', fontSize: 13, lineHeight: 1.5, overflow: 'auto', zIndex: 9999 }}>
          <div style={{ color: '#D67878', fontWeight: 600, marginBottom: 12 }}>React app crashed (root boundary)</div>
          <div style={{ marginBottom: 12, whiteSpace: 'pre-wrap' }}>{this.state.error.message}</div>
          {this.state.error.stack && (
            <pre style={{ marginTop: 12, fontSize: 11, color: '#8E8E97' }}>{this.state.error.stack}</pre>
          )}
        </div>
      )
    }
    return this.props.children
  }
}

// Routes that bypass auth guard and shell chrome
const PUBLIC_PATHS = ['/login', '/setup']

interface RouterContext {
  queryClient: QueryClient
}

export const Route = createRootRouteWithContext<RouterContext>()({
  // beforeLoad runs on every route transition (including initial mount).
  // The full gate logic enumerates the state machine: (setup configured?
  // × authenticated? × path requested) → allow or redirect. Anything not
  // matching an "allow" branch redirects.
  beforeLoad: async ({ location, context }) => {
    const { queryClient } = context
    const path = location.pathname

    // 1. Setup status drives every decision — fetch first.
    let configured: boolean
    try {
      const setup = await queryClient.fetchQuery(queries.setupStatus())
      configured = setup.configured
    } catch {
      // Couldn't reach the BFF. Safest behaviour: keep the user on a
      // public surface rather than let a half-loaded console render.
      // /login is the universal fallback because its error surface is
      // simpler than the wizard's.
      if (path === '/login') return
      throw redirect({ to: '/login' })
    }

    // 2. Not configured → the wizard is the only legal destination.
    if (!configured) {
      if (path.startsWith('/setup')) return
      throw redirect({ to: '/setup' })
    }

    // 3. Configured → check whether this caller is authenticated.
    let authed = false
    try {
      await queryClient.fetchQuery(queries.me())
      authed = true
    } catch (err) {
      // 401 is the expected unauthenticated case; treat any other
      // error (network, 5xx, malformed) as unauthenticated too — the
      // login page is the only surface we trust to show errors safely.
      if (!(err instanceof ApiError) || err.status !== 401) {
        // No-op — drop into the unauthenticated branch below.
      }
      authed = false
    }

    // 4. Setup is done; the wizard no longer applies. Bounce off it.
    if (path.startsWith('/setup')) {
      throw redirect({ to: authed ? '/' : '/login' })
    }

    // 5. Authed users hitting /login → send home.
    if (path.startsWith('/login')) {
      if (authed) throw redirect({ to: '/' })
      return
    }

    // 6. Any other path requires auth.
    if (!authed) {
      throw redirect({ to: '/login' })
    }
  },
  component: RootLayout,
})

function RootLayout() {
  const routerState = useRouterState()
  const pathname = routerState.location.pathname
  const isPublic = PUBLIC_PATHS.some(p => pathname.startsWith(p))
  const { open, close } = useCommandBar()

  if (isPublic) {
    return (
      <RootErrorBoundary>
        <Outlet />
      </RootErrorBoundary>
    )
  }

  // Match the design's .app grid shell exactly: rail + main column. Each
  // page renders its own <PageShell> which provides the .page wrapper
  // and the .page__head + .page__body the design expects.
  return (
    <RootErrorBoundary>
      <div className="app">
        <Sidebar />
        <main className="main">
          <Outlet />
        </main>
        <CommandBar open={open} onClose={close} />
      </div>
    </RootErrorBoundary>
  )
}
