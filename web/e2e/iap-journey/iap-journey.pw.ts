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
 * IAP browser journey (ptone/scion#4240, phase X5). OPERATOR-RUN ONLY:
 * it runs against a live IAP-fronted test hub named by a scion target-hub
 * profile, and skips with a clear reason unless IAP_JOURNEY=1 and its
 * environment are set (see README.md). No CI job or lane runs it.
 *
 * 1. The driver's IAP-audience ID token is obtained in-process from the
 *    runner's metadata emulator (driverIdTokenSource: direct, or
 *    impersonation through IAM Credentials). It stays in memory.
 * 2. The CLI issues a member test identity into the run cache through the
 *    profile and mints a web-login code into a task-owned 0600 file
 *    (`scion --hub-profile <p> hub test-identity web-code <id> --out`).
 * 3. The code is redeemed from Node with the driver token in
 *    Proxy-Authorization; the code file is deleted after the redeem.
 * 4. /auth/me and /api/v1/auth/me (Node side, then from the page through
 *    the IAP driver hook) show the fixture identity, never the driver.
 * 5. Teardown deletes the run's identities; /auth/me then answers 401.
 * 6. No file under the task directory or the test output holds a driver
 *    token, and no storage state is written anywhere.
 *
 * Logs name only the token path taken and HTTP status codes: no token,
 * code or email.
 */

import { execFile } from 'node:child_process';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import { promisify } from 'node:util';

import { expect, test, type APIRequestContext } from '@playwright/test';

import { redeemTestIdentity } from '../harness/auth.js';
import { driverIdTokenSource } from '../harness/driver-id-token.js';
import { iapJourneyConfig, profileTarget } from '../harness/iap-journey-env.js';
import { installIapDriver } from '../harness/iap-driver.js';

const run = promisify(execFile);

const resolved = iapJourneyConfig(process.env);

/** Runs the scion CLI with argv only (no secret in argv); returns stdout. */
async function scion(bin: string, args: string[]): Promise<string> {
  try {
    const { stdout } = await run(bin, args, { timeout: 120_000, maxBuffer: 1 << 20 });
    return stdout;
  } catch (err) {
    const e = err as { code?: unknown; stderr?: unknown };
    // The CLI never prints a token or code; still keep only the first line.
    const stderr = typeof e.stderr === 'string' ? e.stderr : '';
    const first = stderr.split('\n')[0];
    // Only the first stderr line is kept: the caught error carries the whole
    // CLI output and argv, so it is deliberately not attached as the cause.
    // eslint-disable-next-line preserve-caught-error -- see above
    throw new Error(
      `scion ${args
        .filter((a) => !a.startsWith('--'))
        .slice(0, 4)
        .join(' ')} failed (exit ${String(e.code)}): ${first}`
    );
  }
}

/** Every regular file under dir (no symlinks followed). */
function filesUnder(dir: string): string[] {
  if (!fs.existsSync(dir)) {
    return [];
  }
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) {
      return filesUnder(p);
    }
    return e.isFile() ? [p] : [];
  });
}

interface MeCheck {
  status: number;
  isFixture: boolean;
  isDriver: boolean;
  testIdentity: boolean;
}

/** GET path with the driver token in Proxy-Authorization; booleans only. */
async function checkMe(
  request: APIRequestContext,
  url: string,
  proxyAuthorization: string,
  fixtureEmail: string,
  driverEmail: string
): Promise<MeCheck> {
  const res = await request.get(url, {
    headers: { 'Proxy-Authorization': proxyAuthorization },
    maxRedirects: 0,
  });
  const status = res.status();
  let email = '';
  let testIdentity = false;
  if (status === 200) {
    const body = (await res.json()) as { email?: unknown; testIdentity?: { expiresAt?: unknown } };
    email = typeof body.email === 'string' ? body.email : '';
    testIdentity = typeof body.testIdentity?.expiresAt === 'string';
  }
  await res.dispose();
  return {
    status,
    isFixture: email !== '' && email === fixtureEmail,
    isDriver: email !== '' && email.toLowerCase() === driverEmail.toLowerCase(),
    testIdentity,
  };
}

