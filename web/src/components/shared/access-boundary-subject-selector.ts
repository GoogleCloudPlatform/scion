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
 * Access Boundary Subject Selector (Step 2)
 *
 * Radio card selection for the five subject kinds:
 * - Exact user (search autocomplete)
 * - Exact agent (search autocomplete)
 * - Exact group (search autocomplete, group identity only)
 * - Group closure (search autocomplete, direct and nested members)
 * - All principals (no search needed)
 *
 * Changing subject kind MUST clear any previously selected ID.
 * Always shows a visible summary sentence of the current selection.
 * System scope + all principals shows an inline warning.
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property } from 'lit/decorators.js';
import { classMap } from 'lit/directives/class-map.js';

import type { ConstraintSubject, SubjectSelection } from '../../shared/access-boundaries.js';

import './principal-picker.js';
import type { PrincipalChangeDetail } from './principal-picker.js';

export interface SubjectChangeDetail {
  subject: ConstraintSubject | null;
  selection: SubjectSelection;
  displayLabel: string;
}

interface SubjectOption {
  value: SubjectSelection;
  label: string;
  description: string;
  icon: string;
  needsSearch: boolean;
  searchType: 'user' | 'agent' | 'group' | null;
}

const SUBJECT_OPTIONS: SubjectOption[] = [
  {
    value: 'exact_user',
    label: 'Exact user',
    description: 'A single user account',
    icon: 'person',
    needsSearch: true,
    searchType: 'user',
  },
  {
    value: 'exact_agent',
    label: 'Exact agent',
    description: 'A single agent identity',
    icon: 'robot',
    needsSearch: true,
    searchType: 'agent',
  },
  {
    value: 'exact_group',
    label: 'Exact group',
    description: 'Group identity only. Members are not included.',
    icon: 'people',
    needsSearch: true,
    searchType: 'group',
  },
  {
    value: 'group_closure',
    label: 'Group closure',
    description: 'Direct and nested members. Membership changes alter coverage live.',
    icon: 'diagram-3',
    needsSearch: true,
    searchType: 'group',
  },
  {
    value: 'all_principals',
    label: 'All principals',
    description: 'Every supported principal in scope. Requires heightened review.',
    icon: 'globe',
    needsSearch: false,
    searchType: null,
  },
];

@customElement('scion-access-boundary-subject-selector')
export class ScionAccessBoundarySubjectSelector extends LitElement {
  /** Current selection kind. */
  @property() selection: SubjectSelection = 'exact_user';

  /** Selected principal/group ID (empty when no selection has been made). */
  @property() selectedId = '';

  /** Display label for the selected principal. */
  @property() selectedLabel = '';

  /** Whether the scope is system-wide (for showing "all principals" warning). */
  @property({ type: Boolean }) isSystemScope = false;

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .subject-selector {
        display: flex;
        flex-direction: column;
        gap: 1.5rem;
      }

      .option-cards {
        display: flex;
        flex-direction: column;
        gap: 0.5rem;
      }

      .option-card {
        display: flex;
        align-items: flex-start;
        gap: 0.75rem;
        padding: 0.875rem 1rem;
        min-height: 44px;
        border: 2px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-lg, 0.75rem);
        cursor: pointer;
        transition:
          border-color 0.15s ease,
          background-color 0.15s ease;
        background: var(--scion-surface, #ffffff);
      }

