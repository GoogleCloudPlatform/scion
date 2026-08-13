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
 * Three-state visibility toggle for the chat thread.
 *
 * Modes:
 * - Conversation: show only normal messages (default)
 * - Verbose: show normal + verbose (automatic assistant replies)
 * - Full: show everything (including trace/debug)
 *
 * Emits `visibility-change` CustomEvent with `detail.mode` when the
 * user changes the selection.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';

export type VisibilityMode = 'conversation' | 'verbose' | 'full';

export interface VisibilityChangeDetail {
  mode: VisibilityMode;
}

@customElement('scion-chat-visibility-toggle')
export class ScionChatVisibilityToggle extends LitElement {
  @property()
  mode: VisibilityMode = 'conversation';

  static override styles = css`
    :host {
      display: inline-block;
    }

    sl-radio-group::part(form-control) {
      margin: 0;
    }
  `;

  override render() {
    return html`
      <sl-radio-group value=${this.mode} size="small" @sl-change=${this.handleChange}>
        <sl-radio-button value="conversation">
          <sl-icon slot="prefix" name="chat-dots" style="font-size: 0.75rem"></sl-icon>
          Conversation
        </sl-radio-button>
        <sl-radio-button value="verbose">
          <sl-icon slot="prefix" name="chat-text" style="font-size: 0.75rem"></sl-icon>
          Verbose
        </sl-radio-button>
        <sl-radio-button value="full">
          <sl-icon slot="prefix" name="code-slash" style="font-size: 0.75rem"></sl-icon>
          Full
        </sl-radio-button>
      </sl-radio-group>
    `;
  }

  private handleChange(e: Event): void {
    const value = (e.target as HTMLInputElement).value as VisibilityMode;
    if (value === this.mode) return;
    this.mode = value;
    this.dispatchEvent(
      new CustomEvent<VisibilityChangeDetail>('visibility-change', {
        detail: { mode: value },
        bubbles: true,
        composed: true,
      })
    );
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-visibility-toggle': ScionChatVisibilityToggle;
  }
}
