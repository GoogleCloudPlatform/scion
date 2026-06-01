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
  }

  private async handleImport(): Promise<void> {
    this.loading = true;
    this.error = null;
    this.success = null;

    try {
      let endpoint: string;
      let body: Record<string, string>;

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

      const response = await apiFetch(endpoint, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `Failed to import ${this.noun}: HTTP ${response.status}`)
        );
      }

      const data = (await response.json()) as { count: number };
      const count = data.count ?? 0;
      const singular = this.kind === 'template' ? 'template' : 'harness-config';
      this.success = `${count} ${singular}${count !== 1 ? 's' : ''} imported successfully.`;
      this.dispatchEvent(
        new CustomEvent('resource-imported', {
          detail: { count },
          bubbles: true,
          composed: true,
        })
      );
    } catch (err) {
      console.error(`Failed to import ${this.noun}:`, err);
      this.error = err instanceof Error ? err.message : `Failed to import ${this.noun}`;
    } finally {
      this.loading = false;
    }
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

      ${this.loading
        ? html`<div class="sync-status">
            <sl-spinner style="font-size: 0.875rem;"></sl-spinner>
            ${this.mode === 'workspace'
              ? `Importing ${this.noun} from workspace ${this.source || this.defaultWorkspacePath}...`
              : `Importing ${this.noun} from ${this.source}...`}
          </div>`
        : ''}
      ${this.error
        ? html`<div class="sync-status error">
            <sl-icon name="exclamation-triangle"></sl-icon>${this.error}
          </div>`
        : ''}
      ${this.success
        ? html`<div class="sync-status success">
            <sl-icon name="check-circle"></sl-icon>${this.success}
          </div>`
        : ''}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-resource-import': ScionResourceImport;
  }
}
