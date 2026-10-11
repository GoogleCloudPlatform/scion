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
 * Deleting a GCP service account (ptone/scion#4022).
 *
 * The hub refuses a delete with 409 `sa_in_use` while a default points at
 * the account, attaching an impact report. When every listed default can be
 * cleared, the user is asked to confirm, and the delete is retried with
 * `?force=true`, which clears those defaults first (an 'assign' default
 * becomes 'block'). The hub default is never cleared that way, so a refusal
 * naming it is shown as an error instead.
 */

export type SADeleteOutcome =
  | { status: 'deleted' }
  | { status: 'cancelled' }
  | { status: 'failed'; message: string };

interface SAInUseError {
  code?: string;
  message?: string;
  details?: { impact?: { defaults?: { clearable?: boolean }[] } };
}

/** The hub's `?force=true` variant of an account URL. */
export function forceDeleteUrl(url: string): string {
  return url + (url.includes('?') ? '&' : '?') + 'force=true';
}

async function errorMessage(res: Response, fallback: string): Promise<string> {
  try {
    const body = (await res.json()) as { error?: { message?: unknown } };
    const msg = body?.error?.message;
    return typeof msg === 'string' && msg ? msg : fallback;
  } catch {
    return fallback;
  }
}

/**
 * Deletes the account at url. doDelete performs one DELETE request;
 * confirmForce asks the user whether to clear the defaults the hub listed.
 */
export async function deleteGCPServiceAccount(
  url: string,
  doDelete: (url: string) => Promise<Response>,
  confirmForce: (message: string) => Promise<boolean>
): Promise<SADeleteOutcome> {
  let res = await doDelete(url);
  if (res.status === 409) {
    let err: SAInUseError | undefined;
    try {
      err = ((await res.clone().json()) as { error?: SAInUseError })?.error;
    } catch {
      err = undefined;
    }
    if (err?.code === 'sa_in_use') {
      const message = err.message || 'This service account is in use by defaults.';
      const defaults = err.details?.impact?.defaults ?? [];
      if (defaults.length === 0 || defaults.some((d) => !d.clearable)) {
        return { status: 'failed', message };
      }
      if (!(await confirmForce(message + '\n\nClear these defaults and remove the account?'))) {
        return { status: 'cancelled' };
      }
      res = await doDelete(forceDeleteUrl(url));
    }
  }
  if (!res.ok) {
    return {
      status: 'failed',
      message: await errorMessage(res, 'Failed to delete (HTTP ' + res.status + ')'),
    };
  }
  return { status: 'deleted' };
}
