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
 * `<scion-secret-value-field>`: semantic masked secret input shared by the
 * chat secret-request card (`<scion-chat-secret-request>`) and the create-agent
 * Secrets tab (`<scion-agent-config-form>`).
 *
 * Design invariants:
 * - Masked mode always renders a native `<input type="password" autocomplete="off">`
 *   for every secret type so browsers hide the value visually and from the
 *   accessibility tree without blur-triggered layout shifts.
 * - Multi-line text pasted, dropped, inserted, or loaded from a file is captured
 *   verbatim in memory (without altering CR/LF or UTF-8 BOM) while keeping the
 *   password input empty and showing a fixed-height "Value entered (hidden)"
 *   status indicator.
 * - Clicking Show switches to a `<textarea>` and moves focus to it. All-CRLF
 *   values stay editable while preserving CRLF; mixed line endings or bare CR
 *   render readonly with an explanatory note so bytes are never silently altered.
 * - Undo/redo shortcuts and history beforeinput events are blocked to prevent
 *   browser history from desynchronizing the DOM from the raw in-memory value.
 * - Secret values are never stored in `@state()`, reflected attributes,
 *   localStorage, sessionStorage, or logs, and are cleared on `clear()`,
 *   `pagehide`, and `disconnectedCallback()`.
 */

