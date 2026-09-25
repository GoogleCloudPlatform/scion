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
import { openTerminal, terminalHref } from '../../../client/open-terminal.js';
import { isFeatureEnabled } from '../../../utils/feature-flags.js';
import './chat-avatar.js';
import '../status-badge.js';

/** Popup window geometry for a terminal. Roughly 80x24 at a comfortable size. */
const TERMINAL_POPOUT_WIDTH = 1024;
const TERMINAL_POPOUT_HEIGHT = 700;

/**
 * Open an agent's terminal in its own window (legacy mode only).
 *
 * When the terminal workspace feature flag is enabled, callers must use
 * {@link openTerminal} instead so all terminal opens join the singleton
 * coordinator and reuse a retained session.
 *
 * The window is *named per agent*, which is the whole point: clicking the same
 * agent again focuses the window that is already open instead of spawning
 * another one. Six agents means six windows, not one window per click - the
 * tab pile-up that made this control painful in the first place.
 *
 * Note the deliberate absence of `noopener`: a named window cannot be reused
 * or focused if the opener is severed, and the target is our own same-origin
 * route. Falls back to in-app navigation when a popup blocker intervenes, so
 * the control always does something.
 */
function openTerminalPopout(agentId: string): void {
  const features = [
    'popup=yes',
    `width=${TERMINAL_POPOUT_WIDTH}`,
    `height=${TERMINAL_POPOUT_HEIGHT}`,
    'resizable=yes',
    'scrollbars=yes',
  ].join(',');

  const path = `/agents/${agentId}/terminal`;
  const base = import.meta.env.BASE_URL;
  const url = base && base !== '/' ? base.replace(/\/$/, '') + path : path;

  const win = window.open(url, `scion-term-${agentId}`, features);
  if (win) {
    win.focus();
    return;
  }
  navigateTo(`/agents/${agentId}/terminal`);
}

/**
 * Open a terminal for the given agent through the appropriate path.
 *
 * When the terminal workspace is enabled, routes through the singleton
 * coordinator so all entry points converge on one owner/session.  Chat source
 * state remains intact because the route outlet is hidden, not destroyed.
 *
 * When the workspace is disabled, falls back to the legacy popup behaviour.
 */
