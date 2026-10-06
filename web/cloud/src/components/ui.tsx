import { type ReactNode } from 'react'

import { QUIET, matrixFor, runsOf, viewBoxFor } from '@/lib/qr'

// Shared primitives.
//
// No `style={{…}}` anywhere in this file or any other. The app is served under
// `style-src 'self'` with no 'unsafe-inline', which rejects style attributes as
// well as <style> blocks — including `style={{'--x': v}}`, since a custom
// property set that way is still a style attribute. Anything dynamic goes
// through a class name, or through ref + el.style.setProperty().
//
// None of that fails in `vite dev`, which never applies the policy. It fails in
// a tagged binary in a real browser.

export function Card({ title, subtitle, mark = true, children }: {
  title: string
  subtitle?: ReactNode
  // The mark belongs on the screens somebody arrives at, where it says which
  // product is asking for a password. Past sign-in it is decoration on a page
  // whose title already names what is on it.
  mark?: boolean
  children: ReactNode
}) {
  return (
    <div className="shell">
      {/*
        Decorative, and carried as a CSS background rather than an <img>: it is
        a crop with no information in it, so it has no alt text to write, and
        `object-fit` on an <img> would need the same rules anyway.

        aria-hidden because a screen reader has nothing to gain from it, and
        the panel is not in the tab order.
      */}
      <div className="shell__art" aria-hidden="true" />
      <div className="shell__form">
        <main className="card">
          {/*
            The mark and the screen's title are one heading. There is no
            separate brand line: every screen's title already names the product
            or the step, so a wordmark above it repeated what the next line
            said.

            Concentric rings, the same mark the console's rail carries. The
            centre takes var(--accent), so it is whatever the palette's
            interactive colour is rather than a value of its own — under
            Bathysphere that was brass, and under Datum it is the slate blue the
            button below it uses.

            Presentation attributes, not a style attribute: `fill="var(--x)"` is
            an SVG attribute and style-src does not govern it.
          */}
          <h1 className="wordmark">
            {mark ? (
              <svg
                className="wordmark__mark"
                viewBox="0 0 26 26"
                fill="none"
                aria-hidden="true"
              >
                <circle cx="13" cy="13" r="10" stroke="var(--line-strong)" strokeWidth="1.3" />
                <circle cx="13" cy="13" r="5.5" stroke="var(--ink-2)" strokeWidth="1.1" />
                <circle cx="13" cy="13" r="1.9" fill="var(--accent)" />
              </svg>
            ) : null}
            {title}
          </h1>
          {subtitle ? <p className="muted">{subtitle}</p> : null}
          {children}
        </main>
      </div>
    </div>
  )
}

export function Field({ label, hint, ...input }: {
  label: string
  hint?: string
} & React.InputHTMLAttributes<HTMLInputElement>) {
  const id = `f-${input.name ?? label.toLowerCase().replace(/\W+/g, '-')}`
  return (
    <div className="field">
      <label className="label" htmlFor={id}>{label}</label>
      <input id={id} className="input" {...input} />
      {hint ? <p className="hint">{hint}</p> : null}
    </div>
  )
}

export function Button({ children, busy, ...rest }: {
  busy?: boolean
} & React.ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button className="btn" disabled={busy || rest.disabled} {...rest}>
      {busy ? 'Working…' : children}
    </button>
  )
}

/**
 * Notice shows an error or a confirmation.
 *
 * `role="alert"` so a screen reader announces it — the failures here are things
 * like a wrong code, where somebody who cannot see the change has no other
 * signal that anything happened.
 */
export function Notice({ kind, children }: { kind: 'error' | 'info'; children: ReactNode }) {
  return (
    // alert interrupts whatever a screen reader is saying, which is right for
    // a failed sign-in and wrong for "we sent you a link". status queues
    // instead, and is announced when the reader next comes up for air.
    <p
      className={kind === 'error' ? 'notice notice--error' : 'notice'}
      role={kind === 'error' ? 'alert' : 'status'}
    >
      {children}
    </p>
  )
}

export function LinkButton({ children, ...rest }: React.ButtonHTMLAttributes<HTMLButtonElement>) {
  return <button className="linkbtn" type="button" {...rest}>{children}</button>
}

/**
 * QRCode renders an otpauth:// URI as inline SVG rectangles.
 *
 * Rectangles rather than a canvas or a data: URI, because both of those need
 * `img-src data:` and this origin does not grant it. See lib/qr.ts.
 *
 * aria-hidden and a sibling with the secret in text: a QR code is unreadable to
 * anybody using a screen reader, and the secret is the accessible path to the
 * same thing. The `x`/`y`/`width` attributes are SVG geometry, not CSS.
 */
export function QRCode({ uri }: { uri: string }) {
  const matrix = matrixFor(uri)
  return (
    <svg
      className="qr"
      viewBox={viewBoxFor(matrix)}
      shapeRendering="crispEdges"
      aria-hidden="true"
      focusable="false"
    >
      {/* The quiet zone has to be light, whatever the card sits on. */}
      <rect
        className="qr__quiet"
        x={-QUIET}
        y={-QUIET}
        width={matrix.size + QUIET * 2}
        height={matrix.size + QUIET * 2}
      />
      {runsOf(matrix).map((r) => (
        <rect key={`${r.x}-${r.y}`} x={r.x} y={r.y} width={r.w} height={1} />
      ))}
    </svg>
  )
}

