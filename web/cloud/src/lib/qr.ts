import { encode } from 'uqr'

// A QR code as inline SVG, and inline SVG specifically.
//
// The obvious implementations both need a CSP allowance this origin does not
// give: a <canvas> read back with toDataURL() needs `img-src data:`, and an
// <img src="data:image/svg+xml,…"> needs the same. Rectangles in an <svg>
// element need nothing at all — they are just markup — so the tightest policy
// is also the simplest thing to draw.
//
// The rects carry no fill attribute. Colour comes from CSS (`.qr rect`), which
// keeps this out of the way of `style-src 'self'`: an inline `fill="#fff"` is a
// presentation attribute rather than a style attribute and would be allowed,
// but a theme that cannot be changed from CSS is worse than one that can.

export type QRMatrix = {
  /** Modules per side, excluding the quiet zone. */
  size: number
  /** Row-major, true where a module is dark. */
  rows: boolean[][]
}

/** QUIET is the mandatory light margin, in modules. Four is the spec minimum. */
export const QUIET = 4

/**
 * matrixFor encodes text as a bare QR matrix, with no margin.
 *
 * `border: 0` on purpose. uqr defaults to a 1-module border, which is both
 * below the 4 the spec requires and invisible in the returned matrix — it just
 * makes `size` larger and shifts the finder pattern off (0,0). Taking the bare
 * code and adding the quiet zone in the viewBox keeps the margin explicit,
 * correct, and in one place.
 */
export function matrixFor(text: string): QRMatrix {
  const r = encode(text, { border: 0 })
  return { size: r.size, rows: r.data as boolean[][] }
}

/**
 * runsOf collapses each row into horizontal runs of dark modules.
 *
 * One <rect> per dark module is correct and produces roughly `size²/2`
 * elements — around 700 for a typical otpauth URI. Merging horizontally
 * neighbouring modules into a single wide rect cuts that by about two thirds
 * with identical output, which matters because this is rendered by React into
 * the DOM rather than serialised once.
 */
export function runsOf(m: QRMatrix): Array<{ x: number; y: number; w: number }> {
  const out: Array<{ x: number; y: number; w: number }> = []
  for (let y = 0; y < m.size; y++) {
    const row = m.rows[y]
    let x = 0
    while (x < m.size) {
      if (!row[x]) {
        x++
        continue
      }
      const start = x
      while (x < m.size && row[x]) x++
      out.push({ x: start, y, w: x - start })
    }
  }
  return out
}

/**
 * viewBoxFor sizes the drawing to the matrix plus its quiet zone.
 *
 * The origin is negative so the matrix can be drawn at its own coordinates and
 * the margin falls out of the viewBox. The alternative — offsetting every rect
 * by QUIET — is the same picture and one more thing each caller has to remember
 * to do.
 */
export function viewBoxFor(m: QRMatrix): string {
  const side = m.size + QUIET * 2
  return `${-QUIET} ${-QUIET} ${side} ${side}`
}
