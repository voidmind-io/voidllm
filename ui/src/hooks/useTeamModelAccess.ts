import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export function useTeamModelAccess(orgId: string, teamId: string) {
  return useQuery({
    queryKey: ['team-model-access', orgId, teamId],
    queryFn: () =>
      apiClient<{ models: string[] }>(
        `/orgs/${orgId}/teams/${teamId}/model-access`,
      ),
    enabled: !!orgId && !!teamId,
  })
}

export function useSetTeamModelAccess(orgId: string, teamId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (models: string[]) =>
      apiClient<{ models: string[] }>(
        `/orgs/${orgId}/teams/${teamId}/model-access`,
        { method: 'PUT', body: JSON.stringify({ models }) },
      ),
    onSuccess: () => {
      queryClient.invalidateQueries({
        queryKey: ['team-model-access', orgId, teamId],
      })
    },
  })
}
