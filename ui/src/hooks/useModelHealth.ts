import { useQuery } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface ModelHealthInfo {
  name: string
  status: 'healthy' | 'degraded' | 'unhealthy' | 'unknown'
  latency_ms: number
  last_check: string
  last_error?: string
  health_ok: boolean | null
  models_ok: boolean | null
  functional_ok: boolean | null
}

interface ModelHealthResponse {
  models: ModelHealthInfo[]
}

export function useModelHealth() {
  return useQuery({
    queryKey: ['model-health'],
    queryFn: () => apiClient<ModelHealthResponse>('/models/health'),
    refetchInterval: 15_000, // refresh every 15s for near-realtime health
  })
}
