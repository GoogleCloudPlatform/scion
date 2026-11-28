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
 * Agent detail page component
 *
 * Header + Tab layout (Status / Configuration / Logs) as specified in
 * .design/hosted/agent-detail-layout.md
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type {
  PageData,
  Agent,
  AgentAppliedConfig,
  AgentLaunch,
  AgentInlineConfig,
  TelemetryConfig,
  GCPIdentityConfig,
  GCPServiceAccount,
  Project,
  Notification,
  Subscription,
  AgentMetricsSummary,
  RuntimeBroker,
} from '../../shared/types.js';
import type { AgentLifecycleAction } from '../../shared/types.js';
import {
  can,
  canLifecycle,
  canMessageAgent,
  isTerminalAvailable,
  isAgentRunning,
  isAgentLaunching,
  isIncompleteCreate,
  isFailedStartLaunch,
  RESUME_BEST_EFFORT_CONFIRM_MESSAGE,
  lifecycleActionRequestInit,
} from '../../shared/types.js';
import { agentParentTrail, current, type Named } from '../../client/crumbs.js';
import { projectDisplayName, projectLabelNow } from '../../client/project-label.js';
import { isZeroDurationLimit } from '../../shared/duration-limit.js';

const IDLE_SUSPEND_ACTIVITIES = new Set<string>(['idle', 'completed', 'harness_error']);

interface AgentNotificationsResponse {
  userNotifications: Notification[];
  agentNotifications: Notification[];
}

export interface AgentConversationSummary {
  id: string;
  projectId?: string;
  kind: string;
  surface: string;
  externalRef?: string;
  displayName?: string;
  threadName?: string;
  dmPeer?: {
    kind: string;
    id: string;
    name?: string;
  };
  lastActivityAt: string;
  createdAt?: string;
}

interface AgentConversationsResponse {
  conversations?: AgentConversationSummary[];
}
import {
  agentStatusBadge,
  stateLabel,
  suspendReasonHint,
} from '../../shared/agent-state-display.js';
import {
  canForceSuspend,
  isNeverSuspendActive,
  neverSuspendBadge,
  neverSuspendBadgeStyles,
  resolveSuspendAction,
  showSuspendAction,
} from '../../shared/never-suspend.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { showPendingSecretsStartHint } from '../../client/pending-secrets.js';
import { agentPlacementView } from './agent-placement.js';
import {
  gcpModeLabel,
  gcpModeVariant,
  gcpVerificationDisplay,
  matchingAccount,
} from '../../shared/gcp-identity-display.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { stateManager } from '../../client/state.js';
import type { AgentsChangedDetail } from '../../client/state.js';
import { AgentSeedEpoch } from '../../client/agent-seed-epoch.js';
import '../shared/status-badge.js';
import '../shared/detail-header.js';
import { DeletionLeaseController } from '../shared/deletion-badge.js';
import '../shared/authority-badge.js';
import '../shared/deletion-banner.js';
import { runAgentDelete, lifecycleActionErrorMessage } from '../../client/agent-delete.js';
import { showResponseBodyWarnings } from '../../client/response-warnings.js';
import type { AgentDeleteRequest } from '../../client/agent-delete.js';
import '../shared/message-mode-badge.js';
import '../shared/messageability-indicator.js';
import {
  getDenialMessage,
  getMessageModeDisplay,
  MESSAGE_MODE_DISPLAY,
} from '../../shared/message-mode.js';
import type { DeletionInfo, MessageMode, AgentMessageabilityDetail } from '../../shared/types.js';
import '../shared/agent-log-viewer.js';
import type { ScionAgentLogViewer } from '../shared/agent-log-viewer.js';
import '../shared/agent-message-viewer.js';
import type { ScionAgentMessageViewer } from '../shared/agent-message-viewer.js';
import '../shared/agent-schedules-tab.js';
import type { ScionAgentSchedulesTab } from '../shared/agent-schedules-tab.js';
import '../shared/agent-usage-panel.js';
import type { ScionAgentUsagePanel } from '../shared/agent-usage-panel.js';
import '../shared/agent-trajectory-panel.js';
import type { ScionAgentTrajectoryPanel } from '../shared/agent-trajectory-panel.js';
import '../shared/agent-suspend-screen.js';
import { conversationChatPath } from '../../client/chat-routes.js';
import {
  AGENT_SECRETS_FLAG,
  execContainersEnabled,
  FEATURE_FLAGS_CHANGED_EVENT,
  idleSuspendEnabled,
  isFeatureEnabled,
  neverSuspendEnabled,
  trajectoryCaptureEnabled,
  usageLedgerEnabled,
} from '../../utils/feature-flags.js';
import {
  formatTrajectoryCaptureNotice,
  isTrajectoryCaptureEffective,
} from '../../shared/trajectory-capture-notice.js';
import '../shared/secret-list.js';
import {
  REINCARNATE_PREPARE_MESSAGE,
  reincarnatePrepareAvailability,
} from '../../shared/agent-reincarnate.js';
import '../shared/reincarnate-dialog.js';
import type { ReincarnateChoiceDetail } from '../shared/reincarnate-dialog.js';
import '../shared/hash-display.js';
import '../shared/quick-message-dialog.js';
import '../shared/cascade-mode-dialog.js';
import '../shared/effective-role-provenance.js';
import '../shared/effective-access-boundary-notice.js';
import { showToast } from '../../utils/toast.js';
import { showConfirm } from '../shared/confirm-dialog.js';
import { terminalHref } from '../../client/open-terminal.js';
import {
  getAgentPageCurrentAgent,
  setAgentPageCurrentAgent,
} from '../../client/terminal-workspace-events.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { formatNumber } from '../../utils/format-number.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { DEFAULT_TRIGGERS } from '../../shared/notification-triggers.js';
import { navigateTo, stripBasePath } from '../../client/navigation.js';
import { visibleInterval } from '../../client/visible-interval.js';
import {
  launchCodeLabel,
  launchFailedTitle,
  launchKindTitle,
  launchStepLabel,
} from '../../shared/agent-launch-labels.js';

/** Display text for a launch failure cause (api.BrokerRuntimeCause*). */
function launchCauseLabel(cause: string): string {
  return cause.replace(/_/g, ' ');
}

/**
 * Display text for how a launch ended (store.LaunchEndReason*). The hub omits
 * an empty reason rather than sending null, but a missing, null or empty
 * reason all mean "no reason" here.
 */
function launchEndReasonLabel(endReason?: string | null): string | undefined {
  if (!endReason) return undefined;
  switch (endReason) {
    case 'failed':
      return 'failed';
    case 'timed_out':
      return 'timed out';
    case 'lost':
      return 'broker lost';
    default:
      return endReason.replace(/_/g, ' ');
  }
}

/**
 * Parse a Go-style duration string (e.g. "2h30m", "1h", "45m", "90s") into
 * total seconds. Returns 0 if the string cannot be parsed.
 */
function parseDuration(s: string): number {
  if (!s) return 0;
  let total = 0;
  const re = /(\d+(?:\.\d+)?)\s*(h|m|s)/g;
  let match: RegExpExecArray | null;
  while ((match = re.exec(s)) !== null) {
    const val = parseFloat(match[1]);
    switch (match[2]) {
      case 'h':
        total += val * 3600;
        break;
      case 'm':
        total += val * 60;
        break;
      case 's':
        total += val;
        break;
    }
  }
  return total;
}

/**
 * How long to show the client-side "deleted" state before SPA-navigating
 * away, after a successful delete (or an SSE `deleted` event for this
 * agent). Exported (rather than a magic number) so tests can reason about
 * it and use fake timers.
 */
export const DELETE_REDIRECT_DELAY_MS = 1000;

/**
 * Format seconds as "Xh Ym Zs".
 */
function formatDurationHMS(totalSeconds: number): string {
  if (totalSeconds <= 0) return '0s';
  const h = Math.floor(totalSeconds / 3600);
  const m = Math.floor((totalSeconds % 3600) / 60);
  const s = Math.floor(totalSeconds % 60);
  const parts: string[] = [];
  if (h > 0) parts.push(`${h}h`);
  if (m > 0) parts.push(`${m}m`);
  if (s > 0 || parts.length === 0) parts.push(`${s}s`);
  return parts.join(' ');
}

/**
 * A stored config limit as Limits card text, matching the Edit page: a count
 * limit of 0 or any Go zero-duration spelling ("0", "0s", "0m", "0h", etc.) is
 * Unlimited; an absent limit is Inherited (the hub fills it from the template,
 * hub or broker defaults); otherwise the stored value.
 */
function formatConfigLimit(value: number | string | undefined): string {
  if (value == null || value === '') return 'Inherited';
  if (value === 0 || isZeroDurationLimit(value)) return 'Unlimited';
  return String(value);
}

@customElement('scion-page-agent-detail')
export class ScionPageAgentDetail extends LitElement {
  /** Re-renders absolute times when the display timezone changes. */
  readonly _zone = new DisplayZoneController(this);

  /** Lease timer for this agent's delete view (ptone/scion#2483 N4). */
  private readonly deletionLease = new DeletionLeaseController(this, () =>
    this.agent ? [this.agent] : []
  );

  @property({ type: Object })
  pageData: PageData | null = null;

  @property({ type: String })
  agentId = '';

  @state()
  private loading = true;

  @state()
  private agent: Agent | null = null;

  @state()
  private project: Project | null = null;

  /**
   * The Runtime Broker row of a pinned (flat) agent's Runtime Broker, for the
   * placement card's target display name and connection status. Null for an
   * unpinned agent, before it loads, or when it cannot be read.
   */
  @state()
  private placementBroker: RuntimeBroker | null = null;

  @state()
  private error: string | null = null;

  @state()
  private actionLoading: Record<string, boolean> = {};

  /**
   * True once this agent has been deleted (either by this page or by an
   * SSE `deleted` event). Shows a brief client-side-only "deleted" view
   * before the SPA redirect fires.
   */
  @state()
  private deleted = false;

  @state()
  private userNotifications: Notification[] = [];

  @state()
  private agentNotifications: Notification[] = [];

  @state()
  private subscribed = false;

  @state()
  private subscriptionId: string | null = null;

  @state()
  private subscriptionLoading = false;

  @state()
  private quickMessageOpen = false;

  @state()
  private cascadeDialogOpen = false;

  /**
   * True while a confirmed reincarnate request is in flight; drives the
   * button's loading state (ptone/scion#3707).
   */
  @state()
  private reincarnating = false;

  /**
   * Double-submit guard: true from the first click until the request
   * settles, including while the confirm dialog is open. Not rendered, so
   * no spinner shows behind the dialog.
   */
  private reincarnateBusy = false;

  /** The hub's error text from the last failed reincarnate, if any. */
  @state()
  private reincarnateError: string | null = null;

  /** Whether the Reincarnate choice dialog is open. */
  @state()
  private reincarnateDialogOpen = false;

  /** Inline confirmation shown after "Ask the agent to prepare" succeeds. */
  @state()
  private reincarnatePrepareSent = false;

  /** Whether the Conversations|Log toggle is in "conversations" mode (vs "log" mode). */
  @state()
  private chatViewActive = true;

  /** Shared conversations between the viewing user and this agent. */
  @state()
  private sharedConversations: AgentConversationSummary[] = [];

  @state()
  private sharedConversationsLoading = false;

  @state()
  private sharedConversationsLoaded = false;

  @state()
  private sharedConversationsError: string | null = null;

  private sharedConversationsReqSeq = 0;

  private activeTab = 'status';

  /** Current user ID for constructing the DM conversation key. */
  @state()
  private currentUserId = '';

  /** Whether the native chat feature flag is enabled. */
  private get nativeChatEnabled(): boolean {
    return isFeatureEnabled('web.native_chat');
  }

  /** Whether the agent-scoped secrets experiment is enabled. */
  private get agentSecretsEnabled(): boolean {
    return isFeatureEnabled(AGENT_SECRETS_FLAG);
  }

  private get usageLedgerEnabled(): boolean {
    return usageLedgerEnabled();
  }

  private get trajectoryCaptureEnabled(): boolean {
    return trajectoryCaptureEnabled();
  }

  private get trajectoryTabVisible(): boolean {
    return trajectoryCaptureEnabled() && can(this.project?._capabilities, 'trajectory_read');
  }

  /** Whether an agent uses the built-in shell execution container harness. */
  private isShellAgent(agent: Agent | null | undefined): boolean {
    if (!agent) return false;
    const hc = agent.harnessConfig || agent.appliedConfig?.harnessConfig;
    return hc === 'shell';
  }

  private readonly boundOnFeatureFlagsChanged = (): void => {
    this.requestUpdate();
  };

  @state()
  private metricsSummary: AgentMetricsSummary | null = null;

  /**
   * Registered record of the agent's assigned GCP service account, for the
   * GCP Identity card's display name and verification status
   * (ptone/scion#4017). Null until loaded, or when it cannot be loaded.
   */
  @state()
  private gcpServiceAccount: GCPServiceAccount | null = null;

  /** True while the record above is being fetched. */
  @state()
  private gcpServiceAccountLoading = false;

  /** `projectId/serviceAccountId` the record above was requested for; '' for none. */
  private gcpServiceAccountKey = '';

