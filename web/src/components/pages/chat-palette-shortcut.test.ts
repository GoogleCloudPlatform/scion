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
 * Tests for the single Cmd/Ctrl+K shortcut owner in chat.ts: modifier/IME/
 * repeat guards, the terminal/xterm exclusion, the route and page-visibility
 * guards, the unrelated-modal guard, and dispatch to the legacy switcher vs.
 * the new palette under the rollout flag.
 *
 * happy-dom does not retarget events across shadow roots (see the
 * chat-switcher tests), so real composedPath()-through-shadow-DOM and
 * focus-restore assertions live in e2e/chat-palette (Chromium) instead.
 * These tests build the composedPath() arrays directly, which is a fact
 * about how the code consumes the event (it only ever calls
 * `e.composedPath()`), not a claim about shadow-DOM retargeting.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/main.js', () => ({
  navigateTo: vi.fn(),
  stateManager: new EventTarget(),
}));

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return {
    ...actual,
    apiFetch: vi.fn(() => Promise.resolve(new Response('{"agents":[]}', { status: 200 }))),
  };
});

let ScionPageChat: any;

beforeAll(async () => {
  const mod = await import('./chat.js');
  ScionPageChat = mod.ScionPageChat;
  expect(ScionPageChat).toBeDefined();
  // Connecting a page below (`document.body.appendChild`) runs
  // `connectedCallback`'s unawaited `initV2()`, which starts lazily
  // importing chat-space-rail/chat-members in the background and never
  // gets awaited by anything in this file. Importing them here, awaited,
  // warms the module cache so that background import resolves
  // near-instantly instead of running a first-time module transform that
  // can still be unresolved when this file's own tests finish and their
  // environment tears down. `chat-switcher.js` is a different import —
  // `togglePalette`'s first-open lazy load, not `initV2()`'s — and every
  // test below that reaches it already awaits it to completion; it is
  // warmed here too so that a connected page can never leave it in flight
  // at teardown, even if a future test reaches it without awaiting.
  await Promise.all([
    import('../shared/chat/chat-space-rail.js'),
    import('../shared/chat/chat-members.js'),
    import('../shared/chat/chat-switcher.js'),
  ]);
});

function makeKeydownEvent(
  overrides: Partial<{
    key: string;
    metaKey: boolean;
    ctrlKey: boolean;
    altKey: boolean;
    shiftKey: boolean;
    repeat: boolean;
    isComposing: boolean;
    defaultPrevented: boolean;
    path: Element[];
  }>
): any {
  const path = overrides.path ?? [];
  let prevented = overrides.defaultPrevented ?? false;
  return {
    key: overrides.key ?? 'k',
    metaKey: overrides.metaKey ?? false,
    ctrlKey: overrides.ctrlKey ?? false,
    altKey: overrides.altKey ?? false,
    shiftKey: overrides.shiftKey ?? false,
    repeat: overrides.repeat ?? false,
    isComposing: overrides.isComposing ?? false,
    get defaultPrevented() {
      return prevented;
    },
    preventDefault: () => {
      prevented = true;
    },
    composedPath: () => path,
  };
}

function createUnattachedPage(): any {
  const el = document.createElement('scion-page-chat') as any;
  el.pageData = { user: { id: 'user-me' } };
  return el;
}

/** A fake `sl-after-hide` event as if it came from the palette's own dialog. */
function ownDialogAfterHideEvent(): Event {
  const dialog = document.createElement('div');
  dialog.classList.add('palette-dialog');
  return { composedPath: () => [dialog] } as unknown as Event;
}

afterEach(() => {
  document.body.innerHTML = '';
  Object.defineProperty(document, 'hidden', { value: false, configurable: true });
});

/**
 * A page stubbed eligible on every guard *except* the one(s) the caller's
 * test is about to exercise: on /chat, visible, no unrelated modal. Calling
 * `createUnattachedPage()` directly, at the default (non-`/chat`) test
 * document URL, would let `_isOnChatRoute()` reject the event regardless of
 * whether the guard actually under test did anything — the same vacuity
 * that would otherwise mask the terminal-surface/visibility-call/isV2
 * guards. Every test below uses this fixture and ends with a positive
 * control (the identical event with only the condition under test flipped)
 * to prove the rest of the guard chain is actually live.
 */
function createEligiblePage(): any {
  const page = createUnattachedPage();
  vi.spyOn(page, '_isOnChatRoute').mockReturnValue(true);
  vi.spyOn(page, '_isPageVisible').mockReturnValue(true);
  vi.spyOn(page, '_isUnrelatedModalActive').mockReturnValue(false);
  return page;
}

