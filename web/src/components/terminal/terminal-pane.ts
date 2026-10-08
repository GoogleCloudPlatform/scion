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

import { LitElement, html, css, nothing, unsafeCSS, type PropertyValues } from 'lit';
import { customElement, state } from 'lit/decorators.js';
import { ifDefined } from 'lit/directives/if-defined.js';

import {
  canLifecycle,
  isAgentLaunching,
  isIncompleteCreate,
  lifecycleActionRequestInit,
  type Agent,
  type AgentPhase,
  type AgentActivity,
  type ExposedPort,
  type SharedDir,
} from '../../shared/types.js';
import {
  activityTooltip,
  agentStatusBadge,
  suspendReasonHint,
} from '../../shared/agent-state-display.js';
import { lifecycleActionErrorMessage } from '../../client/agent-delete.js';
import { showResponseBodyWarnings } from '../../client/response-warnings.js';
import { showPendingSecretsStartHint } from '../../client/pending-secrets.js';
import {
  TerminalSessionRegistry,
  type TerminalSession,
  type TerminalSessionState,
  type TerminalResources,
  type TerminalDisconnectReason,
} from '../../client/terminal-sessions.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { projectDisplayName } from '../../client/project-label.js';
import { getPublicSettings, invalidatePublicSettings } from '../../client/server-feature-flags.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { dragAxis, wheelNotches } from '../../shared/terminal-wheel.js';
import type { TerminalAgentMetadata } from '../../client/terminal-metadata.js';
import '../shared/breadcrumb.js';
import type { Crumb } from '../shared/breadcrumb.js';
import '../shared/status-badge.js';
import { showToast } from '../../utils/toast.js';
import { copyToClipboard, copyWithToast } from '../../utils/clipboard.js';
import { buildAgentDMKey, chatConversationPath } from '../../client/chat-routes.js';
import { isFeatureEnabled } from '../../utils/feature-flags.js';
import {
  TERMINAL_DRAG_MIME,
  TERMINAL_PANE_DIALOG_OPEN_EVENT,
} from '../../client/terminal-workspace-events.js';
import { navigateTo } from '../../client/navigation.js';
import { TouchPrimaryController } from '../../utils/input-modality.js';
import {
  applyModifiers,
  barKeySequence,
  consumeModifier,
  nextModifierState,
  type ModifierState,
  type Modifiers,
  type TerminalBarKey,
} from './terminal-keys.js';

/**
 * Reads the clipboard text for a paste, resolving `null` instead of throwing
 * when the Clipboard API is unavailable (a plain-http host) or the read is
 * denied.
 */
function readClipboardText(): Promise<string | null> {
  try {
    const clipboard = (navigator as Partial<Navigator>).clipboard;
    if (typeof clipboard?.readText !== 'function') return Promise.resolve(null);
    return clipboard.readText().catch(() => null);
  } catch {
    return Promise.resolve(null);
  }
}

// xterm.js imports are client-side only — guarded by typeof check in lifecycle
// These will be imported dynamically in firstUpdated() since they require DOM APIs
type Terminal = import('@xterm/xterm').Terminal;
type FitAddon = import('@xterm/addon-fit').FitAddon;
// ClipboardAddon replaced with visibility-scoped OSC 52 handler (P1.8)

/** Which tmux window is active */
type TmuxWindow = 'agent' | 'shell';

/** Where captured harness credentials are stored. */
type CaptureAuthScope = 'project' | 'user';

/** localStorage key: 'true' while the user has hidden the touch key bar. */
export const KEY_BAR_HIDDEN_STORAGE_KEY = 'scion-terminal-key-bar-hidden';

/** Keys on the touch key bar, in order: the common ones first, the rest scroll. */
const KEY_BAR_KEYS: ReadonlyArray<{
  label: string;
  aria: string;
  key?: TerminalBarKey;
  char?: string;
  modifier?: 'ctrl' | 'alt';
}> = [
  { label: 'Esc', aria: 'Escape', key: 'escape' },
  { label: 'Tab', aria: 'Tab', key: 'tab' },
  { label: '⇧Tab', aria: 'Shift Tab', key: 'backtab' },
  { label: 'Ctrl', aria: 'Control', modifier: 'ctrl' },
  { label: 'Alt', aria: 'Alt', modifier: 'alt' },
  { label: '←', aria: 'Left arrow', key: 'left' },
  { label: '↑', aria: 'Up arrow', key: 'up' },
  { label: '↓', aria: 'Down arrow', key: 'down' },
  { label: '→', aria: 'Right arrow', key: 'right' },
  { label: 'Home', aria: 'Home', key: 'home' },
  { label: 'End', aria: 'End', key: 'end' },
  { label: 'PgUp', aria: 'Page up', key: 'pageup' },
  { label: 'PgDn', aria: 'Page down', key: 'pagedown' },
  { label: '|', aria: 'Pipe', char: '|' },
  { label: '~', aria: 'Tilde', char: '~' },
  { label: '/', aria: 'Slash', char: '/' },
];

