import { createRoute } from '@tanstack/react-router'
import { Route as RootRoute } from './__root'
import { ImportSchema } from '@/pages/ImportSchema'

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: '/import',
  component: ImportSchema,
})
