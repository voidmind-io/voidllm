import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

// OrgMembershipResponse is the wire shape returned by GET/POST /orgs/:id/members.
export interface OrgMembershipResponse {
  id: string
  org_id: string
  user_id: string
  role: string
  daily_token_limit: number
  monthly_token_limit: number
  requests_per_minute: number
  requests_per_day: number
  created_at: string
}

export interface OrgMembersSummary {
  total: number
  admins: number
}

interface PaginatedOrgMemberships {
  data: OrgMembershipResponse[]
  summary?: OrgMembersSummary
  has_more: boolean
  next_cursor?: string
}

export interface CreateOrgMemberParams {
  user_id: string
  role: string
}

export interface UpdateOrgMemberParams {
  role?: string
  daily_token_limit?: number
  monthly_token_limit?: number
  requests_per_minute?: number
  requests_per_day?: number
}

export function useOrgMembers(orgId: string, cursor?: string, opts?: { summary?: boolean }) {
  const summary = opts?.summary ?? false
  const params = new URLSearchParams({ limit: '20' })
  if (cursor) params.set('cursor', cursor)
  if (summary) params.set('summary', 'true')
  return useQuery({
    queryKey: ['org-members', orgId, cursor, summary],
    queryFn: ({ signal }) =>
      apiClient<PaginatedOrgMemberships>(`/orgs/${orgId}/members?${params}`, { signal }),
    enabled: !!orgId,
  })
}

export function useCreateOrgMember(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (params: CreateOrgMemberParams) =>
      apiClient<OrgMembershipResponse>(`/orgs/${orgId}/members`, {
        method: 'POST',
        body: JSON.stringify(params),
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['org-members', orgId] })
      queryClient.invalidateQueries({ queryKey: ['dashboard-stats'] })
    },
  })
}

export function useUpdateOrgMember(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({
      membershipId,
      params,
    }: {
      membershipId: string
      params: UpdateOrgMemberParams
    }) =>
      apiClient<OrgMembershipResponse>(
        `/orgs/${orgId}/members/${membershipId}`,
        {
          method: 'PATCH',
          body: JSON.stringify(params),
        },
      ),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['org-members', orgId] })
      queryClient.invalidateQueries({ queryKey: ['team-members'] })
      queryClient.invalidateQueries({ queryKey: ['teams', orgId] })
    },
  })
}

export function useDeleteOrgMember(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (membershipId: string) =>
      apiClient<void>(`/orgs/${orgId}/members/${membershipId}`, {
        method: 'DELETE',
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['org-members', orgId] })
      queryClient.invalidateQueries({ queryKey: ['dashboard-stats'] })
      queryClient.invalidateQueries({ queryKey: ['team-members'] })
      queryClient.invalidateQueries({ queryKey: ['teams', orgId] })
    },
  })
}
