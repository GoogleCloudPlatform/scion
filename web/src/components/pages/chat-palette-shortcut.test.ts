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
 * guards, the unrelated-modal guard, and dispatch to the grouped palette.
 *
 * happy-dom does not retarget events across shadow roots (see the
 * quick-palette tests), so real composedPath()-through-shadow-DOM and
 * focus-restore assertions live in e2e/chat-palette (Chromium) instead.
 * These tests build the composedPath() arrays directly, which is a fact
 * about how the code consumes the event (it only ever calls
 * `e.composedPath()`), not a claim about shadow-DOM retargeting.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import { CHAT_PALETTE_OPEN_REQUEST_EVENT } from '../../client/chat-palette-events.js';
import { FakeEventSource } from '../../client/__fixtures__/agent-store-harness.js';
import { TOUCH_PRIMARY_QUERY } from '../../utils/input-modality.js';
import { PALETTE_TYPEAHEAD_MAX_MS } from '../shared/palette/palette-typeahead.js';

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
  // environment tears down. `quick-palette.js` is a different import —
  // `togglePalette`'s first-open lazy load, not `initV2()`'s — and every
  // test below that reaches it already awaits it to completion; it is
  // warmed here too so that a connected page can never leave it in flight
  // at teardown, even if a future test reaches it without awaiting.
  await Promise.all([
    import('../shared/chat/chat-space-rail.js'),
    import('../shared/chat/chat-members.js'),
    import('../shared/palette/quick-palette.js'),
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

// A connected page retains the agent store's hub list, which opens the
// store's feed; it never connects here.
beforeEach(() => {
  vi.stubGlobal('EventSource', FakeEventSource);
});

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
 * that would otherwise mask the terminal-surface/visibility-call
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
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, repeat: true }));
    expect(togglePalette).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, repeat: false }));
    expect(togglePalette).toHaveBeenCalledTimes(1);
  });

  it('ignores IME composition, with a positive control', () => {
    const page = createEligiblePage();
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, isComposing: true }));
    expect(togglePalette).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, isComposing: false }));
    expect(togglePalette).toHaveBeenCalledTimes(1);
  });

  it('ignores an already-defaultPrevented event, with a positive control', () => {
    const page = createEligiblePage();
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, defaultPrevented: true }));
    expect(togglePalette).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, defaultPrevented: false }));
    expect(togglePalette).toHaveBeenCalledTimes(1);
  });

  it('ignores Alt held alongside the modifier, with a positive control', () => {
    const page = createEligiblePage();
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, altKey: true }));
    expect(togglePalette).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, altKey: false }));
    expect(togglePalette).toHaveBeenCalledTimes(1);
  });

  it('ignores Shift held alongside the modifier, with a positive control', () => {
    const page = createEligiblePage();
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ ctrlKey: true, shiftKey: true }));
    expect(togglePalette).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ ctrlKey: true, shiftKey: false }));
    expect(togglePalette).toHaveBeenCalledTimes(1);
  });

  it('ignores neither Ctrl nor Meta held, with a positive control', () => {
    const page = createEligiblePage();
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({}));
    expect(togglePalette).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(togglePalette).toHaveBeenCalledTimes(1);
  });

  it('ignores both Ctrl and Meta held at once (some IMEs), with a positive control', () => {
    const page = createEligiblePage();
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, ctrlKey: true }));
    expect(togglePalette).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(togglePalette).toHaveBeenCalledTimes(1);
  });

  it('ignores keys other than k, with a positive control', () => {
    const page = createEligiblePage();
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ key: 'j', metaKey: true }));
    expect(togglePalette).not.toHaveBeenCalled();
    page._handleGlobalKeydown(makeKeydownEvent({ key: 'k', metaKey: true }));
    expect(togglePalette).toHaveBeenCalledTimes(1);
  });

  it('accepts uppercase K (Shift/Caps Lock does not itself block eligibility via key casing)', () => {
    // No shiftKey here — Shift itself is a separate, already-tested guard
    // above; this only proves key.toLowerCase() is used for the comparison.
    const page = createEligiblePage();
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ key: 'K', metaKey: true }));
    expect(togglePalette).toHaveBeenCalledTimes(1);
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
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    const terminalPane = document.createElement('scion-terminal-pane');

    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, path: [terminalPane] }));
    expect(togglePalette).not.toHaveBeenCalled();

    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true, path: [] }));
    expect(togglePalette).toHaveBeenCalledTimes(1);
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
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);

    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(togglePalette).not.toHaveBeenCalled();

    ancestor.hidden = false;
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(togglePalette).toHaveBeenCalledTimes(1);
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
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(togglePalette).not.toHaveBeenCalled();
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
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(togglePalette).not.toHaveBeenCalled();
  });

  it('the shortcut works again once the unrelated dialog is removed', () => {
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/chat');
    const dialog = document.createElement('sl-dialog') as HTMLElement & { open?: boolean };
    dialog.open = true;
    document.body.appendChild(dialog);
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(togglePalette).not.toHaveBeenCalled();

    dialog.remove();
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(togglePalette).toHaveBeenCalledTimes(1);
  });

  it('does not count the switcher/palette own dialog (inside its shadow root) as an unrelated modal', () => {
    const page = createUnattachedPage();
    const switcherEl = document.createElement('scion-quick-palette');
    const shadow = switcherEl.attachShadow({ mode: 'open' });
    const ownDialog = document.createElement('sl-dialog') as HTMLElement & { open?: boolean };
    ownDialog.open = true;
    shadow.appendChild(ownDialog);
    document.body.appendChild(switcherEl);
    Object.defineProperty(page, '_switcherEl', { value: switcherEl, configurable: true });

    expect(page._isUnrelatedModalActive()).toBe(false);
  });
});

