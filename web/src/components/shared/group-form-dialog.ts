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
 * Group form dialog — create mode (G3).
 *
 * `scion-group-form-dialog` custom element.
 * Mode `create` in this WP; mode `edit` will be added by G4.
 *
 * Fields: name (required, autofocus), slug (live-slugified, detach on
 * manual edit, reject project: prefix), description, label key/value editor.
 *
 * Events:
 * - `group-saved` (detail: AdminGroup) — emitted on successful create.
 * - `group-form-cancel` — emitted when the user cancels.
 *
 * Errors: conflict_slug → inline slug error + focus; validation →
 * field-attributed; otherwise dialog banner (role="alert").
 * Dialog stays open with input preserved on error; close suppressed
 * while submit in flight.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { createGroup, GroupsApiError } from '../../client/groups-api.js';
import type { AdminGroup } from '../../shared/groups.js';

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
/* Component                                                                  */
/* -------------------------------------------------------------------------- */

@customElement('scion-group-form-dialog')
export class ScionGroupFormDialog extends LitElement {
  /** Dialog mode. 'edit' will be added in G4. */
  @property({ type: String }) mode: 'create' | 'edit' = 'create';

  /** Whether the dialog is open. */
  @property({ type: Boolean }) open = false;

  // --- Form fields ---
  @state() private formName = '';
  @state() private formSlug = '';
  @state() private formDescription = '';
  @state() private formLabels: Array<{ key: string; value: string }> = [];

  // --- Slug auto-sync ---
  @state() private slugDetached = false;

  // --- Errors ---
  @state() private nameError = '';
  @state() private slugError = '';
  @state() private bannerError = '';
  @state() private labelErrors: Map<number, string> = new Map();

  // --- Submit state ---
  @state() private submitting = false;

  // Dialog ref (used only by G4 edit mode; kept for future use).

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
  // Form management
  // ---------------------------------------------------------------------------

  private resetForm(): void {
    this.formName = '';
    this.formSlug = '';
    this.formDescription = '';
    this.formLabels = [];
    this.slugDetached = false;
    this.clearErrors();
    this.submitting = false;
  }

  private clearErrors(): void {
    this.nameError = '';
    this.slugError = '';
    this.bannerError = '';
    this.labelErrors = new Map();
  }

  // ---------------------------------------------------------------------------
  // Name → slug sync
  // ---------------------------------------------------------------------------

  private onNameInput(e: Event): void {
    this.formName = (e.target as HTMLInputElement).value;
    if (!this.slugDetached) {
      this.formSlug = slugify(this.formName);
    }
    if (this.nameError) this.nameError = '';
  }

  private onSlugInput(e: Event): void {
    const value = (e.target as HTMLInputElement).value;
    this.formSlug = value;
    this.slugDetached = true;
    if (this.slugError) this.slugError = '';
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

    // Reject project: prefix on slug
    if (this.formSlug.startsWith('project:')) {
      this.slugError =
        'Slugs cannot start with "project:" — that prefix is reserved for system-managed groups.';
      valid = false;
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
  // Submit
  // ---------------------------------------------------------------------------

  private async handleSubmit(): Promise<void> {
    if (!this.validate()) {
      // Focus first error field
      this.focusFirstError();
      return;
    }

    this.submitting = true;
    this.bannerError = '';

    try {
      // Build labels map
      const labels: Record<string, string> = {};
      for (const label of this.formLabels) {
        const key = label.key.trim();
        if (key) {
          labels[key] = label.value;
        }
      }

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
      if (err instanceof GroupsApiError) {
        switch (err.kind) {
          case 'conflict_slug':
            this.slugError = 'A group with this slug already exists. Choose a different slug.';
            this.focusSlugField();
            break;
          case 'validation':
            // Try to attribute to a field, otherwise banner
            this.bannerError = err.message;
            break;
          default:
            this.bannerError = err.message;
            break;
        }
      } else {
        this.bannerError = err instanceof Error ? err.message : 'An unexpected error occurred.';
      }
    } finally {
      this.submitting = false;
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
      >
        <p class="helper-copy">
          You will be added as the group's owner. Groups can be granted roles and used in access
          boundaries.
        </p>

        ${this.bannerError
          ? html`
              <sl-alert class="banner-error" variant="danger" open role="alert">
                <sl-icon slot="icon" name="exclamation-octagon"></sl-icon>
                ${this.bannerError}
              </sl-alert>
            `
          : nothing}

        <div class="form-group">
          <sl-input
            id="name-input"
            label="Name"
            placeholder="e.g., Platform Engineers"
            .value=${this.formName}
            @sl-input=${(e: Event) => this.onNameInput(e)}
            ?data-error=${!!this.nameError}
            required
            autofocus
            help-text=${this.nameError || ''}
          ></sl-input>
        </div>

        <div class="form-group">
          <sl-input
            id="slug-input"
            label="Slug"
            placeholder="auto-generated from name"
            .value=${this.formSlug}
            @sl-input=${(e: Event) => this.onSlugInput(e)}
            ?data-error=${!!this.slugError}
            help-text=${this.slugError ||
            'URL-safe identifier. Auto-filled from name; edit to customize. Slugs are permanent after creation.'}
            style="font-family: var(--scion-font-mono, monospace);"
          ></sl-input>
        </div>

        <div class="form-group">
          <sl-textarea
            label="Description"
            placeholder="Optional description"
            .value=${this.formDescription}
            @sl-input=${(e: Event) => {
              this.formDescription = (e.target as HTMLTextAreaElement).value;
            }}
            rows="2"
          ></sl-textarea>
        </div>

        ${this.renderLabelEditor()}

        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.submitting}
          @click=${() => {
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
          ?disabled=${!this.formName.trim()}
          @click=${() => this.handleSubmit()}
        >
          ${this.mode === 'create' ? 'Create group' : 'Save changes'}
        </sl-button>
      </sl-dialog>
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
                @sl-input=${(e: Event) =>
                  this.updateLabelKey(index, (e.target as HTMLInputElement).value)}
                ?data-error=${!!error}
              ></sl-input>
              <sl-input
                size="small"
                placeholder="Value"
                .value=${label.value}
                @sl-input=${(e: Event) =>
                  this.updateLabelValue(index, (e.target as HTMLInputElement).value)}
              ></sl-input>
              <sl-icon-button
                name="x-lg"
                label="Remove label"
                @click=${() => this.removeLabel(index)}
              ></sl-icon-button>
            </div>
            ${error ? html`<div class="label-error" role="alert">${error}</div>` : nothing}
          `;
        })}
        <sl-button variant="text" size="small" @click=${() => this.addLabel()}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
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