describe('_handleGlobalKeydown: modifier/IME/repeat/key guards (eligible fixture + positive controls)', () => {
  it('ignores a key repeat, with a positive control', () => {
    const page = createEligiblePage();
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, repeat: true }));
    expect(toggleSwitcher).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, repeat: false }));
    expect(toggleSwitcher).toHaveBeenCalledTimes(1);
  });

  it('ignores IME composition, with a positive control', () => {
    const page = createEligiblePage();
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, isComposing: true }));
    expect(toggleSwitcher).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, isComposing: false }));
    expect(toggleSwitcher).toHaveBeenCalledTimes(1);
  });

  it('ignores an already-defaultPrevented event, with a positive control', () => {
    const page = createEligiblePage();
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, defaultPrevented: true }));
    expect(toggleSwitcher).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, defaultPrevented: false }));
    expect(toggleSwitcher).toHaveBeenCalledTimes(1);
  });

  it('ignores Alt held alongside the modifier, with a positive control', () => {
    const page = createEligiblePage();
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, altKey: true }));
    expect(toggleSwitcher).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, altKey: false }));
    expect(toggleSwitcher).toHaveBeenCalledTimes(1);
  });

  it('ignores Shift held alongside the modifier, with a positive control', () => {
    const page = createEligiblePage();
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ ctrlKey: true, shiftKey: true }));
    expect(toggleSwitcher).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ ctrlKey: true, shiftKey: false }));
    expect(toggleSwitcher).toHaveBeenCalledTimes(1);
  });

  it('ignores neither Ctrl nor Meta held, with a positive control', () => {
    const page = createEligiblePage();
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({}));
    expect(toggleSwitcher).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(toggleSwitcher).toHaveBeenCalledTimes(1);
  });

  it('ignores both Ctrl and Meta held at once (some IMEs), with a positive control', () => {
    const page = createEligiblePage();
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, ctrlKey: true }));
    expect(toggleSwitcher).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(toggleSwitcher).toHaveBeenCalledTimes(1);
  });

  it('ignores keys other than k, with a positive control', () => {
    const page = createEligiblePage();
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ key: 'j', metaKey: true }));
    expect(toggleSwitcher).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ key: 'k', metaKey: true }));
    expect(toggleSwitcher).toHaveBeenCalledTimes(1);
  });

  it('accepts uppercase K (Shift/Caps Lock does not itself block eligibility via key casing)', () => {
    // No shiftKey here — Shift itself is a separate, already-tested guard
    // above; this only proves key.toLowerCase() is used for the comparison.
    const page = createEligiblePage();
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ key: 'K', metaKey: true }));
    expect(toggleSwitcher).toHaveBeenCalledTimes(1);
  });
});

describe('_eventFromTerminalSurface', () => {
  it('detects a scion-terminal-pane ancestor in the composed path', () => {
    const page = createUnattachedPage();
    const terminalPane = document.createElement('scion-terminal-pane');
    expect(page._eventFromTerminalSurface(makeKeydownEvent({ path: [terminalPane] }))).toBe(true);
  });

  it('detects an xterm container ancestor in the composed path', () => {
    const page = createUnattachedPage();
    const xtermDiv = document.createElement('div');
    xtermDiv.classList.add('xterm');
    expect(page._eventFromTerminalSurface(makeKeydownEvent({ path: [xtermDiv] }))).toBe(true);
  });

  it('does not flag an unrelated composer textarea', () => {
    const page = createUnattachedPage();
    const textarea = document.createElement('textarea');
    expect(page._eventFromTerminalSurface(makeKeydownEvent({ path: [textarea] }))).toBe(false);
  });

  it('blocks the shortcut on an otherwise-fully-eligible page (on /chat, visible, no modal) when the event originates in the terminal', () => {
    // Push to /chat first so the route guard passes, so only
    // `_eventFromTerminalSurface`'s own check is being exercised, with a
    // positive control proving the listener is still alive for an
    // identical event outside the terminal.
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/chat');
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    const terminalPane = document.createElement('scion-terminal-pane');

    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, path: [terminalPane] }));
    expect(toggleSwitcher).not.toHaveBeenCalled();

    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, path: [] }));
    expect(toggleSwitcher).toHaveBeenCalledTimes(1);
  });
});

describe('the _isPageVisible() call site in _handleGlobalKeydown, isolated from the route guard', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('blocks the shortcut when on /chat but a real ancestor is hidden, with a positive control', () => {
    // _isPageVisible() itself is already unit-tested in isolation above;
    // this proves its *use* as a guard in _handleGlobalKeydown specifically,
    // on a route where the route guard alone would otherwise pass — in the
    // Chromium hidden-chat scenario the page is always also off-route, which
    // masks this guard.
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/chat');
    const ancestor = document.createElement('div');
    ancestor.hidden = true;
    ancestor.appendChild(page);
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);

    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(toggleSwitcher).not.toHaveBeenCalled();

    ancestor.hidden = false;
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(toggleSwitcher).toHaveBeenCalledTimes(1);
  });
});

describe('the isV2 guard in _handleGlobalKeydown', () => {
  it('v1 (isV2 false) never toggles the palette/switcher or calls preventDefault, even on /chat, visible, flag on', () => {
    // Asserting only that no switcher element renders proves nothing about
    // this guard (v1 never renders one). Without it, v1 Ctrl+K would call
    // preventDefault (stealing the browser's native Ctrl+K) and still run
    // togglePalette (lazy import + agents/DM GETs).
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/chat');
    page.isV2 = false;
    page.isPaletteEnabled = true;
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    const event = makeKeydownEvent({ metaKey: true });

    page._handleGlobalKeydown(event);

    expect(togglePalette).not.toHaveBeenCalled();
    expect(toggleSwitcher).not.toHaveBeenCalled();
    expect(event.defaultPrevented).toBe(false);

    // Positive control: flip isV2 back on, identical event does toggle and preventDefault.
    page.isV2 = true;
    const event2 = makeKeydownEvent({ metaKey: true });
    page._handleGlobalKeydown(event2);
    expect(togglePalette).toHaveBeenCalledTimes(1);
    expect(event2.defaultPrevented).toBe(true);
  });
});

