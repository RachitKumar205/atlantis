import { createRoute } from '@tanstack/react-router'
import { Route as RootRoute } from './__root'
import { Sandbox } from '@/pages/Sandbox'

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: '/sandbox',
  component: Sandbox,
})
