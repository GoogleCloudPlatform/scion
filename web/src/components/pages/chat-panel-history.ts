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
 * Browser history for the chat page's mobile panels.
 *
 * On a phone the chat page shows one panel at a time: the rail, the
 * conversation or the members list. Each history entry the page owns
 * records, in `history.state`, which panel it shows and which panels the
 * entries directly beneath it show on the same URL (its run). With that:
 *
 * - moving to a deeper panel (rail → conversation → members) pushes an
 *   entry on the same URL, so Back returns to the panel before it;
 * - moving to a shallower panel that an entry beneath shows goes back
 *   through history to it, so Back and the app's own back controls never
 *   pile up entries, and Forward redoes the move;
 * - a `popstate` only reads the entry, except to correct the entry a back
 *   move of this class's own lands on.
 *
 * Opening a conversation from the rail keeps its own URL push, and first
 * moves the rail entry to the new URL too, so Back from the conversation
 * slides to the rail in place rather than re-rendering the conversation
 * opened before it.
 *
 * Each run has an id that only grows, so (run, index in the run) orders the
 * entries along history: an entry ahead was always written later. On
 * desktop, where all three panels are on screen, nothing is written, and
 * that order (never a guess) decides what to do with entries left from the
 * mobile layout. A `popstate` that is the arrival of a traversal this class
 * started never starts another, so no user action leads to more than one
 * corrective traversal.
 */

import { IN_PAGE_STATE_KEY } from '../../client/route-history.js';

export type ChatPanel = 'left' | 'center' | 'right';

/**
 * What an entry records: its panel, the same-URL panels beneath it in its
 * run (oldest first; their count is the entry's index in the run), and the
 * run's id.
 */
export interface PanelEntry {
  panel: ChatPanel;
  below: ChatPanel[];
  run: number;
}

const DEPTH: Record<ChatPanel, number> = { left: 0, center: 1, right: 2 };

let lastRun = 0;

/** A run id greater than any handed out before, in this document or an earlier one. */
export function newRun(): number {
  lastRun = Math.max(lastRun + 1, Date.now() * 1000);
  return lastRun;
}

function isPanel(value: unknown): value is ChatPanel {
  return value === 'left' || value === 'center' || value === 'right';
}

/** The panel entry recorded in a history state, or null if there is none. */
export function readPanelEntry(state: unknown): PanelEntry | null {
  if (typeof state !== 'object' || state === null) return null;
  const marker = (state as Record<string, unknown>)[IN_PAGE_STATE_KEY];
  if (typeof marker !== 'object' || marker === null) return null;
  const { panel, below, run } = marker as { panel?: unknown; below?: unknown; run?: unknown };
  if (!isPanel(panel)) return null;
  return {
    panel,
    below: Array.isArray(below) ? below.filter(isPanel) : [],
    run: typeof run === 'number' && Number.isFinite(run) ? run : 0,
  };
}

/** `state` with this panel entry recorded, keeping its other keys. */
export function withPanelEntry(state: unknown, entry: PanelEntry): Record<string, unknown> {
  const base = typeof state === 'object' && state !== null ? state : {};
  return {
    ...base,
    [IN_PAGE_STATE_KEY]: { panel: entry.panel, below: [...entry.below], run: entry.run },
  };
}

/** Is entry `a` earlier in history than entry `b`? */
export function isBefore(a: PanelEntry, b: PanelEntry): boolean {
  return a.run !== b.run ? a.run < b.run : a.below.length < b.below.length;
}

/** What the caller does after a user move. */
export type PanelMove =
  /** Show the target panel now; history is already in step. */
  | 'set'
  /** History is going back to an entry that shows the target; show it on `popstate`. */
  | 'wait'
  /** A history traversal is already under way; drop this move. */
  | 'ignore';

/** How long to wait for a traversal's `popstate` before accepting moves again. */
export const TRAVERSAL_TIMEOUT_MS = 1000;

type HistoryLike = Pick<History, 'state' | 'pushState' | 'replaceState' | 'go'>;

export class ChatPanelHistory {
  private traversing = false;
  private traversalTimer: ReturnType<typeof setTimeout> | null = null;
  /** The panel a back move is going to, until its `popstate` arrives. */
  private traversalTarget: ChatPanel | null = null;
  /** The entry this page was last on, as of the last entry it wrote or saw. */
  private position: PanelEntry | null = null;

  constructor(private readonly history: HistoryLike = window.history) {}

  /** The current entry's panel record, if it has one. */
  current(): PanelEntry | null {
    return readPanelEntry(this.history.state);
  }

  /** Take the current entry as the starting point (the page has just been created on it). */
  observe(): void {
    this.position = this.current();
  }

