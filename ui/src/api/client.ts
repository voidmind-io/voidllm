import { LOCAL_STORAGE_KEY } from '../lib/constants'
import { clearSession } from '../lib/session'
import { queryClient } from '../lib/queryClient'
import { loginUrlWithNext } from '../lib/nextPath'

const DEFAULT_TIMEOUT_MS = 15_000

/** Error thrown for non-OK API responses, carrying the HTTP status. */
export class ApiError extends Error {
  status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

/** Request options; timeoutMs defaults to 15s, null disables the timeout. */
export type ApiClientOptions = RequestInit & { timeoutMs?: number | null }

const apiClient = async <T>(endpoint: string, options?: ApiClientOptions): Promise<T> => {
  const { timeoutMs, signal: callerSignal, ...init } = options ?? {}
  const key = localStorage.getItem(LOCAL_STORAGE_KEY) ?? ''

  const timeoutSignal =
    timeoutMs === null ? null : AbortSignal.timeout(timeoutMs ?? DEFAULT_TIMEOUT_MS)
  const signals: AbortSignal[] = []
  if (callerSignal) signals.push(callerSignal)
  if (timeoutSignal) signals.push(timeoutSignal)
  const signal =
    signals.length === 0 ? undefined : signals.length === 1 ? signals[0] : AbortSignal.any(signals)

  const wrapAbort = (err: unknown): unknown =>
    callerSignal?.aborted !== true && timeoutSignal?.aborted === true
      ? new ApiError(0, 'Request timed out')
      : err

  let res: Response
  try {
    res = await fetch(`/api/v1${endpoint}`, {
      ...init,
      signal,
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${key}`,
        ...init.headers,
      },
    })
  } catch (err) {
    throw wrapAbort(err)
  }

  if (res.status === 401) {
    clearSession()
    queryClient.clear()
    window.location.href = loginUrlWithNext()
    throw new ApiError(401, 'Session expired')
  }

  if (!res.ok) {
    let error: unknown
    try {
      error = await res.json()
    } catch (err) {
      if (wrapAbort(err) !== err) throw wrapAbort(err)
      error = { error: { message: res.statusText } }
    }
    throw new ApiError(
      res.status,
      (error as { error?: { message?: string } })?.error?.message ?? 'Unknown error',
    )
  }

  if (res.status === 204) {
    // DELETE endpoints return no body. Callers must use apiClient<void>().
    return undefined as T
  }

  try {
    return (await res.json()) as T
  } catch (err) {
    throw wrapAbort(err)
  }
}

export default apiClient
