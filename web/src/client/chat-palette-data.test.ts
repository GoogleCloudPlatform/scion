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
 * Tests for the chat palette's real Agents/DM data adapter: full pagination
 * past 100 entries including a filtered empty intermediate page with a
 * cursor, repeated-cursor-as-error detection, DM-recency join, and
 * `_messageability`/capability-fallback viability.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, afterEach } from 'vitest';

vi.mock('./api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api.js')>();
  return {
    ...actual,
    apiFetch: vi.fn(),
  };
});

import { apiFetch } from './api.js';
import { setPreferredTimeZone } from '../utils/time.js';
import {
  fetchAllPaletteAgents,
  fetchPaletteDms,
  buildAgentCandidates,
  isPaletteAgentViable,
  fetchAllPaletteUsers,
  buildUserCandidates,
  isPaletteUserViable,
  fetchPaletteSpaces,
  fetchPaletteThreadsForSpace,
  buildThreadCandidates,
  buildDocumentCandidates,
  PaletteLoadError,
  ChatPaletteDataController,
  loadPaletteAgentsBounded,
  AGENT_DMS_CACHE_MS,
  type PaletteAgentSource,
  type RawPaletteAgent,
  type RawPaletteDm,
  type RawPaletteUser,
  type RawPaletteSpace,
  type RawPaletteThread,
} from './chat-palette-data.js';
import type { RecentFile } from './chat-recent-files.js';
import type { AgentListSnapshot } from './agent-store.js';
import type { Agent } from '../shared/types.js';
import { agent as harnessAgent, createHarness } from './__fixtures__/agent-store-harness.js';

const apiFetchMock = vi.mocked(apiFetch);

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status });
}

afterEach(() => {
  apiFetchMock.mockReset();
  vi.useRealTimers();
});

describe('fetchAllPaletteAgents: full pagination', () => {
  it('traverses more than 100 entries across pages', async () => {
    const page1Agents = Array.from({ length: 100 }, (_, i) => ({ id: `a${i}` }));
    const page2Agents = Array.from({ length: 30 }, (_, i) => ({ id: `b${i}` }));
    apiFetchMock
      .mockResolvedValueOnce(jsonResponse({ agents: page1Agents, nextCursor: 'cursor-1' }))
      .mockResolvedValueOnce(jsonResponse({ agents: page2Agents }));

    const all = await fetchAllPaletteAgents();

    expect(all).toHaveLength(130);
    expect(apiFetchMock).toHaveBeenCalledTimes(2);
    expect(apiFetchMock.mock.calls[1][0]).toContain('cursor=cursor-1');
  });

  it('continues past a filtered empty intermediate page that still carries a cursor', async () => {
    apiFetchMock
      .mockResolvedValueOnce(jsonResponse({ agents: [{ id: 'a0' }], nextCursor: 'cursor-1' }))
      // Filtered empty page: zero items, but a cursor is still present.
      .mockResolvedValueOnce(jsonResponse({ agents: [], nextCursor: 'cursor-2' }))
      .mockResolvedValueOnce(jsonResponse({ agents: [{ id: 'a1' }] }));

    const all = await fetchAllPaletteAgents();

    expect(all.map((a) => a.id)).toEqual(['a0', 'a1']);
    expect(apiFetchMock).toHaveBeenCalledTimes(3);
  });

  it('stops when nextCursor is absent even if totalCount implied more', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse({ agents: [{ id: 'a0' }] }));
    const all = await fetchAllPaletteAgents();
    expect(all).toHaveLength(1);
    expect(apiFetchMock).toHaveBeenCalledTimes(1);
  });

  it('treats a repeated cursor as a load error, not an infinite loop', async () => {
    apiFetchMock
      .mockResolvedValueOnce(jsonResponse({ agents: [{ id: 'a0' }], nextCursor: 'loop' }))
      .mockResolvedValueOnce(jsonResponse({ agents: [{ id: 'a1' }], nextCursor: 'loop' }));

    await expect(fetchAllPaletteAgents()).rejects.toThrow(PaletteLoadError);
    expect(apiFetchMock).toHaveBeenCalledTimes(2);
  });

  it('throws PaletteLoadError on a non-ok response', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse({}, 500));
    await expect(fetchAllPaletteAgents()).rejects.toThrow(PaletteLoadError);
  });

  it('an abort landing during the body read rejects with the original AbortError, not PaletteLoadError', async () => {
    // A cancelled/superseded load can abort its signal after `res` has
    // already resolved but before `res.json()` finishes reading the body —
    // that rejects with an AbortError that must propagate as-is, not be
    // rewritten into a load-failure error — a catch block that
    // unconditionally did `throw new PaletteLoadError(...)` here would do
    // exactly that, masking the cancellation as a failure.
    const controller = new AbortController();
    apiFetchMock.mockImplementationOnce(() => {
      controller.abort();
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.reject(new DOMException('aborted', 'AbortError')),
      } as unknown as Response);
    });
    let caught: unknown;
    try {
      await fetchAllPaletteAgents(controller.signal);
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeInstanceOf(DOMException);
    expect((caught as DOMException).name).toBe('AbortError');
    expect(caught).not.toBeInstanceOf(PaletteLoadError);
  });

  it('a genuinely malformed body under a live (non-aborted) signal still becomes a PaletteLoadError', async () => {
    // The abort guard `if (signal?.aborted) { throw err; }` around the
    // `res.json()` catch must check `.aborted` specifically, not just
    // whether a signal was passed — the production caller (loadPaletteAgentsBounded)
    // *always* passes a signal, so weakening the guard to `if (signal)`
    // would turn every real non-JSON response into a raw `SyntaxError` for
    // any caller that happens to pass a signal, which is the only real
    // caller there is. This test drives a live, non-aborted signal
    // alongside a malformed body specifically to pin that distinction.
    const liveSignal = new AbortController().signal;
    expect(liveSignal.aborted).toBe(false);
    apiFetchMock.mockResolvedValueOnce(new Response('not json', { status: 200 }));
    await expect(fetchAllPaletteAgents(liveSignal)).rejects.toBeInstanceOf(PaletteLoadError);
    apiFetchMock.mockResolvedValueOnce(new Response('not json', { status: 200 }));
    await expect(fetchAllPaletteAgents(liveSignal)).rejects.toThrow(/not valid JSON/);
  });

  it('ignores a malformed (non-array) agents field instead of spreading it', async () => {
    // `if (Array.isArray(data.agents))` guards the spread — without it,
    // `data.agents ?? []` only catches null/undefined, not a wrong-shaped
    // value like a string, which would then be spread character-by-character
    // into the candidate list (strings are iterable). Covers the case where
    // the field is present but not an array.
    apiFetchMock.mockResolvedValueOnce(jsonResponse({ agents: 'not-an-array' }));
    const all = await fetchAllPaletteAgents();
    expect(all).toEqual([]);
  });

  it('throws PaletteLoadError on a literal null body instead of a raw TypeError', async () => {
    // A JSON body of `null` parses successfully, so it skips the "not valid
    // JSON" catch block entirely — without the object guard,
    // `Array.isArray(data.agents)` then reads `.agents` off a null receiver
    // and throws a raw TypeError.
    apiFetchMock.mockResolvedValueOnce(jsonResponse(null));
    await expect(fetchAllPaletteAgents()).rejects.toBeInstanceOf(PaletteLoadError);
    apiFetchMock.mockResolvedValueOnce(jsonResponse(null));
    await expect(fetchAllPaletteAgents()).rejects.not.toBeInstanceOf(TypeError);
  });

  it('throws PaletteLoadError on an array JSON body instead of silently treating it as zero agents', async () => {
    // A top-level JSON array parses successfully and is not null, so it
    // would pass a null-only guard — `Array.isArray(data.agents)` on an
    // array is false (arrays have no `.agents` property), so without the
    // stricter object guard this would silently resolve to an empty agents
    // list instead of surfacing the malformed response.
    apiFetchMock.mockResolvedValueOnce(jsonResponse([]));
    await expect(fetchAllPaletteAgents()).rejects.toBeInstanceOf(PaletteLoadError);
    apiFetchMock.mockResolvedValueOnce(jsonResponse([{ id: 'a0' }]));
    await expect(fetchAllPaletteAgents()).rejects.toBeInstanceOf(PaletteLoadError);
  });

  it('throws PaletteLoadError on a primitive JSON body instead of silently treating it as zero agents', async () => {
    // A bare JSON string or number parses successfully and is not null —
    // `typeof data !== 'object'` is what catches these, not the null check.
    // `'x'.agents`/`(42).agents` are just `undefined`, so without this guard
    // `Array.isArray(data.agents)` is false and this would silently resolve
    // to an empty agents list rather than surfacing the malformed response.
    apiFetchMock.mockResolvedValueOnce(jsonResponse('agents-unavailable'));
    await expect(fetchAllPaletteAgents()).rejects.toBeInstanceOf(PaletteLoadError);
    apiFetchMock.mockResolvedValueOnce(jsonResponse(42));
    await expect(fetchAllPaletteAgents()).rejects.toBeInstanceOf(PaletteLoadError);
  });

  it('follows nextCursor through the full 500-page safety bound and then throws, rather than looping forever', async () => {
    // Isolates the `pages < MAX_AGENT_PAGES` half of the while-loop
    // condition, and both halves of the trailing
    // `if (cursor && pages >= MAX_AGENT_PAGES)` check — a server that keeps
    // returning a new cursor forever (buggy or hostile) must not hang the
    // palette. Drives pagination up to the safety bound itself.
    for (let i = 0; i < 501; i++) {
      apiFetchMock.mockResolvedValueOnce(
        jsonResponse({ agents: [{ id: `a${i}` }], nextCursor: `cursor-${i}` })
      );
    }
    await expect(fetchAllPaletteAgents()).rejects.toThrow(PaletteLoadError);
    expect(apiFetchMock).toHaveBeenCalledTimes(500);
  });

  it('exactly 500 pages that terminate normally on the last one does not throw', async () => {
    // Isolates the `cursor` half of the trailing
    // `if (cursor && pages >= MAX_AGENT_PAGES)` check from the `pages >=
    // MAX_AGENT_PAGES` half (the previous test alone can't tell them apart,
    // since it never ends pagination with `pages` at exactly 500 — the
    // `pages >= MAX_AGENT_PAGES` half turns out to be implied by reaching
    // this check with `cursor` still truthy at all, since the loop's own
    // condition, `cursor && pages < MAX_AGENT_PAGES`, cannot otherwise exit
    // while `cursor` is truthy; confirmed redundant by mutation). A hub
    // that happens to have exactly 500 pages, with pagination legitimately
    // ending on the last one (`cursor` empty), must not be treated as
    // having hit the safety bound.
    for (let i = 0; i < 499; i++) {
      apiFetchMock.mockResolvedValueOnce(
        jsonResponse({ agents: [{ id: `a${i}` }], nextCursor: `cursor-${i}` })
      );
    }
    apiFetchMock.mockResolvedValueOnce(jsonResponse({ agents: [{ id: 'a499' }] })); // 500th page, no nextCursor
    const all = await fetchAllPaletteAgents();
    expect(all).toHaveLength(500);
    expect(apiFetchMock).toHaveBeenCalledTimes(500);
  });
});

