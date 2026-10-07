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
 * Shared Token List Component
 *
 * Full CRUD component for user access tokens. Renders a table with
 * create, revoke, and delete actions. Shows a one-time token display
 * modal after creation with a copy button.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import type { Project } from '../../shared/types.js';
import { apiFetch, extractApiError, parseApiError } from '../../client/api.js';
import { resourceStyles } from './resource-styles.js';
import { showToast } from '../../utils/toast.js';
import { showConfirm } from './confirm-dialog.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';

interface AccessToken {
  id: string;
  name: string;
  prefix: string;
  projectId: string;
  scopes: string[];
  revoked: boolean;
  expiresAt?: string | null;
  lastUsed?: string | null;
  created: string;
}

interface ScopeOption {
  value: string;
  label: string;
  description: string;
  /** Resource type extracted from the scope id (e.g. "agent" from "agent:create"). */
  resource: string;
  /** Whether this scope is an alias that expands to multiple scopes. */
  isAlias: boolean;
  /** For aliases, the list of scopes this alias expands to. */
  expandsTo?: string[];
  /**
   * "flat_role" or "relationship" (ptone/scion#2122). Relationship-eligible
   * scopes (agent:attach, agent:port_access) are checked against the
   * specific target on every later request -- own agents and their
   * descendants, plus (for agent:port_access) agents in projects where the
   * holder's role grants it -- never against a target enumerated at
   * selection time.
   */
  eligibilityKind?: string | undefined;
  /**
   * Present only once eligibility was requested for a selected project.
   * Answers only "may you select this restriction"; never a target list
   * and never widened by anything the browser already holds.
   */
  eligible?: boolean | undefined;
  /** Stable machine reason code, present only when eligible is false. */
  eligibilityReason?: string | undefined;
  /** For an alias, which expanded member scopes are not eligible. */
  ineligibleMembers?: string[] | undefined;
}

/** Human-readable text for a MintDenialReason code from the eligibility API. */
const ELIGIBILITY_REASON_LABELS: Record<string, string> = {
  flat_role_insufficient: 'your project role does not include this permission',
  no_relationship_candidacy: 'not eligible under your current project access',
  boundary_not_allowed: 'not selectable for a project-scoped token',
  unknown_selector: 'unknown scope',
  // Appears per-scope only when at least one other scope in the response
  // was eligible (ptone/scion#2122); a project with zero eligible scopes
  // fails the whole request instead, so this label is never the only
  // signal that the project itself is inaccessible.
  project_access_required:
    'you do not currently have authority for this permission in this project',
};

export function formatEligibilityReason(reason?: string): string {
  if (!reason) return 'not currently selectable';
  return ELIGIBILITY_REASON_LABELS[reason] || reason;
}

/**
 * Badge text for a relationship-eligible scope. agent:port_access also
 * reaches agents in projects where the holder's role grants port access
 * (the built-in project owner and admin roles do).
 */
export function relationshipBadgeText(scope: string): string {
  if (scope === 'agent:port_access') {
    return 'Own agents & descendants, or any agent in the project if your role grants port access — checked per agent';
  }
  return 'Own agents & descendants — checked per agent';
}

/**
 * Human-friendly labels for resource type groups in the scope selector.
 */
const RESOURCE_TYPE_LABELS: Record<string, string> = {
  agent: 'Agent',
  artifact: 'Artifact',
  broker: 'Broker',
  gcp_service_account: 'GCP Service Account',
  group: 'Group',
  harness_config: 'Harness Config',
  project: 'Project',
  skill: 'Skill',
  template: 'Template',
  user: 'User',
};

/**
 * Fallback scope list used when the dynamic fetch from /api/v1/auth/scopes
 * fails. This is a static, best-effort snapshot for that offline case only:
 * it never carries eligibility (every entry is selectable), so it must not
 * be used to answer "may I select this restriction" -- only the live
 * /api/v1/auth/scopes response does that. The list mirrors every registry
 * selector, including boundary-restricted ones such as broker:create
 * (hub-boundary tokens only): the live response marks those
 * boundary_not_allowed for a project-scoped token, and the server rejects
 * them on submit.
 */
