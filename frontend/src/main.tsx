import { MutationCache, QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { StrictMode, useEffect } from 'react';
import { createRoot } from 'react-dom/client';
import { RouterProvider, createBrowserRouter, createHashRouter, type RouteObject } from 'react-router';
import { ApiError } from './api';
import { startEventStream } from './api/events';
import { AppShell, NotFound } from './components/AppShell';
import { describeError, needsSettings } from './lib/describeError';
import { pushToast } from './lib/toast';
import { zh } from './i18n/zh';
import { JobsPage } from './pages/Jobs';
import { LibraryPage } from './pages/Library';
import { SettingsPage } from './pages/Settings';
import { SetupPage } from './pages/Setup';
import { VideoDetailPage } from './pages/VideoDetail';
import { platform } from './platform';
import './styles/global.css';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Client errors will not fix themselves; retry only transport / server failures, once.
      retry: (count, error) => count < 1 && !(error instanceof ApiError && error.status < 500),
      refetchOnWindowFocus: false,
    },
  },
  // Failed actions surface as a toast wherever they were triggered. Queries
  // show their error inline, next to a retry button.
  mutationCache: new MutationCache({
    onError(error) {
      if (error instanceof ApiError && error.unauthorized) return;
      const view = describeError(error);
      pushToast({
        kind: 'error',
        message: view.message,
        detail: view.hint ?? undefined,
        action: needsSettings(view) ? { label: zh.errors.goSettings, to: '/settings' } : undefined,
      });
    },
  }),
});

const routes: RouteObject[] = [
  {
    path: '/',
    element: <AppShell />,
    children: [
      { index: true, element: <LibraryPage /> },
      { path: 'videos/:id', element: <VideoDetailPage /> },
      { path: 'jobs', element: <JobsPage /> },
      { path: 'settings', element: <SettingsPage /> },
      { path: 'setup', element: <SetupPage /> },
      { path: '*', element: <NotFound /> },
    ],
  },
];

// Same routes in both modes; only the URL encoding differs (see Platform.router).
const router = platform.router === 'hash' ? createHashRouter(routes) : createBrowserRouter(routes);

function App() {
  useEffect(() => startEventStream(queryClient, platform), []);
  return (
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  );
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
