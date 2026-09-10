import { Analytics } from '@atlantis/shared/analytics'

/**
 * What this application reports.
 *
 * The sign-up funnel, which is measured before a session exists: the screens
 * somebody passes through, and the point at which an anonymous visitor becomes
 * a Cloud user. No address, no password state, no organisation name, no error
 * text — a failed sign-in and a successful one differ here by the screen that
 * follows.
 */
export const events = new Analytics({ app: 'cloud' })

/**
 * routeForScreen is the path a screen reports as.
 *
 * This application is one document. /signin, /organisations and /cli are the
 * only three addresses it has, and the other eight steps happen with no
 * navigation, so window.location names three of eleven.
 */
export function routeForScreen(screen: string): string {
  return '/' + screen
}