describe('fetchPaletteDms', () => {
  it('returns the dms array', async () => {
    apiFetchMock.mockResolvedValueOnce(
      jsonResponse({ dms: [{ conversationKey: 'k', peerId: 'a0', peerKind: 'agent' }] })
    );
    const dms = await fetchPaletteDms();
    expect(dms).toHaveLength(1);
  });

  it('throws PaletteLoadError on failure', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse({}, 403));
    await expect(fetchPaletteDms()).rejects.toThrow(PaletteLoadError);
  });

  it('throws PaletteLoadError, not a raw SyntaxError, on a non-JSON body', async () => {
    apiFetchMock.mockResolvedValue(new Response('not json', { status: 200 }));
    await expect(fetchPaletteDms()).rejects.toThrow(PaletteLoadError);
    await expect(fetchPaletteDms()).rejects.toThrow(/not valid JSON/);
  });

  it('an abort landing during the body read rejects with the original AbortError, not PaletteLoadError', async () => {
    // See the matching fetchAllPaletteAgents test — the second of the two
    // abort-timing scenarios this guards against.
    const controller = new AbortController();
    apiFetchMock.mockImplementationOnce(() => {
      controller.abort();
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.reject(new DOMException('aborted', 'AbortError')),
      } as unknown as Response);
    });
    let caught: unknown;
    try {
      await fetchPaletteDms(controller.signal);
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeInstanceOf(DOMException);
    expect((caught as DOMException).name).toBe('AbortError');
    expect(caught).not.toBeInstanceOf(PaletteLoadError);
  });

  it('a genuinely malformed body under a live (non-aborted) signal still becomes a PaletteLoadError', async () => {
    // See the matching fetchAllPaletteAgents test above.
    const liveSignal = new AbortController().signal;
    expect(liveSignal.aborted).toBe(false);
    apiFetchMock.mockResolvedValueOnce(new Response('not json', { status: 200 }));
    await expect(fetchPaletteDms(liveSignal)).rejects.toBeInstanceOf(PaletteLoadError);
    apiFetchMock.mockResolvedValueOnce(new Response('not json', { status: 200 }));
    await expect(fetchPaletteDms(liveSignal)).rejects.toThrow(/not valid JSON/);
  });

  it('returns an empty list for a malformed (non-array) dms field instead of throwing or returning it as-is', () => {
    // Covers the false branch of `Array.isArray(data.dms) ? data.dms : []`.
    apiFetchMock.mockResolvedValueOnce(jsonResponse({ dms: 'not-an-array' }));
    return expect(fetchPaletteDms()).resolves.toEqual([]);
  });

  it('throws PaletteLoadError on a literal null body instead of a raw TypeError', async () => {
    // A JSON body of `null` parses successfully — without the object guard,
    // `Array.isArray(data.dms) ? data.dms : []` reads `.dms` off a null
    // receiver and throws a raw TypeError instead of yielding `[]`.
    apiFetchMock.mockResolvedValueOnce(jsonResponse(null));
    await expect(fetchPaletteDms()).rejects.toBeInstanceOf(PaletteLoadError);
    apiFetchMock.mockResolvedValueOnce(jsonResponse(null));
    await expect(fetchPaletteDms()).rejects.not.toBeInstanceOf(TypeError);
  });

  it('throws PaletteLoadError on an array JSON body instead of silently treating it as zero DMs', async () => {
    // A top-level JSON array is not null and `Array.isArray(data.dms)` on it
    // is false (arrays have no `.dms` property), so without the stricter
    // object guard this would silently resolve to `[]` instead of
    // surfacing the malformed response.
    apiFetchMock.mockResolvedValueOnce(jsonResponse([]));
    await expect(fetchPaletteDms()).rejects.toBeInstanceOf(PaletteLoadError);
    apiFetchMock.mockResolvedValueOnce(jsonResponse([{ conversationKey: 'k' }]));
    await expect(fetchPaletteDms()).rejects.toBeInstanceOf(PaletteLoadError);
  });

  it('throws PaletteLoadError on a primitive JSON body instead of silently treating it as zero DMs', async () => {
    // A bare JSON string or number parses successfully and is not null —
    // `typeof data !== 'object'` is what catches these, not the null check.
    // `'x'.dms`/`(42).dms` are just `undefined`, so without this guard
    // `Array.isArray(data.dms) ? data.dms : []` would silently resolve to
    // `[]` rather than surfacing the malformed response.
    apiFetchMock.mockResolvedValueOnce(jsonResponse('dms-unavailable'));
    await expect(fetchPaletteDms()).rejects.toBeInstanceOf(PaletteLoadError);
    apiFetchMock.mockResolvedValueOnce(jsonResponse(42));
    await expect(fetchPaletteDms()).rejects.toBeInstanceOf(PaletteLoadError);
  });
});

describe('isPaletteAgentViable: messageability and capability fallback', () => {
  it('honors _messageability.canMessage=true', () => {
    expect(
      isPaletteAgentViable({
        id: 'a',
        _messageability: { canMessage: true, canReachViewer: false },
      })
    ).toBe(true);
  });

  it('honors _messageability.canMessage=false even when capabilities allow management', () => {
    const agent: RawPaletteAgent = {
      id: 'a',
      _messageability: { canMessage: false, canReachViewer: true },
      _capabilities: { actions: ['lifecycle', 'attach'] },
    };
    expect(isPaletteAgentViable(agent)).toBe(false);
  });

  it('falls back to canMessageAgent(_capabilities) when _messageability is absent', () => {
    expect(isPaletteAgentViable({ id: 'a', _capabilities: { actions: ['attach'] } })).toBe(true);
    expect(isPaletteAgentViable({ id: 'a', _capabilities: { actions: ['read'] } })).toBe(false);
  });

  it('also falls back to canMessageAgent when _messageability is present but canMessage is not a boolean', () => {
    // `typeof messageability.canMessage === 'boolean'` half of the guard —
    // distinct from "_messageability is absent" above (a wholly missing
    // object): here the object exists but doesn't carry a canMessage field
    // at all, which `messageability.canMessage` would otherwise silently
    // return as `undefined` instead of falling through to the capability
    // fallback.
    expect(
      isPaletteAgentViable({
        id: 'a',
        _messageability: { canReachViewer: true } as unknown as { canMessage: boolean },
        _capabilities: { actions: ['attach'] },
      })
    ).toBe(true);
  });

  it('fails closed when both _messageability and _capabilities are missing', () => {
    expect(isPaletteAgentViable({ id: 'a' })).toBe(false);
  });

  it('canReachViewer is not a substitute for canMessage', () => {
    expect(
      isPaletteAgentViable({
        id: 'a',
        _messageability: { canMessage: false, canReachViewer: true },
      })
    ).toBe(false);
  });
});

describe('buildAgentCandidates: DM recency join, no membership dependency', () => {
  it('joins an existing DM lastActivityAt onto the matching agent', () => {
    const agents: RawPaletteAgent[] = [
      { id: 'a0', name: 'Coder', _capabilities: { actions: ['attach'] } },
    ];
    const dms: RawPaletteDm[] = [
      {
        conversationKey: 'dm:agent:a0:user:u1',
        peerId: 'a0',
        peerKind: 'agent',
        lastActivityAt: '2026-01-01T00:00:00Z',
      },
    ];
    const candidates = buildAgentCandidates(agents, dms);
    expect(candidates).toHaveLength(1);
    expect(candidates[0].activityMs).toBe(Date.parse('2026-01-01T00:00:00Z'));
    expect(candidates[0].target).toEqual({
      kind: 'dm',
      peerKind: 'agent',
      peerId: 'a0',
      displayName: 'Coder',
    });
  });

  it('a viable agent with no DM yet still appears, with activityMs=0', () => {
    const agents: RawPaletteAgent[] = [
      { id: 'a0', name: 'Coder', _capabilities: { actions: ['attach'] } },
    ];
    const candidates = buildAgentCandidates(agents, []);
    expect(candidates).toHaveLength(1);
    expect(candidates[0].activityMs).toBe(0);
  });

  it('excludes a non-viable agent even if it has an existing DM', () => {
    const agents: RawPaletteAgent[] = [
      { id: 'a0', name: 'Coder', _capabilities: { actions: ['read'] } },
    ];
    const dms: RawPaletteDm[] = [
      {
        conversationKey: 'k',
        peerId: 'a0',
        peerKind: 'agent',
        lastActivityAt: '2026-01-01T00:00:00Z',
      },
    ];
    expect(buildAgentCandidates(agents, dms)).toHaveLength(0);
  });

  it('does not filter agents by phase — only messageability decides viability', () => {
    const agents: RawPaletteAgent[] = [
      {
        id: 'a0',
        name: 'Stopped Bot',
        _messageability: { canMessage: true, canReachViewer: false },
      },
    ];
    expect(buildAgentCandidates(agents, [])).toHaveLength(1);
  });

  it('produces a stable JSON-tuple candidate ID', () => {
    const candidates = buildAgentCandidates(
      [{ id: 'a0', name: 'Coder', _capabilities: { actions: ['attach'] } }],
      []
    );
    expect(candidates[0].id).toBe('["dm","agent","a0"]');
  });

  it('includes the slug as an additional search field when present', () => {
    const candidates = buildAgentCandidates(
      [{ id: 'a0', name: 'Coder One', slug: 'coder-one', _capabilities: { actions: ['attach'] } }],
      []
    );
    expect(candidates[0].searchFields).toEqual(['Coder One', 'coder-one']);
  });

  it('does not add a duplicate search field when the slug equals the display name', () => {
    // `agent.slug !== displayName` half of the extra-searchField condition
    // — without it, an agent whose slug happens to equal its display name
    // (e.g. no separate human-readable name was ever set) would get the
    // same string pushed into searchFields twice.
    const candidates = buildAgentCandidates(
      [{ id: 'a0', name: 'coder-one', slug: 'coder-one', _capabilities: { actions: ['attach'] } }],
      []
    );
    expect(candidates[0].searchFields).toEqual(['coder-one']);
  });

  it('does not push an undefined search field when slug is absent', () => {
    // `agent.slug` truthy half of the same condition — every other test
    // either supplies a slug or never inspects searchFields' exact
    // contents when it's absent.
    const candidates = buildAgentCandidates(
      [{ id: 'a0', name: 'Coder One', _capabilities: { actions: ['attach'] } }],
      []
    );
    expect(candidates[0].searchFields).toEqual(['Coder One']);
  });

  it('falls back to slug for the display name when name is absent', () => {
    // Covers the `agent.name || agent.slug || agent.id` fallback chain's
    // middle link: `name` absent, `slug` present.
    const candidates = buildAgentCandidates(
      [{ id: 'a0', slug: 'coder-one', _capabilities: { actions: ['attach'] } }],
      []
    );
    expect(candidates[0].label).toBe('coder-one');
  });

  it('an agent with a falsy id is skipped, not turned into a candidate with an empty id', () => {
    const candidates = buildAgentCandidates(
      [{ id: '', name: 'Ghost', _capabilities: { actions: ['attach'] } }],
      []
    );
    expect(candidates).toHaveLength(0);
  });

  it('a DM from a non-agent peer (peerKind !== "agent") is never joined onto an agent, even with a matching peerId', () => {
    // `dm.peerKind === 'agent'` half of the DM-index filter — without it, a
    // user-to-user DM whose peerId happens to collide with an agent's id
    // would incorrectly supply that agent's recency.
    const agents: RawPaletteAgent[] = [
      { id: 'a0', name: 'Coder', _capabilities: { actions: ['attach'] } },
    ];
    const dms: RawPaletteDm[] = [
      {
        conversationKey: 'k',
        peerId: 'a0',
        peerKind: 'user',
        lastActivityAt: '2026-01-01T00:00:00Z',
      },
    ];
    const candidates = buildAgentCandidates(agents, dms);
    expect(candidates[0].activityMs).toBe(0);
  });

  it('a DM with a falsy peerId is never indexed', () => {
    const agents: RawPaletteAgent[] = [
      { id: 'a0', name: 'Coder', _capabilities: { actions: ['attach'] } },
    ];
    const dms: RawPaletteDm[] = [
      {
        conversationKey: 'k',
        peerId: '',
        peerKind: 'agent',
        lastActivityAt: '2026-01-01T00:00:00Z',
      },
    ];
    expect(() => buildAgentCandidates(agents, dms)).not.toThrow();
    expect(buildAgentCandidates(agents, dms)[0].activityMs).toBe(0);
  });
});

interface PendingEnsure {
  signal: AbortSignal | undefined;
  onProgress: ((snapshot: AgentListSnapshot) => void) | undefined;
  resolve: (snapshot: AgentListSnapshot) => void;
  reject: (err: unknown) => void;
}

function snapshot(
  agents: RawPaletteAgent[],
  status: AgentListSnapshot['status'] = 'ready'
): AgentListSnapshot {
  return {
    key: 'hub',
    agents: agents as unknown as Agent[],
    status,
    complete: status === 'ready',
    version: 1,
  };
}

/**
 * Stands in for the agent store: each `ensure` stays pending until the test
 * settles it, and rejects with an AbortError when its caller's signal
 * aborts, as the store does.
 */