function openTerminalFromChat(agentId: string): void {
  if (isFeatureEnabled('web.terminal_workspace')) {
    openTerminal(agentId);
  } else {
    openTerminalPopout(agentId);
  }
}

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
  /**
   * Whether the viewer may open a terminal on this agent, as decided by the
   * Hub. The sidebar lists every agent in the space's project, but attaching
   * is gated by authorizeAgentLifecycle, so without this the terminal control
   * appears for agents the viewer cannot open and clicking it is refused.
   *
   * The control renders only on an explicit true. Anything else - absent,
   * undefined, dropped somewhere in the client - hides it. An earlier version
   * tested `=== false` so a missing field would keep the old behaviour, and
   * that is precisely how the gate failed twice: the server omitted false via
   * omitempty, and the page's own mappers dropped the field while rebuilding
   * member objects. A permission gate should fail closed.
   *
   * Explicitly `| undefined` because exactOptionalPropertyTypes is on: the
   * mappers below pass the field through unconditionally, and "present but
   * undefined" has to be assignable for that to typecheck.
   */
  canAttach?: boolean | undefined;
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

  /** Slug of the agent used by default for the current thread. */
  @property({ attribute: 'default-agent-slug' })
  defaultAgentSlug = '';

  /** IDs of members currently typing — shows a dot overlay on their avatar. */
  @property({ type: Array })
  typingUserIds: string[] = [];

  /** IDs of members with unread messages — shows a blue dot on their avatar. */
  @property({ type: Array })
  unreadFromIds: string[] = [];

  /** Filter mode: 'all' shows every member, 'unread' shows only those with unread messages. */
  @state() private memberFilter: 'all' | 'unread' = 'all';

  /** Sort mode: 'alpha' sorts A-Z by display name, 'activity' sorts by recent activity. */
  @state() private memberSort: 'alpha' | 'activity' = 'alpha';

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
      overflow: hidden;
      font-family: var(--sl-font-sans);
    }

    .members-body {
      flex: 1;
      min-height: 0;
      overflow-y: auto;
    }

    .section-label {
      padding: 12px 16px 4px;
      font-size: var(--chat-fs-sm);
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
      font-size: var(--chat-fs-md);
      font-weight: 500;
      color: var(--scion-text, #1e293b);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .member-role {
      font-size: var(--chat-fs-sm);
      color: var(--scion-text-muted, #94a3b8);
    }

    .agent-subsection-label {
      padding: 4px 16px 2px;
      font-size: var(--chat-fs-xs, 0.625rem);
      font-weight: 500;
      text-transform: uppercase;
      letter-spacing: 0.04em;
      color: var(--scion-text-muted, #94a3b8);
    }

    .agent-default-divider {
      border: none;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      margin: 6px 16px;
    }

    .agent-terminal,
    .agent-graph {
      display: inline-flex;
      align-items: center;
      color: var(--scion-text-muted, #94a3b8);
      opacity: 0;
      transition: opacity 0.15s;
      text-decoration: none;
      flex-shrink: 0;
    }

    .member-item:hover .agent-terminal,
    .member-item:hover .agent-graph {
      opacity: 1;
    }

    .agent-terminal:hover,
    .agent-graph:hover {
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

    /* Filter + sort toolbar */
    .members-toolbar {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.375rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .filter-toggle {
      display: inline-flex;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      overflow: hidden;
      flex: 1;
    }

    .filter-toggle button {
      display: inline-flex;
      align-items: center;
      gap: 0.125rem;
      height: 1.5rem;
      border: none;
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      padding: 0 0.5rem;
      font-size: var(--chat-fs-sm);
      font-family: inherit;
      font-weight: 500;
      transition: all 150ms ease;
      white-space: nowrap;
      flex: 1;
      justify-content: center;
    }

    .filter-toggle button:not(:last-child) {
      border-right: 1px solid var(--scion-border, #e2e8f0);
    }

    .filter-toggle button:hover:not(.active) {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .filter-toggle button.active {
      background: var(--scion-primary, #3b82f6);
      color: white;
    }

    .filter-toggle button sl-icon {
      font-size: var(--chat-fs-sm);
    }

    .sort-btn {
      flex-shrink: 0;
    }

    .sort-btn::part(base) {
      font-size: var(--chat-fs-base);
      padding: 0.125rem;
    }

    .empty-note {
      padding: 12px 16px;
      font-size: var(--chat-fs-base);
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
      0%,
      100% {
        transform: translateX(0);
      }
      25% {
        transform: translateX(15%);
      }
      75% {
        transform: translateX(-15%);
      }
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

  override disconnectedCallback(): void {
    super.disconnectedCallback();
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

  override render() {
    return html`
      ${this.renderToolbar()}
      <div class="members-body">${this.renderHumans()} ${this.renderAgents()}</div>
    `;
  }

  /** Render the filter + sort toolbar at the top of the members sidebar. */
  private renderToolbar() {
    return html`
      <div class="members-toolbar">
        <div class="filter-toggle">
          <button
            class=${this.memberFilter === 'all' ? 'active' : ''}
            aria-pressed=${this.memberFilter === 'all'}
            @click=${() => this.setMemberFilter('all')}
          >
            All
          </button>
          <button
            class=${this.memberFilter === 'unread' ? 'active' : ''}
            aria-pressed=${this.memberFilter === 'unread'}
            @click=${() => this.setMemberFilter('unread')}
          >
            <sl-icon name="envelope"></sl-icon>
            Unread
          </button>
        </div>
        <sl-dropdown>
          <sl-icon-button
            slot="trigger"
            name="sort-down"
            class="sort-btn"
            label="Sort members"
          ></sl-icon-button>
          <sl-menu @sl-select=${this.handleMemberSortSelect}>
            <sl-menu-label>Sort members</sl-menu-label>
            <sl-menu-item type="checkbox" value="alpha" ?checked=${this.memberSort === 'alpha'}>
              Alphabetical
            </sl-menu-item>
            <sl-menu-item
              type="checkbox"
              value="activity"
              ?checked=${this.memberSort === 'activity'}
            >
              Recent activity
            </sl-menu-item>
          </sl-menu>
        </sl-dropdown>
      </div>
    `;
  }

  /** Set the member filter mode. */
  private setMemberFilter(filter: 'all' | 'unread'): void {
    if (this.memberFilter === filter) return;
    this.memberFilter = filter;
  }

  /** Handle sort mode selection from the dropdown. */
  private handleMemberSortSelect(e: Event): void {
    const detail = (e as CustomEvent<{ item?: HTMLElement }>).detail;
    const value = detail?.item?.getAttribute('value');
    if (value === 'alpha' || value === 'activity') {
      this.memberSort = value;
    }
  }

  private renderHumans() {
    // Filter out the current user so they don't appear in their own
    // members sidebar.
    let visible = this.humans.filter((m) => m.id !== this.currentUserId);

    // Apply unread filter
    if (this.memberFilter === 'unread') {
      visible = visible.filter((m) => this.unreadFromIds.includes(m.id));
    }

    const sorted = [...visible].sort((a, b) => {
      if (this.memberSort === 'activity') {
        // Active users first, idle second, then rest
        const aActive = a.presenceState === 'active' ? 0 : a.presenceState === 'idle' ? 1 : 2;
        const bActive = b.presenceState === 'active' ? 0 : b.presenceState === 'idle' ? 1 : 2;
        if (aActive !== bActive) return aActive - bActive;
        return a.displayName.localeCompare(b.displayName);
      }
      // Alphabetical
      return a.displayName.localeCompare(b.displayName);
    });

    return html`
      <div class="section-label">People — ${sorted.length}</div>
      ${sorted.length === 0
        ? html`<div class="empty-note">
            ${this.memberFilter === 'unread' ? 'No unread' : 'No members'}
          </div>`
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

  /** Sort agents per the current sort mode (alphabetical or recent activity). */
  private sortAgents(list: ChatAgentMember[]): ChatAgentMember[] {
    return [...list].sort((a, b) => {
      if (this.memberSort === 'activity') {
        // Sort by lastActivityEvent timestamp (most recent first)
        const aRaw = a.lastActivityEvent ? Date.parse(a.lastActivityEvent) : NaN;
        const aTime = Number.isNaN(aRaw) ? 0 : aRaw;
        const bRaw = b.lastActivityEvent ? Date.parse(b.lastActivityEvent) : NaN;
        const bTime = Number.isNaN(bRaw) ? 0 : bRaw;
        if (aTime !== bTime) return bTime - aTime;
        return a.displayName.localeCompare(b.displayName);
      }
      // Alphabetical
      return a.displayName.localeCompare(b.displayName);
    });
  }

  private renderAgents() {
    let visible = [...this.agents];

    // Apply unread filter
    if (this.memberFilter === 'unread') {
      visible = visible.filter((a) => this.unreadFromIds.includes(a.id));
    }

    // Pin the thread-default agent first, under its own sub-heading, as long
    // as it is actually present in the (possibly filtered) visible list —
    // otherwise we'd show an orphaned "Thread default" heading with nothing
    // under it.
    const defaultAgent = this.defaultAgentSlug
      ? visible.find((a) => a.slug === this.defaultAgentSlug)
      : undefined;

    const rest = defaultAgent ? visible.filter((a) => a !== defaultAgent) : visible;
    const sortedRest = this.sortAgents(rest);

    return html`
      <div class="section-label">Agents — ${visible.length}</div>
      ${visible.length === 0
        ? html`<div class="empty-note">
            ${this.memberFilter === 'unread' ? 'No unread' : 'No agents'}
          </div>`
        : html`
            ${defaultAgent
              ? this.renderDefaultAgentGroup(defaultAgent, sortedRest.length > 0)
              : nothing}
            ${sortedRest.map((a) => this.renderAgent(a))}
          `}
    `;
  }

  /**
   * Render the pinned thread-default agent under its "Thread default"
   * sub-heading, followed by a divider when other agents follow it.
   */
  private renderDefaultAgentGroup(defaultAgent: ChatAgentMember, hasRest: boolean) {
    return html`
      <div class="agent-subsection-label">Thread default</div>
      ${this.renderAgent(defaultAgent)}
      ${hasRest ? html`<hr class="agent-default-divider" />` : nothing}
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
          <scion-chat-avatar
            name="${a.slug || a.displayName}"
            color-seed="${a.id}"
            size="28"
          ></scion-chat-avatar>
          ${hasUnread ? html`<div class="unread-dot"></div>` : nothing}
          ${isTyping
            ? html`<div class="typing-overlay"><span></span><span></span><span></span></div>`
            : nothing}
        </div>
        <div class="member-info">
          <div class="member-name">${a.displayName}</div>
          <scion-status-badge status=${badgeStatus} size="small"></scion-status-badge>
        </div>
        ${a.canAttach !== true
          ? nothing
          : html`<a
              href=${terminalHref(a.id)}
              class="agent-terminal"
              title="Open terminal"
              @click=${(e: MouseEvent) => {
                e.stopPropagation();
                // Leave modified and non-primary clicks to the browser so
                // Ctrl/Cmd-click, Shift-click and middle-click behave as they
                // do on any other link.
                if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) {
                  return;
                }
                e.preventDefault();
                openTerminalFromChat(a.id);
              }}
            >
              <sl-icon name="terminal" style="font-size: var(--chat-fs-base);"></sl-icon>
            </a>`}
        ${a.projectId
          ? html`<a
              href="/agents/graph?project=${encodeURIComponent(
                a.projectId
              )}&focus=${encodeURIComponent(a.id)}"
              class="agent-graph"
              title="Open in graph"
              @click=${(e: MouseEvent) => {
                e.stopPropagation();
                if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
                e.preventDefault();
                navigateTo(
                  `/agents/graph?project=${encodeURIComponent(a.projectId!)}&focus=${encodeURIComponent(a.id)}`
                );
              }}
            >
              <sl-icon name="diagram-3" style="font-size: var(--chat-fs-base);"></sl-icon>
            </a>`
          : nothing}
      </div>
    `;

    return html`
      <sl-tooltip .content=${tooltipContent} placement="left" hoist> ${agentRow} </sl-tooltip>
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
