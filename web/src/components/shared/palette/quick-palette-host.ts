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
 * Host lifecycle for an agents-only `<scion-quick-palette>`: everything a
 * surface needs to offer a "Jump to agent" palette except what a pick does.
 *
 * - The component module is imported, and its element created and appended
 *   to `mount`, on the first open only, so neither enters the main bundle.
 *   An element found out of the document at a later open (its mount's
 *   content was replaced) is dropped and a new one mounted, so an open
 *   always shows a palette.
 * - Every open (re)loads the one Agents group through `load`. A newer load
 *   or a close aborts the previous one, and a superseded load can never
 *   publish over a newer one.
 * - A non-selection dismiss (Escape, backdrop, close button) refocuses the
 *   element that was focused when the palette opened. A selection does not:
 *   the surface moves focus to what was picked, from `onSelectionSettled`,
 *   which runs once the close animation and Shoelace's own focus restore
 *   have both finished.
 * - A selection is checked against the last loaded candidates before
 *   `onSelect` sees it, since the palette's rows are stale UI state.
 * - {@link hide} closes without moving focus anywhere when the close
 *   settles, for a surface that is going off screen (its invoker is going
 *   with it).
 * - An open while the close animation is still running is deferred until
 *   that animation ends, since Shoelace would otherwise finish the hide over
 *   the reopen and leave an invisible dialog holding focus. The reopen keeps
 *   the invoker of the open being closed, and replaces that close's own
 *   focus handling: after a pick, `onSelect` has run but
 *   `onSelectionSettled` does not, since focus belongs to the reopened
 *   palette. A pick made in the closing dialog is ignored.
 * - Keys typed from the open until the query input has focus become the
 *   query (see {@link PaletteTypeahead}), however long the module takes
 *   to load, rather than reaching the element focused before the open.
 * - An open is pending until its palette shows: while the element mounts
 *   (on a first open, while the module loads) or while a reopen waits out
 *   the close animation. Escape meanwhile closes it, as it would the shown
 *   palette, and so do {@link close}, {@link hide} and {@link dispose}. A
 *   pending open never shows over a modal that opened in the meantime.
 *
 * The surface owns its shortcut listener (see {@link isQuickPaletteShortcut})
 * and the open-request event its header button sends.
 */

import type {
  GroupState,
  PaletteAgentTarget,
  PaletteCandidate,
  PaletteGroup,
  PaletteTarget,
} from '../../../client/chat-palette-types.js';
import type { ScionQuickPalette } from './quick-palette.js';
import { hasOpenModalDescendant } from '../open-modal.js';
import { deepActiveElement } from '../deep-active-element.js';
import { PaletteTypeahead } from './palette-typeahead.js';

/** What {@link QuickPaletteHostOptions.load} receives for one load. */
export interface QuickPaletteLoadContext {
  /** The load's own controller: aborted when the load is superseded or the palette closes. */
  controller: AbortController;
  /** Whether this load is still the host's current one. */
  isCurrent: () => boolean;
  /** Publishes partial candidates while the load is still running. */
  onProgress: (candidates: PaletteCandidate[]) => void;
}

export interface QuickPaletteHostOptions {
  /** Where the palette element is appended on first open. */
  mount: Element | DocumentFragment;
  /** The dialog's accessible label. */
  label: string;
  /** The query input's placeholder. */
  placeholder: string;
  /**
   * Loads the Agents group's candidates. Rejecting with an AbortError is
   * silent; any other rejection shows the group's error state, with retry.
   */
  load: (context: QuickPaletteLoadContext) => Promise<PaletteCandidate[]>;
  /**
   * Called with a picked agent that is still among the loaded candidates,
   * after the palette has started to close.
   */
  onSelect: (target: PaletteAgentTarget) => void;
  /**
   * Called once a close that a selection caused has fully settled: the close
   * animation is over and Shoelace has restored focus to the dialog's
   * trigger, so focus moved from here sticks.
   */
  onSelectionSettled?: () => void;
}

/**
 * Whether `e` is the quick palette's shortcut: K with exactly one of Ctrl
 * and Meta, no Alt or Shift, not a repeat, not mid-composition, and not
 * already handled.
 */
export function isQuickPaletteShortcut(e: KeyboardEvent): boolean {
  if (e.defaultPrevented || e.repeat || e.isComposing) return false;
  if (e.altKey || e.shiftKey) return false;
  if (e.metaKey === e.ctrlKey) return false;
  return e.key.toLowerCase() === 'k';
}

