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
 * Edit agent page (/agents/{id}/edit), which replaced the Configure page
 * (ptone/scion#3952, ptone/scion#3975).
 *
 * Loads the agent with its per-field editability and renders every field
 * of the shared <scion-agent-config-form>, grouped by function. With the
 * web.agent_edit experiment on, the user can also group the fields by tier
 * (the last choice is remembered) and the page shows the tier summary
 * strip. Save sends only the touched
 * fields, always with the agent's stateVersion; a 409 means the agent
 * changed since the page loaded and offers a reload that keeps the user's
 * edits. The actions follow the phase: Save & Start (created, stopped,
 * error) and Save & Resume (suspended) save, then start the agent, whose
 * new container runs with the edits; Reincarnate… is offered whenever the
 * hub holds edits only a reincarnation applies. Delete stays on the agent's
 * page; Back returns there.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { navigateTo } from '../../client/navigation.js';
import { lifecycleActionErrorMessage } from '../../client/agent-delete.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { GCPServiceAccountsLoader } from '../../client/gcp-sa-accounts-loader.js';
import { getPublicSettings } from '../../client/server-feature-flags.js';
import { agentStatusBadge } from '../../shared/agent-state-display.js';
import {
  heldReincarnateKeys,
  REINCARNATE_CONFIRM_MESSAGE,
} from '../../shared/agent-reincarnate.js';
import { GcpIdentityState } from '../../shared/gcp-identity-state.js';
import { assignStatusTarget } from '../../shared/gcp-sa-assign-status.js';
import { isTargetKubernetesOnly } from '../../shared/runtime-kind.js';
import { isFeatureEnabled, AGENT_EDIT_FLAG } from '../../utils/feature-flags.js';
import { showToast } from '../../utils/toast.js';
import { can } from '../../shared/types.js';
import type {
  Agent,
  AgentInlineConfig,
  AgentUpdateDisposition,
  RuntimeBroker,
} from '../../shared/types.js';
import type {
  AgentConfigEditValues,
  AgentConfigGrouping,
  AgentConfigPatch,
  AgentConfigPlaceholder,
  AgentConfigTopLevel,
  ScionAgentConfigForm,
} from '../shared/agent-config-form.js';
import { showConfirm } from '../shared/confirm-dialog.js';
import '../shared/agent-config-form.js';
import '../shared/detail-header.js';
import '../shared/status-badge.js';
import './not-found.js';

/** The agent PATCH response fields this page reads. */
interface AgentPatchResponse {
  disposition?: AgentUpdateDisposition;
  warnings?: string[];
  /** The agent's pending config edits after the save (held edits on). */
  pendingConfig?: { config?: Record<string, unknown> } | null;
}

/** The agent PATCH body the page sends. */
export type AgentEditPatchBody = AgentConfigTopLevel & {
  stateVersion: number;
  config?: AgentConfigPatch;
};

/** Where the user's last grouping choice is kept. */
export const AGENT_EDIT_GROUPING_KEY = 'scion:agent-edit:grouping';

/** The user's last grouping choice; by function when none is stored. */
export function loadEditGrouping(): AgentConfigGrouping {
  try {
    return localStorage.getItem(AGENT_EDIT_GROUPING_KEY) === 'tier' ? 'tier' : 'function';
  } catch {
    return 'function';
  }
}

function saveEditGrouping(grouping: AgentConfigGrouping): void {
  try {
    localStorage.setItem(AGENT_EDIT_GROUPING_KEY, grouping);
  } catch {
    // Storage unavailable: the choice lasts for this page only.
  }
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
 * The PATCH body for a save: stateVersion always, the touched top-level
 * keys, and config only when the form has touched config fields.
 */
export function buildAgentEditPatchBody(
  config: AgentConfigPatch,
  stateVersion: number | undefined,
  topLevel: AgentConfigTopLevel = {}
): AgentEditPatchBody {
  const body: AgentEditPatchBody = { stateVersion: stateVersion ?? 0, ...topLevel };
  if (Object.keys(config).length > 0) body.config = config;
  return body;
}

/** Inherited values shown as placeholders for the agent's unset fields. */
export function agentEditPlaceholders(agent: Agent): Record<string, AgentConfigPlaceholder> {
  const applied = agent.appliedConfig;
  const out: Record<string, AgentConfigPlaceholder> = {
    'config.max_turns': { source: 'the template or hub defaults' },
    'config.max_model_calls': { source: 'the template or hub defaults' },
    'config.max_duration': { source: 'the template or hub defaults' },
  };
  out['config.model'] = applied?.model
    ? { value: applied.model, source: 'resolved when the agent was created' }
    : { source: 'the template or harness default' };
  out['config.image'] = applied?.image
    ? { value: applied.image, source: 'resolved when the agent was created' }
    : { source: 'the template or harness default' };
  return out;
}

/**
 * The stored values the form shows: the agent's inline config, with the
 * task the hub re-sends at a fresh start when the inline config has none.
 */
export function agentEditValues(agent: Agent): AgentInlineConfig {
  const inline = agent.appliedConfig?.inlineConfig ?? {};
  const task = inline.task || agent.appliedConfig?.task;
  return task ? { ...inline, task } : inline;
}

/**
 * The agent's values outside its inline config, for the form. hubAutoExpose
 * is the hub's auto-expose default, shown for an inherited value.
 */
export function agentEditTopValues(agent: Agent, hubAutoExpose = false): AgentConfigEditValues {
  const applied = agent.appliedConfig;
  return {
    name: agent.name,
    labels: agent.labels ?? {},
    explicitTimezone: applied?.explicitTimezone,
    explicitTimezoneLegacy: applied?.explicitTimezoneLegacy,
    messageMode: agent.messageMode,
    agentRole: applied?.agentRole,
    env: applied?.env,
    // The hub returns the env only to a caller with attach access.
    envVisible: can(agent._capabilities, 'attach'),
    // The explicit record, as the hub reads it: CreateInputs when the agent
    // has them, else the inline config.
    explicitEnv: applied?.createInputs
      ? applied.createInputs.inlineConfig?.env
      : applied?.inlineConfig?.env,
    autoExposeHubDefault: hubAutoExpose,
    detailHref: `/agents/${encodeURIComponent(agent.id)}`,
    fixed: {
      projectId: agent.project || agent.projectSlug || agent.projectId,
      template: agent.template,
      harnessConfig: agent.harnessConfig || applied?.harnessConfig || '',
      runtimeBrokerId: agent.runtimeBrokerName || agent.runtimeBrokerId || '',
      profile: applied?.profile ?? '',
      branch: applied?.branch || applied?.inlineConfig?.branch || '',
    },
  };
}

/**
 * One line summarizing a save's disposition, by when each edit takes
 * effect. pending is the agent's pending config keys after the save (the
 * keys of the response's pendingConfig.config): a heldForReincarnate key
 * that is pending stays pending as a whole through every start, until a
 * reincarnation applies it.
 */
export function dispositionSummary(
  d: AgentUpdateDisposition | undefined,
  pending: readonly string[] = []
): string {
  const short = (k: string) => k.replace(/^config\./, '');
  const names = (keys: string[] | undefined) => (keys ?? []).map(short).join(', ');
  const isPending = new Set(pending);
  const parts: string[] = [];
  const applied = names(d?.applied);
  if (applied) parts.push(`Saved ${applied}.`);
  const held = names(d?.held);
  if (held) parts.push(`Saved ${held} for the next start.`);
  const reincarnateKeys = d?.heldForReincarnate ?? [];
  const pendingWhole = names(reincarnateKeys.filter((k) => isPending.has(short(k))));
  const reincarnate = names(reincarnateKeys.filter((k) => !isPending.has(short(k))));
  if (pendingWhole) {
    parts.push(
      `Saved ${pendingWhole}; held as a whole until a reincarnation, including parts a start could apply.`
    );
  }
  if (reincarnate) parts.push(`Saved ${reincarnate}; takes effect at the next reincarnation.`);
  return parts.length > 0 ? parts.join(' ') : 'Saved; nothing changed.';
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
  @state() private grouping: AgentConfigGrouping = loadEditGrouping();

  /** The by-tier view and the summary strip are behind web.agent_edit. */
  private get tierView(): boolean {
    return isFeatureEnabled(AGENT_EDIT_FLAG);
  }
  /** The agent's runtime broker, to tell whether its target is Kubernetes. */
  @state() private targetBroker: RuntimeBroker | null = null;
  /** The hub's auto-expose default (public settings), for an inherited value. */
  @state() private hubAutoExpose = false;

  /**
   * The GCP identity picker's state: the agent's stored identity is shown,
   * and only an explicit pick is sent.
   */
  readonly gcp = new GcpIdentityState();

  private readonly gcpAccounts = new GCPServiceAccountsLoader({
    projectId: () => this.agent?.projectId ?? '',
    target: () =>
      assignStatusTarget(
        this.targetRuntimeIsKubernetesOnly,
        this.agent?.runtimeBrokerId ?? '',
        this.agent?.appliedConfig?.profile ?? ''
      ),
    accounts: () => this.gcp.gcpServiceAccounts,
    setAccounts: (accounts) => this.gcp.setAccounts(accounts),
  });

  /** Whether the agent's broker and profile are reliably known to be Kubernetes. */
  private get targetRuntimeIsKubernetesOnly(): boolean {
    return isTargetKubernetesOnly(
      this.targetBroker ?? undefined,
      this.agent?.appliedConfig?.profile ?? ''
    );
  }

  override connectedCallback(): void {
    super.connectedCallback();
    const match = window.location.pathname.match(/^\/agents\/([^/]+)\/edit$/);
    this.agentId = match ? decodeURIComponent(match[1]) : '';
    if (!this.agentId) {
      this.loading = false;
      this.notFound = true;
      return;
    }
    void this.load();
  }

  override willUpdate(changed: Map<string, unknown>): void {
    super.willUpdate(changed);
    if (changed.has('targetBroker') || changed.has('agent')) {
      this.gcp.setTarget(
        this.targetRuntimeIsKubernetesOnly,
        this.agent?.appliedConfig?.profile ?? ''
      );
    }
    if (changed.has('targetBroker')) void this.gcpAccounts.targetChanged();
  }

  private get form(): ScionAgentConfigForm | null {
    return this.shadowRoot?.querySelector('scion-agent-config-form') ?? null;
  }

  /** Loads the agent. The form keeps its edits across a reload. */
  private async load(): Promise<void> {
    const first = this.agent === null;
    this.loading = first;
    this.error = null;
    try {
      const res = await apiFetch(`/api/v1/agents/${encodeURIComponent(this.agentId)}`);
      if (res.status === 404) {
        this.notFound = true;
        return;
      }
      if (!res.ok) throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      const agent = (await res.json()) as Agent;
      this.agent = agent;
      this.conflict = false;
      // A stored identity is shown, never a pick the user has made.
      if (!this.gcp.gcpIdentityUserSet) {
        const id = agent.appliedConfig?.gcpIdentity;
        this.gcp.applyStored(id?.metadataMode, id?.serviceAccountId);
      }
      dispatchPageTitle(this, 'Edit', agent.name || this.agentId);
      if (first) {
        void this.loadHubDefaults();
        void this.gcpAccounts.load();
        if (agent.runtimeBrokerId) void this.loadTargetBroker(agent.runtimeBrokerId);
      }
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to load agent';
    } finally {
      this.loading = false;
    }
  }

  /** Loads the hub defaults shown for inherited values; none on failure. */
  private async loadHubDefaults(): Promise<void> {
    const settings = await getPublicSettings();
    this.hubAutoExpose = settings?.autoExposePortsEnabled ?? false;
  }

  /** Loads the agent's runtime broker; an unknown broker leaves the target unknown. */
  private async loadTargetBroker(brokerId: string): Promise<void> {
    try {
      const res = await apiFetch(`/api/v1/runtime-brokers/${encodeURIComponent(brokerId)}`);
      if (res.ok) this.targetBroker = (await res.json()) as RuntimeBroker;
    } catch {
      // Non-critical: the same "do not guess" default as no broker at all.
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
    const topLevel = form.collectTopLevel();
    if (Object.keys(config).length === 0 && Object.keys(topLevel).length === 0) return true;
    const res = await apiFetch(`/api/v1/agents/${encodeURIComponent(this.agentId)}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(buildAgentEditPatchBody(config, this.agent.stateVersion, topLevel)),
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
    showToast(
      dispositionSummary(body.disposition, Object.keys(body.pendingConfig?.config ?? {})),
      'success'
    );
    form.reset();
    // The saved identity is now the stored one.
    this.gcp.gcpIdentityUserSet = false;
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
    const missing = this.form?.missingRequiredEnv() ?? [];
    if (missing.length > 0) {
      this.error = `Missing required environment variables: ${missing.join(', ')}. Fill them in under Environment variables.`;
      return;
    }
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

  /**
   * Reincarnates the agent, which applies the held edits only a
   * reincarnation applies. The confirm names them.
   */
  private async handleReincarnate(): Promise<void> {
    if (this.busy) return;
    const held = heldReincarnateKeys(this.agent);
    const confirmed = await showConfirm(
      `${REINCARNATE_CONFIRM_MESSAGE} It applies the held edits: ${held
        .map((k) => k.replace(/^config\./, ''))
        .join(', ')}.`,
      { title: 'Reincarnate agent', confirmText: 'Reincarnate' }
    );
    if (!confirmed) return;
    this.busy = true;
    this.error = null;
    try {
      const res = await apiFetch(`/api/v1/agents/${encodeURIComponent(this.agentId)}/reincarnate`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: '{}',
      });
      if (!res.ok) {
        this.error = await lifecycleActionErrorMessage(res, 'Failed to reincarnate agent');
        return;
      }
      navigateTo(`/agents/${encodeURIComponent(this.agentId)}`);
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to reincarnate agent';
    } finally {
      this.busy = false;
    }
  }

  private setGrouping(grouping: AgentConfigGrouping): void {
    if (grouping === this.grouping) return;
    this.grouping = grouping;
    saveEditGrouping(grouping);
  }

  private renderActions() {
    const action = editStartAction(this.agent?.phase);
    const nothingTouched = this.touched.length === 0;
    // Reincarnating needs the lifecycle permission, as on the agent page.
    const held = can(this.agent?._capabilities, 'lifecycle') ? heldReincarnateKeys(this.agent) : [];
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
        ${held.length > 0
          ? html`<sl-button
              variant="default"
              ?disabled=${this.busy}
              data-testid="reincarnate"
              @click=${() => void this.handleReincarnate()}
            >
              <sl-icon slot="prefix" name="arrow-repeat"></sl-icon>
              Reincarnate…
            </sl-button>`
          : nothing}
        <a href="/agents/${encodeURIComponent(this.agentId)}" class="back" data-testid="back"
          >Back to agent</a
        >
      </div>
    `;
  }

  private renderGroupingToggle() {
    return html`<sl-radio-group
      class="grouping"
      size="small"
      label="Group fields"
      data-testid="grouping"
      .value=${this.grouping}
      @sl-change=${(e: Event) =>
        this.setGrouping(
          (e.target as HTMLElement & { value: string }).value === 'tier' ? 'tier' : 'function'
        )}
    >
      <sl-radio-button value="function">By function</sl-radio-button>
      <sl-radio-button value="tier">By when it applies</sl-radio-button>
    </sl-radio-group>`;
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
      ${this.tierView ? this.renderGroupingToggle() : nothing}
      <scion-agent-config-form
        mode="edit"
        .grouping=${this.tierView ? this.grouping : 'function'}
        .showTierSummary=${this.tierView}
        .values=${agentEditValues(agent)}
        .editValues=${agentEditTopValues(agent, this.hubAutoExpose)}
        .placeholders=${agentEditPlaceholders(agent)}
        .editability=${agent.editability ?? null}
        .harnessCapabilities=${agent.harnessCapabilities ?? null}
        .gcpIdentity=${this.gcp}
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
    .grouping {
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
