import { createRoute } from '@tanstack/react-router'
import { Route as RootRoute } from './__root'
import { Workers } from '@/pages/Workers'

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: '/workers',
  component: Workers,
})
