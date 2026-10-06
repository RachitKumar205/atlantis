import { describe, it, expect } from 'vitest'
import {
  carriesAssertion,
  DEFAULT_PATH,
  enrolControlState,
  safeNext,
  shouldRenderLogin,
  switcherMode,
} from './session'

// Two branches nobody would notice breaking.
//
// `shouldRenderLogin` returning false on an arrival looks like "the switch did
// nothing" — the browser lands on /schema, in the organisation it started in,
// with no error anywhere and a spent-looking URL. `switcherMode` returning
// 'label' for two organisations looks like nothing at all: a sidebar without a
// control is a sidebar.
//
// Neither is reachable from a type error, and both were one boolean when they
// were written inline.

describe('carriesAssertion', () => {
  it('finds the assertion Cloud actually sends', () => {
    expect(carriesAssertion('#assertion=abc')).toBe(true)
    expect(carriesAssertion('assertion=abc')).toBe(true)
    expect(carriesAssertion('#assertion=abc&return_to=%2Fschema')).toBe(true)
    expect(carriesAssertion('#return_to=%2Fschema&assertion=abc')).toBe(true)
  })

  it('is not fooled by a fragment that merely contains the word', () => {
    // The guard used to test for the substring `assertion=`, which matches
    // both of these. A browser carrying either would have been let onto a page
    // that then found nothing to exchange, and sat there.
    expect(carriesAssertion('#my_assertion=abc')).toBe(false)
    expect(carriesAssertion('#note=see+assertion=abc')).toBe(false)
  })

  it('treats an empty value as absent', () => {
    // `?assertion=` with nothing after it is not an arrival, and letting it
    // through would put an authenticated user on a login page for no reason.
    expect(carriesAssertion('#assertion=')).toBe(false)
    expect(carriesAssertion('#')).toBe(false)
    expect(carriesAssertion('')).toBe(false)
  })
})

describe('shouldRenderLogin', () => {
  it('renders for a browser with no session', () => {
    expect(shouldRenderLogin(false, '')).toBe(true)
    expect(shouldRenderLogin(false, '#assertion=abc')).toBe(true)
  })

  it('bounces a signed-in browser that is just visiting', () => {
    expect(shouldRenderLogin(true, '')).toBe(false)
    expect(shouldRenderLogin(true, '#somethingelse=1')).toBe(false)
  })

  it('lets a signed-in browser through when it is carrying an assertion', () => {
    // The organisation switch. The session for the old organisation is still
    // live when Cloud redirects back here, so "authenticated" is exactly the
    // state a returning switcher is in — bouncing it drops the token.
    expect(shouldRenderLogin(true, '#assertion=abc')).toBe(true)
  })
})

describe('switcherMode', () => {
  it('draws nothing before there is a session', () => {
    expect(switcherMode(undefined)).toBe('none')
    expect(switcherMode({ org: '', orgs: [] })).toBe('none')
  })

  it('draws a label, not a menu, for one organisation', () => {
    expect(switcherMode({ org: 'acme', orgs: [{ name: 'acme', url: 'u' }] })).toBe('label')
  })

  it('draws a label for a session opened before the claim existed', () => {
    // orgs is empty on every session written before migration 0006, and on any
    // assertion minted while Cloud could not read memberships. The current
    // organisation is still known, so the label is still true.
    expect(switcherMode({ org: 'acme', orgs: [] })).toBe('label')
  })

  it('draws a menu once there is somewhere to go', () => {
    expect(switcherMode({
      org: 'acme',
      orgs: [{ name: 'acme', url: 'u1' }, { name: 'globex', url: 'u2' }],
    })).toBe('menu')
  })
})

describe('enrolControlState', () => {
  const enabled = (callers: string[]) => ({
    enrolment_enabled: true,
    certs: callers.map(caller => ({ caller })),
  })

  it('offers nothing when no signer is configured', () => {
    expect(enrolControlState({ enrolment_enabled: false, certs: [] }, 'backend'))
      .toBe('unconfigured')
  })

  it('offers nothing before the answer has arrived', () => {
    // undefined is the loading state and the error state at once. Reading it as
    // ready puts a live button in front of somebody on a console that may have
    // no signer at all, and pressing it returns 503 — which is the failure
    // enrolment was built to remove, reappearing in the browser.
    expect(enrolControlState(undefined, 'backend')).toBe('unconfigured')
  })

  it('is a first enrolment when this console has never enrolled the caller', () => {
    expect(enrolControlState(enabled([]), 'backend')).toBe('first')
    // Another caller's enrolment is not this one's.
    expect(enrolControlState(enabled(['payments']), 'backend')).toBe('first')
  })

  it('is a re-enrolment once the caller has a certificate on record', () => {
    expect(enrolControlState(enabled(['backend']), 'backend')).toBe('reenrol')
    expect(enrolControlState(enabled(['payments', 'backend']), 'backend')).toBe('reenrol')
  })

  it('never confuses a caller with one whose name contains it', () => {
    // `backend` and `backend-worker` are ordinary names to pick, and a prefix
    // or substring match would report the wrong one as already enrolled — so
    // the warning would name a date belonging to a different machine.
    expect(enrolControlState(enabled(['backend-worker']), 'backend')).toBe('first')
    expect(enrolControlState(enabled(['backend']), 'backend-worker')).toBe('first')
  })
})

// safeNext is the only thing standing between a fragment that arrived through
// Cloud and window.location. Everything it rejects is a navigation off this
// origin that a sign-in would have performed on the browser's behalf.
describe('safeNext', () => {
  it('keeps a path on this console', () => {
    expect(safeNext('/approvals')).toBe('/approvals')
    expect(safeNext('/schema?namespace=consumer')).toBe('/schema?namespace=consumer')
  })

  it('refuses anything that names another origin', () => {
    expect(safeNext('https://evil.example')).toBe(DEFAULT_PATH)
    expect(safeNext('//evil.example')).toBe(DEFAULT_PATH)
    expect(safeNext('/\\evil.example')).toBe(DEFAULT_PATH)
    expect(safeNext('javascript:alert(1)')).toBe(DEFAULT_PATH)
  })

  it('falls back when there is nothing to use', () => {
    expect(safeNext(null)).toBe(DEFAULT_PATH)
    expect(safeNext(undefined)).toBe(DEFAULT_PATH)
    expect(safeNext('')).toBe(DEFAULT_PATH)
    expect(safeNext('approvals')).toBe(DEFAULT_PATH)
  })
})
