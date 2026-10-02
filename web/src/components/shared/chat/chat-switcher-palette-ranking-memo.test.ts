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
 * Regression coverage for the ranking memoization cache: ranking (filter + classify +
 * highlight-range computation, then an O(N log N) sort) ran three times per
 * render — once each from `willUpdate`, `renderPalette`'s match count, and
 * `renderPaletteGroup` — plus once per arrow-key press. This mocks
 * `rankCandidates` itself (wrapping the real implementation, so behavior is
 * unchanged) purely to count calls; a separate file from
 * chat-switcher-palette.test.ts so that mock doesn't affect any other test.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi } from 'vitest';

vi.mock('../../../utils/chat-palette-match.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../../utils/chat-palette-match.js')>();
  return { ...actual, rankCandidates: vi.fn(actual.rankCandidates) };
});

import { rankCandidates } from '../../../utils/chat-palette-match.js';
const rankCandidatesMock = vi.mocked(rankCandidates);

await import('./chat-switcher.js');
type ScionChatSwitcher = import('./chat-switcher.js').ScionChatSwitcher;
import type { GroupState } from '../../../client/chat-palette-types.js';
import { dmCandidateId } from '../../../client/chat-palette-types.js';

function agentsGroup(
  candidates: Array<{ peerId: string; label: string; activityMs?: number }>
): Record<'agents', GroupState> {
  return {
    agents: {
      status: 'ready',
      candidates: candidates.map((c) => ({
        id: dmCandidateId('agent', c.peerId),
        group: 'agents',
        label: c.label,
        searchFields: [c.label],
        secondaryLabel: '',
        activityMs: c.activityMs ?? 0,
        target: { kind: 'dm', peerKind: 'agent', peerId: c.peerId, displayName: c.label },
      })),
    },
  };
}

async function mountPalette(groups: Record<'agents', GroupState>): Promise<ScionChatSwitcher> {
  const el = document.createElement('scion-chat-switcher') as ScionChatSwitcher;
  el.open = true;
  el.groups = groups;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

describe('rankedPaletteCandidates is memoized per queryText/groups change', () => {
  it('a single render (which reads the ranked list from willUpdate, the match-count status, and the group list) computes ranking exactly once', async () => {
    rankCandidatesMock.mockClear();
    await mountPalette(
      agentsGroup([
        { peerId: 'a1', label: 'Coder One' },
        { peerId: 'a2', label: 'Reviewer Bot' },
      ])
    );
    // One initial render touches all three call sites (willUpdate's
    // reconcileActiveId, renderPalette's match-count status text, and
    // renderPaletteGroup's list) — without the memoization cache this would
    // be 3 calls.
    expect(rankCandidatesMock).toHaveBeenCalledTimes(1);
  });

  it('typing (a queryText change) recomputes exactly once for that render, not per call site', async () => {
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Coder One' }]));
    rankCandidatesMock.mockClear();

    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    input.value = 'cod';
    input.dispatchEvent(new InputEvent('input'));
    await el.updateComplete;

    expect(rankCandidatesMock).toHaveBeenCalledTimes(1);
  });

  it('an arrow-key press (no queryText/groups change) reuses the cached ranking rather than recomputing', async () => {
    const el = await mountPalette(
      agentsGroup([
        { peerId: 'a1', label: 'Coder One' },
        { peerId: 'a2', label: 'Reviewer Bot' },
      ])
    );
    rankCandidatesMock.mockClear();

    el.shadowRoot
      ?.querySelector('#palette-query-input')
      ?.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;

    expect(rankCandidatesMock).not.toHaveBeenCalled();
  });

  it('reassigning groups to a new (but equal-content) object recomputes; nothing changing does not', async () => {
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Coder One' }]));
    rankCandidatesMock.mockClear();

    el.groups = agentsGroup([{ peerId: 'a1', label: 'Coder One' }]); // new reference, same content
    await el.updateComplete;
    expect(rankCandidatesMock).toHaveBeenCalledTimes(1);

    rankCandidatesMock.mockClear();
    el.requestUpdate(); // force a render with nothing actually changed
    await el.updateComplete;
    expect(rankCandidatesMock).not.toHaveBeenCalled();
  });
});
