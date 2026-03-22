import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface APIKeyResponse {
  id: string
  key?: string
  key_hint: string
  key_type: string
  name: string
  org_id: string
  team_id: string | null
  user_id: string | null
  service_account_id: string | null
  daily_token_limit: number
  monthly_token_limit: number
  requests_per_minute: number
  requests_per_day: number
  expires_at: string | null
  last_used_at: string | null
  created_by: string
  created_at: string
  updated_at: string
}

export interface PaginatedKeys {
  data: APIKeyResponse[]
  has_more: boolean
  next_cursor?: string
}

export interface CreateAPIKeyParams {
  name: string
  key_type: string
  team_id?: string
  user_id?: string
  service_account_id?: string
  expires_at?: string
  daily_token_limit?: number
  monthly_token_limit?: number
  requests_per_minute?: number
  requests_per_day?: number
}

export function useAPIKeys(orgId: string, cursor?: string) {
  const params = new URLSearchParams({ limit: '20' })
  if (cursor) params.set('cursor', cursor)
  return useQuery({
    queryKey: ['api-keys', orgId, cursor],
    queryFn: () => apiClient<PaginatedKeys>(`/orgs/${orgId}/keys?${params}`),
    enabled: !!orgId,
  })
}

export function useCreateAPIKey(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (params: CreateAPIKeyParams) =>
      apiClient<APIKeyResponse>(`/orgs/${orgId}/keys`, {
        method: 'POST',
        body: JSON.stringify(params),
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['api-keys', orgId] })
      queryClient.invalidateQueries({ queryKey: ['dashboard-stats'] })
    },
  })
}

export function useDeleteAPIKey(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (keyId: string) =>
      apiClient<void>(`/orgs/${orgId}/keys/${keyId}`, { method: 'DELETE' }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['api-keys', orgId] })
      queryClient.invalidateQueries({ queryKey: ['dashboard-stats'] })
    },
  })
}

export function useUpdateAPIKey(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ keyId, params }: { keyId: string; params: Record<string, unknown> }) =>
      apiClient<APIKeyResponse>(`/orgs/${orgId}/keys/${keyId}`, {
        method: 'PATCH',
        body: JSON.stringify(params),
      }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['api-keys', orgId] }),
  })
}

export function useRotateAPIKey(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (keyId: string) =>
      apiClient<APIKeyResponse>(`/orgs/${orgId}/keys/${keyId}/rotate`, { method: 'POST' }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['api-keys', orgId] }),
  })
}
