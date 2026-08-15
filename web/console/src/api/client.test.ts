import { describe, it, expect } from 'vitest'
import {
  CHANGE_CLASS_DISPLAY,
  indexPlansByEntity,
  parseRawDiff,
  orderedBuckets,
  planClassBadge,
  planClassLabel,
  type SchemaPlanSummary,
} from './client'

// The console's half of "a change class must never render as nothing".
//
// These are the first tests in this package, and they exist because the Go side
// of this property is guarded by four tests while the browser side was guarded
// by the type-checker — which cannot see that a lookup missed, only that it was
// spelled correctly. Every defect below shipped and none of them failed a build.

describe('parseRawDiff', () => {
  it('keeps a bucket the console has never heard of', () => {
    // The whole point. "destructive" was exactly this case: the server emitted
    // it, the console named three buckets, and a version that dropped a column
    // rendered as an empty diff.
    const parsed = parseRawDiff({
      additive: [{ entity_id: 'a.B' }],
      some_future_class: [{ entity_id: 'a.C' }],
    })
    expect(Object.keys(parsed).sort()).toEqual(['additive', 'some_future_class'])
    expect(parsed.some_future_class).toHaveLength(1)
  })

  it('drops values that are not arrays', () => {
    // The payload is buckets of changes. A scalar under an unexpected key must
    // not become a bucket with junk in it.
    const parsed = parseRawDiff({ additive: [], ir_hash: 'sha256:abc', count: 3 })
    expect(Object.keys(parsed)).toEqual(['additive'])
  })

  it('survives a payload that is not an object', () => {
    for (const raw of [null, undefined, 'string', 42, []]) {
      expect(() => parseRawDiff(raw)).not.toThrow()
    }
    expect(parseRawDiff(null)).toEqual({})
  })
})

describe('planClassBadge', () => {
  // Plan classes arrive hyphenated — every one is proxied from the admin
  // server, where codegen.ChangeClass.String() writes "cross-caller-breaking".
  // The lookup table is keyed with underscores, and the pages used to compare
  // with === against that form, so every breaking and backfill-required version
  // in the history timeline rendered with the grey "plain" badge: the styling
  // that means nothing notable happened.
  it('accepts the underscored spelling the table is keyed with', () => {
    expect(planClassBadge('cross_caller_breaking')).toBe('break')
    expect(planClassBadge('backfill_required')).toBe('back')
  })

  it('accepts the hyphenated spelling the server actually sends', () => {
    expect(planClassBadge('cross-caller-breaking')).toBe('break')
    expect(planClassBadge('backfill-required')).toBe('back')
  })

  it('gives destructive its own badge, not breaking’s and not plain', () => {
    expect(planClassBadge('destructive')).toBe('destroy')
    expect(planClassBadge('destructive')).not.toBe(planClassBadge('cross_caller_breaking'))
  })

  it('falls back to plain for a class this console predates', () => {
    // Degrading to a grey badge is acceptable; the class name is rendered
    // beside it. Returning undefined would produce `badge--undefined` and no
    // styling at all.
    expect(planClassBadge('some_future_class')).toBe('plain')
    expect(planClassBadge('')).toBe('plain')
  })
})

describe('CHANGE_CLASS_DISPLAY', () => {
  // Mirrors changeClassDisplay in cmd/tide/diff.go. The two are not generated
  // from one source, so this at least keeps the console's copy internally
  // coherent; TestEveryChangeClassIsDisplayable guards the Go one.
  it('covers every bucket codegen.Diff can emit', () => {
    expect(CHANGE_CLASS_DISPLAY.map(c => c.bucket)).toEqual([
      'additive',
      'backfill_required',
      'breaking',
      'destructive',
    ])
  })

  it('gives each class a distinct glyph', () => {
    const glyphs = CHANGE_CLASS_DISPLAY.map(c => c.glyph)
    expect(new Set(glyphs).size).toBe(glyphs.length)
  })

  it('gives each class a distinct badge, so severity is not collapsed', () => {
    const badges = CHANGE_CLASS_DISPLAY.map(c => c.badge)
    expect(new Set(badges).size).toBe(badges.length)
  })
})

