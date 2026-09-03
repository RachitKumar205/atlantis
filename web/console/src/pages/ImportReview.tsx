import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useParams } from '@tanstack/react-router'
import { Database, Lightbulb, Stack, UploadSimple, Warning } from '@phosphor-icons/react'
import {
  api,
  planClassLabel,
  queries,
  type AdoptDrift,
  type ImportApplied,
  type ImportPlan,
  type ImportSuggestion,
} from '@/api/client'
import { PageShell } from '@/components/PageShell'
import { SudoConfirmDialog } from '@/pages/Settings'
import { FileBrowser, filesFor } from '@/components/CommitGuide'
import { Sql } from '@/components/Sql'
import { Accordion, AccordionItem, Alert, AlertDescription, AlertTitle } from '@/components/ui'
import { CodeView } from '@/components/Atl'

// What one import found, at the import's own URL.
//
// A screen and not a dialog: a pass over a real database returns 209 tables
// across 12 namespaces with 156 suggestions, and that is read, scrolled and
// come back to. The URL names the import, so a reload keeps it and a link
// hands it to somebody else.
//
// One request per section. The overview is counts and namespace names; the
// declarations and the findings are hundreds of kilobytes and are fetched by
// the section that draws them. The header appears while they are still coming.

type Section = 'commit' | 'tables' | 'suggestions' | 'notes'

// Each kind takes the badge its meaning already has elsewhere in the console:
// slate is what a foreign key is drawn in on the Schema page, and a table with
// no primary key cannot be addressed by a caller, which is the breaking pair.
const KIND: Record<string, { label: string; badge: string }> = {
  'tenant-isolation': { label: 'Tenant isolation', badge: 'badge--back' },
  'no-primary-key': { label: 'No primary key', badge: 'badge--break' },
  'unindexed-foreign-key': { label: 'Unindexed foreign key', badge: 'badge--fk' },
}

export function ImportReview() {
  const { id } = useParams({ from: '/imports/$id' })
  const overview = useQuery(queries.schemaImport(id))

  // Commit first. The declarations are what was read; committing them is what
  // the read was for, and it is the one thing here that is not reversible by
  // closing the tab.
  const [section, setSection] = useState<Section>('commit')

  if (overview.isPending) {
    return (
      <PageShell title="Imported schema" sub="Reading the import…">
        <div className="review">
          <div className="review__nav">
            {[0, 1, 2, 3].map(i => (
              <div key={i} className="skeleton skeleton--tab" />
            ))}
          </div>
          <div className="review__body">
            <div className="tally">
              {[0, 1, 2].map(i => (
                <div key={i} className="skeleton skeleton--stat" />
              ))}
            </div>
            <div className="skeleton skeleton--block" />
          </div>
        </div>
      </PageShell>
    )
  }

  if (overview.isError || !overview.data) {
    return (
      <PageShell title="Imported schema" sub="This import could not be read">
        <Alert variant="error">
          <AlertTitle>No such import</AlertTitle>
          <AlertDescription>
            {overview.error instanceof Error
              ? overview.error.message
              : 'It may have expired.'}{' '}
            Imports are kept for thirty days.
          </AlertDescription>
        </Alert>
      </PageShell>
    )
  }

  const v = overview.data
  const notesCount = v.skipped + v.warnings
  const sub =
    `${v.entities} ${plural(v.entities, 'table')} · ` +
    `${v.namespaces.length} ${plural(v.namespaces.length, 'namespace')} · ${v.source}`

  const NAV: { key: Section; label: string; icon: JSX.Element; count?: number }[] = [
    { key: 'commit', label: 'Commit', icon: <UploadSimple size={14} /> },
    { key: 'tables', label: 'Declarations', icon: <Stack size={14} />, count: v.entities },
    { key: 'suggestions', label: 'Worth changing', icon: <Lightbulb size={14} />, count: v.suggestions },
    { key: 'notes', label: 'Notes', icon: <Warning size={14} />, count: notesCount },
  ]

  return (
    <PageShell
      title="Imported schema"
      sub={sub}
      fill
      action={
        <Link
          to="/schema"
          search={{ namespace: undefined, entity: undefined }}
          className="btn btn--brass"
        >
          Done
        </Link>
      }
    >
      <div className="review">
        <nav className="review__nav" aria-label="Sections">
          {NAV.map(n => (
            <button
              key={n.key}
              type="button"
              className={`review__tab ${section === n.key ? 'is-active' : ''}`}
              onClick={() => setSection(n.key)}
              aria-current={section === n.key}
            >
              {n.icon}
              <span className="review__tablabel">{n.label}</span>
              {n.count !== undefined && <span className="badge badge--plain">{n.count}</span>}
            </button>
          ))}
        </nav>

        <div className="review__body">
          {/* The counts before the sections, so the one that wants acting on
              is visible without opening the tab that holds it. */}
          <div className="tally">
            <Stat n={v.entities} label={plural(v.entities, 'table')} />
            <Stat n={v.namespaces.length} label={plural(v.namespaces.length, 'namespace')} />
            <Stat
              n={v.suggestions}
              label={plural(v.suggestions, 'suggestion')}
              tone={v.suggestions > 0 ? 'warn' : undefined}
            />
          </div>

          {section === 'commit' && <CommitSection id={id} />}
          {section === 'tables' && <TablesSection id={id} namespaces={v.namespaces} />}
          {section === 'suggestions' && <SuggestionsSection id={id} count={v.suggestions} />}
          {section === 'notes' && <NotesSection id={id} count={notesCount} />}
        </div>
      </div>
    </PageShell>
  )
}

