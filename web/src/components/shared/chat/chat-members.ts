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
import { customElement, property, state } from 'lit/decorators.js';
import type { PropertyValues } from 'lit';
import { ACTIVITY_DISPLAY } from '../../../shared/agent-state-display.js';
import { navigateTo } from '../../../client/main.js';
import './chat-avatar.js';
import '../status-badge.js';

/**
 * Statuses that represent a settled agent. Entering one of these is the end of
 * an activity burst, so the avatar must not wobble — it would otherwise draw
 * the eye to an agent that has just gone quiet.
 */
const TERMINAL_STATUSES = new Set([
  'blocked',
  'completed',
  'stalled',
  'error',
  'waiting_for_input',
  'limits_exceeded',
  'offline',
  'stopped',
  'suspended',
]);

/** How long an agent avatar wobbles after a state change. */
const WOBBLE_DURATION_MS = 3000;

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
  lastSeen?: string;
  projectId?: string;
  /** Freeform status detail — what the agent detail page shows as "Detail". */
  detailMessage?: string;
  /** When the agent last changed state (not the heartbeat in `lastSeen`). */
  lastActivityEvent?: string;
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

  /** ID of the current DM peer — highlighted in the members list. */
  @property({ attribute: 'dm-peer-id' })
  dmPeerId = '';

  /** IDs of members currently typing — shows a dot overlay on their avatar. */
  @property({ type: Array })
  typingUserIds: string[] = [];

  /** IDs of members with unread messages — shows a blue dot on their avatar. */
  @property({ type: Array })
  unreadFromIds: string[] = [];

  /** Agent IDs that recently changed state — drives wobble animation. */
  @state() private recentlyChangedAgents = new Set<string>();
  /** Timers for clearing the recently-changed state after WOBBLE_DURATION_MS. */
  private _wobbleTimers = new Map<string, ReturnType<typeof setTimeout>>();
  /** Previous agent state snapshots for change detection. */
  private _prevAgentStates = new Map<string, string>();

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

    .member-item.active-peer {
      background: var(--scion-primary-50, #eff6ff);
      border-left: 2px solid var(--scion-primary, #3b82f6);
      padding-left: 14px;
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


    .agent-terminal,
    .agent-popout {
      display: inline-flex;
      align-items: center;
      color: var(--scion-text-muted, #94a3b8);
      opacity: 0;
      transition: opacity 0.15s;
      text-decoration: none;
      flex-shrink: 0;
    }

    .member-item:hover .agent-terminal,
    .member-item:hover .agent-popout {
      opacity: 1;
    }

    .agent-terminal:hover,
    .agent-popout:hover {
      color: var(--scion-primary, #3b82f6);
    }

    scion-status-badge {
      transform: scale(0.85);
      transform-origin: left center;
    }

    /*
     * The agent tooltip is two lines (detail + updated time) joined by a
     * newline. Shoelace's tooltip body collapses whitespace by default, so
     * the break has to be opted into.
     */
    sl-tooltip::part(body) {
      white-space: pre-line;
      text-align: left;
      max-width: 260px;
    }

    .empty-note {
      padding: 12px 16px;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #94a3b8);
      font-style: italic;
    }

    /* Unread indicator dot — top-left of avatar */
    .unread-dot {
      position: absolute;
      top: 0;
      left: 0;
      width: 8px;
      height: 8px;
      border-radius: 50%;
      background: #3b82f6;
      border: 2px solid var(--scion-bg, #1e293b);
      z-index: 1;
    }

    /* Typing indicator overlay on avatar */
    .avatar-wrapper {
      position: relative;
      flex-shrink: 0;
    }

    .typing-overlay {
      position: absolute;
      bottom: -2px;
      right: -2px;
      display: flex;
      align-items: center;
      gap: 1.5px;
      background: var(--scion-surface, #ffffff);
      border-radius: 6px;
      padding: 2px 3px;
      box-shadow: 0 0 0 1.5px var(--scion-surface, #ffffff);
    }

    .typing-overlay span {
      width: 3px;
      height: 3px;
      border-radius: 50%;
      background: var(--scion-primary, #3b82f6);
      animation: typing-dot-bounce 1.4s ease-in-out infinite;
    }

    .typing-overlay span:nth-child(2) {
      animation-delay: 0.2s;
    }

    .typing-overlay span:nth-child(3) {
      animation-delay: 0.4s;
    }

    @keyframes agent-wobble {
      0%, 100% { transform: translateX(0); }
      25% { transform: translateX(15%); }
      75% { transform: translateX(-15%); }
    }

    .avatar-wrapper.active {
      animation: agent-wobble 0.8s ease-in-out infinite;
    }

    @keyframes typing-dot-bounce {
      0%,
      60%,
      100% {
        transform: translateY(0);
        opacity: 0.4;
      }
      30% {
        transform: translateY(-2px);
        opacity: 1;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    // Clicking empty area in the members sidebar resets to global view
    this.addEventListener('click', this._handleHostClick);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.removeEventListener('click', this._handleHostClick);
    // Clean up wobble timers
    for (const timer of this._wobbleTimers.values()) clearTimeout(timer);
    this._wobbleTimers.clear();
  }

  override updated(changedProps: PropertyValues): void {
    super.updated(changedProps);
    if (changedProps.has('agents')) {
      this.checkAgentStateChanges();
    }
  }

  /**
   * Compare current agent states against previous to detect changes for wobble.
   *
   * A change into a terminal state stops the wobble instead of starting one —
   * the agent has gone quiet and should not keep drawing attention.
   *
   * `recentlyChangedAgents` must be REPLACED, never mutated in place: Lit
   * compares `@state()` values by reference, so `Set.add()` / `Set.delete()`
   * would not schedule a re-render and the wobble would never appear.
   */
  private checkAgentStateChanges(): void {
    for (const a of this.agents) {
      const currentState = `${a.phase}:${a.activity}`;
      const prevState = this._prevAgentStates.get(a.id);

      if (prevState !== undefined && prevState !== currentState) {
        if (TERMINAL_STATUSES.has(this.resolveAgentStatus(a))) {
          this.stopWobble(a.id);
        } else {
          this.startWobble(a.id);
        }
      }

      this._prevAgentStates.set(a.id, currentState);
    }
  }

  /** Start (or restart) the wobble for an agent, ending after WOBBLE_DURATION_MS. */
  private startWobble(agentId: string): void {
    this.recentlyChangedAgents = new Set([...this.recentlyChangedAgents, agentId]);

    const existing = this._wobbleTimers.get(agentId);
    if (existing) clearTimeout(existing);

    this._wobbleTimers.set(
      agentId,
      setTimeout(() => {
        this.stopWobble(agentId);
      }, WOBBLE_DURATION_MS)
    );
  }

  /** Stop an in-flight wobble immediately. */
  private stopWobble(agentId: string): void {
    const timer = this._wobbleTimers.get(agentId);
    if (timer) clearTimeout(timer);
    this._wobbleTimers.delete(agentId);

    if (!this.recentlyChangedAgents.has(agentId)) return;
    const next = new Set(this.recentlyChangedAgents);
    next.delete(agentId);
    this.recentlyChangedAgents = next;
  }

  /** Click on the host element itself (empty space) triggers a reset. */
  private _handleHostClick = (e: MouseEvent): void => {
    // Only fire when the click lands on the host itself or on the
    // scrollable container (not on a member item or section label)
    const path = e.composedPath();
    const clickedMember = path.some(
      (el) => el instanceof HTMLElement && el.classList?.contains('member-item')
    );
    if (clickedMember) return;
    const clickedLabel = path.some(
      (el) => el instanceof HTMLElement && el.classList?.contains('section-label')
    );
    if (clickedLabel) return;

    this.dispatchEvent(new CustomEvent('reset-view', { bubbles: true, composed: true }));
  };

  override render() {
    return html` ${this.renderHumans()} ${this.renderAgents()} `;
  }

  private renderHumans() {
    // Filter out the current user so they don't appear in their own
    // members sidebar.
    const visible = this.humans.filter((m) => m.id !== this.currentUserId);
    const sorted = [...visible].sort((a, b) => {
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
    const isActive = this.dmPeerId === m.id;
    const isTyping = this.typingUserIds.includes(m.id);
    const hasUnread = this.unreadFromIds.includes(m.id);
    return html`
      <div
        class="member-item ${isActive ? 'active-peer' : ''}"
        @click=${() => this.handleMemberClick(m.id, 'user', m.displayName)}
        title="${m.email || m.displayName}"
      >
        <div class="avatar-wrapper">
          <scion-chat-avatar
            name="${m.displayName}"
            color-seed="${m.id}"
            avatar-url="${m.avatarUrl || ''}"
            size="28"
            presence-state="${m.presenceState || ''}"
          ></scion-chat-avatar>
          ${hasUnread ? html`<div class="unread-dot"></div>` : nothing}
          ${isTyping
            ? html`<div class="typing-overlay"><span></span><span></span><span></span></div>`
            : nothing}
        </div>
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

  /** Map an agent's phase/activity to a StatusType for the badge. */
  private resolveAgentStatus(a: ChatAgentMember): string {
    // Mirror getAgentDisplayStatus (shared/types.ts) so the sidebar shows the
    // same fine-grained state as the agent list: while an agent is running its
    // activity ("thinking", "executing", "blocked", ...) is the real status;
    // otherwise the phase is.
    const activity = (a.activity || '').toLowerCase();
    if (a.phase === 'running' && Object.hasOwn(ACTIVITY_DISPLAY, activity)) return activity;
    return a.phase || 'unknown';
  }

  private renderAgent(a: ChatAgentMember) {
    const isActive = this.dmPeerId === a.id;
    const isTyping = this.typingUserIds.includes(a.id);
    const hasUnread = this.unreadFromIds.includes(a.id);

    // Build tooltip: status detail (line 1) + updated time (line 2). The
    // detail message is the same text the agent detail page shows, and
    // "Updated" is the last state change — matching the agent list's column,
    // not the `lastSeen` heartbeat.
    const detailText = a.detailMessage || a.activity || a.phase || 'unknown';
    const updated = a.lastActivityEvent ? this.formatRelativeTime(a.lastActivityEvent) : '';
    const updatedText = updated ? `Updated: ${updated}` : '';
    const tooltipContent = updatedText ? `${detailText}\n${updatedText}` : detailText;

    const badgeStatus = this.resolveAgentStatus(a);

    const agentRow = html`
      <div
        class="member-item ${isActive ? 'active-peer' : ''}"
        @click=${() => this.handleMemberClick(a.id, 'agent', a.displayName)}
      >
        <div class="avatar-wrapper ${this.recentlyChangedAgents.has(a.id) ? 'active' : ''}">
          <scion-chat-avatar name="${a.slug || a.displayName}" color-seed="${a.id}" size="28"></scion-chat-avatar>
          ${hasUnread ? html`<div class="unread-dot"></div>` : nothing}
          ${isTyping
            ? html`<div class="typing-overlay"><span></span><span></span><span></span></div>`
            : nothing}
        </div>
        <div class="member-info">
          <div class="member-name">${a.displayName}</div>
          <scion-status-badge
            status=${badgeStatus}
            size="small"
          ></scion-status-badge>
        </div>
        <a
          href="/agents/${a.id}/terminal"
          class="agent-terminal"
          title="Open terminal"
          @click=${(e: Event) => {
            e.stopPropagation();
            e.preventDefault();
            navigateTo(`/agents/${a.id}/terminal`);
          }}
        >
          <sl-icon name="terminal" style="font-size: 0.75rem;"></sl-icon>
        </a>
        <a
          href="/agents/${a.id}"
          target="_blank"
          class="agent-popout"
          title="Open agent detail"
          @click=${(e: Event) => e.stopPropagation()}
        >
          <sl-icon name="box-arrow-up-right" style="font-size: 0.75rem;"></sl-icon>
        </a>
      </div>
    `;

    return html`
      <sl-tooltip .content=${tooltipContent} placement="left" hoist>
        ${agentRow}
      </sl-tooltip>
    `;
  }

  /** Format an ISO timestamp as relative time (e.g., "2 min ago"). */
  private formatRelativeTime(iso: string): string {
    const d = new Date(iso);
    if (isNaN(d.getTime())) return '';
    const now = Date.now();
    const diffMs = now - d.getTime();
    const diffMin = Math.floor(diffMs / 60000);

    if (diffMin < 1) return 'just now';
    if (diffMin < 60) return `${diffMin} min ago`;
    const diffHrs = Math.floor(diffMin / 60);
    if (diffHrs < 24) return `${diffHrs} hr ago`;
    const diffDays = Math.floor(diffHrs / 24);
    if (diffDays < 7) return `${diffDays}d ago`;
    return d.toLocaleDateString('en', { month: 'short', day: 'numeric' });
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
