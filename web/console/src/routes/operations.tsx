import { createRoute } from '@tanstack/react-router'
import { Route as RootRoute } from './__root'
import { Operations } from '@/pages/Operations'

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: '/operations',
  component: Operations,
})
