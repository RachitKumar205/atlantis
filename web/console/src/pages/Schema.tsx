import { useMemo, useEffect, useState } from 'react'
import { useQuery, useMutation } from '@tanstack/react-query'
import { useSearch, useNavigate } from '@tanstack/react-router'
import { Box, Pencil, Link as LinkIcon, GitPullRequest } from 'lucide-react'
import { queries, api, type EditPreviewResponse } from '@/api/client'
import { PageShell } from '@/components/PageShell'
import { Sql } from '@/components/Sql'

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
  pk: boolean
  fk: boolean
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

function flagsFor(f: IRField, indexedFields: Set<string>): string {
  const parts: string[] = []
  if (f.primary) parts.push('pk')
  if (f.identity) parts.push('identity')
  if (f.serial) parts.push('serial')
  if (f.unique) parts.push('unique')
  if (f.not_null) parts.push('not null')
  if (f.ref?.entity) parts.push(`fk → ${f.ref.namespace ?? ''}${f.ref.namespace ? '.' : ''}${f.ref.entity}`)
  if (indexedFields.has(f.name)) parts.push('indexed')
  return parts.join(', ')
}

function irToEntities(ir: IRRoot | null, owners: Record<string, string>): EntityDecl[] {
  if (!ir?.entities) return []
  return ir.entities.map(e => {
    const id = `${e.namespace}.${e.name}`
    const indexedFields = new Set<string>()
    for (const idx of e.indexes ?? []) for (const f of idx.fields ?? []) indexedFields.add(f)
    const fields: FieldRow[] = (e.fields ?? []).map(f => ({
      name: f.name,
      type: renderType(f.type),
      flags: flagsFor(f, indexedFields),
      pk: !!f.primary,
      fk: !!f.ref?.entity,
    }))
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

  // Schema editing is dormant — the Edit button and EditPanel are kept
  // in this file as commented blocks below so the wiring can be restored
  // when the editing surface is brought back. Backend endpoints
  // (api.schemaEdit.preview / openPR) and the corresponding admin RPC
  // are unaffected — only the UI access is removed.

  const ver = (canonical?.ir as IRRoot | undefined)?.version
  const sub = `${entities.length} entit${entities.length === 1 ? 'y' : 'ies'} · ${namespaces.length} namespace${namespaces.length === 1 ? '' : 's'}${ver ? ` · server v${String(ver).padStart(4, '0')}` : ''}`

  return (
    <PageShell title="Schema" sub={sub} flush>
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
                <div
                  key={ns}
                  className={`nsrow ${ns === selectedNS ? 'is-active' : ''}`}
                  onClick={() => handleSelectNS(ns)}
                >
                  <span className="nsrow__name">{ns}</span>
                  <span className="chip chip--count">{count}</span>
                </div>
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
              <div className="empty__title">No entities</div>
              <div className="empty__sub">{selectedNS ? 'This namespace has no entities yet.' : 'Pick a namespace.'}</div>
            </div>
          ) : (
            nsEntities.map(e => (
              <div
                key={e.id}
                className={`entrow ${e.id === selectedEntityId ? 'is-active' : ''}`}
                onClick={() => handleSelectEntity(e.id)}
              >
                <span className="entrow__name">{e.name}</span>
                <span className="chip chip--count">{e.fields.length}</span>
              </div>
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
            <div className="empty__title">Select an entity to inspect</div>
            <div className="empty__sub">Choose a namespace and entity from the panes on the left.</div>
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
                  <Box size={14} />
                  <span>Try in sandbox</span>
                </button>
              </div>
            </div>

            <div className="detail__body">
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
                          {f.name}{f.pk ? ' ◆' : ''}
                        </td>
                        <td className="f-type">{f.type}</td>
                        <td className="f-flags">{f.flags}</td>
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
                    {selectedEntity.fks.map((f, i) => (
                      <div key={i} className="fkrow">
                        <span className="fkrow__arrow"><LinkIcon size={14} /></span>
                        <span className="fkrow__from">{f.from}</span>
                        <span className="fkrow__arrow">→</span>
                        <span className="fkrow__to">{f.to}</span>
                        <span className="fkrow__via">via {f.via}</span>
                      </div>
                    ))}
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

// Inline edit panel — design's .editpanel with a preview .plan block.
function EditPanel({ entity, onClose }: { entity: EntityDecl; onClose: () => void }) {
  const [op, setOp] = useState<'add' | 'replace' | 'remove'>('add')
  const [field, setField] = useState('')
  const [fieldText, setFieldText] = useState('')
  const [preview, setPreview] = useState<EditPreviewResponse | null>(null)

  const previewM = useMutation({
    mutationFn: () =>
      api.schemaEdit.preview({
        namespace: entity.namespace,
        entity: entity.name,
        op,
        field,
        field_text: fieldText,
      }),
    onSuccess: setPreview,
  })

  const prM = useMutation({
    mutationFn: () =>
      api.schemaEdit.openPR({
        namespace: entity.namespace,
        entity: entity.name,
        op,
        field,
        field_text: fieldText,
        title: `schema: ${op} ${entity.namespace}.${entity.name}.${field}`,
      }),
  })

  return (
    <div className="editpanel">
      <div className="editpanel__head">
        <Pencil size={17} />
        <h4>Edit {entity.namespace}.{entity.name}</h4>
        <span className="badge badge--plain" style={{ marginLeft: 'auto' }}>draft</span>
      </div>
      <div className="editpanel__body">
        <div className="row" style={{ gap: 12, marginBottom: 14, flexWrap: 'wrap' }}>
          <div className="seg">
            {(['add', 'replace', 'remove'] as const).map(o => (
              <button key={o} className={op === o ? 'is-active' : ''} onClick={() => setOp(o)}>
                {o}
              </button>
            ))}
          </div>
          <input
            className="input--boxed"
            placeholder="field name"
            value={field}
            onChange={e => setField(e.target.value)}
            style={{ minWidth: 160 }}
          />
          {op !== 'remove' && (
            <input
              className="input--boxed mono"
              placeholder="varchar(120) not null default ''"
              value={fieldText}
              onChange={e => setFieldText(e.target.value)}
              style={{ flex: 1, minWidth: 240 }}
            />
          )}
        </div>

        <div className="section-label" style={{ marginBottom: 10 }}>tide plan — impact preview</div>
        <div className="plan">
          {!preview && (
            <div className="pl-mut">
              Type a field, then click <span className="brass">Preview</span> to compute the
              impact via <span className="mono">PlanSchema</span>.
            </div>
          )}
          {preview && (
            <>
              {preview.plan_class === 'additive' && (
                <div><span className="pl-add">+ additive</span> · {preview.impact?.length ?? 0} change(s)</div>
              )}
              {preview.plan_class === 'backfill_required' && (
                <div><span className="pl-back">~ backfill required</span></div>
              )}
              {preview.plan_class === 'cross_caller_breaking' && (
                <div><span className="pl-break">✗ breaking — blocks merge</span></div>
              )}
              {preview.up_sql && (
                <Sql style={{ marginTop: 10 }}>{preview.up_sql.slice(0, 600)}</Sql>
              )}
            </>
          )}
        </div>

        <div className="row" style={{ marginTop: 16, justifyContent: 'flex-end', gap: 8 }}>
          <button className="btn" onClick={onClose}>Discard</button>
          <button
            className="btn"
            onClick={() => previewM.mutate()}
            disabled={!field || previewM.isPending}
          >
            {previewM.isPending ? 'Previewing…' : 'Preview'}
          </button>
          <button
            className="btn btn--brass"
            disabled={!preview || preview.plan_class === 'unparseable' || prM.isPending}
            onClick={() => prM.mutate()}
          >
            <GitPullRequest size={14} />
            <span>{prM.isPending ? 'Opening…' : 'Open PR on GitHub'}</span>
          </button>
        </div>

        {prM.isError && (
          <div className="banner banner--error" style={{ marginTop: 12 }}>
            {(prM.error as Error).message}
          </div>
        )}
        {prM.data && (
          <div className="banner banner--ok" style={{ marginTop: 12 }}>
            <a className="brass mono" href={prM.data.pr_url} target="_blank" rel="noreferrer">
              {prM.data.pr_url}
            </a>
          </div>
        )}
      </div>
    </div>
  )
}