import { LitElement, css, html, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

/** Default maximum secret value byte length (64 KiB). */
export const MAX_SECRET_VALUE_BYTES = 64 * 1024;

function formatMaxBytes(bytes: number): string {
  if (bytes >= 1024 && bytes % 1024 === 0) {
    return `${bytes / 1024} KiB`;
  }
  return `${bytes} bytes`;
}

export type LineEndingStyle = 'lf' | 'crlf' | 'mixed';

export interface SecretValueChangeDetail {
  value: string;
}

export interface SecretValueErrorDetail {
  message: string;
}

export function normalizeToLf(value: string): string {
  return value.replace(/\r\n/g, '\n').replace(/\r/g, '\n');
}

function isUndoRedoShortcut(e: KeyboardEvent): boolean {
  if (!(e.ctrlKey || e.metaKey) || e.altKey) return false;
  const key = e.key.toLowerCase();
  return key === 'z' || key === 'y';
}

export function detectLineEndingStyle(value: string): LineEndingStyle {
  if (!value.includes('\r')) {
    return 'lf';
  }
  const withoutCrlf = value.replace(/\r\n/g, '');
  if (withoutCrlf.includes('\r') || withoutCrlf.includes('\n')) {
    return 'mixed';
  }
  return 'crlf';
}

/**
 * Maps a character offset in an LF-normalized textarea display string back to
 * the corresponding offset in the raw stored value (where `\r\n` takes 2 code
 * units in raw and 1 in display).
 */
export function displayOffsetToRawOffset(raw: string, displayOffset: number): number {
  let rawIdx = 0;
  let dispIdx = 0;
  while (rawIdx < raw.length && dispIdx < displayOffset) {
    if (raw[rawIdx] === '\r' && raw[rawIdx + 1] === '\n') {
      rawIdx += 2;
    } else {
      rawIdx += 1;
    }
    dispIdx += 1;
  }
  return rawIdx;
}

@customElement('scion-secret-value-field')
export class ScionSecretValueField extends LitElement {
  /** Secret key name used for labels, data-* hooks, and error messages. */
  @property({ type: String })
  secretKey = '';

  /** Optional row identifier forwarded as `data-row-id` on inner controls. */
  @property({ type: String })
  rowId = '';

  /** Optional visible label rendered above the input row. */
  @property({ type: String })
  label = '';

  /** Optional placeholder override when no value is captured. */
  @property({ type: String })
  placeholder = '';

  /** Enables the "Load from file" picker (used for `file` secret rows). */
  @property({ type: Boolean })
  allowFileLoad = false;

  /** Optional explicit textarea row count when revealed (defaults to 4 for file, 2 otherwise). */
  @property({ type: Number })
  textareaRows = 0;

  /** Disables the input, Show/Hide toggle, and file picker. */
  @property({ type: Boolean })
  disabled = false;

  /** Maximum allowed UTF-8 byte size for file loading (default 64 KiB). */
  @property({ type: Number })
  maxBytes = MAX_SECRET_VALUE_BYTES;

  /** Monotonic counter; incrementing it clears the field's in-memory value and cancels file reads. */
  @property({ type: Number })
  clearToken = 0;

  /** When true, renders file-load validation errors inline below the controls. */
  @property({ type: Boolean })
  showError = false;

  /** Whether the revealed `<textarea>` is currently shown. */
  @state()
  private revealed = false;

  /** Reactive counter bumped on value changes without storing the secret value in `@state()`. */
  @state()
  private revision = 0;

  /** Inline file-load validation error (displayed when `showError` is true). */
  @state()
  private errorMessage = '';

  /** Ephemeral in-memory secret value. Never persisted, reflected, or logged. */
  private _value = '';

  /**
   * True when the current value was captured via verbatim paste/drop/file load
   * (or contains CR/LF) so the masked password input stays empty while the full
   * verbatim value is held in `_value`.
   */
  private captured = false;

  /** Line-ending style snapshotted when revealed, to avoid HTML textarea LF normalization. */
  private revealedLineEnding: LineEndingStyle = 'lf';

  /** Monotonic counter bumped on every edit, clear, or file pick to cancel stale async file reads. */
  private clearGeneration = 0;

  /** True when Show was just clicked and the revealed textarea should receive focus. */
  private pendingFocusTextarea = false;

  /** Clears ephemeral value on pagehide so bfcache navigation cannot restore secrets. */
  private readonly onPageHide = (): void => {
    const hadValue = this._value.length > 0;
    this.clear();
    if (hadValue) {
      this.emitValueChange('');
    }
  };

  static override styles = css`
    :host {
      display: block;
    }

    .field-label {
      display: block;
      margin-bottom: 0.25rem;
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-text);
    }

    .input-wrap {
      display: flex;
      align-items: flex-start;
      gap: 0.4rem;
    }

    .secret-input {
      flex: 1 1 auto;
      min-width: 0;
      height: 2.25rem;
      box-sizing: border-box;
      padding: 0.4rem 0.6rem;
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      background: var(--scion-surface);
      color: var(--scion-text);
      font-family: var(--scion-font-mono);
      font-size: 0.875rem;
      line-height: 1.4;
    }

    .secret-input::-ms-reveal,
    .secret-input::-ms-clear {
      display: none;
    }

    .secret-input:focus {
      outline: none;
      border-color: var(--scion-border-active);
    }

    .secret-input:focus-visible {
      outline: 2px solid var(--scion-primary);
      outline-offset: 1px;
    }

    .secret-input:disabled {
      opacity: 0.6;
      cursor: not-allowed;
      background: var(--scion-bg-subtle);
    }

    .secret-textarea {
      flex: 1 1 auto;
      min-width: 0;
      min-height: calc(var(--secret-rows, 2) * 1.4em + 0.9rem + 2px);
      max-height: 12rem;
      resize: vertical;
      box-sizing: border-box;
      padding: 0.45rem 0.6rem;
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      background: var(--scion-surface);
      color: var(--scion-text);
      font-family: var(--scion-font-mono);
      font-size: 0.875rem;
      line-height: 1.4;
    }

    .secret-textarea:focus {
      outline: none;
      border-color: var(--scion-border-active);
    }

    .secret-textarea:focus-visible {
      outline: 2px solid var(--scion-primary);
      outline-offset: 1px;
    }

    .secret-textarea:disabled {
      opacity: 0.6;
      cursor: not-allowed;
      background: var(--scion-bg-subtle);
    }

    .secret-textarea[readonly] {
      background: var(--scion-bg-subtle);
    }

    .mixed-line-endings-note {
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-muted);
    }

    .file-controls {
      display: flex;
      align-items: center;
      flex-wrap: wrap;
      gap: 0.5rem;
      min-height: 1.875rem;
      margin-top: 0.4rem;
    }

    .file-picker-input {
      position: absolute;
      width: 1px;
      height: 1px;
      padding: 0;
      margin: -1px;
      overflow: hidden;
      clip: rect(0, 0, 0, 0);
      white-space: nowrap;
      border: 0;
    }

    .file-value-status {
      display: inline-flex;
      align-items: center;
      gap: 0.35rem;
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-muted);
    }

    .file-value-status sl-icon {
      flex-shrink: 0;
      font-size: 0.875rem;
    }

    .field-error {
      margin-top: 0.25rem;
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-error-text);
    }

    @media (pointer: coarse) {
      .secret-input,
      .secret-textarea {
        font-size: 1rem;
      }
    }

    .toggle-visibility {
      flex: 0 0 auto;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 2.25rem;
      height: 2.25rem;
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      background: var(--scion-bg);
      color: var(--scion-text-secondary);
      cursor: pointer;
    }

    .toggle-visibility:hover:not(:disabled) {
      border-color: var(--scion-border-hover);
      color: var(--scion-text);
    }

    .toggle-visibility:focus-visible {
      outline: 2px solid var(--scion-primary);
      outline-offset: 1px;
    }

    .toggle-visibility:disabled {
      opacity: 0.5;
      cursor: not-allowed;
    }
  `;

  /** Current in-memory secret value. */
  get value(): string {
    return this._value;
  }

  override connectedCallback(): void {
    super.connectedCallback();
    if (typeof window !== 'undefined') {
      window.addEventListener('pagehide', this.onPageHide);
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (typeof window !== 'undefined') {
      window.removeEventListener('pagehide', this.onPageHide);
    }
    // Defer clearing by one microtask so keyed DOM moves (such as Lit repeat()
    // reordering via insertBefore, which invokes disconnectedCallback followed
    // synchronously by connectedCallback) preserve an active field's value.
    queueMicrotask(() => {
      if (!this.isConnected) {
        this.clear();
      }
    });
  }

  protected override willUpdate(changed: PropertyValues): void {
    if (changed.has('clearToken') && changed.get('clearToken') !== undefined) {
      this.clear();
    }
  }

  protected override updated(): void {
    if (this.pendingFocusTextarea) {
      this.pendingFocusTextarea = false;
      const ta = this.shadowRoot?.querySelector<HTMLTextAreaElement>('textarea.secret-textarea');
      ta?.focus();
    }
  }

  /** Clears the in-memory secret value, resets reveal state, and cancels in-flight file reads. */
  clear(): void {
    this.clearGeneration++;
    this.pendingFocusTextarea = false;
    this._value = '';
    this.captured = false;
    this.revealedLineEnding = 'lf';
    this.revealed = false;
    this.errorMessage = '';
    const input = this.shadowRoot?.querySelector<HTMLInputElement>('input.secret-input');
    if (input) {
      input.value = '';
    }
    const ta = this.shadowRoot?.querySelector<HTMLTextAreaElement>('textarea.secret-textarea');
    if (ta) {
      ta.value = '';
    }
    this.revision++;
  }

  private get isCaptured(): boolean {
    return (
      this._value.length > 0 &&
      (this.captured || this._value.includes('\n') || this._value.includes('\r'))
    );
  }

  private emitValueChange(value: string): void {
    this.dispatchEvent(
      new CustomEvent<SecretValueChangeDetail>('secret-value-change', {
        detail: { value },
        bubbles: false,
        composed: false,
      })
    );
  }

  private setFieldError(message: string): void {
    this.errorMessage = message;
    this.dispatchEvent(
      new CustomEvent<SecretValueErrorDetail>('secret-value-error', {
        detail: { message },
        bubbles: true,
        composed: true,
      })
    );
  }

  private onValueInput(e: Event): void {
    const target = e.target as HTMLInputElement | HTMLTextAreaElement | null;
    let value = target?.value ?? '';
    if (this.revealed) {
      if (this.revealedLineEnding === 'mixed') {
        return;
      }
      if (this.revealedLineEnding === 'crlf') {
        value = value.replace(/\r\n/g, '\n').replace(/\r/g, '\n').replace(/\n/g, '\r\n');
      }
    }
    this.clearGeneration++;
    this.captured = false;
    this._value = value;
    if (this.errorMessage) {
      this.setFieldError('');
    }
    this.revision++;
    this.emitValueChange(this._value);
  }

  private applyMaskedInsert(target: HTMLInputElement | null, incoming: string, e: Event): void {
    const hasNewline = incoming.includes('\n') || incoming.includes('\r');
    const isCaptured = this.isCaptured;

    // Non-captured row receiving single-line text: let the browser handle it
    // natively so password bullets, selection, and caret position are preserved.
    if (!isCaptured && !hasNewline) {
      return;
    }

    e.preventDefault();
    if (!incoming) return;

    let nextValue: string;
    if (isCaptured) {
      nextValue = incoming;
    } else {
      const current = target?.value ?? this._value;
      const selStart = target?.selectionStart ?? current.length;
      const selEnd = target?.selectionEnd ?? selStart;
      nextValue = current.slice(0, selStart) + incoming + current.slice(selEnd);
    }

    this.clearGeneration++;
    this._value = nextValue;
    if (nextValue.includes('\n') || nextValue.includes('\r')) {
      this.captured = true;
      if (target) target.value = '';
    } else {
      this.captured = false;
      if (target) {
        target.value = nextValue;
        target.setSelectionRange?.(nextValue.length, nextValue.length);
      }
    }
    if (this.errorMessage) {
      this.setFieldError('');
    }
    this.revision++;
    this.emitValueChange(this._value);
  }

  private clearCapturedValue(target: HTMLInputElement | null, e: Event): void {
    e.preventDefault();
    this.clearGeneration++;
    this._value = '';
    this.captured = false;
    if (target) {
      target.value = '';
    }
    if (this.errorMessage) {
      this.setFieldError('');
    }
    this.revision++;
    this.emitValueChange('');
  }

  private onMaskedKeydown(e: KeyboardEvent): void {
    if (isUndoRedoShortcut(e)) {
      e.preventDefault();
      return;
    }
    if ((e.key === 'Backspace' || e.key === 'Delete') && this.isCaptured) {
      this.clearCapturedValue(e.target as HTMLInputElement | null, e);
    }
  }

  private extractBeforeInputText(e: InputEvent, includeLineBreaks: boolean): string | null {
    switch (e.inputType) {
      case 'insertFromPaste':
      case 'insertFromDrop':
      case 'insertReplacementText':
      case 'insertText':
        return e.dataTransfer?.getData('text/plain') ?? e.data ?? '';
      case 'insertLineBreak':
      case 'insertParagraph':
        return includeLineBreaks ? '\n' : null;
      default:
        return null;
    }
  }

  private onMaskedBeforeInput(e: InputEvent): void {
    if (e.inputType === 'historyUndo' || e.inputType === 'historyRedo') {
      e.preventDefault();
      return;
    }
    const target = e.target as HTMLInputElement | null;
    if (e.inputType.startsWith('delete') && this.isCaptured) {
      this.clearCapturedValue(target, e);
      return;
    }
    const incoming = this.extractBeforeInputText(e, true);
    if (incoming === null) return;
    this.applyMaskedInsert(target, incoming, e);
  }

  private onMaskedPaste(e: ClipboardEvent): void {
    const pasted = e.clipboardData?.getData('text/plain') ?? '';
    if (!pasted) return;
    this.applyMaskedInsert(e.target as HTMLInputElement | null, pasted, e);
  }

  private onMaskedDrop(e: DragEvent): void {
    const dropped = e.dataTransfer?.getData('text/plain') ?? '';
    if (!dropped) return;
    this.applyMaskedInsert(e.target as HTMLInputElement | null, dropped, e);
  }

  private onRevealedKeydown(e: KeyboardEvent): void {
    if (isUndoRedoShortcut(e)) {
      e.preventDefault();
    }
  }

  private applyRevealedInsert(
    target: HTMLTextAreaElement | null,
    incoming: string,
    e: Event
  ): void {
    if (this.revealedLineEnding === 'mixed') {
      e.preventDefault();
      return;
    }
    if (!incoming.includes('\r') && !incoming.includes('\n')) {
      return;
    }

    e.preventDefault();
    const rawCurrent = this._value;
    const displayCurrent = normalizeToLf(rawCurrent);
    const dispStart = target?.selectionStart ?? displayCurrent.length;
    const dispEnd = target?.selectionEnd ?? dispStart;
    const rawStart = displayOffsetToRawOffset(rawCurrent, dispStart);
    const rawEnd = displayOffsetToRawOffset(rawCurrent, dispEnd);
    const rawBefore = rawCurrent.slice(0, rawStart);
    const rawAfter = rawCurrent.slice(rawEnd);
    const nextValue = rawBefore + incoming + rawAfter;

    this.clearGeneration++;
    this._value = nextValue;
    this.captured = false;
    this.revealedLineEnding = detectLineEndingStyle(nextValue);

    if (target) {
      const nextDisplay = normalizeToLf(nextValue);
      const nextCaret = normalizeToLf(rawBefore + incoming).length;
      target.value = nextDisplay;
      target.setSelectionRange?.(nextCaret, nextCaret);
    }

    if (this.errorMessage) {
      this.setFieldError('');
    }
    this.revision++;
    this.emitValueChange(this._value);
  }

  private onRevealedBeforeInput(e: InputEvent): void {
    if (e.inputType === 'historyUndo' || e.inputType === 'historyRedo') {
      e.preventDefault();
      return;
    }
    // Typing Enter (insertLineBreak/insertParagraph) is handled natively so
    // onValueInput preserves the row's current line-ending style (e.g. CRLF).
    const incoming = this.extractBeforeInputText(e, false);
    if (incoming === null) return;
    this.applyRevealedInsert(e.target as HTMLTextAreaElement | null, incoming, e);
  }

  private onRevealedPaste(e: ClipboardEvent): void {
    const pasted = e.clipboardData?.getData('text/plain') ?? '';
    if (!pasted) return;
    this.applyRevealedInsert(e.target as HTMLTextAreaElement | null, pasted, e);
  }

  private onRevealedDrop(e: DragEvent): void {
    const dropped = e.dataTransfer?.getData('text/plain') ?? '';
    if (!dropped) return;
    this.applyRevealedInsert(e.target as HTMLTextAreaElement | null, dropped, e);
  }

  private triggerFilePicker(): void {
    const input = this.shadowRoot?.querySelector<HTMLInputElement>('input[type="file"]');
    input?.click();
  }

  private keySubject(): string {
    return this.secretKey ? ` for ${this.secretKey}` : '';
  }

  private async onFileSelected(e: Event): Promise<void> {
    const input = e.target as HTMLInputElement | null;
    const file = input?.files?.[0];
    if (!file) return;

    // Clear any existing value immediately so a failed or slow load never leaves stale secret bytes.
    const hadValue = this._value.length > 0;
    this.clearGeneration++;
    const gen = this.clearGeneration;
    this.pendingFocusTextarea = false;
    this._value = '';
    this.captured = false;
    this.revealedLineEnding = 'lf';
    this.revealed = false;
    const pwdInput = this.shadowRoot?.querySelector<HTMLInputElement>('input.secret-input');
    if (pwdInput) {
      pwdInput.value = '';
    }
    const ta = this.shadowRoot?.querySelector<HTMLTextAreaElement>('textarea.secret-textarea');
    if (ta) {
      ta.value = '';
    }
    this.revision++;
    if (hadValue) {
      this.emitValueChange('');
    }

    const subject = this.keySubject();
    const limitLabel = formatMaxBytes(this.maxBytes);

    try {
      if (file.size > this.maxBytes) {
        this.setFieldError(`File${subject} exceeds the ${limitLabel} limit.`);
        return;
      }

      let buf: ArrayBuffer;
      try {
        buf = await file.arrayBuffer();
      } catch {
        if (!this.isConnected || this.clearGeneration !== gen) {
          return;
        }
        this.setFieldError(`Could not read the file${subject}.`);
        return;
      }
      if (!this.isConnected || this.clearGeneration !== gen) {
        return;
      }
      if (buf.byteLength > this.maxBytes) {
        this.setFieldError(`File${subject} exceeds the ${limitLabel} limit.`);
        return;
      }

      let decoded: string;
      try {
        // Preserve a leading UTF-8 BOM verbatim (secret values are never altered).
        decoded = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(buf);
      } catch {
        this.setFieldError(
          `File${subject} is not valid UTF-8 text. Binary files are not supported.`
        );
        return;
      }

      if (decoded.includes('\u0000')) {
        this.setFieldError(
          `File${subject} appears to be binary (contains NUL bytes). Binary files are not supported.`
        );
        return;
      }

      if (decoded.length === 0) {
        this.setFieldError(`Selected file${subject} is empty.`);
        return;
      }

      this._value = decoded;
      this.captured = true;
      if (this.revealed) {
        this.revealed = false;
        this.revealedLineEnding = 'lf';
      }
      this.setFieldError('');
      this.revision++;
      this.emitValueChange(this._value);
    } finally {
      if (input) {
        input.value = '';
      }
    }
  }

  private toggleReveal(): void {
    if (this.revealed) {
      this.revealed = false;
      this.revealedLineEnding = 'lf';
    } else {
      this.revealedLineEnding = detectLineEndingStyle(this._value);
      this.revealed = true;
      this.pendingFocusTextarea = true;
    }
  }

  override render(): TemplateResult {
    const isFile = this.allowFileLoad;
    const rows = this.textareaRows > 0 ? this.textareaRows : isFile ? 4 : 2;
    const isReadonlyRevealed = this.revealed && this.revealedLineEnding === 'mixed';
    const textareaValue = normalizeToLf(this._value);
    const hasCapturedValue = !this.revealed && this.isCaptured;
    const key = this.secretKey;
    const basePlaceholder =
      this.placeholder ||
      (isFile ? 'Paste secret file contents…' : key ? `Enter value for ${key}…` : 'Secret value');
    const maskedPlaceholder = hasCapturedValue
      ? isFile
        ? 'Value entered — type, paste or load to replace'
        : 'Value entered — type or paste to replace'
      : basePlaceholder;

    const inputAriaLabel = key
      ? hasCapturedValue
        ? `Secret value for ${key} entered, hidden`
        : `Secret value for ${key}`
      : hasCapturedValue
        ? 'Secret value entered, hidden'
        : 'Secret value';

    const toggleAriaLabel = key
      ? this.revealed
        ? `Hide value for ${key}`
        : `Show value for ${key}`
      : this.revealed
        ? 'Hide value'
        : 'Show value';

    const fileInputAriaLabel = key ? `Load secret file for ${key}` : 'Load secret file';
    const maskedStatusAriaLabel = key
      ? `Secret value for ${key} entered, hidden`
      : 'Secret value entered, hidden';

    return html`
      ${this.label ? html`<span class="field-label">${this.label}</span>` : nothing}
      <div class="input-wrap" data-field-rev=${this.revision}>
        ${this.revealed
          ? html`
              <textarea
                class="secret-textarea"
                style="--secret-rows: ${rows}"
                data-key=${key}
                data-row-id=${this.rowId || nothing}
                aria-label=${key ? `Secret value for ${key}` : 'Secret value'}
                placeholder=${basePlaceholder}
                autocomplete="off"
                autocorrect="off"
                autocapitalize="off"
                spellcheck="false"
                data-lpignore="true"
                data-1p-ignore="true"
                rows=${rows}
                ?disabled=${this.disabled}
                ?readonly=${isReadonlyRevealed}
                .value=${textareaValue}
                @keydown=${(e: KeyboardEvent): void => this.onRevealedKeydown(e)}
                @beforeinput=${(e: InputEvent): void => this.onRevealedBeforeInput(e)}
                @paste=${(e: ClipboardEvent): void => this.onRevealedPaste(e)}
                @drop=${(e: DragEvent): void => this.onRevealedDrop(e)}
                @input=${(e: Event): void => this.onValueInput(e)}
              ></textarea>
            `
          : html`
              <input
                class="secret-input"
                type="password"
                data-key=${key}
                data-row-id=${this.rowId || nothing}
                aria-label=${inputAriaLabel}
                placeholder=${maskedPlaceholder}
                autocomplete="off"
                autocorrect="off"
                autocapitalize="off"
                spellcheck="false"
                data-lpignore="true"
                data-1p-ignore="true"
                ?disabled=${this.disabled}
                .value=${hasCapturedValue ? '' : this._value}
                @keydown=${(e: KeyboardEvent): void => this.onMaskedKeydown(e)}
                @beforeinput=${(e: InputEvent): void => this.onMaskedBeforeInput(e)}
                @paste=${(e: ClipboardEvent): void => this.onMaskedPaste(e)}
                @drop=${(e: DragEvent): void => this.onMaskedDrop(e)}
                @input=${(e: Event): void => this.onValueInput(e)}
              />
            `}
        <button
          type="button"
          class="toggle-visibility"
          data-toggle-visibility=${key}
          aria-pressed=${this.revealed ? 'true' : 'false'}
          aria-label=${toggleAriaLabel}
          title=${this.revealed ? 'Hide value' : 'Show value'}
          ?disabled=${this.disabled}
          @click=${(): void => this.toggleReveal()}
        >
          <sl-icon name=${this.revealed ? 'eye-slash' : 'eye'} aria-hidden="true"></sl-icon>
        </button>
      </div>
      <div class="file-controls">
        ${isFile
          ? html`
              <input
                type="file"
                tabindex="-1"
                class="file-picker-input"
                data-file-input=${key}
                data-row-id=${this.rowId || nothing}
                aria-label=${fileInputAriaLabel}
                ?disabled=${this.disabled}
                @change=${(e: Event): void => void this.onFileSelected(e)}
              />
              <sl-button
                type="button"
                size="small"
                variant="default"
                class="load-file-btn"
                data-load-file=${key}
                ?disabled=${this.disabled}
                @click=${(): void => this.triggerFilePicker()}
              >
                <sl-icon slot="prefix" name="upload" aria-hidden="true"></sl-icon>
                Load from file
              </sl-button>
            `
          : nothing}
        ${hasCapturedValue
          ? html`
              <span
                class="file-value-status"
                data-testid="file-value-masked"
                role="status"
                aria-label=${maskedStatusAriaLabel}
              >
                <sl-icon name="lock" aria-hidden="true"></sl-icon>
                <span aria-hidden="true">&bull;&bull;&bull;&bull;&bull;&bull;&bull;&bull;</span>
                <span>Value entered (hidden)</span>
              </span>
            `
          : nothing}
        ${isReadonlyRevealed
          ? html`
              <span
                class="mixed-line-endings-note"
                data-testid="mixed-line-endings-note"
                role="status"
              >
                This value has mixed line endings; Hide, then paste or load to replace it.
              </span>
            `
          : nothing}
      </div>
      ${this.showError && this.errorMessage
        ? html`<div class="field-error" data-testid="secret-value-error" role="alert">
            ${this.errorMessage}
          </div>`
        : nothing}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-secret-value-field': ScionSecretValueField;
  }
}
