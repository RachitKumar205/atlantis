import { Analytics } from '@atlantis/shared/analytics'

/**
 * What this application reports.
 *
 * Which pages an operator uses, and who they are — the Cloud user id and the
 * organisation, both of which the server already reports under, so a person is
 * one profile across the browser and the console's own audit events.
 *
 * Route patterns only. `/workers/$id` and `/imports/$id` carry identifiers a
 * customer chose in the segment the pattern replaces.
 */
export const events = new Analytics({ app: 'console' })

/**
 * routeOf is the pattern a set of matched routes reports as.
 *
 * The last match is the leaf, and its id is the declared path. An empty match
 * list reports the root.
 */
export function routeOf(matches: ReadonlyArray<{ routeId: string }>): string {
  if (matches.length === 0) return '/'
  return matches[matches.length - 1].routeId
}