function fakeAgentSource(): {
  source: PaletteAgentSource;
  pending: PendingEnsure[];
  latest: { value: AgentListSnapshot | undefined };
} {
  const pending: PendingEnsure[] = [];
  const latest: { value: AgentListSnapshot | undefined } = { value: undefined };
  const source: PaletteAgentSource = {
    ensure: (_q, opts = {}) =>
      new Promise<AgentListSnapshot>((resolve, reject) => {
        opts.signal?.addEventListener('abort', () =>
          reject(new DOMException('aborted', 'AbortError'))
        );
        pending.push({ signal: opts.signal, onProgress: opts.onProgress, resolve, reject });
      }),
    peek: () => latest.value,
  };
  return { source, pending, latest };
}

const CODER = { id: 'a0', name: 'Coder', _capabilities: { actions: ['attach'] } };
const SECOND = { id: 'a1', name: 'Second', _capabilities: { actions: ['attach'] } };

describe('ChatPaletteDataController: Agents group from the agent store', () => {
  it('joins the store hub snapshot with the DM list and requests no agent list itself', async () => {
    apiFetchMock.mockResolvedValueOnce(
      jsonResponse({
        dms: [
          {
            conversationKey: 'k',
            peerId: 'a0',
            peerKind: 'agent',
            lastActivityAt: '2026-01-01T00:00:00Z',
          },
        ],
      })
    );
    const { source, pending } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    const load = controller.loadAgentsGroup();
    pending[0]?.resolve(snapshot([CODER]));

    const candidates = await load;
    expect(candidates.map((c) => c.label)).toEqual(['Coder']);
    expect(candidates[0]?.activityMs).toBeGreaterThan(0);
    expect(apiFetchMock.mock.calls.map((c) => c[0])).toEqual(['/api/v1/chat/dms']);
  });

  it('asks the store for the hub entry with its own abort signal', async () => {
    const ensure = vi.fn<PaletteAgentSource['ensure']>(() => new Promise(() => {}));
    const controller = new ChatPaletteDataController({ ensure, peek: () => undefined });
    void controller.loadAgentsGroup();

    expect(ensure).toHaveBeenCalledWith(
      { scope: 'hub' },
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    );
  });

  it('builds the result from the latest ready snapshot when the store changed during the DM fetch', async () => {
    let resolveDms!: (v: Response) => void;
    apiFetchMock.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveDms = resolve;
        })
    );
    const { source, pending, latest } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    const load = controller.loadAgentsGroup();
    pending[0]?.resolve(snapshot([CODER]));
    await vi.waitFor(() => expect(apiFetchMock).toHaveBeenCalledTimes(1));

    latest.value = snapshot([CODER, SECOND]);
    resolveDms(jsonResponse({ dms: [] }));

    expect((await load).map((c) => c.label)).toEqual(['Coder', 'Second']);
  });

  it('rejects with the store error when the agent list fails', async () => {
    const { source, pending } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    const load = controller.loadAgentsGroup();
    pending[0]?.reject(new Error('agents list request failed: 500'));

    await expect(load).rejects.toThrow('agents list request failed: 500');
    expect(apiFetchMock).not.toHaveBeenCalled();
  });

  it('a superseded load rejects with an AbortError rather than resolving stale data', async () => {
    apiFetchMock.mockResolvedValue(jsonResponse({ dms: [] }));
    const { source, pending } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    const firstLoad = controller.loadAgentsGroup();
    const secondLoad = controller.loadAgentsGroup();
    pending[1]?.resolve(snapshot([SECOND]));

    await expect(firstLoad).rejects.toMatchObject({ name: 'AbortError' });
    await expect(secondLoad).resolves.toMatchObject([expect.objectContaining({ label: 'Second' })]);
  });

  it('a supersede that arrives during the DM fetch also rejects with an AbortError', async () => {
    let resolveFirstDms!: (v: Response) => void;
    apiFetchMock
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveFirstDms = resolve;
          })
      )
      .mockResolvedValueOnce(jsonResponse({ dms: [] }));
    const { source, pending } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    const firstLoad = controller.loadAgentsGroup();
    pending[0]?.resolve(snapshot([CODER]));
    await vi.waitFor(() => expect(apiFetchMock).toHaveBeenCalledTimes(1));

    const secondLoad = controller.loadAgentsGroup();
    resolveFirstDms(jsonResponse({ dms: [] }));
    pending[1]?.resolve(snapshot([SECOND]));

    await expect(firstLoad).rejects.toMatchObject({ name: 'AbortError' });
    await expect(secondLoad).resolves.toMatchObject([expect.objectContaining({ label: 'Second' })]);
  });

  it('cancel() aborts the store request and the load rejects with an AbortError', async () => {
    const { source, pending } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    const load = controller.loadAgentsGroup();
    controller.cancel();

    await expect(load).rejects.toMatchObject({ name: 'AbortError' });
    expect(pending[0]?.signal?.aborted).toBe(true);
  });

  it("starting a new load aborts the previous load's signal", async () => {
    const { source, pending } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    controller.loadAgentsGroup().catch(() => {});
    expect(pending[0]?.signal?.aborted).toBe(false);

    controller.loadAgentsGroup().catch(() => {});
    expect(pending[0]?.signal?.aborted).toBe(true);
  });

  it('a cancelled load does not publish a real error even when the DM request fails after cancel', async () => {
    let resolveDms!: (v: Response) => void;
    apiFetchMock.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveDms = resolve;
        })
    );
    const { source, pending } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    const load = controller.loadAgentsGroup();
    pending[0]?.resolve(snapshot([CODER]));
    await vi.waitFor(() => expect(apiFetchMock).toHaveBeenCalledTimes(1));
    controller.cancel();
    resolveDms(jsonResponse({}, 403));

    await expect(load).rejects.toMatchObject({ name: 'AbortError' });
  });

  it('a DM-list failure fails the whole group rather than silently degrading every agent to activityMs=0', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse({}, 500));
    const { source, pending } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    const load = controller.loadAgentsGroup();
    pending[0]?.resolve(snapshot([CODER]));

    await expect(load).rejects.toThrow(PaletteLoadError);
  });

  it('a DM fetch that hangs is aborted after the idle bound and surfaces a PaletteLoadError', async () => {
    vi.useFakeTimers();
    let dmAborted = false;
    apiFetchMock.mockImplementationOnce((_url, options) => {
      return new Promise((_resolve, reject) => {
        options?.signal?.addEventListener('abort', () => {
          dmAborted = true;
          reject(new DOMException('aborted', 'AbortError'));
        });
      });
    });
    const { source, pending } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    const load = controller.loadAgentsGroup();
    load.catch(() => {});
    pending[0]?.resolve(snapshot([CODER]));

    await vi.advanceTimersByTimeAsync(89_999);
    expect(dmAborted).toBe(false);

    const expectation = expect(load).rejects.toBeInstanceOf(PaletteLoadError);
    await vi.advanceTimersByTimeAsync(2);
    expect(dmAborted).toBe(true);
    await expectation;
    expect(vi.getTimerCount()).toBe(0);
  });

  it('does not bound the agent list itself: a store walk longer than the idle bound still succeeds', async () => {
    vi.useFakeTimers();
    apiFetchMock.mockResolvedValueOnce(jsonResponse({ dms: [] }));
    const { source, pending } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    const load = controller.loadAgentsGroup();

    await vi.advanceTimersByTimeAsync(120_000);
    expect(pending[0]?.signal?.aborted).toBe(false);
    pending[0]?.resolve(snapshot([CODER]));

    expect(await load).toHaveLength(1);
    expect(vi.getTimerCount()).toBe(0);
  });
});

describe('ChatPaletteDataController: Agents group progressive onProgress', () => {
  it('publishes each store progress snapshot ahead of the final DM-joined result', async () => {
    apiFetchMock.mockResolvedValueOnce(
      jsonResponse({
        dms: [
          {
            conversationKey: 'k',
            peerId: 'a1',
            peerKind: 'agent',
            lastActivityAt: '2026-01-01T00:00:00Z',
          },
        ],
      })
    );
    const { source, pending } = fakeAgentSource();
    const progressCalls: Array<ReadonlyArray<{ label: string; activityMs: number }>> = [];
    const controller = new ChatPaletteDataController(source);
    const load = controller.loadAgentsGroup((partial) => {
      progressCalls.push(partial.map((c) => ({ label: c.label, activityMs: c.activityMs })));
    });

    pending[0]?.onProgress?.(snapshot([CODER], 'loading'));
    pending[0]?.onProgress?.(snapshot([CODER, SECOND], 'loading'));
    pending[0]?.resolve(snapshot([CODER, SECOND]));
    const candidates = await load;

    expect(progressCalls).toEqual([
      [{ label: 'Coder', activityMs: 0 }],
      [
        { label: 'Coder', activityMs: 0 },
        { label: 'Second', activityMs: 0 },
      ],
    ]);
    expect(candidates.find((c) => c.label === 'Second')?.activityMs).toBeGreaterThan(0);
  });

  it('a superseded load never publishes progress after the newer load starts', async () => {
    apiFetchMock.mockResolvedValue(jsonResponse({ dms: [] }));
    const { source, pending } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    const staleProgress: unknown[] = [];
    const firstLoad = controller.loadAgentsGroup((partial) => staleProgress.push(partial));
    const secondLoad = controller.loadAgentsGroup();

    pending[0]?.onProgress?.(snapshot([CODER], 'loading'));
    pending[1]?.resolve(snapshot([SECOND]));

    await expect(firstLoad).rejects.toMatchObject({ name: 'AbortError' });
    await secondLoad;
    expect(staleProgress).toEqual([]);
  });
});

describe('ChatPaletteDataController.deriveAgentCandidates', () => {
  it('returns null before any Agents load has succeeded', () => {
    const { source } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    expect(controller.deriveAgentCandidates(snapshot([CODER]))).toBeNull();
  });

  it('rebuilds candidates from a new snapshot with the last load’s DM recency', async () => {
    apiFetchMock.mockResolvedValueOnce(
      jsonResponse({
        dms: [
          {
            conversationKey: 'k',
            peerId: 'a1',
            peerKind: 'agent',
            lastActivityAt: '2026-01-01T00:00:00Z',
          },
        ],
      })
    );
    const { source, pending } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    const load = controller.loadAgentsGroup();
    pending[0]?.resolve(snapshot([CODER]));
    await load;

    const derived = controller.deriveAgentCandidates(snapshot([CODER, SECOND]));
    expect(derived?.map((c) => c.label)).toEqual(['Coder', 'Second']);
    expect(derived?.find((c) => c.label === 'Second')?.activityMs).toBeGreaterThan(0);
    expect(apiFetchMock).toHaveBeenCalledTimes(1);
  });

  it('returns null while an Agents load is in flight', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse({ dms: [] }));
    const { source, pending } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    const load = controller.loadAgentsGroup();
    pending[0]?.resolve(snapshot([CODER]));
    await load;

    controller.loadAgentsGroup().catch(() => {});
    expect(controller.deriveAgentCandidates(snapshot([CODER]))).toBeNull();
  });
});

