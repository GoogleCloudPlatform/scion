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
 * Reusable JSON browser component with progressive disclosure.
 *
 * Renders structured data as expandable key-value pairs. Objects and arrays
 * are collapsed by default and expand on click. Primitive values use syntax
 * coloring (strings=green, numbers=blue, booleans=purple, null=gray).
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';

@customElement('scion-json-browser')
export class ScionJsonBrowser extends LitElement {
  @property({ type: Object })
  data: unknown = null;

  @property({ type: Boolean, attribute: 'expand-first' })
  expandFirst = false;

  static override styles = css`
    :host {
      display: block;
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.8125rem;
      line-height: 1.5;
      color: var(--scion-text, #1e293b);
    }

    .node {
      padding-left: 1.25rem;
    }

    .entry {
      display: flex;
      align-items: baseline;
      gap: 0.375rem;
      min-height: 1.5em;
    }

    .key {
      color: var(--scion-text-secondary, #475569);
      flex-shrink: 0;
    }

    .colon {
      color: var(--scion-text-muted, #64748b);
      flex-shrink: 0;
    }

    .toggle {
      cursor: pointer;
      user-select: none;
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      border: none;
      background: none;
      padding: 0;
      font: inherit;
      color: inherit;
    }
    .toggle:hover .key {
      color: var(--scion-primary, #3b82f6);
    }

    .arrow {
      display: inline-block;
      width: 0.75em;
      font-size: 0.625rem;
      text-align: center;
      color: var(--scion-text-muted, #64748b);
      transition: transform 0.15s ease;
      flex-shrink: 0;
    }
    .arrow.open {
      transform: rotate(90deg);
    }

    .preview {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.75rem;
    }

    /* Value type colors */
    .val-string {
      color: var(--scion-success-700, #15803d);
      word-break: break-all;
    }
    .val-number {
      color: var(--scion-primary-700, #1d4ed8);
    }
    .val-boolean {
      color: #7c3aed;
    }
    .val-null {
      color: var(--scion-text-muted, #64748b);
      font-style: italic;
    }
  `;

  /** Track which paths are expanded. */
  private expanded = new Set<string>();

  override render() {
    if (this.data == null) return nothing;
    return this.renderValue(this.data, '', true);
  }

  private renderValue(
    value: unknown,
    path: string,
    isRoot: boolean
  ): TemplateResult | typeof nothing {
    if (value === null || value === undefined) {
      return html`<span class="val-null">null</span>`;
    }

    if (Array.isArray(value)) {
      return this.renderComposite(value, path, isRoot, true);
    }

    if (typeof value === 'object') {
      return this.renderComposite(value as Record<string, unknown>, path, isRoot, false);
    }

    return this.renderPrimitive(value);
  }

  private renderPrimitive(value: unknown): TemplateResult {
    if (typeof value === 'string') {
      return html`<span class="val-string">"${value}"</span>`;
    }
    if (typeof value === 'number') {
      return html`<span class="val-number">${value}</span>`;
    }
    if (typeof value === 'boolean') {
      return html`<span class="val-boolean">${String(value)}</span>`;
    }
    return html`<span>${String(value)}</span>`;
  }

  private renderComposite(
    value: Record<string, unknown> | unknown[],
    path: string,
    isRoot: boolean,
    isArray: boolean
  ): TemplateResult {
    const entries = isArray
      ? (value as unknown[]).map((v, i) => [String(i), v] as [string, unknown])
      : Object.entries(value as Record<string, unknown>);

    const isOpen = this.expanded.has(path) || (isRoot && this.expandFirst);
    const count = entries.length;
    const preview = isArray ? `[${count}]` : `{${count}}`;

    if (isRoot && !isArray && typeof value === 'object') {
      // Root object: render entries directly (no toggle wrapper)
      if (!isOpen && !this.expandFirst) {
        // Show collapsed root with toggle
        return html`
          <button class="toggle" @click=${() => this.togglePath(path)}>
            <span class="arrow">&#9654;</span>
            <span class="preview">${preview}</span>
          </button>
        `;
      }
      return html`
        <div class="node" style="padding-left: 0">
          ${entries.map(
            ([key, val]) => html`
              <div class="entry">
                ${this.isComposite(val)
                  ? this.renderCompositeEntry(key, val, `${path}.${key}`)
                  : html`
                      <span class="key">${key}</span>
                      <span class="colon">:</span>
                      ${this.renderValue(val, `${path}.${key}`, false)}
                    `}
              </div>
            `
          )}
        </div>
      `;
    }

    if (!isOpen) {
      return html`<span class="preview">${preview}</span>`;
    }

    return html`
      <div class="node">
        ${entries.map(
          ([key, val]) => html`
            <div class="entry">
              ${this.isComposite(val)
                ? this.renderCompositeEntry(key, val, `${path}.${key}`)
                : html`
                    <span class="key">${key}</span>
                    <span class="colon">:</span>
                    ${this.renderValue(val, `${path}.${key}`, false)}
                  `}
            </div>
          `
        )}
      </div>
    `;
  }

  private renderCompositeEntry(key: string, val: unknown, path: string): TemplateResult {
    const isOpen = this.expanded.has(path);
    const isArr = Array.isArray(val);
    const count = isArr ? (val as unknown[]).length : Object.keys(val as object).length;
    const preview = isArr ? `[${count}]` : `{${count}}`;

    return html`
      <div>
        <button class="toggle" @click=${() => this.togglePath(path)}>
          <span class="arrow ${isOpen ? 'open' : ''}">&#9654;</span>
          <span class="key">${key}</span>
          <span class="colon">:</span>
          ${!isOpen ? html`<span class="preview">${preview}</span>` : nothing}
        </button>
        ${isOpen ? this.renderValue(val, path, false) : nothing}
      </div>
    `;
  }

  private isComposite(value: unknown): boolean {
    return value !== null && value !== undefined && typeof value === 'object';
  }

  private togglePath(path: string): void {
    if (this.expanded.has(path)) {
      this.expanded.delete(path);
    } else {
      this.expanded.add(path);
    }
    this.requestUpdate();
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-json-browser': ScionJsonBrowser;
  }
}
