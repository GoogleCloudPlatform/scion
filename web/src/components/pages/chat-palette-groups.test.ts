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
 * Tests for chat.ts's native chat quick command palette support: the
 * People/Threads/Documents group loaders, retry dispatch, the 30s per-group
 * cache with SSE invalidation and 500ms debounced refresh, the palette
 * selection paths, and `navigateToThread`'s project-routing/encoding
 * behavior.
 *
 * Elements are created but never appended (no connectedCallback), following
 * chat.test.ts's convention — these are plain-method/state tests, not
 * rendering or focus tests (those live in e2e/chat-palette, Chromium) — with
 * one exception: the "unknown identity — connected element" describe block
 * below needs the real Lit lifecycle (`updated()`'s route re-parse runs off
 * `changedProperties`, which a disconnected/unattached instance never
 * populates the same way), so it connects a page and warms the same lazy
 * imports `chat-palette-shortcut.test.ts` does, for the same reason.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../client/api.js';
import { navigateTo } from '../../client/main.js';
import type { PaletteCandidate, PaletteTarget } from '../../client/chat-palette-types.js';
import {
  AGENT_DMS_CACHE_MS,
  AGENTS_IDLE_TIMEOUT_MS,
  ChatPaletteDataController,
  PaletteLoadError,
} from '../../client/chat-palette-data.js';
import { agentStore } from '../../client/agent-store.js';
import {
  FakeEventSource,
  agent,
  createHarness,
} from '../../client/__fixtures__/agent-store-harness.js';
import { chatRecentFiles } from '../../client/chat-recent-files.js';
import type { RecentFile, RecentFilesSnapshot } from '../../client/chat-recent-files.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

const fakeState = vi.hoisted(() => {
  const t = new EventTarget() as any;
  t.agents = new Map<string, any>();
  t.getAgents = () => Array.from(t.agents.values());
  t.getDeletedAgentIds = () => new Set<string>();
  t.seedAgents = (list: any[]) => list.forEach((a: any) => t.agents.set(a.id, a));
  t.removeAgent = (id: string) => t.agents.delete(id);
  t.setScope = () => {};
  return t;
});

vi.mock('../../client/main.js', () => ({ navigateTo: vi.fn(), stateManager: fakeState }));
vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return { ...actual, apiFetch: vi.fn() };
});

let ScionPageChat: any;

beforeAll(async () => {
  // A connected page retains the agent store's hub entry, which opens the
  // store's feed; it never connects here.
  vi.stubGlobal('EventSource', FakeEventSource);
  const mod = await import('./chat.js');
  ScionPageChat = mod.ScionPageChat;
  // Connecting a page (`document.body.appendChild`, used only by the
  // "unknown identity — connected element" tests below) runs
  // `connectedCallback`'s unawaited `initV2()`, which starts lazily
  // importing chat-space-rail/chat-members in the background. Importing them
  // here, awaited, warms the module cache so that background import
  // resolves near-instantly instead of a first-time transform that could
  // still be in flight when this file's tests finish and its environment
  // tears down.
  await Promise.all([
    import('../shared/chat/chat-space-rail.js'),
    import('../shared/chat/chat-members.js'),
  ]);
});

beforeEach(() => {
  vi.mocked(apiFetch).mockReset();
  vi.mocked(navigateTo).mockReset();
  fakeState.agents.clear();
  // Several tests capture "the URL before" and assert it is unchanged after
  // a no-op selection. Without resetting the URL between tests, a stale
  // pathname left over from an *earlier* test's real `pushState` call could
  // coincidentally equal what a buggy guard would also produce, silently
  // masking the assertion.
  window.history.pushState({}, '', '/');
});

afterEach(() => {
  vi.useRealTimers();
});

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status });
}

/** A page instance with a signed-in user, never appended to the DOM. */
function createPage(): any {
  const el = document.createElement('scion-page-chat') as any;
  el.pageData = { user: { id: 'self-user' } };
  return el;
}

function threadTarget(overrides: Partial<PaletteTarget> = {}): PaletteTarget {
  return {
    kind: 'thread',
    projectId: 'p2',
    threadId: 'topic-x',
    threadName: 'General',
    ...overrides,
  } as PaletteTarget;
}

function threadCandidate(target: PaletteTarget): PaletteCandidate {
  return {
    id: JSON.stringify(['thread', (target as any).projectId, (target as any).threadId]),
    group: 'threads',
    label: (target as any).threadName ?? '',
    searchFields: [(target as any).threadName ?? ''],
    secondaryLabel: '',
    activityMs: 0,
    target,
  };
}

// ===========================================================================
// Project-routing and encoding, shared by navigateToThread and the legacy
// switcher.
// ===========================================================================

describe('navigateToThread: routing when a project has no known slug', () => {
  it("routes by the thread's own projectId, never another project's slug, when the target project has no known slug", () => {
    const el = createPage();
    // The map knows alpha -> p1, but the chosen thread belongs to p2.
    el._slugToProjectId.set('alpha', 'p1');
    el._projectIdToSlug.set('p1', 'alpha');

    el.navigateToThread({
      conversationKey: 'topic-x',
      projectId: 'p2',
      threadName: 'General',
    });

    expect(window.location.pathname).toBe('/chat/space/p2/thread/topic-x');
    expect(window.location.pathname).not.toContain('alpha');
    expect(el.v2Conversation).toMatchObject({ projectId: 'p2', conversationKey: 'topic-x' });
  });

  it('reload resolves the same project: the cached mapping is not poisoned with the wrong pair', () => {
    const el = createPage();
    el._slugToProjectId.set('alpha', 'p1');
    el._projectIdToSlug.set('p1', 'alpha');

    el.navigateToThread({ conversationKey: 'topic-x', projectId: 'p2', threadName: 'General' });

    expect(el._projectIdToSlug.get('p2')).toBeUndefined();
    expect(el._projectIdToSlug.get('p1')).toBe('alpha');
  });

  it('uses a known own slug when present, producing the canonical readable route', () => {
    const el = createPage();
    el._projectIdToSlug.set('p2', 'beta');

    el.navigateToThread({ conversationKey: 'topic-x', projectId: 'p2', threadName: 'General' });

    expect(window.location.pathname).toBe('/chat/beta/topic-x');
  });

  it("prefers the target's own carried projectSlug over a locally cached one", () => {
    const el = createPage();
    el._projectIdToSlug.set('p2', 'stale-slug');

    el.navigateToThread({
      conversationKey: 'topic-x',
      projectId: 'p2',
      projectSlug: 'fresh-slug',
      threadName: 'General',
    });

    expect(window.location.pathname).toBe('/chat/fresh-slug/topic-x');
  });

  it('encodes slug and conversation key segments (characters that change route meaning if left raw)', () => {
    const el = createPage();
    // `/`, `?` and `#` are not just "unusual" characters — left unencoded
    // they change what the resulting URL *means* (an extra path segment, a
    // query string, a fragment). A plain space does not discriminate this:
    // the URL parser silently percent-encodes a raw space to the same
    // `%20` an app-level `encodeURIComponent` call would have produced, so a
    // fixture using only spaces cannot tell an encoded route from an
    // unencoded one.
    el._projectIdToSlug.set('p2', 'a/b?c');

    el.navigateToThread({
      conversationKey: 'k#1',
      projectId: 'p2',
      threadName: 'General',
    });

    expect(window.location.pathname).toBe(
      `/chat/${encodeURIComponent('a/b?c')}/${encodeURIComponent('k#1')}`
    );
    expect(window.location.pathname).toBe('/chat/a%2Fb%3Fc/k%231');
  });

  it('encodes both the project ID and the conversation key in the fallback route', () => {
    const el = createPage();
    el.navigateToThread({
      conversationKey: 'k#1',
      projectId: 'p2/weird?x',
      threadName: 'General',
    });
    expect(window.location.pathname).toBe(
      `/chat/space/${encodeURIComponent('p2/weird?x')}/thread/${encodeURIComponent('k#1')}`
    );
    expect(window.location.pathname).toBe('/chat/space/p2%2Fweird%3Fx/thread/k%231');
  });

  it('handleThreadSelect (the rail event path) delegates to navigateToThread', () => {
    const el = createPage();
    el._projectIdToSlug.set('p1', 'alpha');
    el.handleThreadSelect(
      new CustomEvent('thread-select', {
        detail: { conversationKey: 'topic-x', projectId: 'p2', threadName: 'General' },
      })
    );
    expect(window.location.pathname).toBe('/chat/space/p2/thread/topic-x');
  });

  describe('non-root BASE_URL', () => {
    afterEach(() => {
      vi.unstubAllEnvs();
    });

    it('prefixes the fallback (missing-slug) route with a non-root BASE_URL exactly once', () => {
      vi.stubEnv('BASE_URL', '/scion/');
      const el = createPage();
      el.navigateToThread({ conversationKey: 'topic-x', projectId: 'p2', threadName: 'General' });
      expect(window.location.pathname).toBe('/scion/chat/space/p2/thread/topic-x');
      expect(window.location.pathname).not.toContain('/scion/scion/');
    });

    it('prefixes the canonical (known-slug) route with a non-root BASE_URL exactly once', () => {
      vi.stubEnv('BASE_URL', '/scion/');
      const el = createPage();
      el._projectIdToSlug.set('p2', 'beta');
      el.navigateToThread({ conversationKey: 'topic-x', projectId: 'p2', threadName: 'General' });
      expect(window.location.pathname).toBe('/scion/chat/beta/topic-x');
      expect(window.location.pathname).not.toContain('/scion/scion/');
    });
  });

  describe('parseV2Route: a real reload at the fallback route resolves the correct project', () => {
    it('/chat/space/{p2}/thread/{topic-x} resolves v2Conversation.projectId to p2 even though only alpha->p1 is cached', () => {
      const el = createPage();
      el._slugToProjectId.set('alpha', 'p1');
      el._projectIdToSlug.set('p1', 'alpha');
      window.history.pushState({}, '', '/chat/space/p2/thread/topic-x');

      el.parseV2Route();

      expect(el.v2Conversation).toMatchObject({ projectId: 'p2', conversationKey: 'topic-x' });
    });

    it("redirects to the canonical slug route once p2's slug is known", () => {
      const el = createPage();
      el._slugToProjectId.set('beta', 'p2');
      el._projectIdToSlug.set('p2', 'beta');
      window.history.pushState({}, '', '/chat/space/p2/thread/topic-x');

      el.parseV2Route();

      expect(navigateTo).toHaveBeenCalledWith('/chat/beta/topic-x');
    });
  });
});

// ===========================================================================
// Palette Threads selection (_handlePaletteSelect)
// ===========================================================================

describe('_handlePaletteSelect: thread targets', () => {
  it('navigates via navigateToThread when the thread candidate is still present', () => {
    const el = createPage();
    const target = threadTarget();
    el.v2PaletteGroups = { threads: { status: 'ready', candidates: [threadCandidate(target)] } };

    el._handlePaletteSelect(new CustomEvent('palette-select', { detail: { target } }));

    expect(window.location.pathname).toBe('/chat/space/p2/thread/topic-x');
    expect(el.v2Conversation).toMatchObject({ projectId: 'p2', conversationKey: 'topic-x' });
  });

  it('a stale thread candidate (no longer present after a refresh) does not navigate', () => {
    const el = createPage();
    const target = threadTarget({ threadId: 'gone' } as Partial<PaletteTarget>);
    el.v2PaletteGroups = { threads: { status: 'ready', candidates: [] } };
    const before = window.location.pathname;

    el._handlePaletteSelect(new CustomEvent('palette-select', { detail: { target } }));

    expect(window.location.pathname).toBe(before);
  });

  describe('stillPresent: each field of the match discriminates independently', () => {
    // The test above used `candidates: []`, which makes every clause of the
    // `stillPresent` check trivially false — it cannot tell "the projectId
    // check rejected this" from "the threadId check rejected this" from
    // "nothing was compared at all". These fixtures differ from the target
    // by exactly one field each, so only the specific guard under test can
    // reject them.

    it('a candidate with the same threadId but a different projectId does not count as present', () => {
      const el = createPage();
      const target = threadTarget({
        projectId: 'p2',
        threadId: 'topic-x',
      } as Partial<PaletteTarget>);
      const wrongProject = threadCandidate(
        threadTarget({
          projectId: 'different-project',
          threadId: 'topic-x',
        } as Partial<PaletteTarget>)
      );
      el.v2PaletteGroups = { threads: { status: 'ready', candidates: [wrongProject] } };
      const before = window.location.pathname;

      el._handlePaletteSelect(new CustomEvent('palette-select', { detail: { target } }));

      expect(window.location.pathname).toBe(before);
    });

    it('a candidate with the same projectId but a different threadId does not count as present', () => {
      const el = createPage();
      const target = threadTarget({
        projectId: 'p2',
        threadId: 'topic-x',
      } as Partial<PaletteTarget>);
      const wrongThread = threadCandidate(
        threadTarget({ projectId: 'p2', threadId: 'different-thread' } as Partial<PaletteTarget>)
      );
      el.v2PaletteGroups = { threads: { status: 'ready', candidates: [wrongThread] } };
      const before = window.location.pathname;

      el._handlePaletteSelect(new CustomEvent('palette-select', { detail: { target } }));

      expect(window.location.pathname).toBe(before);
    });

    it('a non-thread candidate that happens to share both IDs does not count as present', () => {
      const el = createPage();
      const target = threadTarget({
        projectId: 'p2',
        threadId: 'topic-x',
      } as Partial<PaletteTarget>);
      // A `dm` target's shape doesn't naturally carry projectId/threadId —
      // this fixture only exists to prove the `kind === 'thread'`
      // discriminant is actually checked, not to model real data.
      const lookalikeDm = {
        id: 'lookalike',
        group: 'threads',
        label: 'Lookalike',
        searchFields: ['Lookalike'],
        secondaryLabel: '',
        activityMs: 0,
        target: {
          kind: 'dm',
          peerKind: 'agent',
          peerId: 'x',
          displayName: 'Lookalike',
          projectId: 'p2',
          threadId: 'topic-x',
        },
      } as unknown as PaletteCandidate;
      el.v2PaletteGroups = { threads: { status: 'ready', candidates: [lookalikeDm] } };
      const before = window.location.pathname;

      el._handlePaletteSelect(new CustomEvent('palette-select', { detail: { target } }));

      expect(window.location.pathname).toBe(before);
    });
  });

  it('carries the target projectSlug through to the route when present', () => {
    const el = createPage();
    const target = threadTarget({ projectSlug: 'beta' } as Partial<PaletteTarget>);
    el.v2PaletteGroups = { threads: { status: 'ready', candidates: [threadCandidate(target)] } };

    el._handlePaletteSelect(new CustomEvent('palette-select', { detail: { target } }));

    expect(window.location.pathname).toBe('/chat/beta/topic-x');
  });
});

