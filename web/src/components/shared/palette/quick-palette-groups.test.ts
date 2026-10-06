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
 * Tests for <scion-quick-palette>'s multi-group palette behavior (Agents,
 * Threads and People groups): the four-group keyboard infrastructure
 * (Tab/Shift+Tab cycling, Up/Down within a group, show-more past 10 rows)
 * and the Threads group's incomplete-results notice.
 * `quick-palette.test.ts` continues to cover the single-group
 * (Agents-only) case unmodified.
 *
 * happy-dom does not retarget events across shadow roots, so real
 * composedPath()/focus assertions live in e2e/chat-palette (Chromium).
 */

// @vitest-environment happy-dom

import { describe, it, expect, afterEach } from 'vitest';

await import('./quick-palette.js');
type ScionQuickPalette = import('./quick-palette.js').ScionQuickPalette;
import type {
  GroupState,
  PaletteCandidate,
  PaletteGroup,
} from '../../../client/chat-palette-types.js';
import { dmCandidateId, threadCandidateId } from '../../../client/chat-palette-types.js';

function agentCandidate(id: string, label: string, activityMs = 0): PaletteCandidate {
  return {
    id: dmCandidateId('agent', id),
    group: 'agents',
    label,
    searchFields: [label],
    secondaryLabel: '',
    activityMs,
    target: { kind: 'dm', peerKind: 'agent', peerId: id, displayName: label },
  };
}

function personCandidate(id: string, label: string, activityMs = 0): PaletteCandidate {
  return {
    id: dmCandidateId('user', id),
    group: 'people',
    label,
    searchFields: [label],
    secondaryLabel: '',
    activityMs,
    target: { kind: 'dm', peerKind: 'user', peerId: id, displayName: label },
  };
}

function threadCandidate(
  id: string,
  label: string,
  activityMs = 0,
  projectId = 'p1'
): PaletteCandidate {
  return {
    id: threadCandidateId(projectId, id),
    group: 'threads',
    label,
    searchFields: [label],
    secondaryLabel: 'Alpha',
    activityMs,
    target: { kind: 'thread', projectId, threadId: id, threadName: label },
  };
}

function ready(candidates: PaletteCandidate[], extra: Partial<GroupState> = {}): GroupState {
  return { status: 'ready', candidates, ...extra };
}

function manyThreads(n: number): PaletteCandidate[] {
  return Array.from({ length: n }, (_, i) =>
    threadCandidate(`t${i}`, `Thread ${String(i).padStart(2, '0')}`, n - i)
  );
}

async function mountPalette(
  groups: Partial<Record<PaletteGroup, GroupState>>
): Promise<ScionQuickPalette> {
  const el = document.createElement('scion-quick-palette');
  el.open = true;
  el.groups = groups;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function input(el: ScionQuickPalette): HTMLInputElement {
  return el.shadowRoot!.querySelector('#palette-query-input') as HTMLInputElement;
}

function activeText(el: ScionQuickPalette): string | null | undefined {
  return el.shadowRoot?.querySelector('.palette-option.active')?.textContent?.trim();
}

function press(el: ScionQuickPalette, key: string, extra: KeyboardEventInit = {}): void {
  input(el).dispatchEvent(
    new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...extra })
  );
}

afterEach(() => {
  document.body.innerHTML = '';
});