describe('_isOnChatRoute', () => {
  it('accepts /chat and any route below it', () => {
    const page = createUnattachedPage();
    for (const path of ['/chat', '/chat/', '/chat/dm/abc', '/chat/space/p1/thread/t1']) {
      window.history.pushState({}, '', path);
      expect(page._isOnChatRoute()).toBe(true);
    }
  });

  it('rejects an unrelated route (e.g. /terminals)', () => {
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/terminals');
    expect(page._isOnChatRoute()).toBe(false);
  });

  it('blocks the shortcut end-to-end off the chat route', () => {
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/terminals');
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(toggleSwitcher).not.toHaveBeenCalled();
    window.history.pushState({}, '', '/chat');
  });
});

describe('_isPageVisible', () => {
  it('is false when document.hidden is true', () => {
    const page = createUnattachedPage();
    Object.defineProperty(document, 'hidden', { value: true, configurable: true });
    expect(page._isPageVisible()).toBe(false);
  });

  it('is false when an ancestor carries the hidden attribute', () => {
    const page = createUnattachedPage();
    const ancestor = document.createElement('div');
    ancestor.hidden = true;
    ancestor.appendChild(page);
    expect(page._isPageVisible()).toBe(false);
  });

  it('is true for a connected, unhidden page', () => {
    const page = createUnattachedPage();
    expect(page._isPageVisible()).toBe(true);
  });
});

describe('_isUnrelatedModalActive: live DOM query', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('is false with nothing open', () => {
    const page = createUnattachedPage();
    expect(page._isUnrelatedModalActive()).toBe(false);
  });

  it('is true for a dialog rendered already open ("born open"), never having fired sl-show', () => {
    // Several real chat dialogs (attachment preview, interagent marker,
    // emoji picker) render as `<sl-dialog open>` behind a conditional
    // rather than transitioning — Shoelace never fires sl-show for those.
    const page = createUnattachedPage();
    const dialog = document.createElement('sl-dialog') as HTMLElement & { open?: boolean };
    dialog.open = true; // set before connecting: genuinely "born open"
    document.body.appendChild(dialog);
    expect(page._isUnrelatedModalActive()).toBe(true);
  });

  it('is true for a native <dialog open>, even nested inside a shadow root', () => {
    const page = createUnattachedPage();
    const host = document.createElement('div');
    const shadow = host.attachShadow({ mode: 'open' });
    const dialog = document.createElement('dialog');
    dialog.setAttribute('open', '');
    shadow.appendChild(dialog);
    document.body.appendChild(host);
    expect(page._isUnrelatedModalActive()).toBe(true);
  });

  it('goes false again once the open dialog is disconnected — no leaked state', () => {
    const page = createUnattachedPage();
    const dialog = document.createElement('sl-dialog') as HTMLElement & { open?: boolean };
    dialog.open = true;
    document.body.appendChild(dialog);
    expect(page._isUnrelatedModalActive()).toBe(true);
    dialog.remove(); // disconnected while still "open" — no sl-after-hide fires
    expect(page._isUnrelatedModalActive()).toBe(false);
  });

  it('blocks the shortcut end-to-end while an unrelated dialog is open', () => {
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/chat');
    const dialog = document.createElement('sl-dialog') as HTMLElement & { open?: boolean };
    dialog.open = true;
    document.body.appendChild(dialog);
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(toggleSwitcher).not.toHaveBeenCalled();
  });

  it('the shortcut works again for the flag-off legacy switcher once that dialog is removed', () => {
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/chat');
    const dialog = document.createElement('sl-dialog') as HTMLElement & { open?: boolean };
    dialog.open = true;
    document.body.appendChild(dialog);
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(toggleSwitcher).not.toHaveBeenCalled();

    dialog.remove();
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(toggleSwitcher).toHaveBeenCalledTimes(1);
  });

  it('does not count the switcher/palette own dialog (inside its shadow root) as an unrelated modal', () => {
    const page = createUnattachedPage();
    const switcherEl = document.createElement('scion-chat-switcher');
    const shadow = switcherEl.attachShadow({ mode: 'open' });
    const ownDialog = document.createElement('sl-dialog') as HTMLElement & { open?: boolean };
    ownDialog.open = true;
    shadow.appendChild(ownDialog);
    document.body.appendChild(switcherEl);
    Object.defineProperty(page, '_switcherEl', { value: switcherEl, configurable: true });

    expect(page._isUnrelatedModalActive()).toBe(false);
  });
});

