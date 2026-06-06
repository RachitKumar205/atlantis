/**
 * BathyBg — barely-visible atmospheric texture for login + setup screens.
 *
 * A 32px dot grid at 1% opacity. Reads as drafting-paper, not pattern.
 * Pure CSS — a single `background` rule with a radial-gradient repeat. No
 * SVG, no DOM nodes inside. The aim is "you don't notice it, but the
 * canvas doesn't feel flat either."
 */
export function BathyBg() {
  return (
    <div
      style={{
        position: 'absolute',
        inset: 0,
        pointerEvents: 'none',
        zIndex: 0,
        backgroundImage:
          'radial-gradient(circle at 1px 1px, rgba(212, 165, 116, 0.06) 1px, transparent 0)',
        backgroundSize: '32px 32px',
        // Mask the dots to fade out toward the page edges so the texture
        // doesn't compete with the page header / footer.
        maskImage:
          'radial-gradient(ellipse 80% 70% at 50% 50%, #000 50%, transparent 100%)',
        WebkitMaskImage:
          'radial-gradient(ellipse 80% 70% at 50% 50%, #000 50%, transparent 100%)',
      }}
      aria-hidden="true"
    />
  )
}