describe('ChatPaletteDataController over a real agent store', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('a second open answers from the store, an SSE status change re-derives without a request', async () => {
    vi.useFakeTimers();
    const viable = { _capabilities: { actions: ['attach'] } } as Partial<Agent>;
    const h = createHarness([
      harnessAgent('a0', viable),
      harnessAgent('a1', { ...viable, name: 'Second' }),
    ]);
    apiFetchMock.mockImplementation(() => Promise.resolve(jsonResponse({ dms: [] })));
    const controller = new ChatPaletteDataController(h.store);

    const first = controller.loadAgentsGroup();
    await h.connect();
    expect(await first).toHaveLength(2);
    expect(await controller.loadAgentsGroup()).toHaveLength(2);
    expect(h.server.walks()).toBe(1);

    let latest: AgentListSnapshot | undefined;
    const release = h.store.retain({ scope: 'hub' }, (s) => {
      latest = s;
    });
    await h.emitAgent('status', { agentId: 'a1', phase: 'stopped' });
    expect(latest?.agents.find((a) => a.id === 'a1')?.phase).toBe('stopped');
    expect(controller.deriveAgentCandidates(latest!)).toHaveLength(2);
    expect(h.server.requests).toHaveLength(1);
    expect(apiFetchMock.mock.calls.map((c) => c[0])).toEqual(['/api/v1/chat/dms']);
    release();
    h.store.destroy();
  });

  it('re-reads DMs after a stale mark or once the DM cache expires', async () => {
    vi.useFakeTimers();
    const h = createHarness([harnessAgent('a0', { _capabilities: { actions: ['attach'] } })]);
    apiFetchMock.mockImplementation(() => Promise.resolve(jsonResponse({ dms: [] })));
    const controller = new ChatPaletteDataController(h.store);
    const dmFetches = (): number => apiFetchMock.mock.calls.length;

    const first = controller.loadAgentsGroup();
    await h.connect();
    await first;
    expect(dmFetches()).toBe(1);
    expect(controller.peekAgentsGroup()).toHaveLength(1);

    controller.markAgentDmsStale();
    expect(controller.peekAgentsGroup()).toBeNull();
    await controller.loadAgentsGroup();
    expect(dmFetches()).toBe(2);

    vi.advanceTimersByTime(AGENT_DMS_CACHE_MS - 1);
    await controller.loadAgentsGroup();
    expect(dmFetches()).toBe(2);
    vi.advanceTimersByTime(1);
    expect(controller.peekAgentsGroup()).toBeNull();
    await controller.loadAgentsGroup();
    expect(dmFetches()).toBe(3);
    h.store.destroy();
  });

  it('a stale mark during the DM fetch is not lost', async () => {
    vi.useFakeTimers();
    const h = createHarness([harnessAgent('a0', { _capabilities: { actions: ['attach'] } })]);
    let resolveDms: (r: Response) => void = () => {};
    apiFetchMock.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveDms = resolve;
        })
    );
    apiFetchMock.mockImplementation(() => Promise.resolve(jsonResponse({ dms: [] })));
    const controller = new ChatPaletteDataController(h.store);

    const first = controller.loadAgentsGroup();
    await h.connect();
    controller.markAgentDmsStale();
    resolveDms(jsonResponse({ dms: [] }));
    await first;
    expect(controller.peekAgentsGroup()).toBeNull();
    await controller.loadAgentsGroup();
    expect(apiFetchMock.mock.calls.length).toBe(2);
    h.store.destroy();
  });

  it('an agent created over SSE becomes a candidate with the capabilities the hub returns for it', async () => {
    vi.useFakeTimers();
    const h = createHarness([harnessAgent('a0', { _capabilities: { actions: ['attach'] } })]);
    apiFetchMock.mockImplementation(() => Promise.resolve(jsonResponse({ dms: [] })));
    const controller = new ChatPaletteDataController(h.store);
    const first = controller.loadAgentsGroup();
    await h.connect();
    await first;

    let latest: AgentListSnapshot | undefined;
    const release = h.store.retain({ scope: 'hub' }, (s) => {
      latest = s;
    });
    h.server.agents.push(
      harnessAgent('a1', { name: 'Created', _capabilities: { actions: ['attach'] } })
    );
    // The hub's created payload carries no capabilities or messageability.
    await h.emitAgent('created', {
      agentId: 'a1',
      projectId: 'p1',
      name: 'Created',
      slug: 'a1',
      phase: 'running',
    });
    await vi.waitFor(() => expect(h.server.agentFetches('a1')).toBe(1));
    await vi.waitFor(() =>
      expect((controller.deriveAgentCandidates(latest!) ?? []).map((c) => c.label)).toContain(
        'Created'
      )
    );
    expect(h.server.walks()).toBe(1);
    release();
    h.store.destroy();
  });
});

describe('loadPaletteAgentsBounded: idle timeout', () => {
  function boundedLoad(): { controller: AbortController; load: Promise<unknown[]> } {
    const controller = new AbortController();
    const load = loadPaletteAgentsBounded({
      controller,
      isCurrent: () => true,
      finish: async (agents, signal) => buildAgentCandidates(agents, await fetchPaletteDms(signal)),
    });
    return { controller, load };
  }

  function hanging(onAbort?: () => void) {
    return (_url: string, options?: { signal?: AbortSignal | null }): Promise<Response> =>
      new Promise((_resolve, reject) => {
        options?.signal?.addEventListener('abort', () => {
          onAbort?.();
          reject(new DOMException('aborted', 'AbortError'));
        });
      });
  }

  it('a request that never settles is aborted after the idle bound and surfaces a retryable PaletteLoadError, not a silent AbortError', async () => {
    vi.useFakeTimers();
    apiFetchMock.mockImplementationOnce(hanging());
    const { load } = boundedLoad();
    const expectation = expect(load).rejects.toBeInstanceOf(PaletteLoadError);
    await vi.advanceTimersByTimeAsync(90_000 + 1);
    await expectation;
  });

  it('does not trip 1ms before the idle bound, but does 1ms after', async () => {
    vi.useFakeTimers();
    let aborted = false;
    apiFetchMock.mockImplementationOnce(hanging(() => (aborted = true)));
    const { load } = boundedLoad();
    load.catch(() => {});

    await vi.advanceTimersByTimeAsync(89_999);
    expect(aborted).toBe(false);

    const expectation = expect(load).rejects.toBeInstanceOf(PaletteLoadError);
    await vi.advanceTimersByTimeAsync(2);
    expect(aborted).toBe(true);
    await expectation;
  });

  it('a load whose total duration exceeds the idle bound still succeeds, as long as each step arrives inside its own window', async () => {
    vi.useFakeTimers();
    let resolvePage1!: (v: Response) => void;
    let resolvePage2!: (v: Response) => void;
    let resolvePage3!: (v: Response) => void;
    let resolveDms!: (v: Response) => void;
    const abortable = (resolve: (fn: (v: Response) => void) => void) => {
      return (_url: string, options?: { signal?: AbortSignal | null }) =>
        new Promise<Response>((res, reject) => {
          resolve(res);
          options?.signal?.addEventListener('abort', () =>
            reject(new DOMException('aborted', 'AbortError'))
          );
        });
    };
    apiFetchMock
      .mockImplementationOnce(abortable((r) => (resolvePage1 = r)))
      .mockImplementationOnce(abortable((r) => (resolvePage2 = r)))
      .mockImplementationOnce(abortable((r) => (resolvePage3 = r)))
      .mockImplementationOnce(abortable((r) => (resolveDms = r)));

    const { load } = boundedLoad();
    await vi.advanceTimersByTimeAsync(30_000);
    resolvePage1(jsonResponse({ agents: [CODER], nextCursor: 'c1' }));
    await vi.advanceTimersByTimeAsync(30_000);
    resolvePage2(jsonResponse({ agents: [SECOND], nextCursor: 'c2' }));
    await vi.advanceTimersByTimeAsync(30_000);
    resolvePage3(jsonResponse({ agents: [] }));
    await vi.advanceTimersByTimeAsync(30_000);
    resolveDms(jsonResponse({ dms: [] }));

    expect(await load).toHaveLength(2);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('clears its idle timer once a load settles successfully', async () => {
    vi.useFakeTimers();
    apiFetchMock
      .mockResolvedValueOnce(jsonResponse({ agents: [CODER] }))
      .mockResolvedValueOnce(jsonResponse({ dms: [] }));
    const { load } = boundedLoad();
    expect(await load).toHaveLength(1);
    expect(vi.getTimerCount()).toBe(0);
  });

  it('clears its idle timer when the load is cancelled', async () => {
    vi.useFakeTimers();
    apiFetchMock.mockImplementationOnce(hanging());
    const { controller, load } = boundedLoad();
    const expectation = expect(load).rejects.toMatchObject({ name: 'AbortError' });
    controller.abort();
    await expectation;
    expect(vi.getTimerCount()).toBe(0);
  });

  it('a DM fetch that hangs after every agents page lands is still aborted by the idle bound', async () => {
    vi.useFakeTimers();
    let dmAborted = false;
    apiFetchMock
      .mockResolvedValueOnce(jsonResponse({ agents: [CODER] }))
      .mockImplementationOnce(hanging(() => (dmAborted = true)));
    const { load } = boundedLoad();
    load.catch(() => {});

    await vi.advanceTimersByTimeAsync(89_999);
    expect(dmAborted).toBe(false);

    const expectation = expect(load).rejects.toBeInstanceOf(PaletteLoadError);
    await vi.advanceTimersByTimeAsync(2);
    expect(dmAborted).toBe(true);
    await expectation;
  });
});

// ===========================================================================
// People group
// ===========================================================================

describe('fetchAllPaletteUsers: full pagination', () => {
  it('traverses more than 100 entries across pages', async () => {
    const page1 = Array.from({ length: 100 }, (_, i) => ({ id: `u${i}` }));
    const page2 = Array.from({ length: 30 }, (_, i) => ({ id: `v${i}` }));
    apiFetchMock
      .mockResolvedValueOnce(jsonResponse({ users: page1, nextCursor: 'cursor-1' }))
      .mockResolvedValueOnce(jsonResponse({ users: page2 }));

    const all = await fetchAllPaletteUsers();

    expect(all).toHaveLength(130);
    expect(apiFetchMock).toHaveBeenCalledTimes(2);
    expect(apiFetchMock.mock.calls[1][0]).toContain('cursor=cursor-1');
  });

  it('continues past a filtered empty intermediate page that still carries a cursor', async () => {
    apiFetchMock
      .mockResolvedValueOnce(jsonResponse({ users: [{ id: 'u0' }], nextCursor: 'cursor-1' }))
      .mockResolvedValueOnce(jsonResponse({ users: [], nextCursor: 'cursor-2' }))
      .mockResolvedValueOnce(jsonResponse({ users: [{ id: 'u1' }] }));

    const all = await fetchAllPaletteUsers();

    expect(all.map((u) => u.id)).toEqual(['u0', 'u1']);
    expect(apiFetchMock).toHaveBeenCalledTimes(3);
  });

  it('stops when nextCursor is absent even if totalCount implied more', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse({ users: [{ id: 'u0' }] }));
    const all = await fetchAllPaletteUsers();
    expect(all).toHaveLength(1);
    expect(apiFetchMock).toHaveBeenCalledTimes(1);
  });

  it('treats a repeated cursor as a load error, not an infinite loop', async () => {
    apiFetchMock
      .mockResolvedValueOnce(jsonResponse({ users: [{ id: 'u0' }], nextCursor: 'loop' }))
      .mockResolvedValueOnce(jsonResponse({ users: [{ id: 'u1' }], nextCursor: 'loop' }));

    await expect(fetchAllPaletteUsers()).rejects.toThrow(PaletteLoadError);
    expect(apiFetchMock).toHaveBeenCalledTimes(2);
  });

  it('follows nextCursor through the full 500-page safety bound and then throws, rather than looping forever', async () => {
    // A server that keeps returning a new (never-repeated) cursor forever —
    // buggy or hostile — must not hang the palette on an unbounded loop.
    for (let i = 0; i < 501; i++) {
      apiFetchMock.mockResolvedValueOnce(
        jsonResponse({ users: [{ id: `u${i}` }], nextCursor: `cursor-${i}` })
      );
    }
    await expect(fetchAllPaletteUsers()).rejects.toThrow(PaletteLoadError);
    expect(apiFetchMock).toHaveBeenCalledTimes(500);
  });

  it('exactly 500 pages that terminate normally on the last one does not throw', async () => {
    // A server with exactly 500 pages, with pagination legitimately ending
    // on the last one (no further cursor), must not be treated as having
    // hit the safety bound.
    for (let i = 0; i < 499; i++) {
      apiFetchMock.mockResolvedValueOnce(
        jsonResponse({ users: [{ id: `u${i}` }], nextCursor: `cursor-${i}` })
      );
    }
    apiFetchMock.mockResolvedValueOnce(jsonResponse({ users: [{ id: 'u499' }] })); // 500th page, no nextCursor
    const all = await fetchAllPaletteUsers();
    expect(all).toHaveLength(500);
    expect(apiFetchMock).toHaveBeenCalledTimes(500);
  });

  it('throws PaletteLoadError on a non-ok response', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse({}, 500));
    await expect(fetchAllPaletteUsers()).rejects.toThrow(PaletteLoadError);
  });

  it('an abort landing during the body read rejects with the original AbortError, not PaletteLoadError', async () => {
    const controller = new AbortController();
    apiFetchMock.mockImplementationOnce(() => {
      controller.abort();
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.reject(new DOMException('aborted', 'AbortError')),
      } as unknown as Response);
    });
    await expect(fetchAllPaletteUsers(controller.signal)).rejects.toMatchObject({
      name: 'AbortError',
    });
  });

  it('a genuinely malformed body under a live (non-aborted) signal still becomes a PaletteLoadError', async () => {
    const controller = new AbortController();
    apiFetchMock.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.reject(new SyntaxError('bad json')),
    } as unknown as Response);
    await expect(fetchAllPaletteUsers(controller.signal)).rejects.toBeInstanceOf(PaletteLoadError);
  });

  it('ignores a malformed (non-array) users field instead of spreading it', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse({ users: 'not-an-array' }));
    const all = await fetchAllPaletteUsers();
    expect(all).toEqual([]);
  });

  it('throws PaletteLoadError on a literal null body instead of a raw TypeError', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse(null));
    await expect(fetchAllPaletteUsers()).rejects.toBeInstanceOf(PaletteLoadError);
  });

  it('throws PaletteLoadError on an array body instead of silently returning an empty page', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse([1, 2, 3]));
    await expect(fetchAllPaletteUsers()).rejects.toBeInstanceOf(PaletteLoadError);
  });

  it('throws PaletteLoadError on a primitive (e.g. number) body instead of silently returning an empty page', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse(42));
    await expect(fetchAllPaletteUsers()).rejects.toBeInstanceOf(PaletteLoadError);
  });
});