      .option-card:hover {
        border-color: var(--sl-color-primary-300, #93c5fd);
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .option-card:focus-visible {
        outline: 2px solid var(--sl-color-primary-600, #2563eb);
        outline-offset: 2px;
      }

      .option-card.selected {
        border-color: var(--sl-color-primary-600, #2563eb);
        background: var(--sl-color-primary-50, #eff6ff);
      }

      .option-radio {
        flex-shrink: 0;
        margin-top: 0.125rem;
      }

      .option-icon {
        flex-shrink: 0;
        font-size: 1.125rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.0625rem;
      }

      .option-card.selected .option-icon {
        color: var(--sl-color-primary-600, #2563eb);
      }

      .option-content {
        flex: 1;
        min-width: 0;
      }

      .option-label {
        font-size: 0.875rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
      }

      .option-description {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.125rem;
      }

      .search-section {
        padding: 0 0.25rem;
      }

      .summary {
        padding: 0.75rem 1rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        border-radius: var(--scion-radius, 0.5rem);
        font-size: 0.875rem;
        color: var(--scion-text, #1e293b);
      }

      .summary strong {
        font-weight: 600;
      }

      .warning {
        margin-top: 0;
      }

      fieldset {
        border: none;
        margin: 0;
        padding: 0;
      }

      @media (max-width: 768px) {
        .option-cards {
          flex-direction: column;
        }

        .option-card {
          width: 100%;
        }

        .search-section {
          padding: 0;
        }
      }

      @media (forced-colors: active) {
        .option-card {
          border: 2px solid ButtonText;
        }

        .option-card.selected {
          border-color: Highlight;
          background: none;
        }

        .option-card:focus-visible {
          outline: 2px solid Highlight;
        }
      }

      @media (prefers-reduced-motion: reduce) {
        .option-card {
          transition: none;
        }
      }
    `,
  ];

  private handleOptionSelect(value: SubjectSelection): void {
    if (value !== this.selection) {
      // Changing subject kind MUST clear any previously selected ID
      this.selectedId = '';
      this.selectedLabel = '';
      this.selection = value;
    }
    this.emitChange();
  }

  private handlePrincipalChange(e: CustomEvent<PrincipalChangeDetail>): void {
    this.selectedId = e.detail.principalId;
    this.selectedLabel = e.detail.displayLabel;
    this.emitChange();
  }

  private buildSubject(): ConstraintSubject | null {
    switch (this.selection) {
      case 'exact_user':
        return this.selectedId
          ? { kind: 'principal', principal: { type: 'user', id: this.selectedId } }
          : null;
      case 'exact_agent':
        return this.selectedId
          ? { kind: 'principal', principal: { type: 'agent', id: this.selectedId } }
          : null;
      case 'exact_group':
        return this.selectedId
          ? { kind: 'principal', principal: { type: 'group', id: this.selectedId } }
          : null;
      case 'group_closure':
        return this.selectedId ? { kind: 'group_closure', groupId: this.selectedId } : null;
      case 'all_principals':
        return { kind: 'all_principals' };
    }
  }

  private emitChange(): void {
    const subject = this.buildSubject();
    this.dispatchEvent(
      new CustomEvent<SubjectChangeDetail>('subject-change', {
        detail: {
          subject,
          selection: this.selection,
          displayLabel: this.selectedLabel || this.selectedId,
        },
        bubbles: true,
        composed: true,
      })
    );
  }

  private getSummaryText(): string {
    const option = SUBJECT_OPTIONS.find((o) => o.value === this.selection);
    if (!option) return '';

    if (this.selection === 'all_principals') {
      return 'This access boundary will apply to all principals in the selected scope.';
    }

    if (!this.selectedId) {
      return `Select ${option.label === 'Group closure' ? 'a group' : option.label === 'Exact group' ? 'a group' : option.label === 'Exact user' ? 'a user' : 'an agent'} to continue.`;
    }

    const label = this.selectedLabel || this.selectedId;
    switch (this.selection) {
      case 'exact_user':
        return `This access boundary will apply to user "${label}".`;
      case 'exact_agent':
        return `This access boundary will apply to agent "${label}".`;
      case 'exact_group':
        return `This access boundary will apply to group "${label}" (identity only, not members).`;
      case 'group_closure':
        return `This access boundary will apply to all direct and nested members of group "${label}".`;
      default:
        return '';
    }
  }

  private getSelectedOption(): SubjectOption | undefined {
    return SUBJECT_OPTIONS.find((o) => o.value === this.selection);
  }

  override render() {
    const selectedOption = this.getSelectedOption();

    return html`
      <div class="subject-selector">
        <fieldset>
          <legend class="sr-only">Subject type</legend>
          <div class="option-cards" role="radiogroup" aria-label="Subject type">
            ${SUBJECT_OPTIONS.map((option) => {
              const isSelected = option.value === this.selection;
              return html`
                <div
                  class=${classMap({ 'option-card': true, selected: isSelected })}
                  role="radio"
                  aria-checked=${isSelected ? 'true' : 'false'}
                  tabindex="0"
                  @click=${() => this.handleOptionSelect(option.value)}
                  @keydown=${(e: KeyboardEvent) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault();
                      this.handleOptionSelect(option.value);
                    }
                  }}
                >
                  <sl-icon class="option-icon" name=${option.icon}></sl-icon>
                  <div class="option-content">
                    <div class="option-label">${option.label}</div>
                    <div class="option-description">${option.description}</div>
                  </div>
                </div>
              `;
            })}
          </div>
        </fieldset>

        ${selectedOption?.needsSearch && selectedOption.searchType
          ? html`
              <div class="search-section">
                <scion-principal-picker
                  .principalType=${selectedOption.searchType}
                  .value=${this.selectedId}
                  label=${selectedOption.searchType === 'user'
                    ? 'Search for user'
                    : selectedOption.searchType === 'agent'
                      ? 'Enter agent ID'
                      : 'Search for group'}
                  @principal-change=${(e: CustomEvent<PrincipalChangeDetail>) =>
                    this.handlePrincipalChange(e)}
                ></scion-principal-picker>
              </div>
            `
          : nothing}
        ${this.selection === 'all_principals' && this.isSystemScope
          ? html`
              <sl-alert variant="warning" open class="warning">
                <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                <strong>System-wide all-principals boundary.</strong> This will constrain
                permissions for every principal across the entire system. This configuration
                requires heightened review before it can be committed.
              </sl-alert>
            `
          : nothing}

        <div class="summary">${this.getSummaryText()}</div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-access-boundary-subject-selector': ScionAccessBoundarySubjectSelector;
  }
}
