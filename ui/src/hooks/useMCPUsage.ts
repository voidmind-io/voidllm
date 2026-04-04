import { useQuery } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface MCPUsageDataPoint {
  group_key: string
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

export function useMCPUsage(orgId: string, from: string, to: string, groupBy: string) {
  return useQuery({
    queryKey: ['mcp-usage', orgId, from, to, groupBy],
    queryFn: () =>
      apiClient<MCPUsageResponse>(
        `/orgs/${orgId}/mcp-usage?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}&group_by=${encodeURIComponent(groupBy)}`,
      ),
    enabled: !!orgId && !!from && !!to,
    staleTime: 60_000,
  })
}

export function useCrossOrgMCPUsage(
  params: { from: string; to: string; groupBy: string },
  enabled: boolean,
) {
  const { from, to, groupBy } = params
  const query = new URLSearchParams({ from, to, group_by: groupBy })

  return useQuery({
    queryKey: ['cross-org-mcp-usage', from, to, groupBy],
    queryFn: () => apiClient<MCPUsageResponse>(`/mcp-usage?${query.toString()}`),
    enabled: enabled && !!from && !!to,
    staleTime: 60_000,
  })
}