describe('isPaletteUserViable: status exclusion', () => {
  it('active is viable', () => {
    expect(isPaletteUserViable({ id: 'u', status: 'active' })).toBe(true);
  });
  it('an absent status is viable (existing callers such as loadHubMembers treat this as active)', () => {
    expect(isPaletteUserViable({ id: 'u' })).toBe(true);
  });
  it('an empty-string status is viable', () => {
    expect(isPaletteUserViable({ id: 'u', status: '' })).toBe(true);
  });
  it('suspended is not viable', () => {
    expect(isPaletteUserViable({ id: 'u', status: 'suspended' })).toBe(false);
  });
  it('invited is not viable', () => {
    expect(isPaletteUserViable({ id: 'u', status: 'invited' })).toBe(false);
  });
  it("a hypothetical future 'disabled' status is not viable, by the same not-'active' rule as everything else (not a real backend value today, and not a separate literal check)", () => {
    expect(isPaletteUserViable({ id: 'u', status: 'disabled' })).toBe(false);
  });
  it('an unrecognized future status is not viable (fails closed)', () => {
    expect(isPaletteUserViable({ id: 'u', status: 'something-new' })).toBe(false);
  });
});

describe('buildUserCandidates: DM recency join, self/disabled exclusion', () => {
  const SELF_ID = 'self-user';

  function user(overrides: Partial<RawPaletteUser> & { id: string }): RawPaletteUser {
    return { displayName: overrides.id, ...overrides };
  }

  it('joins an existing DM lastActivityAt onto the matching user', () => {
    const users = [user({ id: 'u0', displayName: 'Alice' })];
    const dms: RawPaletteDm[] = [
      {
        conversationKey: 'dm:user:a:user:b',
        peerId: 'u0',
        peerKind: 'user',
        lastActivityAt: '2026-09-28T12:00:00Z',
      },
    ];
    const [candidate] = buildUserCandidates(users, dms, SELF_ID);
    expect(candidate.activityMs).toBe(Date.parse('2026-09-28T12:00:00Z'));
    expect(candidate.group).toBe('people');
  });

  it('a viable user with no DM yet still appears, with activityMs=0', () => {
    const users = [user({ id: 'u0', displayName: 'Alice' })];
    const [candidate] = buildUserCandidates(users, [], SELF_ID);
    expect(candidate.activityMs).toBe(0);
    expect(candidate.target).toEqual({
      kind: 'dm',
      peerKind: 'user',
      peerId: 'u0',
      displayName: 'Alice',
    });
  });

  it('excludes the current user (self)', () => {
    const users = [
      user({ id: SELF_ID, displayName: 'Me' }),
      user({ id: 'u0', displayName: 'Alice' }),
    ];
    const candidates = buildUserCandidates(users, [], SELF_ID);
    expect(candidates.map((c) => c.target)).toEqual([
      { kind: 'dm', peerKind: 'user', peerId: 'u0', displayName: 'Alice' },
    ]);
  });

  it("excludes a status=disabled user (a hypothetical future value, excluded by the same not-'active' rule, not a separate literal check)", () => {
    const users = [
      user({ id: 'u0', displayName: 'Alice', status: 'disabled' }),
      user({ id: 'u1', displayName: 'Bob' }),
    ];
    const candidates = buildUserCandidates(users, [], SELF_ID);
    expect(candidates.map((c) => c.label)).toEqual(['Bob']);
  });

  it('excludes a status=suspended user (suspended counts as disabled)', () => {
    const users = [
      user({ id: 'u0', displayName: 'Alice', status: 'suspended' }),
      user({ id: 'u1', displayName: 'Bob' }),
    ];
    const candidates = buildUserCandidates(users, [], SELF_ID);
    expect(candidates.map((c) => c.label)).toEqual(['Bob']);
  });

  it('excludes a status=invited user', () => {
    const users = [
      user({ id: 'u0', displayName: 'Alice', status: 'invited' }),
      user({ id: 'u1', displayName: 'Bob' }),
    ];
    const candidates = buildUserCandidates(users, [], SELF_ID);
    expect(candidates.map((c) => c.label)).toEqual(['Bob']);
  });

  it('includes a status=active user', () => {
    const users = [user({ id: 'u0', displayName: 'Alice', status: 'active' })];
    expect(buildUserCandidates(users, [], SELF_ID).map((c) => c.label)).toEqual(['Alice']);
  });

  it('includes a user with an absent/empty status (the field is optional on the real response shape)', () => {
    const users = [user({ id: 'u0', displayName: 'Alice' })];
    expect(buildUserCandidates(users, [], SELF_ID).map((c) => c.label)).toEqual(['Alice']);
  });

  it('a DM whose peer is missing from the authorized user list is never resurrected as a candidate', () => {
    // Only iterates `users`, never `dms` — a denied/disabled/deleted peer's DM
    // entry must not produce a candidate of its own.
    const users = [user({ id: 'u0', displayName: 'Alice' })];
    const dms: RawPaletteDm[] = [
      { conversationKey: 'dm:user:x:user:y', peerId: 'gone-user', peerKind: 'user' },
    ];
    const candidates = buildUserCandidates(users, dms, SELF_ID);
    expect(candidates).toHaveLength(1);
    expect(candidates[0].label).toBe('Alice');
  });

  it('a DM from a non-user peer (peerKind !== "user") is never joined onto a person, even with a matching peerId', () => {
    const users = [user({ id: 'shared-id', displayName: 'Alice' })];
    const dms: RawPaletteDm[] = [
      {
        conversationKey: 'dm:agent:shared-id:user:x',
        peerId: 'shared-id',
        peerKind: 'agent',
        lastActivityAt: '2026-09-28T12:00:00Z',
      },
    ];
    const [candidate] = buildUserCandidates(users, dms, SELF_ID);
    expect(candidate.activityMs).toBe(0);
  });

  it('includes the email as an additional search field and as the secondary label', () => {
    const users = [user({ id: 'u0', displayName: 'Alice', email: 'alice@example.com' })];
    const [candidate] = buildUserCandidates(users, [], SELF_ID);
    expect(candidate.searchFields).toEqual(['Alice', 'alice@example.com']);
    expect(candidate.secondaryLabel).toBe('alice@example.com');
  });

  it('falls back to email for the display name when displayName is absent', () => {
    const users = [user({ id: 'u0', displayName: '', email: 'alice@example.com' })];
    const [candidate] = buildUserCandidates(users, [], SELF_ID);
    expect(candidate.label).toBe('alice@example.com');
  });

  it('a user with a falsy id is skipped, not turned into a candidate with an empty id', () => {
    const users = [user({ id: '', displayName: 'Nobody' })];
    expect(buildUserCandidates(users, [], SELF_ID)).toEqual([]);
  });
});

describe('ChatPaletteDataController: People group cancellation and stale-load guarding', () => {
  it('resolves with candidates for a normal load, excluding self', async () => {
    apiFetchMock
      .mockResolvedValueOnce(
        jsonResponse({
          users: [
            { id: 'self-user', displayName: 'Me' },
            { id: 'u0', displayName: 'Alice' },
          ],
        })
      )
      .mockResolvedValueOnce(jsonResponse({ dms: [] }));
    const controller = new ChatPaletteDataController();
    const candidates = await controller.loadPeopleGroup('self-user');
    expect(candidates.map((c) => c.label)).toEqual(['Alice']);
  });

  it('a superseded load rejects with an AbortError rather than resolving stale data', async () => {
    let resolveFirst!: (v: Response) => void;
    apiFetchMock
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveFirst = resolve;
          })
      )
      .mockResolvedValueOnce(jsonResponse({ users: [{ id: 'u1', displayName: 'Second' }] }))
      .mockResolvedValueOnce(jsonResponse({ dms: [] }));

    const controller = new ChatPaletteDataController();
    const firstLoad = controller.loadPeopleGroup('self-user');
    const secondLoad = controller.loadPeopleGroup('self-user');
    resolveFirst(jsonResponse({ users: [{ id: 'u0', displayName: 'First' }] }));

    await expect(firstLoad).rejects.toMatchObject({ name: 'AbortError' });
    await expect(secondLoad).resolves.toMatchObject([expect.objectContaining({ label: 'Second' })]);
  });

  it('cancel() aborts the in-flight request', async () => {
    const controller = new ChatPaletteDataController();
    apiFetchMock.mockImplementationOnce((_url, options) => {
      return new Promise((_resolve, reject) => {
        options?.signal?.addEventListener('abort', () => {
          reject(new DOMException('aborted', 'AbortError'));
        });
      });
    });
    const load = controller.loadPeopleGroup('self-user');
    controller.cancel();
    await expect(load).rejects.toMatchObject({ name: 'AbortError' });
  });

  it('a DM-list failure fails the whole People group rather than silently degrading recency', async () => {
    apiFetchMock
      .mockResolvedValueOnce(jsonResponse({ users: [{ id: 'u0', displayName: 'Alice' }] }))
      .mockResolvedValueOnce(jsonResponse({}, 500));
    const controller = new ChatPaletteDataController();
    await expect(controller.loadPeopleGroup('self-user')).rejects.toThrow(PaletteLoadError);
  });

  it('a supersede that arrives while the stale load is blocked on its own DMs fetch also rejects with an AbortError (isolates the *second*, post-DMs check)', async () => {
    // Isolates loadPeopleGroup's *second* generation check (after
    // fetchPaletteDms resolves) from the first (after fetchAllPaletteUsers
    // resolves, isolated separately below) — mirrors the equivalent
    // Agents-group test. This check is the last thing before returning (no
    // further fetch follows it), so removing it can only make the stale load
    // *resolve* with stale data — there's no other error for the outer
    // catch-all's `signal.aborted` reclassification to coincidentally mask
    // it with.
    let resolveFirstDms!: (v: Response) => void;
    apiFetchMock
      .mockResolvedValueOnce(jsonResponse({ users: [{ id: 'u0', displayName: 'First' }] }))
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveFirstDms = resolve;
          })
      )
      .mockResolvedValueOnce(jsonResponse({ users: [{ id: 'u1', displayName: 'Second' }] }))
      .mockResolvedValueOnce(jsonResponse({ dms: [] }));

    const controller = new ChatPaletteDataController();
    const firstLoad = controller.loadPeopleGroup('self-user');
    await vi.waitFor(() => expect(apiFetchMock).toHaveBeenCalledTimes(2));

    const secondLoad = controller.loadPeopleGroup('self-user');
    resolveFirstDms(jsonResponse({ dms: [] }));

    await expect(firstLoad).rejects.toMatchObject({ name: 'AbortError' });
    await expect(secondLoad).resolves.toMatchObject([expect.objectContaining({ label: 'Second' })]);
  });

  it('the post-users generation check avoids a wasteful extra DMs fetch for an already-stale load (the immediately-following post-DMs check independently guarantees correctness either way)', async () => {
    // The check immediately after `fetchAllPaletteUsers` resolves and the
    // one immediately after `fetchPaletteDms` resolves test the exact same
    // condition (`myGeneration !== this.peopleGeneration`), with nothing in
    // between able to change it back — so removing the *first* one alone
    // cannot produce a wrong *result*: the second one still throws the
    // AbortError before any stale data could be returned. A plain
    // reject/resolve assertion therefore cannot tell the two apart. What the
    // first check actually buys is avoiding a pointless, already-doomed
    // `fetchPaletteDms` call once a load is known stale — an efficiency
    // property, not a correctness one, so it has to be observed as a call
    // count instead: exactly 3 apiFetch calls when the stale load's own
    // users fetch resolves after being superseded (not 4).
    let resolveFirstUsers!: (v: Response) => void;
    apiFetchMock
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveFirstUsers = resolve;
          })
      )
      .mockResolvedValueOnce(jsonResponse({ users: [{ id: 'u1', displayName: 'Second' }] }))
      .mockResolvedValueOnce(jsonResponse({ dms: [] }))
      // Only reached if the post-users check is missing and the stale load
      // wastefully re-fetches DMs — must resolve validly (not run out of
      // queued responses) so removing the check is caught by the *call
      // count* below, not incidentally by an unrelated TypeError-to-AbortError
      // path.
      .mockResolvedValueOnce(jsonResponse({ dms: [] }));

    const controller = new ChatPaletteDataController();
    const firstLoad = controller.loadPeopleGroup('self-user');
    const secondLoad = controller.loadPeopleGroup('self-user');
    resolveFirstUsers(jsonResponse({ users: [{ id: 'u0', displayName: 'First' }] }));

    await expect(firstLoad).rejects.toMatchObject({ name: 'AbortError' });
    await expect(secondLoad).resolves.toMatchObject([expect.objectContaining({ label: 'Second' })]);
    expect(apiFetchMock).toHaveBeenCalledTimes(3);
  });

  it('cancelling Agents does not abort a concurrently in-flight People load, and vice versa', async () => {
    // Each group has its own generation/AbortController — the palette loads
    // all groups on open, and one group's cancellation must not corrupt an
    // unrelated group's request.
    let peopleSignal!: AbortSignal;
    apiFetchMock.mockImplementationOnce((_url, options) => {
      peopleSignal = options!.signal!;
      return new Promise(() => {});
    });
    const { source, pending } = fakeAgentSource();
    const controller = new ChatPaletteDataController(source);
    controller.loadAgentsGroup().catch(() => {});
    void controller.loadPeopleGroup('self-user');
    await vi.waitFor(() => {
      expect(pending[0]).toBeDefined();
      expect(peopleSignal).toBeDefined();
    });
    const agentsSignal = pending[0]!.signal!;

    controller.loadAgentsGroup().catch(() => {});

    expect(agentsSignal.aborted).toBe(true);
    expect(peopleSignal.aborted).toBe(false);
  });
});

