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

// ===========================================================================
// People and Threads fixtures.
// ===========================================================================

/** A person with an existing DM — mirrors AGENT_WITH_DM's shape for People. */
export const USER_WITH_DM = {
  id: 'user-with-dm',
  displayName: 'Dana Person',
  email: 'dana@example.com',
};
/**
 * A person whose ID sorts *before* `SELF_USER_ID` ("self-user") — both
 * `USER_WITH_DM`/`USER_WITHOUT_DM` sort after it, so neither alone proves the
 * sorted-user DM key actually sorts (covers both ID orderings).
 */
export const USER_SORTS_BEFORE_SELF = {
  id: 'a-user-early',
  displayName: 'Amy Early',
  email: 'amy@example.com',
};
/** A viable person with no DM yet. */
export const USER_WITHOUT_DM = {
  id: 'user-without-dm',
  displayName: 'Eve Person',
  email: 'eve@example.com',
};
/**
 * A suspended user — must never appear as a candidate. The real backend's
 * status enum is active/suspended/invited, with no literal "disabled"
 * value; any non-"active" status is treated as not viable.
 */
export const USER_SUSPENDED = {
  id: 'user-suspended',
  displayName: 'Frank Suspended',
  email: 'frank@example.com',
  status: 'suspended',
};

/** Space Alpha: its slug is known. Thread in Alpha is the palette's starting context. */
export const SPACE_ALPHA = {
  projectId: 'project-alpha',
  projectName: 'Alpha',
  projectSlug: 'alpha',
};
export const THREAD_ALPHA = {
  id: 'thread-alpha',
  projectId: SPACE_ALPHA.projectId,
  name: 'General',
};
/** Space Beta: a different project the palette must switch into without recreating the page. */
export const SPACE_BETA = { projectId: 'project-beta', projectName: 'Beta', projectSlug: 'beta' };
export const THREAD_BETA = { id: 'thread-beta', projectId: SPACE_BETA.projectId, name: 'Planning' };

/** A distinguishing member per space, so a test can prove `loadV2Members` actually re-ran. */
export const SPACE_MEMBERS: Record<
  string,
  { humans: Array<{ id: string; kind: 'user'; displayName: string }> }
> = {
  [SPACE_ALPHA.projectId]: {
    humans: [{ id: 'alpha-member', kind: 'user', displayName: 'Alpha Member' }],
  },
  [SPACE_BETA.projectId]: {
    humans: [{ id: 'beta-member', kind: 'user', displayName: 'Beta Member' }],
  },
};

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
 * Optional People/Threads fixture data. Every field defaults to empty, so a
 * spec calling `setupApiMocks(page)` with no second argument gets 0 threads
 * and 0 people; passing overrides opts a spec into real Threads/People rows.
 */
export interface PaletteFixtureOverrides {
  spaces?: Array<{ projectId: string; projectName: string; projectSlug: string }>;
  threadsByProjectId?: Record<
    string,
    Array<{ id: string; projectId: string; name: string; defaultAgent?: string }>
  >;
  users?: Array<{ id: string; displayName: string; email?: string; status?: string }>;
  membersByProjectId?: Record<
    string,
    { humans: Array<{ id: string; kind: 'user'; displayName: string }> }
  >;
  /** `GET /api/v1/chat/attachments/{id}` bodies, for the Documents preview. */
  attachmentsById?: Record<string, { mime: string; body: string }>;
  /** `GET /api/v1/projects/{projectId}/workspace/files/{filePath}?format=json` bodies, for the Documents preview. */
  workspaceFiles?: Record<string, Record<string, { content: string; size: number }>>;
}

/** A path-based Documents fixture: an existing text file at a known project/path. */
export const DOC_TEXT_FILE = {
  name: 'notes.txt',
  projectId: SPACE_ALPHA.projectId,
  projectName: SPACE_ALPHA.projectName,
  containerPath: '/workspace/notes.txt',
  filePath: 'notes.txt',
  content: 'hello from the seeded recent file',
  sentAt: '2026-09-28T12:00:00Z',
};

/**
 * A binary attachment Documents fixture, well under the inline text-preview
 * size limit — its MIME type, not its size, is what must classify it as
 * binary, so a small non-text file is never fetched and rendered as text.
 */
