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
 * Retained terminal pane
 *
 * Full-screen xterm.js terminal that connects to an agent's tmux session
 * via the session registry and Hub PTY endpoint.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import type { Agent, AgentPhase, AgentActivity, ExposedPort } from '../../shared/types.js';
import {
  TerminalSessionRegistry,
  type TerminalSession,
  type TerminalSessionState,
  type TerminalResources,
  type TerminalDisconnectReason,
} from '../../client/terminal-sessions.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import type { TerminalAgentMetadata } from '../../client/terminal-metadata.js';
import type { StatusType } from '../shared/status-badge.js';
import '../shared/status-badge.js';
import { showToast } from '../../utils/toast.js';
import { buildAgentDMKey, chatConversationPath } from '../../client/chat-routes.js';
import { isFeatureEnabled } from '../../utils/feature-flags.js';
import { TERMINAL_DRAG_MIME } from '../../client/terminal-workspace-events.js';

// xterm.js imports are client-side only — guarded by typeof check in lifecycle
// These will be imported dynamically in firstUpdated() since they require DOM APIs
type Terminal = import('@xterm/xterm').Terminal;
type FitAddon = import('@xterm/addon-fit').FitAddon;
// ClipboardAddon replaced with visibility-scoped OSC 52 handler (P1.8)

/** Which tmux window is active */
type TmuxWindow = 'agent' | 'shell';

@customElement('scion-terminal-pane')
export class ScionTerminalPane extends LitElement {
  /** Identity is assigned once by open(), never inferred from the current route. */
  get agentId(): string {
    return this.session?.state.agentId ?? '';
  }

  get session(): TerminalSession | null {
    return this.ownedSession;
  }

  /** User ID for building chat DM keys. Set by workspace root. */
  @state() userId: string = '';

  private registry: TerminalSessionRegistry | null = null;
  private disposed = false;
  private layoutReady: (() => void) | null = null;

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
  private disconnectReason: TerminalDisconnectReason = null;

  /**
   * True whenever `pending` is set (a connect() call is in flight): used only
   * to guard against double-submitting a manual Reconnect click.
   */
  @state()
  private reconnectInProgress = false;

  /**
   * Derived from `connection ∈ {loading, connecting}` rather than
   * `session.reconnecting`. `pending` clears once the WebSocket is
   * constructed, before the handshake finishes, so it under-reports how
   * long an attempt is actually running; connection state does not.
   */
  @state()
  private attempting = false;

  /** A reconnect attempt failed; auto-retry is blocked. */
  @state()
  private reconnectFailed = false;

  /** Whether the failed attempt above was manually triggered. */
  @state()
  private reconnectFailedManual = false;

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

  @state()
  private captureAuthScopeDialogOpen = false;

  /** Remembers the scope chosen in the scope dialog so force-update reuses it. */
  @state()
  private captureAuthSelectedScope: 'project' | 'user' = 'project';

  // --- Drag-and-drop file upload state ---
  @state() private uploadEnabled = false;
  @state() private uploadDisabledReason = '';
  @state() private uploadTargetDir = ''; // shared dir name (e.g. "scratchpad")
  @state() private uploadBasePath = ''; // container path (e.g. "/scion-volumes/scratchpad")
  @state() private isDragOver = false;
  @state() private isUploading = false;
  @state() private uploadStatus = ''; // progress/error message in overlay

  private terminal: Terminal | null = null;
  private terminalStyle: HTMLStyleElement | null = null;
  private fitAddon: FitAddon | null = null;
  private ownedSession: TerminalSession | null = null;
  /** Explicit visibility state — see setVisible(). */
  private _visible = true;
  /**
   * Whether this pane owns user focus for human input.
   * System clipboard (OSC 52, paste) and input injection (upload paths)
   * require BOTH _visible AND _focused. Protocol responses (DSR, DA)
   * are unrestricted.
   *
   * Derived from actual DOM state, never assigned unconditionally:
   * - focusin on this element or descendant → true
   * - focusout to external element or null (window blur) → false
   * - setVisible(false) → false (blur + inert)
   * - setVisible(true) → derived from current document.activeElement
   * - _onDrop → terminal.focus() → focusin → true
   *
   * Default false: the first focusin event (from auto-focus or user click)
   * establishes the correct state.
   */
  private _focused = false;
  private sessionUnsubscribe: (() => void) | null = null;
  private resizeObserver: ResizeObserver | null = null;
  private resizeTimer: ReturnType<typeof setTimeout> | null = null;
  private metadataUnsubscribe: (() => void) | null = null;
  private metadataError: string | null = null;
  private portDropdownClose: (() => void) | null = null;
  private portDropdownTimer: ReturnType<typeof setTimeout> | null = null;
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