describe('shortcut dispatch: legacy switcher vs. palette', () => {
  it('dispatches to toggleSwitcher when the palette flag is off', () => {
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/chat');
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(toggleSwitcher).toHaveBeenCalledTimes(1);
    expect(togglePalette).not.toHaveBeenCalled();
  });

  it('dispatches to togglePalette when the palette flag is on', () => {
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/chat');
    page.isPaletteEnabled = true;
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(togglePalette).toHaveBeenCalledTimes(1);
    expect(toggleSwitcher).not.toHaveBeenCalled();
  });

  it('preventDefault is called once eligibility is established', () => {
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/chat');
    vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);
    const event = makeKeydownEvent({ metaKey: true });
    page._handleGlobalKeydown(event);
    expect(event.defaultPrevented).toBe(true);
  });

  it('a second toggle while open just flips `open` — a real close animation is exercised in Chromium e2e', async () => {
    const page = createUnattachedPage();
    page.v2PaletteOpen = true;
    await page.togglePalette();
    expect(page.v2PaletteOpen).toBe(false);
  });

  it('a second Ctrl+K during the first-open lazy import cancels the pending open instead of opening twice', async () => {
    const page = createUnattachedPage();
    // `await this.updateComplete` inside togglePalette's first-open branch
    // never resolves for a page that is never connected (Lit's update cycle
    // needs a real connect), so this test — unlike most others in this file
    // — connects the page for real.
    document.body.appendChild(page);
    expect(page.v2SwitcherLoaded).toBe(false);
    const captureSpy = vi.spyOn(page, '_capturePaletteInvokerFocus');

    // Neither call is awaited individually — both presses land while the
    // first press's `await loadChatSwitcher()` is still pending, reproducing
    // the exact race this guards against.
    const first = page.togglePalette();
    const second = page.togglePalette();
    await Promise.all([first, second]);

    expect(page.v2PaletteOpen).toBe(false);
    // Only the first press should ever capture the invoker's focus — a
    // second call re-entering the open path (instead of cancelling it)
    // would call this twice.
    expect(captureSpy).toHaveBeenCalledTimes(1);
  });

  it('after a cancelled pending open, a fresh Ctrl+K opens normally', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    await Promise.all([page.togglePalette(), page.togglePalette()]);
    expect(page.v2PaletteOpen).toBe(false);

    await page.togglePalette();

    expect(page.v2PaletteOpen).toBe(true);
  });

  it('disconnecting the page while a first-open lazy import is in flight cancels the pending open', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);

    const opening = page.togglePalette();
    // Disconnect before the lazy import + updateComplete resolve — the same
    // race the double-press guard above handles for a second keypress, but
    // from disconnect instead. disconnectedCallback clears
    // _palettePendingOpen, so the
    // suspended togglePalette call sees the same "cancelled" signal a
    // second press would have left and backs out instead of setting
    // v2PaletteOpen / starting the watchdog / issuing the agents fetch on a
    // page no longer in the document.
    page.remove();
    expect(page._palettePendingOpen).toBe(false);

    await opening;

    expect(page.v2PaletteOpen).toBe(false);
  });
});

describe('no duplicate document keydown listener after reconnect/disconnect', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('a keydown after disconnect+reconnect toggles exactly once, not twice', () => {
    const page = createUnattachedPage();
    vi.spyOn(page, '_isOnChatRoute').mockReturnValue(true);
    vi.spyOn(page, '_isPageVisible').mockReturnValue(true);
    vi.spyOn(page, '_isUnrelatedModalActive').mockReturnValue(false);
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);

    // Connect, disconnect, reconnect — main.ts's navigateTo recreates the
    // page this way on every route change. A listener added on every
    // connect without a matching removal on disconnect would fire twice per
    // keydown after this cycle.
    document.body.appendChild(page);
    document.body.removeChild(page);
    document.body.appendChild(page);

    document.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'k', ctrlKey: true, bubbles: true })
    );

    expect(toggleSwitcher).toHaveBeenCalledTimes(1);
  });

  it('a disconnected (removed) page does not react to a keydown at all', () => {
    const page = createUnattachedPage();
    vi.spyOn(page, '_isOnChatRoute').mockReturnValue(true);
    vi.spyOn(page, '_isPageVisible').mockReturnValue(true);
    vi.spyOn(page, '_isUnrelatedModalActive').mockReturnValue(false);
    const toggleSwitcher = vi.spyOn(page, 'toggleSwitcher').mockResolvedValue(undefined);

    document.body.appendChild(page);
    document.body.removeChild(page);

    document.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'k', ctrlKey: true, bubbles: true })
    );

    expect(toggleSwitcher).not.toHaveBeenCalled();
  });
});

