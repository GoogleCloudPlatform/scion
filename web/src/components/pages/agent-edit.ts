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
 * Edit agent page (/agents/{id}/edit), behind the web.agent_edit
 * experiment (ptone/scion#3952).
 *
 * Loads the agent with its per-field editability and renders the shared
 * <scion-agent-config-form>. Save sends only the touched fields, always with
 * the agent's stateVersion; a 409 means the agent changed since the page
 * loaded and offers a reload that keeps the user's edits. Save & Start
 * (created, stopped, error) and Save & Resume (suspended) save, then start
 * the agent, whose new container runs with the edits.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { navigateTo } from '../../client/navigation.js';
import { lifecycleActionErrorMessage } from '../../client/agent-delete.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { agentStatusBadge } from '../../shared/agent-state-display.js';
import { isFeatureEnabled, AGENT_EDIT_FLAG } from '../../utils/feature-flags.js';
import { showToast } from '../../utils/toast.js';
import type { Agent, AgentUpdateDisposition } from '../../shared/types.js';
import type {
  AgentConfigPatch,
  AgentConfigPlaceholder,
  ScionAgentConfigForm,
} from '../shared/agent-config-form.js';
import '../shared/agent-config-form.js';
import '../shared/detail-header.js';
import '../shared/status-badge.js';
import './not-found.js';

/**
 * The fields the Edit page renders: Model, Max turns and Max duration. The
 * rest of the shared form's fields arrive on this page with its full field
 * set.
 */
export const AGENT_EDIT_FIELD_KEYS: readonly string[] = [
  'config.model',
  'config.max_turns',
  'config.max_duration',
];

/** The agent PATCH response fields this page reads. */
interface AgentPatchResponse {
  disposition?: AgentUpdateDisposition;
  warnings?: string[];
}

/** The agent PATCH body the page sends. */
export interface AgentEditPatchBody {
  stateVersion: number;
  config?: AgentConfigPatch;
}

/** The start action the page offers for a phase, if any. */
export function editStartAction(phase: string | undefined): 'start' | 'resume' | null {
  switch (phase) {
    case 'created':
    case 'stopped':
    case 'error':
      return 'start';
    case 'suspended':
      return 'resume';
    default:
      return null;
  }
}

/**
 * The PATCH body for a save: stateVersion always, config only when the form
 * has touched fields.
 */
export function buildAgentEditPatchBody(
  config: AgentConfigPatch,
  stateVersion: number | undefined
): AgentEditPatchBody {
  const body: AgentEditPatchBody = { stateVersion: stateVersion ?? 0 };
  if (Object.keys(config).length > 0) body.config = config;
  return body;
}

/** Inherited values shown as placeholders for the agent's unset fields. */
export function agentEditPlaceholders(agent: Agent): Record<string, AgentConfigPlaceholder> {
  const applied = agent.appliedConfig;
  const out: Record<string, AgentConfigPlaceholder> = {
    'config.max_turns': { source: 'the template or hub defaults' },
    'config.max_duration': { source: 'the template or hub defaults' },
  };
  out['config.model'] = applied?.model
    ? { value: applied.model, source: 'resolved when the agent was created' }
    : { source: 'the template or harness default' };
  return out;
}

/** One line summarizing a save's disposition. */
export function dispositionSummary(d: AgentUpdateDisposition | undefined): string {
  const applied = d?.applied ?? [];
  if (applied.length === 0) return 'Nothing to save.';
  const names = applied.map((k) => k.replace(/^config\./, ''));
  return `Saved ${names.join(', ')}.`;
}

@customElement('scion-page-agent-edit')
export class ScionPageAgentEdit extends LitElement {
  @state() private agentId = '';
  @state() private agent: Agent | null = null;
  @state() private loading = true;
  @state() private notFound = false;
  @state() private busy = false;
  @state() private error: string | null = null;
  /** Set when a save was refused because the agent changed (409). */
  @state() private conflict = false;
  @state() private touched: string[] = [];
  @state() private warnings: string[] = [];

  override connectedCallback(): void {
    super.connectedCallback();
    const match = window.location.pathname.match(/^\/agents\/([^/]+)\/edit$/);
    this.agentId = match ? decodeURIComponent(match[1]) : '';
    if (!isFeatureEnabled(AGENT_EDIT_FLAG) || !this.agentId) {
      this.loading = false;
      this.notFound = true;
      return;
    }
    void this.load();
  }

  private get form(): ScionAgentConfigForm | null {
    return this.shadowRoot?.querySelector('scion-agent-config-form') ?? null;
  }

  /** Loads the agent. The form keeps its edits across a reload. */
  private async load(): Promise<void> {
    this.loading = this.agent === null;
    this.error = null;
    try {
      const res = await apiFetch(`/api/v1/agents/${encodeURIComponent(this.agentId)}`);
      if (res.status === 404) {
        this.notFound = true;
        return;
      }
      if (!res.ok) throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      this.agent = (await res.json()) as Agent;
      this.conflict = false;
      dispatchPageTitle(this, 'Edit', this.agent.name || this.agentId);
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to load agent';
    } finally {
      this.loading = false;
    }
  }

  /**
   * Saves the touched fields. Returns true when the agent was saved (or
   * there was nothing to save).
   */
  private async save(): Promise<boolean> {
    const form = this.form;
    if (!form || !this.agent) return false;
    const errors = form.validate();
    if (errors.length > 0) {
      this.error = errors.join(' ');
      return false;
    }
    const config = form.collectConfigPatch();
    if (Object.keys(config).length === 0) return true;
    const res = await apiFetch(`/api/v1/agents/${encodeURIComponent(this.agentId)}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(buildAgentEditPatchBody(config, this.agent.stateVersion)),
    });
    if (res.status === 409) {
      this.conflict = true;
      this.error = await extractApiError(res, 'This agent changed since the page loaded.');
      return false;
    }
    if (!res.ok) {
      this.error = await extractApiError(res, `HTTP ${res.status}`);
      return false;
    }
    let body: AgentPatchResponse = {};
    try {
      body = (await res.json()) as AgentPatchResponse;
    } catch {
      // An unreadable body still means the save succeeded.
    }
    this.warnings = body.warnings ?? [];
    showToast(dispositionSummary(body.disposition), 'success');
    form.reset();
    return true;
  }

  private async handleSave(): Promise<void> {
    if (this.busy) return;
    this.busy = true;
    this.error = null;
    try {
      if (await this.save()) await this.load();
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to save';
    } finally {
      this.busy = false;
    }
  }

  private async handleSaveAndStart(): Promise<void> {
    if (this.busy) return;
    this.busy = true;
    this.error = null;
    try {
      if (!(await this.save())) return;
      const res = await apiFetch(`/api/v1/agents/${encodeURIComponent(this.agentId)}/start`, {
        method: 'POST',
      });
      if (!res.ok) {
        this.error = await lifecycleActionErrorMessage(res, 'Saved, but the agent failed to start');
        await this.load();
        return;
      }
      navigateTo(`/agents/${encodeURIComponent(this.agentId)}`);
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to save and start';
    } finally {
      this.busy = false;
    }
  }

  private renderActions() {
    const action = editStartAction(this.agent?.phase);
    const nothingTouched = this.touched.length === 0;
    return html`
      <div class="actions">
        <sl-button
          variant=${action ? 'default' : 'primary'}
          ?loading=${this.busy}
          ?disabled=${this.busy || nothingTouched}
          data-testid="save"
          @click=${() => void this.handleSave()}
          >Save</sl-button
        >
        ${action
          ? html`<sl-button
              variant="primary"
              ?loading=${this.busy}
              ?disabled=${this.busy}
              data-testid="save-start"
              @click=${() => void this.handleSaveAndStart()}
            >
              <sl-icon slot="prefix" name="play-circle"></sl-icon>
              ${action === 'resume' ? 'Save & Resume' : 'Save & Start'}
            </sl-button>`
          : nothing}
        <a href="/agents/${encodeURIComponent(this.agentId)}" class="back">Back to agent</a>
      </div>
    `;
  }

  override render() {
    if (this.notFound) return html`<scion-page-404></scion-page-404>`;
    if (this.loading) {
      return html`<div class="loading"><sl-spinner></sl-spinner></div>`;
    }
    const agent = this.agent;
    if (!agent) {
      return html`<sl-alert variant="danger" open
        >${this.error ?? 'Failed to load agent'}</sl-alert
      >`;
    }
    return html`
      <scion-detail-header heading=${agent.name || this.agentId}>
        ${agentStatusBadge(agent)}
      </scion-detail-header>
      ${this.conflict
        ? html`<sl-alert variant="warning" open data-testid="conflict">
            <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
            This agent changed since the page loaded, so your changes were not saved. Reload it to
            see the current values; your edits are kept.
            <sl-button size="small" data-testid="reload" @click=${() => void this.load()}
              >Reload</sl-button
            >
          </sl-alert>`
        : this.error
          ? html`<sl-alert variant="danger" open data-testid="error">${this.error}</sl-alert>`
          : nothing}
      ${this.warnings.length > 0
        ? html`<sl-alert variant="primary" open closable data-testid="warnings">
            <sl-icon slot="icon" name="info-circle"></sl-icon>
            ${this.warnings.map((w) => html`<div>${w}</div>`)}
          </sl-alert>`
        : nothing}
      <scion-agent-config-form
        mode="edit"
        .fieldKeys=${AGENT_EDIT_FIELD_KEYS}
        .values=${agent.appliedConfig?.inlineConfig ?? {}}
        .placeholders=${agentEditPlaceholders(agent)}
        .editability=${agent.editability ?? null}
        .harnessCapabilities=${agent.harnessCapabilities ?? null}
        ?disabled=${this.busy}
        @agent-config-change=${(e: CustomEvent<{ touched: string[] }>) => {
          this.touched = e.detail.touched;
        }}
      ></scion-agent-config-form>
      ${this.renderActions()}
    `;
  }

  static override styles = css`
    :host {
      display: block;
      max-width: 52rem;
    }
    .loading {
      display: flex;
      justify-content: center;
      padding: 3rem;
    }
    sl-alert {
      margin-bottom: 1rem;
    }
    .actions {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-top: 1rem;
    }
    .back {
      margin-left: auto;
    }
  `;
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-agent-edit': ScionPageAgentEdit;
  }
}