describe('Tab/Shift+Tab: group cycling in reading order', () => {
  it('Tab moves the active selection from Agents to Threads to People, then wraps back to Agents', async () => {
    const el = await mountPalette({
      agents: ready([agentCandidate('a1', 'Agent One')]),
      threads: ready([threadCandidate('t1', 'Thread One')]),
      people: ready([personCandidate('p1', 'Person One')]),
    });
    // Nothing is active on an empty query until the first Tab picks Agents.
    expect(activeText(el)).toBeUndefined();
    press(el, 'Tab');
    await el.updateComplete;
    expect(activeText(el)).toContain('Agent One');

    press(el, 'Tab');
    await el.updateComplete;
    expect(activeText(el)).toContain('Thread One');

    press(el, 'Tab');
    await el.updateComplete;
    expect(activeText(el)).toContain('Person One');

    press(el, 'Tab');
    await el.updateComplete;
    expect(activeText(el)).toContain('Agent One');
  });

  it('Shift+Tab cycles backward: Agents -> People -> Threads -> Agents', async () => {
    const el = await mountPalette({
      agents: ready([agentCandidate('a1', 'Agent One')]),
      threads: ready([threadCandidate('t1', 'Thread One')]),
      people: ready([personCandidate('p1', 'Person One')]),
    });
    // With nothing active, the first Shift+Tab picks the last group.
    expect(activeText(el)).toBeUndefined();

    press(el, 'Tab', { shiftKey: true });
    await el.updateComplete;
    expect(activeText(el)).toContain('Person One');

    press(el, 'Tab', { shiftKey: true });
    await el.updateComplete;
    expect(activeText(el)).toContain('Thread One');

    press(el, 'Tab', { shiftKey: true });
    await el.updateComplete;
    expect(activeText(el)).toContain('Agent One');

    // Wraps backward from the first group to the last.
    press(el, 'Tab', { shiftKey: true });
    await el.updateComplete;
    expect(activeText(el)).toContain('Person One');
  });

  it('Tab skips an empty group (no ranked matches) rather than landing on it', async () => {
    const el = await mountPalette({
      agents: ready([agentCandidate('a1', 'Agent One')]),
      threads: ready([]), // no candidates at all -> empty group
      people: ready([personCandidate('p1', 'Person One')]),
    });
    press(el, 'Tab');
    await el.updateComplete;
    expect(activeText(el)).toContain('Agent One');
    press(el, 'Tab');
    await el.updateComplete;
    // Threads has no matches, so Tab from Agents must land on People, not Threads.
    expect(activeText(el)).toContain('Person One');
  });

  it('a group with zero matches for the current query is skipped, but "joins the next traversal" once it has a match again', async () => {
    const el = await mountPalette({
      agents: ready([agentCandidate('a1', 'Zulu')]),
      threads: ready([threadCandidate('t1', 'Alpha thread')]),
      people: ready([personCandidate('p1', 'Zulu person')]),
    });
    input(el).value = 'zulu';
    input(el).dispatchEvent(new InputEvent('input'));
    await el.updateComplete;
    // Threads has no "zulu" match right now — Tab from Agents must skip it.
    expect(activeText(el)).toContain('Zulu');
    press(el, 'Tab');
    await el.updateComplete;
    expect(activeText(el)).toContain('Zulu person');

    // Now Threads gets a candidate that matches the current query — a later
    // Tab traversal must be able to reach it (recomputed fresh every press,
    // not cached from before the group had a match). Currently active is
    // People (the last group in reading order), so Tab first wraps to
    // Agents, then a second Tab reaches the newly nonempty Threads group.
    el.groups = {
      ...el.groups,
      threads: ready([threadCandidate('t2', 'Zulu thread')]),
    };
    await el.updateComplete;
    press(el, 'Tab');
    await el.updateComplete;
    expect(activeText(el)).toContain('Zulu');
    press(el, 'Tab');
    await el.updateComplete;
    expect(activeText(el)).toContain('Zulu thread');
  });

  it('Tab with matches in exactly one group keeps re-selecting that group (single-group wrap)', async () => {
    const el = await mountPalette({
      agents: ready([agentCandidate('a1', 'Only Agent')]),
    });
    press(el, 'Tab');
    await el.updateComplete;
    expect(activeText(el)).toContain('Only Agent');
  });

  it('Tab with zero matches anywhere consumes the key safely and leaves selection empty', async () => {
    const el = await mountPalette({
      agents: ready([agentCandidate('a1', 'Alpha')]),
    });
    input(el).value = 'no-such-thing';
    input(el).dispatchEvent(new InputEvent('input'));
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.palette-option.active')).toBeNull();

    expect(() => press(el, 'Tab')).not.toThrow();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.palette-option.active')).toBeNull();
  });

  it('Tab calls preventDefault and stopPropagation so Shoelace never sees an unhandled Tab trap it', async () => {
    const el = await mountPalette({
      agents: ready([agentCandidate('a1', 'Agent One')]),
      threads: ready([threadCandidate('t1', 'Thread One')]),
    });
    const event = new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true });
    let stopped = false;
    const originalStop = event.stopPropagation.bind(event);
    event.stopPropagation = () => {
      stopped = true;
      originalStop();
    };
    input(el).dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
    expect(stopped).toBe(true);
  });
});

