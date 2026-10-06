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
 * Type-ahead for a quick palette that is opening but whose query input does
 * not have focus yet.
 *
 * Between an open request and the moment the query input can take focus,
 * the palette's module may still be loading (it is imported on first open),
 * its element may still be rendering, and Shoelace's dialog moves focus only
 * a frame after it shows. Keys typed in that window would otherwise land
 * wherever focus was before the open: the chat composer, a terminal pane,
 * or the button that opened the palette. While capturing, this swallows
 * those keys before any other listener sees them and keeps the text they
 * would have typed, for the palette to apply as its query once its input
 * has focus.
 *
 * Kept in its own small module, with no Lit or Shoelace imports, so a host
 * can start capturing synchronously from its open request without pulling
 * the palette component into its bundle.
 */

import { isMacPlatform } from '../../../utils/platform.js';

/**
 * How long a capture lasts at most after its latest
 * {@link PaletteTypeahead.start}. A capture that no close or focus ever
 * ends (an open path that forgot to stop it) must not swallow keys for
 * good; the text typed so far is kept for a late {@link PaletteTypeahead.take}.
 */
export const PALETTE_TYPEAHEAD_MAX_MS = 5000;

/** Editing and navigation keys swallowed, so they act on neither the old focus nor the query. */
const SWALLOWED_KEYS = new Set([
  'Enter',
  'Tab',
  'ArrowUp',
  'ArrowDown',
  'ArrowLeft',
  'ArrowRight',
  'Home',
  'End',
  'PageUp',
  'PageDown',
]);

export interface PaletteTypeaheadOptions {
  /**
   * Whether keys follow macOS conventions: a character typed with Option
   * (Alt) and neither Ctrl nor Meta is text, since Option types characters
   * such as `@`, `[` or `€` on many layouts. Elsewhere Alt+key is a
   * shortcut. Defaults to {@link isMacPlatform}.
   */
  mac?: boolean;
}

export class PaletteTypeahead {
  private readonly mac: boolean;
  private text = '';
  private capturing = false;
  private timer: ReturnType<typeof setTimeout> | undefined;

  constructor(options: PaletteTypeaheadOptions = {}) {
    this.mac = options.mac ?? isMacPlatform();
  }

  /** Whether keys are being captured. */
  get isCapturing(): boolean {
    return this.capturing;
  }

  /** The text captured so far. */
  get pending(): string {
    return this.text;
  }

  /**
   * Starts capturing keys, and restarts the time limit. A fresh capture
   * starts with no text; a start while capturing keeps the text captured so
   * far, so an open that runs a queued open request carries its keys.
   */
  start(): void {
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.release(), PALETTE_TYPEAHEAD_MAX_MS);
    if (this.capturing) return;
    this.text = '';
    this.capturing = true;
    window.addEventListener('keydown', this.handleKeydown, true);
  }

  /** Stops capturing and returns the captured text, clearing it. */
  take(): string {
    this.release();
    const text = this.text;
    this.text = '';
    return text;
  }

  /** Stops capturing and discards the captured text. */
  stop(): void {
    this.take();
  }

  private release(): void {
    clearTimeout(this.timer);
    this.timer = undefined;
    if (!this.capturing) return;
    this.capturing = false;
    window.removeEventListener('keydown', this.handleKeydown, true);
  }

  /**
   * Window capture phase, so it runs before every document and element
   * listener, and before window capture listeners added after it. Escape,
   * modifier chords other than Backspace and Delete (the palette's own
   * shortcut, copy, reload…), lone modifiers, function keys and IME input,
   * Backspace included, pass through. A character typed with AltGr
   * (reported as Ctrl+Alt on Windows) is text, and so is one typed with
   * Option on macOS (see {@link PaletteTypeaheadOptions.mac}). Any other key
   * with Meta is a chord. Dead keys pass through, so an accented letter
   * composed from one reaches the old focus.
   *
   * Backspace and Delete are swallowed with any modifier: in the gap they
   * correct what was just typed, and must not delete text in the old focus.
   * Backspace edits the captured text close to how the same chord edits a
   * text field on the platform (see {@link deleteBackward}); Delete has no
   * text after the caret to remove.
   */
  private readonly handleKeydown = (e: KeyboardEvent): void => {
    if (e.isComposing) return;
    if (e.key === 'Backspace' || e.key === 'Delete') {
      if (e.key === 'Backspace') this.text = deleteBackward(this.text, e, this.mac);
      e.preventDefault();
      e.stopImmediatePropagation();
      return;
    }
    if (e.metaKey) return;
    const printable = Array.from(e.key).length === 1;
    const altGraph = printable && e.getModifierState('AltGraph');
    const optionText = printable && this.mac && e.altKey && !e.ctrlKey;
    if (!altGraph && !optionText && (e.ctrlKey || e.altKey)) return;
    if (printable) {
      this.text += e.key;
    } else if (!SWALLOWED_KEYS.has(e.key)) {
      return;
    }
    e.preventDefault();
    e.stopImmediatePropagation();
  };
}

/**
 * The last word of `text` and the spaces after it, close to a browser's
 * word delete: a run of letters, digits and underscores, or a run of other
 * non-space characters. Like a browser, it stops at punctuation runs, so
 * `foo-bar` loses `bar`, `co@` loses `@` and `a!!` loses `!!`. Unlike
 * Chromium, letters or digits joined by `'`, `.` or `,` count as separate
 * words, so `v1.2` loses only `2`.
 */
const LAST_WORD = /(?:[\p{L}\p{N}_]+|[^\p{L}\p{N}_\s]+)?\s*$/u;

/**
 * The captured text after a Backspace, close to the platform's text field
 * conventions. On macOS, Cmd deletes all of it (to the line start), Option
 * deletes the last word, and Ctrl, like a plain or Shift Backspace, deletes
 * one character. Elsewhere Ctrl deletes the last word, with or without
 * Shift (Chromium on Linux deletes to the line start for Ctrl+Shift; the
 * capture lasts only until the input has focus, so one word delete rule is
 * kept), and Alt or Meta (with or without Ctrl) leave it unchanged.
 */
function deleteBackward(text: string, e: KeyboardEvent, mac: boolean): string {
  if (mac) {
    if (e.metaKey) return '';
    if (e.altKey) return text.replace(LAST_WORD, '');
  } else {
    if (e.altKey || e.metaKey) return text;
    if (e.ctrlKey) return text.replace(LAST_WORD, '');
  }
  return Array.from(text).slice(0, -1).join('');
}
