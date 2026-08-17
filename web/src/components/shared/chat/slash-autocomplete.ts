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
 * Slash command autocomplete dropdown component.
 *
 * Provides slash-command suggestions in the chat composer.
 * Follows the same pattern as mention-autocomplete.ts.
 *
 * Design decisions:
 * - Trigger: `/` at position 0 of the input (start of text only)
 * - Matching: case-insensitive prefix match on command name
 * - Keys: Up/Down navigate, Enter/Tab accept, Esc dismiss
 * - On accept: emits `slash-command` event with {command, args}
 * - Dropdown capped at 8 items
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

/** Maximum items shown in the dropdown. */
const MAX_DROPDOWN_ITEMS = 8;

/** A slash command definition. */
export interface SlashCommand {
  name: string;
  description: string;
  usage: string;
}

/** Detail emitted when the user selects a slash command. */
export interface SlashCommandDetail {
  command: string;
  args: string;
}

/** Built-in slash commands. */
const BUILT_IN_COMMANDS: SlashCommand[] = [
  { name: 'status', description: 'Show project status', usage: '/status' },
  { name: 'clear', description: 'Clear the conversation', usage: '/clear' },
  { name: 'help', description: 'Show available commands', usage: '/help' },
  { name: 'spawn', description: 'Spawn a new agent', usage: '/spawn <template>' },
  { name: 'stop', description: 'Stop a running agent', usage: '/stop <agent>' },
  { name: 'default', description: 'Set or clear default agent', usage: '/default <agent|clear>' },
];

@customElement('scion-slash-autocomplete')
export class ScionSlashAutocomplete extends LitElement {
  /** Whether the autocomplete is currently active. */
  @property({ type: Boolean })
  active = false;

  /** Filtered commands matching the current input. */
  @state() private commands: SlashCommand[] = [];

  /** Index of the highlighted command. */
  @state() private selectedIndex = 0;

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
      max-width: 320px;
      min-width: 220px;
      overflow: hidden;
      bottom: calc(100% + 4px);
      left: 0;
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

    .dropdown-item .command-name {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .dropdown-item .command-desc {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
  `;

  override render() {
    if (!this.active || this.commands.length === 0) return nothing;

    return html`
      <div class="dropdown" @mousedown=${this.handleMouseDown}>
        ${this.commands.map(
          (cmd, i) => html`
            <div
              class="dropdown-item ${i === this.selectedIndex ? 'highlighted' : ''}"
              data-index=${i}
              @click=${() => this.acceptCommand(i)}
              @mouseenter=${() => { this.selectedIndex = i; }}
            >
              <span class="command-name">/${cmd.name}</span>
              <span class="command-desc">${cmd.description}</span>
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
   * @param _cursorPos Current cursor position (unused — slash commands trigger at position 0)
   */
  handleInput(text: string, _cursorPos: number): void {
    // Only trigger if the text starts with `/`
    if (!text.startsWith('/')) {
      this.dismiss();
      return;
    }

    // Extract the command prefix (the word after `/`, before any space)
    const afterSlash = text.slice(1);
    const spaceIdx = afterSlash.indexOf(' ');
    const prefix = spaceIdx >= 0 ? afterSlash.slice(0, spaceIdx) : afterSlash;

    // If the user has already typed a full command + space, don't show autocomplete
    if (spaceIdx >= 0) {
      const matched = BUILT_IN_COMMANDS.find(
        (cmd) => cmd.name.toLowerCase() === prefix.toLowerCase()
      );
      if (matched) {
        // Full command typed, dismiss
        this.dismiss();
        return;
      }
    }

    // Filter commands by prefix
    const lowerPrefix = prefix.toLowerCase();
    const matched = BUILT_IN_COMMANDS.filter((cmd) =>
      cmd.name.toLowerCase().startsWith(lowerPrefix)
    ).slice(0, MAX_DROPDOWN_ITEMS);

    if (matched.length === 0) {
      this.dismiss();
      return;
    }

    this.commands = matched;
    this.selectedIndex = 0;
    this.active = true;
  }

  /**
   * Called by the parent on keydown. Returns true if the event was consumed.
   */
  handleKeydown(e: KeyboardEvent): boolean {
    if (!this.active) return false;

    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        this.selectedIndex = (this.selectedIndex + 1) % this.commands.length;
        return true;

      case 'ArrowUp':
        e.preventDefault();
        this.selectedIndex =
          (this.selectedIndex - 1 + this.commands.length) % this.commands.length;
        return true;

      case 'Enter':
      case 'Tab':
        e.preventDefault();
        this.acceptCommand(this.selectedIndex);
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
    this.commands = [];
    this.selectedIndex = 0;
  }

  // ---------------------------------------------------------------------------
  // Private
  // ---------------------------------------------------------------------------

  /** Dispatch the slash-command event and close the dropdown. */
  private acceptCommand(index: number): void {
    const cmd = this.commands[index];
    if (!cmd) return;

    this.dispatchEvent(
      new CustomEvent<SlashCommandDetail>('slash-command', {
        detail: {
          command: cmd.name,
          args: '',
        },
        bubbles: true,
        composed: true,
      })
    );

    this.dismiss();
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
    'scion-slash-autocomplete': ScionSlashAutocomplete;
  }
}
