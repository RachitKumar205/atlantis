import { describe, expect, it } from 'vitest'

import {
  anySettling,
  canCreate,
  canEnter,
  isSettling,
  canDelete,
  canRestore,
  deletedMessage,
  enterURL,
  limitMessage,
  normaliseMe,
  normaliseOrg,
  remainingSlots,
  stateLabel,
  type Me,
  type Org,
} from './orgs'

function org(over: Partial<Org> = {}): Org {
  return {
    name: 'acme',
    displayName: '',
    role: 'admin',
    state: 'ready',
    attempts: 0,
    url: 'https://cloud.test/authorize?org=acme',
    createdByMe: true,
    purgeAfter: '',
    ...over,
  }
}

function me(over: Partial<Me> = {}): Me {
  return { email: 'a@example.com', name: '', orgs: [], orgLimit: 3, orgsCreated: 0, ...over }
}

describe('stateLabel', () => {
  it('describes what somebody should do, not what the system calls itself', () => {
    expect(stateLabel('pending')).toBe('Queued')
    expect(stateLabel('provisioning')).toBe('Setting up')
    expect(stateLabel('ready')).toBe('Ready')
    expect(stateLabel('failed')).toBe('Setup failed')
  })

  // The organisations registered by hand have no queue row and never will. They
  // work; they were simply never queued. Reporting them as unknown would tell
  // their members something is wrong with an organisation that is fine.
  it('treats an organisation with no queue row as ready', () => {
    expect(stateLabel('')).toBe('Ready')
  })

  it('does not guess at a state it does not know', () => {
    expect(stateLabel('deleting' as never)).toBe('Unknown')
  })
})

describe('isSettling', () => {
  it('is true only while something is still coming', () => {
    expect(isSettling('pending')).toBe(true)
    expect(isSettling('provisioning')).toBe(true)
    expect(isSettling('ready')).toBe(false)
    expect(isSettling('')).toBe(false)
  })

  // A failed organisation is retried by the provisioner, on a backoff measured
  // in minutes. Polling it would mean polling for as long as the tab is open
  // and seeing nothing change.
  it('is false for a failure, which the server retries on its own schedule', () => {
    expect(isSettling('failed')).toBe(false)
  })

  it('polls a list while any single organisation is still coming', () => {
    expect(anySettling([org({ state: 'ready' }), org({ state: 'pending' })])).toBe(true)
    expect(anySettling([org({ state: 'ready' }), org({ state: 'failed' })])).toBe(false)
    expect(anySettling([])).toBe(false)
  })
})

describe('canEnter', () => {
  // Keyed on the link the server built rather than on the state. Registration
  // writes the console row and the queue row separately, so for a moment an
  // organisation can read as ready with nowhere to send anybody — and the link
  // is the half that decides whether /authorize answers.
  it('follows the link, not the state', () => {
    expect(canEnter(org({ state: 'ready', url: '' }))).toBe(false)
    expect(canEnter(org({ state: 'pending', url: 'https://cloud.test/authorize?org=acme' }))).toBe(true)
  })
})

describe('the create limit', () => {
  it('counts what is left', () => {
    expect(remainingSlots(me({ orgLimit: 3, orgsCreated: 1 }))).toBe(2)
    expect(canCreate(me({ orgLimit: 3, orgsCreated: 1 }))).toBe(true)
  })

  it('offers no form to an account at its limit', () => {
    expect(remainingSlots(me({ orgLimit: 3, orgsCreated: 3 }))).toBe(0)
    expect(canCreate(me({ orgLimit: 3, orgsCreated: 3 }))).toBe(false)
  })

  // A limit lowered below what an account already has must not report a
  // negative number of remaining slots, which would render as "-2 left".
  it('does not go negative when the limit is lowered', () => {
    expect(remainingSlots(me({ orgLimit: 1, orgsCreated: 3 }))).toBe(0)
    expect(canCreate(me({ orgLimit: 1, orgsCreated: 3 }))).toBe(false)
  })

  it('says the number rather than that a limit exists', () => {
    expect(limitMessage(me({ orgLimit: 3, orgsCreated: 3 }))).toContain('3')
    expect(limitMessage(me({ orgLimit: 1, orgsCreated: 1 }))).toContain('1 organisation,')
  })
})

