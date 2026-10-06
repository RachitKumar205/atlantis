// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
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
const stepUp = vi.fn()

const me = {
  role: 'admin',
  step_up: true,
  step_up_endpoint: 'http://cloud.test/api/orgs/acme/step-up',
  cloud_url: 'http://cloud.test',
}

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
      auth: { sudo, stepUp, me: async () => me },
    },
    queries: {
      callers: () => ({ queryKey: ['callers'], queryFn: async () => ({ callers: [{ caller: 'backend', can_mutate: true }] }) }),
      callerCerts: () => ({ queryKey: ['caller-certs'], queryFn: async () => ({ enrolment_enabled: true, certs: [] }) }),
      instance: () => ({ queryKey: ['instance'], queryFn: async () => ({ endpoint: 'localhost:9090' }) }),
      me: () => ({ queryKey: ['auth', 'me'], queryFn: async () => me }),
    },
  }
})

// Imported after the mock so the page picks it up.
const { Callers } = await import('./Callers')
const { ApiError, STEP_UP_SESSION_REQUIRED } = await import('@/api/client')

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
  // The card's control is icon-only with aria-label "Enrol a machine". The
  // exact name, because the card also carries "Enrolment access" — a loose
  // /enrol/i matched that one first and pressed the wrong gate. Take the
  // first match: once the gate is open the dialog contributes its own.
  const buttons = await screen.findAllByRole('button', { name: /enrol a machine/i })
  await act(async () => {
    buttons[0].click()
  })
}

/** Types a code into the six boxes and confirms, as an operator does. */
async function enterCode(code = '123456') {
  const slots = await screen.findAllByLabelText(/^digit \d of 6$/i)
  await act(async () => {
    code.split('').forEach((ch, i) => fireEvent.change(slots[i], { target: { value: ch } }))
  })
  const confirm = screen.getByRole('button', { name: /mint the token/i })
  await act(async () => {
    confirm.click()
  })
}

beforeEach(() => {
  enroll.mockReset()
  sudo.mockReset()
  stepUp.mockReset()
  stepUp.mockResolvedValue({ assertion: 'assert-1' })
})

// Explicit: vitest runs without globals here, so Testing Library cannot
// register its own cleanup, and a dialog left open by one test is the first
// match in the next.
afterEach(() => {
  cleanup()
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

  // The gate opens on the code boxes, with no step before them.
  it('asks for the code straight away', async () => {
    renderPage()
    await pressEnrol()

    expect(await screen.findAllByLabelText(/^digit \d of 6$/i)).toHaveLength(6)
    expect(screen.queryByText(/confirm with atlantis cloud/i)).toBeNull()
  })

  it('sends the code to Cloud at the endpoint the server named', async () => {
    sudo.mockResolvedValue({ ok: true, expires_in_seconds: 300 })
    enroll.mockResolvedValue({ token: 'tok-1', caller: 'backend', expires_at: '2030-01-01T00:00:00Z' })

    renderPage()
    await pressEnrol()
    await enterCode('482915')

    await waitFor(() => expect(stepUp).toHaveBeenCalledWith(me.step_up_endpoint, '482915'))
  })

  // Cloud's refusal is shown in the dialog, and the boxes are cleared: the same
  // code is refused again because Cloud spends each step once.
  it('shows a refused code and clears the boxes', async () => {
    stepUp.mockRejectedValue(new ApiError(401, 'That code is not right, or it has already been used.', 'code_rejected'))

    renderPage()
    await pressEnrol()
    await enterCode()

    expect(await screen.findByText(/that code is not right/i)).toBeTruthy()
    const slots = screen.getAllByLabelText(/^digit \d of 6$/i) as HTMLInputElement[]
    expect(slots.every(s => s.value === '')).toBe(true)
    expect(sudo).not.toHaveBeenCalled()
    expect(enroll).not.toHaveBeenCalled()
  })

  it('takes a backup code in its own field and keeps it after a refusal', async () => {
    stepUp.mockRejectedValueOnce(new ApiError(401, 'That code is not right, or it has already been used.', 'code_rejected'))

    renderPage()
    await pressEnrol()
    await act(async () => {
      screen.getByRole('button', { name: /use a backup code/i }).click()
    })
    const field = await screen.findByPlaceholderText('XXXXX-XXXXX') as HTMLInputElement
    await act(async () => {
      fireEvent.change(field, { target: { value: 'ABCDE-FGHIJ' } })
    })
    await act(async () => {
      screen.getByRole('button', { name: /mint the token/i }).click()
    })

    await waitFor(() => expect(stepUp).toHaveBeenCalledWith(me.step_up_endpoint, 'ABCDE-FGHIJ'))
    expect(await screen.findByText(/that code is not right/i)).toBeTruthy()
    // A typo in a backup code can be corrected rather than retyped.
    expect((screen.getByPlaceholderText('XXXXX-XXXXX') as HTMLInputElement).value).toBe('ABCDE-FGHIJ')
  })

  it('names the likely causes when Cloud cannot be read', async () => {
    stepUp.mockRejectedValue(new TypeError('Failed to fetch'))

    renderPage()
    await pressEnrol()
    await enterCode()

    expect(await screen.findByText(/could not reach atlantis cloud, or it did not accept/i)).toBeTruthy()
    expect(sudo).not.toHaveBeenCalled()
  })

  it('confirms without a code when the console reports step-up off', async () => {
    sudo.mockResolvedValue({ ok: true, expires_in_seconds: 300 })
    enroll.mockResolvedValue({ token: 'tok-1', caller: 'backend', expires_at: '2030-01-01T00:00:00Z' })
    me.step_up = false
    try {
      renderPage()
      await pressEnrol()
      expect(screen.queryAllByLabelText(/^digit \d of 6$/i)).toHaveLength(0)
      await act(async () => {
        (await screen.findByRole('button', { name: /mint the token/i })).click()
      })

      await waitFor(() => expect(sudo).toHaveBeenCalledWith(''))
      expect(stepUp).not.toHaveBeenCalled()
    } finally {
      me.step_up = true
    }
  })

  it('points to Cloud when its session has ended', async () => {
    stepUp.mockRejectedValue(new ApiError(401, 'sign in first', STEP_UP_SESSION_REQUIRED))

    renderPage()
    await pressEnrol()
    await enterCode()

    const link = await screen.findByRole('link', { name: /sign in to atlantis cloud/i })
    expect(link.getAttribute('href')).toBe(me.cloud_url)
    expect(sudo).not.toHaveBeenCalled()
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
    await enterCode()

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
    await enterCode()

    expect(await screen.findByText(/sudo required/i)).toBeTruthy()
  })
})
