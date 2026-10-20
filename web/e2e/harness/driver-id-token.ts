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
 * The IAP driver's ID token, obtained in-process from the runner's
 * metadata emulator (ptone/scion#4240, phase X5).
 *
 * driverIdTokenSource() returns a callback for installIapDriver() and for
 * the Proxy-Authorization header of the redeem request. Each call returns a
 * Google ID token for the driver principal with the IAP OAuth client ID as
 * its audience. The token lives in memory only: it is never logged, written
 * to a file, put in an environment variable or passed on a command line.
 *
 * Two paths, chosen per call from what the emulator reports:
 *
 * - direct: the runner's assigned service account IS the driver principal.
 *   The token comes from the emulator's identity path
 *   (`.../service-accounts/default/identity?audience=<aud>&format=full`).
 * - impersonate: the assigned service account is another account that may
 *   mint ID tokens for the driver principal. The emulator gives an access
 *   token for the assigned account (`.../service-accounts/default/token`),
 *   and IAM Credentials `generateIdToken` mints the driver's ID token
 *   (`includeEmail: true`). This is what `scion --hub-profile` does for its
 *   IAP transport token.
 *
 * The emulator must be in assign mode (SCION_METADATA_MODE=assign) and
 * GCE_METADATA_HOST must be a loopback host with an optional port, as the
 * CLI requires (pkg/transportauth AssignModeMetadataBaseURL).
 */

import { isIP } from 'node:net';

/** Which path a token came from. */
export type DriverTokenPath = 'direct' | 'impersonate';

/** Options of driverIdTokenSource. */
export interface DriverIdTokenOptions {
  /** The driver principal's email (the hub's auth.proxy.driverPrincipals entry). */
  driverServiceAccount: string;
  /** The IAP OAuth client ID (the hub's auth.proxy.driverTokenAudience). */
  audience: string;
  /** The environment to read (default process.env). */
  env?: NodeJS.ProcessEnv;
  /** IAM Credentials base URL (default https://iamcredentials.googleapis.com), for tests. */
  iamCredentialsURL?: string;
  /** fetch implementation, for tests (default global fetch). */
  fetch?: typeof fetch;
  /**
   * Called once per token with the path taken and the HTTP status codes of
   * the requests made. Never receives a token or an email.
   */
  onToken?: (info: { path: DriverTokenPath; statuses: number[] }) => void;
}

const METADATA_SA_PATH = '/computeMetadata/v1/instance/service-accounts/default';
const DEFAULT_IAM_CREDENTIALS_URL = 'https://iamcredentials.googleapis.com';
const REQUEST_TIMEOUT_MS = 15_000;

/** An error whose message never holds a token or a response body. */
export class DriverTokenError extends Error {}

function isLoopbackHost(host: string): boolean {
  const h = host.toLowerCase();
  if (h === 'localhost') {
    return true;
  }
  const bare = h.startsWith('[') && h.endsWith(']') ? h.slice(1, -1) : h;
  if (isIP(bare) === 4) {
    return bare.startsWith('127.');
  }
  return isIP(bare) === 6 && (bare === '::1' || bare === '0:0:0:0:0:0:0:1');
}

/**
 * Returns the metadata emulator's base URL (http://host[:port]) from env,
 * or throws when the emulator is not in assign mode or its address is not
 * a bare loopback host with an optional port.
 */
export function metadataBaseURL(env: NodeJS.ProcessEnv): string {
  if (env.SCION_METADATA_MODE !== 'assign') {
    throw new DriverTokenError('the metadata emulator is not in assign mode (SCION_METADATA_MODE)');
  }
  const raw = (env.GCE_METADATA_HOST ?? '').trim();
  if (!raw) {
    throw new DriverTokenError(
      'the metadata emulator is not available: GCE_METADATA_HOST is not set'
    );
  }
  const refuse = new DriverTokenError(
    'refusing the metadata emulator address in GCE_METADATA_HOST: it must be a loopback host with an optional port'
  );
  if (/[/?#@\\\s]/.test(raw) || raw.endsWith(':')) {
    throw refuse;
  }
  let u: URL;
  try {
    u = new URL(`http://${raw}`);
  } catch {
    throw refuse;
  }
  if (u.username || u.password || u.host !== raw.toLowerCase() || !isLoopbackHost(u.hostname)) {
    throw refuse;
  }
  return `http://${u.host}`;
}

async function timedFetch(f: typeof fetch, url: string, init: RequestInit): Promise<Response> {
  try {
    return await f(url, {
      ...init,
      redirect: 'error',
      signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
    });
  } catch (err) {
    // Only the first line of the failure; never a header or body.
    throw new DriverTokenError(
      `request to ${new URL(url).host} failed: ${String((err as Error)?.message ?? err).split('\n')[0]}`
    );
  }
}

/** Reads a response body as text, bounded, and discards it on error statuses. */
async function okText(res: Response, what: string, statuses: number[]): Promise<string> {
  statuses.push(res.status);
  if (res.status !== 200) {
    await res.body?.cancel().catch(() => undefined);
    throw new DriverTokenError(`${what} answered ${res.status}`);
  }
  return (await res.text()).trim();
}

function looksLikeJWT(token: string): boolean {
  return /^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/.test(token);
}

/**
 * Returns a callback that obtains a fresh driver ID token on each call
 * (installIapDriver caches it until shortly before its expiry).
 */
export function driverIdTokenSource(opts: DriverIdTokenOptions): () => Promise<string> {
  const env = opts.env ?? process.env;
  const f = opts.fetch ?? fetch;
  const iam = (opts.iamCredentialsURL ?? DEFAULT_IAM_CREDENTIALS_URL).replace(/\/+$/, '');
  const sa = opts.driverServiceAccount.trim();
  const audience = opts.audience.trim();
  if (!sa || !audience) {
    throw new DriverTokenError(
      'driverIdTokenSource: driverServiceAccount and audience are required'
    );
  }
  const flavor = { 'Metadata-Flavor': 'Google' };

  return async () => {
    const base = metadataBaseURL(env);
    const statuses: number[] = [];
    const email = await okText(
      await timedFetch(f, `${base}${METADATA_SA_PATH}/email`, { headers: flavor }),
      'the emulator email path',
      statuses
    );

    let token: string;
    let path: DriverTokenPath;
    if (email.toLowerCase() === sa.toLowerCase()) {
      path = 'direct';
      const q = new URLSearchParams({ audience, format: 'full' });
      token = await okText(
        await timedFetch(f, `${base}${METADATA_SA_PATH}/identity?${q.toString()}`, {
          headers: flavor,
        }),
        'the emulator identity path',
        statuses
      );
    } else {
      path = 'impersonate';
      const accessBody = await okText(
        await timedFetch(f, `${base}${METADATA_SA_PATH}/token`, { headers: flavor }),
        'the emulator token path',
        statuses
      );
      let access: unknown;
      try {
        access = (JSON.parse(accessBody) as { access_token?: unknown }).access_token;
      } catch {
        throw new DriverTokenError('the emulator token path answered a body that is not JSON');
      }
      if (typeof access !== 'string' || !access) {
        throw new DriverTokenError('the emulator token path answered no access token');
      }
      const res = await timedFetch(
        f,
        `${iam}/v1/projects/-/serviceAccounts/${encodeURIComponent(sa)}:generateIdToken`,
        {
          method: 'POST',
          headers: { Authorization: `Bearer ${access}`, 'Content-Type': 'application/json' },
          body: JSON.stringify({ audience, includeEmail: true }),
        }
      );
      const body = await okText(res, 'IAM Credentials generateIdToken', statuses);
      let minted: unknown;
      try {
        minted = (JSON.parse(body) as { token?: unknown }).token;
      } catch {
        throw new DriverTokenError(
          'IAM Credentials generateIdToken answered a body that is not JSON'
        );
      }
      if (typeof minted !== 'string') {
        throw new DriverTokenError('IAM Credentials generateIdToken answered no token');
      }
      token = minted;
    }
    if (!looksLikeJWT(token)) {
      throw new DriverTokenError(`the ${path} path did not return an ID token`);
    }
    opts.onToken?.({ path, statuses });
    return token;
  };
}
