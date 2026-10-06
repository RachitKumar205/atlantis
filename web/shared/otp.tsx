import { Fragment, useRef } from 'react'

/* OtpField is a six-digit one-time code entered one box per digit, with a dash
 * between the groups of three. Styled by otp.css.
 *
 * onOverflow receives a paste that is not six digits, such as a ten-character
 * XXXXX-XXXXX backup code, so the caller can switch to a field that fits it.
 *
 * It carries no style attribute: Cloud's sign-in app is served under
 * `style-src 'self'` with no 'unsafe-inline'.
 */
export function OtpField({
  value,
  onChange,
  onOverflow,
  length = 6,
  autoFocus = true,
}: {
  value: string
  onChange: (next: string) => void
  onOverflow: (pasted: string) => void
  length?: number
  autoFocus?: boolean
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
            autoFocus={autoFocus && i === 0}
          />
        </Fragment>
      ))}
    </div>
  )
}
