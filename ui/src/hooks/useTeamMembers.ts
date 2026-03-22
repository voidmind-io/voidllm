import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface TeamMembershipResponse {
  id: string
  team_id: string
  user_id: string
  role: string
  created_at: string
}

interface PaginatedTeamMemberships {
  data: TeamMembershipResponse[]
  has_more: boolean
  next_cursor?: string
}

export interface CreateTeamMemberParams {
  user_id: string
  role: string
}

export function useTeamMembers(orgId: string, teamId: string, cursor?: string) {
  const params = new URLSearchParams({ limit: '20' })
  if (cursor) params.set('cursor', cursor)
  return useQuery({
    queryKey: ['team-members', orgId, teamId, cursor],
    queryFn: () =>
      apiClient<PaginatedTeamMemberships>(
        `/orgs/${orgId}/teams/${teamId}/members?${params}`,
      ),
    enabled: !!orgId && !!teamId,
  })
}

export function useAddTeamMember(orgId: string, teamId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (params: CreateTeamMemberParams) =>
      apiClient<TeamMembershipResponse>(
        `/orgs/${orgId}/teams/${teamId}/members`,
        { method: 'POST', body: JSON.stringify(params) },
      ),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['team-members', orgId, teamId] })
    },
  })
}

export function useRemoveTeamMember(orgId: string, teamId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (membershipId: string) =>
      apiClient<void>(
        `/orgs/${orgId}/teams/${teamId}/members/${membershipId}`,
        { method: 'DELETE' },
      ),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['team-members', orgId, teamId] })
    },
  })
}

export function useUpdateTeamMember(orgId: string, teamId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ membershipId, role }: { membershipId: string; role: string }) =>
      apiClient<TeamMembershipResponse>(
        `/orgs/${orgId}/teams/${teamId}/members/${membershipId}`,
        {
          method: 'PATCH',
          body: JSON.stringify({ role }),
        },
      ),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['team-members', orgId, teamId] })
    },
  })
}
