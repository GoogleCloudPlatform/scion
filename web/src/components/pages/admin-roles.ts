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
 * Admin Roles page component
 *
 * CRUD management for role definitions. System roles are read-only;
 * custom roles support create, edit, and delete with CanDelegate enforcement.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { navigateTo } from '../../client/main.js';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

interface RoleDefinition {
  id: string;
  name: string;
  description: string;
  scopeType: string;
  permissions: string[];
  system: boolean;
  createdAt: string;
  updatedAt: string;
}

interface Permission {
  ID: string;
  Resource: string;
  Action: string;
  Description: string;
}

/**
 * Portable role representation used for export/import.
 * Omits server-generated fields (id, system, createdAt, updatedAt).
 */
interface ImportableRole {
  name: string;
  description: string;
  scopeType: string;
  permissions: string[];
}

/** Envelope for the exported JSON file. */
interface RoleExportEnvelope {
  version: '1';
  exportedAt: string;
  roles: ImportableRole[];
}

/** Per-role import outcome. */
interface ImportRoleResult {
  name: string;
  status: 'created' | 'skipped' | 'error';
  error?: string;
}

/** Aggregate import result. */
interface ImportResult {
  created: number;
  skipped: number;
  errors: ImportRoleResult[];
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

@customElement('scion-page-admin-roles')
export class ScionPageAdminRoles extends LitElement {
  @state() private loading = true;
  @state() private roles: RoleDefinition[] = [];
  @state() private error: string | null = null;
  @state() private permissions: Permission[] = [];

  // Dialog state
  @state() private showCreateDialog = false;
  @state() private showImportDialog = false;

  // Import state
  @state() private importParsedRoles: ImportableRole[] = [];
  @state() private importParseError: string | null = null;
  @state() private importInProgress = false;
  @state() private importResults: ImportResult | null = null;

  // Form fields
  @state() private formName = '';
  @state() private formDescription = '';
  @state() private formScopeType = 'system';
  @state() private formPermissions: Set<string> = new Set();

  // Action state
  @state() private actionInProgress = false;
  @state() private actionFeedback: { message: string; variant: 'success' | 'danger' } | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1.5rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .header-right {
      display: flex;
      align-items: center;
      gap: 1rem;
    }