describe('_handlePaletteSelect: agent targets', () => {
  it('ignores an agent target, even one carrying the IDs of a present thread candidate', () => {
    const el = createPage();
    const thread = threadTarget();
    el.v2PaletteGroups = { threads: { status: 'ready', candidates: [threadCandidate(thread)] } };
    // Chat builds no `agent` candidates. This fixture carries the thread's IDs
    // only so that, if the agent branch were missing, the target would reach
    // the thread match and navigate.
    const target = { ...thread, kind: 'agent', agentId: 'a1', displayName: 'Agent One' };
    const before = window.location.pathname;

    el._handlePaletteSelect(new CustomEvent('palette-select', { detail: { target } }));

    expect(window.location.pathname).toBe(before);
    expect(el._paletteClosedBySelection).toBe(false);
  });
});

describe('_handlePaletteSelect: People (dm/user) targets', () => {
  // Covers the `'people'` half of the `group` lookup this method shares with
  // Agents (`target.peerKind === 'agent' ? 'agents' : 'people'`): a bug that
  // always looked up `'agents'` regardless of `peerKind` would otherwise
  // pass the whole suite.

  it("opens the person's DM when still present in the People group", () => {
    const el = createPage();
    const openDMSpy = vi.spyOn(el, 'openDM').mockImplementation(() => {});
    el.v2PaletteGroups = {
      people: {
        status: 'ready',
        candidates: [
          {
            id: '["dm","user","u1"]',
            group: 'people',
            label: 'Alice',
            searchFields: ['Alice'],
            secondaryLabel: '',
            activityMs: 0,
            target: { kind: 'dm', peerKind: 'user', peerId: 'u1', displayName: 'Alice' },
          },
        ],
      },
    };

    el._handlePaletteSelect(
      new CustomEvent('palette-select', {
        detail: { target: { kind: 'dm', peerKind: 'user', peerId: 'u1', displayName: 'Alice' } },
      })
    );

    expect(openDMSpy).toHaveBeenCalledWith('u1', 'user', 'Alice');
  });

  it('does not navigate when the person is no longer present in the People group (looked up correctly, not in Agents)', () => {
    const el = createPage();
    const openDMSpy = vi.spyOn(el, 'openDM').mockImplementation(() => {});
    // The peerId exists as an *agent* candidate — if the group lookup ever
    // used 'agents' regardless of peerKind, this would incorrectly count as
    // present.
    el.v2PaletteGroups = {
      agents: {
        status: 'ready',
        candidates: [
          {
            id: '["dm","agent","u1"]',
            group: 'agents',
            label: 'Agent U1',
            searchFields: ['Agent U1'],
            secondaryLabel: '',
            activityMs: 0,
            target: { kind: 'dm', peerKind: 'agent', peerId: 'u1', displayName: 'Agent U1' },
          },
        ],
      },
      // A different person is present in People — if the peerId comparison
      // were ever weakened to just "some dm candidate of this peerKind
      // exists," this other person would incorrectly count as a match for
      // 'u1'.
      people: {
        status: 'ready',
        candidates: [
          {
            id: '["dm","user","u2"]',
            group: 'people',
            label: 'Bob',
            searchFields: ['Bob'],
            secondaryLabel: '',
            activityMs: 0,
            target: { kind: 'dm', peerKind: 'user', peerId: 'u2', displayName: 'Bob' },
          },
        ],
      },
    };

    el._handlePaletteSelect(
      new CustomEvent('palette-select', {
        detail: { target: { kind: 'dm', peerKind: 'user', peerId: 'u1', displayName: 'Alice' } },
      })
    );

    expect(openDMSpy).not.toHaveBeenCalled();
  });
});

// ===========================================================================
// Palette Documents selection (_handlePaletteSelect) and after-hide preview
// ===========================================================================

function documentFile(overrides: Partial<RecentFile> = {}): RecentFile {
  return {
    key: JSON.stringify(['path', 'p1', 'workspace', '', 'notes.txt']),
    name: 'notes.txt',
    source: {
      conversationKey: 'topic-1',
      messageId: 'm1',
      sentAt: '2026-09-28T12:00:00Z',
      projectId: 'p1',
    },
    target: {
      kind: 'path',
      projectId: 'p1',
      containerPath: '/workspace/notes.txt',
      location: { kind: 'workspace', filePath: 'notes.txt' },
    },
    ...overrides,
  } as RecentFile;
}

function documentCandidate(file: RecentFile): PaletteCandidate {
  return {
    id: JSON.stringify(['document', file.key]),
    group: 'documents',
    label: file.name,
    searchFields: [file.name],
    secondaryLabel: '',
    activityMs: 0,
    target: { kind: 'document', file },
  } as PaletteCandidate;
}

function emptySnapshot(): RecentFilesSnapshot {
  return { records: [], persistent: true };
}

describe('Documents group: chatRecentFiles subscription lifecycle', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  it('a v2 page seeds the Documents group from the current snapshot on connect', () => {
    const file = documentFile();
    vi.spyOn(chatRecentFiles, 'snapshot').mockReturnValue({ records: [file], persistent: true });
    vi.spyOn(chatRecentFiles, 'subscribe').mockReturnValue(() => {});
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse({}));
    const el = createPage();

    document.body.appendChild(el);

    expect(el.v2PaletteGroups.documents?.status).toBe('ready');
    expect(el.v2PaletteGroups.documents?.candidates).toHaveLength(1);
    expect(el.v2PaletteGroups.documents?.candidates[0].label).toBe(file.name);
  });

  it('a v2 page subscribes to chatRecentFiles exactly once on connect', () => {
    const subscribeSpy = vi.spyOn(chatRecentFiles, 'subscribe').mockReturnValue(() => {});
    vi.spyOn(chatRecentFiles, 'snapshot').mockReturnValue(emptySnapshot());
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse({}));
    const el = createPage();

    document.body.appendChild(el);

    expect(subscribeSpy).toHaveBeenCalledTimes(1);
  });

  it('a snapshot pushed through the subscription callback later refreshes the Documents group', () => {
    let publish: ((s: RecentFilesSnapshot) => void) | null = null;
    vi.spyOn(chatRecentFiles, 'subscribe').mockImplementation((cb) => {
      publish = cb;
      return () => {};
    });
    vi.spyOn(chatRecentFiles, 'snapshot').mockReturnValue(emptySnapshot());
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse({}));
    const el = createPage();
    document.body.appendChild(el);
    expect(el.v2PaletteGroups.documents?.candidates).toHaveLength(0);

    const file = documentFile();
    publish!({ records: [file], persistent: true });

    expect(el.v2PaletteGroups.documents?.candidates).toHaveLength(1);
  });

  it('disconnecting calls the unsubscribe function chatRecentFiles.subscribe returned', () => {
    const unsubscribe = vi.fn();
    vi.spyOn(chatRecentFiles, 'subscribe').mockReturnValue(unsubscribe);
    vi.spyOn(chatRecentFiles, 'snapshot').mockReturnValue(emptySnapshot());
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse({}));
    const el = createPage();
    document.body.appendChild(el);
    expect(unsubscribe).not.toHaveBeenCalled();

    el.remove();

    expect(unsubscribe).toHaveBeenCalledTimes(1);
  });

  it('reconnecting after a disconnect subscribes again — once per connect, not accumulating', () => {
    const unsubscribe1 = vi.fn();
    const unsubscribe2 = vi.fn();
    const subscribeSpy = vi
      .spyOn(chatRecentFiles, 'subscribe')
      .mockReturnValueOnce(unsubscribe1)
      .mockReturnValueOnce(unsubscribe2);
    vi.spyOn(chatRecentFiles, 'snapshot').mockReturnValue(emptySnapshot());
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse({}));
    const el = createPage();

    document.body.appendChild(el);
    el.remove();
    document.body.appendChild(el);

    expect(subscribeSpy).toHaveBeenCalledTimes(2);
    expect(unsubscribe1).toHaveBeenCalledTimes(1);
    expect(unsubscribe2).not.toHaveBeenCalled();
  });

  it('disconnecting twice in a row without an intervening connect does not call the unsubscribe function a second time', () => {
    const unsubscribe = vi.fn();
    vi.spyOn(chatRecentFiles, 'subscribe').mockReturnValue(unsubscribe);
    vi.spyOn(chatRecentFiles, 'snapshot').mockReturnValue(emptySnapshot());
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse({}));
    const el = createPage();
    document.body.appendChild(el);

    el.remove();
    expect(unsubscribe).toHaveBeenCalledTimes(1);

    // A second disconnect with no intervening reconnect must not re-invoke
    // the same already-called unsubscribe function; only clearing the
    // stored reference to null after calling it once guards this.
    el.disconnectedCallback();

    expect(unsubscribe).toHaveBeenCalledTimes(1);
  });
});

