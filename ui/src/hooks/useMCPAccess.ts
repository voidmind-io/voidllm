import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface AvailableMCPServer {
  id: string
  name: string
  alias: string
}

export function useAvailableGlobalMCPServers(orgId: string) {
  return useQuery({
    queryKey: ['available-mcp-servers', orgId],
    queryFn: () =>
      apiClient<AvailableMCPServer[]>(`/orgs/${orgId}/available-mcp-servers`),
    enabled: !!orgId,
  })
}

export function useOrgMCPAccess(orgId: string) {
  return useQuery({
    queryKey: ['mcp-access', 'org', orgId],
    queryFn: () =>
      apiClient<{ servers: string[] }>(`/orgs/${orgId}/mcp-access`),
    enabled: !!orgId,
  })
}

export function useSetOrgMCPAccess(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (servers: string[]) =>
      apiClient<{ servers: string[] }>(`/orgs/${orgId}/mcp-access`, {
        method: 'PUT',
        body: JSON.stringify({ servers }),
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({
        queryKey: ['mcp-access', 'org', orgId],
      })
    },
  })
}

export function useTeamMCPAccess(orgId: string, teamId: string) {
  return useQuery({
    queryKey: ['mcp-access', 'team', orgId, teamId],
    queryFn: () =>
      apiClient<{ servers: string[] }>(
        `/orgs/${orgId}/teams/${teamId}/mcp-access`,
      ),
    enabled: !!orgId && !!teamId,
  })
}

export function useSetTeamMCPAccess(orgId: string, teamId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (servers: string[]) =>
      apiClient<{ servers: string[] }>(
        `/orgs/${orgId}/teams/${teamId}/mcp-access`,
        { method: 'PUT', body: JSON.stringify({ servers }) },
      ),
    onSuccess: () => {
      queryClient.invalidateQueries({
        queryKey: ['mcp-access', 'team', orgId, teamId],
      })
    },
  })
}