describe('Up/Down: wraps within the active group only', () => {
  it('ArrowDown/ArrowUp move within the active group, never crossing into another group', async () => {
    const el = await mountPalette({
      agents: ready([
        agentCandidate('a1', 'Agent Alpha', 2),
        agentCandidate('a2', 'Agent Beta', 1),
      ]),
      people: ready([personCandidate('p1', 'Person One')]),
    });
    press(el, 'ArrowDown');
    await el.updateComplete;
    expect(activeText(el)).toContain('Agent Alpha');

    press(el, 'ArrowDown');
    await el.updateComplete;
    expect(activeText(el)).toContain('Agent Beta');

    // Wraps back to the first row of the *same* group (Agents), not into People.
    press(el, 'ArrowDown');
    await el.updateComplete;
    expect(activeText(el)).toContain('Agent Alpha');
  });

  it('ArrowUp from the first row of a group wraps to the last row of that same group', async () => {
    const el = await mountPalette({
      threads: ready([
        threadCandidate('t1', 'Thread Alpha', 2),
        threadCandidate('t2', 'Thread Beta', 1),
      ]),
      people: ready([personCandidate('p1', 'Person One')]),
    });
    press(el, 'ArrowDown');
    await el.updateComplete;
    expect(activeText(el)).toContain('Thread Alpha');
    press(el, 'ArrowUp');
    await el.updateComplete;
    expect(activeText(el)).toContain('Thread Beta');
  });

  it('with no active selection, ArrowDown chooses the *first* row of the first nonempty group in reading order', async () => {
    // No active row is the default on an empty query until the user picks
    // one. Setting `activeId` to an id that matches no candidate takes the
    // same branch, so this also covers a selection that no longer matches
    // anything. Two rows in the group (not one) so this discriminates
    // "first row" from "last row" — the ArrowUp test below covers the
    // mirror case.
    const el = await mountPalette({
      threads: ready([
        threadCandidate('t1', 'Thread Alpha', 2),
        threadCandidate('t2', 'Thread Beta', 1),
      ]),
      people: ready([personCandidate('p1', 'Person One')]),
    });
    (el as unknown as { activeId: string | null }).activeId = 'not-a-real-candidate-id';
    press(el, 'ArrowDown');
    await el.updateComplete;
    // Threads is first in reading order among {threads, people}, and Alpha
    // (index 0) is its first row, not Beta (index 1, the last row).
    expect(activeText(el)).toContain('Thread Alpha');
  });

  it('with no active selection, ArrowUp chooses the *last* row of the first nonempty group (mirrors wrap-backward semantics, unlike ArrowDown)', async () => {
    // Discriminates moveActive's `delta === 1 ? 0 : groupRanked.length - 1`
    // ternary in the no-active-match branch — ArrowDown and ArrowUp must not
    // coincidentally agree when the group has more than one row.
    const el = await mountPalette({
      threads: ready([
        threadCandidate('t1', 'Thread Alpha', 2),
        threadCandidate('t2', 'Thread Beta', 1),
      ]),
    });
    (el as unknown as { activeId: string | null }).activeId = 'not-a-real-candidate-id';
    press(el, 'ArrowUp');
    await el.updateComplete;
    expect(activeText(el)).toContain('Thread Beta');
  });
});

