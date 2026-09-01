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
 * Group form dialog — edit mode.
 *
 * Opens an sl-dialog for editing an existing group. Fields:
 *   - Name (editable)
 *   - Slug (read-only, shown disabled with help text)
 *   - Description (blank means unchanged)
 *   - Owner (principal-picker, with persistent transfer warning)
 *   - Labels (JSON editor)
 *
 * On submit, builds a minimal PATCH body containing ONLY changed fields.
 * After save, dispatches 'group-updated' event so the detail page can refetch.
 *
 * NOTE: This file currently only implements edit mode. When G3 lands the
 * create-mode dialog, the two should be unified into a single component
 * that switches layout based on a `mode` property.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { AdminGroup, UpdateGroupRequest } from '../../shared/groups.js';
import { updateGroup, getGroup, GroupsApiError } from '../../client/groups-api.js';
import { showToast } from '../../utils/toast.js';
import './principal-picker.js';
import type { PrincipalChangeDetail } from './principal-picker.js';

/** Event detail emitted on successful group update. */
export interface GroupUpdatedDetail {
  group: AdminGroup;
}

@customElement('scion-group-form-dialog')
export class ScionGroupFormDialog extends LitElement {
  /** The group being edited. Must be set before opening. */
  @property({ type: Object }) group: AdminGroup | null = null;

  @state() private open = false;
  @state() private saving = false;
  @state() private errorMessage = '';

  // Form field state — initialised from the group when dialog opens.
  @state() private editName = '';
  @state() private editDescription = '';
  @state() private editOwnerId = '';
  @state() private editLabels = '';

  // Track original values for changed-fields-only PATCH.
  private originalName = '';
  private originalDescription = '';
  private originalOwnerId = '';
  private originalLabels = '';

  static override styles = css`
    :host {
      display: contents;
    }

    .form-field {
      margin-bottom: 1rem;
    }

    .form-field:last-child {
      margin-bottom: 0;
    }

    .help-text {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
    }

    .owner-warning {
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      padding: 0.625rem 0.75rem;
      margin-top: 0.5rem;
      background: var(--sl-color-warning-50, #fffbeb);
      border: 1px solid var(--sl-color-warning-200, #fde68a);
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.8125rem;
      color: var(--sl-color-warning-700, #b45309);
    }

    .owner-warning sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .error-banner {
      padding: 0.625rem 0.75rem;
      margin-bottom: 1rem;
      background: var(--sl-color-danger-50, #fef2f2);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.8125rem;
      color: var(--sl-color-danger-700, #b91c1c);
    }

    .slug-display {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      padding: 0.5rem 0.75rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .field-label {
      font-size: 0.875rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.25rem;
      display: block;
    }
  `;

  /** Open the dialog for editing. Resets form state from the group prop. */
  show(): void {
    if (!this.group) return;

    const g = this.group;
    this.editName = g.name;
    this.originalName = g.name;

    this.editDescription = g.description ?? '';
    this.originalDescription = g.description ?? '';

    this.editOwnerId = g.ownerId ?? '';
    this.originalOwnerId = g.ownerId ?? '';

    const labelsStr = g.labels ? JSON.stringify(g.labels, null, 2) : '{}';
    this.editLabels = labelsStr;
    this.originalLabels = labelsStr;

    this.errorMessage = '';
    this.saving = false;
    this.open = true;
  }

  /** Close the dialog without saving. */
  hide(): void {
    this.open = false;
  }

  /**
   * Build the PATCH body from only the fields that changed.
   * Returns null if nothing changed.
   */
  buildPatch(): UpdateGroupRequest | null {
    const patch: UpdateGroupRequest = {};
    let hasChanges = false;

    if (this.editName !== this.originalName) {
      patch.name = this.editName;
      hasChanges = true;
    }

    // Description: blank input means unchanged (design doc C1).
    if (this.editDescription !== this.originalDescription && this.editDescription !== '') {
      patch.description = this.editDescription;
      hasChanges = true;
    }

    if (this.editOwnerId !== this.originalOwnerId) {
      patch.ownerId = this.editOwnerId;
      hasChanges = true;
    }

    // Labels: compare JSON strings to detect changes.
    if (this.editLabels !== this.originalLabels) {
      try {
        patch.labels = JSON.parse(this.editLabels) as Record<string, string>;
        hasChanges = true;
      } catch {
        // Invalid JSON — will be caught by validation.
      }
    }

    return hasChanges ? patch : null;
  }

