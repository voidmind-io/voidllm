import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface MCPServerResponse {
  id: string
  name: string
  alias: string
  url: string
  auth_type: string
  auth_header?: string
  /** source is "api" for Admin API-created servers and "yaml" for config-file-sourced servers. */
  source: string
  scope: string
  org_id?: string
  team_id?: string
  is_active: boolean
  created_at: string
  updated_at: string
}

export interface CreateMCPServerParams {
  name: string
  alias: string
  url: string
  auth_type: string
  auth_header?: string
  auth_token?: string
}

export interface UpdateMCPServerParams {
  name?: string
  alias?: string
  url?: string
  auth_type?: string
  auth_header?: string
  auth_token?: string
}

export interface TestMCPServerResponse {
  success: boolean
  tools?: number
  error?: string
}

export function useMCPServers() {
  return useQuery({
    queryKey: ['mcp-servers'],
    queryFn: () => apiClient<MCPServerResponse[]>('/mcp-servers'),
  })
}

export function useOrgMCPServers(orgId: string) {
  return useQuery({
    queryKey: ['mcp-servers', 'org', orgId],
    queryFn: () => apiClient<MCPServerResponse[]>(`/orgs/${orgId}/mcp-servers`),
    enabled: !!orgId,
  })
}

export function useUpdateMCPServer() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ serverId, params }: { serverId: string; params: UpdateMCPServerParams }) =>
      apiClient<MCPServerResponse>(`/mcp-servers/${serverId}`, {
        method: 'PATCH',
        body: JSON.stringify(params),
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['mcp-servers'] })
    },
  })
}

export function useDeleteMCPServer() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (serverId: string) =>
      apiClient<void>(`/mcp-servers/${serverId}`, { method: 'DELETE' }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['mcp-servers'] })
    },
  })
}

export function useToggleMCPServer() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ serverId, activate }: { serverId: string; activate: boolean }) =>
      apiClient<MCPServerResponse>(`/mcp-servers/${serverId}/${activate ? 'activate' : 'deactivate'}`, {
        method: 'PATCH',
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['mcp-servers'] })
    },
  })
}

export function useTestMCPServer() {
  return useMutation({
    mutationFn: (serverId: string) =>
      apiClient<TestMCPServerResponse>(`/mcp-servers/${serverId}/test`, { method: 'POST' }),
  })
}