describe('shortcut dispatch: the palette is the single shortcut owner', () => {
  it('dispatches to togglePalette', () => {
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/chat');
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
    page._handleGlobalKeydown(makeKeydownEvent({ metaKey: true }));
    expect(togglePalette).toHaveBeenCalledTimes(1);
  });

  it('preventDefault is called once eligibility is established', () => {
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/chat');
    vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);
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
    // first press's `await loadQuickPalette()` is still pending, reproducing
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

describe('keys typed while the palette opens', () => {
  /** A focused composer stand-in with its own keydown listener. */
  function composer(): { el: HTMLTextAreaElement; onKeydown: ReturnType<typeof vi.fn> } {
    const el = document.createElement('textarea');
    document.body.appendChild(el);
    const onKeydown = vi.fn();
    el.addEventListener('keydown', onKeydown);
    el.focus();
    return { el, onKeydown };
  }

  function typeAt(el: Element, key: string): KeyboardEvent {
    const e = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true });
    el.dispatchEvent(e);
    return e;
  }

  it('are captured from the start of an open, before the lazy import resolves, and handed to the palette', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    const { el, onKeydown } = composer();

    const opening = page.togglePalette();
    expect(typeAt(el, 'a').defaultPrevented).toBe(true);
    await opening;
    await page.updateComplete;

    expect(onKeydown).not.toHaveBeenCalled();
    const palette = page.shadowRoot.querySelector('scion-quick-palette');
    expect(palette.typeahead).toBe(page._paletteTypeahead);
    expect(page._paletteTypeahead.pending).toBe('a');
  });

  it('are not captured while the palette is not opening', () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    const { el, onKeydown } = composer();
    expect(typeAt(el, 'a').defaultPrevented).toBe(false);
    expect(onKeydown).toHaveBeenCalledTimes(1);
  });

  it('reach the composer again once a second press cancels the pending open', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    const { el, onKeydown } = composer();
    await Promise.all([page.togglePalette(), page.togglePalette()]);

    expect(page.v2PaletteOpen).toBe(false);
    expect(typeAt(el, 'a').defaultPrevented).toBe(false);
    expect(onKeydown).toHaveBeenCalledTimes(1);
  });

  it('reach the composer again once the page disconnects during the pending open', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    const { el } = composer();
    const opening = page.togglePalette();
    page.remove();
    // Before the suspended open resumes: the disconnect itself stops it.
    expect(typeAt(el, 'a').defaultPrevented).toBe(false);
    await opening;
    expect(typeAt(el, 'b').defaultPrevented).toBe(false);
  });

  it('reach the composer again once the palette closes', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    const { el } = composer();
    await page.togglePalette();
    expect(page.v2PaletteOpen).toBe(true);
    await page.togglePalette();
    expect(page.v2PaletteOpen).toBe(false);
    expect(typeAt(el, 'a').defaultPrevented).toBe(false);
  });

  function previewTarget(): unknown {
    return {
      kind: 'path',
      projectId: 'p1',
      containerPath: '/workspace/notes.txt',
      location: { kind: 'workspace', filePath: 'notes.txt' },
      name: 'notes.txt',
    };
  }

  it('are captured from a press queued behind the close animation, and become the reopened query', async () => {
    const page = createEligiblePage();
    document.body.appendChild(page);
    await page.togglePalette();
    await page.togglePalette(); // close: animating
    const { el, onKeydown } = composer();

    await page.togglePalette(); // queued
    expect(typeAt(el, 'c').defaultPrevented).toBe(true);
    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    await Promise.resolve();

    expect(page.v2PaletteOpen).toBe(true);
    expect(onKeydown).not.toHaveBeenCalled();
    expect(page._paletteTypeahead.pending).toBe('c');
  });

  it('are captured from a press queued behind a closing document preview', async () => {
    const page = createEligiblePage();
    page._paletteFilePreviewTarget = previewTarget();
    document.body.appendChild(page);
    const { el } = composer();

    await page.togglePalette({ mode: 'open' });
    expect(typeAt(el, 'c').defaultPrevented).toBe(true);
    expect(page._paletteTypeahead.pending).toBe('c');
  });

  it('are captured for a full time limit from a reopen that runs late behind the document preview', async () => {
    vi.useFakeTimers();
    try {
      const page = createEligiblePage();
      page.v2SwitcherLoaded = true;
      page._paletteFilePreviewTarget = previewTarget();
      document.body.appendChild(page);
      const { el, onKeydown } = composer();

      await page.togglePalette({ mode: 'open' }); // queued
      vi.advanceTimersByTime(PALETTE_TYPEAHEAD_MAX_MS - 1000);
      page._closePaletteFilePreview();
      expect(page.v2PaletteOpen).toBe(true);
      vi.advanceTimersByTime(2000);

      expect(typeAt(el, 'c').defaultPrevented).toBe(true);
      expect(onKeydown).not.toHaveBeenCalled();
      expect(page._paletteTypeahead.pending).toBe('c');
    } finally {
      vi.useRealTimers();
    }
  });

  it('reach the composer again once a second press cancels a queued reopen', async () => {
    const page = createEligiblePage();
    page._paletteCloseAnimating = true;
    document.body.appendChild(page);
    const { el } = composer();
    await page.togglePalette();
    await page.togglePalette();
    expect(page._palettePendingReopen).toBe(false);
    expect(typeAt(el, 'a').defaultPrevented).toBe(false);
  });

  it('reach the composer again once Escape cancels a queued reopen', async () => {
    const page = createEligiblePage();
    page._paletteCloseAnimating = true;
    document.body.appendChild(page);
    const { el } = composer();
    await page.togglePalette();
    page._handleGlobalKeydown(makeKeydownEvent({ key: 'Escape' }));
    expect(page._palettePendingReopen).toBe(false);
    expect(typeAt(el, 'a').defaultPrevented).toBe(false);
  });

  it('reach the composer again when a queued reopen is abandoned after the close animation', async () => {
    const page = createEligiblePage();
    document.body.appendChild(page);
    await page.togglePalette();
    await page.togglePalette();
    const { el } = composer();
    await page.togglePalette(); // queued
    vi.mocked(page._isUnrelatedModalActive).mockReturnValue(true);

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());

    expect(page.v2PaletteOpen).toBe(false);
    expect(typeAt(el, 'a').defaultPrevented).toBe(false);
  });

  it('reach the composer again when a reopen queued behind the document preview is abandoned', async () => {
    const page = createEligiblePage();
    page.v2SwitcherLoaded = true;
    page._paletteFilePreviewTarget = previewTarget();
    document.body.appendChild(page);
    const { el } = composer();
    await page.togglePalette();
    vi.mocked(page._isUnrelatedModalActive).mockReturnValue(true);

    page._closePaletteFilePreview();

    expect(page.v2PaletteOpen).toBe(false);
    expect(typeAt(el, 'a').defaultPrevented).toBe(false);
  });

  it('reach the composer again if the open fails', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    const { el } = composer();
    vi.spyOn(page, '_capturePaletteInvokerFocus').mockImplementation(() => {
      throw new Error('boom');
    });
    await expect(page.togglePalette()).rejects.toThrow('boom');
    expect(typeAt(el, 'a').defaultPrevented).toBe(false);
  });
});

