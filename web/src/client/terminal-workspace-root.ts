import type { User } from '../shared/types.js';
import {
  TerminalSessionRegistry,
  type TerminalConnectionState,
  type TerminalSession,
  type TerminalSessionState,
} from './terminal-sessions.js';
import type { TerminalAgentMetadata } from './terminal-metadata.js';
import type { ScionHeader } from '../components/shared/header.js';
import type { ScionTerminalPane } from '../components/terminal/terminal-pane.js';
import '../components/shared/header.js';
import '../components/terminal/terminal-pane.js';

interface RailEntry {
  session: TerminalSession;
  state: TerminalSessionState;
  metadata: TerminalAgentMetadata;
  unsubscribeState: () => void;
  unsubscribeMetadata: () => void;
}

export interface TerminalSessionCountDetail {
  readonly count: number;
}

export const TERMINAL_SESSION_COUNT_EVENT = 'scion:terminal-session-count';

/** Document-lived presentation. Pane nodes never move through disposable shells. */
export class TerminalWorkspaceRoot {
  readonly element = document.createElement('div');
  private readonly header: ScionHeader = document.createElement('scion-header');
  private readonly shell = document.createElement('div');
  private readonly rail = document.createElement('aside');
  private readonly railList = document.createElement('div');
  private readonly count = document.createElement('span');
  private readonly empty = document.createElement('div');
  private readonly paneHost = document.createElement('section');
  private readonly status = document.createElement('p');
  private readonly panes = new Map<string, ScionTerminalPane>();
  private readonly entries = new Map<string, RailEntry>();
  private registryUnsubscribe: (() => void) | null = null;
  private registry: TerminalSessionRegistry | null = null;
  private currentPath = '/terminals';
  private selected: string | null = null;

  constructor(user: User | null = null) {
    this.element.id = 'terminal-workspace';
    this.element.hidden = true;
    this.element.style.cssText = 'height:100vh;min-height:0;display:none;flex-direction:column';
    this.element.className = 'terminal-workspace-root';

    this.installStyles();
    this.header.user = user;
    this.header.currentPath = this.currentPath;
    this.header.pageTitle = 'Terminals';
    this.header.showMobileMenu = false;

    this.shell.className = 'terminal-workspace-shell';
    this.rail.className = 'terminal-rail';
    this.rail.setAttribute('aria-label', 'Open terminal sessions');
    const railHeader = document.createElement('div');
    railHeader.className = 'terminal-rail-header';
    const title = document.createElement('h2');
    title.textContent = 'Open terminals';
    this.count.className = 'terminal-count';
    railHeader.append(title, this.count);
    this.railList.className = 'terminal-rail-list';
    this.railList.setAttribute('role', 'listbox');
    this.railList.setAttribute('aria-label', 'Retained terminal sessions');
    this.railList.addEventListener('keydown', (event) => this.handleRailKeydown(event));
    this.empty.className = 'terminal-empty';
    this.empty.textContent = 'No terminals are open.';
    this.rail.append(railHeader, this.railList);

    this.paneHost.className = 'terminal-pane-host';
    this.status.className = 'terminal-status';
    this.status.textContent = 'No terminal selected.';
    this.paneHost.append(this.empty, this.status);
    this.shell.append(this.rail, this.paneHost);
    this.element.append(this.header, this.shell);
    this.refresh();
  }

  setUser(user: User | null): void {
    this.header.user = user;
  }

  setCurrentPath(path: string): void {
    this.currentPath = path;
    this.header.currentPath = path;
  }

  create(registry: TerminalSessionRegistry, agentId: string): TerminalSession {
    this.bindRegistry(registry);
    const pane = document.createElement('scion-terminal-pane');
    pane.className = 'terminal-pane';
    pane.style.cssText = 'height:100%;width:100%;min-height:0';
    pane.setVisible(false);
    this.paneHost.appendChild(pane);
    try {
      const session = pane.open(registry, agentId);
      this.panes.set(session.state.key, pane);
      return session;
    } catch (error) {
      pane.remove();
      throw error;
    }
  }

