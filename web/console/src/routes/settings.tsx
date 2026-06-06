import { createRoute } from '@tanstack/react-router'
import { Route as RootRoute } from './__root'
import { Settings } from '@/pages/Settings'

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: '/settings',
  component: Settings,
})
