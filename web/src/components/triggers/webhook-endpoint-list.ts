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
 * Webhook Endpoints list & management tab (Stage 2 §9.1).
 *
 * Security invariant (S25): webhook signing secrets are shown once in the
 * result dialog and cleared immediately when the dialog closes or the
 * component disconnects.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { paginateAll } from '../../client/paginate-all.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { resourceStyles } from '../shared/resource-styles.js';
import type {
  ListWebhookEndpointsResponse,
  ProjectAgentOption,
  TriggerDefinition,
  WebhookEndpoint,
  WebhookEndpointCreated,
  WebhookEndpointRotated,
} from './types.js';

@customElement('scion-webhook-endpoint-list')
export class ScionWebhookEndpointList extends LitElement {
  readonly _zone = new DisplayZoneController(this);

  @property({ type: String })
  projectId = '';

  @property({ type: Array })
  agents: ProjectAgentOption[] = [];

  @property({ type: Array })
  triggers: TriggerDefinition[] = [];

  @state() private endpoints: WebhookEndpoint[] = [];
  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private saving = false;
  @state() private formError: string | null = null;

  // Create / Edit dialog state
  @state() private dialogOpen = false;
  @state() private editingEndpoint: WebhookEndpoint | null = null;
  @state() private formName = '';
  @state() private formDescription = '';
  @state() private formAuth = 'hmac';
  @state() private formAllowProjectAgents = false;
  @state() private formSignatureMode = 'scion_v1';
  @state() private formBoundAgentId = '';
  @state() private formBoundLifetime = 'agent';
  @state() private formMaxBodyBytes = '65536';
  @state() private formRatePerSecond = '';
  @state() private formRateBurst = '';
  @state() private formReplayWindowSeconds = '300';

  // Secret-once result dialog state — cleared immediately on close/disconnect.
  @state() private secretDialogOpen = false;
  @state() private secretDialogTitle = '';
  @state() private revealedSecret: string | null = null;
  @state() private revealedEndpointUrl = '';
  @state() private revealedSignatureMode = '';
  @state() private revealedGraceExpires = '';
  @state() private copiedField: 'url' | 'secret' | 'curl' | null = null;

  // Rotate confirm dialog state
  @state() private rotateTarget: WebhookEndpoint | null = null;
  @state() private rotating = false;

  // Delete confirm dialog state
  @state() private deleteTarget: WebhookEndpoint | null = null;
  @state() private deleting = false;

