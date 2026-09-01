// When something happened, said two ways.
//
// Thresholds follow Atlassian's: minutes, then hours, then yesterday, then
// days, and an absolute date once a week has passed — past that, "23d ago" is
// arithmetic the reader has to do rather than a date they can place.
//
// exactTime is not decoration. Every design system that publishes a rule on
// this says the precise value must stay reachable, and Primer's RelativeTime
// puts it in the title where it doubles as the accessible name. Timestamp.tsx
// is what pairs them.

// relativeTime renders an age. An unparseable value is returned as given
// rather than rendered as "NaN ago".
export function relativeTime(ts: string | number | Date, now: number = Date.now()): string {
  const then = new Date(ts).getTime()
  if (Number.isNaN(then)) return String(ts)

  const sec = Math.floor((now - then) / 1000)
  if (sec < 0) return 'just now'
  if (sec < 60) return 'just now'

  const min = Math.floor(sec / 60)
  if (min < 60) return `${min}m ago`

  const hr = Math.floor(min / 60)
  if (hr < 24) return `${hr}h ago`

  const day = Math.floor(hr / 24)
  if (day === 1) return 'yesterday'
  if (day <= 7) return `${day}d ago`

  return exactDate(then)
}

// exactTime is the full value, for a title attribute.
export function exactTime(ts: string | number | Date): string {
  const then = new Date(ts).getTime()
  if (Number.isNaN(then)) return String(ts)
  return new Date(then).toLocaleString(undefined, {
    year: 'numeric', month: 'short', day: 'numeric',
    hour: '2-digit', minute: '2-digit',
  })
}

function exactDate(ms: number): string {
  return new Date(ms).toLocaleDateString(undefined, {
    year: 'numeric', month: 'short', day: 'numeric',
  })
}
