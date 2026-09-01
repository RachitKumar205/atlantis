import { describe, expect, it } from 'vitest'
import { exactTime, relativeTime } from './time'

// The boundaries, because every one of them is a place the wording changes and
// an off-by-one reads as a bug to whoever sees "0m ago".
const NOW = new Date('2026-09-02T12:00:00Z').getTime()
const ago = (ms: number) => new Date(NOW - ms).toISOString()

const SEC = 1000
const MIN = 60 * SEC
const HOUR = 60 * MIN
const DAY = 24 * HOUR

describe('relativeTime', () => {
  it.each([
    [ago(0), 'just now'],
    [ago(59 * SEC), 'just now'],
    [ago(60 * SEC), '1m ago'],
    [ago(59 * MIN), '59m ago'],
    [ago(60 * MIN), '1h ago'],
    [ago(23 * HOUR), '23h ago'],
    [ago(24 * HOUR), 'yesterday'],
    [ago(47 * HOUR), 'yesterday'],
    [ago(48 * HOUR), '2d ago'],
    [ago(7 * DAY), '7d ago'],
  ])('%s -> %s', (input, want) => {
    expect(relativeTime(input, NOW)).toBe(want)
  })

  // Past a week the reader gets a date rather than a subtraction to perform.
  it('falls back to an absolute date after seven days', () => {
    const out = relativeTime(ago(8 * DAY), NOW)
    expect(out).not.toMatch(/ago/)
    expect(out).toMatch(/2026/)
  })

  // A clock that disagrees with the server must not render "-3m ago".
  it('reads a future timestamp as just now', () => {
    expect(relativeTime(new Date(NOW + 5 * MIN).toISOString(), NOW)).toBe('just now')
  })

  it('returns an unparseable value as given', () => {
    expect(relativeTime('not a date', NOW)).toBe('not a date')
  })
})

describe('exactTime', () => {
  it('carries a year and a time, which the relative form drops', () => {
    const out = exactTime(ago(3 * DAY))
    expect(out).toMatch(/2026/)
    expect(out).toMatch(/\d\d?:\d\d/)
  })

  it('returns an unparseable value as given', () => {
    expect(exactTime('not a date')).toBe('not a date')
  })
})
