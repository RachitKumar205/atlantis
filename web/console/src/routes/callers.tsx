import { createRoute } from '@tanstack/react-router'
import { Route as RootRoute } from './__root'
import { Callers } from '@/pages/Callers'

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: '/callers',
  component: Callers,
})
