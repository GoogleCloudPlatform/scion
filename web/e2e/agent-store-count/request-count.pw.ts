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
 * Agent-list requests per user action, on the full app over a mocked hub.
 *
 * Every surface that shows "the agents of the hub" reads the agent store's
 * one hub entry: the chat quick switcher and the terminal workspace's
 * "Jump to agent" palette share a single walk, a reopen answers from
 * memory, and moving between pages that share the entry walks nothing
 * new. The graph views' palette reads the graph it is opened on and
 * requests nothing. Each scenario runs with immediate and with delayed
 * agent-list responses, since how the loads overlap depends on timing.
 *
 * A walk is a list request without a cursor (a probe, `sort=updated`, is
 * counted apart). Store walks are told apart from the chat sidebar's own
 * hub walk, which is not on the store, by the store's compact view. The
 * hub holds enough agents for a store walk to read several pages, and each
 * walk must read each of them once.
 */

import { test, expect, type Page } from '@playwright/test';
import {
  AGENT_COUNT,
  PROJECT_ID,
  STORE_PAGES_PER_WALK,
  STORE_PAGE_SIZE,
  agentId,
  paletteState,
  setupHub,
  type Hub,
} from './fixtures.js';

const CHAT_PALETTE = 'Quick switcher';
const TERMINAL_PALETTE = 'Jump to agent';

/** Waits until the palette is open with its Agents group ready, holding `count` agents. */
async function expectPaletteReady(page: Page, label: string, count: number): Promise<void> {
  await expect
    .poll(
      async () => {
        const state = await paletteState(page, label);
        return state ? `${state.open}:${state.status}:${state.agentIds.length}` : 'absent';
      },
      { timeout: 15_000 }
    )
    .toBe(`true:ready:${count}`);
}

/** Closes the open palette with Escape and waits out its close animation. */
async function closePalette(page: Page, label: string): Promise<void> {
  await page.keyboard.press('Escape');
  await expect.poll(async () => (await paletteState(page, label))?.open).toBe(false);
  await expect
    .poll(() =>
      page.evaluate((l) => {
        const find = (
          window as unknown as { __findPalette: (label: string) => Element | undefined }
        ).__findPalette;
        const el = find(l);
        const panel = el?.shadowRoot
          ?.querySelector('sl-dialog')
          ?.shadowRoot?.querySelector('.dialog');
        return !panel || panel.getAnimations({ subtree: true }).length === 0;
      }, label)
    )
    .toBe(true);
}

async function openChat(page: Page): Promise<void> {
  await page.goto('/chat');
  await expect(page.locator('scion-page-chat')).toBeAttached();
}

async function openChatPalette(page: Page, count = AGENT_COUNT): Promise<void> {
  await page.keyboard.press('Control+k');
  await expectPaletteReady(page, CHAT_PALETTE, count);
}

async function openTerminalPalette(page: Page, count = AGENT_COUNT): Promise<void> {
  await page.keyboard.press('Meta+k');
  await expectPaletteReady(page, TERMINAL_PALETTE, count);
}

/** Client-side navigation, as the app's own links do: the chat page stays mounted. */
async function navigate(page: Page, path: string): Promise<void> {
  await page.evaluate(
    (p) => document.dispatchEvent(new CustomEvent('nav-click', { detail: { path: p } })),
    path
  );
  await expect.poll(() => new URL(page.url()).pathname).toBe(path);
}

/** Lets any request a page started on load or navigation go out. */
async function settle(page: Page): Promise<void> {
  await page.waitForLoadState('networkidle');
  await page.waitForTimeout(300);
}

function expectNoProbes(hub: Hub): void {
  expect(hub.requests.filter((r) => r.probe)).toEqual([]);
}

/** Exactly one store walk, which read every page once. */
function expectOneStoreWalk(hub: Hub): void {
  expect(hub.storeWalks()).toBe(1);
  const cursors = Array.from({ length: STORE_PAGES_PER_WALK - 1 }, (_, i) =>
    String((i + 1) * STORE_PAGE_SIZE)
  );
  expect(hub.storeCursors()).toEqual(cursors);
  expectNoProbes(hub);
}