  static override styles = [
    neverSuspendBadgeStyles,
    css`
      :host {
        display: block;
      }

      /* ---- Header (layout in scion-detail-header) ---- */
      .header-labels {
        display: inline-flex;
        flex-wrap: wrap;
        align-items: center;
        gap: 0.35em;
        min-width: 0;
        max-width: 100%;
      }
      .header-labels sl-tag {
        max-width: 100%;
      }
      .header-labels sl-tag::part(base) {
        max-width: min(20rem, 100%);
        overflow: hidden;
      }
      .header-labels sl-tag::part(content) {
        display: block;
        min-width: 0;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      .header-labels-tooltip {
        display: flex;
        flex-direction: column;
        gap: 0.125rem;
      }
      .header-meta {
        display: flex;
        flex-wrap: wrap;
        align-items: center;
        gap: 0.5rem 1rem;
        margin-top: 0.5rem;
      }
      .template-badge {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        padding: 0.25rem 0.75rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        border-radius: var(--scion-radius, 0.5rem);
        font-size: 0.875rem;
        color: var(--scion-text-muted, #64748b);
        /* One long template name breaks inside its own item. */
        min-width: 0;
        overflow-wrap: anywhere;
      }
      .project-link,
      .broker-link {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        color: var(--scion-text-muted, #64748b);
        text-decoration: none;
        font-size: 0.875rem;
        /* One long project or broker name breaks inside its own item. */
        min-width: 0;
        overflow-wrap: anywhere;
      }
      /* The icon keeps its size when a long name wraps beside it. */
      .template-badge sl-icon,
      .project-link sl-icon,
      .broker-link sl-icon {
        flex-shrink: 0;
      }
      .exec-container-badge sl-icon {
        margin-right: 0.25rem;
      }
      .project-link:hover,
      .broker-link:hover {
        color: var(--scion-primary, #3b82f6);
      }
      /* ---- Error banner ---- */
      scion-deletion-banner.deletion-banner {
        margin-bottom: 1.5rem;
      }

      .agent-error-banner {
        background: var(--sl-color-danger-50, #fef2f2);
        border: 1px solid var(--sl-color-danger-200, #fecaca);
        border-radius: var(--scion-radius-lg, 0.75rem);
        padding: 1rem 1.25rem;
        margin-bottom: 1.5rem;
        display: flex;
        align-items: flex-start;
        gap: 0.75rem;
      }
      .agent-error-banner sl-icon {
        color: var(--sl-color-danger-500, #ef4444);
        font-size: 1.25rem;
        flex-shrink: 0;
        margin-top: 0.125rem;
      }
      .agent-error-banner .error-content {
        flex: 1;
        min-width: 0;
      }
      .agent-error-banner .error-title {
        font-weight: 600;
        color: var(--sl-color-danger-700, #b91c1c);
        margin-bottom: 0.25rem;
      }
      .agent-error-banner .error-message {
        font-size: 0.875rem;
        color: var(--sl-color-danger-600, #dc2626);
        font-family: var(--scion-font-mono, monospace);
        white-space: pre-wrap;
        word-break: break-word;
      }
      .agent-error-banner .error-code {
        font-size: 0.8125rem;
        font-family: var(--scion-font-mono, monospace);
        color: var(--sl-color-danger-700, #b91c1c);
        margin-bottom: 0.25rem;
      }
      .agent-error-banner .error-cause,
      .agent-error-banner .error-step {
        font-size: 0.8125rem;
        color: var(--sl-color-danger-700, #b91c1c);
        margin-bottom: 0.25rem;
      }
      .agent-error-banner .error-hint {
        font-size: 0.875rem;
        color: var(--scion-text-secondary, #475569);
        margin-top: 0.5rem;
      }
      .agent-error-banner .error-recreate {
        display: grid;
        grid-template-columns: max-content 1fr;
        gap: 0.25rem 0.75rem;
        margin: 0.5rem 0 0;
        font-size: 0.875rem;
      }
      .agent-error-banner .error-recreate dt {
        font-weight: 600;
        color: var(--scion-text-secondary, #475569);
      }
      .agent-error-banner .error-recreate dd {
        margin: 0;
        white-space: pre-wrap;
        word-break: break-word;
      }
      .agent-error-banner .error-actions {
        display: flex;
        gap: 0.5rem;
        margin-top: 0.75rem;
      }
      .agent-error-banner .error-actions sl-icon {
        color: inherit;
        font-size: inherit;
        margin-top: 0;
      }
      .agent-launch-banner {
        background: var(--sl-color-primary-50, #eff6ff);
        border: 1px solid var(--sl-color-primary-200, #bfdbfe);
        border-radius: var(--scion-radius-lg, 0.75rem);
        padding: 1rem 1.25rem;
        margin-bottom: 1.5rem;
        display: flex;
        align-items: flex-start;
        gap: 0.75rem;
      }
      .agent-launch-banner sl-spinner {
        font-size: 1.25rem;
        flex-shrink: 0;
        margin-top: 0.125rem;
      }
      .agent-launch-banner .launch-content {
        flex: 1;
        min-width: 0;
      }
      .agent-launch-banner .launch-title {
        font-weight: 600;
        color: var(--sl-color-primary-700, #1d4ed8);
        margin-bottom: 0.25rem;
      }
      .agent-launch-banner .launch-step {
        font-size: 0.875rem;
        font-family: var(--scion-font-mono, monospace);
        color: var(--sl-color-primary-700, #1d4ed8);
      }
      .agent-launch-banner .launch-message {
        font-size: 0.875rem;
        color: var(--scion-text-secondary, #475569);
        white-space: pre-wrap;
        word-break: break-word;
      }
      .trajectory-capture-banner {
        display: flex;
        align-items: center;
        gap: 0.625rem;
        padding: 0.625rem 1rem;
        margin-bottom: 1rem;
        background: var(--scion-bg-subtle);
        border: 1px solid var(--scion-border);
        border-radius: var(--scion-radius);
        font-size: 0.8125rem;
        color: var(--scion-text-secondary);
      }
      .trajectory-capture-banner sl-icon {
        flex-shrink: 0;
        font-size: 1rem;
        color: var(--scion-text-muted);
      }
      .trajectory-capture-banner.effective sl-icon {
        color: var(--scion-primary);
      }

      /* ---- Tabs ---- */
      sl-tab-group {
        --track-color: var(--scion-border, #e2e8f0);
      }
      sl-tab-group::part(body) {
        padding-top: 1.5rem;
      }

      /* ---- Messages tab: shared conversations list ---- */
      .messages-view-bar {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        margin-bottom: 1rem;
      }
      .messages-view-toggle::part(form-control-label) {
        position: absolute;
        width: 1px;
        height: 1px;
        padding: 0;
        margin: -1px;
        overflow: hidden;
        clip: rect(0, 0, 0, 0);
        white-space: nowrap;
        border: 0;
      }
      .shared-conversations-card {
        background: var(--scion-surface);
        border: 1px solid var(--scion-border);
        border-radius: var(--scion-radius-lg);
        overflow: hidden;
      }
      .shared-conversations-list {
        display: flex;
        flex-direction: column;
      }
      .shared-conversation-row {
        display: flex;
        align-items: center;
        gap: 0.75rem;
        padding: 0.875rem 1rem;
        border-bottom: 1px solid var(--scion-border);
        color: var(--scion-text);
        text-decoration: none;
        transition: background-color 0.15s ease;
      }
      .shared-conversation-row:last-child {
        border-bottom: none;
      }
      .shared-conversation-row--link:hover,
      .shared-conversation-row--link:focus-visible {
        background: var(--scion-bg-subtle);
        outline: none;
      }
      .shared-conversation-row--link:focus-visible {
        box-shadow: inset 0 0 0 2px var(--scion-primary);
      }
      .shared-conversation-icon {
        flex-shrink: 0;
        display: flex;
        align-items: center;
        justify-content: center;
        width: 2rem;
        height: 2rem;
        border-radius: var(--scion-radius);
        background: var(--scion-bg-subtle);
        color: var(--scion-text-secondary);
      }
      .shared-conversation-main {
        flex: 1;
        min-width: 0;
        display: flex;
        flex-direction: column;
        gap: 0.25rem;
      }
      .shared-conversation-title-row {
        display: flex;
        align-items: center;
        flex-wrap: wrap;
        gap: 0.5rem;
        min-width: 0;
      }
      .shared-conversation-name {
        font-weight: 500;
        color: var(--scion-text);
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
        min-width: 0;
      }
      .shared-conversation-meta {
        flex-shrink: 0;
        display: flex;
        align-items: center;
        gap: 0.5rem;
        color: var(--scion-text-muted);
        font-size: 0.8125rem;
      }
      .shared-conversation-chevron {
        color: var(--scion-text-muted);
        font-size: 0.875rem;
      }
      .shared-conversations-state {
        display: flex;
        flex-direction: column;
        align-items: center;
        justify-content: center;
        gap: 0.5rem;
        padding: 2rem 1.5rem;
        text-align: center;
        color: var(--scion-text-muted);
      }
      .shared-conversations-state-title {
        font-size: 0.9375rem;
        font-weight: 500;
        color: var(--scion-text);
      }
      .shared-conversations-state-desc {
        font-size: 0.875rem;
        color: var(--scion-text-muted);
        margin: 0;
      }

      /* ---- Cards ---- */
      .reincarnate-help {
        margin: 0 0 1rem;
        color: var(--scion-text-secondary, #475569);
        font-size: 0.875rem;
      }
      .reincarnate-meta {
        margin-bottom: 1rem;
      }
      .reincarnate-error,
      .reincarnate-prepare-sent {
        margin-bottom: 1rem;
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

      /* ---- Info grid ---- */
      .info-grid {
        display: grid;
        grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
        gap: 1.5rem;
      }
      .info-item {
        display: flex;
        flex-direction: column;
      }
      .info-label {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        text-transform: uppercase;
        letter-spacing: 0.05em;
        margin-bottom: 0.25rem;
      }
      .info-value {
        font-size: 1rem;
        color: var(--scion-text, #1e293b);
      }
      .info-value.mono {
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.875rem;
      }
      .info-subvalue {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.125rem;
        word-break: break-all;
      }
      .info-subvalue.mono {
        font-family: var(--scion-font-mono, monospace);
      }
      /* Messaging: the mode select needs more room than an info-grid column
       gives it, so this card wraps instead of letting the select overlap the
       reachability column. */
      .messaging-grid {
        display: flex;
        flex-wrap: wrap;
        gap: 1.5rem;
      }
      .messaging-grid .messaging-mode {
        flex: 1 1 280px;
        min-width: 0;
      }
      .messaging-grid .messaging-reach {
        flex: 1 1 200px;
        min-width: 0;
      }
      /* Cap the select, not its column, so a read-only mode description can
       use the full column width. */
      .messaging-mode sl-select {
        width: 100%;
        max-width: 360px;
      }

      /* ---- Task summary ---- */
      .task-summary {
        font-size: 1rem;
        color: var(--scion-text, #1e293b);
        padding: 1rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        border-radius: var(--scion-radius, 0.5rem);
        white-space: pre-wrap;
        line-height: 1.5;
      }

      /* ---- Telemetry tag list ---- */
      .tag-list {
        display: flex;
        flex-wrap: wrap;
        gap: 0.375rem;
      }
      .tag-item {
        display: inline-block;
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.75rem;
        padding: 0.125rem 0.5rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        color: var(--scion-text, #1e293b);
      }
      .telemetry-section {
        margin-bottom: 1.25rem;
      }
      .telemetry-section:last-child {
        margin-bottom: 0;
      }
      .telemetry-section-title {
        font-size: 0.8rem;
        font-weight: 600;
        color: var(--scion-text-muted, #64748b);
        margin-bottom: 0.5rem;
      }
      .telemetry-empty {
        font-size: 0.875rem;
        color: var(--scion-text-muted, #64748b);
        font-style: italic;
      }

      /* ---- Progress bars ---- */
      .limits-grid {
        display: grid;
        grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
        gap: 1.5rem;
      }
      .limit-item {
        display: flex;
        flex-direction: column;
        gap: 0.5rem;
      }
      .limit-label {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        text-transform: uppercase;
        letter-spacing: 0.05em;
      }
      .limit-value {
        font-size: 1rem;
        color: var(--scion-text, #1e293b);
        font-weight: 500;
      }
      .limit-inherited {
        font-size: 0.875rem;
        font-weight: 400;
        color: var(--scion-text-muted, #64748b);
      }
      .progress-bar-track {
        width: 100%;
        height: 6px;
        background: var(--scion-bg-subtle, #f1f5f9);
        border-radius: var(--scion-radius-full);
        overflow: hidden;
      }
      .progress-bar-fill {
        height: 100%;
        border-radius: var(--scion-radius-full);
        transition: width 0.3s ease;
      }
      .progress-bar-fill.normal {
        background: var(--scion-success-500, #22c55e);
      }
      .progress-bar-fill.warning {
        background: var(--scion-warning-500, #f59e0b);
      }
      .progress-bar-fill.danger {
        background: var(--scion-danger-500, #ef4444);
      }

      /* ---- Notification items ---- */
      .notif-section-title {
        font-size: 0.8125rem;
        font-weight: 600;
        color: var(--scion-text-muted, #64748b);
        text-transform: uppercase;
        letter-spacing: 0.05em;
        margin: 1rem 0 0.5rem 0;
      }
      .notif-section-title:first-of-type {
        margin-top: 0;
      }
      .notif-list-item {
        display: flex;
        gap: 0.625rem;
        padding: 0.625rem 0;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
      }
      .notif-list-item:last-child {
        border-bottom: none;
      }
      .notif-icon {
        flex-shrink: 0;
        display: flex;
        align-items: flex-start;
        padding-top: 2px;
      }
      .notif-icon sl-icon {
        font-size: 1rem;
      }
      .notif-icon.status-success sl-icon {
        color: var(--scion-status-success, #22c55e);
      }
      .notif-icon.status-warning sl-icon {
        color: var(--scion-status-warning, #f59e0b);
      }
      .notif-icon.status-danger sl-icon {
        color: var(--scion-status-danger, #ef4444);
      }
      .notif-icon.status-info sl-icon {
        color: var(--scion-text-muted, #64748b);
      }
      .notif-body {
        flex: 1;
        min-width: 0;
      }
      .notif-message {
        font-size: 0.8125rem;
        line-height: 1.4;
        color: var(--scion-text, #1e293b);
        word-break: break-word;
        display: -webkit-box;
        -webkit-line-clamp: 2;
        -webkit-box-orient: vertical;
        overflow: hidden;
      }
      .notif-truncation-badge {
        display: inline-flex;
        align-items: center;
        justify-content: center;
        padding: 0 0.375rem;
        margin-top: 0.125rem;
        font-size: 0.6875rem;
        font-weight: 700;
        line-height: 1.25rem;
        border-radius: var(--scion-radius);
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
        cursor: pointer;
        letter-spacing: 0.05em;
      }
      .notif-truncation-badge:hover {
        background: var(--scion-border, #e2e8f0);
      }
      .notif-meta {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        margin-top: 0.25rem;
        font-size: 0.6875rem;
        color: var(--scion-text-muted, #64748b);
      }
      .notif-mark-read {
        border: none;
        background: transparent;
        color: var(--scion-text-muted, #64748b);
        font-size: 0.6875rem;
        cursor: pointer;
        padding: 0;
        transition: color 0.15s ease;
      }
      .notif-mark-read:hover {
        color: var(--scion-primary, #3b82f6);
      }
      .notif-empty {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 1.5rem 0;
        color: var(--scion-text-muted, #64748b);
        font-size: 0.8125rem;
      }
      .notif-empty sl-icon {
        font-size: 1.25rem;
        opacity: 0.4;
      }

      /* ---- Loading / Error states ---- */
      .loading-state {
        display: flex;
        flex-direction: column;
        align-items: center;
        justify-content: center;
        padding: 4rem 2rem;
        color: var(--scion-text-muted, #64748b);
      }
      .loading-state sl-spinner {
        font-size: 2rem;
        margin-bottom: 1rem;
      }
      .error-state {
        text-align: center;
        padding: 3rem 2rem;
        background: var(--scion-surface, #ffffff);
        border: 1px solid var(--sl-color-danger-200, #fecaca);
        border-radius: var(--scion-radius-lg, 0.75rem);
      }
      .error-state sl-icon {
        font-size: 3rem;
        color: var(--sl-color-danger-500, #ef4444);
        margin-bottom: 1rem;
      }
      .error-state h2 {
        font-size: 1.25rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
        margin: 0 0 0.5rem 0;
      }
      .error-state p {
        color: var(--scion-text-muted, #64748b);
        margin: 0 0 1rem 0;
      }
      .error-details {
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.875rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        padding: 0.75rem 1rem;
        border-radius: var(--scion-radius, 0.5rem);
        color: var(--sl-color-danger-700, #b91c1c);
        margin-bottom: 1rem;
      }

      /* ---- Port link ---- */
      .port-link {
        color: var(--sl-color-success-600, #16a34a);
        text-decoration: none;
      }
      .port-link:hover {
        text-decoration: underline;
        color: var(--sl-color-success-700, #15803d);
      }
    `,
  ];

  private boundOnAgentsChanged = this.onAgentsChanged.bind(this);
  private boundOnProjectsUpdated = this.onProjectsUpdated.bind(this);
  private boundOnAgentsResync = this.onAgentsResync.bind(this);
  private stopRelativeTimeTick: (() => void) | null = null;
  /**
   * The stateManager project object last merged into `this.project` (or
   * seeded from it). stateManager replaces a project's object on each of
   * its events, so the same reference means nothing changed for it.
   */
  private mergedProjectSource: Project | null = null;
  /**
   * Pending SPA-redirect timer for the deleted state; cancellable on
   * disconnect. `@state()` so `renderDeletedState` re-renders (dropping
   * "Redirecting…") the moment the timer fires or is cleared, not just when
   * `deleted` changes.
   */
  @state()
  private deleteRedirectTimer: ReturnType<typeof setTimeout> | null = null;

  override connectedCallback(): void {
    super.connectedCallback();
    if (!this.agentId && typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/agents\/([^/]+)/);
      if (match) {
        this.agentId = match[1];
      }
    }
    void this.loadData();