const FALLBACK_SCOPES: ScopeOption[] = [
  {
    value: 'agent:attach',
    label: 'agent:attach',
    description: 'Attach to agent sessions (terminal, exec, env, reset-auth)',
    resource: 'agent',
    isAlias: false,
  },
  {
    value: 'agent:create',
    label: 'agent:create',
    description: 'Create agents',
    resource: 'agent',
    isAlias: false,
  },
  {
    value: 'agent:delete',
    label: 'agent:delete',
    description: 'Delete agents',
    resource: 'agent',
    isAlias: false,
  },
  {
    value: 'agent:lifecycle',
    label: 'agent:lifecycle',
    description: 'Start, stop, suspend, restart, restore, and reincarnate agents',
    resource: 'agent',
    isAlias: false,
  },
  {
    value: 'agent:list',
    label: 'agent:list',
    description: 'List agents in the project',
    resource: 'agent',
    isAlias: false,
  },
  {
    value: 'agent:manage',
    label: 'agent:manage',
    description: 'All agent management operations',
    resource: 'agent',
    isAlias: true,
    expandsTo: [
      'agent:create',
      'agent:delete',
      'agent:lifecycle',
      'agent:list',
      'agent:message',
      'agent:read',
    ],
  },
  {
    value: 'agent:message',
    label: 'agent:message',
    description: 'Send messages to agents',
    resource: 'agent',
    isAlias: false,
  },
  {
    value: 'agent:port_access',
    label: 'agent:port_access',
    description: 'Access agent forwarded ports',
    resource: 'agent',
    isAlias: false,
  },
  {
    value: 'agent:read',
    label: 'agent:read',
    description: 'Read agent status/metadata',
    resource: 'agent',
    isAlias: false,
  },
  {
    value: 'artifact:create',
    label: 'artifact:create',
    description: 'Publish artifacts',
    resource: 'artifact',
    isAlias: false,
  },
  {
    value: 'artifact:delete',
    label: 'artifact:delete',
    description: 'Delete artifacts',
    resource: 'artifact',
    isAlias: false,
  },
  {
    value: 'artifact:manage',
    label: 'artifact:manage',
    description: 'Manage artifact grants and share links',
    resource: 'artifact',
    isAlias: false,
  },
  {
    value: 'artifact:read',
    label: 'artifact:read',
    description: 'Read artifacts',
    resource: 'artifact',
    isAlias: false,
  },
  {
    value: 'artifact:update',
    label: 'artifact:update',
    description: 'Edit artifact metadata (title, key, expiry)',
    resource: 'artifact',
    isAlias: false,
  },
  {
    value: 'broker:create',
    label: 'broker:create',
    description: 'Create brokers',
    resource: 'broker',
    isAlias: false,
  },
  {
    value: 'broker:list',
    label: 'broker:list',
    description: 'List brokers',
    resource: 'broker',
    isAlias: false,
  },
  {
    value: 'broker:read',
    label: 'broker:read',
    description: 'Read brokers',
    resource: 'broker',
    isAlias: false,
  },
  {
    value: 'gcp_service_account:assign',
    label: 'gcp_service_account:assign',
    description: 'Assign GCP service accounts to agents',
    resource: 'gcp_service_account',
    isAlias: false,
  },
  {
    value: 'gcp_service_account:list',
    label: 'gcp_service_account:list',
    description: 'List GCP service accounts',
    resource: 'gcp_service_account',
    isAlias: false,
  },
  {
    value: 'gcp_service_account:read',
    label: 'gcp_service_account:read',
    description: 'Read GCP service accounts',
    resource: 'gcp_service_account',
    isAlias: false,
  },
  {
    value: 'gcp_service_account:verify',
    label: 'gcp_service_account:verify',
    description: 'Verify GCP service accounts',
    resource: 'gcp_service_account',
    isAlias: false,
  },
  {
    value: 'group:addMember',
    label: 'group:addMember',
    description: 'Add group members',
    resource: 'group',
    isAlias: false,
  },
  {
    value: 'group:create',
    label: 'group:create',
    description: 'Create groups',
    resource: 'group',
    isAlias: false,
  },
  {
    value: 'group:delete',
    label: 'group:delete',
    description: 'Delete groups',
    resource: 'group',
    isAlias: false,
  },
  {
    value: 'group:list',
    label: 'group:list',
    description: 'List groups',
    resource: 'group',
    isAlias: false,
  },
  {
    value: 'group:manage',
    label: 'group:manage',
    description: 'All group scopes (convenience alias)',
    resource: 'group',
    isAlias: true,
    expandsTo: [
      'group:addMember',
      'group:create',
      'group:delete',
      'group:list',
      'group:read',
      'group:removeMember',
      'group:update',
    ],
  },
  {
    value: 'group:read',
    label: 'group:read',
    description: 'Read groups',
    resource: 'group',
    isAlias: false,
  },
  {
    value: 'group:removeMember',
    label: 'group:removeMember',
    description: 'Remove group members',
    resource: 'group',
    isAlias: false,
  },
  {
    value: 'group:update',
    label: 'group:update',
    description: 'Update groups',
    resource: 'group',
    isAlias: false,
  },
  {
    value: 'harness_config:create',
    label: 'harness_config:create',
    description: 'Create harness configs',
    resource: 'harness_config',
    isAlias: false,
  },
  {
    value: 'harness_config:delete',
    label: 'harness_config:delete',
    description: 'Delete harness configs',
    resource: 'harness_config',
    isAlias: false,
  },
  {
    value: 'harness_config:list',
    label: 'harness_config:list',
    description: 'List harness configs',
    resource: 'harness_config',
    isAlias: false,
  },
  {
    value: 'harness_config:manage',
    label: 'harness_config:manage',
    description: 'All harness_config scopes (convenience alias)',
    resource: 'harness_config',
    isAlias: true,
    expandsTo: [
      'harness_config:create',
      'harness_config:delete',
      'harness_config:list',
      'harness_config:read',
      'harness_config:update',
    ],
  },
  {
    value: 'harness_config:read',
    label: 'harness_config:read',
    description: 'Read harness configs',
    resource: 'harness_config',
    isAlias: false,
  },
  {
    value: 'harness_config:update',
    label: 'harness_config:update',
    description: 'Update harness configs',
    resource: 'harness_config',
    isAlias: false,
  },
  {
    value: 'inbox:read',
    label: 'inbox:read',
    description: 'Read your own inbox, notifications and direct messages',
    resource: 'inbox',
    isAlias: false,
  },
  {
    value: 'inbox:write',
    label: 'inbox:write',
    description: 'Send, change and remove your own inbox items and direct messages',
    resource: 'inbox',
    isAlias: false,
  },
  {
    value: 'project:clone',
    label: 'project:clone',
    description: 'Clone projects',
    resource: 'project',
    isAlias: false,
  },
  {
    value: 'project:manage',
    label: 'project:manage',
    description: 'Manage project administration (RS1 membership operations)',
    resource: 'project',
    isAlias: false,
  },
  {
    value: 'project:read',
    label: 'project:read',
    description: 'Read project metadata',
    resource: 'project',
    isAlias: false,
  },
  {
    value: 'project:update',
    label: 'project:update',
    description: 'Update projects',
    resource: 'project',
    isAlias: false,
  },
  {
    value: 'project:set_messaging_policy',
    label: 'project:set_messaging_policy',
    description: 'Set project cross-project messaging policy (owner/admin only)',
    resource: 'project',
    isAlias: false,
  },
  {
    value: 'skill:create',
    label: 'skill:create',
    description: 'Create skills',
    resource: 'skill',
    isAlias: false,
  },
  {
    value: 'skill:delete',
    label: 'skill:delete',
    description: 'Delete skills',
    resource: 'skill',
    isAlias: false,
  },
  {
    value: 'skill:list',
    label: 'skill:list',
    description: 'List skills',
    resource: 'skill',
    isAlias: false,
  },
  {
    value: 'skill:manage',
    label: 'skill:manage',
    description: 'All skill scopes (convenience alias)',
    resource: 'skill',
    isAlias: true,
    expandsTo: [
      'skill:create',
      'skill:delete',
      'skill:list',
      'skill:read',
      'skill:register',
      'skill:update',
    ],
  },
  {
    value: 'skill:read',
    label: 'skill:read',
    description: 'Read skills',
    resource: 'skill',
    isAlias: false,
  },
  {
    value: 'skill:register',
    label: 'skill:register',
    description: 'Register skills in registries',
    resource: 'skill',
    isAlias: false,
  },
  {
    value: 'skill:update',
    label: 'skill:update',
    description: 'Update skills',
    resource: 'skill',
    isAlias: false,
  },
  {
    value: 'template:create',
    label: 'template:create',
    description: 'Create templates',
    resource: 'template',
    isAlias: false,
  },
  {
    value: 'template:delete',
    label: 'template:delete',
    description: 'Delete templates',
    resource: 'template',
    isAlias: false,
  },
  {
    value: 'template:list',
    label: 'template:list',
    description: 'List templates',
    resource: 'template',
    isAlias: false,
  },
  {
    value: 'template:manage',
    label: 'template:manage',
    description: 'All template scopes (convenience alias)',
    resource: 'template',
    isAlias: true,
    expandsTo: [
      'template:create',
      'template:delete',
      'template:list',
      'template:read',
      'template:update',
    ],
  },
  {
    value: 'template:read',
    label: 'template:read',
    description: 'Read templates',
    resource: 'template',
    isAlias: false,
  },
  {
    value: 'template:update',
    label: 'template:update',
    description: 'Update templates',
    resource: 'template',
    isAlias: false,
  },
  {
    value: 'user:invite',
    label: 'user:invite',
    description: 'Invite users',
    resource: 'user',
    isAlias: false,
  },
  {
    value: 'user:list',
    label: 'user:list',
    description: 'List users',
    resource: 'user',
    isAlias: false,
  },
  {
    value: 'user:read',
    label: 'user:read',
    description: 'Read users',
    resource: 'user',
    isAlias: false,
  },
  {
    value: 'user_skill_injection:update',
    label: 'user_skill_injection:update',
    description: 'Change the skills injected into your own agents',
    resource: 'user_skill_injection',
    isAlias: false,
  },
];