  /**
   * The user moved from `from` to `target` on the same URL (a swipe, the
   * back control, the members button).
   */
  move(from: ChatPanel, target: ChatPanel): PanelMove {
    if (this.traversing) return 'ignore';
    let entry = this.current();
    if (entry && target === entry.panel) return 'set';
    if (!entry) {
      // An entry not recorded yet shows the panel being left: it starts a run.
      entry = { panel: from, below: [], run: newRun() };
      if (target === from) return 'set';
      if (DEPTH[target] > DEPTH[from]) {
        this.write('replace', entry);
      }
    }
    if (DEPTH[target] > DEPTH[entry.panel]) {
      this.write('push', { panel: target, below: [...entry.below, entry.panel], run: entry.run });
      return 'set';
    }
    const index = entry.below.lastIndexOf(target);
    if (index >= 0) {
      this.startTraversal(target);
      this.history.go(index - entry.below.length);
      return 'wait';
    }
    // Nothing beneath shows it (a deep link opened on the conversation):
    // this entry becomes that panel instead.
    this.write('replace', { ...entry, panel: target });
    return 'set';
  }

  /**
   * The history state for pushing a conversation URL the page has just
   * switched to, showing `panel`. When the entry being left shows the rail
   * and `browserUrl` is given, the rail entry moves to that URL first, as
   * the base of a new run, so it sits beneath the new entry. Without a URL
   * (one the page will rewrite later, such as a legacy thread link) the
   * rail entry stays where it is.
   */
  stateForPush(panel: ChatPanel, browserUrl: string | null): Record<string, unknown> {
    const entry = this.current();
    const run = newRun();
    if (browserUrl !== null && entry && entry.panel === 'left' && DEPTH[panel] > 0) {
      this.history.replaceState(
        withPanelEntry(this.history.state, { panel: 'left', below: [], run }),
        '',
        browserUrl
      );
      const pushed: PanelEntry = { panel, below: ['left'], run };
      this.position = pushed;
      return withPanelEntry({}, pushed);
    }
    const pushed: PanelEntry = { panel, below: [], run };
    this.position = pushed;
    return withPanelEntry({}, pushed);
  }

  /**
   * Record that the current entry shows `panel`, for panel changes the page
   * makes itself (opening the conversation a URL names, for instance).
   * Keeps what is beneath. Never adds an entry.
   */
  sync(panel: ChatPanel): void {
    if (this.traversing) return;
    const entry = this.current();
    if (entry && entry.panel === panel) {
      this.position = entry;
      return;
    }
    this.write('replace', entry ? { ...entry, panel } : { panel, below: [], run: newRun() });
  }

  /**
   * A `popstate` arrived on the URL the page shows. Returns the panel the
   * page should show, or null to leave it alone.
   *
   * The arrival of this class's own back move shows the move's target; an
   * entry that records another panel (a deep link's entry, replaced since
   * it was left) is corrected to it.
   *
   * In the wide layout every panel is on screen, so panel entries left from
   * the mobile layout would be dead Back steps. Landing on one that history
   * orders before the entry this page was on means Back: go on past the
   * first entry of its run, in one traversal. Anything else (Forward, an
   * entry without a record, the arrival of a traversal of this class's own)
   * is left alone: at worst a step with nothing to see, never a loop.
   */
  popped(state: unknown, wide: boolean): ChatPanel | null {
    const ours = this.traversing;
    const target = this.traversalTarget;
    this.endTraversal();
    const entry = readPanelEntry(state);
    const from = this.position;
    this.position = entry;
    if (!entry) return null;
    if (!wide) {
      if (ours && target && entry.panel !== target) {
        this.write('replace', { ...entry, panel: target });
        return target;
      }
      return entry.panel;
    }
    if (ours || !from || !isBefore(entry, from)) return null;
    this.startTraversal(null);
    this.history.go(-(entry.below.length + 1));
    return null;
  }

  /**
   * The page was created in the wide layout on a panel entry above the
   * first of its run (a reload, or Back from the next page). Go back to that
   * first entry, so the next Back leaves the URL.
   */
  toBase(): void {
    const entry = this.current();
    if (!entry || entry.below.length === 0 || this.traversing) return;
    this.startTraversal(null);
    this.history.go(-entry.below.length);
  }

  /** A `popstate` this page does not handle (another URL): end any traversal. */
  settle(): void {
    this.endTraversal();
  }

  /** Stop any pending traversal timer (the page is going away). */
  dispose(): void {
    this.endTraversal();
  }

  private write(kind: 'push' | 'replace', entry: PanelEntry): void {
    const state = withPanelEntry(this.history.state, entry);
    if (kind === 'push') this.history.pushState(state, '');
    else this.history.replaceState(state, '');
    this.position = entry;
  }

  private startTraversal(target: ChatPanel | null): void {
    this.traversing = true;
    this.traversalTarget = target;
    if (this.traversalTimer) clearTimeout(this.traversalTimer);
    this.traversalTimer = setTimeout(() => {
      this.traversalTimer = null;
      this.traversing = false;
      this.traversalTarget = null;
    }, TRAVERSAL_TIMEOUT_MS);
  }

  private endTraversal(): void {
    this.traversing = false;
    this.traversalTarget = null;
    if (this.traversalTimer) {
      clearTimeout(this.traversalTimer);
      this.traversalTimer = null;
    }
  }
}
