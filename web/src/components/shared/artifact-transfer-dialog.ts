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
 * Transfers ownership of an agent-owned artifact to another eligible agent in
 * the same project (experiment hub.artifacts_transfer).
 *
 * Backed by GET /api/v1/artifacts/{id}/transfer-targets and
 * POST /api/v1/artifacts/{id}/transfer. Handles empty target lists, live-owner
 * warnings, key_conflict (with clearKey retry), owner_changed (reload and
 * confirm again), and inline error reporting.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import {
  ArtifactTransferError,
  listTransferTargets,
  transferArtifact,
} from '../../client/artifacts.js';
import type { Artifact, ArtifactResponse, TransferCandidate } from '../../client/artifacts.js';
import { shortId } from '../../client/principal-names.js';
import { ownerDeletedBadge, ownerDeletedStyles } from './artifact-owner-deleted.js';

/** Shoelace badge variant for an agent's lifecycle phase. */
export function phaseBadgeVariant(
  phase?: string | null
): 'success' | 'warning' | 'danger' | 'neutral' | 'primary' {
  switch ((phase ?? '').toLowerCase()) {
    case 'running':
      return 'success';
    case 'starting':
    case 'stopping':
    case 'cloning':
    case 'provisioning':
      return 'primary';
    case 'suspended':
    case 'stopped':
    case 'created':
      return 'warning';
    case 'error':
      return 'danger';
    default:
      return 'neutral';
  }
}

/** Human-readable error message for a failed transfer or target lookup. */
export function transferErrorMessage(err: unknown): string {
  if (err instanceof ArtifactTransferError) {
    switch (err.code) {
      case 'owner_live':
        return 'Cannot transfer ownership from a live agent you do not control.';
      case 'target_ineligible':
        return 'The selected agent is not eligible to take ownership of this artifact.';
      case 'transfer_not_allowed':
        return 'You do not have permission to transfer ownership of this artifact.';
      case 'forbidden':
        return 'Ownership transfer requires an interactive browser session.';
      case 'rate_limited':
        return 'Too many transfer attempts. Please wait before trying again.';
      case 'unavailable':
        return 'Could not complete the ownership transfer right now. Please try again.';
      case 'invalid_target':
        return err.message || 'The selected target agent is invalid.';
      case 'not_found':
        return 'This artifact is no longer available or ownership transfer is disabled.';
      default:
        return err.message || 'Could not transfer ownership.';
    }
  }
  return err instanceof Error && err.message ? err.message : 'Could not transfer ownership.';
}

@customElement('scion-artifact-transfer-dialog')
export class ScionArtifactTransferDialog extends LitElement {
  @property({ type: Object }) artifact: Artifact | null = null;
  @property({ type: Boolean, reflect: true }) open = false;
  /** Resolved display name of the current owner agent, when known. */
  @property({ type: String }) ownerName = '';
  /** Display name of the artifact's home project. */
  @property({ type: String }) projectName = '';
  /** Steward authority ("project_owner" | "delegator" | ""), when known. */
  @property({ type: String }) shareAuthority = '';

  @state() private agents: TransferCandidate[] = [];
  @state() private truncated = false;
  @state() private selectedTarget = '';
  @state() private loading = false;
  @state() private busy = false;
  @state() private loadError: string | null = null;
  @state() private error: string | null = null;
  @state() private keyConflict = false;
  @state() private ownerChangedNotice: string | null = null;
  @state() private transferAllowed = true;

  private loadGen = 0;
  private loadAbort: AbortController | null = null;

