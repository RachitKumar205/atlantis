// What a page reports, and what it does not.
//
// One event per finished navigation or action, carrying the route pattern and
// the application. Never a URL, a query string, a deployment hostname, an
// organisation display name, a caller name, a schema identifier or any error
// text: a console path holds customer identifiers in its segments, and
// `/callers/$caller` is the whole value a funnel needs from
// `/callers/billing-api`.
//
// There is no project key here. Every event goes to this origin's own
// ingestion route, which substitutes the key server-side — see
// internal/analytics/proxy.go. A deployment with no key configured answers 404
// and reporting turns itself off for the session.

/** The PostHog group every event is keyed on. Matches analytics.GroupType. */
const GROUP_TYPE = 'organisation'

/** Where this origin forwards ingestion. Matches analytics.ProxyPathPrefix. */
const INGEST_PATH = '/api/t/batch/'

// The two identities a browser holds, in separate keys.
//
// A signed-in id written over an anonymous one cannot be told apart from it
// afterwards, and $anon_distinct_id given an id that is already a person asks
// PostHog to merge two people. Separate keys make that unrepresentable.
const ANON_KEY = 'atl.analytics.anon'
const USER_KEY = 'atl.analytics.user'

export type Options = {
  /** Which application is reporting: `cloud` or `console`. */
  app: string
  /** Overrides the delivery function. Tests pass a recorder. */
  send?: Sender
  /** Overrides the identity store. Tests pass a map. */
  store?: Store
}

/** Sender delivers one event and resolves the status the route answered. */
export type Sender = (path: string, body: string) => Promise<number>

export type Store = {
  get(key: string): string | null
  set(key: string, value: string): void
}

/** Analytics reports what a page does. Every method is best-effort. */
export class Analytics {
  private app: string
  private send: Sender
  private store: Store
  private org = ''
  private live = true

  // The resolved identities, held here as well as in storage.
  //
  // A browser that refuses to store reports under one id for the life of the
  // page. Reading storage each time would mint a new id per event, because a
  // write that fails silently reads back as absent.
  private anon = ''
  private user = ''

  // Whether anon has labelled an event that was sent. Only then is there
  // anything for $anon_distinct_id to join.
  private anonUsed = false

  constructor(opts: Options) {
    this.app = opts.app
    this.send = opts.send ?? beacon
    this.store = opts.store ?? browserStore()
    this.user = this.load(USER_KEY)
  }

  /**
   * identify attaches every following event to a person, and joins the events
   * already sent anonymously to them.
   *
   * userId is the Cloud user id, which is what the servers report under, so
   * one person is one profile across the browser, the API and the console.
   *
   * A different person on this browser gets a fresh anonymous id and no join:
   * what the last person did stays theirs.
   */
  identify(userId: string, org?: string) {
    // Assigned ahead of every return below. An organisation switch keeps the
    // same person, and the group still has to change.
    if (org !== undefined) this.org = org
    if (!this.live || !userId || this.user === userId) return

    if (this.user) {
      this.anon = this.mint(ANON_KEY)
      this.anonUsed = false
    }
    const previous = this.anonUsed ? this.anon : ''

    this.user = userId
    this.save(USER_KEY, userId)
    this.post('$identify', previous ? { $anon_distinct_id: previous } : {})
  }

  /**
   * reset ends the person, for a sign-out.
   *
   * The next event is anonymous under an id that has never been a person, so
   * whoever uses this browser next is somebody else.
   */
  reset() {
    this.user = ''
    this.save(USER_KEY, '')
    this.anon = this.mint(ANON_KEY)
    this.anonUsed = false
    this.org = ''
  }

  /**
   * page reports one navigation.
   *
   * route is the pattern, not the path: `/callers/$caller`, never
   * `/callers/billing-api`.
   *
   * $current_url carries the route and no origin. PostHog's Paths insight
   * groups each step by that property, and a deployment's own hostname is the
   * customer's — `https://atlantis.bigbank.example` names the customer as
   * surely as any field this module refuses to send.
   */
  page(route: string) {
    this.post('$pageview', {
      $current_url: route,
      $pathname: route,
      route,
    })
  }

  /** capture reports one named thing that is not a navigation. */
  capture(event: string, props: Record<string, unknown> = {}) {
    this.post(event, props)
  }

  /** stop ends reporting for this page. */
  stop() {
    this.live = false
  }

  /** identity is the id events are reported under, created on first use. */
  private identity(): string {
    if (this.user) return this.user
    if (!this.anon) {
      this.anon = this.load(ANON_KEY) || this.mint(ANON_KEY)
    }
    this.anonUsed = true
    return this.anon
  }

  private post(event: string, props: Record<string, unknown>) {
    if (!this.live) return
    const properties: Record<string, unknown> = { ...props, app: this.app }
    if (this.org) properties.$groups = { [GROUP_TYPE]: this.org }

    try {
      const body = JSON.stringify({
        batch: [
          {
            event,
            distinct_id: this.identity(),
            properties,
            timestamp: new Date().toISOString(),
          },
        ],
      })
      // 404 is a deployment with no project key and 403 an origin the route
      // will not accept, which between them are every development and CI
      // build. Reporting stops there, so a navigation costs no failed request.
      // Any other status is transient and the next event is sent.
      void this.send(INGEST_PATH, body)
        .then((status) => {
          if (status === 404 || status === 403) this.live = false
        })
        .catch(() => {})
    } catch {
      // Reporting is silent, and the page carries on.
    }
  }

  /** load reads one key, answering "" for a store that will not read. */
  private load(key: string): string {
    try {
      return this.store.get(key) ?? ''
    } catch {
      return ''
    }
  }

  /** save writes one key, and does nothing where a store will not write. */
  private save(key: string, value: string) {
    try {
      this.store.set(key, value)
    } catch {
      /* see load */
    }
  }

  private mint(key: string): string {
    const id = newID()
    this.save(key, id)
    return id
  }
}

/**
 * beacon delivers one event.
 *
 * keepalive so an event sent during a navigation survives the page it was sent
 * from.
 *
 * The route requires application/json, which sendBeacon cannot set: the three
 * types it can send are the ones a cross-origin page posts without a
 * preflight.
 */
function beacon(path: string, body: string): Promise<number> {
  return fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body,
    keepalive: true,
    credentials: 'omit',
  })
    .then((r) => r.status)
    .catch(() => 0)
}

// Storage is unavailable in a private window and behind some settings, and
// throws on both read and write there. Analytics.load and Analytics.save are
// what answer for that; these are the plain calls.
function browserStore(): Store {
  return {
    get: (key) => window.localStorage.getItem(key),
    set: (key, value) => window.localStorage.setItem(key, value),
  }
}

function newID(): string {
  try {
    return crypto.randomUUID()
  } catch {
    // crypto.randomUUID needs a secure context; a plain-HTTP development
    // origin has none.
    return 'anon-' + Math.random().toString(36).slice(2) + Date.now().toString(36)
  }
}
