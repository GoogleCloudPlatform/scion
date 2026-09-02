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
 * Group delete confirmation dialog.
 *
 * Purpose-built dialog (not bare `showConfirm`) because it shows impact
 * (member count), requires typed-slug confirmation, and has a dedicated
 * constraint-gate 403 variant.
 *
 * Flow:
 *   1. Show impact copy with member count.
 *   2. User types the group slug to confirm.
 *   3. DELETE /api/v1/groups/{id} via groups-api.ts.
 *   4. On 204: toast "Group deleted", dispatch 'group-deleted'.
 *   5. On constraint_gate 403: switch to protection dialog variant.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { AdminGroup } from '../../shared/groups.js';
import { deleteGroup, GroupsApiError } from '../../client/groups-api.js';
import { showToast } from '../../utils/toast.js';

/** Event detail emitted on successful group deletion. */
export interface GroupDeletedDetail {
  groupId: string;
}

type DialogPhase = 'confirm' | 'constraint_gate' | 'error';

@customElement('scion-group-delete-dialog')
export class ScionGroupDeleteDialog extends LitElement {
  /** The group to delete. Must be set before calling show(). */
  @property({ type: Object }) group: AdminGroup | null = null;

  /** Number of members in this group (for impact copy). */
  @property({ type: Number }) memberCount = 0;

  @state() private open = false;
  @state() private phase: DialogPhase = 'confirm';
  @state() private deleting = false;
  @state() private typedSlug = '';
  @state() private errorMessage = '';

  static override styles = css`
    :host {
      display: contents;
    }

    .impact-copy {
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      margin-bottom: 1rem;
      line-height: 1.5;
    }

    .impact-copy strong {
      font-weight: 600;
    }

    .impact-list {
      list-style: disc;
      margin: 0.5rem 0 1rem 1.5rem;
      padding: 0;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .impact-list li {
      margin-bottom: 0.25rem;
    }

    .slug-confirm {
      margin-bottom: 1rem;
    }

    .slug-confirm-label {
      font-size: 0.8125rem;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.375rem;
    }

    .slug-confirm-label code {
      font-family: var(--scion-font-mono, monospace);
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.125rem 0.375rem;
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.8125rem;
      font-weight: 600;
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

    /* Constraint gate variant */
    .constraint-gate-body {
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      line-height: 1.6;
    }

    .constraint-gate-body p {
      margin: 0 0 0.75rem 0;
    }

    .constraint-gate-body sl-icon {
      font-size: 1.5rem;
      color: var(--sl-color-warning-500, #eab308);
      margin-bottom: 0.5rem;
    }
  `;

  /** Open the delete confirmation dialog. */
  show(): void {
    if (!this.group) return;
    this.phase = 'confirm';
    this.typedSlug = '';
    this.deleting = false;
    this.errorMessage = '';
    this.open = true;
  }

  /** Close the dialog. */
  hide(): void {
    this.open = false;
  }

  /** Whether the typed slug matches the group slug (case-sensitive). */
  get slugMatches(): boolean {
    return !!this.group && this.typedSlug === this.group.slug;
  }

  private async handleDelete(): Promise<void> {
    if (!this.group || !this.slugMatches) return;

    this.deleting = true;
    this.errorMessage = '';

    try {
      await deleteGroup(this.group.id);

      showToast('Group deleted', 'success');
      this.open = false;

      this.dispatchEvent(
        new CustomEvent<GroupDeletedDetail>('group-deleted', {
          detail: { groupId: this.group.id },
          bubbles: true,
          composed: true,
        })
      );
    } catch (err) {
      if (err instanceof GroupsApiError && err.kind === 'constraint_gate') {
        // Switch to the constraint-gate protection dialog variant.
        this.phase = 'constraint_gate';
      } else if (err instanceof GroupsApiError) {
        this.errorMessage = err.message;
        this.phase = 'error';
      } else {
        this.errorMessage = err instanceof Error ? err.message : 'Failed to delete group';
        this.phase = 'error';
      }
    } finally {
      this.deleting = false;
    }
  }

  override render() {
    if (this.phase === 'constraint_gate') {
      return this.renderConstraintGate();
    }

    return this.renderConfirmDialog();
  }

  private renderConfirmDialog() {
    const groupName = this.group?.name ?? 'this group';
    const slug = this.group?.slug ?? '';

    return html`
      <sl-dialog
        label=${`Delete group "${groupName}"?`}
        ?open=${this.open}
        @sl-request-close=${(e: Event) => {
          if (this.deleting) {
            e.preventDefault();
            return;
          }
          this.open = false;
        }}
      >
        ${this.phase === 'error' && this.errorMessage
          ? html`<div class="error-banner" role="alert">${this.errorMessage}</div>`
          : nothing}

        <div class="impact-copy">
          This permanently removes the group and its
          <strong>${this.memberCount}</strong> membership${this.memberCount !== 1 ? 's' : ''}.
        </div>

        <ul class="impact-list">
          <li>Role bindings that name this group stop granting access.</li>
          <li>This cannot be undone.</li>
        </ul>

        <div class="slug-confirm">
          <div class="slug-confirm-label">
            Type the group slug (<code>${slug}</code>) to confirm:
          </div>
          <sl-input
            value=${this.typedSlug}
            placeholder=${slug}
            ?disabled=${this.deleting}
            @sl-input=${(e: Event) => {
              this.typedSlug = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>
        </div>

        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.deleting}
          @click=${() => {
            this.open = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="danger"
          ?loading=${this.deleting}
          ?disabled=${!this.slugMatches || this.deleting}
          @click=${() => void this.handleDelete()}
          >Delete group</sl-button
        >
      </sl-dialog>
    `;
  }

  private renderConstraintGate() {
    const groupName = this.group?.name ?? 'this group';
    const groupId = this.group?.id ?? '';
    const boundaryUrl = `/admin/access-boundaries?subjectKind=group_closure&subjectId=${encodeURIComponent(groupId)}`;

    return html`
      <sl-dialog
        label="This group is protected by access boundaries"
        ?open=${this.open}
        @sl-request-close=${() => {
          this.open = false;
        }}
      >
        <div class="constraint-gate-body">
          <p>
            "${groupName}" is the subject of one or more access boundaries. Deleting it would relax
            those boundaries, so it requires access-boundary administration permission, which you do
            not hold.
          </p>
        </div>

        <a href=${boundaryUrl} slot="footer">
          <sl-button variant="default"> View its access boundaries </sl-button>
        </a>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => {
            this.open = false;
          }}
          >Close</sl-button
        >
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-group-delete-dialog': ScionGroupDeleteDialog;
  }
}
