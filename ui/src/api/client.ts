import { LOCAL_STORAGE_KEY } from '../lib/constants'
import { clearSession } from '../lib/session'
import { queryClient } from '../lib/queryClient'

/** Error thrown for non-OK API responses, carrying the HTTP status. */
export class ApiError extends Error {
  status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

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
    clearSession()
    queryClient.clear()
    window.location.href = '/login'
    throw new ApiError(401, 'Session expired')
  }

  if (!res.ok) {
    const error = await res.json().catch(() => ({ error: { message: res.statusText } }))
    throw new ApiError(
      res.status,
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
