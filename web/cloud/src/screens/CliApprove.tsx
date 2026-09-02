import { useState, type FormEvent } from 'react'

import { Button, Card, Field, LinkButton, Notice } from '@/components/ui'
import { cliDecide, cliLookup } from '@/lib/api'
import type { Me } from '@/lib/orgs'

type Details = {
  caller: string
  hostname: string
  address: string
  agent: string
}

/**
 * The `tide login` approval page.
 *
 * The person types the code the CLI printed. Typed, never carried in a link:
 * a link with the code embedded would let anyone mail a victim a click that
 * approves the attacker's machine. Five wrong codes burn the login attempt.
 */
export function CliApprove({ me, onDone }: {
  me: Me
  onDone: () => void
}) {
  const [code, setCode] = useState('')
  const [details, setDetails] = useState<Details | null>(null)
  const [org, setOrg] = useState(me.orgs.length === 1 ? me.orgs[0].name : '')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [decided, setDecided] = useState<'approved' | 'denied' | null>(null)

  async function run(fn: () => Promise<void>) {
    setBusy(true)
    setError(null)
    try {
      await fn()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  function lookup(e: FormEvent) {
    e.preventDefault()
    void run(async () => {
      setDetails(await cliLookup(code))
    })
  }

  function decide(approve: boolean) {
    void run(async () => {
      await cliDecide(code, approve ? org : '', approve)
      setDecided(approve ? 'approved' : 'denied')
    })
  }

  if (decided) {
    return (
      <Card
        title={decided === 'approved' ? 'Machine approved' : 'Request denied'}
        subtitle={
          decided === 'approved'
            ? 'The terminal that showed this code finishes on its own. You can close this page.'
            : 'Nothing was granted. You can close this page.'
        }
        mark={false}
      >
        <p className="foot">
          <LinkButton onClick={onDone}>Back to organisations</LinkButton>
        </p>
      </Card>
    )
  }

  if (details) {
    return (
      <Card
        title="Approve this machine?"
        subtitle="A terminal asked to act as a caller in one of your organisations."
        mark={false}
      >
        {error ? <Notice kind="error">{error}</Notice> : null}

        <dl className="cli-details">
          <dt>Caller</dt>
          <dd>{details.caller}</dd>
          {details.hostname ? (
            <>
              <dt>Machine</dt>
              <dd>{details.hostname}</dd>
            </>
          ) : null}
          <dt>From</dt>
          <dd>{details.address}</dd>
        </dl>

        <div className="field">
          <label className="label" htmlFor="cli-org">
            Organisation
          </label>
          <select
            id="cli-org"
            className="input"
            value={org}
            onChange={(e) => setOrg(e.target.value)}
          >
            {me.orgs.length !== 1 ? <option value="">Choose…</option> : null}
            {me.orgs.map((o) => (
              <option key={o.name} value={o.name}>
                {o.displayName || o.name}
              </option>
            ))}
          </select>
        </div>

        <Button type="button" busy={busy} disabled={org === ''} onClick={() => decide(true)}>
          Approve
        </Button>
        <p className="foot">
          <LinkButton onClick={() => decide(false)}>Deny this request</LinkButton>
        </p>
      </Card>
    )
  }

  return (
    <Card
      title="Approve a terminal"
      subtitle="Type the code your terminal is showing."
      mark={false}
    >
      {error ? <Notice kind="error">{error}</Notice> : null}

      <form onSubmit={lookup}>
        <Field
          label="Code"
          name="user_code"
          value={code}
          onChange={(e) => setCode(e.target.value.toUpperCase())}
          placeholder="XXXX-XXXX"
          autoComplete="off"
          required
          autoFocus
        />
        <Button type="submit" busy={busy}>
          Continue
        </Button>
      </form>

      <p className="foot">
        <LinkButton onClick={onDone}>Back to organisations</LinkButton>
      </p>
    </Card>
  )
}
