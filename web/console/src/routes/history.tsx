import { createRoute } from '@tanstack/react-router'
import { Route as RootRoute } from './__root'
import { History } from '@/pages/History'

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: '/history',
  component: History,
})
