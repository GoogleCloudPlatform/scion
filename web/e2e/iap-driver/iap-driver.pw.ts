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
 * installIapDriver() (ptone/scion#4240, phase WS2) in a real browser
 * against two local HTTP servers: the "hub" origin and another origin. Each
 * server records the Authorization header of every request it gets. The
 * token callback is synthetic; no IAP, hub or credential is involved.
 */

import * as http from 'node:http';
import type { AddressInfo } from 'node:net';

import { expect, test, type Browser, type BrowserContext } from '@playwright/test';

import { installIapDriver, jwtExpiryMs } from '../harness/iap-driver.js';

interface Seen {
  path: string;
  authorization: string | undefined;
}

interface Origin {
  url: string;
  seen: Seen[];
  close: () => Promise<void>;
}

async function startOrigin(): Promise<Origin> {
  const seen: Seen[] = [];
  const server = http.createServer((req, res) => {
    const path = req.url ?? '';
    seen.push({ path, authorization: req.headers.authorization });
    res.setHeader('Access-Control-Allow-Origin', '*');
    res.setHeader('Cache-Control', 'no-store');
    if (path === '/' || path.startsWith('/page')) {
      res.setHeader('Content-Type', 'text/html');
      res.end('<!doctype html><link rel="icon" href="data:,"><title>origin</title><p>origin</p>');
      return;
    }
    res.setHeader('Content-Type', 'text/plain');
    res.end('ok');
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${port}`,
    seen,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
  };
}

/** A JWT-shaped synthetic token (unsigned) with the given exp in ms. */
function syntheticToken(label: string, expMs: number): string {
  const b64 = (v: object) => Buffer.from(JSON.stringify(v)).toString('base64url');
  return `${b64({ alg: 'none' })}.${b64({ sub: label, exp: Math.floor(expMs / 1000) })}.sig-${label}`;
}

/** Authorization values a server saw for paths starting with prefix. */
function authFor(o: Origin, prefix: string): Array<string | undefined> {
  return o.seen.filter((s) => s.path.startsWith(prefix)).map((s) => s.authorization);
}

let hub: Origin;
let other: Origin;

test.beforeEach(async () => {
  hub = await startOrigin();
  other = await startOrigin();
});

test.afterEach(async () => {
  await hub.close();
  await other.close();
});

async function newHubContext(browser: Browser): Promise<BrowserContext> {
  return browser.newContext({ baseURL: hub.url });
}

test('adds the driver token to same-origin requests only', async ({ browser }) => {
  const context = await newHubContext(browser);
  const token = syntheticToken('one', Date.now() + 3600_000);
  await installIapDriver(context, { baseURL: hub.url, token: () => Promise.resolve(token) });
  const page = await context.newPage();

  await page.goto('/page-1');
  await page.evaluate(async (otherURL) => {
    await fetch('/api/same');
    await new Promise<void>((resolve) => {
      const xhr = new XMLHttpRequest();
      xhr.onloadend = () => resolve();
      xhr.open('GET', '/api/xhr-same');
      xhr.send();
    });
    await new Promise<void>((resolve) => {
      const es = new EventSource('/events-same');
      const done = () => {
        es.close();
        resolve();
      };
      es.onerror = done;
      es.onopen = done;
    });
    await fetch(`${otherURL}/api/cross`);
    await new Promise<void>((resolve) => {
      const img = new Image();
      img.onload = img.onerror = () => resolve();
      img.src = `${otherURL}/img/cross.png`;
    });
  }, other.url);
  await page.goto(`${other.url}/page-other`);

  expect(authFor(hub, '/page-1')).toEqual([`Bearer ${token}`]);
  expect(authFor(hub, '/api/same')).toEqual([`Bearer ${token}`]);
  expect(authFor(hub, '/api/xhr-same')).toEqual([`Bearer ${token}`]);
  expect(authFor(hub, '/events-same').length).toBeGreaterThan(0);
  expect(authFor(hub, '/events-same').every((a) => a === `Bearer ${token}`)).toBe(true);
  expect(authFor(other, '/api/cross')).toEqual([undefined]);
  expect(authFor(other, '/img/cross.png')).toEqual([undefined]);
  expect(authFor(other, '/page-other')).toEqual([undefined]);
  expect(other.seen.every((s) => s.authorization === undefined)).toBe(true);
  await context.close();
});

test('refreshes the token through the callback before it expires', async ({ browser }) => {
  const context = await newHubContext(browser);
  let clock = 1_000_000_000_000;
  let calls = 0;
  const issued: string[] = [];
  await installIapDriver(context, {
    baseURL: hub.url,
    now: () => clock,
    refreshBeforeMs: 60_000,
    token: () => {
      calls++;
      const t = syntheticToken(`t${calls}`, clock + 120_000);
      issued.push(t);
      return Promise.resolve(t);
    },
  });
  const page = await context.newPage();
  await page.goto('/page-a');
  clock += 30_000; // still more than 60 s before exp: cached
  await page.evaluate(() => fetch('/api/cached'));
  clock += 31_000; // within 60 s of exp: refreshed
  await page.evaluate(() => fetch('/api/refreshed'));
  clock += 120_000; // expired: one refresh shared by concurrent requests
  await page.evaluate(() => Promise.all([1, 2, 3, 4, 5].map((i) => fetch(`/api/concurrent-${i}`))));

  expect(calls).toBe(3);
  expect(authFor(hub, '/page-a')).toEqual([`Bearer ${issued[0]}`]);
  expect(authFor(hub, '/api/cached')).toEqual([`Bearer ${issued[0]}`]);
  expect(authFor(hub, '/api/refreshed')).toEqual([`Bearer ${issued[1]}`]);
  expect(authFor(hub, '/api/concurrent-')).toEqual(Array(5).fill(`Bearer ${issued[2]}`));
  await context.close();
});

test('keeps an Authorization header the page set itself', async ({ browser }) => {
  const context = await newHubContext(browser);
  await installIapDriver(context, {
    baseURL: hub.url,
    token: () => Promise.resolve(syntheticToken('d', Date.now() + 3600_000)),
  });
  const page = await context.newPage();
  await page.goto('/page-x');
  await page.evaluate(() =>
    fetch('/api/explicit', { headers: { Authorization: 'Bearer page-set' } })
  );
  expect(authFor(hub, '/api/explicit')).toEqual(['Bearer page-set']);
  await context.close();
});

test('never logs or stores the token, and aborts when the callback fails', async ({ browser }) => {
  const context = await newHubContext(browser);
  let clock = 1_000_000_000_000;
  const token = syntheticToken('driver', clock + 90_000);
  let fail = false;
  const logged: string[] = [];
  const saved = { log: console.log, warn: console.warn, error: console.error, info: console.info };
  for (const level of ['log', 'warn', 'error', 'info'] as const) {
    console[level] = (...args: unknown[]) => {
      logged.push(args.map(String).join(' '));
    };
  }
  try {
    await installIapDriver(context, {
      baseURL: hub.url,
      now: () => clock,
      refreshBeforeMs: 60_000,
      token: () =>
        fail ? Promise.reject(new Error('synthetic callback failure')) : Promise.resolve(token),
    });
    const page = await context.newPage();
    const pageLogs: string[] = [];
    page.on('console', (m) => pageLogs.push(m.text()));
    await page.goto('/page-s');
    expect(authFor(hub, '/page-s')).toEqual([`Bearer ${token}`]);

    // 31 s later the token is within 60 s of exp: the next request needs a
    // refresh, the callback fails, and the request is never sent without
    // the driver token.
    fail = true;
    clock += 31_000;
    const result = await page.evaluate(() =>
      fetch('/api/after-failure').then(
        () => 'sent',
        () => 'failed'
      )
    );
    expect(result).toBe('failed');
    expect(authFor(hub, '/api/after-failure')).toEqual([]);

    const state = JSON.stringify(await context.storageState());
    expect(state).not.toContain(token);
    expect(logged.join('\n')).toContain('synthetic callback failure');
    expect(logged.join('\n')).not.toContain(token);
    expect(pageLogs.join('\n')).not.toContain(token);
  } finally {
    Object.assign(console, saved);
    await context.close();
  }
});

test('uninstall stops adding the header; a missing or invalid baseURL is refused', async ({
  browser,
}) => {
  const context = await newHubContext(browser);
  const uninstall = await installIapDriver(context, {
    baseURL: hub.url,
    token: () => Promise.resolve(syntheticToken('u', Date.now() + 3600_000)),
  });
  const page = await context.newPage();
  await page.goto('/page-u1');
  await uninstall();
  await page.goto('/page-u2');
  expect(authFor(hub, '/page-u1')).toHaveLength(1);
  expect(authFor(hub, '/page-u1')[0]).toMatch(/^Bearer /);
  expect(authFor(hub, '/page-u2')).toEqual([undefined]);
  await context.close();

  const bare = await browser.newContext();
  for (const baseURL of ['', 'not a url', 'data:,x']) {
    await expect(
      installIapDriver(bare, { baseURL, token: () => Promise.resolve('x') })
    ).rejects.toThrow(/baseURL/);
  }
  await bare.close();
});

test('reads the exp claim without verifying the token', () => {
  expect(jwtExpiryMs(syntheticToken('e', 1_700_000_000_000))).toBe(1_700_000_000_000);
  expect(jwtExpiryMs('not-a-jwt')).toBeUndefined();
  expect(jwtExpiryMs('a.%%%.c')).toBeUndefined();
});
