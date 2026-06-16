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
 * Skill creation page component
 *
 * Form for creating a new skill with name, scope, visibility, description, and tags.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import type { Capabilities } from '../../shared/types.js';
import { can } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';

@customElement('scion-page-skill-create')
export class ScionPageSkillCreate extends LitElement {
  @state() private loading = true;
  @state() private canCreate = false;
  @state() private submitting = false;
  @state() private error: string | null = null;
  @state() private name = '';
  @state() private description = '';
  @state() private scope: 'global' | 'project' | 'user' = 'global';
  @state() private scopeId = '';
  @state() private visibility: 'private' | 'public' = 'private';
  @state() private tagsInput = '';

  static override styles = css`
    :host {
      display: block;
    }

    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }

    .back-link:hover {
      color: var(--scion-primary, #3b82f6);
    }

    .page-header {
      margin-bottom: 1.5rem;
    }

    .page-header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .page-header h1 sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }

    .page-header p {
      color: var(--scion-text-muted, #64748b);
      margin: 0;
      font-size: 0.875rem;
    }

    .form-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      max-width: 640px;
    }

    .form-field {
      margin-bottom: 1.25rem;
    }

    .form-field label {
      display: block;
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.375rem;
    }

    .form-field .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
    }

    .form-field sl-input,
    .form-field sl-textarea,
    .form-field sl-select,
    .form-field sl-radio-group {
      width: 100%;
    }

    .form-actions {
      display: flex;
      gap: 0.75rem;
      margin-top: 1.5rem;
      padding-top: 1.5rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .error-banner {
      background: var(--sl-color-danger-50, #fef2f2);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      margin-bottom: 1.25rem;
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      color: var(--sl-color-danger-700, #b91c1c);
      font-size: 0.875rem;
    }

    .error-banner sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .tag-chips {
      display: flex;
      flex-wrap: wrap;
      gap: 0.375rem;
      margin-top: 0.5rem;
    }

    .tag-chip {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.75rem;
      padding: 0.125rem 0.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 9999px;
      color: var(--scion-text, #1e293b);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.checkCapabilities();
  }

  private async checkCapabilities(): Promise<void> {
    this.loading = true;
    try {
      const res = await apiFetch('/api/v1/skills');
      if (res.ok) {
        const data = (await res.json()) as { _capabilities?: Capabilities };
        this.canCreate = can(data._capabilities, 'create');
      }
    } catch {
      // fail-closed
    } finally {
      this.loading = false;
    }
  }

  private get parsedTags(): string[] {
    if (!this.tagsInput.trim()) return [];
    return this.tagsInput
      .split(',')
      .map((t) => t.trim())
      .filter((t) => t.length > 0);
  }

  private async handleSubmit(): Promise<void> {
    if (!this.name.trim()) {
      this.error = 'Skill name is required.';
      return;
    }

    if (this.scope === 'project' && !this.scopeId.trim()) {
      this.error = 'Project ID is required for project scope.';
      return;
    }

    this.submitting = true;
    this.error = null;

    try {
      const body: Record<string, unknown> = {
        name: this.name.trim(),
        scope: this.scope,
        visibility: this.visibility,
      };

      if (this.description.trim()) {
        body.description = this.description.trim();
      }

      if (this.scope === 'project' && this.scopeId.trim()) {
        body.scopeId = this.scopeId.trim();
      }

      const tags = this.parsedTags;
      if (tags.length > 0) {
        body.tags = tags;
      }

      const response = await apiFetch('/api/v1/skills', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }

      const result = (await response.json()) as { skill?: { id: string }; id?: string };
      const skillId = result.skill?.id || result.id;

      if (!skillId) {
        throw new Error('No skill ID in response');
      }

      window.history.pushState({}, '', `/skills/${skillId}`);
      window.dispatchEvent(new PopStateEvent('popstate'));
    } catch (err) {
      console.error('Failed to create skill:', err);
      this.error = err instanceof Error ? err.message : 'Failed to create skill';
    } finally {
      this.submitting = false;
    }
  }

  override render() {
    if (this.loading) {
      return html`
        <div style="display: flex; flex-direction: column; align-items: center; padding: 4rem 2rem; color: var(--scion-text-muted, #64748b);">
          <sl-spinner style="font-size: 2rem; margin-bottom: 1rem;"></sl-spinner>
          <p>Loading...</p>
        </div>
      `;
    }

    if (!this.canCreate) {
      return html`
        <a href="/skills" class="back-link">
          <sl-icon name="arrow-left"></sl-icon>
          Back to Skills
        </a>
        <div style="text-align: center; padding: 3rem 2rem; background: var(--scion-surface, #ffffff); border: 1px solid var(--scion-border, #e2e8f0); border-radius: var(--scion-radius-lg, 0.75rem);">
          <sl-icon name="shield-lock" style="font-size: 3rem; color: var(--scion-text-muted, #64748b); margin-bottom: 1rem;"></sl-icon>
          <h2 style="font-size: 1.25rem; font-weight: 600; color: var(--scion-text, #1e293b); margin: 0 0 0.5rem 0;">Access Denied</h2>
          <p style="color: var(--scion-text-muted, #64748b); margin: 0 0 1rem 0;">You do not have permission to create skills.</p>
          <a href="/skills" style="text-decoration: none;">
            <sl-button variant="primary">
              <sl-icon slot="prefix" name="arrow-left"></sl-icon>
              Back to Skills
            </sl-button>
          </a>
        </div>
      `;
    }

    return html`
      <a href="/skills" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Skills
      </a>

