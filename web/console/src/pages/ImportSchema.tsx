import { useState, type FormEvent } from 'react'
import { useMutation } from '@tanstack/react-query'
import { AlertTriangle, Database, Lightbulb, ShieldAlert } from 'lucide-react'
import { api, type ImportedEntity, type SchemaImportResult } from '@/api/client'
import { PageShell } from '@/components/PageShell'

// Point atlantis at a database it does not manage, and get .atl describing it.
//
// One box. Every schema is read and each becomes a namespace of its own, so
// there is nothing to name and nothing to know about the database first.
//
// Nothing here is applied. The declarations are read and committed to the
// caller's repository by hand; `tide plan` still decides what reaches the
// database.

const KIND_LABEL: Record<string, string> = {
  'tenant-isolation': 'Tenant isolation',
  'no-primary-key': 'No primary key',
  'unindexed-foreign-key': 'Unindexed foreign key',
}

export function ImportSchema() {
  const [dsn, setDsn] = useState('')

  // Off on every load and never remembered. Ticking it sends the password
  // across the internet in clear.
  const [allowInsecure, setAllowInsecure] = useState(false)

  const run = useMutation<SchemaImportResult, Error>({
    mutationFn: () => api.schemaImport.run(dsn.trim(), allowInsecure),
  })

  function submit(e: FormEvent) {
    e.preventDefault()
    run.mutate()
  }

  const result = run.data

  return (
    <PageShell
      title="Import a database"
      sub="Read an existing Postgres and generate the declarations for it."
    >
      <div className="page__bodyinner import">
        {!result && (
          <form className="card" onSubmit={submit}>
            <div className="card__body import__form">
              <p className="import__lede">
                Every schema is read, and each becomes a namespace of the same
                name. Nothing is written to your database: the read runs in a
                read-only transaction. Use a read-only role. The connection
                string is kept nowhere — only the host is recorded against this
                organisation.
              </p>

              <label className="field">
                <span className="field__label">Connection string</span>
                <input
                  className="input mono"
                  type="password"
                  autoComplete="off"
                  placeholder="postgres://readonly@db.example.com:5432/app?sslmode=require"
                  value={dsn}
                  onChange={e => setDsn(e.target.value)}
                  required
                  autoFocus
                />
                <span className="field__hint">
                  Must be reachable from the internet and use TLS.
                </span>
              </label>

              <label className="import__ack">
                <input
                  type="checkbox"
                  checked={allowInsecure}
                  onChange={e => setAllowInsecure(e.target.checked)}
                />
                <span>
                  <ShieldAlert size={13} /> This database has no TLS — send the
                  password in clear. Public read-only datasets often need this.
                  Never tick it for a database whose credential is worth stealing.
                </span>
              </label>

              {run.isError && (
                <div className="banner banner--error" role="alert">
                  <span className="banner__icon" />
                  <span>{run.error.message}</span>
                </div>
              )}

              <div>
                <button className="btn btn--brass" type="submit" disabled={run.isPending}>
                  {run.isPending ? 'Reading the schema…' : 'Read the schema'}
                </button>
              </div>
            </div>
          </form>
        )}

        {result && <ImportResult result={result} onAgain={() => run.reset()} />}
      </div>
    </PageShell>
  )
}

// byNamespace groups the declarations the way they will be committed: one
// namespace per file, in the order the namespaces sort.
function byNamespace(entities: ImportedEntity[]): [string, ImportedEntity[]][] {
  const groups = new Map<string, ImportedEntity[]>()
  for (const e of entities) {
    const list = groups.get(e.namespace)
    if (list) list.push(e)
    else groups.set(e.namespace, [e])
  }
  return [...groups.entries()].sort(([a], [b]) => a.localeCompare(b))
}

function ImportResult({ result, onAgain }: { result: SchemaImportResult; onAgain: () => void }) {
  const groups = byNamespace(result.entities)

  return (
    <>
      <div className="card">
        <div className="card__head">
          <Database size={14} />
          <span className="card__title">
            {result.entities.length} table{result.entities.length === 1 ? '' : 's'} in{' '}
            {groups.length} namespace{groups.length === 1 ? '' : 's'} from {result.source}
          </span>
          <span className="spacer" style={{ flex: 1 }} />
          <button className="btn btn--ghost" type="button" onClick={onAgain}>
            Import another
          </button>
        </div>
      </div>

      {result.suggestions.length > 0 && (
        <div className="card">
          <div className="card__head">
            <Lightbulb size={14} />
            <span className="card__title">Worth changing</span>
          </div>
          <div className="card__body">
            <ul className="import__suggestions">
              {result.suggestions.map((s, i) => (
                <li key={`${s.table}-${s.kind}-${i}`}>
                  <div className="import__sughead">
                    <span className="import__table mono">{s.table}</span>
                    <span className="chip">{KIND_LABEL[s.kind] ?? s.kind}</span>
                  </div>
                  <p>{s.detail}</p>
                  {s.line && <pre className="import__line">{s.line}</pre>}
                </li>
              ))}
            </ul>
          </div>
        </div>
      )}

      {/* One card per namespace, because one namespace is one .atl file. */}
      {groups.map(([ns, entities]) => (
        <div className="card" key={ns}>
          <div className="card__head">
            <Database size={14} />
            <span className="card__title mono">{ns}</span>
            <span className="chip">
              {entities.length} table{entities.length === 1 ? '' : 's'}
            </span>
          </div>
          <div className="card__body">
            {entities.map(e => (
              <details key={e.table} className="import__entity">
                <summary>
                  <span className="import__table mono">{e.table}</span> → {e.entity}
                </summary>
                <pre className="import__atl">{e.atl}</pre>
              </details>
            ))}
          </div>
        </div>
      ))}

      {/*
        An index read but not named here makes the first plan propose creating
        one the table already has, and a table found but not declared is one
        no .atl accounts for.
      */}
      {(result.skipped.length > 0 || result.warnings.length > 0) && (
        <div className="card">
          <div className="card__head">
            <AlertTriangle size={14} />
            <span className="card__title">Read before committing</span>
          </div>
          <div className="card__body">
            <ul className="import__notes">
              {result.skipped.map((s, i) => <li key={`s${i}`}>{s}</li>)}
              {result.warnings.map((w, i) => <li key={`w${i}`}>{w}</li>)}
            </ul>
          </div>
        </div>
      )}
    </>
  )
}