    :host([hidden]) {
      display: none;
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

    .reconnect-btn:hover:not(:disabled) {
      border-color: #60a5fa;
      color: #60a5fa;
    }

    .reconnect-btn:disabled {
      opacity: 0.5;
      cursor: default;
    }

    .pane-action-btn {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      background: transparent;
      border: 1px solid #2a2a2a;
      color: #94a3b8;
      width: 32px;
      height: 32px;
      border-radius: 4px;
      cursor: pointer;
      padding: 0;
      line-height: 1;
    }

    .pane-action-btn:hover {
      border-color: #60a5fa;
      color: #60a5fa;
      background: rgba(96, 165, 250, 0.1);
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

    #capture-scope-group {
      margin-top: 0.75rem;
    }

    #capture-scope-group sl-radio {
      display: block;
    }

    #capture-scope-group sl-radio:not(:last-of-type) {
      margin-bottom: 0.5rem;
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
      background: rgba(0, 0, 0, 0.6);
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      gap: 1rem;
      z-index: 10;
      pointer-events: auto;
    }

    .disconnected-overlay .overlay-title {
      color: #ef4444;
      font-size: 1.5rem;
      font-weight: 700;
      letter-spacing: 0.1em;
      text-shadow: 0 2px 8px rgba(0, 0, 0, 0.6);
    }

    .disconnected-overlay.unavailable .overlay-title {
      color: #f59e0b;
    }

    .disconnected-overlay.reconnecting .overlay-title {
      color: #60a5fa;
    }

    .disconnected-overlay sl-spinner {
      font-size: 2rem;
      --track-width: 3px;
    }

    .disconnected-overlay .overlay-detail {
      color: #94a3b8;
      font-size: 0.875rem;
      max-width: 400px;
      text-align: center;
      line-height: 1.5;
    }

    .disconnected-overlay .overlay-reconnect {
      margin-top: 0.5rem;
      background: #3b82f6;
      color: #fff;
      border: none;
      padding: 0.5rem 1.5rem;
      border-radius: 6px;
      cursor: pointer;
      font-size: 0.875rem;
    }

    .disconnected-overlay .overlay-reconnect:hover:not(:disabled) {
      background: #2563eb;
    }

    .disconnected-overlay .overlay-reconnect:disabled {
      opacity: 0.5;
      cursor: default;
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
    if (this.disposed) return;
    // Install global drop prevention only when visible so hidden workspaces
    // do not interfere with Chat/Dashboard file drops. (P1.8)
    if (this._visible) this.installWindowDragPrevention();
    // Track actual DOM focus ownership. focusin/focusout bubble and cover all
    // descendants (toolbar buttons, file picker, xterm textarea). We use the
    // relatedTarget to distinguish focus moving within this pane (toolbar click)
    // from focus leaving entirely (rail/header/sibling click). (P1.8)
    this.addEventListener('focusin', this._onFocusIn);
    this.addEventListener('focusout', this._onFocusOut);
    // "Frontmost" also requires the document itself to be visible (the
    // browser tab is in the foreground), not just this pane's slot in the
    // workspace layout.
    document.addEventListener('visibilitychange', this._onDocumentVisibilityChange);
    this.updateFrontmost();
    void this.reveal();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    // DOM placement is not session lifetime. The retained owner explicitly closes.
    this.removeEventListener('focusin', this._onFocusIn);
    this.removeEventListener('focusout', this._onFocusOut);
    document.removeEventListener('visibilitychange', this._onDocumentVisibilityChange);
    this.session?.setFrontmost(false);
    this.removeWindowListeners();
    this.terminal?.blur();
    this.cancelResize();
  }

  /** Recompute and push the combined frontmost signal. */
  private _onDocumentVisibilityChange = (): void => {
    this.updateFrontmost();
  };

  private updateFrontmost(): void {
    this.session?.setFrontmost(this._visible && document.visibilityState === 'visible');
  }

  /**
   * DOM focus entered this pane or a descendant (terminal textarea, toolbar
   * button, file picker). Set _focused so clipboard/input guards allow
   * human interaction.
   */
  private _onFocusIn = (): void => {
    if (this._visible && !this.disposed) {
      this._focused = true;
      this.dataset.focused = '';
    }
  };

  /**
   * DOM focus left this pane. Only clear _focused if focus actually moved
   * outside — relatedTarget is null (window blur) or outside this element.
   * Focus moving between toolbar/terminal children within this pane keeps
   * _focused true.
   */
  private _onFocusOut = (e: FocusEvent): void => {
    if (this.disposed) return;
    const related = e.relatedTarget as Node | null;
    // Focus moving within this pane (e.g. terminal → toolbar button): keep focused.
    if (related && this.contains(related)) return;
    // Focus moving within Shadow DOM children (relatedTarget may be in shadowRoot):
    if (related && this.shadowRoot?.contains(related)) return;
    this._focused = false;
    delete this.dataset.focused;
  };

  /**
   * Bind once, before or after mounting. Repeated opens on this pane are idempotent.
   * The workspace must retain this element by session key: an existing session
   * cannot acquire a second renderer, and a pane cannot switch agent or registry.
   */
  open(registry: TerminalSessionRegistry, agentId: string): TerminalSession {
    if (this.disposed) throw new Error('Terminal pane is disposed.');
    if (this.session) {
      if (this.registry === registry && this.agentId === agentId.toLowerCase()) return this.session;
      throw new Error('Terminal pane cannot be rebound.');
    }
    if (registry.list().some((session) => session.state.agentId === agentId.toLowerCase())) {
      throw new Error('Terminal session already has a pane; reuse its original element.');
    }
    this.registry = registry;
    this.ownedSession = registry.open(agentId, async (_agent, signal) => {
      this.loading = false;
      await this.updateComplete;
      signal.throwIfAborted();
      try {
        return await this.initTerminal(signal);
      } catch (error) {
        // Covers partial allocation before a failed/aborted layout continuation.
        this.disposeTerminal();
        throw error;
      }
    });
    this.metadataUnsubscribe = registry.metadata.subscribe(this.agentId, (value) =>
      this.applyMetadata(value)
    );
    this.sessionUnsubscribe = this.session!.subscribe((state) => this.applySessionState(state));
    // open() supports bind-after-mount (see the class doc above), and on
    // that path connectedCallback already ran with session == null, so it
    // never pushed a frontmost signal. Push it now that a session exists, so
    // a pane that is already visible arms auto-reconnect immediately instead
    // of waiting for the next visibility change.
    this.updateFrontmost();
    return this.session!;
  }

  /** Presentation only. Output continues to be parsed by the same xterm. */
  setVisible(visible: boolean): void {
    this.hidden = !visible;
    this.inert = !visible;
    this._visible = visible;
    if (visible) {
      // Derive focus from actual DOM state, never assume it.
      // If the terminal or a descendant has focus, _focused is true.
      // Otherwise _focused stays false until a focusin event fires
      // (e.g. from shouldAutoFocusTerminal() → terminal.focus()).
      this._focused =
        this.contains(document.activeElement) ||
        this.shadowRoot?.contains(document.activeElement as Node) ||
        false;
      if (this._focused) this.dataset.focused = '';
      else delete this.dataset.focused;
      // Re-install scoped drop prevention for visible workspace panes.
      if (this.isConnected && !this.disposed) this.installWindowDragPrevention();
      void this.reveal();
    } else {
      this._focused = false;
      delete this.dataset.focused;
      this.terminal?.blur();
      this.cancelResize();
      // Remove drop prevention so Chat/Dashboard drops are unaffected.
      this.removeWindowListeners();
    }
    this.updateFrontmost();
  }

  /** Explicit lifetime boundary. Navigation is reserved for the legacy adapter. */
  dispose(reason: 'explicit' | 'navigation' = 'explicit'): void {
    if (this.disposed) return;
    try {
      this.session?.close(reason);
    } finally {
      this.cleanup();
    }
  }

  private measurable(): boolean {
    const container = this.shadowRoot?.querySelector<HTMLElement>('.terminal-container');
    return (
      !this.disposed &&
      this.isConnected &&
      !this.hidden &&
      !!container &&
      container.clientWidth > 0 &&
      container.clientHeight > 0
    );
  }

  private async reveal(): Promise<void> {
    await this.updateComplete;
    if (!this.isConnected || this.hidden || this.disposed || !this.terminal) return;
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
    if (!this.measurable()) return;
    this.layoutReady?.();
    this.fitAddon?.fit();
    this.terminal?.refresh(0, this.terminal.rows - 1);
    this.sendResize();
  }

  private cancelResize(): void {
    if (this.resizeTimer) clearTimeout(this.resizeTimer);
    this.resizeTimer = null;
  }

  private applySessionState(state: TerminalSessionState): void {
    if (state.connection === 'closed') {
      this.cleanup();
      return;
    }
    if (this.disposed) return;
    const newlyConnected = !this.connected && state.connection === 'connected';
    this.connected = state.connection === 'connected';
    this.error = this.metadataError ?? state.error;
    this.disconnectReason = state.disconnectReason;
    this.reconnectInProgress = this.ownedSession?.reconnecting ?? false;
    this.attempting = state.connection === 'loading' || state.connection === 'connecting';
    this.reconnectFailed = state.reconnectFailed;
    this.reconnectFailedManual = state.reconnectFailedManual;
    if (state.connection !== 'loading') this.loading = false;
    if (newlyConnected) {
      this.wasConnected = true;
      if (this.measurable()) {
        this.fitAddon?.fit();
        this.sendResize();
        if (this.shouldAutoFocusTerminal()) this.terminal?.focus();
      }
    }
  }

  private shouldAutoFocusTerminal(): boolean {
    const active = document.activeElement;
    return !active || active === document.body || active === this || this.contains(active);
  }

  private get agentDisplayStatus(): string {
    if (this.agentPhase === 'running' && this.agentActivity) {
      return this.agentActivity;
    }
    return this.agentPhase;
  }

  /** Metadata is consumed independently of transport/resize snapshots. */
  private applyMetadata(value: TerminalAgentMetadata): void {
    if (this.disposed) return;
    this.metadataError = value.error;
    this.error = value.error ?? this.session?.state.error ?? null;
    const agent = value.agent;
    if (!agent) return;
    const previousProject = this.projectId;
    const previousName = this.agentName;
    this.agent = agent;
    this.agentName = agent.name;
    this.projectId = agent.projectId ?? '';
    this.agentPhase = agent.phase;
    this.agentActivity = agent.activity ?? '';
    this.exposedPorts =
      value.availability === 'deleted' || value.availability === 'unavailable'
        ? []
        : (agent.exposedPorts ?? []);
    if (previousName !== agent.name)
      dispatchPageTitle(this, 'Terminal', agent.name || this.agentId);
    if (this.projectId && previousProject !== this.projectId) void this.resolveUploadTarget();
  }

  private async initTerminal(signal: AbortSignal): Promise<TerminalResources> {
    // Dynamic import — xterm.js requires DOM APIs not available during SSR
    const [{ Terminal }, { FitAddon }, { WebLinksAddon }] = await Promise.all([
      import('@xterm/xterm'),
      import('@xterm/addon-fit'),
      import('@xterm/addon-web-links'),
    ]);

    signal.throwIfAborted();
    const xtermStyle = document.createElement('style');
    try {
      const cssModule = await import('@xterm/xterm/css/xterm.css?inline');
      xtermStyle.textContent = cssModule.default;
    } catch {
      console.warn('[Terminal] Could not load xterm CSS inline, terminal may not render correctly');
    }
    signal.throwIfAborted();
    const container = this.shadowRoot?.querySelector('.terminal-container') as HTMLElement;
    if (!container) throw new Error('Terminal container is not available.');

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

    this.terminalStyle = xtermStyle;
    this.shadowRoot?.appendChild(xtermStyle);

    this.terminal.open(container);
    const terminal = this.terminal;
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

    // Detect active tmux window from OSC 0 title updates emitted by
    // tmux set-titles. This tracks ongoing window switches (Ctrl-B n/p)
    // that OSC 7337 (one-shot at attach) cannot follow.
    this.terminal.parser.registerOscHandler(0, (data: string) => {
      const trimmed = data.trim();
      if (trimmed === 'agent' || trimmed === 'shell') {
        this.activeWindow = trimmed as TmuxWindow;
      }
      // Return false to allow other OSC 0 handlers (if any) to also process
      return false;
    });

    // OSC 52 clipboard relay — scoped to FOCUSED VISIBLE terminal. (P1.8)
    // Hidden or unfocused panes continue parsing output but cannot read or
    // write the system clipboard. Terminal protocol responses (DSR, DA etc.)
    // are unaffected — they flow through xterm's onData → sendData, not this
    // handler. Only the 'c' (system clipboard) selection type accesses the
    // system clipboard. Non-'c' selections (p, q, s) match the original
    // ClipboardAddon's BrowserClipboardProvider exactly: reads receive an
    // empty response (\x1b]52;${sel};\x07), writes are silently ignored.
    // No OS clipboard access occurs for unsupported selections.
    //
    // Limitations:
    // - OSC 52 'c' read requests from unfocused/hidden panes are silently
    //   dropped (no response sent) rather than queued, because the correct
    //   clipboard content depends on user context at response time. Non-'c'
    //   reads always receive an empty response regardless of focus state.
    // - writeText() is asynchronous per the Clipboard API spec. The pre-call
    //   visibility/focus guard prevents unauthorized initiation, but once
    //   writeText() is dispatched to the browser, the OS clipboard write
    //   cannot be revoked by a subsequent focus/visibility change. This is an
    //   inherent API limitation shared with the original ClipboardAddon.
    // - UTF-8 is preserved via TextEncoder/TextDecoder for multi-byte content.
    this.terminal.parser.registerOscHandler(52, (data: string) => {
      const semi = data.indexOf(';');
      if (semi < 0) return true;
      const sel = data.substring(0, semi);
      const payload = data.substring(semi + 1);
      // Only 'c' (system clipboard) is supported for actual clipboard access.
      // Non-'c' selections (p, q, s etc.): reads get an empty response matching
      // the original BrowserClipboardProvider which returns '' for unsupported
      // selections; writes are silently ignored (no-op), also matching original.
      if (sel !== 'c') {
        if (payload === '?') {
          // Send empty response — original addon returned '' for non-'c' reads,
          // which encodes as empty base64 in the response.
          this.sendData(`\x1b]52;${sel};\x07`);
        }
        return true;
      }
      if (payload === '?') {
        // Clipboard read — requires focused + visible + generation match.
        if (!this._visible || !this._focused || this.disposed) return true;
        const gen = this.session?.state.generation ?? 0;
        void navigator.clipboard
          .readText()
          .then((text) => {
            // Recheck at completion: focus/visibility/generation may have changed.
            // Generation check prevents leaking clipboard to a reconnected session.
            if (
              !this._visible ||
              !this._focused ||
              this.disposed ||
              this.session?.state.generation !== gen
            )
              return;
            const bytes = new TextEncoder().encode(text);
            let binary = '';
            for (const byte of bytes) binary += String.fromCharCode(byte);
            this.sendData(`\x1b]52;${sel};${btoa(binary)}\x07`);
          })
          .catch(() => {});
      } else {
        // Clipboard write — guard prevents unauthorized initiation.
        // Note: once writeText() is dispatched, the OS write cannot be revoked
        // by a subsequent visibility/focus change. This is an inherent Clipboard
        // API limitation, not a guard failure.
        if (!this._visible || !this._focused || this.disposed) return true;
        try {
          const binary = atob(payload);
          const bytes = Uint8Array.from(binary, (c) => c.charCodeAt(0));
          const text = new TextDecoder().decode(bytes);
          void navigator.clipboard.writeText(text).catch(() => {});
        } catch {
          // Invalid base64: atob() throws → no clipboard mutation.
          // Deliberate divergence from original addon which decoded invalid
          // base64 to empty string and wrote '' to clipboard. No-mutation
          // is safer — malformed server output should not clear user clipboard.
        }
      }
      return true;
    });

    this.resizeObserver = new ResizeObserver(() => {
      if (!this.measurable()) return;
      this.layoutReady?.();
      this.fitAddon?.fit();
      this.cancelResize();
      this.resizeTimer = setTimeout(() => this.sendResize(), 100);
    });
    this.resizeObserver.observe(container);

    // Defer initial fit until browser has completed layout so the container
    // has its final dimensions (below the toolbar).
    await new Promise((resolve) => requestAnimationFrame(resolve));
    signal.throwIfAborted();
    if (this.measurable()) {
      // Container is already laid out — fit immediately.
      this.fitAddon.fit();
    } else if (!this.hidden) {
      // Pane is visible but container isn't measurable yet (e.g. mid-layout).
      // Wait for the ResizeObserver or reveal() to signal readiness.
      await new Promise<void>((resolve, reject) => {
        const abort = (): void => {
          this.layoutReady = null;
          reject(signal.reason);
        };
        this.layoutReady = () => {
          if (!this.measurable()) return;
          this.layoutReady = null;
          signal.removeEventListener('abort', abort);
          resolve();
        };
        signal.addEventListener('abort', abort, { once: true });
      });
      signal.throwIfAborted();
      this.fitAddon.fit();
    }
    // else: pane is hidden — skip fit, use default 80×24 dimensions.
    // reveal() will call fitAddon.fit() and sendResize() when the pane
    // becomes visible, updating both the local terminal and the remote PTY.

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

      // Ctrl/Cmd+C: copy selection — requires focused + visible.
      if (event.type === 'keydown' && event.key === 'c' && isMod && !event.shiftKey) {
        if (this._visible && this._focused && this.terminal?.hasSelection()) {
          void navigator.clipboard.writeText(this.terminal.getSelection());
          return false; // prevent sending to PTY
        }
        return true; // no selection → send SIGINT
      }

      // Ctrl/Cmd+V: paste — requires focused + visible + generation match
      // at async completion. (P1.8)
      if (event.type === 'keydown' && event.key === 'v' && isMod && !event.shiftKey) {
        event.preventDefault();
        const gen = this.session?.state.generation ?? 0;
        void navigator.clipboard.readText().then((text) => {
          if (
            text &&
            this._visible &&
            this._focused &&
            !this.disposed &&
            this.session?.state.generation === gen
          )
            this.sendData(text);
        });
        return false;
      }

      // Ctrl+Shift+C: always copy (focused + visible)
      if (event.type === 'keydown' && event.key === 'C' && event.ctrlKey && event.shiftKey) {
        if (this._visible && this._focused && this.terminal?.hasSelection()) {
          void navigator.clipboard.writeText(this.terminal.getSelection());
        }
        return false;
      }

      // Ctrl+Shift+V: always paste (focused + visible + generation)
      if (event.type === 'keydown' && event.key === 'V' && event.ctrlKey && event.shiftKey) {
        event.preventDefault();
        const gen = this.session?.state.generation ?? 0;
        void navigator.clipboard.readText().then((text) => {
          if (
            text &&
            this._visible &&
            this._focused &&
            !this.disposed &&
            this.session?.state.generation === gen
          )
            this.sendData(text);
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

    return {
      write: (bytes) => terminal.write(bytes),
      reset: () => terminal.reset(),
      size: () => ({ cols: terminal.cols, rows: terminal.rows }),
      dispose: () => {
        xtermStyle.remove();
        this.disposeTerminal();
      },
    };
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

  private sendData(data: string): void {
    this.session?.sendData(data);
  }

  private sendResize(): void {
    if (!this.measurable() || !this.terminal) return;
    const { cols, rows } = this.terminal;
    const last = this.session?.state.lastSize;
    if (cols !== last?.cols || rows !== last?.rows) this.session?.resize(cols, rows);
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
    // Terminal session drags (from the rail) use a custom MIME type and are
    // handled by the workspace root's drop handlers. Do NOT show the file
    // upload overlay for those — it would obscure the placement feedback.
    if (e.dataTransfer?.types.includes(TERMINAL_DRAG_MIME)) return;
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

  private _onDragLeave(e: DragEvent): void {
    // Terminal drags never incremented _dragCounter (skipped in _onDragEnter).
    if (e.dataTransfer?.types.includes(TERMINAL_DRAG_MIME)) return;
    this._dragCounter = Math.max(0, this._dragCounter - 1);
    if (this._dragCounter === 0) this.isDragOver = false;
  }

  private _onDragOver(e: DragEvent): void {
    // Let terminal drags pass through to the workspace root handler.
    if (e.dataTransfer?.types.includes(TERMINAL_DRAG_MIME)) return;
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = this.uploadEnabled ? 'copy' : 'none';
  }

  private async _onDrop(e: DragEvent): Promise<void> {
    // Terminal drags are handled by the workspace root — do not intercept.
    if (e.dataTransfer?.types.includes(TERMINAL_DRAG_MIME)) return;
    e.preventDefault();
    this._dragCounter = 0;
    this.isDragOver = false;
    if (!this.uploadEnabled || !e.dataTransfer?.files.length) return;
    // A file drop onto this pane is an explicit user interaction. Establish
    // real DOM focus (triggering focusin → _focused = true) rather than
    // setting _focused directly. If focus leaves during the async upload,
    // focusout will clear _focused and the completion guard correctly blocks.
    if (this._visible && !this.disposed) this.terminal?.focus();
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
    // Capture identity at drop time — a late completion must not inject
    // paths into a session that has been hidden, closed or reselected. (P1.8)
    const gen = this.session?.state.generation ?? 0;
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

      // Guard: do not inject paths if pane lost focus, was hidden, disposed
      // or reconnected during the upload. (P1.8)
      if (
        !this._visible ||
        !this._focused ||
        this.disposed ||
        this.session?.state.generation !== gen
      )
        return;

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
    if (this.disposed) return;
    this.disposed = true;
    this.sessionUnsubscribe?.();
    this.sessionUnsubscribe = null;
    this.connected = false;
    this.metadataUnsubscribe?.();
    this.metadataUnsubscribe = null;
    this.closePortDropdown();
    this.removeWindowListeners();
    this.disposeTerminal();
    this.wasConnected = false;
  }

  /**
   * Prevent the browser from navigating to a dropped file, but ONLY when the
   * drag target is within this visible pane. Events targeting Chat/Dashboard
   * or other workspace areas pass through unmodified. Uses composedPath()
   * to correctly detect events retargeted across Shadow DOM boundaries.
   */
  private installWindowDragPrevention(): void {
    if (this._windowDragOver) return;
    this._windowDragOver = (e: DragEvent) => {
      if (this._visible && e.composedPath().includes(this)) e.preventDefault();
    };
    this._windowDrop = (e: DragEvent) => {
      if (this._visible && e.composedPath().includes(this)) e.preventDefault();
    };
    window.addEventListener('dragover', this._windowDragOver);
    window.addEventListener('drop', this._windowDrop);
  }

  private removeWindowListeners(): void {
    if (this._windowDragOver) {
      window.removeEventListener('dragover', this._windowDragOver);
      this._windowDragOver = null;
    }
    if (this._windowDrop) {
      window.removeEventListener('drop', this._windowDrop);
      this._windowDrop = null;
    }
  }

  private disposeTerminal(): void {
    this.terminalStyle?.remove();
    this.terminalStyle = null;
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
    this.wasConnected = false;
  }

  /**
   * Switch to the "agent" tmux window via prefix key binding (Ctrl-B A).
   */
  private switchToAgent(): void {
    if (!this.connected) return;
    this.sendData('\x02A');
    this.activeWindow = 'agent';
    this.terminal?.focus();
  }

  /**
   * Switch to the "shell" tmux window via prefix key binding (Ctrl-B S).
   * The binding in .tmux.conf handles creating the window if it was closed.
   */
  private switchToShell(): void {
    if (!this.connected) return;
    this.sendData('\x02S');
    this.activeWindow = 'shell';
    this.terminal?.focus();
  }

  private closePortDropdown(): void {
    if (this.portDropdownTimer) clearTimeout(this.portDropdownTimer);
    this.portDropdownTimer = null;
    this.portDropdownClose?.();
    this.portDropdownClose = null;
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
            const wasOpen = el.classList.contains('open');
            this.closePortDropdown();
            if (!wasOpen) {
              el.classList.add('open');
              const close = (): void => {
                el.classList.remove('open');
                document.removeEventListener('click', close);
              };
              this.portDropdownClose = close;
              // Defer past this click; disposal cancels the pending listener.
              this.portDropdownTimer = setTimeout(() => {
                this.portDropdownTimer = null;
                document.addEventListener('click', close);
              }, 0);
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

  private async handleCaptureAuth(
    force = false,
    scope: 'project' | 'user' = 'project'
  ): Promise<void> {
    if (!this.agent) return;
    this.captureAuthLoading = true;
    this.captureAuthConflicts = null;
    try {
      const command = ['python3', '/home/scion/.scion/harness/capture_auth.py', '--scope', scope];
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
        for (const m of result.output.matchAll(ScionTerminalPane.SECRET_CONFLICT_RE)) {
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
          @click=${() => void this.handleCaptureAuth(true, this.captureAuthSelectedScope)}
          >Force Update</sl-button
        >
      </sl-dialog>
    `;
  }

  private async refreshAgentData(): Promise<void> {
    await this.registry?.metadata.refresh(this.agentId);
  }

  private handleReconnect(): void {
    if (this.metadataError) void this.refreshAgentData();
    if (this.session) {
      this.reconnectInProgress = true;
      void this.session.connect().finally(() => {
        this.reconnectInProgress = this.ownedSession?.reconnecting ?? false;
      });
    }
  }

  /**
   * Whether the Reconnect button should be disabled in the current state.
   * Disabled when already reconnecting, or for deleted agents.
   */
  private get reconnectDisabled(): boolean {
    if (this.reconnectInProgress) return true;
    if (this.disconnectReason === 'agent-deleted') return true;
    return false;
  }

  /** Human-readable overlay title for disconnected/unavailable states. */
  private get overlayTitle(): string {
    // While an attempt (automatic or manual) is running, the overlay always
    // shows "Reconnecting...", regardless of the reason that preceded it.
    // Derived from connection state, not from session.reconnecting: `pending`
    // clears once the socket is constructed, before the handshake finishes.
    if (this.attempting) return 'RECONNECTING...';
    switch (this.disconnectReason) {
      case 'auth-401':
        return 'AUTHENTICATION REQUIRED';
      case 'auth-403':
        return 'ACCESS DENIED';
      case 'not-found':
        return 'AGENT NOT FOUND';
      case 'session-ended':
        return 'SESSION ENDED';
      case 'detached':
        return 'DETACHED';
      case 'agent-offline':
      case 'agent-phase':
      case 'agent-stopped':
        return 'AGENT UNAVAILABLE';
      case 'agent-deleted':
        return 'AGENT DELETED';
      default:
        return 'DISCONNECTED';
    }
  }

  /**
   * Once a reconnect attempt has failed, replace the generic error detail
   * with the product-specified copy until the next attempt starts. A failed
   * manual attempt uses neutral wording instead ("Automatic" would be
   * wrong). Both share the same trailing punctuation.
   */
  private get overlayDetail(): string | null {
    if (this.reconnectFailed && !this.attempting) {
      return this.reconnectFailedManual
        ? 'Reconnection failed, try manually reconnecting later'
        : 'Automatic reconnection failed, try manually reconnecting later';
    }
    return this.error;
  }

  /** Whether the current disconnect state should be rendered as "unavailable" rather than "disconnected". */
  private get isUnavailableState(): boolean {
    return (
      this.disconnectReason === 'agent-offline' ||
      this.disconnectReason === 'agent-phase' ||
      this.disconnectReason === 'agent-stopped' ||
      this.disconnectReason === 'agent-deleted'
    );
  }

  /** Dispatch SPA navigation via the document-level nav-click listener. */
  private navigateToPath(path: string): void {
    this.dispatchEvent(
      new CustomEvent('nav-click', {
        detail: { path },
        bubbles: true,
        composed: true,
      })
    );
  }

  /** Navigate to the agent graph view for this pane's agent. */
  private openInGraph(): void {
    const path = `/agents/graph?project=${encodeURIComponent(this.projectId)}&focus=${encodeURIComponent(this.agentId)}`;
    this.navigateToPath(path);
  }

  /** Navigate to the DM chat conversation with this pane's agent. */
  private openInChat(): void {
    const dmKey = buildAgentDMKey(this.agentId, this.userId);
    if (!dmKey) return;
    const path = chatConversationPath({ conversationKey: dmKey });
    if (path) this.navigateToPath(path);
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

    // Metadata availability must not remove the host during independent PTY setup.
    if (this.session?.state.error && !this.terminal) {
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
          <p>${this.isUnavailableState ? 'Agent Unavailable' : 'Terminal Unavailable'}</p>
          <div class="error-detail">${this.error}</div>
          <button ?disabled=${this.reconnectDisabled} @click=${() => this.handleReconnect()}>
            ${this.reconnectInProgress ? 'Reconnecting...' : 'Retry'}
          </button>
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
        ${this.projectId
          ? html`<button
              class="pane-action-btn"
              title="Open in graph"
              aria-label="View ${this.agentName || this.agentId} in agent graph"
              @click=${() => this.openInGraph()}
            >
              <sl-icon name="diagram-3"></sl-icon>
            </button>`
          : nothing}
        ${this.userId && isFeatureEnabled('web.native_chat')
          ? html`<button
              class="pane-action-btn"
              title="Open in chat"
              aria-label="Chat with ${this.agentName || this.agentId}"
              @click=${() => this.openInChat()}
            >
              <sl-icon name="chat-dots"></sl-icon>
            </button>`
          : nothing}
        <div class="spacer"></div>
        ${this.renderPortButtons()}
        ${this.showCaptureAuth
          ? html`
              <button
                class="capture-auth-btn"
                ?disabled=${this.captureAuthLoading}
                @click=${() => {
                  this.captureAuthScopeDialogOpen = true;
                }}
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
              <button
                class="reconnect-btn"
                ?disabled=${this.reconnectDisabled}
                @click=${() => this.handleReconnect()}
              >
                ${this.attempting ? 'Reconnecting...' : 'Reconnect'}
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
              ${this.metadataError
                ? html`<button class="metadata-retry" @click=${() => void this.refreshAgentData()}>
                    Retry metadata
                  </button>`
                : nothing}
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
          ? html`<div
              class="disconnected-overlay ${this.isUnavailableState ? 'unavailable' : ''} ${this
                .attempting
                ? 'reconnecting'
                : ''}"
            >
              ${this.attempting ? html`<sl-spinner></sl-spinner>` : nothing}
              <span class="overlay-title">${this.overlayTitle}</span>
              ${this.overlayDetail
                ? html`<span class="overlay-detail">${this.overlayDetail}</span>`
                : nothing}
              <button
                class="overlay-reconnect"
                ?disabled=${this.reconnectDisabled}
                @click=${() => this.handleReconnect()}
              >
                ${this.attempting ? 'Reconnecting...' : 'Reconnect'}
              </button>
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
      ${this.renderCaptureAuthConflictDialog()} ${this.renderCaptureAuthScopeDialog()}
    `;
  }

  private renderCaptureAuthScopeDialog() {
    if (!this.captureAuthScopeDialogOpen) return nothing;
    return html`
      <sl-dialog
        label="Capture Auth — Choose Scope"
        open
        @sl-request-close=${() => {
          this.captureAuthScopeDialogOpen = false;
        }}
      >
        <p>Where should the captured credentials be stored?</p>
        <sl-radio-group
          id="capture-scope-group"
          .value=${this.captureAuthSelectedScope}
          @sl-change=${(e: any) => {
            this.captureAuthSelectedScope = e.target.value;
          }}
        >
          <sl-radio value="project">Project secret (all project agents)</sl-radio>
          <sl-radio value="user">Profile secret (your personal credential)</sl-radio>
        </sl-radio-group>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => {
            this.captureAuthScopeDialogOpen = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          @click=${() => {
            this.captureAuthScopeDialogOpen = false;
            void this.handleCaptureAuth(false, this.captureAuthSelectedScope);
          }}
          >Capture</sl-button
        >
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-terminal-pane': ScionTerminalPane;
  }
}
