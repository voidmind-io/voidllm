import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface ServiceAccountResponse {
  id: string
  name: string
  org_id: string
  team_id?: string | null
  created_by: string
  created_at: string
  updated_at: string
  key_count: number
}

interface PaginatedServiceAccounts {
  data: ServiceAccountResponse[]
  has_more: boolean
  next_cursor?: string
}

export interface CreateServiceAccountParams {
  name: string
  team_id?: string
}

export function useServiceAccounts(orgId: string, cursor?: string) {
  const params = new URLSearchParams({ limit: '20' })
  if (cursor) params.set('cursor', cursor)
  return useQuery({
    queryKey: ['service-accounts', orgId, cursor],
    queryFn: () =>
      apiClient<PaginatedServiceAccounts>(`/orgs/${orgId}/service-accounts?${params}`),
    enabled: !!orgId,
  })
}

export function useCreateServiceAccount(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (params: CreateServiceAccountParams) =>
      apiClient<ServiceAccountResponse>(`/orgs/${orgId}/service-accounts`, {
        method: 'POST',
        body: JSON.stringify(params),
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['service-accounts', orgId] })
    },
  })
}

export function useDeleteServiceAccount(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (saId: string) =>
      apiClient<void>(`/orgs/${orgId}/service-accounts/${saId}`, { method: 'DELETE' }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['service-accounts', orgId] })
    },
  })
}

export function useUpdateServiceAccount(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ saId, name }: { saId: string; name: string }) =>
      apiClient<ServiceAccountResponse>(`/orgs/${orgId}/service-accounts/${saId}`, {
        method: 'PATCH',
        body: JSON.stringify({ name }),
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['service-accounts', orgId] })
    },
  })
}
