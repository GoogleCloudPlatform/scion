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
 * Unified group form dialog — create and edit modes.
 *
 * `scion-group-form-dialog` custom element.
 *
 * **Create mode** (`mode="create"`):
 *   Fields: name (required, autofocus), slug (live-slugified, detach on
 *   manual edit, reject project: prefix), description, label key/value editor.
 *   On submit, calls `createGroup()` and dispatches `group-saved`.
 *
 * **Edit mode** (`mode="edit"`):
 *   Requires `.group` property. Fields: name (editable), slug (read-only),
 *   description, owner (principal-picker with transfer warning),
 *   labels (key/value editor pre-populated from group).
 *   Builds a minimal PATCH body with only changed fields.
 *   On submit, calls `updateGroup()` + `getGroup()` and dispatches `group-updated`.
 *
 * Events:
 * - `group-saved` (detail: AdminGroup) — emitted on successful create.
 * - `group-updated` (detail: { group: AdminGroup }) — emitted on successful edit.
 * - `group-form-cancel` — emitted when the user cancels.
 *
 * Errors: conflict_slug -> inline slug error + focus; validation ->
 * field-attributed; otherwise dialog banner (role="alert").
 * Dialog stays open with input preserved on error; close suppressed
 * while submit in flight.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { AdminGroup, UpdateGroupRequest } from '../../shared/groups.js';
import { createGroup, updateGroup, getGroup, GroupsApiError } from '../../client/groups-api.js';
import { showToast } from '../../utils/toast.js';
import './principal-picker.js';
import type { PrincipalChangeDetail } from './principal-picker.js';

/* -------------------------------------------------------------------------- */
/* Slugify helper                                                             */
/* -------------------------------------------------------------------------- */

/**
 * Convert a name string to a URL-safe slug.
 * Lowercases, replaces non-alphanumeric runs with dashes, trims dashes.
 */
export function slugify(value: string): string {
  return value
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '');
}

/* -------------------------------------------------------------------------- */
/* Exported event detail types                                                */
/* -------------------------------------------------------------------------- */

/** Event detail emitted on successful group update. */
export interface GroupUpdatedDetail {
  group: AdminGroup;
}

/* -------------------------------------------------------------------------- */
/* Component                                                                  */
/* -------------------------------------------------------------------------- */

@customElement('scion-group-form-dialog')
export class ScionGroupFormDialog extends LitElement {
  /** Dialog mode — determines form layout, API call, and emitted events. */
  @property({ type: String }) mode: 'create' | 'edit' = 'create';

  /** The group being edited (required for edit mode). */
  @property({ type: Object }) group: AdminGroup | null = null;

  /** Whether the dialog is open (used by create mode via template binding). */
  @property({ type: Boolean }) open = false;

  // --- Form fields ---
  @state() private formName = '';
  @state() private formSlug = '';
  @state() private formDescription = '';
  @state() private formLabels: Array<{ key: string; value: string }> = [];

  // --- Edit-mode owner ---
  @state() private editOwnerId = '';

  // --- Slug auto-sync (create mode) ---
  @state() private slugDetached = false;

  // --- Errors ---
  @state() private nameError = '';
  @state() private slugError = '';
  @state() private bannerError = '';
  @state() private labelErrors: Map<number, string> = new Map();

  // --- Submit state ---
  @state() private submitting = false;

  // --- Original values for edit-mode change detection ---
  private originalName = '';
  private originalDescription = '';
  private originalOwnerId = '';
  private originalLabels: Array<{ key: string; value: string }> = [];

