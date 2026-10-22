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
 * Self-test of driverIdTokenSource() (../harness/driver-id-token.ts,
 * ptone/scion#4240 X5) against two local HTTP servers standing in for the
 * metadata emulator and IAM Credentials. No browser, hub or credential is
 * involved; every token is synthetic.
 *
 * Checked:
 * - direct path: the emulator's email equals the driver account, so the
 *   token comes from the emulator identity path with the audience and
 *   format=full, and IAM Credentials is not contacted;
 * - impersonate path: the emulator's access token is sent only to IAM
 *   Credentials generateIdToken for the driver account, with the audience
 *   and includeEmail; the minted token is returned;
 * - every emulator request carries Metadata-Flavor: Google;
 * - assign mode and a loopback GCE_METADATA_HOST are required;
 * - a refusal reports the status only, never a token or body;
 * - onToken receives the path and the statuses, never a token or email.
 */

import * as http from 'node:http';
import type { AddressInfo } from 'node:net';

import { expect, test } from '@playwright/test';

import {
  DriverTokenError,
  driverIdTokenSource,
  metadataBaseURL,
} from '../harness/driver-id-token.js';

const DRIVER = 't1-uat@proj.iam.gserviceaccount.com';
const RUNNER = 'runner@proj.iam.gserviceaccount.com';
const AUD = '1234-abc.apps.googleusercontent.com';
const ACCESS = 'SYNTHETIC-ACCESS-TOKEN';

function jwt(label: string): string {
  const b64 = (v: object) => Buffer.from(JSON.stringify(v)).toString('base64url');
  return `${b64({ alg: 'RS256' })}.${b64({ sub: label })}.c2ln`;
}
const DIRECT_TOKEN = jwt('direct');
const MINTED_TOKEN = jwt('minted');

interface Seen {
  method: string;
  url: string;
  headers: http.IncomingHttpHeaders;
  body: string;
}

interface Server {
  host: string;
  url: string;
  seen: Seen[];
  close: () => Promise<void>;
}

async function serve(handler: (s: Seen, res: http.ServerResponse) => void): Promise<Server> {
  const seen: Seen[] = [];
  const server = http.createServer((req, res) => {
    let body = '';
    req.on('data', (c: Buffer) => (body += c.toString()));
    req.on('end', () => {
      const s = { method: req.method ?? '', url: req.url ?? '', headers: req.headers, body };
      seen.push(s);
      handler(s, res);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  return {
    host: `127.0.0.1:${port}`,
    url: `http://127.0.0.1:${port}`,
    seen,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
  };
}

function emulator(email: string): Promise<Server> {
  return serve((s, res) => {
    const base = '/computeMetadata/v1/instance/service-accounts/default';
    if (s.headers['metadata-flavor'] !== 'Google') {
      res.writeHead(403).end();
      return;
    }
    if (s.url === `${base}/email`) {
      res.end(email);
    } else if (s.url.startsWith(`${base}/identity?`)) {
      res.end(DIRECT_TOKEN);
    } else if (s.url === `${base}/token`) {
      res.setHeader('Content-Type', 'application/json');
      res.end(JSON.stringify({ access_token: ACCESS, expires_in: 3600, token_type: 'Bearer' }));
    } else {
      res.writeHead(404).end();
    }
  });
}

function iamServer(status = 200): Promise<Server> {
  return serve((_s, res) => {
    if (status !== 200) {
      res.writeHead(status, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: { message: `denied for ${ACCESS}` } }));
      return;
    }
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ token: MINTED_TOKEN }));
  });
}

const servers: Server[] = [];
async function track(p: Promise<Server>): Promise<Server> {
  const s = await p;
  servers.push(s);
  return s;
}
test.afterEach(async () => {
  await Promise.all(servers.splice(0).map((s) => s.close()));
});

function envFor(emu: Server): NodeJS.ProcessEnv {
  return { SCION_METADATA_MODE: 'assign', GCE_METADATA_HOST: emu.host };
}

