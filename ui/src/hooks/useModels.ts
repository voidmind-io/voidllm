import { useQuery, useMutation, useQueryClient, type QueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface DeploymentResponse {
  id: string
  model_id: string
  name: string
  provider: string
  base_url: string
  azure_deployment?: string
  azure_api_version?: string
  region?: string
  bedrock_model_id?: string
  bedrock_auth?: 'api_key' | 'iam'
  bedrock_role_arn?: string
  weight: number
  priority: number
  is_active: boolean
  pii_filter?: boolean
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
  cached_input_price_per_1m?: number
  cache_write_price_per_1m?: number
  azure_deployment?: string
  azure_api_version?: string
  region?: string
  bedrock_model_id?: string
  bedrock_auth?: 'api_key' | 'iam'
  bedrock_role_arn?: string
  timeout?: string
  is_active: boolean
  source: string
  aliases: string[]
  created_at: string
  updated_at: string
  strategy?: string
  max_retries?: number
  fallback_model_name?: string
  pii_filter?: boolean
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
  cached_input_price_per_1m?: number
  cache_write_price_per_1m?: number
  azure_deployment?: string
  azure_api_version?: string
  region?: string
  bedrock_model_id?: string
  bedrock_auth?: 'api_key' | 'iam'
  bedrock_role_arn?: string
  timeout?: string
  aliases?: string[]
  strategy?: string
  max_retries?: number
  fallback_model_name?: string
  pii_filter?: boolean
}

export interface CreateDeploymentParams {
  name: string
  provider: string
  base_url?: string
  api_key?: string
  azure_deployment?: string
  azure_api_version?: string
  region?: string
  bedrock_model_id?: string
  bedrock_auth?: 'api_key' | 'iam'
  bedrock_role_arn?: string
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
  region?: string
  bedrock_model_id?: string
  bedrock_auth?: 'api_key' | 'iam'
  bedrock_role_arn?: string
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
  cached_input_price_per_1m?: number
  cache_write_price_per_1m?: number
  azure_deployment?: string
  azure_api_version?: string
  region?: string
  bedrock_model_id?: string
  bedrock_auth?: 'api_key' | 'iam'
  bedrock_role_arn?: string
  timeout?: string
  aliases?: string[]
  fallback_model_name?: string | null
  pii_filter?: boolean | null
}

function invalidateModelQueries(queryClient: QueryClient): void {
  for (const key of ['models', 'available-models', 'model-health', 'model-access', 'team-model-access']) {
    queryClient.invalidateQueries({ queryKey: [key] })
  }
}

export function useModels(cursor?: string) {
  const params = new URLSearchParams({ limit: '50', include_inactive: 'true' })
  if (cursor) params.set('cursor', cursor)
  return useQuery({
    queryKey: ['models', cursor],
    queryFn: ({ signal }) => apiClient<PaginatedModels>(`/models?${params}`, { signal }),
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
      invalidateModelQueries(queryClient)
    },
  })
}

export function useDeleteModel() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (modelId: string) =>
      apiClient<void>(`/models/${modelId}`, { method: 'DELETE' }),
    onSuccess: () => {
      invalidateModelQueries(queryClient)
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
      invalidateModelQueries(queryClient)
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
      invalidateModelQueries(queryClient)
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
      invalidateModelQueries(queryClient)
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
      invalidateModelQueries(queryClient)
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
      invalidateModelQueries(queryClient)
    },
  })
}
