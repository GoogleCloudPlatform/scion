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

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state, property } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';

interface DirEntry {
  name: string;
  isDir: boolean;
  isGit?: boolean;
}

interface DirListResponse {
  path: string;
  entries: DirEntry[];
}

@customElement('scion-dir-browser')
export class ScionDirBrowser extends LitElement {
  @property({ type: String })
  selectedPath = '';

  @state() private currentPath = '';
  @state() private entries: DirEntry[] = [];
  @state() private loading = false;
  @state() private error: string | null = null;
  @state() private filterText = '';
  @state() private newFolderMode = false;
  @state() private newFolderName = '';
  @state() private newFolderError: string | null = null;
  @state() private creatingFolder = false;

  static override styles = css`
    :host {
      display: block;
    }

    .browser {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      overflow: hidden;
    }

    .breadcrumb-bar {
      display: flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.5rem 0.75rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      font-size: 0.8125rem;
      flex-wrap: wrap;
    }

    .breadcrumb-segment {
      color: var(--scion-primary, #3b82f6);
      cursor: pointer;
      border: none;
      background: none;
      padding: 0.125rem 0.25rem;
      border-radius: 0.25rem;
      font-size: inherit;
      font-family: inherit;
    }

    .breadcrumb-segment:hover {
      background: var(--sl-color-primary-100, #dbeafe);
    }

    .breadcrumb-sep {
      color: var(--scion-text-muted, #64748b);
    }

    .filter-input {
      flex: 1;
      min-width: 6rem;
    }

    .filter-input::part(base) {
      border: none;
      background: transparent;
      box-shadow: none;
    }

    .entry-list {
      max-height: 18rem;
      overflow-y: auto;
    }

    .entry {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      cursor: pointer;
      font-size: 0.875rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .entry:last-child {
      border-bottom: none;
    }

    .entry:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .entry.is-file {
      opacity: 0.45;
      cursor: default;
    }

    .entry sl-icon {
      font-size: 1rem;
      flex-shrink: 0;
    }

    .entry .name {
      flex: 1;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .entry .badge {
      font-size: 0.6875rem;
      padding: 0.0625rem 0.375rem;
      border-radius: 9999px;
      background: var(--sl-color-neutral-100, #f1f5f9);
      color: var(--sl-color-neutral-600, #475569);
    }

    .toolbar {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .new-folder-row {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .new-folder-row sl-input {
      flex: 1;
    }

    .error-msg {
      color: var(--sl-color-danger-700, #b91c1c);
      font-size: 0.75rem;
      padding: 0.25rem 0.75rem;
    }

    .empty-state {
      padding: 2rem 1rem;
      text-align: center;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }

    .loading-state {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 2rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.navigate('');
  }

  private async navigate(path: string): Promise<void> {
    this.loading = true;
    this.error = null;
    this.filterText = '';
    this.newFolderMode = false;

    try {
      const params = path ? `?path=${encodeURIComponent(path)}` : '';
      const res = await apiFetch(`/api/v1/system/fs/list${params}`);
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to list directory');
        return;
      }
      const data = (await res.json()) as DirListResponse;
      this.currentPath = data.path.replace(/\\/g, '/');
      this.entries = data.entries ?? [];
    } catch {
      this.error = 'Failed to connect to the server.';
    } finally {
      this.loading = false;
    }
  }

  private onEntryClick(entry: DirEntry): void {
    if (!entry.isDir) return;
    void this.navigate(this.joinPath(this.currentPath, entry.name));
  }

  private navigateUp(): void {
    const lastSlash = this.currentPath.lastIndexOf('/');
    if (lastSlash < 0) return;
    let parent = lastSlash === 0 ? '/' : this.currentPath.substring(0, lastSlash);
    if (/^[a-zA-Z]:$/.test(parent)) {
      parent += '/';
    }
    void this.navigate(parent);
  }

  private navigateToBreadcrumb(index: number): void {
    const segments = this.currentPath.split('/').filter(Boolean);
    const subSegments = segments.slice(0, index + 1);
    let path = '';
    if (subSegments[0] && /^[a-zA-Z]:$/.test(subSegments[0])) {
      path = subSegments.join('/');
      if (subSegments.length === 1) {
        path += '/';
      }
    } else {
      path = '/' + subSegments.join('/');
    }
    void this.navigate(path);
  }

  private selectCurrentPath(): void {
    this.selectedPath = this.currentPath;
    this.dispatchEvent(new CustomEvent('path-selected', {
      detail: { path: this.currentPath },
      bubbles: true,
      composed: true,
    }));
  }

  private async handleNewFolder(): Promise<void> {
    const name = this.newFolderName.trim();
    if (!name) {
      this.newFolderError = 'Folder name is required.';
      return;
    }

    this.creatingFolder = true;
    this.newFolderError = null;

    try {
      const res = await apiFetch('/api/v1/system/fs/mkdir', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ parent: this.currentPath, name }),
      });
      if (!res.ok) {
        this.newFolderError = await extractApiError(res, 'Failed to create folder');
        return;
      }
      this.newFolderMode = false;
      this.newFolderName = '';
      void this.navigate(this.joinPath(this.currentPath, name));
    } catch {
      this.newFolderError = 'Failed to connect to the server.';
    } finally {
      this.creatingFolder = false;
    }
  }

  private get filteredEntries(): DirEntry[] {
    if (!this.filterText) return this.entries;
    const lower = this.filterText.toLowerCase();
    return this.entries.filter(e => e.name.toLowerCase().includes(lower));
  }

  private onFilterInput(e: Event): void {
    this.filterText = (e.target as HTMLInputElement).value;
  }

  private onFilterKeydown(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      this.filterText = '';
      return;
    }
    if (e.key === 'Tab') {
      e.preventDefault();
      const dirMatches = this.filteredEntries.filter(entry => entry.isDir);
      if (dirMatches.length === 1) {
        this.filterText = '';
        void this.navigate(this.joinPath(this.currentPath, dirMatches[0].name));
      } else if (dirMatches.length > 1) {
        const prefix = this.commonPrefix(dirMatches.map(d => d.name));
        if (prefix.length > this.filterText.length) {
          this.filterText = prefix;
        }
      }
      return;
    }
    if (e.key === 'Enter') {
      const matches = this.filteredEntries.filter(entry => entry.isDir);
      if (matches.length === 1) {
        this.filterText = '';
        void this.navigate(this.joinPath(this.currentPath, matches[0].name));
      }
    }
  }

  private joinPath(base: string, name: string): string {
    return base.endsWith('/') ? base + name : base + '/' + name;
  }

  private commonPrefix(strings: string[]): string {
    if (strings.length === 0) return '';
    let prefix = strings[0];
    for (let i = 1; i < strings.length; i++) {
      while (!strings[i].toLowerCase().startsWith(prefix.toLowerCase())) {
        prefix = prefix.slice(0, -1);
        if (!prefix) return '';
      }
    }
    return prefix;
  }

  override render() {
    const segments = this.currentPath.split('/').filter(Boolean);

    return html`
      <div class="browser">
        <div class="breadcrumb-bar">
          <button class="breadcrumb-segment" @click=${() => void this.navigate('/')}>
            <sl-icon name="house"></sl-icon>
          </button>
          ${segments.map((seg, i) => html`
            <span class="breadcrumb-sep">/</span>
            <button class="breadcrumb-segment" @click=${() => this.navigateToBreadcrumb(i)}>${seg}</button>
          `)}
          <sl-input
            class="filter-input"
            size="small"
            placeholder="Type to filter..."
            .value=${this.filterText}
            @sl-input=${(e: Event) => this.onFilterInput(e)}
            @keydown=${(e: KeyboardEvent) => this.onFilterKeydown(e)}
          ></sl-input>
        </div>

