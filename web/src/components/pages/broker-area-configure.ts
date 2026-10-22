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
 * Configure tab of the Brokers area detail page (broker UI Option A, slice
 * A1), rendered for a user who may update or delete the broker:
 *
 * - Identity: name, labels, GCP host service account and project
 *   (PATCH /api/v1/runtime-brokers/{id}).
 * - Capacity: max concurrent agents (PUT .../settings, the same rules as
 *   the existing broker page).
 * - Instance config (flat brokers, read-only): where the instance is
 *   configured on its host.
 * - Danger zone: Unregister (DELETE), gated on the broker's delete
 *   capability.
 *
 * After a successful PATCH the tab re-reads the broker (GET) and fires
 * `broker-updated` with that record, which the page takes as its broker. The
 * PATCH response is not merged: it omits cleared (empty) labels and GCP
 * fields, so a merge would keep the old values.
 */

import { LitElement, html, css, nothing, type PropertyValues } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { can } from '../../shared/types.js';
import type { BrokerSettingsResponse, RuntimeBroker } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { showConfirm } from '../shared/confirm-dialog.js';
import { showToast } from '../../utils/toast.js';
import { brokerAreaStyles } from '../shared/broker-area-styles.js';
import { effectiveLimitText, limitSourceLabel } from '../../shared/broker-capacity.js';

/** The neutral hint shown on registration-managed fields of a flat broker. */
export const REGISTRATION_HINT = 'Set by the broker on registration.';

/** Labels as editable text, one "key=value" per line, sorted by key. */
export function labelsToText(labels: Record<string, string> | undefined): string {
  return Object.keys(labels ?? {})
    .sort()
    .map((k) => `${k}=${labels?.[k] ?? ''}`)
    .join('\n');
}

/** Parses "key=value" lines; blank lines are skipped. Throws on a line without "=". */
export function textToLabels(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const raw of text.split('\n')) {
    const line = raw.trim();
    if (!line) continue;
    const eq = line.indexOf('=');
    if (eq <= 0) throw new Error(`Label "${line}" must be key=value.`);
    out[line.slice(0, eq).trim()] = line.slice(eq + 1).trim();
  }
  return out;
}

@customElement('scion-broker-area-configure')
export class ScionBrokerAreaConfigure extends LitElement {
  @property({ type: Object })
  broker: RuntimeBroker | null = null;

  @property({ type: Object })
  settings: BrokerSettingsResponse | null = null;

  /** Projects and agents counts, for the Unregister confirmation. */
  @property({ type: Number })
  projectCount = 0;

  @property({ type: Number })
  agentCount = 0;

  @state() private name = '';
  @state() private labelsText = '';
  @state() private saEmail = '';
  @state() private saProject = '';
  @state() private identitySaving = false;
  @state() private identityError: string | null = null;

  @state() private capacityMode: 'default' | 'custom' = 'default';
  @state() private capacityValue = '';
  @state() private capacitySaving = false;
  @state() private capacityError: string | null = null;

  @state() private unregistering = false;

  private formBrokerId = '';
  private formSettingsRevision = -1;

  static override styles = [
    brokerAreaStyles,
    css`
      .config-section {
        padding: 1.25rem;
        background: var(--scion-surface, #ffffff);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-lg, 0.75rem);
      }

      .field {
        margin-bottom: 0.75rem;
      }

      .danger-zone {
        border-color: var(--sl-color-danger-200, #fecaca);
      }

      .instance-key {
        font-family: var(--scion-font-mono, monospace);
      }
    `,
  ];

  override willUpdate(changed: PropertyValues<this>): void {
    // Sync the forms only when a different broker or settings revision
    // arrives, so typing is not clobbered by re-renders.
    if (changed.has('broker') && this.broker && this.broker.id !== this.formBrokerId) {
      this.resetIdentityForm();
    }
    if (changed.has('settings') && this.settings) {
      if (this.settings.revision !== this.formSettingsRevision) this.resetCapacityForm();
    }
  }

