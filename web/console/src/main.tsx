import React from 'react'
import ReactDOM from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createRouter } from '@tanstack/react-router'
import { routeTree } from './routeTree.gen'
// Design CSS — Bathysphere. Pages target the global class names emitted here.
//
// fonts.css first, then the tokens: @import must lead a stylesheet, and the
// tokens now live in @atlantis/shared, which deliberately carries no font
// load. See web/shared/tokens.css for why the palette does not decide that.
import '@/styles/fonts.css'
// Tailwind before the hand-written sheets. Its rules land in cascade layers
// and everything below is unlayered, so console.css and pages.css keep
// winning a tie; what this ordering fixes is preflight, which has to reach
// the elements those sheets then style.
import '@/styles/tailwind.css'
import '@atlantis/shared/tokens.css'
import '@atlantis/shared/datum.css'
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
