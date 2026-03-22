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

export interface UsageResponse {
  org_id: string
  from: string
  to: string
  group_by: string
  data: UsageDataPoint[]
}

export function useUsage(orgId: string, from: string, to: string, groupBy: string) {
  return useQuery({
    queryKey: ['usage', orgId, from, to, groupBy],
    queryFn: () =>
      apiClient<UsageResponse>(
        `/orgs/${orgId}/usage?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}&group_by=${groupBy}`,
      ),
    enabled: !!orgId && !!from && !!to,
    staleTime: 60_000,
  })
}

export function useCrossOrgUsage(
  params: { from: string; to: string; groupBy: string },
  enabled: boolean,
) {
  const { from, to, groupBy } = params
  const query = new URLSearchParams({ from, to, group_by: groupBy })

  return useQuery({
    queryKey: ['cross-org-usage', from, to, groupBy],
    queryFn: () => apiClient<UsageResponse>(`/usage?${query.toString()}`),
    enabled: enabled && !!from && !!to,
    staleTime: 60_000,
  })
}