describe('togglePalette: a reopen queued behind a still-animating close never opens an invisible palette', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('closing (via toggle) marks the close as animating', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    await page.togglePalette(); // open
    expect(page.v2PaletteOpen).toBe(true);

    await page.togglePalette(); // close

    expect(page.v2PaletteOpen).toBe(false);
    expect(page._paletteCloseAnimating).toBe(true);
  });

  it('a Ctrl+K that arrives while the close is still animating queues a reopen rather than opening immediately', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    await page.togglePalette(); // open
    await page.togglePalette(); // close -> _paletteCloseAnimating = true
    const captureSpy = vi.spyOn(page, '_capturePaletteInvokerFocus');

    await page.togglePalette(); // reopen press during the pending close

    expect(page.v2PaletteOpen).toBe(false);
    expect(page._palettePendingReopen).toBe(true);
    // Never actually runs the open sequence while queued — an open here would
    // flip `v2PaletteOpen` to true on a dialog Shoelace is still mid-hide on,
    // leaving the palette open in JS state but visually still hidden.
    expect(captureSpy).not.toHaveBeenCalled();
  });

  it('the `_paletteCloseAnimating` guard alone is what queues the reopen', async () => {
    // Isolates the guard from an actual prior close, mirroring this
    // codebase's convention of testing each condition independently rather
    // than only end to end.
    const page = createUnattachedPage();
    document.body.appendChild(page);
    page._paletteCloseAnimating = true;
    const captureSpy = vi.spyOn(page, '_capturePaletteInvokerFocus');

    await page.togglePalette();

    expect(page.v2PaletteOpen).toBe(false);
    expect(page._palettePendingReopen).toBe(true);
    expect(captureSpy).not.toHaveBeenCalled();
  });

  it('a second press during the same pending close cancels the queued reopen — the shortcut is still a toggle', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    page._paletteCloseAnimating = true;

    await page.togglePalette(); // queues a reopen
    expect(page._palettePendingReopen).toBe(true);
    await page.togglePalette(); // cancels it — an even number of presses ends closed

    expect(page._palettePendingReopen).toBe(false);
    expect(page.v2PaletteOpen).toBe(false);

    // A third press re-queues it — this isn't a one-shot latch, it keeps
    // toggling with every press for as long as the close stays animating.
    await page.togglePalette();
    expect(page._palettePendingReopen).toBe(true);
  });

  it('a second Escape while a reopen is queued behind a still-animating close cancels the queue', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    await page.togglePalette();
    await page.togglePalette();
    await page.togglePalette(); // queue the reopen
    expect(page._palettePendingReopen).toBe(true);

    page._handleGlobalKeydown(makeKeydownEvent({ key: 'Escape' }));

    expect(page._palettePendingReopen).toBe(false);
  });

  it('a second Escape while a Ctrl+K is queued behind a closing document preview cancels the queue', () => {
    const page = createUnattachedPage();
    page._paletteFilePreviewTarget = {
      kind: 'path',
      projectId: 'p1',
      containerPath: '/workspace/notes.txt',
      location: { kind: 'workspace', filePath: 'notes.txt' },
      name: 'notes.txt',
    };
    page._palettePendingReopen = true;

    page._handleGlobalKeydown(makeKeydownEvent({ key: 'Escape' }));

    expect(page._palettePendingReopen).toBe(false);
  });

  it('Escape while a close is animating but nothing is queued does not queue one', () => {
    // Smoke check only: with nothing queued the guard's only effect
    // (clearing _palettePendingReopen) is already a no-op, so no mutant of
    // the guard can make this fail.
    const page = createUnattachedPage();
    page._paletteCloseAnimating = true;

    page._handleGlobalKeydown(makeKeydownEvent({ key: 'Escape' }));

    expect(page._palettePendingReopen).toBe(false);
  });

  it('Escape does not touch a queued reopen once neither a palette close nor a document-preview close is still pending', () => {
    // Isolates the (_paletteCloseAnimating || _paletteFilePreviewTarget)
    // conjunct as a whole: a queued reopen with neither a palette close
    // still animating nor a document preview still closing (a state a real
    // sequence never leaves) must not be cleared.
    const page = createUnattachedPage();
    page._palettePendingReopen = true;

    page._handleGlobalKeydown(makeKeydownEvent({ key: 'Escape' }));

    expect(page._palettePendingReopen).toBe(true);
  });

  it('a non-Escape key does not cancel a queued reopen, even while a close is animating', () => {
    // Isolates the `e.key === 'Escape'` half: without it, an unrelated
    // keydown reaching this handler while both other conditions happen to
    // hold (e.g. a genuine Ctrl+K, handled further down this same function)
    // would wrongly cancel the queue too.
    const page = createUnattachedPage();
    page._paletteCloseAnimating = true;
    page._palettePendingReopen = true;

    page._handleGlobalKeydown(makeKeydownEvent({ key: 'a' }));

    expect(page._palettePendingReopen).toBe(true);
  });

  it("the queued reopen actually opens once the pending close's sl-after-hide fires, with the open guards still holding", async () => {
    const page = createEligiblePage();
    document.body.appendChild(page);
    await page.togglePalette();
    await page.togglePalette();
    await page.togglePalette(); // queue the reopen

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    await Promise.resolve();

    expect(page.v2PaletteOpen).toBe(true);
    expect(page._palettePendingReopen).toBe(false);
    expect(page._paletteCloseAnimating).toBe(false);
  });

  it('a queued reopen is abandoned, not opened, if an unrelated modal opened while it was queued', async () => {
    const page = createEligiblePage();
    document.body.appendChild(page);
    await page.togglePalette();
    await page.togglePalette();
    await page.togglePalette(); // queue the reopen
    vi.mocked(page._isUnrelatedModalActive).mockReturnValue(true); // opened during the queued window
    const captureSpy = vi.spyOn(page, '_capturePaletteInvokerFocus');

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    await Promise.resolve();

    expect(page.v2PaletteOpen).toBe(false);
    expect(page._palettePendingReopen).toBe(false);
    expect(captureSpy).not.toHaveBeenCalled();
    expect(page._paletteInvoker).toBeNull();
  });

  it('a queued reopen is abandoned, not opened, if the route left /chat while it was queued', async () => {
    const page = createEligiblePage();
    document.body.appendChild(page);
    await page.togglePalette();
    await page.togglePalette();
    await page.togglePalette(); // queue the reopen
    vi.mocked(page._isOnChatRoute).mockReturnValue(false); // navigated away during the queued window

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    await Promise.resolve();

    expect(page.v2PaletteOpen).toBe(false);
    expect(page._palettePendingReopen).toBe(false);
  });

  it('a queued reopen is abandoned, not opened, if the page became hidden while it was queued', async () => {
    const page = createEligiblePage();
    document.body.appendChild(page);
    await page.togglePalette();
    await page.togglePalette();
    await page.togglePalette(); // queue the reopen
    vi.mocked(page._isPageVisible).mockReturnValue(false); // hidden during the queued window

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    await Promise.resolve();

    expect(page.v2PaletteOpen).toBe(false);
    expect(page._palettePendingReopen).toBe(false);
  });

  function pendingDocumentTarget() {
    return {
      kind: 'path' as const,
      projectId: 'p1',
      containerPath: '/workspace/notes.txt',
      location: { kind: 'workspace' as const, filePath: 'notes.txt' },
      name: 'notes.txt',
    };
  }

  it('a pending document preview is abandoned, not opened, if an unrelated modal opened while it was queued', () => {
    const page = createEligiblePage();
    page._pendingDocumentPreviewTarget = pendingDocumentTarget();
    page._paletteInvoker = document.createElement('textarea');
    vi.mocked(page._isUnrelatedModalActive).mockReturnValue(true); // opened during the queued window

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());

    expect(page._paletteFilePreviewTarget).toBeNull();
    expect(page._pendingDocumentPreviewTarget).toBeNull();
    expect(page._paletteInvoker).toBeNull();
  });

  it('a pending document preview is abandoned, not opened, if the route left /chat while it was queued', () => {
    const page = createEligiblePage();
    page._pendingDocumentPreviewTarget = pendingDocumentTarget();
    page._paletteInvoker = document.createElement('textarea');
    vi.mocked(page._isOnChatRoute).mockReturnValue(false); // navigated away during the queued window

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());

    expect(page._paletteFilePreviewTarget).toBeNull();
    expect(page._pendingDocumentPreviewTarget).toBeNull();
    expect(page._paletteInvoker).toBeNull();
  });

  it('a pending document preview is abandoned, not opened, if the page became hidden while it was queued', () => {
    const page = createEligiblePage();
    page._pendingDocumentPreviewTarget = pendingDocumentTarget();
    page._paletteInvoker = document.createElement('textarea');
    vi.mocked(page._isPageVisible).mockReturnValue(false); // hidden during the queued window

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());

    expect(page._paletteFilePreviewTarget).toBeNull();
    expect(page._pendingDocumentPreviewTarget).toBeNull();
    expect(page._paletteInvoker).toBeNull();
  });

  it('two Ctrl+K presses while the document preview is closing leave the reopen queue toggled back off', async () => {
    const page = createEligiblePage();
    page._paletteFilePreviewTarget = pendingDocumentTarget();

    await page.togglePalette();
    expect(page._palettePendingReopen).toBe(true);
    await page.togglePalette();

    expect(page._palettePendingReopen).toBe(false);
  });

  it('closing the document preview with a queued reopen discards the invoker instead of opening, if the guards no longer hold', () => {
    const page = createEligiblePage();
    page.v2SwitcherLoaded = true;
    page._paletteFilePreviewTarget = pendingDocumentTarget();
    page._palettePendingReopen = true;
    page._paletteInvoker = document.createElement('textarea');
    page._paletteInvokerSelection = { start: 2, end: 5, direction: 'none' };
    vi.mocked(page._isUnrelatedModalActive).mockReturnValue(true); // opened during the queued window

    page._closePaletteFilePreview();

    expect(page.v2PaletteOpen).toBe(false);
    expect(page._palettePendingReopen).toBe(false);
    expect(page._paletteInvoker).toBeNull();
    expect(page._paletteInvokerSelection).toBeNull();
  });

  it("the queued reopen's after-hide runs neither the superseded close's composer-focus nor its invoker-restore disposition", async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    await page.togglePalette();
    await page.togglePalette();
    await page.togglePalette(); // queue the reopen
    const restoreSpy = vi.spyOn(page, '_restorePaletteInvokerFocus');
    const focusComposerSpy = vi.spyOn(page, '_focusComposerAfterPaletteSelection');
    page._paletteClosedBySelection = true; // as if the superseded close had been a selection

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    await Promise.resolve();

    expect(restoreSpy).not.toHaveBeenCalled();
    expect(focusComposerSpy).not.toHaveBeenCalled();
    expect(page._paletteClosedBySelection).toBe(false);
  });

  it('a queued reopen also discards a superseded pending document-preview target', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    await page.togglePalette();
    await page.togglePalette();
    await page.togglePalette();
    page._pendingDocumentPreviewTarget = {
      kind: 'attachment',
      id: 'att-1',
      name: 'x.png',
      mime: 'image/png',
      size: 1,
    };

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    await Promise.resolve();

    expect(page._pendingDocumentPreviewTarget).toBeNull();
    expect(page._paletteFilePreviewTarget).toBeNull();
  });

  it('a queued reopen clears the superseded close\'s "skip focus restore" flag, so the reopened palette\'s own later close still restores focus normally', async () => {
    const page = createEligiblePage();
    document.body.appendChild(page);
    await page.togglePalette(); // open
    page._closePaletteWithoutFocusRestore(); // as a guard-initiated close would (modal opened, route changed)
    expect(page._paletteSkipFocusRestore).toBe(true);
    expect(page._paletteCloseAnimating).toBe(true);

    await page.togglePalette(); // queue a reopen behind that close
    expect(page._palettePendingReopen).toBe(true);

    page._handlePaletteAfterHide(ownDialogAfterHideEvent()); // dequeues, reopens
    await Promise.resolve();
    expect(page.v2PaletteOpen).toBe(true);
    expect(page._paletteSkipFocusRestore).toBe(false);

    // A genuine later close on the *reopened* palette (e.g. Escape) — if the
    // superseded close's flag had leaked through, this would wrongly skip
    // the restore below instead of running it.
    const restoreSpy = vi.spyOn(page, '_restorePaletteInvokerFocus');
    page._closePaletteAndCancelLoad();
    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    await Promise.resolve();

    expect(restoreSpy).toHaveBeenCalledTimes(1);
  });

  it("a slow retarget poll does not overwrite a later open's own invoker once superseded", async () => {
    const page = createEligiblePage();
    document.body.appendChild(page);
    // Every _openPalette() call below, including the fire-and-forget one
    // inside _handlePaletteAfterHide, must settle synchronously rather than
    // racing a real dynamic import.
    page.v2SwitcherLoaded = true;

    // Holds the retarget's own poll open indefinitely, simulating a
    // composer mount slow enough that a whole later open/close cycle can
    // complete before it resolves.
    let resolvePoll: (el: HTMLElement | null) => void = () => {};
    const pollPromise = new Promise<HTMLElement | null>((resolve) => {
      resolvePoll = resolve;
    });
    vi.spyOn(page, '_pollForNewComposerTextarea').mockReturnValue(pollPromise);

    page._paletteClosedBySelection = true;
    page._palettePendingReopen = true;
    page._paletteCloseAnimating = true;

    // Dequeues: starts _openPalette (bumping the epoch) and kicks off the
    // retarget poll, captured at that same epoch value.
    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    await Promise.resolve();
    await Promise.resolve();

    // A later open supersedes this one for real, through the same
    // close/reopen path a fresh Cmd/Ctrl+K press takes — not a manual
    // `_paletteOpenEpoch++`, which would bump the epoch without exercising
    // `_openPalette`'s own bump, or the `v2PaletteOpen` check the guard also
    // requires.
    page._closePaletteAndCancelLoad();
    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    await page.togglePalette();
    expect(page.v2PaletteOpen).toBe(true);

    const freshInvoker = document.createElement('textarea');
    document.body.appendChild(freshInvoker);
    page._paletteInvoker = freshInvoker;

    // The slow composer mount the original (now-superseded) retarget was
    // waiting on finally resolves.
    resolvePoll(document.createElement('textarea'));
    await Promise.resolve();
    await Promise.resolve();

    expect(page._paletteInvoker).toBe(freshInvoker);
  });

  it('a retarget poll that resolves while its own reopened palette is still the current open assigns the resolved composer as the invoker', async () => {
    const page = createEligiblePage();
    document.body.appendChild(page);
    page.v2SwitcherLoaded = true;

    let resolvePoll: (el: HTMLElement | null) => void = () => {};
    const pollPromise = new Promise<HTMLElement | null>((resolve) => {
      resolvePoll = resolve;
    });
    vi.spyOn(page, '_pollForNewComposerTextarea').mockReturnValue(pollPromise);

    page._paletteClosedBySelection = true;
    page._palettePendingReopen = true;
    page._paletteCloseAnimating = true;

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    await Promise.resolve();
    await Promise.resolve();
    expect(page.v2PaletteOpen).toBe(true);

    // No later open supersedes this one — the poll resolves while the
    // reopened palette (the one the retarget was captured for) is still the
    // current open, so the resolved composer must actually be assigned.
    const resolvedComposer = document.createElement('textarea');
    resolvePoll(resolvedComposer);
    await Promise.resolve();
    await Promise.resolve();

    expect(page._paletteInvoker).toBe(resolvedComposer);
  });

  it('a retarget poll that resolves after its own reopened palette has since been closed again does not assign a stale invoker', async () => {
    const page = createEligiblePage();
    document.body.appendChild(page);
    page.v2SwitcherLoaded = true;

    let resolvePoll: (el: HTMLElement | null) => void = () => {};
    const pollPromise = new Promise<HTMLElement | null>((resolve) => {
      resolvePoll = resolve;
    });
    vi.spyOn(page, '_pollForNewComposerTextarea').mockReturnValue(pollPromise);

    page._paletteClosedBySelection = true;
    page._palettePendingReopen = true;
    page._paletteCloseAnimating = true;

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    await Promise.resolve();
    await Promise.resolve();
    expect(page.v2PaletteOpen).toBe(true);

    // Nothing supersedes this open — the epoch the retarget captured is
    // still current — but the reopened palette itself is closed again
    // before the slow poll resolves. The epoch check alone cannot catch
    // this: only the `v2PaletteOpen` check does.
    page._closePaletteAndCancelLoad();
    expect(page.v2PaletteOpen).toBe(false);
    const invokerBeforePollResolves = page._paletteInvoker;

    resolvePoll(document.createElement('textarea'));
    await Promise.resolve();
    await Promise.resolve();

    expect(page._paletteInvoker).toBe(invokerBeforePollResolves);
  });

  it('a toggle press after the close has actually finished (sl-after-hide already delivered) opens immediately, not queued', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    await page.togglePalette();
    await page.togglePalette();
    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    await Promise.resolve();
    expect(page._paletteCloseAnimating).toBe(false);

    await page.togglePalette();

    expect(page.v2PaletteOpen).toBe(true);
    expect(page._palettePendingReopen).toBe(false);
  });

  it('disconnecting the page while a reopen is queued clears it, so it cannot resume on a detached page', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    await page.togglePalette();
    await page.togglePalette();
    await page.togglePalette(); // queue the reopen
    expect(page._palettePendingReopen).toBe(true);

    page.remove();

    expect(page._palettePendingReopen).toBe(false);
  });

  it('disconnecting while a close is still animating clears `_paletteCloseAnimating`, so a reconnected page opens on the next press instead of only ever queuing', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    await page.togglePalette(); // open
    await page.togglePalette(); // close -> _paletteCloseAnimating = true
    expect(page._paletteCloseAnimating).toBe(true);

    page.remove(); // this close's own sl-after-hide will never arrive now

    expect(page._paletteCloseAnimating).toBe(false);

    document.body.appendChild(page); // reconnect
    await page.togglePalette();

    expect(page.v2PaletteOpen).toBe(true);
    expect(page._palettePendingReopen).toBe(false);
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
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);

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

    expect(togglePalette).toHaveBeenCalledTimes(1);
  });

  it('a disconnected (removed) page does not react to a keydown at all', () => {
    const page = createUnattachedPage();
    vi.spyOn(page, '_isOnChatRoute').mockReturnValue(true);
    vi.spyOn(page, '_isPageVisible').mockReturnValue(true);
    vi.spyOn(page, '_isUnrelatedModalActive').mockReturnValue(false);
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);

    document.body.appendChild(page);
    document.body.removeChild(page);

    document.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'k', ctrlKey: true, bubbles: true })
    );

    expect(togglePalette).not.toHaveBeenCalled();
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
    const switcherEl = document.createElement('scion-quick-palette');
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