describe('orderedBuckets', () => {
  it('lists known classes in severity order, worst last', () => {
    const got = orderedBuckets({
      breaking: [{ entity_id: 'a.B', field: '', detail: '' }],
      additive: [{ entity_id: 'a.C', field: '', detail: '' }],
      destructive: [{ entity_id: 'a.D', field: '', detail: '' }],
    })
    expect(got.map(b => b.bucket)).toEqual(['additive', 'breaking', 'destructive'])
  })

  it('omits buckets that are present but empty', () => {
    const got = orderedBuckets({ additive: [], breaking: [{ entity_id: 'a.B', field: '', detail: '' }] })
    expect(got.map(b => b.bucket)).toEqual(['breaking'])
  })

  it('renders an unknown bucket last rather than dropping it', () => {
    // The property that makes the fix structural: a future class shows up under
    // its own name with a neutral glyph, instead of the version rendering as
    // though it contained nothing.
    const got = orderedBuckets({
      additive: [{ entity_id: 'a.C', field: '', detail: '' }],
      some_future_class: [{ entity_id: 'a.E', field: '', detail: '' }],
    })
    expect(got.map(b => b.bucket)).toEqual(['additive', 'some_future_class'])
    expect(got[1].badge).toBe('plain')
  })
})

// The Schema page's pending-changes strip. Same property as everything above:
// the failure is not an exception, it is an entity rendering as quiet while a
// change waits against it.

const plan = (id: string, entities: string[]): SchemaPlanSummary => ({
  plan_id: id,
  caller: 'shop',
  change_class: 'PLAN_CLASS_DESTRUCTIVE',
  state: 'pending_approval',
  requested_by: 'shop',
  created_at: '2026-08-15T00:00:00Z',
  approver_role: 'admin',
  entity_ids: entities,
})

describe('indexPlansByEntity', () => {
  it('files one plan under every entity it touches', () => {
    // A plan changing two entities has to appear on both. Filing it only under
    // the first would leave the second looking untouched while an approval
    // that changes it sits in the queue.
    const idx = indexPlansByEntity([plan('p1', ['shop.Order', 'shop.Line'])])
    expect(idx.get('shop.Order')?.map(p => p.plan_id)).toEqual(['p1'])
    expect(idx.get('shop.Line')?.map(p => p.plan_id)).toEqual(['p1'])
  })

  it('keeps every plan touching the same entity', () => {
    // Two plans against one entity is the case an operator most needs to see,
    // and the one a Map assignment rather than an append would collapse.
    const idx = indexPlansByEntity([
      plan('p1', ['shop.Order']),
      plan('p2', ['shop.Order']),
    ])
    expect(idx.get('shop.Order')?.map(p => p.plan_id)).toEqual(['p1', 'p2'])
  })

  it('reports nothing for an entity no plan touches', () => {
    const idx = indexPlansByEntity([plan('p1', ['shop.Order'])])
    expect(idx.get('shop.Untouched')).toBeUndefined()
  })

  it('survives a plan the server sent without entity_ids', () => {
    // entity_ids is absent when the server could not decode a stored diff. The
    // plan is still real; it simply cannot be filed under an entity, and that
    // must not take the whole strip down with it.
    const partial = { ...plan('p1', []), entity_ids: undefined }
    const idx = indexPlansByEntity([partial, plan('p2', ['shop.Order'])])
    expect(idx.get('shop.Order')?.map(p => p.plan_id)).toEqual(['p2'])
  })

  it('survives no plans at all', () => {
    expect(indexPlansByEntity(undefined).size).toBe(0)
    expect(indexPlansByEntity([]).size).toBe(0)
  })
})

describe('planClassLabel', () => {
  it('strips the proto prefix so the badge lookup can hit', () => {
    // These two run together on every surface that shows a class. The label
    // must reach planClassBadge in the spelling its table is keyed on, or a
    // destructive change renders in the grey badge that means "unremarkable".
    expect(planClassLabel('PLAN_CLASS_CROSS_CALLER_BREAKING')).toBe('cross-caller-breaking')
    expect(planClassBadge(planClassLabel('PLAN_CLASS_DESTRUCTIVE'))).toBe('destroy')
    expect(planClassBadge(planClassLabel('PLAN_CLASS_BACKFILL_REQUIRED'))).toBe('back')
    expect(planClassBadge(planClassLabel('PLAN_CLASS_ADDITIVE'))).toBe('add')
  })

  it('leaves an already-stripped class alone', () => {
    // The history endpoint serves the hyphenated spelling with no prefix.
    // Running the label over it a second time must not corrupt it.
    expect(planClassLabel('cross-caller-breaking')).toBe('cross-caller-breaking')
  })
})
