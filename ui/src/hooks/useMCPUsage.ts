import { useQuery } from '@tanstack/react-query'
import apiClient from '../api/client'
import { resolveRange } from '../lib/timeRange'
import type { UsageRange } from '../lib/timeRange'

export interface MCPUsageDataPoint {
  group_key: string
  group_label?: string
  total_calls: number
  success_count: number
  error_count: number
  timeout_count: number
  avg_duration_ms: number
  code_mode_calls: number
}

export interface MCPUsageResponse {
  org_id: string
  from: string
  to: string
  group_by: string
  data: MCPUsageDataPoint[]
}

const DEFAULT_REFETCH_MS = 60_000

export function useMCPUsage(
  orgId: string,
  range: UsageRange,
  groupBy: string,
  opts?: { enabled?: boolean },
) {
  return useQuery({
    queryKey: ['mcp-usage', orgId, range, groupBy],
    queryFn: () => {
      const { from, to } = resolveRange(range)
      return apiClient<MCPUsageResponse>(
        `/orgs/${orgId}/mcp-usage?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}&group_by=${encodeURIComponent(groupBy)}`,
      )
    },
    enabled: !!orgId && (opts?.enabled ?? true),
    staleTime: 60_000,
    refetchInterval: DEFAULT_REFETCH_MS,
  })
}

export function useCrossOrgMCPUsage(
  params: { range: UsageRange; groupBy: string },
  enabled: boolean,
) {
  const { range, groupBy } = params

  return useQuery({
    queryKey: ['cross-org-mcp-usage', range, groupBy],
    queryFn: () => {
      const { from, to } = resolveRange(range)
      const query = new URLSearchParams({ from, to, group_by: groupBy })
      return apiClient<MCPUsageResponse>(`/mcp-usage?${query.toString()}`)
    },
    enabled,
    staleTime: 60_000,
    refetchInterval: DEFAULT_REFETCH_MS,
  })
}