  select(session: TerminalSession): void {
    const pane = this.panes.get(session.state.key);
    if (!pane) throw new Error('Terminal session has no retained pane.');
    this.selected = session.state.key;
    this.status.textContent = '';
    this.show(true);
    this.refresh();
  }

  setStatus(message: string): void {
    if (this.selected) return;
    this.status.textContent = message;
    this.refresh();
  }

  show(visible: boolean): void {
    this.element.hidden = !visible;
    this.element.style.display = visible ? 'flex' : 'none';
    for (const [key, pane] of this.panes) pane.setVisible(visible && key === this.selected);
  }

  private bindRegistry(registry: TerminalSessionRegistry): void {
    if (this.registry === registry) return;
    this.registryUnsubscribe?.();
    this.registry = registry;
    this.registryUnsubscribe = registry.subscribe((sessions) => this.syncSessions(sessions));
  }

  private syncSessions(sessions: readonly TerminalSession[]): void {
    const retained = new Set(sessions.map((session) => session.state.key));
    for (const [key, entry] of this.entries) {
      if (retained.has(key)) continue;
      entry.unsubscribeState();
      entry.unsubscribeMetadata();
      this.entries.delete(key);
      const pane = this.panes.get(key);
      pane?.remove();
      this.panes.delete(key);
      if (this.selected === key) this.selected = null;
    }
    for (const session of sessions) {
      if (this.entries.has(session.state.key)) continue;
      const state = session.state;
      const metadata = this.registry!.metadata.get(state.agentId) ?? {
        agent: state.agent,
        availability: 'loading' as const,
        error: null,
      };
      const entry: RailEntry = {
        session,
        state,
        metadata,
        unsubscribeState: () => {},
        unsubscribeMetadata: () => {},
      };
      entry.unsubscribeState = session.subscribe((next) => {
        entry.state = next;
        this.refresh();
      });
      entry.unsubscribeMetadata = this.registry!.metadata.subscribe(state.agentId, (next) => {
        entry.metadata = next;
        this.refresh();
      });
      this.entries.set(session.state.key, entry);
    }
    if (!this.selected && sessions.length > 0)
      this.selected = sessions[sessions.length - 1].state.key;
    this.publishCount();
    this.refresh();
  }

  private refresh(): void {
    const entries = [...this.entries.values()];
    const total = entries.length;
    const focusedLabel =
      document.activeElement instanceof HTMLElement &&
      this.railList.contains(document.activeElement)
        ? document.activeElement.getAttribute('aria-label')
        : null;
    this.count.textContent = String(total);
    this.empty.hidden = total > 0;
    this.status.hidden = total > 0 && !!this.selected;
    this.railList.replaceChildren(...entries.map((entry) => this.renderRailEntry(entry)));
    if (focusedLabel) {
      this.railList
        .querySelector<HTMLElement>(`[aria-label="${CSS.escape(focusedLabel)}"]`)
        ?.focus();
    }
    for (const [key, pane] of this.panes) {
      pane.setVisible(!this.element.hidden && key === this.selected);
    }
    this.publishCount();
  }