      <div class="page-header">
        <h1>
          <sl-icon name="lightning-charge"></sl-icon>
          Create Skill
        </h1>
        <p>Define a new reusable skill for your agents.</p>
      </div>

      <div class="form-card">
        ${this.error
          ? html`
              <div class="error-banner">
                <sl-icon name="exclamation-triangle"></sl-icon>
                <span>${this.error}</span>
              </div>
            `
          : nothing}

        <div>
          <div class="form-field">
            <label for="name">Name</label>
            <sl-input
              id="name"
              placeholder="my-skill"
              .value=${this.name}
              @sl-input=${(e: Event) => { this.name = (e.target as HTMLElement & { value: string }).value; }}
              required
            ></sl-input>
          </div>

          <div class="form-field">
            <label for="description">Description</label>
            <sl-textarea
              id="description"
              placeholder="What does this skill do?"
              .value=${this.description}
              @sl-input=${(e: Event) => { this.description = (e.target as HTMLElement & { value: string }).value; }}
              maxlength="500"
              rows="3"
            ></sl-textarea>
          </div>

          <div class="form-field">
            <label for="scope">Scope</label>
            <sl-select
              id="scope"
              .value=${this.scope}
              @sl-change=${(e: Event) => { this.scope = (e.target as HTMLElement & { value: string }).value as 'global' | 'project' | 'user'; }}
            >
              <sl-option value="global">Global</sl-option>
              <sl-option value="project">Project</sl-option>
              <sl-option value="user">User</sl-option>
            </sl-select>
            <div class="hint">
              ${this.scope === 'global'
                ? 'Available to all projects and agents.'
                : this.scope === 'project'
                  ? 'Scoped to a specific project.'
                  : 'Scoped to your user account.'}
            </div>
          </div>

          ${this.scope === 'project' ? html`
            <div class="form-field">
              <label for="scopeId">Project ID</label>
              <sl-input
                id="scopeId"
                placeholder="project-uuid"
                .value=${this.scopeId}
                @sl-input=${(e: Event) => { this.scopeId = (e.target as HTMLElement & { value: string }).value; }}
              ></sl-input>
            </div>
          ` : nothing}
          ${this.scope === 'user' ? html`
            <div class="form-field">
              <div class="hint">Skills will be created under your user account.</div>
            </div>
          ` : nothing}

          <div class="form-field">
            <label>Visibility</label>
            <sl-radio-group
              .value=${this.visibility}
              @sl-change=${(e: Event) => { this.visibility = (e.target as HTMLElement & { value: string }).value as 'private' | 'public'; }}
            >
              <sl-radio-button value="private">Private</sl-radio-button>
              <sl-radio-button value="public">Public</sl-radio-button>
            </sl-radio-group>
          </div>

          <div class="form-field">
            <label for="tags">Tags</label>
            <sl-input
              id="tags"
              placeholder="cli, automation, testing"
              .value=${this.tagsInput}
              @sl-input=${(e: Event) => { this.tagsInput = (e.target as HTMLElement & { value: string }).value; }}
            ></sl-input>
            <div class="hint">Comma-separated list of tags.</div>
            ${this.parsedTags.length > 0 ? html`
              <div class="tag-chips">
                ${this.parsedTags.map((tag) => html`<span class="tag-chip">${tag}</span>`)}
              </div>
            ` : nothing}
          </div>

          <div class="form-actions">
            <sl-button
              variant="primary"
              ?loading=${this.submitting}
              ?disabled=${this.submitting}
              @click=${() => this.handleSubmit()}
            >
              <sl-icon slot="prefix" name="lightning-charge"></sl-icon>
              Create Skill
            </sl-button>
            <a href="/skills" style="text-decoration: none;">
              <sl-button variant="default" ?disabled=${this.submitting}>
                Cancel
              </sl-button>
            </a>
          </div>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-skill-create': ScionPageSkillCreate;
  }
}
