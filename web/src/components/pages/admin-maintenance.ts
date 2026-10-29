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
 * Admin Maintenance Operations page component
 *
 * Displays maintenance operations and migrations with execution support.
 * Phase 1: Read-only display of operations and migration checklist.
 * Phase 2: Migration execution with dry-run support and status polling.
 * Phase 3: Routine operation execution with run history.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError, parseApiError } from '../../client/api.js';
import { loadAdminStatus } from '../../client/admin-status.js';
import { waitForHubRestart } from '../../client/hub-restart.js';
import { withTimeout } from '../../client/with-timeout.js';
import { stateManager } from '../../client/state.js';
import { canReissueAllScopes } from '../../lib/admin-permissions.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { showToast } from '../../utils/toast.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { renderReleaseNotesLink } from '../../utils/release-notes-link.js';
import { LAST_UPDATE_LABELS, renderInstallerUnsupported } from './admin-maintenance-update-card.js';
import type {
  LastUpdate,
  PendingUpdate,
  UpdateRequestDetail,
  UpdateStatus,
} from './admin-maintenance-update-card.js';

export type { LastUpdate, PendingUpdate, UpdateStatus };

interface MaintenanceOperation {
  id: string;
  key: string;
  title: string;
  description: string;
  category: string;
  status: string;
  createdAt: string;
  startedAt?: string;
  completedAt?: string;
  startedBy?: string;
  result?: string;
}

/**
 * Run status of a maintenance operation or migration. `restarting` is in
 * progress: an update-binary run whose binary is installed and whose hub is
 * about to restart; the hub settles it on boot.
 */
export type MaintenanceRunStatus = 'pending' | 'running' | 'restarting' | 'completed' | 'failed';

/** Whether a run status means the run has not finished yet. */
export function isRunInProgress(status: string | undefined): boolean {
  return status === 'running' || status === 'restarting';
}

interface MaintenanceRun {
  id: string;
  operationKey?: string;
  /** A {@link MaintenanceRunStatus}; unknown values render as-is. */
  status: MaintenanceRunStatus | (string & {});
  startedAt: string;
  completedAt?: string;
  startedBy?: string;
  result?: string;
  log?: string;
}

interface MaintenanceOperationWithRun extends MaintenanceOperation {
  lastRun?: MaintenanceRun;
}

/**
 * Last systemd unit check of Rebuild from git, from the unitStatus field of
 * GET /api/v1/admin/maintenance/operations. Only match and drift are stored.
 */
interface UnitDriftStatus {
  status: 'match' | 'drift';
  checkedAt: string;
  commit: string;
  branch: string;
  service: string;
  summary?: string[];
}

interface MaintenanceResponse {
  migrations: MaintenanceOperation[] | null;
  operations: MaintenanceOperationWithRun[] | null;
  unitStatus?: UnitDriftStatus;
}

interface UpdateCommitInfo {
  hash: string;
  subject: string;
}

/** Source-tier update check result (git-based). */
interface SourceUpdateCheckResult {
  tier: 'source';
  update_available: boolean;
  current_commit: string;
  latest_commit: string;
  current_branch: string;
  tracking_ref: string;
  commits_behind: number;
  new_commits?: UpdateCommitInfo[];
}

/** Binary-tier update check result (release-based). */
interface BinaryUpdateCheckResult {
  tier: 'binary';
  update_available: boolean;
  current_version: string;
  latest_version: string;
  channel: string;
  release_url?: string;
}

type UpdateCheckResult = SourceUpdateCheckResult | BinaryUpdateCheckResult;

/** Stored update notification from GET /api/v1/admin/maintenance/update-available. */
interface UpdateAvailableResponse {
  update_available: boolean;
  update?: {
    version: string;
    channel: string;
    release_url?: string;
    detected_at: string;
  };
}

/** Whether a parsed body has the parts of an update-status the page relies on. */
function isUpdateStatus(data: unknown): data is UpdateStatus {
  if (!data || typeof data !== 'object') return false;
  const d = data as Partial<UpdateStatus>;
  return (
    !!d.current &&
    !!d.installer &&
    !!d.channel &&
    (d.mode?.effective === 'git' || d.mode?.effective === 'release')
  );
}

/**
 * A readable message for a rejected update-binary run request, by the
 * error code the hub sends.
 */
export function describeUpdateRunError(code: string, message: string, target: string): string {
  switch (code) {
    case 'already_running':
      return `The hub is already running ${target}. ${message}`;
    case 'downgrade_not_yet_supported':
      return `${target} is older than the running version; downgrades are not supported yet. ${message}`;
    case 'unknown_param':
      return `The hub rejected the update request (unknown parameter): ${message}`;
    default:
      return `Update to ${target} could not start: ${message}`;
  }
}

/**
 * An update-binary run this page follows through the hub restart: one it
 * started, or one the hub's pendingUpdate names.
 */
interface TrackedUpdate {
  runId: string;
  /** What was asked for: a tag, or latest:<channel>. */
  target: string;
  /** The tag the hub must come back on; null when not known. */
  expectVersion: string | null;
  /** Last run status seen by a successful poll. */
  lastStatus: string;
  /**
   * The version the hub ran when the update started (or when this page
   * picked the run up); undefined when not known. Recorded once, so a
   * status reload during the restart does not change it.
   */
  fromVersion?: string | undefined;
}

/** Channel, pin and class of an update picked in the release picker. */
interface UpdateConfirmOptions {
  channel: string;
  pin: string;
  releaseClass: string;
}

/** How long hubErrorEnvelope waits for an error body. */
const ERROR_BODY_TIMEOUT_MS = 5_000;

/**
 * The hub's own error envelope ({error: {code, message}}) of a failed
 * response, or null when the body is something else, such as a reverse
 * proxy's 502 page while the hub is down, or when it does not arrive
 * within timeoutMs.
 */
export async function hubErrorEnvelope(
  res: Response,
  timeoutMs = ERROR_BODY_TIMEOUT_MS
): Promise<{ code: string; message: string } | null> {
  try {
    // Bounded: a body that stalls after the headers counts as not the
    // hub's envelope, so the caller does not hang on it.
    const body = (await withTimeout(res.json() as Promise<unknown>, timeoutMs)) as
      | { error?: { code?: unknown; message?: unknown } }
      | null
      | undefined;
    const err = body?.error;
    if (err && typeof err === 'object' && typeof err.code === 'string' && err.code) {
      const message = typeof err.message === 'string' && err.message ? err.message : err.code;
      return { code: err.code, message };
    }
  } catch {
    // Not JSON: not the hub's envelope.
  }
  return null;
}

/** Maintenance mode state from GET and PUT /api/v1/admin/maintenance. */
interface MaintenanceStateResponse {
  enabled: boolean;
  break_glass?: boolean;
}

/**
 * One failed agent in a reset-auth-all result. For a classified failure
 * `error` is a plain reason, not the raw error; `code` names the category.
 */
interface ResetAuthAllFailure {
  id: string;
  name: string;
  error: string;
  /** Failure category, e.g. delegation_creator_missing or agent_not_in_standing. */
  code?: string;
  /** Deny cause of a structural delegation denial. */
  cause?: string;
  /** Creator (depth 1) or ancestor whose delegation is missing. */
  ancestor_id?: string;
  ancestor_depth?: number;
}

/**
 * Result of POST /api/v1/admin/agents/reset-auth-all. The hub sends null
 * for an empty list. failed_by_code counts the failures per code, with
 * "unclassified" for failures that carry none.
 */
interface ResetAuthAllResult {
  succeeded: { id: string; name: string }[] | null;
  failed: ResetAuthAllFailure[] | null;
  total: number;
  failed_by_code?: Record<string, number>;
}

/** One agent's outcome in a bulk scope re-issue. */
interface ScopeReissueAgent {
  id: string;
  name: string;
  project_id?: string;
  depth?: number;
  /** changed | noop | refused | push_failed */
  outcome: string;
  cause?: string;
  added?: string[] | null;
  removed?: string[] | null;
  role_before?: string;
  role_after?: string;
  op_id?: string;
  dispatch_error?: string;
  /**
   * Set on a changed agent whose new credential was delivered but whose
   * running agent did not confirm the reload.
   */
  reload_unconfirmed?: string;
}

/** An agent named in one of the bulk scope re-issue result lists. */
interface ScopeReissueRef {
  id: string;
  name: string;
  cause?: string;
}

/**
 * Result of POST /api/v1/admin/agents/reset-auth-all with
 * {"reissue_scopes": true}. The hub sends null for an empty list.
 */
interface ScopeReissueAllResult {
  batch_op_id: string;
  dry_run: boolean;
  total: number;
  succeeded: ScopeReissueRef[] | null;
  noop: ScopeReissueRef[] | null;
  refused: ScopeReissueRef[] | null;
  push_failed: ScopeReissueRef[] | null;
  agents: ScopeReissueAgent[] | null;
  depth_unresolved: string[] | null;
  batch_audit_recorded: boolean;
}

/** The number of agents whose reload the running agent did not confirm. */
function reloadUnconfirmedCount(r: ScopeReissueAllResult): number {
  return (r.agents ?? []).filter((a) => !!a.reload_unconfirmed).length;
}

/** The counts line of a scope re-issue result, worded as the CLI does. */
function scopeReissueSummary(r: ScopeReissueAllResult): string {
  const verb = r.dry_run ? 'would change' : 'changed';
  return (
    `${r.total} agents: ${r.succeeded?.length ?? 0} ${verb}, ` +
    `${r.noop?.length ?? 0} unchanged, ${r.refused?.length ?? 0} refused, ` +
    `${r.push_failed?.length ?? 0} token not delivered, ` +
    `${reloadUnconfirmedCount(r)} reload unconfirmed`
  );
}

/** Per-agent lines for every agent whose outcome is not noop. */
function scopeReissueAgentLines(r: ScopeReissueAllResult): string {
  const lines: string[] = [];
  for (const a of r.agents ?? []) {
    if (a.outcome === 'noop') continue;
    lines.push(`${a.name || a.id} (${a.id}) ${a.outcome}${a.cause ? `: ${a.cause}` : ''}`);
    if (a.role_after && a.role_before !== a.role_after) {
      lines.push(`  role:     ${a.role_before ?? ''} -> ${a.role_after}`);
    }
    if (a.added?.length) lines.push(`  added:    ${a.added.join(', ')}`);
    if (a.removed?.length) lines.push(`  removed:  ${a.removed.join(', ')}`);
    if (a.dispatch_error) lines.push(`  token not delivered: ${a.dispatch_error}`);
    if (a.reload_unconfirmed) lines.push(`  reload unconfirmed: ${a.reload_unconfirmed}`);
  }
  return lines.join('\n');
}

