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
 * Reusable Role Binding Assignment Form
 *
 * Single source of truth for the role-binding creation form used by:
 *   - admin-role-bindings (global "Assign Role" dialog)
 *   - admin-role-detail (inline "Add Binding" form)
 *   - effective-role-provenance (user-oriented "Assign Role" dialog)
 *
 * Field order: Principal → Scope → Project (when project scope) → Role
 *
 * Because scope determines eligible roles, a scope change recomputes
 * role options and clears an incompatible selected role. Changing scope
 * away from 'project' clears the project value.
 *
 * Emits `form-change` whenever any field changes, with the full form state.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PrincipalChangeDetail } from './principal-picker.js';
import type { ProjectChangeDetail } from './project-picker.js';
import './principal-picker.js';
import './project-picker.js';
import { SYSTEM_DIRECT_USER_ONLY_ROLES } from './role-binding-utils.js';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/** Minimal role definition for the role selector. */
export interface AssignmentRoleDefinition {
  id: string;
  name: string;
  scopeType: string;
}

/** Form values emitted on change. */
export interface AssignmentFormValues {
  principalType: string;
  principalId: string;
  roleId: string;
  scopeType: string;
  scopeId: string;
  valid: boolean;
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

@customElement('scion-role-binding-assignment-form')
export class ScionRoleBindingAssignmentForm extends LitElement {
  // ── Configuration properties ──

  /** Available roles to select from. */
  @property({ type: Array }) roles: AssignmentRoleDefinition[] = [];

  /**
   * Lock the scope type (e.g. role-detail page knows its role's scope).
   * When set, the scope selector is hidden and this value is used.
   */
  @property() lockedScopeType = '';

  /**
   * Lock the role (e.g. role-detail page knows its role).
   * When set, the role selector is hidden and this value is used.
   */
  @property() lockedRoleId = '';

  /**
   * Lock the principal (e.g. effective-role-provenance has a fixed principal).
   * When set, the principal selector is replaced with a read-only display.
   */
  @property() lockedPrincipalType = '';

  /** Locked principal ID. */
  @property() lockedPrincipalId = '';

  /** Whether to disable agent principal type (for system-scope roles). */
  @property({ type: Boolean }) agentDisabled = false;

  /** Whether a mutation is in progress (disables controls). */
  @property({ type: Boolean }) disabled = false;

  // ── Form state ──

  @state() private _principalType = 'user';
  @state() private _principalId = '';
  @state() private _scopeType = 'system';
  @state() private _scopeId = '';
  @state() private _roleId = '';

  /** Validation warning message (e.g. group → direct-user-only role). */
  @state() private _validationWarning = '';

  static override styles = css`
    :host {
      display: block;
    }

    .form-group {
      margin-bottom: 0.75rem;
    }

    .form-group label {
      display: block;
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.25rem;
    }

    .locked-principal {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      background: var(--scion-bg-subtle, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .locked-principal sl-icon {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .validation-warning {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #b45309);
      padding: 0.5rem 0.75rem;
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.8125rem;
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-bottom: 0.75rem;
    }

    .agent-scope-note {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      display: flex;
      align-items: center;
      gap: 0.25rem;
      margin-top: 0.25rem;
      margin-bottom: 0.75rem;
    }

    .agent-scope-note sl-icon {
      font-size: 0.75rem;
    }
  `;

  // ---------------------------------------------------------------------------
  // Lifecycle
  // ---------------------------------------------------------------------------

  override connectedCallback(): void {
    super.connectedCallback();
    // Initialize from locked values if provided
    if (this.lockedScopeType) {
      this._scopeType = this.lockedScopeType;
    }
    if (this.lockedRoleId) {
      this._roleId = this.lockedRoleId;
    }
    if (this.lockedPrincipalType) {
      this._principalType = this.lockedPrincipalType;
    }
    if (this.lockedPrincipalId) {
      this._principalId = this.lockedPrincipalId;
    }
  }

