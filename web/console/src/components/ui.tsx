import { useEffect, useId, type ReactNode } from 'react'
import { CaretDown, Info, Warning, WarningOctagon, CheckCircle } from '@phosphor-icons/react'

// The coss components the console uses, with coss's anatomy and variant names.
//
// coss is Base UI plus a Tailwind layer, and this console is neither, so these
// are the same components written against the Datum tokens: Alert is a root
// with a title and a description, Progress is a label, a value and a track
// holding an indicator, Accordion is an item with a trigger and a panel.
//
// Keeping the names means a screen built here can be read against coss's own
// documentation.

// ── Alert ──────────────────────────────────────────────────────────────────

export type AlertVariant = 'default' | 'error' | 'info' | 'success' | 'warning'

const ALERT_ICON: Record<AlertVariant, ReactNode> = {
  default: <Info size={16} />,
  error: <WarningOctagon size={16} />,
  info: <Info size={16} />,
  success: <CheckCircle size={16} />,
  warning: <Warning size={16} />,
}

// Alert states something about the screen it is on. An error variant is given
// role="alert" so it is announced; the rest are ordinary content.
export function Alert({
  variant = 'default',
  icon,
  children,
}: {
  variant?: AlertVariant
  icon?: ReactNode
  children: ReactNode
}) {
  return (
    <div className={`alert alert--${variant}`} role={variant === 'error' ? 'alert' : undefined}>
      <span className="alert__icon">{icon ?? ALERT_ICON[variant]}</span>
      <div className="alert__content">{children}</div>
    </div>
  )
}

export function AlertTitle({ children }: { children: ReactNode }) {
  return <div className="alert__title">{children}</div>
}

export function AlertDescription({ children }: { children: ReactNode }) {
  return <div className="alert__desc">{children}</div>
}

// ── Progress ───────────────────────────────────────────────────────────────

// Progress carries the step count. coss has no stepper, and a bar with a label
// and a value says the same thing: which step, and how many are left.
export function Progress({ value, max = 100, children }: { value: number; max?: number; children?: ReactNode }) {
  const pct = max <= 0 ? 0 : Math.max(0, Math.min(100, (value / max) * 100))
  return (
    <div
      className="progress"
      role="progressbar"
      aria-valuenow={value}
      aria-valuemin={0}
      aria-valuemax={max}
    >
      {children}
      <div className="progress__track">
        <div className="progress__indicator" style={{ width: `${pct}%` }} />
      </div>
    </div>
  )
}

export function ProgressLabel({ children }: { children: ReactNode }) {
  return <span className="progress__label">{children}</span>
}

export function ProgressValue({ children }: { children: ReactNode }) {
  return <span className="progress__value">{children}</span>
}

// ── Accordion ──────────────────────────────────────────────────────────────

export function Accordion({ children }: { children: ReactNode }) {
  return <div className="accordion">{children}</div>
}

// One item, open or closed on its own. `details` carries the open state and the
// keyboard behaviour, so nothing here holds it.
export function AccordionItem({
  trigger,
  meta,
  defaultOpen,
  children,
}: {
  trigger: ReactNode
  meta?: ReactNode
  defaultOpen?: boolean
  children: ReactNode
}) {
  return (
    <details className="accordion__item" open={defaultOpen}>
      <summary className="accordion__trigger">
        <CaretDown size={13} className="accordion__caret" />
        <span className="accordion__label">{trigger}</span>
        {meta && <span className="accordion__meta">{meta}</span>}
      </summary>
      <div className="accordion__panel">{children}</div>
    </details>
  )
}

// ── Radio Group ────────────────────────────────────────────────────────────

export interface RadioCard {
  value: string
  title: string
  description: string
  icon?: ReactNode
}

// A radio group whose options are cards. The choice is a form control rather
// than two buttons, so arrow keys move between the options and the selection
// survives a step back.
export function RadioCardGroup({
  name,
  value,
  onChange,
  options,
}: {
  name: string
  value: string
  onChange: (value: string) => void
  options: RadioCard[]
}) {
  const id = useId()
  return (
    <div className="radiocards" role="radiogroup">
      {options.map(o => (
        <label
          key={o.value}
          className={`radiocard ${value === o.value ? 'is-selected' : ''}`}
          htmlFor={`${id}-${o.value}`}
        >
          <input
            id={`${id}-${o.value}`}
            type="radio"
            name={name}
            value={o.value}
            checked={value === o.value}
            onChange={() => onChange(o.value)}
            className="radiocard__input"
          />
          {o.icon && <span className="radiocard__icon">{o.icon}</span>}
          <span className="radiocard__text">
            <span className="radiocard__title">{o.title}</span>
            <span className="radiocard__desc">{o.description}</span>
          </span>
          <span className="radiocard__dot" aria-hidden />
        </label>
      ))}
    </div>
  )
}

// ── Separator ──────────────────────────────────────────────────────────────

export function Separator() {
  return <hr className="separator" />
}

// ── Alert Dialog ───────────────────────────────────────────────────────────

// A dialog that interrupts to ask one question. Unlike Dialog, the scrim does
// not dismiss it and there is no close affordance: the two buttons are the
// only ways out, because it is asking rather than showing.
//
// role="alertdialog" and the description wired through aria-describedby, so
// the question is read on open rather than only the title.
export function AlertDialog({
  title,
  confirmLabel,
  cancelLabel = 'Cancel',
  tone = 'default',
  busy,
  onConfirm,
  onCancel,
  children,
}: {
  title: string
  confirmLabel: string
  cancelLabel?: string
  tone?: 'default' | 'danger'
  busy?: boolean
  onConfirm: () => void
  onCancel: () => void
  children: ReactNode
}) {
  const id = useId()

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      // Stops the flow's own Escape handler from closing the dialog behind
      // this one, which would answer the question by discarding the screen
      // that asked it.
      e.stopPropagation()
      onCancel()
    }
    document.addEventListener('keydown', onKey, true)
    return () => document.removeEventListener('keydown', onKey, true)
  }, [onCancel])

  return (
    <div className="overlay overlay--nested is-open">
      <div
        className="modal modal--alert"
        role="alertdialog"
        aria-modal
        aria-labelledby={`${id}-t`}
        aria-describedby={`${id}-d`}
      >
        <div className="alertdialog">
          <div className="alertdialog__title" id={`${id}-t`}>{title}</div>
          <div className="alertdialog__desc" id={`${id}-d`}>{children}</div>
        </div>
        <div className="modal__foot">
          <button className="btn btn--ghost" type="button" onClick={onCancel} disabled={busy}>
            {cancelLabel}
          </button>
          <span className="spacer" style={{ flex: 1 }} />
          <button
            className={`btn ${tone === 'danger' ? 'btn--danger' : 'btn--brass'}`}
            type="button"
            onClick={onConfirm}
            disabled={busy}
            autoFocus
          >
            {busy && <span className="spin" />}
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  )
}
