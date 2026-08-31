import { afterEach, beforeEach, describe, expect, it } from 'vitest'

import {
  importOffered,
  markImportOffered,
  markOnboardingPending,
  takeOnboardingPending,
} from './onboarding'

// Node exposes `localStorage` as a getter that answers undefined without
// --localstorage-file, and jsdom's own storage does not reach globalThis
// through vitest's environment. Both are settled by installing a double.
interface Store {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
}

const original = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')

function install(store: Store) {
  Object.defineProperty(globalThis, 'localStorage', {
    value: store,
    configurable: true,
    writable: true,
  })
}

function memoryStore(): Store {
  const held = new Map<string, string>()
  return {
    getItem: key => held.get(key) ?? null,
    setItem: (key, value) => void held.set(key, value),
  }
}

describe('importOffered', () => {
  beforeEach(() => install(memoryStore()))

  afterEach(() => {
    if (original) Object.defineProperty(globalThis, 'localStorage', original)
  })

  it('is false for an organisation that has not been offered the dialog', () => {
    expect(importOffered('acme')).toBe(false)
  })

  it('is true once the offer is recorded, and stays true', () => {
    markImportOffered('acme')
    expect(importOffered('acme')).toBe(true)
    expect(importOffered('acme')).toBe(true)
  })

  it('records one organisation without answering for another', () => {
    markImportOffered('acme')
    expect(importOffered('beta')).toBe(false)
  })

  it('keeps earlier organisations when a later one is recorded', () => {
    markImportOffered('acme')
    markImportOffered('beta')
    expect(importOffered('acme')).toBe(true)
    expect(importOffered('beta')).toBe(true)
  })

  it('is true for an empty slug, so a session naming no organisation is never offered', () => {
    expect(importOffered('')).toBe(true)
  })

  it('writes nothing for an empty slug', () => {
    markImportOffered('')
    expect(importOffered('acme')).toBe(false)
  })

  // A value that is not the object this wrote answers false rather than
  // throwing: the dialog opens once more and the next close overwrites it.
  it('treats unparseable storage as nothing recorded', () => {
    localStorage.setItem('atlantis.import-offered', 'not json')
    expect(importOffered('acme')).toBe(false)
    markImportOffered('acme')
    expect(importOffered('acme')).toBe(true)
  })

  it('treats a non-object payload as nothing recorded', () => {
    localStorage.setItem('atlantis.import-offered', '"acme"')
    expect(importOffered('acme')).toBe(false)
  })

  it('treats null stored by JSON as nothing recorded', () => {
    localStorage.setItem('atlantis.import-offered', 'null')
    expect(importOffered('acme')).toBe(false)
  })

  // Storage that throws on read cannot be written either, so answering false
  // would open the dialog on every load with no close able to stop it.
  it('is true when storage cannot be read', () => {
    install({
      getItem: () => {
        throw new Error('denied')
      },
      setItem: () => {},
    })
    expect(importOffered('acme')).toBe(true)
  })

  it('does not throw when storage refuses a write', () => {
    install({
      getItem: () => null,
      setItem: () => {
        throw new Error('quota')
      },
    })
    expect(() => markImportOffered('acme')).not.toThrow()
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
