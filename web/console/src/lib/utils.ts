import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'

// cn merges class lists and lets a later Tailwind utility beat an earlier one
// of the same kind. coss ui components take a className prop and expect it to
// override their defaults, which plain concatenation does not do.
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}
