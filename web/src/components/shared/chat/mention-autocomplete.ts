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
 * Mention autocomplete dropdown component.
 *
 * Provides @-mention suggestions for agent names in the chat composer.
 *
 * Design decisions (from design doc §4.5):
 * - Trigger: `@` at a word boundary (start of input or preceded by whitespace)
 * - Source: stateManager.getAgents() — already cached, no network call
 * - Matching: case-insensitive subsequence over slug and name; exact-prefix ranked first
 * - Keys: Up/Down navigate, Enter/Tab accept, Esc dismiss
 * - Insert: plain text `@<slug> ` — no chips, no hidden markup
 * - Code fence guard: `@` inside fenced code blocks does NOT trigger (AC17)
 * - Dropdown capped at 8 items
 *
 * Rejected approaches (design doc):
 * - contenteditable with mention chips — too complex, breaks IME
 * - New /mention-candidates endpoint — GET /agents already has everything
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import type { Agent } from '../../../shared/types.js';

/** Maximum items shown in the dropdown. */
const MAX_DROPDOWN_ITEMS = 8;

/** Detail emitted when the user accepts a mention. */
export interface MentionAcceptDetail {
  slug: string;
  /** The start index of the `@` trigger in the textarea value. */
  triggerStart: number;
}

/** Human member for mention autocomplete (v2). */
export interface MentionMember {
  id: string;
  name: string;
  email: string;
  avatarUrl?: string;
  kind: 'user' | 'agent';
}

/** A unified candidate for the dropdown. */
interface MentionCandidate {
  slug: string;
  name: string;
  kind: 'agent' | 'user';
  avatarUrl: string | undefined;
}

@customElement('scion-mention-autocomplete')
export class ScionMentionAutocomplete extends LitElement {
  /** All agents available for mentioning. Set by the parent. */
  @property({ type: Array })
  agents: Agent[] = [];

  /** Human members available for mentioning (v2). Set by the parent. */
  @property({ type: Array })
  members: MentionMember[] = [];

  /** Whether the autocomplete is currently active. */
  @state() active = false;

  /** Matched candidates (already filtered + ranked). */
  @state() private candidates: MentionCandidate[] = [];

  /** Index of the highlighted candidate. */
  @state() private highlightIndex = 0;

  /** Horizontal offset for the dropdown (pixels from left of host). */
  @state() private dropdownLeft = 0;

  /** Internal tracking of the trigger position. */
  private triggerStart = -1;

  /** Cached mirror div for caret position measurement (O2 fix). */
  private mirrorDiv: HTMLDivElement | null = null;

  /** Cached marker span inside the mirror div. */
  private mirrorMarker: HTMLSpanElement | null = null;

  /** Last known textarea width used for the cached mirror div. */
  private mirrorWidth = 0;

