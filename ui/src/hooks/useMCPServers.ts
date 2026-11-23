import { useQuery, useMutation, useQueryClient, type QueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface MCPServerResponse {
  id: string
  name: string
  alias: string
  url: string
  auth_type: string
  auth_header?: string
  oauth_token_url?: string
  oauth_client_id?: string
  oauth_scopes?: string
  // Note: oauth_client_secret is write-only, never returned
  /** source is "api" for Admin API-created servers and "yaml" for config-file-sourced servers. */
  source: string
  scope: string
  org_id?: string
  team_id?: string
  is_active: boolean
  code_mode_enabled: boolean
  /**
   * Effective MCP protocol era pin: "auto" (the default — VoidLLM probes the
   * upstream and detects it automatically) or one of the explicit revisions
   * ("2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26").
   */
  protocol_version: string
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
  oauth_token_url?: string
  oauth_client_id?: string
  oauth_client_secret?: string
  oauth_scopes?: string
  /** MCP protocol era override. Omit or send "auto" to keep auto-detection. */
  protocol_version?: string
}

export interface UpdateMCPServerParams {
  name?: string
  alias?: string
  url?: string
  auth_type?: string
  auth_header?: string
  auth_token?: string
  oauth_token_url?: string
  oauth_client_id?: string
  oauth_client_secret?: string
  oauth_scopes?: string
  code_mode_enabled?: boolean
  /** MCP protocol era override. Send "auto" to revert to auto-detection. */
  protocol_version?: string
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

export function invalidateMCPServerQueries(queryClient: QueryClient): void {
  for (const key of ['mcp-servers', 'available-mcp-servers', 'mcp-access', 'mcp-server-health']) {
    queryClient.invalidateQueries({ queryKey: [key] })
  }
}

export function useMCPServers() {
  return useQuery({
    queryKey: ['mcp-servers'],
    queryFn: ({ signal }) => apiClient<MCPServerResponse[]>('/mcp-servers', { signal }),
  })
}

export function useOrgMCPServers(orgId: string) {
  return useQuery({
    queryKey: ['mcp-servers', 'org', orgId],
    queryFn: ({ signal }) => apiClient<MCPServerResponse[]>(`/orgs/${orgId}/mcp-servers`, { signal }),
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
      invalidateMCPServerQueries(queryClient)
    },
  })
}

export function useDeleteMCPServer() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (serverId: string) =>
      apiClient<void>(`/mcp-servers/${serverId}`, { method: 'DELETE' }),
    onSuccess: () => {
      invalidateMCPServerQueries(queryClient)
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
      invalidateMCPServerQueries(queryClient)
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
    queryFn: ({ signal }) => apiClient<ToolBlocklistEntry[]>(`/mcp-servers/${serverId}/blocklist`, { signal }),
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
    queryFn: ({ signal }) => apiClient<MCPToolResponse[]>(`/mcp-servers/${serverId}/tools`, { signal }),
    enabled: !!serverId,
  })
}
