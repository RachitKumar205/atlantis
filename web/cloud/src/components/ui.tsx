import type { ReactNode } from 'react'

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

export function Card({ title, subtitle, children }: {
  title: string
  subtitle?: ReactNode
  children: ReactNode
}) {
  return (
    <div className="shell">
      <main className="card">
        <p className="wordmark">atlantis cloud</p>
        <h1 className="title">{title}</h1>
        {subtitle ? <p className="muted">{subtitle}</p> : null}
        {children}
      </main>
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
    <p className={kind === 'error' ? 'notice notice--error' : 'notice'} role="alert">
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