test.describe('IAP browser journey (operator-run)', () => {
  test.skip('skip' in resolved, 'skip' in resolved ? resolved.skip : '');

  test('a test identity browser session on an IAP-fronted hub, with no token file', async ({
    browser,
  }, testInfo) => {
    if (!('config' in resolved)) {
      return;
    }
    const cfg = resolved.config;
    const prof = ['--hub-profile', cfg.profile];

    const target = profileTarget(
      await scion(cfg.scionBin, ['hub', 'profile', 'show', cfg.profile, '--json'])
    );
    const hub = target.hubURL;

    // Every driver token handed out, kept in memory for the file scan.
    const tokens = new Set<string>();
    const source = driverIdTokenSource({
      driverServiceAccount: target.driverServiceAccount,
      audience: target.iapAudience,
      onToken: (info) =>
        console.log(`driver token: path=${info.path} statuses=${info.statuses.join(',')}`),
    });
    const driverToken = async (): Promise<string> => {
      const t = await source();
      tokens.add(t);
      return t;
    };

    // Task-owned private directory, symlinks resolved (web-code refuses a
    // path under a symlinked directory).
    const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'scion-iapj-')));
    fs.chmodSync(dir, 0o700);
    const codeFile = path.join(dir, 'code.txt');
    let issued = false;
    let failed = false;
    const context = await browser.newContext({ baseURL: hub });
    try {
      // 2. Issue a member identity into the run cache, then mint a code.
      // Marked issued first: a CLI failure after the hub issued it still
      // tears the run down (a teardown of an empty run is harmless).
      issued = true;
      const out = await scion(cfg.scionBin, [
        ...prof,
        'hub',
        'test-identity',
        'issue',
        '--as',
        'journey',
        '--run',
        cfg.run,
        '--role',
        'member',
        '--purpose',
        'iap browser journey',
        '--json',
      ]);
      const ident = (JSON.parse(out) as { identity?: { id?: unknown; email?: unknown } }).identity;
      const identityId = typeof ident?.id === 'string' ? ident.id : '';
      const fixtureEmail = typeof ident?.email === 'string' ? ident.email : '';
      expect(identityId, 'issue returned an identity id').not.toBe('');
      expect(fixtureEmail.endsWith('@scion-fixture.invalid'), 'the identity is a fixture').toBe(
        true
      );

      await scion(cfg.scionBin, [
        ...prof,
        'hub',
        'test-identity',
        'web-code',
        identityId,
        '--out',
        codeFile,
      ]);
      expect((fs.lstatSync(codeFile).mode & 0o777).toString(8), 'code file mode').toBe('600');

      // 3. Redeem from Node, before any navigation; the helper deletes the
      // code file after the redeem.
      const proxy = (): Promise<string> => driverToken().then((t) => `Bearer ${t}`);
      await redeemTestIdentity(context, {
        codeFile,
        baseURL: hub,
        proxyAuthorization: await proxy(),
      });
      expect(fs.existsSync(codeFile), 'the code file is deleted after redeem').toBe(false);

      // 4. The session is the fixture, never the driver.
      for (const p of ['/auth/me', '/api/v1/auth/me']) {
        const me = await checkMe(
          context.request,
          p,
          await proxy(),
          fixtureEmail,
          target.driverServiceAccount
        );
        console.log(`${p}: ${me.status}`);
        expect(me.status, p).toBe(200);
        expect(me.isDriver, `${p} is not the driver`).toBe(false);
        expect(me.isFixture, `${p} is the fixture identity`).toBe(true);
        if (p === '/auth/me') {
          expect(me.testIdentity, `${p} is a test identity session`).toBe(true);
        }
      }

      const uninstall = await installIapDriver(context, { baseURL: hub, token: driverToken });
      const page = await context.newPage();
      const nav = await page.goto('/', { waitUntil: 'domcontentloaded' });
      console.log(`page /: ${nav?.status() ?? 0}`);
      expect(nav?.status(), 'the page loads through IAP').toBe(200);
      const pageMe = await page.evaluate(async () => {
        const r = await fetch('/api/v1/auth/me');
        const b = r.ok ? ((await r.json()) as { email?: unknown }) : {};
        return { status: r.status, email: typeof b.email === 'string' ? b.email : '' };
      });
      console.log(`page /api/v1/auth/me: ${pageMe.status}`);
      expect(pageMe.status).toBe(200);
      expect(
        pageMe.email.toLowerCase() === target.driverServiceAccount.toLowerCase(),
        'page identity is not the driver'
      ).toBe(false);
      expect(pageMe.email === fixtureEmail, 'page identity is the fixture').toBe(true);
      await page.close();
      await uninstall();

      // 5. Teardown ends the session.
      await scion(cfg.scionBin, [...prof, 'hub', 'test-identity', 'teardown', '--run', cfg.run]);
      issued = false;
      const after = await checkMe(
        context.request,
        '/auth/me',
        await proxy(),
        fixtureEmail,
        target.driverServiceAccount
      );
      console.log(`/auth/me after teardown: ${after.status}`);
      expect(after.status, 'the session ended with the identity').toBe(401);
    } catch (err) {
      failed = true;
      throw err;
    } finally {
      await context.close();
      if (issued) {
        await scion(cfg.scionBin, [
          ...prof,
          'hub',
          'test-identity',
          'teardown',
          '--run',
          cfg.run,
        ]).catch((err: unknown) =>
          console.log(`teardown failed: ${String((err as Error)?.message ?? err).split('\n')[0]}`)
        );
      }
      // 6. No file holds a driver token or a storage state.
      const scanned = [...filesUnder(dir), ...filesUnder(testInfo.outputDir)];
      const leaks = scanned.filter((f) => {
        const text = fs.readFileSync(f, 'latin1');
        return [...tokens].some((t) => text.includes(t)) || /"cookies"\s*:\s*\[/.test(text);
      });
      fs.rmSync(dir, { recursive: true, force: true });
      if (failed) {
        // Keep the original error; only report a leak next to it.
        if (leaks.length > 0) {
          console.log(`also: ${leaks.length} files hold a driver token or a storage state`);
        }
      } else {
        expect(leaks.length, 'files holding a driver token or a storage state').toBe(0);
        expect(tokens.size, 'driver tokens were obtained').toBeGreaterThan(0);
      }
    }
  });
});