  static override styles = [
    ownerDeletedStyles,
    css`
      sl-dialog::part(panel) {
        width: min(540px, 95vw);
      }
      .what {
        margin: 0 0 0.75rem;
        font-size: 0.875rem;
      }
      .what strong {
        display: block;
        margin-bottom: 0.25rem;
      }
      .owner-row {
        display: flex;
        align-items: center;
        flex-wrap: wrap;
        gap: 0.4rem;
        font-size: 0.875rem;
        margin: 0 0 0.75rem;
      }
      .owner-label {
        color: var(--scion-text-muted, #64748b);
      }
      .hint {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        margin: 0.5rem 0 0;
      }
      .empty-state {
        font-size: 0.875rem;
        color: var(--scion-text-muted, #64748b);
        margin: 0.5rem 0 0;
      }
      .option-slug {
        color: var(--scion-text-muted, #64748b);
        font-size: 0.8125rem;
        margin-left: 0.35rem;
      }
      sl-alert {
        margin-top: 0.75rem;
      }
      .conflict-actions {
        margin-top: 0.6rem;
      }
      .footer {
        display: flex;
        justify-content: flex-end;
        gap: 0.5rem;
      }
    `,
  ];

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.loadAbort?.abort();
    this.loadAbort = null;
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('open')) {
      if (this.open) {
        this.resetState();
        void this.loadTargets();
      } else {
        this.loadGen++;
        this.loadAbort?.abort();
        this.loadAbort = null;
      }
    }
  }

  private resetState(): void {
    this.agents = [];
    this.truncated = false;
    this.selectedTarget = '';
    this.loadError = null;
    this.error = null;
    this.keyConflict = false;
    this.ownerChangedNotice = null;
    this.transferAllowed = true;
    this.busy = false;
  }

  private async loadTargets(): Promise<void> {
    const a = this.artifact;
    if (!a) return;
    const gen = ++this.loadGen;
    this.loadAbort?.abort();
    const abort = new AbortController();
    this.loadAbort = abort;
    this.loading = true;
    this.loadError = null;
    try {
      const res = await listTransferTargets(a.id, abort.signal);
      if (gen !== this.loadGen) return;
      this.agents = res.agents;
      this.truncated = res.truncated;
    } catch (err) {
      if (gen !== this.loadGen) return;
      if (err instanceof DOMException && err.name === 'AbortError') return;
      this.loadError = transferErrorMessage(err);
    } finally {
      if (gen === this.loadGen) this.loading = false;
    }
  }

  private close(): void {
    this.loadGen++;
    this.loadAbort?.abort();
    this.loadAbort = null;
    this.dispatchEvent(
      new CustomEvent('artifact-transfer-closed', { bubbles: true, composed: true })
    );
  }

  private async reloadAfterOwnerChanged(): Promise<void> {
    const a = this.artifact;
    if (!a) return;
    try {
      const res = await apiFetch(`/api/v1/artifacts/${encodeURIComponent(a.id)}`);
      if (!res.ok) {
        this.error = await extractApiError(
          res,
          'The artifact owner changed, and the updated artifact could not be loaded.'
        );
        return;
      }
      const fresh = (await res.json()) as ArtifactResponse;
      this.artifact = fresh.artifact;
      this.shareAuthority = fresh.shareAuthority ?? '';
      this.selectedTarget = '';
      this.keyConflict = false;
      this.dispatchEvent(
        new CustomEvent<ArtifactResponse>('artifact-reloaded', {
          detail: fresh,
          bubbles: true,
          composed: true,
        })
      );
      if (fresh.canTransfer === false) {
        this.transferAllowed = false;
        this.agents = [];
        this.error =
          'The artifact owner changed while this dialog was open, and you can no longer transfer it.';
        return;
      }
      this.ownerChangedNotice =
        'The artifact owner changed while this dialog was open. Review the updated owner and confirm again.';
      await this.loadTargets();
    } catch (err) {
      this.error =
        err instanceof Error && err.message
          ? err.message
          : 'The artifact owner changed; reload the page and try again.';
    }
  }

  private async submit(clearKey = false): Promise<void> {
    const a = this.artifact;
    if (!a || !this.selectedTarget || this.busy || !this.transferAllowed) return;
    this.busy = true;
    this.error = null;
    this.ownerChangedNotice = null;
    if (!clearKey) this.keyConflict = false;
    try {
      const res = await transferArtifact(a.id, {
        to: `agent:${this.selectedTarget}`,
        expectOwner: `${a.ownerKind}:${a.ownerRef}`,
        ...(clearKey ? { clearKey: true } : {}),
      });
      this.keyConflict = false;
      this.dispatchEvent(
        new CustomEvent<ArtifactResponse>('artifact-changed', {
          detail: res.artifact,
          bubbles: true,
          composed: true,
        })
      );
    } catch (err) {
      if (err instanceof ArtifactTransferError && err.code === 'key_conflict') {
        this.keyConflict = true;
        this.error = null;
      } else if (err instanceof ArtifactTransferError && err.code === 'owner_changed') {
        this.keyConflict = false;
        await this.reloadAfterOwnerChanged();
      } else {
        this.keyConflict = false;
        this.error = transferErrorMessage(err);
      }
    } finally {
      this.busy = false;
    }
  }

  private get displayOwner(): string {
    if (this.ownerName) return this.ownerName;
    if (this.artifact?.ownerRef) return shortId(this.artifact.ownerRef);
    return 'unknown';
  }

  private get displayProject(): string {
    return this.projectName || this.artifact?.scopeRef || 'this project';
  }

  private renderOption(c: TransferCandidate): TemplateResult {
    const primary = c.name || c.slug || shortId(c.id);
    const showSlug = c.slug && c.slug !== primary;
    return html`
      <sl-option value=${c.id}>
        ${primary}${showSlug ? html`<span class="option-slug">(${c.slug})</span>` : nothing}
        <sl-badge slot="suffix" variant=${phaseBadgeVariant(c.phase)} pill>
          ${c.phase || 'unknown'}
        </sl-badge>
      </sl-option>
    `;
  }

  override render(): TemplateResult {
    const a = this.artifact;
    const isEmpty =
      !this.loading && !this.loadError && this.transferAllowed && this.agents.length === 0;
    const showSubmit = !isEmpty && this.transferAllowed;

    return html`
      <sl-dialog
        label="Transfer ownership"
        ?open=${this.open}
        @sl-request-close=${(e: Event): void => {
          if (this.busy) e.preventDefault();
        }}
        @sl-after-hide=${(e: Event): void => {
          if (e.target !== e.currentTarget) return;
          if (this.open) this.close();
        }}
      >
        ${a
          ? html`
              <p class="what">
                <strong>${a.title}</strong>
                Choose an eligible agent in ${this.displayProject} to become the new owner. History,
                grants, share links, and reviews stay intact.
              </p>
              <div class="owner-row">
                <span class="owner-label">Current owner:</span>
                ${a.ownerDeleted
                  ? html`<span class="owner-gone">${this.displayOwner}</span> ${ownerDeletedBadge(
                        a.ownerKind
                      )}`
                  : html`<span>${this.displayOwner} (agent)</span>`}
              </div>
              ${!a.ownerDeleted
                ? html`<sl-alert class="live-owner-warning" variant="warning" open>
                    <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                    Agent ${this.displayOwner} is running and will no longer be able to publish new
                    versions of this artifact.
                  </sl-alert>`
                : nothing}
              ${this.shareAuthority === 'project_owner'
                ? html`<p class="hint steward-note">
                    Stewardship will move to the new owner's delegating user; the previous delegator
                    will no longer be a steward.
                  </p>`
                : nothing}
            `
          : nothing}
        ${this.ownerChangedNotice
          ? html`<sl-alert class="owner-changed-alert" variant="warning" open role="alert">
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              ${this.ownerChangedNotice}
            </sl-alert>`
          : nothing}
        ${this.loadError
          ? html`<sl-alert class="load-error" variant="danger" open role="alert">
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              ${this.loadError}
            </sl-alert>`
          : isEmpty
            ? html`<p class="empty-state">
                You have no agents in ${this.displayProject} that can take ownership.
              </p>`
            : this.transferAllowed
              ? html`
                  <sl-select
                    label="New owner agent"
                    size="small"
                    placeholder=${this.loading ? 'Loading agents…' : 'Choose an agent'}
                    ?disabled=${this.loading || this.busy || this.agents.length === 0}
                    value=${this.selectedTarget}
                    @sl-change=${(e: Event): void => {
                      this.selectedTarget = (e.target as HTMLInputElement).value;
                      this.keyConflict = false;
                      this.error = null;
                    }}
                  >
                    ${this.agents.map((c) => this.renderOption(c))}
                  </sl-select>
                  ${this.truncated
                    ? html`<p class="hint">
                        Showing the first ${this.agents.length} eligible agents.
                      </p>`
                    : nothing}
                `
              : nothing}
        ${this.keyConflict
          ? html`<sl-alert class="key-conflict-alert" variant="warning" open role="alert">
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              The selected agent already owns an artifact with
              ${a?.key ? html`key <code>${a.key}</code>` : 'the same key'} in
              ${this.displayProject}. You can remove the key from this artifact and complete the
              transfer.
              <div class="conflict-actions">
                <sl-button
                  size="small"
                  variant="warning"
                  class="clear-key-btn"
                  ?loading=${this.busy}
                  ?disabled=${this.busy}
                  @click=${(): void => void this.submit(true)}
                >
                  Remove the key and transfer
                </sl-button>
              </div>
            </sl-alert>`
          : nothing}
        ${this.error
          ? html`<sl-alert class="transfer-error" variant="danger" open role="alert">
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              ${this.error}
            </sl-alert>`
          : nothing}

        <div slot="footer" class="footer">
          <sl-button size="small" ?disabled=${this.busy} @click=${(): void => this.close()}>
            Cancel
          </sl-button>
          ${showSubmit
            ? html`<sl-button
                size="small"
                variant="primary"
                class="confirm-transfer"
                ?disabled=${!this.selectedTarget || this.loading || this.busy}
                ?loading=${this.busy}
                @click=${(): void => void this.submit(false)}
              >
                Transfer ownership
              </sl-button>`
            : nothing}
        </div>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-artifact-transfer-dialog': ScionArtifactTransferDialog;
  }
}
