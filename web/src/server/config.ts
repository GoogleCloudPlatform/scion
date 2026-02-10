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
 * Server configuration
 *
 * Handles environment-based configuration for the Koa server
 */

export interface AppConfig {
  /** Server port */
  port: number;

  /** Server hostname */
  host: string;

  /** Whether running in production mode */
  production: boolean;

  /** Whether debug mode is enabled */
  debug: boolean;

  /** Hub API URL */
  hubApiUrl: string;

  /** Base URL for OAuth callbacks */
  baseUrl: string;

  /** CORS configuration */
  cors: {
    origin: string;
    credentials: boolean;
  };

  /** Security settings */
  security: {
    /** Content Security Policy */
    csp: string;
    /** HSTS max-age in seconds */
    hstsMaxAge: number;
  };

  /** Session configuration */
  session: {
    /** Session secret for signing cookies */
    secret: string;
    /** Max age in milliseconds */
    maxAge: number;
  };

  /** Authentication configuration */
  auth: {
    /** Google OAuth client ID */
    googleClientId: string;
    /** Google OAuth client secret */
    googleClientSecret: string;
    /** GitHub OAuth client ID */
    githubClientId: string;
    /** GitHub OAuth client secret */
    githubClientSecret: string;
    /** Authorized email domains (empty = allow all) */
    authorizedDomains: string[];
  };
}

function getEnvString(key: string, defaultValue: string): string {
  return process.env[key] ?? defaultValue;
}

function getEnvNumber(key: string, defaultValue: number): number {
  const value = process.env[key];
  if (value === undefined) return defaultValue;
  const parsed = parseInt(value, 10);
  return isNaN(parsed) ? defaultValue : parsed;
}

function getEnvBoolean(key: string, defaultValue: boolean): boolean {
  const value = process.env[key];
  if (value === undefined) return defaultValue;
  return value.toLowerCase() === 'true' || value === '1';
}

function getEnvStringArray(key: string, defaultValue: string[]): string[] {
  const value = process.env[key];
  if (value === undefined) return defaultValue;
  return value
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

export function loadConfig(): AppConfig {
  const production = getEnvString('NODE_ENV', 'development') === 'production';
  const debug = getEnvBoolean('SCION_API_DEBUG', false);
  const port = getEnvNumber('PORT', 8080);
  const host = getEnvString('HOST', '0.0.0.0');

  // Determine base URL for OAuth callbacks
  const defaultBaseUrl = production
    ? '' // Must be configured in production
    : `http://localhost:${port}`;

  return {
    port,
    host,
    production,
    debug,
    hubApiUrl: getEnvString('HUB_API_URL', 'http://localhost:9810'),
    baseUrl: getEnvString('BASE_URL', defaultBaseUrl),

    cors: {
      origin: production ? getEnvString('CORS_ORIGIN', '*') : '*',
      credentials: true,
    },

    security: {
      csp: [
        "default-src 'self'",
        // Allow Shoelace from jsdelivr CDN and Web Awesome
        "script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net https://cdn.webawesome.com",
        "style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net https://cdn.webawesome.com https://fonts.googleapis.com",
        "font-src 'self' https://fonts.gstatic.com https://cdn.jsdelivr.net https://cdn.webawesome.com",
        "img-src 'self' data: https:",
        "connect-src 'self' ws: wss: http://localhost:* http://127.0.0.1:*",
      ].join('; '),
      hstsMaxAge: production ? 31536000 : 0, // 1 year in production
    },

    session: {
      secret: getEnvString(
        'SESSION_SECRET',
        production ? '' : 'scion-dev-session-secret-change-in-prod'
      ),
      maxAge: getEnvNumber('SESSION_MAX_AGE', 24 * 60 * 60 * 1000), // 24 hours
    },

    auth: {
      googleClientId: getEnvString('SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTID', ''),
      googleClientSecret: getEnvString('SCION_SERVER_OAUTH_WEB_GOOGLE_CLIENTSECRET', ''),
      githubClientId: getEnvString('SCION_SERVER_OAUTH_WEB_GITHUB_CLIENTID', ''),
      githubClientSecret: getEnvString('SCION_SERVER_OAUTH_WEB_GITHUB_CLIENTSECRET', ''),
      authorizedDomains: getEnvStringArray('SCION_AUTHORIZED_DOMAINS', []),
    },
  };
}

export const config = loadConfig();
