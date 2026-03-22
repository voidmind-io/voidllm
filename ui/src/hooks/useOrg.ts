import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface OrgResponse {
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

export interface UpdateOrgParams {
  name?: string
  slug?: string
  daily_token_limit?: number
  monthly_token_limit?: number
  requests_per_minute?: number
  requests_per_day?: number
}

export function useOrg(orgId: string) {
  return useQuery({
    queryKey: ['org', orgId],
    queryFn: () => apiClient<OrgResponse>(`/orgs/${orgId}`),
    enabled: !!orgId,
    staleTime: 5 * 60 * 1000,
  })
}

export function useUpdateOrg(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (params: UpdateOrgParams) =>
      apiClient<OrgResponse>(`/orgs/${orgId}`, {
        method: 'PATCH',
        body: JSON.stringify(params),
      }),
    onSuccess: (data) => {
      queryClient.setQueryData(['org', orgId], data)
    },
  })
}