// ===========================================================================
// Threads group
// ===========================================================================

describe('fetchPaletteSpaces', () => {
  it('returns the spaces array', async () => {
    apiFetchMock.mockResolvedValueOnce(
      jsonResponse({ spaces: [{ projectId: 'p1', projectName: 'Alpha', projectSlug: 'alpha' }] })
    );
    const spaces = await fetchPaletteSpaces();
    expect(spaces).toEqual([{ projectId: 'p1', projectName: 'Alpha', projectSlug: 'alpha' }]);
  });

  it('returns an empty list for a malformed (non-array) spaces field', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse({ spaces: null }));
    expect(await fetchPaletteSpaces()).toEqual([]);
  });

  it('throws PaletteLoadError on a literal null body instead of a raw TypeError', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse(null));
    await expect(fetchPaletteSpaces()).rejects.toBeInstanceOf(PaletteLoadError);
  });

  it('throws PaletteLoadError on an array body instead of silently returning an empty list', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse([1, 2, 3]));
    await expect(fetchPaletteSpaces()).rejects.toBeInstanceOf(PaletteLoadError);
  });

  it('throws PaletteLoadError on a primitive (e.g. number) body instead of silently returning an empty list', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse(42));
    await expect(fetchPaletteSpaces()).rejects.toBeInstanceOf(PaletteLoadError);
  });

  it('throws PaletteLoadError on a non-ok response', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse({}, 500));
    await expect(fetchPaletteSpaces()).rejects.toThrow(PaletteLoadError);
  });

  it('an abort landing during the body read rejects with the original AbortError', async () => {
    const controller = new AbortController();
    apiFetchMock.mockImplementationOnce(() => {
      controller.abort();
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.reject(new DOMException('aborted', 'AbortError')),
      } as unknown as Response);
    });
    await expect(fetchPaletteSpaces(controller.signal)).rejects.toMatchObject({
      name: 'AbortError',
    });
  });
});

describe('fetchPaletteThreadsForSpace', () => {
  it('returns the threads array, including muted rows', async () => {
    apiFetchMock.mockResolvedValueOnce(
      jsonResponse({
        threads: [{ id: 't1', projectId: 'p1', name: 'General' }],
      })
    );
    const threads = await fetchPaletteThreadsForSpace('p1');
    expect(threads).toEqual([{ id: 't1', projectId: 'p1', name: 'General' }]);
  });

  it('requests the encoded project ID in the URL', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse({ threads: [] }));
    await fetchPaletteThreadsForSpace('p 1/x');
    expect(apiFetchMock.mock.calls[0][0]).toBe(
      `/api/v1/chat/spaces/${encodeURIComponent('p 1/x')}/threads`
    );
  });

  it('throws PaletteLoadError on a non-ok response', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse({}, 404));
    await expect(fetchPaletteThreadsForSpace('p1')).rejects.toThrow(PaletteLoadError);
  });

  it('throws PaletteLoadError on a literal null body instead of a raw TypeError', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse(null));
    await expect(fetchPaletteThreadsForSpace('p1')).rejects.toBeInstanceOf(PaletteLoadError);
  });

  it('throws PaletteLoadError on an array body instead of silently returning an empty list', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse([1, 2, 3]));
    await expect(fetchPaletteThreadsForSpace('p1')).rejects.toBeInstanceOf(PaletteLoadError);
  });

  it('throws PaletteLoadError on a primitive (e.g. number) body instead of silently returning an empty list', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse(42));
    await expect(fetchPaletteThreadsForSpace('p1')).rejects.toBeInstanceOf(PaletteLoadError);
  });

  it('an abort landing during the body read rejects with the original AbortError', async () => {
    const controller = new AbortController();
    apiFetchMock.mockImplementationOnce(() => {
      controller.abort();
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.reject(new DOMException('aborted', 'AbortError')),
      } as unknown as Response);
    });
    await expect(fetchPaletteThreadsForSpace('p1', controller.signal)).rejects.toMatchObject({
      name: 'AbortError',
    });
  });
});

describe('buildThreadCandidates', () => {
  const SPACE: RawPaletteSpace = { projectId: 'p1', projectName: 'Alpha', projectSlug: 'alpha' };

  it('maps a thread to a Threads-group candidate carrying the real projectId/slug', () => {
    const threads: RawPaletteThread[] = [
      { id: 't1', projectId: 'p1', name: 'General', lastActivityAt: '2026-09-28T12:00:00Z' },
    ];
    const [candidate] = buildThreadCandidates(SPACE, threads);
    expect(candidate.group).toBe('threads');
    expect(candidate.label).toBe('General');
    expect(candidate.secondaryLabel).toBe('Alpha');
    expect(candidate.activityMs).toBe(Date.parse('2026-09-28T12:00:00Z'));
    expect(candidate.target).toEqual({
      kind: 'thread',
      projectId: 'p1',
      threadId: 't1',
      threadName: 'General',
      projectSlug: 'alpha',
    });
  });

  it('includes every returned row, muted or not — no client-side filtering', () => {
    const threads: RawPaletteThread[] = [
      { id: 't1', projectId: 'p1', name: 'Muted thread' },
      { id: 't2', projectId: 'p1', name: 'Normal thread' },
    ];
    expect(buildThreadCandidates(SPACE, threads)).toHaveLength(2);
  });

  it('includes the space name in searchFields, so a thread is findable by its space', () => {
    const [candidate] = buildThreadCandidates(SPACE, [
      { id: 't1', projectId: 'p1', name: 'General' },
    ]);
    expect(candidate.searchFields).toContain('Alpha');
  });

  it('does not duplicate the space name in searchFields when it equals the thread name', () => {
    const [candidate] = buildThreadCandidates(SPACE, [
      { id: 't1', projectId: 'p1', name: 'Alpha' },
    ]);
    expect(candidate.searchFields).toEqual(['Alpha']);
  });

  it('omits projectSlug from the target when the space has none (exactOptionalPropertyTypes)', () => {
    const spaceNoSlug: RawPaletteSpace = { projectId: 'p2', projectName: 'Beta', projectSlug: '' };
    const [candidate] = buildThreadCandidates(spaceNoSlug, [{ id: 't1', projectId: 'p2' }]);
    expect('projectSlug' in candidate.target).toBe(false);
  });

  it('omits defaultAgent from the target when the thread has none', () => {
    const [candidate] = buildThreadCandidates(SPACE, [{ id: 't1', projectId: 'p1' }]);
    expect('defaultAgent' in candidate.target).toBe(false);
  });

  it('includes defaultAgent in the target when present', () => {
    const [candidate] = buildThreadCandidates(SPACE, [
      { id: 't1', projectId: 'p1', defaultAgent: 'agent-1' },
    ]);
    expect(candidate.target).toMatchObject({ defaultAgent: 'agent-1' });
  });

  it('falls back to the thread id for the label when name is absent', () => {
    const [candidate] = buildThreadCandidates(SPACE, [{ id: 't1', projectId: 'p1' }]);
    expect(candidate.label).toBe('t1');
  });

  it('produces a stable JSON-tuple candidate ID including the project ID', () => {
    const [candidate] = buildThreadCandidates(SPACE, [
      { id: 't1', projectId: 'p1', name: 'General' },
    ]);
    expect(candidate.id).toBe(JSON.stringify(['thread', 'p1', 't1']));
  });

  it('a thread with a falsy id is skipped', () => {
    expect(buildThreadCandidates(SPACE, [{ id: '', projectId: 'p1' }])).toEqual([]);
  });

  it("uses the space's own projectId when a thread row omits one", () => {
    const [candidate] = buildThreadCandidates(SPACE, [
      { id: 't1', projectId: '', name: 'General' },
    ]);
    expect(candidate.target).toMatchObject({ projectId: 'p1' });
  });

  it("uses the space's own projectId even when a thread row disagrees (never pair a mismatched ID with the space's slug)", () => {
    // A thread's own `projectId` field disagreeing with the space it was
    // fetched under would indicate a server data-integrity bug; trusting it
    // anyway would route to `space.projectSlug` for the *wrong* project.
    // `space.projectId`, the ID this thread was actually fetched under, is
    // used unconditionally.
    const [candidate] = buildThreadCandidates(SPACE, [
      { id: 't1', projectId: 'some-other-project', name: 'General' },
    ]);
    expect(candidate.target).toMatchObject({ projectId: 'p1' });
    expect(candidate.id).toBe(JSON.stringify(['thread', 'p1', 't1']));
  });
});

