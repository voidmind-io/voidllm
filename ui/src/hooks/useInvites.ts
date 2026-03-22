import { useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface InviteResponse {
  id: string
  token?: string
  token_hint: string
  email: string
  role: string
  org_id: string
  status: string
  expires_at: string
  created_at: string
}

export function useCreateInvite(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (params: { email: string; role: string }) =>
      apiClient<InviteResponse>(`/orgs/${orgId}/invites`, {
        method: 'POST',
        body: JSON.stringify(params),
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['org-members', orgId] })
    },
  })
}
