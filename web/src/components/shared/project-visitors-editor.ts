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
 * Project Visitors editor (ptone/scion#4603)
 *
 * The Visitors tab of project settings > Members. A project owner or admin
 * gives users of a trusted issuer visitor access, by full email (a grant)
 * or by email domain (a visitor rule), with an optional expiry; the list
 * shows each rule and each visitor with its issuer, status, who granted it
 * and its expiry, and offers change expiry and revoke.
 *
 *  - List:   GET    /api/v1/projects/{id}/visitors and /visitors/rules
 *  - Add:    POST   /api/v1/projects/{id}/visitors        {issuer, email, expiresAt?}
 *            POST   /api/v1/projects/{id}/visitors/rules  {issuer, domain, expiresAt?}
 *  - Change expiry: PATCH …/visitors/{visitorId} or …/visitors/rules/{ruleId}
 *            {notBefore, expiresAt}; the PATCH replaces the whole window,
 *            so the current notBefore is sent back unchanged. The new
 *            expiry counts from now, so it can also shorten the window.
 *  - Revoke: DELETE …/visitors/{visitorId} or …/visitors/rules/{ruleId}
 *
 * The page renders this editor only while the federation.project_visitors
 * experiment is on and the caller holds project.manage_visitors; the hub
 * enforces both on every request and its refusal text is shown unchanged.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { loadAdminStatus } from '../../client/admin-status.js';
import { principalLabel, principalName } from '../../client/principal-names.js';
import {
  VISITOR_EXPIRY_CHOICES,
  expiryFromChoice,
  issuerShapeError,
  loadVisitorIssuerChoices,
  normalizeVisitorDomain,
  projectVisitorRulesUrl,
  projectVisitorsUrl,
  visitorStatusLabel,
  visitorWindowState,
  type ProjectVisitor,
  type ProjectVisitorRule,
  type VisitorIssuerChoices,
  type VisitorWindow,
} from '../../shared/project-visitors.js';
import { formatInstant } from '../../utils/time.js';
import { showConfirm } from './confirm-dialog.js';

/** How a new visitor is addressed: one email, or every email of a domain. */
export type VisitorAddKind = 'email' | 'domain';

/** The grant or rule an extend dialog changes. */
interface ExtendTarget {
  kind: 'visitor' | 'rule';
  id: string;
  label: string;
  notBefore: string | null;
}

/** Shown in the add dialog when no issuer accepts visitor grants. */
export const NO_VISITOR_ISSUERS_MESSAGE =
  'No trusted issuer accepts visitor access yet. A hub admin turns on federation, and visitor access per issuer, in Admin > Federation.';

/** The hint of the issuer text field (callers who are not hub admins). */
export const ISSUER_TEXT_HINT =
  "The issuer URL of the visitor's hub or identity provider. A hub admin must first allow visitor access for that issuer.";

/** The description under the Visitors heading. */
const VISITORS_DESCRIPTION =
  "Users of a trusted issuer who may message this project's agents without being members. Add them by email, or by email domain.";

/** The hint under the domain field. */
const DOMAIN_HINT = 'Matches the domain exactly; subdomains are not included.';

/** Shown when the hub answers the visitor routes with 404 (feature off on the hub). */
export const VISITORS_UNAVAILABLE_MESSAGE = 'Visitor access is not available on this hub.';

@customElement('scion-project-visitors-editor')
export class ScionProjectVisitorsEditor extends LitElement {
  /** Project ID whose visitors are managed. */
  @property() projectId = '';

  /** The signed-in user's ID (admin status lookup, "You" in Granted by). */
  @property() userId = '';

  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private visitors: ProjectVisitor[] = [];
  @state() private rules: ProjectVisitorRule[] = [];
  @state() private issuerChoices: VisitorIssuerChoices | null = null;
  @state() private grantedByNames: Record<string, string> = {};
  @state() private feedback: { message: string; variant: 'success' | 'danger' } | null = null;
  @state() private removingKey: string | null = null;

  // Add dialog
  @state() private addOpen = false;
  @state() private addKind: VisitorAddKind = 'email';
  @state() private addIssuer = '';
  @state() private addValue = '';
  @state() private addExpiry = '';
  @state() private addSaving = false;
  @state() private addError: string | null = null;

  // Extend dialog
  @state() private extendTarget: ExtendTarget | null = null;
  @state() private extendExpiry = '30';
  @state() private extendSaving = false;
  @state() private extendError: string | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .section-header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 1rem;
      gap: 1rem;
    }

    .section-header h3 {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .section-header p {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0;
    }

    .table-container {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow-x: auto;
    }

    table {
      width: 100%;
      border-collapse: collapse;
    }

    th {
      text-align: left;
      padding: 0.75rem 1rem;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }

    tr:last-child td {
      border-bottom: none;
    }

    .issuer-cell {
      max-width: 16rem;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .meta-text {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
    }

    .actions-cell {
      white-space: nowrap;
      text-align: right;
    }

    .empty-state,
    .loading-state,
    .error-state {
      padding: 1.5rem;
      text-align: center;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }

    .error-state {
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 0.75rem;
    }

    .feedback-alert {
      margin-bottom: 1rem;
    }

    .dialog-form {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .field-error,
    .dialog-error {
      font-size: 0.8125rem;
      color: var(--scion-error-text, #991b1b);
    }

    .suggestions {
      display: flex;
      flex-wrap: wrap;
      gap: 0.25rem;
      align-items: center;
    }
  `;

  override updated(changed: Map<string, unknown>): void {
    // The first update carries the initial projectId; a later change of
    // project reloads.
    if (changed.has('projectId') && this.projectId) {
      void this.loadData();
    }
  }

  // ---------------------------------------------------------------------------
  // Data
  // ---------------------------------------------------------------------------

  private async loadData(): Promise<void> {
    if (!this.projectId) return;
    this.loading = true;
    this.error = null;
    try {
      const [vRes, rRes] = await Promise.all([
        apiFetch(projectVisitorsUrl(this.projectId), { suppressAccessDeniedToast: true }),
        apiFetch(projectVisitorRulesUrl(this.projectId), { suppressAccessDeniedToast: true }),
      ]);
      if (vRes.status === 404 || rRes.status === 404) {
        this.visitors = [];
        this.rules = [];
        this.error = VISITORS_UNAVAILABLE_MESSAGE;
        return;
      }
      if (!vRes.ok) {
        this.error = await extractApiError(vRes, 'Failed to load visitors');
        return;
      }
      if (!rRes.ok) {
        this.error = await extractApiError(rRes, 'Failed to load visitor rules');
        return;
      }
      const vBody = (await vRes.json()) as { visitors?: ProjectVisitor[] };
      const rBody = (await rRes.json()) as { rules?: ProjectVisitorRule[] };
      this.visitors = vBody.visitors ?? [];
      this.rules = rBody.rules ?? [];
      this.resolveGrantedBy();
    } catch {
      this.error = 'Failed to load visitors';
    } finally {
      this.loading = false;
    }
  }

  /** Looks up the display names of the users who granted access. */
  private resolveGrantedBy(): void {
    const ids = new Set<string>();
    for (const v of this.visitors) if (v.grantedBy) ids.add(v.grantedBy);
    for (const r of this.rules) if (r.createdBy) ids.add(r.createdBy);
    for (const id of ids) {
      if (id in this.grantedByNames) continue;
      void principalName('user', id).then((name) => {
        this.grantedByNames = { ...this.grantedByNames, [id]: name };
      });
    }
  }

  private grantedByLabel(id: string | undefined): string {
    if (!id) return '—';
    return principalLabel('user', id, this.grantedByNames[id] ?? '', this.userId);
  }

  // ---------------------------------------------------------------------------
  // Add
  // ---------------------------------------------------------------------------

  private async openAddDialog(): Promise<void> {
    this.addKind = 'email';
    this.addIssuer = '';
    this.addValue = '';
    this.addExpiry = '';
    this.addError = null;
    this.addSaving = false;
    this.addOpen = true;
    const adminStatus = await loadAdminStatus(this.userId);
    const choices = await loadVisitorIssuerChoices(adminStatus, this.visitors, this.rules);
    this.issuerChoices = choices;
    if (choices.mode === 'picker' && choices.issuers.length === 1) {
      this.addIssuer = choices.issuers[0]!;
    }
  }

  private closeAddDialog(): void {
    this.addOpen = false;
    this.addError = null;
  }

  /** Why the add form cannot be sent yet, or null. */
  private addFormError(): string | null {
    const choices = this.issuerChoices;
    if (!choices) return 'Loading issuers';
    if (choices.mode === 'picker') {
      if (!choices.issuers.includes(this.addIssuer.trim())) return 'Choose an issuer';
    } else {
      const shape = issuerShapeError(this.addIssuer);
      if (shape) return shape;
    }
    if (this.addKind === 'email') {
      const email = this.addValue.trim();
      const at = email.indexOf('@');
      if (at <= 0 || at === email.length - 1 || email.indexOf('@', at + 1) !== -1) {
        return 'Enter a full email address';
      }
    } else {
      const domain = normalizeVisitorDomain(this.addValue);
      if (!domain || domain.includes('@') || !domain.includes('.')) {
        return 'Enter an email domain, such as example.com';
      }
    }
    return null;
  }

  private async submitAdd(): Promise<void> {
    const formError = this.addFormError();
    if (formError) {
      this.addError = formError;
      return;
    }
    this.addSaving = true;
    this.addError = null;
    // Only surrounding whitespace is trimmed: the hub compares the issuer
    // with its trusted issuers.
    const issuer = this.addIssuer.trim();
    const expiresAt = expiryFromChoice(this.addExpiry);
    const body: Record<string, unknown> =
      this.addKind === 'email'
        ? { issuer, email: this.addValue.trim().toLowerCase() }
        : { issuer, domain: normalizeVisitorDomain(this.addValue) };
    if (expiresAt) body['expiresAt'] = expiresAt;
    const url =
      this.addKind === 'email'
        ? projectVisitorsUrl(this.projectId)
        : projectVisitorRulesUrl(this.projectId);
    try {
      const res = await apiFetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
        suppressAccessDeniedToast: true,
      });
      if (!res.ok) {
        this.addError = await extractApiError(res, `Failed to add visitor (HTTP ${res.status})`);
        return;
      }
      this.addOpen = false;
      this.feedback = {
        message:
          this.addKind === 'email'
            ? `Visitor access granted to ${String(body['email'])}`
            : `Visitor access granted to @${String(body['domain'])}`,
        variant: 'success',
      };
      await this.loadData();
    } catch {
      this.addError = 'Failed to add visitor';
    } finally {
      this.addSaving = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Extend
  // ---------------------------------------------------------------------------

  private openExtendDialog(target: ExtendTarget): void {
    this.extendTarget = target;
    this.extendExpiry = '30';
    this.extendError = null;
    this.extendSaving = false;
  }

  private closeExtendDialog(): void {
    this.extendTarget = null;
    this.extendError = null;
  }

  private async submitExtend(): Promise<void> {
    const target = this.extendTarget;
    if (!target) return;
    this.extendSaving = true;
    this.extendError = null;
    const url =
      target.kind === 'visitor'
        ? `${projectVisitorsUrl(this.projectId)}/${encodeURIComponent(target.id)}`
        : `${projectVisitorRulesUrl(this.projectId)}/${encodeURIComponent(target.id)}`;
    try {
      const res = await apiFetch(url, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          notBefore: target.notBefore,
          expiresAt: expiryFromChoice(this.extendExpiry),
        }),
        suppressAccessDeniedToast: true,
      });
      if (!res.ok) {
        this.extendError = await extractApiError(
          res,
          `Failed to change the expiry (HTTP ${res.status})`
        );
        return;
      }
      this.extendTarget = null;
      this.feedback = { message: `Expiry of ${target.label} changed`, variant: 'success' };
      await this.loadData();
    } catch {
      this.extendError = 'Failed to change the expiry';
    } finally {
      this.extendSaving = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Revoke
  // ---------------------------------------------------------------------------

  private async revoke(kind: 'visitor' | 'rule', id: string, label: string): Promise<void> {
    const message =
      kind === 'visitor'
        ? `Revoke visitor access for ${label}? Their next request to this project is refused.`
        : `Remove the visitor rule for ${label}? Every visitor who has access through this rule loses it.`;
    const confirmed = await showConfirm(message, {
      title: kind === 'visitor' ? 'Revoke visitor' : 'Remove visitor rule',
      confirmText: kind === 'visitor' ? 'Revoke' : 'Remove',
    });
    if (!confirmed) return;
    const key = `${kind}:${id}`;
    this.removingKey = key;
    const url =
      kind === 'visitor'
        ? `${projectVisitorsUrl(this.projectId)}/${encodeURIComponent(id)}`
        : `${projectVisitorRulesUrl(this.projectId)}/${encodeURIComponent(id)}`;
    try {
      const res = await apiFetch(url, { method: 'DELETE', suppressAccessDeniedToast: true });
      if (!res.ok && res.status !== 204) {
        this.feedback = {
          message: await extractApiError(res, `Failed to revoke (HTTP ${res.status})`),
          variant: 'danger',
        };
        return;
      }
      this.feedback = {
        message:
          kind === 'visitor' ? `Visitor access revoked for ${label}` : `Rule ${label} removed`,
        variant: 'success',
      };
      await this.loadData();
    } catch {
      this.feedback = { message: 'Failed to revoke', variant: 'danger' };
    } finally {
      this.removingKey = null;
    }
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    return html`
      ${this.renderFeedback()}
      <div class="section-header">
        <div>
          <h3>Visitors</h3>
          <p>${VISITORS_DESCRIPTION}</p>
        </div>
        <sl-button
          size="small"
          variant="default"
          data-add-visitor
          @click=${() => {
            void this.openAddDialog();
          }}
        >
          <sl-icon slot="prefix" name="person-plus"></sl-icon>
          Add Visitor
        </sl-button>
      </div>
      ${this.renderBody()} ${this.renderAddDialog()} ${this.renderExtendDialog()}
    `;
  }

  private renderFeedback() {
    if (!this.feedback) return nothing;
    return html`
      <sl-alert
        class="feedback-alert"
        variant=${this.feedback.variant}
        open
        closable
        duration="5000"
        @sl-after-hide=${() => {
          this.feedback = null;
        }}
      >
        <sl-icon
          slot="icon"
          name=${this.feedback.variant === 'success' ? 'check-circle' : 'exclamation-triangle'}
        ></sl-icon>
        ${this.feedback.message}
      </sl-alert>
    `;
  }

  private renderBody() {
    if (this.loading) {
      return html`<div class="loading-state"><sl-spinner></sl-spinner> Loading visitors...</div>`;
    }
    if (this.error) {
      return html`
        <div class="error-state" data-visitors-error>
          <span>${this.error}</span>
          <sl-button size="small" @click=${() => this.loadData()}>Retry</sl-button>
        </div>
      `;
    }
    if (this.visitors.length === 0 && this.rules.length === 0) {
      return html`<div class="empty-state" data-visitors-empty>No visitors yet.</div>`;
    }
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Visitor</th>
              <th>Issuer</th>
              <th>Status</th>
              <th>Granted by</th>
              <th>Expires</th>
              <th class="actions-cell">Actions</th>
            </tr>
          </thead>
          <tbody>
            ${this.rules.map((r) => this.renderRuleRow(r))}
            ${this.visitors.map((v) => this.renderVisitorRow(v))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderStatus(status: string | undefined, issuerAccepted: boolean, bounds: VisitorWindow) {
    const now = new Date();
    const label = visitorStatusLabel(status, issuerAccepted, bounds, now);
    let variant = 'success';
    if (!issuerAccepted) variant = 'danger';
    else if (visitorWindowState(bounds, now) !== 'current') variant = 'neutral';
    else if (status === 'pending') variant = 'neutral';
    else if (status === 'suspended') variant = 'warning';
    return html`<sl-badge variant=${variant} pill data-visitor-status>${label}</sl-badge>`;
  }

  private renderExpiry(expiresAt: string | undefined) {
    if (!expiresAt) return html`<span class="meta-text">Never</span>`;
    return html`<span title=${expiresAt}>${formatInstant(expiresAt, 'datetime')}</span>`;
  }

  private renderRuleRow(rule: ProjectVisitorRule) {
    const label = `@${rule.domain}`;
    const key = `rule:${rule.id}`;
    return html`
      <tr data-rule-row=${rule.id}>
        <td>
          ${label}
          <div class="meta-text">Everyone with an email at this domain</div>
        </td>
        <td class="issuer-cell" title=${rule.issuer}>${rule.issuer}</td>
        <td>${this.renderStatus(undefined, rule.issuerAccepted, rule)}</td>
        <td>${this.grantedByLabel(rule.createdBy)}</td>
        <td>${this.renderExpiry(rule.expiresAt)}</td>
        <td class="actions-cell">
          <sl-button
            size="small"
            variant="default"
            data-action="extend"
            @click=${() =>
              this.openExtendDialog({
                kind: 'rule',
                id: rule.id,
                label,
                notBefore: rule.notBefore ?? null,
              })}
          >
            Change expiry
          </sl-button>
          <sl-button
            size="small"
            variant="danger"
            outline
            data-action="revoke"
            ?loading=${this.removingKey === key}
            @click=${() => this.revoke('rule', rule.id, label)}
          >
            Remove
          </sl-button>
        </td>
      </tr>
    `;
  }

  private renderVisitorRow(visitor: ProjectVisitor) {
    const key = `visitor:${visitor.id}`;
    const fromRule = visitor.source === 'rule';
    const rule = fromRule ? this.rules.find((r) => r.id === visitor.sourceRuleId) : undefined;
    return html`
      <tr data-visitor-row=${visitor.id}>
        <td>
          ${visitor.email}
          ${fromRule
            ? html`<div class="meta-text">
                Through the rule ${rule ? `@${rule.domain}` : 'for its domain'}
              </div>`
            : nothing}
        </td>
        <td class="issuer-cell" title=${visitor.issuer}>${visitor.issuer}</td>
        <td>${this.renderStatus(visitor.status, visitor.issuerAccepted, visitor)}</td>
        <td>${this.grantedByLabel(visitor.grantedBy)}</td>
        <td>${this.renderExpiry(visitor.expiresAt)}</td>
        <td class="actions-cell">
          ${fromRule
            ? nothing
            : html`<sl-button
                size="small"
                variant="default"
                data-action="extend"
                @click=${() =>
                  this.openExtendDialog({
                    kind: 'visitor',
                    id: visitor.id,
                    label: visitor.email,
                    notBefore: visitor.notBefore ?? null,
                  })}
              >
                Change expiry
              </sl-button>`}
          <sl-button
            size="small"
            variant="danger"
            outline
            data-action="revoke"
            ?loading=${this.removingKey === key}
            @click=${() => this.revoke('visitor', visitor.id, visitor.email)}
          >
            Revoke
          </sl-button>
        </td>
      </tr>
    `;
  }

  private renderIssuerField() {
    const choices = this.issuerChoices;
    if (!choices) {
      return html`<div class="loading-state"><sl-spinner></sl-spinner></div>`;
    }
    if (choices.mode === 'picker') {
      if (choices.issuers.length === 0) {
        return html`<div class="hint" data-no-visitor-issuers>${NO_VISITOR_ISSUERS_MESSAGE}</div>`;
      }
      return html`
        <sl-select
          label="Issuer"
          data-issuer-picker
          .value=${this.addIssuer}
          placeholder="Choose an issuer"
          @sl-change=${(e: Event) => {
            this.addIssuer = (e.target as HTMLSelectElement).value;
          }}
        >
          ${choices.issuers.map((i) => html`<sl-option value=${i}>${i}</sl-option>`)}
        </sl-select>
        <span class="hint">Only issuers that allow visitor access are listed.</span>
      `;
    }
    return html`
      <sl-input
        label="Issuer"
        data-issuer-input
        placeholder="https://hub.example.com"
        .value=${this.addIssuer}
        @sl-input=${(e: Event) => {
          this.addIssuer = (e.target as HTMLInputElement).value;
        }}
      ></sl-input>
      <span class="hint">${ISSUER_TEXT_HINT}</span>
      ${choices.issuers.length > 0
        ? html`<div class="suggestions" data-issuer-suggestions>
            <span class="hint">Used on this project:</span>
            ${choices.issuers.map(
              (i) =>
                html`<sl-button
                  size="small"
                  variant="text"
                  @click=${() => {
                    this.addIssuer = i;
                  }}
                  >${i}</sl-button
                >`
            )}
          </div>`
        : nothing}
    `;
  }

  private renderAddDialog() {
    const noIssuers =
      this.issuerChoices?.mode === 'picker' && this.issuerChoices.issuers.length === 0;
    return html`
      <sl-dialog
        label="Add Visitor"
        ?open=${this.addOpen}
        @sl-request-close=${() => this.closeAddDialog()}
        style="--width: 32rem;"
      >
        ${this.addOpen
          ? html`
              <div class="dialog-form">
                ${this.renderIssuerField()}
                <sl-radio-group
                  label="Give access to"
                  data-add-kind
                  .value=${this.addKind}
                  @sl-change=${(e: Event) => {
                    this.addKind = (e.target as HTMLInputElement).value as VisitorAddKind;
                    this.addError = null;
                  }}
                >
                  <sl-radio-button value="email">One email</sl-radio-button>
                  <sl-radio-button value="domain">An email domain</sl-radio-button>
                </sl-radio-group>
                <sl-input
                  data-add-value
                  label=${this.addKind === 'email' ? 'Email' : 'Email domain'}
                  placeholder=${this.addKind === 'email' ? 'alice@example.com' : 'example.com'}
                  .value=${this.addValue}
                  @sl-input=${(e: Event) => {
                    this.addValue = (e.target as HTMLInputElement).value;
                  }}
                ></sl-input>
                ${this.addKind === 'domain'
                  ? html`<span class="hint">${DOMAIN_HINT}</span>`
                  : nothing}
                <sl-select
                  label="Expires"
                  data-add-expiry
                  .value=${this.addExpiry}
                  @sl-change=${(e: Event) => {
                    this.addExpiry = (e.target as HTMLSelectElement).value;
                  }}
                >
                  ${VISITOR_EXPIRY_CHOICES.map(
                    (c) => html`<sl-option value=${c.value}>${c.label}</sl-option>`
                  )}
                </sl-select>
                ${this.addError
                  ? html`<div class="dialog-error" data-add-error>${this.addError}</div>`
                  : nothing}
              </div>
              <div slot="footer">
                <sl-button variant="default" @click=${() => this.closeAddDialog()}
                  >Cancel</sl-button
                >
                <sl-button
                  variant="primary"
                  data-add-submit
                  ?loading=${this.addSaving}
                  ?disabled=${noIssuers}
                  @click=${() => {
                    void this.submitAdd();
                  }}
                >
                  Add
                </sl-button>
              </div>
            `
          : nothing}
      </sl-dialog>
    `;
  }

  private renderExtendDialog() {
    const target = this.extendTarget;
    return html`
      <sl-dialog
        label="Change Expiry"
        ?open=${target !== null}
        @sl-request-close=${() => this.closeExtendDialog()}
        style="--width: 28rem;"
      >
        ${target
          ? html`
              <div class="dialog-form">
                <p class="hint">New expiry for <strong>${target.label}</strong>, from now.</p>
                <sl-select
                  label="Expires"
                  data-extend-expiry
                  .value=${this.extendExpiry}
                  @sl-change=${(e: Event) => {
                    this.extendExpiry = (e.target as HTMLSelectElement).value;
                  }}
                >
                  ${VISITOR_EXPIRY_CHOICES.map(
                    (c) => html`<sl-option value=${c.value}>${c.label}</sl-option>`
                  )}
                </sl-select>
                ${this.extendError
                  ? html`<div class="dialog-error" data-extend-error>${this.extendError}</div>`
                  : nothing}
              </div>
              <div slot="footer">
                <sl-button variant="default" @click=${() => this.closeExtendDialog()}
                  >Cancel</sl-button
                >
                <sl-button
                  variant="primary"
                  data-extend-submit
                  ?loading=${this.extendSaving}
                  @click=${() => {
                    void this.submitExtend();
                  }}
                >
                  Save
                </sl-button>
              </div>
            `
          : nothing}
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-project-visitors-editor': ScionProjectVisitorsEditor;
  }
}
