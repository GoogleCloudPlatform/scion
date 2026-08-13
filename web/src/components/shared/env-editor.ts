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
 * Reusable key-value editor for environment variables.
 *
 * Displays a dynamic list of key/value rows with add/remove support.
 * Required keys are visually indicated and cannot be removed.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';

export interface EnvEntry {
  key: string;
  value: string;
}

@customElement('scion-env-editor')
export class ScionEnvEditor extends LitElement {
  @property({ type: Array })
  entries: EnvEntry[] = [];

  @property({ type: Array, attribute: 'required-keys' })
  requiredKeys: string[] = [];

  static override styles = css`
    :host {
      display: block;
    }

    .env-header {
      display: grid;
      grid-template-columns: 1fr 1fr 36px;
      gap: 0.5rem;
      margin-bottom: 0.5rem;
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
    }

    .env-row {
      display: grid;
      grid-template-columns: 1fr 1fr 36px;
      gap: 0.5rem;
      margin-bottom: 0.5rem;
      align-items: center;
    }

    .env-row sl-input {
      width: 100%;
    }

    .env-row.required sl-input[data-field='key']::part(base) {
      border-left: 3px solid var(--sl-color-warning-500, #f59e0b);
    }

    .env-row.missing sl-input[data-field='value']::part(base) {
      border-color: var(--sl-color-danger-500, #ef4444);
    }

    .remove-btn {
      display: flex;
      align-items: center;
      justify-content: center;
      width: 36px;
      height: 36px;
      border: none;
      background: none;
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      border-radius: var(--scion-radius, 0.5rem);
      transition: all 150ms ease;
    }

    .remove-btn:hover {
      background: var(--sl-color-danger-50, #fef2f2);
      color: var(--sl-color-danger-500, #ef4444);
    }

    .remove-btn:disabled {
      opacity: 0.3;
      cursor: not-allowed;
    }

    .remove-btn:disabled:hover {
      background: none;
      color: var(--scion-text-muted, #64748b);
    }

    .add-btn {
      margin-top: 0.5rem;
    }

    .empty-state {
      padding: 1.5rem;
      text-align: center;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }
  `;

  private fireChange(): void {
    this.dispatchEvent(
      new CustomEvent('env-change', {
        detail: { entries: [...this.entries] },
        bubbles: true,
        composed: true,
      })
    );
  }

  private handleKeyInput(index: number, e: Event): void {
    const input = e.target as HTMLElement & { value: string };
    this.entries = this.entries.map((entry, i) =>
      i === index ? { ...entry, key: input.value } : entry
    );
    this.fireChange();
  }

  private handleValueInput(index: number, e: Event): void {
    const input = e.target as HTMLElement & { value: string };
    this.entries = this.entries.map((entry, i) =>
      i === index ? { ...entry, value: input.value } : entry
    );
    this.fireChange();
  }

  private addRow(): void {
    this.entries = [...this.entries, { key: '', value: '' }];
    this.fireChange();
  }

  private removeRow(index: number): void {
    this.entries = this.entries.filter((_, i) => i !== index);
    this.fireChange();
  }

  override render() {
    return html`
      ${this.entries.length > 0
        ? html`
            <div class="env-header">
              <span>Key</span>
              <span>Value</span>
              <span></span>
            </div>
          `
        : ''}
      ${this.entries.map((entry, i) => {
        const isRequired = this.requiredKeys.includes(entry.key);
        const isMissing = isRequired && !entry.value;
        return html`
          <div class="env-row ${isRequired ? 'required' : ''} ${isMissing ? 'missing' : ''}">
            <sl-input
              data-field="key"
              size="small"
              placeholder="VARIABLE_NAME"
              .value=${entry.key}
              ?readonly=${isRequired}
              @sl-input=${(e: Event) => this.handleKeyInput(i, e)}
            ></sl-input>
            <sl-input
              data-field="value"
              size="small"
              placeholder=${isMissing ? 'Required' : 'value'}
              .value=${entry.value}
              @sl-input=${(e: Event) => this.handleValueInput(i, e)}
            ></sl-input>
            <button
              class="remove-btn"
              ?disabled=${isRequired}
              title=${isRequired ? 'Required variable' : 'Remove'}
              @click=${() => this.removeRow(i)}
            >
              <sl-icon name="x-circle"></sl-icon>
            </button>
          </div>
        `;
      })}
      ${this.entries.length === 0
        ? html`<div class="empty-state">No environment variables configured.</div>`
        : ''}
      <sl-button class="add-btn" size="small" variant="default" @click=${() => this.addRow()}>
        <sl-icon slot="prefix" name="plus-lg"></sl-icon>
        Add Variable
      </sl-button>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-env-editor': ScionEnvEditor;
  }
}
