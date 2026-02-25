/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * Client entry point
 *
 * Handles client-side routing and real-time state management via SSE.
 */

import type { PageData, User } from '../shared/types.js';
import { stateManager } from './state.js';

// Import Shoelace base path config (needed for icons).
// Icons are copied to public/shoelace/ by scripts/copy-shoelace-icons.mjs
// so they are available under both the Vite dev server and the Go server.
import { setBasePath } from '@shoelace-style/shoelace/dist/utilities/base-path.js';
setBasePath('/shoelace');

// Explicitly import all Shoelace components used in the app.
// The autoloader cannot detect sl-* elements inside LitElement shadow roots,
// so each component must be registered via direct import.
import '@shoelace-style/shoelace/dist/components/breadcrumb/breadcrumb.js';
import '@shoelace-style/shoelace/dist/components/breadcrumb-item/breadcrumb-item.js';
import '@shoelace-style/shoelace/dist/components/button/button.js';
import '@shoelace-style/shoelace/dist/components/drawer/drawer.js';
import '@shoelace-style/shoelace/dist/components/icon/icon.js';
import '@shoelace-style/shoelace/dist/components/icon-button/icon-button.js';
import '@shoelace-style/shoelace/dist/components/input/input.js';
import '@shoelace-style/shoelace/dist/components/option/option.js';
import '@shoelace-style/shoelace/dist/components/select/select.js';
import '@shoelace-style/shoelace/dist/components/spinner/spinner.js';
import '@shoelace-style/shoelace/dist/components/textarea/textarea.js';
import '@shoelace-style/shoelace/dist/components/tooltip/tooltip.js';

// Import all components for client-side hydration and routing
// App shell (imports shared components internally)
import '../components/app-shell.js';

// Shared components (also imported by app-shell, but explicit for clarity)
import '../components/shared/nav.js';
import '../components/shared/header.js';
import '../components/shared/breadcrumb.js';
import '../components/shared/status-badge.js';
import '../components/shared/debug-panel.js';

// Page components
import '../components/pages/home.js';
import '../components/pages/groves.js';
import '../components/pages/grove-create.js';
import '../components/pages/grove-detail.js';
import '../components/pages/agents.js';
import '../components/pages/agent-detail.js';
import '../components/pages/agent-create.js';
import '../components/pages/grove-create.js';
import '../components/pages/terminal.js';
import '../components/pages/brokers.js';
import '../components/pages/not-found.js';
import '../components/pages/login.js';

/** Current authenticated user, fetched once on init */
let currentUser: User | null = null;

/**
 * Fetch the current authenticated user from the backend session.
 * Returns null if not authenticated.
 */
async function fetchCurrentUser(): Promise<User | null> {
  try {
    const res = await fetch('/auth/me', { credentials: 'include' });
    if (!res.ok) return null;
    const data = await res.json();
    return {
      id: data.id,
      email: data.email,
      name: data.displayName || data.name || '',
      avatar: data.avatarUrl || data.avatar,
    };
  } catch {
    return null;
  }
}

/**
 * Route configuration mapping URL patterns to page component tag names
 */
const ROUTES: { pattern: RegExp; tag: string }[] = [
  { pattern: /^\/login$/, tag: 'scion-login-page' },
  { pattern: /^\/$/, tag: 'scion-page-home' },
  { pattern: /^\/groves$/, tag: 'scion-page-groves' },
  { pattern: /^\/agents$/, tag: 'scion-page-agents' },
  { pattern: /^\/brokers$/, tag: 'scion-page-brokers' },
  { pattern: /^\/groves\/new$/, tag: 'scion-page-grove-create' },
  { pattern: /^\/groves\/[^/]+$/, tag: 'scion-page-grove-detail' },
  { pattern: /^\/agents\/new$/, tag: 'scion-page-agent-create' },
  { pattern: /^\/agents\/[^/]+\/terminal$/, tag: 'scion-page-terminal' },
  { pattern: /^\/agents\/[^/]+$/, tag: 'scion-page-agent-detail' },
];

/**
 * Routes that render without the app shell (full-page layout)
 */
const STANDALONE_ROUTES = new Set(['scion-login-page']);

/**
 * Initialize the client-side application
 */
