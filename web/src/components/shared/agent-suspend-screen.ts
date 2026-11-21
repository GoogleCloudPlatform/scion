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
 * Read-only "Last screen" card for a suspended agent (ptone/scion#8530).
 *
 * Fetches GET /api/v1/projects/{projectId}/agents/{agentId}/screen (or the
 * top-level /api/v1/agents/{agentId}/screen fallback) and lazily renders the
 * saved ANSI terminal pane using a read-only @xterm/xterm instance.
 */

import { LitElement, html, css, nothing, type PropertyValues } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { AgentSuspendScreen } from '../../shared/types.js';
import { apiFetch } from '../../client/api.js';
import { visibleInterval } from '../../client/visible-interval.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';

type Terminal = import('@xterm/xterm').Terminal;

const TERMINAL_BACKGROUND = '#1a1a1a';
const TERMINAL_FOREGROUND = '#eaeaea';

export const MAX_SCREEN_ROWS = 60;
export const MIN_SCREEN_COLS = 80;
export const MAX_SCREEN_COLS = 200;

export const NO_SAVED_SCREEN_MESSAGE = 'No screen was saved when this agent was suspended.';

type LoadStatus = 'idle' | 'loading' | 'loaded' | 'empty' | 'error';

/**
 * Builds the hub API path for fetching a saved suspend screen.
 */
export function agentScreenUrl(agentId: string, projectId?: string): string {
  const encodedAgent = encodeURIComponent(agentId);
  if (projectId) {
    return `/api/v1/projects/${encodeURIComponent(projectId)}/agents/${encodedAgent}/screen`;
  }
  return `/api/v1/agents/${encodedAgent}/screen`;
}

const ESC = String.fromCharCode(0x1b);
const BEL = String.fromCharCode(0x07);
// Strip OSC (ESC ] ... BEL/ST), CSI (ESC [ ... final byte), and 2-byte ESC sequences.
const ANSI_PATTERN = new RegExp(
  `${ESC}\\][^${BEL}${ESC}]*(?:${BEL}|${ESC}\\\\)|${ESC}\\[[0-?]*[ -/]*[@-~]|${ESC}[@-Z\\\\-_]`,
  'g'
);

/**
 * Strips ANSI escape sequences from terminal output for width measurement and
 * plain-text fallback rendering.
 */
export function stripAnsi(text: string): string {
  return text.replace(ANSI_PATTERN, '');
}

/**
 * Computes read-only xterm dimensions from saved screen content.
 */
export function computeScreenDimensions(content: string): {
  rows: number;
  cols: number;
  lineCount: number;
} {
  const normalized = content.replace(/\r\n/g, '\n');
  const lines = normalized.split('\n');
  if (lines.length > 1 && lines[lines.length - 1] === '') {
    lines.pop();
  }
  const lineCount = Math.max(lines.length, 1);
  const rows = Math.min(lineCount, MAX_SCREEN_ROWS);

  let maxLineWidth = 0;
  for (const line of lines) {
    const visibleLen = stripAnsi(line).length;
    if (visibleLen > maxLineWidth) {
      maxLineWidth = visibleLen;
    }
  }
  const cols = Math.min(Math.max(maxLineWidth, MIN_SCREEN_COLS), MAX_SCREEN_COLS);
  return { rows, cols, lineCount };
}

@customElement('scion-agent-suspend-screen')
export class ScionAgentSuspendScreen extends LitElement {
  @property({ type: String })
  agentId = '';

  @property({ type: String })
  projectId = '';

  @property({ type: String })
  phase = '';

  @state()
  private status: LoadStatus = 'idle';

  @state()
  private screen: AgentSuspendScreen | null = null;

  @state()
  private xtermFallback = false;

  // Re-render when the user's display-timezone preference changes.
  private readonly displayZone = new DisplayZoneController(this);

  private fetchController: AbortController | null = null;
  private fetchSeq = 0;
  private terminal: Terminal | null = null;
  private xtermStyleInjected = false;
  private stopRelativeTimeTick: (() => void) | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .card-title-row {
      display: flex;
      align-items: center;
      justify-content: space-between;
      flex-wrap: wrap;
      gap: 0.5rem 1rem;
      margin-bottom: 1rem;
      padding-bottom: 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .card-title {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .meta {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .saved-time {
      cursor: help;
      text-decoration: underline dotted;
      text-underline-offset: 2px;
    }

    .loading-row {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .empty-message {
      margin: 0;
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .screen-viewport {
      background: #1a1a1a;
      color: #eaeaea;
      border-radius: var(--scion-radius, 0.5rem);
      border: 1px solid var(--scion-border, #e2e8f0);
      padding: 0.75rem;
      max-height: 28rem;
      overflow: auto;
    }

    .screen-terminal {
      width: fit-content;
      min-width: 100%;
    }

    .screen-fallback {
      margin: 0;
      font-family: 'JetBrains Mono', 'Fira Code', 'Cascadia Code', monospace;
      font-size: 13px;
      line-height: 1.35;
      white-space: pre;
      color: #eaeaea;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    // Keep the relative "Saved … before suspend" text fresh.
    this.stopRelativeTimeTick = visibleInterval(() => this.requestUpdate(), 15000);
    void this.displayZone;
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopRelativeTimeTick?.();
    this.stopRelativeTimeTick = null;
    this.abortFetch();
    this.disposeTerminal();
  }

  protected override willUpdate(changedProperties: PropertyValues): void {
    if (
      changedProperties.has('agentId') ||
      changedProperties.has('projectId') ||
      changedProperties.has('phase')
    ) {
      if (this.phase === 'suspended' && this.agentId) {
        void this.loadScreen();
      } else {
        this.resetState();
      }
    }
  }

  private abortFetch(): void {
    if (this.fetchController) {
      this.fetchController.abort();
      this.fetchController = null;
    }
  }

  private disposeTerminal(): void {
    if (this.terminal) {
      try {
        this.terminal.dispose();
      } catch {
        // Ignore teardown errors during disconnect.
      }
      this.terminal = null;
    }
  }

  private resetState(): void {
    this.fetchSeq += 1;
    this.abortFetch();
    this.disposeTerminal();
    this.screen = null;
    this.xtermFallback = false;
    this.status = 'idle';
  }

  private isStale(seq: number, signal: AbortSignal): boolean {
    return (
      signal.aborted || seq !== this.fetchSeq || !this.isConnected || this.phase !== 'suspended'
    );
  }

  private async loadScreen(): Promise<void> {
    this.fetchSeq += 1;
    const seq = this.fetchSeq;
    this.abortFetch();
    this.disposeTerminal();

    const controller = new AbortController();
    this.fetchController = controller;
    const { signal } = controller;

    this.screen = null;
    this.xtermFallback = false;
    this.status = 'loading';

    try {
      const res = await apiFetch(agentScreenUrl(this.agentId, this.projectId || undefined), {
        signal,
      });

      if (this.isStale(seq, signal)) return;

      if (!res) {
        console.warn('[AgentSuspendScreen] Empty response while fetching saved screen');
        this.status = 'error';
        return;
      }

      if (res.status === 404) {
        this.status = 'empty';
        return;
      }

      if (!res.ok) {
        console.warn('[AgentSuspendScreen] Failed to load saved screen:', res.status);
        this.status = 'error';
        return;
      }

      const data = (await res.json()) as AgentSuspendScreen | null;
      if (this.isStale(seq, signal)) return;

      if (!data || typeof data.content !== 'string' || data.content.length === 0) {
        this.status = 'empty';
        return;
      }

      this.screen = data;
      this.status = 'loaded';

      await this.updateComplete;
      if (this.isStale(seq, signal)) return;

      await this.mountXterm(data.content, seq, signal);
    } catch (err) {
      if (this.isStale(seq, signal)) return;
      if (err instanceof DOMException && err.name === 'AbortError') return;
      console.warn('[AgentSuspendScreen] Error loading saved screen:', err);
      this.status = 'error';
    }
  }

  private async mountXterm(content: string, seq: number, signal: AbortSignal): Promise<void> {
    try {
      const { Terminal } = await import('@xterm/xterm');
      if (this.isStale(seq, signal)) return;

      if (!this.xtermStyleInjected && this.shadowRoot) {
        try {
          const cssModule = await import('@xterm/xterm/css/xterm.css?inline');
          if (this.isStale(seq, signal)) return;
          const styleEl = document.createElement('style');
          styleEl.textContent = cssModule.default;
          this.shadowRoot.appendChild(styleEl);
          this.xtermStyleInjected = true;
        } catch {
          console.warn('[AgentSuspendScreen] Could not load xterm CSS inline');
        }
      }

      const container = this.shadowRoot?.querySelector('.screen-terminal') as HTMLElement | null;
      if (!container) return;

      this.disposeTerminal();
      container.textContent = '';

      const { rows, cols, lineCount } = computeScreenDimensions(content);
      const term = new Terminal({
        disableStdin: true,
        cursorBlink: false,
        convertEol: true,
        rows,
        cols,
        scrollback: Math.max(lineCount, 200),
        fontFamily: "'JetBrains Mono', 'Fira Code', 'Cascadia Code', monospace",
        fontSize: 13,
        theme: {
          background: TERMINAL_BACKGROUND,
          foreground: TERMINAL_FOREGROUND,
          cursor: TERMINAL_BACKGROUND,
          cursorAccent: TERMINAL_BACKGROUND,
          selectionBackground: 'rgba(255, 255, 255, 0.3)',
          black: '#1a1a1a',
          red: '#e74c3c',
          green: '#2ecc71',
          yellow: '#f39c12',
          blue: '#3498db',
          magenta: '#9b59b6',
          cyan: '#1abc9c',
          white: '#eaeaea',
          brightBlack: '#546e7a',
          brightRed: '#e57373',
          brightGreen: '#81c784',
          brightYellow: '#ffd54f',
          brightBlue: '#64b5f6',
          brightMagenta: '#ce93d8',
          brightCyan: '#4dd0e1',
          brightWhite: '#ffffff',
        },
      });

      this.terminal = term;
      term.open(container);
      term.write(content);
    } catch (err) {
      if (this.isStale(seq, signal)) return;
      console.warn(
        '[AgentSuspendScreen] Failed to initialize xterm; falling back to plain text:',
        err
      );
      this.xtermFallback = true;
    }
  }

  private renderMeta() {
    if (this.status !== 'loaded' || !this.screen) return nothing;
    const capturedAt = this.screen.capturedAt;
    const relativeTime = capturedAt ? formatRelative(capturedAt) : '';
    const absoluteTime = capturedAt ? formatInstantWithZone(capturedAt) || capturedAt : '';

    return html`
      <div class="meta">
        ${relativeTime
          ? html`
              <sl-tooltip content=${absoluteTime}>
                <span class="saved-time" data-testid="suspend-screen-saved-at"
                  >Saved ${relativeTime} before suspend</span
                >
              </sl-tooltip>
            `
          : nothing}
        ${this.screen.truncated
          ? html`
              <sl-badge variant="warning" pill data-testid="suspend-screen-truncated"
                >truncated</sl-badge
              >
            `
          : nothing}
      </div>
    `;
  }

  private renderBody() {
    if (this.status === 'loading') {
      return html`
        <div class="loading-row" data-testid="suspend-screen-loading">
          <sl-spinner></sl-spinner>
          <span>Loading last screen…</span>
        </div>
      `;
    }

    if (this.status === 'loaded' && this.screen) {
      if (this.xtermFallback) {
        return html`
          <div class="screen-viewport" data-testid="suspend-screen-viewport">
            <pre class="screen-fallback" data-testid="suspend-screen-fallback">
${stripAnsi(this.screen.content)}</pre
            >
          </div>
        `;
      }
      return html`
        <div class="screen-viewport" data-testid="suspend-screen-viewport">
          <div class="screen-terminal" data-testid="suspend-screen-terminal"></div>
        </div>
      `;
    }

    if (this.status === 'empty' || this.status === 'error') {
      return html`
        <p class="empty-message" data-testid="suspend-screen-empty">${NO_SAVED_SCREEN_MESSAGE}</p>
      `;
    }

    return nothing;
  }

  override render() {
    if (this.phase !== 'suspended' || !this.agentId) {
      return nothing;
    }

    return html`
      <div class="card" data-testid="agent-last-screen-card">
        <div class="card-title-row">
          <h3 class="card-title">Last screen</h3>
          ${this.renderMeta()}
        </div>
        ${this.renderBody()}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-agent-suspend-screen': ScionAgentSuspendScreen;
  }
}
