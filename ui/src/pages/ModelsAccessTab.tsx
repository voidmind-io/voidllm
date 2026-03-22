import { useState, useMemo } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import apiClient from '../api/client'
import { useMe } from '../hooks/useMe'
import { useModels } from '../hooks/useModels'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
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
    <div className="space-y-4">
      <p className="text-sm text-text-secondary">
        Model access control for your organization. An empty allowlist means all models are accessible.
      </p>

      {selectedModels.size === 0 && !isLoading && (
        <div className="rounded-lg border border-border bg-bg-secondary px-4 py-3">
          <p className="text-sm text-text-tertiary">
            All models are accessible (no restrictions).
          </p>
        </div>
      )}

      <div className="rounded-lg border border-border bg-bg-secondary divide-y divide-border">
        {isLoading
          ? Array.from({ length: 4 }).map((_, i) => (
              <div key={i} className="flex items-center gap-3 py-2 px-3">
                <div className="h-4 w-4 rounded bg-bg-tertiary animate-pulse shrink-0" />
                <div className="h-4 w-40 rounded bg-bg-tertiary animate-pulse" />
              </div>
            ))
          : allModels.length === 0
            ? (
              <div className="py-8 text-center">
                <p className="text-sm text-text-tertiary">No models configured.</p>
              </div>
            )
            : allModels.map((model) => {
                const providerKey = isKnownProvider(model.provider) ? model.provider : 'custom'
                return (
                  <label
                    key={model.id}
                    className={cn(
                      'flex items-center gap-3 py-2 px-3 cursor-pointer transition-colors duration-150',
                      'hover:bg-bg-tertiary first:rounded-t-lg last:rounded-b-lg',
                    )}
                  >
                    <input
                      type="checkbox"
                      checked={selectedModels.has(model.name)}
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
