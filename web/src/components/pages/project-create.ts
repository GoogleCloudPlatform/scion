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
 * Project creation page component
 *
 * Form for creating a new project. "Start from" picks either Blank (configure
 * the workspace here, POST /api/v1/projects) or a project template (POST
 * /api/v1/projects/{id}/clone). With a template, everything except name, slug
 * and — for git templates — the git remote comes from the template and is
 * shown read-only. See ptone/scion#2702.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError, parseApiError } from '../../client/api.js';
import {
  displayGitRemote,
  normalizeGitRemote,
  sanitizeGitRemote,
  stripQueryAndFragment,
  trimRemote,
  validateGitRemote,
} from '../../client/git-remote.js';
import { fetchHubProjectCapabilities } from '../../client/hub-capabilities.js';
import type { PageData } from '../../shared/types.js';
import { can, isEmptyPerAgentWorkspace } from '../../shared/types.js';
import '../shared/status-badge.js';
import '../shared/dir-browser.js';

type WorkspaceType = 'git' | 'shared' | 'empty-per-agent' | 'linked';
type GitWorkspaceMode = 'per-agent' | 'worktree-per-agent' | 'shared';

/** "Start from" value meaning no template. Template options use the project ID. */
const START_BLANK = 'blank';

/** Safety cap on template-list pages followed (see loadTemplates). */
const MAX_TEMPLATE_PAGES = 20;

/** A project template as returned by GET /api/v1/projects?isTemplate=true. */
interface ProjectTemplate {
  id: string;
  name: string;
  slug: string;
  gitRemote?: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
}

interface WorkspaceTypeOption {
  value: WorkspaceType;
  label: string;
  hint: string;
  /** Offered only on a workstation hub with an embedded broker (design OQ-10). */
  workstationOnly?: boolean;
  /** Shows a "New" badge on the option (mock 07a). */
  isNew?: boolean;
}

/**
 * Workspace Type options, in display order. Adding a type is one entry here
 * plus its request body in handleSubmit. To gate a type (e.g. behind an
 * experiment), filter it out in availableWorkspaceTypes.
 */
const WORKSPACE_TYPES: readonly WorkspaceTypeOption[] = [
  {
    value: 'git',
    label: 'Git Repository',
    hint: 'Link to an existing git repository for source-controlled workspaces.',
  },
  {
    value: 'shared',
    label: 'Shared workspace directory',
    hint: 'One directory managed by the Hub and shared by every agent in this project. No git repository required.',
  },
  {
    value: 'empty-per-agent',
    label: 'Empty directory per agent',
    hint: 'Each agent gets its own new, empty directory. Nothing is shared between agents. No git repository required.',
    isNew: true,
  },
  {
    value: 'linked',
    label: 'Local Directory (linked)',
    hint: 'Link a local directory. The directory stays where it is and is operated on in place.',
    workstationOnly: true,
  },
];

const GIT_WORKSPACE_MODE_LABELS: Record<GitWorkspaceMode, string> = {
  'per-agent': 'Clone per agent',
  'worktree-per-agent': 'Worktree per agent',
  shared: 'Shared workspace',
};

const LABEL_WORKSPACE_MODE = 'scion.dev/workspace-mode';
const LABEL_CLONE_URL = 'scion.dev/clone-url';
const LABEL_DEFAULT_BRANCH = 'scion.dev/default-branch';
const ANNOTATION_DEFAULT_HARNESS_CONFIG = 'scion.io/default-harness-config';

function workspaceTypeLabel(type: WorkspaceType): string {
  return WORKSPACE_TYPES.find((t) => t.value === type)?.label ?? type;
}

/**
 * The workspace type a clone of this template gets. Without a git remote the
 * clone keeps an empty-per-agent mode (re-derived by the clone) and is
 * otherwise a shared workspace directory — including linked templates, whose
 * providers are not copied (design OQ-7).
 */
function templateWorkspaceType(t: ProjectTemplate): WorkspaceType {
  if (t.gitRemote) return 'git';
  return isEmptyPerAgentWorkspace(t) ? 'empty-per-agent' : 'shared';
}

/** The git workspace mode a clone of this git template gets (re-derived by the clone). */
function templateGitWorkspaceMode(t: ProjectTemplate): GitWorkspaceMode {
  const mode = t.labels?.[LABEL_WORKSPACE_MODE];
  return mode === 'shared' || mode === 'worktree-per-agent' ? mode : 'per-agent';
}

/** One-line description shown next to a template in the "Start from" list. */
function templateDescription(t: ProjectTemplate): string {
  const type = templateWorkspaceType(t);
  if (type === 'git') {
    return `Git · ${GIT_WORKSPACE_MODE_LABELS[templateGitWorkspaceMode(t)].toLowerCase()}`;
  }
  return workspaceTypeLabel(type);
}

/** The template's clone URL, used as the git remote override placeholder. */
function templateCloneUrl(t: ProjectTemplate): string {
  const label = t.labels?.[LABEL_CLONE_URL];
  if (label) return label;
  return t.gitRemote ? `https://${t.gitRemote}.git` : '';
}

/**
 * The repository a git remote override actually switches to, in its safe
 * (credential-, query- and fragment-free) form — or '' when there is no
 * override or it names the template's own repository. The hub treats a
 * same-repository override as no override and keeps the template's git source
 * labels (branch included), so the UI must not claim otherwise.
 */
function effectiveGitRemoteOverride(t: ProjectTemplate, override: string): string {
  const safe = sanitizeGitRemote(override);
  if (!safe) return '';
  return normalizeGitRemote(safe) === normalizeGitRemote(t.gitRemote ?? '') ? '' : safe;
}

interface ValidatePathResponse {
  resolved: string;
  exists: boolean;
  isDir: boolean;
  isGit: boolean;
  isManaged: boolean;
  alreadyLinked: boolean;
  error?: string;
}

/** Display label for a hub role. Display only; never used for gating. */
function hubRoleLabel(role: string | undefined): string | null {
  switch (role) {
    case 'admin':
      return 'Admin';
    case 'member':
      return 'Member';
    case 'viewer':
      return 'Viewer';
    default:
      return null;
  }
}

@customElement('scion-page-project-create')
export class ScionPageProjectCreate extends LitElement {
  /** Page data from the router; used only for the role name in the notice. */
  @property({ type: Object })
  pageData: PageData | null = null;

