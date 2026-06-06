import { createRoute } from '@tanstack/react-router'
import { Route as RootRoute } from './__root'
import { Health } from '@/pages/Health'

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: '/health',
  component: Health,
})
