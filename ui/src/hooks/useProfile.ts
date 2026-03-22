import { useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export function useUpdateProfile() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ userId, params }: { userId: string; params: Record<string, string> }) =>
      apiClient(`/users/${userId}`, {
        method: 'PATCH',
        body: JSON.stringify(params),
      }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['me'] }),
  })
}
