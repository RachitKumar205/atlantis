import { createRoute } from '@tanstack/react-router'
import { Route as RootRoute } from './__root'
import { Approvals } from '@/pages/Approvals'

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: '/approvals',
  component: Approvals,
})
