import { createRoute } from '@tanstack/react-router'
import { Route as RootRoute } from './__root'
import { Parked } from '@/pages/Parked'

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: '/parked',
  component: Parked,
})