describe('_handlePaletteSelect: Document targets', () => {
  it('closes the palette and records a pending preview target when the document candidate is still present', () => {
    const el = createPage();
    const file = documentFile();
    el.v2PaletteGroups = { documents: { status: 'ready', candidates: [documentCandidate(file)] } };
    el.v2PaletteOpen = true;

    el._handlePaletteSelect(
      new CustomEvent('palette-select', { detail: { target: { kind: 'document', file } } })
    );

    expect(el.v2PaletteOpen).toBe(false);
    expect(el._pendingDocumentPreviewTarget).toEqual({
      kind: 'path',
      projectId: 'p1',
      containerPath: '/workspace/notes.txt',
      location: { kind: 'workspace', filePath: 'notes.txt' },
      name: 'notes.txt',
    });
    // The preview must not open until the palette's own close animation
    // actually finishes (see _handlePaletteAfterHide) — setting it here,
    // before sl-after-hide, would risk both dialogs fighting for focus at
    // the same time.
    expect(el._paletteFilePreviewTarget).toBeNull();
  });

  it('converts an attachment target to the preview shape, adding the display name', () => {
    const el = createPage();
    const file = documentFile({
      key: JSON.stringify(['attachment', 'att-1']),
      name: 'photo.png',
      target: { kind: 'attachment', id: 'att-1', mime: 'image/png', size: 10 },
    });
    el.v2PaletteGroups = { documents: { status: 'ready', candidates: [documentCandidate(file)] } };

    el._handlePaletteSelect(
      new CustomEvent('palette-select', { detail: { target: { kind: 'document', file } } })
    );

    expect(el._pendingDocumentPreviewTarget).toEqual({
      kind: 'attachment',
      id: 'att-1',
      name: 'photo.png',
      mime: 'image/png',
      size: 10,
    });
  });

  it('a stale document candidate (no longer present after a refresh) does not set a pending preview', () => {
    const el = createPage();
    const file = documentFile();
    el.v2PaletteGroups = { documents: { status: 'ready', candidates: [] } };

    el._handlePaletteSelect(
      new CustomEvent('palette-select', { detail: { target: { kind: 'document', file } } })
    );

    expect(el._pendingDocumentPreviewTarget).toBeNull();
  });

  it('a candidate with a different file key does not count as present', () => {
    const el = createPage();
    const file = documentFile();
    const other = documentFile({ key: 'different-key', name: 'other.txt' });
    el.v2PaletteGroups = { documents: { status: 'ready', candidates: [documentCandidate(other)] } };

    el._handlePaletteSelect(
      new CustomEvent('palette-select', { detail: { target: { kind: 'document', file } } })
    );

    expect(el._pendingDocumentPreviewTarget).toBeNull();
  });

  it('a same-keyed candidate of a different kind does not count as present — the kind check, not just the key match, guards this', () => {
    // Only `document`-kind candidates are ever published into
    // v2PaletteGroups.documents today, so this array shape can't arise
    // through the palette's own real loaders — but nothing in the type
    // system stops a future group-population bug from putting a
    // differently-shaped candidate here, and `c.target.file` would be
    // `undefined` on one, not a `document` target's `file` object. Forcing
    // the mismatched shape directly (bypassing the type checker, the same
    // way the codebase already does for other "can't happen through the
    // real API" guard tests) proves the `kind` half of the guard is load
    // -bearing on its own, not redundant with the key comparison.
    const el = createPage();
    const file = documentFile();
    const impostor = {
      id: JSON.stringify(['dm', 'agent', 'x']),
      group: 'documents',
      label: file.name,
      searchFields: [file.name],
      secondaryLabel: '',
      activityMs: 0,
      target: { kind: 'dm', peerKind: 'agent', peerId: 'x', displayName: file.name },
    } as unknown as PaletteCandidate;
    el.v2PaletteGroups = { documents: { status: 'ready', candidates: [impostor] } };

    el._handlePaletteSelect(
      new CustomEvent('palette-select', { detail: { target: { kind: 'document', file } } })
    );

    expect(el._pendingDocumentPreviewTarget).toBeNull();
  });

  it('a same-keyed candidate of a different kind does not count as present, even when it exposes a matching file.key at the same shape a document target would', () => {
    // This impostor has a `file.key` equal to the document's, so only the
    // `kind` check (not a key mismatch or a thrown error) can reject it.
    const el = createPage();
    const file = documentFile();
    const impostor = {
      id: JSON.stringify(['dm', 'agent', 'x']),
      group: 'documents',
      label: file.name,
      searchFields: [file.name],
      secondaryLabel: '',
      activityMs: 0,
      target: {
        kind: 'dm',
        peerKind: 'agent',
        peerId: 'x',
        displayName: file.name,
        file: { key: file.key },
      },
    } as unknown as PaletteCandidate;
    el.v2PaletteGroups = { documents: { status: 'ready', candidates: [impostor] } };

    el._handlePaletteSelect(
      new CustomEvent('palette-select', { detail: { target: { kind: 'document', file } } })
    );

    expect(el._pendingDocumentPreviewTarget).toBeNull();
  });

  it('_handlePaletteAfterHide opens the preview from a pending document target and leaves the invoker captured for later', () => {
    const el = createPage();
    vi.spyOn(el, '_isOnChatRoute').mockReturnValue(true);
    vi.spyOn(el, '_isPageVisible').mockReturnValue(true);
    vi.spyOn(el, '_isUnrelatedModalActive').mockReturnValue(false);
    el._pendingDocumentPreviewTarget = {
      kind: 'path',
      projectId: 'p1',
      containerPath: '/workspace/notes.txt',
      location: { kind: 'workspace', filePath: 'notes.txt' },
      name: 'notes.txt',
    };
    const invoker = document.createElement('textarea');
    el._paletteInvoker = invoker;
    const restoreSpy = vi.spyOn(el, '_restorePaletteInvokerFocus');
    const focusComposerSpy = vi.spyOn(el, '_focusComposerAfterPaletteSelection');

    const dialog = document.createElement('div');
    dialog.classList.add('palette-dialog');
    el._handlePaletteAfterHide({ composedPath: () => [dialog] } as unknown as Event);

    expect(el._paletteFilePreviewTarget).toEqual({
      kind: 'path',
      projectId: 'p1',
      containerPath: '/workspace/notes.txt',
      location: { kind: 'workspace', filePath: 'notes.txt' },
      name: 'notes.txt',
    });
    expect(el._pendingDocumentPreviewTarget).toBeNull();
    // The preview dialog becomes the sole modal in control of focus — the
    // palette's own after-hide must not also restore the invoker or focus a
    // composer.
    expect(restoreSpy).not.toHaveBeenCalled();
    expect(focusComposerSpy).not.toHaveBeenCalled();
    expect(el._paletteInvoker).toBe(invoker);
  });

  it('_closePaletteFilePreview clears the target and restores the captured invoker', () => {
    const el = createPage();
    const invoker = document.createElement('textarea');
    document.body.appendChild(invoker);
    el._paletteInvoker = invoker;
    el._paletteFilePreviewTarget = {
      kind: 'attachment',
      id: 'att-1',
      name: 'photo.png',
      mime: 'image/png',
      size: 10,
    };

    el._closePaletteFilePreview();

    expect(el._paletteFilePreviewTarget).toBeNull();
    expect(document.activeElement).toBe(invoker);
    document.body.removeChild(invoker);
  });
});

// ===========================================================================
// People/Threads group loaders
// ===========================================================================

