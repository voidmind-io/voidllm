import { useQuery } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface BudgetWarning {
  window: string
  scope: string
  limit: number
  usage: number
  percent_used: number
}

export interface DashboardStats {
  scope: 'org' | 'team' | 'user'
  active_keys: number
  total_teams?: number
  total_members?: number
  requests_24h: number
  tokens_24h: number
  cost_estimate_24h: number
  budget_warnings?: BudgetWarning[]
  models_healthy: number
  models_unhealthy: number
  models_degraded: number
}

export function useDashboardStats() {
  return useQuery({
    queryKey: ['dashboard-stats'],
    queryFn: () => apiClient<DashboardStats>('/dashboard/stats'),
    refetchInterval: 60_000,
    refetchIntervalInBackground: false,
  })
}
