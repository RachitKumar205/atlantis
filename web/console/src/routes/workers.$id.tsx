import { createRoute } from '@tanstack/react-router'
import { Route as RootRoute } from './__root'
import { WorkerSession } from '@/pages/WorkerSession'

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: '/workers/$id',
  component: WorkerSession,
})
