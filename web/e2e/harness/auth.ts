// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Auth helpers for E2E tests.
 *
 * ## Strategy
 *
 * The hub is started with `--session-secret=e2e-test-secret` and
 * `--enable-test-login`. This module:
 *
 * 1. Derives the user-signing key from the known session secret (mirroring
 *    the Go hub's `deriveSharedSigningKey` function).
 * 2. Mints a short-lived HS256 JWT with audience "scion-test-login" (the
 *    challenge token the test-login endpoint requires).
 * 3. Calls `POST /api/v1/auth/test-login` to create a session for any
 *    email/role/displayName combination.
 * 4. Captures the `Set-Cookie` response headers and saves them as a
 *    Playwright-compatible `storageState` JSON file.
 *
 * For the default admin user, dev-auth auto-login works on first page load,
 * but test-login is used for consistency and to support non-admin identities.
 */

import * as crypto from 'node:crypto';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import type { APIRequestContext, BrowserContext, Page } from '@playwright/test';
import { E2E_SESSION_SECRET } from './hub.js';

// ── Constants mirroring the Go hub ────────────────────────────────────────

const USER_TOKEN_ISSUER = 'scion-hub';
const TEST_LOGIN_AUDIENCE = 'scion-test-login';
const USER_SIGNING_KEY_NAME = 'user_signing_key';

// ── Key derivation ────────────────────────────────────────────────────────

/**
 * Mirrors Go's `deriveSharedSigningKey`:
 *   sha256("scion-hub-signing-key:" + keyName + ":" + secret)
 */
function deriveSigningKey(secret: string, keyName: string): Buffer {
  return crypto.createHash('sha256').update(`scion-hub-signing-key:${keyName}:${secret}`).digest();
}

// ── JWT helpers (HS256, no external dependencies) ─────────────────────────

function base64url(buf: Buffer): string {
  return buf.toString('base64url');
}

function base64urlJSON(obj: unknown): string {
  return base64url(Buffer.from(JSON.stringify(obj)));
}

/**
 * Mint an HS256 JWT.
 */
function signJWT(payload: Record<string, unknown>, signingKey: Buffer): string {
  const header = { alg: 'HS256', typ: 'JWT' };
  const headerB64 = base64urlJSON(header);
  const payloadB64 = base64urlJSON(payload);
  const signingInput = `${headerB64}.${payloadB64}`;
  const signature = crypto.createHmac('sha256', signingKey).update(signingInput).digest();
  return `${signingInput}.${base64url(signature)}`;
}

// ── Test-login challenge token ────────────────────────────────────────────

/**
 * Generate a test-login challenge token — a short-lived JWT with the
 * "scion-test-login" audience, signed with the user signing key derived
 * from the known session secret.
 */
export function generateTestLoginToken(subject = 'e2e-harness'): string {
  const signingKey = deriveSigningKey(E2E_SESSION_SECRET, USER_SIGNING_KEY_NAME);
  const now = Math.floor(Date.now() / 1000);

  const payload = {
    iss: USER_TOKEN_ISSUER,
    sub: subject,
    aud: TEST_LOGIN_AUDIENCE,
    iat: now,
    nbf: now,
    exp: now + 300, // 5 minutes
    jti: crypto.randomBytes(16).toString('base64url'),
  };

  return signJWT(payload, signingKey);
}

// ── Session creation ──────────────────────────────────────────────────────

export interface TestUser {
  email: string;
  role: 'admin' | 'member' | 'viewer';
  displayName?: string;
}

export interface AuthSession {
  user: {
    id: string;
    email: string;
    displayName: string;
    role: string;
  };
  accessToken: string;
  refreshToken: string;
  storageStatePath: string;
}

/**
 * Create a session for the given user via the test-login endpoint.
 * Returns the session info and the path to a storageState JSON file.
 */
export async function createSession(baseURL: string, testUser: TestUser): Promise<AuthSession> {
  const challengeToken = generateTestLoginToken();

  const res = await fetch(`${baseURL}/api/v1/auth/test-login`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${challengeToken}`,
    },
    body: JSON.stringify({
      email: testUser.email,
      role: testUser.role,
      displayName: testUser.displayName || testUser.email,
    }),
    redirect: 'manual',
  });

  if (!res.ok) {
    const body = await res.text();
    throw new Error(`test-login failed (${res.status}): ${body}`);
  }

  const data = (await res.json()) as Pick<AuthSession, 'user' | 'accessToken' | 'refreshToken'>;

  // Extract Set-Cookie headers
  const cookies = extractCookies(res, baseURL);

  // Build storageState
  const storageState = {
    cookies,
    origins: [],
  };

  // Write to a temp file
  const storageDir = path.join(os.tmpdir(), 'scion-e2e-auth');
  fs.mkdirSync(storageDir, { recursive: true });
  const safeName = testUser.email.replace(/[^a-zA-Z0-9]/g, '_');
  const storageStatePath = path.join(storageDir, `${safeName}.json`);
  fs.writeFileSync(storageStatePath, JSON.stringify(storageState, null, 2));

  return {
    user: data.user,
    accessToken: data.accessToken,
    refreshToken: data.refreshToken,
    storageStatePath,
  };
}

/**
 * Extract cookies from the response's Set-Cookie headers and convert them
 * to Playwright's storageState cookie format.
 */
function extractCookies(res: Response, baseURL: string): Array<Record<string, unknown>> {
  const url = new URL(baseURL);
  const cookies: Array<Record<string, unknown>> = [];

  // fetch() returns combined headers; getSetCookie() splits them properly
  const setCookieHeaders =
    typeof res.headers.getSetCookie === 'function'
      ? res.headers.getSetCookie()
      : (res.headers.get('set-cookie') || '').split(/,(?=\s*\w+=)/);

  for (const header of setCookieHeaders) {
    if (!header.trim()) continue;

    const parts = header.split(';').map((s) => s.trim());
    const [nameValue, ...attrs] = parts;
    const eqIdx = nameValue.indexOf('=');
    if (eqIdx === -1) continue;

    const name = nameValue.substring(0, eqIdx);
    const value = nameValue.substring(eqIdx + 1);

    const cookie: Record<string, unknown> = {
      name,
      value,
      domain: url.hostname,
      path: '/',
      expires: -1,
      httpOnly: false,
      secure: false,
      sameSite: 'Lax',
    };

    for (const attr of attrs) {
      const lower = attr.toLowerCase();
      if (lower === 'httponly') cookie.httpOnly = true;
      else if (lower === 'secure') cookie.secure = true;
      else if (lower.startsWith('path=')) cookie.path = attr.split('=')[1];
      else if (lower.startsWith('samesite=')) cookie.sameSite = attr.split('=')[1];
      else if (lower.startsWith('max-age=')) {
        const maxAge = parseInt(attr.split('=')[1], 10);
        cookie.expires = Math.floor(Date.now() / 1000) + maxAge;
      }
    }

    cookies.push(cookie);
  }

  return cookies;
}

/**
 * Create a session for the default admin user.
 */
export async function createAdminSession(baseURL: string): Promise<AuthSession> {
  return createSession(baseURL, {
    email: 'admin@e2e.test',
    role: 'admin',
    displayName: 'E2E Admin',
  });
}

/**
 * Clean up all storageState files.
 */
export function cleanupAuthState(): void {
  const storageDir = path.join(os.tmpdir(), 'scion-e2e-auth');
  try {
    fs.rmSync(storageDir, { recursive: true, force: true });
  } catch {
    // Best-effort cleanup
  }
}

// ── Test identity browser sessions (ptone/scion#4240, WS3) ────────────────
//
// A test identity is a short-lived synthetic user issued on a test hub. The
// runner issues it and mints a single-use web-login code with the CLI:
//
//   scion hub test-identity issue --out <dir>/id.token --json
//   scion hub test-identity web-code <id> --out <dir>/code.txt
//
// redeemTestIdentity() then trades the code for a session cookie in a
// browser context, and assertTestIdentity() checks who the context is. The
// code is valid for 5 minutes and can be redeemed once. See README.md
// ("Test identity browser sessions") for the rules.

/** Options of redeemTestIdentity. Give exactly one of code and codeFile. */
export interface RedeemTestIdentityOptions {
  /** The web-login code. Prefer codeFile so the code is never passed around. */
  code?: string;
  /**
   * Path of the file written by `scion hub test-identity web-code --out`.
   * It is read here and removed after a successful redeem.
   */
  codeFile?: string;
  /** Hub origin, e.g. http://127.0.0.1:8080. Defaults to the context's baseURL. */
  baseURL?: string;
  /** Sent as the Proxy-Authorization header of the redeem request (IAP-fronted hubs). */
  proxyAuthorization?: string;
}

/** What assertTestIdentity() expects. */
export interface ExpectedTestIdentity {
  email: string;
  role?: string;
}

/** What assertTestIdentity() returns. */
export interface TestIdentitySessionInfo {
  email: string;
  role: string;
  /** When the test identity, and so the session, expires (RFC 3339). */
  expiresAt: string;
}

const REDEEM_PATH = '/auth/test-identity/redeem';

/** Reads the code from a code file, refusing a symlink. */
function readCodeFile(codeFile: string): string {
  const st = fs.lstatSync(codeFile);
  if (st.isSymbolicLink() || !st.isFile()) {
    throw new Error(`redeemTestIdentity: codeFile ${codeFile} is not a regular file`);
  }
  const code = fs.readFileSync(codeFile, 'utf-8').trim();
  if (!code) {
    throw new Error(`redeemTestIdentity: codeFile ${codeFile} is empty`);
  }
  return code;
}

/**
 * Returns the hub error code of an error response body (`error.code`), or
 * "unknown". Only a short identifier is ever returned, never body text.
 */
export function hubErrorCode(body: string): string {
  try {
    const parsed = JSON.parse(body) as { error?: { code?: unknown } };
    const code = parsed.error?.code;
    if (typeof code === 'string' && /^[a-z][a-z0-9_]{0,63}$/.test(code)) {
      return code;
    }
  } catch {
    // Not JSON.
  }
  return 'unknown';
}

/**
 * Runs fn with the context's trace recording paused, so the trace never
 * holds the redeem request body or its Set-Cookie answer. Recording is
 * paused by ending the current trace chunk without saving it and resumed
 * with a new chunk, so actions in this context before the call are dropped
 * from the trace: redeem before any navigation. A context without active
 * tracing runs fn unchanged.
 */
export async function withTracingPaused<T>(
  context: BrowserContext,
  fn: () => Promise<T>
): Promise<T> {
  let paused = false;
  try {
    await context.tracing.stopChunk();
    paused = true;
  } catch (err) {
    // Playwright refuses stopChunk when tracing was never started; any
    // other failure is not safe to ignore.
    if (!/must start tracing/i.test(String((err as Error)?.message ?? err))) {
      throw err;
    }
  }
  try {
    return await fn();
  } finally {
    if (paused) {
      await context.tracing.startChunk({ title: 'after test identity redeem' });
    }
  }
}

/**
 * Redeems a web-login code in `context`: POST /auth/test-identity/redeem
 * with `{code}`. On 204 the context's cookie jar holds a session as the
 * test identity, for every page of the context. Any other answer throws
 * with the status and the hub error code only (never the request or
 * response body). With codeFile, the file is removed after a successful
 * redeem. Trace recording is paused around the request.
 */
export async function redeemTestIdentity(
  context: BrowserContext,
  opts: RedeemTestIdentityOptions
): Promise<void> {
  if ((opts.code === undefined) === (opts.codeFile === undefined)) {
    throw new Error('redeemTestIdentity: give exactly one of code and codeFile');
  }
  const code = opts.codeFile !== undefined ? readCodeFile(opts.codeFile) : (opts.code ?? '').trim();
  if (!code) {
    throw new Error('redeemTestIdentity: the code is empty');
  }
  const url = opts.baseURL ? new URL(REDEEM_PATH, opts.baseURL).toString() : REDEEM_PATH;
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (opts.proxyAuthorization) {
    headers['Proxy-Authorization'] = opts.proxyAuthorization;
  }

  const result = await withTracingPaused(context, async () => {
    let res;
    try {
      res = await context.request.post(url, { data: { code }, headers, maxRedirects: 0 });
    } catch (err) {
      // The request error names the URL only; keep it short.
      throw new Error(
        `redeemTestIdentity: request to ${REDEEM_PATH} failed (set baseURL on the context or in opts): ${String((err as Error)?.message ?? err).split('\n')[0]}`
      );
    }
    const status = res.status();
    const errCode = status === 204 ? '' : hubErrorCode(await res.text().catch(() => ''));
    await res.dispose();
    return { status, errCode };
  });

  if (result.status !== 204) {
    throw new Error(
      `redeemTestIdentity: redeem answered ${result.status} (${result.errCode}), want 204`
    );
  }
  if (opts.codeFile !== undefined) {
    fs.rmSync(opts.codeFile, { force: true });
  }
}

/** The request context of a Page (sharing its cookies), or target itself. */
function requestOf(target: Page | APIRequestContext): APIRequestContext {
  return typeof (target as Page).goto === 'function'
    ? (target as Page).request
    : (target as APIRequestContext);
}

/**
 * Asserts that `target` (a page, whose context's cookies are used, or a
 * request context with a baseURL) holds a test identity session for
 * expected.email, and for expected.role when given. It reads GET /auth/me
 * (email, role and testIdentity.expiresAt) and checks that
 * GET /api/v1/auth/me, the API path the web app uses, answers for the same
 * identity. Returns what it read.
 */
export async function assertTestIdentity(
  target: Page | APIRequestContext,
  expected: ExpectedTestIdentity
): Promise<TestIdentitySessionInfo> {
  const request = requestOf(target);

  const me = await request.get('/auth/me', { maxRedirects: 0 });
  if (me.status() !== 200) {
    const errCode = hubErrorCode(await me.text().catch(() => ''));
    throw new Error(`assertTestIdentity: /auth/me answered ${me.status()} (${errCode}), want 200`);
  }
  const body = (await me.json()) as {
    email?: string;
    role?: string;
    testIdentity?: { expiresAt?: string };
  };
  if (body.email !== expected.email) {
    throw new Error(
      `assertTestIdentity: session email is ${String(body.email)}, want ${expected.email}`
    );
  }
  if (expected.role !== undefined && body.role !== expected.role) {
    throw new Error(
      `assertTestIdentity: session role is ${String(body.role)}, want ${expected.role}`
    );
  }
  const expiresAt = body.testIdentity?.expiresAt;
  if (!expiresAt) {
    throw new Error(
      'assertTestIdentity: the session is not a test identity session (no testIdentity.expiresAt)'
    );
  }

  const api = await request.get('/api/v1/auth/me', { maxRedirects: 0 });
  if (api.status() !== 200) {
    const errCode = hubErrorCode(await api.text().catch(() => ''));
    throw new Error(
      `assertTestIdentity: /api/v1/auth/me answered ${api.status()} (${errCode}), want 200`
    );
  }
  const apiBody = (await api.json()) as { email?: string };
  if (apiBody.email !== expected.email) {
    throw new Error(
      `assertTestIdentity: API identity is ${String(apiBody.email)}, want ${expected.email}`
    );
  }

  return { email: body.email, role: body.role ?? '', expiresAt };
}
