import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface SSOConfigResponse {
  enabled: boolean
  issuer: string
  client_id: string
  has_secret: boolean
  redirect_url: string
  scopes: string[]
  allowed_domains: string[]
  auto_provision: boolean
  default_role: string
  group_sync: boolean
  group_claim: string
}

export interface SaveSSOConfigParams {
  enabled: boolean
  issuer: string
  client_id: string
  client_secret?: string
  redirect_url: string
  scopes: string[]
  allowed_domains: string[]
  auto_provision: boolean
  default_role: string
  group_sync: boolean
  group_claim: string
}

export interface SSOTestResult {
  success: boolean
  message: string
  issuer?: string
  authorization_endpoint?: string
}

/** Fetch per-org SSO config. Returns undefined (404) when no org-level config is set. */
export function useOrgSSO(orgId: string) {
  return useQuery({
    queryKey: ['org-sso', orgId],
    queryFn: () => apiClient<SSOConfigResponse>(`/orgs/${orgId}/sso`),
    enabled: !!orgId,
    retry: false,
  })
}

/** Fetch global SSO config — system_admin only. */
export function useGlobalSSO() {
  return useQuery({
    queryKey: ['global-sso'],
    queryFn: () => apiClient<SSOConfigResponse>('/settings/sso'),
    retry: false,
  })
}

/** Create or update the per-org SSO config (PUT). */
export function useSaveOrgSSO(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (params: SaveSSOConfigParams) =>
      apiClient<SSOConfigResponse>(`/orgs/${orgId}/sso`, {
        method: 'PUT',
        body: JSON.stringify(params),
      }),
    onSuccess: (data) => {
      queryClient.setQueryData(['org-sso', orgId], data)
    },
  })
}

/** Remove the per-org SSO config (DELETE) — reverts to global config. */
export function useDeleteOrgSSO(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: () =>
      apiClient<void>(`/orgs/${orgId}/sso`, { method: 'DELETE' }),
    onSuccess: () => {
      queryClient.removeQueries({ queryKey: ['org-sso', orgId] })
    },
  })
}

/** Test the OIDC discovery endpoint for the given org. */
export function useTestSSOConnection(orgId: string) {
  return useMutation({
    mutationFn: (issuer: string) =>
      apiClient<SSOTestResult>(`/orgs/${orgId}/sso/test`, {
        method: 'POST',
        body: JSON.stringify({ issuer }),
      }),
  })
}