  private resetIdentityForm(): void {
    const b = this.broker;
    if (!b) return;
    this.formBrokerId = b.id;
    this.name = b.name;
    this.labelsText = labelsToText(b.labels);
    this.saEmail = b.gcp_host_service_account_email ?? '';
    this.saProject = b.gcp_host_project_id ?? '';
    this.identityError = null;
  }

  private resetCapacityForm(): void {
    const s = this.settings;
    if (!s) return;
    this.formSettingsRevision = s.revision;
    const stored = s.settings.maxAgents;
    if (stored === undefined || stored === null) {
      this.capacityMode = 'default';
      this.capacityValue = '';
    } else {
      this.capacityMode = 'custom';
      this.capacityValue = String(stored);
    }
    this.capacityError = null;
  }

  private get canUpdate(): boolean {
    return can(this.broker?._capabilities, 'update');
  }

  private get canDelete(): boolean {
    return can(this.broker?._capabilities, 'delete');
  }

  /** The PATCH body: only the fields that changed. */
  private identityPatch(): Record<string, unknown> {
    const b = this.broker;
    if (!b) return {};
    const patch: Record<string, unknown> = {};
    if (this.name.trim() && this.name.trim() !== b.name) patch.name = this.name.trim();
    const labels = textToLabels(this.labelsText);
    if (labelsToText(labels) !== labelsToText(b.labels)) patch.labels = labels;
    if (this.saEmail.trim() !== (b.gcp_host_service_account_email ?? '')) {
      patch.gcpHostServiceAccountEmail = this.saEmail.trim();
    }
    if (this.saProject.trim() !== (b.gcp_host_project_id ?? '')) {
      patch.gcpHostProjectId = this.saProject.trim();
    }
    return patch;
  }

