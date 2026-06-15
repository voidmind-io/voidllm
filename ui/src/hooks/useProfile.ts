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

export interface ChangePasswordInput {
  current_password: string
  new_password: string
}

export function useChangePassword() {
  return useMutation({
    mutationFn: (data: ChangePasswordInput) =>
      apiClient<void>('/me/password', {
        method: 'POST',
        body: JSON.stringify(data),
      }),
  })
}
