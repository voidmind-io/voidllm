import { useQuery } from '@tanstack/react-query'
import apiClient from '../api/client'
import { resolveRange } from '../lib/timeRange'
import type { UsageRange } from '../lib/timeRange'

export interface AuditEvent {
  id: string
  timestamp: string
  org_id: string
  actor_id: string
  actor_type: string
  actor_name?: string
  actor_key_id: string
  action: string
  resource_type: string
  resource_id: string
  description: string
  ip_address: string
  status_code: number
}

export interface AuditWindow {
  from: string
  to: string
}

export interface AuditLogResponse {
  data: AuditEvent[]
  has_more: boolean
  cursor?: string
}

/** Audit log page plus the time window it was fetched with. */
export interface AuditLogPage extends AuditLogResponse {
  window: AuditWindow
}

export interface AuditLogParams {
  orgId: string
  resourceType: string
  action: string
  /** Range descriptor, resolved to a fresh window on every fetch of page 1. */
  range: UsageRange
  /** Window of page 1, required while paging with a cursor so pages stay consistent. */
  frozenWindow?: AuditWindow | null
  limit: number
  cursor: string
}

export function useAuditLog(params: AuditLogParams) {
  const { orgId, resourceType, action, range, frozenWindow, limit, cursor } = params
  const pinned = cursor && frozenWindow ? frozenWindow : null

  return useQuery({
    queryKey: pinned
      ? ['audit-log', orgId, resourceType, action, 'window', pinned.from, pinned.to, limit, cursor]
      : ['audit-log', orgId, resourceType, action, range, limit, cursor],
    queryFn: async (): Promise<AuditLogPage> => {
      const window = pinned ?? resolveRange(range)
      const query = new URLSearchParams({ org_id: orgId, limit: String(limit) })
      if (cursor) query.set('cursor', cursor)
      if (resourceType) query.set('resource_type', resourceType)
      if (action) query.set('action', action)
      query.set('from', window.from)
      query.set('to', window.to)
      const res = await apiClient<AuditLogResponse>(`/audit-logs?${query.toString()}`)
      return { ...res, window }
    },
    enabled: !!orgId,
    staleTime: cursor ? 30_000 : 0,
  })
}
