import { createRoute } from '@tanstack/react-router'
import { Route as RootRoute } from './__root'
import { Login } from '@/pages/Login'

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: '/login',
  component: Login,
})
