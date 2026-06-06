/* eslint-disable */
// Manually maintained route tree.
// All child routes use createRoute({ getParentRoute: () => RootRoute, path })
// to avoid the duplicate-__root__ error that createFileRoute triggers without
// the TanStack Router Vite plugin.

import { Route as RootRoute } from './routes/__root'
import { Route as IndexRoute } from './routes/index'
import { Route as SetupRoute } from './routes/setup'
import { Route as LoginRoute } from './routes/login'
import { Route as SchemaRoute } from './routes/schema'
import { Route as HistoryRoute } from './routes/history'
import { Route as HealthRoute } from './routes/health'
import { Route as SettingsRoute } from './routes/settings'
import { Route as CallersRoute } from './routes/callers'
import { Route as OperationsRoute } from './routes/operations'
import { Route as SandboxRoute } from './routes/sandbox'

export const routeTree = RootRoute.addChildren([
  IndexRoute,
  SetupRoute,
  LoginRoute,
  SchemaRoute,
  HistoryRoute,
  HealthRoute,
  SettingsRoute,
  CallersRoute,
  OperationsRoute,
  SandboxRoute,
])

// Module augmentation so typed Link / navigate work
declare module '@tanstack/react-router' {
  interface FileRoutesByPath {
    '/': {
      id: '/'
      path: '/'
      fullPath: '/'
      preLoaderRoute: typeof IndexRoute
      parentRoute: typeof RootRoute
    }
    '/setup': {
      id: '/setup'
      path: '/setup'
      fullPath: '/setup'
      preLoaderRoute: typeof SetupRoute
      parentRoute: typeof RootRoute
    }
    '/login': {
      id: '/login'
      path: '/login'
      fullPath: '/login'
      preLoaderRoute: typeof LoginRoute
      parentRoute: typeof RootRoute
    }
    '/schema': {
      id: '/schema'
      path: '/schema'
      fullPath: '/schema'
      preLoaderRoute: typeof SchemaRoute
      parentRoute: typeof RootRoute
    }
    '/history': {
      id: '/history'
      path: '/history'
      fullPath: '/history'
      preLoaderRoute: typeof HistoryRoute
      parentRoute: typeof RootRoute
    }
    '/health': {
      id: '/health'
      path: '/health'
      fullPath: '/health'
      preLoaderRoute: typeof HealthRoute
      parentRoute: typeof RootRoute
    }
    '/settings': {
      id: '/settings'
      path: '/settings'
      fullPath: '/settings'
      preLoaderRoute: typeof SettingsRoute
      parentRoute: typeof RootRoute
    }
    '/callers': {
      id: '/callers'
      path: '/callers'
      fullPath: '/callers'
      preLoaderRoute: typeof CallersRoute
      parentRoute: typeof RootRoute
    }
    '/operations': {
      id: '/operations'
      path: '/operations'
      fullPath: '/operations'
      preLoaderRoute: typeof OperationsRoute
      parentRoute: typeof RootRoute
    }
    '/sandbox': {
      id: '/sandbox'
      path: '/sandbox'
      fullPath: '/sandbox'
      preLoaderRoute: typeof SandboxRoute
      parentRoute: typeof RootRoute
    }
  }
}
