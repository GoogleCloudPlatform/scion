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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  QuickPaletteHost,
  isQuickPaletteShortcut,
  type QuickPaletteHostOptions,
  type QuickPaletteLoadContext,
} from './quick-palette-host.js';
import type { ScionQuickPalette } from './quick-palette.js';
import { hasOpenModalDescendant } from '../open-modal.js';
import { PALETTE_TYPEAHEAD_MAX_MS } from './palette-typeahead.js';
import type { PaletteAgentTarget, PaletteCandidate } from '../../../client/chat-palette-types.js';

function candidate(agentId: string, label = agentId): PaletteCandidate {
  return {
    id: JSON.stringify(['agent', agentId]),
    group: 'agents',
    label,
    secondaryLabel: '',
    searchFields: [label],
    activityMs: 0,
    target: { kind: 'agent', agentId, displayName: label },
  };
}

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (err: unknown) => void;
} {
  let resolve!: (value: T) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

/** Fires `type` from the palette's own dialog, composed, as Shoelace does. */
function fireFromDialog(palette: ScionQuickPalette, type: 'sl-hide' | 'sl-after-hide'): void {
  palette
    .shadowRoot!.querySelector('sl-dialog')!
    .dispatchEvent(new Event(type, { bubbles: true, composed: true }));
}

function nextTask(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

/** A printable keydown at `el`, as typed into whatever had focus. */
function typeAt(el: Element, key: string): KeyboardEvent {
  const e = new KeyboardEvent('keydown', { key, bubbles: true, composed: true, cancelable: true });
  el.dispatchEvent(e);
  return e;
}

/** Fires the dialog's own sl-initial-focus, as Shoelace does once the shown dialog can take focus. */
async function fireInitialFocus(palette: ScionQuickPalette): Promise<HTMLInputElement> {
  palette
    .shadowRoot!.querySelector('sl-dialog')!
    .dispatchEvent(new CustomEvent('sl-initial-focus', { cancelable: true }));
  await palette.updateComplete;
  await nextTask();
  await palette.updateComplete;
  return palette.shadowRoot!.querySelector<HTMLInputElement>('#palette-query-input')!;
}

/** Tracks capturing keydown listeners on the document, from now on. */
function trackDocumentKeydownCapture(): { count: () => number } {
  const active = new Set<EventListenerOrEventListenerObject>();
  const isKeydownCapture = (type: string, options?: boolean | EventListenerOptions): boolean =>
    type === 'keydown' && (options === true || (typeof options === 'object' && !!options.capture));
  const add = document.addEventListener.bind(document);
  const remove = document.removeEventListener.bind(document);
  vi.spyOn(document, 'addEventListener').mockImplementation((type, listener, options) => {
    if (listener && isKeydownCapture(type, options)) active.add(listener);
    add(type, listener, options);
  });
  vi.spyOn(document, 'removeEventListener').mockImplementation((type, listener, options) => {
    if (listener && isKeydownCapture(type, options)) active.delete(listener);
    remove(type, listener, options);
  });
  return { count: () => active.size };
}

let mount: HTMLElement;
let host: QuickPaletteHost | null = null;

function createHost(overrides: Partial<QuickPaletteHostOptions> = {}): QuickPaletteHost {
  host = new QuickPaletteHost({
    mount,
    label: 'Jump to agent',
    placeholder: 'Search agents…',
    load: (): Promise<PaletteCandidate[]> => Promise.resolve([candidate('a1', 'Alpha')]),
    onSelect: (): void => {},
    ...overrides,
  });
  return host;
}

async function waitForPalette(): Promise<ScionQuickPalette> {
  const palette = await vi.waitFor(
    () => {
      const el = mount.querySelector('scion-quick-palette');
      if (!el) throw new Error('palette not mounted yet');
      return el;
    },
    { timeout: 5000 }
  );
  await palette.updateComplete;
  return palette;
}

async function openReady(h: QuickPaletteHost): Promise<ScionQuickPalette> {
  h.open();
  const palette = await waitForPalette();
  await vi.waitFor(() => {
    expect(palette.open).toBe(true);
    expect(palette.groups.agents?.status).toBe('ready');
  });
  return palette;
}

function select(palette: ScionQuickPalette, target: unknown): void {
  palette.dispatchEvent(new CustomEvent('palette-select', { detail: { target } }));
}

function agentTarget(agentId: string): PaletteAgentTarget {
  return { kind: 'agent', agentId, displayName: agentId };
}

beforeEach(() => {
  mount = document.createElement('div');
  document.body.append(mount);
});

afterEach(() => {
  host?.dispose();
  host = null;
  mount.remove();
  document.body.replaceChildren();
  vi.restoreAllMocks();
});

describe('QuickPaletteHost: mount and load', () => {
  it('creates no element until the first open', async () => {
    await import('./quick-palette.js');
    createHost();
    await nextTask();
    expect(mount.querySelector('scion-quick-palette')).toBeNull();
  });

  it('mounts the palette into the mount element on open, labelled, with the loaded agents', async () => {
    const h = createHost();
    const palette = await openReady(h);
    expect(palette.parentNode).toBe(mount);
    expect(palette.label).toBe('Jump to agent');
    expect(palette.placeholder).toBe('Search agents…');
    expect(palette.groups.agents?.candidates.map((c) => c.label)).toEqual(['Alpha']);
    expect(h.isOpen).toBe(true);
    expect(h.element).toBe(palette);
  });

  it('mounts into a shadow root', async () => {
    const shadowHost = document.createElement('div');
    mount.append(shadowHost);
    const shadow = shadowHost.attachShadow({ mode: 'open' });
    const h = createHost({ mount: shadow });
    h.open();
    await vi.waitFor(() => expect(shadow.querySelector('scion-quick-palette')).not.toBeNull());
    expect(h.element?.getRootNode()).toBe(shadow);
  });

  it('reuses one element across opens and reloads on every open', async () => {
    const load = vi.fn(() => Promise.resolve([candidate('a1')]));
    const h = createHost({ load });
    const first = await openReady(h);
    h.close();
    const second = await openReady(h);
    expect(second).toBe(first);
    expect(mount.querySelectorAll('scion-quick-palette')).toHaveLength(1);
    expect(load).toHaveBeenCalledTimes(2);
  });

  it('publishes progress, then the final candidates', async () => {
    const final = deferred<PaletteCandidate[]>();
    let context!: QuickPaletteLoadContext;
    const h = createHost({
      load: (ctx) => {
        context = ctx;
        return final.promise;
      },
    });
    h.open();
    const palette = await waitForPalette();
    context.onProgress([candidate('a1')]);
    await vi.waitFor(() =>
      expect(palette.groups.agents).toEqual({ status: 'loading', candidates: [candidate('a1')] })
    );
    final.resolve([candidate('a1'), candidate('a2')]);
    await vi.waitFor(() => expect(palette.groups.agents?.status).toBe('ready'));
    expect(palette.groups.agents?.candidates).toHaveLength(2);
  });

  it('closing aborts the in-flight load', async () => {
    const contexts: QuickPaletteLoadContext[] = [];
    const h = createHost({
      load: (ctx) => {
        contexts.push(ctx);
        return new Promise(() => {});
      },
    });
    h.open();
    await waitForPalette();
    expect(contexts[0].controller.signal.aborted).toBe(false);
    h.close();
    expect(contexts[0].controller.signal.aborted).toBe(true);
    expect(h.isOpen).toBe(false);
  });

  it('a superseded load never publishes over a newer one', async () => {
    const loads: Array<ReturnType<typeof deferred<PaletteCandidate[]>>> = [];
    const contexts: QuickPaletteLoadContext[] = [];
    const h = createHost({
      load: (ctx) => {
        contexts.push(ctx);
        const d = deferred<PaletteCandidate[]>();
        loads.push(d);
        return d.promise;
      },
    });
    h.open();
    const palette = await waitForPalette();
    h.close();
    h.open();
    expect(loads).toHaveLength(2);
    loads[1].resolve([candidate('new')]);
    await vi.waitFor(() => expect(palette.groups.agents?.status).toBe('ready'));
    // The first load ignores its abort and resolves late.
    loads[0].resolve([candidate('old')]);
    contexts[0].onProgress([candidate('old')]);
    await nextTask();
    expect(contexts[0].isCurrent()).toBe(false);
    expect(palette.groups.agents?.candidates.map((c) => c.target)).toEqual([agentTarget('new')]);
    expect(palette.groups.agents?.status).toBe('ready');
  });

  it('a failed load shows the error state, keeping candidates; retry reloads', async () => {
    let attempt = 0;
    const h = createHost({
      load: () => {
        attempt++;
        return attempt === 1
          ? Promise.reject(new Error('boom'))
          : Promise.resolve([candidate('a1')]);
      },
    });
    h.open();
    const palette = await waitForPalette();
    await vi.waitFor(() =>
      expect(palette.groups.agents).toEqual({ status: 'error', candidates: [], error: 'boom' })
    );
    palette.dispatchEvent(new CustomEvent('palette-retry', { detail: { group: 'agents' } }));
    await vi.waitFor(() => expect(palette.groups.agents?.status).toBe('ready'));
    expect(attempt).toBe(2);
  });

  it('a load that settles after a close publishes nothing', async () => {
    const pending = deferred<PaletteCandidate[]>();
    const h = createHost({ load: () => pending.promise });
    h.open();
    const palette = await waitForPalette();
    h.close();

    pending.resolve([candidate('a1', 'Alpha')]);
    await nextTask();

    expect(palette.groups.agents).toEqual({ status: 'loading', candidates: [] });
  });

  it('an AbortError rejection is silent', async () => {
    const h = createHost({
      load: () => Promise.reject(new DOMException('aborted', 'AbortError')),
    });
    h.open();
    const palette = await waitForPalette();
    await nextTask();
    expect(palette.groups.agents?.status).toBe('loading');
  });
});

describe('QuickPaletteHost: open, close and focus', () => {
  it('close() closes an open palette', async () => {
    const h = createHost();
    const palette = await openReady(h);
    h.close();
    expect(palette.open).toBe(false);
    expect(h.isOpen).toBe(false);
  });

  it('a close before the module loads leaves the palette closed', async () => {
    const h = createHost();
    h.open();
    h.close();
    const palette = await waitForPalette();
    await nextTask();
    expect(palette.open).toBe(false);
  });

  it('a non-selection dismiss refocuses the invoker once the close settles', async () => {
    const invoker = document.createElement('button');
    document.body.append(invoker);
    invoker.focus();
    const h = createHost();
    const palette = await openReady(h);
    invoker.blur();

    palette.dispatchEvent(new CustomEvent('palette-dismiss', { detail: { reason: 'escape' } }));
    expect(palette.open).toBe(false);
    fireFromDialog(palette, 'sl-after-hide');

    expect(document.activeElement).toBe(invoker);
  });

  it('refocuses an invoker inside a shadow root, not its shadow host', async () => {
    const shadowHost = document.createElement('div');
    document.body.append(shadowHost);
    const invoker = document.createElement('button');
    shadowHost.attachShadow({ mode: 'open' }).append(invoker);
    invoker.focus();
    const h = createHost();
    const palette = await openReady(h);
    invoker.blur();

    palette.dispatchEvent(new CustomEvent('palette-dismiss', { detail: { reason: 'escape' } }));
    fireFromDialog(palette, 'sl-after-hide');

    expect(shadowHost.shadowRoot!.activeElement).toBe(invoker);
  });

  it('hide() closes without refocusing the invoker, and leaves a closed palette closed', async () => {
    const invoker = document.createElement('button');
    document.body.append(invoker);
    invoker.focus();
    const contexts: QuickPaletteLoadContext[] = [];
    const h = createHost({
      load: (ctx) => {
        contexts.push(ctx);
        return new Promise(() => {});
      },
    });
    h.open();
    const palette = await waitForPalette();
    await vi.waitFor(() => expect(palette.open).toBe(true));
    invoker.blur();

    h.hide();
    fireFromDialog(palette, 'sl-after-hide');

    expect(palette.open).toBe(false);
    expect(contexts[0].controller.signal.aborted).toBe(true);
    expect(document.activeElement).not.toBe(invoker);
    h.hide();
    expect(h.isOpen).toBe(false);
  });

  it("hide() during a dismiss's close animation skips the refocus", async () => {
    const invoker = document.createElement('button');
    document.body.append(invoker);
    invoker.focus();
    const h = createHost();
    const palette = await openReady(h);
    invoker.blur();

    palette.dispatchEvent(new CustomEvent('palette-dismiss', { detail: { reason: 'escape' } }));
    fireFromDialog(palette, 'sl-hide');
    h.hide();
    fireFromDialog(palette, 'sl-after-hide');

    expect(document.activeElement).not.toBe(invoker);
  });

  it("hide() during a selection's close animation skips onSelectionSettled", async () => {
    const onSelect = vi.fn();
    const onSelectionSettled = vi.fn();
    const h = createHost({ onSelect, onSelectionSettled });
    const palette = await openReady(h);

    select(palette, agentTarget('a1'));
    fireFromDialog(palette, 'sl-hide');
    h.hide();
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();

    expect(onSelect).toHaveBeenCalledTimes(1);
    expect(onSelectionSettled).not.toHaveBeenCalled();
  });

  it("hide() after a selection's close settles, before onSelectionSettled runs, skips it", async () => {
    const onSelectionSettled = vi.fn();
    const h = createHost({ onSelectionSettled });
    const palette = await openReady(h);

    select(palette, agentTarget('a1'));
    fireFromDialog(palette, 'sl-after-hide');
    h.hide();
    await nextTask();

    expect(onSelectionSettled).not.toHaveBeenCalled();
  });

  it("dispose() after a selection's close settles, before onSelectionSettled runs, skips it", async () => {
    const onSelectionSettled = vi.fn();
    const h = createHost({ onSelectionSettled });
    const palette = await openReady(h);

    select(palette, agentTarget('a1'));
    fireFromDialog(palette, 'sl-after-hide');
    h.dispose();
    await nextTask();

    expect(onSelectionSettled).not.toHaveBeenCalled();
  });

  it('dispose() removes the element and later opens do nothing', async () => {
    const load = vi.fn(() => Promise.resolve([candidate('a1')]));
    const h = createHost({ load });
    await openReady(h);
    h.dispose();
    expect(mount.querySelector('scion-quick-palette')).toBeNull();
    h.open();
    expect(h.isOpen).toBe(false);
    expect(load).toHaveBeenCalledTimes(1);
  });
});

describe('QuickPaletteHost: Escape and other closes while the first mount is pending', () => {
  let release: () => void = () => {};

  /** Holds the host's import of the palette module until `release()`. */
  function holdPaletteImport(): void {
    const gate = deferred<void>();
    release = (): void => gate.resolve();
    vi.doMock('./quick-palette.js', async (importOriginal) => {
      await gate.promise;
      return importOriginal();
    });
  }

  afterEach(() => {
    release();
    vi.doUnmock('./quick-palette.js');
  });

  function escapeAt(el: Element, init: KeyboardEventInit = {}): KeyboardEvent {
    const e = new KeyboardEvent('keydown', {
      key: 'Escape',
      bubbles: true,
      composed: true,
      cancelable: true,
      ...init,
    });
    el.dispatchEvent(e);
    return e;
  }

  /** A focused button with its own keydown listener, standing in for a page handler. */
  function focusedKeyTarget(): { el: HTMLButtonElement; onKeydown: ReturnType<typeof vi.fn> } {
    const el = document.createElement('button');
    document.body.append(el);
    const onKeydown = vi.fn();
    el.addEventListener('keydown', onKeydown);
    el.focus();
    return { el, onKeydown };
  }

  /** Releases the import and waits for the element to mount and settle. */
  async function releaseAndMount(): Promise<ScionQuickPalette> {
    release();
    const palette = await waitForPalette();
    await nextTask();
    return palette;
  }

  it('the palette is not shown, nor the element mounted, while the import is pending', async () => {
    holdPaletteImport();
    const h = createHost();
    h.open();
    await nextTask();
    expect(mount.querySelector('scion-quick-palette')).toBeNull();
    expect(h.isOpen).toBe(true);

    const palette = await releaseAndMount();
    expect(palette.open).toBe(true);
  });

  it('Escape while the import is pending cancels the open, and reaches no other listener', async () => {
    holdPaletteImport();
    const contexts: QuickPaletteLoadContext[] = [];
    const h = createHost({
      load: (ctx) => {
        contexts.push(ctx);
        return new Promise(() => {});
      },
    });
    const { el, onKeydown } = focusedKeyTarget();
    const onDocumentKeydown = vi.fn();
    document.addEventListener('keydown', onDocumentKeydown);

    h.open();
    const e = escapeAt(el);
    document.removeEventListener('keydown', onDocumentKeydown);

    expect(e.defaultPrevented).toBe(true);
    expect(onKeydown).not.toHaveBeenCalled();
    expect(onDocumentKeydown).not.toHaveBeenCalled();
    expect(h.isOpen).toBe(false);
    expect(contexts[0].controller.signal.aborted).toBe(true);

    const palette = await releaseAndMount();
    expect(palette.open).toBe(false);
    expect(h.isOpen).toBe(false);
    expect(document.activeElement).toBe(el);
  });

  it('the shortcut opens again after an Escape cancelled the pending open', async () => {
    holdPaletteImport();
    const h = createHost();
    h.open();
    escapeAt(document.body);
    const palette = await releaseAndMount();
    expect(palette.open).toBe(false);

    await openReady(h);
  });

  it('an Escape that is part of an IME composition is left alone while the import is pending', async () => {
    holdPaletteImport();
    const h = createHost();
    const { el, onKeydown } = focusedKeyTarget();

    h.open();
    const e = escapeAt(el, { isComposing: true });

    expect(e.defaultPrevented).toBe(false);
    expect(onKeydown).toHaveBeenCalledTimes(1);
    expect(h.isOpen).toBe(true);
    const palette = await releaseAndMount();
    expect(palette.open).toBe(true);
  });

  it('keys typed while the import is pending reach no other listener, and become the query once the input has focus', async () => {
    holdPaletteImport();
    const h = createHost();
    const { el, onKeydown } = focusedKeyTarget();
    h.open();
    const typed = ['b', 'o', 'b'].map((key) => typeAt(el, key));

    expect(typed.every((e) => e.defaultPrevented)).toBe(true);
    expect(onKeydown).not.toHaveBeenCalled();
    const palette = await releaseAndMount();
    expect(palette.open).toBe(true);
    const input = await fireInitialFocus(palette);
    expect(input.value).toBe('bob');
    expect(input.selectionStart).toBe(3);
    // Captured no longer: the next key is the input's own.
    expect(typeAt(input, 'x').defaultPrevented).toBe(false);
  });

  it('a Ctrl, Meta or Alt chord while the import is pending is left alone', async () => {
    holdPaletteImport();
    const h = createHost();
    const { el, onKeydown } = focusedKeyTarget();
    h.open();
    for (const init of [{ ctrlKey: true }, { metaKey: true }, { altKey: true }]) {
      const e = new KeyboardEvent('keydown', {
        key: 'c',
        bubbles: true,
        cancelable: true,
        ...init,
      });
      // happy-dom reports AltGraph whenever Alt is held; a browser does not for a plain Alt.
      Object.defineProperty(e, 'getModifierState', { value: (): boolean => false });
      el.dispatchEvent(e);
      expect(e.defaultPrevented).toBe(false);
    }
    expect(onKeydown).toHaveBeenCalledTimes(3);
    const palette = await releaseAndMount();
    expect((await fireInitialFocus(palette)).value).toBe('');
  });

  it('Enter and Tab while the import is pending are swallowed, picking nothing and adding no text', async () => {
    holdPaletteImport();
    const onSelect = vi.fn();
    const h = createHost({ onSelect });
    const { el, onKeydown } = focusedKeyTarget();
    h.open();
    typeAt(el, 'a');
    expect(typeAt(el, 'Enter').defaultPrevented).toBe(true);
    expect(typeAt(el, 'Tab').defaultPrevented).toBe(true);
    expect(onKeydown).not.toHaveBeenCalled();

    const palette = await releaseAndMount();
    expect((await fireInitialFocus(palette)).value).toBe('a');
    expect(onSelect).not.toHaveBeenCalled();
    expect(palette.open).toBe(true);
  });

  it('keys are not captured before an open', () => {
    createHost();
    const { el, onKeydown } = focusedKeyTarget();
    expect(typeAt(el, 'a').defaultPrevented).toBe(false);
    expect(onKeydown).toHaveBeenCalledTimes(1);
  });

  it('keys are not captured after the open has shown and the input has focus, nor after the close', async () => {
    const h = createHost();
    const palette = await openReady(h);
    await fireInitialFocus(palette);
    const { el, onKeydown } = focusedKeyTarget();
    expect(typeAt(el, 'a').defaultPrevented).toBe(false);
    h.close();
    expect(typeAt(el, 'b').defaultPrevented).toBe(false);
    expect(onKeydown).toHaveBeenCalledTimes(2);
  });

  it('Escape while the import is pending stops capturing: later keys reach their target', async () => {
    holdPaletteImport();
    const h = createHost();
    const { el, onKeydown } = focusedKeyTarget();
    h.open();
    typeAt(el, 'a');
    escapeAt(el);
    expect(typeAt(el, 'b').defaultPrevented).toBe(false);
    expect(onKeydown).toHaveBeenCalledTimes(1);
    await releaseAndMount();
    expect(typeAt(el, 'c').defaultPrevented).toBe(false);
  });

  it('close(), hide() or dispose() while the import is pending stops capturing', () => {
    holdPaletteImport();
    const h = createHost();
    const { el } = focusedKeyTarget();
    for (const end of [(): void => h.close(), (): void => h.hide(), (): void => h.dispose()]) {
      h.open();
      expect(typeAt(el, 'a').defaultPrevented).toBe(true);
      end();
      expect(typeAt(el, 'b').defaultPrevented).toBe(false);
    }
  });

  it('a mount that fails once the import loads stops capturing, discarding the keys', async () => {
    holdPaletteImport();
    const h = createHost();
    mount.remove();
    const { el, onKeydown } = focusedKeyTarget();
    h.open();
    typeAt(el, 'a');
    release();
    await vi.waitFor(() => expect(h.isOpen).toBe(false));
    expect(typeAt(el, 'b').defaultPrevented).toBe(false);
    expect(onKeydown).toHaveBeenCalledTimes(1);
  });

  it('a modal that opens while the import is pending stops capturing', async () => {
    holdPaletteImport();
    const h = createHost();
    h.open();
    const dialog = document.createElement('dialog');
    document.body.append(dialog);
    dialog.showModal();
    await releaseAndMount();
    expect(h.isOpen).toBe(false);
    const { el, onKeydown } = focusedKeyTarget();
    expect(typeAt(el, 'a').defaultPrevented).toBe(false);
    expect(onKeydown).toHaveBeenCalledTimes(1);
  });

  it('a pending open whose palette never takes focus stops capturing after the time limit', () => {
    holdPaletteImport();
    const h = createHost();
    const { el } = focusedKeyTarget();
    vi.useFakeTimers();
    try {
      h.open();
      expect(typeAt(el, 'a').defaultPrevented).toBe(true);
      vi.advanceTimersByTime(PALETTE_TYPEAHEAD_MAX_MS);
      expect(typeAt(el, 'b').defaultPrevented).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it('Escape is left alone once the palette has shown, and listening stops', async () => {
    holdPaletteImport();
    const h = createHost();
    const removeListener = vi.spyOn(document, 'removeEventListener');
    h.open();
    const palette = await releaseAndMount();
    expect(palette.open).toBe(true);

    expect(removeListener).toHaveBeenCalledWith('keydown', expect.any(Function), true);
    expect(escapeAt(document.body).defaultPrevented).toBe(false);
    expect(h.isOpen).toBe(true);
  });

  it('Escape is left alone after close(), hide() or dispose() while the import is pending', () => {
    holdPaletteImport();
    const h = createHost();
    h.open();
    h.close();
    expect(escapeAt(document.body).defaultPrevented).toBe(false);
    h.open();
    h.hide();
    expect(escapeAt(document.body).defaultPrevented).toBe(false);
    h.open();
    h.dispose();
    expect(escapeAt(document.body).defaultPrevented).toBe(false);
  });

  it('close() while the import is pending stops listening for Escape at once', () => {
    holdPaletteImport();
    const h = createHost();
    const listeners = trackDocumentKeydownCapture();
    h.open();
    expect(listeners.count()).toBe(1);

    h.close();
    expect(listeners.count()).toBe(0);
  });

  it('a mount that fails once the import loads stops listening for Escape', async () => {
    holdPaletteImport();
    const h = createHost();
    mount.remove();
    const removeListener = vi.spyOn(document, 'removeEventListener');
    h.open();
    expect(h.isOpen).toBe(true);
    release();
    await vi.waitFor(() => expect(h.isOpen).toBe(false));

    expect(removeListener).toHaveBeenCalledWith('keydown', expect.any(Function), true);

    expect(escapeAt(document.body).defaultPrevented).toBe(false);
  });

  it('hide() while the import is pending keeps the palette closed', async () => {
    holdPaletteImport();
    const h = createHost();
    h.open();
    h.hide();
    const palette = await releaseAndMount();
    expect(palette.open).toBe(false);
    expect(h.isOpen).toBe(false);
  });

  it('a modal that opens while the import is pending keeps the palette closed', async () => {
    holdPaletteImport();
    const contexts: QuickPaletteLoadContext[] = [];
    const h = createHost({
      load: (ctx) => {
        contexts.push(ctx);
        return new Promise(() => {});
      },
    });
    h.open();
    const dialog = document.createElement('dialog');
    document.body.append(dialog);
    dialog.showModal();

    const palette = await releaseAndMount();
    expect(palette.open).toBe(false);
    expect(h.isOpen).toBe(false);
    expect(contexts[0].controller.signal.aborted).toBe(true);
    expect(escapeAt(document.body).defaultPrevented).toBe(false);
  });

  it('an open again after an Escape, while the same import is pending, shows once it loads', async () => {
    holdPaletteImport();
    const h = createHost();
    h.open();
    escapeAt(document.body);
    h.open();
    expect(h.isOpen).toBe(true);

    const palette = await releaseAndMount();
    expect(palette.open).toBe(true);
    expect(escapeAt(document.body).defaultPrevented).toBe(false);
  });
});

describe('QuickPaletteHost: element leaving the document', () => {
  it('dispose() while the first mount is pending mounts nothing', async () => {
    await import('./quick-palette.js');
    const h = createHost();
    h.open();
    h.dispose();
    await nextTask();
    await nextTask();
    expect(mount.querySelector('scion-quick-palette')).toBeNull();
    expect(h.element).toBeNull();
    expect(h.isOpen).toBe(false);
  });

  it('an open palette whose element is removed is no longer open, and reopening mounts a new one', async () => {
    const h = createHost();
    const first = await openReady(h);

    mount.replaceChildren();
    expect(h.isOpen).toBe(false);

    h.open();
    const second = await openReady(h);
    expect(second).not.toBe(first);
    expect(second.isConnected).toBe(true);
    expect(h.isOpen).toBe(true);
    expect(mount.querySelectorAll('scion-quick-palette')).toHaveLength(1);
  });

  it('a closed palette whose element is removed opens again in a new element', async () => {
    const h = createHost();
    const first = await openReady(h);
    h.close();
    fireFromDialog(first, 'sl-after-hide');

    mount.replaceChildren();
    const second = await openReady(h);

    expect(second).not.toBe(first);
    expect(second.isConnected).toBe(true);
  });

  it('a mount out of the document leaves the palette closed', async () => {
    const h = createHost();
    mount.remove();
    h.open();
    await vi.waitFor(() => expect(h.isOpen).toBe(false));

    // Not left half-open, to show up once the mount is attached.
    document.body.append(mount);
    await nextTask();
    expect(h.isOpen).toBe(false);
    expect(mount.querySelector('scion-quick-palette')).toBeNull();

    await openReady(h);
  });
});

describe('QuickPaletteHost: setCandidates', () => {
  it('publishes ready candidates over a load still in flight', async () => {
    const pending = deferred<PaletteCandidate[]>();
    const h = createHost({ load: () => pending.promise });
    h.open();
    const palette = await waitForPalette();

    h.setCandidates([candidate('b1', 'Bravo')]);
    expect(palette.groups.agents).toEqual({
      status: 'ready',
      candidates: [candidate('b1', 'Bravo')],
    });

    pending.resolve([candidate('a1', 'Alpha')]);
    await nextTask();
    expect(palette.groups.agents?.candidates).toEqual([candidate('b1', 'Bravo')]);
  });

  it('aborts the load in flight', async () => {
    const contexts: QuickPaletteLoadContext[] = [];
    const h = createHost({
      load: (ctx) => {
        contexts.push(ctx);
        return new Promise(() => {});
      },
    });
    h.open();
    await waitForPalette();

    h.setCandidates([candidate('b1', 'Bravo')]);

    expect(contexts[0].controller.signal.aborted).toBe(true);
  });

  it('supersedes the load in flight, so its progress no longer publishes', async () => {
    const contexts: QuickPaletteLoadContext[] = [];
    const h = createHost({
      load: (ctx) => {
        contexts.push(ctx);
        return new Promise(() => {});
      },
    });
    h.open();
    const palette = await waitForPalette();

    h.setCandidates([candidate('b1', 'Bravo')]);
    contexts[0].onProgress([candidate('a1', 'Alpha')]);

    expect(contexts[0].isCurrent()).toBe(false);
    expect(palette.groups.agents).toEqual({
      status: 'ready',
      candidates: [candidate('b1', 'Bravo')],
    });
  });

  it('a later selection is checked against the published candidates', async () => {
    const onSelect = vi.fn();
    const h = createHost({ onSelect });
    const palette = await openReady(h);

    h.setCandidates([candidate('b1')]);
    select(palette, agentTarget('a1'));
    expect(onSelect).not.toHaveBeenCalled();
  });
});

describe('QuickPaletteHost: selection', () => {
  it('a present agent closes the palette, reaches onSelect, then settles without refocusing the invoker', async () => {
    const invoker = document.createElement('button');
    document.body.append(invoker);
    invoker.focus();
    const events: string[] = [];
    const h = createHost({
      onSelect: (target) => events.push(`select:${target.agentId}`),
      onSelectionSettled: () => events.push('settled'),
    });
    const palette = await openReady(h);
    invoker.blur();

    select(palette, agentTarget('a1'));
    expect(palette.open).toBe(false);
    expect(events).toEqual(['select:a1']);

    fireFromDialog(palette, 'sl-after-hide');
    // Settles in a later task than Shoelace's own focus restore.
    expect(events).toEqual(['select:a1']);
    await nextTask();
    expect(events).toEqual(['select:a1', 'settled']);
    expect(document.activeElement).not.toBe(invoker);
  });

  it('an agent no longer among the loaded candidates is ignored', async () => {
    const onSelect = vi.fn();
    const onSelectionSettled = vi.fn();
    const h = createHost({ onSelect, onSelectionSettled });
    const palette = await openReady(h);

    select(palette, agentTarget('gone'));
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();

    expect(palette.open).toBe(false);
    expect(onSelect).not.toHaveBeenCalled();
    expect(onSelectionSettled).not.toHaveBeenCalled();
  });

  it('a non-agent target is ignored, even with a present agent ID', async () => {
    const onSelect = vi.fn();
    const h = createHost({ onSelect });
    const palette = await openReady(h);
    select(palette, { kind: 'dm', peerKind: 'agent', peerId: 'a1', displayName: 'a1' });
    expect(onSelect).not.toHaveBeenCalled();
  });

  it('a selection does not carry over into the next dismiss', async () => {
    const invoker = document.createElement('button');
    document.body.append(invoker);
    const onSelectionSettled = vi.fn();
    const h = createHost({ onSelectionSettled });
    const first = await openReady(h);
    select(first, agentTarget('a1'));
    fireFromDialog(first, 'sl-after-hide');
    await nextTask();
    expect(onSelectionSettled).toHaveBeenCalledTimes(1);

    invoker.focus();
    const palette = await openReady(h);
    invoker.blur();
    palette.dispatchEvent(new CustomEvent('palette-dismiss', { detail: { reason: 'escape' } }));
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();

    expect(onSelectionSettled).toHaveBeenCalledTimes(1);
    expect(document.activeElement).toBe(invoker);
  });
});

describe('QuickPaletteHost: opening again during the close animation', () => {
  /** Opens with `invoker` focused, then dismisses; the close animation is left running. */
  async function openThenStartClosing(
    h: QuickPaletteHost,
    invoker: HTMLElement
  ): Promise<ScionQuickPalette> {
    invoker.focus();
    const palette = await openReady(h);
    // Focus moves into the palette while it is open.
    palette.shadowRoot!.querySelector('input')!.focus();
    palette.dispatchEvent(new CustomEvent('palette-dismiss', { detail: { reason: 'escape' } }));
    fireFromDialog(palette, 'sl-hide');
    return palette;
  }

  function button(): HTMLButtonElement {
    const el = document.createElement('button');
    document.body.append(el);
    return el;
  }

  it('shows the palette once the animation ends, and a later dismiss refocuses the first invoker', async () => {
    const invoker = button();
    const h = createHost();
    const palette = await openThenStartClosing(h, invoker);

    h.open();
    await nextTask();
    expect(h.isOpen).toBe(true);
    expect(palette.open).toBe(false);

    fireFromDialog(palette, 'sl-after-hide');
    expect(document.activeElement).not.toBe(invoker);
    await nextTask();
    expect(palette.open).toBe(true);
    expect(palette.groups.agents?.status).toBe('ready');

    palette.shadowRoot!.querySelector('input')!.focus();
    palette.dispatchEvent(new CustomEvent('palette-dismiss', { detail: { reason: 'escape' } }));
    fireFromDialog(palette, 'sl-hide');
    fireFromDialog(palette, 'sl-after-hide');
    expect(palette.open).toBe(false);
    expect(document.activeElement).toBe(invoker);
  });

  /** A later open shows the palette at once, with no close animation left to wait for. */
  async function expectOpensAgain(h: QuickPaletteHost, palette: ScionQuickPalette): Promise<void> {
    h.open();
    await vi.waitFor(() => expect(palette.open).toBe(true));
    expect(h.isOpen).toBe(true);
  }

  function pressEscape(): KeyboardEvent {
    const e = new KeyboardEvent('keydown', {
      key: 'Escape',
      bubbles: true,
      composed: true,
      cancelable: true,
    });
    (document.activeElement ?? document.body).dispatchEvent(e);
    return e;
  }

  it('a close before the animation ends cancels the reopen and refocuses the first invoker', async () => {
    const invoker = button();
    const h = createHost();
    const palette = await openThenStartClosing(h, invoker);

    h.open();
    h.close();
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();

    expect(palette.open).toBe(false);
    expect(h.isOpen).toBe(false);
    expect(document.activeElement).toBe(invoker);
    await expectOpensAgain(h, palette);
  });

  it('a close after the animation ends but before the reopen shows refocuses the first invoker', async () => {
    const invoker = button();
    const h = createHost();
    const palette = await openThenStartClosing(h, invoker);

    h.open();
    fireFromDialog(palette, 'sl-after-hide');
    h.close();
    await nextTask();

    expect(palette.open).toBe(false);
    expect(document.activeElement).toBe(invoker);
    await expectOpensAgain(h, palette);
  });

  it('Escape before the animation ends cancels the reopen and refocuses the first invoker', async () => {
    const invoker = button();
    const h = createHost();
    const palette = await openThenStartClosing(h, invoker);

    h.open();
    const escape = pressEscape();
    expect(escape.defaultPrevented).toBe(true);
    expect(h.isOpen).toBe(false);
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();

    expect(palette.open).toBe(false);
    expect(document.activeElement).toBe(invoker);
    await expectOpensAgain(h, palette);
  });

  it('Escape after the animation ends but before the reopen shows cancels it', async () => {
    const invoker = button();
    const h = createHost();
    const palette = await openThenStartClosing(h, invoker);

    h.open();
    fireFromDialog(palette, 'sl-after-hide');
    pressEscape();
    await nextTask();

    expect(palette.open).toBe(false);
    expect(h.isOpen).toBe(false);
    expect(document.activeElement).toBe(invoker);
    await expectOpensAgain(h, palette);
  });

  it('Escape is left alone once the reopen has shown, or has been cancelled', async () => {
    const h = createHost();
    const palette = await openThenStartClosing(h, button());

    h.open();
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();
    expect(palette.open).toBe(true);
    expect(pressEscape().defaultPrevented).toBe(false);
    expect(h.isOpen).toBe(true);

    palette.dispatchEvent(new CustomEvent('palette-dismiss', { detail: { reason: 'escape' } }));
    fireFromDialog(palette, 'sl-hide');
    h.open();
    h.close();
    expect(pressEscape().defaultPrevented).toBe(false);

    h.open();
    h.dispose();
    expect(pressEscape().defaultPrevented).toBe(false);
  });

  it('keys typed while the reopen is pending become the query of the reopened palette', async () => {
    const h = createHost();
    const palette = await openThenStartClosing(h, button());

    h.open();
    const e = typeAt(document.body, 'a');

    expect(e.defaultPrevented).toBe(true);
    expect(h.isOpen).toBe(true);
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();
    expect(palette.open).toBe(true);
    expect((await fireInitialFocus(palette)).value).toBe('a');
  });

  it('a close while the reopen is pending stops capturing', async () => {
    const h = createHost();
    await openThenStartClosing(h, button());
    h.open();
    h.close();
    expect(typeAt(document.body, 'a').defaultPrevented).toBe(false);
  });

  /** A focused element with its own keydown listener, standing in for xterm or a page handler. */
  function keyTarget(): { el: HTMLButtonElement; onKeydown: ReturnType<typeof vi.fn> } {
    const el = button();
    const onKeydown = vi.fn();
    el.addEventListener('keydown', onKeydown);
    el.focus();
    return { el, onKeydown };
  }

  function escapeAt(el: HTMLElement, init: KeyboardEventInit = {}): KeyboardEvent {
    const e = new KeyboardEvent('keydown', {
      key: 'Escape',
      bubbles: true,
      composed: true,
      cancelable: true,
      ...init,
    });
    el.dispatchEvent(e);
    return e;
  }

  it('an Escape while the reopen is pending reaches no other keydown listener', async () => {
    const h = createHost();
    await openThenStartClosing(h, button());
    h.open();
    const onDocumentKeydown = vi.fn();
    document.addEventListener('keydown', onDocumentKeydown);
    const { el, onKeydown } = keyTarget();

    const e = escapeAt(el);
    document.removeEventListener('keydown', onDocumentKeydown);

    expect(e.defaultPrevented).toBe(true);
    expect(onKeydown).not.toHaveBeenCalled();
    expect(onDocumentKeydown).not.toHaveBeenCalled();
    expect(h.isOpen).toBe(false);
  });

  it('an Escape that is part of an IME composition is left alone while the reopen is pending', async () => {
    const h = createHost();
    const palette = await openThenStartClosing(h, button());
    h.open();
    const { el, onKeydown } = keyTarget();

    const e = escapeAt(el, { isComposing: true });

    expect(e.defaultPrevented).toBe(false);
    expect(onKeydown).toHaveBeenCalledTimes(1);
    expect(h.isOpen).toBe(true);
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();
    expect(palette.open).toBe(true);
  });

  it('an element removed while the reopen is pending lets a later Escape through', async () => {
    const h = createHost();
    await openThenStartClosing(h, button());
    h.open();

    mount.replaceChildren();
    expect(h.isOpen).toBe(false);
    const { el, onKeydown } = keyTarget();
    const first = escapeAt(el);
    const second = escapeAt(el);

    expect(first.defaultPrevented).toBe(false);
    expect(second.defaultPrevented).toBe(false);
    expect(onKeydown).toHaveBeenCalledTimes(2);
  });

  it('close() while the reopen is pending stops listening for Escape at once', async () => {
    const h = createHost();
    await openThenStartClosing(h, button());
    const listeners = trackDocumentKeydownCapture();
    h.open();
    expect(listeners.count()).toBe(1);

    h.close();
    expect(listeners.count()).toBe(0);
  });

  it('the next open after an element is removed mid-reopen stops listening for Escape', async () => {
    const h = createHost();
    await openThenStartClosing(h, button());
    h.open();
    mount.replaceChildren();
    const removeListener = vi.spyOn(document, 'removeEventListener');

    h.open();
    const second = await waitForPalette();
    await vi.waitFor(() => expect(second.open).toBe(true));

    expect(removeListener).toHaveBeenCalledWith('keydown', expect.any(Function), true);
  });

  it('an element removed after the animation ends but before the reopen shows stays closed', async () => {
    const contexts: QuickPaletteLoadContext[] = [];
    const h = createHost({
      load: (ctx) => {
        contexts.push(ctx);
        return contexts.length === 1 ? Promise.resolve([candidate('a1')]) : new Promise(() => {});
      },
    });
    const palette = await openThenStartClosing(h, button());

    h.open();
    fireFromDialog(palette, 'sl-after-hide');
    palette.remove();
    await nextTask();

    expect(palette.open).toBe(false);
    expect(h.isOpen).toBe(false);
    expect(contexts[1].controller.signal.aborted).toBe(true);
  });

  it('Enter in the closing dialog while the reopen is pending picks nothing, and the reopen still shows', async () => {
    const onSelect = vi.fn();
    const onSelectionSettled = vi.fn();
    const h = createHost({ onSelect, onSelectionSettled });
    button().focus();
    const palette = await openReady(h);
    // A row picked while open stays active through the dismiss.
    const input = await fireInitialFocus(palette);
    typeAt(input, 'ArrowDown');
    await palette.updateComplete;
    palette.dispatchEvent(new CustomEvent('palette-dismiss', { detail: { reason: 'escape' } }));
    fireFromDialog(palette, 'sl-hide');

    // The reopen captures keys until its time limit; after that, Enter
    // reaches the closing dialog, which commits its active row.
    vi.useFakeTimers();
    try {
      h.open();
      vi.advanceTimersByTime(PALETTE_TYPEAHEAD_MAX_MS);
    } finally {
      vi.useRealTimers();
    }
    await vi.waitFor(() => expect(palette.groups.agents?.status).toBe('ready'));
    await palette.updateComplete;
    const picks: Event[] = [];
    palette.addEventListener('palette-select', (e) => picks.push(e));
    typeAt(input, 'Enter');

    expect(picks).toHaveLength(1);
    expect(onSelect).not.toHaveBeenCalled();
    expect(h.isOpen).toBe(true);
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();
    await nextTask();
    expect(palette.open).toBe(true);
    expect(onSelectionSettled).not.toHaveBeenCalled();
  });

  it('a pick in a dialog that is closing after a dismiss is ignored', async () => {
    const onSelect = vi.fn();
    const invoker = button();
    const h = createHost({ onSelect });
    const palette = await openThenStartClosing(h, invoker);

    select(palette, agentTarget('a1'));
    fireFromDialog(palette, 'sl-after-hide');

    expect(onSelect).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(invoker);
  });

  it('an element removed during the animation is replaced, and shown, by the next open', async () => {
    const h = createHost();
    const palette = await openThenStartClosing(h, button());

    mount.replaceChildren();
    h.open();
    const second = await waitForPalette();
    await vi.waitFor(() => expect(second.open).toBe(true));

    expect(second).not.toBe(palette);
    expect(h.isOpen).toBe(true);
  });

  it('an odd number of opens and closes ends open, an even number closed', async () => {
    const h = createHost();
    const palette = await openThenStartClosing(h, button());

    h.open();
    h.close();
    h.open();
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();
    expect(palette.open).toBe(true);
  });

  it('opens normally once the animation has ended', async () => {
    const invoker = button();
    const h = createHost();
    const palette = await openThenStartClosing(h, button());
    fireFromDialog(palette, 'sl-after-hide');

    invoker.focus();
    h.open();
    await vi.waitFor(() => expect(palette.open).toBe(true));
    palette.dispatchEvent(new CustomEvent('palette-dismiss', { detail: { reason: 'escape' } }));
    fireFromDialog(palette, 'sl-after-hide');
    expect(document.activeElement).toBe(invoker);
  });

  it('dispose() during the animation shows nothing and refocuses nothing', async () => {
    const invoker = button();
    const h = createHost();
    const palette = await openThenStartClosing(h, invoker);

    h.open();
    h.dispose();
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();

    expect(palette.open).toBe(false);
    expect(document.activeElement).not.toBe(invoker);
  });

  it('a modal that opens during the animation keeps the palette closed', async () => {
    const h = createHost();
    const palette = await openThenStartClosing(h, button());

    h.open();
    const other = document.createElement('sl-dialog') as HTMLElement & { open: boolean };
    other.open = true;
    document.body.append(other);
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();

    expect(palette.open).toBe(false);
    expect(h.isOpen).toBe(false);

    other.remove();
    await expectOpensAgain(h, palette);
  });

  it('a modal that opens during the animation aborts the load the reopen started', async () => {
    const contexts: QuickPaletteLoadContext[] = [];
    const h = createHost({
      load: (ctx) => {
        contexts.push(ctx);
        return contexts.length === 1 ? Promise.resolve([candidate('a1')]) : new Promise(() => {});
      },
    });
    const palette = await openThenStartClosing(h, button());

    h.open();
    expect(contexts).toHaveLength(2);
    const other = document.createElement('sl-dialog') as HTMLElement & { open: boolean };
    other.open = true;
    document.body.append(other);
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();

    expect(contexts[1].controller.signal.aborted).toBe(true);
  });

  it('a pick followed by a reopen during the animation reaches onSelect but not onSelectionSettled', async () => {
    const onSelect = vi.fn();
    const onSelectionSettled = vi.fn();
    const h = createHost({ onSelect, onSelectionSettled });
    const palette = await openReady(h);

    select(palette, agentTarget('a1'));
    fireFromDialog(palette, 'sl-hide');
    h.open();
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();
    await nextTask();

    expect(onSelect).toHaveBeenCalledTimes(1);
    expect(onSelectionSettled).not.toHaveBeenCalled();
    expect(palette.open).toBe(true);
  });
});

describe('QuickPaletteHost: events from inside the palette', () => {
  /** Fires `type` from an element inside the palette's dialog, as a nested Shoelace overlay would. */
  function fireFromNested(palette: ScionQuickPalette, type: 'sl-hide' | 'sl-after-hide'): void {
    const nested = document.createElement('div');
    palette.shadowRoot!.querySelector('sl-dialog')!.append(nested);
    nested.dispatchEvent(new Event(type, { bubbles: true, composed: true }));
    nested.remove();
  }

  it('a nested sl-hide does not defer the next open', async () => {
    const h = createHost();
    const palette = await openReady(h);

    fireFromNested(palette, 'sl-hide');
    h.close();
    h.open();

    await vi.waitFor(() => expect(palette.open).toBe(true));
  });

  it('a nested sl-after-hide does not settle a selection', async () => {
    const onSelectionSettled = vi.fn();
    const h = createHost({ onSelectionSettled });
    const palette = await openReady(h);

    select(palette, agentTarget('a1'));
    fireFromNested(palette, 'sl-after-hide');
    await nextTask();
    expect(onSelectionSettled).not.toHaveBeenCalled();

    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();
    expect(onSelectionSettled).toHaveBeenCalledTimes(1);
  });
});

describe('QuickPaletteHost: modal guard', () => {
  it('ignores its own open palette but sees another open dialog', async () => {
    const h = createHost();
    const palette = await openReady(h);
    // happy-dom does not define sl-dialog, so mirror the open state Shoelace
    // would expose on the palette's own dialog.
    const ownDialog = palette.shadowRoot!.querySelector('sl-dialog') as HTMLElement & {
      open?: boolean;
    };
    ownDialog.open = true;
    expect(hasOpenModalDescendant(document, null)).toBe(true);
    expect(h.hasUnrelatedModalOpen()).toBe(false);

    const other = document.createElement('sl-dialog') as HTMLElement & { open: boolean };
    other.open = true;
    document.body.append(other);
    expect(h.hasUnrelatedModalOpen()).toBe(true);
  });
});

describe('isQuickPaletteShortcut', () => {
  const key = (init: KeyboardEventInit): KeyboardEvent => new KeyboardEvent('keydown', init);

  it('accepts K with exactly one of Ctrl and Meta, in either case', () => {
    expect(isQuickPaletteShortcut(key({ key: 'k', metaKey: true }))).toBe(true);
    expect(isQuickPaletteShortcut(key({ key: 'k', ctrlKey: true }))).toBe(true);
    expect(isQuickPaletteShortcut(key({ key: 'K', ctrlKey: true }))).toBe(true);
  });

  it('rejects other chords, repeats, composition and handled events', () => {
    expect(isQuickPaletteShortcut(key({ key: 'k' }))).toBe(false);
    expect(isQuickPaletteShortcut(key({ key: 'k', ctrlKey: true, metaKey: true }))).toBe(false);
    expect(isQuickPaletteShortcut(key({ key: 'k', ctrlKey: true, altKey: true }))).toBe(false);
    expect(isQuickPaletteShortcut(key({ key: 'k', metaKey: true, shiftKey: true }))).toBe(false);
    expect(isQuickPaletteShortcut(key({ key: 'j', metaKey: true }))).toBe(false);
    expect(isQuickPaletteShortcut(key({ key: 'k', metaKey: true, repeat: true }))).toBe(false);
    expect(isQuickPaletteShortcut(key({ key: 'k', metaKey: true, isComposing: true }))).toBe(false);
    const handled = key({ key: 'k', metaKey: true, cancelable: true });
    handled.preventDefault();
    expect(isQuickPaletteShortcut(handled)).toBe(false);
  });

  /**
   * Whether a K keydown typed in `el`, which is put in the document, is the
   * shortcut, as a document listener sees it during dispatch (an event's
   * path is cleared once dispatch ends).
   */
  const shortcutTypedIn = (el: HTMLElement, init: KeyboardEventInit): boolean => {
    document.body.append(el);
    let result: boolean | undefined;
    document.addEventListener('keydown', (e) => (result = isQuickPaletteShortcut(e)), {
      once: true,
    });
    el.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', bubbles: true, ...init }));
    expect(result).toBeDefined();
    return result!;
  };
  const inputOf = (
    type: string,
    attrs: { readOnly?: boolean; disabled?: boolean } = {}
  ): HTMLInputElement => {
    const input = document.createElement('input');
    input.type = type;
    input.readOnly = attrs.readOnly ?? false;
    input.disabled = attrs.disabled ?? false;
    return input;
  };
  const editable = (): HTMLElement => {
    const el = document.createElement('div');
    el.contentEditable = 'true';
    return el;
  };

  it('on a Mac, leaves Ctrl+K typed in an editable text field to the field', () => {
    vi.spyOn(navigator, 'platform', 'get').mockReturnValue('MacIntel');
    for (const type of ['text', 'search', 'email', 'url', 'tel', 'password', 'number']) {
      expect(shortcutTypedIn(inputOf(type), { ctrlKey: true })).toBe(false);
    }
    const textarea = document.createElement('textarea');
    expect(shortcutTypedIn(textarea, { ctrlKey: true })).toBe(false);
    expect(shortcutTypedIn(editable(), { ctrlKey: true })).toBe(false);
  });

  it('on a Mac, a field inside a shadow root counts by its own element', () => {
    vi.spyOn(navigator, 'platform', 'get').mockReturnValue('MacIntel');
    const host = document.createElement('div');
    document.body.append(host);
    const input = inputOf('text');
    host.attachShadow({ mode: 'open' }).append(input);
    let result: boolean | undefined;
    document.addEventListener('keydown', (e) => (result = isQuickPaletteShortcut(e)), {
      once: true,
    });
    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'k', ctrlKey: true, bubbles: true, composed: true })
    );
    expect(result).toBe(false);
  });

  it('on a Mac, Ctrl+K on a non-text, read-only or disabled field is the shortcut', () => {
    vi.spyOn(navigator, 'platform', 'get').mockReturnValue('MacIntel');
    const fields: HTMLElement[] = [
      inputOf('checkbox'),
      inputOf('button'),
      inputOf('text', { readOnly: true }),
      inputOf('text', { disabled: true }),
      Object.assign(document.createElement('textarea'), { readOnly: true }),
      Object.assign(document.createElement('textarea'), { disabled: true }),
      document.createElement('button'),
    ];
    for (const el of fields) {
      expect(shortcutTypedIn(el, { ctrlKey: true })).toBe(true);
    }
  });

  it('on a Mac, Cmd+K typed in a text field is the shortcut', () => {
    vi.spyOn(navigator, 'platform', 'get').mockReturnValue('MacIntel');
    expect(shortcutTypedIn(inputOf('text'), { metaKey: true })).toBe(true);
    expect(shortcutTypedIn(editable(), { metaKey: true })).toBe(true);
  });

  it('off a Mac, Ctrl+K typed in a text field is the shortcut', () => {
    vi.spyOn(navigator, 'platform', 'get').mockReturnValue('Linux x86_64');
    expect(shortcutTypedIn(inputOf('text'), { ctrlKey: true })).toBe(true);
    const textarea = document.createElement('textarea');
    expect(shortcutTypedIn(textarea, { ctrlKey: true })).toBe(true);
  });
});
