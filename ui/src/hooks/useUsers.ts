import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

// UserResponse is the wire shape returned by /users/:user_id.
export interface UserResponse {
  id: string
  email: string
  display_name: string
  auth_provider: string
  is_system_admin: boolean
  created_at: string
  updated_at: string
  deleted_at?: string | null
}

export interface CreateUserParams {
  email: string
  display_name: string
  password: string
  is_system_admin?: boolean
}

export interface PaginatedUsers {
  data: UserResponse[]
  has_more: boolean
  next_cursor?: string
}

export function useUser(userId: string) {
  return useQuery({
    queryKey: ['user', userId],
    queryFn: () => apiClient<UserResponse>(`/users/${userId}`),
    enabled: !!userId,
    staleTime: 5 * 60 * 1000,
  })
}

export function useUsers(cursor?: string) {
  return useQuery({
    queryKey: ['users-list', cursor],
    queryFn: () =>
      apiClient<PaginatedUsers>(
        `/users?limit=20${cursor ? `&cursor=${cursor}` : ''}`,
      ),
  })
}

export function useCreateUser() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (params: CreateUserParams) =>
      apiClient<UserResponse>('/users', {
        method: 'POST',
        body: JSON.stringify(params),
      }),
    onSuccess: (user) => {
      queryClient.setQueryData(['user', user.id], user)
      queryClient.invalidateQueries({ queryKey: ['users-list'] })
    },
  })
}

export function useDeleteUser() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (userId: string) =>
      apiClient<void>(`/users/${userId}`, { method: 'DELETE' }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['users-list'] }),
  })
}
