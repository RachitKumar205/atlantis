import { useQuery } from '@tanstack/react-query'
import { queries } from '@/api/client'

export function useMe() {
  return useQuery({ ...queries.me(), retry: false })
}

export function useIsAdmin(): boolean {
  const { data } = useMe()
  return data?.role === 'admin'
}