    .role-count {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .table-container {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }

    table {
      width: 100%;
      border-collapse: collapse;
    }

    th {
      text-align: left;
      padding: 0.75rem 1rem;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }

    tr:last-child td {
      border-bottom: none;
    }

    .role-name {
      font-weight: 500;
    }

    .role-link {
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
      cursor: pointer;
    }

    .role-link:hover {
      text-decoration: underline;
    }

    .role-description {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      max-width: 300px;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .type-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
    }

    .type-badge.system {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #a16207);
    }

    .type-badge.custom {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .scope-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    .perm-count {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .actions {
      display: flex;
      gap: 0.5rem;
    }

    .empty-state {
      text-align: center;
      padding: 4rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px dashed var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .empty-state > sl-icon {
      font-size: 4rem;
      color: var(--scion-text-muted, #64748b);
      opacity: 0.5;
      margin-bottom: 1rem;
    }

    .empty-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .empty-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0;
    }

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

    /* Dialog form styles */
    .form-group {
      margin-bottom: 1rem;
    }

    .form-group:last-child {
      margin-bottom: 0;
    }

    .permissions-section {
      margin-top: 1rem;
    }

    .permissions-section h4 {
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .permission-group {
      margin-bottom: 0.75rem;
    }

    .permission-group-title {
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.25rem;
      padding-bottom: 0.25rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .permission-item {
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      padding: 0.25rem 0;
    }

    .permission-item sl-checkbox {
      flex-shrink: 0;
    }

    .permission-label {
      font-size: 0.8125rem;
      color: var(--scion-text, #1e293b);
    }

    .permission-desc {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .permissions-scroll {
      max-height: 400px;
      overflow-y: auto;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem;
    }

    .feedback-alert {
      margin-bottom: 1rem;
    }

    .delete-warning {
      color: var(--sl-color-danger-600, #dc2626);
      font-weight: 500;
    }

    /* Import dialog */
    .import-help {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .file-label {
      display: block;
      font-size: 0.875rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.5rem;
    }

    .file-input {
      display: block;
      width: 100%;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .import-error {
      margin-top: 0.75rem;
    }

    .error-pre {
      margin: 0;
      white-space: pre-wrap;
      font-size: 0.8125rem;
      font-family: var(--scion-font-mono, monospace);
    }

    .import-preview {
      margin-top: 1rem;
    }

    .import-preview h4 {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .import-preview-list {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      max-height: 300px;
      overflow-y: auto;
    }

    .import-preview-item {
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .import-preview-item:last-child {
      border-bottom: none;
    }

    .import-preview-item.will-skip {
      opacity: 0.6;
    }

    .import-preview-name {
      font-size: 0.875rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .import-skip-badge {
      font-size: 0.6875rem;
      font-weight: 500;
      padding: 0.0625rem 0.375rem;
      border-radius: 9999px;
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #a16207);
    }

    .import-new-badge {
      font-size: 0.6875rem;
      font-weight: 500;
      padding: 0.0625rem 0.375rem;
      border-radius: 9999px;
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .import-preview-meta {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.125rem;
    }

    .import-results {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
    }

    .import-results-details {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      max-height: 300px;
      overflow-y: auto;
    }

    .import-result-item {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      font-size: 0.875rem;
    }

    .import-result-item:last-child {
      border-bottom: none;
    }

    .import-result-name {
      font-weight: 500;
      color: var(--scion-text, #1e293b);
    }

    .import-result-error {
      font-size: 0.75rem;
      color: var(--sl-color-danger-600, #dc2626);
    }

    @media (max-width: 768px) {
      .hide-mobile {
        display: none;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadData();
  }

  // ---------------------------------------------------------------------------
  // Data loading
  // ---------------------------------------------------------------------------

  private async loadData(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const [rolesRes, permsRes] = await Promise.all([
        apiFetch('/api/v1/admin/roles'),
        apiFetch('/api/v1/admin/permissions'),
      ]);

      if (!rolesRes.ok) {
        throw new Error(await extractApiError(rolesRes, `HTTP ${rolesRes.status}`));
      }
      if (!permsRes.ok) {
        throw new Error(await extractApiError(permsRes, `HTTP ${permsRes.status}`));
      }

      const rolesData = (await rolesRes.json()) as { items: RoleDefinition[] };
      const permsData = (await permsRes.json()) as { items: Permission[] };

      this.roles = rolesData.items || [];
      this.permissions = permsData.items || [];
    } catch (err) {
      console.error('Failed to load roles:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load roles';
    } finally {
      this.loading = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------------

  /** Group permissions by resource for the multi-select UI. */
  private groupPermissions(perms: Permission[] = this.permissions): Map<string, Permission[]> {
    const groups = new Map<string, Permission[]>();
    for (const perm of perms) {
      const resource = perm.Resource || 'other';
      if (!groups.has(resource)) {
        groups.set(resource, []);
      }
      groups.get(resource)!.push(perm);
    }
    return groups;
  }

  /** Human-friendly resource label. */
  private resourceLabel(resource: string): string {
    return resource
      .split('_')
      .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
      .join(' ');
  }

  private formatRelativeTime(dateString: string): string {
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return dateString;
      const diffMs = Date.now() - date.getTime();
      const diffSeconds = Math.round(diffMs / 1000);
      const diffMinutes = Math.round(diffMs / (1000 * 60));
      const diffHours = Math.round(diffMs / (1000 * 60 * 60));
      const diffDays = Math.round(diffMs / (1000 * 60 * 60 * 24));

      const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

      if (Math.abs(diffSeconds) < 60) return rtf.format(-diffSeconds, 'second');
      if (Math.abs(diffMinutes) < 60) return rtf.format(-diffMinutes, 'minute');
      if (Math.abs(diffHours) < 24) return rtf.format(-diffHours, 'hour');
      return rtf.format(-diffDays, 'day');
    } catch {
      return dateString;
    }
  }

  // ---------------------------------------------------------------------------
  // Form management
  // ---------------------------------------------------------------------------

  private openCreateDialog(): void {
    this.formName = '';
    this.formDescription = '';
    this.formScopeType = 'system';
    this.formPermissions = new Set();
    this.showCreateDialog = true;
  }


  private togglePermission(permId: string): void {
    const next = new Set(this.formPermissions);
    if (next.has(permId)) {
      next.delete(permId);
    } else {
      next.add(permId);
    }
    this.formPermissions = next;
  }

  // ---------------------------------------------------------------------------
  // API actions
  // ---------------------------------------------------------------------------

  private async createRole(): Promise<void> {
    this.actionInProgress = true;
    this.actionFeedback = null;
    try {
      const res = await apiFetch('/api/v1/admin/roles', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: this.formName.trim(),
          description: this.formDescription.trim(),
          scopeType: this.formScopeType,
          permissions: [...this.formPermissions],
        }),
      });

      if (!res.ok) {
        const msg = await extractApiError(res, `HTTP ${res.status}`);
        this.actionFeedback = { message: msg, variant: 'danger' };
        return;
      }

      this.showCreateDialog = false;
      this.actionFeedback = { message: `Role "${this.formName}" created`, variant: 'success' };
      void this.loadData();
    } catch (err) {
      this.actionFeedback = {
        message: err instanceof Error ? err.message : 'Failed to create role',
        variant: 'danger',
      };
    } finally {
      this.actionInProgress = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Export
  // ---------------------------------------------------------------------------

  /**
   * Export custom roles via the dedicated backend endpoint
   * GET /api/v1/admin/roles/export. Falls back to client-side
   * export if the endpoint is unavailable.
   */
  private async exportRoles(): Promise<void> {
    const customRoles = this.roles.filter((r) => !r.system);
    if (customRoles.length === 0) {
      this.actionFeedback = { message: 'No custom roles to export', variant: 'danger' };
      return;
    }

    let exportJson: string;

    try {
      const res = await apiFetch('/api/v1/admin/roles/export');
      if (res.ok) {
        const data = await res.json();
        exportJson = JSON.stringify(data, null, 2);
      } else {
        // Fall back to client-side export
        exportJson = this.buildClientExport(customRoles);
      }
    } catch {
      // Fall back to client-side export
      exportJson = this.buildClientExport(customRoles);
    }

    const blob = new Blob([exportJson], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `scion-roles-export-${new Date().toISOString().slice(0, 10)}.json`;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);

    this.actionFeedback = {
      message: `Exported ${customRoles.length} custom role${customRoles.length !== 1 ? 's' : ''}`,
      variant: 'success',
    };
  }

  /** Client-side fallback for building the export envelope. */
  private buildClientExport(customRoles: RoleDefinition[]): string {
    const exportData: RoleExportEnvelope = {
      version: '1',
      exportedAt: new Date().toISOString(),
      roles: customRoles.map((r) => ({
        name: r.name,
        description: r.description,
        scopeType: r.scopeType,
        permissions: [...r.permissions],
      })),
    };
    return JSON.stringify(exportData, null, 2);
  }

  // ---------------------------------------------------------------------------
  // Import
  // ---------------------------------------------------------------------------

  private openImportDialog(): void {
    this.importParsedRoles = [];
    this.importParseError = null;
    this.importResults = null;
    this.showImportDialog = true;
  }

  /**
   * Handle the file input change event: read the file, parse JSON,
   * validate structure, and populate the preview.
   */
  private async handleImportFileSelect(e: Event): Promise<void> {
    this.importParseError = null;
    this.importParsedRoles = [];
    this.importResults = null;

    const input = e.target as HTMLInputElement;
    if (!input.files?.length) return;

    const file = input.files[0];

    // Size guard: 1 MB max
    if (file.size > 1_048_576) {
      this.importParseError = 'File is too large (max 1 MB).';
      return;
    }

    try {
      const text = await file.text();
      const data = JSON.parse(text) as Record<string, unknown>;

      // Accept either the envelope format or a plain array of roles
      let roles: unknown[];
      if (Array.isArray(data)) {
        roles = data;
      } else if (
        data &&
        typeof data === 'object' &&
        'roles' in data &&
        Array.isArray(data.roles)
      ) {
        roles = data.roles as unknown[];
      } else {
        this.importParseError =
          'Invalid format. Expected a JSON file with a "roles" array, or a plain array of role objects.';
        return;
      }

      if (roles.length === 0) {
        this.importParseError = 'The file contains no roles to import.';
        return;
      }

      // Validate each role entry
      const validated: ImportableRole[] = [];
      const validationErrors: string[] = [];

      for (let i = 0; i < roles.length; i++) {
        const entry = roles[i] as Record<string, unknown>;
        if (!entry || typeof entry !== 'object') {
          validationErrors.push(`Entry ${i + 1}: not a valid object`);
          continue;
        }
        if (typeof entry.name !== 'string' || !entry.name.trim()) {
          validationErrors.push(`Entry ${i + 1}: missing or empty "name"`);
          continue;
        }
        if (entry.permissions !== undefined && !Array.isArray(entry.permissions)) {
          validationErrors.push(`Entry ${i + 1} (${entry.name}): "permissions" must be an array`);
          continue;
        }

        validated.push({
          name: entry.name.trim(),
          description: typeof entry.description === 'string' ? entry.description : '',
          scopeType: typeof entry.scopeType === 'string' ? entry.scopeType : 'system',
          permissions: Array.isArray(entry.permissions)
            ? (entry.permissions as string[]).filter((p) => typeof p === 'string')
            : [],
        });
      }

      if (validationErrors.length > 0) {
        this.importParseError = validationErrors.join('\n');
        return;
      }

      this.importParsedRoles = validated;
    } catch {
      this.importParseError = 'Failed to parse the file. Ensure it is valid JSON.';
    }
  }

  /**
   * Import the parsed roles via the dedicated backend endpoint
   * POST /api/v1/admin/roles/import. The endpoint handles duplicate
   * detection, permission validation, and CanDelegate checks
   * server-side.
   */
  private async importRoles(): Promise<void> {
    if (this.importParsedRoles.length === 0) return;

    this.importInProgress = true;
    this.importResults = null;

    try {
      const payload: RoleExportEnvelope = {
        version: '1',
        exportedAt: new Date().toISOString(),
        roles: this.importParsedRoles,
      };

      const res = await apiFetch('/api/v1/admin/roles/import', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });

      if (!res.ok) {
        const msg = await extractApiError(res, `HTTP ${res.status}`);
        this.importResults = {
          created: 0,
          skipped: 0,
          errors: [{ name: '(request)', status: 'error', error: msg }],
        };
        return;
      }

      const data = (await res.json()) as {
        created: number;
        skipped: number;
        errors: number;
        items: { name: string; status: 'created' | 'skipped' | 'error'; reason?: string; id?: string }[];
      };

      const results: ImportResult = {
        created: data.created,
        skipped: data.skipped,
        errors: (data.items || [])
          .filter((item) => item.status !== 'created')
          .map((item) => {
            const entry: ImportRoleResult = {
              name: item.name,
              status: item.status === 'error' ? 'error' as const : 'skipped' as const,
            };
            if (item.reason) entry.error = item.reason;
            return entry;
          }),
      };

      this.importResults = results;

      // If any were created, refresh the list
      if (results.created > 0) {
        void this.loadData();
      }
    } catch (err) {
      this.importResults = {
        created: 0,
        skipped: 0,
        errors: [{
          name: '(request)',
          status: 'error',
          error: err instanceof Error ? err.message : 'Failed to import roles',
        }],
      };
    } finally {
      this.importInProgress = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    return html`
      ${this.actionFeedback
        ? html`
            <sl-alert
              class="feedback-alert"
              variant=${this.actionFeedback.variant}
              open
              closable
              duration="5000"
              @sl-after-hide=${() => {
                this.actionFeedback = null;
              }}
            >
              <sl-icon
                slot="icon"
                name=${this.actionFeedback.variant === 'success'
                  ? 'check-circle'
                  : 'exclamation-triangle'}
              ></sl-icon>
              ${this.actionFeedback.message}
            </sl-alert>
          `
        : ''}

      <div class="header">
        <h1>Roles</h1>
        <div class="header-right">
          ${!this.loading && !this.error
            ? html`<span class="role-count"
                >${this.roles.length} role${this.roles.length !== 1 ? 's' : ''}</span
              >`
            : ''}
          <sl-button
            variant="default"
            size="small"
            @click=${() => this.exportRoles()}
            ?disabled=${this.loading || !!this.error || this.roles.filter((r) => !r.system).length === 0}
          >
            <sl-icon slot="prefix" name="download"></sl-icon>
            Export
          </sl-button>
          <sl-button variant="default" size="small" @click=${() => this.openImportDialog()}>
            <sl-icon slot="prefix" name="upload"></sl-icon>
            Import
          </sl-button>
          <sl-button variant="primary" size="small" @click=${() => this.openCreateDialog()}>
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            Create Role
          </sl-button>
        </div>
      </div>

      ${this.loading ? this.renderLoading() : this.error ? this.renderError() : this.renderRoles()}
      ${this.renderCreateDialog()}
      ${this.renderImportDialog()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading roles...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Roles</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadData()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderRoles() {
    if (this.roles.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="shield-lock"></sl-icon>
          <h2>No Roles Found</h2>
          <p>Create a custom role to get started.</p>
        </div>
      `;
    }

    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Scope</th>
              <th class="hide-mobile">Permissions</th>
              <th>Type</th>
              <th class="hide-mobile">Updated</th>
            </tr>
          </thead>
          <tbody>
            ${this.roles.map((role) => this.renderRoleRow(role))}
          </tbody>
        </table>
      </div>
    `;
  }

  private navigateToRole(roleId: string): void {
    navigateTo(`/admin/roles/${encodeURIComponent(roleId)}`);
  }

  private renderRoleRow(role: RoleDefinition) {
    return html`
      <tr>
        <td>
          <a
            class="role-name role-link"
            href="/admin/roles/${encodeURIComponent(role.id)}"
            @click=${(e: Event) => {
              e.preventDefault();
              this.navigateToRole(role.id);
            }}
          >${role.name}</a>
          <div class="role-description">${role.description || '—'}</div>
        </td>
        <td><span class="scope-badge">${role.scopeType}</span></td>
        <td class="hide-mobile">
          <span class="perm-count">${role.permissions?.length ?? 0}</span>
        </td>
        <td>
          <span class="type-badge ${role.system ? 'system' : 'custom'}">
            ${role.system ? 'System' : 'Custom'}
          </span>
        </td>
        <td class="hide-mobile">
          <span class="perm-count">${this.formatRelativeTime(role.updatedAt)}</span>
        </td>
      </tr>
    `;
  }

  // ---------------------------------------------------------------------------
  // Permission selector (shared between create and edit)
  // ---------------------------------------------------------------------------

  private renderPermissionSelector() {
    const groups = this.groupPermissions();

    return html`
      <div class="permissions-section">
        <h4>Permissions</h4>
        <div class="permissions-scroll">
          ${[...groups.entries()].map(
            ([resource, perms]) => html`
              <div class="permission-group">
                <div class="permission-group-title">${this.resourceLabel(resource)}</div>
                ${perms.map(
                  (perm) => html`
                    <div class="permission-item">
                      <sl-checkbox
                        ?checked=${this.formPermissions.has(perm.ID)}
                        @sl-change=${() => this.togglePermission(perm.ID)}
                      ></sl-checkbox>
                      <div>
                        <div class="permission-label">${perm.ID}</div>
                        <div class="permission-desc">${perm.Description}</div>
                      </div>
                    </div>
                  `
                )}
              </div>
            `
          )}
        </div>
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Dialogs
  // ---------------------------------------------------------------------------

  private renderCreateDialog() {
    if (!this.showCreateDialog) return nothing;

    return html`
      <sl-dialog
        label="Create Role"
        open
        @sl-request-close=${() => {
          if (!this.actionInProgress) this.showCreateDialog = false;
        }}
      >
        <div class="form-group">
          <sl-input
            label="Name"
            placeholder="e.g., project-viewer"
            .value=${this.formName}
            @sl-input=${(e: Event) => {
              this.formName = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>
        </div>
        <div class="form-group">
          <sl-input
            label="Description"
            placeholder="A brief description of this role"
            .value=${this.formDescription}
            @sl-input=${(e: Event) => {
              this.formDescription = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>
        </div>
        <div class="form-group">
          <sl-select
            label="Scope Type"
            .value=${this.formScopeType}
            @sl-change=${(e: Event) => {
              this.formScopeType = (e.target as HTMLSelectElement).value;
            }}
          >
            <sl-option value="system">System</sl-option>
            <sl-option value="project">Project</sl-option>
          </sl-select>
        </div>
        ${this.renderPermissionSelector()}
        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.actionInProgress}
          @click=${() => {
            this.showCreateDialog = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.actionInProgress}
          ?disabled=${!this.formName.trim()}
          @click=${() => this.createRole()}
          >Create Role</sl-button
        >
      </sl-dialog>
    `;
  }

  // ---------------------------------------------------------------------------
  // Import dialog
  // ---------------------------------------------------------------------------

  private renderImportDialog() {
    if (!this.showImportDialog) return nothing;

    return html`
      <sl-dialog
        label="Import Roles"
        open
        @sl-request-close=${() => {
          if (!this.importInProgress) this.showImportDialog = false;
        }}
      >
        ${this.importResults ? this.renderImportResults() : this.renderImportForm()}
      </sl-dialog>
    `;
  }

  private renderImportForm() {
    return html`
      <p class="import-help">
        Upload a JSON file containing custom role definitions.
        Roles with names that already exist will be skipped.
      </p>

      <div class="form-group">
        <label class="file-label" for="role-import-input">Select file</label>
        <input
          id="role-import-input"
          type="file"
          accept=".json,application/json"
          class="file-input"
          @change=${(e: Event) => this.handleImportFileSelect(e)}
        />
      </div>

      ${this.importParseError
        ? html`
            <sl-alert variant="danger" open class="import-error">
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              <pre class="error-pre">${this.importParseError}</pre>
            </sl-alert>
          `
        : nothing}

      ${this.importParsedRoles.length > 0 ? this.renderImportPreview() : nothing}

      <sl-button
        slot="footer"
        variant="default"
        ?disabled=${this.importInProgress}
        @click=${() => {
          this.showImportDialog = false;
        }}
        >Cancel</sl-button
      >
      <sl-button
        slot="footer"
        variant="primary"
        ?loading=${this.importInProgress}
        ?disabled=${this.importParsedRoles.length === 0}
        @click=${() => this.importRoles()}
        >Import ${this.importParsedRoles.length > 0 ? `${this.importParsedRoles.length} Role${this.importParsedRoles.length !== 1 ? 's' : ''}` : 'Roles'}</sl-button
      >
    `;
  }

  private renderImportPreview() {
    const existingNames = new Set(this.roles.map((r) => r.name));

    return html`
      <div class="import-preview">
        <h4>Preview (${this.importParsedRoles.length} role${this.importParsedRoles.length !== 1 ? 's' : ''})</h4>
        <div class="import-preview-list">
          ${this.importParsedRoles.map((role) => {
            const exists = existingNames.has(role.name);
            return html`
              <div class="import-preview-item ${exists ? 'will-skip' : ''}">
                <div class="import-preview-name">
                  ${role.name}
                  ${exists
                    ? html`<span class="import-skip-badge">exists — will skip</span>`
                    : html`<span class="import-new-badge">new</span>`}
                </div>
                <div class="import-preview-meta">
                  ${role.scopeType} · ${role.permissions.length} permission${role.permissions.length !== 1 ? 's' : ''}
                </div>
              </div>
            `;
          })}
        </div>
      </div>
    `;
  }

  private renderImportResults() {
    const results = this.importResults!;
    const hasErrors = results.errors.filter((e) => e.status === 'error').length > 0;

    return html`
      <div class="import-results">
        <sl-alert variant=${hasErrors ? 'warning' : 'success'} open>
          <sl-icon slot="icon" name=${hasErrors ? 'exclamation-triangle' : 'check-circle'}></sl-icon>
          Import complete: ${results.created} created, ${results.skipped} skipped${hasErrors ? `, ${results.errors.filter((e) => e.status === 'error').length} failed` : ''}.
        </sl-alert>

        ${results.errors.length > 0
          ? html`
              <div class="import-results-details">
                ${results.errors.map(
                  (r) => html`
                    <div class="import-result-item ${r.status}">
                      <span class="import-result-name">${r.name}</span>
                      <span class="import-result-status">
                        ${r.status === 'skipped'
                          ? html`<sl-badge variant="neutral">Skipped</sl-badge>`
                          : html`<sl-badge variant="danger">Error</sl-badge>`}
                      </span>
                      ${r.error ? html`<span class="import-result-error">${r.error}</span>` : nothing}
                    </div>
                  `
                )}
              </div>
            `
          : nothing}
      </div>

      <sl-button
        slot="footer"
        variant="primary"
        @click=${() => {
          this.showImportDialog = false;
        }}
        >Done</sl-button
      >
    `;
  }

}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-roles': ScionPageAdminRoles;
  }
}
