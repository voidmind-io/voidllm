import { useQuery } from '@tanstack/react-query'
import apiClient from '../api/client'

export interface AuditEvent {
  id: string
  timestamp: string
  org_id: string
  actor_id: string
  actor_type: string
  actor_key_id: string
  action: string
  resource_type: string
  resource_id: string
  description: string
  ip_address: string
  status_code: number
}

export interface AuditLogResponse {
  data: AuditEvent[]
  has_more: boolean
  cursor?: string
}

export interface AuditLogParams {
  orgId: string
  resourceType: string
  action: string
  from: string
  to: string
  limit: number
  cursor: string
}

export function useAuditLog(params: AuditLogParams) {
  const { orgId, resourceType, action, from, to, limit, cursor } = params

  const query = new URLSearchParams({
    org_id: orgId,
    limit: String(limit),
  })
  if (cursor) query.set('cursor', cursor)
  if (resourceType) query.set('resource_type', resourceType)
  if (action) query.set('action', action)
  if (from) query.set('from', from)
  if (to) query.set('to', to)

  return useQuery({
    queryKey: ['audit-log', orgId, resourceType, action, from, to, limit, cursor],
    queryFn: () => apiClient<AuditLogResponse>(`/audit-logs?${query.toString()}`),
    enabled: !!orgId,
    staleTime: 30_000,
  })
}
