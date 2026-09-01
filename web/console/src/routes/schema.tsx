import { createRoute } from '@tanstack/react-router'
import { Route as RootRoute } from './__root'
import { Schema } from '@/pages/Schema'

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: '/schema',
  validateSearch: (search: Record<string, unknown>) => ({
    namespace: typeof search.namespace === 'string' ? search.namespace : undefined,
    entity: typeof search.entity === 'string' ? search.entity : undefined,
    // Blame is opt-in, and the choice belongs in the URL with the selection:
    // a reader comparing two tables should not re-open it on each one.
    blame: search.blame === '1' || search.blame === true ? true : undefined,
  }),
  component: Schema,
})
