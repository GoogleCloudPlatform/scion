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
 * The Delete artifact confirmation (experiment hub.artifacts;
 * ptone/scion#4535, approved mock state S7), for a caller the hub reports
 * canRetire for. It says what the delete ends: with countsAvailable (a
 * caller who may list the artifact's links and grants, canShare) it loads
 * them and counts the share links that stop working and the grants
 * removed; otherwise those lines are left out. Delete sends DELETE
 * /api/v1/artifacts/{id}; on success it fires artifact-deleted, and a
 * refusal keeps the dialog open with the reason.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import {
  ArtifactDeleteError,
  deleteArtifact,
  listGrants,
  listLinks,
} from '../../client/artifacts.js';
import type { Artifact } from '../../client/artifacts.js';

/** The message for a failed delete. */
export function deleteErrorMessage(err: unknown): string {
  if (err instanceof ArtifactDeleteError) {
    if (err.status === 403) return 'You can no longer delete this artifact.';
    if (err.status === 404) {
      return 'This artifact was already deleted or moved to another project. Reload the page to see its current state.';
    }
  }
  return err instanceof Error && err.message ? err.message : 'Could not delete the artifact.';
}

function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`;
}

@customElement('scion-artifact-delete-dialog')
export class ScionArtifactDeleteDialog extends LitElement {
  @property({ type: Object }) artifact: Artifact | null = null;
  @property({ type: Boolean, reflect: true }) open = false;
  /** The caller may list the artifact's links and grants (canShare). */
  @property({ type: Boolean }) countsAvailable = false;
  /** How many versions the artifact has, when the page knows all of them. */
  @property({ type: Number }) versionCount: number | null = null;

  @state() private deleting = false;
  @state() private error: string | null = null;
  @state() private links: number | null = null;
  @state() private grants: number | null = null;

  private loadGen = 0;

  static override styles = css`
    sl-dialog::part(panel) {
      width: min(520px, 95vw);
    }
    p {
      margin: 0 0 0.6rem;
      font-size: 0.875rem;
    }
    ul {
      margin: 0 0 0.75rem;
      padding-left: 1.25rem;
      font-size: 0.875rem;
    }
    li + li {
      margin-top: 0.25rem;
    }
    sl-alert {
      margin-bottom: 0.6rem;
    }
    .footer {
      display: flex;
      justify-content: flex-end;
      gap: 0.5rem;
    }
  `;

  override updated(changed: Map<string, unknown>): void {
    if (!changed.has('open')) return;
    this.loadGen++;
    if (!this.open) return;
    this.error = null;
    this.deleting = false;
    this.links = null;
    this.grants = null;
    if (this.countsAvailable) void this.loadCounts();
  }

  /** Counts the links and grants the delete ends; a failure leaves the lines out. */
  private async loadCounts(): Promise<void> {
    const a = this.artifact;
    if (!a) return;
    const gen = this.loadGen;
    try {
      const [links, grants] = await Promise.all([listLinks(a.id), listGrants(a.id)]);
      if (gen !== this.loadGen) return;
      this.links = links.links.length;
      this.grants = grants.grants.length;
    } catch {
      // The counts are a courtesy: the dialog works without them.
    }
  }

  private close(): void {
    this.loadGen++;
    this.dispatchEvent(
      new CustomEvent('artifact-delete-closed', { bubbles: true, composed: true })
    );
  }

  private async confirm(): Promise<void> {
    const a = this.artifact;
    if (!a || this.deleting) return;
    this.deleting = true;
    this.error = null;
    try {
      await deleteArtifact(a.id);
      this.dispatchEvent(
        new CustomEvent<Artifact>('artifact-deleted', { detail: a, bubbles: true, composed: true })
      );
    } catch (err) {
      this.error = deleteErrorMessage(err);
    } finally {
      this.deleting = false;
    }
  }

  private renderBody(a: Artifact): TemplateResult {
    const versions =
      this.versionCount && this.versionCount > 1
        ? `All ${this.versionCount} versions are deleted`
        : this.versionCount === 1
          ? 'Its version is deleted'
          : 'All its versions are deleted';
    return html`
      ${this.error
        ? html`<sl-alert variant="danger" open role="alert">
            <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
            ${this.error}
          </sl-alert>`
        : nothing}
      <p>Delete <strong>${a.title}</strong>? It stops being readable everywhere at once.</p>
      <ul>
        ${this.links !== null && this.links > 0
          ? html`<li class="links">
              ${plural(this.links, 'share link stops', 'share links stop')} working.
            </li>`
          : nothing}
        ${this.grants !== null && this.grants > 0
          ? html`<li class="grants">
              ${plural(
                this.grants,
                'grant to people and projects is',
                'grants to people and projects are'
              )}
              removed.
            </li>`
          : nothing}
        <li>Messages that refer to it show it as unavailable.</li>
        <li class="versions">
          ${versions}; the files are removed from storage after the hub's grace period.
        </li>
      </ul>
      <sl-alert variant="primary" open>
        <sl-icon slot="icon" name="info-circle"></sl-icon>
        The deletion is recorded in the artifact's history with your name. It cannot be undone here.
      </sl-alert>
    `;
  }

  override render(): TemplateResult {
    const a = this.artifact;
    return html`
      <sl-dialog
        label="Delete artifact"
        ?open=${this.open}
        @sl-request-close=${(e: Event): void => {
          if (this.deleting) e.preventDefault();
        }}
        @sl-after-hide=${(e: Event): void => {
          if (e.target === e.currentTarget && this.open) this.close();
        }}
      >
        ${a ? this.renderBody(a) : nothing}
        <div slot="footer" class="footer">
          <sl-button size="small" ?disabled=${this.deleting} @click=${(): void => this.close()}
            >Cancel</sl-button
          >
          <sl-button
            size="small"
            variant="danger"
            ?loading=${this.deleting}
            @click=${(): void => void this.confirm()}
          >
            <sl-icon slot="prefix" name="trash"></sl-icon>
            Delete
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-artifact-delete-dialog': ScionArtifactDeleteDialog;
  }
}