  // ---------------------------------------------------------------------------
  // Public API
  // ---------------------------------------------------------------------------

  /** Reset the form to its initial state. */
  reset(): void {
    this._principalType = this.lockedPrincipalType || 'user';
    this._principalId = this.lockedPrincipalId || '';
    this._scopeType = this.lockedScopeType || 'system';
    this._scopeId = '';
    this._roleId = this.lockedRoleId || '';
    this._validationWarning = '';
  }

  /** Get the current form values. */
  getFormValues(): AssignmentFormValues {
    return {
      principalType: this.lockedPrincipalType || this._principalType,
      principalId: this.lockedPrincipalId || this._principalId,
      roleId: this.lockedRoleId || this._roleId,
      scopeType: this.lockedScopeType || this._scopeType,
      scopeId: this._scopeId,
      valid: this._isValid,
    };
  }

  // ---------------------------------------------------------------------------
  // Computed
  // ---------------------------------------------------------------------------

  private get _effectiveScopeType(): string {
    return this.lockedScopeType || this._scopeType;
  }

  /** Roles filtered by the current scope type and principal constraints. */
  private get _filteredRoles(): AssignmentRoleDefinition[] {
    let filtered = this.roles.filter((r) => r.scopeType === this._effectiveScopeType);

    // Groups cannot be assigned to direct-user-only roles
    const pType = this.lockedPrincipalType || this._principalType;
    if (pType === 'group') {
      filtered = filtered.filter((r) => !SYSTEM_DIRECT_USER_ONLY_ROLES.includes(r.name));
    }

    return filtered;
  }

  private get _isValid(): boolean {
    const principalId = this.lockedPrincipalId || this._principalId;
    const roleId = this.lockedRoleId || this._roleId;
    if (!principalId.trim()) return false;
    if (!roleId) return false;
    if (this._effectiveScopeType === 'project' && !this._scopeId.trim()) return false;
    return true;
  }

  // ---------------------------------------------------------------------------
  // Event helpers
  // ---------------------------------------------------------------------------

  private _emitChange(): void {
    this.dispatchEvent(
      new CustomEvent<AssignmentFormValues>('form-change', {
        detail: this.getFormValues(),
        bubbles: true,
        composed: true,
      })
    );
  }

  private _handleScopeChange(newScopeType: string): void {
    const oldScopeType = this._scopeType;
    this._scopeType = newScopeType;

    // Clear project value when scope changes away from project
    if (oldScopeType === 'project' && newScopeType !== 'project') {
      this._scopeId = '';
    }

    // Clear role if the current role is incompatible with the new scope
    if (this._roleId) {
      const currentRole = this.roles.find((r) => r.id === this._roleId);
      if (currentRole && currentRole.scopeType !== newScopeType) {
        this._roleId = '';
      }
    }

    this._updateValidation();
    this._emitChange();
  }

  private _handlePrincipalTypeChange(newType: string): void {
    this._principalType = newType;
    this._principalId = '';
    this._updateValidation();
    this._emitChange();
  }

  private _updateValidation(): void {
    const pType = this.lockedPrincipalType || this._principalType;
    if (pType === 'group' && this._roleId) {
      const role = this.roles.find((r) => r.id === this._roleId);
      if (role && SYSTEM_DIRECT_USER_ONLY_ROLES.includes(role.name)) {
        this._validationWarning = `"${role.name}" can only be assigned to individual users, not groups.`;
        this._roleId = '';
        this._emitChange();
        return;
      }
    }
    this._validationWarning = '';
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    return html`
      ${this._validationWarning
        ? html`
            <div class="validation-warning">
              <sl-icon name="exclamation-triangle"></sl-icon>
              ${this._validationWarning}
            </div>
          `
        : nothing}
      ${this._renderPrincipal()} ${this._renderScope()} ${this._renderProject()}
      ${this._renderRole()}
    `;
  }

