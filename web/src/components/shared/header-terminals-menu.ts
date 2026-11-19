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
 * Lazy-loaded menu item rendering, route current-agent resolution, and
 * keyboard/selection handlers for the wide-tier Terminals split-button
 * dropdown in `<scion-header>` (ptone/scion#8284).
 *
 * Kept out of `header.ts`'s static import graph so `src/client/main.ts` stays
 * within its initial entry-closure gzip budget.
 */

import { html, nothing, type TemplateResult } from 'lit';

import { CHAT_SPACE_ROUTE, dmPeerFromKey } from '../../client/chat-routes.js';
import { openTerminal } from '../../client/open-terminal.js';
import { stateManager } from '../../client/state.js';
import {
  CURRENT_AGENT_CHANGED_EVENT,
  TERMINAL_DROPDOWN_SELECT_EVENT,
  getAgentPageCurrentAgent,
  getChatCurrentAgent,
  type CurrentAgentInfo,
  type TerminalDropdownSelectDetail,
  type TerminalSessionDot,
  type TerminalSessionSummary,
} from '../../client/terminal-workspace-events.js';

export interface TerminalsMenuRenderContext {
  readonly currentPath: string;
  readonly userId: string;
  readonly sessions: readonly TerminalSessionSummary[];
  readonly focusedAgentId: string | null;
}

export interface TerminalDropdownRow {
  readonly agentId: string;
  readonly name: string;
  readonly dot: TerminalSessionDot | null;
}

const MENU_STYLES_ATTR = 'data-terminals-menu-styles';
const MENU_CSS = `
  .terminals-menu {
    min-width: 14rem;
    max-width: 22rem;
  }
  .terminal-row-dot {
    width: 0.5rem;
    height: 0.5rem;
    border-radius: 50%;
    background: #94a3b8;
    display: inline-block;
    flex-shrink: 0;
  }
  .terminal-row-dot[data-dot='green'] {
    background: #22c55e;
  }
  .terminal-row-dot[data-dot='red'] {
    background: #ef4444;
  }
  .terminal-row-dot[data-dot='amber'] {
    background: #f59e0b;
  }
  .terminal-row-dot[data-dot='neutral'] {
    background: transparent;
    box-shadow: inset 0 0 0 2px var(--scion-text-muted, #64748b);
  }
  .terminal-row-dot.placeholder {
    background: transparent;
    box-shadow: inset 0 0 0 1.5px currentColor;
    opacity: 0.35;
  }
  .terminal-row-name {
    display: inline-block;
    max-width: 13rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    vertical-align: bottom;
  }
  .terminal-row-status {
    margin-left: 0.75rem;
    font-size: 0.75rem;
    color: var(--sl-color-neutral-500, var(--scion-text-muted, #64748b));
    white-space: nowrap;
  }
  .terminal-row-status.not-open {
    font-style: italic;
  }
`;

export function ensureTerminalsMenuStyles(root: ShadowRoot | null): void {
  if (!root || root.querySelector(`style[${MENU_STYLES_ATTR}]`)) return;
  const style = document.createElement('style');
  style.setAttribute(MENU_STYLES_ATTR, '');
  style.textContent = MENU_CSS;
  root.appendChild(style);
}

/**
 * Subscribes to current-agent and agent-store updates that can change row
 * labels or ordering in the Terminals dropdown.
 */
export function subscribeCurrentAgentChanges(onChange: () => void): () => void {
  window.addEventListener(CURRENT_AGENT_CHANGED_EVENT, onChange);
  stateManager.addEventListener('agents-changed', onChange);
  return (): void => {
    window.removeEventListener(CURRENT_AGENT_CHANGED_EVENT, onChange);
    stateManager.removeEventListener('agents-changed', onChange);
  };
}

function lookupAgentName(agentId: string, sessions: readonly TerminalSessionSummary[]): string {
  return (
    sessions.find((s) => s.agentId === agentId)?.name ||
    stateManager.getAgent(agentId)?.name ||
    agentId
  );
}

function decodeSegment(raw: string): string {
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

export function resolveCurrentAgentForHeader(
  ctx: TerminalsMenuRenderContext
): CurrentAgentInfo | null {
  const rawPath = ctx.currentPath || window.location.pathname;
  const pathname = rawPath.split(/[?#]/, 1)[0].replace(/\/+$/, '') || '/';

  if (pathname === '/terminals' || pathname.startsWith('/terminals/')) {
    const id = ctx.focusedAgentId;
    return id ? { agentId: id, name: lookupAgentName(id, ctx.sessions) } : null;
  }

  if (pathname === '/chat' || pathname.startsWith('/chat/')) {
    if (pathname === '/chat' || CHAT_SPACE_ROUTE.test(pathname)) return null;
    const dmMatch = pathname.match(/^\/chat\/dm\/([^/]+)$/);
    if (dmMatch) {
      const key = decodeSegment(dmMatch[1]);
      if (key.startsWith('dm:')) {
        const peer = dmPeerFromKey(key, ctx.userId);
        if (!peer || peer.peerKind !== 'agent') return null;
        const chatAgent = getChatCurrentAgent();
        return {
          agentId: peer.peerId,
          name:
            chatAgent?.agentId === peer.peerId
              ? chatAgent.name
              : lookupAgentName(peer.peerId, ctx.sessions),
        };
      }
      return getChatCurrentAgent();
    }
    return getChatCurrentAgent();
  }

  const agentMatch = pathname.match(/^\/agents\/([^/]+)(?:\/edit)?$/);
  if (agentMatch && agentMatch[1] !== 'new' && agentMatch[1] !== 'graph') {
    const agentId = decodeSegment(agentMatch[1]);
    const pageAgent = getAgentPageCurrentAgent();
    return {
      agentId,
      name:
        pageAgent?.agentId === agentId ? pageAgent.name : lookupAgentName(agentId, ctx.sessions),
    };
  }

  return null;
}

export function buildTerminalDropdownRows(ctx: TerminalsMenuRenderContext): TerminalDropdownRow[] {
  const current = resolveCurrentAgentForHeader(ctx);
  const openCurrent = current ? ctx.sessions.find((s) => s.agentId === current.agentId) : undefined;
  const rows: TerminalDropdownRow[] = current
    ? [openCurrent ?? { agentId: current.agentId, name: current.name, dot: null }]
    : [];
  for (const session of ctx.sessions) {
    if (session.agentId !== current?.agentId) rows.push(session);
  }
  return rows;
}

export function renderTerminalsMenuItems(
  ctx: TerminalsMenuRenderContext
): TemplateResult | TemplateResult[] {
  const rows = buildTerminalDropdownRows(ctx);
  if (rows.length === 0) {
    return html`<sl-menu-item class="terminals-empty-item" disabled
      >No open terminals</sl-menu-item
    >`;
  }
  return rows.map((row) => {
    const status = row.dot ? row.dot.meaning : 'not open';
    return html`<sl-menu-item
      value=${row.agentId}
      tabindex="0"
      aria-label=${`${row.name}, ${status}`}
    >
      <span
        slot="prefix"
        class="terminal-row-dot ${row.dot ? '' : 'placeholder'}"
        data-dot=${row.dot?.colour ?? nothing}
        aria-hidden="true"
      ></span>
      <span class="terminal-row-name">${row.name}</span>
      <span
        slot="suffix"
        class="terminal-row-status ${row.dot ? '' : 'not-open'}"
        aria-hidden="true"
        >${status}</span
      >
    </sl-menu-item>`;
  });
}

function menuItems(root: ShadowRoot | null): HTMLElement[] {
  return Array.from(
    root?.querySelectorAll<HTMLElement>(
      '.wide-center .terminals-menu sl-menu-item:not([disabled])'
    ) ?? []
  );
}

function selectTerminalFromDropdown(agentId: string): void {
  openTerminal(agentId);
  queueMicrotask(() =>
    window.dispatchEvent(
      new CustomEvent<TerminalDropdownSelectDetail>(TERMINAL_DROPDOWN_SELECT_EVENT, {
        detail: { agentId },
      })
    )
  );
}

export function onTerminalsTriggerClick(
  e: MouseEvent,
  setOpen: (open: boolean) => void,
  currentOpen: boolean
): void {
  const dd = (e.currentTarget as HTMLElement).closest('sl-dropdown') as { show?: unknown } | null;
  if (!dd?.show) setOpen(!currentOpen);
}

export function onTerminalsTriggerKey(
  e: KeyboardEvent,
  root: ShadowRoot | null,
  setOpen: (open: boolean) => void
): void {
  const dd = (e.currentTarget as HTMLElement).closest('sl-dropdown') as
    | (HTMLElement & { open?: boolean; show?: unknown; updateComplete?: Promise<boolean> })
    | null;
  if (dd?.show) {
    if (e.key === 'Enter' || e.key === ' ') {
      queueMicrotask(
        () => void dd.updateComplete?.then(() => dd.open && menuItems(root)[0]?.focus())
      );
    }
    return;
  }
  if (['ArrowDown', 'Home', 'Enter', ' '].includes(e.key)) {
    e.preventDefault();
    setOpen(true);
    menuItems(root)[0]?.focus();
  } else if (e.key === 'ArrowUp' || e.key === 'End') {
    e.preventDefault();
    setOpen(true);
    menuItems(root).at(-1)?.focus();
  }
}

export function onTerminalsMenuKey(
  e: KeyboardEvent,
  root: ShadowRoot | null,
  setOpen: (open: boolean) => void
): void {
  const dd = (e.currentTarget as HTMLElement).closest('sl-dropdown');
  if ((dd as { show?: unknown } | null)?.show) return;
  if (e.key === 'Escape') {
    e.preventDefault();
    e.stopPropagation();
    setOpen(false);
    dd?.querySelector<HTMLElement>('.terminals-menu-trigger')?.focus();
    return;
  }
  const items = menuItems(root);
  const active = root?.activeElement as HTMLElement | null;
  const idx = active ? items.indexOf(active) : -1;
  if (!items.length) return;
  if (e.key === 'ArrowDown') {
    e.preventDefault();
    items[(idx + 1) % items.length]?.focus();
  } else if (e.key === 'ArrowUp') {
    e.preventDefault();
    items[idx <= 0 ? items.length - 1 : idx - 1]?.focus();
  } else if (e.key === 'Home') {
    e.preventDefault();
    items[0]?.focus();
  } else if (e.key === 'End') {
    e.preventDefault();
    items.at(-1)?.focus();
  } else if ((e.key === 'Enter' || e.key === ' ') && active && idx >= 0) {
    e.preventDefault();
    setOpen(false);
    const id = active.getAttribute('value');
    if (id) selectTerminalFromDropdown(id);
  }
}

export function onTerminalsMenuSelect(
  e: CustomEvent<{ item: HTMLElement }>,
  setOpen: (open: boolean) => void
): void {
  const item = e.detail?.item;
  const id = item && !item.hasAttribute('disabled') ? item.getAttribute('value') : null;
  if (!id) return;
  setOpen(false);
  selectTerminalFromDropdown(id);
}
