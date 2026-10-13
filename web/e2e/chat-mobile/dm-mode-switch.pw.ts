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
 * An agent DM reached by switching views comes back whole: the header names
 * the agent, the members sidebar lists the agent's project, and the
 * composer addresses the agent. A chat page built for the DM's URL (the
 * terminal pane's chat button, a dashboard round trip) has nothing to carry
 * that over from, so it works it out from the conversation key.
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread } from './fixture.js';
import { PROJECT_A } from './mock-api.js';

const USER_ID = '99999999-9999-4999-8999-999999999999';
const AGENT_ID = '11111111-1111-4111-8111-111111111111';
const AGENT_NAME = 'alpha-agent';
const DM_KEY = `dm:agent:${AGENT_ID}:user:${USER_ID}`;
const DM_PATH = `/chat/dm/${encodeURIComponent(DM_KEY)}`;

async function mocks(page: Page): Promise<void> {
  await page.addInitScript(() => {
    window.__SCION_FEATURES__ = {
      ...(window.__SCION_FEATURES__ ?? {}),
      'web.terminal_workspace': true,
    };
  });
  await page.route('**/auth/me', (route) =>
    route.fulfill({ json: { id: USER_ID, email: 'u@example.test' } })
  );
  await page.route(/\/api\/v1\/chat\/spaces\/([^/]+)\/members/, (route) =>
    route.fulfill({
      json: {
        humans: [],
        agents: [
          {
            id: AGENT_ID,
            kind: 'agent',
            displayName: AGENT_NAME,
            slug: AGENT_NAME,
            projectId: PROJECT_A.id,
          },
        ],
      },
    })
  );
  const agent = {
    id: AGENT_ID,
    name: AGENT_NAME,
    slug: AGENT_NAME,
    phase: 'running',
    projectId: PROJECT_A.id,
    canAttach: true,
    _capabilities: { actions: ['attach'] },
  };
  await page.route('**/api/v1/agents/**', (route) => {
    if (/\/agents\/[^/]+\/[a-z]+/.test(route.request().url())) {
      void route.fulfill({ json: {} });
      return;
    }
    void route.fulfill({ json: agent });
  });
  await page.routeWebSocket('**/pty?*', () => {});
}

async function switchMode(page: Page, mode: 'Dashboard' | 'Chat' | 'Terminal'): Promise<void> {
  const header = page.locator('scion-header:visible').first();
  const button = header.locator('.mode-switch button:visible', { has: page.getByText(mode) });
  if ((await button.count()) > 0) {
    await button.first().click();
    return;
  }
  await header.locator('.mode-trigger').click();
  await header.locator('sl-menu-item', { hasText: mode }).click();
}

const isPhone = (page: Page): boolean => (page.viewportSize()?.width ?? 0) <= 768;

/** The DM is shown whole: named header, project crumb, members, composer target. */
async function expectWholeAgentDM(page: Page): Promise<void> {
  await expect(page).toHaveURL(DM_PATH);
  await expect(page.locator('.v2-thread-header .conv-name')).toHaveAttribute('title', AGENT_NAME, {
    timeout: 15_000,
  });
  await expect(page.locator('.v2-thread-header .conv-crumb')).toHaveAttribute(
    'title',
    PROJECT_A.slug
  );
  await expect
    .poll(() =>
      page.evaluate(() => {
        const chat = document.querySelector('scion-page-chat') as
          | (HTMLElement & { v2AgentMembers?: Array<{ id: string }> })
          | null;
        return (chat?.v2AgentMembers ?? []).map((a) => a.id);
      })
    )
    .toContain(AGENT_ID);
  if (!isPhone(page)) {
    await expect(
      page.locator('scion-chat-members .member-item', { hasText: AGENT_NAME }).first()
    ).toBeVisible();
  }
  await expect
    .poll(() =>
      page.evaluate(
        () =>
          (
            document
              .querySelector('scion-page-chat')
              ?.shadowRoot?.querySelector('scion-chat-thread')
              ?.shadowRoot?.querySelector('scion-chat-composer') as
              | (HTMLElement & { peerName?: string })
              | null
          )?.peerName ?? ''
      )
    )
    .toBe(AGENT_NAME);
}

/** Open the agent's DM from the project's members list (an in-page open). */
async function openAgentDMFromMembers(page: Page): Promise<void> {
  await openChatRail(page, mocks);
  await openGeneralThread(page);
  if (isPhone(page)) {
    await page.locator('.mobile-members').click();
    await expect(page.locator('.v2-panels')).toHaveAttribute('data-panel', 'right');
  }
  await page.locator('scion-chat-members .member-item', { hasText: AGENT_NAME }).first().click();
  await expectWholeAgentDM(page);
}

test('the terminal pane chat button opens the agent DM whole', async ({ page }) => {
  await openChatRail(page, mocks);
  await page.evaluate(
    (id) =>
      document.dispatchEvent(
        new CustomEvent('nav-click', { detail: { path: `/terminals/${id}` } })
      ),
    AGENT_ID
  );
  await expect(page).toHaveURL(`/terminals/${AGENT_ID}`);
  await page.locator(`button[aria-label="Chat with ${AGENT_NAME}"]:visible`).first().click();
  await expectWholeAgentDM(page);
});

test('an agent DM survives terminal and dashboard round trips', async ({ page }) => {
  await openAgentDMFromMembers(page);

  await switchMode(page, 'Terminal');
  await expect(page).toHaveURL(/\/terminals/);
  await switchMode(page, 'Chat');
  await expectWholeAgentDM(page);

  await switchMode(page, 'Dashboard');
  await expect(page.locator('scion-page-chat')).toHaveCount(0);
  await switchMode(page, 'Chat');
  await expectWholeAgentDM(page);
});

test('a reload of an agent DM shows it whole', async ({ page }) => {
  await openAgentDMFromMembers(page);
  await page.reload();
  await expectWholeAgentDM(page);
});