test('direct path: the emulator identity path when it is the driver', async () => {
  const emu = await track(emulator(DRIVER));
  const iam = await track(iamServer());
  const infos: unknown[] = [];
  const token = await driverIdTokenSource({
    driverServiceAccount: DRIVER,
    audience: AUD,
    env: envFor(emu),
    iamCredentialsURL: iam.url,
    onToken: (i) => infos.push(i),
  })();
  expect(token).toBe(DIRECT_TOKEN);
  expect(iam.seen).toHaveLength(0);
  const identity = emu.seen.find((s) => s.url.includes('/identity?'));
  expect(identity).toBeDefined();
  const q = new URL(identity?.url ?? '', 'http://x').searchParams;
  expect(q.get('audience')).toBe(AUD);
  expect(q.get('format')).toBe('full');
  expect(emu.seen.every((s) => s.headers['metadata-flavor'] === 'Google')).toBe(true);
  expect(emu.seen.some((s) => s.url.endsWith('/token'))).toBe(false);
  expect(infos).toEqual([{ path: 'direct', statuses: [200, 200] }]);
});

test('impersonate path: generateIdToken for the driver with the emulator access token', async () => {
  const emu = await track(emulator(RUNNER));
  const iam = await track(iamServer());
  const infos: unknown[] = [];
  const token = await driverIdTokenSource({
    driverServiceAccount: DRIVER,
    audience: AUD,
    env: envFor(emu),
    iamCredentialsURL: iam.url,
    onToken: (i) => infos.push(i),
  })();
  expect(token).toBe(MINTED_TOKEN);
  expect(emu.seen.some((s) => s.url.includes('/identity'))).toBe(false);
  expect(iam.seen).toHaveLength(1);
  const call = iam.seen[0];
  expect(call.method).toBe('POST');
  expect(call.url).toBe(
    `/v1/projects/-/serviceAccounts/${encodeURIComponent(DRIVER)}:generateIdToken`
  );
  expect(call.headers.authorization).toBe(`Bearer ${ACCESS}`);
  expect(JSON.parse(call.body)).toEqual({ audience: AUD, includeEmail: true });
  // The access token went only to IAM Credentials.
  expect(emu.seen.every((s) => s.headers.authorization === undefined)).toBe(true);
  expect(infos).toEqual([{ path: 'impersonate', statuses: [200, 200, 200] }]);
  expect(JSON.stringify(infos)).not.toContain(RUNNER);
});

test('a refusal reports the status only', async () => {
  const emu = await track(emulator(RUNNER));
  const iam = await track(iamServer(403));
  const source = driverIdTokenSource({
    driverServiceAccount: DRIVER,
    audience: AUD,
    env: envFor(emu),
    iamCredentialsURL: iam.url,
  });
  let err: unknown;
  try {
    await source();
  } catch (e) {
    err = e;
  }
  expect(err).toBeInstanceOf(DriverTokenError);
  const msg = String((err as Error).message);
  expect(msg).toContain('answered 403');
  expect(msg).not.toContain(ACCESS);
  expect(msg).not.toContain('denied');
});

test('assign mode and a loopback emulator address are required', () => {
  expect(
    metadataBaseURL({ SCION_METADATA_MODE: 'assign', GCE_METADATA_HOST: '127.0.0.1:8099' })
  ).toBe('http://127.0.0.1:8099');
  expect(metadataBaseURL({ SCION_METADATA_MODE: 'assign', GCE_METADATA_HOST: 'localhost' })).toBe(
    'http://localhost'
  );
  expect(metadataBaseURL({ SCION_METADATA_MODE: 'assign', GCE_METADATA_HOST: '[::1]:8099' })).toBe(
    'http://[::1]:8099'
  );
  for (const env of [
    { GCE_METADATA_HOST: '127.0.0.1:8099' },
    { SCION_METADATA_MODE: 'block', GCE_METADATA_HOST: '127.0.0.1:8099' },
    { SCION_METADATA_MODE: 'assign' },
    { SCION_METADATA_MODE: 'assign', GCE_METADATA_HOST: 'metadata.google.internal' },
    { SCION_METADATA_MODE: 'assign', GCE_METADATA_HOST: '10.0.0.1:80' },
    { SCION_METADATA_MODE: 'assign', GCE_METADATA_HOST: 'user@127.0.0.1' },
    { SCION_METADATA_MODE: 'assign', GCE_METADATA_HOST: '127.0.0.1/x' },
    { SCION_METADATA_MODE: 'assign', GCE_METADATA_HOST: '127.0.0.1:' },
  ] as NodeJS.ProcessEnv[]) {
    expect(() => metadataBaseURL(env), JSON.stringify(env)).toThrow(DriverTokenError);
  }
});