  static override styles = css`
    :host {
      display: contents;
    }

    .form-group {
      margin-bottom: 1rem;
    }

    .form-group:last-child {
      margin-bottom: 0;
    }

    .helper-copy {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .banner-error {
      margin-bottom: 1rem;
    }

    .help-text {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
    }

    /* Labels editor */
    .labels-section {
      margin-top: 0.5rem;
    }

    .labels-section h4 {
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .label-row {
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      margin-bottom: 0.5rem;
    }

    .label-row sl-input {
      flex: 1;
    }

    .label-error {
      font-size: 0.75rem;
      color: var(--sl-color-danger-600, #dc2626);
      margin-top: 0.25rem;
    }

    /* Owner transfer warning (edit mode) */
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

    /* Read-only slug display (edit mode) */
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

  // ---------------------------------------------------------------------------
  // Lifecycle
  // ---------------------------------------------------------------------------

  override updated(changed: Map<string | number | symbol, unknown>): void {
    if (changed.has('open') && this.open) {
      this.resetForm();
    }
  }

  // ---------------------------------------------------------------------------
  // Imperative open/close (edit mode)
  // ---------------------------------------------------------------------------

  /** Open the dialog for editing. Resets form state from the group prop. */
  show(): void {
    if (this.mode === 'edit' && !this.group) return;
    this.resetForm();
    this.open = true;
  }

  /** Close the dialog without saving. */
  hide(): void {
    this.open = false;
  }

  // ---------------------------------------------------------------------------
  // Form management
  // ---------------------------------------------------------------------------

  private resetForm(): void {
    this.clearErrors();
    this.submitting = false;

    if (this.mode === 'edit' && this.group) {
      const g = this.group;
      this.formName = g.name;
      this.originalName = g.name;

      this.formSlug = g.slug ?? '';

      this.formDescription = g.description ?? '';
      this.originalDescription = g.description ?? '';

      this.editOwnerId = g.ownerId ?? '';
      this.originalOwnerId = g.ownerId ?? '';

      // Convert group labels to key/value array
      const labels = g.labels ?? {};
      this.formLabels = Object.entries(labels).map(([key, value]) => ({ key, value }));
      this.originalLabels = this.formLabels.map((l) => ({ ...l }));
    } else {
      this.formName = '';
      this.formSlug = '';
      this.formDescription = '';
      this.formLabels = [];
      this.editOwnerId = '';
      this.slugDetached = false;
      this.originalName = '';
      this.originalDescription = '';
      this.originalOwnerId = '';
      this.originalLabels = [];
    }
  }

  private clearErrors(): void {
    this.nameError = '';
    this.slugError = '';
    this.bannerError = '';
    this.labelErrors = new Map();
  }

  // ---------------------------------------------------------------------------
  // Name -> slug sync (create mode)
  // ---------------------------------------------------------------------------

  private onNameInput(e: Event): void {
    this.formName = (e.target as HTMLInputElement).value;
    if (this.mode === 'create' && !this.slugDetached) {
      this.formSlug = slugify(this.formName);
    }
    if (this.nameError) this.nameError = '';
  }

  private onSlugInput(e: Event): void {
    const value = (e.target as HTMLInputElement).value;
    this.formSlug = value;
    this.slugDetached = true;
    // Inline validation: reject reserved project: prefix immediately.
    if (this.mode === 'create' && value.startsWith('project:')) {
      this.slugError =
        'Slugs cannot start with "project:" — that prefix is reserved for system-managed groups.';
    } else if (this.slugError) {
      this.slugError = '';
    }
  }

  // ---------------------------------------------------------------------------
  // Label editor
  // ---------------------------------------------------------------------------

  private addLabel(): void {
    this.formLabels = [...this.formLabels, { key: '', value: '' }];
  }

  private updateLabelKey(index: number, value: string): void {
    const updated = [...this.formLabels];
    updated[index] = { ...updated[index], key: value };
    this.formLabels = updated;
    // Clear error for this row
    if (this.labelErrors.has(index)) {
      const next = new Map(this.labelErrors);
      next.delete(index);
      this.labelErrors = next;
    }
  }

  private updateLabelValue(index: number, value: string): void {
    const updated = [...this.formLabels];
    updated[index] = { ...updated[index], value: value };
    this.formLabels = updated;
  }

  private removeLabel(index: number): void {
    this.formLabels = this.formLabels.filter((_, i) => i !== index);
    // Rebuild label errors with shifted indices
    const next = new Map<number, string>();
    this.labelErrors.forEach((err, i) => {
      if (i < index) next.set(i, err);
      else if (i > index) next.set(i - 1, err);
    });
    this.labelErrors = next;
  }

  // ---------------------------------------------------------------------------
  // Owner change (edit mode)
  // ---------------------------------------------------------------------------

  private handleOwnerChange(e: CustomEvent<PrincipalChangeDetail>): void {
    this.editOwnerId = e.detail.principalId;
  }

  private get ownerChanged(): boolean {
    return this.editOwnerId !== this.originalOwnerId && this.editOwnerId !== '';
  }

  // ---------------------------------------------------------------------------
  // Validation
  // ---------------------------------------------------------------------------

  /** Client-side validation. Returns true if valid. */
  validate(): boolean {
    this.clearErrors();
    let valid = true;

    const trimmedName = this.formName.trim();
    if (!trimmedName) {
      this.nameError = 'Name is required.';
      valid = false;
    }

    if (this.mode === 'create') {
      // Reject project: prefix on slug
      if (this.formSlug.startsWith('project:')) {
        this.slugError =
          'Slugs cannot start with "project:" — that prefix is reserved for system-managed groups.';
        valid = false;
      }
    }

    // Validate labels: keys must be non-empty and unique
    const seenKeys = new Set<string>();
    const nextLabelErrors = new Map<number, string>();
    for (let i = 0; i < this.formLabels.length; i++) {
      const key = this.formLabels[i].key.trim();
      if (!key) {
        nextLabelErrors.set(i, 'Label key cannot be empty.');
        valid = false;
      } else if (seenKeys.has(key)) {
        nextLabelErrors.set(i, `Duplicate label key "${key}".`);
        valid = false;
      } else {
        seenKeys.add(key);
      }
    }
    if (nextLabelErrors.size > 0) {
      this.labelErrors = nextLabelErrors;
    }

    return valid;
  }

  // ---------------------------------------------------------------------------
  // Edit-mode PATCH builder
  // ---------------------------------------------------------------------------

  /**
   * Build the PATCH body from only the fields that changed.
   * Returns null if nothing changed.
   */
  buildPatch(): UpdateGroupRequest | null {
    const patch: UpdateGroupRequest = {};
    let hasChanges = false;

    if (this.formName !== this.originalName) {
      patch.name = this.formName;
      hasChanges = true;
    }

    // Description: blank input means unchanged (design doc C1).
    if (this.formDescription !== this.originalDescription && this.formDescription !== '') {
      patch.description = this.formDescription;
      hasChanges = true;
    }

    if (this.editOwnerId !== this.originalOwnerId) {
      patch.ownerId = this.editOwnerId;
      hasChanges = true;
    }

    // Labels: compare against original (order-independent)
    const labelsMap = this.buildLabelsMap();
    const originalMap = this.buildLabelsMapFrom(this.originalLabels);
    const keysMap = Object.keys(labelsMap);
    const keysOriginal = Object.keys(originalMap);
    const labelsChanged =
      keysMap.length !== keysOriginal.length ||
      keysMap.some((key) => labelsMap[key] !== originalMap[key]);
    if (labelsChanged) {
      patch.labels = labelsMap;
      hasChanges = true;
    }

    return hasChanges ? patch : null;
  }

  private buildLabelsMap(): Record<string, string> {
    const labels: Record<string, string> = {};
    for (const label of this.formLabels) {
      const key = label.key.trim();
      if (key) {
        labels[key] = label.value;
      }
    }
    return labels;
  }

  private buildLabelsMapFrom(
    entries: Array<{ key: string; value: string }>
  ): Record<string, string> {
    const labels: Record<string, string> = {};
    for (const label of entries) {
      const key = label.key.trim();
      if (key) {
        labels[key] = label.value;
      }
    }
    return labels;
  }

  // ---------------------------------------------------------------------------
  // Submit
  // ---------------------------------------------------------------------------

  private async handleSubmit(): Promise<void> {
    if (!this.validate()) {
      this.focusFirstError();
      return;
    }

    if (this.mode === 'edit') {
      await this.handleEditSubmit();
    } else {
      await this.handleCreateSubmit();
    }
  }

  private async handleCreateSubmit(): Promise<void> {
    this.submitting = true;
    this.bannerError = '';

    try {
      const labels = this.buildLabelsMap();

      const req: import('../../shared/groups.js').CreateGroupRequest = {
        name: this.formName.trim(),
      };
      const trimmedSlug = this.formSlug.trim();
      if (trimmedSlug) req.slug = trimmedSlug;
      const trimmedDesc = this.formDescription.trim();
      if (trimmedDesc) req.description = trimmedDesc;
      if (Object.keys(labels).length > 0) req.labels = labels;

      const group = await createGroup(req);

      this.dispatchEvent(
        new CustomEvent<AdminGroup>('group-saved', {
          detail: group,
          bubbles: true,
          composed: true,
        })
      );
    } catch (err) {
      this.handleSubmitError(err);
    } finally {
      this.submitting = false;
    }
  }

  private async handleEditSubmit(): Promise<void> {
    if (!this.group) return;

    const patch = this.buildPatch();
    if (!patch) {
      this.bannerError = 'No changes to save.';
      return;
    }

    this.submitting = true;
    this.bannerError = '';

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
      this.handleSubmitError(err);
    } finally {
      this.submitting = false;
    }
  }

  private handleSubmitError(err: unknown): void {
    if (err instanceof GroupsApiError) {
      switch (err.kind) {
        case 'conflict_slug':
          this.slugError = 'A group with this slug already exists. Choose a different slug.';
          this.focusSlugField();
          break;
        case 'validation':
          this.bannerError = err.message;
          break;
        default:
          this.bannerError = err.message;
          break;
      }
    } else {
      this.bannerError = err instanceof Error ? err.message : 'An unexpected error occurred.';
    }
  }

  private focusFirstError(): void {
    requestAnimationFrame(() => {
      const errInput = this.shadowRoot?.querySelector<HTMLElement>('[data-error="true"]');
      if (errInput) {
        errInput.focus();
      }
    });
  }

  /** Focus the name input after the dialog opens. */
  private focusNameInput(): void {
    requestAnimationFrame(() => {
      const nameInput = this.shadowRoot?.querySelector<HTMLElement>('#name-input');
      if (nameInput) {
        nameInput.focus();
      }
    });
  }

  private focusSlugField(): void {
    requestAnimationFrame(() => {
      const slugInput = this.shadowRoot?.querySelector<HTMLElement>('#slug-input');
      if (slugInput) {
        slugInput.focus();
      }
    });
  }

  // ---------------------------------------------------------------------------
  // Dialog close handling
  // ---------------------------------------------------------------------------

  private onRequestClose(e: Event): void {
    if (this.submitting) {
      e.preventDefault();
      return;
    }
    this.open = false;
    this.dispatchEvent(new CustomEvent('group-form-cancel', { bubbles: true, composed: true }));
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    if (!this.open) return nothing;

    const dialogTitle = this.mode === 'create' ? 'Create group' : 'Edit group';

    return html`
      <sl-dialog
        label=${dialogTitle}
        open
        @sl-request-close=${(e: Event) => this.onRequestClose(e)}
        @sl-after-show=${() => this.focusNameInput()}
      >
        ${this.mode === 'create'
          ? html`
              <p class="helper-copy">
                You will be added as the group's owner. Groups can be granted roles and used in
                access boundaries.
              </p>
            `
          : nothing}
        ${this.bannerError
          ? html`
              <sl-alert class="banner-error" variant="danger" open role="alert">
                <sl-icon slot="icon" name="exclamation-octagon" aria-hidden="true"></sl-icon>
                ${this.bannerError}
              </sl-alert>
            `
          : nothing}

