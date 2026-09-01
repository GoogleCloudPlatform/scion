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
 * Access Boundary Definition Summary (Step 6)
 *
 * Read-only review summary of all selections from steps 1-5.
 * Shows the complete definition before commit.
 * The actual commit/preview functionality is added in F4.
 */

import { LitElement, html, css } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property } from 'lit/decorators.js';

import type {
  ConstraintSubject,
  ConstraintScope,
  PermissionId,
  Iso8601,
  SubjectSelection,
} from '../../shared/access-boundaries.js';

export interface DefinitionSummaryData {
  name: string;
  purpose: string;
  subject: ConstraintSubject | null;
  subjectSelection: SubjectSelection;
  subjectDisplayLabel: string;
  scope: ConstraintScope | null;
  scopeDisplayLabel: string;
  retainedPermissions: PermissionId[];
  totalPermissionCount: number;
  notBefore: Iso8601 | undefined;
  expiresAt: Iso8601 | undefined;
}

@customElement('scion-access-boundary-definition-summary')
export class ScionAccessBoundaryDefinitionSummary extends LitElement {
  @property({ type: Object }) data: DefinitionSummaryData = {
    name: '',
    purpose: '',
    subject: null,
    subjectSelection: 'exact_user',
    subjectDisplayLabel: '',
    scope: null,
    scopeDisplayLabel: '',
    retainedPermissions: [],
    totalPermissionCount: 0,
    notBefore: undefined,
    expiresAt: undefined,
  };

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .summary {
        display: flex;
        flex-direction: column;
        gap: 1.25rem;
      }

