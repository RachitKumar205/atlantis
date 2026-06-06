import { createRoute } from '@tanstack/react-router'
import { Route as RootRoute } from './__root'
import { Setup } from '@/pages/Setup'

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: '/setup',
  component: Setup,
})
