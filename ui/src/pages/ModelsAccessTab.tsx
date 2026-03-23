import { useState, useMemo } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'
import { useMe } from '../hooks/useMe'
import { useModels } from '../hooks/useModels'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { StatCard } from '../components/ui/StatCard'
import { useToast } from '../hooks/useToast'
import { cn } from '../lib/utils'
import { providerBadgeVariant, isKnownProvider } from '../lib/providers'

// ---------------------------------------------------------------------------
// Hooks
// ---------------------------------------------------------------------------

function useOrgModelAccess(orgId: string) {
  return useQuery({
    queryKey: ['model-access', orgId],
    queryFn: () => apiClient<{ models: string[] }>(`/orgs/${orgId}/model-access`),
    enabled: !!orgId,
  })
}

function useSetOrgModelAccess(orgId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (models: string[]) =>
      apiClient<{ models: string[] }>(`/orgs/${orgId}/model-access`, {
        method: 'PUT',
        body: JSON.stringify({ models }),
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['model-access', orgId] })
    },
  })
}

// ---------------------------------------------------------------------------
// ModelsAccessTab
// ---------------------------------------------------------------------------

export default function ModelsAccessTab() {
  const { data: me } = useMe()
  const orgId = me?.org_id ?? ''

  const { data: models, isLoading: modelsLoading } = useModels()
  const { data: access, isLoading: accessLoading } = useOrgModelAccess(orgId)
  const setAccess = useSetOrgModelAccess(orgId)
  const { toast } = useToast()

  // null means "no unsaved edits — derive from server data".
  // Once the user makes a change this is populated with a copy of the server
  // set and subsequent toggles update it. On save success it resets to null.
  const [pendingModels, setPendingModels] = useState<Set<string> | null>(null)

  // The displayed selection: pending edits take priority over server data.
  const serverSet = useMemo(() => new Set(access?.models ?? []), [access?.models])
  const selectedModels = pendingModels ?? serverSet
  const isDirty = pendingModels !== null

  function toggleModel(name: string) {
    setPendingModels((prev) => {
      // First toggle: copy server baseline into local state
      const base = prev ?? new Set(access?.models ?? [])
      const next = new Set(base)
      if (next.has(name)) {
        next.delete(name)
      } else {
        next.add(name)
      }
      return next
    })
  }

  function handleSave() {
    if (!pendingModels) return
    setAccess.mutate(Array.from(pendingModels), {
      onSuccess: () => {
        toast({ variant: 'success', message: 'Access control updated' })
        // Reset to null so UI re-derives from the freshly invalidated query
        setPendingModels(null)
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to update access control',
        })
      },
    })
  }

  const isLoading = modelsLoading || accessLoading
  const allModels = models?.data ?? []

  return (
    <div className="space-y-6">
      {/* Stat cards */}
      <div className="grid grid-cols-2 gap-4">
        <StatCard
          label="Allowed Models"
          value={isLoading ? '—' : selectedModels.size === 0 ? 'All' : selectedModels.size}
          iconColor="purple"
          icon={<svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth={1.75} viewBox="0 0 24 24"><path strokeLinecap="round" strokeLinejoin="round" d="M6.429 9.75 2.25 12l4.179 2.25m0-4.5 5.571 3 5.571-3m-11.142 0L2.25 7.5 12 2.25l9.75 5.25-4.179 2.25m0 0L21.75 12l-4.179 2.25m0 0L12 16.5l-5.571-2.25m11.142 0L21.75 16.5 12 21.75 2.25 16.5l4.179-2.25" /></svg>}
        />
        <StatCard
          label="Available Models"
          value={isLoading ? '—' : allModels.length}
          iconColor="blue"
          icon={<svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth={1.75} viewBox="0 0 24 24"><path strokeLinecap="round" strokeLinejoin="round" d="M3.75 6A2.25 2.25 0 0 1 6 3.75h2.25A2.25 2.25 0 0 1 10.5 6v2.25a2.25 2.25 0 0 1-2.25 2.25H6a2.25 2.25 0 0 1-2.25-2.25V6ZM3.75 15.75A2.25 2.25 0 0 1 6 13.5h2.25a2.25 2.25 0 0 1 2.25 2.25V18a2.25 2.25 0 0 1-2.25 2.25H6A2.25 2.25 0 0 1 3.75 18v-2.25ZM13.5 6a2.25 2.25 0 0 1 2.25-2.25H18A2.25 2.25 0 0 1 20.25 6v2.25A2.25 2.25 0 0 1 18 10.5h-2.25a2.25 2.25 0 0 1-2.25-2.25V6ZM13.5 15.75a2.25 2.25 0 0 1 2.25-2.25H18a2.25 2.25 0 0 1 2.25 2.25V18A2.25 2.25 0 0 1 18 20.25h-2.25a2.25 2.25 0 0 1-2.25-2.25v-2.25Z" /></svg>}
        />
      </div>

      {/* Section header */}
      <div>
        <h3 className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-1">Model Allowlist</h3>
        <p className="text-sm text-text-secondary">
          Select which models this organization can access. An empty list means all models are available.
        </p>
      </div>

      {/* Info box when no restrictions */}
      {selectedModels.size === 0 && !isLoading && (
        <div className="rounded-xl border border-accent/20 bg-accent/5 px-4 py-3">
          <p className="text-sm text-accent">
            All models are accessible — no restrictions applied.
          </p>
        </div>
      )}

      {/* Model checklist */}
      <div className="rounded-xl border border-border bg-bg-secondary divide-y divide-border/50">
        {isLoading
          ? Array.from({ length: 4 }).map((_, i) => (
              <div key={i} className="flex items-center gap-3 py-3 px-4">
                <div className="h-4 w-4 rounded bg-bg-tertiary animate-pulse shrink-0" />
                <div className="h-4 w-40 rounded bg-bg-tertiary animate-pulse" />
              </div>
            ))
          : allModels.length === 0
            ? (
              <div className="py-12 text-center">
                <svg className="w-8 h-8 text-text-tertiary mx-auto mb-3" fill="none" stroke="currentColor" strokeWidth={1.5} viewBox="0 0 24 24"><path strokeLinecap="round" strokeLinejoin="round" d="M6.429 9.75 2.25 12l4.179 2.25m0-4.5 5.571 3 5.571-3m-11.142 0L2.25 7.5 12 2.25l9.75 5.25-4.179 2.25m0 0L21.75 12l-4.179 2.25m0 0L12 16.5l-5.571-2.25m11.142 0L21.75 16.5 12 21.75 2.25 16.5l4.179-2.25" /></svg>
                <p className="text-sm text-text-tertiary">No models configured.</p>
              </div>
            )
            : allModels.map((model) => {
                const providerKey = isKnownProvider(model.provider) ? model.provider : 'custom'
                const isSelected = selectedModels.has(model.name)
                return (
                  <label
                    key={model.id}
                    className={cn(
                      'flex items-center gap-3 py-3 px-4 cursor-pointer transition-colors duration-150',
                      'hover:bg-bg-tertiary first:rounded-t-xl last:rounded-b-xl',
                      isSelected && 'bg-accent/5',
                    )}
                  >
                    <input
                      type="checkbox"
                      checked={isSelected}
                      onChange={() => toggleModel(model.name)}
                      className="accent-accent h-4 w-4 shrink-0 cursor-pointer"
                    />
                    <span className="font-mono text-sm text-text-primary flex-1">
                      {model.name}
                    </span>
                    <Badge variant={providerBadgeVariant[providerKey]}>
                      {model.provider}
                    </Badge>
                  </label>
                )
              })}
      </div>

      <div className="flex justify-end">
        <Button
          onClick={handleSave}
          loading={setAccess.isPending}
          disabled={!isDirty || !orgId}
        >
          Save Changes
        </Button>
      </div>
    </div>
  )
}
