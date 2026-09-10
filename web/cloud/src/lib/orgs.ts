// What the organisations screen decides, as plain functions with tests.
//
// The same reasoning as lib/flow.ts records for the sign-in flow: these lived as
// inline expressions in the component first, where a wrong one is invisible and
// unreachable from a test. web/cloud has no jsdom and no React Testing Library —
// its vitest environment is `node` — so a decision left inside a component is a
// decision nothing can check.

/** The provisioning states the server reports. Empty means no queue row. */
export type OrgState =
  | 'pending'
  | 'provisioning'
  | 'ready'
  | 'failed'
  | 'deleted'
  | 'purging'
  | ''

export type Org = {
  name: string
  displayName: string
  role: string
  state: OrgState
  attempts: number
  /** Cloud's /authorize link. Empty until there is a console to reach. */
  url: string
  createdByMe: boolean
  /**
   * When a deleted organisation stops being restorable, RFC3339, or empty.
   *
   * Sent by the server rather than computed here. The window is a property of
   * the row — set when the delete happened, under whatever promise was made
   * then — so a client that added thirty days to "now" would show a date the
   * server does not agree with.
   */
  purgeAfter: string
}

export type Me = {
  userId: string
  email: string
  name: string
  orgs: Org[]
  orgLimit: number
  orgsCreated: number
}

/**
 * stateLabel is what somebody reads next to an organisation.
 *
 * Deliberately not the raw state. "provisioning" is a word this system uses
 * about itself; the person waiting wants to know whether to keep waiting.
 *
 * The empty state is the organisations registered by hand, which have no queue
 * row and never will. They work — they were simply never queued — so they are
 * described as ready rather than as an unknown.
 */
export function stateLabel(state: OrgState): string {
  switch (state) {
    case 'pending':
      return 'Queued'
    case 'provisioning':
      return 'Setting up'
    case 'ready':
    case '':
      return 'Ready'
    case 'failed':
      return 'Setup failed'
    case 'deleted':
      return 'Deleted'
    case 'purging':
      // Being torn down right now. Distinct from 'deleted' because there is no
      // longer anything to restore, and offering the button would be a lie.
      return 'Being destroyed'
    default:
      return 'Unknown'
  }
}

/**
 * isSettling reports whether this organisation is still on its way.
 *
 * What the screen polls on. `failed` is deliberately excluded even though the
 * provisioner will retry it: a screen that kept polling a failed organisation
 * would poll for as long as the tab stayed open, and the backoff means nothing
 * changes for minutes at a time.
 */
export function isSettling(state: OrgState): boolean {
  return state === 'pending' || state === 'provisioning'
}

/** Whether any organisation in the list is still coming up. */
export function anySettling(orgs: Org[]): boolean {
  return orgs.some(o => isSettling(o.state))
}

/**
 * canEnter reports whether clicking an organisation would go anywhere.
 *
 * Keyed on the link the server built, not on the state. The two can disagree
 * for a moment — registration writes the console row and the queue row
 * separately — and the link is the half that decides whether /authorize will
 * answer.
 */
export function canEnter(org: Org): boolean {
  return org.url !== '' && org.state !== 'deleted' && org.state !== 'purging'
}

/**
 * enterURL adds a console's return path to the /authorize link.
 *
 * The path arrives from the console that sent the browser here and travels
 * back untouched: Cloud never navigates to it, and the console reduces it to a
 * path on its own origin before use.
 *
 * An empty returnTo leaves the link as the server built it.
 */
export function enterURL(url: string, returnTo: string): string {
  if (url === '' || returnTo === '') return url
  const sep = url.includes('?') ? '&' : '?'
  return `${url}${sep}return_to=${encodeURIComponent(returnTo)}`
}

/**
 * canDelete reports whether to offer the delete control.
 *
 * Admin only, matching the server — store.SoftDeleteOrg refuses a viewer — and
 * only for an organisation that is actually serving. Deleting something
 * mid-provision is refused there too, so offering it here would produce a
 * button whose only outcome is an error.
 *
 * This is a hint, not a gate. The server checks the same two things, and it is
 * the one that decides.
 */
export function canDelete(org: Org): boolean {
  return org.role === 'admin' && (org.state === 'ready' || org.state === '')
}

/** canRestore reports whether a deleted organisation can still be brought back. */
export function canRestore(org: Org): boolean {
  return org.role === 'admin' && org.state === 'deleted'
}

/**
 * deletedMessage says how long is left, not when it happened.
 *
 * "Deleted on Tuesday" is a fact; "restorable until 3 April" is the one somebody
 * can act on. An unparseable or absent date degrades to the shorter sentence
 * rather than rendering "Invalid Date", which is what `new Date('')` produces.
 */
export function deletedMessage(org: Org): string {
  if (org.purgeAfter === '') return 'Deleted. It will be destroyed after a retention period.'
  const until = new Date(org.purgeAfter)
  if (Number.isNaN(until.getTime())) {
    return 'Deleted. It will be destroyed after a retention period.'
  }
  return `Deleted. Restorable until ${until.toLocaleDateString()}, then destroyed permanently.`
}

/** How many more organisations this account may create. */
export function remainingSlots(me: Me): number {
  const left = me.orgLimit - me.orgsCreated
  return left > 0 ? left : 0
}

/**
 * canCreate reports whether to offer the form at all.
 *
 * An account at its limit gets an explanation instead of a form that always
 * refuses. The count is of organisations this account *created*, not ones it
 * can act in — being added to somebody else's does not use a slot.
 */
export function canCreate(me: Me): boolean {
  return remainingSlots(me) > 0
}

/**
 * limitMessage explains a full account.
 *
 * Says the number rather than "you have reached your limit", because the next
 * question is always "which is what?".
 */
export function limitMessage(me: Me): string {
  const n = me.orgLimit
  return `This account can create ${n} organisation${n === 1 ? '' : 's'}, and has created ${me.orgsCreated}.`
}

/**
 * normaliseOrg converts one organisation from the wire.
 *
 * snake_case to camelCase happens here and nowhere else, matching how api.ts
 * treats every other response. Unknown states pass through rather than being
 * coerced: stateLabel answers "Unknown" for them, which is honest, where
 * defaulting to 'ready' would tell somebody an organisation works when nothing
 * said so.
 */
export function normaliseOrg(raw: Record<string, unknown>): Org {
  return {
    name: typeof raw.name === 'string' ? raw.name : '',
    displayName: typeof raw.display_name === 'string' ? raw.display_name : '',
    role: typeof raw.role === 'string' ? raw.role : '',
    state: (typeof raw.state === 'string' ? raw.state : '') as OrgState,
    attempts: typeof raw.attempts === 'number' ? raw.attempts : 0,
    url: typeof raw.url === 'string' ? raw.url : '',
    createdByMe: raw.created_by_me === true,
    purgeAfter: typeof raw.purge_after === 'string' ? raw.purge_after : '',
  }
}

/** normaliseMe converts the account payload from the wire. */
export function normaliseMe(raw: Record<string, unknown>): Me {
  const orgs = Array.isArray(raw.orgs) ? raw.orgs : []
  return {
    userId: typeof raw.user_id === 'string' ? raw.user_id : '',
    email: typeof raw.email === 'string' ? raw.email : '',
    name: typeof raw.name === 'string' ? raw.name : '',
    orgs: orgs.map(o => normaliseOrg(o as Record<string, unknown>)),
    orgLimit: typeof raw.org_limit === 'number' ? raw.org_limit : 0,
    orgsCreated: typeof raw.orgs_created === 'number' ? raw.orgs_created : 0,
  }
}