/** Groups scope options by resource type, preserving order. Returns [resourceType, scopes][] */
function groupScopesByResource(scopes: ScopeOption[]): [string, ScopeOption[]][] {
  const groups = new Map<string, ScopeOption[]>();
  for (const scope of scopes) {
    const key = scope.resource;
    if (!groups.has(key)) {
      groups.set(key, []);
    }
    groups.get(key)!.push(scope);
  }
  return Array.from(groups.entries());
}

@customElement('scion-token-list')
export class ScionTokenList extends LitElement {
  /** Re-renders absolute times when the display timezone changes. */
  readonly _zone = new DisplayZoneController(this);

  @state() private loading = true;
  @state() private tokens: AccessToken[] = [];
  @state() private projects: Project[] = [];
  @state() private error: string | null = null;
  @state() private availableScopes: ScopeOption[] = [...FALLBACK_SCOPES];
  /** Cached scope responses, keyed by projectId ('' for the plain catalog). */
  private scopesCache: Map<string, ScopeOption[]> = new Map();
  /**
   * Monotonic counter guarding against out-of-order responses: each
   * loadScopes() call captures the value at its start and checks it again
   * after every await, so a slower, older request cannot overwrite the
   * state a newer one already applied.
   */
  private scopesRequestSeq = 0;
  /**
   * Set to the projectId whose eligibility fetch failed (403, or any other
   * non-OK/network failure), so the picker can say so instead of silently
   * showing a different project's eligibility. Cleared on any successful
   * fetch for that project. Compared against createProjectId at render
   * time, so switching away from the failed project hides the message
   * without an explicit reset.
   */
  @state() private scopesErrorProjectId: string | null = null;

  // Create dialog
  @state() private createDialogOpen = false;
  @state() private createName = '';
  @state() private createProjectId = '';
  @state() private createScopes: Set<string> = new Set();
  @state() private createExpiry = '90';
  @state() private createLoading = false;
  @state() private createError: string | null = null;

