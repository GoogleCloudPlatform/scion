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
 * Security middleware
 *
 * Sets security headers for all responses
 */

import type { Context, Next, Middleware } from 'koa';
import type { AppConfig } from '../config.js';

/**
 * Creates the security headers middleware
 *
 * @param config - Application configuration
 * @returns Koa middleware function
 */
export function security(config: AppConfig): Middleware {
  return async (ctx: Context, next: Next): Promise<void> => {
    // Content Security Policy
    ctx.set('Content-Security-Policy', config.security.csp);

    // Prevent clickjacking
    ctx.set('X-Frame-Options', 'DENY');

    // Prevent MIME type sniffing
    ctx.set('X-Content-Type-Options', 'nosniff');

    // XSS Protection (legacy, but still useful for older browsers)
    ctx.set('X-XSS-Protection', '1; mode=block');

    // Referrer Policy
    ctx.set('Referrer-Policy', 'strict-origin-when-cross-origin');

    // Permissions Policy (disable dangerous features)
    ctx.set(
      'Permissions-Policy',
      'accelerometer=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), usb=()'
    );

    // HSTS (only in production with HTTPS)
    if (config.production && config.security.hstsMaxAge > 0) {
      ctx.set(
        'Strict-Transport-Security',
        `max-age=${config.security.hstsMaxAge}; includeSubDomains`
      );
    }

    await next();
  };
}
