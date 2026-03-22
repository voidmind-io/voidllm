import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface TeamResponse {
  id: string
  org_id: string
  name: string
  slug: string
  member_count: number
  key_count: number
  daily_token_limit: number
  monthly_token_limit: number
  requests_per_minute: number
  requests_per_day: number
  created_at: string
  updated_at: string
}

interface PaginatedTeams {
  data: TeamResponse[]
  has_more: boolean
  next_cursor?: string
}

export interface CreateTeamParams {
  name: string
  slug: string
  daily_token_limit?: number
  monthly_token_limit?: number
  requests_per_minute?: number
  requests_per_day?: number
}

export function useTeam(orgId: string, teamId: string) {
  return useQuery({
    queryKey: ['teams', orgId, 'detail', teamId],
    queryFn: () => apiClient<TeamResponse>(`/orgs/${orgId}/teams/${teamId}`),
    enabled: !!orgId && !!teamId,
  })
}

export function useUpdateTeam(orgId: string, teamId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (params: Partial<CreateTeamParams>) =>
      apiClient<TeamResponse>(`/orgs/${orgId}/teams/${teamId}`, {
        method: 'PATCH',
        body: JSON.stringify(params),
      }),
    onSuccess: (updated) => {
      queryClient.setQueryData(['teams', orgId, 'detail', teamId], updated)
      queryClient.invalidateQueries({ queryKey: ['teams', orgId] })
    },
  })
}

export function useTeams(orgId: string, cursor?: string) {
  const params = new URLSearchParams({ limit: '20' })
  if (cursor) params.set('cursor', cursor)
  return useQuery({
    queryKey: ['teams', orgId, cursor],
    queryFn: () => apiClient<PaginatedTeams>(`/orgs/${orgId}/teams?${params}`),
    enabled: !!orgId,
  })
}

export function useCreateTeam(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (params: CreateTeamParams) =>
      apiClient<TeamResponse>(`/orgs/${orgId}/teams`, {
        method: 'POST',
        body: JSON.stringify(params),
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['teams', orgId] })
      queryClient.invalidateQueries({ queryKey: ['dashboard-stats'] })
    },
  })
}

export function useDeleteTeam(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (teamId: string) =>
      apiClient<void>(`/orgs/${orgId}/teams/${teamId}`, { method: 'DELETE' }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['teams', orgId] })
      queryClient.invalidateQueries({ queryKey: ['dashboard-stats'] })
    },
  })
}