        ${this.loading ? html`
          <div class="loading-state"><sl-spinner></sl-spinner></div>
        ` : this.error ? html`
          <div class="empty-state">${this.error}</div>
        ` : this.entries.length === 0 ? html`
          <div class="empty-state">Empty directory</div>
        ` : html`
          ${this.entries.length > 5 ? html`
            <div style="padding: 0.375rem 0.75rem; border-bottom: 1px solid var(--scion-border, #e2e8f0);">
              <sl-input
                size="small"
                placeholder="Filter…"
                clearable
                .value=${this.filterText}
                @sl-input=${this.onFilterInput}
                @keydown=${this.onFilterKeydown}
              >
                <sl-icon slot="prefix" name="funnel"></sl-icon>
              </sl-input>
            </div>
          ` : nothing}
          <div class="entry-list">
            ${!(segments.length === 0 || (segments.length === 1 && /^[a-zA-Z]:$/.test(segments[0]))) ? html`
              <div class="entry" @click=${() => this.navigateUp()}>
                <sl-icon name="arrow-up"></sl-icon>
                <span class="name">..</span>
              </div>
            ` : nothing}
            ${this.filteredEntries.length === 0 && this.filterText ? html`
              <div class="empty-state">No matches for "${this.filterText}"</div>
            ` : nothing}
            ${this.filteredEntries.map(e => html`
              <div class="entry ${e.isDir ? '' : 'is-file'}" @click=${() => this.onEntryClick(e)}>
                <sl-icon name=${e.isDir ? (e.isGit ? 'git' : 'folder') : 'file-earmark'}></sl-icon>
                <span class="name">${e.name}</span>
                ${e.isGit ? html`<span class="badge">git</span>` : nothing}
              </div>
            `)}
          </div>
        `}

        ${this.newFolderMode ? html`
          <div class="new-folder-row">
            <sl-input
              size="small"
              placeholder="New folder name"
              .value=${this.newFolderName}
              @sl-input=${(e: Event) => { this.newFolderName = (e.target as HTMLInputElement).value; }}
              @keydown=${(e: KeyboardEvent) => { if (e.key === 'Enter') void this.handleNewFolder(); }}
            ></sl-input>
            <sl-button size="small" variant="primary" ?loading=${this.creatingFolder} @click=${() => void this.handleNewFolder()}>
              Create
            </sl-button>
            <sl-button size="small" variant="default" @click=${() => { this.newFolderMode = false; }}>
              Cancel
            </sl-button>
          </div>
          ${this.newFolderError ? html`<div class="error-msg">${this.newFolderError}</div>` : nothing}
        ` : nothing}

        <div class="toolbar">
          <sl-button size="small" variant="default" @click=${() => { this.newFolderMode = true; this.newFolderName = ''; this.newFolderError = null; }}>
            <sl-icon slot="prefix" name="folder-plus"></sl-icon>
            New folder
          </sl-button>
          <div style="flex: 1;"></div>
          <sl-button size="small" variant="primary" @click=${() => this.selectCurrentPath()}>
            Select this folder
          </sl-button>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-dir-browser': ScionDirBrowser;
  }
}
