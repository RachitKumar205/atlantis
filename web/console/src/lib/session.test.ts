import { describe, it, expect } from 'vitest'
import { carriesAssertion, shouldRenderLogin, switcherMode } from './session'

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
    expect(carriesAssertion('#assertion=abc&mode=reauth')).toBe(true)
    expect(carriesAssertion('#mode=reauth&assertion=abc')).toBe(true)
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

  it('lets the step-up popup through', () => {
    // The popup shares the session cookie, so it is authenticated the instant
    // it opens. Bounced, it never posts the assertion back and the dialog that
    // opened it waits for a message nobody sends.
    expect(shouldRenderLogin(true, '#assertion=abc&mode=reauth')).toBe(true)
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