describe('Tab/Shift+Tab with no active selection', () => {
  // No active row is the default on an empty query until the user picks
  // one; as in the Up/Down tests above, an `activeId` that matches no
  // candidate takes the same branch (moveActiveGroup's
  // currentGroupIndex===-1 handling).
  it('Tab (forward) with no active selection lands on the first nonempty group', async () => {
    const el = await mountPalette({
      threads: ready([threadCandidate('t1', 'Thread Alpha')]),
      people: ready([personCandidate('p1', 'Person One')]),
    });
    (el as unknown as { activeId: string | null }).activeId = 'not-a-real-candidate-id';
    press(el, 'Tab');
    await el.updateComplete;
    expect(activeText(el)).toContain('Thread Alpha');
  });

  it('Shift+Tab (backward) with no active selection lands on the *last* nonempty group, not the first', async () => {
    const el = await mountPalette({
      threads: ready([threadCandidate('t1', 'Thread Alpha')]),
      people: ready([personCandidate('p1', 'Person One')]),
    });
    (el as unknown as { activeId: string | null }).activeId = 'not-a-real-candidate-id';
    press(el, 'Tab', { shiftKey: true });
    await el.updateComplete;
    expect(activeText(el)).toContain('Person One');
  });
});

describe('Show more: 10-row visible cap per group', () => {
  it('renders only the first 10 rows of a group with more than 10 matches, plus a show-more control', async () => {
    const el = await mountPalette({ threads: ready(manyThreads(15)) });
    const options = el.shadowRoot?.querySelectorAll('.palette-option');
    expect(options?.length).toBe(10);
    const showMore = el.shadowRoot?.querySelector('.palette-show-more sl-button');
    expect(showMore?.textContent).toContain('5 more');
  });

  it('clicking show-more reveals every remaining row in that group', async () => {
    const el = await mountPalette({ threads: ready(manyThreads(15)) });
    const showMore = el.shadowRoot?.querySelector('.palette-show-more sl-button') as HTMLElement;
    showMore.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await el.updateComplete;
    expect(el.shadowRoot?.querySelectorAll('.palette-option').length).toBe(15);
    expect(el.shadowRoot?.querySelector('.palette-show-more')).toBeNull();
  });

  it('Arrow navigation past the 10th row expands the group so the active option stays mounted', async () => {
    const el = await mountPalette({ threads: ready(manyThreads(15)) });
    // The 15 threads have descending activityMs (15..1). With no row
    // active, ArrowUp picks the last one (index 14, "Thread 14") — beyond
    // the initial 10-row cap.
    press(el, 'ArrowUp');
    await el.updateComplete;
    expect(el.shadowRoot?.querySelectorAll('.palette-option').length).toBe(15);
    expect(activeText(el)).toContain('Thread 14');
  });

  it('the visible cap boundary is inclusive: the 11th row (index 10) must expand, not just rows strictly past it', async () => {
    // Discriminates ensureGroupExpandedFor's `indexInGroup >= PALETTE_GROUP_VISIBLE_LIMIT`
    // from `>` — with exactly 11 rows, the last (index 10) sits precisely at
    // the boundary; the 15-row test above only ever lands on index 14, which
    // both operators already agree needs expanding.
    const el = await mountPalette({ threads: ready(manyThreads(11)) });
    press(el, 'ArrowUp'); // with no row active, picks the last row, index 10
    await el.updateComplete;
    expect(el.shadowRoot?.querySelectorAll('.palette-option').length).toBe(11);
    expect(activeText(el)).toContain('Thread 10');
  });

  it('Tab away from a large group never accidentally expands it (its own top match is always index 0, well within the cap)', async () => {
    const el = await mountPalette({
      agents: ready([agentCandidate('a1', 'Agent One')]),
      threads: ready(manyThreads(15)),
    });
    expect(() => press(el, 'Tab')).not.toThrow();
    await el.updateComplete;
    const threadsGroupEl = el.shadowRoot?.querySelector(
      '[aria-labelledby="palette-heading-threads"]'
    );
    expect(threadsGroupEl?.querySelectorAll('.palette-option').length).toBe(10);
  });

  it('a query edit collapses a previously-expanded group back to the 10-row cap', async () => {
    const el = await mountPalette({ threads: ready(manyThreads(15)) });
    const showMore = el.shadowRoot?.querySelector('.palette-show-more sl-button') as HTMLElement;
    showMore.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await el.updateComplete;
    expect(el.shadowRoot?.querySelectorAll('.palette-option').length).toBe(15);

    input(el).value = 'Thread 0';
    input(el).dispatchEvent(new InputEvent('input'));
    await el.updateComplete;
    // Back under the cap (10 matches: "Thread 00".."Thread 09"), and the
    // show-more state itself was reset (not just coincidentally under 10).
    expect(el.shadowRoot?.querySelectorAll('.palette-option').length).toBe(10);
  });

  it('reopening the palette resets a previously-expanded group back to the 10-row cap', async () => {
    const el = await mountPalette({ threads: ready(manyThreads(15)) });
    const showMore = el.shadowRoot?.querySelector('.palette-show-more sl-button') as HTMLElement;
    showMore.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await el.updateComplete;
    expect(el.shadowRoot?.querySelectorAll('.palette-option').length).toBe(15);

    el.open = false;
    await el.updateComplete;
    el.open = true;
    await el.updateComplete;

    expect(el.shadowRoot?.querySelectorAll('.palette-option').length).toBe(10);
    expect(el.shadowRoot?.querySelector('.palette-show-more')).not.toBeNull();
  });

  it('a background refresh that preserves a manual selection beyond the cap keeps it mounted', async () => {
    const el = await mountPalette({ threads: ready(manyThreads(15)) });
    press(el, 'ArrowUp'); // manually select the last row, "Thread 14"
    await el.updateComplete;
    expect(activeText(el)).toContain('Thread 14');

    // Simulate a background refresh: same candidates, new array reference.
    el.groups = { ...el.groups, threads: ready(manyThreads(15)) };
    await el.updateComplete;
    expect(activeText(el)).toContain('Thread 14');
    expect(el.shadowRoot?.querySelectorAll('.palette-option').length).toBe(15);
  });
});

