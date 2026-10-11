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
 * Moves an artifact whose home project was deleted to another project
 * (experiment hub.artifacts). Offered to the artifact's owner or an admin;
 * the hub checks that the caller may publish in the chosen project.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { patchArtifact } from '../../client/artifacts.js';
import type { Artifact } from '../../client/artifacts.js';

interface ProjectOption {
  id: string;
  name: string;
}

@customElement('scion-artifact-move-dialog')
export class ScionArtifactMoveDialog extends LitElement {
  @property({ type: Object }) artifact: Pick<Artifact, 'id' | 'title' | 'scopeRef'> | null = null;
  @property({ type: Boolean, reflect: true }) open = false;

  @state() private projects: ProjectOption[] = [];
  @state() private target = '';
  @state() private loading = false;
  @state() private busy = false;
  @state() private error: string | null = null;

  private loadGen = 0;

  static override styles = css`
    sl-dialog::part(panel) {
      width: min(520px, 95vw);
    }
    .what {
      margin: 0 0 0.75rem;
      font-size: 0.875rem;
    }
    .what strong {
      display: block;
    }
    .hint {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0.4rem 0 0;
    }
    sl-alert {
      margin-top: 0.75rem;
    }
    .footer {
      display: flex;
      justify-content: flex-end;
      gap: 0.5rem;
    }
  `;

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('open') && this.open) {
      this.error = null;
      this.target = '';
      void this.loadProjects();
    }
  }

  private async loadProjects(): Promise<void> {
    const gen = ++this.loadGen;
    this.loading = true;
    try {
      const res = await apiFetch('/api/v1/projects?mine=true&limit=100');
      if (!res.ok) throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      const body = (await res.json()) as {
        projects?: Array<{ id: string; name?: string; slug?: string }>;
      };
      if (gen !== this.loadGen) return;
      this.projects = (body.projects ?? [])
        .filter((p) => p.id !== this.artifact?.scopeRef)
        .map((p) => ({ id: p.id, name: p.name || p.slug || p.id }));
    } catch (err) {
      if (gen !== this.loadGen) return;
      this.error = err instanceof Error ? err.message : 'Could not load projects';
    } finally {
      if (gen === this.loadGen) this.loading = false;
    }
  }

  private close(): void {
    this.dispatchEvent(new CustomEvent('artifact-move-closed', { bubbles: true, composed: true }));
  }

  private async move(): Promise<void> {
    const a = this.artifact;
    if (!a || !this.target || this.busy) return;
    this.busy = true;
    this.error = null;
    try {
      const res = await patchArtifact(a.id, { scopeRef: this.target });
      this.dispatchEvent(
        new CustomEvent<Artifact>('artifact-moved', {
          detail: res.artifact,
          bubbles: true,
          composed: true,
        })
      );
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Could not move the artifact';
    } finally {
      this.busy = false;
    }
  }

  override render(): TemplateResult {
    const a = this.artifact;
    return html`
      <sl-dialog
        label="Move to another project"
        ?open=${this.open}
        @sl-request-close=${(e: Event): void => {
          if (this.busy) e.preventDefault();
        }}
        @sl-after-hide=${(e: Event): void => {
          if (e.target === e.currentTarget && this.open) this.close();
        }}
      >
        ${a
          ? html`<p class="what">
              <strong>${a.title}</strong>
              The home project of this artifact was deleted. Choose a new home project; its members
              will be able to view the artifact.
            </p>`
          : nothing}
        <sl-select
          label="Project"
          size="small"
          placeholder=${this.loading ? 'Loading projects…' : 'Choose a project'}
          ?disabled=${this.loading || this.projects.length === 0}
          value=${this.target}
          @sl-change=${(e: Event): void => {
            this.target = (e.target as HTMLInputElement).value;
          }}
        >
          ${this.projects.map((p) => html`<sl-option value=${p.id}>${p.name}</sl-option>`)}
        </sl-select>
        <p class="hint">
          ${!this.loading && this.projects.length === 0
            ? 'You are not a member of any other project.'
            : 'Your projects are listed; you need permission to publish in the one you choose. ' +
              'Share links and grants to people and other projects are kept. ' +
              'The old home project loses its access unless you grant it explicitly.'}
        </p>
        ${this.error
          ? html`<sl-alert variant="danger" open role="alert">
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              ${this.error}
            </sl-alert>`
          : nothing}
        <div slot="footer" class="footer">
          <sl-button size="small" ?disabled=${this.busy} @click=${(): void => this.close()}
            >Cancel</sl-button
          >
          <sl-button
            size="small"
            variant="primary"
            ?disabled=${!this.target}
            ?loading=${this.busy}
            @click=${(): void => void this.move()}
            >Move</sl-button
          >
        </div>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-artifact-move-dialog': ScionArtifactMoveDialog;
  }
}
