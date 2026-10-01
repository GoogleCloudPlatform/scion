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
 * Tests for <scion-chat-switcher>'s grouped palette rendering: the sl-dialog
 * presentation, keyboard model, and per-group states.
 *
 * happy-dom does not retarget events across shadow roots, so real
 * composedPath()/focus assertions live in e2e/chat-palette (Chromium)
 * instead. These tests cover pure state/rendering behavior that happy-dom
 * can observe directly.
 */

// @vitest-environment happy-dom

import { describe, it, expect, afterEach } from 'vitest';

await import('./chat-switcher.js');
type ScionChatSwitcher = import('./chat-switcher.js').ScionChatSwitcher;
import type { GroupState, PaletteTarget } from '../../../client/chat-palette-types.js';
import { dmCandidateId } from '../../../client/chat-palette-types.js';

function agentsGroup(
  candidates: Array<{
    peerId: string;
    label: string;
    activityMs?: number;
    // Optional; most tests don't need a secondary label.
    secondaryLabel?: string;
    searchFields?: string[];
  }>
): Record<'agents', GroupState> {
  return {
    agents: {
      status: 'ready',
      candidates: candidates.map((c) => ({
        id: dmCandidateId('agent', c.peerId),
        group: 'agents',
        label: c.label,
        searchFields: c.searchFields ?? [c.label],
        secondaryLabel: c.secondaryLabel ?? '',
        activityMs: c.activityMs ?? 0,
        target: { kind: 'dm', peerKind: 'agent', peerId: c.peerId, displayName: c.label },
      })),
    },
  };
}

