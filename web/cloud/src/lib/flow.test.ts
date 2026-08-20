import { describe, expect, it } from 'vitest'

import {
  arrivalScreen,
  canLeaveBackupCodes,
  isSessionGone,
  retryAfterMessage,
  screenForNext,
} from './flow'

describe('screenForNext', () => {
  it('maps the values the server sends', () => {
    expect(screenForNext('enrol')).toBe('enrol')
    expect(screenForNext('verify')).toBe('verify')
  })

  // Guessing would show the wrong screen and look like an unrelated bug.
  it('does not guess at anything else', () => {
    for (const v of ['', 'ENROL', 'signin', 'enroll', 'next', '../enrol']) {
      expect(screenForNext(v)).toBeNull()
    }
  })
})

describe('arrivalScreen', () => {
  it('starts at sign-in when nothing is in progress', () => {
    expect(arrivalScreen({ pending: false })).toBe('signin')
  })

  // The whole point of GET /api/auth/pending: the cookie is HttpOnly, so
  // without this a reload mid-enrolment shows a sign-in form.
  it('resumes a half-finished sign-in from the server', () => {
    expect(arrivalScreen({ pending: true, next: 'enrol' })).toBe('enrol')
    expect(arrivalScreen({ pending: true, next: 'verify' })).toBe('verify')
  })

  // The OAuth callback hands over as /signin?next=enrol.
  it('uses the query hint when the server confirms a login but not its step', () => {
    expect(arrivalScreen({ pending: true }, 'enrol')).toBe('enrol')
  })

  // The query string is attacker-supplied; the cookie is not. A crafted link
  // must not conjure a flow for somebody who has not signed in.
  it('ignores the query string when there is no pending login', () => {
    expect(arrivalScreen({ pending: false }, 'enrol')).toBe('signin')
    expect(arrivalScreen({ pending: false }, 'verify')).toBe('signin')
  })

  // The server's answer wins over the URL wherever both are present.
  it('prefers the server over the query string', () => {
    expect(arrivalScreen({ pending: true, next: 'verify' }, 'enrol')).toBe('verify')
  })

  // A pending login the server will not explain still needs a screen, and
  // "present your factor" is the one that cannot enrol a new one.
  it('falls back to verify rather than enrol', () => {
    expect(arrivalScreen({ pending: true })).toBe('verify')
    expect(arrivalScreen({ pending: true, next: 'nonsense' }, 'nonsense')).toBe('verify')
  })
})

describe('canLeaveBackupCodes', () => {
  // Shown once, with no route in W4 that shows them again.
  it('needs an acknowledgement', () => {
    expect(canLeaveBackupCodes(false)).toBe(false)
    expect(canLeaveBackupCodes(true)).toBe(true)
  })
})

describe('retryAfterMessage', () => {
  it('says how long, when the server says', () => {
    expect(retryAfterMessage('30')).toContain('30 seconds')
    expect(retryAfterMessage('60')).toContain('1 minute')
    expect(retryAfterMessage('120')).toContain('2 minutes')
  })

  it('rounds part-seconds up rather than down', () => {
    // Down would tell somebody to try again before they can.
    expect(retryAfterMessage('30.2')).toContain('31 seconds')
    expect(retryAfterMessage('61')).toContain('2 minutes')
  })

  it('still says something useful with no header', () => {
    for (const v of [null, '', 'soon', '0', '-5']) {
      const msg = retryAfterMessage(v)
      expect(msg).toContain('Too many attempts')
      expect(msg).not.toContain('NaN')
      expect(msg).not.toContain('undefined')
    }
  })
})

describe('isSessionGone', () => {
  it('is 401 and nothing else', () => {
    expect(isSessionGone(401)).toBe(true)
    for (const s of [200, 400, 403, 409, 429, 500]) {
      expect(isSessionGone(s)).toBe(false)
    }
  })
})