describe('palette selection: stale-candidate guard', () => {
  it('does not navigate when the selected candidate is no longer present in the current group', () => {
    const page = createUnattachedPage();
    const openDM = vi.spyOn(page, 'openDM').mockImplementation(() => {});
    page.v2PaletteGroups = { agents: { status: 'ready', candidates: [] } };
    page._handlePaletteSelect({
      detail: {
        target: { kind: 'dm', peerKind: 'agent', peerId: 'gone', displayName: 'Gone Bot' },
      },
    } as any);
    expect(openDM).not.toHaveBeenCalled();
  });

  it('a rejected stale candidate takes the invoker-restore path, not the "focus new composer" path', () => {
    // If the "closed by selection" flag were set before the stale-candidate
    // check, sl-after-hide would try to focus a composer that openDM never
    // actually opened — so it must stay false here, leaving the
    // invoker-restore path to run instead.
    const page = createUnattachedPage();
    vi.spyOn(page, 'openDM').mockImplementation(() => {});
    page.v2PaletteGroups = { agents: { status: 'ready', candidates: [] } };
    page._handlePaletteSelect({
      detail: { target: { kind: 'dm', peerKind: 'agent', peerId: 'gone', displayName: 'Gone' } },
    } as any);
    expect(page._paletteClosedBySelection).toBe(false);
  });

  it('an actual navigation does set the "closed by selection" flag', () => {
    const page = createUnattachedPage();
    vi.spyOn(page, 'openDM').mockImplementation(() => {});
    page.v2PaletteGroups = {
      agents: {
        status: 'ready',
        candidates: [
          {
            id: '["dm","agent","a1"]',
            group: 'agents',
            label: 'Coder',
            searchFields: ['Coder'],
            secondaryLabel: '',
            activityMs: 0,
            target: { kind: 'dm', peerKind: 'agent', peerId: 'a1', displayName: 'Coder' },
          },
        ],
      },
    };
    page._handlePaletteSelect({
      detail: { target: { kind: 'dm', peerKind: 'agent', peerId: 'a1', displayName: 'Coder' } },
    } as any);
    expect(page._paletteClosedBySelection).toBe(true);
  });

  it('navigates via openDM when the candidate is still present', () => {
    const page = createUnattachedPage();
    const openDM = vi.spyOn(page, 'openDM').mockImplementation(() => {});
    page.v2PaletteGroups = {
      agents: {
        status: 'ready',
        candidates: [
          {
            id: '["dm","agent","a1"]',
            group: 'agents',
            label: 'Coder',
            searchFields: ['Coder'],
            secondaryLabel: '',
            activityMs: 0,
            target: { kind: 'dm', peerKind: 'agent', peerId: 'a1', displayName: 'Coder' },
          },
        ],
      },
    };
    page._handlePaletteSelect({
      detail: { target: { kind: 'dm', peerKind: 'agent', peerId: 'a1', displayName: 'Coder' } },
    } as any);
    expect(openDM).toHaveBeenCalledWith('a1', 'agent', 'Coder');
  });
});

describe('_handlePaletteAfterHide is filtered to the owned dialog', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('ignores an sl-after-hide whose real origin is not the palette dialog', () => {
    const page = createUnattachedPage();
    const restoreSpy = vi.spyOn(page, '_restorePaletteInvokerFocus');
    const focusComposerSpy = vi.spyOn(page, '_focusComposerAfterPaletteSelection');
    page._paletteClosedBySelection = true; // would normally route to focusing the composer

    const somethingElse = document.createElement('sl-tooltip');
    page._handlePaletteAfterHide({ composedPath: () => [somethingElse] } as unknown as Event);

    expect(restoreSpy).not.toHaveBeenCalled();
    expect(focusComposerSpy).not.toHaveBeenCalled();
    // Neither dismissal flag was consumed by the ignored event.
    expect(page._paletteClosedBySelection).toBe(true);
  });

  it('handles an sl-after-hide whose real origin is the palette dialog', () => {
    const page = createUnattachedPage();
    const restoreSpy = vi.spyOn(page, '_restorePaletteInvokerFocus').mockImplementation(() => {});

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());

    expect(restoreSpy).toHaveBeenCalledTimes(1);
  });
});