/** The note for agents the bulk run could not order by delegation depth. */
function unresolvedNote(n: number): string {
  return (
    `${n} agents could not be ordered (their delegation record could not be read); ` +
    'see their results below.'
  );
}

/** Display labels for reset-auth-all failure codes. */
const RESET_AUTH_CODE_LABELS: Record<string, string> = {
  delegation_creator_missing: 'Creator delegation missing',
  delegation_own_missing: 'Own delegation missing',
  delegation_ceiling: 'Delegation chain invalid',
  agent_not_in_standing: 'Not in good standing',
  runtime_unavailable: 'Runtime unavailable',
  reload_unconfirmed: 'Reload unconfirmed',
  internal_error: 'Internal error',
  unclassified: 'Other',
};

function resetAuthCodeLabel(code: string): string {
  return RESET_AUTH_CODE_LABELS[code] ?? code;
}

/** The per-category failure counts, sorted by code, on one line. */
function resetAuthCategorySummary(result: ResetAuthAllResult): string {
  return Object.entries(result.failed_by_code ?? {})
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([code, n]) => `${resetAuthCodeLabel(code)}: ${n}`)
    .join(' \u00b7 ');
}

/** One failed agent: its name, the reason and, when coded, the category. */
function resetAuthFailureLine(f: ResetAuthAllFailure): string {
  const category = f.code ? ` [${resetAuthCodeLabel(f.code)}]` : '';
  return `${f.name || f.id}: ${f.error}${category}`;
}

/**
 * How long Restart Hub waits for the hub to come back. Shorter than the
 * update default: a plain restart does not install anything.
 */
const RESTART_HUB_TIMEOUT_MS = 60_000;

/**
 * Failed list polls in a row, while an update run is in progress, after
 * which the page treats the hub as restarting and waits on /healthz.
 */
const FAILED_POLLS_BEFORE_RECONNECT = 2;

/** Minimum gap between run status checks during a restart wait. */
const RUN_CHECK_INTERVAL_MS = 5_000;

@customElement('scion-page-admin-maintenance')
export class ScionPageAdminMaintenance extends LitElement {
  /** Re-renders absolute times when the display timezone changes. */
  readonly _zone = new DisplayZoneController(this);

  @state()
  private loading = true;

  @state()
  private error: string | null = null;

  @state()
  private migrations: MaintenanceOperation[] = [];

  @state()
  private operations: MaintenanceOperationWithRun[] = [];

  /** Key of migration being confirmed for execution via dialog. */
  @state()
  private runDialogKey: string | null = null;

  /** Category of the item being run (migration or operation). */
  @state()
  private runDialogCategory: string = 'migration';

  /** Dry-run checkbox state in the run dialog. */
  @state()
  private runDialogDryRun = false;

  /** Whether a run request is in-flight. */
  @state()
  private runInProgress = false;

  /**
   * Open state of the completed-migrations group. Kept on the component so it
   * survives reloads that swap the page content for the loading spinner.
   */
  @state()
  private completedMigrationsOpen = false;

  /** Polling timer for running operations/migrations. */
  private pollTimer: ReturnType<typeof setInterval> | null = null;

  /** Keys of operations whose run history is expanded. */
  @state()
  private expandedHistory: Set<string> = new Set();

  /** Loaded run history keyed by operation key. */
  @state()
  private runHistory: Map<string, MaintenanceRun[]> = new Map();

  /** Whether maintenance mode is enabled. */
  @state()
  private maintenanceEnabled = false;

  /**
   * True when the hub reports break_glass: a workstation hub started in
   * admin mode (SCION_SERVER_ADMIN_MODE / settings.yaml admin_mode) stays in
   * maintenance whatever is saved here.
   */
  @state()
  private maintenanceBreakGlass = false;

  /** Run detail currently being viewed. */
  @state()
  private viewingRun: MaintenanceRun | null = null;

  /** Whether the restart hub confirmation dialog is open. */
  @state()
  private restartDialogOpen = false;

  /** Whether a restart request is in-flight. */
  @state()
  private restartLoading = false;

  /** Whether the bulk reset-auth request is in-flight. */
  @state()
  private resetAuthAllLoading = false;

  /** Result of the last bulk reset-auth request. */
  @state()
  private resetAuthAllResult: ResetAuthAllResult | null = null;

  /**
   * Whether the scope re-issue controls are shown: the user is a
   * super-admin per the shared admin status. The hub stays the authority.
   */
  @state()
  private canReissueScopes = false;

  /** Whether a scope re-issue request (dry run or apply) is in-flight. */
  @state()
  private reissueLoading = false;

  /** Result of the last scope re-issue request. */
  @state()
  private reissueResult: ScopeReissueAllResult | null = null;

  /** Error of the last scope re-issue request. */
  @state()
  private reissueError: string | null = null;

  /** Whether the apply confirmation dialog is open. */
  @state()
  private reissueConfirmOpen = false;

  /** Server version, mode and installer, from GET .../update-status. */
  @state()
  private updateStatus: UpdateStatus | null = null;

  /** Load error of update-status; the page does not guess the mode then. */
  @state()
  private updateStatusError: string | null = null;

  /** Target tag of the update being confirmed in the dialog (null: closed). */
  @state()
  private updateConfirmTarget: string | null = null;

  /**
   * Channel, pin and class of a target picked in the release picker; null
   * for the banner and the operation card, which post pin 'latest' only.
   */
  @state()
  private updateConfirmOptions: UpdateConfirmOptions | null = null;

  /** The update run followed through the restart (null: none). */
  @state()
  private trackedUpdate: TrackedUpdate | null = null;

  /** Stops the update restart wait when the page is left. */
  private updateWaitAbort: AbortController | null = null;

  /** Stops the Restart Hub wait when the page is left. */
  private restartWaitAbort: AbortController | null = null;

  /**
   * Id of the restarting run whose pendingUpdate was already checked, so a
   * run that is not followed is peeked once, not on every poll.
   */
  private peekedRestartingRunId: string | null = null;

  /** Consecutive failed list polls while the tracked update is in progress. */
  private failedPollsWhileUpdating = 0;

  /** True while waiting for the hub to come back after an update restart. */
  @state()
  private updateReconnecting = false;

  /** Shown when the hub did not come back on the update target in time. */
  @state()
  private updateReconnectError: string | null = null;

  /** Stored update-available notification (from scheduled check). */
  @state()
  private updateAvailable: UpdateAvailableResponse | null = null;

  /** Last systemd unit check of Rebuild from git (null when none). */
  @state()
  private unitStatus: UnitDriftStatus | null = null;

  /** Whether an update-binary operation is being triggered. */
  @state()
  private applyUpdateLoading = false;

  /** Whether the dismiss request is in-flight. */
  @state()
  private dismissUpdateLoading = false;

  /** Update check state for rebuild-server / check-for-updates. */
  @state()
  private updateCheckLoading = false;

  @state()
  private updateCheckError: string | null = null;

