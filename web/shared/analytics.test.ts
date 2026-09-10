import { describe, expect, it } from 'vitest'

import { Analytics, type Store } from './analytics'

type sent = { path: string; body: Record<string, any> }

/** recorder collects deliveries and answers 200. */
function recorder(status = 200) {
  const got: sent[] = []
  return {
    got,
    send: (path: string, body: string) => {
      got.push({ path, body: JSON.parse(body) })
      return Promise.resolve(status)
    },
  }
}

function memory(seed: Record<string, string> = {}): Store {
  const held = { ...seed }
  return {
    get: (k) => held[k] ?? null,
    set: (k, v) => {
      held[k] = v
    },
  }
}

/** event returns the single event of the nth delivery. */
function event(got: sent[], n = 0) {
  expect(got.length).toBeGreaterThan(n)
  expect(got[n].body.batch).toHaveLength(1)
  return got[n].body.batch[0]
}

describe('Analytics', () => {
  it('posts one event to the ingestion route with no project key', async () => {
    const r = recorder()
    new Analytics({ app: 'console', send: r.send, store: memory() }).capture('a.thing', { n: 1 })

    expect(r.got).toHaveLength(1)
    expect(r.got[0].path).toBe('/api/t/batch/')
    expect(r.got[0].body.api_key).toBeUndefined()
    expect(r.got[0].body.token).toBeUndefined()

    const e = event(r.got)
    expect(e.event).toBe('a.thing')
    expect(e.properties.n).toBe(1)
    expect(e.properties.app).toBe('console')
    expect(typeof e.distinct_id).toBe('string')
    expect(e.distinct_id.length).toBeGreaterThan(0)
  })

  it('reports a route pattern and never the path it came from', async () => {
    const r = recorder()
    new Analytics({ app: 'console', send: r.send, store: memory() }).page('/callers/$caller')

    const e = event(r.got)
    expect(e.event).toBe('$pageview')
    expect(e.properties.route).toBe('/callers/$caller')
    expect(e.properties.$pathname).toBe('/callers/$caller')
    expect(JSON.stringify(e)).not.toContain('billing-api')
  })

  // A console's own hostname names the customer it belongs to, so no property
  // may carry an origin. $current_url is the one PostHog's Paths insight
  // groups by, and it holds the route alone.
  it('reports no hostname', async () => {
    const r = recorder()
    new Analytics({ app: 'console', send: r.send, store: memory() }).page('/schema')

    const e = event(r.got)
    expect(e.properties.$current_url).toBe('/schema')
    for (const value of Object.values(e.properties)) {
      if (typeof value !== 'string') continue
      expect(value).not.toContain('//')
      expect(value).not.toContain('http')
    }
  })

  it('keeps one identity across events and across visits', async () => {
    const store = memory()
    const first = recorder()
    new Analytics({ app: 'cloud', send: first.send, store }).capture('a')
    const second = recorder()
    new Analytics({ app: 'cloud', send: second.send, store }).capture('b')

    expect(event(first.got).distinct_id).toBe(event(second.got).distinct_id)
  })

  it('stitches the anonymous events onto the person who signs in', async () => {
    const r = recorder()
    const a = new Analytics({ app: 'cloud', send: r.send, store: memory() })
    a.capture('signup.viewed')
    const anon = event(r.got).distinct_id

    a.identify('user-1')

    const id = event(r.got, 1)
    expect(id.event).toBe('$identify')
    expect(id.distinct_id).toBe('user-1')
    expect(id.properties.$anon_distinct_id).toBe(anon)

    a.capture('after')
    expect(event(r.got, 2).distinct_id).toBe('user-1')
  })

  it('identifies the same person once', async () => {
    const r = recorder()
    const a = new Analytics({ app: 'console', send: r.send, store: memory() })
    a.identify('user-1')
    a.identify('user-1')

    expect(r.got.filter((s) => s.body.batch[0].event === '$identify')).toHaveLength(1)
  })

  it('keys events to an organisation once one is known', async () => {
    const r = recorder()
    const a = new Analytics({ app: 'console', send: r.send, store: memory() })
    a.capture('before')
    a.identify('user-1', 'acme')
    a.capture('after')

    expect(event(r.got, 0).properties.$groups).toBeUndefined()
    expect(event(r.got, 2).properties.$groups).toEqual({ organisation: 'acme' })
  })

  // An organisation switch keeps the same person, so identify returns early —
  // the group assignment has to happen ahead of that return.
  it('changes the organisation without reporting a second person', async () => {
    const r = recorder()
    const a = new Analytics({ app: 'console', send: r.send, store: memory() })
    a.identify('user-1', 'acme')
    a.identify('user-1', 'globex')
    a.capture('after')

    expect(r.got.filter((s) => s.body.batch[0].event === '$identify')).toHaveLength(1)
    expect(event(r.got, 1).properties.$groups).toEqual({ organisation: 'globex' })
  })

  // A deployment with no project key answers 404, which is every development
  // and CI build.
  it('stops reporting when the route is absent', async () => {
    const r = recorder(404)
    const a = new Analytics({ app: 'cloud', send: r.send, store: memory() })
    a.capture('first')
    await Promise.resolve()
    a.capture('second')
    a.page('/signin')

    expect(r.got).toHaveLength(1)
  })

  // A rate limit or a gateway failure is the deployment's, not the page's.
  it('keeps reporting after a transient failure', async () => {
    const r = recorder(429)
    const a = new Analytics({ app: 'cloud', send: r.send, store: memory() })
    a.capture('first')
    await Promise.resolve()
    a.capture('second')

    expect(r.got).toHaveLength(2)
  })

  it('reports nothing after stop', async () => {
    const r = recorder()
    const a = new Analytics({ app: 'console', send: r.send, store: memory() })
    a.stop()
    a.capture('a')
    a.page('/b')
    a.identify('user-1')

    expect(r.got).toHaveLength(0)
  })

  // Whatever goes wrong inside reporting stays inside it: the page carries on
  // doing what it was doing.
  it('does not throw out of a report', async () => {
    const r = recorder()
    const broken: Store = {
      get() {
        throw new Error('denied')
      },
      set() {
        throw new Error('denied')
      },
    }
    const a = new Analytics({ app: 'cloud', send: r.send, store: broken })

    expect(() => a.capture('a')).not.toThrow()
    expect(() => a.page('/signin')).not.toThrow()
    expect(() => a.identify('user-1')).not.toThrow()
  })

  // Storage that refuses every write reads back as empty, so an id resolved
  // from storage each time is a new id on every event: one operator's session
  // arrives as one person per page.
  it('reports under one identity when nothing can be stored', async () => {
    const r = recorder()
    const forgetful: Store = { get: () => null, set: () => {} }
    const a = new Analytics({ app: 'console', send: r.send, store: forgetful })

    a.page('/schema')
    a.page('/callers')
    a.capture('a.thing')

    const ids = new Set(r.got.map((s) => s.body.batch[0].distinct_id))
    expect(ids.size).toBe(1)
  })

  it('identifies through a store that will not write', async () => {
    const r = recorder()
    const forgetful: Store = { get: () => null, set: () => {} }
    const a = new Analytics({ app: 'console', send: r.send, store: forgetful })

    a.page('/login')
    a.identify('user-1')
    a.page('/schema')

    expect(event(r.got, 2).distinct_id).toBe('user-1')
  })

  // Sign out, then the next person. Nothing they do may reach the profile of
  // the person before them, and the two must not be asked to merge.
  it('does not carry one person on a shared browser into the next', async () => {
    const store = memory()
    const first = recorder()
    const a = new Analytics({ app: 'console', send: first.send, store })
    a.identify('user-a')
    a.page('/schema')
    a.reset()

    const second = recorder()
    const b = new Analytics({ app: 'console', send: second.send, store })
    b.page('/login')
    const anonymous = event(second.got, 0).distinct_id
    expect(anonymous).not.toBe('user-a')

    b.identify('user-b')
    const id = event(second.got, 1)
    expect(id.event).toBe('$identify')
    expect(id.distinct_id).toBe('user-b')
    // The join names the anonymous id this browser has just used, never the
    // person who signed out.
    expect(id.properties.$anon_distinct_id).toBe(anonymous)
    expect(JSON.stringify(second.got)).not.toContain('user-a')
  })

  // Without a reset — a sign-out path that forgot to call it, or a session
  // that ended elsewhere — a second person still may not be merged into the
  // first.
  //
  // The anonymous page view before the first sign-in is what makes this bite:
  // that id is joined to user-a, so handing it to user-a's successor is what
  // asks PostHog to make one person out of two.
  it('never asks for two people to be joined', async () => {
    const r = recorder()
    const a = new Analytics({ app: 'console', send: r.send, store: memory() })

    a.page('/login')
    const anonymous = event(r.got, 0).distinct_id
    a.identify('user-a')
    expect(event(r.got, 1).properties.$anon_distinct_id).toBe(anonymous)

    a.page('/schema')
    a.identify('user-b')

    const id = event(r.got, 3)
    expect(id.event).toBe('$identify')
    expect(id.distinct_id).toBe('user-b')
    expect(id.properties.$anon_distinct_id).not.toBe(anonymous)
    expect(id.properties.$anon_distinct_id).toBeUndefined()
  })

  it('reports the next person anonymously after a reset', async () => {
    const r = recorder()
    const a = new Analytics({ app: 'console', send: r.send, store: memory() })
    a.identify('user-a', 'acme')
    a.reset()
    a.page('/login')

    const e = event(r.got, 1)
    expect(e.distinct_id).not.toBe('user-a')
    expect(e.properties.$groups).toBeUndefined()
  })

  // The anonymous id exists to join what was reported before a sign-in. A
  // sign-in with nothing before it has nothing to join.
  it('does not join an anonymous id that labelled no event', async () => {
    const r = recorder()
    const a = new Analytics({ app: 'cloud', send: r.send, store: memory() })
    a.identify('user-1')

    expect(event(r.got, 0).properties.$anon_distinct_id).toBeUndefined()
  })
})