  private async handleSave(): Promise<void> {
    if (!this.group) return;

    // Validate name is non-empty.
    if (!this.editName.trim()) {
      this.errorMessage = 'Name is required.';
      return;
    }

    // Validate labels JSON.
    if (this.editLabels.trim()) {
      try {
        JSON.parse(this.editLabels);
      } catch {
        this.errorMessage = 'Labels must be valid JSON (e.g. {"key": "value"}).';
        return;
      }
    }

    const patch = this.buildPatch();
    if (!patch) {
      this.errorMessage = 'No changes to save.';
      return;
    }

    this.saving = true;
    this.errorMessage = '';

    try {
      // PATCH does not return _capabilities, so refetch after.
      await updateGroup(this.group.id, patch);
      const refreshed = await getGroup(this.group.id);

      showToast('Group updated', 'success');
      this.open = false;

      this.dispatchEvent(
        new CustomEvent<GroupUpdatedDetail>('group-updated', {
          detail: { group: refreshed },
          bubbles: true,
          composed: true,
        })
      );
    } catch (err) {
      if (err instanceof GroupsApiError) {
        this.errorMessage = err.message;
      } else {
        this.errorMessage = err instanceof Error ? err.message : 'Failed to update group';
      }
    } finally {
      this.saving = false;
    }
  }

  private handleOwnerChange(e: CustomEvent<PrincipalChangeDetail>): void {
    this.editOwnerId = e.detail.principalId;
  }

  private get ownerChanged(): boolean {
    return this.editOwnerId !== this.originalOwnerId && this.editOwnerId !== '';
  }

  override render() {
    return html`
      <sl-dialog
        label="Edit Group"
        ?open=${this.open}
        @sl-request-close=${(e: Event) => {
          if (this.saving) {
            e.preventDefault();
            return;
          }
          this.open = false;
        }}
      >
        ${this.errorMessage ? html`<div class="error-banner">${this.errorMessage}</div>` : nothing}

        <div class="form-field">
          <sl-input
            label="Name"
            value=${this.editName}
            ?disabled=${this.saving}
            @sl-input=${(e: Event) => {
              this.editName = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>
        </div>

        <div class="form-field">
          <span class="field-label">Slug</span>
          <div class="slug-display">${this.group?.slug ?? ''}</div>
          <div class="help-text">Slugs are permanent and cannot be changed.</div>
        </div>

        <div class="form-field">
          <sl-textarea
            label="Description"
            value=${this.editDescription}
            ?disabled=${this.saving}
            rows="3"
            @sl-input=${(e: Event) => {
              this.editDescription = (e.target as HTMLTextAreaElement).value;
            }}
          ></sl-textarea>
          <div class="help-text">Leave blank to keep the current description.</div>
        </div>

        <div class="form-field">
          <scion-principal-picker
            principalType="user"
            label="Owner"
            value=${this.editOwnerId}
            ?disabled=${this.saving}
            @principal-change=${(e: CustomEvent<PrincipalChangeDetail>) =>
              this.handleOwnerChange(e)}
          ></scion-principal-picker>
          ${this.ownerChanged
            ? html`
                <div class="owner-warning">
                  <sl-icon name="exclamation-triangle"></sl-icon>
                  <span
                    >The new owner gains full control of this group (edit, delete,
                    membership).</span
                  >
                </div>
              `
            : nothing}
        </div>

        <div class="form-field">
          <sl-textarea
            label="Labels (JSON)"
            value=${this.editLabels}
            ?disabled=${this.saving}
            rows="3"
            style="font-family: var(--scion-font-mono, monospace); font-size: 0.8125rem;"
            @sl-input=${(e: Event) => {
              this.editLabels = (e.target as HTMLTextAreaElement).value;
            }}
          ></sl-textarea>
          <div class="help-text">Key-value pairs as JSON. Send {} to clear all labels.</div>
        </div>

        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.saving}
          @click=${() => {
            this.open = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.saving}
          ?disabled=${this.saving}
          @click=${() => void this.handleSave()}
          >Save Changes</sl-button
        >
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-group-form-dialog': ScionGroupFormDialog;
  }
}