        <div class="form-group">
          <sl-input
            id="name-input"
            label="Name"
            placeholder=${this.mode === 'create' ? 'e.g., Platform Engineers' : ''}
            .value=${this.formName}
            @sl-input=${(e: Event) => this.onNameInput(e)}
            ?data-error=${!!this.nameError}
            ?disabled=${this.submitting}
            required
            ?autofocus=${this.mode === 'create'}
            help-text=${this.nameError || ''}
          ></sl-input>
        </div>

        ${this.mode === 'create' ? this.renderCreateSlug() : this.renderEditSlug()}

        <div class="form-group">
          <sl-textarea
            label="Description"
            placeholder=${this.mode === 'create' ? 'Optional description' : ''}
            .value=${this.formDescription}
            ?disabled=${this.submitting}
            @sl-input=${(e: Event) => {
              this.formDescription = (e.target as HTMLTextAreaElement).value;
            }}
            rows="2"
          ></sl-textarea>
          ${this.mode === 'edit'
            ? html`<div class="help-text">Leave blank to keep the current description.</div>`
            : nothing}
        </div>

        ${this.mode === 'edit' ? this.renderOwnerPicker() : nothing} ${this.renderLabelEditor()}

        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.submitting}
          @click=${() => {
            this.open = false;
            this.dispatchEvent(
              new CustomEvent('group-form-cancel', { bubbles: true, composed: true })
            );
          }}
        >
          Cancel
        </sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.submitting}
          ?disabled=${this.mode === 'create'
            ? !this.formName.trim() || this.submitting
            : this.submitting}
          @click=${() => void this.handleSubmit()}
        >
          ${this.mode === 'create' ? 'Create group' : 'Save changes'}
        </sl-button>
      </sl-dialog>
    `;
  }

  // ---------------------------------------------------------------------------
  // Slug field renderers
  // ---------------------------------------------------------------------------

  private renderCreateSlug() {
    return html`
      <div class="form-group">
        <sl-input
          id="slug-input"
          label="Slug"
          placeholder="auto-generated from name"
          .value=${this.formSlug}
          @sl-input=${(e: Event) => this.onSlugInput(e)}
          ?data-error=${!!this.slugError}
          ?disabled=${this.submitting}
          help-text=${this.slugError ||
          'URL-safe identifier. Auto-filled from name; edit to customize. Slugs are permanent after creation.'}
          style="font-family: var(--scion-font-mono, monospace);"
        ></sl-input>
      </div>
    `;
  }

  private renderEditSlug() {
    return html`
      <div class="form-group">
        <span class="field-label">Slug</span>
        <div class="slug-display">${this.group?.slug ?? ''}</div>
        <div class="help-text">Slugs are permanent and cannot be changed.</div>
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Owner picker (edit mode)
  // ---------------------------------------------------------------------------

  private renderOwnerPicker() {
    return html`
      <div class="form-group">
        <scion-principal-picker
          principalType="user"
          label="Owner"
          value=${this.editOwnerId}
          ?disabled=${this.submitting}
          @principal-change=${(e: CustomEvent<PrincipalChangeDetail>) => this.handleOwnerChange(e)}
        ></scion-principal-picker>
        ${this.ownerChanged
          ? html`
              <div class="owner-warning" role="alert">
                <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
                <span
                  >The new owner gains full control of this group (edit, delete, membership).</span
                >
              </div>
            `
          : nothing}
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Label editor
  // ---------------------------------------------------------------------------

  private renderLabelEditor() {
    return html`
      <div class="labels-section">
        <h4>Labels</h4>
        ${this.formLabels.map((label, index) => {
          const error = this.labelErrors.get(index);
          return html`
            <div class="label-row">
              <sl-input
                size="small"
                placeholder="Key"
                .value=${label.key}
                ?disabled=${this.submitting}
                @sl-input=${(e: Event) =>
                  this.updateLabelKey(index, (e.target as HTMLInputElement).value)}
                ?data-error=${!!error}
              ></sl-input>
              <sl-input
                size="small"
                placeholder="Value"
                .value=${label.value}
                ?disabled=${this.submitting}
                @sl-input=${(e: Event) =>
                  this.updateLabelValue(index, (e.target as HTMLInputElement).value)}
              ></sl-input>
              <sl-icon-button
                name="x-lg"
                label="Remove label"
                ?disabled=${this.submitting}
                @click=${() => this.removeLabel(index)}
              ></sl-icon-button>
            </div>
            ${error ? html`<div class="label-error" role="alert">${error}</div>` : nothing}
          `;
        })}
        <sl-button
          variant="text"
          size="small"
          ?disabled=${this.submitting}
          @click=${() => this.addLabel()}
        >
          <sl-icon slot="prefix" name="plus-lg" aria-hidden="true"></sl-icon>
          Add label
        </sl-button>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-group-form-dialog': ScionGroupFormDialog;
  }
}
