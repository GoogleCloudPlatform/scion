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
 * Handles hydration of SSR content and client-side routing
 */

// IMPORTANT: Import hydration support BEFORE any Lit components
// This enables proper hydration of SSR-rendered declarative shadow DOM
import '@lit-labs/ssr-client/lit-element-hydrate-support.js';

// @vaadin/router reserved for future use in client-side routing
// import { Router } from '@vaadin/router';

import type { PageData } from '../shared/types.js';

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
import '../components/pages/grove-detail.js';
import '../components/pages/agents.js';
import '../components/pages/agent-detail.js';
import '../components/pages/not-found.js';
import '../components/pages/login.js';

/**
 * Initialize the client-side application
 */
async function init(): Promise<void> {
  console.info('[Scion] Initializing client...');

  // Get initial data from SSR
  const initialData = getInitialData();
  if (initialData) {
    console.info('[Scion] Initial page data:', initialData.path);
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
    customElements.whenDefined('scion-page-grove-detail'),
    customElements.whenDefined('scion-page-agents'),
    customElements.whenDefined('scion-page-agent-detail'),
    customElements.whenDefined('scion-page-404'),
    customElements.whenDefined('scion-login-page'),
  ]);

  console.info('[Scion] Components defined, setting up router...');

  // Setup client-side router for navigation
  setupRouter();

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
 * Sets up the Vaadin Router for client-side navigation
 */
function setupRouter(): void {
  // Find the content outlet within the app shell
  const appShell = document.querySelector('scion-app');
  if (!appShell) {
    console.error('[Scion] App shell not found');
    return;
  }

  // The router outlet is the content slot in the app shell
  // For now, we handle navigation by updating the app shell's currentPath
  // and letting the server re-render on full navigation

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
    updateCurrentPath(window.location.pathname);
  });
}

/**
 * Navigates to a new path using the History API
 */
function navigateTo(path: string): void {
  if (path === window.location.pathname) return;

  window.history.pushState({}, '', path);
  updateCurrentPath(path);

  // For now, do a full page load to get SSR content
  // In the future, we could fetch just the page content via AJAX
  window.location.href = path;
}

/**
 * Updates the app shell's current path
 */
function updateCurrentPath(path: string): void {
  const appShell = document.querySelector('scion-app') as HTMLElement & {
    currentPath?: string;
  };
  if (appShell) {
    appShell.currentPath = path;
  }
}

// Initialize when DOM is ready
if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', () => {
    void init();
  });
} else {
  void init();
}

// Export for potential use in tests
export { getInitialData, navigateTo };
