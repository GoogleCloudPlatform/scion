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
    return html`<sl-menu-item value=${row.agentId} aria-label=${`${row.name}, ${status}`}>
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

type SlDropdownLike = HTMLElement & {
  open?: boolean;
  show?: () => Promise<void> | void;
  updateComplete?: Promise<boolean>;
  getMenu?: () => { setCurrentItem?: (item: HTMLElement) => void } | undefined;
};

function focusMenuItemAfterOpen(
  dd: SlDropdownLike,
  root: ShadowRoot | null,
  which: 'first' | 'last'
): void {
  const focus = (): void => {
    requestAnimationFrame(() => {
      if (!dd.open) return;
      const items = menuItems(root);
      const target = which === 'last' ? items[items.length - 1] : items[0];
      if (!target) return;
      dd.getMenu?.()?.setCurrentItem?.(target);
      target.focus();
    });
  };
  if (dd.updateComplete) void dd.updateComplete.then(focus);
  else focus();
}

/**
 * Opens a newly-mounted `<sl-dropdown class="terminals-dropdown">`. Focuses the
 * mounted trigger button so replacing the pre-mount placeholder does not drop
 * focus to `<body>`, and when opened via keyboard moves roving focus to the
 * first or last enabled item.
 */
export function openMountedTerminalsDropdown(
  root: ShadowRoot | null,
  focusTarget: 'none' | 'first' | 'last'
): void {
  const dd = root?.querySelector<SlDropdownLike>('.wide-center .terminals-dropdown');
  if (!dd) return;
  const focusTrigger = (): void => {
    root?.querySelector<HTMLElement>('.wide-center .terminals-menu-trigger')?.focus();
  };
  if (typeof dd.show === 'function') {
    const shown = Promise.resolve(dd.show());
    // Wait for `<sl-dropdown>` to render its `<slot name="trigger">` before focusing;
    // focusing an unslotted child of a shadow host is a no-op in Chromium.
    if (dd.updateComplete) {
      void dd.updateComplete.then(() => {
        if (dd.open) focusTrigger();
      });
    }
    void shown.then(() => {
      if (focusTarget !== 'none') focusMenuItemAfterOpen(dd, root, focusTarget);
      else if (dd.open) focusTrigger();
    });
  } else {
    focusTrigger();
    dd.open = true;
    if (focusTarget !== 'none') focusMenuItemAfterOpen(dd, root, focusTarget);
  }
}

/**
 * Shoelace `<sl-dropdown>` opens on `Enter`/`Space` via `handleTriggerClick()`
 * without focusing the first `<sl-menu-item>` (unlike `ArrowDown`/`Home`).
 * Focus the first enabled item once the dropdown finishes opening so keyboard
 * users land directly in the menu on `Enter` or `Space` as well.
 */
export function onTerminalsTriggerKey(e: KeyboardEvent, root: ShadowRoot | null): void {
  if (e.key !== 'Enter' && e.key !== ' ') return;
  const dd = (e.currentTarget as HTMLElement).closest('sl-dropdown') as SlDropdownLike | null;
  if (!dd || dd.open) return;
  queueMicrotask(() => focusMenuItemAfterOpen(dd, root, 'first'));
}

export interface TerminalsDropdownRenderOptions extends TerminalsMenuRenderContext {
  readonly isTerminal: boolean;
  readonly open: boolean;
  readonly root: ShadowRoot | null;
  readonly setOpen: (open: boolean) => void;
}

export function syncTerminalsTriggerAria(root: ShadowRoot | null): void {
  root
    ?.querySelector('.wide-center .terminals-menu-trigger')
    ?.setAttribute('aria-haspopup', 'menu');
}

export function renderMountedTerminalsDropdown(
  opts: TerminalsDropdownRenderOptions
): TemplateResult {
  const onOpenChange = (open: boolean): void => {
    opts.setOpen(open);
    syncTerminalsTriggerAria(opts.root);
  };
  return html`<sl-dropdown
    class="terminals-dropdown"
    placement="bottom-end"
    distance="4"
    @sl-show=${(): void => onOpenChange(true)}
    @sl-hide=${(): void => onOpenChange(false)}
  >
    <button
      type="button"
      slot="trigger"
      class="terminals-menu-trigger ${opts.isTerminal ? 'active' : ''}"
      title="Open terminals menu"
      aria-label="Open terminals menu"
      aria-haspopup="menu"
      aria-expanded=${opts.open ? 'true' : 'false'}
      @keydown=${(e: KeyboardEvent): void => onTerminalsTriggerKey(e, opts.root)}
    >
      <sl-icon name="chevron-down" aria-hidden="true"></sl-icon>
    </button>
    <sl-menu
      class="terminals-menu"
      @sl-select=${(e: CustomEvent<{ item: HTMLElement }>): void =>
        onTerminalsMenuSelect(e, onOpenChange)}
    >
      ${renderTerminalsMenuItems(opts)}
    </sl-menu>
  </sl-dropdown>`;
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
