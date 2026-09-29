import type { ReactNode } from 'react'

// HoverInfo wraps an element with a hover/focus popover. Open state is the
// parent's CSS :hover/:focus-within — no portal, no positioner library, no
// render-on-hover. Don't pass tall content: there's no viewport-collision
// avoidance, so a tall popover clips behind the next surface.
//
// inline=true switches the wrapper to display: inline-flex with auto width,
// for icon buttons that sit inside a flex row (the default mode stretches
// the trigger to 100% width, which is what the Sandbox boot grid needs but
// breaks icon-button layouts like the Callers card header).
//
// align="end" lines the popover up with the trigger's right edge, for a
// trigger at the right of its container. A centred popover there reaches past
// the container, and a hidden popover still widens its scroll area.
export function HoverInfo({
  children,
  content,
  side = 'bottom',
  inline = false,
  align = 'center',
}: {
  children: ReactNode
  content: ReactNode
  side?: 'top' | 'bottom' | 'right'
  inline?: boolean
  align?: 'center' | 'end'
}) {
  const end = align === 'end' && side !== 'right' ? ' hover-info-pop--end' : ''
  return (
    <span className={`hover-info${inline ? ' hover-info--inline' : ''}`}>
      {children}
      <span className={`hover-info-pop hover-info-pop--${side}${end}`} role="tooltip">
        {content}
      </span>
    </span>
  )
}