describe('palette focus capture/restore: textarea selection', () => {
  it('captures and restores a textarea selection range around the palette open', () => {
    const page = createUnattachedPage();
    const textarea = document.createElement('textarea');
    textarea.value = 'hello world';
    document.body.appendChild(textarea);
    textarea.focus();
    textarea.setSelectionRange(2, 5, 'forward');

    page._capturePaletteInvokerFocus();

    // Simulate the palette stealing focus while open.
    const other = document.createElement('input');
    document.body.appendChild(other);
    other.focus();

    page._restorePaletteInvokerFocus();

    expect(document.activeElement).toBe(textarea);
    expect(textarea.selectionStart).toBe(2);
    expect(textarea.selectionEnd).toBe(5);
    expect(textarea.selectionDirection).toBe('forward');
  });

  it('falls back to a visible page element when the invoker has disappeared', () => {
    const page = createUnattachedPage();
    const textarea = document.createElement('textarea');
    document.body.appendChild(textarea);
    textarea.focus();
    page._capturePaletteInvokerFocus();
    textarea.remove(); // invoker disconnected while the palette was open

    // `_focusPaletteFallback` (a trivial shadowRoot query + focus()) is
    // exercised directly for real in the Chromium e2e fixture, where the
    // page is actually connected; this test only proves the delegation.
    const fallbackSpy = vi.spyOn(page, '_focusPaletteFallback').mockImplementation(() => {});
    page._restorePaletteInvokerFocus();
    expect(fallbackSpy).toHaveBeenCalled();
  });

  // Note: happy-dom implements neither `checkVisibility()` nor real layout
  // (its `getClientRects()` always returns one stub rect regardless of CSS),
  // so a connected element actually hidden by CSS can't be distinguished
  // from a connected-and-visible one here — that scenario is covered by the
  // Chromium e2e fixture instead. `_isInvokerVisible`'s own branching is
  // still unit-tested below by stubbing `checkVisibility`/`getClientRects`
  // directly on the element.

  it('restores focus to a visible position:fixed invoker instead of falling back', () => {
    // `offsetParent` is null both for a genuinely hidden element and for a
    // `position: fixed` one (per spec), so a visibility test built on
    // `offsetParent === null` alone always treats a fixed invoker as hidden
    // and wrongly falls back instead of restoring it. Simulating a fixed
    // invoker here (`offsetParent` stubbed to null, `checkVisibility`
    // stubbed to true, the way a real visible `position: fixed` element
    // reports itself) proves restore reads `checkVisibility()`, not
    // `offsetParent`.
    const page = createUnattachedPage();
    const input = document.createElement('input');
    document.body.appendChild(input);
    input.focus();
    Object.defineProperty(input, 'offsetParent', { value: null });
    (input as unknown as { checkVisibility: () => boolean }).checkVisibility = () => true;

    page._capturePaletteInvokerFocus();
    const other = document.createElement('textarea');
    document.body.appendChild(other);
    other.focus();

    const fallbackSpy = vi.spyOn(page, '_focusPaletteFallback').mockImplementation(() => {});
    page._restorePaletteInvokerFocus();

    expect(fallbackSpy).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(input);
  });

  it('passes visibilityProperty and checkVisibilityCSS to checkVisibility, so a CSS-hidden invoker is not missed', () => {
    // checkVisibility() does not check the CSS `visibility` property by
    // default — without `visibilityProperty` (and its older alias
    // `checkVisibilityCSS`, for engines that predate the rename), a
    // `visibility: hidden` invoker would report itself as visible and
    // restore would call `.focus()` on an element that cannot actually
    // receive it, silently leaving focus wherever it already was instead of
    // falling back to a real focusable target.
    const page = createUnattachedPage();
    const input = document.createElement('input');
    document.body.appendChild(input);
    input.focus();
    const checkVisibilitySpy = vi.fn().mockReturnValue(true);
    (input as unknown as { checkVisibility: () => boolean }).checkVisibility = checkVisibilitySpy;

    page._capturePaletteInvokerFocus();
    const other = document.createElement('textarea');
    document.body.appendChild(other);
    other.focus();

    page._restorePaletteInvokerFocus();

    expect(checkVisibilitySpy).toHaveBeenCalledWith({
      visibilityProperty: true,
      checkVisibilityCSS: true,
    });
  });

  it('falls back when checkVisibility reports the invoker as hidden', () => {
    const page = createUnattachedPage();
    const input = document.createElement('input');
    document.body.appendChild(input);
    input.focus();
    (input as unknown as { checkVisibility: () => boolean }).checkVisibility = () => false;

    page._capturePaletteInvokerFocus();
    const other = document.createElement('textarea');
    document.body.appendChild(other);
    other.focus();

    const fallbackSpy = vi.spyOn(page, '_focusPaletteFallback').mockImplementation(() => {});
    page._restorePaletteInvokerFocus();

    expect(fallbackSpy).toHaveBeenCalled();
    expect(document.activeElement).not.toBe(input);
  });

  it('falls back to the getClientRects check when checkVisibility is not implemented', () => {
    // No `checkVisibility` property at all (the happy-dom/legacy-engine
    // case): an invoker with no client rects (no layout box, e.g.
    // `display: none`) must still fall back.
    const page = createUnattachedPage();
    const input = document.createElement('input');
    document.body.appendChild(input);
    input.focus();
    expect((input as unknown as { checkVisibility?: unknown }).checkVisibility).toBeUndefined();
    vi.spyOn(input, 'getClientRects').mockReturnValue([] as unknown as DOMRectList);

    page._capturePaletteInvokerFocus();
    const other = document.createElement('textarea');
    document.body.appendChild(other);
    other.focus();

    const fallbackSpy = vi.spyOn(page, '_focusPaletteFallback').mockImplementation(() => {});
    page._restorePaletteInvokerFocus();

    expect(fallbackSpy).toHaveBeenCalled();
    expect(document.activeElement).not.toBe(input);
  });

  it('captures and restores a text <input> selection range around the palette open', () => {
    // Without the `HTMLInputElement` branch, `_capturePaletteInvokerFocus`
    // would only recognize HTMLTextAreaElement, so an <input> invoker would
    // always capture `_paletteInvokerSelection = null`, and restore's own
    // instanceof guard would then never call `setSelectionRange`. A DOM
    // element's selection otherwise persists across an unrelated focus
    // change on its own, with no help from this code — so the range is
    // deliberately perturbed here (to (0, 0)) between capture and restore.
    // Only an actual restore call can put it back to (2, 5); without a
    // restore it stays at (0, 0).
    const page = createUnattachedPage();
    const input = document.createElement('input');
    input.value = 'hello world';
    document.body.appendChild(input);
    input.focus();
    input.setSelectionRange(2, 5, 'forward');

    page._capturePaletteInvokerFocus();

    const other = document.createElement('textarea');
    document.body.appendChild(other);
    other.focus();
    input.setSelectionRange(0, 0);

    page._restorePaletteInvokerFocus();

    expect(document.activeElement).toBe(input);
    expect(input.selectionStart).toBe(2);
    expect(input.selectionEnd).toBe(5);
    expect(input.selectionDirection).toBe('forward');
  });

  it('attempts and safely swallows setSelectionRange on an <input> type that does not support selection', () => {
    // `type="number"` inputs reject selection entirely: `setSelectionRange`
    // always throws a DOMException on them regardless of arguments.
    // Without the `HTMLInputElement` branch, restore's
    // `el instanceof HTMLTextAreaElement` guard excludes HTMLInputElement,
    // so `setSelectionRange` is never even attempted on an <input> invoker —
    // asserting the call happened (not just "did not throw", which also
    // holds when it's never attempted at all) is what actually discriminates
    // this behavior.
    const page = createUnattachedPage();
    const input = document.createElement('input');
    input.type = 'number';
    document.body.appendChild(input);
    input.focus();

    page._capturePaletteInvokerFocus();

    const other = document.createElement('textarea');
    document.body.appendChild(other);
    other.focus();
    const setSelectionRangeSpy = vi.spyOn(input, 'setSelectionRange');

    expect(() => page._restorePaletteInvokerFocus()).not.toThrow();

    expect(setSelectionRangeSpy).toHaveBeenCalled();
    expect(document.activeElement).toBe(input);
  });

  it('treats a throwing selectionStart getter the same as one that returns null', () => {
    // Older engines (pre-2016 spec) threw an InvalidStateError reading
    // `selectionStart`/`selectionEnd`/`selectionDirection` on a
    // selection-unsupported input type; current ones (and happy-dom) return
    // null instead — `_readInvokerSelection`'s try/catch must swallow the
    // throwing case too, not just a null return.
    const page = createUnattachedPage();
    const input = document.createElement('input');
    document.body.appendChild(input);
    Object.defineProperty(input, 'selectionStart', {
      get(): number {
        throw new DOMException('selectionStart is not supported', 'InvalidStateError');
      },
    });

    const selection = page._readInvokerSelection(input);

    expect(selection).toBeNull();
  });
});

