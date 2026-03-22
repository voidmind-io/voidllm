import { useQuery } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface UsageDataPoint {
  group_key: string
  total_requests: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cost_estimate: number
  avg_duration_ms: number
}

interface UsageResponse {
  org_id: string
  from: string
  to: string
  group_by: string
  data: UsageDataPoint[]
}

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