// CommitSection previews and applies an import: Commit computes the plan
// for review, Apply records the declarations as the baseline checkpoint.
// The apply runs no DDL — the imported tables already exist.
function CommitSection({ id }: { id: string }) {
  const { data, isPending, isError } = useQuery(queries.schemaImportEntities(id))
  const qc = useQueryClient()
  const [confirmApply, setConfirmApply] = useState(false)

  const plan = useMutation<ImportPlan, Error, void>({
    mutationFn: () => api.schemaImport.plan(id),
  })
  // Apply re-authenticates at Cloud first: it rewrites the caller's
  // checkpoint, and the route refuses without sudo.
  const apply = useMutation<ImportApplied, Error, string>({
    mutationFn: async (assertion: string) => {
      await api.auth.sudo(assertion)
      return api.schemaImport.apply(id)
    },
    onSuccess: () => {
      // The schema page reads the checkpoint this just rewrote.
      qc.invalidateQueries({ queryKey: ['schema'] })
      setConfirmApply(false)
    },
  })

  if (isPending) return <Loading what="the declarations" />
  if (isError || !data) return <Failed what="the declarations" />

  const p = plan.data
  const done = apply.data
  const blocked = [...(p?.parse_errors ?? []), ...(p?.custom_sql_errors ?? [])]
  const statements = p?.up_sql ? p.up_sql.split(';').filter(x => x.trim()).length : 0
  const reviewing = !!p && !done

  return (
    <div className="commit">
      {/* The plan replaces the files rather than sitting under them. At this
          point the question is what will run, and 183 statements beside 889
          lines of declarations gives neither enough room. */}
      {reviewing ? (
        <div className="planview">
          <div className="planview__head">
            <span className="planview__title">
              {statements} statement{statements === 1 ? '' : 's'}
            </span>
            <span className={`badge ${planClassBadgeClass(p.class)}`}>
              {planClassLabel(p.class ?? '')}
            </span>
            <span className="spacer" style={{ flex: 1 }} />
            <span className="planview__note">Nothing has run</span>
          </div>
          {blocked.length > 0 ? (
            <div className="planview__body">
              <Alert variant="error">
                <AlertTitle>These declarations will not apply</AlertTitle>
                <AlertDescription>
                  <ul className="import__notes">
                    {blocked.slice(0, 12).map((e, i) => (
                      <li key={i}>{e}</li>
                    ))}
                  </ul>
                </AlertDescription>
              </Alert>
            </div>
          ) : (
            <Sql className="planview__sql">{p.up_sql ?? ''}</Sql>
          )}
        </div>
      ) : (
        <FileBrowser files={filesFor(data.entities, data.header)} />
      )}

      {done && (
        <Alert variant={done.checkpoint_written ? 'success' : 'warning'}>
          <AlertTitle>
            {done.checkpoint_written
              ? done.already_adopted
                ? 'Already registered'
                : 'Atlantis is ready'
              : 'The declarations do not match the database'}
          </AlertTitle>
          <AlertDescription>
            {done.checkpoint_written ? (
              <>
                Atlantis holds this schema. Nothing was run against your
                database — the tables were already there. The Schema page shows
                them, and the next change plans against them.
              </>
            ) : (
              <>
                Nothing was written. Introspection found{' '}
                {done.drift?.length ?? 0} difference
                {(done.drift?.length ?? 0) === 1 ? '' : 's'}.
                {done.drift && done.drift.length > 0 && (
                  <DriftList drift={done.drift} />
                )}
              </>
            )}
          </AlertDescription>
        </Alert>
      )}

      {(plan.isError || apply.isError) && (
        <Alert variant="error">
          <AlertTitle>{plan.isError ? 'Could not plan' : 'Could not apply'}</AlertTitle>
          <AlertDescription>{(plan.error ?? apply.error)?.message}</AlertDescription>
        </Alert>
      )}

      <div className="commit__bar">
        <span className="commit__note">
          {done
            ? 'Applied.'
            : reviewing
              ? 'Applying records these declarations as the baseline. The statements above are not executed — the tables already exist.'
              : 'Shows the SQL the declarations correspond to. Nothing runs.'}
        </span>
        {reviewing && (
          <button
            className="btn btn--ghost"
            type="button"
            onClick={() => plan.reset()}
            disabled={apply.isPending}
          >
            Back
          </button>
        )}
        <button
          className="btn btn--brass"
          type="button"
          disabled={
            plan.isPending ||
            apply.isPending ||
            !!done ||
            (reviewing && blocked.length > 0)
          }
          onClick={() => (p ? setConfirmApply(true) : plan.mutate())}
        >
          {(plan.isPending || apply.isPending) && <span className="spin" />}
          {done
            ? 'Committed'
            : apply.isPending
              ? 'Applying…'
              : plan.isPending
                ? 'Planning…'
                : reviewing
                  ? 'Apply'
                  : 'Commit'}
        </button>
      </div>

      {confirmApply && p && (
        <SudoConfirmDialog
          title="Apply these declarations"
          icon={<UploadSimple />}
          body={
            <p>
              Atlantis will register this schema as the baseline —{' '}
              <strong>{statements} statement{statements === 1 ? '' : 's'}</strong> against
              the checkpoint every later plan compares to.
            </p>
          }
          confirmLabel="Apply"
          pending={apply.isPending}
          error={apply.error ? (apply.error as Error).message : null}
          onCancel={() => { apply.reset(); setConfirmApply(false) }}
          onConfirm={(assertion) => apply.mutate(assertion)}
        />
      )}
    </div>
  )
}

