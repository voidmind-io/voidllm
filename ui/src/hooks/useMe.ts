import { useQuery } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface MeResponse {
  id: string
  email: string
  display_name: string
  role: string
  org_id?: string
  is_system_admin: boolean
}

export function useMe() {
  return useQuery({
    queryKey: ['me'],
    queryFn: () => apiClient<MeResponse>('/me'),
    staleTime: 5 * 60 * 1000,
  })
}