async function init(): Promise<void> {
  console.info('[Scion] Initializing client...');

  // Get initial data from SSR and hydrate state manager
  const initialData = getInitialData();
  if (initialData) {
    console.info('[Scion] Initial page data:', initialData.path);
    if (initialData.user) {
      currentUser = initialData.user;
    }
    if (initialData.data) {
      stateManager.hydrate(
        initialData.data as {
          agents?: import('../shared/types.js').Agent[];
          groves?: import('../shared/types.js').Grove[];
        }
      );
    }
  }

  // Fetch current user from session if not provided by SSR
  if (!currentUser) {
    currentUser = await fetchCurrentUser();
  }

  // Wait for custom elements to be defined
  await Promise.all([
    // Core components
    customElements.whenDefined('scion-app'),
    customElements.whenDefined('scion-nav'),
    customElements.whenDefined('scion-header'),
    customElements.whenDefined('scion-breadcrumb'),
    customElements.whenDefined('scion-status-badge'),
    customElements.whenDefined('scion-debug-panel'),
    // Page components
    customElements.whenDefined('scion-page-home'),
    customElements.whenDefined('scion-page-groves'),
    customElements.whenDefined('scion-page-grove-create'),
    customElements.whenDefined('scion-page-grove-detail'),
    customElements.whenDefined('scion-page-agents'),
    customElements.whenDefined('scion-page-agent-detail'),
    customElements.whenDefined('scion-page-agent-create'),
    customElements.whenDefined('scion-page-terminal'),
    customElements.whenDefined('scion-page-brokers'),
    customElements.whenDefined('scion-page-404'),
    customElements.whenDefined('scion-login-page'),
  ]);

  console.info('[Scion] Components defined, setting up router...');

  // Render the initial page based on current URL
  renderRoute(window.location.pathname);

  // Setup client-side router for navigation
  setupRouter();

  // Disconnect SSE on page unload
  window.addEventListener('beforeunload', () => {
    stateManager.disconnect();
  });

  console.info('[Scion] Client initialization complete');
}

/**
 * Retrieves initial page data from SSR-injected script tag
 */
function getInitialData(): PageData | null {
  const script = document.getElementById('__SCION_DATA__');
  if (!script) {
    console.warn('[Scion] No initial data found');
    return null;
  }

  try {
    return JSON.parse(script.textContent || '{}') as PageData;
  } catch (e) {
    console.error('[Scion] Failed to parse initial data:', e);
    return null;
  }
}

/**
 * Resolves a URL path to a page component tag name
 */
function resolveRoute(path: string): string {
  for (const route of ROUTES) {
    if (route.pattern.test(path)) {
      return route.tag;
    }
  }
  return 'scion-page-404';
}

/**
 * Renders the page component for the given path into #app
 */
function renderRoute(path: string): void {
  const appContainer = document.getElementById('app');
  if (!appContainer) return;

  const tag = resolveRoute(path);

  // Clear previous content
  appContainer.innerHTML = '';

  if (STANDALONE_ROUTES.has(tag)) {
    // Standalone pages render without the app shell
    const page = document.createElement(tag);
    appContainer.appendChild(page);
  } else {
    // Wrapped pages render inside the app shell
    const shell = document.createElement('scion-app') as HTMLElement & { currentPath: string; user: User | null };
    shell.currentPath = path;
    shell.user = currentUser;
    const page = document.createElement(tag);
    shell.appendChild(page);
    appContainer.appendChild(shell);
  }
}

/**
 * Sets up the client-side router for navigation
 */
function setupRouter(): void {
  // Add click handlers for client-side navigation
  document.addEventListener('click', (e: MouseEvent) => {
    const target = e.target as HTMLElement;
    const anchor = target.closest('a');

    if (!anchor) return;

    const href = anchor.getAttribute('href');
    if (!href) return;

    // Skip external links
    if (href.startsWith('http') || href.startsWith('//')) return;

    // Skip special links
    if (href.startsWith('javascript:')) return;
    if (href.startsWith('#')) return;

    // Skip links that should trigger full page loads
    if (href.startsWith('/api/')) return;
    if (href.startsWith('/auth/')) return;
    if (href.startsWith('/events')) return;

    // Handle client-side navigation
    e.preventDefault();
    navigateTo(href);
  });

  // Handle browser back/forward
  window.addEventListener('popstate', () => {
    renderRoute(window.location.pathname);
  });
}

/**
 * Navigates to a new path using the History API
 */
function navigateTo(path: string): void {
  if (path === window.location.pathname) return;

  window.history.pushState({}, '', path);
  renderRoute(path);
}

// Initialize when DOM is ready
if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', () => {
    void init();
  });
} else {
  void init();
}

// Export for use in components and tests
export { getInitialData, navigateTo, stateManager };
