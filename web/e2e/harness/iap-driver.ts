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
 * The IAP driver hook (ptone/scion#4240, phase WS2).
 *
 * On an IAP-fronted test hub every browser request must carry the driver's
 * Google OIDC ID token, and pages cannot send Proxy-Authorization, so the
 * token goes in Authorization. installIapDriver() adds it to the context's
 * same-origin page requests (the hub's origin only, never any other) and
 * fetches a fresh token through the caller's callback before the cached one
 * expires. The token is never logged and never written anywhere: it lives
 * in memory for the life of the hook.
 *
 * The hub honours the test identity session over this token on /api/v1/
 * only when the hub lists the driver in auth.proxy.driverPrincipals and its
 * auth.proxy.driverTokenAudience matches the token's audience. Redeem still
 * uses redeemTestIdentity({proxyAuthorization}) from Node. See README.md
 * ("IAP-fronted hubs").
 */

import type { BrowserContext, Request, Route } from '@playwright/test';

/** Options of installIapDriver. */
export interface IapDriverOptions {
  /**
   * Returns a Google OIDC ID token for the driver principal, with the IAP
   * OAuth client ID as its audience. Called on first use and again when the
   * cached token is within refreshBeforeMs of its expiry.
   */
  token: () => Promise<string>;
  /**
   * The hub origin (or any URL on it), e.g. https://test-hub.example.
   * Required. Only requests to exactly this origin get the header.
   */
  baseURL: string;
  /** Refresh this long before the token's exp claim. Default 60 s. */
  refreshBeforeMs?: number;
  /**
   * How long to keep a token whose exp claim cannot be read. Default 4 min
   * (Google ID tokens live 1 h, so this is conservative).
   */
  fallbackLifetimeMs?: number;
  /** The clock, for tests. Defaults to Date.now. */
  now?: () => number;
}

/** Removes the hook from the context. */
export type UninstallIapDriver = () => Promise<void>;

const DEFAULT_REFRESH_BEFORE_MS = 60_000;
const DEFAULT_FALLBACK_LIFETIME_MS = 4 * 60_000;

/**
 * Returns the exp claim of a JWT in milliseconds, or undefined. The token
 * is only parsed, never verified: the hub verifies it.
 */
export function jwtExpiryMs(token: string): number | undefined {
  const parts = token.split('.');
  if (parts.length !== 3) {
    return undefined;
  }
  try {
    const payload = JSON.parse(Buffer.from(parts[1], 'base64url').toString('utf-8')) as {
      exp?: unknown;
    };
    return typeof payload.exp === 'number' && Number.isFinite(payload.exp)
      ? payload.exp * 1000
      : undefined;
  } catch {
    return undefined;
  }
}

/** The origin of url, or undefined for a URL without one (data:, blob:). */
function originOf(url: string): string | undefined {
  try {
    const origin = new URL(url).origin;
    return origin === 'null' ? undefined : origin;
  } catch {
    return undefined;
  }
}

/**
 * Adds `Authorization: Bearer <driver token>` to every request of `context`
 * whose origin is exactly the hub origin, page navigations and the page's
 * fetch/XHR/EventSource requests alike, and to no other request. A request
 * that already carries an Authorization header keeps it. The token comes
 * from opts.token(), cached until refreshBeforeMs before its exp claim;
 * concurrent requests share one refresh. When the callback fails, the
 * request is aborted (never sent without the driver token) and the failure
 * is reported without the token.
 *
 * Returns a function that removes the hook.
 */
export async function installIapDriver(
  context: BrowserContext,
  opts: IapDriverOptions
): Promise<UninstallIapDriver> {
  const hubOrigin = opts.baseURL ? originOf(opts.baseURL) : undefined;
  if (!hubOrigin) {
    throw new Error('installIapDriver: opts.baseURL must be the hub URL');
  }
  const refreshBefore = opts.refreshBeforeMs ?? DEFAULT_REFRESH_BEFORE_MS;
  const fallbackLifetime = opts.fallbackLifetimeMs ?? DEFAULT_FALLBACK_LIFETIME_MS;
  const now = opts.now ?? Date.now;

  let cached: { token: string; refreshAt: number } | undefined;
  let pending: Promise<string> | undefined;

  const currentToken = async (): Promise<string> => {
    if (cached && now() < cached.refreshAt) {
      return cached.token;
    }
    if (!pending) {
      pending = (async () => {
        try {
          const token = (await opts.token()).trim();
          if (!token) {
            throw new Error('the token callback returned an empty token');
          }
          const exp = jwtExpiryMs(token);
          const refreshAt = exp !== undefined ? exp - refreshBefore : now() + fallbackLifetime;
          cached = { token, refreshAt };
          return token;
        } finally {
          pending = undefined;
        }
      })();
    }
    return pending;
  };

  const matches = (url: URL): boolean => url.origin === hubOrigin;

  const handler = async (route: Route, request: Request): Promise<void> => {
    if (originOf(request.url()) !== hubOrigin) {
      // Never reached through the matcher; kept so a cross-origin request
      // can never get the header.
      await route.fallback();
      return;
    }
    const headers = request.headers();
    if (headers['authorization'] !== undefined) {
      await route.fallback();
      return;
    }
    let token: string;
    try {
      token = await currentToken();
    } catch (err) {
      // Only the first line of the callback's own message; never the token.
      console.warn(
        `installIapDriver: the token callback failed; request aborted: ${String((err as Error)?.message ?? err).split('\n')[0]}`
      );
      await route.abort('failed');
      return;
    }
    await route.fallback({ headers: { ...headers, authorization: `Bearer ${token}` } });
  };

  await context.route(matches, handler);
  return async () => {
    await context.unroute(matches, handler);
  };
}