/** Whether `e` was fired by `palette`'s own dialog, not by something inside it. */
function isFromOwnDialog(palette: ScionQuickPalette, e: Event): boolean {
  const dialog = palette.shadowRoot?.querySelector('sl-dialog');
  return dialog != null && e.composedPath()[0] === dialog;
}

export class QuickPaletteHost {
  private readonly options: QuickPaletteHostOptions;
  private palette: ScionQuickPalette | null = null;
  private paletteMount: Promise<ScionQuickPalette> | null = null;
  /** Whether the palette is open or opening (its module may still be loading). */
  private paletteOpen = false;
  private groups: Partial<Record<PaletteGroup, GroupState>> = {
    agents: { status: 'loading', candidates: [] },
  };
  private abort: AbortController | null = null;
  /** Bumped on every {@link load} call, so a superseded load can't publish over a newer one. */
  private generation = 0;
  /** The element focused when the palette opened, refocused on a non-selection dismiss. */
  private invoker: HTMLElement | null = null;
  private closedBySelection = false;
  /** The timer that runs `onSelectionSettled` once a selection's close settles. */
  private settleTimer: ReturnType<typeof setTimeout> | undefined;
  private disposed = false;
  /**
   * The palette element whose dialog is running its close animation: set on
   * the dialog's `sl-hide`, cleared once its `sl-after-hide` has been
   * handled. An open meanwhile only marks the palette open, and
   * {@link handleAfterHide} shows it.
   */
  private hidingPalette: ScionQuickPalette | null = null;
  /**
   * The mount the current open is waiting on, from {@link open} until it
   * settles. A mount that settles after a later open has replaced it does
   * nothing.
   */
  private pendingMount: Promise<ScionQuickPalette> | null = null;
  /** Whether {@link handlePendingEscape} is listening, while an open is pending. */
  private listeningForEscape = false;
  /** Captures keys typed from {@link open} until the palette's input has focus. */
  private readonly typeahead = new PaletteTypeahead();

  constructor(options: QuickPaletteHostOptions) {
    this.options = options;
  }

  /**
   * Whether the palette is open or opening. A palette whose element has
   * left the document is not open, whatever it was last told.
   */
  get isOpen(): boolean {
    return this.paletteOpen && (this.palette?.isConnected ?? true);
  }

  /** The palette element, once the first open has mounted it. */
  get element(): ScionQuickPalette | null {
    return this.palette;
  }

  /**
   * Opens the palette: captures the focused element to refocus on a
   * non-selection dismiss, and (re)loads the Agents group, since agents can
   * change between opens.
   *
   * The element is mounted closed and only then opened, so Shoelace always
   * sees a real false->true transition on a connected element.
   */
  open(): void {
    if (this.isOpen || this.disposed) return;
    const hiding = this.isHiding();
    // A reopen during the close animation keeps the invoker it already has:
    // focus is still inside the closing palette, or Shoelace has blurred it.
    if (!hiding) {
      const invoker = deepActiveElement();
      this.invoker = invoker instanceof HTMLElement ? invoker : null;
    }
    this.closedBySelection = false;
    this.paletteOpen = true;
    this.typeahead.start();
    void this.load();
    if (hiding) {
      // The closing dialog no longer handles Escape, so listen for it here
      // until the reopen shows or is cancelled.
      this.listenForEscape(true);
      return;
    }
    const mount = this.mountPalette();
    this.pendingMount = mount;
    // Nothing handles Escape until the palette shows. A reopen pending on an
    // element that left the document can never show, so this open now owns
    // the listener.
    this.listenForEscape(true);
    mount.then(
      (palette) => {
        if (this.pendingMount !== mount) return;
        this.settleMount();
        if (this.paletteOpen && !this.hasUnrelatedModalOpen()) {
          palette.open = true;
          return;
        }
        this.cancelPendingOpen();
      },
      () => {
        if (this.pendingMount !== mount) return;
        this.settleMount();
        this.cancelPendingOpen();
      }
    );
  }

  /**
   * Closes the palette, refocusing its invoker once the close settles. An
   * open still pending does not show.
   */
  close(): void {
    this.listenForEscape(false);
    this.paletteOpen = false;
    this.typeahead.stop();
    if (this.palette) this.palette.open = false;
    this.abort?.abort();
  }

