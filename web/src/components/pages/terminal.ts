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
 * Terminal page component
 *
 * Full-screen xterm.js terminal that connects to an agent's tmux session
 * via WebSocket proxy through Koa to the Hub PTY endpoint.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Agent, AgentPhase, AgentActivity } from '../../shared/types.js';
import { isTerminalAvailable } from '../../shared/types.js';
import { extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { SSEClient } from '../../client/sse-client.js';
import type { SSEUpdateEvent } from '../../client/sse-client.js';
import type { StatusType } from '../shared/status-badge.js';
import '../shared/status-badge.js';

// xterm.js imports are client-side only — guarded by typeof check in lifecycle
// These will be imported dynamically in firstUpdated() since they require DOM APIs
type Terminal = import('@xterm/xterm').Terminal;
type FitAddon = import('@xterm/addon-fit').FitAddon;
type ClipboardAddon = import('@xterm/addon-clipboard').ClipboardAddon;

/** PTY WebSocket message types */
interface PTYDataMessage {
  type: 'data';
  data: string; // base64
}

interface PTYResizeMessage {
  type: 'resize';
  cols: number;
  rows: number;
}

type PTYMessage = PTYDataMessage | PTYResizeMessage;

/** Which tmux window is active */
type TmuxWindow = 'agent' | 'shell';

@customElement('scion-page-terminal')
export class ScionPageTerminal extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @property({ type: String })
  agentId = '';

  @state()
  private connected = false;

  @state()
  private error: string | null = null;

  @state()
  private agentName = '';

  @state()
  private projectId = '';

  @state()
  private loading = true;

  @state()
  private activeWindow: TmuxWindow = 'agent';

  @state()
  private agentPhase: AgentPhase = 'created';

  @state()
  private agentActivity: AgentActivity | '' = '';

  private terminal: Terminal | null = null;
  private fitAddon: FitAddon | null = null;
  private clipboardAddon: ClipboardAddon | null = null;
  private socket: WebSocket | null = null;
  private resizeObserver: ResizeObserver | null = null;
  private resizeTimer: ReturnType<typeof setTimeout> | null = null;
  private sseClient: SSEClient | null = null;
  private sseUpdateHandler: ((e: CustomEvent<SSEUpdateEvent>) => void) | null = null;

  static override styles = css`
    :host {
      display: flex;
      flex-direction: column;
      flex: 1;
      min-height: 0;
      background: #1a1a1a;
      color: #eaeaea;
      overflow: hidden;
    }

    .toolbar {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.5rem 1rem;
      background: #141414;
      border-bottom: 1px solid #2a2a2a;
      flex-shrink: 0;
      min-height: 40px;
    }

    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      color: #94a3b8;
      text-decoration: none;
      font-size: 0.8125rem;
      white-space: nowrap;
    }

    .back-link:hover {
      color: #60a5fa;
    }

    .separator {
      width: 1px;
      height: 20px;
      background: #2a2a2a;
    }

    .agent-name {
      font-size: 0.875rem;
      font-weight: 500;
      color: #eaeaea;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .spacer {
      flex: 1;
    }

    .status-indicator {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      font-size: 0.75rem;
      color: #94a3b8;
    }

    .status-dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      background: #ef4444;
    }

    .status-dot.connected {
      background: #22c55e;
    }

    .reconnect-btn {
      background: transparent;
      border: 1px solid #2a2a2a;
      color: #94a3b8;
      padding: 0.25rem 0.75rem;
      border-radius: 4px;
      cursor: pointer;
      font-size: 0.75rem;
    }

    .reconnect-btn:hover {
      border-color: #60a5fa;
      color: #60a5fa;
    }

    /* Window switcher toggle group: two rectangular icon buttons */
    .toggle-group {
      display: inline-flex;
      border: 1px solid #2a2a2a;
      border-radius: 4px;
      overflow: hidden;
    }

    .toggle-group button {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      background: transparent;
      border: none;
      color: #555;
      width: 44px;
      height: 32px;
      cursor: pointer;
      line-height: 1;
      padding: 0;
      transition: color 0.15s, background 0.15s;
    }

    .toggle-group button:first-child {
      border-right: 1px solid #2a2a2a;
    }

    .toggle-group button:hover {
      color: #94a3b8;
      background: #1e1e1e;
    }

    .toggle-group button.active {
      color: #22c55e;
      background: #1a2e1a;
    }

    .toggle-group button:disabled {
      cursor: default;
      opacity: 0.4;
    }

    .terminal-container {
      flex: 1;
      position: relative;
      overflow: hidden;
    }

    .loading-state,
    .error-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      flex: 1;
      padding: 2rem;
      text-align: center;
    }

    .loading-state p {
      color: #94a3b8;
      margin-top: 1rem;
    }

    .spinner {
      width: 32px;
      height: 32px;
      border: 3px solid #2a2a2a;
      border-top-color: #60a5fa;
      border-radius: 50%;
      animation: spin 0.8s linear infinite;
    }

    @keyframes spin {
      to {
        transform: rotate(360deg);
      }
    }

    .error-state p {
      color: #ef4444;
      margin: 0 0 1rem 0;
    }

    .error-state .error-detail {
      color: #94a3b8;
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }

    .error-state button {
      background: #3b82f6;
      color: #fff;
      border: none;
      padding: 0.5rem 1.5rem;
      border-radius: 6px;
      cursor: pointer;
      font-size: 0.875rem;
    }

    .error-state button:hover {
      background: #2563eb;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    // SSR property bindings (.agentId=) aren't restored during client-side
    // hydration for top-level page components. Fall back to URL parsing.
    if (!this.agentId && typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/agents\/([^/]+)/);
      if (match) {
        this.agentId = match[1];
      }
    }
    void this.loadAgentInfo();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.cleanup();
  }

  private async loadAgentInfo(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const response = await fetch(`/api/v1/agents/${this.agentId}`, {
        credentials: 'include',
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      const agent = (await response.json()) as Agent;
      this.agentName = agent.name;
      this.projectId = agent.projectId ?? '';
      this.agentPhase = agent.phase;
      this.agentActivity = agent.activity ?? '';
      dispatchPageTitle(this, 'Terminal', agent.name || this.agentId);
      this.connectSSE();

      if (!isTerminalAvailable(agent)) {
        this.error = agent.activity === 'offline'
          ? 'Agent is offline. Terminal is not available while the agent is unreachable.'
          : `Agent phase is ${agent.phase}. Terminal is not available until the agent has started.`;
        this.loading = false;
        return;
      }

      this.loading = false;

      // Wait for render, then initialize terminal
      await this.updateComplete;
      await this.initTerminal();
      this.connectWebSocket();
    } catch (err) {
      console.error('Failed to load agent:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load agent';
      this.loading = false;
    }
  }

  private get agentDisplayStatus(): string {
    if (this.agentPhase === 'running' && this.agentActivity) {
      return this.agentActivity;
    }
    return this.agentPhase;
  }

  private connectSSE(): void {
    this.disconnectSSE();
    const client = new SSEClient();
    this.sseClient = client;
    this.sseUpdateHandler = (e: CustomEvent<SSEUpdateEvent>) => {
      const { subject, data } = e.detail;
      // Only handle events for this agent
      if (!subject.startsWith(`agent.${this.agentId}.`)) return;
      const delta = data as Partial<Agent>;
      if (delta.phase) this.agentPhase = delta.phase;
      if (delta.activity !== undefined) this.agentActivity = delta.activity ?? '';
    };
    client.addEventListener('update', this.sseUpdateHandler);
    client.connect([`agent.${this.agentId}.>`]);
  }

  private disconnectSSE(): void {
    if (this.sseClient) {
      if (this.sseUpdateHandler) {
        this.sseClient.removeEventListener(
          'update',
          this.sseUpdateHandler as EventListenerOrEventListenerObject
        );
        this.sseUpdateHandler = null;
      }
      this.sseClient.disconnect();
      this.sseClient = null;
    }
  }

  private async initTerminal(): Promise<void> {
    // Dynamic import — xterm.js requires DOM APIs not available during SSR
    const [{ Terminal }, { FitAddon }, { WebLinksAddon }, { ClipboardAddon }] = await Promise.all([
      import('@xterm/xterm'),
      import('@xterm/addon-fit'),
      import('@xterm/addon-web-links'),
      import('@xterm/addon-clipboard'),
    ]);

    const container = this.shadowRoot?.querySelector('.terminal-container') as HTMLElement;
    if (!container) return;

    this.terminal = new Terminal({
      theme: {
        background: '#1a1a1a',
        foreground: '#eaeaea',
        cursor: '#f39c12',
        cursorAccent: '#1a1a1a',
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
      fontFamily: "'JetBrains Mono', 'Fira Code', 'Cascadia Code', monospace",
      fontSize: 14,
      cursorBlink: true,
      cursorStyle: 'block',
      // Keep tmux mouse mode enabled for wheel/pane interactions while still
      // allowing browser-native text selection with Option-drag on macOS.
      macOptionClickForcesSelection: true,
      allowProposedApi: true,
    });

    this.fitAddon = new FitAddon();
    this.terminal.loadAddon(this.fitAddon);
    this.terminal.loadAddon(new WebLinksAddon());

    // ClipboardAddon handles OSC 52 sequences from tmux for clipboard relay
    this.clipboardAddon = new ClipboardAddon();
    this.terminal.loadAddon(this.clipboardAddon);

    // Inject xterm.css into shadow root
    const xtermStyle = document.createElement('style');
    // We need to fetch and inject xterm CSS since it can't penetrate shadow DOM
    try {
      const cssModule = await import('@xterm/xterm/css/xterm.css?inline');
      xtermStyle.textContent = cssModule.default;
    } catch {
      // Fallback: try to find xterm CSS in bundled assets
      console.warn('[Terminal] Could not load xterm CSS inline, terminal may not render correctly');
    }
    this.shadowRoot?.appendChild(xtermStyle);

    this.terminal.open(container);
    this.enableShiftSelectionOnMac();

    // Detect active tmux window from OSC 7337 sequence sent by the broker
    // on connect. Format: \033]7337;tmuxwindow=<name>\007
    this.terminal.parser.registerOscHandler(7337, (data: string) => {
      const match = data.match(/^tmuxwindow=(.+)$/);
      if (match) {
        const name = match[1];
        if (name === 'agent' || name === 'shell') {
          this.activeWindow = name as TmuxWindow;
        }
      }
      return true;
    });

    // Defer initial fit until browser has completed layout so the container
    // has its final dimensions (below the toolbar).
    await new Promise((resolve) => requestAnimationFrame(resolve));
    this.fitAddon.fit();

    // Clipboard key bindings & CSI u extended keys — xterm.js inside Shadow DOM
    // needs explicit handling for these since it doesn't natively emit CSI u
    // sequences for modified keys.
    this.terminal.attachCustomKeyEventHandler((event: KeyboardEvent) => {
      // Shift+Enter: send ESC CR (\x1b\r) so that inner applications
      // (e.g. claude-code) can distinguish it from plain Enter.
      // This matches what native terminals send for Alt+Enter / Alt+Shift+Enter.
      if (event.key === 'Enter' && event.shiftKey && !event.ctrlKey && !event.altKey && !event.metaKey) {
        if (event.type === 'keydown') {
          console.debug('[Terminal] Shift+Enter detected, sending ESC CR');
          this.sendData('\x1b\r');
        }
        // Suppress both keydown and keypress to prevent xterm.js
        // from also sending a plain \r on the keypress event.
        return false;
      }

      const isMod = event.ctrlKey || event.metaKey;

      // Ctrl/Cmd+C: copy selection if present, otherwise send SIGINT
      if (event.type === 'keydown' && event.key === 'c' && isMod && !event.shiftKey) {
        if (this.terminal?.hasSelection()) {
          void navigator.clipboard.writeText(this.terminal.getSelection());
          return false; // prevent sending to PTY
        }
        return true; // no selection → send SIGINT
      }

      // Ctrl/Cmd+V: paste from clipboard
      // preventDefault() stops the browser from also firing a native paste
      // event, which xterm would pick up separately — causing a double-paste.
      if (event.type === 'keydown' && event.key === 'v' && isMod && !event.shiftKey) {
        event.preventDefault();
        void navigator.clipboard.readText().then((text) => {
          if (text) this.sendData(text);
        });
        return false;
      }

      // Ctrl+Shift+C: always copy
      if (event.type === 'keydown' && event.key === 'C' && event.ctrlKey && event.shiftKey) {
        if (this.terminal?.hasSelection()) {
          void navigator.clipboard.writeText(this.terminal.getSelection());
        }
        return false;
      }

      // Ctrl+Shift+V: always paste
      if (event.type === 'keydown' && event.key === 'V' && event.ctrlKey && event.shiftKey) {
        event.preventDefault();
        void navigator.clipboard.readText().then((text) => {
          if (text) this.sendData(text);
        });
        return false;
      }

      return true;
    });

    // Handle terminal input
    this.terminal.onData((data: string) => {
      this.sendData(data);
    });

    this.terminal.onBinary((data: string) => {
      this.sendData(data);
    });

    // Handle terminal resize — fit immediately for visual feedback,
    // debounce the WebSocket resize message to avoid flooding tmux
    this.resizeObserver = new ResizeObserver(() => {
      if (this.fitAddon) {
        this.fitAddon.fit();
        if (this.resizeTimer) clearTimeout(this.resizeTimer);
        this.resizeTimer = setTimeout(() => this.sendResize(), 100);
      }
    });
    this.resizeObserver.observe(container);
  }

  /**
   * xterm.js only treats Option as the force-selection modifier on macOS.
   * Patch the instantiated selection service so Shift-drag also bypasses
   * tmux mouse reporting and starts terminal selection.
   */
  private enableShiftSelectionOnMac(): void {
    if (typeof navigator === 'undefined') return;
    const isMac = /Mac|iPhone|iPad|iPod/.test(navigator.platform);
    if (!isMac || !this.terminal) return;

    const selectionService = (this.terminal as Terminal & {
      _core?: { _selectionService?: { shouldForceSelection?: (event: MouseEvent) => boolean } };
    })._core?._selectionService;
    if (!selectionService?.shouldForceSelection) return;

    const originalShouldForceSelection = selectionService.shouldForceSelection.bind(selectionService);
    selectionService.shouldForceSelection = (event: MouseEvent): boolean => {
      return event.shiftKey || originalShouldForceSelection(event);
    };
  }

  private connectWebSocket(): void {
    if (!this.terminal) return;

    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const url = `${protocol}//${window.location.host}/api/v1/agents/${this.agentId}/pty?cols=${this.terminal.cols}&rows=${this.terminal.rows}`;

    console.debug('[Terminal] Connecting to', url);
    this.socket = new WebSocket(url);

    this.socket.onopen = () => {
      console.debug('[Terminal] WebSocket connected');
      this.connected = true;
      this.error = null;
      // Re-fit now that the connection is live so tmux gets accurate dimensions
      if (this.fitAddon) {
        this.fitAddon.fit();
        this.sendResize();
      }
      this.terminal?.focus();
    };

    this.socket.onmessage = (event: MessageEvent) => {
      try {
        const raw = event.data;
        if (typeof raw !== 'string') {
          console.warn('[Terminal] Received non-string message frame (binary/Blob), type:', typeof raw, raw);
          return;
        }
        const msg = JSON.parse(raw) as PTYMessage;
        if (msg.type === 'data') {
          const bytes = Uint8Array.from(atob(msg.data), (c) => c.charCodeAt(0));
          this.terminal?.write(bytes);
        }
      } catch (err) {
        console.warn('[Terminal] Failed to parse WebSocket message:', err, event.data);
      }
    };

    this.socket.onclose = (event: CloseEvent) => {
      console.debug('[Terminal] WebSocket closed, code:', event.code, 'reason:', event.reason);
      this.connected = false;
      if (event.code !== 1000) {
        this.error = `Connection closed (code: ${event.code})`;
      }
    };

    this.socket.onerror = (event) => {
      console.error('[Terminal] WebSocket error:', event);
      this.connected = false;
      this.error = 'WebSocket connection error';
    };
  }

  private sendData(data: string): void {
    if (this.socket?.readyState !== WebSocket.OPEN) return;

    // Encode to base64 — handle Unicode properly
    const bytes = new TextEncoder().encode(data);
    const base64 = btoa(String.fromCharCode(...bytes));

    const msg: PTYDataMessage = { type: 'data', data: base64 };
    this.socket.send(JSON.stringify(msg));
  }

  private sendResize(): void {
    if (this.socket?.readyState !== WebSocket.OPEN || !this.terminal) return;

    const msg: PTYResizeMessage = {
      type: 'resize',
      cols: this.terminal.cols,
      rows: this.terminal.rows,
    };
    this.socket.send(JSON.stringify(msg));
  }

  /**
   * Sends a tmux detach sequence (Ctrl-B d) so the tmux client exits cleanly
   * instead of being killed, which would tear down the container.
   */
  private sendTmuxDetach(): void {
    if (this.socket?.readyState === WebSocket.OPEN) {
      // tmux default prefix is Ctrl-B (0x02), detach key is 'd'
      this.sendData('\x02d');
    }
  }

  private cleanup(): void {
    this.sendTmuxDetach();
    this.disconnectSSE();
    if (this.socket) {
      this.socket.close(1000, 'detach');
      this.socket = null;
    }
    if (this.terminal) {
      this.terminal.dispose();
      this.terminal = null;
    }
    if (this.resizeObserver) {
      this.resizeObserver.disconnect();
      this.resizeObserver = null;
    }
    if (this.resizeTimer) {
      clearTimeout(this.resizeTimer);
      this.resizeTimer = null;
    }
    this.fitAddon = null;
    this.clipboardAddon = null;
  }

  /**
   * Switch to the "agent" tmux window via prefix key binding (Ctrl-B A).
   */
  private switchToAgent(): void {
    if (this.socket?.readyState !== WebSocket.OPEN) return;
    this.sendData('\x02A');
    this.activeWindow = 'agent';
    this.terminal?.focus();
  }

  /**
   * Switch to the "shell" tmux window via prefix key binding (Ctrl-B S).
   * The binding in .tmux.conf handles creating the window if it was closed.
   */
  private switchToShell(): void {
    if (this.socket?.readyState !== WebSocket.OPEN) return;
    this.sendData('\x02S');
    this.activeWindow = 'shell';
    this.terminal?.focus();
  }

  private handleReconnect(): void {
    this.cleanup();
    void this.loadAgentInfo();
  }

  // --- SVG icon helpers ---

  /** Robot icon (agent) */
  private renderRobotIcon() {
    return html`<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="10" rx="2"/><circle cx="12" cy="5" r="2"/><line x1="12" y1="7" x2="12" y2="11"/><line x1="8" y1="16" x2="8" y2="16" stroke-width="3" stroke-linecap="round"/><line x1="16" y1="16" x2="16" y2="16" stroke-width="3" stroke-linecap="round"/></svg>`;
  }

  /** Terminal/shell icon */
  private renderTerminalIcon() {
    return html`<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="4 17 10 11 4 5"/><line x1="12" y1="19" x2="20" y2="19"/></svg>`;
  }

  override render() {
    if (this.loading) {
      return html`
        <div class="toolbar">
          ${this.projectId ? html`<a href="/projects/${this.projectId}" class="back-link">&larr; Back to Project</a>` : ''}
          <a href="/agents/${this.agentId}" class="back-link">
            &larr; Back to Agent
          </a>
        </div>
        <div class="loading-state">
          <div class="spinner"></div>
          <p>Connecting to agent...</p>
        </div>
      `;
    }

    if (this.error && !this.terminal) {
      return html`
        <div class="toolbar">
          ${this.projectId ? html`<a href="/projects/${this.projectId}" class="back-link">&larr; Back to Project</a>` : ''}
          <a href="/agents/${this.agentId}" class="back-link">
            &larr; Back to Agent
          </a>
          ${this.agentName
            ? html`
                <div class="separator"></div>
                <span class="agent-name">${this.agentName}</span>
              `
            : ''}
        </div>
        <div class="error-state">
          <p>Terminal Unavailable</p>
          <div class="error-detail">${this.error}</div>
          <button @click=${() => this.handleReconnect()}>Retry</button>
        </div>
      `;
    }

    return html`
      <div class="toolbar">
        ${this.projectId ? html`<a href="/projects/${this.projectId}" class="back-link">&larr; Back to Project</a>` : ''}
        <a href="/agents/${this.agentId}" class="back-link">
          &larr; Back to Agent
        </a>
        <div class="separator"></div>
        <span class="agent-name">${this.agentName || this.agentId}</span>
        <div class="toggle-group" title="Switch between agent and shell tmux windows">
          <button
            class=${this.activeWindow === 'agent' ? 'active' : ''}
            title="Agent window"
            @click=${() => this.switchToAgent()}
            ?disabled=${!this.connected}
          >${this.renderRobotIcon()}</button>
          <button
            class=${this.activeWindow === 'shell' ? 'active' : ''}
            title="Shell window"
            @click=${() => this.switchToShell()}
            ?disabled=${!this.connected}
          >${this.renderTerminalIcon()}</button>
        </div>
        <div class="spacer"></div>
        <scion-status-badge
          status=${this.agentDisplayStatus as StatusType}
          size="small"
        ></scion-status-badge>
        <div class="status-indicator">
          <span class="status-dot ${this.connected ? 'connected' : ''}"></span>
          ${this.connected ? 'Connected' : 'Disconnected'}
        </div>
        ${!this.connected
          ? html`
              <button class="reconnect-btn" @click=${() => this.handleReconnect()}>
                Reconnect
              </button>
            `
          : ''}
      </div>
      ${this.error
        ? html`
            <div
              style="padding: 0.375rem 1rem; background: #7f1d1d; color: #fecaca; font-size: 0.75rem;"
            >
              ${this.error}
            </div>
          `
        : ''}
      <div class="terminal-container"></div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-terminal': ScionPageTerminal;
  }
}
