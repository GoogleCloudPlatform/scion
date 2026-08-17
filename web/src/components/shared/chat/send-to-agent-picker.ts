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
 * Send-to-agent picker component.
 *
 * Shows a dropdown of available agents. When the user selects an agent,
 * emits an `agent-selected` event so the parent can navigate to the
 * agent's DM with the source message content as quoted context.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import type { Agent } from '../../../shared/types.js';

/** Detail emitted when the user selects an agent from the picker. */
export interface AgentSelectedDetail {
  agentSlug: string;
  agentId: string;
}

@customElement('scion-send-to-agent-picker')
export class ScionSendToAgentPicker extends LitElement {
  /** All agents available for sending to. Set by the parent. */
  @property({ type: Array })
  agents: Agent[] = [];

  /** Whether the picker is currently open. */
  @property({ type: Boolean, reflect: true })
  open = false;

  /** Absolute X position for the picker. */
  @property({ type: Number })
  posX = 0;

  /** Absolute Y position for the picker. */
  @property({ type: Number })
  posY = 0;

  /** Filter text for narrowing the agent list. */
  @state() private filterText = '';

  /** Index of the highlighted agent in the filtered list. */
  @state() private highlightIndex = 0;

  static override styles = css`
    :host {
      display: block;
      position: fixed;
      z-index: 200;
    }

    .picker-overlay {
      position: fixed;
      inset: 0;
      z-index: 199;
    }

    .picker {
      position: relative;
      z-index: 200;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.5rem;
      box-shadow: 0 4px 16px rgba(0, 0, 0, 0.15);
      min-width: 220px;
      max-width: 300px;
      max-height: 320px;
      overflow: hidden;
      display: flex;
      flex-direction: column;
    }

    .picker-header {
      padding: 0.5rem 0.75rem;
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .picker-filter {
      padding: 0.375rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .picker-filter input {
      width: 100%;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.25rem;
      padding: 0.25rem 0.5rem;
      font-size: 0.8125rem;
      outline: none;
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text, #1e293b);
    }

    .picker-filter input:focus {
      border-color: var(--scion-primary, #3b82f6);
    }

    .picker-list {
      overflow-y: auto;
      flex: 1;
    }

    .picker-item {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      cursor: pointer;
      font-size: 0.8125rem;
      color: var(--scion-text, #1e293b);
      transition: background 0.1s;
    }

    .picker-item:hover,
    .picker-item.highlighted {
      background: var(--scion-primary-50, #eff6ff);
    }

    .picker-item sl-icon {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .picker-item .agent-slug {
      font-weight: 600;
    }

    .picker-item .agent-name {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .no-agents {
      padding: 0.75rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      font-style: italic;
      text-align: center;
    }
  `;

  override render() {
    if (!this.open) return nothing;

    const filtered = this.filteredAgents();

    return html`
      <div class="picker-overlay" @click=${this.handleOverlayClick}></div>
      <div class="picker" style="left: ${this.posX}px; top: ${this.posY}px;">
        <div class="picker-header">Send to Agent</div>
        <div class="picker-filter">
          <input
            type="text"
            placeholder="Filter agents..."
            .value=${this.filterText}
            @input=${this.handleFilterInput}
            @keydown=${this.handleKeydown}
          />
        </div>
        <div class="picker-list">
          ${filtered.length > 0
            ? filtered.map(
                (agent, i) => html`
                  <div
                    class="picker-item ${i === this.highlightIndex ? 'highlighted' : ''}"
                    @click=${() => this.selectAgent(agent)}
                    @mouseenter=${() => { this.highlightIndex = i; }}
                  >
                    <sl-icon name="cpu"></sl-icon>
                    <span class="agent-slug">${agent.slug || agent.name}</span>
                    ${agent.slug && agent.name && agent.slug !== agent.name
                      ? html`<span class="agent-name">${agent.name}</span>`
                      : nothing}
                  </div>
                `
              )
            : html`<div class="no-agents">No agents found</div>`}
        </div>
      </div>
    `;
  }

  override updated(changedProperties: Map<string, unknown>): void {
    super.updated(changedProperties);
    if (changedProperties.has('open') && this.open) {
      this.filterText = '';
      this.highlightIndex = 0;
      // Focus the filter input after render
      void this.updateComplete.then(() => {
        const input = this.shadowRoot?.querySelector('.picker-filter input') as HTMLInputElement | null;
        if (input) input.focus();
      });
    }
  }

  /** Close the picker. */
  close(): void {
    this.open = false;
    this.filterText = '';
    this.highlightIndex = 0;
  }

  /** Filter agents based on the filter text. */
  private filteredAgents(): Agent[] {
    if (!this.filterText) return this.agents;
    const lower = this.filterText.toLowerCase();
    return this.agents.filter((a) => {
      const slug = (a.slug || a.name || '').toLowerCase();
      const name = (a.name || '').toLowerCase();
      return slug.includes(lower) || name.includes(lower);
    });
  }

  private handleFilterInput(e: Event): void {
    this.filterText = (e.target as HTMLInputElement).value;
    this.highlightIndex = 0;
  }

  private handleKeydown(e: KeyboardEvent): void {
    const filtered = this.filteredAgents();
    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        if (filtered.length > 0) {
          this.highlightIndex = (this.highlightIndex + 1) % filtered.length;
        }
        break;
      case 'ArrowUp':
        e.preventDefault();
        if (filtered.length > 0) {
          this.highlightIndex = (this.highlightIndex - 1 + filtered.length) % filtered.length;
        }
        break;
      case 'Enter':
        e.preventDefault();
        if (filtered.length > 0 && this.highlightIndex < filtered.length) {
          this.selectAgent(filtered[this.highlightIndex]);
        }
        break;
      case 'Escape':
        e.preventDefault();
        this.close();
        break;
    }
  }

  private selectAgent(agent: Agent): void {
    this.dispatchEvent(
      new CustomEvent<AgentSelectedDetail>('agent-selected', {
        detail: {
          agentSlug: agent.slug || agent.name || '',
          agentId: agent.id,
        },
        bubbles: true,
        composed: true,
      })
    );
    this.close();
  }

  private handleOverlayClick(): void {
    this.close();
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-send-to-agent-picker': ScionSendToAgentPicker;
  }
}
