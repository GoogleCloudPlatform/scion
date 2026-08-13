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

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type {
  PageData,
  Agent,
  AgentPhase,
  AgentActivity,
  ExposedPort,
} from '../../shared/types.js';
import { isTerminalAvailable } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { SSEClient } from '../../client/sse-client.js';
import type { SSEUpdateEvent } from '../../client/sse-client.js';
import type { StatusType } from '../shared/status-badge.js';
import '../shared/status-badge.js';
import { showToast } from '../../utils/toast.js';

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
  private wasConnected = false;

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

  @state()
  private agent: Agent | null = null;

  @state()
  private exposedPorts: ExposedPort[] = [];

  @state()
  private captureAuthLoading = false;

  @state()
  private captureAuthConflicts: string[] | null = null;

  // --- Drag-and-drop file upload state ---
  @state() private uploadEnabled = false;
  @state() private uploadDisabledReason = '';
  @state() private uploadTargetDir = ''; // shared dir name (e.g. "scratchpad")
  @state() private uploadBasePath = ''; // container path (e.g. "/scion-volumes/scratchpad")
  @state() private isDragOver = false;
  @state() private isUploading = false;
  @state() private uploadStatus = ''; // progress/error message in overlay

  private terminal: Terminal | null = null;
  private fitAddon: FitAddon | null = null;
  private clipboardAddon: ClipboardAddon | null = null;
  private socket: WebSocket | null = null;
  private resizeObserver: ResizeObserver | null = null;
  private resizeTimer: ReturnType<typeof setTimeout> | null = null;
  private sseClient: SSEClient | null = null;
  private sseUpdateHandler: ((e: CustomEvent<SSEUpdateEvent>) => void) | null = null;
  private _dragCounter = 0;
  private _errorTimer: ReturnType<typeof setTimeout> | null = null;
  private _windowDragOver: ((e: DragEvent) => void) | null = null;
  private _windowDrop: ((e: DragEvent) => void) | null = null;

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

    .capture-auth-btn {
      background: transparent;
      border: 1px solid #2a2a2a;
      color: #f59e0b;
      padding: 0.25rem 0.75rem;
      border-radius: 4px;
      cursor: pointer;
      font-size: 0.75rem;
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
    }

    .capture-auth-btn:hover {
      border-color: #f59e0b;
      background: rgba(245, 158, 11, 0.1);
    }

    .capture-auth-btn:disabled {
      opacity: 0.5;
      cursor: default;
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
      transition:
        color 0.15s,
        background 0.15s;
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

    .terminal-wrapper {
      flex: 1;
      position: relative;
      overflow: hidden;
    }

    .terminal-container {
      position: absolute;
      top: 0;
      left: 0;
      right: 0;
      bottom: 0;
    }

    .disconnected-overlay {
      position: absolute;
      top: 0;
      left: 0;
      right: 0;
      bottom: 0;
      background: rgba(0, 0, 0, 0.5);
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      z-index: 10;
      pointer-events: none;
    }

    .disconnected-overlay .overlay-text {
      color: #ef4444;
      font-size: 2rem;
      font-weight: 700;
      letter-spacing: 0.15em;
      text-shadow: 0 2px 8px rgba(0, 0, 0, 0.6);
    }

    .drop-overlay {
      position: absolute;
      top: 0;
      left: 0;
      right: 0;
      bottom: 0;
      background: rgba(0, 0, 0, 0.6);
      display: none;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      gap: 0.75rem;
      z-index: 11;
      pointer-events: none;
      border: 2px dashed transparent;
      font-size: 1rem;
      color: #94a3b8;
    }

    .drop-overlay.visible {
      display: flex;
    }

    .drop-overlay.visible:not(.disabled) {
      border-color: #60a5fa;
    }

    .drop-overlay.disabled {
      border-color: #ef4444;
      color: #ef4444;
    }

    .drop-overlay sl-spinner {
      font-size: 1.5rem;
      --indicator-color: #60a5fa;
    }

    .drop-overlay sl-icon {
      font-size: 2rem;
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

    /* Port forwarding buttons */
    .port-btn {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      background: transparent;
      border: 1px solid #2a5d2a;
      color: #4ade80;
      padding: 0.25rem 0.75rem;
      border-radius: 4px;
      font-size: 0.75rem;
      text-decoration: none;
      white-space: nowrap;
      transition:
        border-color 0.15s,
        background 0.15s;
      animation: port-appear 0.3s ease-out;
      cursor: pointer;
    }

    .port-btn:hover {
      border-color: #22c55e;
      background: rgba(34, 197, 94, 0.1);
      color: #22c55e;
    }

    @keyframes port-appear {
      from {
        opacity: 0;
        transform: scale(0.9);
      }
      to {
        opacity: 1;
        transform: scale(1);
      }
    }

    /* Port dropdown for 4+ ports */
    .port-dropdown {
      position: relative;
      display: inline-flex;
    }

    .port-dropdown-trigger {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      background: transparent;
      border: 1px solid #2a5d2a;
      color: #4ade80;
      padding: 0.25rem 0.75rem;
      border-radius: 4px;
      font-size: 0.75rem;
      cursor: pointer;
      white-space: nowrap;
    }

    .port-dropdown-trigger:hover {
      border-color: #22c55e;
      background: rgba(34, 197, 94, 0.1);
      color: #22c55e;
    }

    .port-dropdown-menu {
      display: none;
      position: absolute;
      top: 100%;
      right: 0;
      margin-top: 4px;
      background: var(--card-bg, #1a1a2e);
      border: 1px solid var(--border-color, #333);
      border-radius: 6px;
      padding: 0.25rem 0;
      min-width: 180px;
      z-index: 100;
      box-shadow: 0 4px 12px rgba(0, 0, 0, 0.3);
    }

    .port-dropdown.open .port-dropdown-menu {
      display: block;
    }

    .port-dropdown-menu a {
      display: block;
      padding: 0.5rem 0.75rem;
      color: #4ade80;
      text-decoration: none;
      font-size: 0.8rem;
      white-space: nowrap;
    }

    .port-dropdown-menu a:hover {
      background: rgba(34, 197, 94, 0.1);
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

    // Prevent the browser from navigating to a dropped file (which would
    // destroy the terminal session). Must be on window, not the drop target,
    // to catch near-miss drops outside the wrapper.
    this._windowDragOver = (e: DragEvent) => {
      e.preventDefault();
    };
    this._windowDrop = (e: DragEvent) => {
      e.preventDefault();
    };
    window.addEventListener('dragover', this._windowDragOver);
    window.addEventListener('drop', this._windowDrop);

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
        throw new Error(
          await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`)
        );
      }

      const agent = (await response.json()) as Agent;
      this.agent = agent;
      this.agentName = agent.name;
      this.projectId = agent.projectId ?? '';
      this.agentPhase = agent.phase;
      this.agentActivity = agent.activity ?? '';
      this.exposedPorts = agent.exposedPorts ?? [];
      dispatchPageTitle(this, 'Terminal', agent.name || this.agentId);
      this.connectSSE();

      // Resolve upload target shared dir (best-effort, non-blocking for terminal init)
      if (this.projectId) {
        void this.resolveUploadTarget();
      }

      if (!isTerminalAvailable(agent)) {
        this.error =
          agent.activity === 'offline'
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
      if (subject.endsWith('.ports')) {
        const portsData = data as { ports: ExposedPort[] };
        this.exposedPorts = portsData.ports ?? [];
        return;
      }
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
      if (
        event.key === 'Enter' &&
        event.shiftKey &&
        !event.ctrlKey &&
        !event.altKey &&
        !event.metaKey
      ) {
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

    const selectionService = (
      this.terminal as Terminal & {
        _core?: { _selectionService?: { shouldForceSelection?: (event: MouseEvent) => boolean } };
      }
    )._core?._selectionService;
    if (!selectionService?.shouldForceSelection) return;

    const originalShouldForceSelection =
      selectionService.shouldForceSelection.bind(selectionService);
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
      this.wasConnected = true;
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
          console.warn(
            '[Terminal] Received non-string message frame (binary/Blob), type:',
            typeof raw,
            raw
          );
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

  // --- Drag-and-drop file upload ---

  private async resolveUploadTarget(): Promise<void> {
    try {
      const resp = await apiFetch(`/api/v1/projects/${this.projectId}/shared-dirs`);
      if (!resp.ok) {
        this.uploadEnabled = false;
        this.uploadDisabledReason = 'Could not determine shared directories for file upload';
        return;
      }
      const data = await resp.json();
      const dirs = (data.sharedDirs ?? []) as Array<{
        name: string;
        read_only?: boolean;
        in_workspace?: boolean;
      }>;
      // Filter: writable, non-in_workspace
      const candidates = dirs.filter((d) => !d.read_only && !d.in_workspace);
      const target = candidates.find((d) => d.name === 'scratchpad') || candidates[0];
      if (target) {
        this.uploadEnabled = true;
        this.uploadTargetDir = target.name;
        this.uploadBasePath = `/scion-volumes/${target.name}`;
      } else {
        this.uploadEnabled = false;
        this.uploadDisabledReason = 'No writable shared directory available for file upload';
      }
    } catch {
      this.uploadEnabled = false;
      this.uploadDisabledReason = 'Could not determine shared directories for file upload';
    }
  }

  private _onDragEnter(e: DragEvent): void {
    e.preventDefault();
    this._dragCounter++;
    if (this._dragCounter === 1) {
      if (this._errorTimer) {
        clearTimeout(this._errorTimer);
        this._errorTimer = null;
        this.uploadStatus = '';
      }
      this.isDragOver = true;
    }
  }

  private _onDragLeave(_e: DragEvent): void {
    this._dragCounter = Math.max(0, this._dragCounter - 1);
    if (this._dragCounter === 0) this.isDragOver = false;
  }

  private _onDragOver(e: DragEvent): void {
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = this.uploadEnabled ? 'copy' : 'none';
  }

  private async _onDrop(e: DragEvent): Promise<void> {
    e.preventDefault();
    this._dragCounter = 0;
    this.isDragOver = false;
    if (!this.uploadEnabled || !e.dataTransfer?.files.length) return;
    await this._handleFileDrop(e.dataTransfer.files);
  }

  private async _handleFileDrop(files: FileList): Promise<void> {
    // Client-side size validation
    const MAX_FILE = 50 * 1024 * 1024; // 50MB
    const MAX_TOTAL = 100 * 1024 * 1024; // 100MB
    let total = 0;
    for (const f of files) {
      if (f.size > MAX_FILE) {
        this._showUploadError(`File "${f.name}" exceeds 50MB limit`);
        return;
      }
      total += f.size;
    }
    if (total > MAX_TOTAL) {
      this._showUploadError('Total upload exceeds 100MB limit');
      return;
    }

    this.isUploading = true;
    const batchId =
      typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function'
        ? crypto.randomUUID()
        : Math.random().toString(36).substring(2, 15) + Date.now().toString(36);
    const formData = new FormData();
    const paths: string[] = [];

    for (const file of files) {
      const relPath = `.attachments/_web/${batchId}/${file.name}`;
      formData.append(relPath, file);
      paths.push(`${this.uploadBasePath}/.attachments/_web/${batchId}/${file.name}`);
    }

    this.uploadStatus = `Uploading ${files.length} file${files.length > 1 ? 's' : ''}...`;

    try {
      const resp = await apiFetch(
        `/api/v1/projects/${this.projectId}/shared-dirs/${this.uploadTargetDir}/files`,
        { method: 'POST', body: formData }
      );
      if (!resp.ok) {
        if (resp.status === 409) {
          this._showUploadError('File upload requires a co-located runtime broker');
          this.uploadEnabled = false;
          this.uploadDisabledReason = 'File upload requires a co-located runtime broker';
          return;
        }
        const err = await extractApiError(resp, 'Upload failed');
        this._showUploadError(err);
        return;
      }

      // Inject paths into terminal
      const quoted = paths.map((p) => this._quoteForShell(p));
      this.sendData(quoted.join(' ') + ' ');
      this.terminal?.focus();
    } catch {
      this._showUploadError('Upload failed: network error');
    } finally {
      // Only clear on success — error paths use _showUploadError which manages its own state
      if (this.isUploading) {
        this.isUploading = false;
        this.uploadStatus = '';
      }
    }
  }

  private _quoteForShell(path: string): string {
    if (/^[A-Za-z0-9._\/-]+$/.test(path)) return path;
    return "'" + path.replace(/'/g, "'\\''") + "'";
  }

  private _showUploadError(msg: string): void {
    if (this._errorTimer) clearTimeout(this._errorTimer);
    this.uploadStatus = msg;
    this.isUploading = false;
    this.isDragOver = true;
    this._errorTimer = setTimeout(() => {
      this.uploadStatus = '';
      this.isDragOver = false;
      this._errorTimer = null;
    }, 4000);
  }

  private cleanup(): void {
    this.sendTmuxDetach();
    this.disconnectSSE();
    if (this._windowDragOver) {
      window.removeEventListener('dragover', this._windowDragOver);
      this._windowDragOver = null;
    }
    if (this._windowDrop) {
      window.removeEventListener('drop', this._windowDrop);
      this._windowDrop = null;
    }
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
    if (this._errorTimer) {
      clearTimeout(this._errorTimer);
      this._errorTimer = null;
    }
    this.fitAddon = null;
    this.clipboardAddon = null;
    this.wasConnected = false;
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

  private renderPortButtons() {
    if (this.exposedPorts.length === 0) return nothing;

    if (this.exposedPorts.length <= 3) {
      return this.exposedPorts.map(
        (p) => html`
          <a
            class="port-btn"
            href="/api/v1/agents/${this.agentId}/ports/${p.port}/proxy/"
            target="_blank"
            rel="noopener"
            title=${p.label || `Port ${p.port}`}
          >
            Open :${p.port}
          </a>
        `
      );
    }

    // 4+ ports: dropdown
    return html`
      <div class="port-dropdown">
        <button
          class="port-dropdown-trigger"
          @click=${(e: Event) => {
            e.stopPropagation();
            const el = (e.currentTarget as HTMLElement).parentElement!;
            const isOpen = el.classList.toggle('open');
            if (isOpen) {
              const close = () => {
                el.classList.remove('open');
                document.removeEventListener('click', close);
              };
              // Use setTimeout to avoid the current click triggering the close
              setTimeout(() => document.addEventListener('click', close), 0);
            }
          }}
        >
          Ports (${this.exposedPorts.length}) ▾
        </button>
        <div class="port-dropdown-menu">
          ${this.exposedPorts.map(
            (p) => html`
              <a
                href="/api/v1/agents/${this.agentId}/ports/${p.port}/proxy/"
                target="_blank"
                rel="noopener"
              >
                :${p.port}${p.label ? ` — ${p.label}` : ''}
              </a>
            `
          )}
        </div>
      </div>
    `;
  }

  private get showCaptureAuth(): boolean {
    const agent = this.agent;
    if (!agent) return false;
    if (agent.phase !== 'running') return false;
    const isNoAuth = agent.appliedConfig?.noAuth === true || agent.harnessAuth === 'none';
    return isNoAuth && !!agent.resolvedHarness;
  }

  private static readonly SECRET_CONFLICT_RE = /secret "([^"]+)" already exists/g;

  private async handleCaptureAuth(force = false): Promise<void> {
    if (!this.agent) return;
    this.captureAuthLoading = true;
    this.captureAuthConflicts = null;
    try {
      const command = ['python3', '/home/scion/.scion/harness/capture_auth.py'];
      if (force) command.push('--force');

      const response = await apiFetch(`/api/v1/agents/${this.agent.id}/exec`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ command, timeout: 60 }),
      });

      if (!response.ok) {
        const msg = await extractApiError(response, 'Failed to run capture auth');
        showToast(msg);
        return;
      }

      const result = (await response.json()) as { output: string; exitCode: number };

      if (result.exitCode === 0) {
        showToast('Credentials captured successfully.', 'success');
        if (result.output) console.log('Capture auth output:', result.output);
        await this.refreshAgentData();
      } else if (result.exitCode === 2) {
        showToast('No credentials found yet. Authenticate first, then try again.', 'neutral');
        if (result.output) console.log('Capture auth output:', result.output);
      } else {
        const conflicts: string[] = [];
        for (const m of result.output.matchAll(ScionPageTerminal.SECRET_CONFLICT_RE)) {
          conflicts.push(m[1]);
        }
        if (conflicts.length > 0) {
          this.captureAuthConflicts = conflicts;
        } else {
          showToast(`Capture failed (exit ${result.exitCode}).`);
          if (result.output) console.log('Capture auth output:', result.output);
        }
      }
    } catch (err) {
      console.error('Failed to capture auth:', err);
      showToast(err instanceof Error ? err.message : 'Failed to capture auth');
    } finally {
      this.captureAuthLoading = false;
    }
  }

  private renderCaptureAuthConflictDialog() {
    if (!this.captureAuthConflicts) return nothing;
    const secrets = this.captureAuthConflicts;
    const label =
      secrets.length === 1
        ? `Secret "${secrets[0]}" already exists`
        : `${secrets.length} secrets already exist`;
    return html`
      <sl-dialog
        label=${label}
        open
        @sl-request-close=${() => {
          if (!this.captureAuthLoading) this.captureAuthConflicts = null;
        }}
      >
        <p>
          The following secret${secrets.length > 1 ? 's' : ''} already
          exist${secrets.length === 1 ? 's' : ''}:
        </p>
        <ul>
          ${secrets.map((s) => html`<li><code>${s}</code></li>`)}
        </ul>
        <p>Do you want to force-update ${secrets.length > 1 ? 'them' : 'it'}?</p>
        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.captureAuthLoading}
          @click=${() => {
            this.captureAuthConflicts = null;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="warning"
          ?loading=${this.captureAuthLoading}
          @click=${() => void this.handleCaptureAuth(true)}
          >Force Update</sl-button
        >
      </sl-dialog>
    `;
  }

  private async refreshAgentData(): Promise<void> {
    try {
      const response = await apiFetch(`/api/v1/agents/${this.agentId}`);
      if (!response.ok) return;

      const agent = (await response.json()) as Agent;
      this.agent = agent;
      this.agentPhase = agent.phase;
      this.agentActivity = agent.activity ?? '';
    } catch (err) {
      console.warn('Failed to refresh agent data:', err);
    }
  }

  private handleReconnect(): void {
    this.cleanup();
    void this.loadAgentInfo();
  }

  // --- SVG icon helpers ---

  /** Robot icon (agent) */
  private renderRobotIcon() {
    return html`<svg
      width="18"
      height="18"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      stroke-linecap="round"
      stroke-linejoin="round"
    >
      <rect x="3" y="11" width="18" height="10" rx="2" />
      <circle cx="12" cy="5" r="2" />
      <line x1="12" y1="7" x2="12" y2="11" />
      <line x1="8" y1="16" x2="8" y2="16" stroke-width="3" stroke-linecap="round" />
      <line x1="16" y1="16" x2="16" y2="16" stroke-width="3" stroke-linecap="round" />
    </svg>`;
  }

  /** Terminal/shell icon */
  private renderTerminalIcon() {
    return html`<svg
      width="18"
      height="18"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="2"
      stroke-linecap="round"
      stroke-linejoin="round"
    >
      <polyline points="4 17 10 11 4 5" />
      <line x1="12" y1="19" x2="20" y2="19" />
    </svg>`;
  }

  override render() {
    if (this.loading) {
      return html`
        <div class="toolbar">
          ${this.projectId
            ? html`<a href="/projects/${this.projectId}" class="back-link"
                >&larr; Back to Project</a
              >`
            : ''}
          <a href="/agents/${this.agentId}" class="back-link"> &larr; Back to Agent </a>
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
          ${this.projectId
            ? html`<a href="/projects/${this.projectId}" class="back-link"
                >&larr; Back to Project</a
              >`
            : ''}
          <a href="/agents/${this.agentId}" class="back-link"> &larr; Back to Agent </a>
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
        ${this.projectId
          ? html`<a href="/projects/${this.projectId}" class="back-link">&larr; Back to Project</a>`
          : ''}
        <a href="/agents/${this.agentId}" class="back-link"> &larr; Back to Agent </a>
        <div class="separator"></div>
        <span class="agent-name">${this.agentName || this.agentId}</span>
        <div class="toggle-group" title="Switch between agent and shell tmux windows">
          <button
            class=${this.activeWindow === 'agent' ? 'active' : ''}
            title="Agent window"
            @click=${() => this.switchToAgent()}
            ?disabled=${!this.connected}
          >
            ${this.renderRobotIcon()}
          </button>
          <button
            class=${this.activeWindow === 'shell' ? 'active' : ''}
            title="Shell window"
            @click=${() => this.switchToShell()}
            ?disabled=${!this.connected}
          >
            ${this.renderTerminalIcon()}
          </button>
        </div>
        <div class="spacer"></div>
        ${this.renderPortButtons()}
        ${this.showCaptureAuth
          ? html`
              <button
                class="capture-auth-btn"
                ?disabled=${this.captureAuthLoading}
                @click=${() => this.handleCaptureAuth()}
                title="Capture credentials from inside the container"
              >
                ${this.captureAuthLoading ? 'Capturing...' : 'Capture Auth'}
              </button>
            `
          : ''}
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
      <div
        class="terminal-wrapper"
        @dragenter=${(e: DragEvent) => this._onDragEnter(e)}
        @dragleave=${(e: DragEvent) => this._onDragLeave(e)}
        @dragover=${(e: DragEvent) => this._onDragOver(e)}
        @drop=${(e: DragEvent) => this._onDrop(e)}
      >
        <div class="terminal-container"></div>
        ${!this.connected && this.wasConnected
          ? html`<div class="disconnected-overlay">
              <span class="overlay-text">DISCONNECTED</span>
            </div>`
          : ''}
        <div
          class="drop-overlay ${this.isDragOver || this.uploadStatus ? 'visible' : ''} ${!this
            .uploadEnabled
            ? 'disabled'
            : ''}"
        >
          ${this.isUploading
            ? html`<sl-spinner></sl-spinner><span>${this.uploadStatus}</span>`
            : this.uploadStatus
              ? html`<sl-icon name="x-circle"></sl-icon><span>${this.uploadStatus}</span>`
              : this.uploadEnabled
                ? html`<sl-icon name="cloud-upload"></sl-icon><span>Drop files to upload</span>`
                : html`<sl-icon name="x-circle"></sl-icon
                    ><span>${this.uploadDisabledReason}</span>`}
        </div>
      </div>
      ${this.renderCaptureAuthConflictDialog()}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-terminal': ScionPageTerminal;
  }
}
