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
 * Members sidebar component for the chat view.
 *
 * Renders two sections:
 * - **Humans** — project members with presence indicators
 *   (green dot = active, moon = idle)
 * - **Agents** — project agents with status badges
 *
 * Listens to `chat.presence` SSE events (via the parent passing updated
 * presence state) to update presence indicators in real time.
 *
 * Clicking a member opens a DM in the centre panel.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import './chat-avatar.js';

/** A human member from the GET /chat/spaces/{id}/members endpoint. */
export interface ChatHumanMember {
  id: string;
  kind: 'user';
  displayName: string;
  email?: string;
  avatarUrl?: string;
  role?: string;
  presenceState?: 'active' | 'idle' | '';
}

/** An agent member from the GET /chat/spaces/{id}/members endpoint. */
export interface ChatAgentMember {
  id: string;
  kind: 'agent';
  displayName: string;
  slug?: string;
  phase?: string;
  activity?: string;
}

export type ChatMember = ChatHumanMember | ChatAgentMember;

/** Detail emitted when a member is clicked (to open a DM). */
export interface MemberClickDetail {
  memberId: string;
  memberKind: 'user' | 'agent';
  displayName: string;
}

@customElement('scion-chat-members')
export class ScionChatMembers extends LitElement {
  /** Human members of the space. */
  @property({ type: Array })
  humans: ChatHumanMember[] = [];

  /** Agent members of the space. */
  @property({ type: Array })
  agents: ChatAgentMember[] = [];

  /** Current user ID — used to skip "DM yourself" on click. */
  @property({ attribute: 'current-user-id' })
  currentUserId = '';

  static override styles = css`
    :host {
      display: flex;
      flex-direction: column;
      height: 100%;
      overflow-y: auto;
      font-family: var(--sl-font-sans);
    }

    .section-label {
      padding: 12px 16px 4px;
      font-size: 0.6875rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #94a3b8);
    }

    .member-item {
      display: flex;
      align-items: center;
      gap: 10px;
      padding: 6px 16px;
      cursor: pointer;
      border-radius: 4px;
      margin: 0 8px;
      transition: background 0.15s;
    }

    .member-item:hover {
      background: var(--scion-surface-hover, rgba(0, 0, 0, 0.05));
    }

    .member-info {
      flex: 1;
      min-width: 0;
    }

    .member-name {
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .member-role {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #94a3b8);
    }

    .agent-status {
      display: inline-flex;
      align-items: center;
      gap: 4px;
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #94a3b8);
    }

    .agent-status .dot {
      width: 6px;
      height: 6px;
      border-radius: 50%;
      flex-shrink: 0;
    }

    .dot.running {
      background: #22c55e;
    }

    .dot.idle {
      background: #f59e0b;
    }

    .dot.stopped {
      background: #94a3b8;
    }

    .dot.error {
      background: #ef4444;
    }

    .empty-note {
      padding: 12px 16px;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #94a3b8);
      font-style: italic;
    }
  `;

  override render() {
    return html` ${this.renderHumans()} ${this.renderAgents()} `;
  }

  private renderHumans() {
    const sorted = [...this.humans].sort((a, b) => {
      // Active users first, then alphabetical
      const aActive = a.presenceState === 'active' ? 0 : 1;
      const bActive = b.presenceState === 'active' ? 0 : 1;
      if (aActive !== bActive) return aActive - bActive;
      return a.displayName.localeCompare(b.displayName);
    });

    return html`
      <div class="section-label">People — ${sorted.length}</div>
      ${sorted.length === 0
        ? html`<div class="empty-note">No members</div>`
        : sorted.map((m) => this.renderHuman(m))}
    `;
  }

  private renderHuman(m: ChatHumanMember) {
    return html`
      <div
        class="member-item"
        @click=${() => this.handleMemberClick(m.id, 'user', m.displayName)}
        title="${m.email || m.displayName}"
      >
        <scion-chat-avatar
          name="${m.displayName}"
          avatar-url="${m.avatarUrl || ''}"
          size="28"
          presence-state="${m.presenceState || ''}"
        ></scion-chat-avatar>
        <div class="member-info">
          <div class="member-name">${m.displayName}</div>
          ${m.role ? html`<div class="member-role">${m.role}</div>` : nothing}
        </div>
      </div>
    `;
  }

  private renderAgents() {
    const sorted = [...this.agents].sort((a, b) => {
      // Running agents first, then alphabetical
      const aRunning = a.phase === 'running' ? 0 : 1;
      const bRunning = b.phase === 'running' ? 0 : 1;
      if (aRunning !== bRunning) return aRunning - bRunning;
      return a.displayName.localeCompare(b.displayName);
    });

    return html`
      <div class="section-label">Agents — ${sorted.length}</div>
      ${sorted.length === 0
        ? html`<div class="empty-note">No agents</div>`
        : sorted.map((a) => this.renderAgent(a))}
    `;
  }

  private renderAgent(a: ChatAgentMember) {
    const dotClass = this.agentDotClass(a.phase || '');
    const statusLabel = a.activity || a.phase || 'unknown';

    return html`
      <div
        class="member-item"
        @click=${() => this.handleMemberClick(a.id, 'agent', a.displayName)}
        title="${a.slug || a.displayName}"
      >
        <scion-chat-avatar name="${a.slug || a.displayName}" size="28"></scion-chat-avatar>
        <div class="member-info">
          <div class="member-name">${a.displayName}</div>
          <div class="agent-status">
            <span class="dot ${dotClass}"></span>
            ${statusLabel}
          </div>
        </div>
      </div>
    `;
  }

  private agentDotClass(phase: string): string {
    switch (phase) {
      case 'running':
        return 'running';
      case 'idle':
      case 'waiting':
        return 'idle';
      case 'error':
      case 'failed':
        return 'error';
      default:
        return 'stopped';
    }
  }

  private handleMemberClick(id: string, kind: 'user' | 'agent', displayName: string) {
    if (kind === 'user' && id === this.currentUserId) return;

    this.dispatchEvent(
      new CustomEvent<MemberClickDetail>('member-click', {
        detail: { memberId: id, memberKind: kind, displayName },
        bubbles: true,
        composed: true,
      })
    );
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-members': ScionChatMembers;
  }
}