describe('_handlePaletteOpenRequest: the header button opens with { mode: "open" }, guarded the same as the shortcut', () => {
  it('dispatches togglePalette({ mode: "open" }) when every guard holds', () => {
    const page = createEligiblePage();
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);

    page._handlePaletteOpenRequest();

    expect(togglePalette).toHaveBeenCalledTimes(1);
    expect(togglePalette).toHaveBeenCalledWith({ mode: 'open' });
  });

  it('does nothing — no togglePalette call — when the route guard fails, with a positive control', () => {
    const page = createEligiblePage();
    vi.mocked(page._isOnChatRoute).mockReturnValue(false);
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);

    page._handlePaletteOpenRequest();
    expect(togglePalette).not.toHaveBeenCalled();

    vi.mocked(page._isOnChatRoute).mockReturnValue(true);
    page._handlePaletteOpenRequest();
    expect(togglePalette).toHaveBeenCalledTimes(1);
  });

  it('does nothing when the page is not visible, with a positive control', () => {
    const page = createEligiblePage();
    vi.mocked(page._isPageVisible).mockReturnValue(false);
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);

    page._handlePaletteOpenRequest();
    expect(togglePalette).not.toHaveBeenCalled();

    vi.mocked(page._isPageVisible).mockReturnValue(true);
    page._handlePaletteOpenRequest();
    expect(togglePalette).toHaveBeenCalledTimes(1);
  });

  it('does nothing when an unrelated modal is active, with a positive control', () => {
    const page = createEligiblePage();
    vi.mocked(page._isUnrelatedModalActive).mockReturnValue(true);
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);

    page._handlePaletteOpenRequest();
    expect(togglePalette).not.toHaveBeenCalled();

    vi.mocked(page._isUnrelatedModalActive).mockReturnValue(false);
    page._handlePaletteOpenRequest();
    expect(togglePalette).toHaveBeenCalledTimes(1);
  });

  it('a blocked open-request issues no fetch — same no-state-change guarantee as the shortcut', () => {
    const page = createEligiblePage();
    vi.mocked(page._isUnrelatedModalActive).mockReturnValue(true);
    const loadSpy = vi.spyOn(page, '_openPalette');

    page._handlePaletteOpenRequest();

    expect(loadSpy).not.toHaveBeenCalled();
    expect(page.v2PaletteOpen).toBe(false);
  });

  it('the document-level listener registered in connectedCallback dispatches through to togglePalette', () => {
    // Spies on togglePalette, not _handlePaletteOpenRequest itself: the
    // document listener is a reference bound once in a field initializer,
    // captured before any spy from inside a test could replace it — exactly
    // like the existing keydown-listener tests below (dynamic `this.`
    // dispatch inside the handler is what spying relies on instead).
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/chat');
    vi.spyOn(page, '_isOnChatRoute').mockReturnValue(true);
    vi.spyOn(page, '_isPageVisible').mockReturnValue(true);
    vi.spyOn(page, '_isUnrelatedModalActive').mockReturnValue(false);
    document.body.appendChild(page);
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);

    document.dispatchEvent(
      new CustomEvent(CHAT_PALETTE_OPEN_REQUEST_EVENT, { bubbles: true, composed: true })
    );

    expect(togglePalette).toHaveBeenCalledWith({ mode: 'open' });
  });

  it('a reconnected page reacts to exactly one dispatch, not a leaked extra listener from the first connect', () => {
    // Re-adding the identical bound function via addEventListener is a
    // silent no-op (the DOM dedupes it), so a stale listener left behind by
    // a missing removeEventListener would not actually double-fire here —
    // it would just make the *disconnected* page's listener a leak with no
    // observable symptom in this specific reconnect shape. The dedicated
    // "disconnected page does not react at all" test below is what actually
    // proves removeEventListener ran; this test only establishes that a
    // normal reconnect still works at all.
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/chat');
    vi.spyOn(page, '_isOnChatRoute').mockReturnValue(true);
    vi.spyOn(page, '_isPageVisible').mockReturnValue(true);
    vi.spyOn(page, '_isUnrelatedModalActive').mockReturnValue(false);
    document.body.appendChild(page);
    document.body.removeChild(page);
    document.body.appendChild(page);
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);

    document.dispatchEvent(
      new CustomEvent(CHAT_PALETTE_OPEN_REQUEST_EVENT, { bubbles: true, composed: true })
    );

    expect(togglePalette).toHaveBeenCalledTimes(1);
  });

  it('a disconnected (removed) page does not react to an open-request dispatch at all', () => {
    const page = createUnattachedPage();
    window.history.pushState({}, '', '/chat');
    vi.spyOn(page, '_isOnChatRoute').mockReturnValue(true);
    vi.spyOn(page, '_isPageVisible').mockReturnValue(true);
    vi.spyOn(page, '_isUnrelatedModalActive').mockReturnValue(false);
    const togglePalette = vi.spyOn(page, 'togglePalette').mockResolvedValue(undefined);

    document.body.appendChild(page);
    document.body.removeChild(page);

    document.dispatchEvent(
      new CustomEvent(CHAT_PALETTE_OPEN_REQUEST_EVENT, { bubbles: true, composed: true })
    );

    expect(togglePalette).not.toHaveBeenCalled();
  });
});

