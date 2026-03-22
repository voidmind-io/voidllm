export const LOCAL_STORAGE_KEY = 'voidllm_session'

/** Maps backend key_type values to their display prefixes. */
export const KEY_PREFIXES: Record<string, string> = {
  user_key: 'vl_uk_',
  team_key: 'vl_tk_',
  sa_key: 'vl_sa_',
  session_key: 'vl_sk_',
} as const

export type KeyType = 'user_key' | 'team_key' | 'sa_key' | 'session_key'
