import { useQuery } from '@tanstack/react-query'
import apiClient from '../api/client'
import { resolveRange } from '../lib/timeRange'
import type { UsageRange } from '../lib/timeRange'

export interface UsageDataPoint {
  group_key: string
  group_label?: string
  total_requests: number
  prompt_tokens: number
  completion_tokens: number
  cached_read_tokens: number
  cache_write_tokens: number
  total_tokens: number
  cost_estimate: number
  cache_savings?: number | null
  avg_duration_ms: number
}

export interface UsageResponse {
  org_id: string
  from: string
  to: string
  group_by: string
  data: UsageDataPoint[]
}

/** Options shared by the usage query hooks. */
export interface UsageQueryOptions {
  enabled?: boolean
  refetchInterval?: number | false
}

const DEFAULT_REFETCH_MS = 60_000

export function useUsage(
  orgId: string,
  range: UsageRange,
  groupBy: string,
  opts?: UsageQueryOptions,
) {
  return useQuery({
    queryKey: ['usage', orgId, range, groupBy],
    queryFn: ({ signal }) => {
      const { from, to } = resolveRange(range)
      return apiClient<UsageResponse>(
        `/orgs/${orgId}/usage?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}&group_by=${groupBy}`, { signal },
      )
    },
    enabled: !!orgId && (opts?.enabled ?? true),
    staleTime: 60_000,
    refetchInterval: opts?.refetchInterval ?? DEFAULT_REFETCH_MS,
  })
}

/** Usage of the calling user only (GET /usage/me). Available to every role. */
export function useMyUsage(range: UsageRange, groupBy: string, opts?: UsageQueryOptions) {
  return useQuery({
    queryKey: ['my-usage', range, groupBy],
    queryFn: ({ signal }) => {
      const { from, to } = resolveRange(range)
      const query = new URLSearchParams({ from, to, group_by: groupBy })
      return apiClient<UsageResponse>(`/usage/me?${query.toString()}`, { signal })
    },
    enabled: opts?.enabled ?? true,
    staleTime: 60_000,
    refetchInterval: opts?.refetchInterval ?? DEFAULT_REFETCH_MS,
  })
}

export function useCrossOrgUsage(
  params: { range: UsageRange; groupBy: string },
  enabled: boolean,
) {
  const { range, groupBy } = params

  return useQuery({
    queryKey: ['cross-org-usage', range, groupBy],
    queryFn: ({ signal }) => {
      const { from, to } = resolveRange(range)
      const query = new URLSearchParams({ from, to, group_by: groupBy })
      return apiClient<UsageResponse>(`/usage?${query.toString()}`, { signal })
    },
    enabled,
    staleTime: 60_000,
    refetchInterval: DEFAULT_REFETCH_MS,
  })
}