export const DOC_BINARY_ATTACHMENT = {
  id: 'att-binary-1',
  name: 'archive.bin',
  mime: 'application/octet-stream',
  size: 2048,
  sentAt: '2026-09-28T13:00:00Z',
};

/**
 * Endpoint-shaped request interception for the chat palette fixture: every
 * `/api/v1/**` request is intercepted (no live Hub), with the specific
 * shapes the palette slice and the real `scion-page-chat`/`scion-chat-thread`
 * it mounts actually read. Anything not named here gets an empty but
 * well-typed response so an unrelated fetch elsewhere in the real page never
 * throws — it is not a claim that the palette itself uses it.
 */
export async function setupApiMocks(
  page: Page,
  overrides: PaletteFixtureOverrides = {}
): Promise<TrackedRequest[]> {
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
      return route.fulfill({ json: { spaces: overrides.spaces ?? [] } });
    }
    if (path === '/api/v1/projects') {
      // Cold-load slug resolution (chat.ts's resolveProjectBySlug) for a
      // fixture navigated straight to /chat/{slug}/{topicId} before the
      // rail's own /api/v1/chat/spaces response has populated the slug map.
      const slug = url.searchParams.get('slug');
      const match = overrides.spaces?.find((s) => s.projectSlug === slug);
      return route.fulfill({
        json: {
          items: match
            ? [{ id: match.projectId, slug: match.projectSlug, name: match.projectName }]
            : [],
        },
      });
    }
    if (path === '/api/v1/users') {
      return route.fulfill({ json: { users: overrides.users ?? [] } });
    }
    const threadsMatch = path.match(/^\/api\/v1\/chat\/spaces\/([^/]+)\/threads$/);
    if (threadsMatch) {
      const projectId = decodeURIComponent(threadsMatch[1]);
      return route.fulfill({
        json: { threads: overrides.threadsByProjectId?.[projectId] ?? [] },
      });
    }
    const membersMatch = path.match(/^\/api\/v1\/chat\/spaces\/([^/]+)\/members$/);
    if (membersMatch) {
      const projectId = decodeURIComponent(membersMatch[1]);
      return route.fulfill({
        json: overrides.membersByProjectId?.[projectId] ?? { humans: [], agents: [] },
      });
    }
    const topicMatch = path.match(/^\/api\/v1\/chat\/topics\/([^/]+)$/);
    if (topicMatch) {
      // Cold-load/deep-link thread name+defaultAgent resolution
      // (chat.ts's fetchThreadDetails) — the route alone only carries the
      // topic ID.
      const topicId = decodeURIComponent(topicMatch[1]);
      const allThreads = Object.values(overrides.threadsByProjectId ?? {}).flat();
      const thread = allThreads.find((t) => t.id === topicId);
      return route.fulfill({
        json: thread ? { name: thread.name, defaultAgent: thread.defaultAgent ?? '' } : {},
      });
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
    const attachmentMatch = path.match(/^\/api\/v1\/chat\/attachments\/([^/]+)$/);
    if (attachmentMatch) {
      const id = decodeURIComponent(attachmentMatch[1]);
      const fixture = overrides.attachmentsById?.[id];
      if (!fixture) {
        return route.fulfill({
          status: 404,
          json: { error: { code: 'not_found', message: 'Attachment not found' } },
        });
      }
      return route.fulfill({ contentType: fixture.mime, body: fixture.body });
    }
    const workspaceFileMatch = path.match(/^\/api\/v1\/projects\/([^/]+)\/workspace\/files\/(.+)$/);
    if (workspaceFileMatch) {
      const projectId = decodeURIComponent(workspaceFileMatch[1]);
      const filePath = decodeURIComponent(workspaceFileMatch[2]);
      const fixture = overrides.workspaceFiles?.[projectId]?.[filePath];
      if (!fixture) {
        return route.fulfill({
          status: 404,
          json: { error: { code: 'not_found', message: 'File not found' } },
        });
      }
      return route.fulfill({ json: { content: fixture.content, size: fixture.size } });
    }
    // Unnamed endpoint: empty object keeps the real components' defensive
    // `data.foo ?? []`-style parsing harmless without asserting they call it.
    return route.fulfill({ json: {} });
  });

  return requests;
}