  static override styles = css`
    :host {
      display: block;
      position: relative;
    }

    .dropdown {
      position: absolute;
      z-index: 100;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.5rem;
      box-shadow: 0 4px 12px rgba(0, 0, 0, 0.12);
      max-width: 280px;
      min-width: 200px;
      overflow: hidden;
    }

    .dropdown-item {
      display: flex;
      flex-direction: column;
      padding: 0.375rem 0.75rem;
      cursor: pointer;
      font-size: 0.8125rem;
      transition: background 0.1s;
    }

    .dropdown-item:hover,
    .dropdown-item.highlighted {
      background: var(--scion-primary-50, #eff6ff);
    }

    .dropdown-item .slug {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .dropdown-item .name {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .no-results {
      padding: 0.5rem 0.75rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      font-style: italic;
    }
  `;

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.removeMirrorDiv();
  }

  /** Remove the cached mirror div from the DOM (cleanup). */
  private removeMirrorDiv(): void {
    if (this.mirrorDiv && this.mirrorDiv.parentNode) {
      this.mirrorDiv.parentNode.removeChild(this.mirrorDiv);
    }
    this.mirrorDiv = null;
    this.mirrorMarker = null;
    this.mirrorWidth = 0;
  }

  override render() {
    if (!this.active || this.candidates.length === 0) return nothing;

    return html`
      <div
        class="dropdown"
        style="bottom: calc(100% + 4px); left: ${this.dropdownLeft}px;"
        @mousedown=${this.handleMouseDown}
      >
        ${this.candidates.map(
          (candidate, i) => html`
            <div
              class="dropdown-item ${i === this.highlightIndex ? 'highlighted' : ''}"
              data-index=${i}
              @click=${() => this.acceptCandidate(i)}
              @mouseenter=${() => {
                this.highlightIndex = i;
              }}
            >
              <span class="slug">
                <sl-icon
                  name="${candidate.kind === 'agent' ? 'cpu' : 'person'}"
                  style="font-size: 0.6875rem; vertical-align: -1px; margin-right: 0.125rem;"
                ></sl-icon>
                @${candidate.slug}
              </span>
              ${candidate.slug !== candidate.name
                ? html`<span class="name">${candidate.name}</span>`
                : nothing}
            </div>
          `
        )}
      </div>
    `;
  }

  /**
   * Called by the parent composer on every input event.
   * Determines whether to open/update/close the autocomplete.
   *
   * @param text Full textarea value
   * @param cursorPos Current cursor position in the textarea
   * @param textarea The textarea element (for caret measurement)
   */
  handleInput(text: string, cursorPos: number, textarea: HTMLTextAreaElement): void {
    // Find the @ trigger working backwards from cursor.
    const triggerInfo = this.findTrigger(text, cursorPos);

    if (!triggerInfo) {
      this.dismiss();
      return;
    }

    this.triggerStart = triggerInfo.start;
    const query = text.slice(triggerInfo.start + 1, cursorPos);

    // Filter and rank agents + members.
    const matched = this.matchCandidates(query);

    if (matched.length === 0) {
      this.dismiss();
      return;
    }

    this.candidates = matched;
    this.highlightIndex = 0;
    this.active = true;

    // Position the dropdown near the caret.
    this.positionDropdown(textarea, triggerInfo.start);
  }

  /**
   * Called by the parent on keydown. Returns true if the event was consumed.
   */
  handleKeydown(e: KeyboardEvent): boolean {
    if (!this.active) return false;

    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        this.highlightIndex = (this.highlightIndex + 1) % this.candidates.length;
        return true;

      case 'ArrowUp':
        e.preventDefault();
        this.highlightIndex =
          (this.highlightIndex - 1 + this.candidates.length) % this.candidates.length;
        return true;

      case 'Enter':
      case 'Tab':
        e.preventDefault();
        this.acceptCandidate(this.highlightIndex);
        return true;

      case 'Escape':
        e.preventDefault();
        this.dismiss();
        return true;

      default:
        return false;
    }
  }

  /** Dismiss the dropdown. */
  dismiss(): void {
    this.active = false;
    this.candidates = [];
    this.highlightIndex = 0;
    this.triggerStart = -1;
  }

  // ---------------------------------------------------------------------------
  // Private
  // ---------------------------------------------------------------------------

  /**
   * Find the @ trigger position, working backwards from cursorPos.
   * Returns null if no valid trigger is found.
   *
   * Rules:
   * - The `@` must be at position 0 or preceded by whitespace (word boundary).
   * - The `@` must NOT be inside a fenced code block (AC17).
   */
  private findTrigger(text: string, cursorPos: number): { start: number } | null {
    // Walk backwards from cursor to find @.
    for (let i = cursorPos - 1; i >= 0; i--) {
      const ch = text[i];

      // If we hit whitespace before finding @, no trigger.
      if (/\s/.test(ch)) return null;

      if (ch === '@') {
        // Must be at word boundary: start of text or preceded by whitespace.
        if (i > 0 && !/\s/.test(text[i - 1])) return null;

        // Code fence guard (AC17): check if this @ is inside a fenced code block.
        if (this.isInsideCodeFence(text, i)) return null;

        return { start: i };
      }
    }
    return null;
  }

  /**
   * Checks whether a position in the text is inside a fenced code block.
   * Fences are exactly triple backticks (```) at the start of a line,
   * optionally followed by a language tag — but NOT four or more backticks
   * (which are inline constructs, not fence delimiters) (O3 fix).
   */
  private isInsideCodeFence(text: string, pos: number): boolean {
    const before = text.slice(0, pos);
    // Match exactly triple backticks at line start, not followed by another backtick.
    const fencePattern = /^```(?!`)/gm;
    let count = 0;
    while (fencePattern.exec(before) !== null) {
      count++;
    }
    // Inside a fence if count is odd (opened but not closed).
    return count % 2 === 1;
  }

  /**
   * Match agents and human members against a query string using case-insensitive
   * subsequence matching over slug and name. Exact-prefix matches ranked first.
   * Agents appear first, then humans (distinct icon styling in the dropdown).
   */
  private matchCandidates(query: string): MentionCandidate[] {
    // Build a unified list of candidates from agents + members
    const allCandidates: MentionCandidate[] = [];

    for (const agent of this.agents || []) {
      allCandidates.push({
        slug: agent.slug || agent.name || '',
        name: agent.name || '',
        kind: 'agent',
        avatarUrl: undefined,
      });
    }

    for (const member of this.members || []) {
      // Avoid duplicate entries if a member is also an agent
      const slug = member.name.toLowerCase().replace(/\s+/g, '-');
      if (!allCandidates.some((c) => c.slug === slug)) {
        allCandidates.push({
          slug,
          name: member.name,
          kind: member.kind === 'agent' ? 'agent' : 'user',
          avatarUrl: member.avatarUrl,
        });
      }
    }

    if (query === '') {
      return allCandidates.slice(0, MAX_DROPDOWN_ITEMS);
    }

    const lowerQuery = query.toLowerCase();
    const prefixMatches: MentionCandidate[] = [];
    const subsequenceMatches: MentionCandidate[] = [];

    for (const candidate of allCandidates) {
      const slug = candidate.slug.toLowerCase();
      const name = candidate.name.toLowerCase();

      if (slug.startsWith(lowerQuery) || name.startsWith(lowerQuery)) {
        prefixMatches.push(candidate);
      } else if (this.isSubsequence(lowerQuery, slug) || this.isSubsequence(lowerQuery, name)) {
        subsequenceMatches.push(candidate);
      }
    }

    // Agents first, then humans, within each tier
    const sortByKind = (a: MentionCandidate, b: MentionCandidate) =>
      a.kind === 'agent' && b.kind !== 'agent'
        ? -1
        : a.kind !== 'agent' && b.kind === 'agent'
          ? 1
          : 0;
    prefixMatches.sort(sortByKind);
    subsequenceMatches.sort(sortByKind);

    return [...prefixMatches, ...subsequenceMatches].slice(0, MAX_DROPDOWN_ITEMS);
  }

  /** Check if `sub` is a subsequence of `str`. */
  private isSubsequence(sub: string, str: string): boolean {
    let si = 0;
    for (let i = 0; i < str.length && si < sub.length; i++) {
      if (str[i] === sub[si]) si++;
    }
    return si === sub.length;
  }

  /** Dispatch the accept event and close the dropdown. */
  private acceptCandidate(index: number): void {
    const candidate = this.candidates[index];
    if (!candidate) return;

    this.dispatchEvent(
      new CustomEvent<MentionAcceptDetail>('mention-accept', {
        detail: {
          slug: candidate.slug,
          triggerStart: this.triggerStart,
        },
        bubbles: true,
        composed: true,
      })
    );

    this.dismiss();
  }

  /** Styles copied from the textarea onto the mirror div. */
  private static readonly MIRROR_STYLES = [
    'font-family',
    'font-size',
    'font-weight',
    'line-height',
    'letter-spacing',
    'word-spacing',
    'padding-top',
    'padding-right',
    'padding-bottom',
    'padding-left',
    'border-width',
    'box-sizing',
    'white-space',
    'word-wrap',
    'overflow-wrap',
  ] as const;

  /**
   * Position the dropdown near the caret using a mirrored-div measurement.
   * The mirror div is cached and reused across keystrokes; it is only
   * recreated when the textarea dimensions change (O2 fix).
   */
  private positionDropdown(textarea: HTMLTextAreaElement, triggerPos: number): void {
    const currentWidth = textarea.offsetWidth;

    // Create or recreate the mirror div if needed.
    if (!this.mirrorDiv || !this.mirrorDiv.parentNode || this.mirrorWidth !== currentWidth) {
      this.removeMirrorDiv();

      const mirror = document.createElement('div');
      const computed = window.getComputedStyle(textarea);

      mirror.style.position = 'absolute';
      mirror.style.visibility = 'hidden';
      mirror.style.whiteSpace = 'pre-wrap';
      mirror.style.wordWrap = 'break-word';
      mirror.style.width = `${currentWidth}px`;

      for (const prop of ScionMentionAutocomplete.MIRROR_STYLES) {
        mirror.style.setProperty(prop, computed.getPropertyValue(prop));
      }

      const marker = document.createElement('span');
      marker.textContent = '@';

      mirror.appendChild(document.createTextNode(''));
      mirror.appendChild(marker);

      document.body.appendChild(mirror);

      this.mirrorDiv = mirror;
      this.mirrorMarker = marker;
      this.mirrorWidth = currentWidth;
    }

    // Update the text content before the marker.
    const textBefore = textarea.value.slice(0, triggerPos);
    this.mirrorDiv.firstChild!.textContent = textBefore;

    // Measure position relative to the host element.
    const markerRect = this.mirrorMarker!.getBoundingClientRect();
    const hostRect = this.getBoundingClientRect();

    this.dropdownLeft = Math.max(0, markerRect.left - hostRect.left);
  }

  /**
   * Prevent the textarea from losing focus when clicking the dropdown.
   */
  private handleMouseDown(e: Event): void {
    e.preventDefault();
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-mention-autocomplete': ScionMentionAutocomplete;
  }
}
