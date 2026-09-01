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
  return crypto
    .createHash('sha256')
    .update(`scion-hub-signing-key:${keyName}:${secret}`)
    .digest();
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
function signJWT(
  payload: Record<string, unknown>,
  signingKey: Buffer,
): string {
  const header = { alg: 'HS256', typ: 'JWT' };
  const headerB64 = base64urlJSON(header);
  const payloadB64 = base64urlJSON(payload);
  const signingInput = `${headerB64}.${payloadB64}`;
  const signature = crypto
    .createHmac('sha256', signingKey)
    .update(signingInput)
    .digest();
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
export async function createSession(
  baseURL: string,
  testUser: TestUser,
): Promise<AuthSession> {
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
    throw new Error(
      `test-login failed (${res.status}): ${body}`,
    );
  }

  const data = await res.json();

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
function extractCookies(
  res: Response,
  baseURL: string,
): Array<Record<string, unknown>> {
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
      else if (lower.startsWith('samesite='))
        cookie.sameSite = attr.split('=')[1];
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
export async function createAdminSession(
  baseURL: string,
): Promise<AuthSession> {
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
