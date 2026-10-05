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
 * How long a capture lasts at most. A capture that no close or focus ever
 * ends (an open path that forgot to stop it) must not swallow keys for
 * good; the text typed so far is kept for a late {@link PaletteTypeahead.take}.
 */
export const PALETTE_TYPEAHEAD_MAX_MS = 5000;

/** Editing and navigation keys swallowed, so they act on neither the old focus nor the query. */
const SWALLOWED_KEYS = new Set([
  'Enter',
  'Tab',
  'Delete',
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
   * Starts capturing keys, unless already capturing. A fresh capture starts
   * with no text.
   */
  start(): void {
    if (this.capturing) return;
    this.text = '';
    this.capturing = true;
    window.addEventListener('keydown', this.handleKeydown, true);
    this.timer = setTimeout(() => this.release(), PALETTE_TYPEAHEAD_MAX_MS);
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
   * modifier chords (the palette's own shortcut, copy, reload…), lone
   * modifiers, function keys and IME input pass through. A character typed
   * with AltGr (reported as Ctrl+Alt on Windows) is text, and so is one
   * typed with Option on macOS (see {@link PaletteTypeaheadOptions.mac}). Any
   * key with Meta is a chord. Dead keys pass through, so an accented letter
   * composed from one reaches the old focus.
   */
  private readonly handleKeydown = (e: KeyboardEvent): void => {
    if (e.isComposing || e.metaKey) return;
    const printable = Array.from(e.key).length === 1;
    const altGraph = printable && e.getModifierState('AltGraph');
    const optionText = printable && this.mac && e.altKey && !e.ctrlKey;
    if (!altGraph && !optionText && (e.ctrlKey || e.altKey)) return;
    if (printable) {
      this.text += e.key;
    } else if (e.key === 'Backspace') {
      this.text = Array.from(this.text).slice(0, -1).join('');
    } else if (!SWALLOWED_KEYS.has(e.key)) {
      return;
    }
    e.preventDefault();
    e.stopImmediatePropagation();
  };
}
