import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface ModelResponse {
  id: string
  name: string
  provider: string
  base_url: string
  max_context_tokens: number
  input_price_per_1m: number
  output_price_per_1m: number
  azure_deployment?: string
  azure_api_version?: string
  timeout?: string
  is_active: boolean
  source: string
  aliases: string[]
  created_at: string
  updated_at: string
}

interface PaginatedModels {
  data: ModelResponse[]
  has_more: boolean
  next_cursor?: string
}

export interface CreateModelParams {
  name: string
  provider: string
  base_url: string
  api_key?: string
  max_context_tokens?: number
  input_price_per_1m?: number
  output_price_per_1m?: number
  azure_deployment?: string
  azure_api_version?: string
  timeout?: string
  aliases?: string[]
}

export interface UpdateModelParams {
  name?: string
  provider?: string
  base_url?: string
  api_key?: string
  max_context_tokens?: number
  input_price_per_1m?: number
  output_price_per_1m?: number
  azure_deployment?: string
  azure_api_version?: string
  timeout?: string
  aliases?: string[]
}

export function useModels(cursor?: string) {
  const params = new URLSearchParams({ limit: '50', include_inactive: 'true' })
  if (cursor) params.set('cursor', cursor)
  return useQuery({
    queryKey: ['models', cursor],
    queryFn: () => apiClient<PaginatedModels>(`/models?${params}`),
  })
}

export function useCreateModel() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (params: CreateModelParams) =>
      apiClient<ModelResponse>('/models', {
        method: 'POST',
        body: JSON.stringify(params),
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['models'] })
    },
  })
}

export function useDeleteModel() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (modelId: string) =>
      apiClient<void>(`/models/${modelId}`, { method: 'DELETE' }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['models'] })
    },
  })
}

export function useUpdateModel() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ modelId, params }: { modelId: string; params: UpdateModelParams }) =>
      apiClient<ModelResponse>(`/models/${modelId}`, {
        method: 'PATCH',
        body: JSON.stringify(params),
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['models'] })
    },
  })
}

export function useToggleModel() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ modelId, activate }: { modelId: string; activate: boolean }) =>
      apiClient<ModelResponse>(`/models/${modelId}/${activate ? 'activate' : 'deactivate'}`, {
        method: 'PATCH',
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['models'] })
    },
  })
}