  /**
   * Closes the palette, if it is open, and moves focus nowhere when the
   * close settles: neither to the invoker nor through `onSelectionSettled`,
   * even for a close already animating or one that has settled but not yet
   * run `onSelectionSettled`. For a surface going off screen,
   * whose invoker goes with it. Closing also releases Shoelace's focus trap
   * and scroll lock, which would otherwise stay active on whatever is shown
   * next.
   */
  hide(): void {
    clearTimeout(this.settleTimer);
    this.invoker = null;
    this.closedBySelection = false;
    this.close();
  }

  /** Hides the palette, removes its element, and makes later opens no-ops. */
  dispose(): void {
    this.hide();
    this.disposed = true;
    this.palette?.remove();
  }

  /**
   * Whether a modal other than this palette is open anywhere on the page,
   * including inside shadow roots and excluding hidden subtrees (see
   * {@link hasOpenModalDescendant}). Surfaces check this before opening
   * from a shortcut, so the palette never stacks over another dialog.
   */
  hasUnrelatedModalOpen(): boolean {
    return hasOpenModalDescendant(document, this.palette);
  }

  /**
   * Loads the component and creates its element, once, or again if the
   * element has left the document. A failed load is not cached; a host
   * disposed while the module loads, or a mount out of the document, mounts
   * nothing.
   */
  private mountPalette(): Promise<ScionQuickPalette> {
    if (this.palette && !this.palette.isConnected) {
      this.hidingPalette = null;
      this.palette = null;
      this.paletteMount = null;
    }
    this.paletteMount ??= import('./quick-palette.js')
      .then(async () => {
        if (this.disposed) throw new DOMException('Palette host disposed', 'AbortError');
        const palette = document.createElement('scion-quick-palette');
        palette.label = this.options.label;
        palette.placeholder = this.options.placeholder;
        palette.groups = this.groups;
        palette.typeahead = this.typeahead;
        palette.addEventListener('palette-select', (e) =>
          this.handleSelect(e as CustomEvent<{ target: PaletteTarget }>)
        );
        palette.addEventListener('palette-retry', () => void this.load());
        palette.addEventListener('palette-dismiss', () => this.close());
        // Only the palette's own dialog: a Shoelace overlay nested in it
        // (a tooltip, a dropdown) fires the same composed events.
        palette.addEventListener('sl-hide', (e) => {
          if (isFromOwnDialog(palette, e)) this.hidingPalette = palette;
        });
        palette.addEventListener('sl-after-hide', (e) => {
          if (isFromOwnDialog(palette, e)) this.handleAfterHide(palette);
        });
        this.options.mount.append(palette);
        // A detached element never renders, so it could never open.
        if (!palette.isConnected) {
          palette.remove();
          throw new DOMException('Palette mount is not in the document', 'AbortError');
        }
        // Assigned before the first render settles, so a load that finishes
        // in the meantime still reaches the element.
        this.palette = palette;
        await palette.updateComplete;
        return palette;
      })
      .catch((err: unknown) => {
        this.paletteMount = null;
        throw err;
      });
    return this.paletteMount;
  }

  /**
   * Publishes `candidates` as the Agents group, ready, superseding any load
   * in flight: for a surface whose candidates change while the palette is
   * open. The next open loads afresh as usual.
   */
  setCandidates(candidates: PaletteCandidate[]): void {
    this.abort?.abort();
    this.abort = null;
    ++this.generation;
    this.setAgents({ status: 'ready', candidates });
  }

  /** Whether the mounted palette's dialog is running its close animation. */
  private isHiding(): boolean {
    const palette = this.hidingPalette;
    return palette !== null && palette === this.palette && palette.isConnected;
  }

  /**
   * Whether an open is still waiting to show: its element is mounting, or
   * it was made during the close animation.
   */
  private isOpenPending(): boolean {
    return this.paletteOpen && (this.pendingMount !== null || this.isHiding());
  }

  /** Ends the wait on the current open's mount. */
  private settleMount(): void {
    this.pendingMount = null;
    this.listenForEscape(false);
  }

  /**
   * Gives up an open that can no longer show. Focus never left the invoker,
   * so there is nothing to refocus.
   */
  private cancelPendingOpen(): void {
    this.paletteOpen = false;
    this.typeahead.stop();
    this.abort?.abort();
    this.invoker = null;
  }

