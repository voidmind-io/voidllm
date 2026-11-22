import { QueryClient } from '@tanstack/react-query'

/** Application-wide React Query client, shared so non-React code can clear the cache on logout. */
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: 1,
      staleTime: 30_000,
    },
  },
})
