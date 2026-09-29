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
import {
  fetchAllPaletteAgents,
  fetchPaletteDms,
  buildAgentCandidates,
  isPaletteAgentViable,
  PaletteLoadError,
  ChatPaletteDataController,
  type RawPaletteAgent,
  type RawPaletteDm,
} from './chat-palette-data.js';

const apiFetchMock = vi.mocked(apiFetch);

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status });
}

afterEach(() => {
  apiFetchMock.mockReset();
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
    // whether a signal was passed — the production caller (loadAgentsGroup)
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

describe('ChatPaletteDataController: cancellation and stale-load guarding', () => {
  it('resolves with candidates for a normal load', async () => {
    apiFetchMock
      .mockResolvedValueOnce(
        jsonResponse({
          agents: [{ id: 'a0', name: 'Coder', _capabilities: { actions: ['attach'] } }],
        })
      )
      .mockResolvedValueOnce(jsonResponse({ dms: [] }));
    const controller = new ChatPaletteDataController();
    const candidates = await controller.loadAgentsGroup();
    expect(candidates).toHaveLength(1);
  });

  it('a malformed body during a normal (non-cancelled, non-superseded) load still rejects with PaletteLoadError, not a raw SyntaxError', async () => {
    // Controller-level variant of the two standalone tests above, covering
    // the real call shape: ChatPaletteDataController.loadAgentsGroup always
    // passes its own live signal, so this is the shape that actually matters
    // in production.
    apiFetchMock.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.reject(new SyntaxError('Unexpected token')),
    } as unknown as Response);
    const controller = new ChatPaletteDataController();
    await expect(controller.loadAgentsGroup()).rejects.toBeInstanceOf(PaletteLoadError);
  });

  it('a superseded load rejects with an AbortError rather than resolving stale data', async () => {
    let resolveFirstAgents!: (v: Response) => void;
    apiFetchMock
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveFirstAgents = resolve;
          })
      )
      .mockResolvedValueOnce(
        jsonResponse({
          agents: [{ id: 'a1', name: 'Second', _capabilities: { actions: ['attach'] } }],
        })
      )
      .mockResolvedValueOnce(jsonResponse({ dms: [] }));

    const controller = new ChatPaletteDataController();
    const firstLoad = controller.loadAgentsGroup();
    const secondLoad = controller.loadAgentsGroup();

    // Let the first (stale) request finally resolve after the second has started.
    resolveFirstAgents(
      jsonResponse({
        agents: [{ id: 'a0', name: 'First', _capabilities: { actions: ['attach'] } }],
      })
    );

    await expect(firstLoad).rejects.toMatchObject({ name: 'AbortError' });
    await expect(secondLoad).resolves.toMatchObject([expect.objectContaining({ label: 'Second' })]);
  });

  it('a supersede that arrives between the agents and DMs fetches also rejects with an AbortError', async () => {
    // Isolates loadAgentsGroup's *second* generation check (after
    // fetchPaletteDms resolves) from the first (after fetchAllPaletteAgents
    // resolves, exercised by the test above) — a supersede landing in the
    // window between the two fetches must still be caught, not just one
    // landing during the first fetch.
    let resolveFirstDms!: (v: Response) => void;
    apiFetchMock
      .mockResolvedValueOnce(
        jsonResponse({
          agents: [{ id: 'a0', name: 'First', _capabilities: { actions: ['attach'] } }],
        })
      )
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveFirstDms = resolve;
          })
      )
      .mockResolvedValueOnce(
        jsonResponse({
          agents: [{ id: 'a1', name: 'Second', _capabilities: { actions: ['attach'] } }],
        })
      )
      .mockResolvedValueOnce(jsonResponse({ dms: [] }));

    const controller = new ChatPaletteDataController();
    const firstLoad = controller.loadAgentsGroup();

    // Wait until the first load has passed its first generation check and
    // is actually blocked on its (still-pending) DMs fetch.
    await vi.waitFor(() => expect(apiFetchMock).toHaveBeenCalledTimes(2));

    const secondLoad = controller.loadAgentsGroup();
    resolveFirstDms(jsonResponse({ dms: [] }));

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
    const load = controller.loadAgentsGroup();
    controller.cancel();
    await expect(load).rejects.toMatchObject({ name: 'AbortError' });
  });

  it('a cancelled load does not publish a real error even when the failure is not from an aborted body read (e.g. a 403 arriving after cancel)', async () => {
    // The per-catch abort guard only reclassifies an AbortError that
    // surfaces from `res.json()` itself. It does nothing for a failure that
    // has nothing to do with reading the body — `fetchAllPaletteAgents`'s
    // `if (!res.ok) throw new PaletteLoadError(...)` fires unconditionally
    // on any non-2xx status, with no signal check at all. So a request that
    // happens to resolve with e.g. a 403 *after* this load has already been
    // cancelled would otherwise surface as a real `PaletteLoadError` — the
    // same class of residual failure apiFetch's own internal 403 handling
    // (api.ts) has, reproduced here without needing to fake that internal
    // timing: the outcome (a non-ok response settling after cancel()) is the
    // same regardless of why the response is late.
    let resolveAgentsFetch!: (v: Response) => void;
    apiFetchMock.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveAgentsFetch = resolve;
        })
    );
    const controller = new ChatPaletteDataController();
    const load = controller.loadAgentsGroup();
    controller.cancel();
    resolveAgentsFetch(jsonResponse({}, 403));

    await expect(load).rejects.toMatchObject({ name: 'AbortError' });
  });

  it("a supersede landing during the first load's DMs body read rejects with an AbortError, not a PaletteLoadError", async () => {
    // Distinct from the "between the agents and DMs fetches" test above:
    // there, the second generation check (which runs *after* a successful
    // fetchPaletteDms) catches the supersede. Here the abort instead lands
    // while fetchPaletteDms's own `res.json()` is still reading the body, so
    // it is fetchPaletteDms's own catch block — not the generation
    // check — that must let the AbortError through.
    let firstSignal!: AbortSignal;
    apiFetchMock
      .mockImplementationOnce((_url, options) => {
        firstSignal = options!.signal!;
        return Promise.resolve(
          jsonResponse({
            agents: [{ id: 'a0', name: 'First', _capabilities: { actions: ['attach'] } }],
          })
        );
      })
      .mockImplementationOnce(
        () =>
          Promise.resolve({
            ok: true,
            status: 200,
            json: () =>
              new Promise((_resolve, reject) => {
                firstSignal.addEventListener('abort', () =>
                  reject(new DOMException('aborted', 'AbortError'))
                );
              }),
          }) as unknown as Promise<Response>
      )
      .mockResolvedValueOnce(
        jsonResponse({
          agents: [{ id: 'a1', name: 'Second', _capabilities: { actions: ['attach'] } }],
        })
      )
      .mockResolvedValueOnce(jsonResponse({ dms: [] }));

    const controller = new ChatPaletteDataController();
    const firstLoad = controller.loadAgentsGroup();
    await vi.waitFor(() => expect(apiFetchMock).toHaveBeenCalledTimes(2));
    const secondLoad = controller.loadAgentsGroup(); // aborts firstSignal

    await expect(firstLoad).rejects.toMatchObject({ name: 'AbortError' });
    await expect(secondLoad).resolves.toMatchObject([expect.objectContaining({ label: 'Second' })]);
  });

  it("starting a new load actually aborts the previous load's signal", async () => {
    // The generation counter alone stops a superseded load's *result* from
    // being used, but the earlier request should also be cancelled at the
    // network/stream level — otherwise an in-flight body read for a
    // long-abandoned load keeps running. This discriminates the
    // `this.abortController?.abort()` call at the top of loadAgentsGroup
    // from the generation check, which a mutation could delete without any
    // other test noticing.
    let firstSignal!: AbortSignal;
    apiFetchMock.mockImplementationOnce((_url, options) => {
      firstSignal = options!.signal!;
      return new Promise(() => {});
    });
    const controller = new ChatPaletteDataController();
    void controller.loadAgentsGroup();
    await vi.waitFor(() => expect(firstSignal).toBeDefined());
    expect(firstSignal.aborted).toBe(false);

    apiFetchMock.mockImplementationOnce(() => new Promise(() => {}));
    void controller.loadAgentsGroup();
    expect(firstSignal.aborted).toBe(true);
  });

  it('a DM-list failure fails the whole group rather than silently degrading every agent to activityMs=0', async () => {
    apiFetchMock
      .mockResolvedValueOnce(
        jsonResponse({
          agents: [{ id: 'a0', name: 'Coder', _capabilities: { actions: ['attach'] } }],
        })
      )
      .mockResolvedValueOnce(jsonResponse({}, 500));
    const controller = new ChatPaletteDataController();
    await expect(controller.loadAgentsGroup()).rejects.toThrow(PaletteLoadError);
  });
});
