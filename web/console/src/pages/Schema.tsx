import { useMemo, useEffect, useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { useSearch, useNavigate } from '@tanstack/react-router'
// LinkSimple aliased, because @tanstack/react-router's Link is the other thing
// called that in this codebase and the JSX here reads clearer with the suffix.
import { ArrowSquareIn, Cube, LinkSimple as LinkIcon, ShieldChevron } from '@phosphor-icons/react'
import type { Blame, EntityOwnerEntry } from '@/api/client'
import { api, indexPlansByEntity, planClassBadge, planClassLabel, queries } from '@/api/client'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsList, TabsPanel, TabsTab } from '@/components/ui/tabs'
import { InputGroup, InputGroupAddon, InputGroupText } from '@/components/ui/input-group'
import { physicalTable } from '@/lib/physical'
import { Timestamp } from '@/components/Timestamp'
import { Actor } from '@/components/Actor'
import { PageShell } from '@/components/PageShell'
import { OnboardingDialog } from '@/components/OnboardingDialog'
import { onboardingDecision, takeOnboardingPending } from '@/lib/onboarding'
import { useMe } from '@/hooks/useAuth'

// ── IR shapes (mirror internal/dsl/ir.go) ──────────────────────────────────
interface IRFieldType {
  name: string
  array?: boolean
  elem?: IRFieldType
  vec_dim?: number
  len?: number
  num_p?: number
  num_s?: number
}
interface IRRef { entity?: string; namespace?: string; field?: string }
interface IRField {
  name: string
  type: IRFieldType
  primary?: boolean
  identity?: boolean
  serial?: boolean
  not_null?: boolean
  unique?: boolean
  ref?: IRRef
}
interface IRIndex { fields?: string[] }
interface IREntity { name: string; namespace: string; kind?: string; table_name?: string; fields: IRField[]; indexes?: IRIndex[] }
interface IRRoot { version?: number; entities?: IREntity[] }

interface FieldRow {
  name: string
  type: string
  flags: string
  ref: string
  pk: boolean
}

interface EntityDecl {
  id: string
  name: string
  namespace: string
  table: string
  caller: string
  introduced?: Blame
  lastModified?: Blame
  fields: FieldRow[]
  fks: { from: string; to: string; via: string }[]
}

function renderType(t: IRFieldType): string {
  if (t.array) return (t.elem ? renderType(t.elem) : 'text') + '[]'
  switch (t.name) {
    case 'varchar': return t.len ? `varchar(${t.len})` : 'varchar'
    case 'numeric': return t.num_p != null ? `numeric(${t.num_p}, ${t.num_s ?? 0})` : 'numeric'
    case 'vector':  return t.vec_dim ? `vector(${t.vec_dim})` : 'vector'
    default:        return t.name
  }
}

// The reference is returned separately from the other flags: it is the one
// that names another entity, and the field table draws it in --slate.
function flagsFor(f: IRField, indexedFields: Set<string>): { flags: string; ref: string } {
  const parts: string[] = []
  if (f.primary) parts.push('pk')
  if (f.identity) parts.push('identity')
  if (f.serial) parts.push('serial')
  if (f.unique) parts.push('unique')
  if (f.not_null) parts.push('not null')
  if (indexedFields.has(f.name)) parts.push('indexed')
  const ns = f.ref?.namespace ? `${f.ref.namespace}.` : ''
  return { flags: parts.join(', '), ref: f.ref?.entity ? `${ns}${f.ref.entity}` : '' }
}

function irToEntities(ir: IRRoot | null, owners: Record<string, EntityOwnerEntry>): EntityDecl[] {
  if (!ir?.entities) return []
  return ir.entities.map(e => {
    const id = `${e.namespace}.${e.name}`
    const indexedFields = new Set<string>()
    for (const idx of e.indexes ?? []) for (const f of idx.fields ?? []) indexedFields.add(f)
    const fields: FieldRow[] = (e.fields ?? []).map(f => {
      const { flags, ref } = flagsFor(f, indexedFields)
      return { name: f.name, type: renderType(f.type), flags, ref, pk: !!f.primary }
    })
    const fks = (e.fields ?? [])
      .filter(f => !!f.ref?.entity)
      .map(f => ({
        from: `${e.name}.${f.name}`,
        to:  `${f.ref!.namespace ?? e.namespace}.${f.ref!.entity}`,
        via: f.name,
      }))
    const own = owners[id]
    return {
      id, name: e.name, namespace: e.namespace,
      table: physicalTable(e.namespace, e.name, e.table_name),
      // Unattributed, not unknown. After the lineage fix a missing row is an
      // explicable state — a schema baselined before blame was recorded — and
      // not a value that went astray.
      caller: own?.introduced_by || 'unattributed',
      introduced: own?.introduced,
      lastModified: own?.last_modified,
      fields, fks,
    }
  }).sort((a, b) => a.id.localeCompare(b.id))
}