describe('_loadPalettePeople', () => {
  it('excludes self and resolves to ready candidates', async () => {
    const el = createPage();
    el.v2PaletteOpen = true;
    vi.mocked(apiFetch).mockImplementation((url: string) => {
      if (url.startsWith('/api/v1/users')) {
        return Promise.resolve(
          jsonResponse({
            users: [
              { id: 'self-user', displayName: 'Me' },
              { id: 'u1', displayName: 'Alice' },
            ],
          })
        );
      }
      return Promise.resolve(jsonResponse({ dms: [] }));
    });

    await el._loadPalettePeople();

    expect(el.v2PaletteGroups.people.status).toBe('ready');
    expect(el.v2PaletteGroups.people.candidates.map((c: PaletteCandidate) => c.label)).toEqual([
      'Alice',
    ]);
  });

  it('a failure sets status=error with the message', async () => {
    const el = createPage();
    el.v2PaletteOpen = true;
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse({}, 500));

    await el._loadPalettePeople();

    expect(el.v2PaletteGroups.people.status).toBe('error');
  });

  describe('unknown identity', () => {
    it('resolves self via /api/v1/auth/me when pageData.user.id is unknown, then excludes it correctly', async () => {
      const el = createPage();
      el.pageData = {}; // no user at all
      el.v2PaletteOpen = true;
      vi.mocked(apiFetch).mockImplementation((url: string) => {
        if (url === '/api/v1/auth/me') {
          return Promise.resolve(jsonResponse({ id: 'resolved-self' }));
        }
        if (url.startsWith('/api/v1/users')) {
          return Promise.resolve(
            jsonResponse({
              users: [
                { id: 'resolved-self', displayName: 'Me' },
                { id: 'u1', displayName: 'Alice' },
              ],
            })
          );
        }
        return Promise.resolve(jsonResponse({ dms: [] }));
      });

      await el._loadPalettePeople();

      expect(el.v2PaletteGroups.people.status).toBe('ready');
      expect(el.v2PaletteGroups.people.candidates.map((c: PaletteCandidate) => c.label)).toEqual([
        'Alice',
      ]);
      // The resolved ID is cached onto pageData for later callers (openDM's
      // own buildDMKey, etc.) — not just used transiently for this one load.
      expect(el.pageData.user.id).toBe('resolved-self');
    });

    it('preserves the rest of pageData.user when filling in a previously-unknown id', async () => {
      const el = createPage();
      el.pageData = { user: { email: 'ada@example.com', name: 'Ada' } };
      vi.mocked(apiFetch).mockImplementation((url: string) =>
        url === '/api/v1/auth/me'
          ? Promise.resolve(jsonResponse({ id: 'resolved-self' }))
          : Promise.resolve(jsonResponse({ users: [], dms: [] }))
      );

      await el._resolveSelfUserId();

      expect(el.pageData.user).toEqual({
        email: 'ada@example.com',
        name: 'Ada',
        id: 'resolved-self',
      });
    });

    it('ignores an id-shaped field in a non-ok /auth/me response body (the ok check, not just parsing, gates whether the body is trusted)', async () => {
      const el = createPage();
      el.pageData = {};
      vi.mocked(apiFetch).mockImplementation((url: string) => {
        // A real error response is unlikely to coincidentally carry a
        // top-level `id`, but this proves `_resolveSelfUserId` checks
        // `authRes.ok` before trusting the body, not just that it happens to
        // parse — a non-ok response's `id`-shaped body must never be used.
        if (url === '/api/v1/auth/me') {
          return Promise.resolve(jsonResponse({ id: 'should-not-be-used' }, 500));
        }
        return Promise.resolve(jsonResponse({ users: [], dms: [] }));
      });

      const selfId = await el._resolveSelfUserId();

      expect(selfId).toBe('');
      expect(el.pageData.user).toBeUndefined();
    });

    it('never publishes or caches a self-inclusive ready list when identity cannot be resolved at all', async () => {
      const el = createPage();
      el.pageData = {};
      el.v2PaletteOpen = true;
      vi.mocked(apiFetch).mockImplementation((url: string) => {
        if (url === '/api/v1/auth/me') return Promise.resolve(jsonResponse({}, 401));
        // If identity resolution were skipped, this is the exact shape that
        // would produce a self-inclusive "ready" list.
        return Promise.resolve(
          jsonResponse({
            users: [
              { id: '', displayName: 'Me' },
              { id: 'u1', displayName: 'Alice' },
            ],
          })
        );
      });

      await el._loadPalettePeople();

      expect(el.v2PaletteGroups.people.status).toBe('error');
      expect(el.v2PaletteGroups.people.candidates).toEqual([]);
      expect(el._paletteGroupCacheAt.people).toBeUndefined();
      expect(el._shouldUseCachedPaletteGroup('people')).toBe(false);
    });

    it('closing the palette while /auth/me is still pending publishes nothing once it fails', async () => {
      const el = createPage();
      el.pageData = {};
      el.v2PaletteOpen = true;
      let resolveAuthMe!: (v: Response) => void;
      const fetchCalls: string[] = [];
      vi.mocked(apiFetch).mockImplementation((url: string) => {
        fetchCalls.push(url);
        if (url === '/api/v1/auth/me') {
          return new Promise<Response>((resolve) => (resolveAuthMe = resolve));
        }
        return Promise.resolve(jsonResponse({ users: [], dms: [] }));
      });

      const load = el._loadPalettePeople();
      await vi.waitFor(() => expect(fetchCalls).toContain('/api/v1/auth/me'));
      // The transient loading state was published before the closed-mid-flight guard applies.
      expect(el.v2PaletteGroups.people.status).toBe('loading');

      el._closePaletteAndCancelLoad();
      resolveAuthMe(jsonResponse({}, 401));
      await load;

      // Never published the error state the still-open case would have —
      // the identity fetch's own failure is silently dropped once closed.
      expect(el.v2PaletteGroups.people.status).toBe('loading');
      expect(el._paletteGroupCacheAt.people).toBeUndefined();
      // No further fetches beyond the one already in flight when closed.
      expect(fetchCalls).toEqual(['/api/v1/auth/me']);
    });

    it('closing the palette while /auth/me is still pending starts no users/DMs fetch once it succeeds', async () => {
      const el = createPage();
      el.pageData = {};
      el.v2PaletteOpen = true;
      let resolveAuthMe!: (v: Response) => void;
      const fetchCalls: string[] = [];
      vi.mocked(apiFetch).mockImplementation((url: string) => {
        fetchCalls.push(url);
        if (url === '/api/v1/auth/me') {
          return new Promise<Response>((resolve) => (resolveAuthMe = resolve));
        }
        return Promise.resolve(jsonResponse({ users: [], dms: [] }));
      });

      const load = el._loadPalettePeople();
      await vi.waitFor(() => expect(fetchCalls).toContain('/api/v1/auth/me'));

      el._closePaletteAndCancelLoad();
      resolveAuthMe(jsonResponse({ id: 'resolved-self' }));
      await load;

      // The resolved ID is still cached onto pageData — that is a
      // site-wide identity cache, not palette-specific state — but the
      // People load itself never continues past the closed-mid-flight guard:
      // no users/DMs fetch, and never published as ready while closed.
      expect(el.pageData.user.id).toBe('resolved-self');
      expect(fetchCalls).toEqual(['/api/v1/auth/me']);
      expect(el.v2PaletteGroups.people.status).toBe('loading');
    });

    it('a People load that resolves identity itself does not mark People dirty or schedule a second load', async () => {
      const el = createPage();
      el.pageData = {};
      el.v2PaletteOpen = true;
      vi.useFakeTimers();
      vi.mocked(apiFetch).mockImplementation((url: string) => {
        if (url === '/api/v1/auth/me')
          return Promise.resolve(jsonResponse({ id: 'resolved-self' }));
        if (url.startsWith('/api/v1/users')) {
          return Promise.resolve(jsonResponse({ users: [{ id: 'u1', displayName: 'Alice' }] }));
        }
        return Promise.resolve(jsonResponse({ dms: [] }));
      });
      const peopleSpy = vi.spyOn(el, '_loadPalettePeople');

      await el._loadPalettePeople();

      expect(el.v2PaletteGroups.people.status).toBe('ready');
      // Resolving identity from inside its own load must not mark the group
      // dirty — that would bump the epoch out from under this same load's
      // own _finishPaletteGroupLoad and schedule a redundant reload.
      expect(el._paletteGroupDirty.people).toBe(false);

      vi.advanceTimersByTime(1000);
      expect(peopleSpy).toHaveBeenCalledTimes(1);
    });

    it('a stale load whose identity resolution is still pending across a close and reopen never overwrites a newer ready state', async () => {
      // The scenario this guards against: People is open without a known
      // identity; load 1 starts and awaits /auth/me. The palette closes
      // (v2PaletteOpen -> false) and reopens (v2PaletteOpen -> true again)
      // before load 1's /auth/me settles. Load 2 starts fresh, resolves
      // identity immediately, and publishes ready. Only then does load 1's
      // stale /auth/me finally settle (as a failure). A guard keyed on
      // `v2PaletteOpen` alone cannot tell load 1 apart from load 2, since
      // both see the palette open — it must be keyed on which load is
      // current.
      const el = createPage();
      el.pageData = {};
      el.v2PaletteOpen = true;
      let resolveFirstAuthMe!: (v: Response) => void;
      let authMeCalls = 0;
      vi.mocked(apiFetch).mockImplementation((url: string) => {
        if (url === '/api/v1/auth/me') {
          authMeCalls++;
          if (authMeCalls === 1) {
            return new Promise<Response>((resolve) => (resolveFirstAuthMe = resolve));
          }
          return Promise.resolve(jsonResponse({ id: 'resolved-self' }));
        }
        return Promise.resolve(
          jsonResponse({ users: [{ id: 'u1', displayName: 'Alice' }], dms: [] })
        );
      });

      const load1 = el._loadPalettePeople();
      await vi.waitFor(() => expect(authMeCalls).toBe(1));

      // Close, then reopen, before load 1's /auth/me has settled.
      el._closePaletteAndCancelLoad();
      el.v2PaletteOpen = true;

      const load2 = el._loadPalettePeople();
      await load2;
      expect(el.v2PaletteGroups.people.status).toBe('ready');

      // Now let load 1's stale /auth/me finally fail.
      resolveFirstAuthMe(jsonResponse({}, 401));
      await load1;

      // Load 1 must not have overwritten load 2's fresh ready state.
      expect(el.v2PaletteGroups.people.status).toBe('ready');
    });

    it('a stale load whose identity resolution is still pending across a second load — with no close at all — never overwrites a newer ready state', async () => {
      // The same guarantee as the close/reopen test above, but reached
      // without ever closing the palette: a refresh or a retry click can
      // start a second People load while the first one's identity
      // resolution is still pending. Superseding load 1 the moment load 2
      // *starts* (not only when the palette closes) is what makes this
      // work — the guard token must be bumped at the top of every
      // `_loadPalettePeople` call, not only on close.
      const el = createPage();
      el.pageData = {};
      el.v2PaletteOpen = true;
      let resolveFirstAuthMe!: (v: Response) => void;
      let authMeCalls = 0;
      vi.mocked(apiFetch).mockImplementation((url: string) => {
        if (url === '/api/v1/auth/me') {
          authMeCalls++;
          if (authMeCalls === 1) {
            return new Promise<Response>((resolve) => (resolveFirstAuthMe = resolve));
          }
          return Promise.resolve(jsonResponse({ id: 'resolved-self' }));
        }
        return Promise.resolve(
          jsonResponse({ users: [{ id: 'u1', displayName: 'Alice' }], dms: [] })
        );
      });

      const load1 = el._loadPalettePeople();
      await vi.waitFor(() => expect(authMeCalls).toBe(1));

      // A second load starts — e.g. a debounced refresh or a Retry click —
      // while the palette stays open the whole time and load 1's /auth/me
      // is still pending.
      await el._loadPalettePeople();
      expect(el.v2PaletteGroups.people.status).toBe('ready');

      // Now let load 1's stale /auth/me finally fail.
      resolveFirstAuthMe(jsonResponse({}, 401));
      await load1;

      // Load 1 must not have overwritten load 2's fresh ready state.
      expect(el.v2PaletteGroups.people.status).toBe('ready');
    });

    it('a hung /auth/me is aborted after the shared idle bound, so the group settles instead of holding its token forever', async () => {
      const el = createPage();
      el.pageData = {};
      el.v2PaletteOpen = true;
      vi.useFakeTimers();

      vi.mocked(apiFetch).mockImplementation((url: string, options?: { signal?: AbortSignal }) => {
        if (url === '/api/v1/auth/me') {
          return new Promise((_resolve, reject) => {
            options?.signal?.addEventListener('abort', () =>
              reject(new DOMException('aborted', 'AbortError'))
            );
          });
        }
        return Promise.resolve(jsonResponse({ users: [], dms: [] }));
      });

      const load = el._loadPalettePeople();
      const tokenWhilePending = el._paletteGroupLoadToken.people;
      expect(tokenWhilePending).toBeDefined();

      await vi.advanceTimersByTimeAsync(AGENTS_IDLE_TIMEOUT_MS + 1);
      await load;

      // The hung fetch's own abort is treated like any other identity
      // failure: the group settles to a retryable error, and — critically —
      // the token is actually cleared, not held forever.
      expect(el.v2PaletteGroups.people.status).toBe('error');
      expect(el._paletteGroupLoadToken.people).toBeUndefined();
    });

    it('clears its identity timeout once /auth/me resolves, leaving no pending timers behind', async () => {
      const el = createPage();
      el.pageData = {};
      vi.useFakeTimers();

      vi.mocked(apiFetch).mockResolvedValue(jsonResponse({ id: 'resolved-self' }));

      const selfId = await el._resolveSelfUserId();

      expect(selfId).toBe('resolved-self');
      expect(vi.getTimerCount()).toBe(0);
    });

    it('does not abort /auth/me 1ms before the shared idle bound, but does 1ms after', async () => {
      const el = createPage();
      el.pageData = {};
      vi.useFakeTimers();
      let aborted = false;

      vi.mocked(apiFetch).mockImplementation((url: string, options?: { signal?: AbortSignal }) => {
        if (url === '/api/v1/auth/me') {
          return new Promise((_resolve, reject) => {
            options?.signal?.addEventListener('abort', () => {
              aborted = true;
              reject(new DOMException('aborted', 'AbortError'));
            });
          });
        }
        return Promise.resolve(jsonResponse({}));
      });

      const resolved = el._resolveSelfUserId();

      await vi.advanceTimersByTimeAsync(AGENTS_IDLE_TIMEOUT_MS - 1);
      expect(aborted).toBe(false);

      await vi.advanceTimersByTimeAsync(2);
      expect(aborted).toBe(true);
      expect(await resolved).toBe('');
    });

    it('aborts a pending /auth/me immediately on close, instead of leaving it running for the idle timeout', async () => {
      const el = createPage();
      el.pageData = {};
      el.v2PaletteOpen = true;
      let aborted = false;
      const fetchCalls: string[] = [];
      vi.mocked(apiFetch).mockImplementation((url: string, options?: { signal?: AbortSignal }) => {
        fetchCalls.push(url);
        if (url === '/api/v1/auth/me') {
          return new Promise<Response>((_resolve, reject) => {
            options?.signal?.addEventListener('abort', () => {
              aborted = true;
              reject(new DOMException('aborted', 'AbortError'));
            });
          });
        }
        return Promise.resolve(jsonResponse({ users: [], dms: [] }));
      });

      const load = el._loadPalettePeople();
      await vi.waitFor(() => expect(fetchCalls).toContain('/api/v1/auth/me'));
      expect(aborted).toBe(false);

      el._closePaletteAndCancelLoad();

      expect(aborted).toBe(true);
      await load;
    });

    it('aborts a pending /auth/me immediately when a newer People load starts, instead of leaving it running for the idle timeout', async () => {
      const el = createPage();
      el.pageData = {};
      el.v2PaletteOpen = true;
      let firstAborted = false;
      let authMeCalls = 0;
      vi.mocked(apiFetch).mockImplementation((url: string, options?: { signal?: AbortSignal }) => {
        if (url === '/api/v1/auth/me') {
          authMeCalls++;
          if (authMeCalls === 1) {
            return new Promise<Response>((_resolve, reject) => {
              options?.signal?.addEventListener('abort', () => {
                firstAborted = true;
                reject(new DOMException('aborted', 'AbortError'));
              });
            });
          }
          return Promise.resolve(jsonResponse({ id: 'resolved-self' }));
        }
        return Promise.resolve(
          jsonResponse({ users: [{ id: 'u1', displayName: 'Alice' }], dms: [] })
        );
      });

      const load1 = el._loadPalettePeople();
      await vi.waitFor(() => expect(authMeCalls).toBe(1));
      expect(firstAborted).toBe(false);

      const load2 = el._loadPalettePeople();
      // The abort fires synchronously, inside the new call's own
      // synchronous prologue (before its first await) — no await needed
      // between starting load2 and observing load1's controller abort.
      expect(firstAborted).toBe(true);

      await Promise.all([load1, load2]);
      expect(el.v2PaletteGroups.people.status).toBe('ready');
    });

    it("a superseded load's own finally does not null out a newer load's live controller (owner guard)", async () => {
      // Two hanging /auth/me loads, each resolving only via its own abort
      // signal. load2's predecessor-abort step fires load1's abort
      // synchronously, but load1's own `await`-continuation (where its
      // `finally` actually runs) is deferred to a microtask — by the time it
      // runs, `_selfUserAbortController` already holds load2's controller,
      // not load1's own. Without the `=== timeoutController` owner guard in
      // that `finally`, load1 would null the *shared* field out from under
      // load2, and the close below would find nothing to abort: load2 would
      // keep running until the idle timeout instead of being stopped here.
      const el = createPage();
      el.pageData = {};
      el.v2PaletteOpen = true;
      const abortedOrder: number[] = [];
      let authMeCalls = 0;
      vi.mocked(apiFetch).mockImplementation((url: string, options?: { signal?: AbortSignal }) => {
        if (url === '/api/v1/auth/me') {
          authMeCalls++;
          const n = authMeCalls;
          return new Promise<Response>((_resolve, reject) => {
            options?.signal?.addEventListener('abort', () => {
              abortedOrder.push(n);
              reject(new DOMException('aborted', 'AbortError'));
            });
          });
        }
        return Promise.resolve(jsonResponse({ users: [], dms: [] }));
      });

      const load1 = el._loadPalettePeople();
      await vi.waitFor(() => expect(authMeCalls).toBe(1));

      // Starting load2 synchronously aborts load1's controller (call #1) —
      // awaiting load1 here lets its (now-rejected) `_resolveSelfUserId` run
      // to completion, including its `finally`, before the close below.
      const load2 = el._loadPalettePeople();
      await load1;
      expect(abortedOrder).toEqual([1]);

      el._closePaletteAndCancelLoad();

      // Close must still reach load2's controller (call #2) — it would not
      // if load1's finally had already nulled the shared field.
      expect(abortedOrder).toEqual([1, 2]);
      await load2;
      // The abort resolves `_resolveSelfUserId` to '', but `_peopleLoadSeq`
      // (bumped by the close above) supersedes load2 before it can publish
      // that as an error.
      expect(el.v2PaletteGroups.people.status).not.toBe('error');

      // Deleting the guarded clear entirely (rather than making it
      // unconditional) is invisible to every assertion above: the supersede
      // path above only ever exercises the guard's *false* (skip) branch,
      // which "always skip" (no clear at all) satisfies by accident just as
      // well as "skip only when superseded" does. Only a later, uncontested
      // call — nothing pending, nothing closing — exercises the guard's
      // *true* (actually clear) branch, which a deleted clear fails: the
      // field would be left holding this call's own, by-then-dead
      // controller instead of null.
      authMeCalls = 0;
      vi.mocked(apiFetch).mockImplementation((url: string) =>
        url === '/api/v1/auth/me'
          ? Promise.resolve(jsonResponse({ id: 'resolved-self' }))
          : Promise.resolve(jsonResponse({ users: [], dms: [] }))
      );
      await el._resolveSelfUserId();
      expect(el._selfUserAbortController).toBeNull();
    });

    it('aborts a pending /auth/me even when a newer call resolves synchronously from a now-known id', async () => {
      // The predecessor-abort step in _resolveSelfUserId runs before the
      // `known` early return, specifically so this case (identity becomes
      // known elsewhere while a fetch is still in flight) still stops the
      // old request. Moving the abort below that check would make a
      // known-id call skip it entirely.
      const el = createPage();
      el.pageData = {};
      el.v2PaletteOpen = true;
      const abortedOrder: number[] = [];
      let authMeCalls = 0;
      vi.mocked(apiFetch).mockImplementation((url: string, options?: { signal?: AbortSignal }) => {
        if (url === '/api/v1/auth/me') {
          authMeCalls++;
          const n = authMeCalls;
          return new Promise<Response>((_resolve, reject) => {
            options?.signal?.addEventListener('abort', () => {
              abortedOrder.push(n);
              reject(new DOMException('aborted', 'AbortError'));
            });
          });
        }
        return Promise.resolve(jsonResponse({ users: [], dms: [] }));
      });

      const load1 = el._loadPalettePeople();
      await vi.waitFor(() => expect(authMeCalls).toBe(1));

      // Identity becomes known from elsewhere (e.g. resolveDMByPeerId) while
      // load1's /auth/me is still pending.
      el.pageData.user = { id: 'me', email: '', name: '' };
      const load2 = el._loadPalettePeople();
      // Synchronous: load2's predecessor-abort runs before its own `known`
      // check even returns, in the same synchronous prologue — no await
      // needed between starting load2 and observing load1's controller abort.
      expect(abortedOrder).toEqual([1]);

      await Promise.all([load1, load2]);

      expect(el._selfUserAbortController).toBeNull();
      expect(el.v2PaletteGroups.people.status).toBe('ready');
      // Only load1 ever reached /auth/me — load2 resolved synchronously from
      // the now-known pageData.user.id instead of fetching again.
      expect(authMeCalls).toBe(1);
    });

    it('aborts a pending /auth/me on disconnect, and the resulting abort is never published as an identity error', async () => {
      const el = createPage();
      el.pageData = {};
      el.v2PaletteOpen = true;
      document.body.appendChild(el);
      let aborted = false;
      let authMeCalled = false;
      vi.mocked(apiFetch).mockImplementation((url: string, options?: { signal?: AbortSignal }) => {
        if (url === '/api/v1/auth/me') {
          authMeCalled = true;
          return new Promise<Response>((_resolve, reject) => {
            options?.signal?.addEventListener('abort', () => {
              aborted = true;
              reject(new DOMException('aborted', 'AbortError'));
            });
          });
        }
        return Promise.resolve(jsonResponse({ users: [], dms: [] }));
      });

      const load = el._loadPalettePeople();
      await vi.waitFor(() => expect(authMeCalled).toBe(true));
      expect(aborted).toBe(false);
      el.remove();

      expect(aborted).toBe(true);
      await load;
      // The seq bump in disconnectedCallback is what keeps this from
      // publishing as "Could not resolve your identity yet." on an element
      // nothing can see anymore.
      expect(el.v2PaletteGroups.people.status).toBe('loading');
    });
  });

  describe('unknown identity — connected element: no route side effects from resolving it', () => {
    it.each([
      ['pageData.user already exists, with its id unknown', { user: {} }],
      ['pageData has no user at all', {}],
    ])(
      'does not re-parse the route, replace v2Conversation, or refetch members/topic detail — but does refresh a currentUserId binding (%s)',
      async (_label, pageData) => {
        window.history.pushState({}, '', '/chat/alpha/topic-1');
        const el = createPage();
        el.pageData = pageData;
        el._slugToProjectId.set('alpha', 'p1');

        const fetchedUrls: string[] = [];
        vi.mocked(apiFetch).mockImplementation((url: string) => {
          fetchedUrls.push(url);
          if (url === '/api/v1/auth/me') {
            return Promise.resolve(jsonResponse({ id: 'resolved-self' }));
          }
          return Promise.resolve(jsonResponse({}));
        });

        document.body.appendChild(el);
        await el.updateComplete;
        // Let connectedCallback's own fire-and-forget initV2 (lazy rail/members
        // import, its own initial route parse, its own member/rail loads)
        // fully settle before this test's own baseline.
        await vi.waitFor(() => expect(el.v2SpaceRailLoaded).toBe(true));
        await new Promise((r) => setTimeout(r, 20));
        await el.updateComplete;

        // An empty threadName/defaultAgent is what a fresh route parse would
        // also produce on first reaching this thread (no known metadata yet),
        // so a wrongly re-triggered parse is detectable by reference identity
        // and by re-issuing the members/topic-detail fetches, not only by a
        // value that would happen to differ.
        el.v2Conversation = { ...el.v2Conversation, threadName: '', defaultAgent: '' };
        await el.updateComplete;
        fetchedUrls.length = 0;
        const conversationBefore = el.v2Conversation;

        await el._resolveSelfUserId();
        await el.updateComplete;

        expect(el.v2Conversation).toBe(conversationBefore);
        expect(fetchedUrls).toEqual(['/api/v1/auth/me']);
        expect(el.shadowRoot?.querySelector('scion-chat-space-rail')?.currentUserId).toBe(
          'resolved-self'
        );

        el.remove();
      }
    );
  });
});

