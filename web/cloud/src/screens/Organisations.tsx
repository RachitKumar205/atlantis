import { useState, type FormEvent } from 'react'

import { Button, Card, Field, LinkButton, Notice } from '@/components/ui'
import {
  canCreate,
  canEnter,
  limitMessage,
  stateLabel,
  type Me,
  type Org,
} from '@/lib/orgs'

/**
 * Where a completed sign-in lands, and the first page in this application
 * somebody stays on.
 *
 * Presentational, like every other screen here: it takes what to show and what
 * to call, and holds only the text of the form it draws. Every decision — which
 * label, whether to offer the form, whether a link goes anywhere — is a tested
 * function in lib/orgs.ts, because web/cloud has no jsdom and a decision left
 * in a component is one nothing can check.
 *
 * No inline styles anywhere in this file. Cloud's CSP is `style-src 'self'`
 * with no 'unsafe-inline', and `vite dev` does not enforce it — a style
 * attribute here would work all the way through development and fail only in a
 * binary built with -tags embedspa.
 */
export function Organisations({
  me,
  error,
  busy,
  onCreate,
  onSignOut,
}: {
  me: Me
  error: string | null
  busy: boolean
  onCreate: (name: string, displayName: string) => void
  onSignOut: () => void
}) {
  const [name, setName] = useState('')
  const [displayName, setDisplayName] = useState('')

  function submit(e: FormEvent) {
    e.preventDefault()
    onCreate(name.trim(), displayName.trim())
  }

  const empty = me.orgs.length === 0

  return (
    <Card
      title={empty ? 'Create your organisation' : 'Your organisations'}
      subtitle={
        empty
          ? 'An organisation gets its own atlantis, its own database and its own certificate authority.'
          : me.email
      }
    >
      {error ? <Notice kind="error">{error}</Notice> : null}

      {me.orgs.length > 0 ? (
        <ul className="orglist">
          {me.orgs.map(o => (
            <OrgRow key={o.name} org={o} />
          ))}
        </ul>
      ) : null}

      {canCreate(me) ? (
        <form onSubmit={submit}>
          <Field
            label="Name"
            name="org"
            inputMode="text"
            autoComplete="off"
            value={name}
            onChange={e => setName(e.target.value)}
            required
            autoFocus={empty}
          />
          {/*
            Said before the field is submitted rather than after it is refused.
            The rule is enforced in three places — this hint, Go, and a CHECK
            constraint — and only the last of those is the enforcement.
          */}
          <p className="hint">
            Lowercase letters, digits and hyphens. This becomes part of every
            address and certificate for the organisation, and cannot be changed.
          </p>
          <Field
            label="Display name (optional)"
            name="display_name"
            inputMode="text"
            autoComplete="off"
            value={displayName}
            onChange={e => setDisplayName(e.target.value)}
          />
          <Button type="submit" busy={busy}>Create organisation</Button>
        </form>
      ) : (
        <Notice kind="info">{limitMessage(me)}</Notice>
      )}

      <p className="foot">
        <LinkButton onClick={onSignOut}>Sign out</LinkButton>
      </p>
    </Card>
  )
}

/**
 * One organisation.
 *
 * A link only when there is somewhere to go. An organisation still being set up
 * renders its state as text rather than a link that would answer 503 — a
 * control that reliably does nothing teaches people the page is broken.
 */
function OrgRow({ org }: { org: Org }) {
  const label = stateLabel(org.state)
  return (
    <li className="orglist__item">
      <span className="orglist__name">{org.displayName || org.name}</span>
      {canEnter(org) ? (
        <a className="orglist__enter" href={org.url}>Open</a>
      ) : (
        <span className="orglist__state">{label}</span>
      )}
    </li>
  )
}