async function mountPalette(groups?: Record<'agents', GroupState>): Promise<ScionChatSwitcher> {
  const el = document.createElement('scion-chat-switcher') as ScionChatSwitcher;
  el.open = true;
  if (groups) el.groups = groups;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

describe('scion-chat-switcher: renders a grouped Agents list', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('renders an sl-dialog with a combobox input and an Agents group heading', async () => {
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Coder One' }]));
    expect(el.shadowRoot?.querySelector('sl-dialog')).not.toBeNull();
    const input = el.shadowRoot?.querySelector('#palette-query-input');
    expect(input).not.toBeNull();
    expect(input?.getAttribute('role')).toBe('combobox');
    const heading = el.shadowRoot?.querySelector('.palette-group-heading');
    expect(heading?.textContent).toContain('Agents');
  });

  it('shows a loading state while the group is loading', async () => {
    const el = await mountPalette({ agents: { status: 'loading', candidates: [] } });
    expect(el.shadowRoot?.querySelector('.palette-loading')).not.toBeNull();
  });

  it('shows an error state with a retry control', async () => {
    const el = await mountPalette({
      agents: { status: 'error', candidates: [], error: 'boom' },
    });
    const errorEl = el.shadowRoot?.querySelector('.palette-group-error');
    expect(errorEl?.textContent).toContain('boom');
    expect(el.shadowRoot?.querySelector('.palette-group-error sl-button')).not.toBeNull();
    expect(el.shadowRoot?.querySelector('.palette-loading')).toBeNull();
  });

  it('a background refresh (status loading, candidates already present) shows the stale candidates, not the spinner', async () => {
    // `renderPaletteGroup`'s loading-spinner condition is `status ===
    // 'loading' && candidates.length === 0` — stale-while-revalidate: only
    // show the spinner on a genuine first load, not a background refresh of
    // a group that already has results. Covers the `candidates.length ===
    // 0` half independently of the loading+empty test above: dropping
    // either half alone still shows the spinner for that case.
    const el = await mountPalette({
      agents: {
        status: 'loading',
        candidates: [
          {
            id: dmCandidateId('agent', 'a1'),
            group: 'agents',
            label: 'Coder One',
            searchFields: ['Coder One'],
            secondaryLabel: '',
            activityMs: 0,
            target: { kind: 'dm', peerKind: 'agent', peerId: 'a1', displayName: 'Coder One' },
          },
        ],
      },
    });
    expect(el.shadowRoot?.querySelector('.palette-loading')).toBeNull();
    expect(el.shadowRoot?.querySelector('.palette-option')?.textContent).toContain('Coder One');
  });

  it('"No matches" only shows once ready with zero ranked candidates — not while loading, not on error, not with matches', async () => {
    // The "No matches" condition is `status !== 'loading' && status !==
    // 'error' && ranked.length === 0` — three independent conditions, so
    // dropping any one condition fails a case below.
    // loading, zero candidates: spinner, not "No matches".
    let el = await mountPalette({ agents: { status: 'loading', candidates: [] } });
    expect(el.shadowRoot?.querySelector('.palette-empty')).toBeNull();
    el.remove();

    // error, zero candidates: error message, not "No matches".
    el = await mountPalette({ agents: { status: 'error', candidates: [], error: 'boom' } });
    expect(el.shadowRoot?.querySelector('.palette-empty')).toBeNull();
    el.remove();

    // ready, non-zero ranked: the candidate list, not "No matches".
    el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Coder One' }]));
    expect(el.shadowRoot?.querySelector('.palette-empty')).toBeNull();
    el.remove();

    // ready, zero ranked: "No matches" (the only true case).
    el = await mountPalette({ agents: { status: 'ready', candidates: [] } });
    expect(el.shadowRoot?.querySelector('.palette-empty')).not.toBeNull();
    expect(el.shadowRoot?.querySelector('.palette-loading')).toBeNull();
  });

  it('a candidate without a secondaryLabel renders no secondary line at all', async () => {
    // `renderPaletteGroup`'s secondary-line block is entirely conditional on
    // `r.candidate.secondaryLabel` being truthy — covers its absence for a
    // candidate with none.
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Coder One' }]));
    expect(el.shadowRoot?.querySelector('.palette-secondary')).toBeNull();
  });

  it('the global best (first ranked candidate) is auto-selected on open', async () => {
    const el = await mountPalette(
      agentsGroup([
        { peerId: 'old', label: 'Old Bot', activityMs: 1 },
        { peerId: 'new', label: 'New Bot', activityMs: 1000 },
      ])
    );
    const active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('New Bot');
  });

  it("aria-activedescendant matches the active option's own id, and each option keeps a stable id across re-renders", async () => {
    // domIdFor caches one DOM id per candidate id specifically so the
    // *same* id is returned both times it's called in a
    // single render pass — once for the active option's own `id=` attribute
    // (renderPaletteGroup) and once for the query input's
    // `aria-activedescendant` (renderPalette). Without the cache, each call
    // increments the counter independently, so `aria-activedescendant`
    // would point at a ghost id that no rendered option actually has, and a
    // candidate's `id` would change on every re-render (breaking anything
    // that referenced it, e.g. `aria-activedescendant` after a re-render
    // triggered by something unrelated to that candidate).
    const el = await mountPalette(
      agentsGroup([
        { peerId: 'a1', label: 'Alpha', activityMs: 2 },
        { peerId: 'a2', label: 'Beta', activityMs: 1 },
      ])
    );
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    const activeOption = () => el.shadowRoot?.querySelector('.palette-option.active');

    const idBefore = activeOption()?.id;
    expect(idBefore).toBeTruthy();
    expect(input.getAttribute('aria-activedescendant')).toBe(idBefore);

    // Force an unrelated re-render (moving away and back) and confirm the
    // same candidate keeps the same id rather than getting a fresh one.
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true }));
    await el.updateComplete;

    const idAfter = activeOption()?.id;
    expect(idAfter).toBe(idBefore);
    expect(input.getAttribute('aria-activedescendant')).toBe(idAfter);
  });

  it('editing the query resets the selection to the new global best, discarding a prior manual pick', async () => {
    // reconcileActiveId's `if (queryChanged) { manualSelection = false;
    // activeId = ranked[0]...; return; }` branch resets to the new global
    // best on every query edit, even one that still matches the
    // manually-picked candidate — without this branch, the other branch's
    // `ranked.some(...)` check alone would keep the stale manual pick
    // instead.
    const el = await mountPalette(
      agentsGroup([
        { peerId: 'a1', label: 'Coder One', activityMs: 1 },
        { peerId: 'a2', label: 'Reviewer Bot', activityMs: 100 },
      ])
    );
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    let active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Reviewer Bot'); // global best (higher activityMs) on open

    // Manually select Coder One.
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;
    active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Coder One');

    // Both labels still match "o" (same tier), but Reviewer Bot outranks
    // Coder One within that tier (activityMs tiebreak) — a query edit must
    // snap back to it, not keep the earlier manual pick.
    input.value = 'o';
    input.dispatchEvent(new InputEvent('input'));
    await el.updateComplete;
    active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Reviewer Bot');
  });

  it('a group refresh (not a query edit) updates the auto-selection to track the new global best', async () => {
    // Isolates the `this.manualSelection` half of reconcileActiveId's
    // `if (this.manualSelection && ranked.some(...)) return;` from the
    // `ranked.some(...)` half (next test): with manualSelection still
    // false (user never pressed an arrow key),
    // a *non*-query-edit ranked-list change (e.g. a group finishing a
    // background refresh) must still track the new global best, not freeze
    // on whichever candidate happened to be active before the refresh.
    const el = await mountPalette(
      agentsGroup([
        { peerId: 'a1', label: 'Coder One', activityMs: 100 },
        { peerId: 'a2', label: 'Reviewer Bot', activityMs: 1 },
      ])
    );
    let active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Coder One'); // global best

    // A group refresh that keeps both candidates but changes their
    // recency, so Reviewer Bot becomes the new global best. This is a
    // `groups` change, not a `queryText` change.
    el.groups = agentsGroup([
      { peerId: 'a1', label: 'Coder One', activityMs: 1 },
      { peerId: 'a2', label: 'Reviewer Bot', activityMs: 100 },
    ]);
    await el.updateComplete;

    active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Reviewer Bot');
  });

  it('a group refresh that removes the manually-selected candidate falls back to the new global best', async () => {
    // Isolates the `ranked.some(...)` half: with manualSelection true (the
    // other branch alone would otherwise "preserve" this selection
    // unconditionally), but the manually-picked candidate no longer exists
    // in the refreshed ranked list (e.g. the agent became non-viable), the
    // guard must fall through to the new global best rather than leave
    // `activeId` pointing at a candidate that no longer exists — which
    // would also make a subsequent Enter silently no-op via
    // commitActivePaletteCandidate's own `if (!active) return;`.
    const el = await mountPalette(
      agentsGroup([
        { peerId: 'a1', label: 'Coder One', activityMs: 100 },
        { peerId: 'a2', label: 'Reviewer Bot', activityMs: 1 },
      ])
    );
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    let active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Coder One');

    // Manually select Reviewer Bot.
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;
    active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Reviewer Bot');

    // Refresh removes Reviewer Bot entirely.
    el.groups = agentsGroup([{ peerId: 'a1', label: 'Coder One', activityMs: 100 }]);
    await el.updateComplete;

    active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Coder One');
  });

  it('typing narrows the list and highlights the match', async () => {
    const el = await mountPalette(
      agentsGroup([
        { peerId: 'a1', label: 'Coder One' },
        { peerId: 'a2', label: 'Reviewer Bot' },
      ])
    );
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    input.value = 'cod';
    input.dispatchEvent(new InputEvent('input'));
    await el.updateComplete;

    const options = el.shadowRoot?.querySelectorAll('.palette-option');
    expect(options?.length).toBe(1);
    expect(options?.[0].querySelector('mark')?.textContent).toBe('Cod');
  });

  it('renders a query or a label containing markup as text, never as HTML', async () => {
    const maliciousLabel = '<img src=x onerror=alert(1)> <b>Bold</b> Bot';
    const el = await mountPalette({
      agents: {
        status: 'ready',
        candidates: [
          {
            id: dmCandidateId('agent', 'a1'),
            group: 'agents',
            label: maliciousLabel,
            searchFields: [maliciousLabel],
            secondaryLabel: '<script>window.__pwned = true</script>',
            activityMs: 0,
            target: { kind: 'dm', peerKind: 'agent', peerId: 'a1', displayName: maliciousLabel },
          },
        ],
      },
    });

    // Type a query that itself contains markup and matches via subsequence.
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    input.value = '<b>';
    input.dispatchEvent(new InputEvent('input'));
    await el.updateComplete;

    const option = el.shadowRoot?.querySelector('.palette-option');
    // No actual markup elements were ever created from either the query or the label/secondary text.
    expect(option?.querySelector('img, script, b, i, svg')).toBeNull();
    expect((window as unknown as { __pwned?: boolean }).__pwned).toBeUndefined();
    // The raw text — including angle brackets — round-trips as plain text.
    expect(option?.textContent).toContain(maliciousLabel);
    expect(option?.textContent).toContain('<script>window.__pwned = true</script>');
    // The matched substring is still wrapped in our own <mark>, as text.
    const mark = option?.querySelector('mark');
    expect(mark).not.toBeNull();
    expect(mark?.textContent).toBe('<b>');
    expect(mark?.querySelector('*')).toBeNull();
  });

  it('the query input is described by a visible keyboard-help region', async () => {
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Coder One' }]));
    const input = el.shadowRoot?.querySelector('#palette-query-input');
    expect(input?.getAttribute('aria-describedby')).toBe('palette-keyboard-help');
    const help = el.shadowRoot?.querySelector('#palette-keyboard-help');
    expect(help).not.toBeNull();
    expect(help?.textContent).toContain('navigate');
    expect(help?.textContent).toContain('open');
    expect(help?.textContent).toContain('close');
  });

  it('the status region announces the ranked match count for the current query, not the raw group size', async () => {
    const el = await mountPalette(
      agentsGroup([
        { peerId: 'a1', label: 'Coder One' },
        { peerId: 'a2', label: 'Reviewer Bot' },
      ])
    );
    const status = () => el.shadowRoot?.querySelector('.palette-status');
    expect(status()?.getAttribute('role')).toBe('status');
    expect(status()?.textContent?.trim()).toBe('2 matching agents');

    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    input.value = 'cod';
    input.dispatchEvent(new InputEvent('input'));
    await el.updateComplete;

    // Narrowed to one match — the raw group still has 2 candidates, so this
    // proves the count tracks the ranked/filtered list, not group size.
    expect(status()?.textContent?.trim()).toBe('1 matching agent');
  });

  it('the status region says "Loading agents…" while loading and "Agents failed to load." on error, not a match count', async () => {
    // Covers the `statusText` computation's loading/error branches by
    // asserting the status region's own text directly, distinct from the
    // loading spinner / error message divs.
    let el = await mountPalette({ agents: { status: 'loading', candidates: [] } });
    expect(el.shadowRoot?.querySelector('.palette-status')?.textContent?.trim()).toBe(
      'Loading agents…'
    );
    el.remove();

    el = await mountPalette({ agents: { status: 'error', candidates: [], error: 'boom' } });
    expect(el.shadowRoot?.querySelector('.palette-status')?.textContent?.trim()).toBe(
      'Agents failed to load.'
    );
  });

  it('aria-activedescendant is absent when there is no active candidate', async () => {
    // Covers the falsy branch of the `activeDomId` ternary
    // (`this.activeId ? this.domIdFor(this.activeId) : undefined`).
    const el = await mountPalette({ agents: { status: 'ready', candidates: [] } });
    const input = el.shadowRoot?.querySelector('#palette-query-input');
    expect(input?.hasAttribute('aria-activedescendant')).toBe(false);
  });

  it('Tab resets the active selection to the first ranked candidate', async () => {
    const el = await mountPalette(
      agentsGroup([
        { peerId: 'a1', label: 'Alpha', activityMs: 1 },
        { peerId: 'a2', label: 'Beta', activityMs: 2 },
      ])
    );
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;

    // Beta is the global best (higher activityMs) and starts active; move
    // away from it first so this test can actually distinguish "Tab did
    // something" from "nothing changed" (a fixed active candidate that
    // happens to equal ranked[0] both before and after Tab would pass even
    // if Tab were a no-op).
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;
    let active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Alpha');

    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true })
    );
    await el.updateComplete;

    active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Beta');
  });

  it('Tab with no candidates does not throw', async () => {
    // This is a no-throw smoke test only, not a discriminating one. By the
    // time Tab is pressed,
    // `willUpdate`'s `reconcileActiveId` has already nulled `activeId` on the
    // query-change render triggered by narrowing to zero matches (asserted
    // below, *before* Tab), and Tab's own
    // `ranked.length === 0 -> activeId = null` branch is unreachable in
    // the current architecture for the same reason (see chat-switcher.ts's
    // `reconcileActiveId`), so removing just that inner assignment leaves
    // this test green. What this test actually verifies is that the Tab
    // handler's other code (`this.rankedPaletteCandidates`,
    // `ranked.length === 0` check itself) does not throw when the ranked
    // list is empty.
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Alpha' }]));
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    input.value = 'no-such-agent';
    input.dispatchEvent(new InputEvent('input'));
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.palette-option.active')).toBeNull();

    expect(() => {
      input.dispatchEvent(
        new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true })
      );
    }).not.toThrow();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.palette-option.active')).toBeNull();
  });

  it('Arrow keys move the active selection and wrap', async () => {
    const el = await mountPalette(
      agentsGroup([
        { peerId: 'a1', label: 'Alpha', activityMs: 2 },
        { peerId: 'a2', label: 'Beta', activityMs: 1 },
      ])
    );
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    let active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Alpha');

    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;
    active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Beta');

    // Wraps back around.
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;
    active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Alpha');
  });

  it('ArrowUp moves the active selection backward and wraps (the test above only ever presses ArrowDown)', async () => {
    const el = await mountPalette(
      agentsGroup([
        { peerId: 'a1', label: 'Alpha', activityMs: 2 },
        { peerId: 'a2', label: 'Beta', activityMs: 1 },
      ])
    );
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    let active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Alpha');

    // Wraps backward from the first candidate to the last.
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true }));
    await el.updateComplete;
    active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Beta');

    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true }));
    await el.updateComplete;
    active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Alpha');
  });

  it('ArrowDown/ArrowUp with zero matches do not throw and leave no active option', async () => {
    // moveActive's `if (ranked.length === 0) return;` guard
    // is reachable in practice — type a query that matches nothing, then
    // press an arrow key — and without it `ranked[0].candidate` (or
    // `ranked[ranked.length - 1].candidate`) throws a TypeError from inside
    // the keydown handler.
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Alpha' }]));
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    input.value = 'no-such-agent';
    input.dispatchEvent(new InputEvent('input'));
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.palette-option.active')).toBeNull();

    expect(() => {
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    }).not.toThrow();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.palette-option.active')).toBeNull();

    expect(() => {
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true }));
    }).not.toThrow();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.palette-option.active')).toBeNull();
  });

  it('Enter commits the active candidate as palette-select with its target', async () => {
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Coder One' }]));
    let detailTarget: PaletteTarget | undefined;
    el.addEventListener('palette-select', (e) => {
      detailTarget = (e as CustomEvent<{ target: PaletteTarget }>).detail.target;
    });
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    expect(detailTarget).toEqual({
      kind: 'dm',
      peerKind: 'agent',
      peerId: 'a1',
      displayName: 'Coder One',
    });
  });

  it('Enter is a no-op, not a throw or a garbage commit, if activeId ever points at a candidate no longer in the ranked list', async () => {
    // commitActivePaletteCandidate's own `if (!active) return;`
    // is a defensive backstop: the two tests above
    // show `reconcileActiveId` keeps `activeId` in sync with the ranked
    // list on every render that could desync it (a query edit or a group
    // refresh), so in the current architecture this specific line should
    // never see a stale id in practice. It stays cheap insurance against a
    // future regression in that reconciliation (or a future call site that
    // bypasses it) rather than crashing or dispatching a
    // `palette-select` with a `target` from a candidate that no longer
    // exists. Sets `activeId` directly (bypassing the public API, since no
    // reachable sequence of public calls currently produces this state) to
    // exercise the guard in isolation.
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Coder One' }]));
    (el as unknown as { activeId: string | null }).activeId = 'not-a-real-candidate-id';
    let commits = 0;
    el.addEventListener('palette-select', () => {
      commits++;
    });
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    expect(() => {
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    }).not.toThrow();
    expect(commits).toBe(0);
  });

  it('a repeated Enter (key repeat) does not double-commit', async () => {
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Coder One' }]));
    let commits = 0;
    el.addEventListener('palette-select', () => {
      commits++;
    });
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, repeat: true })
    );
    expect(commits).toBe(1);
  });

  it('a held-Enter repeat is rejected even as the very first Enter seen this open (isolated from the committed-once guard)', async () => {
    // In the test above, the *second* Enter has `repeat: true`, so
    // `handlePaletteKeydown`'s own `if (e.repeat) return;` rejects it
    // before `commitActivePaletteCandidate` — and its separate `committed`
    // flag — are ever reached at all. `committed` is not what blocks that
    // second Enter; `e.repeat` is (see "a second, ordinary (non-repeat)
    // Enter after a commit does not commit again" below for one that
    // actually isolates `committed`). This test isolates the
    // repeat check in the other direction: it mounts fresh (nothing has
    // committed yet this open) and sends only a single Enter whose
    // `repeat` is already true — the scenario where a user was already
    // holding Enter down when the palette opened via some other trigger.
    // Only `handlePaletteKeydown`'s own repeat check can reject this one.
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Coder One' }]));
    let commits = 0;
    el.addEventListener('palette-select', () => {
      commits++;
    });
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, repeat: true })
    );
    expect(commits).toBe(0);
  });

  it('a second, ordinary (non-repeat) Enter after a commit does not commit again', async () => {
    // Isolates `commitActivePaletteCandidate`'s own `if (this.committed)
    // return;` from `handlePaletteKeydown`'s `if (e.repeat) return;`: a
    // second Enter marked `repeat: true` never reaches the `committed`
    // guard at all, since the *other* guard already rejects it first. A
    // real double-tapped Enter
    // (or Enter followed by a row click before the dialog's close
    // animation finishes) delivers two perfectly ordinary, non-repeat
    // keydowns, which only `committed` can reject.
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Coder One' }]));
    let commits = 0;
    el.addEventListener('palette-select', () => {
      commits++;
    });
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    expect(commits).toBe(1);
  });

  it('Enter during IME composition does not commit', async () => {
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Coder One' }]));
    let commits = 0;
    el.addEventListener('palette-select', () => {
      commits++;
    });
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    input.dispatchEvent(new Event('compositionstart', { bubbles: true }));
    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, isComposing: true })
    );
    expect(commits).toBe(0);
  });

  it('Enter is rejected by the internal composing flag alone, even if isComposing is already false (some engines deliver the confirming Enter this way)', async () => {
    // The Enter case guards on `this.composing || e.isComposing` — the test
    // above sets both at once, so neither half is discriminated from the
    // other. This isolates the internal `composing` flag (set by our own
    // compositionstart/compositionend handlers): a real compositionstart
    // fired, but the Enter keydown itself arrives with `isComposing: false`
    // — some engines report the composition as already ended by the time
    // the confirming Enter's keydown fires, even though compositionend
    // hasn't been dispatched yet.
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Coder One' }]));
    let commits = 0;
    el.addEventListener('palette-select', () => {
      commits++;
    });
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    input.dispatchEvent(new Event('compositionstart', { bubbles: true }));
    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, isComposing: false })
    );
    expect(commits).toBe(0);
  });

  it('Enter is rejected by isComposing alone, even with no preceding compositionstart', async () => {
    // Isolates the other half: `e.isComposing` true with no compositionstart
    // ever dispatched on this element (e.g. composition started before the
    // palette itself gained focus, so this element never saw its own
    // compositionstart event, but the browser still reports the keydown as
    // mid-composition).
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Coder One' }]));
    let commits = 0;
    el.addEventListener('palette-select', () => {
      commits++;
    });
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, isComposing: true })
    );
    expect(commits).toBe(0);
  });

  it('clicking a row commits it', async () => {
    const el = await mountPalette(
      agentsGroup([
        { peerId: 'a1', label: 'Alpha' },
        { peerId: 'a2', label: 'Beta' },
      ])
    );
    let detailTarget: PaletteTarget | undefined;
    el.addEventListener('palette-select', (e) => {
      detailTarget = (e as CustomEvent<{ target: PaletteTarget }>).detail.target;
    });
    const options = el.shadowRoot?.querySelectorAll('.palette-option');
    (options?.[1] as HTMLElement).click();
    expect(detailTarget?.peerId).toBe('a2');
  });

  it('Escape reaching sl-request-close dispatches palette-dismiss with reason escape', async () => {
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Alpha' }]));
    let reason = '';
    el.addEventListener('palette-dismiss', (e) => {
      reason = (e as CustomEvent<{ reason: string }>).detail.reason;
    });
    const dialog = el.shadowRoot?.querySelector('sl-dialog');
    dialog?.dispatchEvent(
      new CustomEvent('sl-request-close', {
        detail: { source: 'keyboard' },
        bubbles: true,
        composed: true,
      })
    );
    expect(reason).toBe('escape');
  });

  it('sl-request-close from the overlay dispatches palette-dismiss with reason backdrop', async () => {
    // Covers handlePaletteRequestClose's source-to-reason ternary for the
    // 'overlay' branch; the Chromium "backdrop click restores deep focus"
    // test exercises this path end to end but
    // never asserts the *reason* value itself.
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Alpha' }]));
    let reason = '';
    el.addEventListener('palette-dismiss', (e) => {
      reason = (e as CustomEvent<{ reason: string }>).detail.reason;
    });
    const dialog = el.shadowRoot?.querySelector('sl-dialog');
    dialog?.dispatchEvent(
      new CustomEvent('sl-request-close', {
        detail: { source: 'overlay' },
        bubbles: true,
        composed: true,
      })
    );
    expect(reason).toBe('backdrop');
  });

  it('sl-request-close from the close button dispatches palette-dismiss with reason close', async () => {
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Alpha' }]));
    let reason = '';
    el.addEventListener('palette-dismiss', (e) => {
      reason = (e as CustomEvent<{ reason: string }>).detail.reason;
    });
    const dialog = el.shadowRoot?.querySelector('sl-dialog');
    dialog?.dispatchEvent(
      new CustomEvent('sl-request-close', {
        detail: { source: 'close-button' },
        bubbles: true,
        composed: true,
      })
    );
    expect(reason).toBe('close');
  });

  it('sl-request-close bubbling from a nested element (not the dialog itself) is ignored', async () => {
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Alpha' }]));
    let dismissed = false;
    el.addEventListener('palette-dismiss', () => {
      dismissed = true;
    });
    const dialog = el.shadowRoot?.querySelector('sl-dialog') as HTMLElement;
    const nested = document.createElement('div');
    dialog.appendChild(nested);

    nested.dispatchEvent(
      new CustomEvent('sl-request-close', {
        detail: { source: 'keyboard' },
        bubbles: true,
        composed: true,
      })
    );

    expect(dismissed).toBe(false);
  });

  it('sl-initial-focus bubbling from a nested element does not refocus the query input', async () => {
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Alpha' }]));
    const decoy = document.createElement('button');
    document.body.appendChild(decoy);
    decoy.focus();
    const dialog = el.shadowRoot?.querySelector('sl-dialog') as HTMLElement;
    const nested = document.createElement('div');
    dialog.appendChild(nested);

    nested.dispatchEvent(new CustomEvent('sl-initial-focus', { bubbles: true, composed: true }));
    await el.updateComplete;

    expect(document.activeElement).toBe(decoy);
    decoy.remove();
  });

  it('retry dispatches palette-retry with the failed group', async () => {
    const el = await mountPalette({ agents: { status: 'error', candidates: [], error: 'boom' } });
    let retried = '';
    el.addEventListener('palette-retry', (e) => {
      retried = (e as CustomEvent<{ group: string }>).detail.group;
    });
    (el.shadowRoot?.querySelector('.palette-group-error sl-button') as HTMLElement)?.click();
    expect(retried).toBe('agents');
  });

  it('reopening resets the query text', async () => {
    const el = await mountPalette(agentsGroup([{ peerId: 'a1', label: 'Alpha' }]));
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    input.value = 'al';
    input.dispatchEvent(new InputEvent('input'));
    await el.updateComplete;
    expect((el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement).value).toBe(
      'al'
    );

    el.open = false;
    await el.updateComplete;
    el.open = true;
    await el.updateComplete;
    expect((el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement).value).toBe(
      ''
    );
  });

  it('a query that matches only the slug highlights the secondary line, not the label', async () => {
    const el = await mountPalette(
      agentsGroup([
        {
          peerId: 'a1',
          label: 'Coder One',
          secondaryLabel: 'coder-1',
          searchFields: ['Coder One', 'coder-1'],
        },
      ])
    );
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    // "r-1" is a substring of the slug "coder-1" but not of the label
    // "Coder One" (no hyphen or digit there), nor even a subsequence of it.
    input.value = 'r-1';
    input.dispatchEvent(new InputEvent('input'));
    await el.updateComplete;

    const option = el.shadowRoot?.querySelector('.palette-option');
    expect(option?.querySelector(':scope > div:not(.palette-secondary) mark')).toBeNull();
    const secondary = option?.querySelector('.palette-secondary');
    expect(secondary?.querySelector('mark')?.textContent).toBe('r-1');
  });

  it('a query that matches only the label highlights the label, not the secondary line (converse of the test above)', async () => {
    const el = await mountPalette(
      agentsGroup([
        {
          peerId: 'a1',
          label: 'Coder One',
          secondaryLabel: 'coder-1',
          searchFields: ['Coder One', 'coder-1'],
        },
      ])
    );
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    // "One" is a substring of the label but neither a substring nor a
    // subsequence of the slug "coder-1" (no "n" in it at all).
    input.value = 'One';
    input.dispatchEvent(new InputEvent('input'));
    await el.updateComplete;

    const option = el.shadowRoot?.querySelector('.palette-option');
    const secondary = option?.querySelector('.palette-secondary');
    expect(secondary?.querySelector('mark')).toBeNull();
    expect(option?.querySelector(':scope > div:not(.palette-secondary) mark')?.textContent).toBe(
      'One'
    );
  });

  it('exactly one [role=option][aria-selected=true] exists at a time and it follows ArrowDown', async () => {
    const el = await mountPalette(
      agentsGroup([
        { peerId: 'a1', label: 'Alpha' },
        { peerId: 'a2', label: 'Beta' },
        { peerId: 'a3', label: 'Gamma' },
      ])
    );
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    const selected = () =>
      Array.from(el.shadowRoot?.querySelectorAll('[role="option"]') ?? []).filter(
        (o) => o.getAttribute('aria-selected') === 'true'
      );

    let sel = selected();
    expect(sel).toHaveLength(1);
    expect(sel[0].textContent).toContain('Alpha');

    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;
    sel = selected();
    expect(sel).toHaveLength(1);
    expect(sel[0].textContent).toContain('Beta');
  });

  it('reopening with the same groups reference (no groups/query change) resets the active row to the new global best', async () => {
    // willUpdate's `changedKeys.has('queryText') || changed.has('groups') ||
    // changed.has('open')` (chat-switcher.ts) needs the `|| changed.has(
    // 'open')` half: the earlier "fresh open" block always resets
    // `manualSelection` to false, but does not itself touch `activeId` — so
    // without also re-running reconcileActiveId on open, a manually-selected
    // `activeId` from the previous open would survive, stale, into the next
    // one even though `manualSelection` now (incorrectly) reads false.
    const el = await mountPalette(
      agentsGroup([
        { peerId: 'a1', label: 'Alpha', activityMs: 1 },
        { peerId: 'a2', label: 'Beta', activityMs: 100 },
      ])
    );
    const groups = el.groups; // capture the exact reference; never reassigned below
    let active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Beta'); // global best on open

    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;
    active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Alpha'); // manual pick

    el.open = false;
    await el.updateComplete;
    el.open = true;
    await el.updateComplete;

    expect(el.groups).toBe(groups); // still the identical reference — no groups change occurred
    active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Beta'); // reset to the global best, not stuck on Alpha
  });

  it('shows the default "Failed to load." text when the group state carries no error message', async () => {
    const el = await mountPalette({ agents: { status: 'error', candidates: [] } });
    expect(el.shadowRoot?.querySelector('.palette-group-error')?.textContent).toContain(
      'Failed to load.'
    );
  });

  it('groups={} (every group absent) renders without throwing', async () => {
    // renderPaletteGroup's `if (!state) return nothing;` guard
    // (chat-switcher.ts) is reachable: `groups` is typed
    // `Partial<Record<PaletteGroup, GroupState>>` and is a public,
    // attribute-false property, so `{}` is a valid value reachable through
    // that public API even though the current sole caller (chat.ts) never
    // actually passes it. The guard is kept as a real, exercised defense,
    // not dead code.
    const el = await mountPalette({} as unknown as Record<'agents', GroupState>);
    expect(el.shadowRoot?.querySelectorAll('[role="group"]')).toHaveLength(0);
    expect(el.shadowRoot?.querySelectorAll('.palette-option')).toHaveLength(0);
  });

  it('moveActive with no current selection chooses the first row on ArrowDown and the last on ArrowUp', async () => {
    // reconcileActiveId always assigns a valid activeId whenever
    // ranked.length > 0 (see the reconciliation tests above), so
    // moveActive's own `currentIndex === -1` branch — "no current
    // selection, choose first/last" — is a defensive backstop not reachable
    // through any public sequence of calls today; the empty-ranked-list
    // case is caught earlier by `if (ranked.length === 0) return;` instead.
    // Sets `activeId` directly to exercise the branch in isolation,
    // matching the precedent set for commitActivePaletteCandidate's
    // `!active` guard elsewhere in this file.
    const el = await mountPalette(
      agentsGroup([
        { peerId: 'a1', label: 'Alpha' },
        { peerId: 'a2', label: 'Beta' },
        { peerId: 'a3', label: 'Gamma' },
      ])
    );
    const input = el.shadowRoot?.querySelector('#palette-query-input') as HTMLInputElement;

    (el as unknown as { activeId: string | null }).activeId = 'not-a-real-candidate-id';
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
    await el.updateComplete;
    let active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Alpha');

    (el as unknown as { activeId: string | null }).activeId = 'not-a-real-candidate-id';
    await el.updateComplete;
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true }));
    await el.updateComplete;
    active = el.shadowRoot?.querySelector('.palette-option.active');
    expect(active?.textContent).toContain('Gamma');
  });
});