    stateManager.addEventListener('agents-changed', this.boundOnAgentsChanged);
    stateManager.addEventListener('projects-updated', this.boundOnProjectsUpdated);
    stateManager.addEventListener('agents-resync', this.boundOnAgentsResync);
    if (typeof window !== 'undefined') {
      window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFeatureFlagsChanged);
    }

    this.stopRelativeTimeTick = visibleInterval(() => this.requestUpdate(), 15000);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.agentId && getAgentPageCurrentAgent()?.agentId === this.agentId) {
      setAgentPageCurrentAgent(null);
    }
    stateManager.removeEventListener('agents-changed', this.boundOnAgentsChanged);
    stateManager.removeEventListener('projects-updated', this.boundOnProjectsUpdated);
    stateManager.removeEventListener('agents-resync', this.boundOnAgentsResync);
    if (typeof window !== 'undefined') {
      window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFeatureFlagsChanged);
    }
    this.stopRelativeTimeTick?.();
    this.stopRelativeTimeTick = null;
    if (this.deleteRedirectTimer) {
      clearTimeout(this.deleteRedirectTimer);
      this.deleteRedirectTimer = null;
    }
  }

  /**
   * React to a coalesced `agents-changed` flush only when it names this
   * page's agent. Other agents in the project scope change often, and
   * merging on their flushes gave `this.agent` a new identity each time,
   * re-rendering the whole page for nothing.
   */
  private onAgentsChanged(event: Event): void {
    // Typed as partial: a malformed or partial payload must not throw here.
    const change = (event as CustomEvent<{ data?: Partial<AgentsChangedDetail> }>).detail?.data;
    const id = this.agentId;
    if (
      !change ||
      !id ||
      !(change.upserted?.includes(id) || change.deleted?.includes(id) || change.unknown?.has(id))
    ) {
      return;
    }
    const updatedAgent = stateManager.getAgent(this.agentId);
    if (updatedAgent && this.agent) {
      const prevPhase = this.agent.phase;
      const prevActivity = this.agent.activity;
      const merged: Agent = { ...this.agent, ...updatedAgent };
      const phaseOrActivityChanged = merged.phase !== prevPhase || merged.activity !== prevActivity;
      if (phaseOrActivityChanged) {
        if (
          merged.phase !== 'running' ||
          !merged.activity ||
          !IDLE_SUSPEND_ACTIVITIES.has(merged.activity)
        ) {
          delete merged.idleSuspendAt;
        }
        if (merged.phase !== 'running') {
          delete merged.activitySince;
        }
      }
      this.agent = merged;
      setAgentPageCurrentAgent({
        agentId: this.agentId,
        name: this.agent.name || this.agentId,
      });
      if (phaseOrActivityChanged && merged.phase === 'running' && !this.deleted) {
        this.backgroundRefresh();
      }
      return;
    }
    // The agent is missing from stateManager's map. That alone does not
    // mean it was deleted: setScope() clears the map before this page's
    // own reseed lands at the end of loadData (which awaits the project,
    // auth/me, notification and subscription fetches first), and
    // removeAgent() prunes stale rows without a tombstone. Either way, a
    // flush naming this agent during that window would otherwise
    // be read as "this agent was deleted". Key on the authoritative
    // tombstone instead of absence. (No `!this.deleted` guard here:
    // `showDeletedStateThenRedirect` already no-ops once `deleted` is true.)
    if (!updatedAgent && this.agent && stateManager.getDeletedAgentIds().has(this.agentId)) {
      this.showDeletedStateThenRedirect();
    }
  }

  /**
   * After an SSE reconnect, changes to this agent during the outage were
   * missed and arrive in no `agents-changed` flush. Refetch it, so a quiet
   * agent does not keep a stale phase until its next change.
   */
  private onAgentsResync(): void {
    if (!this.agent || this.deleted) return;
    this.backgroundRefresh();
  }

  /**
   * True while this element is still attached AND the app's current route
   * is still this agent's detail page. Guards the deferred SPA-redirect in
   * {@link showDeletedStateThenRedirect}: `renderRoute` (main.ts) keeps the
   * previous page connected-but-hidden behind `/terminals`, so
   * `isConnected` alone cannot distinguish "visible" from "hidden behind
   * another route". The base path is stripped the same way the router does
   * (so this works behind a reverse proxy), then the app path must be
   * exactly this agent's route. Trailing slashes are stripped first so
   * `/agents/<id>/` still counts as this agent's route.
   */
  private isOnThisAgentRoute(): boolean {
    if (!this.isConnected || typeof window === 'undefined') return false;
    const appPath = stripBasePath(window.location.pathname).replace(/\/+$/, '');
    return appPath === `/agents/${this.agentId}`;
  }

  /**
   * Parent project for the breadcrumb trail and deleted-state redirect:
   * prefers the fetched `this.project`, falls back to `agent.project` /
   * `agent.projectSlug`, or `null` (which yields the `Agents` root).
   * Deliberately *not* captured at timer schedule time so a project that
   * finishes loading before the 1s redirect still wins.
   */
  private get projectNamed(): Named | null {
    const projectLabel =
      projectDisplayName(this.project) ||
      projectDisplayName({
        name: this.agent?.project,
        slug: this.agent?.projectSlug,
      });
    if (this.project) {
      return {
        id: this.project.id,
        label: projectLabel || this.project.id,
      };
    }
    if (this.agent?.projectId) {
      if (projectLabel) {
        return {
          id: this.agent.projectId,
          label: projectLabel,
        };
      }
      const cached = projectLabelNow(this.agent.projectId);
      if (!cached.pending) {
        return cached;
      }
    }
    return null;
  }

  private get redirectTarget(): string {
    return this.projectNamed ? `/projects/${encodeURIComponent(this.projectNamed.id)}` : '/agents';
  }

  /**
   * Show a brief client-side-only "deleted" state, then SPA-navigate to the
   * project page (or /agents if there is no project). Used both for a
   * delete/force-delete initiated from this page and for an SSE `deleted`
   * event removing this agent elsewhere. The timer is cancellable so a
   * disconnect (or a test using fake timers) does not leak it. The redirect
   * itself only fires while this page is still the active route — see
   * `isOnThisAgentRoute` — so it never pulls the user out of `/terminals`.
   * When the redirect is skipped (or the element is disconnected and
   * reconnected with no timer left to fire), `renderDeletedState` still
   * offers a link out instead of leaving the page on "Redirecting…" forever.
   */
  private showDeletedStateThenRedirect(): void {
    if (this.deleted) return;
    this.deleted = true;
    this.deleteRedirectTimer = setTimeout(() => {
      this.deleteRedirectTimer = null;
      if (this.isOnThisAgentRoute()) {
        navigateTo(this.redirectTarget);
      }
    }, DELETE_REDIRECT_DELAY_MS);
  }

  /**
   * Delete through the shared helper (ptone/scion#2483 phase 2): the header
   * Delete button (`event` for the Alt-key bypass), and the failure
   * banner's Retry (no confirm) and Force. On 204 show the deleted state and
   * redirect. On 202 stay with "Deleting…" (the helper applied the
   * response's view); the SSE `deleted` event then runs
   * `showDeletedStateThenRedirect` through `onAgentsChanged`, as it does for
   * a delete started elsewhere.
   */
  private async deleteAgent(opts: AgentDeleteRequest = {}): Promise<void> {
    if (!this.agent) return;
    const outcome = await runAgentDelete({
      agentId: this.agentId,
      agentName: this.agent.name,
      ...opts,
      onBusy: (busy): void => {
        this.actionLoading = { ...this.actionLoading, delete: busy };
      },
    });
    if (outcome.kind === 'deleted') {
      this.showDeletedStateThenRedirect();
    } else if (outcome.kind === 'failed') {
      showToast(outcome.message);
    }
  }

  /** Failure banner with Retry and Force, under the header. */
  private renderDeletionBanner(): TemplateResult | typeof nothing {
    const agent = this.agent;
    const view = agent ? this.deletionLease.view(agent) : null;
    if (!agent || view?.state !== 'failed') return nothing;
    return html`<scion-deletion-banner
      class="deletion-banner"
      live
      .deletion=${view}
      agent-name=${agent.name}
      ?can-delete=${can(agent._capabilities, 'delete')}
      ?busy=${this.actionLoading['delete'] || false}
      @deletion-retry=${(): void => void this.deleteAgent({ confirm: false })}
      @deletion-force=${(): void => void this.deleteAgent({ force: true })}
    ></scion-deletion-banner>`;
  }

  private onProjectsUpdated(): void {
    if (!this.agent?.projectId || !this.project) return;
    const updatedProject = stateManager.getProject(this.agent.projectId);
    if (!updatedProject) return;
    // `projects-updated` carries no ID. Skip a flush that did not touch
    // this page's project: the same object as the one last merged (or the
    // one loadData seeded, which is `this.project` itself). stateManager
    // never mutates a stored project in place, so the same reference means
    // nothing changed.
    const sameObject =
      updatedProject === this.project || updatedProject === this.mergedProjectSource;
    if (sameObject) return;
    this.mergedProjectSource = updatedProject;
    this.project = { ...this.project, ...updatedProject };
  }

  override willUpdate(changed: Map<PropertyKey, unknown>): void {
    super.willUpdate(changed);
    if (changed.has('agent')) this.syncGCPServiceAccount();
  }

  /**
   * Keeps `gcpServiceAccount` in step with the agent's applied identity.
   * Fetches only when the assigned account changes, so live agent updates
   * do not refetch it.
   */
  private syncGCPServiceAccount(): void {
    const identity = this.agent?.appliedConfig?.gcpIdentity;
    const saId = identity?.metadataMode === 'assign' ? identity.serviceAccountId || '' : '';
    const projectId = this.agent?.projectId || '';
    const key = saId && projectId ? `${projectId}/${saId}` : '';
    if (key === this.gcpServiceAccountKey) return;
    this.gcpServiceAccountKey = key;
    this.gcpServiceAccount = null;
    this.gcpServiceAccountLoading = Boolean(key);
    if (key) void this.loadGCPServiceAccount(projectId, saId, key);
  }

  /**
   * Loads the assigned account through the agent's project. The nested
   * by-id route answers for both project-scoped accounts and hub-scoped
   * accounts usable from the project, which are the accounts an agent can be
   * assigned. Any failure leaves the record null, which the card shows as
   * verification unknown.
   */
  private async loadGCPServiceAccount(projectId: string, saId: string, key: string): Promise<void> {
    try {
      // A refused lookup is shown inline as Unknown, so it must not also
      // raise the global access-denied toast.
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(projectId)}/gcp-service-accounts/${encodeURIComponent(saId)}`,
        { suppressAccessDeniedToast: true }
      );
      if (!res.ok) return;
      const account = (await res.json()) as GCPServiceAccount;
      if (key === this.gcpServiceAccountKey) this.gcpServiceAccount = account;
    } catch {
      // Optional: the card shows verification as unknown.
    } finally {
      if (key === this.gcpServiceAccountKey) this.gcpServiceAccountLoading = false;
    }
  }

  private async loadData(): Promise<void> {
    this.loading = true;
    this.error = null;
    this.sharedConversations = [];
    this.sharedConversationsLoaded = false;
    this.sharedConversationsError = null;
    this.sharedConversationsLoading = false;
    this.sharedConversationsReqSeq++;
    // Opened before the agent request, so a live change that lands while
    // any request below is in flight is re-applied over the agent response
    // when it is seeded.
    let epoch = new AgentSeedEpoch();
    let epochGeneration = stateManager.scopeGeneration;
    const agentId = this.agentId;

    try {
      // Use SSR-prefetched agent data when available to avoid a redundant fetch.
      const ssrAgent = this.pageData?.data as Agent | undefined;
      if (ssrAgent && ssrAgent.id === this.agentId) {
        this.agent = ssrAgent;
      } else {
        const agentResponse = await apiFetch(`/api/v1/agents/${this.agentId}`);

        if (!agentResponse.ok) {
          throw new Error(
            await extractApiError(
              agentResponse,
              `HTTP ${agentResponse.status}: ${agentResponse.statusText}`
            )
          );
        }

        this.agent = (await agentResponse.json()) as Agent;
      }

      if (this.agent.projectId) {
        stateManager.setScope({
          type: 'agent-detail',
          projectId: this.agent.projectId,
          agentId: this.agentId,
        });
      }

      // A scope change (this setScope, or another) discards the epoch's
      // store epoch: reopen it for the requests below.
      if (stateManager.scopeGeneration !== epochGeneration) {
        epoch.close();
        epoch = new AgentSeedEpoch();
        epochGeneration = stateManager.scopeGeneration;
      }

      // Fetch project and notifications in parallel — they are independent.
      const parallel: Promise<void>[] = [];

      // A pinned (flat) agent's placement card reads its Runtime Broker row.
      const pinnedBrokerId = this.agent.pinnedRuntimeTarget?.runtimeBrokerId;
      if (pinnedBrokerId) {
        parallel.push(
          apiFetch(`/api/v1/runtime-brokers/${encodeURIComponent(pinnedBrokerId)}`)
            .then(async (res) => {
              this.placementBroker = res.ok ? ((await res.json()) as RuntimeBroker) : null;
            })
            .catch(() => {
              // The placement card falls back to the stored pin alone.
              this.placementBroker = null;
            })
        );
      }

      if (this.agent.projectId) {
        const projectId = this.agent.projectId;
        parallel.push(
          apiFetch(`/api/v1/projects/${projectId}`)
            .then(async (projectResponse) => {
              if (projectResponse.ok) {
                this.project = (await projectResponse.json()) as Project;
              }
            })
            .catch(() => {
              // Project loading is optional
            })
        );
      }

      // Resolve current user ID for the DM conversation key.
      // Use pageData first, fall back to auth/me endpoint.
      if (this.pageData?.user?.id) {
        this.currentUserId = this.pageData.user.id;
      } else if (!this.currentUserId) {
        parallel.push(
          apiFetch('/api/v1/auth/me')
            .then(async (res) => {
              if (res.ok) {
                const data = (await res.json()) as { id?: string };
                if (data.id) this.currentUserId = data.id;
              }
            })
            .catch(() => {
              // User ID resolution is optional; chat will degrade gracefully
            })
        );
      }

      if (this.pageData?.user) {
        parallel.push(
          apiFetch(`/api/v1/notifications?agentId=${this.agentId}`)
            .then(async (notifRes) => {
              if (notifRes.ok) {
                const data = (await notifRes.json()) as AgentNotificationsResponse;
                this.userNotifications = data.userNotifications ?? [];
                this.agentNotifications = data.agentNotifications ?? [];
              }
            })
            .catch(() => {
              // Notification loading is optional
            })
        );

        // Check if user has a subscription for this agent
        parallel.push(
          apiFetch(`/api/v1/notifications/subscriptions?agentId=${this.agentId}`)
            .then(async (subRes) => {
              if (subRes.ok) {
                const data = (await subRes.json()) as
                  | Subscription[]
                  | { subscriptions?: Subscription[] };
                const subs = Array.isArray(data) ? data : data.subscriptions || [];
                const match = subs.find((s) => s.scope === 'agent' && s.agentId === this.agentId);
                if (match) {
                  this.subscribed = true;
                  this.subscriptionId = match.id;
                }
              }
            })
            .catch(() => {
              // Subscription check is optional
            })
        );
      }

      await Promise.all(parallel);

      // Load metrics summary (non-blocking).
      void this.loadMetricsSummary();

      if (
        this.activeTab === 'messages' &&
        !this.isShellAgent(this.agent) &&
        this.nativeChatEnabled &&
        this.chatViewActive
      ) {
        void this.loadSharedConversations();
      }

      this.seedAgent(this.agent, agentId, epoch, epochGeneration);
      setAgentPageCurrentAgent({
        agentId: this.agentId,
        name: this.agent.name || this.agentId,
      });
      if (this.project) {
        stateManager.seedProjects([this.project]);
        dispatchPageTitle(this, this.agent.name, this.project.name || this.agent.projectId);
      } else {
        dispatchPageTitle(this, this.agent.name, 'Agents');
      }
    } catch (err) {
      console.error('Failed to load agent:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load agent';
    } finally {
      epoch.close();
      this.loading = false;
    }
  }

  private async loadMetricsSummary(): Promise<void> {
    try {
      const res = await apiFetch(`/api/v1/agents/${this.agentId}/metrics/summary`);
      if (res.ok) {
        this.metricsSummary = (await res.json()) as AgentMetricsSummary;
      }
    } catch {
      // Metrics loading is optional — do not block the page.
    }
  }

  private formatDate(dateString: string): string {
    if (this.isZeroDate(dateString)) return '—';
    return formatInstantWithZone(dateString) || dateString;
  }

  /**
   * The Suspend reason row of a suspended agent with a known reason: the
   * reason as `scion list` shows it, with what brings the agent back.
   */
  private renderSuspendReason(agent: Agent) {
    const hint = suspendReasonHint(agent);
    if (!hint) return nothing;
    return html`
      <div class="info-item" data-testid="agent-suspend-reason">
        <span class="info-label">Suspend reason</span>
        <span class="info-value"
          >${agent.suspendReason}
          <span style="color: var(--scion-text-muted, #64748b); font-size: 0.85em;"
            >${hint}</span
          ></span
        >
      </div>
    `;
  }

  /**
   * The Suspend policy row when `lifecycle.never_suspend` is enabled.
   */
  private renderSuspendPolicy(agent: Agent) {
    if (!neverSuspendEnabled()) return nothing;
    const isNever = agent.suspendPolicy === 'never';
    const policy = isNever ? 'never' : 'auto';
    const hint = isNever
      ? 'Skipped by automatic idle/stalled and bulk suspends; manual suspend requires force.'
      : 'Eligible for automatic idle and stalled suspension.';
    return html`
      <div class="info-item" data-testid="agent-suspend-policy">
        <span class="info-label">Suspend policy</span>
        <span class="info-value"
          >${policy}
          <span style="color: var(--scion-text-muted, #64748b); font-size: 0.85em;"
            >${hint}</span
          ></span
        >
      </div>
    `;
  }

  /**
   * Renders an absolute timestamp with a muted relative suffix, or a muted
   * empty-state label when the timestamp is unset or Go's zero time.
   */
  private renderTimestampValue(ts: string | undefined, emptyLabel: string) {
    if (ts && !this.isZeroDate(ts)) {
      return html`${this.formatDate(ts)}<span
          style="color: var(--scion-text-muted, #64748b); font-size: 0.85em; margin-left: 0.5em;"
          >(${this.formatRelativeTime(ts)})</span
        >`;
    }
    return html`<span style="color: var(--scion-text-muted, #64748b);">${emptyLabel}</span>`;
  }

  /**
   * The Container started row inside Status -> Current State.
   */
  private renderContainerStarted(agent: Agent) {
    return html`
      <div class="info-item" data-testid="agent-container-started">
        <span class="info-label">Container started</span>
        <span class="info-value">${this.renderTimestampValue(agent.startedAt, '—')}</span>
      </div>
    `;
  }

  /**
   * Formats a hub-computed idleSuspendAt timestamp as a countdown string,
   * clamping past-due instants (within the 5m sweep cadence) to "Suspends soon".
   */
  private formatSuspendCountdown(iso: string): string {
    const targetMs = new Date(iso).getTime();
    if (Number.isNaN(targetMs)) return '';
    if (targetMs - Date.now() <= 500) {
      return 'Suspends soon';
    }
    return `Suspends ${formatRelative(iso)}`;
  }

  /**
   * The Idle since / Auto-suspend countdown row inside Status -> Current State.
   * Shows "Idle since …" when running in a suspend-eligible activity with
   * activitySince, paired with "Suspends in …" / "Suspends soon" when the hub
   * supplies idleSuspendAt, "Never suspends" when never-suspend applies, or no
   * countdown when idleSuspendAt is absent.
   */
  private renderIdleSuspendStatus(agent: Agent) {
    const isRunning = agent.phase === 'running';
    if (!isRunning) return nothing;

    const isIdleActivity = Boolean(agent.activity && IDLE_SUSPEND_ACTIVITIES.has(agent.activity));
    const hasActivitySince = Boolean(agent.activitySince && !this.isZeroDate(agent.activitySince));
    const hasIdleSuspendAt = Boolean(agent.idleSuspendAt && !this.isZeroDate(agent.idleSuspendAt));
    const showIdleSince = hasActivitySince && (isIdleActivity || hasIdleSuspendAt);
    const isNeverSuspend = isNeverSuspendActive(agent);

    const countdownText = isNeverSuspend
      ? 'Never suspends'
      : hasIdleSuspendAt
        ? this.formatSuspendCountdown(agent.idleSuspendAt!)
        : '';

    if (!showIdleSince && !countdownText) return nothing;

    const countdownTitle =
      hasIdleSuspendAt && !isNeverSuspend ? this.formatDate(agent.idleSuspendAt!) : '';

    return html`
      <div class="info-item" data-testid="agent-idle-suspend">
        <span class="info-label">Auto-suspend</span>
        <span class="info-value">
          ${showIdleSince
            ? html`<span title=${this.formatDate(agent.activitySince!)}
                >Idle since ${this.formatRelativeTime(agent.activitySince!)}</span
              >`
            : nothing}${showIdleSince && countdownText
            ? html`<span style="color: var(--scion-text-muted, #64748b); margin: 0 0.35em;"
                >·</span
              >`
            : nothing}${countdownText
            ? html`<span
                title=${countdownTitle}
                style=${isNeverSuspend ? 'color: var(--scion-text-muted, #64748b);' : ''}
                >${countdownText}</span
              >`
            : nothing}
        </span>
      </div>
    `;
  }

  private isZeroDate(dateString: string): boolean {
    if (!dateString) return true;
    const date = new Date(dateString);
    return isNaN(date.getTime()) || date.getFullYear() < 2000;
  }

  private formatRelativeTime(dateString: string): string {
    if (this.isZeroDate(dateString)) return '—';
    return formatRelative(dateString);
  }

  private async handleAction(action: AgentLifecycleAction, event?: MouseEvent): Promise<void> {
    if (!this.agent) return;

    if (action === 'suspend') {
      const resolved = await resolveSuspendAction(this.agent);
      if (!resolved) return;
      action = resolved;
    }

    if (action === 'force-resume') {
      if (
        !(await showConfirm(RESUME_BEST_EFFORT_CONFIRM_MESSAGE, {
          title: 'Resume (best effort)',
          confirmText: 'Resume',
          variant: 'primary',
        }))
      ) {
        return;
      }
    }

    if (action === 'delete') {
      await this.deleteAgent({ event });
      return;
    }

    const optimisticPhase: Record<string, string> = {
      start: 'starting',
      stop: 'stopping',
      suspend: 'stopping',
      'force-suspend': 'stopping',
      resume: 'starting',
      'force-resume': 'starting',
    };
    this.agent = {
      ...this.agent,
      phase: optimisticPhase[action] as Agent['phase'],
    };

    const actionUrls: Record<string, string> = {
      start: `/api/v1/agents/${this.agentId}/start`,
      stop: `/api/v1/agents/${this.agentId}/stop`,
      suspend: `/api/v1/agents/${this.agentId}/suspend`,
      'force-suspend': `/api/v1/agents/${this.agentId}/suspend`,
      resume: `/api/v1/agents/${this.agentId}/start`,
      'force-resume': `/api/v1/agents/${this.agentId}/start`,
    };

    try {
      const response = await apiFetch(actionUrls[action], lifecycleActionRequestInit(action));

      if (!response.ok) {
        throw new Error(await lifecycleActionErrorMessage(response, `Failed to ${action} agent`));
      }
      if (actionUrls[action].endsWith('/start')) {
        // The hub's start warnings (for example, a token issued without the
        // artifact scopes).
        await showResponseBodyWarnings(response);
        void showPendingSecretsStartHint();
      }

      this.backgroundRefresh();
    } catch (err) {
      console.error(`Failed to ${action} agent:`, err);
      showToast(err instanceof Error ? err.message : `Failed to ${action} agent`);
      this.backgroundRefresh();
    }
  }

  /**
   * "Keep suspended" for an agent the hub suspended (reason idle or stalled)
   * while the lifecycle.idle_suspend experiment is on: suspending it again
   * makes the hub rewrite the reason to user in place, so new work no longer
   * wakes it (ptone/scion#7826). The hub checks the experiment; the flag here
   * only decides whether to offer the action.
   */
  private renderKeepSuspended(agent: Agent) {
    const hubSuspended = agent.suspendReason === 'idle' || agent.suspendReason === 'stalled';
    if (!hubSuspended || !idleSuspendEnabled()) return nothing;
    if (isNeverSuspendActive(agent) && !canForceSuspend(agent._capabilities)) {
      return nothing;
    }
    return html`
      <sl-tooltip content="New work will no longer wake this agent; only a resume brings it back.">
        <sl-button
          variant="default"
          size="small"
          outline
          data-testid="header-keep-suspended"
          ?loading=${this.actionLoading['keep-suspended']}
          ?disabled=${this.actionLoading['keep-suspended']}
          @click=${() => this.keepSuspended()}
        >
          <sl-icon slot="prefix" name="pause-circle"></sl-icon>
          Keep suspended
        </sl-button>
      </sl-tooltip>
    `;
  }

  /** Converts a hub suspension to a user suspension (POST suspend on a suspended agent). */
  private async keepSuspended(): Promise<void> {
    if (!this.agent) return;
    const action = await resolveSuspendAction(this.agent, { keepSuspended: true });
    if (!action) return;
    this.actionLoading = { ...this.actionLoading, 'keep-suspended': true };
    try {
      const response = await apiFetch(
        `/api/v1/agents/${this.agentId}/suspend`,
        lifecycleActionRequestInit(action)
      );
      if (!response.ok) {
        throw new Error(
          await lifecycleActionErrorMessage(response, 'Failed to keep agent suspended')
        );
      }
    } catch (err) {
      console.error('Failed to keep agent suspended:', err);
      showToast(err instanceof Error ? err.message : 'Failed to keep agent suspended');
    } finally {
      this.actionLoading = { ...this.actionLoading, 'keep-suspended': false };
      this.backgroundRefresh();
    }
  }

  private async toggleSubscription(): Promise<void> {
    if (!this.agent) return;
    this.subscriptionLoading = true;

    try {
      if (this.subscribed && this.subscriptionId) {
        const res = await apiFetch(
          `/api/v1/notifications/subscriptions/${encodeURIComponent(this.subscriptionId)}`,
          { method: 'DELETE' }
        );
        if (res.ok || res.status === 204) {
          this.subscribed = false;
          this.subscriptionId = null;
        }
      } else {
        const res = await apiFetch('/api/v1/notifications/subscriptions', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            scope: 'agent',
            agentId: this.agentId,
            projectId: this.agent.projectId,
            triggerActivities: [...DEFAULT_TRIGGERS],
          }),
        });
        if (res.ok) {
          const sub = (await res.json()) as Subscription;
          this.subscribed = true;
          this.subscriptionId = sub.id;
        }
      }
    } catch (err) {
      console.error('Failed to toggle subscription:', err);
    } finally {
      this.subscriptionLoading = false;
    }
  }

  private backgroundRefresh(): void {
    this.fetchAndMergeAgent().catch((err) => {
      console.warn('Background refresh failed:', err);
    });
  }

  private async fetchAndMergeAgent(): Promise<void> {
    const epoch = new AgentSeedEpoch();
    const epochGeneration = stateManager.scopeGeneration;
    const agentId = this.agentId;
    try {
      const agentResponse = await apiFetch(`/api/v1/agents/${agentId}`);
      if (!agentResponse.ok) return;

      const agent = (await agentResponse.json()) as Agent;
      this.seedAgent(agent, agentId, epoch, epochGeneration);
    } finally {
      epoch.close();
    }
  }

  /**
   * Seed the store with an agent response read under `epoch`, so live
   * changes that landed while the request was in flight are re-applied
   * over it, and show the result. A scope change since the epoch opened
   * means the user left this view: the response may belong to a scope the
   * store no longer holds, so it is not seeded, and it is shown only if
   * the page still shows the agent it was requested for (`agentId`).
   */
  private seedAgent(
    agent: Agent,
    agentId: string,
    epoch: AgentSeedEpoch,
    epochGeneration: number
  ): void {
    if (stateManager.scopeGeneration !== epochGeneration) {
      if (this.agentId === agentId) this.agent = agent;
      return;
    }
    const seeded = epoch.seed([agent], { partial: false });
    // Empty when the agent was deleted while the request was in flight;
    // the deleted state then follows from the store's tombstone.
    this.agent = seeded.agents[0] ?? agent;
  }

  private async loadSharedConversations(): Promise<void> {
    if (!this.agentId) return;
    const reqSeq = ++this.sharedConversationsReqSeq;
    const agentId = this.agentId;
    this.sharedConversationsLoading = true;
    this.sharedConversationsError = null;

    try {
      const res = await apiFetch(`/api/v1/conversations?agent_id=${encodeURIComponent(agentId)}`);
      if (reqSeq !== this.sharedConversationsReqSeq || agentId !== this.agentId) return;
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}: ${res.statusText}`));
      }
      const data = (await res.json()) as AgentConversationsResponse;
      if (reqSeq !== this.sharedConversationsReqSeq || agentId !== this.agentId) return;
      this.sharedConversations = data.conversations ?? [];
      this.sharedConversationsLoaded = true;
    } catch (err) {
      if (reqSeq !== this.sharedConversationsReqSeq || agentId !== this.agentId) return;
      this.sharedConversationsError =
        err instanceof Error ? err.message : 'Failed to load conversations';
    } finally {
      if (reqSeq === this.sharedConversationsReqSeq) {
        this.sharedConversationsLoading = false;
      }
    }
  }

  private handleTabShow(e: CustomEvent<{ name: string }>): void {
    this.activeTab = e.detail.name;
    if (e.detail.name === 'logs') {
      const viewer = this.shadowRoot?.querySelector(
        'scion-agent-log-viewer'
      ) as ScionAgentLogViewer | null;
      viewer?.loadLogs();
    }
    if (e.detail.name === 'messages') {
      if (this.nativeChatEnabled && this.chatViewActive) {
        void this.loadSharedConversations();
      } else {
        const viewer = this.shadowRoot?.querySelector(
          'scion-agent-message-viewer'
        ) as ScionAgentMessageViewer | null;
        viewer?.loadMessages();
      }
    }
    if (e.detail.name === 'usage') {
      const usagePanel = this.shadowRoot?.querySelector(
        'scion-agent-usage-panel'
      ) as ScionAgentUsagePanel | null;
      void usagePanel?.loadIfNeeded();
    }
    if (e.detail.name === 'trajectory') {
      const trajectoryPanel = this.shadowRoot?.querySelector(
        'scion-agent-trajectory-panel'
      ) as ScionAgentTrajectoryPanel | null;
      void trajectoryPanel?.loadIfNeeded();
    }
    if (e.detail.name === 'schedules') {
      const schedulesTab = this.shadowRoot?.querySelector(
        'scion-agent-schedules-tab'
      ) as ScionAgentSchedulesTab | null;
      void schedulesTab?.loadSchedules();
    }
  }

  private handleMessageViewToggle(mode: 'conversations' | 'log'): void {
    this.chatViewActive = mode === 'conversations';
    // Trigger load for the newly active view
    if (this.chatViewActive) {
      void this.loadSharedConversations();
    } else {
      void this.updateComplete.then(() => {
        const viewer = this.shadowRoot?.querySelector(
          'scion-agent-message-viewer'
        ) as ScionAgentMessageViewer | null;
        viewer?.loadMessages();
      });
    }
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  /**
   * The banner under the header: a create that did not complete, an async
   * launch in progress, a start or restart that did not complete, or the
   * generic error banner.
   */
  private renderStateBanner(): TemplateResult | typeof nothing {
    const agent = this.agent!;
    const message = agent.detail?.message || agent.message;
    if (isIncompleteCreate(agent)) {
      return this.renderIncompleteCreateBanner(agent, message);
    }
    if (isAgentLaunching(agent)) {
      const step = agent.launch?.step;
      const stepText = step ? `Step: ${launchStepLabel(step)}` : '';
      const stepEl = step
        ? html`<div class="launch-step" data-testid="launch-step" data-step=${step}>
            ${stepText}
          </div>`
        : nothing;
      const messageEl = message
        ? html`<div class="launch-message" data-testid="launch-message">${message}</div>`
        : nothing;
      return html`
        <div class="agent-launch-banner" data-testid="agent-launch-banner">
          <sl-spinner></sl-spinner>
          <div class="launch-content">
            <div class="launch-title">${launchKindTitle(agent.launch?.kind)}</div>
            ${stepEl} ${messageEl}
          </div>
        </div>
      `;
    }
    if (isFailedStartLaunch(agent)) {
      return this.renderFailedStartBanner(agent, message);
    }
    if (agent.phase === 'error' && message) {
      return html`
        <div class="agent-error-banner">
          <sl-icon name="exclamation-octagon"></sl-icon>
          <div class="error-content">
            <div class="error-title">Agent Error</div>
            <div class="error-message">${message}</div>
          </div>
        </div>
      `;
    }
    return nothing;
  }

  /**
   * A failed async create (ptone/scion#4664). The hub keeps the agent but
   * refuses to start it, so the banner shows the failure code and message,
   * what the agent was created with, and offers delete and create again.
   */
  private renderIncompleteCreateBanner(agent: Agent, message?: string): TemplateResult {
    const launch = agent.launch!;
    const reason = launch.state === 'active' ? 'stopping' : launchEndReasonLabel(launch.endReason);
    const task = agent.appliedConfig?.task;
    const canDelete = can(agent._capabilities, 'delete') && !this.deletionLease.isDeleting(agent);
    return html`
      <div class="agent-error-banner" data-testid="incomplete-create-banner">
        <sl-icon name="exclamation-octagon"></sl-icon>
        <div class="error-content">
          <div class="error-title">${launchFailedTitle(launch.kind)}</div>
          ${this.renderLaunchFailureDetails(launch, reason, message)}
          <div class="error-hint">
            This agent cannot be started or resumed. Delete it and create it again.
          </div>
          ${agent.template || task
            ? html`
                <dl class="error-recreate">
                  ${agent.template
                    ? html`<dt>Template</dt>
                        <dd data-testid="incomplete-create-template">${agent.template}</dd>`
                    : nothing}
                  ${task
                    ? html`<dt>Task</dt>
                        <dd data-testid="incomplete-create-task">${task}</dd>`
                    : nothing}
                </dl>
              `
            : nothing}
          <div class="error-actions">
            ${canDelete
              ? html`
                  <sl-button
                    variant="danger"
                    size="small"
                    data-testid="incomplete-create-delete"
                    ?loading=${this.actionLoading['delete']}
                    ?disabled=${this.actionLoading['delete']}
                    @click=${(e: MouseEvent): void => void this.handleAction('delete', e)}
                  >
                    <sl-icon slot="prefix" name="trash"></sl-icon>
                    Delete
                  </sl-button>
                `
              : nothing}
            <a
              href="/agents/new?projectId=${encodeURIComponent(agent.projectId)}"
              style="text-decoration: none;"
              data-testid="incomplete-create-recreate"
            >
              <sl-button variant="default" size="small">
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                Create again
              </sl-button>
            </a>
          </div>
        </div>
      </div>
    `;
  }

  /**
   * The failure lines a failed-launch banner shows: the code with its label
   * and how the launch ended, the runtime cause, the step the launch failed
   * in, and the message. launch-code keeps the raw code for automation.
   */
  private renderLaunchFailureDetails(
    launch: AgentLaunch,
    reason: string | undefined,
    message?: string
  ): TemplateResult {
    const code = launch.error;
    const codeEl = code
      ? html`<span data-testid="launch-code">${code}</span>:
          <span data-testid="launch-code-label">${launchCodeLabel(code)}</span>`
      : nothing;
    const reasonEl = reason
      ? html`<span data-testid="launch-end-reason">${reason}</span>`
      : nothing;
    return html`
      ${code || reason
        ? html`<div class="error-code" data-testid="launch-error-code">
            ${codeEl}${code && reason ? ' — ' : ''}${reasonEl}
          </div>`
        : nothing}
      ${launch.cause
        ? html`<div class="error-cause" data-testid="launch-error-cause">
            Cause: ${launchCauseLabel(launch.cause)}
          </div>`
        : nothing}
      ${code && launch.step
        ? html`<div class="error-step" data-testid="launch-failed-step" data-step=${launch.step}>
            Failed at: ${launchStepLabel(launch.step)}
          </div>`
        : nothing}
      ${message
        ? html`<div class="error-message" data-testid="launch-error-message">${message}</div>`
        : nothing}
    `;
  }

  /**
   * A start or restart launch that failed (ptone/scion#4669). Unlike a
   * create that did not complete, the hub still accepts a start or resume,
   * so the header keeps those actions and the banner offers no delete.
   */
  private renderFailedStartBanner(agent: Agent, message?: string): TemplateResult {
    const launch = agent.launch!;
    return html`
      <div class="agent-error-banner" data-testid="failed-start-banner">
        <sl-icon name="exclamation-octagon"></sl-icon>
        <div class="error-content">
          <div class="error-title">${launchFailedTitle(launch.kind)}</div>
          ${this.renderLaunchFailureDetails(
            launch,
            launchEndReasonLabel(launch.endReason),
            message
          )}
          <div class="error-hint">Start or resume the agent to try again.</div>
        </div>
      </div>
    `;
  }

  /**
   * Compact Track 2 trajectory capture notice banner, gated on the
   * tokenomics.trajectory_capture presentation flag and Hub-provided policy.
   */
  private renderTrajectoryCaptureBanner(): TemplateResult | typeof nothing {
    if (!this.trajectoryCaptureEnabled || !this.agent?.trajectory_capture) {
      return nothing;
    }
    const status = this.agent.trajectory_capture;
    const notice = formatTrajectoryCaptureNotice(status);
    if (!notice) {
      return nothing;
    }
    const effective = isTrajectoryCaptureEffective(status.policy);
    return html`
      <div
        class="trajectory-capture-banner ${effective ? 'effective' : ''}"
        role="status"
        data-testid="trajectory-capture-banner"
        data-policy=${status.policy}
        data-enforced=${String(!!status.enforced_by_hub)}
      >
        <sl-icon name=${effective ? 'eye' : 'eye-slash'}></sl-icon>
        <span>${notice}</span>
      </div>
    `;
  }

  override render() {
    if (this.deleted) {
      return this.renderDeletedState();
    }

    if (this.loading) {
      return this.renderLoading();
    }

    if (this.error || !this.agent) {
      return this.renderError();
    }

    const isShell = this.isShellAgent(this.agent);

    return html`
      ${this.renderHeader()} ${this.renderDeletionBanner()} ${this.renderStateBanner()}
      ${this.renderTrajectoryCaptureBanner()}

      <sl-tab-group @sl-tab-show=${this.handleTabShow}>
        <sl-tab slot="nav" panel="status">Status</sl-tab>
        ${this.usageLedgerEnabled
          ? html`<sl-tab slot="nav" panel="usage" data-testid="agent-usage-tab">Usage</sl-tab>`
          : nothing}
        ${this.trajectoryTabVisible
          ? html`<sl-tab slot="nav" panel="trajectory" data-testid="agent-trajectory-tab"
              >Trajectory</sl-tab
            >`
          : nothing}
        <sl-tab slot="nav" panel="metrics">Metrics</sl-tab>
        <sl-tab slot="nav" panel="logs">Logs</sl-tab>
        ${!isShell ? html`<sl-tab slot="nav" panel="messages">Messages</sl-tab>` : nothing}
        <sl-tab slot="nav" panel="schedules" data-testid="agent-schedules-tab">Schedules</sl-tab>
        <sl-tab slot="nav" panel="configuration">Configuration</sl-tab>
        ${this.agentSecretsEnabled
          ? html`<sl-tab slot="nav" panel="secrets" data-testid="agent-secrets-tab"
              >Secrets</sl-tab
            >`
          : nothing}

        <sl-tab-panel name="status">${this.renderStatusTab()}</sl-tab-panel>
        ${this.usageLedgerEnabled
          ? html`<sl-tab-panel name="usage" data-testid="agent-usage-panel">
              <scion-agent-usage-panel agentId=${this.agent.id}></scion-agent-usage-panel>
            </sl-tab-panel>`
          : nothing}
        ${this.trajectoryTabVisible
          ? html`<sl-tab-panel name="trajectory" data-testid="agent-trajectory-panel">
              <scion-agent-trajectory-panel agentId=${this.agent.id}></scion-agent-trajectory-panel>
            </sl-tab-panel>`
          : nothing}
        <sl-tab-panel name="metrics">${this.renderMetricsTab()}</sl-tab-panel>
        <sl-tab-panel name="logs">
          <scion-agent-log-viewer
            agentId=${this.agentId}
            ?cloudLogging=${this.agent.cloudLogging || false}
            .brokers=${this.agent.runtimeBrokerId
              ? {
                  [this.agent.runtimeBrokerId]:
                    this.agent.runtimeBrokerName || this.agent.runtimeBrokerId,
                }
              : {}}
          ></scion-agent-log-viewer>
        </sl-tab-panel>
        ${!isShell
          ? html`<sl-tab-panel name="messages"> ${this.renderMessagesPanel()} </sl-tab-panel>`
          : nothing}
        <sl-tab-panel name="schedules" data-testid="agent-schedules-panel">
          <scion-agent-schedules-tab
            projectId=${this.agent.projectId || ''}
            agentId=${this.agent.id}
          ></scion-agent-schedules-tab>
        </sl-tab-panel>
        <sl-tab-panel name="configuration">${this.renderConfigurationTab()}</sl-tab-panel>
        ${this.agentSecretsEnabled
          ? html`<sl-tab-panel name="secrets" data-testid="agent-secrets-panel">
              <scion-secret-list
                scope="agent"
                scopeId=${this.agent.id}
                compact
                ?readonly=${!!this.agent._capabilities && !can(this.agent._capabilities, 'update')}
              ></scion-secret-list>
            </sl-tab-panel>`
          : nothing}
      </sl-tab-group>

      ${!isShell
        ? html`
            <scion-quick-message-dialog
              agentId=${this.agentId}
              agentName=${this.agent.name || ''}
              userId=${this.currentUserId}
              ?open=${this.quickMessageOpen}
              @sl-request-close=${() => {
                this.quickMessageOpen = false;
              }}
            ></scion-quick-message-dialog>
          `
        : nothing}
      ${this.renderReincarnateDialog(this.agent)}
    `;
  }

  // ---------------------------------------------------------------------------
  // Messages Panel (Conversations | Log toggle)
  // ---------------------------------------------------------------------------

  /** Render messaging authorization banners (denial warning / sealed notice). */
  private renderMessagingBanners() {
    const agent = this.agent!;
    return html`
      ${agent._messageability?.canMessage === false && agent.messageMode !== 'none'
        ? html`
            <div
              class="alert alert-warning"
              style="margin-bottom: 0.5em; padding: 0.75em; border-radius: var(--scion-radius-sm); background: var(--sl-color-warning-100); border: 1px solid var(--sl-color-warning-300);"
            >
              <sl-icon name="exclamation-triangle" style="margin-right: 0.5em;"></sl-icon>
              ${getDenialMessage(agent._messageability.reason, agent.name)}
            </div>
          `
        : nothing}
      ${agent.messageMode === 'none'
        ? html`
            <div
              class="alert alert-danger"
              style="margin-bottom: 0.5em; padding: 0.75em; border-radius: var(--scion-radius-sm); background: var(--sl-color-danger-100); border: 1px solid var(--sl-color-danger-300);"
            >
              <sl-icon name="shield-lock" style="margin-right: 0.5em;"></sl-icon>
              This agent is sealed (mode: none). Only super-admins can message it.
            </div>
          `
        : nothing}
    `;
  }

  private renderMessagesPanel() {
    const agent = this.agent!;
    // S5: shell execution containers never receive messages.
    if (this.isShellAgent(agent)) {
      return nothing;
    }

    // When the feature flag is off, render exactly the existing message viewer
    // (AC9 — byte-for-byte current Messages tab behaviour).
    if (!this.nativeChatEnabled) {
      return html`
        ${this.renderMessagingBanners()}
        <scion-agent-message-viewer
          agentId=${this.agentId}
          agentName=${agent.name || ''}
          ?canSend=${canMessageAgent(agent._capabilities) &&
          agent._messageability?.canMessage !== false}
          ?cloudLogging=${agent.cloudLogging || false}
        ></scion-agent-message-viewer>
      `;
    }

    // Feature flag ON: show Conversations|Log toggle, default to Conversations
    return html`
      ${this.renderMessagingBanners()}
      <div class="messages-view-bar">
        <sl-radio-group
          class="messages-view-toggle"
          label="Messages view"
          value=${this.chatViewActive ? 'conversations' : 'log'}
          size="small"
          @sl-change=${(e: Event) => {
            const value = (e.target as HTMLInputElement).value as 'conversations' | 'log';
            this.handleMessageViewToggle(value);
          }}
        >
          <sl-radio-button value="conversations">
            <sl-icon slot="prefix" name="chat-dots" style="font-size: 0.875rem"></sl-icon>
            Conversations
          </sl-radio-button>
          <sl-radio-button value="log">
            <sl-icon slot="prefix" name="list-ul" style="font-size: 0.875rem"></sl-icon>
            Log
          </sl-radio-button>
        </sl-radio-group>
      </div>
      <div
        class="shared-conversations-container"
        data-testid="shared-conversations-panel"
        style="display: ${this.chatViewActive ? '' : 'none'}"
      >
        ${this.renderSharedConversations(agent)}
      </div>
      <scion-agent-message-viewer
        agentId=${this.agentId}
        agentName=${agent.name || ''}
        .projectId=${agent.projectId || ''}
        ?canSend=${canMessageAgent(agent._capabilities) &&
        agent._messageability?.canMessage !== false}
        ?cloudLogging=${agent.cloudLogging || false}
        style="display: ${this.chatViewActive ? 'none' : ''}"
      ></scion-agent-message-viewer>
    `;
  }

  private sharedConversationLabel(conv: AgentConversationSummary, agent: Agent): string {
    if (conv.displayName) return conv.displayName;
    if (conv.threadName) return conv.threadName;
    if (conv.kind === 'direct') {
      return conv.dmPeer?.name || agent.name || 'Direct message';
    }
    return 'Untitled conversation';
  }

  private renderSharedConversations(agent: Agent): TemplateResult {
    if (this.sharedConversationsLoading && !this.sharedConversationsLoaded) {
      return html`
        <div class="shared-conversations-card">
          <div class="shared-conversations-state" data-testid="shared-conversations-loading">
            <sl-spinner></sl-spinner>
            <span>Loading conversations...</span>
          </div>
        </div>
      `;
    }

    if (this.sharedConversationsError) {
      return html`
        <div class="shared-conversations-card">
          <div class="shared-conversations-state" data-testid="shared-conversations-error">
            <sl-icon name="exclamation-triangle" style="font-size: 1.5rem;"></sl-icon>
            <div class="shared-conversations-state-title">Failed to load conversations</div>
            <p class="shared-conversations-state-desc">${this.sharedConversationsError}</p>
            <sl-button
              variant="default"
              size="small"
              data-testid="shared-conversations-retry"
              @click=${() => void this.loadSharedConversations()}
            >
              <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
              Retry
            </sl-button>
          </div>
        </div>
      `;
    }

    if (this.sharedConversations.length === 0) {
      return html`
        <div class="shared-conversations-card">
          <div class="shared-conversations-state" data-testid="shared-conversations-empty">
            <sl-icon name="chat-dots" style="font-size: 1.5rem;"></sl-icon>
            <div class="shared-conversations-state-title">No shared conversations</div>
            <p class="shared-conversations-state-desc">
              Conversations you share with ${agent.name || 'this agent'} will appear here.
            </p>
          </div>
        </div>
      `;
    }

    return html`
      <div class="shared-conversations-card">
        <div class="shared-conversations-list" data-testid="shared-conversations-list">
          ${this.sharedConversations.map((conv) => {
            const label = this.sharedConversationLabel(conv, agent);
            const isDirect = conv.kind === 'direct';
            const kindLabel = isDirect ? 'Direct' : conv.kind === 'group' ? 'Group' : conv.kind;
            const kindIcon = isDirect ? 'chat-dots' : 'people';
            const href = conversationChatPath(conv);
            const lastActivity = this.formatRelativeTime(conv.lastActivityAt);
            const fullDate = this.formatDate(conv.lastActivityAt);

            const content = html`
              <span class="shared-conversation-icon" aria-hidden="true">
                <sl-icon name=${kindIcon}></sl-icon>
              </span>
              <span class="shared-conversation-main">
                <span class="shared-conversation-title-row">
                  <span class="shared-conversation-name">${label}</span>
                  <sl-badge variant=${isDirect ? 'primary' : 'neutral'}>${kindLabel}</sl-badge>
                  ${conv.surface && conv.surface !== 'native'
                    ? html`<sl-badge variant="neutral">${conv.surface}</sl-badge>`
                    : nothing}
                </span>
              </span>
              <span class="shared-conversation-meta" title=${fullDate}>
                <span>${lastActivity}</span>
                ${href
                  ? html`<sl-icon
                      class="shared-conversation-chevron"
                      name="chevron-right"
                    ></sl-icon>`
                  : nothing}
              </span>
            `;

            if (href) {
              return html`
                <a
                  class="shared-conversation-row shared-conversation-row--link"
                  data-testid="shared-conversation-row"
                  data-conversation-id=${conv.id}
                  href=${href}
                >
                  ${content}
                </a>
              `;
            }

            return html`
              <div
                class="shared-conversation-row shared-conversation-row--unlinked"
                data-testid="shared-conversation-row"
                data-conversation-id=${conv.id}
              >
                ${content}
              </div>
            `;
          })}
        </div>
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Header
  // ---------------------------------------------------------------------------

  /**
   * The header badge shows only a live delete; a failed one (abandoned
   * included) shows the failure banner under the header instead.
   */
  private deletingView(agent: Agent): DeletionInfo | null {
    const view = this.deletionLease.view(agent);
    return view?.state === 'deleting' ? view : null;
  }

  private renderHeaderLabels(agent: Agent): TemplateResult | typeof nothing {
    if (!agent.labels) return nothing;
    const entries = Object.entries(agent.labels).sort(([a], [b]) => a.localeCompare(b));
    if (entries.length === 0) return nothing;
    const visible = entries.slice(0, 3);
    const overflow = entries.slice(3);
    return html`
      <span class="header-labels" data-testid="agent-header-labels">
        ${visible.map(
          ([k, v]) =>
            html`<sl-tag size="small" variant="neutral" title="${k}: ${v}">${k}: ${v}</sl-tag>`
        )}
        ${overflow.length > 0
          ? html`
              <sl-tooltip>
                <div slot="content" class="header-labels-tooltip">
                  ${overflow.map(([k, v]) => html`<div>${k}: ${v}</div>`)}
                </div>
                <sl-tag size="small" variant="neutral" class="header-labels-more">
                  +${overflow.length} more
                </sl-tag>
              </sl-tooltip>
            `
          : nothing}
      </span>
    `;
  }

  private renderHeader() {
    const agent = this.agent!;
    const isShell = this.isShellAgent(agent);
    // While the hub is deleting, hide every lifecycle action and Delete.
    const deleting = this.deletionLease.isDeleting(agent);
    const lifecycleOk = canLifecycle(agent._capabilities) && !deleting;
    // Start and Resume are hidden while the hub launches the agent (before
    // the launch deadline the hub answers a start with the launching agent,
    // so the button would do nothing), and for a create that did not
    // complete, which the hub refuses to start or resume (409
    // agent_create_incomplete); the launch banner offers delete instead.
    const startOk = lifecycleOk && !isAgentLaunching(agent) && !isIncompleteCreate(agent);
    return html`
      <scion-detail-header heading=${agent.name}>
        <scion-breadcrumb
          slot="back"
          .crumbs=${[...agentParentTrail(this.projectNamed), current(agent.name)]}
        ></scion-breadcrumb>
        <sl-icon slot="icon" name="cpu"></sl-icon>
        ${agentStatusBadge(agent)} ${neverSuspendBadge(agent)}
        ${isShell && execContainersEnabled()
          ? html`
              <sl-badge
                variant="neutral"
                class="exec-container-badge"
                data-testid="exec-container-badge"
              >
                <sl-icon name="terminal"></sl-icon>
                Execution container
              </sl-badge>
            `
          : nothing}
        <scion-deletion-badge .deletion=${this.deletingView(agent)} live></scion-deletion-badge>
        <scion-authority-badge .authority=${agent.authority}></scion-authority-badge>
        ${this.renderHeaderLabels(agent)}
        ${!isShell
          ? html`
              <scion-message-mode-badge
                mode=${agent.messageMode || 'project'}
                size="medium"
              ></scion-message-mode-badge>
            `
          : nothing}
        <div slot="meta" class="header-meta">
          ${!isShell || agent.template
            ? html`
                <span class="template-badge">
                  <sl-icon name="code-square"></sl-icon>
                  ${agent.template}
                </span>
              `
            : nothing}
          ${this.project
            ? html`
                <a href="/projects/${this.project.id}" class="project-link">
                  <sl-icon name="folder"></sl-icon>
                  ${this.project.name}
                </a>
              `
            : ''}
          ${agent.runtimeBrokerId
            ? html`
                <a href="/brokers/${agent.runtimeBrokerId}" class="broker-link">
                  <sl-icon name="hdd-rack"></sl-icon>
                  ${agent.runtimeBrokerName || agent.runtimeBrokerId}
                </a>
              `
            : ''}
        </div>
        <div slot="actions" class="header-actions">
          <sl-tooltip content="See this agent in graph">
            <a
              href="/agents/graph?project=${encodeURIComponent(
                agent.projectId
              )}&focus=${encodeURIComponent(this.agentId)}"
              style="text-decoration: none;"
            >
              <sl-button variant="default" size="small">
                <sl-icon slot="prefix" name="diagram-3"></sl-icon>
              </sl-button>
            </a>
          </sl-tooltip>
          ${isShell || agent.messageMode === 'none'
            ? nothing
            : agent._messageability?.canMessage === false
              ? html`
                  <sl-tooltip
                    content="${getDenialMessage(agent._messageability.reason, agent.name)}"
                  >
                    <sl-button variant="default" size="small" outline disabled>
                      <sl-icon slot="prefix" name="chat-dots"></sl-icon>
                      Message
                    </sl-button>
                  </sl-tooltip>
                `
              : canMessageAgent(agent._capabilities)
                ? html`
                    <sl-button
                      variant="default"
                      size="small"
                      outline
                      @click=${() => {
                        this.quickMessageOpen = true;
                      }}
                    >
                      <sl-icon slot="prefix" name="chat-dots"></sl-icon>
                      Message
                    </sl-button>
                  `
                : nothing}
          ${can(agent._capabilities, 'attach')
            ? html`
                <a href=${terminalHref(this.agentId)} style="text-decoration: none;">
                  <sl-button
                    variant="primary"
                    size="small"
                    ?disabled=${!isTerminalAvailable(agent)}
                  >
                    <sl-icon slot="prefix" name="terminal"></sl-icon>
                    Terminal
                  </sl-button>
                </a>
              `
            : nothing}
          ${isAgentRunning(agent)
            ? html`
                ${lifecycleOk
                  ? html`
                      ${showSuspendAction(agent)
                        ? html`
                            <sl-button
                              variant="warning"
                              size="small"
                              outline
                              data-testid="header-suspend"
                              ?loading=${this.actionLoading['suspend']}
                              ?disabled=${this.actionLoading['suspend']}
                              @click=${() => this.handleAction('suspend')}
                            >
                              <sl-icon slot="prefix" name="pause-circle"></sl-icon>
                              Suspend
                            </sl-button>
                          `
                        : nothing}
                      <sl-button
                        variant="danger"
                        size="small"
                        outline
                        ?loading=${this.actionLoading['stop']}
                        ?disabled=${this.actionLoading['stop']}
                        @click=${() => this.handleAction('stop')}
                      >
                        <sl-icon slot="prefix" name="stop-circle"></sl-icon>
                        Stop
                      </sl-button>
                    `
                  : nothing}
              `
            : agent.phase === 'suspended'
              ? html`${startOk
                  ? html`
                      <sl-button
                        variant="success"
                        size="small"
                        data-testid="header-resume"
                        ?loading=${this.actionLoading['resume']}
                        ?disabled=${this.actionLoading['resume']}
                        @click=${() => this.handleAction('resume')}
                      >
                        <sl-icon slot="prefix" name="play-circle"></sl-icon>
                        Resume
                      </sl-button>
                    `
                  : nothing}${lifecycleOk ? this.renderKeepSuspended(agent) : nothing}`
              : startOk
                ? html`
                    ${agent.phase === 'error'
                      ? html`
                          <sl-button
                            variant="default"
                            outline
                            size="small"
                            data-testid="header-force-resume"
                            ?loading=${this.actionLoading['force-resume']}
                            ?disabled=${this.actionLoading['force-resume']}
                            @click=${() => this.handleAction('force-resume')}
                          >
                            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                            Resume (best effort)
                          </sl-button>
                        `
                      : nothing}
                    <sl-button
                      variant="success"
                      size="small"
                      data-testid="header-start"
                      ?loading=${this.actionLoading['start']}
                      ?disabled=${this.actionLoading['start']}
                      @click=${() => this.handleAction('start')}
                    >
                      <sl-icon slot="prefix" name="play-circle"></sl-icon>
                      Start
                    </sl-button>
                  `
                : nothing}
          ${can(agent._capabilities, 'update')
            ? html`
                <a
                  href="/agents/${this.agentId}/edit"
                  style="text-decoration: none;"
                  data-testid="edit-agent"
                >
                  <sl-button variant="default" size="small">
                    <sl-icon slot="prefix" name="pencil-square"></sl-icon>
                    Edit
                  </sl-button>
                </a>
              `
            : nothing}
          ${can(agent._capabilities, 'delete') && !deleting
            ? html`
                <sl-button
                  variant="danger"
                  size="small"
                  ?loading=${this.actionLoading['delete']}
                  ?disabled=${this.actionLoading['delete']}
                  @click=${(e: MouseEvent) => this.handleAction('delete', e)}
                >
                  <sl-icon slot="prefix" name="trash"></sl-icon>
                </sl-button>
              `
            : nothing}
        </div>
      </scion-detail-header>
    `;
  }

  // ---------------------------------------------------------------------------
  // Status Tab
  // ---------------------------------------------------------------------------

  private renderStatusTab() {
    const agent = this.agent!;
    return html`
      ${this.renderCurrentStateCard(agent)} ${this.renderLastScreenCard(agent)}
      ${this.renderCurrentTaskCard(agent)} ${this.renderLimitsUsageCard(agent)}
      ${this.renderPlacementCard(agent)} ${this.renderConnectivityCard(agent)}
      ${this.renderExposedPortsCard(agent)} ${this.renderNotificationsCard()}
    `;
  }

  private renderLastScreenCard(agent: Agent) {
    if (agent.phase !== 'suspended' || !can(agent._capabilities, 'attach')) {
      return nothing;
    }
    return html`
      <scion-agent-suspend-screen
        .agentId=${agent.id}
        .projectId=${agent.projectId || ''}
        .phase=${agent.phase}
      ></scion-agent-suspend-screen>
    `;
  }

  private renderCurrentStateCard(agent: Agent) {
    const activityTimestamp =
      (agent.activitySince && !this.isZeroDate(agent.activitySince) && agent.activitySince) ||
      (agent.lastActivityEvent &&
        !this.isZeroDate(agent.lastActivityEvent) &&
        agent.lastActivityEvent) ||
      agent.updated ||
      agent.updatedAt;
    return html`
      <div class="card">
        <div class="card-title-row">
          <h3 class="card-title">Current State</h3>
          ${this.pageData?.user
            ? html`
                <sl-tooltip
                  content=${this.subscribed
                    ? 'Unsubscribe from notifications'
                    : 'Subscribe to notifications'}
                >
                  <sl-button
                    size="small"
                    variant=${this.subscribed ? 'primary' : 'default'}
                    outline
                    @click=${() => void this.toggleSubscription()}
                    ?loading=${this.subscriptionLoading}
                    aria-label=${this.subscribed ? 'Unsubscribe' : 'Subscribe'}
                  >
                    <sl-icon slot="prefix" name=${this.subscribed ? 'bell-fill' : 'bell'}></sl-icon>
                    ${this.subscribed ? 'Subscribed' : 'Subscribe'}
                  </sl-button>
                </sl-tooltip>
              `
            : nothing}
        </div>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Phase</span>
            <span class="info-value">
              ${agentStatusBadge(agent, {
                status: agent.phase,
                label: agent.phase,
                size: 'small',
                testId: 'agent-phase',
              })}
              <scion-deletion-badge
                .deletion=${this.deletionLease.view(agent)}
                size="small"
              ></scion-deletion-badge>
            </span>
          </div>
          <div class="info-item">
            <span class="info-label">Activity</span>
            <span class="info-value">
              ${agent.activity
                ? html`${agentStatusBadge(agent, {
                    status: agent.activity,
                    label: stateLabel(agent.activity),
                    size: 'small',
                  })}${activityTimestamp
                    ? html`<span
                        data-testid="agent-activity-since"
                        title=${this.formatDate(activityTimestamp)}
                        style="color: var(--scion-text-muted, #64748b); font-size: 0.85em; margin-left: 0.5em;"
                        >${this.formatRelativeTime(activityTimestamp)}</span
                      >`
                    : ''}`
                : html`<span style="color: var(--scion-text-muted, #64748b);">—</span>`}
            </span>
          </div>
          ${this.renderContainerStarted(agent)} ${this.renderIdleSuspendStatus(agent)}
          ${this.renderSuspendReason(agent)} ${this.renderSuspendPolicy(agent)}
          ${agent.detail?.toolName
            ? html`
                <div class="info-item">
                  <span class="info-label">Tool</span>
                  <span class="info-value mono">${agent.detail.toolName}</span>
                </div>
              `
            : ''}
          ${agent.detail?.message && agent.phase !== 'error'
            ? html`
                <div class="info-item">
                  <span class="info-label">Detail</span>
                  <span class="info-value">${agent.detail.message}</span>
                </div>
              `
            : ''}
        </div>
      </div>
    `;
  }

  private renderCurrentTaskCard(agent: Agent) {
    if (this.isShellAgent(agent)) return nothing;
    const task = agent.detail?.taskSummary || agent.taskSummary;
    if (!task) return nothing;
    return html`
      <div class="card">
        <h3 class="card-title">Current Task</h3>
        <div class="task-summary">${task}</div>
      </div>
    `;
  }

  private renderLimitsUsageCard(agent: Agent) {
    const cfg = agent.appliedConfig?.inlineConfig;
    const eff = agent.effectiveConfig;
    const clamped = new Set(eff?.limitsClampedBy ?? []);

    const effTurns = eff?.maxTurns && eff.maxTurns > 0 ? eff.maxTurns : undefined;
    const effModelCalls =
      eff?.maxModelCalls && eff.maxModelCalls > 0 ? eff.maxModelCalls : undefined;
    const effDuration = eff?.maxDuration?.trim() || undefined;

    const rawTurns = effTurns ?? cfg?.max_turns;
    const rawModelCalls = effModelCalls ?? cfg?.max_model_calls;
    const rawDuration = effDuration ?? cfg?.max_duration;

    // Distinguish unset (inherited from template/hub/broker defaults, cap
    // unknown to the hub) from explicit 0 / "0" (Unlimited, no cap applies).
    const turnsInherited = rawTurns == null;
    const modelCallsInherited = rawModelCalls == null;
    const durationInherited = !rawDuration;

    const maxTurns = turnsInherited ? 0 : rawTurns;
    const maxModelCalls = modelCallsInherited ? 0 : rawModelCalls;
    const maxDurationSec =
      durationInherited || rawDuration === '0' ? 0 : parseDuration(rawDuration);

    const showTurns = turnsInherited || maxTurns > 0;
    const showModelCalls = modelCallsInherited || maxModelCalls > 0;
    const showDuration = durationInherited || maxDurationSec > 0;

    if (!showTurns && !showModelCalls && !showDuration) return nothing;

    const currentTurns = agent.currentTurns ?? 0;
    const currentModelCalls = agent.currentModelCalls ?? 0;

    let timeRemainingSec = 0;
    let elapsedSec = 0;
    const startedAt =
      agent.startedAt && !this.isZeroDate(agent.startedAt) ? agent.startedAt : undefined;
    if (startedAt) {
      elapsedSec = Math.max(0, (Date.now() - new Date(startedAt).getTime()) / 1000);
      if (maxDurationSec > 0) {
        timeRemainingSec = Math.max(0, maxDurationSec - elapsedSec);
      }
    }

    const showInheritedElapsed =
      Boolean(startedAt) && (agent.phase === 'running' || agent.phase === 'stopping');

    return html`
      <div class="card">
        <h3 class="card-title">Limits & Usage</h3>
        <div class="limits-grid">
          ${turnsInherited
            ? this.renderInheritedLimitItem('Turns', currentTurns)
            : maxTurns > 0
              ? this.renderLimitItem('Turns', currentTurns, maxTurns, clamped.has('max_turns'))
              : nothing}
          ${modelCallsInherited
            ? this.renderInheritedLimitItem('Model Calls', currentModelCalls)
            : maxModelCalls > 0
              ? this.renderLimitItem(
                  'Model Calls',
                  currentModelCalls,
                  maxModelCalls,
                  clamped.has('max_model_calls')
                )
              : nothing}
          ${durationInherited
            ? this.renderInheritedTimeLimitItem(showInheritedElapsed ? elapsedSec : null)
            : maxDurationSec > 0
              ? this.renderTimeLimitItem(
                  timeRemainingSec,
                  elapsedSec,
                  maxDurationSec,
                  clamped.has('max_duration')
                )
              : nothing}
        </div>
      </div>
    `;
  }

  private renderLimitItem(label: string, current: number, max: number, clamped = false) {
    const pct = Math.min(100, (current / max) * 100);
    const colorClass = pct > 90 ? 'danger' : pct > 75 ? 'warning' : 'normal';
    return html`
      <div class="limit-item">
        <span class="limit-label">${label}</span>
        <span class="limit-value"
          >${current} /
          ${max}${clamped
            ? html` <span class="limit-inherited">(hub ceiling)</span>`
            : nothing}</span
        >
        <div class="progress-bar-track">
          <div class="progress-bar-fill ${colorClass}" style="width: ${pct}%"></div>
        </div>
      </div>
    `;
  }

  private renderInheritedLimitItem(label: string, current: number) {
    return html`
      <div class="limit-item">
        <span class="limit-label">${label}</span>
        <span class="limit-value">${current} <span class="limit-inherited">/ Inherited</span></span>
      </div>
    `;
  }

  private renderTimeLimitItem(
    remainingSec: number,
    elapsedSec: number,
    totalSec: number,
    clamped = false
  ) {
    const pct = Math.min(100, (elapsedSec / totalSec) * 100);
    const remainPct = 100 - pct;
    const colorClass = remainPct < 10 ? 'danger' : remainPct < 25 ? 'warning' : 'normal';
    return html`
      <div class="limit-item">
        <span class="limit-label">Time Remaining</span>
        <span class="limit-value"
          >${formatDurationHMS(remainingSec)}${clamped
            ? html` <span class="limit-inherited">(hub ceiling)</span>`
            : nothing}</span
        >
        <div class="progress-bar-track">
          <div class="progress-bar-fill ${colorClass}" style="width: ${pct}%"></div>
        </div>
      </div>
    `;
  }

  private renderInheritedTimeLimitItem(elapsedSec: number | null) {
    return html`
      <div class="limit-item">
        <span class="limit-label">${elapsedSec !== null ? 'Elapsed' : 'Max Duration'}</span>
        <span class="limit-value"
          >${elapsedSec !== null
            ? html`${formatDurationHMS(elapsedSec)}
                <span class="limit-inherited">/ Inherited</span>`
            : html`<span class="limit-inherited">Inherited</span>`}</span
        >
      </div>
    `;
  }

  /**
   * Placement of a pinned (flat Runtime Broker) agent, from stored data: the
   * Runtime Broker and runtime target it is pinned to, with the Runtime
   * Broker's connection status and the agent's failed runtime operations as
   * two separate indicators. Unpinned agents render nothing here.
   */
  private renderPlacementCard(agent: Agent): TemplateResult | typeof nothing {
    const view = agentPlacementView(agent, this.placementBroker);
    if (!view) return nothing;
    return html`
      <div class="card placement-card">
        <h3 class="card-title">Placement</h3>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Runtime Broker</span>
            <span class="info-value">
              <a href="/brokers/${view.runtimeBrokerId}" class="broker-link"
                >${view.runtimeBrokerName}</a
              >
            </span>
          </div>
          <div class="info-item">
            <span class="info-label">Runtime Target</span>
            <span class="info-value placement-target"
              >${view.targetLabel}${view.targetLabel !== view.targetType
                ? ` (${view.targetType})`
                : ''}</span
            >
          </div>
          <div class="info-item">
            <span class="info-label">Target ID</span>
            <span class="info-value mono placement-target-id">${view.targetId}</span>
          </div>
          <div class="info-item">
            <span class="info-label">Runtime Broker Connection</span>
            <span class="info-value placement-connection">
              <scion-status-badge
                status=${view.connection.status}
                label=${view.connection.detail
                  ? `${view.connection.label} (${view.connection.detail})`
                  : view.connection.label}
                size="small"
              ></scion-status-badge>
            </span>
          </div>
          <div class="info-item">
            <span class="info-label">Last Runtime Operation</span>
            <span class="info-value placement-runtime-op">
              <scion-status-badge
                status=${view.lastRuntimeOperation.status}
                label=${view.lastRuntimeOperation.label}
                size="small"
              ></scion-status-badge>
              ${view.lastRuntimeOperation.detail
                ? html`<span class="placement-runtime-op-detail"
                    >${view.lastRuntimeOperation.detail}</span
                  >`
                : nothing}
            </span>
          </div>
          ${view.stale
            ? html`
                <div class="info-item placement-stale">
                  <span class="info-label">Placement</span>
                  <span class="info-value">
                    Pinned to a different Runtime Broker than the agent's current one; starts are
                    refused until the placement is repaired.
                  </span>
                </div>
              `
            : nothing}
        </div>
      </div>
    `;
  }

  private renderConnectivityCard(agent: Agent) {
    const candidates = [
      agent.lastSeen,
      agent.updated,
      agent.updatedAt,
      agent.created,
      agent.createdAt,
    ];
    const lastSeenStr = candidates.find((d) => d && !this.isZeroDate(d)) || '';
    return html`
      <div class="card">
        <h3 class="card-title">Connectivity</h3>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Last Seen</span>
            <span class="info-value" title="${this.formatDate(lastSeenStr)}">
              ${this.formatRelativeTime(lastSeenStr)}
            </span>
          </div>
          ${agent.connectionState
            ? html`
                <div class="info-item">
                  <span class="info-label">Connection State</span>
                  <span class="info-value">
                    <scion-status-badge
                      status=${agent.connectionState === 'connected' ? 'success' : 'danger'}
                      label=${agent.connectionState}
                      size="small"
                    ></scion-status-badge>
                  </span>
                </div>
              `
            : ''}
        </div>
      </div>
    `;
  }

  private renderExposedPortsCard(agent: Agent) {
    const ports = agent.exposedPorts;
    if (!ports || ports.length === 0) return nothing;

    // Opening a port goes through the port proxy, which the hub authorizes
    // with agent.port_access. Fail closed: without the capability, list the
    // ports but offer no link that would only return 403 (ptone/scion#2540).
    const canOpen = can(agent._capabilities, 'port_access');

    return html`
      <div class="card">
        <h3 class="card-title">Exposed Ports</h3>
        ${canOpen
          ? nothing
          : html`<p class="port-no-access" style="color: var(--scion-text-muted, #64748b);">
              You don't have port access on this agent.
            </p>`}
        <div class="info-grid">
          ${ports.map(
            (p) => html`
              <div class="info-item">
                <span class="info-label"> :${p.port}${p.label ? ` (${p.label})` : ''} </span>
                <span class="info-value">
                  ${canOpen
                    ? html`<a
                        href="/api/v1/agents/${agent.id}/ports/${p.port}/proxy/"
                        target="_blank"
                        rel="noopener"
                        class="port-link"
                      >
                        Open in new tab
                      </a>`
                    : html`<span style="color: var(--scion-text-muted, #64748b);">—</span>`}
                </span>
              </div>
            `
          )}
        </div>
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Metrics Tab
  // ---------------------------------------------------------------------------

  private renderMetricsTab() {
    const m = this.metricsSummary;
    if (!m) {
      return html`
        <div class="card">
          <h3 class="card-title">Session Metrics</h3>
          <p style="color: var(--scion-text-muted, #888)">No session metrics available yet.</p>
        </div>
      `;
    }

    return html`
      <div class="card">
        <h3 class="card-title">Token Usage</h3>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">TOTAL SESSIONS</span>
            <span class="info-value">${formatNumber(m.totalSessions)}</span>
          </div>
          <div class="info-item">
            <span class="info-label">INPUT TOKENS</span>
            <span class="info-value">${formatNumber(m.totalTokensInput)}</span>
          </div>
          <div class="info-item">
            <span class="info-label">OUTPUT TOKENS</span>
            <span class="info-value">${formatNumber(m.totalTokensOutput)}</span>
          </div>
          <div class="info-item">
            <span class="info-label">CACHED TOKENS</span>
            <span class="info-value">${formatNumber(m.totalTokensCached)}</span>
          </div>
          <div class="info-item">
            <span class="info-label">REASONING TOKENS</span>
            <span class="info-value">${formatNumber(m.totalTokensReasoning)}</span>
          </div>
          <div class="info-item">
            <span class="info-label">AVG TOKENS / SESSION</span>
            <span class="info-value">${formatNumber(m.avgTokensPerSession)}</span>
          </div>
        </div>
      </div>

      <div class="card">
        <h3 class="card-title">Session Statistics</h3>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">TOTAL TOOL CALLS</span>
            <span class="info-value">${formatNumber(m.totalToolCalls)}</span>
          </div>
          <div class="info-item">
            <span class="info-label">AVG DURATION</span>
            <span class="info-value">${this.formatDurationMs(m.avgSessionDurationMs)}</span>
          </div>
        </div>
      </div>

      ${m.mostUsedTools.length > 0
        ? html`
            <div class="card">
              <h3 class="card-title">Most Used Tools</h3>
              <div class="info-grid">
                ${m.mostUsedTools.map(
                  (t) => html`
                    <div class="info-item">
                      <span class="info-label">${t.name.toUpperCase()}</span>
                      <span class="info-value">
                        ${t.calls} calls
                        ${t.error > 0
                          ? html`<span style="color: var(--scion-danger-500, #ef4444)">
                              (${t.error} errors)</span
                            >`
                          : nothing}
                      </span>
                    </div>
                  `
                )}
              </div>
            </div>
          `
        : nothing}
      ${m.mostUsedModels.length > 0
        ? html`
            <div class="card">
              <h3 class="card-title">Models</h3>
              <div class="info-grid">
                ${m.mostUsedModels.map(
                  (model) => html`
                    <div class="info-item">
                      <span class="info-label">${model.model.toUpperCase()}</span>
                      <span class="info-value">${model.sessions} sessions</span>
                    </div>
                  `
                )}
              </div>
            </div>
          `
        : nothing}
    `;
  }

  private formatDurationMs(ms: number): string {
    if (ms <= 0) return '—';
    const totalSeconds = Math.round(ms / 1000);
    const hours = Math.floor(totalSeconds / 3600);
    const minutes = Math.floor((totalSeconds % 3600) / 60);
    const seconds = totalSeconds % 60;
    if (hours > 0) return `${hours}h ${minutes}m`;
    if (minutes > 0) return `${minutes}m ${seconds}s`;
    return `${seconds}s`;
  }

  // ---------------------------------------------------------------------------
  // Configuration Tab
  // ---------------------------------------------------------------------------

  private renderConfigurationTab() {
    const agent = this.agent!;
    const cfg = agent.appliedConfig;
    const inline = cfg?.inlineConfig;

    return html`
      ${this.renderIdentityCard(agent)} ${this.renderReincarnateCard(agent)}
      <scion-effective-role-provenance
        principalType="agent"
        principalId=${this.agentId}
        compact
        sectionTitle="Effective Roles"
      ></scion-effective-role-provenance>
      <scion-effective-access-boundary-notice
        contextType="agent"
        contextId=${this.agentId}
      ></scion-effective-access-boundary-notice>
      ${this.renderMessagingCard()} ${this.renderHarnessModelCard(agent, cfg, inline)}
      ${this.renderRuntimeCard(agent, inline)}
      ${this.renderGCPIdentityCard(
        cfg?.gcpIdentity,
        this.gcpServiceAccount,
        this.gcpServiceAccountLoading
      )}
      ${this.renderConfigLimitsCard(inline, agent.effectiveConfig)}
      ${this.renderTelemetryCard(inline?.telemetry)} ${this.renderInitialTaskCard(cfg)}
    `;
  }

  /**
   * Reincarnate action (ptone/scion#3707). The hub authorizes a user's
   * reincarnate with the same `agent.lifecycle` permission as start/stop
   * (authorizeAgentReincarnate), and the agent payload carries no separate
   * `reincarnate` capability, so the card is gated on `lifecycle` exactly
   * like the header's lifecycle buttons, and hidden while a delete runs.
   */
  private renderReincarnateCard(agent: Agent): TemplateResult | typeof nothing {
    if (!canLifecycle(agent._capabilities) || this.deletionLease.isDeleting(agent)) {
      return nothing;
    }
    return html`
      <div class="card reincarnate-card">
        <h3 class="card-title">Reincarnate</h3>
        <p class="reincarnate-help">
          Stop this agent and provision it again from its current settings and template. Use this to
          apply configuration changes.
        </p>
        <div class="info-item reincarnate-meta" data-testid="agent-last-reincarnated">
          <span class="info-label">Last reincarnated</span>
          <span class="info-value"
            >${this.renderTimestampValue(agent.lastReincarnatedAt, 'Never')}</span
          >
        </div>
        ${this.reincarnatePrepareSent
          ? html`<sl-alert
              variant="primary"
              open
              closable
              class="reincarnate-prepare-sent"
              data-testid="reincarnate-prepare-sent"
              @sl-after-hide=${(e: Event) => {
                if (e.target === e.currentTarget) {
                  this.reincarnatePrepareSent = false;
                }
              }}
            >
              <sl-icon slot="icon" name="info-circle"></sl-icon>
              Asked the agent to prepare. It will reincarnate itself when ready; Last reincarnated
              updates when it completes.
            </sl-alert>`
          : nothing}
        ${this.reincarnateError
          ? html`<sl-alert variant="danger" open class="reincarnate-error">
              <sl-icon slot="icon" name="exclamation-octagon"></sl-icon>
              ${this.reincarnateError}
            </sl-alert>`
          : nothing}
        <sl-button
          size="small"
          variant="default"
          ?loading=${this.reincarnating}
          ?disabled=${this.reincarnating}
          @click=${() => this.openReincarnateDialog()}
        >
          <sl-icon slot="prefix" name="arrow-repeat"></sl-icon>
          Reincarnate
        </sl-button>
      </div>
    `;
  }

  private renderReincarnateDialog(agent: Agent): TemplateResult {
    const prepare = reincarnatePrepareAvailability(agent);
    return html`
      <scion-reincarnate-dialog
        agentName=${agent.name || ''}
        ?open=${this.reincarnateDialogOpen}
        .prepareAvailable=${prepare.available}
        prepareUnavailableReason=${prepare.reason}
        ?busy=${this.reincarnating}
        @reincarnate-choice=${(e: CustomEvent<ReincarnateChoiceDetail>) =>
          void this.handleReincarnateChoice(e.detail.mode)}
        @reincarnate-cancel=${() => {
          if (!this.reincarnating) {
            this.reincarnateDialogOpen = false;
          }
        }}
      ></scion-reincarnate-dialog>
    `;
  }

  private openReincarnateDialog(): void {
    if (!this.agent || this.reincarnateBusy || this.reincarnateDialogOpen) return;
    this.reincarnateError = null;
    this.reincarnatePrepareSent = false;
    this.reincarnateDialogOpen = true;
  }

  /**
   * Executes the choice made in `<scion-reincarnate-dialog>`:
   * - `now`: POST `/api/v1/agents/{id}/reincarnate` with `{}` and refresh.
   * - `prepare`: POST `/api/v1/agents/{id}/message` with `REINCARNATE_PREPARE_MESSAGE`,
   *   `interrupt: false`, and `wake: true`.
   */
  private async handleReincarnateChoice(mode: 'now' | 'prepare'): Promise<void> {
    if (!this.agent || this.reincarnateBusy) return;
    this.reincarnateBusy = true;
    this.reincarnating = true;
    this.reincarnateError = null;
    this.reincarnatePrepareSent = false;
    this.reincarnateDialogOpen = false;

    try {
      if (mode === 'prepare') {
        const response = await apiFetch(`/api/v1/agents/${this.agentId}/message`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            structured_message: { msg: REINCARNATE_PREPARE_MESSAGE, plain: false },
            interrupt: false,
            wake: true,
          }),
        });
        if (!response.ok) {
          try {
            const errorBody = (await response.clone().json()) as {
              error?: { code?: string; details?: { reason?: string } };
            };
            if (errorBody.error?.code === 'message_denied' && errorBody.error?.details?.reason) {
              this.reincarnateError = getDenialMessage(
                errorBody.error.details.reason,
                this.agent.name || ''
              );
              return;
            }
          } catch {
            // Fall through to generic API error extraction.
          }
          this.reincarnateError = await extractApiError(response, 'Failed to send message');
          return;
        }
        this.reincarnatePrepareSent = true;
        return;
      }

      const response = await apiFetch(`/api/v1/agents/${this.agentId}/reincarnate`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: '{}',
      });
      if (!response.ok) {
        throw new Error(await lifecycleActionErrorMessage(response, 'Failed to reincarnate agent'));
      }
      this.backgroundRefresh();
    } catch (err) {
      console.error('Failed to reincarnate agent:', err);
      this.reincarnateError = err instanceof Error ? err.message : 'Failed to reincarnate agent';
      if (mode === 'now') {
        this.backgroundRefresh();
      }
    } finally {
      this.reincarnating = false;
      this.reincarnateBusy = false;
    }
  }

  private renderMessagingCard() {
    const agent = this.agent!;
    if (this.isShellAgent(agent)) return nothing;
    const modeDisplay = getMessageModeDisplay(agent.messageMode);
    const messageability = agent._messageability as AgentMessageabilityDetail | undefined;
    const canSetMode = can(agent._capabilities, 'set_message_mode');
    const hasChildren = (agent.childrenIds?.length ?? 0) > 0;

    return html`
      <div class="card">
        <h3 class="card-title">Messaging</h3>
        <div class="messaging-grid">
          <div class="info-item messaging-mode">
            <span class="info-label">Message Mode</span>
            <span class="info-value">
              ${canSetMode
                ? html`
                    <sl-select
                      size="small"
                      value=${agent.messageMode || 'project'}
                      @sl-change=${(e: Event) => {
                        const newMode = (e.target as HTMLSelectElement).value as MessageMode;
                        void this.handleModeChange(newMode, e.target as HTMLElement);
                      }}
                    >
                      ${(Object.keys(MESSAGE_MODE_DISPLAY) as MessageMode[]).map(
                        (mode) => html`
                          <sl-option value=${mode}>
                            <sl-icon
                              slot="prefix"
                              name=${MESSAGE_MODE_DISPLAY[mode].icon}
                            ></sl-icon>
                            ${MESSAGE_MODE_DISPLAY[mode].label} —
                            ${MESSAGE_MODE_DISPLAY[mode].description}
                          </sl-option>
                        `
                      )}
                    </sl-select>
                    ${(agent.messageMode || 'project') === 'hub'
                      ? html`<div
                          style="font-size: 0.75rem; color: var(--sl-color-neutral-500); margin-top: 0.25rem; max-width: 360px;"
                        >
                          Hub mode: sends within this project and to permitted agents in other
                          projects. External messaging requires the Hub cross-project switch to be
                          enabled.
                        </div>`
                      : nothing}
                  `
                : html`
                    <scion-message-mode-badge
                      mode=${agent.messageMode || 'project'}
                      size="medium"
                    ></scion-message-mode-badge>
                    <span style="margin-left: 0.5em; color: var(--sl-color-neutral-600);">
                      ${modeDisplay.description}
                    </span>
                    ${(agent.messageMode || 'project') === 'hub'
                      ? html`<div
                          style="font-size: 0.75rem; color: var(--sl-color-neutral-500); margin-top: 0.25rem;"
                        >
                          External messaging requires the Hub cross-project switch to be enabled.
                        </div>`
                      : nothing}
                  `}
            </span>
          </div>
          ${messageability && 'reachableAgentCount' in messageability
            ? html`
                <div class="info-item messaging-reach">
                  <span class="info-label">Reachability</span>
                  <span class="info-value">
                    Can reach ${messageability.reachableAgentCount} agents,
                    ${messageability.reachableUserCount} users in this project
                  </span>
                </div>
              `
            : nothing}
        </div>
        ${canSetMode && hasChildren
          ? html`
              <div
                style="margin-top: 1rem; padding-top: 0.75rem; border-top: 1px solid var(--scion-border, #e2e8f0);"
              >
                <sl-button
                  size="small"
                  variant="default"
                  outline
                  @click=${() => {
                    this.cascadeDialogOpen = true;
                  }}
                >
                  <sl-icon slot="prefix" name="diagram-3"></sl-icon>
                  Apply to all descendants
                </sl-button>
              </div>
            `
          : nothing}
      </div>

      ${canSetMode
        ? html`
            <scion-cascade-mode-dialog
              agentId=${agent.id}
              agentName=${agent.name || ''}
              ?open=${this.cascadeDialogOpen}
              currentMode=${agent.messageMode || 'project'}
              @sl-request-close=${() => {
                this.cascadeDialogOpen = false;
              }}
              @cascade-applied=${() => {
                this.cascadeDialogOpen = false;
                this.backgroundRefresh();
              }}
            ></scion-cascade-mode-dialog>
          `
        : nothing}
    `;
  }

  /**
   * Handle message mode change with graduated confirmation flows.
   * Expanding to project requires no confirmation; restricting or quarantining
   * requires confirmation proportional to the severity of the change.
   */
  private async handleModeChange(newMode: MessageMode, selectEl: HTMLElement): Promise<void> {
    const agent = this.agent!;
    const oldMode = agent.messageMode || 'project';
    if (newMode === oldMode) return;

    const revertSelect = () => {
      // Revert the sl-select back to the previous value
      (selectEl as HTMLElement & { value: string }).value = oldMode;
    };

    // Determine if confirmation is needed
    const needsConfirm = newMode !== 'project'; // Expanding to project = no confirm

    if (needsConfirm) {
      let message: string;
      let variant: 'primary' | 'danger' = 'primary';

      if (newMode === 'none') {
        // High severity: quarantine
        variant = 'danger';
        message = `Seal ${agent.name}? It will not be able to send or receive any messages. Only super-admins can communicate with sealed agents. This takes effect immediately.`;
      } else if (oldMode === 'none') {
        // Medium severity: unsealing
        const modeLabel = getMessageModeDisplay(newMode).label.toLowerCase();
        message = `Unseal ${agent.name}? It will be able to send and receive messages in ${modeLabel} mode. This takes effect immediately.`;
      } else if (newMode === 'owner') {
        // Medium severity: restricting to owner
        message = `Change ${agent.name} to Owner mode? It will only be able to message its owner. All agent-to-agent messaging will be denied.`;
      } else if (newMode === 'direct') {
        // Medium severity: restricting to direct plus owner
        message = `Change ${agent.name} to Direct plus owner mode? It will only be able to message its direct parent and children (in Direct or Tree mode) and its owner.`;
      } else if (newMode === 'tree') {
        // Medium severity: restricting to tree plus owner
        message = `Change ${agent.name} to Tree plus owner mode? It will only be able to message same-project agents in its own creation tree and its owner.`;
      } else {
        message = `Change ${agent.name} to ${getMessageModeDisplay(newMode).label.toLowerCase()} mode?`;
      }

      const confirmed = await showConfirm(message, {
        title: 'Change Message Mode',
        confirmText: newMode === 'none' ? 'Seal Agent' : 'Change Mode',
        variant,
      });

      if (!confirmed) {
        revertSelect();
        return;
      }
    }

    // Apply the mode change via API
    try {
      const response = await apiFetch(`/api/v1/agents/${agent.id}/set_message_mode`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ mode: newMode }),
      });

      if (!response.ok) {
        // Detect hub mode grant denial: server returns 403 when the caller
        // lacks full-role + current-hub-mode authority to grant hub.
        if (response.status === 403 && newMode === 'hub') {
          throw new Error(
            'Cannot grant Hub mode: the granting agent or user must have full role and already be in Hub mode.'
          );
        }
        throw new Error(await extractApiError(response, 'Failed to change message mode.'));
      }

      // Optimistic update
      const modeLabel = getMessageModeDisplay(newMode).label;
      this.agent = { ...agent, messageMode: newMode };
      showToast(`Message mode changed to ${modeLabel}.`, 'success');

      // Background refresh to get updated _messageability
      this.backgroundRefresh();
    } catch (err) {
      revertSelect();
      showToast(err instanceof Error ? err.message : 'Failed to change message mode.', 'danger');
    }
  }

  private renderIdentityCard(agent: Agent) {
    return html`
      <div class="card">
        <h3 class="card-title">Identity</h3>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Agent ID</span>
            <span class="info-value mono">${agent.id}</span>
          </div>
          ${agent.slug
            ? html`
                <div class="info-item">
                  <span class="info-label">Slug</span>
                  <span class="info-value mono">${agent.slug}</span>
                </div>
              `
            : ''}
          <div class="info-item">
            <span class="info-label">Created</span>
            <span class="info-value"
              >${this.formatDate(agent.created || agent.createdAt || '')}</span
            >
          </div>
          ${agent.appliedConfig?.creatorName
            ? html`
                <div class="info-item">
                  <span class="info-label">Created By</span>
                  <span class="info-value">${agent.appliedConfig.creatorName}</span>
                </div>
              `
            : ''}
          ${agent.harnessAuth || agent.appliedConfig?.harnessAuth
            ? html`
                <div class="info-item">
                  <span class="info-label">Auth Method</span>
                  <span class="info-value"
                    >${agent.harnessAuth || agent.appliedConfig?.harnessAuth}</span
                  >
                </div>
              `
            : ''}
          ${agent.appliedConfig?.agentRole
            ? html`
                <div class="info-item">
                  <span class="info-label">Authorization Role</span>
                  <span class="info-value">
                    <sl-badge
                      variant=${agent.appliedConfig.agentRole === 'full'
                        ? 'success'
                        : agent.appliedConfig.agentRole === 'baseline'
                          ? 'primary'
                          : agent.appliedConfig.agentRole === 'readonly'
                            ? 'neutral'
                            : agent.appliedConfig.agentRole === 'none'
                              ? 'warning'
                              : 'neutral'}
                      >${agent.appliedConfig.agentRole}</sl-badge
                    >
                  </span>
                </div>
              `
            : ''}
        </div>
      </div>
    `;
  }

  private renderHarnessModelCard(
    agent: Agent,
    cfg: AgentAppliedConfig | undefined,
    inline: AgentInlineConfig | undefined
  ) {
    const harness = agent.harnessConfig || cfg?.harnessConfig;
    const auth = agent.harnessAuth || cfg?.harnessAuth;
    const model = inline?.model || cfg?.model;

    return html`
      <div class="card">
        <h3 class="card-title">Harness & Model</h3>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Template</span>
            <span class="info-value">${agent.template}</span>
          </div>
          ${cfg?.templateId
            ? html`
                <div class="info-item">
                  <span class="info-label">Template ID</span>
                  <span class="info-value mono">${cfg.templateId}</span>
                </div>
              `
            : ''}
          ${cfg?.templateHash
            ? html`
                <div class="info-item">
                  <span class="info-label">Template Hash</span>
                  <span class="info-value mono">
                    <scion-hash-display
                      .hash=${cfg.templateHash}
                      max-width="14ch"
                    ></scion-hash-display>
                  </span>
                </div>
              `
            : ''}
          ${harness
            ? html`
                <div class="info-item">
                  <span class="info-label">Harness Config</span>
                  <span class="info-value">${harness}</span>
                </div>
              `
            : ''}
          ${auth
            ? html`
                <div class="info-item">
                  <span class="info-label">Auth Method</span>
                  <span class="info-value">${auth}</span>
                </div>
              `
            : ''}
          ${model
            ? html`
                <div class="info-item">
                  <span class="info-label">Model</span>
                  <span class="info-value mono">${model}</span>
                </div>
              `
            : ''}
        </div>
      </div>
    `;
  }

  private renderRuntimeCard(agent: Agent, inline: AgentInlineConfig | undefined) {
    const image = agent.image || inline?.image || agent.appliedConfig?.image;
    const branch = inline?.branch;
    // A pinned (flat) agent has no Runtime Broker Profile; its placement
    // card shows the runtime target instead.
    const profile = agent.pinnedRuntimeTarget ? undefined : agent.appliedConfig?.profile;

    return html`
      <div class="card">
        <h3 class="card-title">Runtime Environment</h3>
        <div class="info-grid">
          ${agent.runtimeBrokerId
            ? html`
                <div class="info-item">
                  <span class="info-label">Runtime Broker</span>
                  <span class="info-value">
                    <a
                      href="/brokers/${agent.runtimeBrokerId}"
                      class="broker-link"
                      style="font-size: 1rem;"
                    >
                      ${agent.runtimeBrokerName || agent.runtimeBrokerId}
                    </a>
                  </span>
                </div>
              `
            : ''}
          ${agent.runtime
            ? html`
                <div class="info-item">
                  <span class="info-label">Runtime Type</span>
                  <span class="info-value">${agent.runtime}</span>
                </div>
              `
            : ''}
          ${profile
            ? html`
                <div class="info-item">
                  <span class="info-label">Profile</span>
                  <span class="info-value">${profile}</span>
                </div>
              `
            : ''}
          ${image
            ? html`
                <div class="info-item">
                  <span class="info-label">Image</span>
                  <span class="info-value mono">${image}</span>
                </div>
              `
            : ''}
          ${branch
            ? html`
                <div class="info-item">
                  <span class="info-label">Branch</span>
                  <span class="info-value mono">${branch}</span>
                </div>
              `
            : ''}
        </div>
      </div>
    `;
  }

  /**
   * GCP Identity card. `account` is the registered record of the assigned
   * service account, when it could be loaded; it supplies the display name
   * and the verification status (ptone/scion#4017). Without it the card
   * falls back to the email from the agent payload and shows verification
   * as unknown; `accountLoading` says the record is still being fetched.
   */
  private renderGCPIdentityCard(
    gcpIdentity: GCPIdentityConfig | undefined,
    account: GCPServiceAccount | null = null,
    accountLoading = false
  ) {
    if (!gcpIdentity) return nothing;

    const mode = gcpIdentity.metadataMode;
    const record = matchingAccount(gcpIdentity, account);
    const email = gcpIdentity.serviceAccountEmail || record?.email || '';
    const displayName = record?.displayName?.trim() || '';
    const hasAccount = mode === 'assign' && Boolean(gcpIdentity.serviceAccountId || email);
    const verification = gcpVerificationDisplay(record, accountLoading);
    const verificationBadge = html`<sl-badge
      class="gcp-verification"
      data-state=${verification.state}
      variant=${verification.variant}
      >${verification.label}</sl-badge
    >`;

    return html`
      <div class="card">
        <h3 class="card-title">GCP Identity</h3>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Metadata Mode</span>
            <span class="info-value">
              <sl-badge class="gcp-mode" variant=${gcpModeVariant(mode)}
                >${gcpModeLabel(mode)}</sl-badge
              >
            </span>
          </div>
          ${email || displayName
            ? html`
                <div class="info-item">
                  <span class="info-label">Service Account</span>
                  ${displayName
                    ? html`
                        <span class="info-value gcp-sa-name">${displayName}</span>
                        ${email
                          ? html`<span class="info-subvalue mono gcp-sa-email">${email}</span>`
                          : nothing}
                      `
                    : html`<span class="info-value mono gcp-sa-email">${email}</span>`}
                </div>
              `
            : nothing}
          ${hasAccount
            ? html`
                <div class="info-item">
                  <span class="info-label">Verification</span>
                  <span class="info-value">
                    ${verification.detail
                      ? html`<sl-tooltip content=${verification.detail}
                          >${verificationBadge}</sl-tooltip
                        >`
                      : verificationBadge}
                  </span>
                </div>
              `
            : nothing}
          ${gcpIdentity.projectId
            ? html`
                <div class="info-item">
                  <span class="info-label">GCP Project</span>
                  <span class="info-value mono">${gcpIdentity.projectId}</span>
                </div>
              `
            : nothing}
        </div>
      </div>
    `;
  }

  private renderConfigLimitsCard(
    inline: AgentInlineConfig | undefined,
    effective?: Agent['effectiveConfig']
  ) {
    const effTurns = effective?.maxTurns && effective.maxTurns > 0 ? effective.maxTurns : undefined;
    const effModelCalls =
      effective?.maxModelCalls && effective.maxModelCalls > 0 ? effective.maxModelCalls : undefined;
    const effDuration = effective?.maxDuration?.trim() || undefined;
    const clamped = new Set(effective?.limitsClampedBy ?? []);

    const maxTurns = inline?.max_turns;
    const maxModelCalls = inline?.max_model_calls;
    const maxDuration = inline?.max_duration;

    // A stored 0 (or any zero duration spelling) is Unlimited, so the card hides
    // only when neither inline nor effective limits are present.
    if (
      maxTurns == null &&
      maxModelCalls == null &&
      !maxDuration &&
      effTurns == null &&
      effModelCalls == null &&
      !effDuration
    ) {
      return nothing;
    }

    const formatEffectiveOrInline = (
      effVal: number | string | undefined,
      inlineVal: number | string | undefined,
      limitKey: string
    ): string => {
      if (effVal !== undefined) {
        return clamped.has(limitKey) ? `${effVal} (hub ceiling)` : String(effVal);
      }
      return formatConfigLimit(inlineVal);
    };

    return html`
      <div class="card">
        <h3 class="card-title">Limits</h3>
        <div class="info-grid">
          <div class="info-item">
            <span class="info-label">Max Turns</span>
            <span class="info-value"
              >${formatEffectiveOrInline(effTurns, maxTurns, 'max_turns')}</span
            >
          </div>
          <div class="info-item">
            <span class="info-label">Max Model Calls</span>
            <span class="info-value"
              >${formatEffectiveOrInline(effModelCalls, maxModelCalls, 'max_model_calls')}</span
            >
          </div>
          <div class="info-item">
            <span class="info-label">Max Duration</span>
            <span class="info-value"
              >${formatEffectiveOrInline(effDuration, maxDuration, 'max_duration')}</span
            >
          </div>
        </div>
        <p
          class="limits-guardrail-note"
          style="margin: 0.75rem 0 0; font-size: 0.8125rem; color: var(--scion-text-muted);"
        >
          Turn and model-call limits are cooperative budget guardrails, not a security boundary.
        </p>
      </div>
    `;
  }

  private renderTelemetryCard(telemetry: TelemetryConfig | undefined) {
    if (!telemetry) return nothing;

    const enabledLabel = telemetry.enabled === false ? 'Disabled' : 'Enabled';
    const filter = telemetry.filter;
    const cloud = telemetry.cloud;
    const hub = telemetry.hub;

    const hasDestinations = cloud || hub;
    const hasFilter = filter?.events || filter?.attributes || filter?.sampling;

    return html`
      <div class="card">
        <h3 class="card-title">Telemetry</h3>
        <div
          class="info-grid"
          style="margin-bottom: ${hasDestinations || hasFilter ? '1.25rem' : '0'}"
        >
          <div class="info-item">
            <span class="info-label">Status</span>
            <span class="info-value">${enabledLabel}</span>
          </div>
        </div>

        ${hasDestinations
          ? html`
              <div class="telemetry-section">
                <div class="telemetry-section-title">Export Destinations</div>
                <div class="info-grid">
                  ${cloud
                    ? html`
                        <div class="info-item">
                          <span class="info-label">Cloud</span>
                          <span class="info-value"
                            >${cloud.enabled === false ? 'Disabled' : 'Enabled'}${cloud.provider
                              ? ` (${cloud.provider})`
                              : ''}${cloud.endpoint
                              ? html`<br /><span class="mono" style="font-size: 0.8rem"
                                    >${cloud.endpoint}</span
                                  >`
                              : ''}</span
                          >
                        </div>
                      `
                    : ''}
                  ${hub
                    ? html`
                        <div class="info-item">
                          <span class="info-label">Hub</span>
                          <span class="info-value"
                            >${hub.enabled === false ? 'Disabled' : 'Enabled'}${hub.report_interval
                              ? ` (${hub.report_interval})`
                              : ''}</span
                          >
                        </div>
                      `
                    : ''}
                </div>
              </div>
            `
          : ''}
        ${hasFilter
          ? html`
              <div class="telemetry-section">
                <div class="telemetry-section-title">Filters</div>
                ${filter?.events?.exclude?.length
                  ? html`
                      <div class="info-item" style="margin-bottom: 0.75rem">
                        <span class="info-label">Excluded Events</span>
                        <div class="tag-list">
                          ${filter.events.exclude.map(
                            (e) => html`<span class="tag-item">${e}</span>`
                          )}
                        </div>
                      </div>
                    `
                  : ''}
                ${filter?.events?.include?.length
                  ? html`
                      <div class="info-item" style="margin-bottom: 0.75rem">
                        <span class="info-label">Included Events</span>
                        <div class="tag-list">
                          ${filter.events.include.map(
                            (e) => html`<span class="tag-item">${e}</span>`
                          )}
                        </div>
                      </div>
                    `
                  : ''}
                ${filter?.attributes?.redact?.length
                  ? html`
                      <div class="info-item" style="margin-bottom: 0.75rem">
                        <span class="info-label">Redacted Attributes</span>
                        <div class="tag-list">
                          ${filter.attributes.redact.map(
                            (a) => html`<span class="tag-item">${a}</span>`
                          )}
                        </div>
                      </div>
                    `
                  : ''}
                ${filter?.attributes?.hash?.length
                  ? html`
                      <div class="info-item" style="margin-bottom: 0.75rem">
                        <span class="info-label">Hashed Attributes</span>
                        <div class="tag-list">
                          ${filter.attributes.hash.map(
                            (a) => html`<span class="tag-item">${a}</span>`
                          )}
                        </div>
                      </div>
                    `
                  : ''}
                ${filter?.sampling
                  ? html`
                      <div class="info-item">
                        <span class="info-label">Sampling</span>
                        <span class="info-value"
                          >${filter.sampling.default != null
                            ? `Default: ${(filter.sampling.default * 100).toFixed(0)}%`
                            : ''}${filter.sampling.rates
                            ? Object.entries(filter.sampling.rates)
                                .map(([k, v]) => `${k}: ${(v * 100).toFixed(0)}%`)
                                .join(', ')
                            : ''}</span
                        >
                      </div>
                    `
                  : ''}
              </div>
            `
          : ''}
        ${!hasDestinations && !hasFilter
          ? html`<div class="telemetry-empty">No detailed configuration</div>`
          : ''}
      </div>
    `;
  }

  private renderInitialTaskCard(cfg: AgentAppliedConfig | undefined) {
    if (this.isShellAgent(this.agent)) return nothing;
    const task = cfg?.task;
    if (!task) return nothing;

    return html`
      <div class="card">
        <h3 class="card-title">Initial Task</h3>
        <div class="task-summary">${task}</div>
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Notifications card
  // ---------------------------------------------------------------------------

  private renderNotificationsCard() {
    const hasUser = this.userNotifications.length > 0;
    const hasAgent = this.agentNotifications.length > 0;

    return html`
      <div class="card">
        <h3 class="card-title">Notifications</h3>
        ${!hasUser && !hasAgent
          ? html`<div class="notif-empty">
              <sl-icon name="bell-slash"></sl-icon>
              <span>No notifications for this agent</span>
            </div>`
          : html`
              ${hasUser
                ? html`
                    ${hasAgent
                      ? html`<div class="notif-section-title">Your Notifications</div>`
                      : nothing}
                    ${this.userNotifications.map((n) => this.renderNotifItem(n, true))}
                  `
                : nothing}
              ${hasAgent
                ? html`
                    ${hasUser
                      ? html`<div class="notif-section-title">Agent Notifications</div>`
                      : nothing}
                    ${this.agentNotifications.map((n) => this.renderNotifItem(n, false))}
                  `
                : nothing}
            `}
      </div>
    `;
  }

  private renderNotifItem(n: Notification, canAck: boolean) {
    return html`
      <div class="notif-list-item">
        <div class="notif-icon ${this.notifStatusClass(n.status)}">
          <sl-icon name=${this.notifStatusIcon(n.status)}></sl-icon>
        </div>
        <div class="notif-body">
          <div class="notif-message">${n.message}</div>
          <sl-tooltip content=${n.message} hoist>
            <span class="notif-truncation-badge" style="display:none">...</span>
          </sl-tooltip>
          <div class="notif-meta">
            <span>${this.formatRelativeTime(n.createdAt)}</span>
            ${canAck && !n.acknowledged
              ? html`<button class="notif-mark-read" @click=${() => this.ackNotification(n.id)}>
                  Mark read
                </button>`
              : nothing}
          </div>
        </div>
      </div>
    `;
  }

  private notifStatusIcon(status: string): string {
    switch (status) {
      case 'COMPLETED':
        return 'check-circle-fill';
      case 'WAITING_FOR_INPUT':
        return 'exclamation-circle-fill';
      case 'LIMITS_EXCEEDED':
        return 'x-circle-fill';
      default:
        return 'info-circle-fill';
    }
  }

  private notifStatusClass(status: string): string {
    switch (status) {
      case 'COMPLETED':
        return 'status-success';
      case 'WAITING_FOR_INPUT':
        return 'status-warning';
      case 'LIMITS_EXCEEDED':
        return 'status-danger';
      default:
        return 'status-info';
    }
  }

  private async ackNotification(id: string): Promise<void> {
    try {
      await apiFetch(`/api/v1/notifications/${id}/ack`, { method: 'POST' });
      this.userNotifications = this.userNotifications.filter((n) => n.id !== id);
    } catch {
      // Ignore
    }
  }

  override updated(changed: Map<string, unknown>): void {
    super.updated(changed);
    this.detectNotifTruncation();
  }

  private detectNotifTruncation(): void {
    const messages = this.shadowRoot?.querySelectorAll('.notif-message');
    if (!messages) return;
    messages.forEach((el) => {
      const badge = el.parentElement?.querySelector(
        '.notif-truncation-badge'
      ) as HTMLElement | null;
      if (!badge) return;
      badge.style.display = el.scrollHeight > el.clientHeight ? 'inline-flex' : 'none';
    });
  }

  // ---------------------------------------------------------------------------
  // Loading / Error states
  // ---------------------------------------------------------------------------

  private renderLoading() {
    return html`
      <scion-breadcrumb .crumbs=${agentParentTrail(this.projectNamed)}></scion-breadcrumb>
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading agent...</p>
      </div>
    `;
  }

  /**
   * Brief client-side-only state shown after a successful delete (or an
   * SSE `deleted` event for this agent), before the SPA redirect fires.
   * Action buttons are not rendered here, so there is nothing left to
   * click on the way out. "Redirecting…" is shown only while
   * `deleteRedirectTimer` is still pending; once it has fired (or was
   * skipped — hidden behind `/terminals`, see `isOnThisAgentRoute` — or
   * never re-armed after a disconnect/reconnect while deleted), the copy
   * drops back to a plain statement so the page never claims a redirect
   * that is not actually coming. The breadcrumb covers all of those cases, so
   * the user always has a way out.
   */
  private renderDeletedState(): TemplateResult {
    return html`
      <scion-breadcrumb
        data-testid="agent-deleted-link"
        .crumbs=${agentParentTrail(this.projectNamed)}
      ></scion-breadcrumb>
      <div class="loading-state" data-testid="agent-deleted-state">
        <sl-icon name="trash"></sl-icon>
        <p>Agent deleted.${this.deleteRedirectTimer ? ' Redirecting…' : ''}</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <scion-breadcrumb .crumbs=${agentParentTrail(this.projectNamed)}></scion-breadcrumb>

      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Agent</h2>
        <p>There was a problem loading this agent.</p>
        <div class="error-details">${this.error || 'Agent not found'}</div>
        <sl-button variant="primary" @click=${() => this.loadData()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-agent-detail': ScionPageAgentDetail;
  }
}
