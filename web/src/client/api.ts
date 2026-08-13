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
 * API fetch wrapper with automatic 403 handling.
 *
 * Wraps the standard fetch() with credential inclusion and dispatches a
 * `scion:access-denied` CustomEvent on the window when a 403 response is
 * received, allowing the app shell to display a toast notification.
 *
 * This is additive — existing components can continue using fetch() directly.
 * Phase 3 will migrate them to apiFetch().
 */

/** Detail payload for the scion:access-denied custom event. */
export interface AccessDeniedDetail {
  resource?: string;
  action?: string;
  reason?: string;
}

/**
 * Fetch wrapper that includes credentials and handles 403 responses.
 *
 * Returns the raw Response object so callers can handle the body themselves.
 * On 403, dispatches a `scion:access-denied` event on `window` with parsed
 * error details, but does NOT re-throw or alter the response.
 */
const API_SLOW_THRESHOLD_MS = 2000;
let sessionExpiredRedirectPending = false;

export async function apiFetch(path: string, options?: RequestInit): Promise<Response> {
  const start = performance.now();
  const response = await fetch(path, {
    ...options,
    credentials: 'include',
  });
  const elapsed = performance.now() - start;

  if (elapsed > API_SLOW_THRESHOLD_MS) {
    console.warn(
      `[api] Slow response: ${options?.method ?? 'GET'} ${path} took ${elapsed.toFixed(0)}ms`
    );
  }

  if (response.status === 401) {
    // Session expired or signing key rotated — redirect to login.
    // Use a flag to prevent multiple concurrent redirects.
    if (!sessionExpiredRedirectPending) {
      sessionExpiredRedirectPending = true;
      const returnTo = encodeURIComponent(window.location.pathname);
      window.location.href = `/login?error=session_expired&returnTo=${returnTo}`;
    }
    return response;
  }

  if (response.status === 403) {
    let detail: AccessDeniedDetail = {};
    try {
      const body = await response.clone().json();
      detail = {
        resource: body.resource,
        action: body.action,
        reason: body.reason || body.error || body.message,
      };
    } catch {
      // Body wasn't JSON — use empty detail
    }
    window.dispatchEvent(new CustomEvent('scion:access-denied', { detail }));
  }

  return response;
}

/**
 * Extract a human-readable error message from an API error response.
 *
 * The backend returns errors in the format: `{"error": {"code": "...", "message": "..."}}`.
 * This helper parses that structure and returns just the message string.
 */
export async function extractApiError(res: Response, fallback: string): Promise<string> {
  try {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const data = (await res.json()) as any;
    if (typeof data.error === 'object' && data.error?.message) {
      let msg: string = data.error.message;
      // Append guidance hint when available (e.g. clone/pull error details)
      if (data.error?.details?.guidance) {
        msg += ` — ${data.error.details.guidance}`;
      }
      return msg;
    }
    if (typeof data.message === 'string') return data.message;
    if (typeof data.error === 'string') return data.error;
  } catch {
    // Response wasn't JSON
  }
  return fallback;
}

/** Structured API error info returned by {@link parseApiError}. */
export interface ApiErrorInfo {
  code: string;
  message: string;
  details?: Record<string, unknown>;
}

/**
 * Parse a failed API response into structured error info (code, message, details).
 *
 * Use this instead of {@link extractApiError} when the caller needs to inspect
 * the error code to provide context-aware guidance.
 */
export async function parseApiError(res: Response, fallback: string): Promise<ApiErrorInfo> {
  try {
    const data = (await res.json()) as {
      error?: { code?: string; message?: string; details?: Record<string, unknown> } | string;
      message?: string;
    };
    if (typeof data.error === 'object' && data.error) {
      const info: ApiErrorInfo = {
        code: data.error.code ?? '',
        message: data.error.message ?? fallback,
      };
      if (data.error.details) info.details = data.error.details;
      return info;
    }
    if (typeof data.message === 'string') return { code: '', message: data.message };
    if (typeof data.error === 'string') return { code: '', message: data.error };
  } catch {
    // Response wasn't JSON
  }
  return { code: '', message: fallback };
}