describe('Threads incomplete-results notice', () => {
  it('a ready-but-incomplete Threads group shows a notice and a retry, while keeping its successful rows selectable', async () => {
    const el = await mountPalette({
      threads: ready([threadCandidate('t1', 'Thread One')], { incomplete: true }),
    });
    const notice = el.shadowRoot?.querySelector('.palette-group-incomplete');
    expect(notice).not.toBeNull();
    expect(notice?.textContent).toContain("couldn't load");
    // The successfully-loaded row is still rendered and selectable.
    expect(el.shadowRoot?.querySelector('.palette-option')?.textContent).toContain('Thread One');
  });

  it('a complete (incomplete=false/unset) ready group shows no incomplete notice', async () => {
    const el = await mountPalette({ threads: ready([threadCandidate('t1', 'Thread One')]) });
    expect(el.shadowRoot?.querySelector('.palette-group-incomplete')).toBeNull();
  });

  it("a *loading* group with a carried-over incomplete=true does not show the notice yet (the notice is gated on status==='ready', not just the flag)", async () => {
    const el = await mountPalette({
      threads: {
        status: 'loading',
        candidates: [threadCandidate('t1', 'Thread One')],
        incomplete: true,
      },
    });
    expect(el.shadowRoot?.querySelector('.palette-group-incomplete')).toBeNull();
  });

  it("the incomplete notice's retry button dispatches palette-retry for the Threads group", async () => {
    const el = await mountPalette({
      threads: ready([threadCandidate('t1', 'Thread One')], { incomplete: true }),
    });
    let retriedGroup: PaletteGroup | undefined;
    el.addEventListener('palette-retry', (e) => {
      retriedGroup = (e as CustomEvent<{ group: PaletteGroup }>).detail.group;
    });
    const retryButton = el.shadowRoot?.querySelector(
      '.palette-group-incomplete sl-button'
    ) as HTMLElement;
    retryButton.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    expect(retriedGroup).toBe('threads');
  });
});

