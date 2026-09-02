/**
 * Who a schema event is attributed to.
 *
 * Two facts, and both are shown: the person, and the caller credential the
 * change arrived under. The server authenticates the caller and cannot
 * authenticate the person, so they are never merged into one name.
 */

/** The fields every attributed record carries, from Blame or a version row. */
export interface ActorFields {
  caller: string
  actor?: string
  actor_email?: string
  actor_name?: string
}

/**
 * Resolves what to print. The name the account gave, then its address, then
 * neither — an unattended apply names no person and the caller stands alone.
 */
export function actorParts(a: ActorFields | undefined, fallbackCaller = ''): {
  person: string
  caller: string
  title: string
} {
  const caller = a?.caller || fallbackCaller
  const name = (a?.actor_name ?? '').trim()
  const email = (a?.actor_email ?? '').trim()
  if (name) {
    return { person: name, caller, title: `${email || a?.actor || name} · via ${caller}` }
  }
  if (email) {
    return { person: email, caller, title: `${email} · via ${caller}` }
  }
  return { person: '', caller, title: `${caller} · no person was recorded` }
}

/**
 * Renders "Rachit Kumar via atlantis-console", or the caller alone when no
 * person was recorded.
 *
 * Prose for the person and monospace for the caller, which is what tells a
 * human from a service account here — the same division a circular avatar and
 * a square one make in an interface that has avatars.
 */
export function Actor({ of, fallbackCaller = '', suppressCaller = '' }: {
  of: ActorFields | undefined
  fallbackCaller?: string
  // The caller already named in the surrounding sentence. Matching it drops
  // the via clause, so a header reading "owned by X" does not go on to say
  // "last modified by someone via X".
  suppressCaller?: string
}) {
  const a = actorParts(of, fallbackCaller)
  if (!a.person) {
    return <span className="mono" title={a.title}>{a.caller}</span>
  }
  if (suppressCaller && suppressCaller === a.caller) {
    return <span title={a.title}>{a.person}</span>
  }
  return (
    <span title={a.title}>
      {a.person} <span className="actor__via">via</span>{' '}
      <span className="mono">{a.caller}</span>
    </span>
  )
}
