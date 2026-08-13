import { describe, it, expect } from 'vitest'
import {
  CHANGE_CLASS_DISPLAY,
  PLAN_CLASS_BADGE,
  parseRawDiff,
  orderedBuckets,
  planClassBadge,
  planClassPreview,
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
  // A plan class reaches this console in two spellings. /api/plan lowercases
  // the proto enum to "cross_caller_breaking"; /api/history proxies the
  // schema_versions column through untouched, where the same class is written
  // "cross-caller-breaking" by codegen.ChangeClass.String().
  //
  // The pages matched the underscored form only, so every breaking and
  // backfill-required version in the history timeline rendered with the grey
  // "plain" badge — the styling that means nothing notable happened.
  it('accepts the underscored spelling from a live plan', () => {
    expect(planClassBadge('cross_caller_breaking')).toBe('break')
    expect(planClassBadge('backfill_required')).toBe('back')
  })

  it('accepts the hyphenated spelling from schema history', () => {
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

describe('planClassPreview', () => {
  it('has a line for every class a plan can come back as', () => {
    // Schema.tsx rendered three classes with `===` and nothing for the rest, so
    // a destructive preview showed the SQL that drops the column with no
    // verdict above it — which reads as a preview that found no problem.
    for (const cls of Object.keys(PLAN_CLASS_BADGE)) {
      expect(planClassPreview(cls), `${cls} has no preview line`).toBeDefined()
    }
  })

  it('accepts both spellings, like planClassBadge', () => {
    expect(planClassPreview('cross-caller-breaking')).toEqual(
      planClassPreview('cross_caller_breaking'),
    )
  })

  it('distinguishes destructive from breaking in the text a human reads', () => {
    expect(planClassPreview('destructive')?.text).not.toEqual(
      planClassPreview('cross_caller_breaking')?.text,
    )
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