// The terminal viewport stays dark in both app themes: it renders TUI output
// that is generally authored against a dark background. The viewport wrapper
// and the xterm theme share these so they cannot drift apart.
export const TERMINAL_BACKGROUND = '#1a1a1a';
export const TERMINAL_FOREGROUND = '#eaeaea';

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

  /**
   * Set by the workspace root when the pane header has little room (a
   * narrow viewport or the 4-up layout): Back to Project and Back to Agent
   * show as icon-only links (ptone/scion#4324).
   */
  @state() compactHeader = false;

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
   * True while `connection ∈ {loading, connecting}` or `session.reconnecting`.
   * Connection state covers the attempt through the handshake (`pending`
   * clears once the WebSocket is constructed); `session.reconnecting` adds
   * the jitter wait before an automatic attempt after a 4503 close.
   */
  @state()
  private attempting = false;

  /** A reconnect attempt failed; auto-retry is blocked. */
  @state()
  private reconnectFailed = false;

  /**
   * True while the underlying session's connection is 'idle': restored but
   * not yet connected. No Reconnect button and no error styling — just a
   * neutral "select to connect" prompt.
   */
  @state()
  private idle = false;

  /** Whether the failed attempt above was manually triggered. */
  @state()
  private reconnectFailedManual = false;

  @state()
  private activeWindow: TmuxWindow = 'agent';

  @state()
  private agentPhase: AgentPhase = 'created';

  /**
   * The agent is stopping (e.g. while it is being deleted, ptone/scion#2483
   * C#11). Derived from the shared metadata in `applyMetadata`; it only
   * shows a non-fatal notice and never tears the session down. Running
   * clears it; stopped/deleted clear it while the workspace root's SSE
   * bridge marks the session unavailable as before.
   */
  @state()
  private agentStopping = false;

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
  private captureAuthSelectedScope: CaptureAuthScope = 'project';

  /**
   * Hub admin policy (agent_secrets.user_scope_only), fetched fresh from
   * /api/v1/settings/public each time the capture scope dialog opens
   * (design ptone/scion#2291 §7). When true, Project is disabled and
   * Profile is preselected. Fails open (false) if the fetch fails.
   */
  @state()
  private agentSecretsUserScopeOnly = false;

  /** True while the settings/public fetch that gates the scope dialog is in flight. */
  @state()
  private captureAuthSettingsLoading = false;

  /** Tracks an in-progress Resume initiated from this terminal pane (#8954). */
  @state()
  private resumeState: 'idle' | 'resuming' | 'starting' = 'idle';

  /** Error message from a failed Resume attempt or an agent that stopped before connecting. */
  @state()
  private resumeError: string | null = null;

  /** Monotonic sequence guarding async resume responses against disposal or supersession. */
  private resumeRequestSeq = 0;

  /** True once metadata has moved off 'suspended' after a resume was accepted. */
  private resumeSawTransition = false;

  // --- Drag-and-drop file upload state ---
  @state() private uploadEnabled = false;
  @state() private uploadDisabledReason = '';
  @state() private uploadTargetDir = ''; // shared dir name (e.g. "scratchpad")
  @state() private uploadBasePath = ''; // container path of the target dir, from the hub (SharedDir.target)
  @state() private isDragOver = false;
  @state() private isUploading = false;
  @state() private uploadStatus = ''; // progress/error message in overlay

  // --- Touch key bar state ---
  /** The key bar shows only where the primary pointer is touch; see input-modality.ts. */
  private touchPrimary = new TouchPrimaryController(this);
  @state() private keyBarHidden = false;
  @state() private ctrlState: ModifierState = 'off';
  @state() private altState: ModifierState = 'off';
  /** True while a bar key's own sequence passes through xterm, so onData leaves it as is. */
  private sendingBarKey = false;

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
  /** Aborts the touch-scroll listeners installed on the xterm element. */
  private _touchScrollAbort: AbortController | null = null;

  // Theme: the pane chrome (toolbar, buttons, dialogs, loading/error states)
  // follows the app theme through --scion-* tokens. The terminal viewport and
  // the overlays drawn on it stay dark in both themes, matching the xterm
  // palette set in initTerminal (TERMINAL_BACKGROUND / TERMINAL_FOREGROUND).
  static override styles = css`
    :host {
      display: flex;
      flex-direction: column;
      flex: 1;
      min-height: 0;
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text, #1e293b);
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
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      flex-shrink: 0;
      min-height: 40px;
    }

    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
      font-size: 0.8125rem;
      white-space: nowrap;
    }

    .back-link:hover {
      color: var(--scion-primary, #3b82f6);
    }

    scion-breadcrumb.toolbar-crumbs {
      width: auto;
      margin-bottom: 0;
      flex-shrink: 1;
      min-width: 0;
      container-type: normal;
    }

    /* Compact header (ptone/scion#4324): icon-only back links, the size of
       the other header buttons. */
    .back-link.compact {
      justify-content: center;
      width: 32px;
      height: 32px;
      border-radius: var(--scion-radius-sm);
      font-size: 1rem;
      line-height: 1;
    }

    .back-link.compact:hover,
    .back-link.compact:focus-visible {
      background: var(--scion-badge-primary-bg, #dbeafe);
    }

    /* Header controls keep their full size when the header is crowded; only
       the agent name gives way (see .agent-name). */
    .back-link,
    .separator,
    .toggle-group,
    .pane-action-btn {
      flex-shrink: 0;
    }

    .separator {
      width: 1px;
      height: 20px;
      background: var(--scion-border, #e2e8f0);
    }

    .agent-name {
      /* Shrinks first, with an ellipsis, but never below a readable width:
         4rem (about eight characters), or the whole name when it is shorter
         (--agent-name-chars is its length, set in renderAgentName). */
      flex: 0 1 auto;
      min-width: min(4rem, calc(var(--agent-name-chars, 8) * 1ch));
      font-size: 0.875rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
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
      color: var(--scion-text-muted, #64748b);
    }

    .status-dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      background: var(--scion-status-danger, #ef4444);
    }

    .status-dot.connected {
      background: var(--scion-status-success, #22c55e);
    }

    .reconnect-btn {
      background: transparent;
      border: 1px solid var(--scion-border, #e2e8f0);
      color: var(--scion-text-muted, #64748b);
      padding: 0.25rem 0.75rem;
      border-radius: var(--scion-radius-sm);
      cursor: pointer;
      font-size: 0.75rem;
    }

    .reconnect-btn:hover:not(:disabled) {
      border-color: var(--scion-primary, #3b82f6);
      color: var(--scion-primary, #3b82f6);
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
      border: 1px solid var(--scion-border, #e2e8f0);
      color: var(--scion-text-muted, #64748b);
      width: 32px;
      height: 32px;
      border-radius: var(--scion-radius-sm);
      cursor: pointer;
      padding: 0;
      line-height: 1;
    }

    .pane-action-btn:hover {
      border-color: var(--scion-primary, #3b82f6);
      color: var(--scion-primary, #3b82f6);
      background: var(--scion-badge-primary-bg, #dbeafe);
    }

    .capture-auth-btn {
      background: transparent;
      border: 1px solid var(--scion-border, #e2e8f0);
      color: var(--scion-badge-warning-text, #92400e);
      padding: 0.25rem 0.75rem;
      border-radius: var(--scion-radius-sm);
      cursor: pointer;
      font-size: 0.75rem;
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
    }

    .capture-auth-btn:hover {
      border-color: var(--scion-status-warning, #f59e0b);
      background: var(--scion-badge-warning-bg, #fef3c7);
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

    .capture-scope-restricted-hint {
      font-size: 0.75rem;
      color: var(--scion-text-secondary, #64748b);
      margin: -0.25rem 0 0.5rem 1.5rem;
    }

    /* Window switcher toggle group: two rectangular icon buttons */
    .toggle-group {
      display: inline-flex;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-sm);
      overflow: hidden;
    }

    .toggle-group button {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      background: transparent;
      border: none;
      color: var(--scion-text-muted, #64748b);
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
      border-right: 1px solid var(--scion-border, #e2e8f0);
    }

    .toggle-group button:hover {
      color: var(--scion-text, #1e293b);
      background: var(--scion-badge-neutral-bg, #e2e8f0);
    }

    .toggle-group button.active {
      color: var(--scion-badge-success-text, #166534);
      background: var(--scion-badge-success-bg, #dcfce7);
    }

    .toggle-group button:disabled {
      cursor: default;
      opacity: 0.4;
    }

    /* The capture-auth dialogs inherit :host text colour, so their panel must
       come from the same theme tokens (as shared/confirm-dialog.ts does);
       otherwise dark mode renders light text on Shoelace's light panel. */
    sl-dialog {
      --sl-panel-background-color: var(--scion-surface-raised, #ffffff);
      --sl-panel-border-color: var(--scion-border, #e2e8f0);
    }

    .terminal-wrapper {
      flex: 1;
      position: relative;
      overflow: hidden;
      background: ${unsafeCSS(TERMINAL_BACKGROUND)};
      color: ${unsafeCSS(TERMINAL_FOREGROUND)};
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

    .disconnected-overlay .overlay-resume-status {
      display: flex;
      flex-direction: column;
      align-items: center;
      gap: 1rem;
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
      border-radius: calc(var(--scion-radius) * 0.75);
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

    .disconnected-overlay .overlay-actions {
      display: inline-flex;
      align-items: center;
      gap: 0.75rem;
      margin-top: 0.5rem;
    }

    .disconnected-overlay .overlay-actions .overlay-reconnect {
      margin-top: 0;
    }

    .disconnected-overlay .overlay-resume-error {
      color: var(--scion-status-danger, #ef4444);
      font-size: 0.875rem;
      max-width: 400px;
      text-align: center;
      line-height: 1.5;
    }

    /* Always rendered so screen readers see the live regions before text
       arrives; positioned out of flex flow while idle so gap adds no space. */
    .disconnected-overlay .overlay-resume-status.idle,
    .disconnected-overlay .overlay-resume-error.idle {
      position: absolute;
      height: 0;
      margin: 0;
      padding: 0;
      overflow: hidden;
      pointer-events: none;
    }

    .idle-overlay {
      position: absolute;
      top: 0;
      left: 0;
      right: 0;
      bottom: 0;
      background: rgba(0, 0, 0, 0.4);
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      gap: 0.5rem;
      z-index: 10;
      pointer-events: none;
    }

    .idle-overlay .overlay-title {
      color: #94a3b8;
      font-size: 1rem;
      font-weight: 600;
    }

    .idle-overlay .overlay-detail {
      color: #94a3b8;
      font-size: 0.875rem;
      max-width: 400px;
      text-align: center;
      line-height: 1.5;
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
      color: var(--scion-text-muted, #64748b);
      margin-top: 1rem;
    }

    .spinner {
      width: 32px;
      height: 32px;
      border: 3px solid var(--scion-border, #e2e8f0);
      border-top-color: var(--scion-primary, #3b82f6);
      border-radius: 50%;
      animation: spin 0.8s linear infinite;
    }

    @keyframes spin {
      to {
        transform: rotate(360deg);
      }
    }

    .error-state p {
      color: var(--scion-badge-danger-text, #991b1b);
      margin: 0 0 1rem 0;
    }

    .error-state .resume-progress {
      display: flex;
      flex-direction: column;
      align-items: center;
    }

    .error-state p.resume-status-title {
      color: var(--scion-text, #1e293b);
      font-weight: 500;
      margin: 1rem 0 0.5rem 0;
    }

    .error-state .error-detail {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }

    .error-state button {
      background: var(--scion-primary, #3b82f6);
      color: var(--scion-primary-text, #ffffff);
      border: none;
      padding: 0.5rem 1.5rem;
      border-radius: calc(var(--scion-radius) * 0.75);
      cursor: pointer;
      font-size: 0.875rem;
    }

    .error-state button:hover {
      background: var(--scion-primary-hover, #2563eb);
    }

    .error-state .suspend-hint,
    .error-state .resume-unauthorized {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
      max-width: 420px;
      margin-bottom: 0.75rem;
      line-height: 1.5;
    }

    .error-state .resume-error {
      color: var(--scion-badge-danger-text, #991b1b);
      font-size: 0.8125rem;
      max-width: 420px;
      margin-bottom: 0.75rem;
      line-height: 1.5;
    }

    /* Always rendered so screen readers see the live regions before text
       arrives; they take no space while idle. */
    .error-state .resume-progress.idle,
    .error-state .resume-error.idle {
      height: 0;
      margin: 0;
      padding: 0;
      overflow: hidden;
    }

    .error-state .error-actions {
      display: inline-flex;
      align-items: center;
      gap: 0.75rem;
      margin-top: 0.25rem;
    }

    .error-state .secondary-retry-btn {
      background: transparent;
      color: var(--scion-text-muted, #64748b);
      border: 1px solid var(--scion-border, #e2e8f0);
      padding: 0.375rem 1rem;
      border-radius: var(--scion-radius-sm);
      font-size: 0.8125rem;
    }

    .error-state .secondary-retry-btn:hover:not(:disabled) {
      border-color: var(--scion-primary, #3b82f6);
      color: var(--scion-primary, #3b82f6);
      background: transparent;
    }

    .error-state .secondary-retry-btn:disabled {
      opacity: 0.5;
      cursor: default;
    }

    /* Always rendered so screen readers see the live region before its
       text arrives; it takes no space while idle. */
    .stopping-notice.idle {
      padding: 0;
      height: 0;
      overflow: hidden;
    }

    .stopping-notice {
      padding: 0.375rem 1rem;
      background: var(--scion-badge-warning-bg, #fef3c7);
      color: var(--scion-badge-warning-text, #92400e);
      font-size: 0.75rem;
    }

    .error-banner {
      padding: 0.375rem 1rem;
      background: var(--scion-badge-danger-bg, #fee2e2);
      color: var(--scion-badge-danger-text, #991b1b);
      font-size: 0.75rem;
    }

    /* Inline text-link action inside the themed banner; inherits its colour. */
    .metadata-retry {
      background: transparent;
      border: none;
      padding: 0;
      margin-left: 0.5rem;
      color: inherit;
      font: inherit;
      text-decoration: underline;
      cursor: pointer;
    }

    .metadata-retry:hover {
      text-decoration: none;
    }

    .metadata-retry:focus-visible {
      outline: 2px solid currentColor;
      outline-offset: 2px;
    }

    /* Port forwarding buttons */
    .port-btn {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      background: transparent;
      border: 1px solid var(--scion-status-success, #22c55e);
      color: var(--scion-badge-success-text, #166534);
      padding: 0.25rem 0.75rem;
      border-radius: var(--scion-radius-sm);
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
      border-color: var(--scion-status-success, #22c55e);
      background: var(--scion-badge-success-bg, #dcfce7);
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
      border: 1px solid var(--scion-status-success, #22c55e);
      color: var(--scion-badge-success-text, #166534);
      padding: 0.25rem 0.75rem;
      border-radius: var(--scion-radius-sm);
      font-size: 0.75rem;
      cursor: pointer;
      white-space: nowrap;
    }

    .port-dropdown-trigger:hover {
      border-color: var(--scion-status-success, #22c55e);
      background: var(--scion-badge-success-bg, #dcfce7);
    }

    .port-dropdown-menu {
      display: none;
      position: absolute;
      top: 100%;
      right: 0;
      margin-top: 4px;
      background: var(--scion-surface-raised, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: calc(var(--scion-radius) * 0.75);
      padding: 0.25rem 0;
      min-width: 180px;
      z-index: 100;
      box-shadow: var(
        --scion-shadow-md,
        0 4px 6px -1px rgb(0 0 0 / 0.1),
        0 2px 4px -2px rgb(0 0 0 / 0.1)
      );
    }

    .port-dropdown.open .port-dropdown-menu {
      display: block;
    }

    .port-dropdown-menu a {
      display: block;
      padding: 0.5rem 0.75rem;
      color: var(--scion-badge-success-text, #166534);
      text-decoration: none;
      font-size: 0.8rem;
      white-space: nowrap;
    }

    .port-dropdown-menu a:hover {
      background: var(--scion-badge-success-bg, #dcfce7);
    }

    /* Touch only, so as wide as the window toggles; it must not shrink away
       in a phone-width toolbar. */
    .key-bar-toggle {
      width: 44px;
      flex-shrink: 0;
    }

    .key-bar-toggle[aria-pressed='true'] {
      color: var(--scion-primary, #3b82f6);
      border-color: var(--scion-primary, #3b82f6);
    }

    /* Touch key bar. It sits below the terminal in the pane's column, so it
       stays just above the on-screen keyboard while client/viewport.ts
       shrinks the app frame to the visible area. It scrolls sideways when
       the keys do not fit; the bottom safe-area inset is dropped while the
       keyboard is open (--scion-kb-open). */
    .key-bar {
      display: flex;
      gap: 0.25rem;
      padding: 0.25rem 0.25rem
        calc(0.25rem + env(safe-area-inset-bottom, 0px) * (1 - var(--scion-kb-open, 0)));
      background: var(--scion-bg-subtle, #f1f5f9);
      border-top: 1px solid var(--scion-border, #e2e8f0);
      flex-shrink: 0;
      overflow-x: auto;
      overscroll-behavior-x: contain;
      touch-action: pan-x;
      scrollbar-width: none;
      -webkit-user-select: none;
      user-select: none;
    }

    .key-bar::-webkit-scrollbar {
      display: none;
    }

    .key-bar button {
      flex: 0 0 auto;
      min-width: 44px;
      height: 44px;
      padding: 0 0.5rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 6px;
      color: var(--scion-text, #1e293b);
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      cursor: pointer;
      -webkit-tap-highlight-color: transparent;
      -webkit-touch-callout: none;
    }

    .key-bar button:active:not(:disabled) {
      background: var(--scion-badge-neutral-bg, #e2e8f0);
    }

    .key-bar button.armed {
      color: var(--scion-primary, #3b82f6);
      border-color: var(--scion-primary, #3b82f6);
      background: var(--scion-badge-primary-bg, #dbeafe);
    }

    .key-bar button.locked {
      color: var(--scion-primary-text, #ffffff);
      border-color: var(--scion-primary, #3b82f6);
      background: var(--scion-primary, #3b82f6);
    }

    .key-bar button:disabled {
      opacity: 0.4;
      cursor: default;
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
    try {
      this.keyBarHidden = localStorage.getItem(KEY_BAR_HIDDEN_STORAGE_KEY) === 'true';
    } catch {
      // localStorage may be unavailable (SecurityError in restricted contexts)
    }
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

  /**
   * Announces each capture-auth dialog as it opens, with
   * TERMINAL_PANE_DIALOG_OPEN_EVENT. The dialogs render already open, so
   * they fire no `sl-show` a surface outside the pane could observe.
   */
  protected override updated(changed: PropertyValues): void {
    super.updated(changed);
    const opened = (key: 'captureAuthConflicts' | 'captureAuthScopeDialogOpen'): boolean =>
      changed.has(key) && !changed.get(key) && Boolean(this[key]);
    if (opened('captureAuthConflicts') || opened('captureAuthScopeDialogOpen')) {
      this.dispatchEvent(
        new CustomEvent(TERMINAL_PANE_DIALOG_OPEN_EVENT, { bubbles: true, composed: true })
      );
    }
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
  open(
    registry: TerminalSessionRegistry,
    agentId: string,
    options?: { deferConnect?: boolean }
  ): TerminalSession {
    if (this.disposed) throw new Error('Terminal pane is disposed.');
    if (this.session) {
      if (this.registry === registry && this.agentId === agentId.toLowerCase()) return this.session;
      throw new Error('Terminal pane cannot be rebound.');
    }
    if (registry.list().some((session) => session.state.agentId === agentId.toLowerCase())) {
      throw new Error('Terminal session already has a pane; reuse its original element.');
    }
    this.registry = registry;
    this.ownedSession = registry.open(
      agentId,
      async (_agent, signal) => {
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
      },
      options
    );
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

  /**
   * Moves focus into this pane: the terminal when it exists, otherwise the
   * pane itself, so the terminal takes focus once it connects (see
   * `shouldAutoFocusTerminal`).
   */
  focusTerminal(): void {
    if (this.terminal) {
      this.terminal.focus();
      return;
    }
    if (!this.hasAttribute('tabindex')) this.tabIndex = -1;
    this.focus();
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
        this.shadowRoot?.contains(document.activeElement) ||
        false;
      if (this._focused) this.dataset.focused = '';
      else delete this.dataset.focused;
      // Re-install scoped drop prevention for visible workspace panes.
      if (this.isConnected && !this.disposed) this.installWindowDragPrevention();
      void this.reveal();
    } else {
      this._focused = false;
      delete this.dataset.focused;
      this.clearKeyBarModifiers();
      this.terminal?.blur();
      this.cancelResize();
      // Navigating away cancels any in-flight resume wait and disarms
      // resume-triggered auto-connect.
      if (this.resumeState !== 'idle') {
        this.resumeRequestSeq++;
        this.resumeState = 'idle';
        this.resumeSawTransition = false;
      }
      this.session?.disarmResumeConnect();
      // Remove drop prevention so Chat/Dashboard drops are unaffected.
      this.removeWindowListeners();
    }
    this.updateFrontmost();
  }

  /**
   * Explicit lifetime boundary: closes the bound session explicitly (sending
   * the tmux detach key) and releases the pane.
   */
  dispose(): void {
    if (this.disposed) return;
    try {
      this.session?.close('explicit');
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
    // Also true during the jitter wait before an automatic attempt
    // (session.reconnecting), so the overlay says RECONNECTING... from the
    // close through the redial.
    this.attempting =
      state.connection === 'loading' ||
      state.connection === 'connecting' ||
      this.reconnectInProgress;
    this.idle = state.connection === 'idle';
    this.reconnectFailed = state.reconnectFailed;
    this.reconnectFailedManual = state.reconnectFailedManual;
    if (state.connection !== 'loading') this.loading = false;
    if (
      state.connection === 'connecting' ||
      state.connection === 'connected' ||
      (state.connection === 'loading' && this.resumeState === 'starting')
    ) {
      this.resumeState = 'idle';
      this.resumeError = null;
      this.resumeSawTransition = false;
    } else if (state.connection === 'disconnected' || state.disconnectReason === 'agent-deleted') {
      this.resumeState = 'idle';
      this.resumeSawTransition = false;
    }
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
    this.agentStopping = value.availability !== 'deleted' && value.agent?.phase === 'stopping';
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
    this.reconcileResumeWithMetadata(value, agent);
  }

  private reconcileResumeWithMetadata(value: TerminalAgentMetadata, agent: Agent): void {
    if (this.resumeState === 'idle') return;
    if (value.availability === 'deleted') {
      this.resumeState = 'idle';
      this.resumeSawTransition = false;
      this.session?.disarmResumeConnect();
      return;
    }
    if (agent.phase === 'running' && agent.activity !== 'offline') {
      this.resumeSawTransition = true;
      this.session?.noteAgentAvailable();
      return;
    }
    if (agent.phase === 'starting' || agent.phase === 'provisioning') {
      this.resumeSawTransition = true;
      return;
    }
    if (this.resumeState === 'starting') {
      if (agent.phase === 'stopped' || agent.phase === 'error') {
        this.resumeState = 'idle';
        this.resumeSawTransition = false;
        this.session?.disarmResumeConnect();
        this.resumeError =
          agent.phase === 'error'
            ? 'Agent failed to start.'
            : 'Agent stopped before the terminal could connect.';
      } else if (agent.phase === 'suspended' && this.resumeSawTransition) {
        this.resumeState = 'idle';
        this.resumeSawTransition = false;
        this.session?.disarmResumeConnect();
        this.resumeError = 'Agent returned to suspended before the terminal could connect.';
      }
    }
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
        background: TERMINAL_BACKGROUND,
        foreground: TERMINAL_FOREGROUND,
        cursor: '#f39c12',
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
    this.enableTouchWheelScroll();

    // Detect active tmux window from OSC 7337 sequence sent by the broker
    // on connect. Format: \033]7337;tmuxwindow=<name>\007
    this.terminal.parser.registerOscHandler(7337, (data: string) => {
      const match = data.match(/^tmuxwindow=(.+)$/);
      if (match) {
        const name = match[1];
        if (name === 'agent' || name === 'shell') {
          this.activeWindow = name;
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
        this.activeWindow = trimmed;
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
        void readClipboardText().then((text) => {
          if (text === null) return;
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
        });
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
          // Program-driven write: no toast, and a failure is ignored.
          void copyToClipboard(text);
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
    } else {
      // Container isn't measurable yet: either the pane is hidden, or it is
      // visible but mid-layout. Wait for the ResizeObserver or reveal() to
      // signal readiness, so the session attaches once, at the measured size,
      // only after the pane is shown. Closing the pane aborts the signal,
      // which rejects this wait and clears layoutReady, so it never attaches.
      await new Promise<void>((resolve, reject) => {
        const abort = (): void => {
          this.layoutReady = null;
          // eslint-disable-next-line @typescript-eslint/prefer-promise-reject-errors -- rejects with the AbortSignal reason unchanged, as signal.throwIfAborted() does, so callers can still match it.
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
          void copyWithToast(this.terminal.getSelection());
          return false; // prevent sending to PTY
        }
        return true; // no selection → send SIGINT
      }

      // Ctrl/Cmd+V: paste — requires focused + visible + generation match
      // at async completion. (P1.8)
      if (event.type === 'keydown' && event.key === 'v' && isMod && !event.shiftKey) {
        event.preventDefault();
        const gen = this.session?.state.generation ?? 0;
        void readClipboardText().then((text) => {
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
          void copyWithToast(this.terminal.getSelection());
        }
        return false;
      }

      // Ctrl+Shift+V: always paste (focused + visible + generation)
      if (event.type === 'keydown' && event.key === 'V' && event.ctrlKey && event.shiftKey) {
        event.preventDefault();
        const gen = this.session?.state.generation ?? 0;
        void readClipboardText().then((text) => {
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

    // Handle terminal input. The key bar's sticky Ctrl/Alt apply to the next
    // character typed on the on-screen keyboard.
    this.terminal.onData((data: string) => {
      this.sendData(this.sendingBarKey ? data : this.applyKeyBarModifiers(data));
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

  /**
   * Turns a one-finger vertical drag into wheel events while the application
   * has mouse reporting on (tmux with `mouse on`), so its scrollback can be
   * reached on a device that has no wheel. xterm.js ignores touch while mouse
   * reporting is on. With it off this does nothing and xterm.js scrolls its
   * own viewport on touch, which has scrollback only on the normal screen: an
   * application on the alternate screen that does not report the mouse (tmux
   * with `mouse off`) does not scroll by touch.
   *
   * Only a vertical drag is taken. A horizontal one, a second finger and a
   * pinch stay with the browser.
   *
   * The wheel events are dispatched on the xterm element, so xterm.js reports
   * them itself: in the encoding the application asked for, and at the cell
   * under the finger, so a split tmux window scrolls the pane that is touched.
   *
   * The listeners are bound to an AbortController that disposeTerminal()
   * aborts, so they live as long as this pane's xterm element.
   */
  private enableTouchWheelScroll(): void {
    const el = this.terminal?.element;
    if (!el) return;
    this._touchScrollAbort?.abort();
    const abort = new AbortController();
    this._touchScrollAbort = abort;
    const { signal } = abort;

    const mouseActive = (): boolean =>
      !!this.terminal && this.terminal.modes.mouseTrackingMode !== 'none';
    // One wheel notch per row of finger travel. How far a notch scrolls is
    // up to the application: tmux's default copy-mode binding scrolls five
    // lines for each, so its scrollback moves faster than the finger.
    const rowHeight = (): number => Math.max(8, el.clientHeight / (this.terminal?.rows ?? 24));
    // Stops the browser panning vertically on the next gesture; panning
    // sideways and pinch-zoom still work.
    const setTouchAction = (on: boolean): void => {
      el.style.touchAction = on ? 'pan-x pinch-zoom' : '';
    };

    let startX = 0;
    let startY = 0;
    let lastY: number | null = null;
    let carry = 0;
    // Whether this gesture can still be, or is, a drag to translate.
    let consuming = false;
    // Set once the drag is known to be vertical (dragAxis).
    let vertical = false;

    el.addEventListener(
      'touchstart',
      (ev: TouchEvent): void => {
        consuming = ev.touches.length === 1 && mouseActive();
        // The browser reads touch-action when a gesture begins, so this
        // applies to the next one; preventDefault below handles this one.
        setTouchAction(consuming);
        if (!consuming) return;
        startX = ev.touches[0].clientX;
        startY = lastY = ev.touches[0].clientY;
        carry = 0;
        vertical = false;
      },
      { passive: true, signal }
    );

    el.addEventListener(
      'touchmove',
      (ev: TouchEvent): void => {
        if (!consuming || lastY === null) return;
        // A second finger is a pinch-zoom, which belongs to the browser. The
        // application can also turn mouse reporting off during a drag.
        if (ev.touches.length !== 1 || !mouseActive()) {
          consuming = false;
          return;
        }
        const touch = ev.touches[0];
        if (!vertical) {
          // Browsers hold the first touchmove back until the finger has
          // left a slop region, so this is normally decided on that move.
          const axis = dragAxis(touch.clientX - startX, touch.clientY - startY);
          if (axis === null) return;
          if (axis === 'horizontal') {
            consuming = false;
            return;
          }
          vertical = true;
        }
        // Every move of a vertical drag, not only one that crosses a row, or
        // the page moves under the finger. The first gesture after mouse
        // reporting turns on can arrive with the browser's scroll already
        // under way, and that move cannot be cancelled.
        if (ev.cancelable) ev.preventDefault();

        carry += lastY - touch.clientY;
        lastY = touch.clientY;

        const { notches, up, remainder } = wheelNotches(carry, rowHeight());
        carry = remainder;
        for (let i = 0; i < notches; i++) {
          el.dispatchEvent(
            new WheelEvent('wheel', {
              deltaY: up ? -1 : 1,
              deltaMode: WheelEvent.DOM_DELTA_LINE,
              clientX: touch.clientX,
              clientY: touch.clientY,
              bubbles: true,
              cancelable: true,
            })
          );
        }
      },
      { passive: false, signal }
    );

    const end = (ev: TouchEvent): void => {
      if (ev.touches.length > 0) return;
      lastY = null;
      carry = 0;
      consuming = false;
      setTouchAction(mouseActive());
    };
    el.addEventListener('touchend', end, { passive: true, signal });
    el.addEventListener('touchcancel', end, { passive: true, signal });
  }

  private sendData(data: string): void {
    this.session?.sendData(data);
  }

  // --- Touch key bar ---

  private get keyBarShown(): boolean {
    return this.touchPrimary.isTouch && !this.keyBarHidden;
  }

  private get keyBarModifiers(): Modifiers {
    return { ctrl: this.ctrlState !== 'off', alt: this.altState !== 'off' };
  }

  private consumeKeyBarModifiers(): void {
    this.ctrlState = consumeModifier(this.ctrlState);
    this.altState = consumeModifier(this.altState);
  }

  private clearKeyBarModifiers(): void {
    this.ctrlState = 'off';
    this.altState = 'off';
  }

  private applyKeyBarModifiers(data: string): string {
    if (!this.keyBarShown) return data;
    const result = applyModifiers(data, this.keyBarModifiers);
    if (result.consumed) this.consumeKeyBarModifiers();
    return result.data;
  }

  private toggleKeyBar(): void {
    this.keyBarHidden = !this.keyBarHidden;
    this.clearKeyBarModifiers();
    try {
      if (this.keyBarHidden) localStorage.setItem(KEY_BAR_HIDDEN_STORAGE_KEY, 'true');
      else localStorage.removeItem(KEY_BAR_HIDDEN_STORAGE_KEY);
    } catch {
      // localStorage may be unavailable (SecurityError in restricted contexts)
    }
  }

  /**
   * Keeps focus where it is (normally xterm's hidden textarea) when a bar
   * key or the bar toggle is pressed, so the on-screen keyboard stays open.
   * Focus moves on mousedown, which a tap also fires, so preventing it is
   * enough and click still fires. Not pointerdown: preventing that cancels
   * the tap's click in WebKit.
   */
  private keepFocus = (e: Event): void => {
    e.preventDefault();
  };

  /**
   * Sends a bar key through xterm's own input path (terminal.input fires
   * onData exactly as a typed key does, and scrolls to the bottom and
   * clears the selection like one). A named key carries its modifiers in
   * its own sequence; a character key is typed and so picks them up in
   * onData like an on-screen keyboard character.
   */
  private pressBarKey(entry: (typeof KEY_BAR_KEYS)[number]): void {
    if (entry.modifier === 'ctrl') {
      this.ctrlState = nextModifierState(this.ctrlState);
      return;
    }
    if (entry.modifier === 'alt') {
      this.altState = nextModifierState(this.altState);
      return;
    }
    const terminal = this.terminal;
    if (!terminal || !this.connected) return;
    if (entry.char) {
      terminal.input(entry.char);
      return;
    }
    const sequence = barKeySequence(
      entry.key!,
      terminal.modes.applicationCursorKeysMode,
      this.keyBarModifiers
    );
    this.consumeKeyBarModifiers();
    this.sendingBarKey = true;
    try {
      terminal.input(sequence);
    } finally {
      this.sendingBarKey = false;
    }
  }

  private renderKeyBar() {
    if (!this.keyBarShown) return nothing;
    return html`
      <div class="key-bar" role="toolbar" aria-label="Terminal keys" @mousedown=${this.keepFocus}>
        ${KEY_BAR_KEYS.map((entry) => {
          const state = entry.modifier
            ? entry.modifier === 'ctrl'
              ? this.ctrlState
              : this.altState
            : null;
          return html`<button
            type="button"
            class=${state && state !== 'off' ? state : ''}
            aria-label=${state === 'locked' ? `${entry.aria} (locked)` : entry.aria}
            aria-pressed=${state ? String(state !== 'off') : nothing}
            ?disabled=${!entry.modifier && !this.connected}
            @click=${() => this.pressBarKey(entry)}
          >
            ${entry.label}
          </button>`;
        })}
      </div>
    `;
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
      const data = (await resp.json()) as { sharedDirs?: SharedDir[] };
      const dirs = data.sharedDirs ?? [];
      // Filter: writable, non-in_workspace, with a container path from the
      // hub (the layout authority computes it; the UI never builds one).
      const candidates = dirs.filter((d) => !d.read_only && !d.in_workspace && !!d.target);
      const target = candidates.find((d) => d.name === 'scratchpad') || candidates[0];
      if (target && target.target) {
        this.uploadEnabled = true;
        this.uploadTargetDir = target.name;
        this.uploadBasePath = target.target;
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
    if (/^[A-Za-z0-9._/-]+$/.test(path)) return path;
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
    this.resumeRequestSeq++;
    this.resumeState = 'idle';
    this.resumeSawTransition = false;
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
    this._touchScrollAbort?.abort();
    this._touchScrollAbort = null;
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

  private static readonly CAPTURE_AUTH_SCRIPT_PATH = '/home/scion/.scion/harness/capture_auth.py';

  /**
   * Exit code returned by the python3 precheck when capture_auth.py is not
   * staged in the container (for example when container-script harnesses are
   * disallowed on the broker, ptone/scion#8121). Invoking python3 directly on
   * a missing file exits 2, colliding with capture_auth.py's no-creds exit 2.
   */
  private static readonly CAPTURE_AUTH_MISSING_EXIT_CODE = 127;

  private static readonly SECRET_CONFLICT_RE = /secret "([^"]+)" already exists/g;

  /** Matches the 403 error code the hub returns when agent_secrets.user_scope_only
   * blocks a project-scope write (pkg/hub/errors.go ErrCodeSecretScopeRestricted). */
  private static readonly SECRET_SCOPE_RESTRICTED_RE = /secret_scope_restricted/;

  /**
   * Monotonically increasing token for the settings/public fetch below.
   * If the dialog is closed and reopened before a fetch resolves, a stale
   * response (or the stale fetch's `finally`) must not clear loading or
   * overwrite a newer result (round-1 review N2).
   */
  private captureAuthSettingsRequestSeq = 0;

  /**
   * Opens the capture scope dialog and fetches /api/v1/settings/public fresh
   * (design ptone/scion#2291 §7), so an admin toggling the policy while the
   * terminal is open takes effect the next time the dialog opens — this is
   * also how the "retry after a rejection" flow picks up a since-changed
   * policy. Fails open (today's dialog, unrestricted) if the fetch fails.
   *
   * The fresh read goes through the shared per-page cache
   * (ptone/scion#5311): dropping it first forces the request, and its result
   * then refreshes the copy the create and configure pages read.
   */
  private async openCaptureAuthScopeDialog(): Promise<void> {
    this.captureAuthScopeDialogOpen = true;
    this.captureAuthSettingsLoading = true;
    const requestSeq = ++this.captureAuthSettingsRequestSeq;
    let agentSecretsUserScopeOnly = false;
    invalidatePublicSettings();
    const settings = await getPublicSettings();
    if (settings) {
      agentSecretsUserScopeOnly = settings.agentSecretsUserScopeOnly ?? false;
    }
    // A newer open has since started its own fetch; let that one own the
    // final state instead of overwriting it with this stale result.
    if (requestSeq !== this.captureAuthSettingsRequestSeq) return;
    this.agentSecretsUserScopeOnly = agentSecretsUserScopeOnly;
    // The setting forces the profile scope whatever was previously
    // selected — it must win even if the dialog was already open with
    // "project" chosen when an admin flipped it (§7: "forced to 'user'
    // when the dialog opens, whatever the previous selection was").
    if (agentSecretsUserScopeOnly) {
      this.captureAuthSelectedScope = 'user';
    }
    this.captureAuthSettingsLoading = false;
  }

  private async handleCaptureAuth(
    force = false,
    scope: 'project' | 'user' = 'project'
  ): Promise<void> {
    if (!this.agent) return;
    this.captureAuthLoading = true;
    this.captureAuthConflicts = null;
    try {
      const script = ScionTerminalPane.CAPTURE_AUTH_SCRIPT_PATH;
      const missingRc = ScionTerminalPane.CAPTURE_AUTH_MISSING_EXIT_CODE;
      const command = [
        'python3',
        '-c',
        `import os, sys; p = ${JSON.stringify(script)}; os.path.isfile(p) or sys.exit(${missingRc}); os.execv(sys.executable, [sys.executable, p, *sys.argv[1:]])`,
        '--scope',
        scope,
      ];
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
      } else if (result.exitCode === ScionTerminalPane.CAPTURE_AUTH_MISSING_EXIT_CODE) {
        showToast(
          'Capture Auth is unavailable for this agent: no capture script is installed (container-script harnesses may be disabled on its broker).',
          'warning'
        );
        if (result.output) console.log('Capture auth output:', result.output);
      } else if (ScionTerminalPane.SECRET_SCOPE_RESTRICTED_RE.test(result.output)) {
        // Checked before the conflict regex (design §7): a policy rejection
        // is not a conflict, and must not open the conflict/force-update
        // dialog. Covers the race where an admin turns the setting on while
        // this dialog was already open with "project" selected.
        showToast(
          'Your hub administrator only allows capturing credentials to your profile. Choose Profile and try again.'
        );
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

  private get isSuspended(): boolean {
    return (
      this.agent?.phase === 'suspended' &&
      this.disconnectReason !== 'agent-deleted' &&
      this.registry?.metadata.get(this.agentId)?.availability !== 'deleted'
    );
  }

  /**
   * Matches agent-detail.ts / agents.ts / project-detail.ts authorization for
   * explicit Resume on a suspended agent: requires `agent.lifecycle` and that
   * the agent is neither already launching nor an incomplete create.
   */
  private get canResumeAgent(): boolean {
    const agent = this.agent;
    if (!agent || !this.isSuspended) return false;
    return (
      canLifecycle(agent._capabilities) && !isAgentLaunching(agent) && !isIncompleteCreate(agent)
    );
  }

  private get showResumeUnauthorizedHint(): boolean {
    return this.isSuspended && !!this.agent && !canLifecycle(this.agent._capabilities);
  }

  private get isResumingOrStarting(): boolean {
    return this.resumeState === 'resuming' || this.resumeState === 'starting';
  }

  private async handleResume(): Promise<void> {
    const agent = this.agent;
    if (!agent || this.isResumingOrStarting || !this.canResumeAgent) return;

    const reqSeq = ++this.resumeRequestSeq;
    this.resumeState = 'resuming';
    this.resumeError = null;
    this.resumeSawTransition = false;
    this.session?.armResumeConnect();

    try {
      const response = await apiFetch(
        `/api/v1/agents/${agent.id}/start`,
        lifecycleActionRequestInit('resume')
      );
      if (this.disposed || reqSeq !== this.resumeRequestSeq) return;

      if (!response.ok) {
        const msg = await lifecycleActionErrorMessage(response, 'Failed to resume agent');
        if (this.disposed || reqSeq !== this.resumeRequestSeq) return;
        this.session?.disarmResumeConnect();
        this.resumeState = 'idle';
        this.resumeError = msg;
        void this.refreshAgentData();
        return;
      }

      await showResponseBodyWarnings(response);
      void showPendingSecretsStartHint();
      if (this.disposed || reqSeq !== this.resumeRequestSeq) return;

      // SSE may have already delivered 'running' and kicked off the connect
      // while the POST response was in flight.
      if (this.connected || this.attempting) {
        this.resumeState = 'idle';
        this.resumeSawTransition = false;
        return;
      }

      this.resumeState = 'starting';
      void this.refreshAgentData();
    } catch (err) {
      if (this.disposed || reqSeq !== this.resumeRequestSeq) return;
      this.session?.disarmResumeConnect();
      const msg = err instanceof Error ? err.message : 'Failed to resume agent';
      this.resumeState = 'idle';
      this.resumeError = msg;
      void this.refreshAgentData();
    }
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
   * Disabled when already reconnecting, resuming/starting, or for deleted agents.
   */
  private get reconnectDisabled(): boolean {
    if (this.reconnectInProgress || this.isResumingOrStarting) return true;
    if (this.disconnectReason === 'agent-deleted') return true;
    return false;
  }

  /** Human-readable overlay title for disconnected/unavailable states. */
  private get overlayTitle(): string {
    if (this.resumeState === 'resuming') return 'RESUMING...';
    if (this.resumeState === 'starting') return 'STARTING AGENT...';
    // While an attempt (automatic or manual) is running, the overlay always
    // shows "Reconnecting...", regardless of the reason that preceded it.
    // Connection state covers the attempt through the handshake;
    // session.reconnecting adds the jitter wait before an automatic attempt.
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
      case 'attach-unsupported':
        return 'ATTACH NOT SUPPORTED';
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
    if (this.resumeState === 'resuming') return 'Sending resume request...';
    if (this.resumeState === 'starting')
      return 'Waiting for agent to start. Terminal will reconnect automatically.';
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

  /** Navigate to the agent graph view for this pane's agent. */
  private openInGraph(): void {
    const path = `/agents/graph?project=${encodeURIComponent(this.projectId)}&focus=${encodeURIComponent(this.agentId)}`;
    navigateTo(path);
  }

  /** Navigate to the DM chat conversation with this pane's agent. */
  private openInChat(): void {
    const dmKey = buildAgentDMKey(this.agentId, this.userId);
    if (!dmKey) return;
    const path = chatConversationPath({ conversationKey: dmKey });
    if (path) navigateTo(path);
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

  /**
   * Breadcrumb navigation to the agent's project (when present) and agent
   * detail page. Full mode renders `<scion-breadcrumb>` with linked crumbs
   * (no current-page crumb, since the terminal pane is the active context).
   * Compact mode renders icon-only links with the same destinations, full
   * accessible names, and tooltips.
   */
  private renderBackLinks() {
    if (!this.agentId) {
      return nothing;
    }
    const projectHref = `/projects/${encodeURIComponent(this.projectId)}`;
    const agentHref = `/agents/${encodeURIComponent(this.agentId)}`;
    const projectName = projectDisplayName({
      name: this.agent?.project,
      slug: this.agent?.projectSlug,
    });
    const compactProjectLabel = projectName ? `Back to ${projectName}` : 'Back to Project';
    if (this.compactHeader) {
      return html`${this.projectId
          ? html`<sl-tooltip content=${compactProjectLabel}>
              <a href=${projectHref} class="back-link compact" aria-label=${compactProjectLabel}
                ><sl-icon name="folder" aria-hidden="true"></sl-icon
              ></a>
            </sl-tooltip>`
          : nothing}
        <sl-tooltip content="Back to Agent">
          <a href=${agentHref} class="back-link compact" aria-label="Back to Agent"
            ><sl-icon name="cpu" aria-hidden="true"></sl-icon
          ></a>
        </sl-tooltip>`;
    }
    const crumbs: Crumb[] = [];
    if (this.projectId) {
      crumbs.push({
        label: projectName || this.projectId,
        href: projectHref,
        truncate: true,
      });
    }
    crumbs.push({
      label: this.agentName || this.agentId,
      href: agentHref,
      truncate: true,
    });
    return html`<scion-breadcrumb class="toolbar-crumbs" .crumbs=${crumbs}></scion-breadcrumb>`;
  }

  /** The agent name in the header, with its length for the .agent-name minimum width. */
  private renderAgentName(name: string) {
    return html`<span class="agent-name" style="--agent-name-chars: ${[...name].length}"
      >${name}</span
    >`;
  }

  override render() {
    if (this.loading) {
      return html`
        <div class="toolbar">${this.renderBackLinks()}</div>
        <div class="loading-state">
          <div class="spinner"></div>
          <p>Connecting to agent...</p>
        </div>
      `;
    }

    // Metadata availability must not remove the host during independent PTY setup.
    if (this.session?.state.error && !this.terminal) {
      const suspendHint =
        this.isSuspended && this.agent ? suspendReasonHint(this.agent) : undefined;
      return html`
        <div class="toolbar">
          ${this.renderBackLinks()}
          ${this.agentName
            ? html`
                <div class="separator"></div>
                ${this.renderAgentName(this.agentName)}
              `
            : ''}
        </div>
        <div class="error-state">
          <div
            class="resume-progress ${this.isResumingOrStarting ? '' : 'idle'}"
            role="status"
            aria-live="polite"
            aria-atomic="true"
          >
            ${this.isResumingOrStarting
              ? html`
                  <div class="spinner"></div>
                  <p class="resume-status-title">
                    ${this.resumeState === 'resuming' ? 'Resuming Agent' : 'Starting Agent'}
                  </p>
                  <div class="error-detail">
                    ${this.resumeState === 'resuming'
                      ? 'Sending resume request...'
                      : 'Waiting for agent to start. Terminal will connect automatically.'}
                  </div>
                `
              : nothing}
          </div>
          ${this.isResumingOrStarting
            ? nothing
            : html`
                <p>${this.isUnavailableState ? 'Agent Unavailable' : 'Terminal Unavailable'}</p>
                <div class="error-detail">${this.error}</div>
                ${suspendHint ? html`<div class="suspend-hint">${suspendHint}</div>` : nothing}
              `}
          <div
            class="resume-error ${!this.isResumingOrStarting && this.resumeError ? '' : 'idle'}"
            role="alert"
            data-testid="terminal-resume-error"
          >
            ${!this.isResumingOrStarting && this.resumeError ? this.resumeError : nothing}
          </div>
          ${this.isResumingOrStarting
            ? nothing
            : html`
                ${this.showResumeUnauthorizedHint
                  ? html`<div
                      class="resume-unauthorized"
                      data-testid="terminal-resume-unauthorized"
                    >
                      Only the agent's owner or a project owner/admin can resume it.
                    </div>`
                  : nothing}
                ${this.canResumeAgent
                  ? html`
                      <div class="error-actions">
                        <sl-button
                          variant="success"
                          size="small"
                          data-testid="terminal-resume-btn"
                          @click=${() => void this.handleResume()}
                        >
                          <sl-icon slot="prefix" name="play-circle"></sl-icon>
                          Resume
                        </sl-button>
                        <button
                          class="secondary-retry-btn"
                          ?disabled=${this.reconnectDisabled}
                          @click=${() => this.handleReconnect()}
                        >
                          ${this.reconnectInProgress ? 'Reconnecting...' : 'Retry'}
                        </button>
                      </div>
                    `
                  : html`
                      <button
                        ?disabled=${this.reconnectDisabled}
                        @click=${() => this.handleReconnect()}
                      >
                        ${this.reconnectInProgress ? 'Reconnecting...' : 'Retry'}
                      </button>
                    `}
              `}
        </div>
      `;
    }

    // prettier-ignore
    return html`
      <div class="toolbar">
        ${this.renderBackLinks()}
        <div class="separator"></div>
        ${this.renderAgentName(this.agentName || this.agentId)}
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
        ${this.touchPrimary.isTouch
          ? html`<button
              class="pane-action-btn key-bar-toggle"
              title=${this.keyBarHidden ? 'Show terminal keys' : 'Hide terminal keys'}
              aria-label="Terminal keys"
              aria-pressed=${String(!this.keyBarHidden)}
              @mousedown=${this.keepFocus}
              @click=${() => this.toggleKeyBar()}
            >
              <sl-icon name="keyboard"></sl-icon>
            </button>`
          : nothing}
        <div class="spacer"></div>
        ${this.renderPortButtons()}
        ${this.showCaptureAuth
          ? html`
              <button
                class="capture-auth-btn"
                ?disabled=${this.captureAuthLoading}
                @click=${() => void this.openCaptureAuthScopeDialog()}
                title="Capture credentials from inside the container"
              >
                ${this.captureAuthLoading ? 'Capturing...' : 'Capture Auth'}
              </button>
            `
          : ''}
        ${this.agent
          ? agentStatusBadge(this.agent, {
              status: this.agentDisplayStatus,
              size: 'small',
            })
          : html`<scion-status-badge
              status=${this.agentDisplayStatus}
              tooltip=${ifDefined(activityTooltip(this.agentDisplayStatus))}
              size="small"
            ></scion-status-badge>`}
        <div class="status-indicator">
          <span class="status-dot ${this.connected ? 'connected' : ''}"></span>
          ${this.connected ? 'Connected' : this.idle ? 'Not connected' : 'Disconnected'}
        </div>
        ${!this.connected && !this.idle
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
      <div class="stopping-notice ${this.agentStopping ? '' : 'idle'}" role="status">
        ${this.agentStopping ? 'Agent is stopping…' : nothing}
      </div>
      ${this.error
        ? html`
            <div class="error-banner">
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
        ${this.idle
          ? html`<div class="idle-overlay">
              <span class="overlay-title">Not connected.</span>
              <span class="overlay-detail">Select this terminal to connect.</span>
            </div>`
          : ''}
        ${!this.connected && this.wasConnected
          ? html`<div
              class="disconnected-overlay ${this.isUnavailableState ? 'unavailable' : ''} ${this
                .attempting || this.isResumingOrStarting
                ? 'reconnecting'
                : ''}"
            >
              ${this.attempting || this.isResumingOrStarting
                ? html`<sl-spinner></sl-spinner>`
                : nothing}
              <div
                class="overlay-resume-status ${this.isResumingOrStarting ? '' : 'idle'}"
                role="status"
                aria-live="polite"
                aria-atomic="true"
              >
                ${this.isResumingOrStarting
                  ? html`<span class="overlay-title">${this.overlayTitle}</span>
                      ${this.overlayDetail
                        ? html`<span class="overlay-detail">${this.overlayDetail}</span>`
                        : nothing}`
                  : nothing}
              </div>
              ${this.isResumingOrStarting
                ? nothing
                : html`<span class="overlay-title">${this.overlayTitle}</span>
                    ${this.overlayDetail
                      ? html`<span class="overlay-detail">${this.overlayDetail}</span>`
                      : nothing}`}
              ${!this.attempting && !this.isResumingOrStarting && this.isSuspended && this.agent
                ? (() => {
                    const hint = suspendReasonHint(this.agent);
                    return hint ? html`<span class="overlay-detail">${hint}</span>` : nothing;
                  })()
                : nothing}
              <span
                class="overlay-resume-error ${!this.attempting &&
                !this.isResumingOrStarting &&
                this.resumeError
                  ? ''
                  : 'idle'}"
                role="alert"
                data-testid="terminal-resume-error"
                >${!this.attempting && !this.isResumingOrStarting && this.resumeError
                  ? this.resumeError
                  : nothing}</span
              >
              ${!this.attempting && !this.isResumingOrStarting && this.showResumeUnauthorizedHint
                ? html`<span class="overlay-detail" data-testid="terminal-resume-unauthorized"
                    >Only the agent's owner or a project owner/admin can resume it.</span
                  >`
                : nothing}
              ${this.isResumingOrStarting
                ? nothing
                : this.canResumeAgent && !this.attempting
                  ? html`<div class="overlay-actions">
                      <sl-button
                        variant="success"
                        size="small"
                        data-testid="terminal-resume-btn"
                        @click=${() => void this.handleResume()}
                      >
                        <sl-icon slot="prefix" name="play-circle"></sl-icon>
                        Resume
                      </sl-button>
                      <button
                        class="overlay-reconnect"
                        ?disabled=${this.reconnectDisabled}
                        @click=${() => this.handleReconnect()}
                      >
                        Reconnect
                      </button>
                    </div>`
                  : html`<button
                      class="overlay-reconnect"
                      ?disabled=${this.reconnectDisabled}
                      @click=${() => this.handleReconnect()}
                    >
                      ${this.attempting ? 'Reconnecting...' : 'Reconnect'}
                    </button>`}
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
      ${this.renderKeyBar()} ${this.renderCaptureAuthConflictDialog()}
      ${this.renderCaptureAuthScopeDialog()}
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
          @sl-change=${(e: Event) => {
            this.captureAuthSelectedScope = (e.target as HTMLInputElement)
              .value as CaptureAuthScope;
          }}
        >
          <sl-radio
            value="project"
            ?disabled=${this.captureAuthSettingsLoading || this.agentSecretsUserScopeOnly}
            >Project secret (all project agents)</sl-radio
          >
          ${this.agentSecretsUserScopeOnly
            ? html`<div class="capture-scope-restricted-hint">
                Disabled by your hub administrator: captured credentials can only be stored in your
                profile.
              </div>`
            : nothing}
          <sl-radio value="user" ?disabled=${this.captureAuthSettingsLoading}
            >Profile secret (your personal credential)</sl-radio
          >
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
          ?disabled=${this.captureAuthSettingsLoading}
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
