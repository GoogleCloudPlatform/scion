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
 * Experiments tab (ptone/scion#2217): `<scion-admin-experiments>`.
 *
 * Self-contained — owns its own fetch, state and saves. Embedded as a child
 * of the Experiments panel in `admin-server-config.ts`, which sets `.active`
 * to true only while that panel is shown. The component lazy-loads on the
 * first time `active` becomes true, so visiting another tab never calls the
 * admin endpoint.
 *
 * Writes are strictly sequential: every switch and reset button on the tab
 * is disabled while a write is pending, so a second write always carries the
 * revision the previous one returned.
 */

import { LitElement, html, css, nothing, type PropertyValues } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch, extractApiError, parseApiError } from '../../client/api.js';

// ---------------------------------------------------------------------------
// Types — mirror pkg/hub/admin_experiments.go response shapes exactly.
// ---------------------------------------------------------------------------

interface ExperimentEntry {
  name: string;
  title: string;
  description: string;
  layers: string[];
  stage: string;
  default: boolean;
  override: boolean | null;
  enabled: boolean;
  issue: string;
  owner: string;
  review_by: string;
  review_overdue: boolean;
}

interface AdminExperimentsResponse {
  revision: number;
  malformed: boolean;
  experiments: ExperimentEntry[];
  unknown_overrides: Record<string, boolean>;
  updated_at: string | null;
  updated_by: string | null;
}