  static override styles = [
    resourceStyles,
    css`
      .toolbar {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 0.75rem;
        margin-bottom: 1rem;
      }

      .toolbar-actions {
        display: flex;
        align-items: center;
        gap: 0.5rem;
      }

      .endpoint-name {
        font-weight: 600;
        color: var(--scion-text);
      }

      .endpoint-desc {
        font-size: 0.75rem;
        color: var(--scion-text-muted);
        margin-top: 0.125rem;
      }

      .endpoint-url {
        font-family: var(--scion-font-mono);
        font-size: 0.75rem;
        color: var(--scion-text-muted);
        word-break: break-all;
      }

      .meta-stack {
        display: flex;
        flex-direction: column;
        gap: 0.25rem;
      }

      .badge-row {
        display: flex;
        flex-wrap: wrap;
        gap: 0.25rem;
        align-items: center;
      }

      .actions-cell {
        white-space: nowrap;
      }

      .dialog-form {
        display: flex;
        flex-direction: column;
        gap: 0.875rem;
      }

      .field-grid {
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: 0.75rem;
      }

      .secret-box {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.5rem 0.75rem;
        background: var(--scion-bg-subtle);
        border: 1px solid var(--scion-border);
        border-radius: var(--scion-radius);
        font-family: var(--scion-font-mono);
        font-size: 0.8125rem;
        word-break: break-all;
      }

      .secret-box code {
        flex: 1;
      }

      .curl-example {
        margin: 0;
        padding: 0.75rem;
        background: var(--scion-bg-subtle);
        border: 1px solid var(--scion-border);
        border-radius: var(--scion-radius);
        font-family: var(--scion-font-mono);
        font-size: 0.75rem;
        overflow-x: auto;
        white-space: pre;
      }

      .affected-list {
        margin: 0.5rem 0 0 0;
        padding-left: 1.25rem;
        font-size: 0.875rem;
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.projectId) {
      void this.loadEndpoints();
    }
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('projectId') && this.projectId) {
      void this.loadEndpoints();
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.clearSecretState();
  }

  /** Clears any once-only secret from component state immediately. */
  private clearSecretState(): void {
    this.revealedSecret = null;
    this.revealedEndpointUrl = '';
    this.revealedSignatureMode = '';
    this.revealedGraceExpires = '';
    this.secretDialogTitle = '';
    this.copiedField = null;
  }

  private emitEndpointsLoaded(endpoints: WebhookEndpoint[]): void {
    this.dispatchEvent(
      new CustomEvent<{ endpoints: WebhookEndpoint[] }>('endpoints-loaded', {
        detail: { endpoints },
        bubbles: true,
        composed: true,
      })
    );
  }

  async loadEndpoints(): Promise<void> {
    if (!this.projectId) return;
    this.loading = true;
    this.error = null;
    try {
      const items = await paginateAll<WebhookEndpoint>({
        path: `/api/v1/projects/${encodeURIComponent(this.projectId)}/webhook-endpoints`,
        pageSize: 100,
        parsePage: (raw) => {
          const data = raw as ListWebhookEndpointsResponse;
          const pageItems = Array.isArray(data.endpoints) ? data.endpoints : [];
          return data.nextCursor
            ? { items: pageItems, nextCursor: data.nextCursor }
            : { items: pageItems };
        },
      });
      this.endpoints = items;
      this.emitEndpointsLoaded(items);
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to load webhook endpoints';
    } finally {
      this.loading = false;
    }
  }

  private openCreateDialog(): void {
    this.editingEndpoint = null;
    this.formName = '';
    this.formDescription = '';
    this.formAuth = 'hmac';
    this.formAllowProjectAgents = false;
    this.formSignatureMode = 'scion_v1';
    this.formBoundAgentId = '';
    this.formBoundLifetime = 'agent';
    this.formMaxBodyBytes = '65536';
    this.formRatePerSecond = '';
    this.formRateBurst = '';
    this.formReplayWindowSeconds = '300';
    this.formError = null;
    this.dialogOpen = true;
  }

  private openEditDialog(ep: WebhookEndpoint): void {
    this.editingEndpoint = ep;
    this.formName = ep.name;
    this.formDescription = ep.description ?? '';
    this.formAuth = ep.auth || 'hmac';
    this.formAllowProjectAgents = Boolean(ep.allowProjectAgents);
    this.formSignatureMode = ep.signatureMode || 'scion_v1';
    this.formBoundAgentId = ep.boundAgent?.id ?? '';
    this.formBoundLifetime = ep.boundLifetime || 'agent';
    this.formMaxBodyBytes = String(ep.maxBodyBytes || 65536);
    this.formRatePerSecond = ep.rateLimit?.perSecond ? String(ep.rateLimit.perSecond) : '';
    this.formRateBurst = ep.rateLimit?.burst ? String(ep.rateLimit.burst) : '';
    this.formReplayWindowSeconds = String(ep.replayWindowSeconds || 300);
    this.formError = null;
    this.dialogOpen = true;
  }

  private async submitEndpointForm(): Promise<void> {
    if (!this.projectId) return;
    if (!this.editingEndpoint && !this.formName.trim()) {
      this.formError = 'Name is required.';
      return;
    }

    const maxBody = parseInt(this.formMaxBodyBytes, 10);
    const replayWin = parseInt(this.formReplayWindowSeconds, 10);
    const ratePerSec = parseInt(this.formRatePerSecond, 10);
    const rateBurst = parseInt(this.formRateBurst, 10);

    const payload: Record<string, unknown> = {
      description: this.formDescription.trim(),
      auth: this.formAuth,
      allowProjectAgents: this.formAuth === 'hub_token' ? this.formAllowProjectAgents : false,
      signatureMode: this.formAuth === 'hmac' ? this.formSignatureMode : undefined,
      maxBodyBytes: Number.isFinite(maxBody) && maxBody > 0 ? maxBody : undefined,
      replayWindowSeconds: Number.isFinite(replayWin) && replayWin > 0 ? replayWin : undefined,
    };

    if (
      (Number.isFinite(ratePerSec) && ratePerSec > 0) ||
      (Number.isFinite(rateBurst) && rateBurst > 0)
    ) {
      payload['rateLimit'] = {
        perSecond: Number.isFinite(ratePerSec) && ratePerSec > 0 ? ratePerSec : undefined,
        burst: Number.isFinite(rateBurst) && rateBurst > 0 ? rateBurst : undefined,
      };
    }

    if (this.formBoundAgentId) {
      payload['boundAgentId'] = this.formBoundAgentId;
      payload['boundLifetime'] = this.formBoundLifetime || 'agent';
    } else if (this.editingEndpoint?.boundAgent?.id) {
      // Explicitly unbind when clearing boundAgentId on PATCH
      payload['boundAgentId'] = '';
      payload['boundLifetime'] = '';
    }

    if (!this.editingEndpoint) {
      payload['name'] = this.formName.trim();
    }

    this.saving = true;
    this.formError = null;
    try {
      const basePath = `/api/v1/projects/${encodeURIComponent(this.projectId)}/webhook-endpoints`;
      const res = await apiFetch(
        this.editingEndpoint
          ? `${basePath}/${encodeURIComponent(this.editingEndpoint.id)}`
          : basePath,
        {
          method: this.editingEndpoint ? 'PATCH' : 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload),
          suppressAccessDeniedToast: true,
        }
      );
      if (!res.ok) {
        this.formError = await extractApiError(res, `HTTP ${res.status}`);
        return;
      }
      const data = (await res.json()) as WebhookEndpointCreated;
      const wasCreate = !this.editingEndpoint;
      this.dialogOpen = false;
      this.editingEndpoint = null;

      if (wasCreate && data.secret) {
        this.secretDialogTitle = `Webhook Endpoint Created: ${data.name}`;
        this.revealedEndpointUrl = data.url;
        this.revealedSignatureMode = data.signatureMode || 'scion_v1';
        this.revealedSecret = data.secret;
        this.revealedGraceExpires = '';
        this.secretDialogOpen = true;
      }

      await this.loadEndpoints();
    } catch (err) {
      this.formError = err instanceof Error ? err.message : 'Request failed';
    } finally {
      this.saving = false;
    }
  }

  private async toggleStatus(ep: WebhookEndpoint): Promise<void> {
    const nextStatus = ep.status === 'active' ? 'disabled' : 'active';
    try {
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/webhook-endpoints/${encodeURIComponent(ep.id)}`,
        {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ status: nextStatus }),
        }
      );
      if (!res.ok) {
        this.error = await extractApiError(res, `HTTP ${res.status}`);
        return;
      }
      await this.loadEndpoints();
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to update endpoint status';
    }
  }

  private async confirmRotateSecret(): Promise<void> {
    if (!this.rotateTarget) return;
    this.rotating = true;
    try {
      const target = this.rotateTarget;
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/webhook-endpoints/${encodeURIComponent(target.id)}/rotate-secret`,
        { method: 'POST' }
      );
      if (!res.ok) {
        this.error = await extractApiError(res, `HTTP ${res.status}`);
        this.rotateTarget = null;
        return;
      }
      const data = (await res.json()) as WebhookEndpointRotated;
      this.rotateTarget = null;
      this.secretDialogTitle = `Secret Rotated: ${data.name}`;
      this.revealedEndpointUrl = data.url;
      this.revealedSignatureMode = data.signatureMode || target.signatureMode || 'scion_v1';
      this.revealedSecret = data.secret;
      this.revealedGraceExpires = data.graceWindowExpires ?? '';
      this.secretDialogOpen = true;
      await this.loadEndpoints();
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to rotate secret';
      this.rotateTarget = null;
    } finally {
      this.rotating = false;
    }
  }

  private async confirmDelete(): Promise<void> {
    if (!this.deleteTarget) return;
    this.deleting = true;
    try {
      const target = this.deleteTarget;
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/webhook-endpoints/${encodeURIComponent(target.id)}`,
        { method: 'DELETE' }
      );
      if (!res.ok) {
        this.error = await extractApiError(res, `HTTP ${res.status}`);
        return;
      }
      this.deleteTarget = null;
      await this.loadEndpoints();
      this.dispatchEvent(
        new CustomEvent('triggers-refresh-requested', { bubbles: true, composed: true })
      );
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to delete endpoint';
    } finally {
      this.deleting = false;
    }
  }

  private closeSecretDialog(): void {
    this.secretDialogOpen = false;
    this.clearSecretState();
  }

  private async copyToClipboard(text: string, field: 'url' | 'secret' | 'curl'): Promise<void> {
    try {
      await navigator.clipboard.writeText(text);
      this.copiedField = field;
      setTimeout(() => {
        if (this.copiedField === field) this.copiedField = null;
      }, 2000);
    } catch {
      // Clipboard may be unavailable in non-secure contexts
    }
  }

  private buildCurlExample(mode: string, url: string, secret: string): string {
    return buildWebhookCurlExample(mode, url, secret);
  }

  private affectedTriggersFor(ep: WebhookEndpoint | null): TriggerDefinition[] {
    if (!ep) return [];
    return this.triggers.filter((t) => t.source.kind === 'webhook' && t.source.id === ep.id);
  }

  override render() {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading webhook endpoints...</p>
        </div>
      `;
    }

    return html`
      <div class="toolbar">
        <div class="toolbar-actions">
          <sl-button
            variant="primary"
            size="small"
            data-create-endpoint
            @click=${() => this.openCreateDialog()}
          >
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            Create Endpoint
          </sl-button>
        </div>
        <sl-button size="small" variant="default" @click=${() => this.loadEndpoints()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Refresh
        </sl-button>
      </div>

      ${this.error
        ? html`
            <sl-alert variant="danger" open style="margin-bottom: 1rem;">
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              ${this.error}
            </sl-alert>
          `
        : nothing}
      ${this.endpoints.length === 0
        ? html`
            <div class="empty-state">
              <sl-icon name="lightning-charge"></sl-icon>
              <h3>No webhook endpoints</h3>
              <p>Create a webhook endpoint to receive signed events into this project.</p>
            </div>
          `
        : html`
            <div class="table-container">
              <table>
                <thead>
                  <tr>
                    <th>Endpoint</th>
                    <th>Auth &amp; Mode</th>
                    <th>Status</th>
                    <th>Activity</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  ${this.endpoints.map(
                    (ep) => html`
                      <tr data-endpoint-row=${ep.id}>
                        <td>
                          <div class="meta-stack">
                            <span class="endpoint-name">${ep.name}</span>
                            ${ep.description
                              ? html`<span class="endpoint-desc">${ep.description}</span>`
                              : nothing}
                            <span class="endpoint-url">${ep.url}</span>
                            ${ep.boundAgent
                              ? html`
                                  <div class="badge-row">
                                    <sl-badge variant="primary">
                                      Bound: ${ep.boundAgent.name || ep.boundAgent.id}
                                      (${ep.boundLifetime || 'agent'})
                                    </sl-badge>
                                  </div>
                                `
                              : nothing}
                          </div>
                        </td>
                        <td>
                          <div class="badge-row">
                            <sl-badge variant="neutral">${ep.auth}</sl-badge>
                            ${ep.auth === 'hmac'
                              ? html`<sl-badge variant="neutral">${ep.signatureMode}</sl-badge>`
                              : nothing}
                            ${ep.auth === 'hub_token' && ep.allowProjectAgents
                              ? html`<sl-badge variant="neutral">project agents</sl-badge>`
                              : nothing}
                          </div>
                        </td>
                        <td>
                          <sl-badge variant=${ep.status === 'active' ? 'success' : 'neutral'}>
                            ${ep.status}
                          </sl-badge>
                        </td>
                        <td>
                          <div class="meta-stack">
                            <span>${ep.receiveCount} received / ${ep.rejectCount} rejected</span>
                            ${ep.lastReceivedAt
                              ? html`
                                  <span
                                    class="endpoint-desc"
                                    title=${formatInstantWithZone(ep.lastReceivedAt)}
                                  >
                                    Last: ${formatRelative(ep.lastReceivedAt)}
                                  </span>
                                `
                              : html`<span class="endpoint-desc">Never received</span>`}
                          </div>
                        </td>
                        <td class="actions-cell">
                          <sl-icon-button
                            name="pencil"
                            label="Edit endpoint"
                            data-edit-endpoint=${ep.id}
                            @click=${() => this.openEditDialog(ep)}
                          ></sl-icon-button>
                          <sl-icon-button
                            name=${ep.status === 'active' ? 'pause-circle' : 'play-circle'}
                            label=${ep.status === 'active' ? 'Disable endpoint' : 'Enable endpoint'}
                            data-toggle-endpoint=${ep.id}
                            @click=${() => this.toggleStatus(ep)}
                          ></sl-icon-button>
                          ${ep.auth === 'hmac'
                            ? html`
                                <sl-icon-button
                                  name="arrow-repeat"
                                  label="Rotate signing secret"
                                  data-rotate-endpoint=${ep.id}
                                  @click=${() => {
                                    this.rotateTarget = ep;
                                  }}
                                ></sl-icon-button>
                              `
                            : nothing}
                          <sl-icon-button
                            name="trash"
                            label="Delete endpoint"
                            data-delete-endpoint=${ep.id}
                            @click=${() => {
                              this.deleteTarget = ep;
                            }}
                          ></sl-icon-button>
                        </td>
                      </tr>
                    `
                  )}
                </tbody>
              </table>
            </div>
          `}
      ${this.renderFormDialog()} ${this.renderSecretDialog()} ${this.renderRotateDialog()}
      ${this.renderDeleteDialog()}
    `;
  }

  private renderFormDialog() {
    const isEdit = Boolean(this.editingEndpoint);
    return html`
      <sl-dialog
        label=${isEdit ? `Edit Endpoint: ${this.editingEndpoint?.name}` : 'Create Webhook Endpoint'}
        ?open=${this.dialogOpen}
        @sl-after-hide=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.dialogOpen = false;
          this.editingEndpoint = null;
        }}
      >
        <div class="dialog-form">
          ${this.formError
            ? html`
                <sl-alert variant="danger" open>
                  <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                  ${this.formError}
                </sl-alert>
              `
            : nothing}
          ${!isEdit
            ? html`
                <sl-input
                  label="Name"
                  required
                  placeholder="ci-deploys"
                  .value=${this.formName}
                  data-endpoint-name-input
                  @sl-input=${(e: Event) => {
                    this.formName = (e.target as HTMLInputElement).value;
                  }}
                ></sl-input>
              `
            : nothing}

          <sl-input
            label="Description"
            placeholder="Optional description"
            .value=${this.formDescription}
            @sl-input=${(e: Event) => {
              this.formDescription = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>

          <div class="field-grid">
            <sl-select
              label="Auth mode"
              hoist
              .value=${this.formAuth}
              data-endpoint-auth-select
              @sl-change=${(e: Event) => {
                this.formAuth = (e.target as HTMLSelectElement).value;
              }}
            >
              <sl-option value="hmac">HMAC signature</sl-option>
              <sl-option value="hub_token">Hub token</sl-option>
            </sl-select>

            ${this.formAuth === 'hmac'
              ? html`
                  <sl-select
                    label="Signature mode"
                    hoist
                    data-signature-mode-select
                    .value=${this.formSignatureMode}
                    @sl-change=${(e: Event) => {
                      this.formSignatureMode = (e.target as HTMLSelectElement).value;
                    }}
                  >
                    <sl-option value="scion_v1">scion_v1 (replay protected)</sl-option>
                    <sl-option value="github_v1"
                      >github_v1 (GitHub webhooks, message rules only)</sl-option
                    >
                    <sl-option value="stripe_v1"
                      >stripe_v1 (Stripe webhooks, replay protected)</sl-option
                    >
                  </sl-select>
                `
              : html`
                  <div style="display: flex; align-items: flex-end; padding-bottom: 0.375rem;">
                    <sl-checkbox
                      ?checked=${this.formAllowProjectAgents}
                      @sl-change=${(e: Event) => {
                        this.formAllowProjectAgents = (e.target as HTMLInputElement).checked;
                      }}
                    >
                      Allow project agents to send
                    </sl-checkbox>
                  </div>
                `}
          </div>

          <div class="field-grid">
            <sl-select
              label="Bind to agent (optional)"
              hoist
              clearable
              .value=${this.formBoundAgentId}
              @sl-change=${(e: Event) => {
                this.formBoundAgentId = (e.target as HTMLSelectElement).value;
              }}
            >
              ${this.agents.map((ag) => html`<sl-option value=${ag.id}>${ag.name}</sl-option>`)}
            </sl-select>

            ${this.formBoundAgentId
              ? html`
                  <sl-select
                    label="Binding lifetime"
                    hoist
                    .value=${this.formBoundLifetime}
                    @sl-change=${(e: Event) => {
                      this.formBoundLifetime = (e.target as HTMLSelectElement).value;
                    }}
                  >
                    <sl-option value="agent">Agent lifetime (delete with agent)</sl-option>
                    <sl-option value="persistent">Persistent (unbind on delete)</sl-option>
                  </sl-select>
                `
              : nothing}
          </div>

          <div class="field-grid">
            <sl-input
              label="Max body bytes"
              type="number"
              min="1024"
              max="1048576"
              .value=${this.formMaxBodyBytes}
              @sl-input=${(e: Event) => {
                this.formMaxBodyBytes = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
            <sl-input
              label="Replay window (seconds)"
              type="number"
              min="30"
              max="900"
              .value=${this.formReplayWindowSeconds}
              @sl-input=${(e: Event) => {
                this.formReplayWindowSeconds = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>

          <div class="field-grid">
            <sl-input
              label="Rate limit (req/sec, optional)"
              type="number"
              min="1"
              .value=${this.formRatePerSecond}
              @sl-input=${(e: Event) => {
                this.formRatePerSecond = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
            <sl-input
              label="Rate burst (optional)"
              type="number"
              min="1"
              .value=${this.formRateBurst}
              @sl-input=${(e: Event) => {
                this.formRateBurst = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
        </div>

        <sl-button
          slot="footer"
          variant="default"
          @click=${() => {
            this.dialogOpen = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.saving}
          data-submit-endpoint
          @click=${() => this.submitEndpointForm()}
        >
          ${isEdit ? 'Save Changes' : 'Create Endpoint'}
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderSecretDialog() {
    const secret = this.revealedSecret ?? '';
    const curlSnippet = secret
      ? this.buildCurlExample(this.revealedSignatureMode, this.revealedEndpointUrl, secret)
      : '';
    return html`
      <sl-dialog
        label=${this.secretDialogTitle || 'Webhook Secret'}
        ?open=${this.secretDialogOpen}
        data-secret-dialog
        @sl-after-hide=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.closeSecretDialog();
        }}
      >
        <div class="dialog-form">
          <sl-alert variant="warning" open>
            <sl-icon slot="icon" name="shield-lock"></sl-icon>
            Copy this signing secret now. It is shown <strong>once</strong> and cannot be retrieved
            again after you close this dialog.
            ${this.revealedGraceExpires
              ? html`<br />The previous secret remains valid during the 24-hour grace window until
                  ${formatInstantWithZone(this.revealedGraceExpires)}.`
              : nothing}
          </sl-alert>

          <div>
            <div class="endpoint-desc">Ingress URL</div>
            <div class="secret-box">
              <code data-revealed-url>${this.revealedEndpointUrl}</code>
              <sl-icon-button
                name=${this.copiedField === 'url' ? 'check' : 'copy'}
                label="Copy URL"
                @click=${() => this.copyToClipboard(this.revealedEndpointUrl, 'url')}
              ></sl-icon-button>
            </div>
          </div>

          ${secret
            ? html`
                <div>
                  <div class="endpoint-desc">Signing Secret</div>
                  <div class="secret-box">
                    <code data-revealed-secret>${secret}</code>
                    <sl-icon-button
                      name=${this.copiedField === 'secret' ? 'check' : 'copy'}
                      label="Copy secret"
                      @click=${() => this.copyToClipboard(secret, 'secret')}
                    ></sl-icon-button>
                  </div>
                </div>

                ${curlSnippet
                  ? html`
                      <div>
                        <div
                          style="display: flex; align-items: center; justify-content: space-between; margin-bottom: 0.25rem;"
                        >
                          <span class="endpoint-desc"
                            >Example signed sender (${this.revealedSignatureMode}, curl +
                            openssl)</span
                          >
                          <sl-button
                            size="small"
                            variant="text"
                            data-copy-curl-example
                            @click=${() => this.copyToClipboard(curlSnippet, 'curl')}
                          >
                            <sl-icon
                              slot="prefix"
                              name=${this.copiedField === 'curl' ? 'check' : 'copy'}
                            ></sl-icon>
                            ${this.copiedField === 'curl' ? 'Copied' : 'Copy snippet'}
                          </sl-button>
                        </div>
                        <pre class="curl-example" data-curl-example>${curlSnippet}</pre>
                      </div>
                    `
                  : nothing}
              `
            : nothing}
        </div>

        <sl-button
          slot="footer"
          variant="primary"
          data-close-secret-dialog
          @click=${() => this.closeSecretDialog()}
        >
          Done
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderRotateDialog() {
    return html`
      <sl-dialog
        label="Rotate Signing Secret"
        ?open=${Boolean(this.rotateTarget)}
        @sl-after-hide=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.rotateTarget = null;
        }}
      >
        <p>Rotate the HMAC signing secret for <strong>${this.rotateTarget?.name}</strong>?</p>
        <p class="endpoint-desc">
          A new secret will be generated and shown once. The previous secret will remain valid for a
          24-hour grace window while senders are updated.
        </p>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => {
            this.rotateTarget = null;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="warning"
          ?loading=${this.rotating}
          data-confirm-rotate
          @click=${() => this.confirmRotateSecret()}
        >
          Rotate Secret
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderDeleteDialog() {
    const affected = this.affectedTriggersFor(this.deleteTarget);
    return html`
      <sl-dialog
        label="Delete Webhook Endpoint"
        ?open=${Boolean(this.deleteTarget)}
        @sl-after-hide=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.deleteTarget = null;
        }}
      >
        <p>
          Delete webhook endpoint <strong>${this.deleteTarget?.name}</strong>? Its signing secrets
          will be permanently removed.
        </p>
        ${affected.length > 0
          ? html`
              <sl-alert variant="warning" open>
                <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                The following ${affected.length} rule${affected.length === 1 ? '' : 's'} will be
                paused (<code>source_deleted</code>):
                <ul class="affected-list">
                  ${affected.map((tr) => html`<li>${tr.name}</li>`)}
                </ul>
              </sl-alert>
            `
          : nothing}
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => {
            this.deleteTarget = null;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="danger"
          ?loading=${this.deleting}
          data-confirm-delete-endpoint
          @click=${() => this.confirmDelete()}
        >
          Delete Endpoint
        </sl-button>
      </sl-dialog>
    `;
  }
}

/** Hub-supported HMAC webhook signature modes (`pkg/hub/handlers_webhook_endpoints.go`). */
export const SUPPORTED_SIGNATURE_MODES = ['scion_v1', 'github_v1', 'stripe_v1'] as const;

/**
 * Builds the scion_v1 curl + openssl sender example matching `cmd/trigger_webhook.go`
 * and `pkg/hub/trigger/signature.go` (`VerifyScionV1SignatureWithWindow`).
 *
 * Canonical string: `"v1\n" + t + "\n" + delivery + "\n" + event + "\n" + subject + "\n" + body`.
 */
export function buildScionV1CurlExample(url: string, secret: string): string {
  return [
    `HOOK_URL="${url}"`,
    `SECRET="${secret}"`,
    `TS="$(date +%s)"`,
    `DELIVERY="$(uuidgen 2>/dev/null || date +%s%N)"`,
    `# X-Scion-Event: <name> sets event.type=webhook.<name> (default webhook.received rules then stop matching; use --type webhook.<name>)`,
    `EVENT=""`,
    `# X-Scion-Subject is stored as event.subject=webhook:<endpoint-id>/<subject>`,
    `SUBJECT=""`,
    `BODY='{"status":"ok","commit":"abc1234"}'`,
    `SIG="$(printf 'v1\\n%s\\n%s\\n%s\\n%s\\n%s' "$TS" "$DELIVERY" "$EVENT" "$SUBJECT" "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $NF}')"`,
    `curl -fsS -X POST "$HOOK_URL" \\`,
    `  -H "Content-Type: application/json" \\`,
    `  -H "X-Scion-Signature: t=\${TS},v1=\${SIG}" \\`,
    `  -H "X-Scion-Delivery: \${DELIVERY}" \\`,
    `  -H "X-Scion-Event: \${EVENT}" \\`,
    `  -H "X-Scion-Subject: \${SUBJECT}" \\`,
    `  --data-raw "$BODY"`,
  ].join('\n');
}

/**
 * Builds the github_v1 curl + openssl sender example matching `cmd/trigger_webhook.go`
 * and `pkg/hub/trigger/verifier_github_v1.go`.
 */
export function buildGitHubV1CurlExample(url: string, secret: string): string {
  return [
    `HOOK_URL="${url}"`,
    `SECRET="${secret}"`,
    `DELIVERY="$(uuidgen 2>/dev/null || date +%s%N)"`,
    `BODY='{"action":"opened","repository":{"full_name":"org/repo"}}'`,
    `SIG="$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $NF}')"`,
    `curl -fsS -X POST "$HOOK_URL" \\`,
    `  -H "Content-Type: application/json" \\`,
    `  -H "X-Hub-Signature-256: sha256=\${SIG}" \\`,
    `  -H "X-GitHub-Event: issues" \\`,
    `  -H "X-GitHub-Delivery: \${DELIVERY}" \\`,
    `  --data-raw "$BODY"`,
  ].join('\n');
}

/**
 * Builds the stripe_v1 curl + openssl sender example matching `cmd/trigger_webhook.go`
 * and `pkg/hub/trigger/verifier_stripe_v1.go`.
 */
export function buildStripeV1CurlExample(url: string, secret: string): string {
  return [
    `HOOK_URL="${url}"`,
    `SECRET="${secret}"`,
    `TS="$(date +%s)"`,
    `BODY='{"id":"evt_123","type":"invoice.payment_succeeded","data":{"object":{"id":"in_456"}}}'`,
    `SIG="$(printf '%s.%s' "$TS" "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $NF}')"`,
    `curl -fsS -X POST "$HOOK_URL" \\`,
    `  -H "Content-Type: application/json" \\`,
    `  -H "Stripe-Signature: t=\${TS},v1=\${SIG}" \\`,
    `  --data-raw "$BODY"`,
  ].join('\n');
}

/**
 * Builds the curl + openssl sender example for a hub-supported HMAC signature mode,
 * or returns `""` for unrecognized modes.
 */
export function buildWebhookCurlExample(mode: string, url: string, secret: string): string {
  switch (mode) {
    case 'scion_v1':
      return buildScionV1CurlExample(url, secret);
    case 'github_v1':
      return buildGitHubV1CurlExample(url, secret);
    case 'stripe_v1':
      return buildStripeV1CurlExample(url, secret);
    default:
      return '';
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-webhook-endpoint-list': ScionWebhookEndpointList;
  }
}