describe('ChatPaletteDataController: Threads group', () => {
  function spacesResponse(spaces: RawPaletteSpace[]) {
    return jsonResponse({ spaces });
  }

  it('resolves with candidates for every space, incomplete=false when all succeed', async () => {
    apiFetchMock
      .mockResolvedValueOnce(
        spacesResponse([
          { projectId: 'p1', projectName: 'Alpha', projectSlug: 'alpha' },
          { projectId: 'p2', projectName: 'Beta', projectSlug: 'beta' },
        ])
      )
      .mockResolvedValueOnce(jsonResponse({ threads: [{ id: 't1', projectId: 'p1' }] }))
      .mockResolvedValueOnce(jsonResponse({ threads: [{ id: 't2', projectId: 'p2' }] }));

    const controller = new ChatPaletteDataController();
    const result = await controller.loadThreadsGroup();

    expect(result.incomplete).toBe(false);
    expect(result.candidates.map((c) => c.target)).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ threadId: 't1', projectId: 'p1' }),
        expect.objectContaining({ threadId: 't2', projectId: 'p2' }),
      ])
    );
  });

  it('resolves with an empty, non-incomplete result when there are no spaces at all', async () => {
    apiFetchMock.mockResolvedValueOnce(spacesResponse([]));
    const controller = new ChatPaletteDataController();
    const result = await controller.loadThreadsGroup();
    expect(result).toEqual({ candidates: [], incomplete: false });
  });

  it('a space with an empty projectId is dropped before any thread-list request, and never taints incomplete', async () => {
    // A space with a falsy projectId is filtered out before
    // fetchThreadsForSpaces runs, so no request for it is ever issued (an
    // unfiltered projectId would produce a malformed
    // /api/v1/chat/spaces//threads request) and it never occupies a
    // threadsSpacesById/threadsFailedProjectIds entry under an empty-string
    // key — a failed empty-key entry could never be resolved by
    // retryThreadsGroup either, permanently flagging the group incomplete
    // for a row that was never real.
    apiFetchMock
      .mockResolvedValueOnce(
        spacesResponse([
          { projectId: '', projectName: 'Malformed' },
          { projectId: 'p1', projectName: 'Alpha' },
        ])
      )
      .mockResolvedValueOnce(jsonResponse({ threads: [{ id: 't1', projectId: 'p1' }] }));
    const controller = new ChatPaletteDataController();
    const result = await controller.loadThreadsGroup();

    // Exactly two requests total: the spaces list, then p1's threads. No
    // second thread-list request — in particular, never one for the
    // malformed space's own (empty-projectId) URL.
    expect(apiFetchMock).toHaveBeenCalledTimes(2);
    expect(apiFetchMock.mock.calls[1][0]).toContain('/chat/spaces/p1/threads');
    expect(result.incomplete).toBe(false);
    expect(result.candidates).toHaveLength(1);
  });

  it('every space having an empty projectId resolves as an honestly empty, non-incomplete group rather than a failed load', async () => {
    // With every space filtered out before any thread-list request is
    // attempted, attemptedSpaceCount is 0 — the same "no spaces exist"
    // outcome as an actually-empty spaces list, not "every attempted space
    // failed" (which would reject with PaletteLoadError instead).
    apiFetchMock.mockResolvedValueOnce(
      spacesResponse([
        { projectId: '', projectName: 'Malformed One' },
        { projectId: '', projectName: 'Malformed Two' },
      ])
    );
    const controller = new ChatPaletteDataController();
    const result = await controller.loadThreadsGroup();

    expect(apiFetchMock).toHaveBeenCalledTimes(1);
    expect(result).toEqual({ candidates: [], incomplete: false });
  });

  it('a partial failure keeps the succeeded spaces and flags incomplete=true, rather than failing the whole group', async () => {
    apiFetchMock
      .mockResolvedValueOnce(
        spacesResponse([
          { projectId: 'p1', projectName: 'Alpha' },
          { projectId: 'p2', projectName: 'Beta' },
        ])
      )
      .mockResolvedValueOnce(jsonResponse({ threads: [{ id: 't1', projectId: 'p1' }] }))
      .mockResolvedValueOnce(jsonResponse({}, 500));

    const controller = new ChatPaletteDataController();
    const result = await controller.loadThreadsGroup();

    expect(result.incomplete).toBe(true);
    expect(result.candidates).toHaveLength(1);
    expect(result.candidates[0].target).toMatchObject({ threadId: 't1' });
  });

  it('rejects with PaletteLoadError (not a silent empty group) when every space fails', async () => {
    apiFetchMock
      .mockResolvedValueOnce(spacesResponse([{ projectId: 'p1', projectName: 'Alpha' }]))
      .mockResolvedValueOnce(jsonResponse({}, 500));
    const controller = new ChatPaletteDataController();
    await expect(controller.loadThreadsGroup()).rejects.toThrow(PaletteLoadError);
  });

  it('rejects with PaletteLoadError when the spaces list itself fails', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse({}, 500));
    const controller = new ChatPaletteDataController();
    await expect(controller.loadThreadsGroup()).rejects.toThrow(PaletteLoadError);
  });

  it('bounds concurrent thread-list requests to 4', async () => {
    const spaces = Array.from({ length: 9 }, (_, i) => ({
      projectId: `p${i}`,
      projectName: `Space ${i}`,
    }));
    apiFetchMock.mockResolvedValueOnce(spacesResponse(spaces));

    let inFlight = 0;
    let peak = 0;
    let totalStarted = 0;
    const pending: Array<() => void> = [];
    // A single default implementation (rather than one `mockImplementationOnce`
    // per space) applies to every thread-list call after the spaces call
    // above consumes the `mockResolvedValueOnce` — vitest mocks always drain
    // the once-queue before falling back to this.
    apiFetchMock.mockImplementation(
      () =>
        new Promise<Response>((resolve) => {
          totalStarted++;
          inFlight++;
          peak = Math.max(peak, inFlight);
          pending.push(() => {
            inFlight--;
            resolve(jsonResponse({ threads: [] }));
          });
        })
    );

    const controller = new ChatPaletteDataController();
    const resultPromise = controller.loadThreadsGroup();

    // The worker pool should start exactly 4 requests up front, never more.
    await vi.waitFor(() => expect(pending.length).toBe(4));
    expect(peak).toBe(4);

    // Resolve one request at a time; each completion lets exactly one more
    // start, so concurrency never exceeds 4 for the rest of the run either.
    for (let i = 0; i < spaces.length; i++) {
      await vi.waitFor(() => expect(pending.length).toBeGreaterThan(0));
      pending.shift()!();
      expect(peak).toBeLessThanOrEqual(4);
    }

    await resultPromise;
    expect(totalStarted).toBe(spaces.length);
    expect(peak).toBeLessThanOrEqual(4);
  });

  it('a superseded load rejects with an AbortError rather than resolving stale data', async () => {
    let resolveFirstSpaces!: (v: Response) => void;
    apiFetchMock
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveFirstSpaces = resolve;
          })
      )
      .mockResolvedValueOnce(spacesResponse([{ projectId: 'p2', projectName: 'Second' }]))
      .mockResolvedValueOnce(jsonResponse({ threads: [{ id: 't2', projectId: 'p2' }] }));

    const controller = new ChatPaletteDataController();
    const firstLoad = controller.loadThreadsGroup();
    const secondLoad = controller.loadThreadsGroup();
    resolveFirstSpaces(spacesResponse([{ projectId: 'p1', projectName: 'First' }]));

    await expect(firstLoad).rejects.toMatchObject({ name: 'AbortError' });
    const secondResult = await secondLoad;
    expect(secondResult.candidates).toHaveLength(1);
  });

  it('cancel() aborts the in-flight spaces request', async () => {
    const controller = new ChatPaletteDataController();
    apiFetchMock.mockImplementationOnce((_url, options) => {
      return new Promise((_resolve, reject) => {
        options?.signal?.addEventListener('abort', () => {
          reject(new DOMException('aborted', 'AbortError'));
        });
      });
    });
    const load = controller.loadThreadsGroup();
    controller.cancel();
    await expect(load).rejects.toMatchObject({ name: 'AbortError' });
  });

  it('a supersede that arrives between the spaces fetch and the per-space thread fetches also rejects with an AbortError', async () => {
    // Isolates loadThreadsGroup's *second* generation check (after
    // fetchThreadsForSpaces resolves) from the first (after fetchPaletteSpaces
    // resolves, exercised by the "superseded load" test above).
    let resolveFirstThreads!: (v: Response) => void;
    apiFetchMock
      .mockResolvedValueOnce(spacesResponse([{ projectId: 'p1', projectName: 'First' }]))
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveFirstThreads = resolve;
          })
      )
      .mockResolvedValueOnce(spacesResponse([{ projectId: 'p2', projectName: 'Second' }]))
      .mockResolvedValueOnce(jsonResponse({ threads: [{ id: 't2', projectId: 'p2' }] }));

    const controller = new ChatPaletteDataController();
    const firstLoad = controller.loadThreadsGroup();
    await vi.waitFor(() => expect(apiFetchMock).toHaveBeenCalledTimes(2));

    const secondLoad = controller.loadThreadsGroup();
    resolveFirstThreads(jsonResponse({ threads: [{ id: 't1', projectId: 'p1' }] }));

    await expect(firstLoad).rejects.toMatchObject({ name: 'AbortError' });
    const secondResult = await secondLoad;
    expect(secondResult.candidates.map((c) => c.target.threadId)).toEqual(['t2']);
  });

  it("a stale first load's per-space write never lands in a later read of shared state (the generation check runs before applying results)", async () => {
    // Resolving the stale fetch only *before* reading the second load's own
    // result would mean the write this test checks for could never be
    // observed either way — the assertions would only ever look at a
    // snapshot taken before the stale write could land. Keeping the
    // controller alive and taking a *third* reading — via retryThreadsGroup —
    // after the stale write has had a chance to land is what makes an
    // unconditional apply actually observable.
    let resolveFirstThreads!: (v: Response) => void;
    apiFetchMock
      .mockResolvedValueOnce(spacesResponse([{ projectId: 'p1', projectName: 'First' }]))
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveFirstThreads = resolve;
          })
      )
      .mockResolvedValueOnce(spacesResponse([{ projectId: 'p2', projectName: 'Second' }]))
      .mockResolvedValueOnce(jsonResponse({ threads: [{ id: 't2', projectId: 'p2' }] }));

    const controller = new ChatPaletteDataController();
    const firstLoad = controller.loadThreadsGroup();
    await vi.waitFor(() => expect(apiFetchMock).toHaveBeenCalledTimes(2));
    const secondLoad = controller.loadThreadsGroup();
    const secondResult = await secondLoad;
    expect(secondResult.candidates.map((c) => c.target.threadId)).toEqual(['t2']);
    expect(secondResult.incomplete).toBe(false);

    // Now resolve the stale first load's per-space fetch as a *failure*,
    // strictly after the second load has already cleared and repopulated
    // the shared per-space maps with only p2. An unconditional apply would
    // add p1 — a project the second load never even requested — to the
    // shared failed-project set.
    resolveFirstThreads(jsonResponse({}, 500));
    await expect(firstLoad).rejects.toMatchObject({ name: 'AbortError' });

    // A third read: with the failed set genuinely empty, retryThreadsGroup
    // falls back to a full, fresh load, so queue responses for that —
    // spaces and threads for p2 again. If the stale write above had instead
    // landed in the shared failed-project set, retryThreadsGroup would take
    // the narrow-retry branch instead, fetch nothing further (p1 is not in
    // the current space map), and report incomplete=true even though
    // nothing about the second load's own data actually failed.
    apiFetchMock
      .mockResolvedValueOnce(spacesResponse([{ projectId: 'p2', projectName: 'Second' }]))
      .mockResolvedValueOnce(jsonResponse({ threads: [{ id: 't2', projectId: 'p2' }] }));
    const thirdResult = await controller.retryThreadsGroup();

    expect(thirdResult.incomplete).toBe(false);
    expect(thirdResult.candidates.map((c) => c.target.threadId)).toEqual(['t2']);
  });

  describe('retryThreadsGroup: only the failed spaces', () => {
    it('re-fetches only the previously-failed space, keeping the succeeded one intact', async () => {
      apiFetchMock
        .mockResolvedValueOnce(
          spacesResponse([
            { projectId: 'p1', projectName: 'Alpha' },
            { projectId: 'p2', projectName: 'Beta' },
          ])
        )
        .mockResolvedValueOnce(jsonResponse({ threads: [{ id: 't1', projectId: 'p1' }] }))
        .mockResolvedValueOnce(jsonResponse({}, 500));

      const controller = new ChatPaletteDataController();
      const first = await controller.loadThreadsGroup();
      expect(first.incomplete).toBe(true);

      apiFetchMock.mockResolvedValueOnce(
        jsonResponse({ threads: [{ id: 't2', projectId: 'p2' }] })
      );
      const retried = await controller.retryThreadsGroup();

      // Exactly one more request — for p2's threads only, never p1's spaces
      // or threads endpoint again.
      expect(apiFetchMock).toHaveBeenCalledTimes(4);
      expect(apiFetchMock.mock.calls[3][0]).toContain('/chat/spaces/p2/threads');
      expect(retried.incomplete).toBe(false);
      expect(retried.candidates.map((c) => c.target.threadId).sort()).toEqual(['t1', 't2']);
    });

    it('falls back to a full load when nothing failed (or nothing has loaded yet)', async () => {
      apiFetchMock
        .mockResolvedValueOnce(spacesResponse([{ projectId: 'p1', projectName: 'Alpha' }]))
        .mockResolvedValueOnce(jsonResponse({ threads: [{ id: 't1', projectId: 'p1' }] }));
      const controller = new ChatPaletteDataController();
      const result = await controller.retryThreadsGroup();
      expect(apiFetchMock).toHaveBeenCalledTimes(2);
      expect(result.candidates).toHaveLength(1);
    });

    it('a still-failing retried space remains incomplete without dropping the other space', async () => {
      apiFetchMock
        .mockResolvedValueOnce(
          spacesResponse([
            { projectId: 'p1', projectName: 'Alpha' },
            { projectId: 'p2', projectName: 'Beta' },
          ])
        )
        .mockResolvedValueOnce(jsonResponse({ threads: [{ id: 't1', projectId: 'p1' }] }))
        .mockResolvedValueOnce(jsonResponse({}, 500));

      const controller = new ChatPaletteDataController();
      await controller.loadThreadsGroup();

      apiFetchMock.mockResolvedValueOnce(jsonResponse({}, 500));
      const retried = await controller.retryThreadsGroup();

      expect(retried.incomplete).toBe(true);
      expect(retried.candidates.map((c) => c.target.threadId)).toEqual(['t1']);
    });

    it("a supersede during retry's own per-space fetch rejects with an AbortError, not stale retry data", async () => {
      apiFetchMock
        .mockResolvedValueOnce(
          spacesResponse([
            { projectId: 'p1', projectName: 'Alpha' },
            { projectId: 'p2', projectName: 'Beta' },
          ])
        )
        .mockResolvedValueOnce(jsonResponse({ threads: [{ id: 't1', projectId: 'p1' }] }))
        .mockResolvedValueOnce(jsonResponse({}, 500));

      const controller = new ChatPaletteDataController();
      const first = await controller.loadThreadsGroup();
      expect(first.incomplete).toBe(true);

      let resolveFirstRetry!: (v: Response) => void;
      apiFetchMock.mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveFirstRetry = resolve;
          })
      );
      const firstRetry = controller.retryThreadsGroup();
      await vi.waitFor(() => expect(apiFetchMock).toHaveBeenCalledTimes(4));

      apiFetchMock.mockResolvedValueOnce(
        jsonResponse({ threads: [{ id: 't2-fresh', projectId: 'p2' }] })
      );
      const secondRetry = controller.retryThreadsGroup();
      resolveFirstRetry(jsonResponse({ threads: [{ id: 't2-stale', projectId: 'p2' }] }));

      await expect(firstRetry).rejects.toMatchObject({ name: 'AbortError' });
      const secondResult = await secondRetry;
      expect(secondResult.incomplete).toBe(false);
      expect(secondResult.candidates.map((c) => c.target.threadId).sort()).toEqual([
        't1',
        't2-fresh',
      ]);
    });

    it("a stale retry's per-space write never lands in a later read of shared state, even when the load that supersedes it is a fresh full load", async () => {
      // retryThreadsGroup has the identical
      // fetch-then-generation-check-then-apply shape as loadThreadsGroup and
      // needs its own coverage for the same stale-write race.
      apiFetchMock
        .mockResolvedValueOnce(
          spacesResponse([
            { projectId: 'p1', projectName: 'Alpha' },
            { projectId: 'p2', projectName: 'Beta' },
          ])
        )
        .mockResolvedValueOnce(jsonResponse({ threads: [{ id: 't1', projectId: 'p1' }] }))
        .mockResolvedValueOnce(jsonResponse({}, 500));

      const controller = new ChatPaletteDataController();
      const first = await controller.loadThreadsGroup();
      expect(first.incomplete).toBe(true); // p2 failed

      // Start a retry of the failed space (p2), and hold its per-space fetch
      // pending — this is the load whose write will go stale.
      let resolveStaleRetryThreads!: (v: Response) => void;
      apiFetchMock.mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveStaleRetryThreads = resolve;
          })
      );
      const staleRetry = controller.retryThreadsGroup();
      await vi.waitFor(() => expect(apiFetchMock).toHaveBeenCalledTimes(4));

      // Before the stale retry's fetch resolves, a fresh full load supersedes
      // it entirely (a different space set — p1 is gone, replaced by p3) and
      // completes.
      apiFetchMock
        .mockResolvedValueOnce(spacesResponse([{ projectId: 'p3', projectName: 'Third' }]))
        .mockResolvedValueOnce(jsonResponse({ threads: [{ id: 't3', projectId: 'p3' }] }));
      const freshLoad = controller.loadThreadsGroup();
      const freshResult = await freshLoad;
      expect(freshResult.incomplete).toBe(false);
      expect(freshResult.candidates.map((c) => c.target.threadId)).toEqual(['t3']);

      // Now resolve the stale retry's per-space fetch as a *failure*, well
      // after the fresh load has cleared and repopulated shared state with
      // only p3. An unconditional apply would add p2 — a project the fresh
      // load never requested and that no longer exists in the current space
      // map — to the shared failed-project set.
      resolveStaleRetryThreads(jsonResponse({}, 500));
      await expect(staleRetry).rejects.toMatchObject({ name: 'AbortError' });

      // A further read: with the failed set genuinely empty, retryThreadsGroup
      // falls back to a full, fresh load, so queue responses for that — spaces
      // and threads for p3 again. If the stale write above had instead landed
      // in the shared failed-project set, retryThreadsGroup would take the
      // narrow-retry branch instead, fetch nothing further (p2 is not in the
      // current space map), and report incomplete=true even though nothing
      // about the fresh load's own data actually failed.
      apiFetchMock
        .mockResolvedValueOnce(spacesResponse([{ projectId: 'p3', projectName: 'Third' }]))
        .mockResolvedValueOnce(jsonResponse({ threads: [{ id: 't3', projectId: 'p3' }] }));
      const finalResult = await controller.retryThreadsGroup();

      expect(finalResult.incomplete).toBe(false);
      expect(finalResult.candidates.map((c) => c.target.threadId)).toEqual(['t3']);
    });
  });
});