// planClassBadgeClass maps a plan class to the badge its severity already has
// elsewhere in the console.
function planClassBadgeClass(cls?: string): string {
  switch (cls) {
    case 'PLAN_CLASS_ADDITIVE':
      return 'badge--add'
    case 'PLAN_CLASS_BACKFILL_REQUIRED':
      return 'badge--back'
    case 'PLAN_CLASS_DESTRUCTIVE':
      return 'badge--destroy'
    case 'PLAN_CLASS_CROSS_CALLER_BREAKING':
      return 'badge--break'
    default:
      return 'badge--plain'
  }
}

// ── Declarations: one accordion per namespace, each fetched when opened ────
function TablesSection({
  id,
  namespaces,
}: {
  id: string
  namespaces: { name: string; tables: number }[]
}) {
  return (
    <Accordion>
      {namespaces.map(ns => (
        <AccordionItem
          key={ns.name}
          trigger={
            <>
              <Database size={13} />
              <span className="mono">{ns.name}</span>
            </>
          }
          meta={<span className="badge badge--plain">{ns.tables}</span>}
        >
          <NamespaceTables id={id} namespace={ns.name} />
        </AccordionItem>
      ))}
    </Accordion>
  )
}

// One namespace's declarations, fetched when its panel is first rendered.
//
// `details` mounts its panel only when open, so opening one namespace fetches
// one namespace rather than all of them.
function NamespaceTables({ id, namespace }: { id: string; namespace: string }) {
  const { data, isPending, isError } = useQuery(queries.schemaImportEntities(id, namespace))

  if (isPending) return <Loading what={namespace} />
  if (isError || !data) return <Failed what={namespace} />

  return (
    <div className="decls">
      {data.entities.map(e => (
        <details key={e.table} className="decl">
          <summary>
            <span className="mono">{e.table}</span>
            <span className="decl__arrow">→</span>
            <span className="decl__entity">{e.entity}</span>
          </summary>
          <CodeView body={e.atl} path=".atl" className="decl__code" />
        </details>
      ))}
    </div>
  )
}

function SuggestionsSection({ id, count }: { id: string; count: number }) {
  const { data, isPending, isError } = useQuery({
    ...queries.schemaImportNotes(id),
    enabled: count > 0,
  })

  if (count === 0) {
    return <Nothing title="Nothing to change" sub="No table wanted tightening." />
  }
  if (isPending) return <Loading what="the suggestions" />
  if (isError || !data) return <Failed what="the suggestions" />

  return <SuggestionList suggestions={data.suggestions} />
}