// The header reads as a sentence rather than a row of labelled numbers.
// "keyed on id" is the table's shape; "KEY 1" is a count of something the
// reader then has to go and find.
function fieldCount(e: EntityDecl): string {
  const n = e.fields.length
  return `${n} field${n === 1 ? '' : 's'}`
}

function keyPhrase(e: EntityDecl): string {
  const keys = e.fields.filter(f => f.pk).map(f => f.name)
  if (keys.length === 0) return 'no primary key'
  return `keyed on ${keys.join(', ')}`
}

function refPhrase(e: EntityDecl): string {
  const n = e.fks.length
  if (n === 0) return 'no references'
  return `${n} reference${n === 1 ? '' : 's'}`
}

// ── Schema page — design HTML 1:1 ──────────────────────────────────────────
export function Schema() {
  const navigate = useNavigate()
  const search = useSearch({ from: '/schema' })

  const { data: canonical, isLoading } = useQuery(queries.schemaCanonical())
  const { data: me } = useMe()
  const { data: ownersData } = useQuery(queries.entityOwners())

  const entities = useMemo(() => {
    if (!canonical) return []
    const owners: Record<string, EntityOwnerEntry> = {}
    for (const o of ownersData?.owners ?? []) owners[o.entity_id] = o
    return irToEntities(canonical.ir as IRRoot, owners)
  }, [canonical, ownersData])

  const namespaces = useMemo(
    () => [...new Set(entities.map(e => e.namespace))].sort(),
    [entities],
  )

  const selectedNS = search.namespace ?? namespaces[0] ?? ''
  const selectedEntityId = search.entity ?? ''
  const nsEntities = useMemo(
    () => entities.filter(e => e.namespace === selectedNS),
    [entities, selectedNS],
  )
  const selectedEntity = entities.find(e => e.id === selectedEntityId)

  // Blame is opt-in, the way every tool that has it gates it — GitHub behind a
  // tab, GitLab behind a panel, Sourcegraph behind a shortcut. None of them
  // makes it the default view of a file, because the attribution is wider than
  // the thing it annotates.
  const blame = search.blame ?? false
  const { data: lineageData } = useQuery({
    ...queries.entityLineage(selectedEntityId),
    enabled: blame && !!selectedEntityId,
  })
  // The versions that touched this table. Ten is a section, not a page — the
  // History screen is where the whole record lives.
  const { data: entityHistory } = useQuery({
    ...queries.historyList({ entityId: selectedEntityId, limit: 10 }),
    enabled: !!selectedEntityId,
  })

  const blameByField = useMemo(() => {
    const m = new Map<string, Blame>()
    for (const e of lineageData?.entries ?? []) {
      if (e.field_name && e.last_modified) m.set(e.field_name, e.last_modified)
    }
    return m
  }, [lineageData])

  // Everything waiting on a human, indexed by the entity it touches.
  //
  // One request for the whole queue rather than one per entity: the list is
  // bounded server-side, and each plan already names the entities it touches,
  // so a single pass indexes them all and the strip stays instant as the
  // selection moves.
  //
  // The query key is deliberately the one Approvals.tsx uses. That page
  // invalidates the ['schema-plans'] prefix after a decision, so approving
  // something there clears it from here too — rather than leaving this page
  // showing an approval that has already been granted.
  const { data: pendingPlans, isError: pendingUnavailable } = useQuery({
    queryKey: ['schema-plans', 'pending_approval'],
    queryFn: () => api.plans.list('pending_approval'),
  })

  const pendingByEntity = useMemo(
    () => indexPlansByEntity(pendingPlans?.plans),
    [pendingPlans],
  )

  const pendingHere = (selectedEntity && pendingByEntity.get(selectedEntity.id)) ?? []

  useEffect(() => {
    if (selectedNS && !selectedEntityId && nsEntities.length > 0) {
      navigate({ to: '/schema', search: { namespace: selectedNS, entity: nsEntities[0].id, blame } })
    }
  }, [selectedNS, selectedEntityId, nsEntities, navigate])

  const handleSelectNS = (ns: string) => {
    const first = entities.find(e => e.namespace === ns)
    navigate({ to: '/schema', search: { namespace: ns, entity: first?.id ?? '', blame } })
  }
  const handleSelectEntity = (id: string) =>
    navigate({ to: '/schema', search: { namespace: selectedNS, entity: id, blame } })

  // This page is read-only, and that is the design rather than a gap.
  //
  // It carried an EditPanel that composed a field edit and opened a GitHub pull
  // request with the result. Both the panel and the endpoints behind it are
  // gone: a hosted customer has no such repository wired to this console, and a
  // second authoring path beside their own git repo is a second place for the
  // schema to diverge from what is applied.
  //
  // What replaces it is not an editor. The pending-changes strip below shows
  // what is waiting on a human for the selected entity, which turns this page
  // from "browse a schema you cannot change" into "current state, plus
  // everything in flight" — the question an operator actually opens it to
  // answer.

  const ver = (canonical?.ir as IRRoot | undefined)?.version
  const sub = `${entities.length} entit${entities.length === 1 ? 'y' : 'ies'} · ${namespaces.length} namespace${namespaces.length === 1 ? '' : 's'}${ver ? ` · server v${String(ver).padStart(4, '0')}` : ''}`

  // An arrival from Cloud opens the flow once, and only for an organisation
  // that has neither answered it nor already got a schema.
  //
  // Three conditions, each covering what the others cannot:
  //
  // takeOnboardingPending clears the mark, so one sign-in opens it once rather
  // than on every return to this page.
  //
  // me.onboarded is the organisation's own answer, held by the server. It is
  // what stops the second admin being asked, and the same person on a second
  // machine.
  //
  // An organisation with entities has plainly done this already, whatever the
  // column says. That is what carries every organisation that existed before
  // the column did, with no backfill. Gated on the load as well, so a schema
  // that is still arriving does not read as an empty one.
  //
  // Which step it opens at is the difference between the two entry points: an
  // arrival is asked what it wants, and a button press already said.
  const [onboarding, setOnboarding] = useState<'welcome' | 'connect' | null>(null)
  useEffect(() => {
    const decision = onboardingDecision({
      loading: isLoading,
      onboarded: me?.onboarded,
      entities: entities.length,
    })
    if (decision === 'wait') return

    // The arrival is spent either way. Leaving it set on a settled
    // organisation would open the flow on the next page it visits.
    const arrived = takeOnboardingPending()
    if (decision === 'offer' && arrived) setOnboarding('welcome')
  }, [isLoading, me, entities.length])

  // Closing counts as answering. The flow stays reachable from the action
  // below while the schema is empty, so a dialog closed by accident is one
  // click away.
  const markDone = useMutation({ mutationFn: api.auth.onboardingDone })

  function closeOnboarding() {
    setOnboarding(null)
    if (me && !me.onboarded) markDone.mutate()
  }

  return (
    <PageShell
      title="Schema"
      sub={sub}
      flush
      // Only while there is nothing to browse. The action fills an empty
      // schema, and once one is loaded it offers to do what has been done.
      //
      // Gated on the load as well, so it does not appear and then withdraw
      // under the pointer on a schema that was there all along.
      action={
        !isLoading && entities.length === 0 ? (
          <button className="btn btn--brass" type="button" onClick={() => setOnboarding('connect')}>
            <ArrowSquareIn size={14} />
            Import a database
          </button>
        ) : undefined
      }
    >
    {onboarding && (
      <OnboardingDialog start={onboarding} onClose={closeOnboarding} />
    )}
    {/* Namespaces and tables are vertical tab rails: each is a mutually
        exclusive choice that swaps the pane beside it, and Base UI gives the
        rails arrow-key roving focus.

        One panel per rail, always bound to the current value, rather than one
        per tab. A panel per tab would mount 97 detail panes to show one. */}
    <Tabs
      className="schema"
      orientation="vertical"
      value={selectedNS}
      onValueChange={v => handleSelectNS(String(v))}
    >
      <div className="srail srail--ns">
        <div className="pane__head">
          <h3>Namespaces</h3>
          <span className="spacer" />
          <span className="chip chip--count">{namespaces.length}</span>
        </div>
        <div className="srail__scroll">
        {isLoading ? (
          <SkeletonRows />
        ) : (
          <TabsList variant="underline" className="srail__list">
            {namespaces.map(ns => {
              const count = entities.filter(e => e.namespace === ns).length
              return (
                <TabsTab key={ns} value={ns} className="srailtab">
                  <span className="srailtab__name" title={ns}>{ns}</span>
                  <span
                    className="chip chip--count"
                    title={`${count} ${count === 1 ? 'table' : 'tables'}`}
                  >
                    {count}
                  </span>
                </TabsTab>
              )
            })}
          </TabsList>
        )}
        </div>
      </div>

      <TabsPanel value={selectedNS} className="schema__rest">
        <Tabs
          className="schema__rest"
          orientation="vertical"
          value={selectedEntityId}
          onValueChange={v => handleSelectEntity(String(v))}
        >
          <div className="srail srail--ent">
            <div className="pane__head">
              <h3>Tables</h3>
            </div>
            <div className="srail__scroll">
            {isLoading ? (
              <SkeletonRows />
            ) : nsEntities.length === 0 ? (
              <div className="empty">
                {namespaces.length === 0 ? (
                  <>
                    <div className="empty__title">No schema yet</div>
                    <div className="empty__sub">
                      Import a database to generate declarations from its tables.
                    </div>
                    <button
                      className="btn btn--brass"
                      type="button"
                      onClick={() => setOnboarding('welcome')}
                    >
                      <ArrowSquareIn size={14} />
                      Import a database
                    </button>
                  </>
                ) : (
                  <>
                    <div className="empty__title">No tables</div>
                    <div className="empty__sub">The {selectedNS} namespace has no tables.</div>
                  </>
                )}
              </div>
            ) : (
              <TabsList variant="underline" className="srail__list">
                {nsEntities.map(e => (
                  <TabsTab key={e.id} value={e.id} className="srailtab">
                    <span className="srailtab__name" title={e.name}>{e.name}</span>
                    {/* Without a marker the queue is only findable by clicking
                        every table in the namespace, which for the operator
                        asking "is anything waiting on me" is the same as it
                        not being there. */}
                    {pendingByEntity.has(e.id) && (
                      <span className="dot dot--warn" title="Changes waiting for approval" />
                    )}
                    <span
                      className="chip chip--count"
                      title={`${e.fields.length} ${e.fields.length === 1 ? 'field' : 'fields'}`}
                    >
                      {e.fields.length}
                    </span>
                  </TabsTab>
                ))}
              </TabsList>
            )}
            </div>
          </div>

          <TabsPanel value={selectedEntityId} className="detail">
            {!selectedEntity ? (
              <div className="empty" style={{ height: '100%' }}>
                <div className="empty__icon">
                  <svg width="40" height="40" viewBox="0 0 40 40" fill="none">
                    {[8, 14, 20].map(r => (
                      <circle key={r} cx="20" cy="20" r={r} stroke="var(--ink-3)" strokeWidth="1" opacity={0.4} />
                    ))}
                  </svg>
                </div>
                <div className="empty__title">No table selected</div>
                <div className="empty__sub">
                  Pick a table to read its fields and foreign-key references.
                </div>
              </div>
            ) : (
              <div className="detail__doc">
                <header className="detail__head">
                  <h2 className="detail__path">
                    <span className="ns">{selectedEntity.namespace}.</span>
                    <span className="nm">{selectedEntity.name}</span>
                  </h2>
                  {/* The entity is what the API calls it; this is what psql
                      does. A reader moving between the two needs both. */}
                  <InputGroup className="tablepill">
                    <InputGroupAddon align="inline-start">
                      <InputGroupText>table</InputGroupText>
                    </InputGroupAddon>
                    <span className="tablepill__v">{selectedEntity.table}</span>
                  </InputGroup>
                  <p className="detail__facts">
                    <span>{fieldCount(selectedEntity)}</span>
                    <span className="sep">·</span>
                    <span>{keyPhrase(selectedEntity)}</span>
                    <span className="sep">·</span>
                    <span>{refPhrase(selectedEntity)}</span>
                    <span className="sep">·</span>
                    <span>owned by <span className="mono">{selectedEntity.caller}</span></span>
                    {selectedEntity.lastModified?.at && (() => {
                      return (
                        <>
                          <span className="sep">·</span>
                          <span>
                            last modified by{' '}
                            <Actor
                              of={selectedEntity.lastModified}
                              fallbackCaller={selectedEntity.caller}
                              suppressCaller={selectedEntity.caller}
                            />{' '}
                            <Timestamp at={selectedEntity.lastModified!.at} />
                          </span>
                        </>
                      )
                    })()}
                  </p>
                  <button
                    className="btn btn--ghost detail__action"
                    onClick={() => navigate({
                      to: '/sandbox',
                      search: { focus: `${selectedEntity.namespace}.${selectedEntity.name}`, boot: 'sim' },
                    })}
                    title="Open this table in a sandbox — sub-millisecond boot, fully isolated."
                  >
                    <Cube size={14} />
                    <span>Try in sandbox</span>
                  </button>
                </header>

                {/* Pending sits above Fields: an operator reading a column list
                    needs to know it is about to change before they read it. */}
                {pendingUnavailable ? (
                  // Not silently empty. "Nothing pending" and "could not ask"
                  // are different answers, and rendering the second as the
                  // first is how somebody concludes a table is quiet while a
                  // destructive plan sits in the queue against it.
                  <section className="sec">
                    <h3 className="sec__label">Pending changes</h3>
                    <p className="sec__note">
                      The approval queue did not load, so anything in flight for this
                      table is not shown. <button className="linkbtn" type="button" onClick={() => navigate({ to: '/approvals' })}>Open Approvals</button> to check.
                    </p>
                  </section>
                ) : pendingHere.length > 0 && (
                  <section className="sec">
                    <h3 className="sec__label">Pending changes</h3>
                    {pendingHere.map(p => (
                      <div key={p.plan_id} className="planrow">
                        {/* The PLAN's class, not a per-entity one. A plan is the
                            unit of approval, so its class is the rule that
                            governs — a narrower per-entity class here would
                            show a gentler badge than the thing an approver is
                            actually signing off. */}
                        <span className={`badge badge--${planClassBadge(planClassLabel(p.change_class))}`}>
                          {planClassLabel(p.change_class)}
                        </span>
                        <span className="planrow__id" title={p.plan_id}>
                          {p.plan_id.slice(0, 12)}
                        </span>
                        <span className="planrow__by">
                          by {p.requested_by || 'unknown'}
                          {p.expired ? ' · expired' : ''}
                        </span>
                        <span className="spacer" />
                        <button
                          className="btn btn--ghost"
                          onClick={() => navigate({ to: '/approvals' })}
                          title="Review this change on the Approvals page"
                        >
                          <ShieldChevron size={14} />
                          <span>Review</span>
                        </button>
                      </div>
                    ))}
                  </section>
                )}

                <section className="sec">
                  <h3 className="sec__label">
                    Fields
                    <button
                      type="button"
                      className={`blametoggle ${blame ? 'is-on' : ''}`}
                      aria-pressed={blame}
                      onClick={() => navigate({
                        to: '/schema',
                        search: { namespace: selectedNS, entity: selectedEntityId, blame: blame ? undefined : true },
                      })}
                    >
                      blame
                    </button>
                  </h3>
                  {/* The key gutter is the first column: a primary key and a
                      foreign key are what a reader scans a schema for, and
                      reading them off the left edge is one pass down the
                      table rather than one per row across it. */}
                  <Table variant="card" className="ftbl">
                    <TableHeader>
                      <TableRow>
                        <TableHead className="f-key"><span className="sr-only">Key</span></TableHead>
                        <TableHead>Field</TableHead>
                        <TableHead>Type</TableHead>
                        <TableHead>Constraints</TableHead>
                        {blame && <TableHead>Last modified</TableHead>}
                        {blame && <TableHead>When</TableHead>}
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {selectedEntity.fields.map(f => (
                        <TableRow key={f.name}>
                          <TableCell className="f-key">
                            {f.pk && <span className="mark mark--pk" title="Primary key">◆</span>}
                            {!f.pk && f.ref && <span className="mark mark--fk" title={`References ${f.ref}`}>→</span>}
                          </TableCell>
                          <TableCell className={`f-name ${f.pk ? 'is-pk' : ''}`}>{f.name}</TableCell>
                          <TableCell className="f-type">{f.type}</TableCell>
                          <TableCell className="f-flags">
                            {f.flags}
                            {f.flags && f.ref ? ' · ' : ''}
                            {f.ref && <span className="f-fk">{f.ref}</span>}
                          </TableCell>
                          {blame && (() => {
                            const b = blameByField.get(f.name)
                            // An em-dash, not "unknown": a field with no row
                            // predates blame rather than having lost it.
                            if (!b?.at) {
                              return (
                                <>
                                  <TableCell className="f-blame">—</TableCell>
                                  <TableCell className="f-when">—</TableCell>
                                </>
                              )
                            }
                            return (
                              <>
                                <TableCell className="f-blame">
                                  <Actor of={b} fallbackCaller={b.caller} />
                                </TableCell>
                                <TableCell className="f-when"><Timestamp at={b.at} /></TableCell>
                              </>
                            )
                          })()}
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </section>

                <section className="sec">
                  <h3 className="sec__label">References</h3>
                  {selectedEntity.fks.length === 0 ? (
                    <p className="sec__note">
                      No field on {selectedEntity.name} points at another table.
                    </p>
                  ) : (
                    <div className="fklist">
                      {selectedEntity.fks.map((f, i) => {
                        // A target outside the loaded schema leaves the row inert
                        // rather than navigating to a selection that renders empty.
                        const target = entities.find(x => x.id === f.to)
                        return (
                          <button
                            key={i}
                            type="button"
                            className="fkrow"
                            disabled={!target}
                            title={target ? `Open ${f.to}` : `${f.to} is not in this schema`}
                            onClick={() => target && navigate({
                              to: '/schema',
                              search: { namespace: target.namespace, entity: target.id, blame },
                            })}
                          >
                            <span className="fkrow__via">{f.via}</span>
                            <span className="fkrow__arrow"><LinkIcon size={13} /></span>
                            <span className="fkrow__to">{f.to}</span>
                          </button>
                        )
                      })}
                    </div>
                  )}
                </section>

                {(entityHistory?.versions?.length ?? 0) > 0 && (
                  <section className="sec">
                    <h3 className="sec__label">History</h3>
                    <div className="ehist">
                      {(entityHistory?.versions ?? []).map(v => {
                        const who = {
                          caller: v.caller,
                          actor: v.actor ?? '',
                          actor_email: v.actor_email ?? '',
                          actor_name: v.actor_name ?? '',
                        }
                        return (
                          <button
                            key={v.version}
                            type="button"
                            className="ehist__row"
                            onClick={() => navigate({ to: '/history' })}
                          >
                            <span className="ehist__v">v{String(v.version).padStart(4, '0')}</span>
                            <span className={`badge badge--${planClassBadge(planClassLabel(v.plan_class))}`}>
                              {planClassLabel(v.plan_class)}
                            </span>
                            <span className="ehist__who">
                              <Actor of={who} fallbackCaller={v.caller} />
                            </span>
                            <span className="spacer" />
                            <Timestamp at={v.created_at} className="ehist__when" />
                          </button>
                        )
                      })}
                    </div>
                  </section>
                )}
              </div>
            )}
          </TabsPanel>
        </Tabs>
      </TabsPanel>
    </Tabs>
    </PageShell>
  )
}
function SkeletonRows() {
  return (
    <>
      {[100, 85, 70, 90, 65].map((w, i) => (
        <div key={i} className="sk" style={{ height: 30, margin: 4, width: `${w}%` }} />
      ))}
    </>
  )
}