describe('buildDocumentCandidates', () => {
  function pathFile(overrides: Partial<RecentFile> = {}): RecentFile {
    return {
      key: JSON.stringify(['path', 'p1', 'workspace', '', 'notes.txt']),
      name: 'notes.txt',
      source: {
        conversationKey: 'topic-1',
        messageId: 'm1',
        sentAt: '2026-09-28T12:00:00Z',
        projectId: 'p1',
        projectName: 'Alpha',
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

  function attachmentFile(overrides: Partial<RecentFile> = {}): RecentFile {
    return {
      key: JSON.stringify(['attachment', 'att-1']),
      name: 'photo.png',
      source: {
        conversationKey: 'dm:agent:a1:user:self',
        messageId: 'm2',
        sentAt: '2026-09-28T12:00:00Z',
        projectId: 'p1',
        projectName: 'Alpha',
      },
      target: { kind: 'attachment', id: 'att-1', mime: 'image/png', size: 1234 },
      ...overrides,
    } as RecentFile;
  }

  it('maps a path file to a Documents-group candidate with the container path and project in searchFields', () => {
    const [candidate] = buildDocumentCandidates([pathFile()]);
    expect(candidate.group).toBe('documents');
    expect(candidate.label).toBe('notes.txt');
    expect(candidate.searchFields).toEqual(['notes.txt', '/workspace/notes.txt', 'Alpha']);
    expect(candidate.secondaryLabel).toBe('Alpha — /workspace/notes.txt');
    expect(candidate.activityMs).toBe(Date.parse('2026-09-28T12:00:00Z'));
    expect(candidate.target).toEqual({ kind: 'document', file: pathFile() });
  });

  it('maps an attachment file with a project-and-metadata-labeled secondary line and no path in searchFields', () => {
    const [candidate] = buildDocumentCandidates([attachmentFile()]);
    expect(candidate.group).toBe('documents');
    expect(candidate.label).toBe('photo.png');
    expect(candidate.searchFields).toEqual(['photo.png', 'Alpha']);
    expect(candidate.secondaryLabel).toBe('Alpha — Attachment · 1.2 KB · Sep 28, 2026');
  });

  it('two attachments sharing a name and project are distinguishable by size and date', () => {
    // Midday UTC, not midnight: the formatted date is in the display zone
    // (Auto falls back to the browser zone), and a midnight-UTC fixture
    // rolls back to the previous day in every zone west of UTC.
    const older = attachmentFile({
      key: 'att-older',
      target: { kind: 'attachment', id: 'att-older', mime: 'image/png', size: 500 },
      source: {
        conversationKey: 'dm:a',
        messageId: 'm-older',
        sentAt: '2026-01-01T12:00:00Z',
        projectId: 'p1',
        projectName: 'Alpha',
      },
    });
    const newer = attachmentFile({
      key: 'att-newer',
      target: { kind: 'attachment', id: 'att-newer', mime: 'image/png', size: 50_000 },
      source: {
        conversationKey: 'dm:b',
        messageId: 'm-newer',
        sentAt: '2026-06-01T12:00:00Z',
        projectId: 'p1',
        projectName: 'Alpha',
      },
    });
    const [a, b] = buildDocumentCandidates([older, newer]);
    expect(a.label).toBe(b.label); // both "photo.png" — the same-name case this exists for
    expect(a.secondaryLabel).not.toBe(b.secondaryLabel);
    expect(a.secondaryLabel).toBe('Alpha — Attachment · 500 B · Jan 1, 2026');
    expect(b.secondaryLabel).toBe('Alpha — Attachment · 48.8 KB · Jun 1, 2026');
  });

  it('dates an attachment in the display zone, not the browser zone', () => {
    // vitest pins the browser zone to UTC; 15:00Z is the next day in Tokyo.
    setPreferredTimeZone('Asia/Tokyo');
    try {
      const file = attachmentFile({
        source: {
          conversationKey: 'dm:x',
          messageId: 'm2',
          sentAt: '2026-09-28T15:00:00Z',
          projectId: 'p1',
          projectName: 'Alpha',
        },
      });
      const [candidate] = buildDocumentCandidates([file]);
      expect(candidate.secondaryLabel).toBe('Alpha — Attachment · 1.2 KB · Sep 29, 2026');
    } finally {
      setPreferredTimeZone('');
    }
  });

  it('omits the date from an attachment secondary label when sentAt is a Go zero timestamp', () => {
    const file = attachmentFile({
      source: {
        conversationKey: 'dm:x',
        messageId: 'm2',
        sentAt: '0001-01-01T00:00:00Z',
        projectId: 'p1',
        projectName: 'Alpha',
      },
    });
    const [candidate] = buildDocumentCandidates([file]);
    expect(candidate.secondaryLabel).toBe('Alpha — Attachment · 1.2 KB');
  });

  it('omits the project name from the path secondary label and searchFields when absent', () => {
    const file = pathFile({
      source: { conversationKey: 'topic-1', messageId: 'm1', sentAt: '2026-09-28T12:00:00Z' },
    });
    const [candidate] = buildDocumentCandidates([file]);
    expect(candidate.secondaryLabel).toBe('/workspace/notes.txt');
    expect(candidate.searchFields).toEqual(['notes.txt', '/workspace/notes.txt']);
  });

  it('falls back to plain "Attachment · <size> · <date>" when an attachment has no captured project name', () => {
    const file = attachmentFile({
      source: { conversationKey: 'dm:x', messageId: 'm2', sentAt: '2026-09-28T12:00:00Z' },
    });
    const [candidate] = buildDocumentCandidates([file]);
    expect(candidate.secondaryLabel).toBe('Attachment · 1.2 KB · Sep 28, 2026');
    expect(candidate.searchFields).toEqual(['photo.png']);
  });

  it('produces a stable JSON-tuple candidate ID from the file key', () => {
    const file = pathFile();
    const [candidate] = buildDocumentCandidates([file]);
    expect(candidate.id).toBe(JSON.stringify(['document', file.key]));
  });

  it('treats a Go-zero sentAt as activityMs 0', () => {
    const file = pathFile({
      source: {
        conversationKey: 'topic-1',
        messageId: 'm1',
        sentAt: '0001-01-01T00:00:00Z',
        projectId: 'p1',
      },
    });
    const [candidate] = buildDocumentCandidates([file]);
    expect(candidate.activityMs).toBe(0);
  });

  it('maps every record in the snapshot, preserving order', () => {
    const a = pathFile({ key: 'a', name: 'a.txt' });
    const b = attachmentFile({ key: 'b', name: 'b.png' });
    const candidates = buildDocumentCandidates([a, b]);
    expect(candidates.map((c) => c.label)).toEqual(['a.txt', 'b.png']);
  });

  it('returns an empty list for an empty snapshot', () => {
    expect(buildDocumentCandidates([])).toEqual([]);
  });
});