/** Builds a GitHub issue URL from an "owner/repo#123" reference. Returns null if it doesn't parse. */
function issueUrl(issue: string): string | null {
  const m = /^([^/\s]+\/[^/#\s]+)#(\d+)$/.exec(issue);
  return m ? `https://github.com/${m[1]}/issues/${m[2]}` : null;
}

@customElement('scion-admin-experiments')
export class ScionAdminExperiments extends LitElement {
  /** Set by the parent tab panel; the component fetches on the first `true`. */
  @property({ type: Boolean }) active = false;

  @state() private loaded = false;
  @state() private loading = false;
  @state() private forbidden = false;
  @state() private loadError: string | null = null;
  @state() private writeError: string | null = null;
  @state() private pending = false;
  @state() private showResetDialog = false;

  @state() private experiments: ExperimentEntry[] = [];
  @state() private revision = 0;
  @state() private malformed = false;
  @state() private unknownOverrides: Record<string, boolean> = {};
  @state() private updatedAt: string | null = null;
  @state() private updatedBy: string | null = null;

  static override styles = css`
    :host {
      display: block;
    }
    .note,
    .attribution,
    .empty,
    .caption {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }
    sl-alert {
      margin-bottom: 1rem;
    }
    .row {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1rem;
      margin-bottom: 0.75rem;
    }
    .row-header {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      flex-wrap: wrap;
      margin-bottom: 0.25rem;
    }
    .row-header .title {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }
    .row-header .name {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }
    .description {
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      margin: 0.25rem 0;
    }
    .row-controls {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-top: 0.5rem;
      flex-wrap: wrap;
    }
  `;

  protected override updated(changed: PropertyValues): void {
    if (changed.has('active') && this.active && !this.loaded && !this.loading) {
      this.loaded = true;
      void this.load();
    }
  }

  private async load(): Promise<void> {
    this.loading = true;
    this.loadError = null;
    this.forbidden = false;
    try {
      const res = await apiFetch('/api/v1/admin/experiments', {
        suppressAccessDeniedToast: true,
      });
      if (res.status === 403) {
        this.forbidden = true;
        return;
      }
      if (!res.ok) {
        this.loadError = await extractApiError(res, `Failed to load experiments (HTTP ${res.status})`);
        return;
      }
      const data = (await res.json()) as AdminExperimentsResponse;
      this.applyResponse(data);
    } catch {
      this.loadError = 'Failed to load experiments.';
    } finally {
      this.loading = false;
    }
  }

  private applyResponse(data: AdminExperimentsResponse): void {
    this.experiments = data.experiments ?? [];
    this.revision = data.revision ?? 0;
    this.malformed = data.malformed ?? false;
    this.unknownOverrides = data.unknown_overrides ?? {};
    this.updatedAt = data.updated_at ?? null;
    this.updatedBy = data.updated_by ?? null;
  }

  /** `value` is the new override: `true`/`false` sets it, `null` resets to default. */
  private async setOverride(name: string, value: boolean | null): Promise<void> {
    if (this.pending) return;
    this.pending = true;
    this.writeError = null;
    const previous = this.experiments;
    // Optimistic update.
    this.experiments = this.experiments.map((e) =>
      e.name === name ? { ...e, override: value, enabled: value ?? e.default } : e
    );

    try {
      const res = await apiFetch('/api/v1/admin/experiments', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          overrides: { [name]: value },
          expected_revision: this.revision,
        }),
      });

      if (res.status === 409) {
        const info = await parseApiError(res, 'The experiment settings were modified concurrently.');
        this.experiments = previous;
        this.writeError =
          info.code === 'experiments_malformed'
            ? info.message
            : 'Changed by another administrator. Reloading current values.';
        await this.load();
        return;
      }

      if (!res.ok) {
        this.writeError = await extractApiError(res, 'Failed to update experiment');
        this.experiments = previous;
        await this.load();
        return;
      }

      const data = (await res.json()) as AdminExperimentsResponse;
      this.applyResponse(data);
    } catch {
      this.writeError = 'Failed to update experiment';
      this.experiments = previous;
      await this.load();
    } finally {
      this.pending = false;
    }
  }

  private async resetAllMalformed(): Promise<void> {
    if (this.pending) return;
    this.pending = true;
    this.writeError = null;
    try {
      const res = await apiFetch('/api/v1/admin/experiments', {
        method: 'DELETE',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ confirm_reset_malformed: true }),
      });
      if (!res.ok) {
        this.writeError = await extractApiError(res, 'Failed to reset experiments');
        await this.load();
        return;
      }
      const data = (await res.json()) as AdminExperimentsResponse;
      this.applyResponse(data);
    } catch {
      this.writeError = 'Failed to reset experiments';
      await this.load();
    } finally {
      this.pending = false;
      this.showResetDialog = false;
    }
  }

  override render() {
    if (this.forbidden) {
      return html`
        <sl-alert variant="warning" open>
          You do not have permission to manage experiments. Ask a hub administrator for the
          <code>hub.experiments.update</code> permission.
        </sl-alert>
      `;
    }

    if (this.loadError) {
      return html`<sl-alert variant="danger" open>${this.loadError}</sl-alert>`;
    }

    if (this.loading && !this.loaded) {
      return html`<p class="note">Loading experiments…</p>`;
    }

    return html`
      ${this.renderAttribution()}
      ${this.malformed ? this.renderMalformedBanner() : nothing}
      ${this.writeError ? html`<sl-alert variant="danger" open>${this.writeError}</sl-alert>` : nothing}
      ${this.renderUnknownOverridesNote()}
      <p class="note">
        Changes apply to all users of this hub. Users see them the next time they load or refresh
        the page.
      </p>
      ${this.experiments.length === 0
        ? html`<p class="empty">No experiments are currently registered.</p>`
        : this.experiments.map((exp) => this.renderRow(exp))}
      ${this.malformed ? this.renderResetDialog() : nothing}
    `;
  }

  private renderAttribution() {
    if (!this.updatedAt) return nothing;
    return html`<p class="attribution">Last changed by ${this.updatedBy} at ${this.updatedAt}</p>`;
  }

  private renderUnknownOverridesNote() {
    const names = Object.keys(this.unknownOverrides);
    if (names.length === 0) return nothing;
    return html`
      <p class="note">
        ${names.length} override${names.length === 1 ? '' : 's'} for experiment${names.length === 1
          ? ''
          : 's'}
        not known to this hub version ${names.length === 1 ? 'is' : 'are'} kept: ${names.join(', ')}.
      </p>
    `;
  }

  private renderMalformedBanner() {
    return html`
      <sl-alert variant="danger" open>
        Stored experiment settings are unreadable. Server experiments are off and UI-only
        experiments are at their defaults until you reset.
      </sl-alert>
      <sl-button
        variant="danger"
        ?disabled=${this.pending}
        @click=${() => {
          this.showResetDialog = true;
        }}
      >
        Reset all to defaults
      </sl-button>
    `;
  }

  private renderResetDialog() {
    return html`
      <sl-dialog
        label="Reset all experiments"
        ?open=${this.showResetDialog}
        @sl-hide=${() => {
          this.showResetDialog = false;
        }}
      >
        <p>
          This clears every stored override, including overrides for experiments not known to this
          hub version. This cannot be undone.
        </p>
        <sl-button
          slot="footer"
          variant="danger"
          ?loading=${this.pending}
          @click=${() => {
            void this.resetAllMalformed();
          }}
        >
          Reset all to defaults
        </sl-button>
        <sl-button
          slot="footer"
          @click=${() => {
            this.showResetDialog = false;
          }}
        >
          Cancel
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderRow(exp: ExperimentEntry) {
    const hasOverride = exp.override !== null;
    const url = issueUrl(exp.issue);
    return html`
      <div class="row">
        <div class="row-header">
          <span class="title">${exp.title}</span>
          <span class="name">${exp.name}</span>
          <sl-badge variant="neutral">${exp.stage}</sl-badge>
          ${exp.layers.map(
            (l) => html`<sl-badge variant="primary">${l === 'web' ? 'UI' : 'Server'}</sl-badge>`
          )}
        </div>
        <p class="description">${exp.description}</p>
        ${url
          ? html`<p class="issue-link"><a href=${url} target="_blank" rel="noopener">${exp.issue}</a></p>`
          : nothing}
        <div class="row-controls">
          <sl-switch
            ?checked=${exp.enabled}
            ?disabled=${this.pending || this.malformed}
            @sl-change=${() => {
              void this.setOverride(exp.name, !exp.enabled);
            }}
          ></sl-switch>
          <span class="caption"
            >Default: ${exp.default ? 'on' : 'off'}${hasOverride ? ' · overridden' : ''}</span
          >
          ${hasOverride
            ? html`
                <sl-button
                  size="small"
                  ?disabled=${this.pending}
                  @click=${() => {
                    void this.setOverride(exp.name, null);
                  }}
                >
                  Reset to default
                </sl-button>
              `
            : nothing}
        </div>
        ${exp.review_overdue
          ? html`<sl-alert variant="warning" open>Review overdue</sl-alert>`
          : nothing}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-admin-experiments': ScionAdminExperiments;
  }
}