  /** Search filter for the scope selector. */
  @state() private scopeFilter = '';
  /** Tracks which resource type groups are collapsed in the scope selector. */
  @state() private collapsedGroups: Set<string> = new Set();

  // Token reveal dialog (shown once after creation)
  @state() private revealDialogOpen = false;
  @state() private revealToken = '';
  @state() private revealCopied = false;

  // Action loading
  @state() private actionLoadingId: string | null = null;

  static override styles = [
    resourceStyles,
    css`
      .scope-badge {
        display: inline-flex;
        align-items: center;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
        font-family: var(--scion-font-mono, monospace);
        background: var(--sl-color-primary-100, #dbeafe);
        color: var(--sl-color-primary-700, #1d4ed8);
      }

      .scopes-cell {
        display: flex;
        flex-wrap: wrap;
        gap: 0.25rem;
      }

      .status-revoked {
        display: inline-flex;
        align-items: center;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
        background: var(--sl-color-danger-100, #fee2e2);
        color: var(--sl-color-danger-700, #b91c1c);
      }

      .status-expired {
        display: inline-flex;
        align-items: center;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
        background: var(--sl-color-warning-100, #fef3c7);
        color: var(--sl-color-warning-700, #b45309);
      }

      .status-active {
        display: inline-flex;
        align-items: center;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
        background: var(--sl-color-success-100, #dcfce7);
        color: var(--sl-color-success-700, #15803d);
      }

      .token-reveal {
        display: flex;
        flex-direction: column;
        gap: 1rem;
      }

      .token-value {
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.8125rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        padding: 0.75rem 1rem;
        border-radius: var(--scion-radius, 0.5rem);
        border: 1px solid var(--scion-border, #e2e8f0);
        word-break: break-all;
        user-select: all;
      }

      .token-copy-row {
        display: flex;
        gap: 0.5rem;
        align-items: center;
      }

      .token-copy-row sl-button {
        flex-shrink: 0;
      }

      .scope-selector {
        max-height: 360px;
        overflow-y: auto;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
      }

      .scope-search {
        position: sticky;
        top: 0;
        z-index: 1;
        padding: 0.5rem;
        background: var(--scion-surface, #ffffff);
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
      }

      .scope-group {
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
      }

      .scope-group:last-child {
        border-bottom: none;
      }

      .scope-group-header {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.5rem 0.75rem;
        cursor: pointer;
        user-select: none;
        background: var(--scion-bg-subtle, #f8fafc);
        font-size: 0.75rem;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.03em;
        color: var(--scion-text-muted, #64748b);
        transition: background 0.15s ease;
      }

      .scope-group-header:hover {
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .scope-group-header sl-icon {
        font-size: 0.75rem;
        transition: transform 0.15s ease;
      }

      .scope-group-header sl-icon.collapsed {
        transform: rotate(-90deg);
      }

      .scope-group-header .scope-group-count {
        margin-left: auto;
        font-size: 0.6875rem;
        font-weight: 400;
        color: var(--scion-text-muted, #94a3b8);
      }

      .scope-group-items {
        padding: 0.25rem 0;
      }

      .scope-checkboxes {
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: 0.25rem;
        padding: 0 0.5rem;
      }

      @media (max-width: 640px) {
        .scope-checkboxes {
          grid-template-columns: 1fr;
        }
      }

      .scope-checkbox-item {
        display: flex;
        align-items: flex-start;
        gap: 0.375rem;
        padding: 0.25rem 0.25rem;
        border-radius: 0.25rem;
      }

      .scope-checkbox-item:hover {
        background: var(--scion-bg-subtle, #f8fafc);
      }

      .scope-checkbox-item sl-checkbox {
        --sl-spacing-x-small: 0;
      }

      .scope-checkbox-label {
        font-size: 0.8125rem;
        font-family: var(--scion-font-mono, monospace);
        color: var(--scion-text, #1e293b);
      }

      .scope-checkbox-desc {
        font-size: 0.6875rem;
        color: var(--scion-text-muted, #64748b);
        font-family: inherit;
      }

      .scope-alias-item {
        background: var(--sl-color-primary-50, #eff6ff);
        border: 1px solid var(--sl-color-primary-200, #bfdbfe);
        border-radius: 0.375rem;
        margin: 0.25rem 0.5rem;
        padding: 0.375rem 0.5rem;
      }

      .scope-alias-badge {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        font-size: 0.625rem;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.03em;
        color: var(--sl-color-primary-600, #2563eb);
        margin-top: 0.125rem;
      }

      .scope-relationship-badge {
        display: inline-block;
        margin-left: 0.375rem;
        font-size: 0.625rem;
        font-weight: 500;
        color: var(--scion-text-muted, #64748b);
        font-family: inherit;
      }

      .scope-ineligible-reason {
        font-size: 0.6875rem;
        color: var(--sl-color-danger-600, #dc2626);
        font-family: inherit;
      }

      sl-checkbox[disabled] .scope-checkbox-label {
        color: var(--scion-text-muted, #94a3b8);
      }

      .scope-selected-count {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.25rem;
      }

      .scope-no-results {
        padding: 1rem;
        text-align: center;
        color: var(--scion-text-muted, #64748b);
        font-size: 0.8125rem;
      }

      .field-label {
        font-size: 0.875rem;
        font-weight: 500;
        color: var(--scion-text, #1e293b);
        margin-bottom: 0.375rem;
      }

      .project-name {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
      }

      tr.revoked td {
        opacity: 0.6;
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadScopes('');
    void this.loadData();
  }

  /**
   * Fetch scopes from /api/v1/auth/scopes and cache the result per project.
   * With projectId set, each entry additionally carries mint eligibility
   * for that project -- computed fresh by the server for the current user,
   * never inferred or cached across projects. Falls back to the hardcoded
   * FALLBACK_SCOPES list (with no eligibility at all) on failure.
   *
   * Guards against two races when the user switches projects quickly:
   * responses are matched against a monotonic sequence number so a slower,
   * older request cannot overwrite state a newer one already applied, and
   * a non-OK response never leaves a DIFFERENT project's eligibility on
   * screen -- it falls back to the plain catalog (no eligibility) and
   * records which project failed, for an inline message.
   *
   * Scopes are enriched with `resource` (for grouping) and `isAlias` fields.
   * Aliases are listed first within their resource group for visual separation.
   */
  private async loadScopes(projectId: string): Promise<void> {
    const seq = ++this.scopesRequestSeq;
    const cacheKey = projectId || '';
    const cached = this.scopesCache.get(cacheKey);
    if (cached) {
      this.availableScopes = cached;
      this.scopesErrorProjectId = null;
      return;
    }
    try {
      const url = projectId
        ? `/api/v1/auth/scopes?projectId=${encodeURIComponent(projectId)}`
        : '/api/v1/auth/scopes';
      const res = await apiFetch(url);
      if (seq !== this.scopesRequestSeq) return; // superseded by a newer request
      if (!res.ok) {
        // Never keep a different project's eligibility on screen: fall
        // back to the plain catalog (server enforcement still applies
        // regardless of what this picker shows) and surface the failure --
        // only when a specific project was requested; the parameterless
        // catalog call has no project to blame.
        this.availableScopes = this.scopesCache.get('') ?? [...FALLBACK_SCOPES];
        if (projectId) this.scopesErrorProjectId = projectId;
        return;
      }
      const data = (await res.json()) as {
        scopes?: Array<{
          id: string;
          resource: string;
          action: string;
          description: string;
          eligibilityKind?: string;
          eligibility?: { eligible: boolean; reason?: string };
        }>;
        aliases?: Array<{
          id: string;
          description: string;
          expands_to: string[];
          eligibility?: { eligible: boolean; ineligibleMembers?: string[] };
        }>;
      };
      if (seq !== this.scopesRequestSeq) return; // superseded while parsing
      const scopes: ScopeOption[] = [];
      for (const s of data.scopes || []) {
        scopes.push({
          value: s.id,
          label: s.id,
          description: s.description,
          resource: s.resource,
          isAlias: false,
          eligibilityKind: s.eligibilityKind,
          eligible: s.eligibility?.eligible,
          eligibilityReason: s.eligibility?.reason,
        });
      }
      for (const a of data.aliases || []) {
        // Extract resource from the alias id (e.g. "agent:manage" -> "agent")
        const resource = a.id.split(':')[0];
        scopes.push({
          value: a.id,
          label: a.id,
          description: a.description,
          resource,
          isAlias: true,
          expandsTo: a.expands_to,
          eligible: a.eligibility?.eligible,
          ineligibleMembers: a.eligibility?.ineligibleMembers,
        });
      }
      // Sort: aliases first within each resource, then alphabetically
      scopes.sort((a, b) => {
        if (a.resource !== b.resource) return a.resource.localeCompare(b.resource);
        // Aliases before individual scopes within a group
        if (a.isAlias !== b.isAlias) return a.isAlias ? -1 : 1;
        return a.value.localeCompare(b.value);
      });
      // Apply the result even when it is empty, so a project with no
      // eligible scopes replaces the previous project's list instead of
      // leaving it on screen.
      this.scopesCache.set(cacheKey, scopes);
      this.availableScopes = scopes;
      this.scopesErrorProjectId = null;
    } catch {
      if (seq !== this.scopesRequestSeq) return; // superseded before the catch
      this.availableScopes = this.scopesCache.get('') ?? [...FALLBACK_SCOPES];
      if (projectId) this.scopesErrorProjectId = projectId;
      console.warn('Failed to fetch scopes from /api/v1/auth/scopes');
    }
  }

  private async loadData(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const [tokensRes, projectsRes] = await Promise.all([
        apiFetch('/api/v1/auth/tokens'),
        apiFetch('/api/v1/projects'),
      ]);

      if (!tokensRes.ok) {
        throw new Error(await extractApiError(tokensRes, 'Failed to load tokens'));
      }
      if (!projectsRes.ok) {
        throw new Error(await extractApiError(projectsRes, 'Failed to load projects'));
      }

      const tokensData = (await tokensRes.json()) as { items?: AccessToken[] };
      const projectsData = (await projectsRes.json()) as { projects?: Project[] };

      this.tokens = tokensData.items || [];
      this.projects = projectsData.projects || [];
    } catch (err) {
      console.error('Failed to load token data:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load data';
    } finally {
      this.loading = false;
    }
  }

  private getProjectName(projectId: string): string {
    const project = this.projects.find((p) => p.id === projectId);
    return project?.name || project?.slug || projectId;
  }

  private getTokenStatus(token: AccessToken): 'revoked' | 'expired' | 'active' {
    if (token.revoked) return 'revoked';
    if (token.expiresAt && new Date(token.expiresAt) < new Date()) return 'expired';
    return 'active';
  }

  // ── Create dialog ──────────────────────────────────────────────────

  private openCreateDialog(): void {
    this.createName = '';
    this.createProjectId = this.projects.length === 1 ? this.projects[0].id : '';
    this.createScopes = new Set();
    this.createExpiry = '90';
    this.createError = null;
    this.scopeFilter = '';
    this.collapsedGroups = new Set();
    this.createDialogOpen = true;
    void this.loadScopes(this.createProjectId);
  }

  private handleProjectChange(projectId: string): void {
    this.createProjectId = projectId;
    // Selected scopes may no longer be eligible under the new project;
    // clear them rather than silently submitting a stale selection.
    this.createScopes = new Set();
    void this.loadScopes(projectId);
  }

  private closeCreateDialog(): void {
    this.createDialogOpen = false;
  }

  private toggleGroup(resource: string): void {
    const next = new Set(this.collapsedGroups);
    if (next.has(resource)) {
      next.delete(resource);
    } else {
      next.add(resource);
    }
    this.collapsedGroups = next;
  }

  private toggleScope(scope: string): void {
    const option = this.availableScopes.find((s) => s.value === scope);
    if (option?.eligible === false) return; // defense in depth; checkbox is also disabled
    const next = new Set(this.createScopes);
    if (next.has(scope)) {
      next.delete(scope);
    } else {
      next.add(scope);
    }
    this.createScopes = next;
  }

  private async handleCreate(e: Event): Promise<void> {
    e.preventDefault();

    const name = this.createName.trim();
    if (!name) {
      this.createError = 'Name is required';
      return;
    }
    if (!this.createProjectId) {
      this.createError = 'Project is required';
      return;
    }

    if (this.createScopes.size === 0) {
      this.createError = 'At least one scope is required';
      return;
    }

    this.createLoading = true;
    this.createError = null;

    try {
      const days = parseInt(this.createExpiry, 10) || 90;
      const expiresAt = new Date();
      expiresAt.setDate(expiresAt.getDate() + days);

      const response = await apiFetch('/api/v1/auth/tokens', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name,
          projectId: this.createProjectId,
          scopes: Array.from(this.createScopes),
          expiresAt: expiresAt.toISOString(),
        }),
      });

      if (!response.ok) {
        const info = await parseApiError(response, 'Failed to create token');
        let message = info.message;
        // scope_violation carries {selector, reason} (ptone/scion#2122); name
        // the denied selector so the user knows which checkbox to remove,
        // without duplicating the server's reason vocabulary here.
        if (info.code === 'scope_violation' && typeof info.details?.selector === 'string') {
          message = `${message} (scope: ${info.details.selector})`;
        }
        throw new Error(message);
      }

      const data = (await response.json()) as { token: string };

      this.closeCreateDialog();
      this.revealToken = data.token;
      this.revealCopied = false;
      this.revealDialogOpen = true;

      await this.loadData();
    } catch (err) {
      console.error('Failed to create token:', err);
      this.createError = err instanceof Error ? err.message : 'Failed to create token';
    } finally {
      this.createLoading = false;
    }
  }

  // ── Revoke / Delete ────────────────────────────────────────────────

  private async handleRevoke(token: AccessToken): Promise<void> {
    if (
      !(await showConfirm(
        `Revoke token "${token.name}"? It will no longer be usable for authentication.`
      ))
    ) {
      return;
    }

    this.actionLoadingId = token.id;
    try {
      const response = await apiFetch(`/api/v1/auth/tokens/${token.id}/revoke`, {
        method: 'POST',
      });

      if (!response.ok && response.status !== 204) {
        throw new Error(await extractApiError(response, 'Failed to revoke token'));
      }

      await this.loadData();
    } catch (err) {
      console.error('Failed to revoke token:', err);
      showToast(err instanceof Error ? err.message : 'Failed to revoke');
    } finally {
      this.actionLoadingId = null;
    }
  }

  private async handleDelete(token: AccessToken): Promise<void> {
    if (!(await showConfirm(`Permanently delete token "${token.name}"? This cannot be undone.`))) {
      return;
    }

    this.actionLoadingId = token.id;
    try {
      const response = await apiFetch(`/api/v1/auth/tokens/${token.id}`, {
        method: 'DELETE',
      });

      if (!response.ok && response.status !== 204) {
        throw new Error(await extractApiError(response, 'Failed to delete token'));
      }

      await this.loadData();
    } catch (err) {
      console.error('Failed to delete token:', err);
      showToast(err instanceof Error ? err.message : 'Failed to delete');
    } finally {
      this.actionLoadingId = null;
    }
  }

  // ── Copy ───────────────────────────────────────────────────────────

  private async copyToken(): Promise<void> {
    try {
      await navigator.clipboard.writeText(this.revealToken);
      this.revealCopied = true;
      setTimeout(() => {
        this.revealCopied = false;
      }, 2000);
    } catch {
      // Fallback: select the text
      const el = this.shadowRoot?.querySelector('.token-value') as HTMLElement | null;
      if (el) {
        const range = document.createRange();
        range.selectNodeContents(el);
        const sel = window.getSelection();
        sel?.removeAllRanges();
        sel?.addRange(range);
      }
    }
  }

  // ── Formatting ─────────────────────────────────────────────────────

  // ── Rendering ──────────────────────────────────────────────────────

  override render() {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading access tokens...</p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load</h2>
          <p>There was a problem loading your access tokens.</p>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${() => this.loadData()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    return html`
      <div class="list-header">
        <sl-button variant="primary" @click=${this.openCreateDialog}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Create Token
        </sl-button>
      </div>
      ${this.tokens.length === 0 ? this.renderEmpty() : this.renderTable()}
      ${this.renderCreateDialog()} ${this.renderRevealDialog()}
    `;
  }

  private renderEmpty() {
    return html`
      <div class="empty-state">
        <sl-icon name="key"></sl-icon>
        <h3>No Access Tokens</h3>
        <p>
          Create personal access tokens to authenticate CI/CD pipelines and automation tools with
          your projects.
        </p>
        <sl-button variant="primary" size="small" @click=${this.openCreateDialog}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Create Token
        </sl-button>
      </div>
    `;
  }

  private renderTable() {
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Status</th>
              <th class="hide-mobile">Project</th>
              <th class="hide-mobile">Scopes</th>
              <th class="hide-mobile">Created</th>
              <th class="hide-mobile">Last Used</th>
              <th>Expires</th>
              <th class="actions-cell"></th>
            </tr>
          </thead>
          <tbody>
            ${this.tokens.map((token) => this.renderRow(token))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderRow(token: AccessToken) {
    const status = this.getTokenStatus(token);
    const isActionLoading = this.actionLoadingId === token.id;

    return html`
      <tr class=${status === 'revoked' ? 'revoked' : ''}>
        <td class="key-cell">
          <div class="key-info">
            <div
              class="key-icon"
              style="background: var(--sl-color-primary-100, #dbeafe); color: var(--sl-color-primary-600, #2563eb);"
            >
              <sl-icon name="key"></sl-icon>
            </div>
            <div>
              ${token.name}
              <div class="project-name">${token.prefix}</div>
            </div>
          </div>
        </td>
        <td>
          ${status === 'revoked'
            ? html`<span class="status-revoked">Revoked</span>`
            : status === 'expired'
              ? html`<span class="status-expired">Expired</span>`
              : html`<span class="status-active">Active</span>`}
        </td>
        <td class="hide-mobile">
          <span class="project-name">${this.getProjectName(token.projectId)}</span>
        </td>
        <td class="hide-mobile">
          <div class="scopes-cell">
            ${token.scopes.map((scope) => html`<span class="scope-badge">${scope}</span>`)}
          </div>
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${formatRelative(token.created)}</span>
        </td>
        <td class="hide-mobile">
          <span class="meta-text">
            ${token.lastUsed ? formatRelative(token.lastUsed) : '\u2014'}
          </span>
        </td>
        <td>
          <span class="meta-text">
            ${token.expiresAt
              ? formatInstantWithZone(token.expiresAt) || token.expiresAt
              : '\u2014'}
          </span>
        </td>
        <td class="actions-cell">
          ${status === 'active'
            ? html`
                <sl-icon-button
                  name="x-circle"
                  label="Revoke"
                  ?disabled=${isActionLoading}
                  @click=${() => this.handleRevoke(token)}
                ></sl-icon-button>
              `
            : nothing}
          <sl-icon-button
            name="trash"
            label="Delete"
            ?disabled=${isActionLoading}
            @click=${() => this.handleDelete(token)}
          ></sl-icon-button>
        </td>
      </tr>
    `;
  }

  /**
   * Filter scopes by the current search term, matching against
   * the scope value, description, or resource type label.
   */
  private getFilteredScopes(): ScopeOption[] {
    const filter = this.scopeFilter.toLowerCase().trim();
    if (!filter) return this.availableScopes;
    return this.availableScopes.filter(
      (scope) =>
        scope.value.toLowerCase().includes(filter) ||
        scope.description.toLowerCase().includes(filter) ||
        (RESOURCE_TYPE_LABELS[scope.resource] || scope.resource).toLowerCase().includes(filter)
    );
  }

  private renderCreateDialog() {
    const filteredScopes = this.getFilteredScopes();
    const groups = groupScopesByResource(filteredScopes);

    return html`
      <sl-dialog
        label="Create Access Token"
        ?open=${this.createDialogOpen}
        @sl-request-close=${this.closeCreateDialog}
      >
        <form class="dialog-form" @submit=${this.handleCreate}>
          <sl-input
            label="Name"
            placeholder="e.g. github-actions, ci-deploy"
            value=${this.createName}
            @sl-input=${(e: Event) => {
              this.createName = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>

          <sl-select
            label="Project"
            placeholder="Select a project"
            value=${this.createProjectId}
            @sl-change=${(e: Event) => {
              this.handleProjectChange((e.target as HTMLSelectElement).value);
            }}
            required
          >
            ${this.projects.map(
              (project) =>
                html`<sl-option value=${project.id}
                  >${project.name || project.slug || project.id}</sl-option
                >`
            )}
          </sl-select>

          <div>
            <div class="field-label">
              Scopes
              ${this.createScopes.size > 0
                ? html`<span class="scope-selected-count"
                    >(${this.createScopes.size} selected)</span
                  >`
                : nothing}
            </div>
            ${this.scopesErrorProjectId && this.scopesErrorProjectId === this.createProjectId
              ? html`<div class="dialog-error">
                  Could not check which scopes you can select for this project. Showing the full
                  catalog with no eligibility -- the server still enforces access when you submit.
                </div>`
              : nothing}
            <div class="scope-selector">
              <div class="scope-search">
                <sl-input
                  aria-label="Filter scopes"
                  placeholder="Filter scopes..."
                  size="small"
                  clearable
                  value=${this.scopeFilter}
                  @sl-input=${(e: Event) => {
                    this.scopeFilter = (e.target as HTMLInputElement).value;
                  }}
                >
                  <sl-icon name="search" slot="prefix"></sl-icon>
                </sl-input>
              </div>
              ${groups.length === 0
                ? html`<div class="scope-no-results">No scopes match "${this.scopeFilter}"</div>`
                : groups.map(([resource, scopes]) => this.renderScopeGroup(resource, scopes))}
            </div>
          </div>

          <sl-select
            label="Expires in"
            value=${this.createExpiry}
            @sl-change=${(e: Event) => {
              this.createExpiry = (e.target as HTMLSelectElement).value;
            }}
          >
            <sl-option value="7">7 days</sl-option>
            <sl-option value="30">30 days</sl-option>
            <sl-option value="90">90 days</sl-option>
            <sl-option value="180">180 days</sl-option>
            <sl-option value="365">365 days (maximum)</sl-option>
          </sl-select>

          ${this.createError ? html`<div class="dialog-error">${this.createError}</div>` : nothing}
        </form>

        <sl-button
          slot="footer"
          variant="default"
          @click=${this.closeCreateDialog}
          ?disabled=${this.createLoading}
        >
          Cancel
        </sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.createLoading}
          ?disabled=${this.createLoading}
          @click=${this.handleCreate}
        >
          Create Token
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderScopeGroup(resource: string, scopes: ScopeOption[]) {
    const isCollapsed = this.collapsedGroups.has(resource);
    const label = RESOURCE_TYPE_LABELS[resource] || resource;
    const selectedInGroup = scopes.filter((s) => this.createScopes.has(s.value)).length;
    const aliases = scopes.filter((s) => s.isAlias);
    const regularScopes = scopes.filter((s) => !s.isAlias);

    return html`
      <div class="scope-group">
        <div
          class="scope-group-header"
          @click=${() => this.toggleGroup(resource)}
          role="button"
          tabindex="0"
          aria-expanded=${!isCollapsed}
          @keydown=${(e: KeyboardEvent) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              this.toggleGroup(resource);
            }
          }}
        >
          <sl-icon name="chevron-down" class=${isCollapsed ? 'collapsed' : ''}></sl-icon>
          ${label}
          <span class="scope-group-count"
            >${selectedInGroup > 0 ? `${selectedInGroup}/` : ''}${scopes.length}</span
          >
        </div>
        ${isCollapsed
          ? nothing
          : html`
              <div class="scope-group-items">
                ${aliases.map((scope) => this.renderAliasScope(scope))}
                <div class="scope-checkboxes">
                  ${regularScopes.map((scope) => this.renderScopeCheckbox(scope))}
                </div>
              </div>
            `}
      </div>
    `;
  }

  private renderAliasScope(scope: ScopeOption) {
    const ineligible = scope.eligible === false;
    return html`
      <div class="scope-alias-item">
        <sl-checkbox
          ?checked=${this.createScopes.has(scope.value)}
          ?disabled=${ineligible}
          @sl-change=${() => this.toggleScope(scope.value)}
        >
          <span class="scope-checkbox-label">${scope.label}</span>
          <br />
          <span class="scope-checkbox-desc">${scope.description}</span>
          <br />
          <span class="scope-alias-badge">
            <sl-icon name="collection"></sl-icon>
            Alias${scope.expandsTo ? ` — expands to ${scope.expandsTo.length} scopes` : ''}
          </span>
          ${ineligible
            ? html`<br /><span class="scope-ineligible-reason"
                  >Not
                  selectable${scope.ineligibleMembers?.length
                    ? `: missing ${scope.ineligibleMembers.join(', ')}`
                    : ''}</span
                >`
            : nothing}
        </sl-checkbox>
      </div>
    `;
  }

  private renderScopeCheckbox(scope: ScopeOption) {
    const ineligible = scope.eligible === false;
    return html`
      <div class="scope-checkbox-item">
        <sl-checkbox
          ?checked=${this.createScopes.has(scope.value)}
          ?disabled=${ineligible}
          @sl-change=${() => this.toggleScope(scope.value)}
        >
          <span class="scope-checkbox-label">${scope.label}</span>
          ${scope.eligibilityKind === 'relationship'
            ? html`<span class="scope-relationship-badge"
                >${relationshipBadgeText(scope.value)}</span
              >`
            : nothing}
          <br />
          <span class="scope-checkbox-desc">${scope.description}</span>
          ${ineligible
            ? html`<br /><span class="scope-ineligible-reason"
                  >Not selectable: ${formatEligibilityReason(scope.eligibilityReason)}</span
                >`
            : nothing}
        </sl-checkbox>
      </div>
    `;
  }

  private renderRevealDialog() {
    return html`
      <sl-dialog
        label="Token Created"
        ?open=${this.revealDialogOpen}
        @sl-request-close=${() => {
          this.revealDialogOpen = false;
        }}
      >
        <div class="token-reveal">
          <div class="dialog-hint">
            <sl-icon name="exclamation-triangle"></sl-icon>
            Copy this token now. You won't be able to see it again.
          </div>

          <div class="token-value">${this.revealToken}</div>

          <div class="token-copy-row">
            <sl-button variant="primary" size="small" @click=${this.copyToken}>
              <sl-icon slot="prefix" name=${this.revealCopied ? 'check-lg' : 'clipboard'}></sl-icon>
              ${this.revealCopied ? 'Copied!' : 'Copy to clipboard'}
            </sl-button>
          </div>
        </div>

        <sl-button
          slot="footer"
          variant="primary"
          @click=${() => {
            this.revealDialogOpen = false;
          }}
        >
          Done
        </sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-token-list': ScionTokenList;
  }
}