      .summary-section {
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-lg, 0.75rem);
        overflow: hidden;
      }

      .section-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        padding: 0.625rem 1rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
      }

      .section-title {
        font-size: 0.8125rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
        display: flex;
        align-items: center;
        gap: 0.5rem;
      }

      .section-step {
        font-size: 0.6875rem;
        color: var(--scion-text-muted, #64748b);
        font-weight: 400;
      }

      .edit-link {
        font-size: 0.75rem;
        cursor: pointer;
        color: var(--sl-color-primary-600, #2563eb);
        text-decoration: none;
        border: none;
        background: none;
        font-family: inherit;
        padding: 0.25rem 0.5rem;
        min-height: 44px;
        min-width: 44px;
        display: inline-flex;
        align-items: center;
        justify-content: center;
        border-radius: var(--scion-radius, 0.5rem);
      }

      .edit-link:hover {
        text-decoration: underline;
      }

      .section-body {
        padding: 0.75rem 1rem;
      }

      .field {
        display: flex;
        flex-direction: column;
        gap: 0.125rem;
      }

      .field + .field {
        margin-top: 0.75rem;
      }

      .field-label {
        font-size: 0.75rem;
        font-weight: 600;
        color: var(--scion-text-muted, #64748b);
        text-transform: uppercase;
        letter-spacing: 0.025em;
      }

      .field-value {
        font-size: 0.875rem;
        color: var(--scion-text, #1e293b);
        overflow-wrap: anywhere;
      }

      .field-value.purpose {
        white-space: pre-wrap;
        line-height: 1.5;
        overflow-wrap: anywhere;
      }

      .field-value.mono {
        font-family: var(--sl-font-mono, monospace);
        font-size: 0.8125rem;
        overflow-wrap: anywhere;
      }

      .permission-stats {
        display: flex;
        gap: 1.5rem;
        flex-wrap: wrap;
      }

      .stat {
        display: flex;
        flex-direction: column;
        gap: 0.125rem;
      }

      .stat-value {
        font-size: 1.25rem;
        font-weight: 700;
      }

      .stat-value.retained {
        color: var(--sl-color-success-600, #16a34a);
      }

      .stat-value.removed {
        color: var(--sl-color-danger-600, #dc2626);
      }

      .stat-label {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }

      .missing-value {
        color: var(--sl-color-warning-600, #ca8a04);
        font-style: italic;
      }

      .preview-placeholder {
        padding: 1.5rem;
        text-align: center;
        color: var(--scion-text-muted, #64748b);
        font-size: 0.875rem;
        border: 1px dashed var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-lg, 0.75rem);
      }

      @media (max-width: 768px) {
        .section-header {
          flex-direction: column;
          align-items: flex-start;
          gap: 0.5rem;
        }

        .permission-stats {
          flex-direction: column;
          gap: 0.75rem;
        }

        .stat {
          flex-direction: row;
          gap: 0.5rem;
          align-items: baseline;
        }

        .stat-value {
          font-size: 1rem;
        }

        .field-value {
          word-break: break-word;
        }
      }

      @media (forced-colors: active) {
        .summary-section {
          border: 2px solid ButtonText;
        }

        .section-header {
          border-bottom: 2px solid ButtonText;
        }

        .edit-link {
          color: LinkText;
        }

        .edit-link:focus-visible {
          outline: 2px solid Highlight;
        }

        .stat-value.retained,
        .stat-value.removed {
          color: ButtonText;
        }
      }
    `,
  ];

  private emitNavigateToStep(step: number): void {
    this.dispatchEvent(
      new CustomEvent<number>('navigate-to-step', {
        detail: step,
        bubbles: true,
        composed: true,
      })
    );
  }

  private getSubjectDescription(): string {
    const d = this.data;
    if (!d.subject) return 'Not selected';

    switch (d.subjectSelection) {
      case 'exact_user':
        return `User: ${d.subjectDisplayLabel || (d.subject.kind === 'principal' ? (d.subject as { kind: 'principal'; principal: { id: string } }).principal.id : '')}`;
      case 'exact_agent':
        return `Agent: ${d.subjectDisplayLabel || (d.subject.kind === 'principal' ? (d.subject as { kind: 'principal'; principal: { id: string } }).principal.id : '')}`;
      case 'exact_group':
        return `Group (identity only): ${d.subjectDisplayLabel || (d.subject.kind === 'principal' ? (d.subject as { kind: 'principal'; principal: { id: string } }).principal.id : '')}`;
      case 'group_closure':
        return `Group closure (all members): ${d.subjectDisplayLabel || (d.subject.kind === 'group_closure' ? (d.subject as { kind: 'group_closure'; groupId: string }).groupId : '')}`;
      case 'all_principals':
        return 'All principals';
    }
  }

  private getScopeDescription(): string {
    const d = this.data;
    if (!d.scope) return 'Not selected';
    if (d.scope.type === 'system') return 'System-wide';
    return `Project: ${d.scopeDisplayLabel || d.scope.projectId}`;
  }

  private formatDatetime(iso: Iso8601 | undefined): string {
    if (!iso) return 'Not set';
    try {
      const date = new Date(iso);
      if (isNaN(date.getTime())) return iso;
      return date.toLocaleString(undefined, {
        dateStyle: 'medium',
        timeStyle: 'short',
      });
    } catch {
      return iso;
    }
  }

  override render() {
    const d = this.data;
    const removedCount = d.totalPermissionCount - d.retainedPermissions.length;

    return html`
      <div class="summary">
        <!-- Step 1: Name and Purpose -->
        <div class="summary-section">
          <div class="section-header">
            <span class="section-title">
              Details
              <span class="section-step">Step 1</span>
            </span>
            <button
              class="edit-link"
              aria-label="Edit name and purpose"
              @click=${() => this.emitNavigateToStep(1)}
            >
              Edit
            </button>
          </div>
          <div class="section-body">
            <div class="field">
              <span class="field-label">Name</span>
              <span class="field-value">
                ${d.name || html`<span class="missing-value">Not provided</span>`}
              </span>
            </div>
            <div class="field">
              <span class="field-label">Purpose</span>
              <span class="field-value purpose">
                ${d.purpose || html`<span class="missing-value">Not provided</span>`}
              </span>
            </div>
          </div>
        </div>

        <!-- Step 2: Subject -->
        <div class="summary-section">
          <div class="section-header">
            <span class="section-title">
              Subject
              <span class="section-step">Step 2</span>
            </span>
            <button
              class="edit-link"
              aria-label="Edit subject"
              @click=${() => this.emitNavigateToStep(2)}
            >
              Edit
            </button>
          </div>
          <div class="section-body">
            <div class="field">
              <span class="field-label">Applies to</span>
              <span class="field-value">
                ${d.subject
                  ? this.getSubjectDescription()
                  : html`<span class="missing-value">Not selected</span>`}
              </span>
            </div>
          </div>
        </div>

        <!-- Step 3: Scope -->
        <div class="summary-section">
          <div class="section-header">
            <span class="section-title">
              Scope
              <span class="section-step">Step 3</span>
            </span>
            <button
              class="edit-link"
              aria-label="Edit scope"
              @click=${() => this.emitNavigateToStep(3)}
            >
              Edit
            </button>
          </div>
          <div class="section-body">
            <div class="field">
              <span class="field-label">Scope</span>
              <span class="field-value">
                ${d.scope
                  ? this.getScopeDescription()
                  : html`<span class="missing-value">Not selected</span>`}
              </span>
            </div>
          </div>
        </div>

        <!-- Step 4: Permissions -->
        <div class="summary-section">
          <div class="section-header">
            <span class="section-title">
              Maximum Permissions
              <span class="section-step">Step 4</span>
            </span>
            <button
              class="edit-link"
              aria-label="Edit maximum permissions"
              @click=${() => this.emitNavigateToStep(4)}
            >
              Edit
            </button>
          </div>
          <div class="section-body">
            <div class="permission-stats">
              <div class="stat">
                <span class="stat-value">${d.totalPermissionCount}</span>
                <span class="stat-label">In registry</span>
              </div>
              <div class="stat">
                <span class="stat-value retained">${d.retainedPermissions.length}</span>
                <span class="stat-label">Retained</span>
              </div>
              <div class="stat">
                <span class="stat-value removed">${removedCount}</span>
                <span class="stat-label">Removed</span>
              </div>
            </div>
          </div>
        </div>

        <!-- Step 5: Schedule -->
        <div class="summary-section">
          <div class="section-header">
            <span class="section-title">
              Activation Window
              <span class="section-step">Step 5</span>
            </span>
            <button
              class="edit-link"
              aria-label="Edit activation window"
              @click=${() => this.emitNavigateToStep(5)}
            >
              Edit
            </button>
          </div>
          <div class="section-body">
            ${d.notBefore || d.expiresAt
              ? html`
                  <div class="field">
                    <span class="field-label">Activation start</span>
                    <span class="field-value">${this.formatDatetime(d.notBefore)}</span>
                  </div>
                  <div class="field">
                    <span class="field-label">Expiration</span>
                    <span class="field-value">${this.formatDatetime(d.expiresAt)}</span>
                  </div>
                `
              : html`
                  <div class="field">
                    <span class="field-value">
                      No activation window — effective immediately, no expiration.
                    </span>
                  </div>
                `}
          </div>
        </div>

        <!-- Impact preview is triggered via the "Preview impact" button in the editor navigation -->
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-access-boundary-definition-summary': ScionAccessBoundaryDefinitionSummary;
  }
}
