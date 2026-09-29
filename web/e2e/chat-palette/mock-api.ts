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

import type { Page } from '@playwright/test';

/** An agent with an existing DM — selecting it must reuse that DM, not create one. */
export const AGENT_WITH_DM = { id: 'agent-coder-one', name: 'Coder One', slug: 'coder-one' };
/** A viable agent with no DM yet — selecting it opens an empty DM, no create request. */
export const AGENT_WITHOUT_DM = { id: 'agent-review-bot', name: 'Review Bot', slug: 'review-bot' };
/** Explicitly non-messageable (messageability.canMessage=false) despite management capabilities — must not appear. */
export const AGENT_NOT_VIABLE = { id: 'agent-denied', name: 'Denied Agent', slug: 'denied-agent' };

export const SELF_USER_ID = 'self-user';

/** The agent a fixture terminal pane attaches to, for the real-xterm scenario. */
export const TERMINAL_AGENT_ID = '11111111-1111-4111-8111-111111111111';

export interface TrackedRequest {
  method: string;
  url: string;
  postData: string | null;
}

/**
 * chat.ts (and chat-members.ts/chat-thread.ts) import `navigateTo` and
 * `stateManager` from `client/main.js` — the app's real bootstrap module,
 * which self-initializes on `DOMContentLoaded` (SSR hydration, feature-flag
 * fetch, the full page router, admin-status probe...) the instant anything
 * imports it, real hub or not. That is exactly the router/bootstrap this
 * fixture deliberately does not run (it mounts scion-page-chat directly), so
 * the module is replaced at the network layer with the minimal real surface
 * those components actually call — this is the browser-test equivalent of
 * `vi.mock('../../client/main.js', ...)` in the vitest unit tests.
 */
export async function stubMainClientModule(page: Page): Promise<void> {
  await page.route('**/src/client/main.ts', (route) =>
    route.fulfill({
      contentType: 'text/javascript',
      body: `
        class FixtureStateManager extends EventTarget {
          currentScope = null;
          isConnected() { return false; }
          setScope() {}
          setCurrentUserId() {}
          hydrate() {}
          getAgent() { return undefined; }
          getAgents() { return new Map(); }
          getDeletedAgentIds() { return new Set(); }
          removeAgent() {}
          seedAgents() {}
        }
        export const stateManager = new FixtureStateManager();
        export function navigateTo(path) {
          const url = new URL(path, location.origin);
          history.pushState({}, '', url.pathname + url.search + url.hash);
          window.dispatchEvent(new PopStateEvent('popstate'));
        }
      `,
    })
  );
}

/**
 * Endpoint-shaped request interception for the chat palette fixture: every
 * `/api/v1/**` request is intercepted (no live Hub), with the specific
 * shapes the palette slice and the real `scion-page-chat`/`scion-chat-thread`
 * it mounts actually read. Anything not named here gets an empty but
 * well-typed response so an unrelated fetch elsewhere in the real page never
 * throws — it is not a claim that the palette itself uses it.
 */
export async function setupApiMocks(page: Page): Promise<TrackedRequest[]> {
  const requests: TrackedRequest[] = [];
  await stubMainClientModule(page);

  await page.route('**/api/v1/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const method = request.method();
    requests.push({ method, url: request.url(), postData: request.postData() });

    if (path === '/api/v1/auth/me') {
      return route.fulfill({
        json: { id: SELF_USER_ID, email: 'self@example.com', displayName: 'Self User' },
      });
    }
    if (path === '/api/v1/agents') {
      return route.fulfill({
        json: {
          agents: [
            { ...AGENT_WITH_DM, phase: 'running', _capabilities: { actions: ['attach'] } },
            { ...AGENT_WITHOUT_DM, phase: 'running', _capabilities: { actions: ['attach'] } },
            {
              ...AGENT_NOT_VIABLE,
              phase: 'running',
              _capabilities: { actions: ['lifecycle', 'attach'] },
              _messageability: { canMessage: false, canReachViewer: true },
            },
          ],
        },
      });
    }
    if (path === `/api/v1/agents/${TERMINAL_AGENT_ID}`) {
      // The terminal session's own attach flow (client/terminal-sessions.ts)
      // fetches this by exact ID and requires a matching id + running phase
      // before it will open the PTY WebSocket at all.
      return route.fulfill({
        json: {
          id: TERMINAL_AGENT_ID,
          name: 'fixture-terminal-agent',
          phase: 'running',
          projectId: 'fixture-project',
          harnessAuth: 'none',
          resolvedHarness: 'claude',
        },
      });
    }
    if (path === `/api/v1/agents/${TERMINAL_AGENT_ID}/pty`) {
      // Preflight authorization check only (client/terminal-sessions.ts) — a
      // 200 is all it inspects before opening the real WebSocket.
      return route.fulfill({ json: {} });
    }
    if (path === '/api/v1/chat/dms') {
      return route.fulfill({
        json: {
          dms: [
            {
              conversationKey: `dm:agent:${AGENT_WITH_DM.id}:user:${SELF_USER_ID}`,
              peerId: AGENT_WITH_DM.id,
              peerKind: 'agent',
              peerName: AGENT_WITH_DM.name,
              lastActivityAt: '2026-09-28T12:00:00Z',
            },
          ],
        },
      });
    }
    if (path === '/api/v1/chat/spaces') {
      return route.fulfill({ json: { spaces: [] } });
    }
    if (path === '/api/v1/users') {
      return route.fulfill({ json: { users: [] } });
    }
    if (path.endsWith('/messages') && method === 'GET') {
      return route.fulfill({ json: { messages: [] } });
    }
    if (path.endsWith('/read')) {
      return route.fulfill({ json: {} });
    }
    if (path.endsWith('/typing')) {
      return route.fulfill({ json: {} });
    }
    if (path === '/api/v1/chat/presence') {
      return route.fulfill({ json: {} });
    }
    // Unnamed endpoint: empty object keeps the real components' defensive
    // `data.foo ?? []`-style parsing harmless without asserting they call it.
    return route.fulfill({ json: {} });
  });

  return requests;
}
