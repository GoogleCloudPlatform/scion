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
 * Test identity browser session smoke (ptone/scion#4240, WS3).
 *
 * On a throwaway member-level test hub without test-login:
 *
 * 1. The dev-auth admin issues a member test identity with the CLI
 *    (`scion hub test-identity issue`) and mints a single-use web-login
 *    code (`scion hub test-identity web-code --out`).
 * 2. redeemTestIdentity() redeems the code in a fresh browser context,
 *    before any navigation; assertTestIdentity() checks the identity.
 * 3. A UI journey runs as the identity: the dashboard shows the test
 *    identity chip, the Projects page loads its data through the session,
 *    and no admin navigation is offered. The identity is asserted again.
 * 4. Teardown deletes the identity; the next /auth/me answers 401.
 */

import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';

import { expect, test } from '@playwright/test';

import { assertTestIdentity, redeemTestIdentity } from '../harness/auth.js';
import { readHubState, runScionCLI, type HubState } from '../harness/hub.js';
import { stateFile } from './state.js';

interface IssuedIdentity {
  identity: { id: string; email: string; role: string };
}

function hubState(): HubState {
  const file = stateFile();
  const state = readHubState(file);
  if (!state?.homeDir) {
    throw new Error(`No member-level test hub state at ${file}; did global-setup run?`);
  }
  return state;
}

test('a test identity holds a browser session for a UI journey', async ({ browser }) => {
  const state = hubState();
  // Task-owned private directory for the token and code files. Resolved,
  // since web-code refuses a path under a symlinked directory (macOS /var).
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'scion-e2e-ti-')));
  fs.chmodSync(dir, 0o700);
  let identityId = '';
  let deleted = false;

  try {
    // 1. Issue a member test identity (the dev-auth admin is the issuer).
    const tokenFile = path.join(dir, 'identity.token');
    const issued = JSON.parse(
      runScionCLI(state, [
        'hub',
        'test-identity',
        'issue',
        '--role',
        'member',
        '--purpose',
        'e2e web session smoke',
        '--out',
        tokenFile,
        '--json',
      ])
    ) as IssuedIdentity;
    identityId = issued.identity.id;
    const email = issued.identity.email;
    expect(email).toMatch(/@scion-fixture\.invalid$/);
    // The browser path needs no API token.
    fs.rmSync(tokenFile, { force: true });

    // Mint the web-login code into a 0600 file.
    const codeFile = path.join(dir, 'code.txt');
    const webCodeOut = runScionCLI(state, [
      'hub',
      'test-identity',
      'web-code',
      identityId,
      '--out',
      codeFile,
    ]);
    expect(webCodeOut).toContain(`Web-login code for test identity ${identityId}`);
    expect(fs.statSync(codeFile).mode & 0o777).toBe(0o600);

    // 2. Redeem before any navigation, in a fresh context.
    const context = await browser.newContext({ baseURL: state.baseURL });
    await redeemTestIdentity(context, { codeFile });
    expect(fs.existsSync(codeFile), 'the code file is removed after the redeem').toBe(false);

    const afterRedeem = await assertTestIdentity(context.request, { email, role: 'member' });
    expect(afterRedeem.role).toBe('member');
    expect(Date.parse(afterRedeem.expiresAt)).toBeGreaterThan(Date.now());

    // 3. UI journey as the test identity.
    const page = await context.newPage();
    await page.goto('/', { waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('test-identity-chip')).toContainText(`Test identity ${email}`);

    const projectsLoaded = page.waitForResponse(
      (res) =>
        new URL(res.url()).pathname === '/api/v1/projects' && res.request().method() === 'GET'
    );
    await page.locator('scion-nav a[href="/projects"]').filter({ visible: true }).first().click();
    await expect(page).toHaveURL(/\/projects$/);
    expect((await projectsLoaded).status()).toBe(200);
    await expect(
      page.locator('scion-page-projects').getByRole('heading', { level: 1, name: 'Projects' })
    ).toBeVisible();
    await expect(page.getByTestId('test-identity-chip')).toContainText(email);
    // A member session: no admin navigation.
    await expect(page.locator('scion-nav a[href="/admin/server-config"]')).toHaveCount(0);

    const afterJourney = await assertTestIdentity(page, { email, role: 'member' });
    expect(afterJourney.expiresAt).toBe(afterRedeem.expiresAt);

    // 4. Teardown. Close the page first so no background request (SSE)
    // consumes the ended session before the check below.
    await page.close();
    runScionCLI(state, ['hub', 'test-identity', 'delete', identityId]);
    deleted = true;
    const me = await context.request.get('/auth/me', { maxRedirects: 0 });
    expect(me.status(), 'the session ends with the identity').toBe(401);
    await me.dispose();
    await context.close();
  } finally {
    if (identityId && !deleted) {
      try {
        runScionCLI(state, ['hub', 'test-identity', 'delete', identityId]);
      } catch (err) {
        console.warn(`[teardown] ${String((err as Error).message)}`);
      }
    }
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('redeem and assert failures report the status and hub error code only', async ({
  browser,
}) => {
  const state = hubState();
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'scion-e2e-ti-')));
  fs.chmodSync(dir, 0o700);
  let identityId = '';

  try {
    const contextA = await browser.newContext({ baseURL: state.baseURL });
    const contextB = await browser.newContext({ baseURL: state.baseURL });

    // Exactly one of code and codeFile.
    await expect(redeemTestIdentity(contextA, {})).rejects.toThrow(
      /exactly one of code and codeFile/
    );

    // An unknown code: 401 invalid_or_expired_code. The message carries
    // neither the code nor a body, and the code file is kept.
    const marker = 'NOT-A-REAL-CODE-MARKER-3f9d';
    const bogusFile = path.join(dir, 'bogus.txt');
    fs.writeFileSync(bogusFile, `tiwc_${marker}`, { mode: 0o600 });
    const bogus = await redeemTestIdentity(contextA, { codeFile: bogusFile }).then(
      () => null,
      (err: Error) => err
    );
    expect(bogus?.message).toBe(
      'redeemTestIdentity: redeem answered 401 (invalid_or_expired_code), want 204'
    );
    expect(fs.existsSync(bogusFile), 'a failed redeem keeps the code file').toBe(true);

    // A real code redeems once; the second redeem of the same code fails.
    const tokenFile = path.join(dir, 'identity.token');
    const issued = JSON.parse(
      runScionCLI(state, [
        'hub',
        'test-identity',
        'issue',
        '--role',
        'viewer',
        '--purpose',
        'e2e web session errors',
        '--out',
        tokenFile,
        '--json',
      ])
    ) as IssuedIdentity;
    identityId = issued.identity.id;
    fs.rmSync(tokenFile, { force: true });
    const codeFile = path.join(dir, 'code.txt');
    runScionCLI(state, ['hub', 'test-identity', 'web-code', identityId, '--out', codeFile]);
    const code = fs.readFileSync(codeFile, 'utf-8');

    await redeemTestIdentity(contextA, { code });
    const again = await redeemTestIdentity(contextB, { code }).then(
      () => null,
      (err: Error) => err
    );
    expect(again?.message).toBe(
      'redeemTestIdentity: redeem answered 401 (invalid_or_expired_code), want 204'
    );
    expect(again?.message).not.toContain(code);

    // assertTestIdentity: the right identity passes; a role or email
    // mismatch fails; a context without a session fails with 401.
    const info = await assertTestIdentity(contextA.request, { email: issued.identity.email });
    expect(info.role).toBe('viewer');
    await expect(
      assertTestIdentity(contextA.request, { email: issued.identity.email, role: 'member' })
    ).rejects.toThrow(/session role is viewer, want member/);
    await expect(
      assertTestIdentity(contextA.request, { email: 'someone-else@scion-fixture.invalid' })
    ).rejects.toThrow(/session email is/);
    // contextB holds no session: with dev-auth on, the hub signs it in as
    // the dev admin, which is not a test identity session.
    await expect(
      assertTestIdentity(contextB.request, { email: issued.identity.email })
    ).rejects.toThrow(/assertTestIdentity: /);

    await contextA.close();
    await contextB.close();
  } finally {
    if (identityId) {
      try {
        runScionCLI(state, ['hub', 'test-identity', 'delete', identityId]);
      } catch (err) {
        console.warn(`[teardown] ${String((err as Error).message)}`);
      }
    }
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