  private renderRailEntry(entry: RailEntry): HTMLElement {
    const metadata = entry.metadata;
    const agent = metadata.agent ?? entry.state.agent;
    const agentName = agent?.name || entry.state.agentId;
    const projectId = agent?.projectId || 'Unknown project';
    const item = document.createElement('div');
    item.className = 'terminal-rail-item';
    item.setAttribute('role', 'option');
    item.setAttribute('aria-selected', String(entry.state.key === this.selected));
    item.dataset.connection = entry.state.connection;
    item.dataset.availability = metadata.availability;

    const select = document.createElement('button');
    select.type = 'button';
    select.className = 'terminal-rail-select';
    select.setAttribute('aria-label', `Show terminal for ${agentName} in ${projectId}`);
    select.addEventListener('click', () => this.openSessionRoute(entry));

    const connection = document.createElement('span');
    connection.className = 'terminal-connection-dot';
    connection.title = connectionLabel(entry.state.connection);
    connection.setAttribute('aria-hidden', 'true');
    const text = document.createElement('span');
    text.className = 'terminal-rail-text';
    const name = document.createElement('span');
    name.className = 'terminal-agent-name';
    name.textContent = agentName;
    const project = document.createElement('span');
    project.className = 'terminal-project-name';
    project.textContent = projectId;
    const details = document.createElement('span');
    details.className = 'terminal-state-label';
    details.textContent = `${connectionLabel(entry.state.connection)} · ${availabilityLabel(
      metadata.availability
    )}`;
    text.append(name, project, details);
    select.append(connection, text);

    const actions = document.createElement('span');
    actions.className = 'terminal-rail-actions';
    const reconnect = document.createElement('button');
    reconnect.type = 'button';
    reconnect.className = 'terminal-icon-action';
    reconnect.setAttribute('aria-label', `Reconnect ${agentName}`);
    reconnect.title = 'Reconnect';
    reconnect.innerHTML = '<sl-icon name="arrow-clockwise"></sl-icon>';
    reconnect.disabled =
      entry.state.connection === 'connected' || entry.state.connection === 'closed';
    reconnect.addEventListener('click', (event) => {
      event.stopPropagation();
      void this.registry?.metadata.refresh(entry.state.agentId);
      void entry.session.connect();
    });
    const close = document.createElement('button');
    close.type = 'button';
    close.className = 'terminal-icon-action';
    close.setAttribute('aria-label', `Close ${agentName}`);
    close.title = 'Close';
    close.innerHTML = '<sl-icon name="x-circle"></sl-icon>';
    close.addEventListener('click', (event) => {
      event.stopPropagation();
      entry.session.close();
    });
    actions.append(reconnect, close);
    item.append(select, actions);
    return item;
  }

  private openSessionRoute(entry: RailEntry): void {
    this.dispatchNavigation(`/terminals/${entry.state.agentId}`);
  }

  private dispatchNavigation(path: string): void {
    this.element.dispatchEvent(
      new CustomEvent('nav-click', {
        detail: { path },
        bubbles: true,
        composed: true,
      })
    );
  }

  private handleRailKeydown(event: KeyboardEvent): void {
    if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
    const buttons = [...this.railList.querySelectorAll<HTMLButtonElement>('.terminal-rail-select')];
    if (!buttons.length) return;
    event.preventDefault();
    const active = document.activeElement;
    const current = active instanceof HTMLButtonElement ? buttons.indexOf(active) : -1;
    let next = 0;
    if (event.key === 'End') next = buttons.length - 1;
    else if (event.key === 'ArrowDown') next = Math.min(buttons.length - 1, current + 1);
    else if (event.key === 'ArrowUp') next = current <= 0 ? 0 : current - 1;
    buttons[next]?.focus();
  }

  private publishCount(): void {
    window.dispatchEvent(
      new CustomEvent<TerminalSessionCountDetail>(TERMINAL_SESSION_COUNT_EVENT, {
        detail: { count: this.entries.size },
      })
    );
  }

