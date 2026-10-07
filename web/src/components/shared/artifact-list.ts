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
 * A project's artifacts (experiment hub.artifacts): the artifacts homed in
 * the project that the caller can read, newest first, with search, paging
 * and a New artifact button. A row opens the artifact's page.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { navigateTo } from '../../client/navigation.js';
import { formatInstant } from '../../utils/time.js';
import { artifactPagePath, listProjectArtifacts } from '../../client/artifacts.js';
import type { ArtifactListItem, ArtifactResponse } from '../../client/artifacts.js';
import './artifact-publish-dialog.js';

/** Delay before a search runs, so typing does not send a request per key. */
const SEARCH_DELAY_MS = 300;

@customElement('scion-artifact-list')
export class ScionArtifactList extends LitElement {
  @property({ type: String }) projectId = '';

  @state() private items: ArtifactListItem[] = [];
  @state() private cursor = '';
  @state() private loading = true;
  @state() private loadingMore = false;
  @state() private error: string | null = null;
  @state() private query = '';
  @state() private publishOpen = false;

  private searchTimer: ReturnType<typeof setTimeout> | null = null;
  private abort: AbortController | null = null;

  static override styles = css`
    :host {
      display: block;
    }
    .toolbar {
      display: flex;
      gap: 0.75rem;
      align-items: center;
      margin-bottom: 0.75rem;
    }
    .toolbar sl-input {
      flex: 1;
      max-width: 22rem;
    }
    .toolbar .spacer {
      flex: 1;
    }
    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 0.875rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
    }
    th {
      text-align: left;
      font-weight: 600;
      font-size: 0.75rem;
      text-transform: uppercase;
      letter-spacing: 0.03em;
      color: var(--sl-color-neutral-600);
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }
    td {
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }
    tbody tr {
      cursor: pointer;
    }
    tbody tr:hover {
      background: var(--scion-bg-subtle, #f8fafc);
    }
    .title {
      display: inline-flex;
      align-items: center;
      gap: 0.4rem;
      color: inherit;
      text-decoration: none;
      font-weight: 500;
    }
    .key {
      display: block;
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.75rem;
      color: var(--sl-color-neutral-500);
      margin-left: 1.4rem;
    }
    .muted {
      color: var(--sl-color-neutral-600);
    }
    .more {
      text-align: center;
      margin-top: 0.75rem;
    }
    .empty {
      text-align: center;
      padding: 2.5rem 1rem;
      color: var(--sl-color-neutral-600);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-surface, #ffffff);
    }
    .empty > sl-icon {
      font-size: 2.5rem;
      color: var(--sl-color-neutral-400);
    }
    .empty h3 {
      margin: 0.5rem 0 0.25rem;
      color: var(--scion-text, #1e293b);
    }
    .empty p {
      margin: 0 0 1rem;
    }
    .help {
      margin-top: 0.75rem;
      font-size: 0.8125rem;
    }
    .help-panel {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      box-shadow: var(--sl-shadow-large);
      padding: 0.75rem 1rem;
      max-width: 26rem;
      text-align: left;
      font-size: 0.8125rem;
    }
    .help-panel pre {
      margin: 0.4rem 0 0;
      white-space: pre-wrap;
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.75rem;
      background: var(--scion-bg-subtle, #f8fafc);
      padding: 0.5rem;
      border-radius: 0.25rem;
    }
    .error-state,
    .loading-state {
      text-align: center;
      padding: 2rem;
      color: var(--sl-color-neutral-500);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.load();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.searchTimer) clearTimeout(this.searchTimer);
    this.abort?.abort();
  }

  private async load(more = false): Promise<void> {
    if (!this.projectId) return;
    this.abort?.abort();
    const abort = new AbortController();
    this.abort = abort;
    if (more) {
      this.loadingMore = true;
    } else {
      this.loading = true;
    }
    this.error = null;
    try {
      const page = await listProjectArtifacts(this.projectId, {
        q: this.query.trim(),
        cursor: more ? this.cursor : '',
        signal: abort.signal,
      });
      this.items = more ? [...this.items, ...page.artifacts] : page.artifacts;
      this.cursor = page.nextCursor ?? '';
    } catch (err) {
      if (abort.signal.aborted) return;
      this.error = err instanceof Error ? err.message : 'Could not load artifacts';
    } finally {
      if (this.abort === abort) {
        this.loading = false;
        this.loadingMore = false;
      }
    }
  }

  private onSearch = (e: Event): void => {
    this.query = (e.target as HTMLInputElement).value;
    if (this.searchTimer) clearTimeout(this.searchTimer);
    this.searchTimer = setTimeout(() => void this.load(), SEARCH_DELAY_MS);
  };

  private open(item: ArtifactListItem): void {
    navigateTo(artifactPagePath(item.scopeRef || this.projectId, item.id));
  }

  private onPublished = (e: CustomEvent<ArtifactResponse>): void => {
    this.publishOpen = false;
    const a = e.detail.artifact;
    navigateTo(artifactPagePath(a.scopeRef || this.projectId, a.id));
  };

  private owner(item: ArtifactListItem): string {
    const ref = item.ownerRef.length > 12 ? `${item.ownerRef.slice(0, 8)}…` : item.ownerRef;
    return `${item.ownerKind} ${ref}`;
  }

  private newButton(): TemplateResult {
    return html`<sl-button
      size="small"
      variant="primary"
      @click=${(): void => {
        this.publishOpen = true;
      }}
    >
      <sl-icon slot="prefix" name="plus-lg"></sl-icon>
      New artifact
    </sl-button>`;
  }

  private renderEmpty(): TemplateResult {
    if (this.query.trim()) {
      return html`<div class="empty">No artifacts match "${this.query.trim()}".</div>`;
    }
    return html`
      <div class="empty">
        <sl-icon name="file-earmark-richtext"></sl-icon>
        <h3>No artifacts in this project yet</h3>
        <p>
          Documents, reports and small sites you or your agents publish appear here, with versions.
        </p>
        ${this.newButton()}
        <div class="help">
          <sl-dropdown placement="bottom" distance="6">
            <sl-button slot="trigger" variant="text" size="small">
              <sl-icon slot="prefix" name="question-circle"></sl-icon>
              How agents publish
            </sl-button>
            <div class="help-panel">
              An agent in this project can publish from its workspace:
              <pre>scion artifact publish ./report --title "Weekly report"</pre>
            </div>
          </sl-dropdown>
        </div>
      </div>
    `;
  }

  private renderTable(): TemplateResult {
    return html`
      <table>
        <thead>
          <tr>
            <th>Title</th>
            <th>Owner</th>
            <th>Version</th>
            <th>Updated</th>
            <th>Review</th>
          </tr>
        </thead>
        <tbody>
          ${this.items.map(
            (item) => html`
              <tr @click=${(): void => this.open(item)}>
                <td>
                  <a
                    class="title"
                    href=${artifactPagePath(item.scopeRef || this.projectId, item.id)}
                    @click=${(e: MouseEvent): void => {
                      e.preventDefault();
                      e.stopPropagation();
                      this.open(item);
                    }}
                  >
                    <sl-icon name="file-earmark-richtext"></sl-icon>
                    ${item.title}
                  </a>
                  ${item.key ? html`<span class="key">${item.key}</span>` : nothing}
                </td>
                <td>${this.owner(item)}</td>
                <td>v${item.currentSeq}</td>
                <td class="muted">${formatInstant(item.updatedAt)}</td>
                <td>
                  ${item.reviewPending
                    ? html`<sl-badge pill variant="warning">Review pending</sl-badge>`
                    : nothing}
                </td>
              </tr>
            `
          )}
        </tbody>
      </table>
      ${this.cursor
        ? html`<div class="more">
            <sl-button
              size="small"
              ?loading=${this.loadingMore}
              @click=${(): void => void this.load(true)}
              >Load more</sl-button
            >
          </div>`
        : nothing}
    `;
  }

  override render(): TemplateResult {
    let body: TemplateResult;
    if (this.loading) {
      body = html`<div class="loading-state"><sl-spinner></sl-spinner></div>`;
    } else if (this.error) {
      body = html`<div class="error-state">
        <p>${this.error}</p>
        <sl-button size="small" @click=${(): void => void this.load()}>Retry</sl-button>
      </div>`;
    } else if (this.items.length === 0) {
      body = this.renderEmpty();
    } else {
      body = this.renderTable();
    }
    return html`
      <div class="toolbar">
        <sl-input
          size="small"
          placeholder="Search title or key"
          clearable
          .value=${this.query}
          @sl-input=${this.onSearch}
          @sl-clear=${this.onSearch}
        >
          <sl-icon slot="prefix" name="search"></sl-icon>
        </sl-input>
        <span class="spacer"></span>
        ${this.newButton()}
      </div>
      ${body}
      <scion-artifact-publish-dialog
        .projectId=${this.projectId}
        ?open=${this.publishOpen}
        @artifact-published=${this.onPublished}
        @artifact-publish-closed=${(): void => {
          this.publishOpen = false;
        }}
      ></scion-artifact-publish-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-artifact-list': ScionArtifactList;
  }
}
