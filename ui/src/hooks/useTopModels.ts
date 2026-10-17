import { useQuery } from '@tanstack/react-query'
import apiClient from '../api/client'
import type { UsageDataPoint, UsageResponse } from './useUsage'

export type { UsageDataPoint, UsageResponse }

export function useTopModels(orgId: string, enabled = true) {
  return useQuery({
    queryKey: ['top-models', orgId],
    queryFn: () => {
      const from = new Date(Date.now() - 24 * 60 * 60 * 1000).toISOString()
      const to = new Date().toISOString()
      return apiClient<UsageResponse>(
        `/orgs/${orgId}/usage?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}&group_by=model`,
      )
    },
    enabled: !!orgId && enabled,
    staleTime: 60_000,
  })
}
