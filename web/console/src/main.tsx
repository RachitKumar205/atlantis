import React from 'react'
import ReactDOM from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createRouter } from '@tanstack/react-router'
import { routeTree } from './routeTree.gen'
// Design CSS — Bathysphere. Three files copied verbatim from the
// handoff bundle; pages target the global class names emitted here.
import '@/styles/tokens.css'
import '@/styles/console.css'
import '@/styles/pages.css'
// Thin compat shim so legacy CSS-module imports still resolve while
// pages are being migrated to literal class names.
import '@/styles/globals.css'

// ---------------------------------------------------------------------------
// QueryClient — shared singleton
// ---------------------------------------------------------------------------
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 15_000,
      retry: (failureCount, error) => {
        // Don't retry auth errors
        if ((error as { status?: number })?.status === 401) return false
        if ((error as { status?: number })?.status === 403) return false
        return failureCount < 2
      },
    },
  },
})

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------
const router = createRouter({
  routeTree,
  context: {
    queryClient,
  },
  defaultPreload: 'intent',
  defaultPreloadStaleTime: 0,
})

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

// ---------------------------------------------------------------------------
// App root
// ---------------------------------------------------------------------------
function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}

const rootEl = document.getElementById('root')!
ReactDOM.createRoot(rootEl).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)