describe('palette visibility watchdog: catches pushState-based route hides', () => {
  afterEach(() => {
    vi.useRealTimers();
    document.body.innerHTML = '';
    window.history.pushState({}, '', '/chat');
  });

  it('closes the palette once the route stops being /chat, even without a popstate event', () => {
    // The terminal workspace transition uses history.pushState directly
    // (main.ts), which does not fire popstate itself — only actual
    // back/forward navigation does. This reproduces that scenario, which the
    // watchdog exists to catch.
    vi.useFakeTimers();
    const page = createUnattachedPage();
    page._startPaletteVisibilityWatchdog();
    page.v2PaletteOpen = true;

    window.history.pushState({}, '', '/terminals/some-agent'); // no popstate fires
    expect(page.v2PaletteOpen).toBe(true); // not yet — the watchdog hasn't ticked

    vi.advanceTimersByTime(300);
    expect(page.v2PaletteOpen).toBe(false);
    expect(page._paletteSkipFocusRestore).toBe(true);
  });

  it('does nothing while the route stays on /chat', () => {
    vi.useFakeTimers();
    const page = createUnattachedPage();
    page._startPaletteVisibilityWatchdog();
    page.v2PaletteOpen = true;

    vi.advanceTimersByTime(1000);
    expect(page.v2PaletteOpen).toBe(true);
  });

  it('stops polling once the palette is closed some other way', () => {
    vi.useFakeTimers();
    const page = createUnattachedPage();
    page._startPaletteVisibilityWatchdog();
    page.v2PaletteOpen = false; // closed by some other path without going through the stop helper

    // Should self-cancel on its next tick rather than polling forever.
    vi.advanceTimersByTime(300);
    expect(page._paletteVisibilityWatchdog).toBeNull();
  });

  it('_stopPaletteVisibilityWatchdog prevents a pending tick from doing anything', () => {
    vi.useFakeTimers();
    const page = createUnattachedPage();
    page._startPaletteVisibilityWatchdog();
    page.v2PaletteOpen = true;
    window.history.pushState({}, '', '/terminals/some-agent');

    page._stopPaletteVisibilityWatchdog();
    vi.advanceTimersByTime(1000);

    expect(page.v2PaletteOpen).toBe(true); // never got the chance to close it
  });
});

