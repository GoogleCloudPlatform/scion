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
 * Project Schedules page component
 *
 * Displays recurring schedules for a project with full CRUD management.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import '../shared/schedule-list.js';

interface Project {
  id: string;
  name: string;
  slug?: string;
}

@customElement('scion-page-project-schedules')
export class ScionPageProjectSchedules extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @state() private projectId = '';
  @state() private project: Project | null = null;
  @state() private loading = true;
  @state() private error: string | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 0.5rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      font-size: 0.875rem;
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
      margin-bottom: 1rem;
    }

    .back-link:hover {
      text-decoration: underline;
    }

    .subtitle {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1.5rem 0;
    }

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 4rem 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .loading-state sl-spinner {
      font-size: 2rem;
      margin-bottom: 1rem;
    }

    .error-state {
      text-align: center;
      padding: 3rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .error-state sl-icon {
      font-size: 3rem;
      color: var(--sl-color-danger-500, #ef4444);
      margin-bottom: 1rem;
    }

    .error-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .error-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .error-details {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      color: var(--sl-color-danger-700, #b91c1c);
      margin-bottom: 1rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    this.extractProjectId();
    void this.loadProject();
  }

  private extractProjectId(): void {
    const path = this.pageData?.path || window.location.pathname;
    const match = path.match(/\/projects\/([^/]+)\/schedules/);
    if (match) {
      this.projectId = decodeURIComponent(match[1]);
    }
  }

  private async loadProject(): Promise<void> {
    if (!this.projectId) {
      this.error = 'No project ID found in URL';
      this.loading = false;
      return;
    }

    try {
      const response = await apiFetch(`/api/v1/projects/${encodeURIComponent(this.projectId)}`);
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      this.project = (await response.json()) as Project;
      dispatchPageTitle(this, 'Schedules', this.project.name || this.projectId);
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to load project';
    } finally {
      this.loading = false;
    }
  }

  override render() {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading...</p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load</h2>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${() => this.loadProject()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    const projectName = this.project?.name || this.project?.slug || this.projectId;

    return html`
      <a class="back-link" href="/projects/${encodeURIComponent(this.projectId)}">
        <sl-icon name="arrow-left"></sl-icon>
        Back to ${projectName}
      </a>

      <div class="header">
        <h1>Recurring Schedules</h1>
      </div>
      <p class="subtitle">Automated recurring tasks for ${projectName}.</p>

      <scion-schedule-list .projectId=${this.projectId}></scion-schedule-list>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-project-schedules': ScionPageProjectSchedules;
  }
}
