// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import { act } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

// Minting an enrolment token, from the button to the two calls it makes.
//
// This file exists because the Enrol button shipped in a state where it could
// never succeed: it called a route guarded by requireSudo directly, the server
// refused every time, and the page rendered the refusal as a toast. The whole
// observable behaviour of the control was displaying its own rejection.
//
// Nothing caught it. The server tests assert the route refuses WITHOUT sudo —
// correctly — and no test could assert that an operator is able to OBTAIN sudo,
// because vitest ran with `environment: 'node'` and there was no way to press
// anything. K7c saw the risk and lifted the wrong decision into lib/: it moved
// "should the control be offered" and left "what does the control do".
//
// The environment is set per-file by the docblock above rather than globally.
// vitest 4 removed `environmentMatchGlobs`, and the two existing suites are
// pure functions that have no business paying for a DOM.

const enroll = vi.fn()
const sudo = vi.fn()

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return {
    ...actual,
    api: {
      callers: {
        list: async () => ({ callers: [{ caller: 'backend', can_mutate: true }] }),
        certs: async () => ({ enrolment_enabled: true, certs: [] }),
        enroll,
        revoke: vi.fn(),
        register: vi.fn(),
      },
      instance: { get: async () => ({ endpoint: 'localhost:9090' }) },
      auth: { sudo, me: async () => ({ role: 'admin', step_up_url: 'http://cloud.test/authorize' }) },
    },
    queries: {
      callers: () => ({ queryKey: ['callers'], queryFn: async () => ({ callers: [{ caller: 'backend', can_mutate: true }] }) }),
      callerCerts: () => ({ queryKey: ['caller-certs'], queryFn: async () => ({ enrolment_enabled: true, certs: [] }) }),
      instance: () => ({ queryKey: ['instance'], queryFn: async () => ({ endpoint: 'localhost:9090' }) }),
      me: () => ({ queryKey: ['auth', 'me'], queryFn: async () => ({ role: 'admin', step_up_url: 'http://cloud.test/authorize' }) }),
    },
  }
})

// Imported after the mock so the page picks it up.
const { Callers } = await import('./Callers')
const { STEP_UP_MESSAGE } = await import('./Login')

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={qc}>
      <Callers />
    </QueryClientProvider>,
  )
}

/** Presses Enrol on the one caller the fixture has. */
async function pressEnrol() {
  // The card's control is icon-only with aria-label "Enrol a machine". Take
  // the first match: once the gate is open the dialog contributes its own.
  const buttons = await screen.findAllByRole('button', { name: /enrol/i })
  await act(async () => {
    buttons[0].click()
  })
}

/** Delivers an assertion the way the step-up popup does. */
async function deliverAssertion(assertion = 'assert-1') {
  await act(async () => {
    window.dispatchEvent(
      new MessageEvent('message', {
        data: { type: STEP_UP_MESSAGE, assertion },
        origin: window.location.origin,
      }),
    )
  })
}

beforeEach(() => {
  enroll.mockReset()
  sudo.mockReset()
  // jsdom's window.open returns null, which the dialog reads as a blocked
  // popup — it then reveals the paste field and keeps working. That is the
  // path this test drives, and it is also a real one.
  vi.stubGlobal('open', () => null)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('the Enrol control', () => {
  // The defect, stated as a property: pressing Enrol must not call the
  // sudo-gated route. It has to ask for a second factor first.
  it('opens the sudo gate instead of calling the route', async () => {
    renderPage()
    await pressEnrol()

    expect(enroll).not.toHaveBeenCalled()
    expect(await screen.findByText(/confirm it is you|mint an enrolment token/i)).toBeTruthy()
  })

  // Order matters and is not interchangeable: sudo is a property of the
  // session, so minting before elevating is refused exactly as it was in
  // production.
  it('elevates before minting', async () => {
    const order: string[] = []
    sudo.mockImplementation(async () => {
      order.push('sudo')
      return { ok: true, expires_in_seconds: 300 }
    })
    enroll.mockImplementation(async () => {
      order.push('enroll')
      return { token: 'tok-1', caller: 'backend', expires_at: '2030-01-01T00:00:00Z' }
    })

    renderPage()
    await pressEnrol()
    await deliverAssertion()

    await waitFor(() => expect(order).toEqual(['sudo', 'enroll']))
    expect(sudo).toHaveBeenCalledWith('assert-1')
    expect(enroll).toHaveBeenCalledWith('backend')
  })

  // A refusal keeps the gate on screen with the reason, so a stale code is one
  // retry rather than a return to the list.
  it('keeps the dialog open when minting fails', async () => {
    sudo.mockResolvedValue({ ok: true, expires_in_seconds: 300 })
    enroll.mockRejectedValue(new Error('sudo required'))

    renderPage()
    await pressEnrol()
    await deliverAssertion()

    expect(await screen.findByText(/sudo required/i)).toBeTruthy()
  })
})