describe('palette closes proactively on route change or another modal opening', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    window.history.pushState({}, '', '/chat');
  });

  it('popstate away from /chat closes an open palette without restoring focus', () => {
    const page = createUnattachedPage();
    page.v2PaletteOpen = true;
    window.history.pushState({}, '', '/terminals/some-agent');
    const restoreSpy = vi.spyOn(page, '_restorePaletteInvokerFocus');

    page._handlePopStateForPalette();

    expect(page.v2PaletteOpen).toBe(false);
    expect(page._paletteSkipFocusRestore).toBe(true);
    // Confirm the skip actually short-circuits sl-after-hide's restore path.
    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    expect(restoreSpy).not.toHaveBeenCalled();
  });

  it('popstate while still on /chat and visible does nothing', () => {
    const page = createUnattachedPage();
    page.v2PaletteOpen = true;
    window.history.pushState({}, '', '/chat/dm/some-key');

    page._handlePopStateForPalette();

    expect(page.v2PaletteOpen).toBe(true);
  });

  it('popstate while the palette is already closed is a no-op', () => {
    const page = createUnattachedPage();
    page.v2PaletteOpen = false;
    window.history.pushState({}, '', '/terminals/some-agent');

    page._handlePopStateForPalette();

    expect(page.v2PaletteOpen).toBe(false);
    expect(page._paletteSkipFocusRestore).toBe(false);
  });

  it('another dialog opening while the palette is open closes it without restoring focus', () => {
    const page = createUnattachedPage();
    page.v2PaletteOpen = true;
    const restoreSpy = vi.spyOn(page, '_restorePaletteInvokerFocus');
    const otherDialog = document.createElement('sl-dialog') as HTMLElement & { open?: boolean };
    otherDialog.open = true; // sl-show only fires after the open transition completes

    page._handleDocumentModalShow({ composedPath: () => [otherDialog] } as unknown as Event);

    expect(page.v2PaletteOpen).toBe(false);
    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    expect(restoreSpy).not.toHaveBeenCalled();
  });

  it('an open, non-contained drawer closes the palette', () => {
    const page = createUnattachedPage();
    page.v2PaletteOpen = true;
    const drawer = document.createElement('sl-drawer') as HTMLElement & { open?: boolean };
    drawer.open = true;

    page._handleDocumentModalShow({ composedPath: () => [drawer] } as unknown as Event);

    expect(page.v2PaletteOpen).toBe(false);
  });

  it('a contained drawer (not a page-blocking modal) does not close the palette', () => {
    const page = createUnattachedPage();
    page.v2PaletteOpen = true;
    const drawer = document.createElement('sl-drawer') as HTMLElement & { open?: boolean };
    drawer.open = true;
    drawer.setAttribute('contained', '');

    page._handleDocumentModalShow({ composedPath: () => [drawer] } as unknown as Event);

    expect(page.v2PaletteOpen).toBe(true);
  });

  it('a toast (sl-alert) sl-show does not close the palette or drop focus', () => {
    const page = createUnattachedPage();
    page.v2PaletteOpen = true;
    const restoreSpy = vi.spyOn(page, '_restorePaletteInvokerFocus');
    const toast = document.createElement('sl-alert');

    page._handleDocumentModalShow({ composedPath: () => [toast] } as unknown as Event);

    expect(page.v2PaletteOpen).toBe(true);
    expect(page._paletteSkipFocusRestore).toBe(false);
    expect(restoreSpy).not.toHaveBeenCalled();
  });

  it('sl-tooltip/sl-dropdown/sl-details/sl-select sl-show do not close the palette', () => {
    const page = createUnattachedPage();
    for (const tag of ['sl-tooltip', 'sl-dropdown', 'sl-details', 'sl-select']) {
      page.v2PaletteOpen = true;
      const el = document.createElement(tag);
      page._handleDocumentModalShow({ composedPath: () => [el] } as unknown as Event);
      expect(page.v2PaletteOpen).toBe(true);
    }
  });

  it('the palette opening its own dialog does not close itself', () => {
    const page = createUnattachedPage();
    page.v2PaletteOpen = true;
    const switcherEl = document.createElement('scion-chat-switcher');
    Object.defineProperty(page, '_switcherEl', { value: switcherEl, configurable: true });
    const ownDialog = document.createElement('sl-dialog');

    page._handleDocumentModalShow({
      composedPath: () => [ownDialog, switcherEl],
    } as unknown as Event);

    expect(page.v2PaletteOpen).toBe(true);
  });

  it('an unrelated dialog opening while the palette is already closed is a no-op', () => {
    const page = createUnattachedPage();
    page.v2PaletteOpen = false;
    const otherDialog = document.createElement('sl-dialog') as HTMLElement & { open?: boolean };
    otherDialog.open = true;

    page._handleDocumentModalShow({ composedPath: () => [otherDialog] } as unknown as Event);

    expect(page.v2PaletteOpen).toBe(false);
  });
});

describe('palette load is cancelled on every close path', () => {
  it('togglePalette (toggle-close) cancels the data controller', () => {
    const page = createUnattachedPage();
    page.v2PaletteOpen = true;
    const cancelSpy = vi.spyOn(page._paletteDataController, 'cancel');
    void page.togglePalette();
    expect(cancelSpy).toHaveBeenCalled();
  });

  it('_handlePaletteDismiss (escape/backdrop/close) cancels the data controller', () => {
    const page = createUnattachedPage();
    const cancelSpy = vi.spyOn(page._paletteDataController, 'cancel');
    page._handlePaletteDismiss();
    expect(cancelSpy).toHaveBeenCalled();
  });

  it('_handlePaletteSelect (committed selection) cancels the data controller', () => {
    const page = createUnattachedPage();
    vi.spyOn(page, 'openDM').mockImplementation(() => {});
    const cancelSpy = vi.spyOn(page._paletteDataController, 'cancel');
    page.v2PaletteGroups = { agents: { status: 'ready', candidates: [] } };
    page._handlePaletteSelect({
      detail: { target: { kind: 'dm', peerKind: 'agent', peerId: 'gone', displayName: 'Gone' } },
    } as any);
    expect(cancelSpy).toHaveBeenCalled();
  });

  it('_closePaletteWithoutFocusRestore (route/modal close) cancels the data controller', () => {
    const page = createUnattachedPage();
    page.v2PaletteOpen = true;
    const cancelSpy = vi.spyOn(page._paletteDataController, 'cancel');
    page._closePaletteWithoutFocusRestore();
    expect(cancelSpy).toHaveBeenCalled();
  });
});
