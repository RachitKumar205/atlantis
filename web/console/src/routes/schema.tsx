import { createRoute } from '@tanstack/react-router'
import { Route as RootRoute } from './__root'
import { Schema } from '@/pages/Schema'

// Optional keys, so a link may name only the parameters it cares about.
// Inferred, every key is required with an undefined-able value, and each one
// added here stops every existing navigate() and <Link> from type-checking.
export type SchemaSearch = {
  namespace?: string
  entity?: string
  blame?: boolean
}

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: '/schema',
  validateSearch: (search: Record<string, unknown>): SchemaSearch => ({
    namespace: typeof search.namespace === 'string' ? search.namespace : undefined,
    entity: typeof search.entity === 'string' ? search.entity : undefined,
    // Blame is opt-in, and the choice belongs in the URL with the selection:
    // a reader comparing two tables should not re-open it on each one.
    blame: search.blame === '1' || search.blame === true ? true : undefined,
  }),
  component: Schema,
})
