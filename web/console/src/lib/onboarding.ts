// Whether the import dialog has already been offered for an organisation.
//
// Keyed by organisation slug, not by browser: an account with two
// organisations provisions the second one long after answering for the first.
//
// Held in localStorage. Losing the record costs one reopened dialog on a
// schema that is still empty.

const KEY = 'atlantis.import-offered'

// null distinguishes storage that cannot be read from storage holding nothing.
//
// A value that will not parse is storage holding nothing: the read itself
// worked, so the write that replaces the value will work too.
function read(): Record<string, true> | null {
  let raw: string | null
  try {
    raw = localStorage.getItem(KEY)
  } catch {
    return null
  }
  if (!raw) return {}
  try {
    const parsed: unknown = JSON.parse(raw)
    if (!parsed || typeof parsed !== 'object') return {}
    return parsed as Record<string, true>
  } catch {
    return {}
  }
}

// importOffered reports whether the dialog has been shown and closed for org.
//
// Storage that cannot be read answers true. A write will not survive either, so
// answering false opens the dialog on every load with no way to stop it.
export function importOffered(org: string): boolean {
  if (!org) return true
  const seen = read()
  if (seen === null) return true
  return seen[org] === true
}

export function markImportOffered(org: string): void {
  if (!org) return
  const seen = read()
  if (seen === null) return
  try {
    localStorage.setItem(KEY, JSON.stringify({ ...seen, [org]: true }))
  } catch {
    // Reads can succeed where writes fail once the quota is full. The dialog
    // opens again on the next load.
  }
}

// Whether a sign-in arriving from Cloud has just completed.
//
// Set when the assertion is exchanged and read once by the schema page. The
// exchange ends in a full page load, so a module variable does not survive it;
// sessionStorage does, and is scoped to the one tab that signed in.
const PENDING = 'atlantis:onboarding-pending'

export function markOnboardingPending(): void {
  try {
    sessionStorage.setItem(PENDING, '1')
  } catch {
    // Nothing to fall back to. The flow stays reachable from the button.
  }
}

// takeOnboardingPending reports whether an arrival is waiting and clears it, so
// one sign-in opens the flow once however many times the page is mounted.
export function takeOnboardingPending(): boolean {
  try {
    if (sessionStorage.getItem(PENDING) === null) return false
    sessionStorage.removeItem(PENDING)
    return true
  } catch {
    return false
  }
}