  /**
   * Hub-scope project.create check: pending, granted, not granted, or not
   * determinable (capabilities could not be loaded). Only 'allowed' shows
   * the form.
   */
  @state()
  private createAccess: 'checking' | 'allowed' | 'denied' | 'unknown' = 'checking';

  @state()
  private submitting = false;

  @state()
  private error: string | null = null;

  @state()
  private existingProjectId: string | null = null;

  /** Existing projects sharing the same git remote */
  @state()
  private existingProjectsForRemote: Array<{ id: string; name: string; slug: string }> = [];

  /** Form field values */
  @state()
  private name = '';

  @state()
  private slug = '';

  @state()
  private slugManuallyEdited = false;

  @state()
  private gitRemote = '';

  @state()
  private branch = 'main';

  /** "Start from": START_BLANK or the ID of the selected project template. */
  @state()
  private startFrom = START_BLANK;

  @state()
  private mode: WorkspaceType = 'shared';

  @state()
  private gitWorkspaceMode: GitWorkspaceMode = 'per-agent';

  @state()
  private githubToken = '';

  @state()
  private githubAppUrl: string | null = null;

  // Linked-mode state
  @state()
  private localPath = '';

  @state()
  private pathValidation: ValidatePathResponse | null = null;

  @state()
  private validatingPath = false;

  @state()
  private browseDialogOpen = false;

  @state()
  private embeddedBrokerID = '';

  /** `workstation` from GET /api/v1/system/status. */
  @state()
  private workstation = false;

  // Template ("Start from") state
  @state()
  private templates: ProjectTemplate[] = [];

  @state()
  private templatesLoading = false;

  @state()
  private templatesLoadFailed = false;

  /** Git remote override for a git template; empty means use the template's. */
  @state()
  private templateGitRemote = '';

  /** Inline error on the git remote override (client check or a hub 400 on gitRemote). */
  @state()
  private templateGitRemoteError: string | null = null;

  /** Inline error on the Slug field (clone 409 for a colliding explicit slug). */
  @state()
  private slugError: string | null = null;

