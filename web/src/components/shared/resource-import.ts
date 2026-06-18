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
 * Shared resource import form
 *
 * Renders the import affordance (mode toggle + source input + button + status)
 * for file-based resources (templates / harness-configs). Used by both the
 * project settings Resources section and the Hub Resources page so the import UI
 * is defined once.
 *
 * - Project scope posts to the per-project endpoint and supports both URL and
 *   workspace-path modes.
 * - Global scope posts to the unified `/api/v1/resources/import` endpoint and is
 *   URL-only (no project workspace to resolve).
 *
 * On a successful import it dispatches a `resource-imported` CustomEvent (with
 * `{ count }` detail) so the host page can refresh its resource list.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';

export type ResourceImportKind = 'template' | 'harness-config';

/** One progress event from the streaming (NDJSON) import endpoints. */
interface ImportEvent {
  type: 'discovered' | 'started' | 'completed' | 'failed' | 'skipped' | 'done' | 'error';
  name?: string;
  reason?: string;
  completed?: number;
  total?: number;
  names?: string[];
  imported?: string[];
  skipped?: string[];
  failed?: string[];
}

/** Live progress state rendered while a streaming import is in flight. */
interface ImportProgress {
  total: number;
  completed: number;
  inFlight: string[];
}

/** Final per-resource summary after a streaming import completes. */
interface ImportSummary {
  imported: string[];
  skipped: string[];
  failed: string[];
}

@customElement('scion-resource-import')
export class ScionResourceImport extends LitElement {
  /** Which resource type to import. */
  @property({ type: String })
  kind: ResourceImportKind = 'template';

  /** Resource scope: 'project' or 'global'. */
  @property({ type: String })
  scope: 'project' | 'global' = 'project';

  /** Scope id (project id) — required for project scope, omitted for global. */
  @property({ type: String })
  scopeId = '';

  /** Whether the workspace-path import mode is offered (project scope only). */
  @property({ type: Boolean })
  allowWorkspace = false;

  /** Whether the caller may import — gates the whole form when false. */
  @property({ type: Boolean })
  canImport = false;

  /** Optional URL prefill (e.g. the project's git remote). */
  @property({ type: String })
  gitRemote = '';

  @state() private mode: 'url' | 'workspace' = 'url';
  @state() private source = '';
  @state() private loading = false;
  @state() private error: string | null = null;
  @state() private success: string | null = null;
  @state() private progress: ImportProgress | null = null;
  @state() private summary: ImportSummary | null = null;
  @state() private discoveredNames: string[] = [];
  @state() private showSelectionDialog = false;
  @state() private selectedNames: Set<string> = new Set();

  static override styles = css`
    :host {
      display: block;
      margin-bottom: 1rem;
    }

    .header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      gap: 1rem;
      margin-bottom: 1rem;
    }

    .header p {
      margin: 0;
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .controls {
      margin-bottom: 1rem;
    }

    .hint {
      margin-top: 0.25rem;
      font-size: 0.75rem;
      color: var(--sl-color-neutral-500, #64748b);
    }

    .sync-status {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.875rem;
      padding: 0.5rem 0;
    }

    .sync-status.error {
      color: var(--sl-color-danger-600, #dc2626);
    }

    .sync-status.success {
      color: var(--sl-color-success-600, #16a34a);
    }

    .progress {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
      padding: 0.5rem 0;
    }

    .progress-label {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.875rem;
    }

    sl-progress-bar {
      --height: 6px;
    }

    .selection-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding-bottom: 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      margin-bottom: 0.75rem;
    }

    .selection-count {
      font-size: 0.75rem;
      color: var(--sl-color-neutral-500, #64748b);
    }

    .selection-list {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
      max-height: 400px;
      overflow-y: auto;
    }

    .selection-item {
      padding: 0.25rem 0;
    }
  `;

  private get noun(): string {
    return this.kind === 'template' ? 'templates' : 'harness-configs';
  }

  private get label(): string {
    return this.kind === 'template' ? 'Templates' : 'Harness Configs';
  }

  private get defaultWorkspacePath(): string {
    return this.kind === 'template' ? '/.scion/templates' : '/.scion/harness-configs';
  }

