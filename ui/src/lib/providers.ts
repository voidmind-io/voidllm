import type { BadgeProps } from '../components/ui/Badge'

export type ProviderKey = 'openai' | 'anthropic' | 'azure' | 'vllm' | 'ollama' | 'custom'

export const providerBadgeVariant: Record<ProviderKey, NonNullable<BadgeProps['variant']>> = {
  openai: 'default',
  anthropic: 'info',
  azure: 'warning',
  vllm: 'success',
  ollama: 'success',
  custom: 'muted',
}

export function isKnownProvider(v: string): v is ProviderKey {
  return v in providerBadgeVariant
}
