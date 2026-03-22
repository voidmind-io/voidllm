import { LOCAL_STORAGE_KEY } from '../lib/constants'

const apiClient = async <T>(endpoint: string, options?: RequestInit): Promise<T> => {
  const key = localStorage.getItem(LOCAL_STORAGE_KEY) ?? ''
  const res = await fetch(`/api/v1${endpoint}`, {
    ...options,
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${key}`,
      ...options?.headers,
    },
  })

  if (res.status === 401) {
    localStorage.removeItem(LOCAL_STORAGE_KEY)
    window.location.href = '/login'
    throw new Error('Session expired')
  }

  if (!res.ok) {
    const error = await res.json().catch(() => ({ error: { message: res.statusText } }))
    throw new Error(
      (error as { error?: { message?: string } })?.error?.message ?? 'Unknown error',
    )
  }

  if (res.status === 204) {
    // DELETE endpoints return no body. Callers must use apiClient<void>().
    return undefined as T
  }

  return res.json() as Promise<T>
}

export default apiClient