function NotesSection({ id, count }: { id: string; count: number }) {
  const { data, isPending, isError } = useQuery({
    ...queries.schemaImportNotes(id),
    enabled: count > 0,
  })

  if (count === 0) {
    return <Nothing title="Nothing to note" sub="Every table was read and declared." />
  }
  if (isPending) return <Loading what="the notes" />
  if (isError || !data) return <Failed what="the notes" />

  // An index read but not named here makes the first plan propose creating one
  // the table already has, and a table found but not declared is one no .atl
  // accounts for.
  const notes = [...data.skipped, ...data.warnings]
  return (
    <ul className="import__notes">
      {notes.map((n, i) => (
        <li key={i}>{n}</li>
      ))}
    </ul>
  )
}

// The drift report, grouped by what differs.
//
// Every row names its entity and column. Rendering the detail alone gives a
// list of identical verbs — "FK removed" twelve times says nothing about which
// twelve.
function DriftList({ drift }: { drift: AdoptDrift[] }) {
  const byKind = new Map<string, AdoptDrift[]>()
  for (const d of drift) {
    const k = d.kind ?? 'difference'
    const list = byKind.get(k)
    if (list) list.push(d)
    else byKind.set(k, [d])
  }

  return (
    <div className="drift">
      {[...byKind.entries()].map(([kind, rows]) => (
        <div className="drift__group" key={kind}>
          <div className="drift__head">
            <span className={`badge ${driftBadge(rows[0].severity)}`}>{driftLabel(kind)}</span>
            <span className="drift__count">
              {rows.length} column{rows.length === 1 ? '' : 's'}
            </span>
          </div>
          {/* The detail is the same sentence for every row in a group, so it
              is said once and the rows carry what differs between them. */}
          {rows[0].detail && <p className="drift__detail">{rows[0].detail}</p>}
          <ul className="drift__rows">
            {rows.slice(0, 40).map((d, i) => (
              <li key={i}>
                <span className="mono">{d.entity_id}</span>
                <span className="drift__dot">·</span>
                <span className="mono drift__field">{d.field}</span>
              </li>
            ))}
          </ul>
          {rows.length > 40 && (
            <p className="drift__more">and {rows.length - 40} more</p>
          )}
        </div>
      ))}
    </div>
  )
}

// The kinds adopt reports, as the thing that differs rather than the enum.
function driftLabel(kind: string): string {
  switch (kind) {
    case 'field_serial_removed':
      return 'Sequence not declared'
    case 'field_removed':
      return 'Column not declared'
    case 'field_reference_removed':
      return 'Foreign key not declared'
    case 'field_not_null_tightened':
      return 'NOT NULL only in the declaration'
    default:
      return kind.replace(/_/g, ' ')
  }
}

function driftBadge(severity?: string): string {
  switch (severity) {
    case 'removal':
      return 'badge--break'
    case 'mismatch':
      return 'badge--back'
    default:
      return 'badge--plain'
  }
}

function SuggestionList({ suggestions }: { suggestions: ImportSuggestion[] }) {
  return (
    <ul className="import__suggestions">
      {suggestions.map((s, i) => (
        <li key={`${s.table}-${s.kind}-${i}`}>
          <div className="import__sughead">
            <span className="import__table mono">{s.table}</span>
            <span className={`badge ${KIND[s.kind]?.badge ?? 'badge--plain'}`}>
              {KIND[s.kind]?.label ?? s.kind}
            </span>
          </div>
          <p>{s.detail}</p>
          {s.line && <pre className="import__line">{s.line}</pre>}
        </li>
      ))}
    </ul>
  )
}

// Names what is being fetched. A section that says only "Loading" cannot be
// told from one that has stopped.
function Loading({ what }: { what: string }) {
  return (
    <div className="loadingrow">
      <span className="spin" />
      <span>Reading {what}…</span>
    </div>
  )
}

function Failed({ what }: { what: string }) {
  return (
    <Alert variant="error">
      <AlertTitle>Could not read {what}</AlertTitle>
      <AlertDescription>Reload the page to try again.</AlertDescription>
    </Alert>
  )
}

function Stat({ n, label, tone }: { n: number; label: string; tone?: 'warn' }) {
  return (
    <div className={`stat ${tone ? `stat--${tone}` : ''}`}>
      <span className="stat__n">{n}</span>
      <span className="stat__label">{label}</span>
    </div>
  )
}

function Nothing({ title, sub }: { title: string; sub: string }) {
  return (
    <div className="empty">
      <div className="empty__title">{title}</div>
      <div className="empty__sub">{sub}</div>
    </div>
  )
}

function plural(n: number, word: string): string {
  return n === 1 ? word : `${word}s`
}