describe('Multi-group status text', () => {
  function status(el: ScionQuickPalette): string | null | undefined {
    return el.shadowRoot?.querySelector('.palette-status')?.textContent?.trim();
  }

  it('reports a combined match count across every populated group', async () => {
    const el = await mountPalette({
      agents: ready([agentCandidate('a1', 'Agent One')]),
      threads: ready([threadCandidate('t1', 'Thread One'), threadCandidate('t2', 'Thread Two')]),
      people: ready([personCandidate('p1', 'Person One')]),
    });
    expect(status(el)).toBe('4 matching results');
  });

  it('reports "Loading…" when every populated group is still loading with no candidates yet', async () => {
    const el = await mountPalette({
      agents: { status: 'loading', candidates: [] },
      threads: { status: 'loading', candidates: [] },
    });
    expect(status(el)).toBe('Loading…');
  });

  it('also reports "Loading…", not a bare match count, when every loading group already has candidates — a group that is still loading has not settled yet, regardless of how many rows it already published', async () => {
    const el = await mountPalette({
      agents: { status: 'loading', candidates: [agentCandidate('a1', 'Agent One')] },
      threads: { status: 'loading', candidates: [threadCandidate('t1', 'Thread One')] },
    });
    expect(status(el)).toBe('Loading…');
  });

  it('announces a loading group as still loading, not a bare zero-match count, when the query only matches a page it has not published yet', async () => {
    // The scenario a progressively-publishing Agents group newly exposes: a
    // query can match zero of the rows loaded *so far* while the group is
    // still loading — this must read as "Agents still loading", not a final
    // "0 matching results" (which a user searching for a specific agent
    // would read as "it does not exist").
    const el = await mountPalette({
      agents: { status: 'loading', candidates: [agentCandidate('a1', 'Agent One')] },
      threads: ready([threadCandidate('t1', 'Thread One')]),
    });
    input(el).value = 'Thread One';
    input(el).dispatchEvent(new InputEvent('input'));
    await el.updateComplete;
    expect(status(el)).toContain('1 matching result');
    expect(status(el)).toContain('Agents still loading');
    expect(status(el)).not.toContain('0 matching');
  });

  it('does not call a ready-but-empty group "still loading"', async () => {
    const el = await mountPalette({
      agents: ready([agentCandidate('a1', 'Agent One')]),
      people: ready([]),
    });
    expect(status(el)).toBe('1 matching result');
    expect(status(el)).not.toContain('still loading');
  });

  it('reports a zero match count, not "Loading…", when every present group is ready but empty', async () => {
    const el = await mountPalette({
      agents: ready([]),
      people: ready([]),
    });
    expect(status(el)).toBe('0 matching results');
  });

  it('reports which groups are still loading alongside the count from groups that already resolved', async () => {
    const el = await mountPalette({
      agents: ready([agentCandidate('a1', 'Agent One')]),
      threads: { status: 'loading', candidates: [] },
    });
    expect(status(el)).toContain('1 matching result');
    expect(status(el)).toContain('Threads still loading');
  });

  it('joins multiple still-loading groups by name, in reading order', async () => {
    // The single-loading-group test above never exercises the `.join(', ')`
    // in paletteStatusText's multi-group branch — with only one element to
    // join, a changed separator or a changed order produces the identical
    // string. Two loading groups are required to actually distinguish it.
    const el = await mountPalette({
      agents: ready([agentCandidate('a1', 'Agent One')]),
      threads: { status: 'loading', candidates: [] },
      people: { status: 'loading', candidates: [] },
    });
    expect(status(el)).toBe('1 matching result; Threads, People still loading');
  });

  it('reports which groups failed to load alongside the count from groups that succeeded', async () => {
    const el = await mountPalette({
      agents: ready([agentCandidate('a1', 'Agent One')]),
      people: { status: 'error', candidates: [], error: 'boom' },
    });
    expect(status(el)).toContain('1 matching result');
    expect(status(el)).toContain('People failed to load');
  });
});