describe('togglePalette({ mode: "open" }): the button never closes or cancels, only ever (re)queues an open', () => {
  it('open -> no-op: the palette stays open and the data controller is not cancelled', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    await page.togglePalette(); // open
    const cancelSpy = vi.spyOn(page._paletteDataController, 'cancel');

    await page.togglePalette({ mode: 'open' });

    expect(page.v2PaletteOpen).toBe(true);
    expect(cancelSpy).not.toHaveBeenCalled();
  });

  it('pending-open -> no-op: a second button press during the first-open lazy import does not cancel it', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    const captureSpy = vi.spyOn(page, '_capturePaletteInvokerFocus');

    const first = page.togglePalette({ mode: 'open' });
    const second = page.togglePalette({ mode: 'open' }); // must not cancel `first`
    await Promise.all([first, second]);

    expect(page.v2PaletteOpen).toBe(true);
    expect(captureSpy).toHaveBeenCalledTimes(1);
  });

  it('closing (close-animating) -> queue set to true twice and still true: a double press during a closing animation does not cancel itself out', async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    page._paletteCloseAnimating = true;

    await page.togglePalette({ mode: 'open' });
    expect(page._palettePendingReopen).toBe(true);

    await page.togglePalette({ mode: 'open' }); // a 'toggle' here would flip this back to false
    expect(page._palettePendingReopen).toBe(true);
  });

  it('closing (document-preview queue) -> queue set to true twice and still true', () => {
    const page = createUnattachedPage();
    page._paletteFilePreviewTarget = {
      kind: 'path',
      projectId: 'p1',
      containerPath: '/workspace/notes.txt',
      location: { kind: 'workspace', filePath: 'notes.txt' },
      name: 'notes.txt',
    };

    void page.togglePalette({ mode: 'open' });
    expect(page._palettePendingReopen).toBe(true);

    void page.togglePalette({ mode: 'open' });
    expect(page._palettePendingReopen).toBe(true);
  });

  it("the existing 'toggle' behaviour (default, no options) is unchanged: open then toggle-close", async () => {
    const page = createUnattachedPage();
    document.body.appendChild(page);
    await page.togglePalette();
    expect(page.v2PaletteOpen).toBe(true);

    await page.togglePalette();
    expect(page.v2PaletteOpen).toBe(false);
  });
});

