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
 * Measures how tall the composer's text field may grow without pushing the
 * composer out of the visible frame, and hands that to the composer as
 * `--composer-field-room` (whole CSS px) on its element.
 *
 * Everything is measured as the layout would be uncompacted, so the result
 * is a continuous function of the frame height:
 * - the column is the visible frame below the top of the message list
 *   (below the conversation header and any bar or banner above the list),
 *   plus the app's top bar while the chat shell has hidden it in a short
 *   frame (`--scion-chat-top-bar-hidden` × `--scion-chat-top-bar-h`);
 * - the chrome is everything between the message list and the bottom of
 *   the composer apart from the field (a send error, the destination chip,
 *   a reply or edit bar, attachments, the composer's padding and footer),
 *   plus what a tight frame takes out of flow (`tightSavings()`) and what
 *   a too-short frame clips off (`clippedHeight()`), so it is the
 *   composer's natural chrome even when it does not fit.
 * The field may then take the smaller of:
 * - the room: the column less one message row (so some of the
 *   conversation stays in view) and the chrome, with part of what the
 *   hidden chrome frees credited back (below);
 * - the curve: the composer as a whole may take `COMPOSER_CURVE_SLOPE` ×
 *   column + `COMPOSER_CURVE_BASE_REM`, and the field gets what the chrome
 *   leaves of it;
 * but never less than two lines where the room with a shorter list reserve
 * (`MIN_ROW_RESERVE_REM`) allows them.
 * The credit phases in over `CREDIT_BAND_PX` below each threshold: none at
 * the threshold itself, all of it (up to the band's width) once the frame
 * is a band below. So the room stays continuous at the threshold, and
 * since the credit grows by at most 1px per px the frame shrinks, the room
 * never falls as the frame grows. The bands abut (the short one ends where
 * the tight one starts) so their slopes never add up.
 *
 * Both terms grow with the frame and neither jumps when the frame crosses
 * a size threshold (client/viewport.ts), so a taller frame never gets a
 * shorter field, whatever sits above the list or around the field. And
 * since the real column is at least as tall and the real chrome at most as
 * tall as what the room counts, the field always fits.
 *
 * The field is excluded from the measurement, so the value does not depend
 * on it: when the field grows by d, the message list shrinks by d and the
 * room stays the same. Writing the variable can resize the field and re-fire
 * the observer, but the next read gives the same value, and a write only
 * happens when the room moves by a whole px or more (so sub-px layout drift
 * never writes). Reads are throttled to one per animation frame.
 *
 * Only on a phone or tablet layout (the same media query as the composer's
 * cap); elsewhere the variable is removed and the field is not capped by it.
 */

import type { ReactiveController, ReactiveControllerHost } from 'lit';
import { SHORT_FRAME_MAX_PX, TIGHT_FRAME_MAX_PX } from '../../../client/viewport.js';

/** The layouts the composer caps its field on; matches its CSS. */
export const COMPOSER_CAP_MEDIA = '(max-width: 768px), (pointer: coarse)';

/** Room kept for the message list, in rem: about one message row. */
export const MESSAGE_ROW_RESERVE_REM = 4;

/**
 * The least the message list keeps, in rem, when the field takes its
 * two-line floor: about one short message row.
 */
export const MIN_ROW_RESERVE_REM = 2.5;

/** The composer's curve grows by this many px per px of column height... */
export const COMPOSER_CURVE_SLOPE = 0.15;

/**
 * ...from this base, in rem. With the usual chrome (chip, padding, footer:
 * about 4.6rem) and the usual headers above the list, that leaves the field
 * about 4 lines in a 380px frame and about 8 in a 1024px one.
 */
export const COMPOSER_CURVE_BASE_REM = 8.3;

/**
 * Width, in CSS px, of the band below each frame-size threshold over which
 * what the hidden chrome frees is credited to the field's room.
 */
export const CREDIT_BAND_PX = SHORT_FRAME_MAX_PX - TIGHT_FRAME_MAX_PX;

export const FIELD_ROOM_PROPERTY = '--composer-field-room';

/** 0 at `threshold`, rising to 1 a band below it. */
function credit(frame: number, threshold: number): number {
  return Math.min(1, Math.max(0, (threshold - frame) / CREDIT_BAND_PX));
}

/** What the controller needs from the composer element. */
export interface RoomComposer extends HTMLElement {
  /** The text field's current border-box height in CSS px, or null before it renders. */
  fieldHeight(): number | null;
  /** The height in CSS px the tight-frame compaction frees, or 0. */
  tightSavings(): number;
  /** The field's height in CSS px for `lines` lines of draft. */
  fieldLinesHeight(lines: number): number;
  /** How much of the composer a too-short frame cuts off, in CSS px, or 0. */
  clippedHeight(): number;
}

/** The parts of the thread the measurement reads. */
export interface RoomParts {
  messages: HTMLElement | null;
  composer: RoomComposer | null;
}

/**
 * The visible frame height: the keyboard-shrunk height `client/viewport.ts`
 * writes inline on the root, or the window height when it writes none.
 */
function frameHeight(win: Window): number {
  const inline = parseFloat(
    win.document.documentElement.style.getPropertyValue('--scion-app-height')
  );
  return Number.isFinite(inline) ? inline : win.innerHeight;
}

export class ComposerRoomController implements ReactiveController {
  private readonly host: ReactiveControllerHost & HTMLElement;
  private readonly getParts: () => RoomParts;
  private readonly win: Window;
  private observer: ResizeObserver | null = null;
  private observed = new Set<Element>();
  private frame: number | null = null;
  private written: { el: HTMLElement; value: string } | null = null;
  /** The unrounded room behind the last write, to ignore sub-px drift. */
  private writtenRaw = Number.NaN;
  private media: MediaQueryList | null = null;
  /** How many times the variable was written or removed; for tests. */
  writes = 0;

  constructor(
    host: ReactiveControllerHost & HTMLElement,
    getParts: () => RoomParts,
    win: Window = window
  ) {
    this.host = host;
    this.getParts = getParts;
    this.win = win;
    host.addController(this);
  }

  hostConnected(): void {
    this.win.addEventListener('resize', this.schedule);
    this.win.visualViewport?.addEventListener('resize', this.schedule);
    this.host.addEventListener('focusin', this.schedule);
    this.media = this.win.matchMedia?.(COMPOSER_CAP_MEDIA) ?? null;
    this.media?.addEventListener('change', this.handleMediaChange);
    // A host moved or reconnected without a re-render gets its observer back.
    this.syncObserved();
    this.schedule();
  }

  hostUpdated(): void {
    this.syncObserved();
    this.schedule();
  }

  /** Whether the layout is one the composer caps its field on (phone or tablet). */
  get capped(): boolean {
    return this.media?.matches ?? false;
  }

  private readonly handleMediaChange = (): void => {
    this.syncObserved();
    this.schedule();
    // The host may render differently on the capped layout.
    this.host.requestUpdate();
  };

  /**
   * Observe the current message list and composer (they can be re-rendered),
   * or nothing at all outside the capped layout, so the controller costs
   * nothing on desktop.
   */
  private syncObserved(): void {
    const { messages, composer } = this.getParts();
    const current = new Set<Element>(
      this.capped ? [messages, composer].filter((el): el is HTMLElement => !!el) : []
    );
    if (current.size > 0 && !this.observer && typeof ResizeObserver !== 'undefined') {
      this.observer = new ResizeObserver(this.schedule);
    }
    for (const el of this.observed) {
      if (!current.has(el)) this.observer?.unobserve(el);
    }
    for (const el of current) {
      if (!this.observed.has(el)) this.observer?.observe(el);
    }
    this.observed = current;
  }

  hostDisconnected(): void {
    this.win.removeEventListener('resize', this.schedule);
    this.win.visualViewport?.removeEventListener('resize', this.schedule);
    this.host.removeEventListener('focusin', this.schedule);
    this.media?.removeEventListener('change', this.handleMediaChange);
    this.media = null;
    this.observer?.disconnect();
    this.observer = null;
    this.observed.clear();
    if (this.frame !== null) {
      this.win.cancelAnimationFrame(this.frame);
      this.frame = null;
    }
  }

  private readonly schedule = (): void => {
    // Outside the capped layout there is nothing to do but clear a write.
    if (!this.capped) {
      this.writtenRaw = Number.NaN;
      this.write(null);
      return;
    }
    if (this.frame !== null) return;
    this.frame = this.win.requestAnimationFrame(() => {
      this.frame = null;
      this.measure();
    });
  };

  /** Read the layout and write the room, if it changed. Public for tests. */
  measure(): void {
    const { messages, composer } = this.getParts();
    const field = composer?.fieldHeight() ?? null;
    if (!this.capped || !messages || !composer || field === null) {
      this.writtenRaw = Number.NaN;
      this.write(null);
      return;
    }
    const list = messages.getBoundingClientRect();
    const box = composer.getBoundingClientRect();
    const rootFont = parseFloat(
      this.win.getComputedStyle(this.win.document.documentElement).fontSize
    );
    const rem = Number.isFinite(rootFont) ? rootFont : 16;
    const style = this.win.getComputedStyle(composer);
    const num = (name: string): number => parseFloat(style.getPropertyValue(name)) || 0;
    const hiddenTopBar = num('--scion-chat-top-bar-hidden') * num('--scion-chat-top-bar-h');
    const savings = composer.tightSavings();
    const frame = frameHeight(this.win);
    // The column below the top of the list, counting a hidden top bar.
    const column = frame - list.top - hiddenTopBar;
    // Everything from the bottom of the message list to the bottom of the
    // composer except the field, counting what a tight frame takes out of
    // flow.
    const chrome = box.bottom - list.bottom - field + savings + composer.clippedHeight();
    // What the hidden chrome frees, phased in below each threshold.
    const credited =
      Math.min(hiddenTopBar, CREDIT_BAND_PX) * credit(frame, SHORT_FRAME_MAX_PX) +
      Math.min(savings, CREDIT_BAND_PX) * credit(frame, TIGHT_FRAME_MAX_PX);
    const room = column - MESSAGE_ROW_RESERVE_REM * rem - chrome + credited;
    const curve = COMPOSER_CURVE_SLOPE * column + COMPOSER_CURVE_BASE_REM * rem - chrome;
    // A multi-line draft gets two lines where the room allows it with a
    // shorter (2.5rem) reserve for the list, even where the curve is lower:
    // a reply bar or an error then comes out of the list's spare room, not
    // only the field. Both terms grow with the frame, so this stays
    // monotonic, and it never exceeds that room, so it still fits.
    const twoLines = Math.min(
      composer.fieldLinesHeight(2),
      room + (MESSAGE_ROW_RESERVE_REM - MIN_ROW_RESERVE_REM) * rem
    );
    const raw = Math.max(0, Math.min(room, curve), twoLines);
    // Sub-pixel layout drift could flip a rounded value back and forth.
    if (this.written?.el === composer && Math.abs(raw - this.writtenRaw) < 1) return;
    this.writtenRaw = raw;
    this.write({ el: composer, value: `${Math.floor(raw)}px` });
  }

  private write(next: { el: HTMLElement; value: string } | null): void {
    const prev = this.written;
    if (prev && next && prev.el === next.el && prev.value === next.value) return;
    if (!prev && !next) return;
    if (prev && prev.el !== next?.el) prev.el.style.removeProperty(FIELD_ROOM_PROPERTY);
    if (next) next.el.style.setProperty(FIELD_ROOM_PROPERTY, next.value);
    this.written = next;
    this.writes++;
  }
}
