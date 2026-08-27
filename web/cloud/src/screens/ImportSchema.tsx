import { useState, type FormEvent } from 'react'

import { Button, Card, Field, LinkButton, Notice } from '@/components/ui'
import type { SchemaImport } from '@/lib/api'

// The form, and then what came back.
//
// One screen rather than two because the result is read against what was asked
// for: which host, which namespace. A second screen would have to carry both
// forward to say anything a reader could place.

export function ImportSchema({ error, busy, result, onSubmit, onBack }: {
  error: string | null
  busy: boolean
  result: SchemaImport | null
  onSubmit: (dsn: string, namespace: string, schemas: string[], allowInsecure: boolean) => void
  onBack: () => void
}) {
  const [dsn, setDsn] = useState('')
  const [namespace, setNamespace] = useState('')
  const [schemas, setSchemas] = useState('public')

  // Off every time the screen opens, and never remembered. A database that
  // needed it once is not evidence the next one does, and the cost of being
  // wrong is a live password sent in clear.
  const [allowInsecure, setAllowInsecure] = useState(false)

  function submit(e: FormEvent) {
    e.preventDefault()
    onSubmit(
      dsn.trim(),
      namespace.trim(),
      schemas.split(',').map((s) => s.trim()).filter(Boolean),
      allowInsecure,
    )
  }

  if (result) {
    return <ImportResult result={result} onBack={onBack} />
  }

  return (
    <Card
      title="Import an existing database"
      subtitle="atlantis reads the catalogue and writes the declarations for you."
    >
      {error ? <Notice kind="error">{error}</Notice> : null}

      <Notice kind="info">
        Nothing is written to your database: the read runs in a read-only
        transaction. Use a read-only role, and the connection string is kept
        nowhere — only the host is recorded.
      </Notice>

      <form onSubmit={submit}>
        <Field
          label="Connection string"
          name="dsn"
          type="password"
          placeholder="postgres://readonly@db.example.com:5432/app?sslmode=require"
          autoComplete="off"
          value={dsn}
          onChange={(e) => setDsn(e.target.value)}
          required
          autoFocus
          hint="Must be reachable from the internet and use TLS."
        />
        <Field
          label="Namespace"
          name="namespace"
          placeholder="shop"
          value={namespace}
          onChange={(e) => setNamespace(e.target.value)}
          required
          hint="Groups the generated entities. Becomes part of every entity's name."
        />
        <Field
          label="Schemas"
          name="schemas"
          placeholder="public"
          value={schemas}
          onChange={(e) => setSchemas(e.target.value)}
          hint="Comma separated."
        />
        {/*
          A plain label rather than a Field: Field renders a text input, and
          this is the one control on the screen whose value is a decision rather
          than a value.
        */}
        <label className="import__ack">
          <input
            type="checkbox"
            checked={allowInsecure}
            onChange={(e) => setAllowInsecure(e.target.checked)}
          />
          <span>
            This database has no TLS — send the password in clear. Public
            read-only datasets often need this. Never tick it for a database
            whose credential is worth stealing.
          </span>
        </label>

        <Button type="submit" busy={busy}>Read the schema</Button>
      </form>

      <p className="foot">
        <LinkButton onClick={onBack}>Back</LinkButton>
      </p>
    </Card>
  )
}

function ImportResult({ result, onBack }: { result: SchemaImport; onBack: () => void }) {
  return (
    <Card
      title={`${result.entities.length} table${result.entities.length === 1 ? '' : 's'} from ${result.source}`}
      subtitle="Review these, then commit them to your repository."
    >
      {result.suggestions.length > 0 ? (
        <section className="import__section">
          <h3>Worth changing</h3>
          <ul className="import__suggestions">
            {result.suggestions.map((s, i) => (
              <li key={`${s.Table}-${s.Kind}-${i}`}>
                <span className="import__table">{s.Table}</span>
                <p>{s.Detail}</p>
                {s.Line ? <pre className="import__line">{s.Line}</pre> : null}
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      <section className="import__section">
        <h3>Declarations</h3>
        {result.entities.map((e) => (
          <details key={e.Table} className="import__entity">
            <summary>
              <span className="import__table">{e.Table}</span> → {e.Name}
            </summary>
            <pre className="import__atl">{e.Atl}</pre>
          </details>
        ))}
      </section>

      {/* Named rather than dropped. A table that was found and not declared is
          one nobody knows to declare by hand, and an index nothing mentions is
          one the first plan proposes creating on a table that has it. */}
      {result.skipped.length > 0 ? (
        <section className="import__section">
          <h3>Not declared</h3>
          <ul className="import__notes">
            {result.skipped.map((s, i) => <li key={i}>{s}</li>)}
          </ul>
        </section>
      ) : null}

      {result.warnings.length > 0 ? (
        <section className="import__section">
          <h3>Read this before committing</h3>
          <ul className="import__notes">
            {result.warnings.map((w, i) => <li key={i}>{w}</li>)}
          </ul>
        </section>
      ) : null}

      <p className="foot">
        <LinkButton onClick={onBack}>Done</LinkButton>
      </p>
    </Card>
  )
}
