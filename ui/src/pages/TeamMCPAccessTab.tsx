import { useState, useMemo } from 'react'
import { useParams } from 'react-router-dom'
import { useMe } from '../hooks/useMe'
import { useTeamMCPAccess, useSetTeamMCPAccess, useOrgMCPAccess, useAvailableGlobalMCPServers } from '../hooks/useMCPAccess'
import { Button } from '../components/ui/Button'
import { StatCard } from '../components/ui/StatCard'
import { useToast } from '../hooks/useToast'
import { cn } from '../lib/utils'

export default function TeamMCPAccessTab() {
  const { teamId = '' } = useParams<{ teamId: string }>()
  const { data: me } = useMe()
  const orgId = me?.org_id ?? ''

  const { data: availableServers, isLoading: serversLoading } = useAvailableGlobalMCPServers(orgId)
  const { data: orgAccess, isLoading: orgAccessLoading } = useOrgMCPAccess(orgId)
  const { data: teamAccess, isLoading: teamAccessLoading } = useTeamMCPAccess(orgId, teamId)
  const setAccess = useSetTeamMCPAccess(orgId, teamId)
  const { toast } = useToast()

  // null means "no unsaved edits — derive from server data".
  // Once the user makes a change this is populated with a copy of the server
  // set and subsequent toggles update it. On save success it resets to null.
  const [pendingServers, setPendingServers] = useState<Set<string> | null>(null)

  // The displayed selection: pending edits take priority over server data.
  const serverSet = useMemo(() => new Set(teamAccess?.servers ?? []), [teamAccess?.servers])
  const selectedServers = pendingServers ?? serverSet
  const isDirty = pendingServers !== null

  // Global servers filtered to only those the org has explicitly allowed.
  const orgAllowedSet = useMemo(() => new Set(orgAccess?.servers ?? []), [orgAccess?.servers])

  const eligibleServers = useMemo(() => {
    const servers = availableServers ?? []
    // If the org allowlist is non-empty, restrict to only those servers.
    if (orgAllowedSet.size > 0) {
      return servers.filter((s) => orgAllowedSet.has(s.id))
    }
    return servers
  }, [availableServers, orgAllowedSet])

  const orgHasNoServers = !orgAccessLoading && orgAllowedSet.size === 0

  function toggleServer(id: string) {
    setPendingServers((prev) => {
      const base = prev ?? new Set(teamAccess?.servers ?? [])
      const next = new Set(base)
      if (next.has(id)) {
        next.delete(id)
      } else {
        next.add(id)
      }
      return next
    })
  }

  function handleSave() {
    if (!pendingServers) return
    setAccess.mutate(Array.from(pendingServers), {
      onSuccess: () => {
        toast({ variant: 'success', message: 'Team MCP access updated' })
        setPendingServers(null)
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to update team MCP access',
        })
      },
    })
  }

  const isLoading = serversLoading || orgAccessLoading || teamAccessLoading
  const allowedValue = isLoading ? '—' : selectedServers.size === 0 ? 'All' : String(selectedServers.size)

  return (
    <div className="space-y-6">
      {/* Stat cards */}
      <div className="grid grid-cols-2 gap-4">
        <StatCard
          label="Allowed Servers"
          value={allowedValue}
          iconColor="purple"
          icon={
            <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth={1.75} viewBox="0 0 24 24" aria-hidden="true">
              <path strokeLinecap="round" strokeLinejoin="round" d="M5.25 14.25h13.5m-13.5 0a3 3 0 0 1-3-3m3 3a3 3 0 1 0 6 0m-6 0H3.375A1.875 1.875 0 0 1 1.5 12.375V7.875A1.875 1.875 0 0 1 3.375 6h17.25A1.875 1.875 0 0 1 22.5 7.875v4.5A1.875 1.875 0 0 1 20.625 14.25H18.75m-13.5 0a3 3 0 0 0 3 3h3a3 3 0 0 0 3-3m-9 0H3.375" />
            </svg>
          }
        />
        <StatCard
          label="Org-Enabled Servers"
          value={isLoading ? '—' : eligibleServers.length}
          iconColor="blue"
          icon={
            <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth={1.75} viewBox="0 0 24 24" aria-hidden="true">
              <path strokeLinecap="round" strokeLinejoin="round" d="M12 21a9.004 9.004 0 0 0 8.716-6.747M12 21a9.004 9.004 0 0 1-8.716-6.747M12 21c2.485 0 4.5-4.03 4.5-9S14.485 3 12 3m0 18c-2.485 0-4.5-4.03-4.5-9S9.515 3 12 3m0 0a8.997 8.997 0 0 1 7.843 4.582M12 3a8.997 8.997 0 0 0-7.843 4.582m15.686 0A11.953 11.953 0 0 1 12 10.5c-2.998 0-5.74-1.1-7.843-2.918m15.686 0A8.959 8.959 0 0 1 21 12c0 .778-.099 1.533-.284 2.253m0 0A17.919 17.919 0 0 1 12 16.5c-3.162 0-6.133-.815-8.716-2.247m0 0A9.015 9.015 0 0 1 3 12c0-1.605.42-3.113 1.157-4.418" />
            </svg>
          }
        />
      </div>

      {/* Section header */}
      <div>
        <h3 className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-1">
          MCP Server Allowlist
        </h3>
        <p className="text-sm text-text-secondary">
          Restrict which MCP servers this team can access. An empty selection means the team inherits
          all servers allowed at the org level. The team can only restrict within what the org permits.
        </p>
      </div>

      {/* Info: org has no servers enabled */}
      {orgHasNoServers && !isLoading && (
        <div className="rounded-xl border border-border bg-bg-secondary px-4 py-3">
          <p className="text-sm text-text-secondary">
            No global MCP servers are enabled at the organization level. Configure org-level access first.
          </p>
        </div>
      )}

      {/* Info: team inherits all org servers */}
      {!orgHasNoServers && selectedServers.size === 0 && !isLoading && (
        <div className="rounded-lg border border-border bg-bg-secondary px-4 py-3">
          <p className="text-sm text-text-tertiary">
            All org-accessible MCP servers are available (no team restrictions).
          </p>
        </div>
      )}

      {/* Server checklist — hidden when org has no servers */}
      {!orgHasNoServers && (
        <div className="rounded-xl border border-border bg-bg-secondary divide-y divide-border/50">
          {isLoading
            ? Array.from({ length: 3 }).map((_, i) => (
                <div key={i} className="flex items-center gap-3 py-3 px-4">
                  <div className="h-4 w-4 rounded bg-bg-tertiary animate-pulse shrink-0" />
                  <div className="h-4 w-48 rounded bg-bg-tertiary animate-pulse" />
                  <div className="ml-auto h-4 w-24 rounded bg-bg-tertiary animate-pulse" />
                </div>
              ))
            : eligibleServers.length === 0
              ? (
                <div className="py-12 text-center">
                  <svg className="w-8 h-8 text-text-tertiary mx-auto mb-3" fill="none" stroke="currentColor" strokeWidth={1.5} viewBox="0 0 24 24" aria-hidden="true">
                    <path strokeLinecap="round" strokeLinejoin="round" d="M5.25 14.25h13.5m-13.5 0a3 3 0 0 1-3-3m3 3a3 3 0 1 0 6 0m-6 0H3.375A1.875 1.875 0 0 1 1.5 12.375V7.875A1.875 1.875 0 0 1 3.375 6h17.25A1.875 1.875 0 0 1 22.5 7.875v4.5A1.875 1.875 0 0 1 20.625 14.25H18.75m-13.5 0a3 3 0 0 0 3 3h3a3 3 0 0 0 3-3m-9 0H3.375" />
                  </svg>
                  <p className="text-sm text-text-tertiary">No MCP servers available for this team.</p>
                </div>
              )
              : eligibleServers.map((server) => {
                  const isSelected = selectedServers.has(server.id)
                  return (
                    <label
                      key={server.id}
                      className={cn(
                        'flex items-center gap-3 py-3 px-4 cursor-pointer transition-colors duration-150',
                        'hover:bg-bg-tertiary first:rounded-t-xl last:rounded-b-xl',
                        isSelected && 'bg-accent/5',
                      )}
                    >
                      <input
                        type="checkbox"
                        checked={isSelected}
                        onChange={() => toggleServer(server.id)}
                        className="accent-accent h-4 w-4 shrink-0 cursor-pointer"
                      />
                      <span className="text-sm text-text-primary flex-1 font-medium">
                        {server.name}
                      </span>
                      {server.alias && (
                        <span className="text-xs text-text-tertiary font-mono">
                          {server.alias}
                        </span>
                      )}
                    </label>
                  )
                })}
        </div>
      )}

      <div className="flex justify-end">
        <Button
          onClick={handleSave}
          loading={setAccess.isPending}
          disabled={!isDirty || !orgId || !teamId || orgHasNoServers}
        >
          Save Changes
        </Button>
      </div>
    </div>
  )
}