describe('_loadPaletteThreads', () => {
  it('a full load resolves ready with incomplete=false when every space succeeds', async () => {
    const el = createPage();
    vi.mocked(apiFetch).mockImplementation((url: string) => {
      if (url === '/api/v1/chat/spaces') {
        return Promise.resolve(
          jsonResponse({ spaces: [{ projectId: 'p1', projectName: 'Alpha' }] })
        );
      }
      return Promise.resolve(
        jsonResponse({ threads: [{ id: 't1', projectId: 'p1', name: 'General' }] })
      );
    });

    await el._loadPaletteThreads();

    expect(el.v2PaletteGroups.threads.status).toBe('ready');
    expect(el.v2PaletteGroups.threads.incomplete).toBeFalsy();
    expect(el.v2PaletteGroups.threads.candidates).toHaveLength(1);
  });

  it('a partial failure stays ready with incomplete=true', async () => {
    const el = createPage();
    vi.mocked(apiFetch).mockImplementation((url: string) => {
      if (url === '/api/v1/chat/spaces') {
        return Promise.resolve(
          jsonResponse({
            spaces: [
              { projectId: 'p1', projectName: 'Alpha' },
              { projectId: 'p2', projectName: 'Beta' },
            ],
          })
        );
      }
      if (url.includes('/p1/threads')) {
        return Promise.resolve(jsonResponse({ threads: [{ id: 't1', projectId: 'p1' }] }));
      }
      return Promise.resolve(jsonResponse({}, 500));
    });

    await el._loadPaletteThreads();

    expect(el.v2PaletteGroups.threads.status).toBe('ready');
    expect(el.v2PaletteGroups.threads.incomplete).toBe(true);
  });

  it('retryOnly=true calls retryThreadsGroup, not a fresh loadThreadsGroup', async () => {
    const el = createPage();
    const controller = el._paletteDataController;
    const loadSpy = vi.spyOn(controller, 'loadThreadsGroup');
    const retrySpy = vi
      .spyOn(controller, 'retryThreadsGroup')
      .mockResolvedValue({ candidates: [], incomplete: false });

    await el._loadPaletteThreads(true);

    expect(retrySpy).toHaveBeenCalledTimes(1);
    expect(loadSpy).not.toHaveBeenCalled();
  });
});

describe('a cancelled or superseded load publishes nothing (page level)', () => {
  it('an aborted or superseded People load never publishes an error state', async () => {
    const el = createPage();
    vi.spyOn(el._paletteDataController, 'loadPeopleGroup').mockRejectedValue(
      new DOMException('superseded by a later load', 'AbortError')
    );

    await el._loadPalettePeople();

    // The transient loading state set at the top of _loadPalettePeople is
    // the last thing published — the catch block's AbortError branch must
    // return silently rather than turning this into an error the palette
    // would show a Retry button for.
    expect(el.v2PaletteGroups.people.status).toBe('loading');
    expect(el.v2PaletteGroups.people.status).not.toBe('error');
  });

  it('an aborted or superseded Threads load never publishes an error state', async () => {
    const el = createPage();
    vi.spyOn(el._paletteDataController, 'loadThreadsGroup').mockRejectedValue(
      new DOMException('superseded by a later load', 'AbortError')
    );

    await el._loadPaletteThreads();

    expect(el.v2PaletteGroups.threads.status).toBe('loading');
    expect(el.v2PaletteGroups.threads.status).not.toBe('error');
  });
});

const NAMES = ['Alice', 'Bob', 'Carol'];

function viableAgent(id: string, name: string): any {
  return agent(id, { name, _capabilities: { actions: ['attach'] } } as any);
}

function agentCandidate(id: string, label: string): PaletteCandidate {
  return {
    id: JSON.stringify(['dm', 'agent', id]),
    group: 'agents',
    label,
    searchFields: [label],
    secondaryLabel: '',
    activityMs: 0,
    target: { kind: 'dm', peerKind: 'agent', peerId: id, displayName: label },
  };
}

