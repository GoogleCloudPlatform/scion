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
 * On a touch-primary device, starting a capture also focuses a hidden text
 * field (see {@link PaletteTypeaheadOptions.holdsKeyboard}). iOS Safari
 * shows the on-screen keyboard only for a text field focused synchronously
 * inside the tap that asked for it, and the palette's query input can only
 * take focus later: after the module loads, the element renders and
 * Shoelace's dialog shows. Focus moved from one text field to another keeps
 * the keyboard up, so the query input inherits it from the hidden field
 * when it takes focus. Hosts therefore start capturing from the open
 * request itself, after reading the element to refocus on close.
 *
 * Kept in its own small module, with no Lit or Shoelace imports, so a host
 * can start capturing synchronously from its open request without pulling
 * the palette component into its bundle.
 */

import { isMacPlatform } from '../../../utils/platform.js';
import { TOUCH_PRIMARY_QUERY } from '../../../utils/input-modality.js';
import { deepActiveElement } from '../deep-active-element.js';

/**
 * How long a capture lasts at most after its latest
 * {@link PaletteTypeahead.start}. A capture that no close or focus ever
 * ends (an open path that forgot to stop it) must not swallow keys for
 * good; the text typed so far is kept for a late {@link PaletteTypeahead.take}.
 */
export const PALETTE_TYPEAHEAD_MAX_MS = 5000;

/**
 * How long the hidden text field holding the on-screen keyboard lasts at
 * most after the latest {@link PaletteTypeahead.start}. It outlives the
 * capture's own time limit, since a first open over a slow network can take
 * longer than that to show, and keys typed into it meanwhile still become
 * the query; this only bounds a field that no open ever settles or cancels.
 */
export const PALETTE_KEYBOARD_PROXY_MAX_MS = 30_000;

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
  /**
   * Whether a capture should hold the on-screen keyboard: focus a hidden
   * text field from {@link PaletteTypeahead.start} until the capture ends.
   * Read at each start. Defaults to whether the primary pointer is touch
   * ({@link TOUCH_PRIMARY_QUERY}); elsewhere focus stays where it was until
   * the query input takes it.
   */
  holdsKeyboard?: () => boolean;
}

function primaryPointerIsTouch(): boolean {
  return typeof window.matchMedia === 'function' && window.matchMedia(TOUCH_PRIMARY_QUERY).matches;
}

/**
 * A text field that holds focus, and with it the on-screen keyboard, until
 * the palette's query input can take it. Fixed at the top left, transparent
 * and 16px, so focusing it neither scrolls nor zooms the page and nothing
 * shows; it takes no pointer events and is out of the tab order.
 */
function createKeyboardProxy(): HTMLInputElement {
  const input = document.createElement('input');
  input.type = 'text';
  input.tabIndex = -1;
  input.autocomplete = 'off';
  input.setAttribute('autocapitalize', 'off');
  input.setAttribute('autocorrect', 'off');
  input.spellcheck = false;
  input.setAttribute('aria-label', 'Search');
  input.dataset.paletteKeyboardProxy = '';
  Object.assign(input.style, {
    position: 'fixed',
    top: '0',
    left: '0',
    width: '1px',
    height: '1px',
    margin: '0',
    padding: '0',
    border: '0',
    opacity: '0',
    fontSize: '16px',
    pointerEvents: 'none',
  });
  return input;
}

export class PaletteTypeahead {
  private readonly mac: boolean;
  private readonly holdsKeyboard: () => boolean;
  private text = '';
  private capturing = false;
  private timer: ReturnType<typeof setTimeout> | undefined;
  /** Drops {@link proxy} at {@link PALETTE_KEYBOARD_PROXY_MAX_MS}. */
  private proxyTimer: ReturnType<typeof setTimeout> | undefined;
  /** The hidden text field holding the on-screen keyboard, while a capture holds it. */
  private proxy: HTMLInputElement | null = null;
  /** What had focus when {@link proxy} took it, given focus back if the proxy still has it at the end. */
  private proxyReturnFocus: HTMLElement | null = null;

  constructor(options: PaletteTypeaheadOptions = {}) {
    this.mac = options.mac ?? isMacPlatform();
    this.holdsKeyboard = options.holdsKeyboard ?? primaryPointerIsTouch;
  }

