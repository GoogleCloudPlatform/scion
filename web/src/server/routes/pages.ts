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
 * Page routes for SSR
 *
 * Handles rendering of all SPA pages server-side
 */

import Router from '@koa/router';
import type { Context } from 'koa';

import { renderPage, isSpaRoute } from '../ssr/index.js';
import type { AppConfig } from '../config.js';

// We need to access config for auth settings
// This is set during app initialization
let appConfig: AppConfig | null = null;

/**
 * Set the app config for the page routes
 */
export function setPageRoutesConfig(config: AppConfig): void {
  appConfig = config;
}

const router = new Router();

/**
 * Catch-all route for SPA pages
 *
 * Renders pages server-side using Lit SSR
 */
router.get('(.*)', async (ctx: Context) => {
  const url = ctx.path;

  // Skip non-SPA routes (handled by other middleware)
  if (!isSpaRoute(url)) {
    return;
  }

  try {
    // Render the page server-side
    const user = ctx.state.user as import('../../shared/types.js').User | undefined;

    // Build auth config for login page
    const authConfig = appConfig
      ? {
          googleEnabled: !!appConfig.auth.googleClientId,
          githubEnabled: !!appConfig.auth.githubClientId,
        }
      : undefined;

    const html = await renderPage({
      url,
      user,
      data: {}, // Additional data can be fetched here
      authConfig,
    });

    // Set 404 status for not-found pages
    if (url !== '/' && !isKnownRoute(url)) {
      ctx.status = 404;
    } else {
      ctx.status = 200;
    }

    ctx.type = 'text/html';
    ctx.body = html;
  } catch (error) {
    console.error('SSR rendering error:', error);

    // Fallback to error page
    ctx.status = 500;
    ctx.type = 'text/html';
    ctx.body = getErrorHtml(error);
  }
});

/**
 * Check if a URL matches a known route
 */
function isKnownRoute(url: string): boolean {
  const knownRoutes = ['/', '/groves', '/agents', '/settings', '/login', '/unauthorized'];

  // Exact match
  if (knownRoutes.includes(url)) {
    return true;
  }

  // Pattern matches for dynamic routes
  const patterns = [
    /^\/groves\/[^/]+$/,
    /^\/groves\/[^/]+\/agents$/,
    /^\/agents\/[^/]+$/,
    /^\/agents\/[^/]+\/terminal$/,
  ];

  return patterns.some((pattern) => pattern.test(url));
}

/**
 * Generate error HTML for server errors
 */
function getErrorHtml(error: unknown): string {
  const message = error instanceof Error ? error.message : 'Unknown error';

  return `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Server Error - Scion</title>
    <style>
        body {
            font-family: system-ui, -apple-system, sans-serif;
            display: flex;
            align-items: center;
            justify-content: center;
            min-height: 100vh;
            margin: 0;
            background: #f8fafc;
        }
        .container {
            text-align: center;
            padding: 2rem;
        }
        .code {
            font-size: 6rem;
            font-weight: 800;
            color: #ef4444;
            margin-bottom: 1rem;
        }
        h1 {
            font-size: 1.5rem;
            color: #1e293b;
            margin-bottom: 0.5rem;
        }
        p {
            color: #64748b;
        }
        pre {
            background: #1e293b;
            color: #f8fafc;
            padding: 1rem;
            border-radius: 0.5rem;
            text-align: left;
            overflow-x: auto;
            max-width: 600px;
            margin: 1rem auto;
        }
        a {
            display: inline-block;
            margin-top: 1rem;
            padding: 0.75rem 1.5rem;
            background: #3b82f6;
            color: white;
            text-decoration: none;
            border-radius: 0.5rem;
        }
        a:hover {
            background: #2563eb;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="code">500</div>
        <h1>Server Error</h1>
        <p>Something went wrong while rendering this page.</p>
        ${process.env.NODE_ENV !== 'production' ? `<pre>${message}</pre>` : ''}
        <a href="/">Back to Dashboard</a>
    </div>
</body>
</html>`;
}

export const pageRoutes = router;