  private pathCheckTimer: ReturnType<typeof setTimeout> | null = null;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.checkCreateCapability();
    this.checkGitHubApp();
    void this.loadSystemStatus();
    void this.loadTemplates();
  }

  /**
   * Gate the form on hub-scope `project.create` (the same `_capabilities`
   * the Projects list uses), not on the role string. Fail-closed: if the
   * capabilities cannot be loaded the form stays hidden, with a neutral
   * notice rather than one that blames the user's role.
   */
  private async checkCreateCapability(): Promise<void> {
    const caps = await fetchHubProjectCapabilities();
    if (!caps) {
      this.createAccess = 'unknown';
      return;
    }
    this.createAccess = can(caps, 'create') ? 'allowed' : 'denied';
  }

  private async checkGitHubApp(): Promise<void> {
    try {
      // suppressAccessDeniedToast: this is a speculative probe on page load.
      // A non-admin cannot read the GitHub App config, and the section is
      // simply not shown — the user has nothing to act on, so a toast is noise.
      const res = await apiFetch('/api/v1/github-app', { suppressAccessDeniedToast: true });
      if (!res.ok) return;
      const data = (await res.json()) as { configured: boolean; installation_url?: string };
      if (data.configured && data.installation_url) {
        this.githubAppUrl = data.installation_url;
      }
    } catch {
      // Non-fatal
    }
  }

  private async loadSystemStatus(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/system/status');
      if (!res.ok) return;
      const data = (await res.json()) as { embeddedBrokerID?: string; workstation?: boolean };
      if (data.embeddedBrokerID) {
        this.embeddedBrokerID = data.embeddedBrokerID;
      }
      this.workstation = data.workstation === true;
    } catch {
      // Non-fatal
    }
  }

  /**
   * Linked needs both workstation mode and an embedded broker to link to, so
   * the option is never offered when submitting it would fail (design OQ-10).
   */
  private get linkedAvailable(): boolean {
    return this.workstation && !!this.embeddedBrokerID;
  }

  private get availableWorkspaceTypes(): WorkspaceTypeOption[] {
    return WORKSPACE_TYPES.filter((t) => !t.workstationOnly || this.linkedAvailable);
  }

  private get selectedTemplate(): ProjectTemplate | null {
    if (this.startFrom === START_BLANK) return null;
    return this.templates.find((t) => t.id === this.startFrom) ?? null;
  }

  private onLocalPathInput(e: Event): void {
    this.localPath = (e.target as HTMLElement & { value: string }).value;

    if (this.pathCheckTimer) {
      clearTimeout(this.pathCheckTimer);
    }
    const path = this.localPath.trim();
    if (path.length > 1) {
      this.pathCheckTimer = setTimeout(() => void this.validateLocalPath(path), 500);
    } else {
      this.pathValidation = null;
    }
  }

  private async validateLocalPath(path: string): Promise<void> {
    this.validatingPath = true;
    this.pathValidation = null;
    try {
      const res = await apiFetch('/api/v1/system/fs/validate-path', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ path }),
      });
      if (!res.ok) return;
      this.pathValidation = (await res.json()) as ValidatePathResponse;
    } catch {
      // Best-effort
    } finally {
      this.validatingPath = false;
    }
  }

  private onDirBrowserPathSelected(e: CustomEvent<{ path: string }>): void {
    this.localPath = e.detail.path;
    this.browseDialogOpen = false;
    if (!this.name) {
      const segments = e.detail.path.replace(/\/+$/, '').split('/');
      const derived = segments[segments.length - 1] || '';
      if (derived && !/^[a-zA-Z]:$/.test(derived)) {
        this.name = derived;
        if (!this.slugManuallyEdited) {
          this.slug = this.slugify(derived);
        }
      }
    }
    void this.validateLocalPath(e.detail.path);
  }

  override updated(changedProperties: Map<string, unknown>): void {
    super.updated(changedProperties);
    if (changedProperties.has('error') && this.error) {
      this.scrollIntoView({ behavior: 'smooth', block: 'start' });
    }
    // Mark the inner native input invalid too, so assistive tech reports it
    // (the host's aria-invalid does not reach into sl-input's shadow DOM).
    if (changedProperties.has('slugError')) {
      this.setInputValidity('#slug', this.slugError);
    }
    if (changedProperties.has('templateGitRemoteError')) {
      this.setInputValidity('#templateGitRemote', this.templateGitRemoteError);
    }
  }

  private setInputValidity(selector: string, message: string | null): void {
    const input = this.shadowRoot?.querySelector(selector) as
      | (HTMLElement & { setCustomValidity?: (message: string) => void })
      | null;
    input?.setCustomValidity?.(message ?? '');
  }

  static override styles = css`
    :host {
      display: block;
    }

    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }

    .back-link:hover {
      color: var(--scion-primary, #3b82f6);
    }

    .page-header {
      margin-bottom: 1.5rem;
    }

    .page-header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .page-header h1 sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }

    .page-header p {
      color: var(--scion-text-muted, #64748b);
      margin: 0;
      font-size: 0.875rem;
    }

    .capability-loading {
      display: flex;
      justify-content: center;
      padding: 2rem;
      max-width: 640px;
    }

    .create-notice {
      display: flex;
      align-items: flex-start;
      gap: 0.75rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-top: 3px solid var(--scion-text-muted, #64748b);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.25rem 1.5rem;
      max-width: 640px;
      color: var(--scion-text, #1e293b);
      font-size: 0.875rem;
    }

    .create-notice sl-icon {
      flex-shrink: 0;
      font-size: 1.25rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.0625rem;
    }

    .create-notice p {
      margin: 0.25rem 0 0 0;
      color: var(--scion-text-muted, #64748b);
      line-height: 1.5;
    }

    .create-notice a {
      color: var(--scion-primary, #3b82f6);
    }

    .form-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      max-width: 640px;
    }

    .form-field {
      margin-bottom: 1.25rem;
    }

    .form-field label,
    .form-field sl-select::part(form-control-label) {
      display: block;
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.375rem;
    }

    .form-field .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
    }

    .form-field sl-input,
    .form-field sl-select,
    .form-field sl-radio-group {
      width: 100%;
    }

    .workspace-mode-note {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.5rem;
      padding: 0.5rem 0.75rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .form-actions {
      display: flex;
      gap: 0.75rem;
      margin-top: 1.5rem;
      padding-top: 1.5rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .error-banner {
      background: var(--sl-color-danger-50, #fef2f2);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      margin-bottom: 1.25rem;
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      color: var(--sl-color-danger-700, #b91c1c);
      font-size: 0.875rem;
    }

    .error-banner sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .info-banner {
      background: var(--sl-color-primary-50, #eff6ff);
      border: 1px solid var(--sl-color-primary-200, #bfdbfe);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      margin-bottom: 1.25rem;
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      color: var(--sl-color-primary-700, #1d4ed8);
      font-size: 0.875rem;
    }

    .info-banner sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .github-app-hint {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 1.25rem;
      padding: 0.625rem 0.75rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .github-app-hint sl-icon {
      flex-shrink: 0;
      font-size: 1rem;
    }

    .github-app-hint a {
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
      white-space: nowrap;
    }

    .github-app-hint a:hover {
      text-decoration: underline;
    }

    .exists-dialog-body {
      font-size: 0.925rem;
      color: var(--scion-text, #1e293b);
    }

    .path-input-row {
      display: flex;
      gap: 0.5rem;
      align-items: flex-start;
    }

    .path-input-row sl-input {
      flex: 1;
    }

    .path-input-row sl-button {
      margin-top: 0;
    }

    .validation-result {
      font-size: 0.8125rem;
      margin-top: 0.375rem;
      padding: 0.5rem 0.75rem;
      border-radius: var(--scion-radius, 0.5rem);
    }

    .validation-result.valid {
      background: var(--sl-color-success-50, #f0fdf4);
      border: 1px solid var(--sl-color-success-200, #bbf7d0);
      color: var(--sl-color-success-700, #15803d);
    }

    .validation-result.warning {
      background: var(--sl-color-warning-50, #fefce8);
      border: 1px solid var(--sl-color-warning-200, #fef08a);
      color: var(--sl-color-warning-700, #a16207);
    }

    .validation-result.error {
      background: var(--sl-color-danger-50, #fef2f2);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      color: var(--sl-color-danger-700, #b91c1c);
    }

    /* "Start from" (Blank or a project template) */
    .start-from {
      padding-bottom: 1.25rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .form-field .label-row {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.375rem;
    }

    .opt-desc {
      font-size: 0.75rem;
      /* inherit so it stays legible on the highlighted (blue) option */
      color: inherit;
      opacity: 0.75;
      margin-left: 0.75rem;
    }

    sl-option.template-option::part(base) {
      padding-left: 0.5rem;
    }

    .template-summary {
      border: 1px solid var(--sl-color-primary-200, #bfdbfe);
      background: var(--sl-color-primary-50, #eff6ff);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.875rem 1rem;
      margin-bottom: 1.25rem;
      font-size: 0.8125rem;
    }

    .template-summary h3 {
      margin: 0 0 0.5rem;
      font-size: 0.875rem;
      display: flex;
      align-items: center;
      gap: 0.5rem;
      color: var(--sl-color-primary-800, #1e40af);
    }

    .locked-grid {
      display: grid;
      grid-template-columns: max-content 1fr;
      gap: 0.375rem 1rem;
      margin: 0.25rem 0 0.75rem;
    }

    .locked-grid dt {
      color: var(--scion-text-muted, #64748b);
      display: flex;
      align-items: center;
      gap: 0.375rem;
    }

    .locked-grid dt sl-icon.overridden {
      color: var(--sl-color-warning-600, #d97706);
    }

    .locked-grid dd {
      margin: 0;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      overflow-wrap: anywhere;
    }

    .locked-grid dd.mono {
      font-family: var(--scion-font-mono, ui-monospace, monospace);
      font-size: 0.78rem;
    }

    .copies {
      line-height: 1.5;
      color: var(--scion-text-secondary, #475569);
    }

    .copies strong {
      color: var(--scion-text, #1e293b);
    }

    .copies + .copies {
      margin-top: 0.25rem;
    }

    .badge-override {
      font-size: 0.6875rem;
      font-weight: 600;
      color: var(--sl-color-warning-800, #92400e);
      background: var(--sl-color-warning-50, #fffbeb);
      border: 1px solid var(--sl-color-warning-300, #fcd34d);
      border-radius: 999px;
      padding: 0 0.5rem;
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
    }

    .badge-override.active {
      color: #fff;
      background: var(--sl-color-warning-600, #d97706);
      border-color: var(--sl-color-warning-600, #d97706);
    }

    .reset-link {
      margin-left: auto;
      font-size: 0.75rem;
      font-weight: 500;
      color: var(--scion-primary, #3b82f6);
      background: none;
      border: none;
      padding: 0;
      cursor: pointer;
      display: inline-flex;
      gap: 0.25rem;
      align-items: center;
    }

    .override-field sl-input::part(base) {
      border-color: var(--sl-color-warning-300, #fcd34d);
    }

    .override-field.active sl-input::part(base) {
      border-color: var(--sl-color-warning-600, #d97706);
      box-shadow: 0 0 0 1px var(--sl-color-warning-600, #d97706);
    }

    .warn-note {
      font-size: 0.75rem;
      margin-top: 0.5rem;
      padding: 0.5rem 0.75rem;
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--sl-color-warning-50, #fffbeb);
      border: 1px solid var(--sl-color-warning-200, #fde68a);
      color: var(--sl-color-warning-800, #92400e);
      display: flex;
      gap: 0.5rem;
    }

    .warn-note sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .field-error {
      font-size: 0.75rem;
      color: var(--sl-color-danger-700, #b91c1c);
      margin-top: 0.25rem;
    }
  `;

  private slugify(text: string): string {
    return text
      .toLowerCase()
      .trim()
      .replace(/[^a-z0-9]+/g, '-')
      .replace(/^-+|-+$/g, '');
  }

  /**
   * Extract a display name from a git URL.
   * Handles HTTPS and SSH formats.
   */
  private deriveNameFromUrl(url: string): string {
    try {
      const cleaned = url.trim().replace(/\.git$/, '');
      const sshMatch = cleaned.match(/[:/]([^/:]+)$/);
      if (sshMatch) {
        return sshMatch[1];
      }
      const parts = cleaned.split('/');
      return parts[parts.length - 1] || '';
    } catch {
      return '';
    }
  }

  private onNameInput(e: Event): void {
    this.name = (e.target as HTMLElement & { value: string }).value;
    if (!this.slugManuallyEdited) {
      this.slug = this.slugify(this.name);
    }
  }

  private onSlugInput(e: Event): void {
    this.slug = (e.target as HTMLElement & { value: string }).value;
    this.slugManuallyEdited = true;
    this.slugError = null;
  }

  private onModeChange(e: Event): void {
    this.mode = (e.target as HTMLElement & { value: string }).value as WorkspaceType;
  }

  private onStartFromChange(e: Event): void {
    this.startFrom = (e.target as HTMLElement & { value: string }).value || START_BLANK;
    this.templateGitRemote = '';
    this.templateGitRemoteError = null;
    this.slugError = null;
    this.error = null;
  }

  private onTemplateGitRemoteInput(e: Event): void {
    this.templateGitRemote = (e.target as HTMLElement & { value: string }).value;
    this.templateGitRemoteError = null;
  }

  /**
   * Client-side check of the override, mirroring the hub's rules; returns
   * whether it is acceptable and sets the inline error when not. The hub
   * re-validates (a 400 on gitRemote is shown inline too).
   */
  private checkTemplateGitRemote(): boolean {
    const remote = stripQueryAndFragment(trimRemote(this.templateGitRemote));
    this.templateGitRemoteError = remote ? validateGitRemote(remote) : null;
    return this.templateGitRemoteError === null;
  }

  /**
   * Load the project templates for "Start from", following `nextCursor` so the
   * list is never silently truncated. Failure is non-fatal: Blank still works,
   * and the hint says templates could not be loaded.
   */
  private async loadTemplates(): Promise<void> {
    this.templatesLoading = true;
    this.templatesLoadFailed = false;
    const templates: ProjectTemplate[] = [];
    try {
      let cursor = '';
      let page = 0;
      do {
        // MAX_TEMPLATE_PAGES only guards against a server that never stops
        // returning a cursor; at the server's default page size it is far
        // beyond any realistic template count. Say so rather than truncate
        // silently.
        if (page === MAX_TEMPLATE_PAGES) {
          console.warn(
            `[project-create] Stopped loading project templates after ${MAX_TEMPLATE_PAGES} pages (${templates.length} templates); the list is incomplete.`
          );
          this.templatesLoadFailed = true;
          break;
        }
        const url =
          '/api/v1/projects?isTemplate=true' +
          (cursor ? `&cursor=${encodeURIComponent(cursor)}` : '');
        const res = await apiFetch(url);
        if (!res.ok) {
          this.templatesLoadFailed = true;
          break;
        }
        const data = (await res.json()) as { projects?: ProjectTemplate[]; nextCursor?: string };
        templates.push(...(data.projects ?? []));
        cursor = data.nextCursor ?? '';
        page++;
      } while (cursor);
    } catch {
      this.templatesLoadFailed = true;
    } finally {
      // Keep whatever loaded: a failure on a later page still leaves the
      // earlier templates usable (the hint says the list is incomplete).
      this.templates = templates;
      this.templatesLoading = false;
    }
  }

  private gitRemoteCheckTimer: ReturnType<typeof setTimeout> | null = null;

  private onGitRemoteInput(e: Event): void {
    this.gitRemote = (e.target as HTMLElement & { value: string }).value;

    // Auto-derive name from git URL if name is empty
    if (!this.name) {
      const derived = this.deriveNameFromUrl(this.gitRemote);
      if (derived) {
        this.name = derived;
        if (!this.slugManuallyEdited) {
          this.slug = this.slugify(derived);
        }
      }
    }

    // Debounced check for existing projects sharing this git remote
    if (this.gitRemoteCheckTimer) {
      clearTimeout(this.gitRemoteCheckTimer);
    }
    const url = this.gitRemote.trim();
    if (url.length > 5) {
      this.gitRemoteCheckTimer = setTimeout(() => this.checkExistingProjects(url), 500);
    } else {
      this.existingProjectsForRemote = [];
    }
  }

  private async checkExistingProjects(gitUrl: string): Promise<void> {
    try {
      const response = await apiFetch(`/api/v1/projects?gitRemote=${encodeURIComponent(gitUrl)}`);
      if (!response.ok) return;
      const data = (await response.json()) as {
        projects?: Array<{ id: string; name: string; slug: string }>;
      };
      this.existingProjectsForRemote = data.projects ?? [];
    } catch {
      // Best-effort check; ignore errors
    }
  }

  private navigateToProject(projectId: string): void {
    window.history.pushState({}, '', `/projects/${projectId}`);
    window.dispatchEvent(new PopStateEvent('popstate'));
  }

  private async handleSubmit(_e: Event): Promise<void> {
    if (!this.name.trim()) {
      this.error = 'Project name is required.';
      return;
    }

    const template = this.selectedTemplate;
    if (template) {
      await this.submitFromTemplate(template);
      return;
    }

    if (this.mode === 'git' && !this.gitRemote.trim()) {
      this.error = 'Git remote URL is required for git-backed projects.';
      return;
    }

    if (this.mode === 'linked') {
      if (!this.localPath.trim()) {
        this.error = 'Local directory path is required.';
        return;
      }
      if (
        !this.pathValidation ||
        this.pathValidation.error ||
        !this.pathValidation.exists ||
        !this.pathValidation.isDir
      ) {
        this.error = 'Please select a valid directory path.';
        return;
      }
      if (!this.embeddedBrokerID) {
        this.error =
          'No embedded broker available. Ensure the server is running in workstation mode.';
        return;
      }
    }

    this.submitting = true;
    this.error = null;

    try {
      const body: Record<string, unknown> = {
        name: this.name.trim(),
      };

      if (this.slug.trim()) {
        body.slug = this.slug.trim();
      }

      if (this.mode === 'git') {
        const trimmedUrl = this.gitRemote.trim();
        // Build an HTTPS clone URL from whatever the user entered.
        // Strip known schemes/prefixes, then re-add https:// and .git
        // (except for Azure DevOps URLs where .git would break the path).
        let cloneUrl = trimmedUrl;
        const hadGitAt = cloneUrl.startsWith('git@');
        cloneUrl = cloneUrl.replace(/^(https?:\/\/|ssh:\/\/|git:\/\/|git@)/, '');
        if (hadGitAt) {
          cloneUrl = cloneUrl.replace(':', '/'); // git@host:org/repo → host/org/repo
        }
        const lowerUrl = cloneUrl.toLowerCase();
        const isADO =
          lowerUrl.startsWith('dev.azure.com/') ||
          /^[^.]+\.visualstudio\.com(\/|$)/.test(lowerUrl) ||
          lowerUrl.includes('/_git/');
        if (isADO) {
          cloneUrl = cloneUrl.replace(/\.git$/, '');
        } else if (!cloneUrl.endsWith('.git')) {
          cloneUrl += '.git';
        }
        cloneUrl = `https://${cloneUrl}`;
        body.gitRemote = trimmedUrl;
        const labels: Record<string, string> = {
          'scion.dev/default-branch': this.branch.trim() || 'main',
          'scion.dev/clone-url': cloneUrl,
          'scion.dev/source-url': trimmedUrl,
        };
        if (this.gitWorkspaceMode === 'shared') {
          labels['scion.dev/workspace-mode'] = 'shared';
          body.workspaceMode = 'shared';
        } else if (this.gitWorkspaceMode === 'per-agent') {
          labels['scion.dev/workspace-mode'] = 'per-agent';
          body.workspaceMode = 'per-agent';
        } else if (this.gitWorkspaceMode === 'worktree-per-agent') {
          labels['scion.dev/workspace-mode'] = 'worktree-per-agent';
          body.workspaceMode = 'worktree-per-agent';
        }
        body.labels = labels;
        if (this.githubToken.trim()) {
          body.githubToken = this.githubToken.trim();
        }
      } else if (this.mode === 'empty-per-agent') {
        // Option C (#2703): per-agent on a project without git is an empty
        // directory per agent. The hub owns the workspace-mode label.
        body.workspaceMode = 'per-agent';
      }

      const response = await apiFetch('/api/v1/projects', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }

      const result = (await response.json()) as { project?: { id: string }; id?: string };
      const projectId = result.project?.id || result.id;

      if (!projectId) {
        throw new Error('No project ID in response');
      }

      // Backend returns 200 for an existing project, 201 for newly created
      if (response.status === 200 && this.mode !== 'linked') {
        this.existingProjectId = projectId;
        return;
      }

      // Two-step linked create: add provider after project creation
      if (this.mode === 'linked') {
        const providerRes = await apiFetch(`/api/v1/projects/${projectId}/providers`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            brokerId: this.embeddedBrokerID,
            localPath: this.pathValidation!.resolved,
          }),
        });
        if (!providerRes.ok) {
          this.error = await extractApiError(
            providerRes,
            'Project created but failed to link directory. You can retry.'
          );
          this.submitting = false;
          return;
        }
      }

      // Navigate to the newly created project
      this.navigateToProject(projectId);
    } catch (err) {
      console.error('Failed to create project:', err);
      this.error = err instanceof Error ? err.message : 'Failed to create project';
    } finally {
      this.submitting = false;
    }
  }

  /**
   * Create from a template: POST /api/v1/projects/{id}/clone with
   * {name, slug?, gitRemote?}. The slug is sent only when the user edited it,
   * so an auto-derived slug that collides gets the server's serial suffix (as
   * on the Blank path) instead of a 409. The git remote override is sent only
   * for git templates, and only when non-empty.
   */
  private async submitFromTemplate(template: ProjectTemplate): Promise<void> {
    const body: Record<string, unknown> = { name: this.name.trim() };
    const slug = this.slug.trim();
    if (this.slugManuallyEdited && slug) {
      body.slug = slug;
    }
    const gitRemote = trimRemote(this.templateGitRemote);
    if (templateWorkspaceType(template) === 'git' && gitRemote) {
      if (!this.checkTemplateGitRemote()) return;
      body.gitRemote = gitRemote;
    }

    this.submitting = true;
    this.error = null;
    this.slugError = null;
    this.templateGitRemoteError = null;
    try {
      const response = await apiFetch(`/api/v1/projects/${template.id}/clone`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      // A 409 is a slug collision only when we sent an explicit slug; without
      // one the server picks a free slug, so any other conflict goes to the
      // banner like every other error.
      if (response.status === 409 && body.slug !== undefined) {
        this.slugError = await extractApiError(
          response,
          'A project with this slug already exists.'
        );
        return;
      }
      if (!response.ok) {
        const info = await parseApiError(response, 'Failed to create project from template');
        // A 400 about the override belongs on its field, like a slug 409.
        if (response.status === 400 && info.details?.field === 'gitRemote') {
          this.templateGitRemoteError = info.message;
          return;
        }
        throw new Error(info.message);
      }
      const created = (await response.json()) as { id?: string } | null;
      if (!created?.id) {
        throw new Error('No project ID in response');
      }
      this.navigateToProject(created.id);
    } catch (err) {
      console.error('Failed to create project from template:', err);
      this.error = err instanceof Error ? err.message : 'Failed to create project from template';
    } finally {
      this.submitting = false;
    }
  }

  override render() {
    return html`
      <a href="/projects" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Projects
      </a>

      <div class="page-header">
        <h1>
          <sl-icon name="folder-plus"></sl-icon>
          Create Project
        </h1>
        <p>Set up a new project workspace for your agents.</p>
      </div>

      ${this.createAccess === 'checking'
        ? html`<div class="capability-loading"><sl-spinner></sl-spinner></div>`
        : this.createAccess === 'allowed'
          ? this.renderForm()
          : this.createAccess === 'denied'
            ? this.renderCreateDeniedNotice()
            : this.renderCreateUnknownNotice()}
    `;
  }

  /**
   * Shown instead of the form when the hub does not grant project.create.
   * A deep link to /projects/new explains itself rather than redirecting.
   */
  private renderCreateDeniedNotice(): TemplateResult {
    const roleLabel = hubRoleLabel(this.pageData?.user?.role);
    return html`
      <div class="create-notice create-denied-notice" role="status">
        <sl-icon name="info-circle"></sl-icon>
        <div>
          <strong class="create-notice-title"
            >${roleLabel
              ? `Your hub role (${roleLabel}) can't create projects.`
              : "Your hub role can't create projects."}</strong
          >
          <p>
            Ask a hub admin to change your role, or to add you to an existing project.
            <a href="/projects">Browse projects</a>
          </p>
        </div>
      </div>
    `;
  }

  /**
   * Shown when the capability check itself failed. Still fail-closed (no
   * form), but it does not claim the user's role lacks permission.
   */
  private renderCreateUnknownNotice(): TemplateResult {
    return html`
      <div class="create-notice create-unknown-notice" role="status">
        <sl-icon name="exclamation-circle"></sl-icon>
        <div>
          <strong class="create-notice-title"
            >Couldn't check whether you can create projects.</strong
          >
          <p>
            Reload the page to try again.
            <a href="/projects">Browse projects</a>
          </p>
        </div>
      </div>
    `;
  }

  private renderStartFrom(template: ProjectTemplate | null): TemplateResult {
    return html`
      <div class="form-field start-from">
        <sl-select
          id="startFrom"
          label="Start from"
          .value=${this.startFrom}
          @sl-change=${(e: Event) => this.onStartFromChange(e)}
        >
          <sl-icon slot="prefix" name=${template ? 'files' : 'file-earmark'}></sl-icon>
          <sl-option value=${START_BLANK}>
            <sl-icon slot="prefix" name="file-earmark"></sl-icon>
            Blank
            <span slot="suffix" class="opt-desc">Configure everything yourself</span>
          </sl-option>
          ${this.templates.length > 0
            ? html`
                <sl-divider></sl-divider>
                <small>Project templates</small>
                ${this.templates.map(
                  (t) => html`
                    <sl-option class="template-option" value=${t.id}>
                      <sl-icon
                        slot="prefix"
                        name=${templateWorkspaceType(t) === 'git' ? 'diagram-3' : 'folder-fill'}
                      ></sl-icon>
                      ${t.name}
                      <span slot="suffix" class="opt-desc">${templateDescription(t)}</span>
                    </sl-option>
                  `
                )}
              `
            : nothing}
        </sl-select>
        <div class="hint start-from-hint">
          ${template
            ? html`Settings shown under <strong>From template</strong> are copied when the project
                is created.`
            : this.templatesLoading
              ? 'Blank starts with no preset configuration. Loading project templates…'
              : this.templatesLoadFailed && this.templates.length > 0
                ? "Blank starts with no preset configuration. Couldn't load all project templates; reload to see the rest."
                : this.templatesLoadFailed
                  ? "Blank starts with no preset configuration. Couldn't load project templates; reload to try again."
                  : this.templates.length === 0
                    ? 'Blank starts with no preset configuration. No project templates yet — use “Create Template” on a project to make one.'
                    : 'Blank starts with no preset configuration. Pick a project template to copy its settings, env vars, skills, hooks and agent templates.'}
        </div>
      </div>
    `;
  }

  /** Read-only "From template" card: what the clone takes from the template. */
  private renderTemplateSummary(t: ProjectTemplate): TemplateResult {
    const type = templateWorkspaceType(t);
    const override = effectiveGitRemoteOverride(t, this.templateGitRemote);
    const harnessConfig = t.annotations?.[ANNOTATION_DEFAULT_HARNESS_CONFIG];
    const lock = html`<sl-icon name="lock"></sl-icon>`;
    return html`
      <div class="template-summary">
        <h3><sl-icon name="files"></sl-icon>From template: ${t.name}</h3>
        <dl class="locked-grid">
          <dt>${lock}Workspace type</dt>
          <dd class="summary-workspace-type">${workspaceTypeLabel(type)}</dd>
          ${type === 'git'
            ? html`
                <dt>
                  ${override
                    ? html`<sl-icon name="pencil" class="overridden"></sl-icon>`
                    : lock}Repository
                </dt>
                <dd class="mono summary-repository">
                  ${override
                    ? `${displayGitRemote(override)} (override)`
                    : displayGitRemote(t.gitRemote ?? '')}
                </dd>
                <dt>${lock}Workspace mode</dt>
                <dd class="summary-workspace-mode">
                  ${GIT_WORKSPACE_MODE_LABELS[templateGitWorkspaceMode(t)]}
                </dd>
                <dt>${lock}Default branch</dt>
                <dd class="mono summary-branch">
                  ${override ? 'main' : t.labels?.[LABEL_DEFAULT_BRANCH] || 'main'}
                </dd>
              `
            : nothing}
          ${harnessConfig
            ? html`
                <dt>${lock}Default harness config</dt>
                <dd class="mono summary-harness-config">${harnessConfig}</dd>
              `
            : nothing}
        </dl>
        <div class="copies">
          <strong>Also copied:</strong> project settings, labels, environment variables
          (non-secret), injected skills, the pre-start hook, project harness configs and agent
          templates, shared dirs, git identity, default broker, GCP service accounts and the default
          service account.
        </div>
        <div class="copies">
          <strong>Not copied:</strong> secrets, agents, history, chat integrations. Add secrets
          after the project is created.
        </div>
      </div>
    `;
  }

  /** Git remote override for a git template (the only overridable template field). */
  private renderGitRemoteOverride(t: ProjectTemplate): TemplateResult {
    const entered = trimRemote(this.templateGitRemote) !== '';
    // "active" = the clone will really use a different repository.
    const active = effectiveGitRemoteOverride(t, this.templateGitRemote) !== '';
    const error = this.templateGitRemoteError;
    return html`
      <div class="form-field override-field ${active ? 'active' : ''}">
        <div class="label-row">
          <label for="templateGitRemote" style="margin: 0;">Git Remote URL</label>
          <span class="badge-override ${active ? 'active' : ''}"
            ><sl-icon name="pencil"></sl-icon>${active ? 'Overridden' : 'Override'}</span
          >
          ${entered
            ? html`<button
                type="button"
                class="reset-link"
                @click=${() => {
                  this.templateGitRemote = '';
                  this.templateGitRemoteError = null;
                }}
              >
                <sl-icon name="arrow-counterclockwise"></sl-icon>Use template value
              </button>`
            : nothing}
        </div>
        <sl-input
          id="templateGitRemote"
          placeholder=${templateCloneUrl(t)}
          .value=${this.templateGitRemote}
          aria-invalid=${error ? 'true' : 'false'}
          @sl-input=${(e: Event) => this.onTemplateGitRemoteInput(e)}
          @sl-blur=${() => this.checkTemplateGitRemote()}
          @sl-clear=${() => {
            this.templateGitRemote = '';
            this.templateGitRemoteError = null;
          }}
          clearable
        >
          ${error
            ? html`<div slot="help-text" class="field-error git-remote-error" role="alert">
                ${error}
              </div>`
            : nothing}
        </sl-input>
        <div class="hint override-hint">
          ${active
            ? 'Agents will clone this repository instead of the template’s, from its main branch. Workspace mode still comes from the template.'
            : entered && !error
              ? 'Same repository as the template — nothing is overridden; the template’s branch is kept.'
              : 'Optional. Leave blank to use the template’s repository (shown as placeholder).'}
        </div>
        ${active
          ? html`<div class="warn-note">
              <sl-icon name="exclamation-triangle"></sl-icon>
              <span
                >Private repository? The template's GitHub token is a secret and is not copied —
                make sure the GitHub App can reach this repo, or add a token in project settings
                after creation.</span
              >
            </div>`
          : nothing}
      </div>
    `;
  }

  private renderSlugField(): TemplateResult {
    return html`
      <div class="form-field">
        <label for="slug">Slug</label>
        <sl-input
          id="slug"
          placeholder="my-project"
          .value=${this.slug}
          aria-invalid=${this.slugError ? 'true' : 'false'}
          @sl-input=${(e: Event) => this.onSlugInput(e)}
        >
          ${this.slugError
            ? html`<div slot="help-text" class="field-error slug-error" role="alert">
                ${this.slugError}
              </div>`
            : nothing}
        </sl-input>
        <div class="hint">URL-safe identifier. Auto-derived from name if left unchanged.</div>
      </div>
    `;
  }

  /** Blank: Workspace Type and the fields for that type, then Slug. */
  private renderBlankFields(workspaceTypes: WorkspaceTypeOption[]): TemplateResult {
    const selectedType = workspaceTypes.find((t) => t.value === this.mode);
    return html`
      <div class="form-field">
        <sl-select
          id="mode"
          label="Workspace Type"
          .value=${this.mode}
          @sl-change=${(e: Event) => this.onModeChange(e)}
        >
          ${workspaceTypes.map(
            (t) =>
              html`<sl-option value=${t.value}
                >${t.label}${t.isNew
                  ? html`<sl-badge slot="suffix" variant="primary" pill class="new-badge"
                      >New</sl-badge
                    >`
                  : nothing}</sl-option
              >`
          )}
        </sl-select>
        <div class="hint">${selectedType?.hint ?? ''}</div>
        ${this.mode === 'empty-per-agent'
          ? html`<div class="workspace-mode-note empty-per-agent-note">
              The directory is created on the broker when the agent starts. It is kept across
              suspend/resume where the broker's storage allows, and is
              <strong>deleted when the agent is deleted</strong>. Use shared dirs or a git remote
              for anything you need to keep.
            </div>`
          : nothing}
      </div>

      ${this.mode === 'git'
        ? html`
            <div class="form-field">
              <label for="gitRemote">Git Remote URL</label>
              <sl-input
                id="gitRemote"
                placeholder="https://github.com/org/repo.git"
                .value=${this.gitRemote}
                @sl-input=${(e: Event) => this.onGitRemoteInput(e)}
                required
              ></sl-input>
              <div class="hint">HTTPS or SSH URL of the git repository.</div>
            </div>

            ${this.githubAppUrl
              ? html`
                  <div class="github-app-hint">
                    <sl-icon name="github"></sl-icon>
                    <span
                      >Ensure this repository is accessible via the
                      <a href=${this.githubAppUrl} target="_blank" rel="noopener"
                        >GitHub App
                        <sl-icon
                          name="box-arrow-up-right"
                          style="font-size: 0.7em; vertical-align: middle;"
                        ></sl-icon
                      ></a>
                    </span>
                  </div>
                `
              : nothing}

            <div class="form-field">
              <label for="githubToken">GitHub Token</label>
              <sl-input
                id="githubToken"
                type="password"
                placeholder="Paste token here"
                .value=${this.githubToken}
                @sl-input=${(e: Event) => {
                  this.githubToken = (e.target as HTMLElement & { value: string }).value;
                }}
                password-toggle
              ></sl-input>
              <div class="hint">
                Optional. A personal access token for cloning private repositories. Saved as a
                project secret.
              </div>
            </div>

            ${this.existingProjectsForRemote.length > 0
              ? html`
                  <div class="info-banner">
                    <sl-icon name="info-circle"></sl-icon>
                    <div>
                      <strong>${this.existingProjectsForRemote.length} existing project(s)</strong>
                      share this git remote. A new project will be created with a unique slug.
                      <ul style="margin: 0.25rem 0 0; padding-left: 1.25rem;">
                        ${this.existingProjectsForRemote.map(
                          (p) =>
                            html`<li>${p.name} <span style="opacity: 0.7">(${p.slug})</span></li>`
                        )}
                      </ul>
                    </div>
                  </div>
                `
              : nothing}

            <div class="form-field">
              <label>Workspace Mode</label>
              <sl-radio-group
                .value=${this.gitWorkspaceMode}
                @sl-change=${(e: Event) => {
                  this.gitWorkspaceMode = (e.target as HTMLElement & { value: string })
                    .value as GitWorkspaceMode;
                }}
              >
                <sl-radio-button value="per-agent">Clone per agent</sl-radio-button>
                <sl-radio-button value="worktree-per-agent">Worktree per agent</sl-radio-button>
                <sl-radio-button value="shared">Shared workspace</sl-radio-button>
              </sl-radio-group>
              <div class="hint">
                ${this.gitWorkspaceMode === 'per-agent'
                  ? 'Each agent gets its own full clone. Most isolated.'
                  : this.gitWorkspaceMode === 'worktree-per-agent'
                    ? 'Agents share one base clone via git worktrees — fast startup, low disk.'
                    : 'A single git clone is shared by all agents in this project.'}
              </div>
              ${this.gitWorkspaceMode === 'worktree-per-agent'
                ? html`<div class="workspace-mode-note">
                    A single base clone is created, and each agent gets a lightweight git worktree.
                    Requires git ≥ 2.47 on the node. On Kubernetes, requires the NFS backend.
                  </div>`
                : this.gitWorkspaceMode === 'shared'
                  ? html`<div class="workspace-mode-note">
                      A single git clone will be created on the hub and shared by all agents. Agents
                      can commit, push, and pull but must coordinate branch changes.
                    </div>`
                  : nothing}
            </div>
          `
        : nothing}
      ${this.mode === 'linked'
        ? html`
            <div class="form-field">
              <label for="localPath">Local Directory Path</label>
              <div class="path-input-row">
                <sl-input
                  id="localPath"
                  placeholder="/home/user/projects/my-project"
                  .value=${this.localPath}
                  @sl-input=${(e: Event) => this.onLocalPathInput(e)}
                ></sl-input>
                <sl-button
                  variant="default"
                  @click=${() => {
                    this.browseDialogOpen = true;
                  }}
                >
                  Browse…
                </sl-button>
              </div>
              <div class="hint">
                Absolute path to a local directory. The directory is operated on in place.
              </div>
            </div>

            ${this.validatingPath
              ? html`<div
                  class="validation-result valid"
                  style="display: flex; align-items: center; gap: 0.5rem;"
                >
                  <sl-spinner style="font-size: 0.875rem;"></sl-spinner> Validating path…
                </div>`
              : this.pathValidation
                ? html`
                    ${this.pathValidation.error
                      ? html`<div class="validation-result error">
                          <sl-icon name="exclamation-triangle"></sl-icon>
                          ${this.pathValidation.error}
                        </div>`
                      : !this.pathValidation.exists
                        ? html`<div class="validation-result error">
                            <sl-icon name="exclamation-triangle"></sl-icon>
                            Path does not exist.
                          </div>`
                        : !this.pathValidation.isDir
                          ? html`<div class="validation-result error">
                              <sl-icon name="exclamation-triangle"></sl-icon>
                              Path is not a directory.
                            </div>`
                          : html`
                              <div class="validation-result valid">
                                <sl-icon name="check-circle"></sl-icon>
                                Path resolved to: ${this.pathValidation.resolved}
                              </div>
                              ${this.pathValidation.isGit
                                ? html`<div
                                    class="validation-result warning"
                                    style="margin-top: 0.25rem;"
                                  >
                                    <sl-icon name="info-circle"></sl-icon>
                                    This is a git repository. Agents will operate on the working
                                    tree.
                                  </div>`
                                : nothing}
                              ${this.pathValidation.alreadyLinked
                                ? html`<div
                                    class="validation-result warning"
                                    style="margin-top: 0.25rem;"
                                  >
                                    <sl-icon name="info-circle"></sl-icon>
                                    This directory is already linked to another project.
                                  </div>`
                                : nothing}
                            `}
                  `
                : nothing}
          `
        : nothing}
      ${this.renderSlugField()}
      ${this.mode === 'git'
        ? html`
            <div class="form-field">
              <label for="branch">Default Branch</label>
              <sl-input
                id="branch"
                placeholder="main"
                .value=${this.branch}
                @sl-input=${(e: Event) => {
                  this.branch = (e.target as HTMLElement & { value: string }).value;
                }}
              ></sl-input>
              <div class="hint">The default branch to use for this repository.</div>
            </div>
          `
        : nothing}
    `;
  }

  private renderForm(): TemplateResult {
    const template = this.selectedTemplate;
    const workspaceTypes = this.availableWorkspaceTypes;
    return html`
      <div class="form-card">
        ${this.error
          ? html`
              <div class="error-banner">
                <sl-icon name="exclamation-triangle"></sl-icon>
                <span>${this.error}</span>
              </div>
            `
          : nothing}

        <div>
          ${this.renderStartFrom(template)}

          <div class="form-field">
            <label for="name">Name</label>
            <sl-input
              id="name"
              placeholder="my-project"
              .value=${this.name}
              @sl-input=${(e: Event) => this.onNameInput(e)}
              required
            ></sl-input>
          </div>

          ${template
            ? html`
                ${this.renderTemplateSummary(template)}
                ${templateWorkspaceType(template) === 'git'
                  ? this.renderGitRemoteOverride(template)
                  : nothing}
                ${this.renderSlugField()}
              `
            : this.renderBlankFields(workspaceTypes)}
          <div class="form-actions">
            <sl-button
              variant="primary"
              ?loading=${this.submitting}
              ?disabled=${this.submitting}
              @click=${(e: Event) => this.handleSubmit(e)}
            >
              <sl-icon slot="prefix" name="folder-plus"></sl-icon>
              ${template ? 'Create from template' : 'Create Project'}
            </sl-button>
            <a href="/projects" style="text-decoration: none;">
              <sl-button variant="default" ?disabled=${this.submitting}> Cancel </sl-button>
            </a>
          </div>
        </div>
      </div>

      <sl-dialog
        label="Browse Directory"
        ?open=${this.browseDialogOpen}
        @sl-after-hide=${() => {
          this.browseDialogOpen = false;
        }}
        style="--width: 36rem;"
      >
        <scion-dir-browser
          @path-selected=${(e: CustomEvent<{ path: string }>) => this.onDirBrowserPathSelected(e)}
        ></scion-dir-browser>
      </sl-dialog>

      <sl-dialog
        label="Project Already Exists"
        ?open=${this.existingProjectId !== null}
        @sl-after-hide=${() => {
          this.existingProjectId = null;
        }}
      >
        <div class="exists-dialog-body">A project with this ID already exists.</div>
        <sl-button
          slot="footer"
          variant="primary"
          @click=${() => {
            if (this.existingProjectId) {
              this.navigateToProject(this.existingProjectId);
            }
          }}
        >
          Take me there
        </sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-project-create': ScionPageProjectCreate;
  }
}
