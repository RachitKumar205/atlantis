import { useMemo, useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useSearch, useNavigate } from '@tanstack/react-router'
// LinkSimple aliased, because @tanstack/react-router's Link is the other thing
// called that in this codebase and the JSX here reads clearer with the suffix.
import { ArrowSquareIn, Cube, LinkSimple as LinkIcon, ShieldChevron } from '@phosphor-icons/react'
import { api, indexPlansByEntity, planClassBadge, planClassLabel, queries } from '@/api/client'
import { PageShell } from '@/components/PageShell'
import { OnboardingDialog } from '@/components/OnboardingDialog'
import { takeOnboardingPending } from '@/lib/onboarding'

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
interface IREntity { name: string; namespace: string; kind?: string; fields: IRField[]; indexes?: IRIndex[] }
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
  caller: string
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

function irToEntities(ir: IRRoot | null, owners: Record<string, string>): EntityDecl[] {
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
    return { id, name: e.name, namespace: e.namespace, caller: owners[id] ?? 'unknown', fields, fks }
  }).sort((a, b) => a.id.localeCompare(b.id))
}

// ── Schema page — design HTML 1:1 ──────────────────────────────────────────
export function Schema() {
  const navigate = useNavigate()
  const search = useSearch({ from: '/schema' }) as { namespace?: string; entity?: string }

  const { data: canonical, isLoading } = useQuery(queries.schemaCanonical())
  const { data: ownersData } = useQuery(queries.entityOwners())

  const entities = useMemo(() => {
    if (!canonical) return []
    const owners: Record<string, string> = {}
    for (const o of ownersData?.owners ?? []) owners[o.entity_id] = o.introduced_by
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
      navigate({ to: '/schema', search: { namespace: selectedNS, entity: nsEntities[0].id } })
    }
  }, [selectedNS, selectedEntityId, nsEntities, navigate])

  const handleSelectNS = (ns: string) => {
    const first = entities.find(e => e.namespace === ns)
    navigate({ to: '/schema', search: { namespace: ns, entity: first?.id ?? '' } })
  }
  const handleSelectEntity = (id: string) =>
    navigate({ to: '/schema', search: { namespace: selectedNS, entity: id } })

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

  // Every arrival from Cloud opens the flow, whatever the organisation holds
  // and however many times it has been closed before. While onboarding is being
  // written, seeing it costs a sign-in; the conditions that will gate it are in
  // lib/onboarding.ts and nothing reads them yet.
  //
  // takeOnboardingPending clears the mark, so one sign-in opens it once rather
  // than on every return to this page.
  //
  // Which step it opens at is the difference between the two entry points: an
  // arrival is asked what it wants, and a button press already said.
  const [onboarding, setOnboarding] = useState<'welcome' | 'connect' | null>(null)
  useEffect(() => {
    if (takeOnboardingPending()) setOnboarding('welcome')
  }, [])

  function closeOnboarding() {
    setOnboarding(null)
  }

  return (
    <PageShell
      title="Schema"
      sub={sub}
      flush
      action={
        <button className="btn btn--brass" type="button" onClick={() => setOnboarding('connect')}>
          <ArrowSquareIn size={14} />
          Import a database
        </button>
      }
    >
    {onboarding && (
      <OnboardingDialog start={onboarding} onClose={closeOnboarding} />
    )}
    <div className="schema">
      {/* ── Namespace pane ── */}
      <div className="schema__pane">
        <div className="pane__head">
          <h3>Namespaces</h3>
          <span className="spacer" />
          <span className="chip">{namespaces.length}</span>
        </div>
        <div className="pane__list">
          {isLoading ? (
            <SkeletonRows />
          ) : (
            namespaces.map(ns => {
              const count = entities.filter(e => e.namespace === ns).length
              return (
                <button
                  key={ns}
                  type="button"
                  className="nsrow"
                  aria-current={ns === selectedNS ? 'true' : undefined}
                  onClick={() => handleSelectNS(ns)}
                >
                  <span className="nsrow__name" title={ns}>{ns}</span>
                  <span className="chip chip--count" title={`${count} ${count === 1 ? 'entity' : 'entities'}`}>
                    {count}
                  </span>
                </button>
              )
            })
          )}
        </div>
      </div>

      {/* ── Entity pane ── */}
      <div className="schema__pane">
        <div className="pane__head">
          <h3>{selectedNS || 'Entities'}</h3>
        </div>
        <div className="pane__list">
          {isLoading ? (
            <SkeletonRows />
          ) : nsEntities.length === 0 ? (
            <div className="empty">
              {/* With no namespaces there is nothing to pick, so the empty
                  pane names the one action that fills it. */}
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
                  <div className="empty__title">No entities</div>
                  <div className="empty__sub">The {selectedNS} namespace has no entities.</div>
                </>
              )}
            </div>
          ) : (
            nsEntities.map(e => (
              <button
                key={e.id}
                type="button"
                className="entrow"
                aria-current={e.id === selectedEntityId ? 'true' : undefined}
                onClick={() => handleSelectEntity(e.id)}
              >
                <span className="entrow__name" title={e.name}>{e.name}</span>
                {/* Without a marker here the strip is only findable by
                    clicking through every entity in the namespace, which for
                    the operator asking "is anything waiting on me" is the same
                    as it not being there. */}
                {pendingByEntity.has(e.id) && (
                  <span
                    className="badge badge--warn"
                    title="This entity has changes waiting for approval"
                  >
                    pending
                  </span>
                )}
                <span
                  className="chip chip--count"
                  title={`${e.fields.length} ${e.fields.length === 1 ? 'field' : 'fields'}`}
                >
                  {e.fields.length}
                </span>
              </button>
            ))
          )}
        </div>
      </div>

      {/* ── Detail pane ── */}
      <div className="schema__pane">
        {!selectedEntity ? (
          <div className="empty" style={{ height: '100%' }}>
            <div className="empty__icon">
              <svg width="40" height="40" viewBox="0 0 40 40" fill="none">
                {[8, 14, 20].map(r => (
                  <circle key={r} cx="20" cy="20" r={r} stroke="var(--ink-3)" strokeWidth="1" opacity={0.4} />
                ))}
              </svg>
            </div>
            <div className="empty__title">No entity selected</div>
            <div className="empty__sub">
              Pick an entity to read its fields, owner and foreign-key references.
            </div>
          </div>
        ) : (
          <div className="detail">
            <div className="detail__head">
              <div className="row" style={{ alignItems: 'flex-start' }}>
                <div>
                  <div className="detail__path mono">
                    <span className="ns">{selectedEntity.namespace}.</span>
                    <span className="nm">{selectedEntity.name}</span>
                  </div>
                  <div className="detail__meta">
                    <div className="metaitem">
                      <span className="metaitem__l">owner caller</span>
                      <span className="metaitem__v">{selectedEntity.caller}</span>
                    </div>
                    <div className="metaitem">
                      <span className="metaitem__l">fields</span>
                      <span className="metaitem__v num">{selectedEntity.fields.length}</span>
                    </div>
                    <div className="metaitem">
                      <span className="metaitem__l">schema</span>
                      <span className="metaitem__v">v0048</span>
                    </div>
                  </div>
                </div>
                <div className="spacer" />
                {/* Phase 2 contextual entry: jump straight to /sandbox
                    with this entity focused. /sandbox auto-boots a sim
                    sandbox via ?boot=sim when the user has none, and
                    pre-fills the Inspect tab via ?focus. */}
                <button
                  className="btn btn--ghost"
                  onClick={() => navigate({
                    to: '/sandbox',
                    search: { focus: `${selectedEntity.namespace}.${selectedEntity.name}`, boot: 'sim' },
                  })}
                  title="Open this entity in a sandbox — sub-millisecond boot, fully isolated."
                >
                  <Cube size={14} />
                  <span>Try in sandbox</span>
                </button>
              </div>
            </div>

            <div className="detail__body">
              {/* Pending changes.
                  Above Fields on purpose: an operator reading a column list
                  needs to know it is about to change before they read it. */}
              {pendingUnavailable ? (
                // Not silently empty. "No pending changes" and "could not ask"
                // are different answers, and rendering the second as the first
                // is how somebody concludes a table is quiet while a
                // destructive plan sits in the queue against it.
                <div className="detail__sec">
                  <div className="detail__seclabel">
                    <h4>Pending changes</h4>
                    <div className="line" />
                  </div>
                  <div className="muted" style={{ fontSize: 12 }}>
                    Could not load the approval queue, so anything in flight for this
                    entity is not shown here. Open Approvals to check.
                  </div>
                </div>
              ) : pendingHere.length > 0 && (
                <div className="detail__sec">
                  <div className="detail__seclabel">
                    <h4>Pending changes</h4>
                    <div className="line" />
                    <span className="chip chip--count">{pendingHere.length}</span>
                  </div>
                  {pendingHere.map(p => (
                    <div key={p.plan_id} className="planrow">
                      {/* The PLAN's class, not a per-entity one. A plan is the
                          unit of approval, so its class is the rule that
                          governs — inventing a narrower per-entity class here
                          would show a gentler badge than the thing an approver
                          is actually signing off. */}
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
                </div>
              )}

              {/* Fields */}
              <div className="detail__sec">
                <div className="detail__seclabel">
                  <h4>Fields</h4>
                  <div className="line" />
                </div>
                <table className="ftbl">
                  <tbody>
                    {selectedEntity.fields.map(f => (
                      <tr key={f.name}>
                        <td className={`f-name ${f.pk ? 'f-pk' : ''}`}>
                          {f.name}
                          {f.pk && <span className="pkmark" aria-hidden="true">◆</span>}
                        </td>
                        <td className="f-type">{f.type}</td>
                        <td className="f-flags">
                          {f.flags}
                          {f.flags && f.ref ? ', ' : ''}
                          {f.ref && <span className="f-fk">→ {f.ref}</span>}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>

              {/* Owners */}
              <div className="detail__sec">
                <div className="detail__seclabel">
                  <h4>Owners</h4>
                  <div className="line" />
                </div>
                <div className="row" style={{ flexWrap: 'wrap', gap: 8 }}>
                  <span className="owner">
                    <span className="dot" />
                    {selectedEntity.caller}
                  </span>
                </div>
              </div>

              {/* FK references */}
              {selectedEntity.fks.length > 0 && (
                <div className="detail__sec">
                  <div className="detail__seclabel">
                    <h4>FK references</h4>
                    <div className="line" />
                    <span className="badge badge--fk">
                      {selectedEntity.fks.length} link{selectedEntity.fks.length > 1 ? 's' : ''}
                    </span>
                  </div>
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
                            search: { namespace: target.namespace, entity: target.id },
                          })}
                        >
                          <span className="fkrow__arrow"><LinkIcon size={14} /></span>
                          <span className="fkrow__from">{f.from}</span>
                          <span className="fkrow__arrow">→</span>
                          <span className="fkrow__to">{f.to}</span>
                          <span className="fkrow__via">via {f.via}</span>
                        </button>
                      )
                    })}
                  </div>
                </div>
              )}

              {/* Edit panel removed — schema editing is currently dormant.
                  Restore alongside the edit button + editOpen state.
              {editOpen && (
                <EditPanel
                  entity={selectedEntity}
                  onClose={() => setEditOpen(false)}
                />
              )}
              */}
            </div>
          </div>
        )}
      </div>
    </div>
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

