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
 * Session middleware
 *
 * Configures koa-session for session management with secure cookie settings
 */

import type Koa from 'koa';
import type { Context, Next } from 'koa';
import session from 'koa-session';

import type { AppConfig } from '../config.js';
import type { User } from '../../shared/types.js';

/** Debug flag for session middleware */
let sessionDebugEnabled = false;

/**
 * Enable or disable session debug logging
 */
export function setSessionDebug(enabled: boolean): void {
  sessionDebugEnabled = enabled;
}

/**
 * Debug logger for session middleware
 */
function sessionDebug(message: string, data?: Record<string, unknown>): void {
  if (!sessionDebugEnabled) return;
  const timestamp = new Date().toISOString();
  if (data) {
    console.log(`[SESSION ${timestamp}] ${message}`, JSON.stringify(data, null, 2));
  } else {
    console.log(`[SESSION ${timestamp}] ${message}`);
  }
}

/**
 * Session data stored in the session
 */
export interface SessionData {
  /** Authenticated user */
  user?: User;
  /** OAuth return URL after login */
  returnTo?: string;
  /** OAuth state for CSRF protection */
  oauthState?: string;
  /** Hub-issued access token (for API calls to Hub) */
  hubAccessToken?: string;
  /** Hub-issued refresh token (for refreshing access token) */
  hubRefreshToken?: string;
  /** Hub access token expiry timestamp (ms since epoch) */
  hubTokenExpiry?: number;
}

/**
 * Augment Koa's session type with our custom data
 */
declare module 'koa-session' {
  interface Session extends SessionData {}
}

/**
 * Session configuration options
 */
export interface SessionConfig {
  /** Session key (cookie name) */
  key: string;
  /** Max age in milliseconds */
  maxAge: number;
  /** Whether to use secure cookies (HTTPS only) */
  secure: boolean;
  /** HTTP only cookies (not accessible via JavaScript) */
  httpOnly: boolean;
  /** SameSite attribute */
  sameSite: 'strict' | 'lax' | 'none';
  /** Whether cookies are signed */
  signed: boolean;
}

/**
 * Get session configuration from app config
 */
export function getSessionConfig(config: AppConfig): SessionConfig {
  return {
    // Note: Cookie names cannot contain colons per RFC 6265
    key: 'scion_sess',
    maxAge: config.session.maxAge,
    secure: config.production,
    httpOnly: true,
    sameSite: 'lax',
    signed: true,
  };
}

/**
 * Create session middleware
 *
 * @param app - Koa application instance
 * @param config - Application configuration
 * @returns Session middleware
 */
export function createSessionMiddleware(app: Koa, config: AppConfig): Koa.Middleware {
  // Enable debug if config says so
  if (config.debug) {
    setSessionDebug(true);
  }

  // Validate session secret in production
  if (config.production && !config.session.secret) {
    throw new Error('SESSION_SECRET must be set in production');
  }

  // Set app keys for signed cookies
  app.keys = [config.session.secret];

  const sessionConfig = getSessionConfig(config);

  sessionDebug('Session middleware configured', {
    key: sessionConfig.key,
    maxAge: sessionConfig.maxAge,
    secure: sessionConfig.secure,
    sameSite: sessionConfig.sameSite,
    secretLength: config.session.secret?.length || 0,
  });

  // Create session middleware with koa-session options
  const sessionMiddleware = session(
    {
      key: sessionConfig.key,
      maxAge: sessionConfig.maxAge,
      httpOnly: sessionConfig.httpOnly,
      signed: sessionConfig.signed,
      secure: sessionConfig.secure,
      sameSite: sessionConfig.sameSite,
      // Enable auto commit
      autoCommit: true,
      // Renew session if more than half the maxAge has passed
      renew: true,
    },
    app
  );

  // Wrap with debug logging
  return async function sessionMiddlewareWithDebug(ctx: Context, next: Next) {
    const cookieHeader = ctx.headers.cookie || '';
    const hasSessionCookie = cookieHeader.includes(sessionConfig.key);

    sessionDebug(`Session middleware: ${ctx.method} ${ctx.path}`, {
      hasSessionCookie,
      cookieNames: cookieHeader
        .split(';')
        .map((c) => c.trim().split('=')[0])
        .filter(Boolean),
    });

    // Run the actual session middleware
    await sessionMiddleware(ctx, async () => {
      sessionDebug(`Session loaded`, {
        path: ctx.path,
        isNew: ctx.session?.isNew,
        hasUser: !!ctx.session?.user,
        userEmail: ctx.session?.user?.email,
        hasHubToken: !!ctx.session?.hubAccessToken,
        hubTokenExpiresIn: ctx.session?.hubTokenExpiry
          ? Math.round((ctx.session.hubTokenExpiry - Date.now()) / 1000) + 's'
          : 'n/a',
        keys: ctx.session ? Object.keys(ctx.session) : [],
      });

      await next();

      sessionDebug(`Session after handler`, {
        path: ctx.path,
        hasUser: !!ctx.session?.user,
        userEmail: ctx.session?.user?.email,
      });
    });
  };
}
