import { useState, type FormEvent } from 'react'
import { TrashIcon } from '@phosphor-icons/react'

import { Button, Card, Field, LinkButton, Notice } from '@/components/ui'
import {
  canCreate,
  canDelete,
  canEnter,
  canRestore,
  deletedMessage,
  enterURL,
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
// RETURN_TO is the path the console was trying to reach when it sent the
// browser here, carried back on the /authorize link.
//
// Read at module scope, which runs before the first screen: the sign-in
// screens replace the URL as they go, and a later read returns whatever the
// last one left.
//
// A sign-in through GitHub or Google leaves this origin and returns to
// /signin, so the value is empty on that path and the console lands on its
// default page.
const RETURN_TO = new URLSearchParams(window.location.search).get('return_to') ?? ''

export function Organisations({
  me,
  error,
  busy,
  onCreate,
  onDelete,
  onRestore,
  onSignOut,
}: {
  me: Me
  error: string | null
  busy: boolean
  onCreate: (name: string, displayName: string) => void
  onDelete: (name: string, confirm: string) => void
  onRestore: (name: string) => void
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
      mark={false}
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
            <OrgRow key={o.name} org={o} busy={busy} onDelete={onDelete} onRestore={onRestore} />
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
            /*
              Through the prop, so it renders inside the field and takes the
              field's own spacing. As a sibling paragraph it sat in the gap
              between two fields, 18px below the input it described and flush
              against the label of the next one.

              Said before the field is submitted rather than after it is
              refused. The rule is enforced in three places — this hint, Go, and
              a CHECK constraint — and only the last of those is the
              enforcement.
            */
            hint="Lowercase letters, digits and hyphens. This becomes part of every address and certificate for the organisation, and cannot be changed."
          />
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
 * One organisation, and what may be done to it.
 *
 * A link only when there is somewhere to go. An organisation still being set up
 * renders its state as text rather than a link that would answer 503 — a
 * control that reliably does nothing teaches people the page is broken.
 *
 * The delete control is a two-step: pressing Delete opens a field, and the
 * organisation's own name has to be typed into it. That is not decoration over
 * a confirm dialog — the server requires the same string in the request body
 * and refuses without it. Doing it here as well means the refusal is a hint
 * beside the field rather than a round trip.
 */
function OrgRow({
  org,
  busy,
  onDelete,
  onRestore,
}: {
  org: Org
  busy: boolean
  onDelete: (name: string, confirm: string) => void
  onRestore: (name: string) => void
}) {
  const [confirming, setConfirming] = useState(false)
  const [confirm, setConfirm] = useState('')
  const label = stateLabel(org.state)
  const matches = confirm.trim() === org.name

  return (
    <li className="orglist__item">
      {/*
        The row is the link, by way of an ::after that covers it. The delete
        control sits above that overlay rather than inside the anchor, which is
        how the whole row can be clickable without nesting one interactive
        element in another.
      */}
      <span className="orglist__avatar" aria-hidden="true">
        {(org.displayName || org.name).trim().charAt(0)}
      </span>

      <span className="orglist__main">
        {canEnter(org) ? (
          <a className="orglist__link" href={enterURL(org.url, RETURN_TO)}>
            {org.displayName || org.name}
          </a>
        ) : (
          <span className="orglist__name">{org.displayName || org.name}</span>
        )}
        {/* The slug carries into every address and certificate the organisation
            has, so it is shown rather than left to be remembered. Only when it
            differs from the name above it. */}
        {org.displayName && org.displayName !== org.name ? (
          <span className="orglist__meta">{org.name}</span>
        ) : null}
      </span>

      {/* A word, not just a colour. Absent while the organisation is simply
          ready, because a badge on every row says nothing. */}
      {!canEnter(org) ? (
        <span className={`badge ${org.state === 'deleted' ? 'badge--danger' : 'badge--warn'}`}>
          {label}
        </span>
      ) : null}

      <span className="orglist__actions">
        {canDelete(org) && !confirming ? (
          <button
            type="button"
            className="btn btn--inline btn--outline-danger"
            onClick={() => setConfirming(true)}
          >
            <TrashIcon aria-hidden="true" weight="regular" />
            Delete
          </button>
        ) : null}

        {canRestore(org) ? (
          <LinkButton className="linkbtn linkbtn--quiet" onClick={() => onRestore(org.name)}>
            Restore
          </LinkButton>
        ) : null}
      </span>

      {/*
        Said where the decision is made. A deleted organisation is not gone, and
        the only useful fact about it is how long that stays true.
      */}
      {org.state === 'deleted' ? (
        <p className="hint">{deletedMessage(org)}</p>
      ) : null}

      {confirming ? (
        <div className="orglist__confirm">
          <Field
            label={`Type ${org.name} to confirm`}
            name={`confirm-${org.name}`}
            inputMode="text"
            autoComplete="off"
            value={confirm}
            onChange={e => setConfirm(e.target.value)}
            autoFocus
          />
          <p className="hint">
            The organisation stops serving immediately. Its database and
            certificate authority are kept for a retention period, and it can be
            restored until then — after that it is destroyed permanently.
          </p>
          <Button
            type="button"
            busy={busy}
            disabled={!matches}
            onClick={() => onDelete(org.name, confirm.trim())}
          >
            Delete organisation
          </Button>
          <LinkButton
            onClick={() => {
              setConfirming(false)
              setConfirm('')
            }}
          >
            Cancel
          </LinkButton>
        </div>
      ) : null}
    </li>
  )
}