  private installStyles(): void {
    const style = document.createElement('style');
    style.textContent = `
      #terminal-workspace {
        background: var(--scion-bg, #f8fafc);
        color: var(--scion-text, #1e293b);
      }
      .terminal-workspace-shell {
        flex: 1;
        min-height: 0;
        display: grid;
        grid-template-columns: minmax(220px, 280px) minmax(0, 1fr);
      }
      .terminal-rail {
        min-width: 0;
        border-right: 1px solid var(--scion-border, #e2e8f0);
        background: var(--scion-surface, #fff);
        display: flex;
        flex-direction: column;
      }
      .terminal-rail-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        padding: 0.875rem 1rem;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
      }
      .terminal-rail-header h2 {
        margin: 0;
        font-size: 0.875rem;
        font-weight: 650;
      }
      .terminal-count {
        min-width: 1.5rem;
        text-align: center;
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }
      .terminal-rail-list {
        flex: 1;
        min-height: 0;
        overflow: auto;
        padding: 0.375rem;
      }
      .terminal-rail-item {
        display: grid;
        grid-template-columns: minmax(0, 1fr) auto;
        align-items: center;
        gap: 0.25rem;
        border-radius: 6px;
      }
      .terminal-rail-item[aria-selected='true'] {
        background: color-mix(in srgb, var(--scion-primary, #3b82f6) 10%, transparent);
      }
      .terminal-rail-select {
        min-width: 0;
        display: flex;
        align-items: flex-start;
        gap: 0.625rem;
        border: 0;
        background: transparent;
        color: inherit;
        text-align: left;
        padding: 0.625rem;
        cursor: pointer;
      }
      .terminal-rail-select:hover,
      .terminal-rail-select:focus-visible {
        outline: none;
        background: var(--scion-bg-subtle, #f1f5f9);
        border-radius: 6px;
      }
      .terminal-connection-dot {
        width: 0.625rem;
        height: 0.625rem;
        border-radius: 50%;
        margin-top: 0.25rem;
        background: #94a3b8;
        flex: 0 0 auto;
      }
      .terminal-rail-item[data-connection='connected'] .terminal-connection-dot {
        background: #22c55e;
      }
      .terminal-rail-item[data-connection='disconnected'] .terminal-connection-dot,
      .terminal-rail-item[data-availability='deleted'] .terminal-connection-dot,
      .terminal-rail-item[data-availability='unavailable'] .terminal-connection-dot {
        background: #ef4444;
      }
      .terminal-rail-item[data-connection='loading'] .terminal-connection-dot,
      .terminal-rail-item[data-connection='connecting'] .terminal-connection-dot {
        background: #f59e0b;
      }
      .terminal-rail-text {
        min-width: 0;
        display: flex;
        flex-direction: column;
        gap: 0.125rem;
      }
      .terminal-agent-name,
      .terminal-project-name,
      .terminal-state-label {
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      .terminal-agent-name {
        font-size: 0.875rem;
        font-weight: 600;
      }
      .terminal-project-name,
      .terminal-state-label {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }
      .terminal-rail-actions {
        display: inline-flex;
        align-items: center;
        gap: 0.125rem;
        padding-right: 0.375rem;
      }
      .terminal-icon-action {
        width: 1.875rem;
        height: 1.875rem;
        display: inline-flex;
        align-items: center;
        justify-content: center;
        border: 0;
        background: transparent;
        color: var(--scion-text-muted, #64748b);
        border-radius: 4px;
        cursor: pointer;
      }
      .terminal-icon-action:hover,
      .terminal-icon-action:focus-visible {
        color: var(--scion-text, #1e293b);
        background: var(--scion-bg-subtle, #f1f5f9);
        outline: none;
      }
      .terminal-icon-action:disabled {
        cursor: default;
        opacity: 0.35;
      }
      .terminal-pane-host {
        position: relative;
        min-width: 0;
        min-height: 0;
        display: flex;
        flex-direction: column;
        background: #111827;
      }
      .terminal-pane {
        flex: 1;
        min-height: 0;
      }
      .terminal-empty,
      .terminal-status {
        position: absolute;
        inset: 0;
        display: flex;
        align-items: center;
        justify-content: center;
        margin: 0;
        padding: 2rem;
        color: var(--scion-text-muted, #64748b);
        background: var(--scion-bg, #f8fafc);
        text-align: center;
      }
      #terminal-workspace [hidden] {
        display: none !important;
      }
      @media (max-width: 760px) {
        .terminal-workspace-shell {
          grid-template-columns: 1fr;
          grid-template-rows: minmax(9rem, 35vh) minmax(0, 1fr);
        }
        .terminal-rail {
          border-right: 0;
          border-bottom: 1px solid var(--scion-border, #e2e8f0);
        }
      }
    `;
    this.element.appendChild(style);
  }
}

function connectionLabel(state: TerminalConnectionState): string {
  switch (state) {
    case 'loading':
      return 'Pending';
    case 'connecting':
      return 'Connecting';
    case 'connected':
      return 'Connected';
    case 'disconnected':
      return 'Disconnected';
    case 'unavailable':
      return 'Unavailable';
    case 'closed':
      return 'Closed';
  }
}

function availabilityLabel(availability: TerminalAgentMetadata['availability']): string {
  switch (availability) {
    case 'loading':
      return 'metadata pending';
    case 'ready':
      return 'agent available';
    case 'deleted':
      return 'agent deleted';
    case 'unavailable':
      return 'metadata unavailable';
  }
}
