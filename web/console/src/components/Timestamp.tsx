import { exactTime, relativeTime } from '@/lib/time'

// A time, shown as an age with the precise value one hover away.
//
// The pattern is Primer's RelativeTime: the title carries the exact value and
// doubles as the accessible name, so the reader who needs a date is never
// stuck with "3d ago" and no way to resolve it. <time dateTime> is what makes
// the machine-readable value available without parsing the text back.
export function Timestamp({ at, className }: { at: string; className?: string }) {
  if (!at) return null
  return (
    <time className={className} dateTime={at} title={exactTime(at)}>
      {relativeTime(at)}
    </time>
  )
}