describe('_loadPaletteAgents: a refresh does not shrink an already-ready list, and an error keeps whatever is on screen', () => {
  it('a refresh of an already-ready group does not wire onProgress, so it cannot replace the full list with a partial page', async () => {
    const el = createPage();
    const fullList = [agentCandidate('a1', 'Alice'), agentCandidate('a2', 'Bob')];
    el.v2PaletteGroups = {
      ...el.v2PaletteGroups,
      agents: { status: 'ready', candidates: fullList },
    };
    el._agentsSnapshotComplete = true;

    let capturedOnProgress: unknown;
    let resolveLoad!: (v: PaletteCandidate[]) => void;
    vi.spyOn(el._paletteDataController, 'loadAgentsGroup').mockImplementation((onProgress) => {
      capturedOnProgress = onProgress;
      return new Promise((resolve) => {
        resolveLoad = resolve;
      });
    });

    const reload = el._loadPaletteAgents();
    expect(capturedOnProgress).toBeUndefined();
    // The previous full snapshot stays on screen for the whole reload.
    expect(el.v2PaletteGroups.agents.candidates).toEqual(fullList);

    resolveLoad([agentCandidate('a3', 'Carol')]);
    await reload;
    expect(el.v2PaletteGroups.agents.candidates).toEqual([agentCandidate('a3', 'Carol')]);
  });

  it('a first load (no previous ready snapshot) wires onProgress and publishes partial pages as they arrive', async () => {
    const el = createPage();
    let capturedOnProgress!: (partial: PaletteCandidate[]) => void;
    vi.spyOn(el._paletteDataController, 'loadAgentsGroup').mockImplementation((onProgress) => {
      capturedOnProgress = onProgress!;
      return new Promise(() => {});
    });

    void el._loadPaletteAgents();
    expect(capturedOnProgress).toBeDefined();

    capturedOnProgress([agentCandidate('a1', 'Alice')]);
    expect(el.v2PaletteGroups.agents.status).toBe('loading');
    expect(el.v2PaletteGroups.agents.candidates).toEqual([agentCandidate('a1', 'Alice')]);
  });

  it('a retry after an error also wires onProgress (no complete snapshot to protect)', async () => {
    const el = createPage();
    el.v2PaletteGroups = {
      ...el.v2PaletteGroups,
      agents: { status: 'error', candidates: [], error: 'boom' },
    };
    let capturedOnProgress: unknown;
    vi.spyOn(el._paletteDataController, 'loadAgentsGroup').mockImplementation((onProgress) => {
      capturedOnProgress = onProgress;
      return new Promise(() => {});
    });

    void el._loadPaletteAgents();
    expect(capturedOnProgress).toBeDefined();
  });

  it('an error during a refresh of an already-ready group preserves the previous candidates instead of wiping them', async () => {
    const el = createPage();
    const fullList = [agentCandidate('a1', 'Alice')];
    el.v2PaletteGroups = {
      ...el.v2PaletteGroups,
      agents: { status: 'ready', candidates: fullList },
    };
    el._agentsSnapshotComplete = true;
    vi.spyOn(el._paletteDataController, 'loadAgentsGroup').mockRejectedValue(
      new PaletteLoadError('boom')
    );

    await el._loadPaletteAgents();

    expect(el.v2PaletteGroups.agents.status).toBe('error');
    expect(el.v2PaletteGroups.agents.candidates).toEqual(fullList);
  });

  it('an error during a first load preserves whatever partial progress was already published', async () => {
    const el = createPage();
    let capturedOnProgress!: (partial: PaletteCandidate[]) => void;
    let rejectLoad!: (err: unknown) => void;
    vi.spyOn(el._paletteDataController, 'loadAgentsGroup').mockImplementation((onProgress) => {
      capturedOnProgress = onProgress!;
      return new Promise((_resolve, reject) => {
        rejectLoad = reject;
      });
    });

    const load = el._loadPaletteAgents();
    capturedOnProgress([agentCandidate('a1', 'Alice')]);
    rejectLoad(new PaletteLoadError('boom'));
    await load;

    expect(el.v2PaletteGroups.agents.status).toBe('error');
    expect(el.v2PaletteGroups.agents.candidates).toEqual([agentCandidate('a1', 'Alice')]);
  });

  // The next two run the real `ChatPaletteDataController` over a real agent
  // store (in-memory agent server, fake feed connection), unlike the
  // spied-`loadAgentsGroup` tests above, so the group's real `status`
  // transitions are exercised.

  it('closing and reopening the palette mid-refresh keeps the complete list, answered from the store', async () => {
    vi.useFakeTimers();
    const h = createHarness(['a1', 'a2', 'a3'].map((id, i) => viableAgent(id, NAMES[i]!)));
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse({ dms: [] }));
    const el = createPage();
    el._paletteDataController = new ChatPaletteDataController(h.store);
    el.v2PaletteOpen = true;
    const fullList = [
      agentCandidate('a1', 'Alice'),
      agentCandidate('a2', 'Bob'),
      agentCandidate('a3', 'Carol'),
    ];

    const first = el._loadPaletteAgents();
    await h.connect();
    await first;
    expect(el.v2PaletteGroups.agents.status).toBe('ready');
    expect(el.v2PaletteGroups.agents.candidates).toEqual(fullList);

    // The refresh's DM fetch hangs; the full list stays on screen.
    vi.mocked(apiFetch).mockImplementation(
      (_url: string, options?: { signal?: AbortSignal | null }) =>
        new Promise((_resolve, reject) => {
          options?.signal?.addEventListener('abort', () =>
            reject(new DOMException('aborted', 'AbortError'))
          );
        })
    );
    const refresh = el._loadPaletteAgents();
    expect(el.v2PaletteGroups.agents.candidates).toEqual(fullList);
    el._closePaletteAndCancelLoad();
    await refresh;
    expect(el.v2PaletteGroups.agents.candidates).toEqual(fullList);

    el.v2PaletteOpen = true;
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse({ dms: [] }));
    await el._loadPaletteAgents();
    expect(el.v2PaletteGroups.agents).toEqual({ status: 'ready', candidates: fullList });
    expect(h.server.walks()).toBe(1);
    h.store.destroy();
  });

  it('reopening within the DM cache shows the list as ready at once, with no request', async () => {
    vi.useFakeTimers();
    const h = createHarness(['a1', 'a2'].map((id, i) => viableAgent(id, NAMES[i]!)));
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse({ dms: [] }));
    const el = createPage();
    el._paletteDataController = new ChatPaletteDataController(h.store);
    el.v2PaletteOpen = true;
    const first = el._loadPaletteAgents();
    await h.connect();
    await first;
    el._closePaletteAndCancelLoad();
    el.v2PaletteGroups = { ...el.v2PaletteGroups, agents: { status: 'loading', candidates: [] } };

    el.v2PaletteOpen = true;
    const reopen = el._loadPaletteAgents();
    expect(el.v2PaletteGroups.agents).toEqual({
      status: 'ready',
      candidates: [agentCandidate('a1', 'Alice'), agentCandidate('a2', 'Bob')],
    });
    await reopen;
    expect(vi.mocked(apiFetch)).toHaveBeenCalledTimes(1);
    expect(h.server.walks()).toBe(1);
    h.store.destroy();
  });

  it('reopening after the DM cache expires shows loading until the DMs return', async () => {
    vi.useFakeTimers();
    const h = createHarness([viableAgent('a1', 'Alice')]);
    vi.mocked(apiFetch).mockImplementation(() => Promise.resolve(jsonResponse({ dms: [] })));
    const el = createPage();
    el._paletteDataController = new ChatPaletteDataController(h.store);
    el.v2PaletteOpen = true;
    const first = el._loadPaletteAgents();
    await h.connect();
    await first;
    el._closePaletteAndCancelLoad();
    el.v2PaletteGroups = { ...el.v2PaletteGroups, agents: { status: 'loading', candidates: [] } };
    vi.advanceTimersByTime(AGENT_DMS_CACHE_MS);

    el.v2PaletteOpen = true;
    const reopen = el._loadPaletteAgents();
    expect(el.v2PaletteGroups.agents.status).toBe('loading');
    await reopen;
    expect(el.v2PaletteGroups.agents).toEqual({
      status: 'ready',
      candidates: [agentCandidate('a1', 'Alice')],
    });
    expect(vi.mocked(apiFetch)).toHaveBeenCalledTimes(2);
    h.store.destroy();
  });

  it('a chat message while the palette is open re-reads the DMs, keeping the list ready', async () => {
    vi.useFakeTimers();
    const h = createHarness([viableAgent('a1', 'Alice')]);
    const at = '2026-01-02T03:04:05Z';
    let dms: unknown[] = [];
    vi.mocked(apiFetch).mockImplementation(() => Promise.resolve(jsonResponse({ dms })));
    const el = createPage();
    // The unread-dot refresh reads the DM list on its own; only palette reads count here.
    vi.spyOn(el, 'loadUnreadDMPeers').mockResolvedValue(undefined);
    vi.spyOn(el, '_loadPalettePeople').mockResolvedValue(undefined);
    vi.spyOn(el, '_loadPaletteThreads').mockResolvedValue(undefined);
    el._paletteDataController = new ChatPaletteDataController(h.store);
    el.v2PaletteOpen = true;
    const first = el._loadPaletteAgents();
    await h.connect();
    await first;
    expect(vi.mocked(apiFetch)).toHaveBeenCalledTimes(1);

    dms = [{ conversationKey: 'k', peerId: 'a1', peerKind: 'agent', lastActivityAt: at }];
    const agentLoads = vi.spyOn(el, '_loadPaletteAgents');
    el.handleChatMessage(new CustomEvent('chat-message-received', { detail: {} }));
    await vi.advanceTimersByTimeAsync(499);
    expect(vi.mocked(apiFetch)).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(1);
    expect(el.v2PaletteGroups.agents.status).toBe('ready');
    await vi.advanceTimersByTimeAsync(0);

    expect(vi.mocked(apiFetch)).toHaveBeenCalledTimes(2);
    expect(el.v2PaletteGroups.agents).toEqual({
      status: 'ready',
      candidates: [{ ...agentCandidate('a1', 'Alice'), activityMs: Date.parse(at) }],
    });
    expect(h.server.walks()).toBe(1);

    // A later refresh for another group does not re-read the DMs again.
    el.handleChatTopic(new CustomEvent('chat-topic-updated', { detail: {} }));
    await vi.advanceTimersByTimeAsync(500);
    expect(agentLoads).toHaveBeenCalledTimes(1);
    expect(vi.mocked(apiFetch)).toHaveBeenCalledTimes(2);
    h.store.destroy();
  });

  it('a chat message while the palette is closed re-reads the DMs only on the next open', async () => {
    vi.useFakeTimers();
    const h = createHarness([viableAgent('a1', 'Alice')]);
    vi.mocked(apiFetch).mockImplementation(() => Promise.resolve(jsonResponse({ dms: [] })));
    const el = createPage();
    // The unread-dot refresh reads the DM list on its own; only palette reads count here.
    vi.spyOn(el, 'loadUnreadDMPeers').mockResolvedValue(undefined);
    el._paletteDataController = new ChatPaletteDataController(h.store);
    el.v2PaletteOpen = true;
    const first = el._loadPaletteAgents();
    await h.connect();
    await first;
    el._closePaletteAndCancelLoad();

    el.handleChatMessage(new CustomEvent('chat-message-received', { detail: {} }));
    await vi.advanceTimersByTimeAsync(5_000);
    expect(vi.mocked(apiFetch)).toHaveBeenCalledTimes(1);

    el.v2PaletteOpen = true;
    await el._loadPaletteAgents();
    expect(vi.mocked(apiFetch)).toHaveBeenCalledTimes(2);
    h.store.destroy();
  });

  it('marking the agent DMs stale on its own refreshes them while the palette is open', () => {
    const el = createPage();
    vi.useFakeTimers();
    el.v2PaletteOpen = true;
    const agentsSpy = vi.spyOn(el, '_loadPaletteAgents').mockResolvedValue(undefined);

    el._markAgentDmsStale();
    vi.advanceTimersByTime(500);

    expect(agentsSpy).toHaveBeenCalledTimes(1);
    expect(agentsSpy).toHaveBeenCalledWith({ keepReady: true });
  });

  it('a chat message during an Agents load re-reads the DMs once that load settles', async () => {
    const el = createPage();
    vi.useFakeTimers();
    el.v2PaletteOpen = true;
    let finish = (): void => {};
    const agentsSpy = vi.spyOn(el, '_loadPaletteAgents').mockImplementation(() => {
      const token = {};
      el._paletteGroupLoadToken.agents = token;
      el._agentDmsRefreshPending = false;
      return new Promise<void>((resolve) => {
        finish = (): void => {
          delete el._paletteGroupLoadToken.agents;
          resolve();
        };
      });
    });
    vi.spyOn(el, '_loadPalettePeople').mockResolvedValue(undefined);
    vi.spyOn(el, '_loadPaletteThreads').mockResolvedValue(undefined);
    void el._loadPaletteAgents();
    expect(agentsSpy).toHaveBeenCalledTimes(1);

    el.handleChatMessage(new CustomEvent('chat-message-received', { detail: {} }));
    vi.advanceTimersByTime(500);
    expect(agentsSpy).toHaveBeenCalledTimes(1);

    finish();
    vi.advanceTimersByTime(500);
    expect(agentsSpy).toHaveBeenCalledTimes(2);
    expect(agentsSpy).toHaveBeenLastCalledWith({ keepReady: true });
    vi.advanceTimersByTime(5_000);
    expect(agentsSpy).toHaveBeenCalledTimes(2);
  });

  it('retrying after an agent-list error keeps the list on screen and publishes the store result', async () => {
    vi.useFakeTimers();
    const h = createHarness([viableAgent('a1', 'Alice'), viableAgent('a2', 'Bob')]);
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse({ dms: [] }));
    const el = createPage();
    el._paletteDataController = new ChatPaletteDataController(h.store);
    const fullList = [agentCandidate('a1', 'Alice'), agentCandidate('a2', 'Bob')];
    el.v2PaletteGroups = {
      ...el.v2PaletteGroups,
      agents: { status: 'ready', candidates: fullList },
    };
    el._agentsSnapshotComplete = true;

    h.server.status = 500;
    const failing = el._loadPaletteAgents();
    await h.connect();
    await failing;
    expect(el.v2PaletteGroups.agents.status).toBe('error');
    expect(el.v2PaletteGroups.agents.candidates).toEqual(fullList);

    h.server.status = 200;
    h.server.agents.push(viableAgent('a4', 'Dave'));
    await el._loadPaletteAgents();
    expect(el.v2PaletteGroups.agents).toEqual({
      status: 'ready',
      candidates: [...fullList, agentCandidate('a4', 'Dave')],
    });
    h.store.destroy();
  });
});

describe('Agents group follows the agent store', () => {
  it('connecting retains the hub entry once and disconnecting releases it', () => {
    const release = vi.fn();
    const retain = vi.spyOn(agentStore, 'retain').mockReturnValue(release);
    const el = createPage();
    document.body.appendChild(el);
    expect(retain).toHaveBeenCalledTimes(1);
    expect(retain.mock.calls[0]?.[0]).toEqual({ scope: 'hub' });

    el.remove();
    expect(release).toHaveBeenCalledTimes(1);
    retain.mockRestore();
  });

  it('an SSE status change re-derives the loaded Agents group with no request', async () => {
    vi.useFakeTimers();
    const h = createHarness([viableAgent('a1', 'Alice'), viableAgent('a2', 'Bob')]);
    vi.mocked(apiFetch).mockResolvedValue(jsonResponse({ dms: [] }));
    const el = createPage();
    el._paletteDataController = new ChatPaletteDataController(h.store);
    const release = h.store.retain({ scope: 'hub' }, (snapshot) =>
      el._handlePaletteAgentSnapshot(snapshot)
    );
    el.v2PaletteOpen = true;
    const load = el._loadPaletteAgents();
    await h.connect();
    await load;
    const dmRequests = vi.mocked(apiFetch).mock.calls.length;

    // A messageability change removes Bob; a created agent adds Carol.
    await h.emitAgent('status', {
      agentId: 'a2',
      _messageability: { canMessage: false },
    });
    await h.emitAgent('created', {
      agentId: 'a3',
      projectId: 'p1',
      name: 'Carol',
      phase: 'running',
      _capabilities: { actions: ['attach'] },
    });

    expect(el.v2PaletteGroups.agents).toEqual({
      status: 'ready',
      candidates: [agentCandidate('a1', 'Alice'), agentCandidate('a3', 'Carol')],
    });
    expect(h.server.requests).toHaveLength(1);
    expect(vi.mocked(apiFetch).mock.calls.length).toBe(dmRequests);
    release();
    h.store.destroy();
  });

  it('ignores store snapshots while the palette is closed', () => {
    const el = createPage();
    el.v2PaletteOpen = false;
    const derive = vi
      .spyOn(el._paletteDataController, 'deriveAgentCandidates')
      .mockReturnValue([agentCandidate('a1', 'Alice')]);
    el.v2PaletteGroups = {
      ...el.v2PaletteGroups,
      agents: { status: 'ready', candidates: [] },
    };
    el._handlePaletteAgentSnapshot({
      key: 'hub',
      agents: [viableAgent('a1', 'Alice')],
      status: 'ready',
      complete: true,
      version: 2,
    });
    expect(derive).not.toHaveBeenCalled();
    expect(el.v2PaletteGroups.agents).toEqual({ status: 'ready', candidates: [] });
  });

  it('ignores store snapshots before the first load, while a load is in flight, and while loading', async () => {
    const el = createPage();
    el.v2PaletteOpen = true;
    const ready = {
      key: 'hub',
      agents: [viableAgent('a1', 'Alice')],
      status: 'ready',
      complete: true,
      version: 2,
    };
    el._handlePaletteAgentSnapshot(ready);
    expect(el.v2PaletteGroups.agents).toEqual({ status: 'loading', candidates: [] });

    const derive = vi
      .spyOn(el._paletteDataController, 'deriveAgentCandidates')
      .mockReturnValue([agentCandidate('a1', 'Alice')]);
    el._handlePaletteAgentSnapshot({ ...ready, status: 'loading' });
    expect(derive).not.toHaveBeenCalled();

    el._paletteGroupLoadToken.agents = {};
    el._handlePaletteAgentSnapshot(ready);
    expect(derive).not.toHaveBeenCalled();

    delete el._paletteGroupLoadToken.agents;
    el._handlePaletteAgentSnapshot(ready);
    expect(el.v2PaletteGroups.agents).toEqual({
      status: 'ready',
      candidates: [agentCandidate('a1', 'Alice')],
    });
  });
});

describe('_handlePaletteRetry: dispatch per group', () => {
  it('routes agents/people/threads retries to their own loader', () => {
    const el = createPage();
    const agentsSpy = vi.spyOn(el, '_loadPaletteAgents').mockResolvedValue(undefined);
    const peopleSpy = vi.spyOn(el, '_loadPalettePeople').mockResolvedValue(undefined);
    const threadsSpy = vi.spyOn(el, '_loadPaletteThreads').mockResolvedValue(undefined);

    el._handlePaletteRetry(new CustomEvent('palette-retry', { detail: { group: 'agents' } }));
    expect(agentsSpy).toHaveBeenCalledTimes(1);

    el._handlePaletteRetry(new CustomEvent('palette-retry', { detail: { group: 'people' } }));
    expect(peopleSpy).toHaveBeenCalledTimes(1);

    el._handlePaletteRetry(new CustomEvent('palette-retry', { detail: { group: 'threads' } }));
    expect(threadsSpy).toHaveBeenCalledWith(true);
  });
});

// ===========================================================================
// 30s per-group cache, SSE invalidation, and debounced refresh
// ===========================================================================