  /** Fires once Shoelace's close animation completes, however the palette closed. */
  private handleAfterHide(palette: ScionQuickPalette): void {
    // A disposed host, or an element replaced after leaving the document, has nothing to settle.
    if (this.disposed || palette !== this.palette) return;
    if (this.paletteOpen) {
      // Opened again while closing. Shoelace queues its focus restore to the
      // dialog's trigger in a timeout just before this event; showing from a
      // later timeout keeps that restore from landing after the reopen.
      setTimeout(() => this.finishReopen(palette));
      return;
    }
    this.hidingPalette = null;
    if (this.closedBySelection) {
      // Shoelace queues its own focus restore to the dialog's trigger in a
      // timeout just before firing this event; acting in a later timeout
      // keeps that restore from overriding the surface's own focus move.
      this.settleTimer = setTimeout(() => this.options.onSelectionSettled?.());
    } else {
      this.invoker?.focus();
    }
    this.invoker = null;
  }

  /**
   * Shows a palette that was opened again during its close animation, unless
   * it was closed again, disposed, detached or overtaken by another modal
   * in the meantime.
   */
  private finishReopen(palette: ScionQuickPalette): void {
    this.listenForEscape(false);
    if (this.hidingPalette === palette) this.hidingPalette = null;
    // An element replaced after leaving the document has nothing to show. A
    // disposed host needs no check of its own: dispose() closed it and
    // cleared its invoker, so the branch below does nothing.
    if (palette !== this.palette) return;
    if (!this.paletteOpen) {
      // Closed again before it could show: settle like any dismiss.
      this.invoker?.focus();
      this.invoker = null;
      return;
    }
    if (!palette.isConnected || this.hasUnrelatedModalOpen()) {
      this.cancelPendingOpen();
      return;
    }
    palette.open = true;
  }

  private listenForEscape(listen: boolean): void {
    if (listen === this.listeningForEscape) return;
    this.listeningForEscape = listen;
    if (listen) document.addEventListener('keydown', this.handlePendingEscape, true);
    else document.removeEventListener('keydown', this.handlePendingEscape, true);
  }

  /**
   * Escape while an open is pending: the palette is open as far as the user
   * knows. A listener whose open can no longer show (a reopen's element left
   * the document) removes itself and lets the key through.
   */
  private readonly handlePendingEscape = (e: KeyboardEvent): void => {
    if (!this.isOpenPending()) {
      this.listenForEscape(false);
      return;
    }
    if (e.key !== 'Escape' || e.isComposing) return;
    e.preventDefault();
    e.stopPropagation();
    this.close();
  };

  private setAgents(state: GroupState): void {
    this.groups = { agents: state };
    if (this.palette) this.palette.groups = this.groups;
  }

  private async load(): Promise<void> {
    this.abort?.abort();
    const controller = new AbortController();
    this.abort = controller;
    const generation = ++this.generation;
    const isCurrent = (): boolean => generation === this.generation;
    this.setAgents({ status: 'loading', candidates: this.groups.agents?.candidates ?? [] });
    try {
      const candidates = await this.options.load({
        controller,
        isCurrent,
        onProgress: (partial) => {
          if (isCurrent()) this.setAgents({ status: 'loading', candidates: partial });
        },
      });
      if (!isCurrent() || controller.signal.aborted) return;
      this.setAgents({ status: 'ready', candidates });
    } catch (err) {
      if (!isCurrent()) return;
      if (err instanceof DOMException && err.name === 'AbortError') return;
      const message = err instanceof Error ? err.message : '';
      this.setAgents({
        status: 'error',
        candidates: this.groups.agents?.candidates ?? [],
        ...(message ? { error: message } : {}),
      });
    }
  }

  private handleSelect(e: CustomEvent<{ target: PaletteTarget }>): void {
    // A closing dialog still takes keys and clicks, but its rows belong to a
    // palette the user has dismissed, even if it is about to open again.
    if (!this.paletteOpen || this.isHiding()) return;
    const target = e.detail?.target;
    this.close();
    if (!target || target.kind !== 'agent') return;
    const stillPresent = (this.groups.agents?.candidates ?? []).some(
      (c) => c.target.kind === 'agent' && c.target.agentId === target.agentId
    );
    if (!stillPresent) return;
    this.closedBySelection = true;
    this.options.onSelect(target);
  }
}