  private async saveIdentity(): Promise<void> {
    if (!this.broker) return;
    let patch: Record<string, unknown>;
    try {
      patch = this.identityPatch();
    } catch (err) {
      this.identityError = err instanceof Error ? err.message : String(err);
      return;
    }
    if (Object.keys(patch).length === 0) {
      this.identityError = 'Nothing to save.';
      return;
    }
    this.identitySaving = true;
    this.identityError = null;
    try {
      const res = await apiFetch(`/api/v1/runtime-brokers/${this.broker.id}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(patch),
      });
      if (!res.ok) {
        throw new Error(await extractApiError(res, 'Failed to save the broker'));
      }
      const fresh = await apiFetch(`/api/v1/runtime-brokers/${this.broker.id}`);
      if (!fresh.ok) {
        const cause = await extractApiError(fresh, `HTTP ${fresh.status}`);
        throw new Error(`Saved. Refreshing the broker failed (${cause}); reload the page.`);
      }
      const updated = (await fresh.json()) as RuntimeBroker;
      this.formBrokerId = '';
      this.dispatchEvent(
        new CustomEvent<RuntimeBroker>('broker-updated', {
          detail: updated,
          bubbles: true,
          composed: true,
        })
      );
      showToast('Broker saved.', 'success');
    } catch (err) {
      this.identityError = err instanceof Error ? err.message : 'Failed to save the broker';
    } finally {
      this.identitySaving = false;
    }
  }

  private get capacityInvalid(): boolean {
    if (this.capacityMode !== 'custom') return false;
    const raw = this.capacityValue.trim();
    if (raw === '') return true;
    const parsed = Number(raw);
    return !Number.isInteger(parsed) || parsed < 0;
  }

  private async saveCapacity(): Promise<void> {
    if (!this.broker || !this.settings) return;
    if (this.capacityInvalid) {
      this.capacityError = 'Enter a whole number 0 or greater (0 means unlimited).';
      return;
    }
    const maxAgents = this.capacityMode === 'default' ? null : Number(this.capacityValue.trim());
    this.capacitySaving = true;
    this.capacityError = null;
    try {
      const res = await apiFetch(`/api/v1/runtime-brokers/${this.broker.id}/settings`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          settings: { maxAgents },
          expectedRevision: this.settings.revision,
        }),
      });
      if (res.status === 409) {
        this.capacityError = 'Settings were changed by someone else. Reload the page.';
        return;
      }
      if (!res.ok) {
        throw new Error(await extractApiError(res, 'Failed to save broker settings'));
      }
      const saved = (await res.json()) as BrokerSettingsResponse;
      this.dispatchEvent(
        new CustomEvent<BrokerSettingsResponse>('broker-settings-updated', {
          detail: saved,
          bubbles: true,
          composed: true,
        })
      );
      showToast('Broker settings saved.', 'success');
    } catch (err) {
      this.capacityError = err instanceof Error ? err.message : 'Failed to save broker settings';
    } finally {
      this.capacitySaving = false;
    }
  }

  private async unregister(): Promise<void> {
    const b = this.broker;
    if (!b) return;
    const parts: string[] = [];
    if (this.projectCount > 0) {
      const plural = this.projectCount !== 1 ? 's' : '';
      parts.push(`remove it from ${this.projectCount} project${plural}`);
    }
    if (this.agentCount > 0) {
      parts.push(`affect ${this.agentCount} agent${this.agentCount !== 1 ? 's' : ''}`);
    }
    const consequence = parts.length > 0 ? `\n\nThis will ${parts.join(' and ')}.` : '';
    const confirmed = await showConfirm(`Unregister broker "${b.name}"?${consequence}`, {
      title: 'Unregister Broker',
      confirmText: 'Unregister',
      variant: 'danger',
    });
    if (!confirmed) return;
    this.unregistering = true;
    try {
      const res = await apiFetch(`/api/v1/runtime-brokers/${b.id}`, { method: 'DELETE' });
      if (!res.ok) {
        throw new Error(await extractApiError(res, 'Failed to unregister broker'));
      }
      window.location.href = '/brokers';
    } catch (err) {
      showToast(err instanceof Error ? err.message : 'Failed to unregister broker', 'danger');
    } finally {
      this.unregistering = false;
    }
  }

  private renderIdentity() {
    const b = this.broker;
    if (!b || !this.canUpdate) return nothing;
    const flat = !!b.runtimeTarget;
    const fileName = b.runtimeTargetPresentation?.name;
    const hint = flat ? html`<p class="hint registration-hint">${REGISTRATION_HINT}</p>` : nothing;
    return html`
      <div class="section config-section identity-section">
        <div class="section-header"><h2>Identity</h2></div>
        ${this.identityError ? html`<div class="form-error">${this.identityError}</div>` : nothing}
        <div class="field">
          <sl-input
            label="Name"
            name="name"
            .value=${this.name}
            ?disabled=${this.identitySaving}
            @sl-input=${(e: Event) => (this.name = (e.target as HTMLInputElement).value)}
          ></sl-input>
          ${flat && fileName && fileName !== b.name
            ? html`<p class="hint instance-file-name">Name in instance file: ${fileName}</p>`
            : nothing}
        </div>
        <div class="field">
          <sl-textarea
            label="Labels (one key=value per line)"
            name="labels"
            rows="3"
            .value=${this.labelsText}
            ?disabled=${this.identitySaving}
            @sl-input=${(e: Event) => (this.labelsText = (e.target as HTMLTextAreaElement).value)}
          ></sl-textarea>
          ${hint}
        </div>
        <div class="field">
          <sl-input
            label="GCP host service account email"
            name="gcpHostServiceAccountEmail"
            .value=${this.saEmail}
            ?disabled=${this.identitySaving}
            @sl-input=${(e: Event) => (this.saEmail = (e.target as HTMLInputElement).value)}
          ></sl-input>
          ${hint}
        </div>
        <div class="field">
          <sl-input
            label="GCP host project ID"
            name="gcpHostProjectId"
            .value=${this.saProject}
            ?disabled=${this.identitySaving}
            @sl-input=${(e: Event) => (this.saProject = (e.target as HTMLInputElement).value)}
          ></sl-input>
          ${hint}
        </div>
        <sl-button
          class="save-identity"
          variant="primary"
          size="small"
          ?loading=${this.identitySaving}
          ?disabled=${this.identitySaving}
          @click=${() => this.saveIdentity()}
          >Save</sl-button
        >
      </div>
    `;
  }

  private renderCapacity() {
    const s = this.settings;
    if (!s) return nothing;
    const canEdit = s._capabilities.update;
    const effective = s.effective.maxAgents;
    return html`
      <div class="section config-section capacity-config">
        <div class="section-header"><h2>Capacity</h2></div>
        ${this.capacityError ? html`<div class="form-error">${this.capacityError}</div>` : nothing}
        <sl-radio-group
          label="Max concurrent agents"
          value=${this.capacityMode}
          @sl-change=${(e: Event) => {
            if (!canEdit) return;
            this.capacityMode = (e.target as HTMLInputElement).value as 'default' | 'custom';
          }}
        >
          <sl-radio value="default" ?disabled=${!canEdit || this.capacitySaving}
            >Use the inherited limit (${effectiveLimitText(effective.inherited.value)})</sl-radio
          >
          <sl-radio value="custom" ?disabled=${!canEdit || this.capacitySaving}>Custom</sl-radio>
        </sl-radio-group>
        ${this.capacityMode === 'custom'
          ? html`<sl-input
              type="number"
              min="0"
              step="1"
              size="small"
              name="maxAgents"
              .value=${this.capacityValue}
              ?disabled=${!canEdit || this.capacitySaving}
              help-text="0 means unlimited"
              @sl-input=${(e: Event) => (this.capacityValue = (e.target as HTMLInputElement).value)}
            ></sl-input>`
          : nothing}
        <p class="tab-note effective-limit">
          Effective limit: ${effectiveLimitText(effective.value)} (from
          ${limitSourceLabel(effective.source)}).
        </p>
        ${canEdit
          ? html`<sl-button
              class="save-capacity"
              variant="primary"
              size="small"
              ?loading=${this.capacitySaving}
              ?disabled=${this.capacitySaving || this.capacityInvalid}
              @click=${() => this.saveCapacity()}
              >Save</sl-button
            >`
          : html`<p class="tab-note">Only Hub administrators can change this setting.</p>`}
      </div>
    `;
  }

  private renderInstanceConfig() {
    const key = this.broker?.runtimeTargetPresentation?.key;
    if (!this.broker?.runtimeTarget || !key) return nothing;
    const path = `server.broker.instances[${key}]`;
    return html`
      <div class="section config-section instance-config">
        <div class="section-header"><h2>Instance config</h2></div>
        <p class="tab-note">
          File-owned on the host: <span class="instance-key">${path}</span>
          <sl-icon-button
            name="clipboard"
            label="Copy the instance key"
            @click=${() => void navigator.clipboard?.writeText(key)}
          ></sl-icon-button>
        </p>
      </div>
    `;
  }

  private renderDangerZone() {
    if (!this.canDelete) return nothing;
    return html`
      <div class="section config-section danger-zone">
        <div class="section-header"><h2>Danger zone</h2></div>
        <sl-button
          class="unregister"
          variant="danger"
          size="small"
          ?loading=${this.unregistering}
          ?disabled=${this.unregistering}
          @click=${() => this.unregister()}
        >
          <sl-icon slot="prefix" name="trash"></sl-icon>
          Unregister
        </sl-button>
      </div>
    `;
  }

  override render() {
    if (!this.broker) return nothing;
    return html`
      ${this.renderIdentity()} ${this.renderCapacity()} ${this.renderInstanceConfig()}
      ${this.renderDangerZone()}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-broker-area-configure': ScionBrokerAreaConfigure;
  }
}