  @state()
  private updateCheckResult: UpdateCheckResult | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 2rem;
    }

    .header sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    /* -- Maintenance mode toggle ------------------------------------------ */

    .maintenance-mode-toggle {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .maintenance-mode-toggle .toggle-track {
      position: relative;
      width: 36px;
      height: 20px;
      background: var(--scion-border, #e2e8f0);
      border-radius: 10px;
      cursor: pointer;
      transition: background 0.2s ease;
      border: none;
      padding: 0;
      flex-shrink: 0;
    }

    .maintenance-mode-toggle .toggle-track:hover {
      background: var(--scion-text-muted, #94a3b8);
    }

    .maintenance-mode-toggle .toggle-track.active {
      background: var(--scion-warning, #f59e0b);
    }

    .maintenance-mode-toggle .toggle-track.active:hover {
      background: #d97706;
    }

    .maintenance-mode-toggle .toggle-knob {
      position: absolute;
      top: 2px;
      left: 2px;
      width: 16px;
      height: 16px;
      background: white;
      border-radius: 50%;
      transition: transform 0.2s ease;
      pointer-events: none;
    }

    .maintenance-mode-toggle .toggle-track.active .toggle-knob {
      transform: translateX(16px);
    }

    .maintenance-mode-toggle .toggle-label {
      font-size: 0.875rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
    }

    /* -- Sections --------------------------------------------------------- */

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .section-title {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .section-description {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    /* -- Cards ------------------------------------------------------------ */

    .card-list {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .card-list + .completed-migrations {
      display: block;
      margin-top: 1rem;
    }

    .completed-migrations::part(base) {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .completed-migrations::part(header) {
      font-size: 0.9375rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      padding: 0.875rem 1rem;
    }

    .completed-migrations::part(content) {
      padding: 0 1rem 1rem 1rem;
    }

    .card {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 1.25rem;
    }

    .card-header {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 0.5rem;
    }

    .card-header sl-icon {
      font-size: 1.25rem;
      flex-shrink: 0;
    }

    .card-header sl-icon.pending {
      color: var(--scion-text-muted, #64748b);
    }

    .card-header sl-icon.completed {
      color: var(--sl-color-success-600, #16a34a);
    }

    .card-header sl-icon.failed {
      color: var(--sl-color-danger-600, #dc2626);
    }

    .card-header sl-icon.running {
      color: var(--scion-primary, #3b82f6);
    }

    .card-title {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      flex: 1;
    }

    .card-description {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      line-height: 1.5;
      margin-bottom: 0.75rem;
    }

    .card-meta {
      display: flex;
      gap: 1.5rem;
      flex-wrap: wrap;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .card-meta span {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
    }

    .card-footer {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-top: 0.75rem;
    }

    .reissue-scopes {
      margin-top: 1rem;
      padding-top: 0.75rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .reissue-scopes .card-footer {
      margin-top: 0;
      margin-bottom: 0.5rem;
    }

    .reissue-actions {
      display: inline-flex;
      gap: 0.5rem;
    }

    /* -- Result log ------------------------------------------------------- */

    .result-log {
      margin-top: 0.75rem;
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.8125rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      white-space: pre-wrap;
      word-break: break-word;
      max-height: 300px;
      overflow-y: auto;
      color: var(--scion-text, #1e293b);
    }

    .result-error {
      color: var(--sl-color-danger-700, #b91c1c);
    }

    /* -- Status badges ---------------------------------------------------- */

    .status-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
    }

    .status-badge.pending {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #a16207);
    }

    .status-badge.completed {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .status-badge.failed {
      background: var(--sl-color-danger-100, #fee2e2);
      color: var(--sl-color-danger-700, #b91c1c);
    }

    .status-badge.running,
    .status-badge.restarting {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    /* -- Update check banner --------------------------------------------- */

    .update-check-banner {
      margin-top: 0.5rem;
      margin-bottom: 0.5rem;
      padding: 0.625rem 0.75rem;
      border-radius: 0.375rem;
      border: 1px solid var(--sl-color-primary-200, #bfdbfe);
      background: var(--sl-color-primary-50, #eff6ff);
      font-size: 0.8125rem;
    }

    .update-check-banner.up-to-date {
      border-color: var(--sl-color-success-200, #bbf7d0);
      background: var(--sl-color-success-50, #f0fdf4);
    }

    .update-check-header {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      font-weight: 600;
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .update-check-banner.up-to-date .update-check-header {
      color: var(--sl-color-success-700, #15803d);
    }

    .update-check-header sl-icon {
      font-size: 0.875rem;
    }

    .update-check-commits {
      margin-top: 0.375rem;
      max-height: 8rem;
      overflow-y: auto;
      font-family: var(--sl-font-mono, monospace);
      line-height: 1.5;
      color: var(--scion-text, #1e293b);
    }

    .update-check-commits .commit-hash {
      color: var(--sl-color-primary-600, #2563eb);
      margin-right: 0.375rem;
    }

    .update-check-error {
      margin-top: 0.375rem;
      font-size: 0.8125rem;
      color: var(--sl-color-danger-700, #b91c1c);
    }

    /* -- Run history table ------------------------------------------------ */

    .history-toggle {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.8125rem;
      color: var(--scion-primary, #3b82f6);
      cursor: pointer;
      border: none;
      background: none;
      padding: 0.25rem 0;
      margin-top: 0.5rem;
    }

    .history-toggle:hover {
      text-decoration: underline;
    }

    .history-toggle sl-icon {
      font-size: 0.75rem;
      transition: transform 0.2s;
    }

    .history-toggle.expanded sl-icon {
      transform: rotate(90deg);
    }

    .run-history-table {
      width: 100%;
      border-collapse: collapse;
      margin-top: 0.75rem;
      font-size: 0.8125rem;
    }

    .run-history-table th {
      text-align: left;
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      color: var(--scion-text-muted, #64748b);
      font-weight: 500;
    }

    .run-history-table td {
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border-subtle, #f1f5f9);
      color: var(--scion-text, #1e293b);
    }

    .run-history-table tr {
      cursor: pointer;
    }

    .run-history-table tr:hover td {
      background: var(--scion-bg-subtle, #f8fafc);
    }

    /* -- Dialog ----------------------------------------------------------- */

    .dialog-body {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .dialog-body p {
      margin: 0;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      line-height: 1.5;
    }

    /* -- Empty state ------------------------------------------------------ */

    .empty-inline {
      padding: 1.5rem;
      text-align: center;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }

    /* -- Loading / Error -------------------------------------------------- */

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

    /* -- Update available banner (top-level) ------------------------------- */

    .update-available-banner {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.75rem 1rem;
      margin-bottom: 1.5rem;
      border-radius: var(--scion-radius, 0.5rem);
      border: 1px solid var(--sl-color-primary-200, #bfdbfe);
      background: var(--sl-color-primary-50, #eff6ff);
    }

    .update-available-banner sl-icon {
      font-size: 1.25rem;
      color: var(--sl-color-primary-600, #2563eb);
      flex-shrink: 0;
    }

    .update-available-banner .banner-text {
      flex: 1;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .update-available-banner .banner-text strong {
      font-weight: 600;
    }

    .unit-drift-banner {
      align-items: flex-start;
      border-color: var(--sl-color-warning-300, #fcd34d);
      background: var(--sl-color-warning-50, #fffbeb);
    }

    .unit-drift-banner sl-icon {
      color: var(--sl-color-warning-600, #d97706);
    }

    .unit-drift-banner .unit-drift-summary {
      margin-top: 0.5rem;
    }

    .unit-drift-banner .unit-drift-note {
      display: block;
      margin-top: 0.25rem;
      font-size: 0.8125rem;
    }

    .update-available-banner .banner-actions {
      display: flex;
      gap: 0.5rem;
      flex-shrink: 0;
    }

    /* -- Binary update check result --------------------------------------- */

    .release-info {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
      margin-top: 0.375rem;
      font-size: 0.8125rem;
      color: var(--scion-text, #1e293b);
    }

    .release-info .release-field {
      display: flex;
      gap: 0.5rem;
    }

    .release-info .release-label {
      color: var(--scion-text-muted, #64748b);
      min-width: 7rem;
    }

    .release-info a {
      color: var(--sl-color-primary-600, #2563eb);
      text-decoration: none;
    }

    .release-info a:hover {
      text-decoration: underline;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadData();
    void this.fetchUpdateStatus();
    void this.fetchMaintenanceState();
    void this.fetchUpdateAvailable();
    void this.loadReissueCapability();
  }

  private async loadReissueCapability(): Promise<void> {
    try {
      const status = await loadAdminStatus(stateManager.getCurrentUserId());
      this.canReissueScopes = canReissueAllScopes(status);
    } catch {
      this.canReissueScopes = false;
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopPolling();
    this.updateWaitAbort?.abort();
    this.updateWaitAbort = null;
    this.restartWaitAbort?.abort();
    this.restartWaitAbort = null;
  }

  private async fetchMaintenanceState(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/admin/maintenance');
      if (res.ok) {
        const data = (await res.json()) as MaintenanceStateResponse;
        this.maintenanceEnabled = data.enabled;
        this.maintenanceBreakGlass = data.break_glass === true;
      }
    } catch {
      // Silently ignore — toggle will default to off.
    }
  }

  private async toggleMaintenance(): Promise<void> {
    const newValue = !this.maintenanceEnabled;
    try {
      const res = await apiFetch('/api/v1/admin/maintenance', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled: newValue }),
      });
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      const data = (await res.json()) as MaintenanceStateResponse;
      this.maintenanceEnabled = data.enabled;
      this.maintenanceBreakGlass = data.break_glass === true;
    } catch (err) {
      showToast(err instanceof Error ? err.message : 'Failed to update maintenance mode');
      // The toggle renders from maintenanceEnabled, which is set only from a
      // server response; re-read it so the toggle shows the saved state.
      await this.fetchMaintenanceState();
    }
  }

  private async fetchUpdateAvailable(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/admin/maintenance/update-available');
      if (res.ok) {
        const data = (await res.json()) as UpdateAvailableResponse;
        this.updateAvailable = data;
      }
    } catch {
      // Silently ignore — banner will not show.
    }
  }

  /**
   * Loads GET .../update-status. On failure the page shows an error and
   * hides the mode-specific operations rather than guessing the mode.
   */
  private async fetchUpdateStatus(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/admin/maintenance/update-status');
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      const data: unknown = await res.json();
      if (!isUpdateStatus(data)) {
        throw new Error('Unexpected update-status response');
      }
      this.updateStatus = data;
      this.updateStatusError = null;
    } catch (err) {
      this.updateStatus = null;
      this.updateStatusError = err instanceof Error ? err.message : 'Failed to load update status';
    }
  }

  /**
   * Opens the update dialog from the update-binary operation card. The
   * target is the notified or checked release tag when one is known;
   * otherwise check-updates resolves the latest tag in the hub's channel,
   * so the page knows which version the hub must come back on. Only when
   * that fails is latest:<channel> posted, for the hub to resolve.
   */
  private async openUpdateFromCard(): Promise<void> {
    const notified = this.updateAvailable?.update_available
      ? this.updateAvailable.update?.version
      : undefined;
    if (notified) {
      this.openUpdateConfirm(notified);
      return;
    }
    let checked = this.updateCheckResult;
    if (checked?.tier !== 'binary') {
      await this.checkForUpdates();
      checked = this.updateCheckResult;
    }
    if (checked?.tier === 'binary') {
      if (!checked.update_available) {
        showToast(`Already up to date (${checked.current_version}).`, 'success');
        return;
      }
      if (checked.latest_version) {
        this.openUpdateConfirm(checked.latest_version);
        return;
      }
    }
    const channel = this.updateStatus?.channel.effective;
    this.openUpdateConfirm(channel ? `latest:${channel}` : null);
  }

  private openUpdateConfirm(
    target: string | null,
    options: UpdateConfirmOptions | null = null
  ): void {
    if (!target) {
      showToast('No update target is known: update status has not loaded.');
      return;
    }
    this.updateConfirmTarget = target;
    this.updateConfirmOptions = options;
  }

  private closeUpdateConfirm(): void {
    if (!this.applyUpdateLoading) {
      this.updateConfirmTarget = null;
      this.updateConfirmOptions = null;
    }
  }

  /**
   * Starts update-binary for the confirmed target. The request carries only
   * target_version, pin and (from the release picker) channel; the hub
   * resolves the release assets itself.
   */
  private async applyUpdate(): Promise<void> {
    const target = this.updateConfirmTarget;
    if (!target) return;
    const options = this.updateConfirmOptions;
    const params: Record<string, string> = options
      ? { target_version: target, channel: options.channel, pin: options.pin }
      : { target_version: target, pin: 'latest' };
    const fromVersion = this.updateStatus?.current.version;
    this.applyUpdateLoading = true;
    try {
      const response = await apiFetch('/api/v1/admin/maintenance/operations/update-binary/run', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ params }),
      });

      if (!response.ok) {
        const info = await parseApiError(response, `HTTP ${response.status}`);
        throw new Error(describeUpdateRunError(info.code, info.message, target));
      }
      const started = (await response.json().catch(() => ({}))) as { runId?: string };

      showToast(
        `Update to ${target} started. The hub will restart when it is installed.`,
        'success'
      );
      this.updateConfirmTarget = null;
      this.updateConfirmOptions = null;
      this.updateAvailable = null;
      this.updateReconnectError = null;
      if (started.runId) {
        this.trackedUpdate = {
          runId: started.runId,
          target,
          expectVersion: target.startsWith('latest:') ? null : target,
          lastStatus: 'running',
          fromVersion,
        };
      } else {
        // Without a run id no result can be tied to this update; the run
        // is still followed if a poll sees it running (trackUpdateRun).
        showToast('The hub did not return a run id; follow the update in Run History.', 'warning');
      }

      // Reload operations and poll the run (see trackUpdateRun). A polling
      // load: if the hub is already going down, the failure counts towards
      // the restart wait instead of replacing the page with an error.
      await this.loadData(true);
      this.startPolling();
    } catch (err) {
      // The dialog stays open so the user can read the error and cancel.
      showToast(err instanceof Error ? err.message : 'Failed to start binary update');
    } finally {
      this.applyUpdateLoading = false;
    }
  }

  /**
   * Follows a run that is already restarting when the page first sees it,
   * only when a fresh pendingUpdate names it. A restarting run without one
   * (for example one boot reconciliation did not settle) is shown as in
   * progress and polled, but not waited for. Checked once per run.
   */
  private async adoptRestartingRun(run: MaintenanceRun): Promise<void> {
    if (this.peekedRestartingRunId === run.id) return;
    this.peekedRestartingRunId = run.id;
    const pending = await this.peekPendingUpdate();
    if (!this.isConnected) return;
    if (this.trackedUpdate || !pending?.runId || pending.runId !== run.id) return;
    this.trackedUpdate = {
      runId: run.id,
      target: pending.to,
      expectVersion: pending.to || null,
      lastStatus: 'restarting',
      fromVersion: pending.from || this.updateStatus?.current.version,
    };
    void this.reconnectAfterUpdate();
  }

  private updateBinaryRun(): MaintenanceRun | undefined {
    return this.operations.find((op) => op.key === 'update-binary')?.lastRun;
  }

  /**
   * Called after each successful list load: follows the update-binary run.
   * A run seen running is followed whoever started it (another tab, a
   * reload during the download); restarting switches to the hub restart
   * wait; a finished run (the restart came and went between polls, or the
   * run failed before it) reports its result.
   */
  private trackUpdateRun(): void {
    if (!this.isConnected) return;
    const run = this.updateBinaryRun();
    if (!this.trackedUpdate && run?.status === 'running') {
      this.trackedUpdate = {
        runId: run.id,
        target: '',
        expectVersion: null,
        lastStatus: 'running',
        fromVersion: this.updateStatus?.current.version,
      };
    }
    const tracked = this.trackedUpdate;
    if (!tracked) {
      if (run?.status === 'restarting') void this.adoptRestartingRun(run);
      return;
    }
    if (!run || run.id !== tracked.runId) return;
    tracked.lastStatus = run.status;
    if (run.status === 'restarting') {
      void this.reconnectAfterUpdate();
    } else if (!isRunInProgress(run.status) && !this.updateReconnecting) {
      this.trackedUpdate = null;
      void this.fetchUpdateStatus().then(() => {
        this.refreshUpdateNoticesAfterRun(tracked);
        this.reportUpdateResult(tracked, run.status);
      });
    }
  }

  /**
   * The version the hub ran before a tracked update: the one recorded when
   * the run was tracked, else the hub's own record for this run
   * (pendingUpdate.from while it restarts, lastUpdate.from once settled).
   * A run adopted on first load may be tracked before update-status has
   * loaded. Undefined when none is known.
   */
  private updateFromVersion(tracked: TrackedUpdate): string | undefined {
    if (tracked.fromVersion) return tracked.fromVersion;
    const pending = this.updateStatus?.pendingUpdate ?? null;
    if (pending && pending.runId === tracked.runId && pending.from) return pending.from;
    const last = this.updateStatus?.lastUpdate ?? null;
    if (last && last.runId === tracked.runId && last.from) return last.from;
    return undefined;
  }

  /**
   * Brings the "Update available" notices up to date once an update run
   * has ended and update-status has been reloaded (ptone/scion#6557).
   *
   * - The hub recorded this run as verified and runs a different version
   *   than before it: the notices were computed against the old version.
   *   The update check result is dropped, and the stored notification is
   *   cleared and re-read.
   * - Otherwise (failed, rolled back, not verified yet, or a version not
   *   known): the check result stays, and the stored notification is only
   *   re-read. Starting the update hid the banner, and the hub keeps the
   *   notification for a retry, so the banner comes back.
   *
   * fetchUpdateAvailable handles its own errors and is not awaited.
   */
  private refreshUpdateNoticesAfterRun(tracked: TrackedUpdate): void {
    const from = this.updateFromVersion(tracked);
    const after = this.updateStatus?.current.version;
    const last = this.updateStatus?.lastUpdate ?? null;
    const verified = !!last && last.runId === tracked.runId && last.status === 'verified';
    if (verified && from && after && from !== after) {
      this.updateCheckResult = null;
      this.updateCheckError = null;
      // Cleared first: if the re-read fails, a notice about the old
      // version must not stay up.
      this.updateAvailable = null;
    }
    void this.fetchUpdateAvailable();
  }

  /** When the restart wait last checked the run status (epoch ms). */
  private lastRunCheckAt = 0;

  /**
   * For the restart wait, while the hub answers but has not restarted:
   * whether the run has already finished (for example Restart() failed
   * and the run was marked failed), so the wait need not run out. Reads
   * the list without touching page state, at most every few seconds.
   */
  private async trackedRunFinished(runId: string, signal: AbortSignal): Promise<boolean> {
    const now = Date.now();
    if (now - this.lastRunCheckAt < RUN_CHECK_INTERVAL_MS) return false;
    this.lastRunCheckAt = now;
    try {
      const res = await apiFetch('/api/v1/admin/maintenance/operations', {
        authErrors: 'caller',
        signal,
      });
      if (!res.ok) return false;
      const data = (await res.json()) as MaintenanceResponse;
      const run = (data.operations ?? []).find((op) => op.key === 'update-binary')?.lastRun;
      return !!run && run.id === runId && !isRunInProgress(run.status);
    } catch {
      return false;
    }
  }

  /**
   * Reads pendingUpdate without touching the page state: the hub may be
   * going down, and a failure must not replace the header with an error.
   */
  private async peekPendingUpdate(): Promise<PendingUpdate | null> {
    try {
      const res = await apiFetch('/api/v1/admin/maintenance/update-status', {
        authErrors: 'caller',
      });
      if (!res.ok) return null;
      const data: unknown = await res.json();
      return isUpdateStatus(data) ? data.pendingUpdate : null;
    } catch {
      return null;
    }
  }

  /**
   * Waits for the hub to restart after the tracked update run reached
   * restarting (or a poll failed while it was in progress), then reloads
   * update-status and reports this run's result.
   */
  private async reconnectAfterUpdate(): Promise<void> {
    const tracked = this.trackedUpdate;
    // A poll or peek that resolves after the page was left must not start
    // a wait nothing would abort.
    if (this.updateReconnecting || !tracked || !this.isConnected) return;
    // This run is handled here: if it is still restarting after the wait,
    // the post-wait reload must not adopt it for a second wait.
    this.peekedRestartingRunId = tracked.runId;
    this.updateReconnecting = true;
    this.updateReconnectError = null;
    this.stopPolling();
    this.updateWaitAbort?.abort();
    this.updateWaitAbort = new AbortController();
    const signal = this.updateWaitAbort.signal;
    try {
      if (!tracked.expectVersion) {
        // latest:<channel>: the hub writes the concrete tag to pendingUpdate
        // just before it restarts; it may still answer.
        const pending = await this.peekPendingUpdate();
        if (pending && pending.runId === tracked.runId && pending.to) {
          this.trackedUpdate = { ...tracked, expectVersion: pending.to };
        }
      }
      const expectVersion = this.trackedUpdate?.expectVersion ?? undefined;
      const result = await waitForHubRestart({
        expectVersion,
        signal,
        stopWhen: () => this.trackedRunFinished(tracked.runId, signal),
      });
      if (!result.ok && result.aborted) return;
      this.trackedUpdate = null;
      if (!result.ok && !result.observed) {
        this.updateReconnectError = this.notBackMessage(expectVersion);
        return;
      }
      // The hub answers (on the target, or on another version after the
      // wait ran out): show what it recorded.
      await this.fetchUpdateStatus();
      await this.loadData(true);
      const run = this.updateBinaryRun();
      const runIsTracked = !!run && run.id === tracked.runId;
      if (!result.ok && runIsTracked && run.status === 'running') {
        // A blip, not a restart: the run is still going; keep polling it.
        this.trackedUpdate = { ...tracked, lastStatus: 'running' };
        this.startPolling();
        return;
      }
      this.refreshUpdateNoticesAfterRun(tracked);
      if (result.ok) {
        this.reportUpdateResult(tracked, 'restarted');
        return;
      }
      const recorded = this.updateStatus?.lastUpdate?.runId === tracked.runId;
      if (recorded || (runIsTracked && run.status === 'failed')) {
        this.reportUpdateResult(tracked, run?.status ?? '');
        return;
      }
      this.updateReconnectError = this.notBackMessage(expectVersion);
    } finally {
      this.updateReconnecting = false;
    }
  }

  private notBackMessage(expectVersion: string | undefined): string {
    return (
      `Hub did not come back on ${expectVersion ?? 'the new version'}. ` +
      'Check journalctl -u scion-hub'
    );
  }

  /**
   * Reports the outcome of a tracked run from lastUpdate, but only when
   * lastUpdate belongs to that run; an older result is never reported as
   * this run's.
   */
  private reportUpdateResult(tracked: TrackedUpdate, runStatus: string): void {
    const last = this.updateStatus?.lastUpdate;
    if (last && tracked.runId && last.runId === tracked.runId) {
      if (last.status === 'verified') {
        showToast(`Hub updated to ${last.to} (verified).`, 'success');
      } else {
        showToast(
          `Update to ${last.to} ${LAST_UPDATE_LABELS[last.status] ?? last.status}.`,
          'warning'
        );
      }
    } else if (runStatus === 'failed') {
      const target = tracked.target || 'the new version';
      showToast(`Update to ${target} failed. See the run log.`, 'warning');
    } else {
      showToast('Hub restarted; the update result is not recorded yet.', 'warning');
    }
  }

  private async dismissUpdate(): Promise<void> {
    this.dismissUpdateLoading = true;
    try {
      const res = await apiFetch('/api/v1/admin/maintenance/update-available', {
        method: 'DELETE',
      });
      if (res.ok) {
        this.updateAvailable = null;
      }
    } catch {
      showToast('Failed to dismiss update notification');
    } finally {
      this.dismissUpdateLoading = false;
    }
  }

  private async loadData(isPolling = false): Promise<void> {
    // Only show loading spinner on initial load, not during poll refreshes.
    if (!isPolling) {
      this.loading = true;
      this.error = null;
    }

    try {
      const response = await apiFetch('/api/v1/admin/maintenance/operations');
      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`)
        );
      }

      const data = (await response.json()) as MaintenanceResponse;
      const newMigrations = data.migrations ?? [];
      const newOperations = data.operations ?? [];

      // Only update state (and trigger re-render) if data actually changed.
      if (JSON.stringify(this.migrations) !== JSON.stringify(newMigrations)) {
        this.migrations = newMigrations;
      }
      if (JSON.stringify(this.operations) !== JSON.stringify(newOperations)) {
        this.operations = newOperations;
      }
      const newUnitStatus = data.unitStatus ?? null;
      if (JSON.stringify(this.unitStatus) !== JSON.stringify(newUnitStatus)) {
        this.unitStatus = newUnitStatus;
      }

      this.failedPollsWhileUpdating = 0;
      // A later successful load recovers a page left on the error view.
      if (this.error) this.error = null;
      this.trackUpdateRun();
      if (this.updateReconnecting && this.trackedUpdate) return;

      // Start polling if any migration or operation run is active.
      const hasRunning =
        this.migrations.some((m) => m.status === 'running') ||
        this.operations.some((op) => isRunInProgress(op.lastRun?.status));
      if (hasRunning) {
        this.startPolling();
      } else {
        this.stopPolling();
      }
    } catch (err) {
      console.error('Failed to load maintenance operations:', err);
      // The hub stops answering when it restarts; usually the poll never
      // sees the brief restarting status, only failures. One failure may be
      // a blip during the download; a second in a row starts the wait.
      if (isPolling && this.trackedUpdate && isRunInProgress(this.trackedUpdate.lastStatus)) {
        this.failedPollsWhileUpdating++;
        if (
          this.trackedUpdate.lastStatus === 'restarting' ||
          this.failedPollsWhileUpdating >= FAILED_POLLS_BEFORE_RECONNECT
        ) {
          void this.reconnectAfterUpdate();
        }
      }
      if (!isPolling) {
        this.error = err instanceof Error ? err.message : 'Failed to load maintenance operations';
      }
    } finally {
      if (!isPolling) {
        this.loading = false;
      }
    }
  }

  private startPolling(): void {
    if (this.pollTimer || !this.isConnected) return;
    this.pollTimer = setInterval(() => void this.loadData(true), 3000);
  }

  private stopPolling(): void {
    if (this.pollTimer) {
      clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
  }

  private formatDate(dateString: string | undefined): string {
    if (!dateString) return '';
    return formatInstantWithZone(dateString, 'date');
  }

  private formatDateTime(dateString: string | undefined): string {
    if (!dateString) return '';
    return formatInstantWithZone(dateString);
  }

  private formatRelativeTime(dateString: string | undefined): string {
    if (!dateString || Number.isNaN(new Date(dateString).getTime())) return '';
    return formatRelative(dateString);
  }

  private formatDuration(startStr: string, endStr?: string): string {
    if (!startStr || !endStr) return '--';
    try {
      const start = new Date(startStr).getTime();
      const end = new Date(endStr).getTime();
      const diffMs = end - start;
      if (diffMs < 0 || isNaN(diffMs)) return '--';
      if (diffMs < 1000) return '<1s';
      const seconds = Math.round(diffMs / 1000);
      if (seconds < 60) return `${seconds}s`;
      const minutes = Math.floor(seconds / 60);
      const remainSecs = seconds % 60;
      return `${minutes}m ${remainSecs}s`;
    } catch {
      return '--';
    }
  }

  private statusIcon(status: string): string {
    switch (status) {
      case 'completed':
        return 'check-circle-fill';
      case 'failed':
        return 'exclamation-circle-fill';
      case 'running':
        return 'hourglass-split';
      case 'restarting':
        return 'arrow-repeat';
      default:
        return 'circle';
    }
  }

  // -- Migration execution ------------------------------------------------

  private openRunDialog(key: string, category: string): void {
    this.runDialogKey = key;
    this.runDialogCategory = category;
    this.runDialogDryRun = false;
  }

  private closeRunDialog(): void {
    if (!this.runInProgress) {
      this.runDialogKey = null;
      this.runDialogDryRun = false;
    }
  }

  private async executeRunMigration(): Promise<void> {
    if (!this.runDialogKey) return;

    this.runInProgress = true;
    try {
      const response = await apiFetch(
        `/api/v1/admin/maintenance/migrations/${this.runDialogKey}/run`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            params: {
              dryRun: this.runDialogDryRun,
            },
          }),
        }
      );

      if (!response.ok) {
        const errMsg = await extractApiError(response, `HTTP ${response.status}`);
        throw new Error(errMsg);
      }

      this.runDialogKey = null;
      this.runDialogDryRun = false;

      // Reload and start polling for status.
      await this.loadData();
      this.startPolling();
    } catch (err) {
      console.error('Failed to start migration:', err);
      // The page stays as it is; a failed action is reported as a toast.
      // The dialog stays open so the user can retry or cancel.
      showToast(err instanceof Error ? err.message : 'Failed to start migration');
    } finally {
      this.runInProgress = false;
    }
  }

  // -- Operation execution ------------------------------------------------

  private async executeRunOperation(): Promise<void> {
    if (!this.runDialogKey) return;

    this.runInProgress = true;
    try {
      const response = await apiFetch(
        `/api/v1/admin/maintenance/operations/${this.runDialogKey}/run`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ params: {} }),
        }
      );

      if (!response.ok) {
        const errMsg = await extractApiError(response, `HTTP ${response.status}`);
        throw new Error(errMsg);
      }

      this.runDialogKey = null;

      // Reload and start polling for status.
      await this.loadData();
      this.startPolling();
    } catch (err) {
      console.error('Failed to start operation:', err);
      // The page stays as it is; a failed action is reported as a toast.
      // The dialog stays open so the user can retry or cancel.
      showToast(err instanceof Error ? err.message : 'Failed to start operation');
    } finally {
      this.runInProgress = false;
    }
  }

  // -- Run history --------------------------------------------------------

  private async toggleHistory(key: string): Promise<void> {
    const expanded = new Set(this.expandedHistory);
    if (expanded.has(key)) {
      expanded.delete(key);
      this.expandedHistory = expanded;
      return;
    }

    expanded.add(key);
    this.expandedHistory = expanded;

    // Load history if not cached.
    if (!this.runHistory.has(key)) {
      await this.loadRunHistory(key);
    }
  }

  private async loadRunHistory(key: string): Promise<void> {
    try {
      const response = await apiFetch(`/api/v1/admin/maintenance/operations/${key}/runs?limit=20`);
      if (!response.ok) return;

      const data = (await response.json()) as { runs: MaintenanceRun[] | null };
      const updated = new Map(this.runHistory);
      updated.set(key, data.runs ?? []);
      this.runHistory = updated;
    } catch {
      // Silently ignore load errors for history.
    }
  }

  private async viewRunDetail(run: MaintenanceRun): Promise<void> {
    // If we already have log data, show it immediately.
    if (run.log) {
      this.viewingRun = run;
      return;
    }

    // Otherwise fetch the full run detail.
    try {
      const key = run.operationKey ?? '';
      const response = await apiFetch(`/api/v1/admin/maintenance/operations/${key}/runs/${run.id}`);
      if (response.ok) {
        const detail = (await response.json()) as MaintenanceRun;
        this.viewingRun = detail;
      } else {
        this.viewingRun = run;
      }
    } catch {
      this.viewingRun = run;
    }
  }

  private closeRunDetail(): void {
    this.viewingRun = null;
  }

  private parseMigrationResult(
    resultStr: string | undefined
  ): { log?: string; error?: string; dryRun?: boolean } | null {
    if (!resultStr) return null;
    try {
      return JSON.parse(resultStr) as { log?: string; error?: string; dryRun?: boolean };
    } catch {
      return null;
    }
  }

  // -- Rendering ----------------------------------------------------------

  override render() {
    return html`
      <div class="header">
        <sl-icon name="wrench-adjustable"></sl-icon>
        <h1>Maintenance</h1>
      </div>

      ${this.loading
        ? this.renderLoading()
        : this.error
          ? this.renderError()
          : this.renderContent()}
      ${this.renderRunDialog()} ${this.renderRunDetailDialog()} ${this.renderRestartDialog()}
      ${this.renderReissueConfirmDialog()} ${this.renderUpdateConfirmDialog()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading maintenance operations...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadData()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderContent() {
    return html`
      ${this.renderServerVersion()} ${this.renderUpdateAvailableBanner()}
      ${this.renderUnitDriftBanner()} ${this.renderMaintenanceMode()} ${this.renderQuickActions()}
      ${this.renderMigrations()} ${this.renderOperations()}
    `;
  }

  /**
   * Banner shown while the last Rebuild from git found the installed systemd
   * unit different from the template. It carries the full remediation, as the
   * run log may be missing or superseded. All values are text bindings.
   */
  private renderUnitDriftBanner() {
    const st = this.unitStatus;
    if (!st || st.status !== 'drift') {
      return nothing;
    }
    const where = st.commit ? `commit ${st.commit}` : 'the checked-out commit';
    const rev = st.branch ? `${where} on branch ${st.branch}` : where;
    const checked = this.formatDateTime(st.checkedAt) || 'unknown time';
    const summary = st.summary ?? [];
    return html`
      <div class="update-available-banner unit-drift-banner">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <span class="banner-text">
          <strong>Re-provision needed:</strong> the installed systemd unit for ${st.service} differs
          from the template (checked ${checked}). Rebuild from git updates the binary only.
          <br />
          To apply the template, run <code>scripts/starter-hub/gce-start-hub.sh --full</code> from
          the workstation and checkout used to provision this hub, at ${rev}, with the same flags
          and environment used at provisioning (for example --no-tls, HUB_BASE_URL). Running it
          without the original flags can reconfigure the hub (for example enable TLS on a --no-tls
          hub). --full also re-applies settings.yaml, hub.env, the Caddyfile and the sudoers rules
          from that workstation, then restarts the hub.
          <br />
          Intentional local changes belong in a drop-in (${st.service}.d), which is not compared.
          This notice clears after a Rebuild from git reports a match.
          ${summary.length
            ? html`<div class="result-log unit-drift-summary">${summary.join('\n')}</div>`
            : nothing}
          <span class="unit-drift-note">Details may also be in the Rebuild from git run log.</span>
        </span>
      </div>
    `;
  }

  /**
   * "Server Version & Updates" card. It shows update-status and picks a
   * release; the update itself (confirm, run, restart wait) stays here.
   */
  private renderServerVersion() {
    const busy =
      this.applyUpdateLoading ||
      this.updateReconnecting ||
      isRunInProgress(this.updateBinaryRun()?.status);
    return html`
      <scion-admin-maintenance-update-card
        .status=${this.updateStatus}
        .statusError=${this.updateStatusError}
        ?reconnecting=${this.updateReconnecting}
        .reconnectVersion=${this.trackedUpdate?.expectVersion ?? null}
        .reconnectError=${this.updateReconnectError}
        ?busy=${busy}
        @reload-status=${() => this.fetchUpdateStatus()}
        @view-run=${(e: CustomEvent<{ runId: string }>) =>
          void this.viewRunDetail({
            id: e.detail.runId,
            operationKey: 'update-binary',
            status: '',
            startedAt: '',
          })}
        @update-request=${(e: CustomEvent<UpdateRequestDetail>) =>
          this.openUpdateConfirm(e.detail.target, {
            channel: e.detail.channel,
            pin: e.detail.pin,
            releaseClass: e.detail.releaseClass,
          })}
      ></scion-admin-maintenance-update-card>
    `;
  }

  /** What a picker update changes besides the version: channel and pin. */
  private renderUpdateConfirmDetails(
    target: string,
    options: UpdateConfirmOptions | null
  ): TemplateResult | typeof nothing {
    if (!options) return nothing;
    const from = this.updateStatus?.channel.effective || this.updateStatus?.current.channel;
    return html`
      ${options.releaseClass === 'switch-newer' || (from && from !== options.channel)
        ? html`<p class="confirm-channel-switch">
            This switches the
            hub${from ? html` from the <strong>${from}</strong> channel` : nothing} to the
            <strong>${options.channel}</strong> channel.
          </p>`
        : nothing}
      ${options.pin === 'latest'
        ? html`<p class="confirm-pin">
            The hub follows the latest <strong>${options.channel}</strong> release.
          </p>`
        : html`<p class="confirm-pin">
            The hub stays pinned to <strong>${target}</strong>: automatic updates do not move it
            until you pick Latest again.
          </p>`}
    `;
  }

  private renderUpdateConfirmDialog() {
    const target = this.updateConfirmTarget;
    if (!target) return nothing;
    const latestChannel = target.startsWith('latest:') ? target.slice('latest:'.length) : null;
    const unsupported = this.updateStatus ? !this.updateStatus.installer.supported : false;
    return html`
      <sl-dialog label="Update Hub" open @sl-request-close=${() => this.closeUpdateConfirm()}>
        <div class="dialog-body">
          <p>
            ${latestChannel
              ? html`Update the hub to the latest release in the
                  <strong>${latestChannel}</strong> channel?`
              : html`Update the hub to <strong>${target}</strong>?`}
          </p>
          ${this.renderUpdateConfirmDetails(target, this.updateConfirmOptions)}
          <p style="color: var(--sl-color-warning-700, #a16207); font-weight: 500;">
            The hub restarts once the new binary is installed. You will temporarily lose
            connectivity; this page reconnects when the hub is back.
          </p>
          ${unsupported && this.updateStatus
            ? renderInstallerUnsupported(this.updateStatus)
            : nothing}
        </div>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => this.closeUpdateConfirm()}
          ?disabled=${this.applyUpdateLoading}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="warning"
          class="confirm-update"
          ?loading=${this.applyUpdateLoading}
          ?disabled=${unsupported}
          @click=${() => this.applyUpdate()}
        >
          <sl-icon slot="prefix" name="download"></sl-icon>
          Update &amp; Restart
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderUpdateAvailableBanner() {
    if (!this.updateAvailable?.update_available || !this.updateAvailable.update) {
      return nothing;
    }

    const update = this.updateAvailable.update;

    return html`
      <div class="update-available-banner">
        <sl-icon name="arrow-up-circle"></sl-icon>
        <span class="banner-text">
          <strong>Update available: ${update.version}</strong>
          ${update.channel ? html` (${update.channel} channel)` : nothing}
        </span>
        <div class="banner-actions">
          <sl-button
            variant="primary"
            size="small"
            ?loading=${this.applyUpdateLoading}
            @click=${() => this.openUpdateConfirm(update.version)}
          >
            <sl-icon slot="prefix" name="download"></sl-icon>
            Apply Update
          </sl-button>
          <sl-button
            variant="default"
            size="small"
            ?loading=${this.dismissUpdateLoading}
            @click=${() => this.dismissUpdate()}
          >
            Dismiss
          </sl-button>
        </div>
      </div>
    `;
  }

  private renderQuickActions() {
    return html`
      <div class="section">
        <h2 class="section-title">Quick Actions</h2>
        <p class="section-description">One-off administrative actions across all agents.</p>
        <div class="card-list">
          <div class="card">
            <div class="card-header">
              <sl-icon name="arrow-clockwise" class="pending"></sl-icon>
              <span class="card-title">Restart Hub</span>
              ${this.restartLoading
                ? html`<sl-button size="small" disabled loading>Restarting...</sl-button>`
                : html`
                    <sl-button
                      variant="warning"
                      size="small"
                      @click=${() => this.openRestartDialog()}
                    >
                      <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                      Restart
                    </sl-button>
                  `}
            </div>
            <div class="card-description">
              Restart the hub service via systemd. This will briefly drop all active connections
              (including WebSockets) while the process restarts. Use this after changing settings
              that require a restart to take effect.
            </div>
          </div>
          <div class="card">
            <div class="card-header">
              <sl-icon name="key" class="pending"></sl-icon>
              <span class="card-title">Reset Auth &mdash; All Running Agents</span>
              ${this.resetAuthAllLoading
                ? html`<sl-button size="small" disabled loading>Running...</sl-button>`
                : html`
                    <sl-button
                      variant="warning"
                      size="small"
                      @click=${() => this.handleResetAuthAll()}
                    >
                      <sl-icon slot="prefix" name="key"></sl-icon>
                      Reset Auth
                    </sl-button>
                  `}
            </div>
            <div class="card-description">
              Inject a fresh auth token into every running agent without restarting them. The token
              refresh loop in each agent will pick up the new credentials automatically.
            </div>
            ${this.resetAuthAllResult
              ? html`
                  <div class="card-meta">
                    <span>
                      Total: ${this.resetAuthAllResult.total} &middot; Succeeded:
                      ${this.resetAuthAllResult.succeeded?.length ?? 0} &middot; Failed:
                      ${this.resetAuthAllResult.failed?.length ?? 0}
                    </span>
                  </div>
                  ${Object.keys(this.resetAuthAllResult.failed_by_code ?? {}).length > 0
                    ? html`
                        <div class="card-meta reset-auth-categories">
                          <span>${resetAuthCategorySummary(this.resetAuthAllResult)}</span>
                        </div>
                      `
                    : nothing}
                  ${(this.resetAuthAllResult.failed?.length ?? 0) > 0
                    ? html`
                        <div class="result-log result-error">
                          ${this.resetAuthAllResult.failed!.map(resetAuthFailureLine).join('\n')}
                        </div>
                      `
                    : nothing}
                `
              : nothing}
            ${this.canReissueScopes ? this.renderReissueScopes() : nothing}
          </div>
        </div>
      </div>
    `;
  }

  private renderReissueScopes() {
    return html`
      <div class="reissue-scopes">
        <div class="card-footer">
          <span class="card-title">Re-issue scopes</span>
          <span class="reissue-actions">
            <sl-button
              class="reissue-dry-run"
              size="small"
              ?disabled=${this.reissueLoading}
              ?loading=${this.reissueLoading && !this.reissueConfirmOpen}
              @click=${() => this.handleReissueDryRun()}
            >
              <sl-icon slot="prefix" name="shield-check"></sl-icon>
              Dry Run
            </sl-button>
            ${this.reissueResult?.dry_run
              ? html`
                  <sl-button
                    class="reissue-apply"
                    variant="warning"
                    size="small"
                    ?disabled=${this.reissueLoading}
                    @click=${() => this.openReissueConfirm()}
                  >
                    <sl-icon slot="prefix" name="key"></sl-icon>
                    Apply
                  </sl-button>
                `
              : nothing}
          </span>
        </div>
        <div class="card-description">
          Re-issue every agent's role scopes from its delegator's current authority, the same as
          scion reset-auth --all --reissue-scopes. A dry run shows what would change; nothing
          changes until you apply it.
        </div>
        ${this.reissueError
          ? html`<div class="result-log result-error reissue-error">${this.reissueError}</div>`
          : nothing}
        ${this.reissueResult ? this.renderReissueResult(this.reissueResult) : nothing}
      </div>
    `;
  }

  private renderReissueResult(r: ScopeReissueAllResult) {
    const lines = scopeReissueAgentLines(r);
    const unresolved = r.depth_unresolved?.length ?? 0;
    const pushFailed = r.push_failed?.length ?? 0;
    const warnings: string[] = [];
    if (!r.batch_audit_recorded) {
      warnings.push(
        `The hub could not record the batch audit entry (batch ${r.batch_op_id}); ` +
          'the per-agent results stand.'
      );
    }
    if (pushFailed > 0) {
      warnings.push(
        `${pushFailed} agents did not receive their new token; run reset-auth for each.`
      );
    }
    const reloadUnconfirmed = reloadUnconfirmedCount(r);
    if (reloadUnconfirmed > 0) {
      warnings.push(
        `${reloadUnconfirmed} agents received their new token but did not confirm the reload; ` +
          'if one keeps failing to authenticate, restart it.'
      );
    }
    return html`
      <div class="card-meta reissue-summary">
        <span>${scopeReissueSummary(r)}</span>
      </div>
      ${r.dry_run
        ? html`
            <div class="card-meta reissue-dry-run-note">
              <span>Dry run: nothing was changed. Apply to make these changes.</span>
            </div>
          `
        : nothing}
      ${unresolved > 0
        ? html`
            <div class="card-meta reissue-unresolved">
              <span>${unresolvedNote(unresolved)}</span>
            </div>
          `
        : nothing}
      ${lines ? html`<div class="result-log reissue-agents">${lines}</div>` : nothing}
      ${warnings.length > 0
        ? html`<div class="result-log result-error reissue-warnings">${warnings.join('\n')}</div>`
        : nothing}
    `;
  }

  private renderReissueConfirmDialog() {
    if (!this.reissueConfirmOpen) return nothing;
    const r = this.reissueResult;
    return html`
      <sl-dialog
        class="reissue-confirm"
        label="Apply Scope Re-issue"
        open
        @sl-request-close=${(e: Event) => this.closeReissueConfirm(e)}
      >
        <div class="dialog-body">
          <p>Re-issue role scopes for every agent on the hub?</p>
          ${r ? html`<p>Last dry run: ${scopeReissueSummary(r)}.</p>` : nothing}
          <p style="color: var(--sl-color-warning-700, #a16207); font-weight: 500;">
            Changed agents get new tokens with their re-issued scopes. The result may differ from
            the dry run if agents changed since.
          </p>
        </div>
        <sl-button
          slot="footer"
          variant="default"
          class="reissue-cancel"
          ?disabled=${this.reissueLoading}
          @click=${() => this.closeReissueConfirm()}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="warning"
          class="reissue-confirm-apply"
          ?loading=${this.reissueLoading}
          @click=${() => this.handleReissueApply()}
        >
          <sl-icon slot="prefix" name="key"></sl-icon>
          Apply
        </sl-button>
      </sl-dialog>
    `;
  }

  private openReissueConfirm(): void {
    if (this.reissueResult?.dry_run) this.reissueConfirmOpen = true;
  }

  /**
   * Closes the apply confirmation. While a request is in flight the dialog
   * stays open: the sl-request-close event is cancelled so it is not hidden.
   */
  private closeReissueConfirm(e?: Event): void {
    if (this.reissueLoading) {
      e?.preventDefault();
      return;
    }
    this.reissueConfirmOpen = false;
  }

  /** Runs the bulk scope re-issue as a dry run and shows its result. */
  private async handleReissueDryRun(): Promise<void> {
    await this.runReissue(true);
  }

  /**
   * Applies the bulk scope re-issue; reached only from the confirm dialog.
   * A click while a request is in flight sends nothing.
   */
  private async handleReissueApply(): Promise<void> {
    if (!this.reissueConfirmOpen || this.reissueLoading) return;
    await this.runReissue(false);
    this.reissueConfirmOpen = false;
  }

  private async runReissue(dryRun: boolean): Promise<void> {
    this.reissueLoading = true;
    this.reissueError = null;
    this.reissueResult = null;
    try {
      const response = await apiFetch('/api/v1/admin/agents/reset-auth-all', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ reissue_scopes: true, dry_run: dryRun }),
        suppressAccessDeniedToast: true,
      });
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      this.reissueResult = (await response.json()) as ScopeReissueAllResult;
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'Failed to re-issue scopes';
      this.reissueError = `${dryRun ? 'Dry run' : 'Apply'} failed: ${msg}`;
    } finally {
      this.reissueLoading = false;
    }
  }

  private async handleResetAuthAll() {
    this.resetAuthAllLoading = true;
    this.resetAuthAllResult = null;
    try {
      const response = await apiFetch('/api/v1/admin/agents/reset-auth-all', {
        method: 'POST',
      });
      if (!response.ok) {
        const errMsg = await extractApiError(response, `HTTP ${response.status}`);
        throw new Error(errMsg);
      }
      this.resetAuthAllResult = (await response.json()) as ResetAuthAllResult;
    } catch (err) {
      showToast(err instanceof Error ? err.message : 'Failed to reset auth for all agents');
    } finally {
      this.resetAuthAllLoading = false;
    }
  }

  private openRestartDialog(): void {
    this.restartDialogOpen = true;
  }

  private closeRestartDialog(): void {
    if (!this.restartLoading) {
      this.restartDialogOpen = false;
    }
  }

  /** A fresh signal for the Restart Hub wait, aborted on disconnect. */
  private newRestartHubSignal(): AbortSignal {
    this.restartWaitAbort?.abort();
    this.restartWaitAbort = new AbortController();
    return this.restartWaitAbort.signal;
  }

  private async handleRestartHub(): Promise<void> {
    this.restartLoading = true;
    this.restartDialogOpen = false;

    // Fire the restart request. The server will restart and likely drop the
    // connection before a response arrives, so we expect a network error or
    // 502 from the reverse proxy. Either outcome means the restart was
    // initiated — not that it failed. Only the hub's own error envelope
    // (for example 409 conflict while an update runs) means
    // it refused, and then there is nothing to wait for.
    try {
      const res = await apiFetch('/api/v1/admin/maintenance/restart', { method: 'POST' });
      const refusal = res.ok ? null : await hubErrorEnvelope(res);
      if (refusal) {
        // apiFetch already shows the access-denied toast for a 403.
        if (res.status !== 403) {
          showToast(`Restart refused: ${refusal.message}`, 'warning');
        }
        this.restartLoading = false;
        return;
      }
    } catch {
      // Expected — the server went away.
    }

    // Wait for /healthz to answer with a fresh uptime, confirming the
    // process actually restarted.
    const result = await waitForHubRestart({
      timeoutMs: RESTART_HUB_TIMEOUT_MS,
      signal: this.newRestartHubSignal(),
    });
    if (!result.ok && result.aborted) {
      this.restartLoading = false;
      return;
    }
    if (result.ok) {
      showToast('Server restarted successfully.', 'success');
    } else if (result.observed) {
      showToast(
        `Server responded but appears not to have restarted (uptime: ${result.observed.uptime ?? 'unknown'}).`,
        'warning'
      );
    } else {
      showToast('Server did not respond after restart. It may still be starting up.', 'warning', {
        icon: 'exclamation-triangle',
        duration: 10000,
      });
    }
    this.restartLoading = false;
  }

  private renderMaintenanceMode() {
    return html`
      <div class="section">
        <h2 class="section-title">Maintenance Mode</h2>
        <p class="section-description">
          When maintenance mode is enabled, only administrators can log in. All other users will be
          temporarily blocked until maintenance mode is disabled.
        </p>
        <div class="maintenance-mode-toggle">
          <button
            class="toggle-track ${this.maintenanceEnabled ? 'active' : ''}"
            @click=${() => {
              void this.toggleMaintenance();
            }}
            aria-label="Toggle maintenance mode"
          >
            <span class="toggle-knob"></span>
          </button>
          <span class="toggle-label"> ${this.maintenanceEnabled ? 'Enabled' : 'Disabled'} </span>
        </div>
        ${this.maintenanceBreakGlass
          ? html`<p class="section-description break-glass-notice">
              Maintenance mode is forced on by the server's startup configuration
              (SCION_SERVER_ADMIN_MODE or <code>admin_mode</code> in settings.yaml). Turning it off
              here is saved but has no effect until the server is restarted without that setting.
              <code>admin_mode</code> in settings.yaml can only be cleared by editing the file.
            </p>`
          : nothing}
      </div>
    `;
  }

  private renderMigrations() {
    const completed = this.migrations.filter((m) => m.status === 'completed');
    const active = this.migrations.filter((m) => m.status !== 'completed');
    return html`
      <div class="section">
        <h2 class="section-title">Migrations</h2>
        <p class="section-description">
          One-time data migrations that transition the system between states. Completed migrations
          cannot be re-run from the UI.
        </p>
        ${this.migrations.length === 0
          ? html`<div class="empty-inline">No migrations registered.</div>`
          : html`
              ${active.length > 0
                ? html`
                    <div class="card-list">${active.map((m) => this.renderMigrationCard(m))}</div>
                  `
                : nothing}
              ${this.renderCompletedMigrations(completed)}
            `}
      </div>
    `;
  }

  /**
   * Completed migrations are grouped in one collapsible element, collapsed by
   * default. Its open state is tracked in completedMigrationsOpen from
   * sl-show/sl-hide and bound back, so it is kept across poll refreshes and
   * across reloads that re-create the element.
   */
  private renderCompletedMigrations(completed: MaintenanceOperation[]) {
    if (completed.length === 0) return nothing;
    return html`
      <sl-details
        class="completed-migrations"
        summary="Completed migrations (${completed.length})"
        ?open=${this.completedMigrationsOpen}
        @sl-show=${(e: Event) => this.onCompletedMigrationsToggle(e, true)}
        @sl-hide=${(e: Event) => this.onCompletedMigrationsToggle(e, false)}
      >
        <div class="card-list">${completed.map((m) => this.renderMigrationCard(m))}</div>
      </sl-details>
    `;
  }

  private onCompletedMigrationsToggle(e: Event, open: boolean): void {
    // Ignore sl-show/sl-hide bubbling up from elements inside the cards.
    if (e.target !== e.currentTarget) return;
    this.completedMigrationsOpen = open;
  }

  private renderMigrationCard(m: MaintenanceOperation) {
    const canRun = m.status === 'pending' || m.status === 'failed';
    const isRunning = m.status === 'running';
    const result = this.parseMigrationResult(m.result);

    return html`
      <div class="card">
        <div class="card-header">
          ${isRunning
            ? html`<sl-spinner style="font-size: 1.25rem;"></sl-spinner>`
            : html`<sl-icon name="${this.statusIcon(m.status)}" class="${m.status}"></sl-icon>`}
          <span class="card-title">${m.title}</span>
          <span class="status-badge ${m.status}">${m.status}</span>
        </div>
        <div class="card-description">${m.description}</div>
        <div class="card-meta">
          <span>Created: ${this.formatDate(m.createdAt)}</span>
          ${m.completedAt
            ? html`<span>Completed: ${this.formatDate(m.completedAt)}</span>`
            : nothing}
          ${m.startedBy ? html`<span>By: ${m.startedBy}</span>` : nothing}
        </div>
        ${result?.log
          ? html`<div class="result-log ${result.error ? 'result-error' : ''}">${result.log}</div>`
          : nothing}
        ${result?.error && !result.log
          ? html`<div class="result-log result-error">${result.error}</div>`
          : nothing}
        ${canRun || isRunning
          ? html`
              <div class="card-footer">
                <div></div>
                ${canRun
                  ? html`
                      <sl-button
                        variant="primary"
                        size="small"
                        @click=${() => this.openRunDialog(m.key, 'migration')}
                      >
                        <sl-icon slot="prefix" name="play-circle"></sl-icon>
                        ${m.status === 'failed' ? 'Retry' : 'Run'}
                      </sl-button>
                    `
                  : html` <sl-button size="small" disabled loading> Running... </sl-button> `}
              </div>
            `
          : nothing}
      </div>
    `;
  }

  /** Operations hidden in release mode (they need the git checkout). */
  private static readonly SOURCE_ONLY_OPS = new Set([
    'rebuild-server',
    'rebuild-web',
    'rebuild-container-binaries',
  ]);

  /** Operations hidden in git mode. */
  private static readonly BINARY_ONLY_OPS = new Set(['update-binary']);

  /**
   * Operations shown for the hub's effective mode from update-status. Until
   * it loads (or when it fails), both mode-specific sets stay hidden.
   */
  private get visibleOperations(): MaintenanceOperationWithRun[] {
    const mode = this.updateStatus?.mode.effective;
    const hidden = (key: string): boolean => {
      if (mode === 'git') return ScionPageAdminMaintenance.BINARY_ONLY_OPS.has(key);
      if (mode === 'release') return ScionPageAdminMaintenance.SOURCE_ONLY_OPS.has(key);
      return (
        ScionPageAdminMaintenance.SOURCE_ONLY_OPS.has(key) ||
        ScionPageAdminMaintenance.BINARY_ONLY_OPS.has(key)
      );
    };
    return this.operations.filter((op) => !hidden(op.key));
  }

  private renderOperations() {
    const ops = this.visibleOperations;
    return html`
      <div class="section">
        <h2 class="section-title">Routine Operations</h2>
        <p class="section-description">
          Repeatable infrastructure tasks that can be run on demand.
        </p>
        ${ops.length === 0
          ? html`<div class="empty-inline">No operations registered.</div>`
          : html` <div class="card-list">${ops.map((op) => this.renderOperationCard(op))}</div> `}
      </div>
    `;
  }

  private renderOperationCard(op: MaintenanceOperationWithRun) {
    const isRunning = isRunInProgress(op.lastRun?.status);
    const historyExpanded = this.expandedHistory.has(op.key);
    const runs = this.runHistory.get(op.key) ?? [];
    const showCheckButton = op.key === 'rebuild-server' || op.key === 'update-binary';
    const isDestructiveOp = op.key === 'rebuild-server' || op.key === 'update-binary';

    return html`
      <div class="card">
        <div class="card-header">
          ${isRunning
            ? html`<sl-spinner style="font-size: 1.25rem;"></sl-spinner>`
            : html`<sl-icon name="play-circle" class="pending"></sl-icon>`}
          <span class="card-title">${op.title}</span>
          ${showCheckButton
            ? html`
                <sl-button
                  size="small"
                  variant="default"
                  ?loading=${this.updateCheckLoading}
                  ?disabled=${isRunning}
                  @click=${() => this.checkForUpdates()}
                >
                  <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                  Check for Updates
                </sl-button>
              `
            : nothing}
          ${isRunning
            ? html`<sl-button size="small" disabled loading
                >${op.lastRun?.status === 'restarting' ? 'Restarting...' : 'Running...'}</sl-button
              >`
            : html`
                <sl-button
                  variant="${isDestructiveOp ? 'warning' : 'primary'}"
                  size="small"
                  @click=${() =>
                    op.key === 'update-binary'
                      ? void this.openUpdateFromCard()
                      : this.openRunDialog(op.key, 'operation')}
                >
                  <sl-icon slot="prefix" name="play-circle"></sl-icon>
                  Run
                </sl-button>
              `}
        </div>
        <div class="card-description">${op.description}</div>
        ${showCheckButton ? this.renderUpdateCheckBanner() : nothing}
        ${op.lastRun
          ? html`
              <div class="card-meta">
                <span>
                  Last run: ${this.formatRelativeTime(op.lastRun.startedAt)} (<span
                    class="status-badge ${op.lastRun.status}"
                    >${op.lastRun.status}</span
                  >)
                </span>
                ${op.lastRun.startedBy ? html`<span>by ${op.lastRun.startedBy}</span>` : nothing}
              </div>
              ${op.lastRun.status === 'failed' ? this.renderLastRunError(op.lastRun) : nothing}
            `
          : html`
              <div class="card-meta">
                <span>Never run</span>
              </div>
            `}
        <button
          class="history-toggle ${historyExpanded ? 'expanded' : ''}"
          @click=${() => this.toggleHistory(op.key)}
        >
          <sl-icon name="chevron-right"></sl-icon>
          Run History
        </button>
        ${historyExpanded ? this.renderRunHistoryTable(op.key, runs) : nothing}
      </div>
    `;
  }

  private renderLastRunError(run: MaintenanceRun) {
    const result = this.parseMigrationResult(run.result);
    const errorMsg = result?.error;
    if (!errorMsg) return nothing;
    return html`<div class="result-log result-error">${errorMsg}</div>`;
  }

  private renderRunHistoryTable(key: string, runs: MaintenanceRun[]) {
    if (runs.length === 0) {
      return html`<div class="empty-inline">No runs recorded yet.</div>`;
    }

    return html`
      <table class="run-history-table">
        <thead>
          <tr>
            <th>Started</th>
            <th>Duration</th>
            <th>Status</th>
            <th>By</th>
          </tr>
        </thead>
        <tbody>
          ${runs.map(
            (run) => html`
              <tr @click=${() => this.viewRunDetail({ ...run, operationKey: key })}>
                <td>${this.formatDateTime(run.startedAt)}</td>
                <td>${this.formatDuration(run.startedAt, run.completedAt)}</td>
                <td><span class="status-badge ${run.status}">${run.status}</span></td>
                <td>${run.startedBy ?? '--'}</td>
              </tr>
            `
          )}
        </tbody>
      </table>
    `;
  }

  private renderRunDialog() {
    if (!this.runDialogKey) return nothing;

    if (this.runDialogCategory === 'migration') {
      return this.renderMigrationRunDialog();
    }
    return this.renderOperationRunDialog();
  }

  private renderMigrationRunDialog() {
    const migration = this.migrations.find((m) => m.key === this.runDialogKey);
    if (!migration) return nothing;

    return html`
      <sl-dialog label="Run Migration" open @sl-request-close=${() => this.closeRunDialog()}>
        <div class="dialog-body">
          <p><strong>${migration.title}</strong></p>
          <p>${migration.description}</p>
          <sl-checkbox
            ?checked=${this.runDialogDryRun}
            @sl-change=${(e: Event) => {
              this.runDialogDryRun = (e.target as HTMLInputElement).checked;
            }}
          >
            Dry run (preview changes without applying)
          </sl-checkbox>
        </div>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => this.closeRunDialog()}
          ?disabled=${this.runInProgress}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.runInProgress}
          @click=${() => this.executeRunMigration()}
        >
          <sl-icon slot="prefix" name="play-circle"></sl-icon>
          ${this.runDialogDryRun ? 'Dry Run' : 'Run Migration'}
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderOperationRunDialog() {
    const operation = this.operations.find((op) => op.key === this.runDialogKey);
    if (!operation) return nothing;

    const isDestructive =
      this.runDialogKey === 'rebuild-server' || this.runDialogKey === 'update-binary';

    return html`
      <sl-dialog label="Run Operation" open @sl-request-close=${() => this.closeRunDialog()}>
        <div class="dialog-body">
          <p><strong>${operation.title}</strong></p>
          <p>${operation.description}</p>
          ${isDestructive
            ? html`<p style="color: var(--sl-color-warning-700, #a16207); font-weight: 500;">
                This operation will restart the server. You will temporarily lose connectivity.
              </p>`
            : nothing}
        </div>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => this.closeRunDialog()}
          ?disabled=${this.runInProgress}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="${isDestructive ? 'warning' : 'primary'}"
          ?loading=${this.runInProgress}
          @click=${() => this.executeRunOperation()}
        >
          <sl-icon slot="prefix" name="play-circle"></sl-icon>
          Run
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderRunDetailDialog() {
    if (!this.viewingRun) return nothing;
    const run = this.viewingRun;
    const result = this.parseMigrationResult(run.result);

    return html`
      <sl-dialog
        label="Run Detail"
        open
        @sl-request-close=${() => this.closeRunDetail()}
        style="--width: 48rem;"
      >
        <div class="dialog-body">
          <div class="card-meta">
            <span>Status: <span class="status-badge ${run.status}">${run.status}</span></span>
            <span>Started: ${this.formatDateTime(run.startedAt)}</span>
            ${run.completedAt
              ? html`<span>Duration: ${this.formatDuration(run.startedAt, run.completedAt)}</span>`
              : nothing}
            ${run.startedBy ? html`<span>By: ${run.startedBy}</span>` : nothing}
          </div>
          ${result?.error
            ? html`<div class="result-log result-error">${result.error}</div>`
            : nothing}
          ${run.log
            ? html`<div class="result-log">${run.log}</div>`
            : html`<div class="empty-inline">No log output captured.</div>`}
        </div>
        <sl-button slot="footer" variant="default" @click=${() => this.closeRunDetail()}
          >Close</sl-button
        >
      </sl-dialog>
    `;
  }

  private renderRestartDialog() {
    if (!this.restartDialogOpen) return nothing;

    return html`
      <sl-dialog label="Restart Hub" open @sl-request-close=${() => this.closeRestartDialog()}>
        <div class="dialog-body">
          <p>Are you sure you want to restart the hub service?</p>
          <p style="color: var(--sl-color-warning-700, #a16207); font-weight: 500;">
            This will briefly disconnect all users and drop active WebSocket connections. The hub
            will be back online within a few seconds.
          </p>
        </div>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => this.closeRestartDialog()}
          ?disabled=${this.restartLoading}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="warning"
          ?loading=${this.restartLoading}
          @click=${() => this.handleRestartHub()}
        >
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Restart Hub
        </sl-button>
      </sl-dialog>
    `;
  }

  // ── Update check ──

  private async checkForUpdates(): Promise<void> {
    this.updateCheckLoading = true;
    this.updateCheckError = null;
    this.updateCheckResult = null;
    try {
      const res = await apiFetch('/api/v1/admin/maintenance/check-updates', {
        method: 'POST',
      });
      if (!res.ok) {
        this.updateCheckError = await extractApiError(res, 'Failed to check for updates');
        return;
      }
      this.updateCheckResult = (await res.json()) as UpdateCheckResult;
    } catch {
      this.updateCheckError = 'Failed to connect to server';
    } finally {
      this.updateCheckLoading = false;
    }
  }

  private renderUpdateCheckBanner() {
    if (this.updateCheckError) {
      return html`<div class="update-check-error">${this.updateCheckError}</div>`;
    }
    const r = this.updateCheckResult;
    if (!r) return nothing;

    // Binary-tier result: version-based display.
    if (r.tier === 'binary') {
      return this.renderBinaryUpdateCheckBanner(r);
    }

    // Source-tier result: git commit-based display.
    return this.renderSourceUpdateCheckBanner(r);
  }

  private renderSourceUpdateCheckBanner(r: SourceUpdateCheckResult) {
    const branchLabel =
      r.current_branch && r.current_branch !== 'main'
        ? html` on <code>${r.current_branch}</code>`
        : nothing;

    if (!r.update_available) {
      return html`
        <div class="update-check-banner up-to-date">
          <div class="update-check-header">
            <sl-icon name="check-circle"></sl-icon>
            Server is up to date${branchLabel}
          </div>
        </div>
      `;
    }

    return html`
      <div class="update-check-banner">
        <div class="update-check-header">
          <sl-icon name="info-circle"></sl-icon>
          Update available${branchLabel} &mdash; ${r.commits_behind} new
          commit${r.commits_behind === 1 ? '' : 's'}
        </div>
        ${r.new_commits && r.new_commits.length > 0
          ? html`
              <div class="update-check-commits">
                ${r.new_commits.map(
                  (c) => html` <div><span class="commit-hash">${c.hash}</span>${c.subject}</div> `
                )}
              </div>
            `
          : nothing}
      </div>
    `;
  }

  private renderBinaryUpdateCheckBanner(r: BinaryUpdateCheckResult) {
    if (!r.update_available) {
      return html`
        <div class="update-check-banner up-to-date">
          <div class="update-check-header">
            <sl-icon name="check-circle"></sl-icon>
            Server is up to date (${r.current_version})
          </div>
        </div>
      `;
    }

    return html`
      <div class="update-check-banner">
        <div class="update-check-header">
          <sl-icon name="info-circle"></sl-icon>
          Update available
        </div>
        <div class="release-info">
          <div class="release-field">
            <span class="release-label">Current version:</span>
            <span>${r.current_version}</span>
          </div>
          <div class="release-field">
            <span class="release-label">Latest version:</span>
            <span>${r.latest_version}</span>
          </div>
          <div class="release-field">
            <span class="release-label">Channel:</span>
            <span>${r.channel}</span>
          </div>
          ${r.release_url
            ? html`
                <div class="release-field">
                  <span class="release-label">Release notes:</span>
                  ${renderReleaseNotesLink(r.release_url)}
                </div>
              `
            : nothing}
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-maintenance': ScionPageAdminMaintenance;
  }
}