describe('touch focus handoff: a conversation selection on touch does not focus the composer', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  /** Stubs `window.matchMedia(TOUCH_PRIMARY_QUERY)` to report touch/desktop, independent of the real test environment. */
  function stubTouchPrimary(matches: boolean): void {
    vi.stubGlobal(
      'matchMedia',
      vi.fn((query: string) => ({
        matches: query === TOUCH_PRIMARY_QUERY && matches,
        media: query,
        addEventListener: () => {},
        removeEventListener: () => {},
      }))
    );
  }

  it('on touch, a selection close falls back instead of focusing the composer', () => {
    stubTouchPrimary(true);
    const page = createUnattachedPage();
    // TouchPrimaryController only reads matchMedia() once connected (it
    // ties the query's change listener to the host's connected lifetime),
    // so the stub above is only observed once the page is actually in the
    // document -- an unconnected page would see the controller's default
    // (false) regardless of the stub, defeating this test.
    document.body.appendChild(page);
    const focusComposerSpy = vi.spyOn(page, '_focusComposerAfterPaletteSelection');
    const fallbackSpy = vi.spyOn(page, '_focusPaletteFallback').mockImplementation(() => {});
    page._paletteClosedBySelection = true;

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());

    expect(fallbackSpy).toHaveBeenCalledTimes(1);
    expect(focusComposerSpy).not.toHaveBeenCalled();
    expect(page._paletteClosedBySelection).toBe(false);
  });

  it('on desktop (the default/positive control), a selection close still focuses the composer as before', () => {
    stubTouchPrimary(false);
    const page = createUnattachedPage();
    document.body.appendChild(page);
    const focusComposerSpy = vi
      .spyOn(page, '_focusComposerAfterPaletteSelection')
      .mockResolvedValue(undefined);
    const fallbackSpy = vi.spyOn(page, '_focusPaletteFallback');
    page._paletteClosedBySelection = true;

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());

    expect(focusComposerSpy).toHaveBeenCalledTimes(1);
    expect(fallbackSpy).not.toHaveBeenCalled();
  });

  it('on touch, a reopen queued behind a selection-triggered close does not retarget the invoker to the new composer', async () => {
    // The direct selection-close path above is not the only one that can
    // hand focus to a composer: a reopen queued during the close (e.g. a
    // second button press while the close from a selection is still
    // animating) takes over instead, and has its own, separate retarget
    // call — _retargetPaletteInvokerToNewComposer — that must respect the
    // same touch rule, or a phone keyboard would pop open on the eventual
    // close of the *reopened* palette instead.
    stubTouchPrimary(true);
    const page = createEligiblePage();
    document.body.appendChild(page);
    page.v2SwitcherLoaded = true;
    const retargetSpy = vi.spyOn(page, '_retargetPaletteInvokerToNewComposer');

    page._paletteClosedBySelection = true;
    page._palettePendingReopen = true;
    page._paletteCloseAnimating = true;

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    await Promise.resolve();

    expect(page.v2PaletteOpen).toBe(true);
    expect(retargetSpy).not.toHaveBeenCalled();
  });

  it('on desktop (positive control), the same queued reopen does retarget the invoker to the new composer', async () => {
    stubTouchPrimary(false);
    const page = createEligiblePage();
    document.body.appendChild(page);
    page.v2SwitcherLoaded = true;
    const retargetSpy = vi
      .spyOn(page, '_retargetPaletteInvokerToNewComposer')
      .mockResolvedValue(undefined);

    page._paletteClosedBySelection = true;
    page._palettePendingReopen = true;
    page._paletteCloseAnimating = true;

    page._handlePaletteAfterHide(ownDialogAfterHideEvent());
    await Promise.resolve();

    expect(page.v2PaletteOpen).toBe(true);
    expect(retargetSpy).toHaveBeenCalledTimes(1);
  });
});
