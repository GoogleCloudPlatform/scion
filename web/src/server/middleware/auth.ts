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
 * Authentication middleware
 *
 * Provides route protection and user context for authenticated routes.
 * Works alongside dev-auth middleware - dev-auth sets user if dev token is present,
 * this middleware enforces authentication for protected routes.
 */

import type { Context, Next } from 'koa';

import type { User } from '../../shared/types.js';
import type { AppConfig } from '../config.js';

/** Debug flag for auth middleware */
let authDebugEnabled = false;

/**
 * Enable or disable auth debug logging
 */
export function setAuthDebug(enabled: boolean): void {
  authDebugEnabled = enabled;
}

/**
 * Debug logger for auth middleware
 */
function authDebug(message: string, data?: Record<string, unknown>): void {
  if (!authDebugEnabled) return;
  const timestamp = new Date().toISOString();
  if (data) {
    console.log(`[AUTH ${timestamp}] ${message}`, JSON.stringify(data, null, 2));
  } else {
    console.log(`[AUTH ${timestamp}] ${message}`);
  }
}

/**
 * Extended Koa state with auth information
 */
export interface AuthState {
  /** Currently authenticated user */
  user?: User;
  /** Dev token (if dev auth is enabled) */
  devToken?: string;
  /** Whether dev auth is enabled */
  devAuthEnabled?: boolean;
  /** Request ID for tracing */
  requestId?: string;
}

/**
 * Check if a URL should be protected (require authentication)
 */
function isProtectedRoute(url: string): boolean {
  // Public routes that don't require authentication
  const publicPaths = [
    '/healthz',
    '/readyz',
    '/login',
    '/unauthorized',
    '/auth/login',
    '/auth/callback',
    '/auth/error',
    '/auth/debug', // Debug endpoint (only available when debug mode is enabled)
    '/assets/',
    '/favicon.ico',
  ];

  // Check if the URL matches any public path
  for (const path of publicPaths) {
    if (url.startsWith(path)) {
      return false;
    }
  }

  return true;
}

/**
 * Check if the request accepts HTML (is a browser request)
 */
function acceptsHtml(ctx: Context): boolean {
  const accept = ctx.headers.accept || '';
  return accept.includes('text/html');
}

/**
 * Create auth middleware that enforces authentication on protected routes
 *
 * @param config - Application configuration
 * @returns Koa middleware function
 */
export function createAuthMiddleware(config: AppConfig) {
  // Enable debug if config says so
  if (config.debug) {
    setAuthDebug(true);
  }

  return async function authMiddleware(ctx: Context, next: Next) {
    const state = ctx.state as AuthState;
    const requestId = state.requestId || 'unknown';

    authDebug(`Request: ${ctx.method} ${ctx.path}`, {
      requestId,
      hasSession: !!ctx.session,
      sessionKeys: ctx.session ? Object.keys(ctx.session) : [],
      hasSessionUser: !!ctx.session?.user,
      hasStateUser: !!state.user,
      hasDevToken: !!state.devToken,
      cookies: ctx.headers.cookie ? 'present' : 'missing',
    });

    // Check if route needs protection
    if (!isProtectedRoute(ctx.path)) {
      authDebug(`Route ${ctx.path} is public, skipping auth`);
      return next();
    }

    // Check if user is authenticated (either from dev-auth or session)
    let user: User | undefined = state.user;

    authDebug(`Auth check for protected route`, {
      path: ctx.path,
      stateUserPresent: !!state.user,
      stateUserEmail: state.user?.email,
    });

    // If no user from dev-auth, check session
    if (!user && ctx.session?.user) {
      user = ctx.session.user;
      authDebug(`Found user in session`, {
        userId: user?.id,
        userEmail: user?.email,
      });
      if (user) {
        state.user = user;
      }
    }

    // If still no user, handle unauthenticated request
    if (!user) {
      authDebug(`No authenticated user found`, {
        path: ctx.path,
        isApiRequest: ctx.path.startsWith('/api/'),
        sessionExists: !!ctx.session,
        sessionIsNew: ctx.session?.isNew,
      });

      // Store the original URL for redirect after login
      if (ctx.session) {
        ctx.session.returnTo = ctx.originalUrl;
      }

      // For API requests, return 401
      if (ctx.path.startsWith('/api/')) {
        authDebug(`Returning 401 for API request ${ctx.path}`);
        ctx.status = 401;
        ctx.body = {
          error: 'Unauthorized',
          message: 'Authentication required',
        };
        return;
      }

      // For browser requests, redirect to login
      if (acceptsHtml(ctx)) {
        authDebug(`Redirecting to login for browser request ${ctx.path}`);
        ctx.redirect('/auth/login');
        return;
      }

      // Default: 401 response
      authDebug(`Returning 401 for non-browser request ${ctx.path}`);
      ctx.status = 401;
      ctx.body = {
        error: 'Unauthorized',
        message: 'Authentication required',
      };
      return;
    }

    authDebug(`User authenticated, continuing`, {
      userId: user.id,
      userEmail: user.email,
    });

    // User is authenticated - continue
    await next();
  };
}

/**
 * Validate that a user's email domain is authorized
 *
 * @param email - User email address
 * @param authorizedDomains - List of authorized email domains
 * @returns true if authorized, false otherwise
 */
export function isEmailAuthorized(email: string, authorizedDomains: string[]): boolean {
  // If no domains are configured, allow all
  if (!authorizedDomains || authorizedDomains.length === 0) {
    return true;
  }

  // Extract domain from email
  const atIndex = email.lastIndexOf('@');
  if (atIndex === -1) {
    return false;
  }

  const domain = email.substring(atIndex + 1).toLowerCase();

  // Check if domain is in the authorized list
  return authorizedDomains.some((authorized) => authorized.toLowerCase() === domain);
}
