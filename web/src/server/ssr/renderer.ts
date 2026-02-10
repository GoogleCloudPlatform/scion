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
 * Lit SSR Renderer
 *
 * Renders Lit components server-side and wraps in HTML shell
 */

import { render } from '@lit-labs/ssr';
import { html, type TemplateResult } from 'lit';
import { collectResult } from '@lit-labs/ssr/lib/render-result.js';

import type { PageData, User } from '../../shared/types.js';
import { getHtmlTemplate, getPageTitle } from './templates.js';

// Import components for server-side rendering
// These must be imported so they're registered before rendering

// App shell (imports shared components internally)
import '../../components/app-shell.js';

// Shared components (explicit imports for SSR registration)
import '../../components/shared/nav.js';
import '../../components/shared/header.js';
import '../../components/shared/breadcrumb.js';
import '../../components/shared/status-badge.js';

// Page components
import '../../components/pages/home.js';
import '../../components/pages/groves.js';
import '../../components/pages/grove-detail.js';
import '../../components/pages/agents.js';
import '../../components/pages/agent-detail.js';
import '../../components/pages/not-found.js';
import '../../components/pages/login.js';
import '../../components/pages/unauthorized.js';

export interface RenderContext {
  /** Current URL path */
  url: string;
  /** Authenticated user (if any) */
  user?: User | undefined;
  /** Additional data to pass to the page */
  data?: Record<string, unknown> | undefined;
  /** Auth configuration for login page */
  authConfig?:
    | {
        googleEnabled: boolean;
        githubEnabled: boolean;
      }
    | undefined;
}

/**
 * Renders a page with the app shell and page content
 */
export async function renderPage(ctx: RenderContext): Promise<string> {
  const { url, user, data, authConfig } = ctx;
  const pageTitle = getPageTitle(url);

  // Create the initial page data for hydration
  const initialData: PageData = {
    path: url,
    title: pageTitle,
    user,
    data,
  };

  // Login page is rendered without the app shell
  if (url === '/login') {
    const loginTemplate = html`
      <scion-login-page
        ?googleEnabled=${authConfig?.googleEnabled ?? false}
        ?githubEnabled=${authConfig?.githubEnabled ?? false}
      ></scion-login-page>
    `;

    const componentHtml = await collectResult(render(loginTemplate));

    return getHtmlTemplate({
      title: 'Sign In',
      content: componentHtml,
      initialData,
      scripts: ['/assets/main.js'],
      styles: ['/assets/main.css'],
    });
  }

  // Unauthorized page is rendered without the app shell
  if (url === '/unauthorized' || url.startsWith('/unauthorized?')) {
    const unauthorizedTemplate = html`
      <scion-page-unauthorized .pageData=${initialData}></scion-page-unauthorized>
    `;

    const componentHtml = await collectResult(render(unauthorizedTemplate));

    return getHtmlTemplate({
      title: 'Access Denied',
      content: componentHtml,
      initialData,
      scripts: ['/assets/main.js'],
      styles: ['/assets/main.css'],
    });
  }

  // Determine which page to render based on URL
  const pageContent = getPageTemplate(url, initialData);

  // Render the full app shell with page content
  const appTemplate = html`
    <scion-app .user=${user} .currentPath=${url}> ${pageContent} </scion-app>
  `;

  // Collect the rendered HTML
  const componentHtml = await collectResult(render(appTemplate));

  // Wrap in HTML shell with hydration scripts
  const fullHtml = getHtmlTemplate({
    title: pageTitle,
    content: componentHtml,
    initialData,
    scripts: ['/assets/main.js'],
    styles: ['/assets/main.css'],
  });

  return fullHtml;
}

/**
 * Renders just a page component (without the shell) for client-side navigation
 */
export async function renderPageContent(
  url: string,
  data?: Record<string, unknown>
): Promise<string> {
  const pageTitle = getPageTitle(url);
  const pageData: PageData = {
    path: url,
    title: pageTitle,
    data,
  };

  const pageContent = getPageTemplate(url, pageData);
  return await collectResult(render(pageContent));
}

/**
 * Gets the appropriate page template based on URL
 */
function getPageTemplate(url: string, pageData: PageData): TemplateResult {
  // Route matching
  if (url === '/' || url === '') {
    return html`<scion-page-home .pageData=${pageData}></scion-page-home>`;
  }

  if (url === '/groves') {
    return html`<scion-page-groves .pageData=${pageData}></scion-page-groves>`;
  }

  // Grove detail: /groves/:groveId
  const groveDetailMatch = url.match(/^\/groves\/([^/]+)$/);
  if (groveDetailMatch) {
    const groveId = groveDetailMatch[1];
    return html`<scion-page-grove-detail
      .pageData=${pageData}
      .groveId=${groveId}
    ></scion-page-grove-detail>`;
  }

  // Grove agents: /groves/:groveId/agents (redirect to grove detail)
  const groveAgentsMatch = url.match(/^\/groves\/([^/]+)\/agents$/);
  if (groveAgentsMatch) {
    const groveId = groveAgentsMatch[1];
    return html`<scion-page-grove-detail
      .pageData=${pageData}
      .groveId=${groveId}
    ></scion-page-grove-detail>`;
  }

  if (url === '/agents') {
    return html`<scion-page-agents .pageData=${pageData}></scion-page-agents>`;
  }

  // Agent terminal: /agents/:agentId/terminal (placeholder - will be implemented in M8)
  const terminalMatch = url.match(/^\/agents\/([^/]+)\/terminal$/);
  if (terminalMatch) {
    const agentId = terminalMatch[1];
    return html`<scion-page-agent-detail
      .pageData=${pageData}
      .agentId=${agentId}
    ></scion-page-agent-detail>`;
  }

  // Agent detail: /agents/:agentId
  const agentDetailMatch = url.match(/^\/agents\/([^/]+)$/);
  if (agentDetailMatch) {
    const agentId = agentDetailMatch[1];
    return html`<scion-page-agent-detail
      .pageData=${pageData}
      .agentId=${agentId}
    ></scion-page-agent-detail>`;
  }

  // 404 for unmatched routes
  return html`<scion-page-404 .pageData=${pageData}></scion-page-404>`;
}

/**
 * Check if a URL should be handled by the SPA router
 * (as opposed to static files or API routes)
 */
export function isSpaRoute(url: string): boolean {
  // Skip static assets
  if (url.startsWith('/assets/')) return false;
  if (url.startsWith('/healthz')) return false;
  if (url.startsWith('/readyz')) return false;
  if (url.startsWith('/api/')) return false;
  if (url.startsWith('/auth/')) return false;
  if (url.startsWith('/events')) return false;

  // Skip file extensions
  const ext = url.split('.').pop();
  if (ext && ['js', 'css', 'png', 'jpg', 'svg', 'ico', 'json', 'txt'].includes(ext)) {
    return false;
  }

  return true;
}
