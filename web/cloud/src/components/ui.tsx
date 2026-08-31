import { Fragment, useRef, type ReactNode } from 'react'

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

/* OTP field: one box per digit, following coss's shape — a row of small square
 * inputs at gap 8, centred text, and the same lit-edge treatment the buttons
 * carry, with a dash between the two groups of three.
 *
 * Six boxes rather than one text field because that is what the screen is
 * asking for nine times out of ten. The tenth is a backup code, which is ten
 * characters in XXXXX-XXXXX form and cannot fit here — onOverflow is how that
 * gets out. A paste that is not six digits is handed up rather than silently
 * refused, so pasting a backup code into this field works instead of appearing
 * to do nothing.
 */
export function OtpField({
  value,
  onChange,
  onOverflow,
  length = 6,
}: {
  value: string
  onChange: (next: string) => void
  onOverflow: (pasted: string) => void
  length?: number
}) {
  const slots = useRef<Array<HTMLInputElement | null>>([])

  function focusSlot(i: number) {
    slots.current[Math.max(0, Math.min(length - 1, i))]?.focus()
  }

  function setAt(i: number, ch: string) {
    const next = value.padEnd(length, ' ').split('')
    next[i] = ch
    onChange(next.join('').replace(/ /g, '').slice(0, length))
  }

  function handleChange(i: number, raw: string) {
    // The last character typed, so overtyping a filled box replaces it rather
    // than being ignored.
    const ch = raw.replace(/\D/g, '').slice(-1)
    if (!ch) return
    setAt(i, ch)
    focusSlot(i + 1)
  }

  function handleKeyDown(i: number, e: React.KeyboardEvent<HTMLInputElement>) {
    if (e.key === 'Backspace') {
      e.preventDefault()
      if (value[i]) setAt(i, '')
      else {
        focusSlot(i - 1)
        setAt(i - 1, '')
      }
      return
    }
    if (e.key === 'ArrowLeft') {
      e.preventDefault()
      focusSlot(i - 1)
    }
    if (e.key === 'ArrowRight') {
      e.preventDefault()
      focusSlot(i + 1)
    }
  }

  function handlePaste(e: React.ClipboardEvent<HTMLInputElement>) {
    const text = e.clipboardData.getData('text').trim()
    if (!text) return
    e.preventDefault()
    const digits = text.replace(/\D/g, '')
    if (digits.length >= length) {
      onChange(digits.slice(0, length))
      focusSlot(length - 1)
      return
    }
    onOverflow(text)
  }

  return (
    <div className="otp" role="group" aria-label="One-time code">
      {Array.from({ length }, (_, i) => (
        <Fragment key={i}>
          {i === length / 2 ? <span className="otp__sep" aria-hidden="true" /> : null}
          <input
            ref={(el) => {
              slots.current[i] = el
            }}
            className="otp__slot"
            value={value[i] ?? ''}
            onChange={(e) => handleChange(i, e.target.value)}
            onKeyDown={(e) => handleKeyDown(i, e)}
            onPaste={handlePaste}
            onFocus={(e) => e.target.select()}
            inputMode="numeric"
            autoComplete={i === 0 ? 'one-time-code' : 'off'}
            aria-label={`Digit ${i + 1} of ${length}`}
            maxLength={1}
            spellCheck={false}
            autoFocus={i === 0}
          />
        </Fragment>
      ))}
    </div>
  )
}
