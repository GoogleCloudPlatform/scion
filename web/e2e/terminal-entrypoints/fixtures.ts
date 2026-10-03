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
 * API mocks and helpers shared by the terminal entry-point and graph
 * palette suites: a fixture user and agents, feature flags, the agent and
 * project endpoints, and a pty WebSocket stub.
 */

import type { Page } from '@playwright/test';

// ---------------------------------------------------------------------------
// Fixture agent UUIDs — trusted format matching the router regex.
// ---------------------------------------------------------------------------
export const agentA = '11111111-1111-4111-8111-111111111111';
export const agentB = '22222222-2222-4222-8222-222222222222';

// Fixture project
export const projectId = 'fixture-project';

// ---------------------------------------------------------------------------
// API fixture helpers
// ---------------------------------------------------------------------------

export interface AgentFixture {
  id: string;
  name: string;
  phase: string;
  projectId: string;
  slug?: string;
  canAttach?: boolean;
  activity?: string;
  ancestry?: string[];
}

export const defaultAgents: Record<string, AgentFixture> = {
  [agentA]: {
    id: agentA,
    name: 'alpha-agent',
    phase: 'running',
    projectId,
    slug: 'alpha-agent',
    canAttach: true,
  },
  [agentB]: {
    id: agentB,
    name: 'beta-agent',
    phase: 'running',
    projectId,
    slug: 'beta-agent',
    canAttach: true,
  },
};

/** Build the API-shaped agent object with _capabilities. */
export function apiAgent(a: AgentFixture): Record<string, unknown> {
  return {
    ...a,
    _capabilities: a.canAttach !== false ? { actions: ['attach'] } : { actions: [] },
  };
}

export interface SetupResult {
  readonly attaches: number;
  readonly closes: number;
  sent: string[];
}

export async function setup(
  page: Page,
  opts: {
    enabled?: boolean;
    agents?: Record<string, AgentFixture>;
    nativeChatEnabled?: boolean;
  } = {}
): Promise<SetupResult> {
  const { enabled = true, agents = defaultAgents, nativeChatEnabled = true } = opts;

  let attaches = 0;
  let closes = 0;
  const sent: string[] = [];

  await page.addInitScript(
    ({ enabled: e }) => {
      window.__SCION_FEATURES__ = {
        'web.terminal_workspace': e,
        'web.native_chat': true,
      };
      window.EventSource = class extends EventTarget {
        onopen: (() => void) | null = null;
        constructor() {
          super();
          queueMicrotask(() => this.onopen?.());
        }
        close(): void {}
      } as unknown as typeof EventSource;
    },
    { enabled }
  );

  await page.route('**/auth/me', (route) =>
    route.fulfill({ json: { id: 'fixture-user', email: 'fixture@example.test' } })
  );
  await page.route('**/api/v1/settings/public', (route) =>
    route.fulfill({ json: { nativeChatEnabled } })
  );
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );

  // Agent detail + sub-resources (paths with segments after /agents/).
  await page.route('**/api/v1/agents/**', (route) => {
    const url = route.request().url();

    // PTY metadata stub
    if (url.endsWith('/pty')) {
      void route.fulfill({ json: {} });
      return;
    }

    // Sub-resource stubs (metrics, etc.)
    if (url.match(/\/agents\/[^/]+\/(metrics|start|stop|suspend|resume)/)) {
      void route.fulfill({ json: {} });
      return;
    }

    // Extract the UUID from the path
    const id =
      url.match(
        /\/api\/v1\/agents\/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/i
      )?.[1] ?? agentA;
    void route.fulfill({
      status: agents[id] ? 200 : 404,
      json: agents[id] ? apiAgent(agents[id]) : { error: 'not found' },
    });
  });

  // Agent list (bare /api/v1/agents with optional query string).
  await page.route(/\/api\/v1\/agents(\?|$)/, (route) => {
    void route.fulfill({
      json: Object.values(agents).map((a) => apiAgent(a)),
    });
  });

  // Project API
  await page.route('**/api/v1/projects/**', (route) => {
    void route.fulfill({
      json: {
        id: projectId,
        name: 'Fixture Project',
        slug: projectId,
        agents: Object.values(agents).map((a) => apiAgent(a)),
        _capabilities: { actions: ['attach'] },
      },
    });
  });

  // Notifications API stub (used by agent detail page)
  await page.route('**/api/v1/notifications**', (route) =>
    route.fulfill({ json: { userNotifications: [], subscriptions: [] } })
  );

  // Auth admin status stub
  await page.route('**/api/v1/auth/admin-status', (route) =>
    route.fulfill({ json: { isAdmin: false, isSuperAdmin: false, permissions: [] } })
  );

  // Chat API stubs
  await page.route('**/api/v2/chat/**', (route) => void route.fulfill({ json: {} }));
  await page.route('**/api/v1/chat/**', (route) => void route.fulfill({ json: {} }));

  // WebSocket for terminal pty
  await page.routeWebSocket('**/pty?*', (socket) => {
    attaches++;
    socket.onMessage((message) => sent.push(String(message)));
    socket.onClose(() => closes++);
  });

  return {
    get attaches(): number {
      return attaches;
    },
    get closes(): number {
      return closes;
    },
    sent,
  };
}

/**
 * Switch the agents or project page view mode by dispatching a synthetic
 * view-change event on the scion-view-toggle component, then toggling the
 * localStorage key so the component re-renders.
 */
export async function switchView(page: Page, storageKey: string, mode: string): Promise<void> {
  await page.evaluate(
    ({ storageKey: k, mode: m }) => {
      localStorage.setItem(k, m);
      const toggle = document.querySelector('scion-view-toggle');
      if (toggle) {
        toggle.dispatchEvent(
          new CustomEvent('view-change', { detail: { view: m }, bubbles: true, composed: true })
        );
      }
    },
    { storageKey, mode }
  );
}
