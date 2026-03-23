import React, { useState } from 'react'
import { PageHeader } from '../components/ui/PageHeader'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Dialog, ConfirmDialog } from '../components/ui/Dialog'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Input } from '../components/ui/Input'
import { Select } from '../components/ui/Select'
import { Toggle } from '../components/ui/Toggle'
import { StatCard } from '../components/ui/StatCard'
import {
  useModels,
  useCreateModel,
  useUpdateModel,
  useDeleteModel,
  useToggleModel,
} from '../hooks/useModels'
import type { ModelResponse, CreateModelParams, UpdateModelParams } from '../hooks/useModels'
import { useToast } from '../hooks/useToast'
import { providerBadgeVariant, isKnownProvider } from '../lib/providers'
import type { ProviderKey } from '../lib/providers'
import apiClient from '../api/client'
import { cn } from '../lib/utils'

// ---------------------------------------------------------------------------
// Module-level constants
// ---------------------------------------------------------------------------

const providerLabels: Record<ProviderKey, string> = {
  openai: 'OpenAI',
  anthropic: 'Anthropic',
  azure: 'Azure',
  vllm: 'vLLM',
  ollama: 'Ollama',
  custom: 'Custom',
}

const PROVIDER_OPTIONS = [
  { value: 'openai', label: 'OpenAI' },
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'azure', label: 'Azure' },
  { value: 'vllm', label: 'vLLM' },
  { value: 'ollama', label: 'Ollama' },
  { value: 'custom', label: 'Custom' },
]

const BASE_URL_PLACEHOLDERS: Record<string, string> = {
  openai: 'https://api.openai.com/v1',
  anthropic: 'https://api.anthropic.com',
  azure: 'https://<resource>.openai.azure.com',
  vllm: 'http://localhost:8000/v1',
  ollama: 'http://localhost:11434/v1',
  custom: 'https://your-endpoint/v1',
}

// ---------------------------------------------------------------------------
// Icons
// ---------------------------------------------------------------------------

function IconLayers() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M12 2 2 7l10 5 10-5-10-5z" />
      <path d="M2 17l10 5 10-5" />
      <path d="M2 12l10 5 10-5" />
    </svg>
  )
}

function IconActivity() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <polyline points="22 12 18 12 15 21 9 3 6 12 2 12" />
    </svg>
  )
}

function IconPauseCircle() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <circle cx="12" cy="12" r="10" />
      <line x1="10" y1="15" x2="10" y2="9" />
      <line x1="14" y1="15" x2="14" y2="9" />
    </svg>
  )
}

function IconPencil() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7" />
      <path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z" />
    </svg>
  )
}

function IconTrash() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <polyline points="3 6 5 6 21 6" />
      <path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6" />
      <path d="M10 11v6" />
      <path d="M14 11v6" />
      <path d="M9 6V4a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2" />
    </svg>
  )
}

// ---------------------------------------------------------------------------
// CreateModelDialog
// ---------------------------------------------------------------------------

interface CreateModelDialogProps {
  open: boolean
  onClose: () => void
}

interface FormErrors {
  name?: string
  provider?: string
  base_url?: string
}

