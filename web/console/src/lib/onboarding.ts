// Whether an organisation still has the onboarding flow to answer.
//
// The record of having answered is the organisation's, held by the server and
// read from /api/auth/me. It was once a localStorage entry keyed by slug,
// which answered for one browser: the second admin was asked again, and so was
// the same person on a second machine.

export type OnboardingState = {
  // The schema is still being read. An empty schema and an unread one look
  // the same from here, and only one of them is an organisation to ask.
  loading: boolean

  // The organisation's own answer, or undefined until /api/auth/me answers.
  onboarded: boolean | undefined

  // How many entities the schema holds. An organisation with any has plainly
  // done this, whatever the server recorded — which is what carries the
  // organisations that existed before the column did.
  entities: number
}

// What to do about an arrival.
//
// `wait` is not `settled`: nothing is known yet, and spending the arrival here
// would lose it. The two are separate so a slow schema read cannot silently
// cost an organisation its one offer.
export type OnboardingDecision = 'wait' | 'offer' | 'settled'

export function onboardingDecision(s: OnboardingState): OnboardingDecision {
  if (s.loading || s.onboarded === undefined) return 'wait'
  if (s.onboarded || s.entities > 0) return 'settled'
  return 'offer'
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
