import { QueryClient } from '@tanstack/react-query'
import { ApiError } from '../api/client'

/** Application-wide React Query client, shared so non-React code can clear the cache on logout. */
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: (failureCount: number, error: unknown) =>
        failureCount < 1 && !(error instanceof ApiError && error.status < 500),
      staleTime: 30_000,
    },
  },
})
