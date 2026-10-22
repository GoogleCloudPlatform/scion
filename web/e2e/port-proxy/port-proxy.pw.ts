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
 * Port proxy journeys in a signed-in browser (ptone/scion#5568).
 *
 * A member test identity, project-admin of the fixture project, opens the
 * agents' exposed port through the hub's path proxy in a browser context
 * that holds only its session cookie (never a bearer token). Each journey
 * runs once per transport: conduit and the legacy tunnel.
 *
 * - Journey 1, single-file page: single.html renders and its inline script
 *   writes #probe.
 * - Journey 2, multi-file page: index.html loads, but its stylesheet,
 *   module script and image are answered with the plain-text 401 that
 *   names the cause (X-Scion-Proxy-Error: no-credentials), so the module
 *   never runs and #probe stays empty. The bare .../proxy URL answers 308
 *   to .../proxy/ with the query kept.
 *
 * Later journeys (the page view experiment, isolation checks, a
 * legacy-only agent) reuse the same rig: fixture/index.html already does a
 * relative fetch and WebSocket echo and writes "ok" to #probe when both
 * work, and fixture/abs.html loads its script by root-relative URL.
 */

import * as fs from 'node:fs';
import * as path from 'node:path';

import { type BrowserContext, expect, type Page, type Response, test } from '@playwright/test';

import { assertTestIdentity, redeemTestIdentity } from '../harness/auth.js';
import { type HubState, readHubState, runScionCLI } from '../harness/hub.js';
import {
  hubStateFile,
  readSuiteState,
  SINGLE_PROBE_TEXT,
  stateDir,
  type SuiteState,
  TRANSPORTS,
} from './state.js';

/** The multi-file page's sub-resources, relative to the proxy root. */
const SUBRESOURCES = ['site.css', 'app.js', 'img/dot.png'] as const;

/** Start of the no-credentials answer's body. */
const NO_CREDENTIALS_BODY_START = 'Scion port proxy: ';

let hub: HubState;
let suite: SuiteState;
let context: BrowserContext;
let identityId = '';
/** Requests the browser context sent with an Authorization header. */
const bearerRequests: string[] = [];

/** The path proxy root of an agent's fixture port, without the trailing slash. */
function proxyBase(agentId: string): string {
  return `${hub.baseURL}/api/v1/agents/${agentId}/ports/${suite.port}/proxy`;
}

function agentFor(transport: string): string {
  const agent = suite.agents.find((a) => a.transport === transport);
  if (!agent) throw new Error(`no fixture agent for transport ${transport}`);
  return agent.id;
}

/** An admin API call (setup only; the browser never holds this token). */
async function adminJSON(method: string, apiPath: string, body?: unknown): Promise<unknown> {
  const res = await fetch(`${hub.baseURL}${apiPath}`, {
    method,
    headers: { Authorization: `Bearer ${hub.devToken}`, 'Content-Type': 'application/json' },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  if (!res.ok) throw new Error(`${method} ${apiPath} answered ${res.status}`);
  return res.status === 204 ? null : ((await res.json()) as unknown);
}

/** Opens a page and records the response of each proxied sub-resource. */
async function openRecorded(
  url: string,
  agentId: string
): Promise<{ page: Page; nav: Response | null; subs: Map<string, Response> }> {
  const page = await context.newPage();
  const subs = new Map<string, Response>();
  const prefix = new URL(`${proxyBase(agentId)}/`).pathname;
  page.on('response', (res) => {
    const { pathname } = new URL(res.url());
    if (pathname.startsWith(prefix)) subs.set(pathname.slice(prefix.length), res);
  });
  const nav = await page.goto(url, { waitUntil: 'load' });
  return { page, nav, subs };
}

test.describe.configure({ mode: 'serial' });

test.beforeAll(async ({ browser }) => {
  const h = readHubState(hubStateFile());
  const s = readSuiteState();
  if (!h || !s) throw new Error('global setup did not record the hub and suite state');
  hub = h;
  suite = s;

  // A member test identity with a web-login code; the API token is not used.
  const tokenFile = path.join(stateDir(), 'member.token');
  const issued = JSON.parse(
    runScionCLI(hub, [
      'hub',
      'test-identity',
      'issue',
      '--role',
      'member',
      '--purpose',
      'e2e port proxy',
      '--out',
      tokenFile,
      '--json',
    ])
  ) as { identity: { id: string; email: string } };
  fs.rmSync(tokenFile, { force: true });
  identityId = issued.identity.id;
  const codeFile = path.join(stateDir(), 'member.code');
  runScionCLI(hub, ['hub', 'test-identity', 'web-code', identityId, '--out', codeFile]);

  // project-admin of the fixture project: the role that opens its agents'
  // exposed ports.
  const roles = (await adminJSON(
    'GET',
    `/api/v1/projects/${suite.projectId}/members/assignable-roles`
  )) as { items?: Array<{ id: string; name: string }> };
  const role = roles.items?.find((r) => r.name === 'project-admin');
  if (!role) throw new Error('no assignable project-admin role');
  await adminJSON('POST', `/api/v1/projects/${suite.projectId}/members`, {
    roleDefinitionId: role.id,
    principalType: 'user',
    principalId: issued.identity.email,
  });

  context = await browser.newContext({ baseURL: hub.baseURL });
  context.on('request', (req) => {
    if (req.headers()['authorization'] !== undefined) bearerRequests.push(req.url());
  });
  await redeemTestIdentity(context, { codeFile });
  await assertTestIdentity(context.request, { email: issued.identity.email, role: 'member' });
});

test.afterAll(async () => {
  await context?.close();
  if (identityId) runScionCLI(hub, ['hub', 'test-identity', 'delete', identityId]);
});

test.afterEach(() => {
  // The journeys hold a cookie session only.
  expect(bearerRequests, 'requests sent with an Authorization header').toEqual([]);
});

for (const transport of TRANSPORTS) {
  test.describe(`${transport} transport`, () => {
    test('journey 1: a single-file page renders', async () => {
      const agentId = agentFor(transport);
      const { page, nav } = await openRecorded(`${proxyBase(agentId)}/single.html`, agentId);
      expect(nav?.status()).toBe(200);
      await expect(page.locator('#probe')).toHaveText(SINGLE_PROBE_TEXT);
      await expect(page.locator('#probe')).toHaveCSS('color', 'rgb(0, 128, 0)');
      await page.close();
    });

    test('journey 2: sub-resources of a multi-file page get the plain-text 401', async () => {
      const agentId = agentFor(transport);
      const { page, nav, subs } = await openRecorded(`${proxyBase(agentId)}/index.html`, agentId);
      expect(nav?.status()).toBe(200);
      await expect
        .poll(() => SUBRESOURCES.filter((name) => !subs.has(name)), {
          message: 'sub-resources not requested yet',
        })
        .toEqual([]);

      for (const name of SUBRESOURCES) {
        const res = subs.get(name)!;
        const headers = await res.allHeaders();
        const sent = await res.request().allHeaders();
        expect.soft(res.status(), `${name} status`).toBe(401);
        expect
          .soft(headers['content-type'], `${name} content type`)
          .toBe('text/plain; charset=utf-8');
        expect.soft(headers['x-scion-proxy-error'], `${name} error header`).toBe('no-credentials');
        expect
          .soft(headers['x-scion-proxy-hint'], `${name} hint header`)
          .toBe('sandboxed-subresource');
        expect.soft(sent['cookie'], `${name} request cookie`).toBeUndefined();
        expect.soft(await res.text(), `${name} body`).toContain(NO_CREDENTIALS_BODY_START);
      }
      // The module never ran and the stylesheet never applied.
      await expect(page.locator('#probe')).toHaveText('');
      await expect(page.locator('#title')).not.toHaveCSS('color', 'rgb(0, 0, 255)');
      await page.close();
    });

    test('journey 2: the bare proxy URL answers 308 to the trailing slash', async () => {
      const agentId = agentFor(transport);
      const bare = `${proxyBase(agentId)}?view=e2e&n=1`;
      const res = await context.request.get(bare, { maxRedirects: 0 });
      expect(res.status()).toBe(308);
      const location = res.headers()['location'] ?? '';
      expect(new URL(location, bare).toString()).toBe(`${proxyBase(agentId)}/?view=e2e&n=1`);
      await res.dispose();

      // A browser navigation follows it and lands on the proxy root.
      const page = await context.newPage();
      const nav = await page.goto(bare, { waitUntil: 'load' });
      expect(nav?.status()).toBe(200);
      expect(page.url()).toBe(`${proxyBase(agentId)}/?view=e2e&n=1`);
      await page.close();
    });
  });
}