describe('normalising the wire shape', () => {
  it('converts snake_case at the boundary', () => {
    const o = normaliseOrg({
      name: 'acme',
      display_name: 'Acme',
      role: 'admin',
      state: 'ready',
      attempts: 2,
      url: 'https://cloud.test/authorize?org=acme',
      created_by_me: true,
      purge_after: '2026-09-23T00:00:00Z',
    })
    expect(o).toEqual({
      name: 'acme',
      displayName: 'Acme',
      role: 'admin',
      state: 'ready',
      attempts: 2,
      url: 'https://cloud.test/authorize?org=acme',
      createdByMe: true,
      purgeAfter: '2026-09-23T00:00:00Z',
    })
  })

  // The server omits url and display_name when they are empty, and omits state
  // for an organisation with no queue row. A missing field must not become
  // `undefined` in a template.
  it('fills in what the server omits', () => {
    const o = normaliseOrg({ name: 'acme', role: 'admin' })
    expect(o.url).toBe('')
    expect(o.displayName).toBe('')
    expect(o.state).toBe('')
    expect(o.attempts).toBe(0)
    expect(o.createdByMe).toBe(false)
  })

  // created_by_me decides what this account may do to an organisation later.
  // Anything other than a literal true is false — a truthy string from a
  // mis-serialised payload must not read as ownership.
  it('treats ownership as strictly true', () => {
    expect(normaliseOrg({ created_by_me: 'yes' }).createdByMe).toBe(false)
    expect(normaliseOrg({ created_by_me: 1 }).createdByMe).toBe(false)
    expect(normaliseOrg({ created_by_me: true }).createdByMe).toBe(true)
  })

  it('reads the account and its organisations', () => {
    const m = normaliseMe({
      email: 'a@example.com',
      orgs: [{ name: 'one', state: 'ready' }, { name: 'two', state: 'pending' }],
      org_limit: 3,
      orgs_created: 2,
    })
    expect(m.email).toBe('a@example.com')
    expect(m.orgs.map(o => o.name)).toEqual(['one', 'two'])
    expect(m.orgLimit).toBe(3)
    expect(m.orgsCreated).toBe(2)
  })

  it('survives an account with no organisations', () => {
    const m = normaliseMe({ email: 'a@example.com', org_limit: 3, orgs_created: 0 })
    expect(m.orgs).toEqual([])
    expect(canCreate(m)).toBe(true)
  })
})

describe('deletion', () => {
  it('offers delete to an admin of a serving organisation', () => {
    expect(canDelete(org({ role: 'admin', state: 'ready' }))).toBe(true)
    // The organisations registered by hand have no queue row and work fine.
    expect(canDelete(org({ role: 'admin', state: '' }))).toBe(true)
  })

  it('does not offer delete to a viewer', () => {
    // A hint, not a gate — store.SoftDeleteOrg refuses this too. Offering the
    // button anyway would produce a control whose only outcome is a 403.
    expect(canDelete(org({ role: 'viewer', state: 'ready' }))).toBe(false)
  })

  it('does not offer delete mid-provision or mid-teardown', () => {
    for (const state of ['pending', 'provisioning', 'deleted', 'purging'] as const) {
      expect(canDelete(org({ state }))).toBe(false)
    }
  })

  it('offers restore only while it is still deleted', () => {
    expect(canRestore(org({ role: 'admin', state: 'deleted' }))).toBe(true)
    // Once the reaper has it there is nothing left to bring back.
    expect(canRestore(org({ role: 'admin', state: 'purging' }))).toBe(false)
    expect(canRestore(org({ role: 'viewer', state: 'deleted' }))).toBe(false)
  })

  it('does not let anybody enter a deleted organisation', () => {
    // The URL survives the delete — the server builds it from the console row,
    // which is untouched — so a check on url alone would keep the link live.
    const deleted = org({ state: 'deleted' })
    expect(deleted.url).not.toBe('')
    expect(canEnter(deleted)).toBe(false)
    expect(canEnter(org({ state: 'purging' }))).toBe(false)
  })

  it('says when a deleted organisation stops being restorable', () => {
    const msg = deletedMessage(org({ state: 'deleted', purgeAfter: '2026-09-23T00:00:00Z' }))
    expect(msg).toContain('Restorable until')
    expect(msg).not.toContain('Invalid Date')
  })

  it('degrades to a shorter sentence rather than rendering Invalid Date', () => {
    // new Date('') is Invalid Date, and `${Invalid Date}` renders as the words
    // "Invalid Date" on the screen.
    for (const bad of ['', 'not-a-date']) {
      const msg = deletedMessage(org({ state: 'deleted', purgeAfter: bad }))
      expect(msg).not.toContain('Invalid Date')
      expect(msg).toContain('retention period')
    }
  })

  it('reads purge_after off the wire', () => {
    const o = normaliseOrg({ name: 'acme', state: 'deleted', purge_after: '2026-09-23T00:00:00Z' })
    expect(o.purgeAfter).toBe('2026-09-23T00:00:00Z')
    expect(normaliseOrg({ name: 'acme' }).purgeAfter).toBe('')
  })

  it('labels the two new states', () => {
    expect(stateLabel('deleted')).toBe('Deleted')
    expect(stateLabel('purging')).toBe('Being destroyed')
  })
})

// enterURL decides whether a console that sent somebody here gets them back to
// the page they asked for. A dropped parameter is invisible: the sign-in works
// and lands on the console's default page.
describe('enterURL', () => {
  it('adds the return path to the link the server built', () => {
    expect(enterURL('https://cloud.test/authorize?org=acme', '/approvals'))
      .toBe('https://cloud.test/authorize?org=acme&return_to=%2Fapprovals')
  })

  it('opens the query when the link has none', () => {
    expect(enterURL('https://console.test/', '/schema'))
      .toBe('https://console.test/?return_to=%2Fschema')
  })

  it('leaves the link alone when there is nothing to carry', () => {
    expect(enterURL('https://cloud.test/authorize?org=acme', ''))
      .toBe('https://cloud.test/authorize?org=acme')
    expect(enterURL('', '/approvals')).toBe('')
  })
})