  private get placeholder(): string {
    if (this.mode === 'workspace') return this.defaultWorkspacePath;
    return `https://github.com/org/repo/tree/main/${this.defaultWorkspacePath.replace(/^\//, '')}`;
  }

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.scope === 'project' && this.gitRemote && !this.source) {
      this.source = this.gitRemote;
    }
  }

  private onModeChange(mode: 'url' | 'workspace'): void {
    this.mode = mode;
    this.source = mode === 'url' && this.gitRemote ? this.gitRemote : '';
    this.error = null;
    this.success = null;
    this.progress = null;
    this.summary = null;
  }

  // Discover-then-import: the remote URL is fetched twice (once for discovery,
  // once for the actual import) to keep the API stateless. GitHub's CDN caching
  // makes the second fetch fast in practice; a server-side cache token could
  // eliminate it later if needed.
  private async handleImport(): Promise<void> {
    this.loading = true;
    this.error = null;
    this.success = null;
    this.progress = null;
    this.summary = null;

    try {
      let discoverEndpoint: string;
      let discoverBody: Record<string, string>;

      if (this.scope === 'global') {
        discoverEndpoint = '/api/v1/resources/discover';
        discoverBody = { kind: this.kind, scope: 'global', sourceUrl: this.source };
      } else {
        const path =
          this.kind === 'template' ? 'discover-templates' : 'discover-harness-configs';
        discoverEndpoint = `/api/v1/projects/${this.scopeId}/${path}`;
        discoverBody =
          this.mode === 'workspace'
            ? { workspacePath: this.source || this.defaultWorkspacePath }
            : { sourceUrl: this.source };
      }

      const discoverResponse = await apiFetch(discoverEndpoint, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(discoverBody),
      });

      if (!discoverResponse.ok) {
        throw new Error(
          await extractApiError(discoverResponse, `Failed to discover ${this.noun}`)
        );
      }

      const discovered = (await discoverResponse.json()) as {
        resources: string[];
        count: number;
      };

      if (discovered.count === 0) {
        this.error = `No ${this.noun} found at the specified location`;
        return;
      }

      if (discovered.count === 1) {
        await this.executeImport(discovered.resources);
        return;
      }

      this.discoveredNames = discovered.resources;
      this.selectedNames = new Set(discovered.resources);
      this.showSelectionDialog = true;
    } catch (err) {
      console.error(`Failed to discover ${this.noun}:`, err);
      this.error = err instanceof Error ? err.message : `Failed to discover ${this.noun}`;
    } finally {
      this.loading = false;
    }
  }

  private async executeImport(names?: string[]): Promise<void> {
    this.loading = true;
    this.error = null;
    this.success = null;
    this.progress = null;
    this.summary = null;

    try {
      let endpoint: string;
      let body: Record<string, unknown>;

      if (this.scope === 'global') {
        endpoint = '/api/v1/resources/import';
        body = { kind: this.kind, scope: 'global', sourceUrl: this.source };
      } else {
        const path = this.kind === 'template' ? 'import-templates' : 'import-harness-configs';
        endpoint = `/api/v1/projects/${this.scopeId}/${path}`;
        body =
          this.mode === 'workspace'
            ? { workspacePath: this.source || this.defaultWorkspacePath }
            : { sourceUrl: this.source };
      }

      if (names && names.length > 0) {
        body.names = names;
      }

      const response = await apiFetch(endpoint, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Accept: 'application/x-ndjson' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `Failed to import ${this.noun}: HTTP ${response.status}`)
        );
      }

      const contentType = response.headers.get('Content-Type') ?? '';
      if (contentType.includes('application/x-ndjson') && response.body) {
        await this.consumeStream(response.body);
      } else {
        const data = (await response.json()) as { count?: number; imported?: string[] };
        this.finishSummary({ imported: data.imported ?? [], skipped: [], failed: [] }, data.count);
      }
    } catch (err) {
      console.error(`Failed to import ${this.noun}:`, err);
      this.error = err instanceof Error ? err.message : `Failed to import ${this.noun}`;
    } finally {
      this.loading = false;
      this.progress = null;
    }
  }

  private async handleSelectionConfirm(): Promise<void> {
    const names = Array.from(this.selectedNames);
    this.showSelectionDialog = false;
    await this.executeImport(names);
  }

  /** Read an NDJSON stream of {@link ImportEvent}s, updating progress state. */
  private async consumeStream(stream: ReadableStream<Uint8Array>): Promise<void> {
    const reader = stream.getReader();
    const decoder = new TextDecoder();
    let buffer = '';

    const dispatchLine = (line: string): void => {
      const trimmed = line.trim();
      if (trimmed) this.handleEvent(JSON.parse(trimmed) as ImportEvent);
    };

    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let nl: number;
      while ((nl = buffer.indexOf('\n')) >= 0) {
        dispatchLine(buffer.slice(0, nl));
        buffer = buffer.slice(nl + 1);
      }
    }
    // Flush any bytes still held inside the decoder (e.g. a trailing multi-byte
    // UTF-8 sequence split across reads) before dispatching the final line.
    buffer += decoder.decode();
    dispatchLine(buffer);
  }

  /** Fold one streamed import event into the live progress / summary state. */
  private handleEvent(ev: ImportEvent): void {
    switch (ev.type) {
      case 'discovered':
        this.progress = { total: ev.total ?? 0, completed: 0, inFlight: [] };
        break;
      case 'started':
        if (this.progress && ev.name) {
          this.progress = { ...this.progress, inFlight: [...this.progress.inFlight, ev.name] };
        }
        break;
      case 'completed':
      case 'failed':
        if (this.progress) {
          this.progress = {
            ...this.progress,
            completed: ev.completed ?? this.progress.completed,
            inFlight: this.progress.inFlight.filter((n) => n !== ev.name),
          };
        }
        break;
      case 'done':
        this.finishSummary({
          imported: ev.imported ?? [],
          skipped: ev.skipped ?? [],
          failed: ev.failed ?? [],
        });
        break;
      case 'error':
        this.error = ev.reason ?? `Failed to import ${this.noun}`;
        break;
      // 'skipped' is folded into the final summary's `skipped` list via 'done'.
    }
  }

  /** Record the final summary and notify the host to refresh its list. */
  private finishSummary(summary: ImportSummary, countOverride?: number): void {
    this.summary = summary;
    const count = countOverride ?? summary.imported.length;
    const singular = this.kind === 'template' ? 'template' : 'harness-config';
    let msg = `${count} ${singular}${count !== 1 ? 's' : ''} imported successfully.`;
    if (summary.skipped.length > 0) msg += ` ${summary.skipped.length} skipped.`;
    if (summary.failed.length > 0) msg += ` ${summary.failed.length} failed.`;
    this.success = msg;
    this.dispatchEvent(
      new CustomEvent('resource-changed', {
        detail: { action: 'imported', kind: this.kind, count },
        bubbles: true,
        composed: true,
      })
    );
  }

  override render() {
    if (!this.canImport) return html``;

    const importDisabled = this.loading || (this.mode === 'url' && !this.source);

    return html`
      <div class="header">
        <sl-button
          size="small"
          variant="default"
          ?loading=${this.loading}
          ?disabled=${importDisabled}
          @click=${() => this.handleImport()}
        >
          <sl-icon slot="prefix" name="download"></sl-icon>
          Import ${this.label}
        </sl-button>
      </div>

      <div class="controls">
        ${this.allowWorkspace
          ? html`
              <sl-radio-group
                size="small"
                value=${this.mode}
                style="margin-bottom: 0.5rem;"
                @sl-change=${(e: Event) =>
                  this.onModeChange((e.target as HTMLInputElement).value as 'url' | 'workspace')}
              >
                <sl-radio-button value="url">Import from URL</sl-radio-button>
                <sl-radio-button value="workspace">Import from workspace</sl-radio-button>
              </sl-radio-group>
            `
          : ''}
        <sl-input
          placeholder=${this.placeholder}
          size="small"
          clearable
          .value=${this.source}
          ?disabled=${this.loading}
          @sl-input=${(e: Event) => {
            this.source = (e.target as HTMLInputElement).value;
          }}
          @sl-clear=${() => {
            this.source = '';
          }}
        >
          <sl-icon slot="prefix" name=${this.mode === 'workspace' ? 'folder' : 'github'}></sl-icon>
        </sl-input>
        <div class="hint">
          ${this.mode === 'workspace'
            ? 'Path within the project workspace — the default will be used if no path is provided'
            : `GitHub URL to a ${this.kind} or ${this.noun} directory — supports arbitrary deep paths`}
        </div>
      </div>

      ${this.loading ? this.renderProgress() : ''}
      ${this.error
        ? html`<div class="sync-status error">
            <sl-icon name="exclamation-triangle"></sl-icon>${this.error}
          </div>`
        : ''}
      ${!this.loading && this.success
        ? html`<div class="sync-status success">
              <sl-icon name="check-circle"></sl-icon>${this.success}
            </div>
            ${this.renderSummaryDetail()}`
        : ''}
      ${this.renderSelectionDialog()}
    `;
  }

  private renderSelectionDialog() {
    if (!this.showSelectionDialog) return '';

    const allSelected = this.selectedNames.size === this.discoveredNames.length;
    const noneSelected = this.selectedNames.size === 0;

    return html`
      <sl-dialog
        label="Select ${this.label} to Import"
        open
        @sl-request-close=${() => {
          this.showSelectionDialog = false;
        }}
      >
        <div class="selection-header">
          <sl-checkbox
            ?checked=${allSelected}
            ?indeterminate=${!allSelected && !noneSelected}
            @sl-change=${(e: Event) => {
              const checked = (e.target as HTMLInputElement).checked;
              this.selectedNames = checked
                ? new Set(this.discoveredNames)
                : new Set();
              this.requestUpdate();
            }}
          >
            Select All
          </sl-checkbox>
          <span class="selection-count">
            ${this.selectedNames.size} of ${this.discoveredNames.length} selected
          </span>
        </div>
        <div class="selection-list">
          ${this.discoveredNames.map(
            (name) => html`
              <div class="selection-item">
                <sl-checkbox
                  ?checked=${this.selectedNames.has(name)}
                  @sl-change=${(e: Event) => {
                    const checked = (e.target as HTMLInputElement).checked;
                    const updated = new Set(this.selectedNames);
                    if (checked) updated.add(name);
                    else updated.delete(name);
                    this.selectedNames = updated;
                  }}
                >
                  ${name}
                </sl-checkbox>
              </div>
            `
          )}
        </div>
        <div slot="footer">
          <sl-button
            variant="default"
            @click=${() => {
              this.showSelectionDialog = false;
            }}
          >
            Cancel
          </sl-button>
          <sl-button
            variant="primary"
            ?disabled=${noneSelected}
            @click=${() => this.handleSelectionConfirm()}
          >
            <sl-icon slot="prefix" name="download"></sl-icon>
            Import Selected (${this.selectedNames.size})
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  /** Render the skipped/failed resource names from the final summary, if any. */
  private renderSummaryDetail() {
    const s = this.summary;
    if (!s || (s.skipped.length === 0 && s.failed.length === 0)) return '';
    return html`
      <div class="hint">
        ${s.failed.length > 0 ? html`<div>Failed: ${s.failed.join(', ')}</div>` : ''}
        ${s.skipped.length > 0 ? html`<div>Skipped: ${s.skipped.join(', ')}</div>` : ''}
      </div>
    `;
  }

  /** Render the in-flight import status: a per-resource progress bar once the
   * resource list is known, or an indeterminate spinner during fetch/discovery. */
  private renderProgress() {
    const p = this.progress;
    if (!p || p.total === 0) {
      return html`<div class="sync-status">
        <sl-spinner style="font-size: 0.875rem;"></sl-spinner>
        ${this.mode === 'workspace'
          ? `Importing ${this.noun} from workspace ${this.source || this.defaultWorkspacePath}...`
          : `Fetching ${this.noun} from ${this.source}...`}
      </div>`;
    }

    const pct = Math.round((p.completed / p.total) * 100);
    const current = p.inFlight.length > 0 ? p.inFlight.join(', ') : '…';
    return html`
      <div class="progress">
        <div class="progress-label">
          <sl-spinner style="font-size: 0.875rem;"></sl-spinner>
          <span>Importing ${current} (${p.completed}/${p.total})…</span>
        </div>
        <sl-progress-bar value=${pct}></sl-progress-bar>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-resource-import': ScionResourceImport;
  }
}
