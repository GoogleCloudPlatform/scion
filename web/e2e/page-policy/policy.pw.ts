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
 * Page script policy smoke (ptone/scion#5912): the shell's theme script
 * still runs, before the page is painted, under a page policy that allows
 * only the app's own scripts; the page runs with no enforced policy
 * violation; and the browser policy report endpoint accepts a report.
 */

import { expect, test, type Page } from '@playwright/test';

import { readHubState, type HubState } from '../harness/hub.js';
import { stateFile } from './state.js';

function hubState(): HubState {
  const state = readHubState(stateFile());
  if (!state) throw new Error('no hub state; did global-setup run?');
  return state;
}

/** What the init script records about the page. */
interface PolicyProbe {
  /** Whether data-theme was set while the document had no body yet. */
  themeSetBeforeBody: boolean | null;
  /** Enforced policy violations seen by the page. */
  enforced: string[];
}

declare global {
  interface Window {
    __policyProbe?: PolicyProbe;
  }
}

/**
 * Records, from before any page script runs, when the theme attribute is
 * set and every enforced policy violation. Report-only violations are
 * expected and ignored here.
 */
async function installProbe(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const probe: PolicyProbe = { themeSetBeforeBody: null, enforced: [] };
    window.__policyProbe = probe;
    document.addEventListener('securitypolicyviolation', (e) => {
      if (e.disposition === 'enforce') {
        probe.enforced.push(
          `${e.effectiveDirective} ${e.blockedURI} ${e.sourceFile}:${e.lineNumber}`
        );
      }
    });
    // Observed on the document, since the html element may not exist yet.
    new MutationObserver((_records, observer) => {
      if (document.documentElement?.hasAttribute('data-theme')) {
        probe.themeSetBeforeBody = document.body === null;
        observer.disconnect();
      }
    }).observe(document, { subtree: true, attributes: true, attributeFilter: ['data-theme'] });
  });
}

/** Collects console messages that report a blocked (enforced) resource. */
function collectBlocked(page: Page): string[] {
  const blocked: string[] = [];
  page.on('console', (msg) => {
    const text = msg.text();
    if (/Content Security Policy|Refused to/i.test(text) && !/\[Report Only\]/i.test(text)) {
      blocked.push(text);
    }
  });
  return blocked;
}

for (const scheme of ['dark', 'light'] as const) {
  test(`theme script runs before paint (${scheme}) with no enforced policy violation`, async ({
    browser,
  }) => {
    const state = hubState();
    const context = await browser.newContext({ baseURL: state.baseURL, colorScheme: scheme });
    const page = await context.newPage();
    await installProbe(page);
    const blocked = collectBlocked(page);

    const response = await page.goto('/login', { waitUntil: 'domcontentloaded' });
    expect(response?.status()).toBe(200);
    const headers = response?.headers() ?? {};
    const policy = headers['content-security-policy'] ?? '';
    const scriptSrc = policy.split(';').find((d) => d.trim().startsWith('script-src')) ?? '';
    expect(scriptSrc).toMatch(/'nonce-[A-Za-z0-9_-]+'/);
    expect(scriptSrc).not.toContain("'unsafe-inline'");
    expect(headers['content-security-policy-report-only']).toBe(
      "require-trusted-types-for 'script'; report-to csp"
    );

    // The theme script ran: without it the attribute is never set.
    await expect(page.locator('html')).toHaveAttribute('data-theme', scheme);
    if (scheme === 'dark') {
      await expect(page.locator('html')).toHaveClass(/sl-theme-dark/);
    }
    // The app itself started (its bundled scripts are allowed).
    await expect(page.locator('scion-login-page')).toBeAttached();
    await page.waitForFunction(() => customElements.get('scion-login-page') !== undefined);

    const probe = await page.evaluate(() => window.__policyProbe);
    expect(probe?.themeSetBeforeBody).toBe(true);
    expect(probe?.enforced).toEqual([]);
    expect(blocked).toEqual([]);

    // Positive control: an inline script without the nonce is blocked, and
    // the probe sees it, so the empty lists above are not vacuous.
    await page.evaluate(() => {
      const el = document.createElement('script');
      el.textContent = 'window.__inlineRan = true;';
      document.head.append(el);
    });
    await expect
      .poll(() => page.evaluate(() => window.__policyProbe?.enforced.length ?? 0))
      .toBe(1);
    const ran = await page.evaluate(
      () => (window as unknown as { __inlineRan?: boolean }).__inlineRan === true
    );
    expect(ran).toBe(false);
    await context.close();
  });
}

test('the report endpoint accepts a browser policy report', async ({ browser }) => {
  const state = hubState();
  const context = await browser.newContext({ baseURL: state.baseURL });
  const page = await context.newPage();
  await page.goto('/login', { waitUntil: 'domcontentloaded' });
  const status = await page.evaluate(async () => {
    const res = await fetch('/api/v1/csp-reports', {
      method: 'POST',
      headers: { 'Content-Type': 'application/reports+json' },
      credentials: 'omit',
      body: JSON.stringify([
        {
          type: 'csp-violation',
          body: {
            effectiveDirective: 'require-trusted-types-for',
            blockedURL: 'trusted-types-sink',
          },
        },
      ]),
    });
    return res.status;
  });
  expect(status).toBe(204);
  await context.close();
});
