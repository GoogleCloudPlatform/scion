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
 * Development Authentication Middleware
 *
 * Implements auto-login for development mode using the dev-token from ~/.scion/dev-token
 * Based on the dev-auth design in .design/hosted/dev-auth.md
 */

import type { Context, Next } from 'koa';
import { readFileSync, existsSync } from 'fs';
import { join } from 'path';
import { homedir } from 'os';

import type { User } from '../../shared/types.js';

/**
 * Development user constant
 * Matches the DevUser from the Go implementation
 */
export const DEV_USER: User = {
  id: 'dev-user',
  email: 'dev@localhost',
  name: 'Development User',
};

/**
 * Dev auth configuration
 */
export interface DevAuthConfig {
  /** Whether dev auth is enabled */
  enabled: boolean;
  /** Explicit dev token (overrides file) */
  token?: string;
  /** Path to token file (default: ~/.scion/dev-token) */
  tokenFile?: string;
}

/**
 * Dev auth state stored in Koa context
 */
export interface DevAuthState {
  /** The dev token loaded at startup */
  devToken: string | null;
  /** Whether dev auth is enabled */
  devAuthEnabled: boolean;
}

/**
 * Resolves the dev token from environment or file
 *
 * Resolution order:
 * 1. SCION_DEV_TOKEN environment variable
 * 2. Explicit token in config
 * 3. Token file (SCION_DEV_TOKEN_FILE env or ~/.scion/dev-token)
 */
export function resolveDevToken(config?: DevAuthConfig): string | null {
  // Priority 1: Environment variable
  const envToken = process.env.SCION_DEV_TOKEN;
  if (envToken) {
    return envToken.trim();
  }

  // Priority 2: Explicit config token
  if (config?.token) {
    return config.token.trim();
  }

  // Determine token file path
  let tokenFile = config?.tokenFile || process.env.SCION_DEV_TOKEN_FILE;
  if (!tokenFile) {
    tokenFile = join(homedir(), '.scion', 'dev-token');
  }

  // Priority 3: Token file
  try {
    if (existsSync(tokenFile)) {
      const token = readFileSync(tokenFile, 'utf-8').trim();
      if (token) {
        return token;
      }
    }
  } catch {
    // File doesn't exist or can't be read
  }

  return null;
}

/**
 * Checks if a token is a development token (has the scion_dev_ prefix)
 */
export function isDevToken(token: string): boolean {
  return token.startsWith('scion_dev_');
}

/**
 * Creates the dev auth middleware
 *
 * When dev auth is enabled and a token is available:
 * - Sets ctx.state.user to the development user
 * - Sets ctx.state.devToken for use by the API proxy
 */
export function devAuth(devToken: string | null) {
  return async function devAuthMiddleware(ctx: Context, next: Next) {
    // Store dev auth state for other middleware
    ctx.state.devToken = devToken;
    ctx.state.devAuthEnabled = !!devToken;

    // If we have a dev token, auto-login the user
    if (devToken) {
      ctx.state.user = DEV_USER;
    }

    await next();
  };
}

/**
 * Initialize dev auth and return the middleware
 *
 * Logs appropriate messages about dev auth status
 */
export function initDevAuth(config?: DevAuthConfig): {
  middleware: ReturnType<typeof devAuth>;
  token: string | null;
  enabled: boolean;
} {
  // Check if dev auth should be enabled
  // In development mode, enable by default unless explicitly disabled
  const isProduction = process.env.NODE_ENV === 'production';
  const envEnabled = process.env.SCION_DEV_AUTH_ENABLED;

  let enabled: boolean;
  if (config?.enabled !== undefined) {
    enabled = config.enabled;
  } else if (envEnabled !== undefined) {
    enabled = envEnabled.toLowerCase() === 'true' || envEnabled === '1';
  } else {
    // Default: enabled in development, disabled in production
    enabled = !isProduction;
  }

  if (!enabled) {
    console.log('Dev auth disabled');
    return {
      middleware: devAuth(null),
      token: null,
      enabled: false,
    };
  }

  // Resolve the dev token
  const token = resolveDevToken(config);

  if (token) {
    console.log('');
    console.log('\x1b[33m%s\x1b[0m', '='.repeat(60));
    console.log(
      '\x1b[33m%s\x1b[0m',
      'WARNING: Development authentication enabled - not for production'
    );
    console.log('\x1b[33m%s\x1b[0m', '='.repeat(60));
    console.log('');
    console.log('Dev token:', token.substring(0, 20) + '...');
    console.log('Auto-login user:', DEV_USER.name, `<${DEV_USER.email}>`);
    console.log('');
  } else {
    console.log('');
    console.log('\x1b[33m%s\x1b[0m', 'Dev auth enabled but no token found');
    console.log('Expected token file at: ~/.scion/dev-token');
    console.log('Or set SCION_DEV_TOKEN environment variable');
    console.log('');
    console.log('The Hub API will generate a token on startup.');
    console.log('Copy it from the Hub output or cat ~/.scion/dev-token');
    console.log('');
  }

  return {
    middleware: devAuth(token),
    token,
    enabled: true,
  };
}
