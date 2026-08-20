import { describe, expect, it } from 'vitest'

import { QUIET, matrixFor, runsOf, viewBoxFor } from './qr'

const uri = 'otpauth://totp/Atlantis%20Cloud:you@example.com?secret=JBSWY3DPEHPK3PXP&issuer=Atlantis%20Cloud'

describe('matrixFor', () => {
  it('produces a square matrix', () => {
    const m = matrixFor(uri)
    expect(m.size).toBeGreaterThan(0)
    expect(m.rows).toHaveLength(m.size)
    for (const row of m.rows) expect(row).toHaveLength(m.size)
  })

  // The three finder patterns are what a camera locks onto, and they sit at the
  // very corners — which also asserts the matrix carries no margin of its own.
  // uqr adds a 1-module border by default; if that ever comes back, the quiet
  // zone is double-counted and the finder moves off (0,0).
  it('has a finder pattern at the very corner, with no built-in margin', () => {
    const m = matrixFor(uri)
    // Outer ring dark, inner ring light, centre block dark.
    expect(m.rows[0][0]).toBe(true)
    expect(m.rows[6][6]).toBe(true)
    expect(m.rows[1][1]).toBe(false)
    expect(m.rows[3][3]).toBe(true)
    // Top-right finder too, so this is not just one lucky corner.
    expect(m.rows[0][m.size - 1]).toBe(true)
  })
})

describe('runsOf', () => {
  // Merging horizontal neighbours is the whole reason this function exists:
  // one rect per module is ~size²/2 DOM nodes.
  it('merges neighbouring dark modules into one rect', () => {
    const m = { size: 4, rows: [
      [true, true, false, true],
      [false, false, false, false],
      [true, false, true, true],
      [true, true, true, true],
    ] }
    expect(runsOf(m)).toEqual([
      { x: 0, y: 0, w: 2 },
      { x: 3, y: 0, w: 1 },
      { x: 0, y: 2, w: 1 },
      { x: 2, y: 2, w: 2 },
      { x: 0, y: 3, w: 4 },
    ])
  })

  it('emits nothing for an empty matrix', () => {
    expect(runsOf({ size: 2, rows: [[false, false], [false, false]] })).toEqual([])
  })

  // Every dark module must appear in exactly one run, or the code is unreadable
  // in a way that looks like a camera problem.
  it('covers every dark module exactly once', () => {
    const m = matrixFor(uri)
    const covered = new Set<string>()
    for (const r of runsOf(m)) {
      for (let i = 0; i < r.w; i++) {
        const key = `${r.x + i},${r.y}`
        expect(covered.has(key)).toBe(false)
        covered.add(key)
      }
    }
    let dark = 0
    for (const row of m.rows) for (const cell of row) if (cell) dark++
    expect(covered.size).toBe(dark)
  })
})

describe('viewBoxFor', () => {
  // The quiet zone is part of the spec, not decoration: without it a reader
  // sitting on a coloured background often will not lock on at all.
  it('includes the quiet zone on both sides', () => {
    const m = { size: 21, rows: [] as boolean[][] }
    expect(viewBoxFor(m)).toBe(`${-QUIET} ${-QUIET} ${21 + QUIET * 2} ${21 + QUIET * 2}`)
  })

  // A negative origin is what lets the matrix draw at its own coordinates. If
  // it ever becomes "0 0", every rect needs a +QUIET offset and the margin
  // silently vanishes from two sides.
  it('starts at a negative origin so the matrix needs no offset', () => {
    expect(viewBoxFor({ size: 25, rows: [] })).toMatch(/^-\d+ -\d+ /)
  })
})