describe('palette group cache: 30s freshness window', () => {
  it('a ready group within the cache window is reused, skipping a refetch on open', () => {
    const el = createPage();
    vi.useFakeTimers();
    el.v2PaletteGroups = { agents: { status: 'ready', candidates: [] } };
    el._paletteGroupCacheAt.agents = Date.now();

    expect(el._shouldUseCachedPaletteGroup('agents')).toBe(true);
  });

  it('a group past the 30s window is stale, even if never invalidated', () => {
    const el = createPage();
    vi.useFakeTimers();
    el.v2PaletteGroups = { agents: { status: 'ready', candidates: [] } };
    el._paletteGroupCacheAt.agents = Date.now();
    vi.advanceTimersByTime(30_001);

    expect(el._shouldUseCachedPaletteGroup('agents')).toBe(false);
  });

  it('the 30s boundary itself is exclusive: exactly 30000ms elapsed is stale, 29999ms is still fresh', () => {
    // Discriminates `<` from `<=` in the cache-age comparison, which the
    // 30001ms test above cannot (both operators agree once elapsed is
    // already past the window).
    const el = createPage();
    vi.useFakeTimers();
    el.v2PaletteGroups = { agents: { status: 'ready', candidates: [] } };
    el._paletteGroupCacheAt.agents = Date.now();

    vi.advanceTimersByTime(29_999);
    expect(el._shouldUseCachedPaletteGroup('agents')).toBe(true);

    vi.advanceTimersByTime(1); // now exactly 30_000ms elapsed
    expect(el._shouldUseCachedPaletteGroup('agents')).toBe(false);
  });

  it('a group marked dirty is not reused even inside the cache window', () => {
    const el = createPage();
    el.v2PaletteGroups = { agents: { status: 'ready', candidates: [] } };
    el._paletteGroupCacheAt.agents = Date.now();
    el._paletteGroupDirty.agents = true;

    expect(el._shouldUseCachedPaletteGroup('agents')).toBe(false);
  });

  it('a group that never finished loading (status not ready) is never cached', () => {
    const el = createPage();
    el.v2PaletteGroups = { agents: { status: 'loading', candidates: [] } };
    el._paletteGroupCacheAt.agents = Date.now();
    expect(el._shouldUseCachedPaletteGroup('agents')).toBe(false);
  });

  it('a group with no recorded cache timestamp is never reused', () => {
    const el = createPage();
    el.v2PaletteGroups = { agents: { status: 'ready', candidates: [] } };
    expect(el._shouldUseCachedPaletteGroup('agents')).toBe(false);
  });

  it('_loadPaletteGroupsOnOpen skips a fresh cached group but reloads a dirty one, and always loads Agents from the store', () => {
    const el = createPage();
    el.v2PaletteGroups = {
      agents: { status: 'ready', candidates: [] },
      people: { status: 'ready', candidates: [] },
      threads: { status: 'ready', candidates: [] },
    };
    const now = Date.now();
    el._paletteGroupCacheAt = { agents: now, people: now, threads: now };
    el._paletteGroupDirty.people = true;

    const agentsSpy = vi.spyOn(el, '_loadPaletteAgents').mockResolvedValue(undefined);
    const peopleSpy = vi.spyOn(el, '_loadPalettePeople').mockResolvedValue(undefined);
    const threadsSpy = vi.spyOn(el, '_loadPaletteThreads').mockResolvedValue(undefined);

    el._loadPaletteGroupsOnOpen();

    expect(agentsSpy).toHaveBeenCalledTimes(1);
    expect(threadsSpy).not.toHaveBeenCalled();
    expect(peopleSpy).toHaveBeenCalledTimes(1);
  });

  it('an Agents load keeps no cache timestamp or dirty flag: the agent store keeps that group current', async () => {
    const el = createPage();
    vi.spyOn(el._paletteDataController, 'loadAgentsGroup').mockResolvedValue([]);

    await el._loadPaletteAgents();

    expect(el.v2PaletteGroups.agents.status).toBe('ready');
    expect(el._paletteGroupDirty.agents).toBeUndefined();
    expect(el._paletteGroupCacheAt.agents).toBeUndefined();
  });

  it('a successful People load clears People dirty specifically', async () => {
    const el = createPage();
    el.v2PaletteOpen = true;
    el._paletteGroupDirty.people = true;
    vi.spyOn(el._paletteDataController, 'loadPeopleGroup').mockResolvedValue([]);

    await el._loadPalettePeople();

    expect(el._paletteGroupDirty.people).toBe(false);
    expect(el._paletteGroupCacheAt.people).toBeGreaterThan(0);
  });

  it('a successful Threads load clears Threads dirty specifically', async () => {
    const el = createPage();
    el._paletteGroupDirty.threads = true;
    vi.spyOn(el._paletteDataController, 'loadThreadsGroup').mockResolvedValue({
      candidates: [],
      incomplete: false,
    });

    await el._loadPaletteThreads();

    expect(el._paletteGroupDirty.threads).toBe(false);
    expect(el._paletteGroupCacheAt.threads).toBeGreaterThan(0);
  });
});

describe('Threads "incomplete" is carried through a reload, not dropped while loading', () => {
  it('a reload of a previously-incomplete Threads group keeps incomplete=true on the transient loading state', async () => {
    const el = createPage();
    el.v2PaletteGroups = {
      ...el.v2PaletteGroups,
      threads: { status: 'ready', candidates: [], incomplete: true },
    };
    let resolveLoad!: (v: { candidates: unknown[]; incomplete: boolean }) => void;
    vi.spyOn(el._paletteDataController, 'loadThreadsGroup').mockImplementation(
      () => new Promise((resolve) => (resolveLoad = resolve))
    );

    const reload = el._loadPaletteThreads();
    // Synchronous portion of _loadPaletteThreads has already run and set the
    // transient loading state before awaiting the controller.
    expect(el.v2PaletteGroups.threads.status).toBe('loading');
    expect(el.v2PaletteGroups.threads.incomplete).toBe(true);

    resolveLoad({ candidates: [], incomplete: false });
    await reload;
    expect(el.v2PaletteGroups.threads.incomplete).toBeFalsy();
  });

  it('a reload of a previously-*complete* Threads group does not spuriously mark the transient loading state incomplete', async () => {
    const el = createPage();
    el.v2PaletteGroups = {
      ...el.v2PaletteGroups,
      threads: { status: 'ready', candidates: [] },
    };
    vi.spyOn(el._paletteDataController, 'loadThreadsGroup').mockImplementation(
      () => new Promise(() => {})
    );

    void el._loadPaletteThreads();
    expect(el.v2PaletteGroups.threads.incomplete).toBeFalsy();
  });
});

describe('palette group dirty-marking: SSE invalidation', () => {
  it('handleChatTopic marks only Threads dirty', () => {
    const el = createPage();
    el.handleChatTopic(new CustomEvent('chat-topic-updated', { detail: {} }));
    expect(el._paletteGroupDirty.threads).toBe(true);
    expect(el._paletteGroupDirty.agents).toBeFalsy();
    expect(el._paletteGroupDirty.people).toBeFalsy();
  });

  it('_handleAgentCreated and _handleAgentsUpdated mark nothing dirty and schedule no refresh', () => {
    const el = createPage();
    el.v2PaletteOpen = true;
    el._handleAgentCreated(new CustomEvent('agent-created', { detail: {} }));
    el._handleAgentsUpdated();
    expect(el._paletteGroupDirty).toEqual({});
    expect(el._paletteRefreshDebounce).toBeNull();
  });

  it('handleChatMessage and handleDMPromoted mark People and Threads dirty, not Agents, and mark agent DMs stale', () => {
    const el = createPage();
    const stale = vi.spyOn(el._paletteDataController, 'markAgentDmsStale');
    el.handleChatMessage(new CustomEvent('chat-message-received', { detail: {} }));
    expect(el._paletteGroupDirty).toEqual({ people: true, threads: true });
    expect(stale).toHaveBeenCalledTimes(1);

    el._paletteGroupDirty = {};
    el.handleDMPromoted(
      new CustomEvent('chat-dm-promoted', {
        detail: { oldConversationKey: 'dm:x', newTopic: { id: 't1', projectId: 'p1', name: 'x' } },
      })
    );
    expect(el._paletteGroupDirty).toEqual({ people: true, threads: true });
    expect(stale).toHaveBeenCalledTimes(2);
  });

  it('marking a group dirty while the palette is open schedules a debounced refresh', () => {
    const el = createPage();
    vi.useFakeTimers();
    el.v2PaletteOpen = true;
    const threadsSpy = vi.spyOn(el, '_loadPaletteThreads').mockResolvedValue(undefined);

    el.handleChatTopic(new CustomEvent('chat-topic-updated', { detail: {} }));
    expect(threadsSpy).not.toHaveBeenCalled();

    vi.advanceTimersByTime(500);
    expect(threadsSpy).toHaveBeenCalledTimes(1);
  });

  it('marking a group dirty while the palette is closed does not schedule any refresh', () => {
    const el = createPage();
    vi.useFakeTimers();
    el.v2PaletteOpen = false;
    const threadsSpy = vi.spyOn(el, '_loadPaletteThreads').mockResolvedValue(undefined);

    el.handleChatTopic(new CustomEvent('chat-topic-updated', { detail: {} }));
    vi.advanceTimersByTime(5000);

    expect(threadsSpy).not.toHaveBeenCalled();
  });

  it('closing the palette cancels a pending debounced refresh scheduled while it was open', () => {
    // Distinct from the "closed" test above: that one marks a group dirty
    // while *already* closed, which _markPaletteGroupsDirty's own
    // `if (this.v2PaletteOpen)` check short-circuits before a debounce timer
    // is ever scheduled — it can never observe either
    // _refreshDirtyPaletteGroups' `if (!this.v2PaletteOpen) return;` guard or
    // _closePaletteAndCancelLoad's debounce-stop, since neither is reached.
    // This test schedules the debounce for real (while open), then closes
    // before it fires, and confirms the loader that debounce would have
    // called is never invoked.
    const el = createPage();
    vi.useFakeTimers();
    el.v2PaletteOpen = true;
    const threadsSpy = vi.spyOn(el, '_loadPaletteThreads').mockResolvedValue(undefined);

    el.handleChatTopic(new CustomEvent('chat-topic-updated', { detail: {} }));
    vi.advanceTimersByTime(300); // still inside the 500ms debounce window

    el._closePaletteAndCancelLoad();
    vi.advanceTimersByTime(5000); // long past when the original debounce would have fired

    expect(threadsSpy).not.toHaveBeenCalled();
  });

  it('disconnecting cancels a pending debounced refresh, so a detached page never fires the deferred reload', () => {
    // Distinct from the "closing the palette" test above: disconnecting
    // does not clear v2PaletteOpen, so _refreshDirtyPaletteGroups' own
    // `if (!this.v2PaletteOpen) return;` guard would not stop a debounce
    // that fires after disconnection — only disconnectedCallback's own
    // _stopPaletteDebouncedRefresh() call does.
    const el = createPage();
    vi.useFakeTimers();
    el.v2PaletteOpen = true;
    const threadsSpy = vi.spyOn(el, '_loadPaletteThreads').mockResolvedValue(undefined);

    el.handleChatTopic(new CustomEvent('chat-topic-updated', { detail: {} }));
    vi.advanceTimersByTime(300); // still inside the 500ms debounce window

    el.disconnectedCallback();
    vi.advanceTimersByTime(5000); // long past when the original debounce would have fired

    expect(threadsSpy).not.toHaveBeenCalled();
  });

  it('rapid repeated invalidation only refreshes once after the quiet 500ms window (debounced, not throttled)', () => {
    const el = createPage();
    vi.useFakeTimers();
    el.v2PaletteOpen = true;
    const threadsSpy = vi.spyOn(el, '_loadPaletteThreads').mockResolvedValue(undefined);

    el.handleChatTopic(new CustomEvent('chat-topic-updated', { detail: {} }));
    vi.advanceTimersByTime(300);
    el.handleChatTopic(new CustomEvent('chat-topic-updated', { detail: {} }));
    vi.advanceTimersByTime(300);
    expect(threadsSpy).not.toHaveBeenCalled();
    vi.advanceTimersByTime(200);
    expect(threadsSpy).toHaveBeenCalledTimes(1);
  });

  it('a debounced refresh reloads only the group an SSE event actually invalidated (Threads), leaving Agents and People untouched', () => {
    const el = createPage();
    vi.useFakeTimers();
    el.v2PaletteOpen = true;
    const agentsSpy = vi.spyOn(el, '_loadPaletteAgents').mockResolvedValue(undefined);
    const peopleSpy = vi.spyOn(el, '_loadPalettePeople').mockResolvedValue(undefined);
    const threadsSpy = vi.spyOn(el, '_loadPaletteThreads').mockResolvedValue(undefined);

    // handleChatTopic marks only Threads dirty.
    el.handleChatTopic(new CustomEvent('chat-topic-updated', { detail: {} }));
    vi.advanceTimersByTime(500);

    expect(threadsSpy).toHaveBeenCalledTimes(1);
    expect(agentsSpy).not.toHaveBeenCalled();
    expect(peopleSpy).not.toHaveBeenCalled();
  });

  it('agent SSE events while the palette is open never reload the Agents group', () => {
    const el = createPage();
    vi.useFakeTimers();
    el.v2PaletteOpen = true;
    const agentsSpy = vi.spyOn(el, '_loadPaletteAgents').mockResolvedValue(undefined);
    vi.spyOn(el, '_loadPalettePeople').mockResolvedValue(undefined);
    vi.spyOn(el, '_loadPaletteThreads').mockResolvedValue(undefined);

    el._handleAgentCreated(new CustomEvent('agent-created', { detail: {} }));
    el._handleAgentsUpdated();
    vi.advanceTimersByTime(5_000);

    expect(agentsSpy).not.toHaveBeenCalled();
  });

  it('a debounced refresh reloads only the group marked dirty (People), leaving Agents and Threads untouched', () => {
    const el = createPage();
    vi.useFakeTimers();
    el.v2PaletteOpen = true;
    const agentsSpy = vi.spyOn(el, '_loadPaletteAgents').mockResolvedValue(undefined);
    const peopleSpy = vi.spyOn(el, '_loadPalettePeople').mockResolvedValue(undefined);
    const threadsSpy = vi.spyOn(el, '_loadPaletteThreads').mockResolvedValue(undefined);

    el._markPaletteGroupsDirty('people');
    vi.advanceTimersByTime(500);

    expect(peopleSpy).toHaveBeenCalledTimes(1);
    expect(agentsSpy).not.toHaveBeenCalled();
    expect(threadsSpy).not.toHaveBeenCalled();
  });
});