for (const latencyMs of [0, 800]) {
  test.describe(`agent-list responses after ${latencyMs}ms`, () => {
    let hub: Hub;

    test.beforeEach(async ({ page }) => {
      hub = await setupHub(page, { latencyMs });
    });

    test('/chat, then the quick switcher, closed and reopened: one store walk', async ({
      page,
    }) => {
      await openChat(page);
      await settle(page);
      const sidebarWalks = hub.otherWalks();
      await openChatPalette(page);
      await closePalette(page, CHAT_PALETTE);
      await openChatPalette(page);
      await closePalette(page, CHAT_PALETTE);
      await settle(page);

      expectOneStoreWalk(hub);
      expect(hub.otherWalks()).toBe(sidebarWalks);
    });

    test('/terminals/:id mounts with no agent-list request', async ({ page }) => {
      await page.goto(`/terminals/${agentId(1)}`);
      await expect(page.locator('scion-terminal-pane')).toBeAttached();
      await settle(page);

      expect(hub.requests).toEqual([]);
    });

    test('/terminals/:id, then Jump to agent three times: one store walk', async ({ page }) => {
      await page.goto(`/terminals/${agentId(1)}`);
      await expect(page.locator('scion-terminal-pane')).toBeAttached();

      for (let i = 0; i < 3; i++) {
        await openTerminalPalette(page);
        await closePalette(page, TERMINAL_PALETTE);
      }
      await settle(page);

      expectOneStoreWalk(hub);
      expect(hub.otherWalks()).toBe(0);
    });

    test('/chat, /terminals/:id with Jump to agent three times, back, then the quick switcher: one store walk', async ({
      page,
    }) => {
      await openChat(page);
      await settle(page);
      const sidebarWalks = hub.otherWalks();

      await navigate(page, `/terminals/${agentId(1)}`);
      await expect(page.locator('scion-terminal-pane')).toBeAttached();
      for (let i = 0; i < 3; i++) {
        await openTerminalPalette(page);
        await closePalette(page, TERMINAL_PALETTE);
      }
      await page.goBack();
      await expect.poll(() => new URL(page.url()).pathname).toBe('/chat');
      await openChatPalette(page);
      await closePalette(page, CHAT_PALETTE);
      await settle(page);

      expectOneStoreWalk(hub);
      // The palettes add no walk of their own beyond the store's.
      expect(hub.otherWalks()).toBe(sidebarWalks);
    });

    test('the quick switcher opened while the Jump to agent walk is in flight joins it', async ({
      page,
    }) => {
      await openChat(page);
      await navigate(page, `/terminals/${agentId(1)}`);
      await expect(page.locator('scion-terminal-pane')).toBeAttached();
      // The walk Jump to agent starts stays on its first page until the quick
      // switcher has opened. The chat page, still mounted, retains the entry,
      // so closing Jump to agent and leaving /terminals keep the walk going.
      const release = hub.holdNextStoreWalk();
      await page.keyboard.press('Meta+k');
      await expect.poll(() => hub.storeWalks()).toBe(1);
      await page.keyboard.press('Escape');
      await page.goBack();
      await expect.poll(() => new URL(page.url()).pathname).toBe('/chat');
      await page.keyboard.press('Control+k');
      await expect
        .poll(async () => {
          const state = await paletteState(page, CHAT_PALETTE);
          return state ? `${state.open}:${state.status}` : 'absent';
        })
        .toBe('true:loading');
      // Still in flight: only the walk's held first page has been requested.
      expect(hub.requests.filter((r) => r.store)).toHaveLength(1);

      release();
      await expectPaletteReady(page, CHAT_PALETTE, AGENT_COUNT);
      await closePalette(page, CHAT_PALETTE);
      await settle(page);

      expectOneStoreWalk(hub);
    });

    test('hub events reach both open palettes with no agent-list request', async ({ page }) => {
      await openChat(page);
      await openChatPalette(page);
      await settle(page);
      const before = hub.total();

      // A deleted agent leaves the open quick switcher.
      await hub.emit(`project.${PROJECT_ID}.agent.deleted`, {
        agentId: agentId(1),
        projectId: PROJECT_ID,
      });
      await expectPaletteReady(page, CHAT_PALETTE, AGENT_COUNT - 1);
      expect((await paletteState(page, CHAT_PALETTE))!.agentIds).not.toContain(agentId(1));
      await closePalette(page, CHAT_PALETTE);

      await navigate(page, `/terminals/${agentId(5)}`);
      await expect(page.locator('scion-terminal-pane')).toBeAttached();
      await openTerminalPalette(page, AGENT_COUNT - 1);
      expect((await paletteState(page, TERMINAL_PALETTE))!.agentIds).not.toContain(agentId(1));

      // A stopped agent leaves the open Jump to agent palette.
      await hub.emit(`project.${PROJECT_ID}.agent.status`, {
        agentId: agentId(2),
        projectId: PROJECT_ID,
        phase: 'stopped',
      });
      await expectPaletteReady(page, TERMINAL_PALETTE, AGENT_COUNT - 2);
      expect((await paletteState(page, TERMINAL_PALETTE))!.agentIds).not.toContain(agentId(2));
      await closePalette(page, TERMINAL_PALETTE);
      await settle(page);

      expectOneStoreWalk(hub);
      expect(hub.total()).toBe(before);
    });
  });
}

/** The graph views' "Jump to agent": each host's palette reads the graph it shows. */
const graphHosts = [
  { path: '/agents', storage: { 'scion-view-agents': 'graph' } },
  { path: '/agents/graph', storage: {} },
  { path: `/projects/${PROJECT_ID}`, storage: { 'scion-view-project-agents': 'graph' } },
];

for (const host of graphHosts) {
  test(`the graph palette on ${host.path} opens twice with no agent-list request`, async ({
    page,
  }) => {
    const hub = await setupHub(page);
    await page.addInitScript((entries) => {
      for (const [key, value] of Object.entries(entries)) localStorage.setItem(key, value);
    }, host.storage);
    await page.goto(host.path);
    await expect(page.locator('scion-agent-tree-view a.node').first()).toBeVisible();
    await settle(page);
    const before = hub.total();

    for (let i = 0; i < 2; i++) {
      await page.keyboard.press('Control+k');
      await expect
        .poll(async () => {
          const state = await paletteState(page, TERMINAL_PALETTE);
          return state ? `${state.open}:${state.agentIds.length}` : 'absent';
        })
        .toBe(`true:${AGENT_COUNT}`);
      await closePalette(page, TERMINAL_PALETTE);
    }
    await settle(page);

    expect(hub.total()).toBe(before);
  });
}