  private _renderPrincipal() {
    // Locked principal: read-only display
    if (this.lockedPrincipalType && this.lockedPrincipalId) {
      return html`
        <div class="form-group">
          <label>Principal</label>
          <div class="locked-principal">
            <sl-icon
              name=${this.lockedPrincipalType === 'user' ? 'person' : 'cpu'}
            ></sl-icon>
            <span>${this.lockedPrincipalType}: ${this.lockedPrincipalId}</span>
            <sl-icon name="lock" style="margin-left: auto;"></sl-icon>
          </div>
        </div>
      `;
    }

    // Editable principal: type selector + picker
    return html`
      <div class="form-group">
        <sl-select
          label="Principal Type"
          .value=${this._principalType}
          ?disabled=${this.disabled}
          @sl-change=${(e: Event) => {
            this._handlePrincipalTypeChange((e.target as HTMLSelectElement).value);
          }}
        >
          <sl-option value="user">
            <sl-icon slot="prefix" name="person"></sl-icon>
            User
          </sl-option>
          <sl-option value="agent" ?disabled=${this.agentDisabled}>
            <sl-icon slot="prefix" name="cpu"></sl-icon>
            Agent${this.agentDisabled ? ' (project-scoped roles only)' : ''}
          </sl-option>
          <sl-option value="group">
            <sl-icon slot="prefix" name="diagram-3"></sl-icon>
            Group
          </sl-option>
        </sl-select>
      </div>
      <div class="form-group">
        <scion-principal-picker
          .principalType=${this._principalType as 'user' | 'agent' | 'group'}
          ?disabled=${this.disabled}
          @principal-change=${(e: CustomEvent<PrincipalChangeDetail>) => {
            this._principalId = e.detail.principalId;
            this._emitChange();
          }}
        ></scion-principal-picker>
      </div>
    `;
  }

  private _renderScope() {
    // When scope is locked (role-detail page), don't show the selector
    if (this.lockedScopeType) return nothing;

    return html`
      <div class="form-group">
        <sl-select
          label="Scope"
          .value=${this._scopeType}
          ?disabled=${this.disabled}
          @sl-change=${(e: Event) => {
            this._handleScopeChange((e.target as HTMLSelectElement).value);
          }}
        >
          <sl-option value="system">System</sl-option>
          <sl-option value="project">Project</sl-option>
        </sl-select>
      </div>
    `;
  }

  private _renderProject() {
    if (this._effectiveScopeType !== 'project') return nothing;

    return html`
      <div class="form-group">
        <scion-project-picker
          label="Project"
          .value=${this._scopeId}
          ?disabled=${this.disabled}
          @project-change=${(e: CustomEvent<ProjectChangeDetail>) => {
            this._scopeId = e.detail.projectId;
            this._emitChange();
          }}
        ></scion-project-picker>
      </div>
      ${(this.lockedPrincipalType || this._principalType) === 'agent' && !this.agentDisabled
        ? html`<p class="agent-scope-note">
            <sl-icon name="info-circle"></sl-icon>
            Agents are project-bound. This binding is effective only within the
            specified project scope.
          </p>`
        : nothing}
    `;
  }

  private _renderRole() {
    // When role is locked (role-detail page), don't show the selector
    if (this.lockedRoleId) return nothing;

    return html`
      <div class="form-group">
        <sl-select
          label="Role"
          .value=${this._roleId}
          ?disabled=${this.disabled}
          @sl-change=${(e: Event) => {
            this._roleId = (e.target as HTMLSelectElement).value;
            this._updateValidation();
            this._emitChange();
          }}
        >
          ${this._filteredRoles.length === 0
            ? html`<sl-option value="" disabled
                >No roles available for this scope</sl-option
              >`
            : this._filteredRoles.map(
                (role) => html`
                  <sl-option value=${role.id}>
                    ${role.name}
                    <small style="color: var(--scion-text-muted, #64748b)">
                      (${role.scopeType})
                    </small>
                  </sl-option>
                `
              )}
        </sl-select>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-role-binding-assignment-form': ScionRoleBindingAssignmentForm;
  }
}