  /** The hidden text field holding the on-screen keyboard, if a capture holds it. */
  get keyboardProxy(): HTMLInputElement | null {
    return this.proxy;
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
   *
   * Where {@link PaletteTypeaheadOptions.holdsKeyboard} holds, also moves
   * focus to a hidden text field, synchronously, so a start from a tap
   * shows the on-screen keyboard. A host reads the element to refocus on
   * close before it starts. The field outlives the capture's time limit,
   * up to {@link PALETTE_KEYBOARD_PROXY_MAX_MS}: it lasts until
   * {@link take} or {@link stop}, so a slow open still shows with the
   * keyboard up, and keys typed into it after the limit still become the
   * query.
   */
  start(): void {
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.release(), PALETTE_TYPEAHEAD_MAX_MS);
    if (!this.capturing) {
      this.text = '';
      if (this.proxy) this.proxy.value = '';
      this.capturing = true;
      window.addEventListener('keydown', this.handleKeydown, true);
    }
    if (!this.proxy && this.holdsKeyboard()) this.holdKeyboard();
    if (this.proxy) {
      clearTimeout(this.proxyTimer);
      this.proxyTimer = setTimeout(() => this.dropKeyboardProxy(), PALETTE_KEYBOARD_PROXY_MAX_MS);
    }
  }

  /**
   * Stops capturing and returns the captured text, clearing it. Drops the
   * hidden text field: text the field took itself (keys the capture let
   * through, such as IME input, or typed after its time limit) is kept
   * after the text captured so far.
   */
  take(): string {
    this.release();
    this.dropKeyboardProxy();
    const text = this.text;
    this.text = '';
    return text;
  }

  /**
   * Stops capturing and discards the captured text. A hidden text field
   * that still has focus gives it back to what had it before, as the query
   * input never took it, unless `restoreFocus` is false: for a host whose
   * surface is going off screen, and focus with it.
   */
  stop(options: { restoreFocus?: boolean } = {}): void {
    this.release();
    this.dropKeyboardProxy(options.restoreFocus ?? true);
    this.text = '';
  }

  /** Stops capturing keys, keeping the text captured so far and the hidden text field. */
  private release(): void {
    clearTimeout(this.timer);
    this.timer = undefined;
    if (!this.capturing) return;
    this.capturing = false;
    window.removeEventListener('keydown', this.handleKeydown, true);
  }

  private holdKeyboard(): void {
    if (!document.body) return;
    const active = deepActiveElement();
    const proxy = createKeyboardProxy();
    proxy.addEventListener('input', this.handleProxyInput);
    proxy.addEventListener('compositionend', this.flushProxy);
    document.body.append(proxy);
    this.proxy = proxy;
    this.proxyReturnFocus = active instanceof HTMLElement ? active : null;
    proxy.focus({ preventScroll: true });
  }

  /**
   * Text the hidden field took itself (IME composition, dictation, a
   * predictive suggestion, or keys after the capture's time limit) joins the
   * captured text as soon as it is committed, so it keeps its place among
   * keys captured before and after it. Text still being composed stays in
   * the field until its composition ends.
   */
  private readonly handleProxyInput = (e: Event): void => {
    if ((e as InputEvent).isComposing) return;
    this.flushProxy();
  };

  private readonly flushProxy = (): void => {
    const proxy = this.proxy;
    if (!proxy || !proxy.value) return;
    this.text += proxy.value;
    proxy.value = '';
  };

  /**
   * Removes the hidden text field, keeping its text after the text captured
   * so far. A field that still has focus gives it back to what had it
   * before, unless `restoreFocus` is false.
   */
  private dropKeyboardProxy(restoreFocus = true): void {
    clearTimeout(this.proxyTimer);
    this.proxyTimer = undefined;
    const proxy = this.proxy;
    if (!proxy) return;
    const returnFocus = this.proxyReturnFocus;
    this.proxy = null;
    this.proxyReturnFocus = null;
    this.text += proxy.value;
    const focused = document.activeElement === proxy;
    proxy.remove();
    if (restoreFocus && focused && returnFocus?.isConnected)
      returnFocus.focus({ preventScroll: true });
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
