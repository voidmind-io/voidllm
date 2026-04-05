import { useQuery } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface UpdateInfo {
  current_version: string
  available_version?: string
  release_notes?: string
  release_url?: string
  needs_update: boolean
  checked_at?: string
}

export function useUpdateCheck() {
  return useQuery<UpdateInfo>({
    queryKey: ['update-check'],
    queryFn: () => apiClient<UpdateInfo>('/system/update-check'),
    staleTime: 60 * 60 * 1000, // 1 hour
    refetchInterval: 60 * 60 * 1000,
  })
}
