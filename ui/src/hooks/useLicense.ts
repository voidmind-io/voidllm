import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface LicenseInfo {
  edition: string        // "community", "pro", "enterprise"
  valid: boolean         // true if license is currently active
  features: string[]     // ["audit_logs", "otel_tracing", ...]
  expires_at?: string    // RFC3339, omitted for community
  max_orgs: number       // -1 = unlimited
  max_teams: number      // -1 = unlimited
  customer_id?: string   // only visible to admins
}

export function useLicense() {
  return useQuery({
    queryKey: ['license'],
    queryFn: () => apiClient<LicenseInfo>('/license'),
    staleTime: 5 * 60 * 1000, // 5 min — license doesn't change often
  })
}

export function useActivateLicense() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (key: string) =>
      apiClient('/settings/license', {
        method: 'PUT',
        body: JSON.stringify({ key }),
      }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['license'] }),
  })
}
