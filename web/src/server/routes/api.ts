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
 * API proxy routes
 *
 * Proxies requests to the Hub API with authentication
 */

import Router from '@koa/router';
import type { Context, Next } from 'koa';

import type { AppConfig } from '../config.js';
import { getDebugLogger } from '../middleware/logger.js';

const router = new Router();

/**
 * Creates the API proxy router
 */
export function createApiRouter(config: AppConfig): Router {
  const debug = getDebugLogger(config.debug);

  /**
   * Proxy all /api/* requests to Hub API
   */
  router.all('/(.*)', async (ctx: Context, _next: Next) => {
    const params = ctx.params as Record<string, string>;
    const targetPath = params['0'] || '';
    const hubUrl = `${config.hubApiUrl}/api/v1/${targetPath}`;

    debug.log(`API Proxy: ${ctx.method} ${ctx.path} -> ${hubUrl}`);

    try {
      // Build request options
      const requestId = (ctx.state as { requestId?: string }).requestId || '';
      const headers: Record<string, string> = {
        'Content-Type': 'application/json',
        'X-Request-ID': requestId,
        'X-Forwarded-For': ctx.ip,
      };

      // Forward authorization in priority order:
      // 1. Client-provided authorization header (passthrough)
      // 2. Dev token (development mode)
      // 3. Hub access token from session (OAuth-authenticated users)
      if (ctx.headers.authorization) {
        headers['Authorization'] = ctx.headers.authorization;
        debug.log(`API Proxy: Using client-provided Authorization header`);
      } else if (ctx.state.devToken) {
        // Inject dev token for development authentication
        headers['Authorization'] = `Bearer ${ctx.state.devToken}`;
        debug.log(`API Proxy: Using dev token`);
      } else if (ctx.session?.hubAccessToken) {
        // Use Hub-issued access token from OAuth login (Option A from server-auth-design.md)
        headers['Authorization'] = `Bearer ${ctx.session.hubAccessToken}`;
        debug.log(`API Proxy: Using Hub access token from session`);

        // TODO: Check if token needs refresh before expiry
        // This would require making a refresh call to Hub if token is about to expire
      } else {
        debug.log(`API Proxy: No authorization available`, {
          hasAuthHeader: !!ctx.headers.authorization,
          hasDevToken: !!ctx.state.devToken,
          hasSession: !!ctx.session,
          hasUser: !!ctx.session?.user,
          userEmail: ctx.session?.user?.email,
          hasHubToken: !!ctx.session?.hubAccessToken,
          hubTokenExpiry: ctx.session?.hubTokenExpiry,
          sessionKeys: ctx.session ? Object.keys(ctx.session) : [],
        });
      }

      // Forward cookies if present (for session-based auth)
      if (ctx.headers.cookie) {
        headers['Cookie'] = ctx.headers.cookie;
      }

      // Build fetch options
      const fetchOptions: RequestInit = {
        method: ctx.method,
        headers,
      };

      // Include body for methods that support it
      if (ctx.method !== 'GET' && ctx.method !== 'HEAD') {
        const body = ctx.request.body;
        if (body && Object.keys(body).length > 0) {
          fetchOptions.body = JSON.stringify(body);
        }
      }

      // Add query string to URL
      const url = new URL(hubUrl);
      const queryParams = ctx.querystring;
      if (queryParams) {
        url.search = queryParams;
      }

      debug.log(`API Proxy request`, {
        url: url.toString(),
        method: ctx.method,
        headers: Object.keys(headers),
      });

      // Make the request to Hub API
      const response = await fetch(url.toString(), fetchOptions);

      // Copy status
      ctx.status = response.status;

      // Forward relevant headers
      const headersToForward = [
        'content-type',
        'x-ratelimit-limit',
        'x-ratelimit-remaining',
        'x-ratelimit-reset',
        'cache-control',
        'etag',
        'last-modified',
      ];

      for (const header of headersToForward) {
        const value = response.headers.get(header);
        if (value) {
          ctx.set(header, value);
        }
      }

      // Get response body
      const contentType = response.headers.get('content-type') || '';

      if (contentType.includes('application/json')) {
        const data: unknown = await response.json();
        debug.log(`API Proxy response: ${ctx.status}`, {
          preview: JSON.stringify(data).substring(0, 200),
        });
        ctx.body = data;
      } else {
        const text = await response.text();
        debug.log(`API Proxy response: ${ctx.status} (${text.length} chars)`);
        ctx.body = text;
      }
    } catch (error) {
      debug.error('API Proxy error', error);

      // Handle network errors
      if (error instanceof TypeError && error.message.includes('fetch')) {
        ctx.status = 502;
        ctx.body = {
          error: 'Bad Gateway',
          message: 'Failed to connect to Hub API',
          details: config.debug ? String(error) : undefined,
        };
        return;
      }

      // Handle timeout errors
      if (error instanceof Error && error.name === 'AbortError') {
        ctx.status = 504;
        ctx.body = {
          error: 'Gateway Timeout',
          message: 'Hub API request timed out',
        };
        return;
      }

      // Handle other errors
      ctx.status = 500;
      ctx.body = {
        error: 'Internal Server Error',
        message: 'An unexpected error occurred',
        details: config.debug ? String(error) : undefined,
      };
    }
  });

  return router;
}

export { router as apiRouter };