function CreateModelDialog({ open, onClose }: CreateModelDialogProps) {
  const [name, setName] = useState('')
  const [provider, setProvider] = useState('openai')
  const [baseUrl, setBaseUrl] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [aliases, setAliases] = useState('')
  const [maxContextTokens, setMaxContextTokens] = useState('')
  const [inputPricePer1m, setInputPricePer1m] = useState('')
  const [outputPricePer1m, setOutputPricePer1m] = useState('')
  const [azureDeployment, setAzureDeployment] = useState('')
  const [azureApiVersion, setAzureApiVersion] = useState('')
  const [timeout, setTimeout] = useState('')
  const [errors, setErrors] = useState<FormErrors>({})
  const [testResult, setTestResult] = useState<{ success: boolean; message: string } | null>(null)
  const [testing, setTesting] = useState(false)

  const createModel = useCreateModel()
  const { toast } = useToast()

  function handleClose() {
    setName('')
    setProvider('openai')
    setBaseUrl('')
    setApiKey('')
    setAliases('')
    setMaxContextTokens('')
    setInputPricePer1m('')
    setOutputPricePer1m('')
    setAzureDeployment('')
    setAzureApiVersion('')
    setTimeout('')
    setErrors({})
    setTestResult(null)
    onClose()
  }

  function handleProviderChange(value: string) {
    setProvider(value)
    setBaseUrl('')
    setTestResult(null)
  }

  async function handleTestConnection() {
    setTesting(true)
    setTestResult(null)
    try {
      const res = await apiClient<{ success: boolean; message: string }>('/models/test-connection', {
        method: 'POST',
        body: JSON.stringify({
          provider,
          base_url: baseUrl.trim(),
          api_key: apiKey.trim(),
        }),
      })
      setTestResult(res)
    } catch (err) {
      setTestResult({ success: false, message: err instanceof Error ? err.message : 'Test failed' })
    } finally {
      setTesting(false)
    }
  }

  function validate(): boolean {
    const next: FormErrors = {}

    if (!name.trim()) {
      next.name = 'Name is required'
    }
    if (!provider) {
      next.provider = 'Provider is required'
    }
    if (!baseUrl.trim()) {
      next.base_url = 'Base URL is required'
    }

    setErrors(next)
    return Object.keys(next).length === 0
  }

  function handleSubmit(e: React.FormEvent | React.MouseEvent) {
    e.preventDefault()
    if (!validate()) return

    const params: CreateModelParams = {
      name: name.trim(),
      provider,
      base_url: baseUrl.trim(),
    }

    if (apiKey.trim()) {
      params.api_key = apiKey.trim()
    }
    if (maxContextTokens.trim()) {
      const parsed = parseInt(maxContextTokens, 10)
      if (!isNaN(parsed)) params.max_context_tokens = parsed
    }
    if (inputPricePer1m.trim()) {
      const parsed = parseFloat(inputPricePer1m)
      if (!isNaN(parsed)) params.input_price_per_1m = parsed
    }
    if (outputPricePer1m.trim()) {
      const parsed = parseFloat(outputPricePer1m)
      if (!isNaN(parsed)) params.output_price_per_1m = parsed
    }
    if (provider === 'azure') {
      if (azureDeployment.trim()) params.azure_deployment = azureDeployment.trim()
      if (azureApiVersion.trim()) params.azure_api_version = azureApiVersion.trim()
    }
    if (timeout.trim()) {
      params.timeout = timeout.trim()
    }
    const parsedAliases = aliases.split(',').map((a) => a.trim()).filter(Boolean)
    if (parsedAliases.length > 0) {
      params.aliases = parsedAliases
    }

    createModel.mutate(params, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'Model added' })
        handleClose()
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to add model',
        })
      },
    })
  }

  const isAzure = provider === 'azure'

  return (
    <Dialog open={open} onClose={handleClose} title="Add Model">
      <form onSubmit={handleSubmit} className="space-y-4" noValidate>
        <Input
          label="Name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="e.g. gpt-4o"
          error={errors.name}
          disabled={createModel.isPending}
        />
        <Select
          label="Provider"
          options={PROVIDER_OPTIONS}
          value={provider}
          onChange={handleProviderChange}
          disabled={createModel.isPending}
        />
        <Input
          label="Base URL"
          value={baseUrl}
          onChange={(e) => { setBaseUrl(e.target.value); setTestResult(null) }}
          placeholder={BASE_URL_PLACEHOLDERS[provider] ?? 'https://'}
          error={errors.base_url}
          disabled={createModel.isPending}
        />
        <Input
          label="API Key"
          type="password"
          value={apiKey}
          onChange={(e) => { setApiKey(e.target.value); setTestResult(null) }}
          placeholder="sk-..."
          description="Encrypted at rest, never shown again"
          disabled={createModel.isPending}
        />
        <div className="flex items-center gap-3">
          <Button
            type="button"
            variant="secondary"
            size="sm"
            loading={testing}
            disabled={!baseUrl.trim()}
            onClick={handleTestConnection}
          >
            Test Connection
          </Button>
          {testResult && (
            <span className={cn('text-sm', testResult.success ? 'text-success' : 'text-error')}>
              {testResult.success ? '✓' : '✗'} {testResult.message}
            </span>
          )}
        </div>
        <Input
          label="Max Context Tokens"
          type="number"
          value={maxContextTokens}
          onChange={(e) => setMaxContextTokens(e.target.value)}
          placeholder="e.g. 128000"
          disabled={createModel.isPending}
        />
        <div className="grid grid-cols-2 gap-4">
          <Input
            label="Input Price per 1M tokens"
            type="number"
            value={inputPricePer1m}
            onChange={(e) => setInputPricePer1m(e.target.value)}
            placeholder="e.g. 2.50"
            disabled={createModel.isPending}
          />
          <Input
            label="Output Price per 1M tokens"
            type="number"
            value={outputPricePer1m}
            onChange={(e) => setOutputPricePer1m(e.target.value)}
            placeholder="e.g. 10.00"
            disabled={createModel.isPending}
          />
        </div>
        {isAzure && (
          <>
            <Input
              label="Azure Deployment"
              value={azureDeployment}
              onChange={(e) => setAzureDeployment(e.target.value)}
              placeholder="e.g. gpt-4o-deployment"
              disabled={createModel.isPending}
            />
            <Input
              label="Azure API Version"
              value={azureApiVersion}
              onChange={(e) => setAzureApiVersion(e.target.value)}
              placeholder="e.g. 2024-02-01"
              disabled={createModel.isPending}
            />
          </>
        )}
        <Input
          label="Timeout"
          value={timeout}
          onChange={(e) => setTimeout(e.target.value)}
          placeholder="e.g. 30s, 2m, 5m"
          description="Per-model upstream timeout. Empty = use global default."
          disabled={createModel.isPending}
        />
        <Input
          label="Aliases"
          value={aliases}
          onChange={(e) => setAliases(e.target.value)}
          placeholder="default, gpt4, latest"
          description="Comma-separated. Must be globally unique."
          disabled={createModel.isPending}
        />
        <div className="flex justify-end gap-2 pt-2">
          <Button
            variant="secondary"
            onClick={handleClose}
            disabled={createModel.isPending}
          >
            Cancel
          </Button>
          <Button onClick={handleSubmit} loading={createModel.isPending}>
            Add Model
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// EditModelDialog
// ---------------------------------------------------------------------------

interface EditModelDialogProps {
  model: ModelResponse
  onClose: () => void
}

function EditModelDialog({ model, onClose }: EditModelDialogProps) {
  const [name, setName] = useState(model.name)
  const [provider, setProvider] = useState(model.provider)
  const [baseUrl, setBaseUrl] = useState(model.base_url)
  const [apiKey, setApiKey] = useState('')
  const [aliases, setAliases] = useState((model.aliases ?? []).join(', '))
  const [maxContextTokens, setMaxContextTokens] = useState(
    model.max_context_tokens > 0 ? String(model.max_context_tokens) : '',
  )
  const [inputPrice, setInputPrice] = useState(
    model.input_price_per_1m > 0 ? String(model.input_price_per_1m) : '',
  )
  const [outputPrice, setOutputPrice] = useState(
    model.output_price_per_1m > 0 ? String(model.output_price_per_1m) : '',
  )
  const [azureDeployment, setAzureDeployment] = useState(model.azure_deployment ?? '')
  const [azureApiVersion, setAzureApiVersion] = useState(model.azure_api_version ?? '')
  const [timeout, setTimeout] = useState(model.timeout ?? '')

  const updateModel = useUpdateModel()
  const { toast } = useToast()

  const isAzure = provider === 'azure'

  function handleSubmit(e: React.FormEvent | React.MouseEvent) {
    e.preventDefault()

    const params: UpdateModelParams = {}

    if (name.trim() !== model.name) params.name = name.trim()
    if (provider !== model.provider) params.provider = provider
    if (baseUrl.trim() !== model.base_url) params.base_url = baseUrl.trim()
    if (apiKey.trim()) params.api_key = apiKey.trim()

    if (maxContextTokens.trim()) {
      const parsed = parseInt(maxContextTokens, 10)
      if (!isNaN(parsed) && parsed !== model.max_context_tokens) {
        params.max_context_tokens = parsed
      }
    } else if (model.max_context_tokens > 0) {
      params.max_context_tokens = 0
    }

    if (inputPrice.trim()) {
      const parsed = parseFloat(inputPrice)
      if (!isNaN(parsed) && parsed !== model.input_price_per_1m) {
        params.input_price_per_1m = parsed
      }
    } else if (model.input_price_per_1m > 0) {
      params.input_price_per_1m = 0
    }

    if (outputPrice.trim()) {
      const parsed = parseFloat(outputPrice)
      if (!isNaN(parsed) && parsed !== model.output_price_per_1m) {
        params.output_price_per_1m = parsed
      }
    } else if (model.output_price_per_1m > 0) {
      params.output_price_per_1m = 0
    }

    if (isAzure) {
      if (azureDeployment.trim() !== (model.azure_deployment ?? '')) {
        params.azure_deployment = azureDeployment.trim()
      }
      if (azureApiVersion.trim() !== (model.azure_api_version ?? '')) {
        params.azure_api_version = azureApiVersion.trim()
      }
    }

    const trimmedTimeout = timeout.trim()
    if (trimmedTimeout !== (model.timeout ?? '')) {
      params.timeout = trimmedTimeout || undefined
    }

    const newAliases = aliases.split(',').map((a) => a.trim()).filter(Boolean)
    const sortedNew = [...newAliases].sort()
    const sortedOld = [...(model.aliases ?? [])].sort()
    if (JSON.stringify(sortedNew) !== JSON.stringify(sortedOld)) {
      params.aliases = newAliases
    }

    if (Object.keys(params).length === 0) {
      onClose()
      return
    }

    updateModel.mutate(
      { modelId: model.id, params },
      {
        onSuccess: () => {
          toast({ variant: 'success', message: 'Model updated' })
          onClose()
        },
        onError: (err) => {
          toast({
            variant: 'error',
            message: err instanceof Error ? err.message : 'Update failed',
          })
        },
      },
    )
  }

  return (
    <Dialog open onClose={onClose} title="Edit Model">
      <form onSubmit={handleSubmit} className="space-y-4" noValidate>
        <Input
          label="Name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="e.g. gpt-4o"
          disabled={updateModel.isPending}
        />
        <Select
          label="Provider"
          options={PROVIDER_OPTIONS}
          value={provider}
          onChange={setProvider}
          disabled={updateModel.isPending}
        />
        <Input
          label="Base URL"
          value={baseUrl}
          onChange={(e) => setBaseUrl(e.target.value)}
          placeholder={BASE_URL_PLACEHOLDERS[provider] ?? 'https://'}
          disabled={updateModel.isPending}
        />
        <Input
          label="API Key"
          type="password"
          value={apiKey}
          onChange={(e) => setApiKey(e.target.value)}
          placeholder="Leave empty to keep current key"
          description="Leave empty to keep current key. Enter a new value to replace."
          disabled={updateModel.isPending}
        />
        <Input
          label="Max Context Tokens"
          type="number"
          value={maxContextTokens}
          onChange={(e) => setMaxContextTokens(e.target.value)}
          placeholder="e.g. 128000"
          disabled={updateModel.isPending}
        />
        <div className="grid grid-cols-2 gap-4">
          <Input
            label="Input Price per 1M tokens"
            type="number"
            value={inputPrice}
            onChange={(e) => setInputPrice(e.target.value)}
            placeholder="e.g. 2.50"
            disabled={updateModel.isPending}
          />
          <Input
            label="Output Price per 1M tokens"
            type="number"
            value={outputPrice}
            onChange={(e) => setOutputPrice(e.target.value)}
            placeholder="e.g. 10.00"
            disabled={updateModel.isPending}
          />
        </div>
        {isAzure && (
          <>
            <Input
              label="Azure Deployment"
              value={azureDeployment}
              onChange={(e) => setAzureDeployment(e.target.value)}
              placeholder="e.g. gpt-4o-deployment"
              disabled={updateModel.isPending}
            />
            <Input
              label="Azure API Version"
              value={azureApiVersion}
              onChange={(e) => setAzureApiVersion(e.target.value)}
              placeholder="e.g. 2024-02-01"
              disabled={updateModel.isPending}
            />
          </>
        )}
        <Input
          label="Timeout"
          value={timeout}
          onChange={(e) => setTimeout(e.target.value)}
          placeholder="e.g. 30s, 2m, 5m"
          description="Per-model upstream timeout. Empty = use global default."
          disabled={updateModel.isPending}
        />
        <Input
          label="Aliases"
          value={aliases}
          onChange={(e) => setAliases(e.target.value)}
          placeholder="default, gpt4, latest"
          description="Comma-separated. Must be globally unique."
          disabled={updateModel.isPending}
        />
        <div className="flex justify-end gap-2 pt-2">
          <Button variant="secondary" onClick={onClose} disabled={updateModel.isPending}>
            Cancel
          </Button>
          <Button onClick={handleSubmit} loading={updateModel.isPending}>
            Save Changes
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// ModelsPage
// ---------------------------------------------------------------------------

export default function ModelsPage() {
  const [showCreateDialog, setShowCreateDialog] = useState(false)
  const [editModel, setEditModel] = useState<ModelResponse | null>(null)
  const [deleteModelId, setDeleteModelId] = useState<string | null>(null)

  const { data: models, isLoading } = useModels()
  const deleteModel = useDeleteModel()
  const toggleModel = useToggleModel()
  const { toast } = useToast()

  const allModels = models?.data ?? []
  const activeCount = allModels.filter((m) => m.is_active).length
  const inactiveCount = allModels.length - activeCount

  const columns: Column<ModelResponse>[] = [
    {
      key: 'name',
      header: 'Name',
      render: (row) => (
        <span className="font-mono text-text-primary text-sm">{row.name}</span>
      ),
    },
    {
      key: 'provider',
      header: 'Provider',
      render: (row) => {
        const key = isKnownProvider(row.provider) ? row.provider : 'custom'
        return (
          <Badge variant={providerBadgeVariant[key]}>
            {providerLabels[key]}
          </Badge>
        )
      },
    },
    {
      key: 'aliases',
      header: 'Aliases',
      render: (row) => {
        const list = row.aliases ?? []
        if (list.length === 0) return <span className="text-text-tertiary">—</span>
        return (
          <div className="flex flex-wrap gap-1">
            {list.map((a) => (
              <Badge key={a} variant="muted">{a}</Badge>
            ))}
          </div>
        )
      },
    },
    {
      key: 'max_context_tokens',
      header: 'Context',
      render: (row) =>
        row.max_context_tokens > 0 ? (
          <span className="text-text-secondary">
            {row.max_context_tokens.toLocaleString()}
          </span>
        ) : (
          <span className="text-text-tertiary">—</span>
        ),
    },
    {
      key: 'source',
      header: 'Source',
      render: (row) => (
        <Badge variant={row.source === 'yaml' ? 'muted' : 'default'}>
          {row.source}
        </Badge>
      ),
    },
    {
      key: 'is_active',
      header: 'Status',
      render: (row) => (
        <Toggle
          checked={row.is_active}
          onChange={(activate) =>
            toggleModel.mutate(
              { modelId: row.id, activate },
              {
                onError: (err) => {
                  toast({
                    variant: 'error',
                    message:
                      err instanceof Error
                        ? err.message
                        : 'Failed to update model status',
                  })
                },
              },
            )
          }
          disabled={toggleModel.isPending && toggleModel.variables?.modelId === row.id}
          size="sm"
        />
      ),
    },
    {
      key: 'actions',
      header: '',
      align: 'right',
      render: (row) => {
        if (row.source !== 'api') return null
        return (
          <div className="flex items-center justify-end gap-1">
            <button
              type="button"
              onClick={() => setEditModel(row)}
              disabled={deleteModel.isPending && deleteModelId === row.id}
              title="Edit model"
              className="p-1.5 rounded-md text-text-tertiary hover:text-text-primary hover:bg-bg-tertiary transition-colors disabled:opacity-40"
            >
              <IconPencil />
            </button>
            <button
              type="button"
              onClick={() => setDeleteModelId(row.id)}
              disabled={deleteModel.isPending && deleteModelId === row.id}
              title="Delete model"
              className="p-1.5 rounded-md text-text-tertiary hover:text-error hover:bg-error/10 transition-colors disabled:opacity-40"
            >
              <IconTrash />
            </button>
          </div>
        )
      },
    },
  ]

  function handleDelete() {
    if (!deleteModelId) return
    deleteModel.mutate(deleteModelId, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'Model deleted' })
        setDeleteModelId(null)
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to delete model',
        })
        setDeleteModelId(null)
      },
    })
  }

  return (
    <>
      <PageHeader
        title="Models"
        description="System model registry"
        actions={
          <Button onClick={() => setShowCreateDialog(true)}>Add Model</Button>
        }
      />

      {/* Stat cards */}
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-4 mb-6">
        <StatCard
          label="Total Models"
          value={isLoading ? '—' : allModels.length}
          icon={<IconLayers />}
          iconColor="purple"
        />
        <StatCard
          label="Active"
          value={isLoading ? '—' : activeCount}
          icon={<IconActivity />}
          iconColor="green"
        />
        <StatCard
          label="Inactive"
          value={isLoading ? '—' : inactiveCount}
          icon={<IconPauseCircle />}
          iconColor="yellow"
        />
      </div>

      <Table<ModelResponse>
        columns={columns}
        data={allModels}
        keyExtractor={(row) => row.id}
        loading={isLoading}
        emptyMessage="No models configured"
      />

      <CreateModelDialog
        open={showCreateDialog}
        onClose={() => setShowCreateDialog(false)}
      />

      {editModel !== null && (
        <EditModelDialog
          model={editModel}
          onClose={() => setEditModel(null)}
        />
      )}

      <ConfirmDialog
        open={deleteModelId !== null}
        onClose={() => setDeleteModelId(null)}
        onConfirm={handleDelete}
        title="Delete Model"
        description="Are you sure you want to delete this model? This action cannot be undone. YAML-sourced models must be removed from the config file."
        confirmLabel="Delete"
        loading={deleteModel.isPending}
      />
    </>
  )
}
