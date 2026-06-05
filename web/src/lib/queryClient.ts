import { QueryClient } from "@tanstack/react-query";

// Single client for the SPA. Sensible defaults: one retry, short cache, no
// refetch-on-focus thrash.
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: 1,
      staleTime: 30_000,
      refetchOnWindowFocus: false,
    },
  },
});
