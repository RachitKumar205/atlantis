import { describe, expect, it } from 'vitest'

import {
  anySettling,
  canCreate,
  canEnter,
  isSettling,
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
    })
    expect(o).toEqual({
      name: 'acme',
      displayName: 'Acme',
      role: 'admin',
      state: 'ready',
      attempts: 2,
      url: 'https://cloud.test/authorize?org=acme',
      createdByMe: true,
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
