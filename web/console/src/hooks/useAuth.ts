import { useEffect } from 'react'
import { useQuery } from '@tanstack/react-query'
import { queries } from '@/api/client'
import { events } from '@/lib/analytics'

export function useMe() {
  const query = useQuery({ ...queries.me(), retry: false })

  // Who the following events belong to. This is the one place the console
  // learns an identity, and the values are the two the server already reports
  // under: Cloud's user id and the organisation. Neither the address nor the
  // display name crosses.
  const { subject, org } = query.data ?? {}
  useEffect(() => {
    if (subject) events.identify(subject, org)
  }, [subject, org])

  return query
}

export function useIsAdmin(): boolean {
  const { data } = useMe()
  return data?.role === 'admin'
}