describe('ensureGroupExpandedFor: defensive guards (private method, direct invocation)', () => {
  // `setActiveId` only calls this with a real, currently-ranked candidate ID
  // in every reachable call path today, so a bogus ID has no reachable
  // sequence of public calls — invoked directly, same technique as the
  // no-active-selection tests above.
  it('a candidate ID not present in the ranked list is a safe no-op, not a throw or an expand', async () => {
    const el = await mountPalette({ threads: ready(manyThreads(15)) });
    const before = el.shadowRoot?.querySelectorAll('.palette-option').length;
    expect(() => {
      (el as unknown as { ensureGroupExpandedFor(id: string): void }).ensureGroupExpandedFor(
        'not-a-real-id'
      );
    }).not.toThrow();
    expect(el.shadowRoot?.querySelectorAll('.palette-option').length).toBe(before);
  });

  it("calling it again for an already-expanded group is a harmless no-op (equivalent to expandGroup's own idempotency, not a separate behavior)", async () => {
    const el = await mountPalette({ threads: ready(manyThreads(15)) });
    press(el, 'ArrowUp'); // expands threads once, landing on the last row
    await el.updateComplete;
    expect(el.shadowRoot?.querySelectorAll('.palette-option').length).toBe(15);

    const activeId = (el as unknown as { activeId: string | null }).activeId!;
    expect(() => {
      (el as unknown as { ensureGroupExpandedFor(id: string): void }).ensureGroupExpandedFor(
        activeId
      );
    }).not.toThrow();
    expect(el.shadowRoot?.querySelectorAll('.palette-option').length).toBe(15);
  });
});

// ===========================================================================
// The listbox-owned placeholder option for "every group finished loading with
// nothing to show" — a role=listbox requires at least one real role=option/
// role=group descendant (WAI-ARIA's required-owned-elements rule), which a
// query (or an empty index) matching nothing anywhere would otherwise leave
// unsatisfied, since each group's own "No matches"/error text carries no
// ARIA role of its own.
// ===========================================================================

function placeholderOption(el: ScionQuickPalette): Element | null {
  return el.shadowRoot!.querySelector('#palette-empty-overall');
}

function listboxOwns(el: ScionQuickPalette): string[] {
  return (
    el.shadowRoot!.querySelector('#palette-result-list')?.getAttribute('aria-owns') ?? ''
  ).split(' ');
}

describe('the "no results anywhere" listbox placeholder', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('is absent when at least one group has a real match', async () => {
    const el = await mountPalette({
      agents: ready([agentCandidate('a1', 'Agent One')]),
      people: ready([]),
    });
    expect(placeholderOption(el)).toBeNull();
  });

  it('is present when every present group is ready with zero candidates', async () => {
    const el = await mountPalette({ agents: ready([]), people: ready([]) });
    expect(placeholderOption(el)).not.toBeNull();
    expect(placeholderOption(el)?.getAttribute('role')).toBe('option');
    expect(placeholderOption(el)?.getAttribute('aria-disabled')).toBe('true');
    expect(placeholderOption(el)?.getAttribute('aria-selected')).toBe('false');
  });

  it('is absent while any present group is still loading, even if every other group is empty', async () => {
    const el = await mountPalette({
      agents: { status: 'loading', candidates: [] },
      people: ready([]),
    });
    expect(placeholderOption(el)).toBeNull();
  });

  it('is present once every group has finished loading, including an all-error case', async () => {
    const el = await mountPalette({
      agents: { status: 'error', candidates: [], error: 'boom' },
      people: ready([]),
    });
    expect(placeholderOption(el)).not.toBeNull();
  });

  it("is included in the listbox's aria-owns alongside every present group's region, in reading order, only when rendered", async () => {
    const elWithMatches = await mountPalette({
      agents: ready([agentCandidate('a1', 'Agent One')]),
    });
    expect(listboxOwns(elWithMatches)).toEqual(['palette-group-region-agents']);

    const elEmpty = await mountPalette({ agents: ready([]), people: ready([]) });
    expect(listboxOwns(elEmpty)).toEqual([
      'palette-group-region-agents',
      'palette-group-region-people',
      'palette-empty-overall',
    ]);
  });
});
