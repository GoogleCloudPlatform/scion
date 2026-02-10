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
 * Logger middleware
 *
 * Logs HTTP requests with timing, status, and basic request info.
 * When debug mode is enabled (SCION_API_DEBUG=true), logs additional details.
 */

import type { Context, Next, Middleware } from 'koa';
import type { AppConfig } from '../config.js';

export interface LogEntry {
  timestamp: string;
  requestId: string;
  method: string;
  path: string;
  status: number;
  duration: number;
  ip: string;
  userAgent: string;
  query?: Record<string, unknown>;
  headers?: Record<string, string>;
}

/**
 * Debug logger utility for verbose logging when SCION_API_DEBUG is enabled
 */
class DebugLogger {
  private enabled: boolean;

  constructor(enabled: boolean) {
    this.enabled = enabled;
  }

  log(message: string, data?: unknown): void {
    if (!this.enabled) return;
    const timestamp = new Date().toISOString();
    if (data !== undefined) {
      console.log(`[DEBUG ${timestamp}] ${message}`, JSON.stringify(data, null, 2));
    } else {
      console.log(`[DEBUG ${timestamp}] ${message}`);
    }
  }

  error(message: string, error?: unknown): void {
    if (!this.enabled) return;
    const timestamp = new Date().toISOString();
    console.error(`[DEBUG ${timestamp}] ERROR: ${message}`, error);
  }

  request(ctx: Context): void {
    if (!this.enabled) return;
    this.log(`Request: ${ctx.method} ${ctx.url}`, {
      headers: this.sanitizeHeaders(ctx.headers),
      query: ctx.query,
      body: ctx.request.body,
    });
  }

  response(ctx: Context, duration: number): void {
    if (!this.enabled) return;
    this.log(`Response: ${ctx.status} (${duration}ms)`, {
      headers: this.getResponseHeaders(ctx),
      bodyPreview: this.getBodyPreview(ctx.body),
    });
  }

  private sanitizeHeaders(headers: Record<string, unknown>): Record<string, unknown> {
    const sanitized = { ...headers };
    // Don't log sensitive headers
    const sensitiveHeaders = ['authorization', 'cookie', 'x-api-key'];
    for (const header of sensitiveHeaders) {
      if (sanitized[header]) {
        sanitized[header] = '[REDACTED]';
      }
    }
    return sanitized;
  }

  private getResponseHeaders(ctx: Context): Record<string, string> {
    const headers: Record<string, string> = {};
    const rawHeaders = ctx.response.headers;
    if (rawHeaders && typeof rawHeaders === 'object') {
      for (const [key, value] of Object.entries(rawHeaders)) {
        if (typeof value === 'string') {
          headers[key] = value;
        } else if (Array.isArray(value)) {
          headers[key] = value.join(', ');
        } else if (value !== undefined && value !== null) {
          headers[key] = String(value);
        }
      }
    }
    return headers;
  }

  private getBodyPreview(body: unknown): string {
    if (body === undefined || body === null) return '(empty)';
    if (typeof body === 'string') {
      return body.length > 200 ? `${body.substring(0, 200)}... (${body.length} chars)` : body;
    }
    if (typeof body === 'object') {
      const str = JSON.stringify(body);
      return str.length > 200 ? `${str.substring(0, 200)}... (${str.length} chars)` : str;
    }
    return String(body);
  }
}

// Singleton debug logger, initialized on first use
let debugLoggerInstance: DebugLogger | null = null;

/**
 * Gets the debug logger instance
 */
export function getDebugLogger(debug?: boolean): DebugLogger {
  if (!debugLoggerInstance) {
    // Default to SCION_API_DEBUG env var if not explicitly set
    const enabled =
      debug ??
      (process.env.SCION_API_DEBUG?.toLowerCase() === 'true' ||
        process.env.SCION_API_DEBUG === '1');
    debugLoggerInstance = new DebugLogger(enabled);
  }
  return debugLoggerInstance;
}

/**
 * Generates a unique request ID
 */
function generateRequestId(): string {
  return `${Date.now().toString(36)}-${Math.random().toString(36).substring(2, 9)}`;
}

/**
 * Formats a log entry as JSON (for structured logging)
 */
function formatLogEntry(entry: LogEntry): string {
  return JSON.stringify(entry);
}

/**
 * Creates the logger middleware
 *
 * @param config - Application configuration
 * @returns Koa middleware function
 */
export function logger(config?: AppConfig): Middleware {
  const debug = getDebugLogger(config?.debug);

  return async (ctx: Context, next: Next): Promise<void> => {
    const start = Date.now();
    const requestId = generateRequestId();

    // Attach request ID and debug logger to context state for downstream use
    ctx.state.requestId = requestId;
    ctx.state.debug = debug;

    // Set request ID header for tracing
    ctx.set('X-Request-ID', requestId);

    // Debug log request
    debug.request(ctx);

    try {
      await next();
    } finally {
      const duration = Date.now() - start;

      const entry: LogEntry = {
        timestamp: new Date().toISOString(),
        requestId,
        method: ctx.method,
        path: ctx.path,
        status: ctx.status,
        duration,
        ip: ctx.ip,
        userAgent: ctx.get('User-Agent') || 'unknown',
      };

      // Add extra fields in debug mode
      if (config?.debug) {
        entry.query = ctx.query as Record<string, unknown>;
      }

      // Log to stdout as JSON
      console.info(formatLogEntry(entry));

      // Debug log response
      debug.response(ctx, duration);
    }
  };
}