describe('palette group invalidation during an in-flight load', () => {
  it('an invalidation arriving mid-load leaves the group dirty despite the load completing, and the already-scheduled debounced refresh reloads it', async () => {
    const el = createPage();
    el.v2PaletteOpen = true;
    vi.useFakeTimers();

    let resolveFirstLoad!: (v: { candidates: PaletteCandidate[]; incomplete: boolean }) => void;
    const controller = el._paletteDataController;
    vi.spyOn(controller, 'loadThreadsGroup').mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveFirstLoad = resolve;
        })
    );

    const firstLoad = el._loadPaletteThreads();

    // An SSE event arrives while the load above is still in flight — this
    // schedules the 500ms debounced refresh immediately (from now, not from
    // load completion).
    el.handleChatTopic(new CustomEvent('chat-topic-updated', { detail: {} }));
    expect(el._paletteGroupDirty.threads).toBe(true);

    // The in-flight load (started *before* the invalidation) now resolves.
    // Its own completion must not clear `dirty` — the epoch it captured at
    // start is stale relative to the invalidation that arrived during it.
    resolveFirstLoad({ candidates: [], incomplete: false });
    await firstLoad;

    expect(el.v2PaletteGroups.threads.status).toBe('ready'); // the data is still published...
    expect(el._paletteGroupDirty.threads).toBe(true); // ...but not trusted as fresh
    expect(el._shouldUseCachedPaletteGroup('threads')).toBe(false);

    // The debounce the mid-load event scheduled now fires and reloads.
    const secondLoadSpy = vi.spyOn(el, '_loadPaletteThreads').mockResolvedValue(undefined);
    vi.advanceTimersByTime(500);
    expect(secondLoadSpy).toHaveBeenCalledTimes(1);
  });

  it('control: no invalidation during the load clears dirty and stamps the cache normally', async () => {
    const el = createPage();
    vi.useFakeTimers();
    const controller = el._paletteDataController;
    vi.spyOn(controller, 'loadThreadsGroup').mockResolvedValue({
      candidates: [],
      incomplete: false,
    });

    await el._loadPaletteThreads();

    expect(el._paletteGroupDirty.threads).toBeFalsy();
    expect(el._shouldUseCachedPaletteGroup('threads')).toBe(true);
  });
});

describe('palette group refresh: a dirty-mark debounce firing mid-load defers instead of aborting/restarting it', () => {
  it('Threads: the debounce does not re-enter the controller while its fetch is still in flight; it reloads exactly once the fetch frees up', async () => {
    const el = createPage();
    el.v2PaletteOpen = true;
    vi.useFakeTimers();

    let resolveLoad!: (v: { candidates: PaletteCandidate[]; incomplete: boolean }) => void;
    const controller = el._paletteDataController;
    const loadSpy = vi
      .spyOn(controller, 'loadThreadsGroup')
      .mockImplementation(() => new Promise((resolve) => (resolveLoad = resolve)));

    const firstLoad = el._loadPaletteThreads();
    expect(loadSpy).toHaveBeenCalledTimes(1);

    el._markPaletteGroupsDirty('threads');
    expect(el._paletteGroupDirty.threads).toBe(true);

    vi.advanceTimersByTime(500);
    expect(loadSpy).toHaveBeenCalledTimes(1);

    resolveLoad({ candidates: [], incomplete: false });
    await firstLoad;
    expect(el.v2PaletteGroups.threads?.status).toBe('ready');
    expect(el._paletteGroupDirty.threads).toBe(true);

    vi.advanceTimersByTime(500);
    expect(loadSpy).toHaveBeenCalledTimes(2);
  });
});

describe('_paletteGroupLoadToken: a superseded load cannot clear or overwrite a newer load for the same group', () => {
  it("Agents: load A superseded by load B does not clear B's token, and the next debounce tick leaves B alone", async () => {
    const el = createPage();
    el.v2PaletteOpen = true;
    const controller = el._paletteDataController;

    let resolveA!: (v: PaletteCandidate[]) => void;
    let resolveB!: (v: PaletteCandidate[]) => void;
    const loadSpy = vi
      .spyOn(controller, 'loadAgentsGroup')
      .mockImplementationOnce(() => new Promise((resolve) => (resolveA = resolve)))
      .mockImplementationOnce(() => new Promise((resolve) => (resolveB = resolve)));

    const loadA = el._loadPaletteAgents();
    const tokenAfterA = el._paletteGroupLoadToken.agents;
    expect(tokenAfterA).toBeDefined();

    const loadB = el._loadPaletteAgents();
    const tokenAfterB = el._paletteGroupLoadToken.agents;
    expect(tokenAfterB).toBeDefined();
    expect(tokenAfterB).not.toBe(tokenAfterA);

    // A (superseded) resolves and unwinds first.
    resolveA([]);
    await loadA;
    // A's own `finally` must not have cleared B's token.
    expect(el._paletteGroupLoadToken.agents).toBe(tokenAfterB);

    // A debounce tick running right now must see the group as still
    // occupied (by B), not call the controller a third time.
    el._refreshDirtyPaletteGroups();
    expect(loadSpy).toHaveBeenCalledTimes(2);

    resolveB([]);
    await loadB;
    expect(el._paletteGroupLoadToken.agents).toBeUndefined();
    expect(el.v2PaletteGroups.agents?.status).toBe('ready');
  });

  it("People: a load superseded via _peopleLoadSeq while resolving identity does not clear a newer load's token", async () => {
    const el = createPage();
    el.v2PaletteOpen = true;
    el.pageData = { user: {} }; // forces _resolveSelfUserId to await /auth/me for both loads

    let resolveAuthA!: (v: Response) => void;
    let resolveAuthB!: (v: Response) => void;
    vi.mocked(apiFetch)
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            resolveAuthA = resolve;
          })
      )
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            resolveAuthB = resolve;
          })
      );
    vi.spyOn(el._paletteDataController, 'loadPeopleGroup').mockResolvedValue([]);

    const loadA = el._loadPalettePeople();
    const tokenAfterA = el._paletteGroupLoadToken.people;
    expect(tokenAfterA).toBeDefined();

    // B supersedes A via `_peopleLoadSeq` before either identity fetch
    // resolves — both are still pending at this point.
    const loadB = el._loadPalettePeople();
    const tokenAfterB = el._paletteGroupLoadToken.people;
    expect(tokenAfterB).not.toBe(tokenAfterA);

    // A's identity fetch resolves first; A takes its `mySeq` early return
    // while B's own identity fetch is still pending.
    resolveAuthA(jsonResponse({ id: 'self-user', email: 's@example.com', displayName: 'Self' }));
    await loadA;
    // A's `finally` must not have cleared B's token.
    expect(el._paletteGroupLoadToken.people).toBe(tokenAfterB);

    resolveAuthB(jsonResponse({ id: 'self-user', email: 's@example.com', displayName: 'Self' }));
    await loadB;
    expect(el._paletteGroupLoadToken.people).toBeUndefined();
    expect(el.v2PaletteGroups.people?.status).toBe('ready');
  });

  it("Threads: load A superseded by load B does not clear B's token, and the next debounce tick leaves B alone", async () => {
    const el = createPage();
    el.v2PaletteOpen = true;
    const controller = el._paletteDataController;

    let rejectA!: (err: unknown) => void;
    let resolveB!: (v: { candidates: PaletteCandidate[]; incomplete: boolean }) => void;
    const loadSpy = vi
      .spyOn(controller, 'loadThreadsGroup')
      .mockImplementationOnce(() => new Promise((_resolve, reject) => (rejectA = reject)))
      .mockImplementationOnce(() => new Promise((resolve) => (resolveB = resolve)));

    const loadA = el._loadPaletteThreads();
    const tokenAfterA = el._paletteGroupLoadToken.threads;
    expect(tokenAfterA).toBeDefined();

    const loadB = el._loadPaletteThreads();
    const tokenAfterB = el._paletteGroupLoadToken.threads;
    expect(tokenAfterB).toBeDefined();
    expect(tokenAfterB).not.toBe(tokenAfterA);

    // A (superseded) is rejected the way the real data controller's own
    // generation check rejects a stale call — an AbortError-shaped
    // rejection — and unwinds first.
    rejectA(new DOMException('superseded by a later load', 'AbortError'));
    await loadA;
    // A's own `finally` must not have cleared B's token.
    expect(el._paletteGroupLoadToken.threads).toBe(tokenAfterB);

    // A debounce tick running right now must see the group as still
    // occupied (by B), not call the controller a third time.
    el._refreshDirtyPaletteGroups();
    expect(loadSpy).toHaveBeenCalledTimes(2);

    resolveB({ candidates: [], incomplete: false });
    await loadB;
    expect(el._paletteGroupLoadToken.threads).toBeUndefined();
    expect(el.v2PaletteGroups.threads?.status).toBe('ready');
  });

  it("People: load A's loadPeopleGroup resolving after load B has already taken over (still awaiting its own identity) does not publish A's stale result", async () => {
    // The one reachable publish guard: A is already past `_resolveSelfUserId`
    // and inside `loadPeopleGroup` — nothing the data controller tracks has
    // superseded that specific call — while B is still awaiting its own
    // `/auth/me`. Unlike the overlap test above (A superseded while
    // resolving identity), here A is the one that resolves successfully
    // first, and must not be allowed to publish over B's still-in-flight
    // load.
    const el = createPage();
    el.v2PaletteOpen = true;
    el.pageData = { user: { id: 'self-user' } }; // known id: _resolveSelfUserId resolves synchronously

    let resolveAuthB!: (v: Response) => void;
    vi.mocked(apiFetch).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveAuthB = resolve;
        })
    );

    let resolveLoadA!: (v: PaletteCandidate[]) => void;
    vi.spyOn(el._paletteDataController, 'loadPeopleGroup').mockImplementationOnce(
      () => new Promise((resolve) => (resolveLoadA = resolve))
    );

    const loadA = el._loadPalettePeople();
    const tokenAfterA = el._paletteGroupLoadToken.people;
    // `_resolveSelfUserId` still suspends at its own `await`, even on the
    // known-id path with no real fetch — let A actually reach and call
    // `loadPeopleGroup` before touching anything else.
    await vi.waitFor(() => expect(resolveLoadA).toBeDefined());

    // B starts — known id is already gone (pageData.user.id cleared) so it
    // awaits its own `/auth/me`, still pending.
    el.pageData = { user: {} };
    const loadB = el._loadPalettePeople();
    const tokenAfterB = el._paletteGroupLoadToken.people;
    expect(tokenAfterB).not.toBe(tokenAfterA);

    // A's loadPeopleGroup resolves while B is still awaiting identity.
    const finishSpy = vi.spyOn(el, '_finishPaletteGroupLoad' as never);
    resolveLoadA([]);
    await loadA;

    // A must not have published `ready`, cached, or cleared B's token.
    expect(el.v2PaletteGroups.people?.status).not.toBe('ready');
    expect(finishSpy).not.toHaveBeenCalled();
    expect(el._paletteGroupLoadToken.people).toBe(tokenAfterB);

    resolveAuthB(jsonResponse({ id: 'self-user', email: 's@example.com', displayName: 'Self' }));
    vi.spyOn(el._paletteDataController, 'loadPeopleGroup').mockResolvedValueOnce([]);
    await loadB;
    expect(el.v2PaletteGroups.people?.status).toBe('ready');
  });

  it("People: load A's loadPeopleGroup rejecting (a real, non-abort failure) after load B has already taken over does not publish A's stale error", async () => {
    // Same window as the success-path test above, but A fails instead of
    // succeeding: nothing the data controller tracks has superseded A's
    // call, so without this check A's `catch` would publish a stale `error`
    // (wiping candidates) over B's own in-progress or already-`ready` state.
    const el = createPage();
    el.v2PaletteOpen = true;
    el.pageData = { user: { id: 'self-user' } };

    let resolveAuthB!: (v: Response) => void;
    vi.mocked(apiFetch).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveAuthB = resolve;
        })
    );

    let rejectLoadA!: (err: unknown) => void;
    vi.spyOn(el._paletteDataController, 'loadPeopleGroup').mockImplementationOnce(
      () =>
        new Promise((_resolve, reject) => {
          rejectLoadA = reject;
        })
    );

    const loadA = el._loadPalettePeople();
    const tokenAfterA = el._paletteGroupLoadToken.people;
    await vi.waitFor(() => expect(rejectLoadA).toBeDefined());

    el.pageData = { user: {} };
    const loadB = el._loadPalettePeople();
    const tokenAfterB = el._paletteGroupLoadToken.people;
    expect(tokenAfterB).not.toBe(tokenAfterA);

    // A fails for a real (non-abort) reason while B is still awaiting identity.
    rejectLoadA(new Error('network down'));
    await loadA;

    // A must not have published `error` (wiping candidates) or cleared B's token.
    expect(el.v2PaletteGroups.people?.status).not.toBe('error');
    expect(el._paletteGroupLoadToken.people).toBe(tokenAfterB);

    resolveAuthB(jsonResponse({ id: 'self-user', email: 's@example.com', displayName: 'Self' }));
    vi.spyOn(el._paletteDataController, 'loadPeopleGroup').mockResolvedValueOnce([]);
    await loadB;
    expect(el.v2PaletteGroups.people?.status).toBe('ready');
  });
});
