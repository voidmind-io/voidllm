import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface OrgListItem {
  id: string
  name: string
  slug: string
  timezone: string | null
  daily_token_limit: number
  monthly_token_limit: number
  requests_per_minute: number
  requests_per_day: number
  created_at: string
  updated_at: string
}

interface PaginatedOrgs {
  data: OrgListItem[]
  has_more: boolean
  next_cursor?: string
}

export interface CreateOrgParams {
  name: string
  slug: string
}

export function useOrgs(cursor?: string) {
  return useQuery({
    queryKey: ['orgs', cursor],
    queryFn: () =>
      apiClient<PaginatedOrgs>(
        `/orgs?limit=20${cursor ? `&cursor=${cursor}` : ''}`,
      ),
  })
}

export function useCreateOrg() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (params: CreateOrgParams) =>
      apiClient<OrgListItem>('/orgs', {
        method: 'POST',
        body: JSON.stringify(params),
      }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['orgs'] }),
  })
}

export function useDeleteOrg() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (orgId: string) =>
      apiClient<void>(`/orgs/${orgId}`, { method: 'DELETE' }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['orgs'] }),
  })
}
