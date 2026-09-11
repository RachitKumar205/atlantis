import { afterEach, beforeEach, describe, expect, it } from 'vitest'

import {
  markOnboardingPending,
  onboardingDecision,
  takeOnboardingPending,
  type OnboardingState,
} from './onboarding'

// The pending mark's harness needs a storage shape; jsdom's own does not reach
// globalThis through vitest's environment, so a double is installed below.
interface Store {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
}

function state(over: Partial<OnboardingState> = {}): OnboardingState {
  return { loading: false, onboarded: false, entities: 0, ...over }
}

describe('onboardingDecision', () => {
  it('offers the flow to an organisation that has not answered', () => {
    expect(onboardingDecision(state())).toBe('offer')
  })

  // The whole point: one member answers, and nobody in the organisation is
  // asked again — on any browser, on any machine.
  it('is settled once the organisation has answered', () => {
    expect(onboardingDecision(state({ onboarded: true }))).toBe('settled')
  })

  // Carries every organisation that existed before the column did, with no
  // backfill. A schema is proof the flow is behind them.
  it('is settled for an organisation that already has a schema', () => {
    expect(onboardingDecision(state({ entities: 12 }))).toBe('settled')
  })

  it('is settled when both say so', () => {
    expect(onboardingDecision(state({ onboarded: true, entities: 12 }))).toBe('settled')
  })

  // `wait` is not `settled`. An unread schema looks exactly like an empty one,
  // and spending the arrival here would cost an organisation its one offer.
  it('waits while the schema is still being read', () => {
    expect(onboardingDecision(state({ loading: true }))).toBe('wait')
    expect(onboardingDecision(state({ loading: true, onboarded: true }))).toBe('wait')
    expect(onboardingDecision(state({ loading: true, entities: 12 }))).toBe('wait')
  })

  it('waits until the organisation answer has arrived', () => {
    expect(onboardingDecision(state({ onboarded: undefined }))).toBe('wait')
  })
})


describe('onboarding pending', () => {
  const originalSession = Object.getOwnPropertyDescriptor(globalThis, 'sessionStorage')

  function installSession(store: Store & { removeItem(key: string): void }) {
    Object.defineProperty(globalThis, 'sessionStorage', {
      value: store,
      configurable: true,
      writable: true,
    })
  }

  function memorySession() {
    const held = new Map<string, string>()
    return {
      getItem: (key: string) => held.get(key) ?? null,
      setItem: (key: string, value: string) => void held.set(key, value),
      removeItem: (key: string) => void held.delete(key),
    }
  }

  beforeEach(() => installSession(memorySession()))

  afterEach(() => {
    if (originalSession) Object.defineProperty(globalThis, 'sessionStorage', originalSession)
  })

  it('is false when no arrival has been marked', () => {
    expect(takeOnboardingPending()).toBe(false)
  })

  it('is true once after a mark', () => {
    markOnboardingPending()
    expect(takeOnboardingPending()).toBe(true)
  })

  // One sign-in opens the flow once, not on every return to the schema page.
  it('clears the mark, so a second read is false', () => {
    markOnboardingPending()
    takeOnboardingPending()
    expect(takeOnboardingPending()).toBe(false)
  })

  it('is true again after the next arrival', () => {
    markOnboardingPending()
    expect(takeOnboardingPending()).toBe(true)
    markOnboardingPending()
    expect(takeOnboardingPending()).toBe(true)
  })

  it('is false when storage cannot be read', () => {
    installSession({
      getItem: () => {
        throw new Error('denied')
      },
      setItem: () => {},
      removeItem: () => {},
    })
    expect(takeOnboardingPending()).toBe(false)
  })

  it('does not throw when storage refuses a write', () => {
    installSession({
      getItem: () => null,
      setItem: () => {
        throw new Error('quota')
      },
      removeItem: () => {},
    })
    expect(() => markOnboardingPending()).not.toThrow()
  })
})
