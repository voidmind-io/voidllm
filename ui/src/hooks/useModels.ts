import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface DeploymentResponse {
  id: string
  model_id: string
  name: string
  provider: string
  base_url: string
  azure_deployment?: string
  azure_api_version?: string
  aws_region?: string
  bedrock_model_id?: string
  weight: number
  priority: number
  is_active: boolean
  created_at: string
  updated_at: string
}

export interface ModelResponse {
  id: string
  name: string
  type: string
  provider: string
  base_url: string
  max_context_tokens: number
  input_price_per_1m: number
  output_price_per_1m: number
  azure_deployment?: string
  azure_api_version?: string
  aws_region?: string
  bedrock_model_id?: string
  timeout?: string
  is_active: boolean
  source: string
  aliases: string[]
  created_at: string
  updated_at: string
  strategy?: string
  max_retries?: number
  deployments?: DeploymentResponse[]
}

interface PaginatedModels {
  data: ModelResponse[]
  has_more: boolean
  next_cursor?: string
}

export interface CreateModelParams {
  name: string
  type: string
  provider?: string
  base_url?: string
  api_key?: string
  max_context_tokens?: number
  input_price_per_1m?: number
  output_price_per_1m?: number
  azure_deployment?: string
  azure_api_version?: string
  aws_access_key?: string
  aws_secret_key?: string
  aws_session_token?: string
  aws_region?: string
  bedrock_model_id?: string
  timeout?: string
  aliases?: string[]
  strategy?: string
  max_retries?: number
}

export interface CreateDeploymentParams {
  name: string
  provider: string
  base_url: string
  api_key?: string
  azure_deployment?: string
  azure_api_version?: string
  aws_access_key?: string
  aws_secret_key?: string
  aws_session_token?: string
  aws_region?: string
  bedrock_model_id?: string
  weight?: number
  priority?: number
}

export interface UpdateDeploymentParams {
  name?: string
  provider?: string
  base_url?: string
  api_key?: string
  azure_deployment?: string
  azure_api_version?: string
  aws_access_key?: string
  aws_secret_key?: string
  aws_session_token?: string
  aws_region?: string
  bedrock_model_id?: string
  weight?: number
  priority?: number
}

export interface UpdateModelParams {
  name?: string
  type?: string
  provider?: string
  base_url?: string
  api_key?: string
  max_context_tokens?: number
  input_price_per_1m?: number
  output_price_per_1m?: number
  azure_deployment?: string
  azure_api_version?: string
  aws_access_key?: string
  aws_secret_key?: string
  aws_session_token?: string
  aws_region?: string
  bedrock_model_id?: string
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

export function useCreateDeployment() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ modelId, params }: { modelId: string; params: CreateDeploymentParams }) =>
      apiClient<DeploymentResponse>(`/models/${modelId}/deployments`, {
        method: 'POST',
        body: JSON.stringify(params),
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['models'] })
    },
  })
}

export function useUpdateDeployment() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ modelId, deploymentId, params }: { modelId: string; deploymentId: string; params: UpdateDeploymentParams }) =>
      apiClient<DeploymentResponse>(`/models/${modelId}/deployments/${deploymentId}`, {
        method: 'PATCH',
        body: JSON.stringify(params),
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['models'] })
    },
  })
}

export function useDeleteDeployment() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ modelId, deploymentId }: { modelId: string; deploymentId: string }) =>
      apiClient<void>(`/models/${modelId}/deployments/${deploymentId}`, {
        method: 'DELETE',
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['models'] })
    },
  })
}
