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
  code_mode_enabled: boolean
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
  code_mode_enabled?: boolean
}

export interface ToolBlocklistEntry {
  id: string
  server_id: string
  tool_name: string
  reason: string
  created_by: string | null
  created_at: string
}

export interface RefreshToolsResponse {
  tool_count: number
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

export function useMCPServerBlocklist(serverId: string) {
  return useQuery<ToolBlocklistEntry[]>({
    queryKey: ['mcp-server-blocklist', serverId],
    queryFn: () => apiClient<ToolBlocklistEntry[]>(`/mcp-servers/${serverId}/blocklist`),
    enabled: !!serverId,
  })
}

export function useAddBlocklistEntry() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ serverId, toolName, reason }: { serverId: string; toolName: string; reason?: string }) =>
      apiClient<ToolBlocklistEntry>(`/mcp-servers/${serverId}/blocklist`, {
        method: 'POST',
        body: JSON.stringify({ tool_name: toolName, reason: reason ?? '' }),
      }),
    onSuccess: (_, vars) => {
      queryClient.invalidateQueries({ queryKey: ['mcp-server-blocklist', vars.serverId] })
      queryClient.invalidateQueries({ queryKey: ['mcp-server-tools', vars.serverId] })
    },
  })
}

export function useRemoveBlocklistEntry() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ serverId, toolName }: { serverId: string; toolName: string }) =>
      apiClient<void>(`/mcp-servers/${serverId}/blocklist?tool_name=${encodeURIComponent(toolName)}`, {
        method: 'DELETE',
      }),
    onSuccess: (_, vars) => {
      queryClient.invalidateQueries({ queryKey: ['mcp-server-blocklist', vars.serverId] })
      queryClient.invalidateQueries({ queryKey: ['mcp-server-tools', vars.serverId] })
    },
  })
}

export function useRefreshMCPServerTools() {
  const queryClient = useQueryClient()
  return useMutation<RefreshToolsResponse, Error, string>({
    mutationFn: (serverId: string) =>
      apiClient<RefreshToolsResponse>(`/mcp-servers/${serverId}/refresh-tools`, { method: 'POST' }),
    onSuccess: (_, serverId) => {
      queryClient.invalidateQueries({ queryKey: ['mcp-server-blocklist'] })
      queryClient.invalidateQueries({ queryKey: ['mcp-server-tools', serverId] })
    },
  })
}

export interface MCPToolResponse {
  name: string
  description: string
  blocked: boolean
}

export function useMCPServerTools(serverId: string) {
  return useQuery<MCPToolResponse[]>({
    queryKey: ['mcp-server-tools', serverId],
    queryFn: () => apiClient<MCPToolResponse[]>(`/mcp-servers/${serverId}/tools`),
    enabled: !!serverId,
  })
}
