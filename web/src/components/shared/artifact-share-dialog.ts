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
 * The Share dialog of an artifact (experiment hub.artifacts), for its owner
 * or an admin: share links (create with a fixed lifetime, shown once;
 * revoke), people and projects (grants), and the artifact's retention.
 *
 * A share link opens the artifact in the hub's sandboxed viewer; the token
 * is shown only in the answer that created it and never again.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { live } from 'lit/directives/live.js';

import { apiFetch } from '../../client/api.js';
import {
  DEFAULT_LINK_HOURS,
  LINK_LIFETIMES,
  PERMISSION_LABELS,
  createLink,
  deleteGrant,
  expiryImpact,
  listGrants,
  listLinks,
  patchArtifact,
  putGrant,
  revokeLink,
  shareLinkUrl,
} from '../../client/artifacts.js';
import type {
  Artifact,
  ArtifactGrant,
  CreateLinkResponse,
  GrantPermission,
  ShareLink,
} from '../../client/artifacts.js';
import { principalLabel, principalName, projectName } from '../../client/principal-names.js';
import { formatInstant, formatRelative } from '../../utils/time.js';

/** Delay before the subject search runs. */
const SEARCH_DELAY_MS = 250;

/** Fewest characters the subject search runs for. */
const MIN_SEARCH_LENGTH = 2;

/** Links ending sooner than this also show how far off they are. */
const SOON_MS = 3 * 24 * 60 * 60 * 1000;

/** A user or project the search offers. */
export interface ShareSubject {
  subjectKind: ArtifactGrant['subjectKind'];
  subjectRef: string;
  label: string;
  detail: string;
}

/** A retention choice: keep, the current expiry, or a number of days from now. */
export type RetentionChoice = 'never' | 'current' | `${number}d`;

/** The retention choices the dialog offers, besides the current expiry. */
export const RETENTION_DAYS = [1, 7, 30, 90] as const;

/** The expiry a retention choice stands for; null keeps the artifact until deleted. */
export function retentionExpiry(
  choice: RetentionChoice,
  current: string | undefined,
  now: Date
): Date | null {
  if (choice === 'never') return null;
  if (choice === 'current') return current ? new Date(current) : null;
  const days = Number(choice.slice(0, -1));
  return new Date(now.getTime() + days * 24 * 60 * 60 * 1000);
}

/** The sentence warning what an expiry change cuts, or '' when it cuts nothing. */
export function expiryWarning(linksCutShort: number, grantsRemoved: number): string {
  const parts: string[] = [];
  if (linksCutShort > 0) {
    parts.push(`${linksCutShort} ${linksCutShort === 1 ? 'link' : 'links'} will be cut short`);
  }
  if (grantsRemoved > 0) {
    parts.push(`${grantsRemoved} ${grantsRemoved === 1 ? 'grant' : 'grants'} will be removed`);
  }
  return parts.join(' and ');
}

@customElement('scion-artifact-share-dialog')
export class ScionArtifactShareDialog extends LitElement {
  @property({ type: Object }) artifact: Artifact | null = null;
  @property({ type: Boolean, reflect: true }) open = false;
  /** The signed-in user, shown as "You". */
  @property({ type: String }) currentUserId = '';

  @state() private loading = false;
  @state() private loadError: string | null = null;
  @state() private error: string | null = null;
  @state() private links: ShareLink[] = [];
  @state() private grants: ArtifactGrant[] = [];
  @state() private crossProjectSharing = false;
  @state() private ttlHours = DEFAULT_LINK_HOURS;
  @state() private creating = false;
  @state() private created: CreateLinkResponse | null = null;
  @state() private copied = false;
  @state() private busyId = '';
  @state() private query = '';
  @state() private suggestions: ShareSubject[] = [];
  @state() private subject: ShareSubject | null = null;
  @state() private permission: GrantPermission = 'read';
  @state() private adding = false;
  @state() private retention: RetentionChoice = 'never';
  @state() private saving = false;
  /** Display names by "user:id", "agent:id" or "project:id". */
  @state() private names = new Map<string, string>();

  private searchTimer: ReturnType<typeof setTimeout> | null = null;
  private searchGen = 0;
  private loadGen = 0;
  /**
   * Bumped whenever a created link is forgotten, so a link whose creation
   * answers after the dialog closed or reopened is never shown.
   */
  private createGen = 0;

  static override styles = css`
    sl-dialog::part(panel) {
      width: min(640px, 95vw);
    }
    section + section {
      border-top: 1px solid var(--scion-border, #e2e8f0);
      margin-top: 1rem;
      padding-top: 1rem;
    }
    h3 {
      display: flex;
      align-items: center;
      gap: 0.4rem;
      margin: 0 0 0.25rem;
      font-size: 0.875rem;
    }
    .hint {
      margin: 0 0 0.6rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }
    .hint sl-icon {
      vertical-align: -0.125em;
    }
    .row {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-bottom: 0.6rem;
    }
    .row label {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }
    .row .grow {
      flex: 1;
      min-width: 0;
    }
    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 0.8125rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
    }
    th {
      text-align: left;
      font-size: 0.6875rem;
      text-transform: uppercase;
      letter-spacing: 0.04em;
      color: var(--sl-color-neutral-600);
      padding: 0.4rem 0.6rem;
      background: var(--scion-bg-subtle, #f8fafc);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }
    td {
      padding: 0.4rem 0.6rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }
    tr:last-child td {
      border-bottom: none;
    }
    td.action {
      text-align: right;
    }
    .muted {
      color: var(--scion-text-muted, #64748b);
    }
    .empty {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0.25rem 0;
    }
    .created {
      margin-bottom: 0.6rem;
    }
    .created .url {
      display: flex;
      gap: 0.5rem;
      margin: 0.4rem 0;
    }
    .created .url sl-input {
      flex: 1;
    }
    .created .url sl-input::part(input) {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.75rem;
    }
    .picker {
      position: relative;
    }
    .suggestions {
      position: absolute;
      z-index: 10;
      left: 0;
      right: 0;
      top: 100%;
      margin: 0.125rem 0 0;
      padding: 0.25rem 0;
      list-style: none;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      box-shadow: var(--sl-shadow-medium);
      max-height: 14rem;
      overflow-y: auto;
    }
    .suggestions button {
      display: flex;
      width: 100%;
      gap: 0.5rem;
      align-items: center;
      padding: 0.35rem 0.6rem;
      border: none;
      background: none;
      font: inherit;
      font-size: 0.8125rem;
      text-align: left;
      cursor: pointer;
    }
    .suggestions button:hover,
    .suggestions button:focus-visible {
      background: var(--scion-bg-subtle, #f1f5f9);
    }
    .grant {
      display: flex;
      align-items: center;
      gap: 0.6rem;
      padding: 0.45rem 0;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      font-size: 0.875rem;
    }
    .grant .who {
      flex: 1;
      min-width: 0;
    }
    .grant .who small {
      display: block;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }
    .grant > sl-icon {
      color: var(--sl-color-neutral-500);
    }
    sl-alert {
      margin-bottom: 0.6rem;
    }
    .sr-only {
      position: absolute;
      width: 1px;
      height: 1px;
      overflow: hidden;
      clip: rect(0 0 0 0);
      white-space: nowrap;
    }
    .footer {
      display: flex;
      justify-content: flex-end;
      gap: 0.5rem;
    }
  `;

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.searchTimer) clearTimeout(this.searchTimer);
    this.forgetCreated();
  }

  override updated(changed: Map<string, unknown>): void {
    if (!changed.has('open')) return;
    if (this.open) {
      this.reset();
      void this.load();
    } else {
      // A created link is shown once: forget it as soon as the dialog
      // closes, so it stays neither in memory nor in the hidden dialog.
      this.forgetCreated();
    }
  }

  private forgetCreated(): void {
    this.createGen++;
    this.created = null;
    this.copied = false;
  }

  private reset(): void {
    this.error = null;
    this.forgetCreated();
    this.ttlHours = DEFAULT_LINK_HOURS;
    this.query = '';
    this.suggestions = [];
    this.subject = null;
    this.permission = 'read';
    this.retention = this.artifact?.expiresAt ? 'current' : 'never';
  }

  private async load(): Promise<void> {
    const a = this.artifact;
    if (!a) return;
    const gen = ++this.loadGen;
    this.loading = true;
    this.loadError = null;
    try {
      const [links, grants] = await Promise.all([listLinks(a.id), listGrants(a.id)]);
      if (gen !== this.loadGen) return;
      this.links = links;
      this.grants = grants.grants;
      this.crossProjectSharing = grants.crossProjectSharing;
      this.resolveNames();
    } catch (err) {
      if (gen !== this.loadGen) return;
      this.loadError = err instanceof Error ? err.message : 'Could not load sharing settings';
    } finally {
      if (gen === this.loadGen) this.loading = false;
    }
  }

  private resolveNames(): void {
    const want: Array<[string, Promise<string>]> = [];
    const add = (key: string, lookup: () => Promise<string>): void => {
      if (!this.names.has(key)) want.push([key, lookup()]);
    };
    for (const l of this.links) {
      const [kind, ref] = splitPrincipal(l.createdBy ?? '');
      if (kind) add(`${kind}:${ref}`, () => principalName(kind, ref));
    }
    for (const g of this.grants) {
      if (g.subjectKind === 'scope') {
        add(`project:${g.subjectRef}`, () => projectName(g.subjectRef));
      } else {
        const [kind, ref] = splitPrincipal(g.subjectRef);
        if (kind) add(`${kind}:${ref}`, () => principalName(kind, ref));
      }
    }
    for (const [key, p] of want) {
      void p.then((name) => {
        if (name && this.isConnected) this.names = new Map(this.names).set(key, name);
      });
    }
  }

  private close(): void {
    this.forgetCreated();
    this.dispatchEvent(new CustomEvent('artifact-share-closed', { bubbles: true, composed: true }));
  }

  private fail(err: unknown, fallback: string): void {
    this.error = err instanceof Error ? err.message : fallback;
  }

  // ── Share links ─────────────────────────────────────────────

  private async create(): Promise<void> {
    const a = this.artifact;
    if (!a || this.creating) return;
    this.creating = true;
    this.error = null;
    this.copied = false;
    const gen = this.createGen;
    try {
      const created = await createLink(a.id, this.ttlHours);
      // Closed (or closed and reopened) meanwhile: the link is not shown.
      // It is listed, and revocable, the next time the dialog opens.
      if (gen !== this.createGen || !this.open) return;
      this.created = created;
      this.links = [created.link, ...this.links.filter((l) => l.id !== created.link.id)];
      this.resolveNames();
    } catch (err) {
      this.fail(err, 'Could not create the link');
    } finally {
      this.creating = false;
    }
  }

  private async copyCreated(): Promise<void> {
    if (!this.created) return;
    try {
      await navigator.clipboard.writeText(shareLinkUrl(this.created.url, window.location.origin));
      this.copied = true;
    } catch {
      this.error = 'Could not copy. Select the link and copy it yourself.';
    }
  }

  private async revoke(link: ShareLink): Promise<void> {
    const a = this.artifact;
    if (!a || this.busyId) return;
    this.busyId = link.id;
    this.error = null;
    try {
      await revokeLink(a.id, link.id);
      this.links = this.links.filter((l) => l.id !== link.id);
      if (this.created?.link.id === link.id) this.created = null;
    } catch (err) {
      this.fail(err, 'Could not revoke the link');
    } finally {
      this.busyId = '';
    }
  }

  // ── People and projects ─────────────────────────────────────

  private onQuery(e: Event): void {
    this.query = (e.target as HTMLInputElement).value;
    this.subject = null;
    if (this.searchTimer) clearTimeout(this.searchTimer);
    const q = this.query.trim();
    if (q.length < MIN_SEARCH_LENGTH) {
      this.searchGen++;
      this.suggestions = [];
      return;
    }
    this.searchTimer = setTimeout(() => void this.search(q), SEARCH_DELAY_MS);
  }

  private async search(q: string): Promise<void> {
    const gen = ++this.searchGen;
    const users = searchUsers(q);
    const projects = this.crossProjectSharing ? searchProjects(q) : Promise.resolve([]);
    const [u, p] = await Promise.all([users, projects]);
    if (gen !== this.searchGen) return;
    const taken = new Set(this.grants.map((g) => `${g.subjectKind}:${g.subjectRef}`));
    this.suggestions = [...u, ...p].filter((s) => !taken.has(`${s.subjectKind}:${s.subjectRef}`));
  }

  private pick(s: ShareSubject): void {
    this.subject = s;
    this.query = s.label;
    this.suggestions = [];
  }

  private async add(): Promise<void> {
    const a = this.artifact;
    const s = this.subject;
    if (!a || !s || this.adding) return;
    this.adding = true;
    this.error = null;
    try {
      const g = await putGrant(a.id, s.subjectKind, s.subjectRef, this.permission);
      this.grants = [...this.grants.filter((x) => x.id !== g.id), g];
      const key = s.subjectKind === 'scope' ? `project:${s.subjectRef}` : s.subjectRef;
      this.names = new Map(this.names).set(key, s.label);
      this.subject = null;
      this.query = '';
    } catch (err) {
      this.fail(err, 'Could not share');
    } finally {
      this.adding = false;
    }
  }

  private async changePermission(g: ArtifactGrant, permission: GrantPermission): Promise<void> {
    const a = this.artifact;
    if (!a || permission === g.permission || this.busyId) return;
    this.busyId = g.id;
    this.error = null;
    try {
      const updated = await putGrant(a.id, g.subjectKind, g.subjectRef, permission);
      this.grants = this.grants.map((x) => (x.id === g.id ? updated : x));
    } catch (err) {
      this.fail(err, 'Could not change the permission');
      // live() puts the select back to the stored permission.
      this.requestUpdate();
    } finally {
      this.busyId = '';
    }
  }

  private async removeGrant(g: ArtifactGrant): Promise<void> {
    const a = this.artifact;
    if (!a || this.busyId) return;
    this.busyId = g.id;
    this.error = null;
    try {
      await deleteGrant(a.id, g.id);
      this.grants = this.grants.filter((x) => x.id !== g.id);
    } catch (err) {
      this.fail(err, 'Could not remove access');
    } finally {
      this.busyId = '';
    }
  }

  // ── Retention ───────────────────────────────────────────────

  private get initialRetention(): RetentionChoice {
    return this.artifact?.expiresAt ? 'current' : 'never';
  }

  private get retentionChanged(): boolean {
    return this.retention !== this.initialRetention;
  }

  private async saveRetention(): Promise<void> {
    const a = this.artifact;
    if (!a || this.saving) return;
    const expiry = retentionExpiry(this.retention, a.expiresAt, new Date());
    this.saving = true;
    this.error = null;
    try {
      const res = await patchArtifact(a.id, { expiresAt: expiry ? expiry.toISOString() : null });
      this.artifact = res.artifact;
      this.retention = this.initialRetention;
      this.dispatchEvent(
        new CustomEvent<Artifact>('artifact-changed', {
          detail: res.artifact,
          bubbles: true,
          composed: true,
        })
      );
    } catch (err) {
      this.fail(err, 'Could not change the expiry');
    } finally {
      this.saving = false;
    }
  }

  // ── Rendering ───────────────────────────────────────────────

  private label(kind: string, ref: string): string {
    return principalLabel(kind, ref, this.names.get(`${kind}:${ref}`) ?? '', this.currentUserId);
  }

  private renderLinks(a: Artifact): TemplateResult {
    return html`
      <section aria-labelledby="links-heading">
        <h3 id="links-heading"><sl-icon name="link-45deg"></sl-icon>Share links</h3>
        <p class="hint">
          Anyone with a link can view the current version without signing in. Links always expire.
          ${a.expiresAt
            ? html`<br /><sl-icon name="clock"></sl-icon> This artifact expires
                ${formatInstant(a.expiresAt)}.`
            : nothing}
        </p>
        <div class="row">
          <label for="ttl">Expires in</label>
          <sl-select
            id="ttl"
            size="small"
            .value=${live(String(this.ttlHours))}
            @sl-change=${(e: Event): void => {
              this.ttlHours = Number((e.target as HTMLInputElement).value);
            }}
          >
            ${LINK_LIFETIMES.map(
              (l) => html`<sl-option value=${String(l.hours)}>${l.label}</sl-option>`
            )}
          </sl-select>
          <sl-button
            size="small"
            variant="primary"
            ?loading=${this.creating}
            @click=${(): void => void this.create()}
          >
            <sl-icon slot="prefix" name="link-45deg"></sl-icon>
            Create link
          </sl-button>
        </div>
        ${this.renderCreated()}
        ${this.links.length === 0
          ? html`<p class="empty">No active links.</p>`
          : html`
              <table>
                <thead>
                  <tr>
                    <th>Created by</th>
                    <th>Created</th>
                    <th>Expires</th>
                    <th><span class="sr-only">Actions</span></th>
                  </tr>
                </thead>
                <tbody>
                  ${this.links.map((l) => this.renderLink(l, a))}
                </tbody>
              </table>
            `}
      </section>
    `;
  }

  private renderCreated(): TemplateResult | typeof nothing {
    const c = this.created;
    if (!c) return nothing;
    const url = shareLinkUrl(c.url, window.location.origin);
    // Copy follows the approved Share mock: the success alert says the link
    // is shown once and until when it works; when the artifact's expiry cut
    // the link short, the alert keeps only the shown-once line and a
    // separate notice gives the end (mock states 1b and 1d).
    const until = formatInstant(c.link.expiresAt);
    return html`
      <sl-alert class="created" variant="success" open>
        <sl-icon slot="icon" name="check-circle-fill"></sl-icon>
        <strong>Link created.</strong>
        <div class="url">
          <sl-input
            size="small"
            readonly
            aria-label="Share link"
            .value=${url}
            @focus=${(e: Event): void => (e.target as HTMLInputElement).select?.()}
          ></sl-input>
          <sl-button size="small" @click=${(): void => void this.copyCreated()}>
            <sl-icon slot="prefix" name="clipboard"></sl-icon>
            ${this.copied ? 'Copied' : 'Copy'}
          </sl-button>
        </div>
        <div class="once">
          <sl-icon name="exclamation-triangle"></sl-icon>
          ${c.clampedToArtifactExpiry
            ? 'Copy it now: the link is shown only once.'
            : `Copy it now: the link is shown only once. Anyone with it can view this artifact until ${until} or until you revoke it.`}
        </div>
      </sl-alert>
      ${c.clampedToArtifactExpiry
        ? html`<sl-alert class="clamped" variant="primary" open>
            <sl-icon slot="icon" name="clock"></sl-icon>
            The link ends <strong>${until}</strong>, when the artifact expires.
          </sl-alert>`
        : nothing}
    `;
  }

  private renderLink(l: ShareLink, a: Artifact): TemplateResult {
    const [kind, ref] = splitPrincipal(l.createdBy ?? '');
    const end = new Date(l.expiresAt).getTime();
    const soon = end - Date.now() < SOON_MS;
    const atArtifactExpiry = !!a.expiresAt && new Date(a.expiresAt).getTime() === end;
    return html`
      <tr>
        <td>${kind ? this.label(kind, ref) : html`<span class="muted">—</span>`}</td>
        <td>${formatInstant(l.createdAt)}</td>
        <td>
          ${formatInstant(l.expiresAt)}
          ${atArtifactExpiry
            ? html`<span class="muted">(artifact expiry)</span>`
            : soon
              ? html`<span class="muted">(${formatRelative(l.expiresAt)})</span>`
              : nothing}
        </td>
        <td class="action">
          <sl-button
            size="small"
            variant="danger"
            outline
            ?loading=${this.busyId === l.id}
            ?disabled=${!!this.busyId && this.busyId !== l.id}
            @click=${(): void => void this.revoke(l)}
          >
            <sl-icon slot="prefix" name="trash"></sl-icon>
            Revoke
          </sl-button>
        </td>
      </tr>
    `;
  }

  private renderPeople(): TemplateResult {
    return html`
      <section aria-labelledby="people-heading">
        <h3 id="people-heading"><sl-icon name="people"></sl-icon>People and projects</h3>
        <div class="row">
          <div class="picker grow">
            <sl-input
              size="small"
              placeholder=${this.crossProjectSharing ? 'User or project' : 'User'}
              aria-label=${this.crossProjectSharing ? 'User or project' : 'User'}
              autocomplete="off"
              .value=${this.query}
              @sl-input=${(e: Event): void => this.onQuery(e)}
            ></sl-input>
            ${this.suggestions.length > 0
              ? html`<ul class="suggestions" role="listbox">
                  ${this.suggestions.map(
                    (s) =>
                      html`<li role="option">
                        <button type="button" @click=${(): void => this.pick(s)}>
                          <sl-icon
                            name=${s.subjectKind === 'scope' ? 'folder' : 'person'}
                          ></sl-icon>
                          <span>${s.label}</span>
                          <span class="muted">${s.detail}</span>
                        </button>
                      </li>`
                  )}
                </ul>`
              : nothing}
          </div>
          ${this.renderPermissionSelect(this.permission, false, (p) => {
            this.permission = p;
          })}
          <sl-button
            size="small"
            ?disabled=${!this.subject}
            ?loading=${this.adding}
            @click=${(): void => void this.add()}
            >Add</sl-button
          >
        </div>
        ${this.crossProjectSharing
          ? nothing
          : html`<p class="hint">Sharing with other projects is turned off on this hub.</p>`}
        ${this.grants.map((g) => this.renderGrant(g))}
      </section>
    `;
  }

  private renderPermissionSelect(
    value: GrantPermission,
    disabled: boolean,
    onChange: (p: GrantPermission) => void
  ): TemplateResult {
    return html`<sl-select
      size="small"
      aria-label="Permission"
      .value=${live(value)}
      ?disabled=${disabled}
      @sl-change=${(e: Event): void =>
        onChange((e.target as HTMLInputElement).value as GrantPermission)}
    >
      ${(Object.keys(PERMISSION_LABELS) as GrantPermission[]).map(
        (p) => html`<sl-option value=${p}>${PERMISSION_LABELS[p]}</sl-option>`
      )}
    </sl-select>`;
  }

  private renderGrant(g: ArtifactGrant): TemplateResult {
    const isScope = g.subjectKind === 'scope';
    let who: string;
    if (isScope) {
      who = this.names.get(`project:${g.subjectRef}`) || g.subjectRef;
    } else {
      const [kind, ref] = splitPrincipal(g.subjectRef);
      who = kind ? this.label(kind, ref) : g.subjectRef;
    }
    if (g.home) {
      return html`<div class="grant">
        <sl-icon name="folder"></sl-icon>
        <span class="who"><strong>${who}</strong> <span class="muted">(home project)</span></span>
        <span class="muted">${PERMISSION_LABELS[g.permission]}</span>
      </div>`;
    }
    return html`<div class="grant">
      <sl-icon name=${isScope ? 'folder' : 'person'}></sl-icon>
      <span class="who">
        <strong>${who}</strong> ${isScope ? html`<span class="muted">(project)</span>` : nothing}
        ${isScope && this.crossProjectSharing
          ? html`<small>Cross-project sharing is on for this hub</small>`
          : nothing}
      </span>
      ${this.renderPermissionSelect(g.permission, !!this.busyId, (p) => {
        void this.changePermission(g, p);
      })}
      <sl-button
        size="small"
        variant="text"
        ?loading=${this.busyId === g.id}
        ?disabled=${!!this.busyId && this.busyId !== g.id}
        @click=${(): void => void this.removeGrant(g)}
        >Remove</sl-button
      >
    </div>`;
  }

  private renderRetention(a: Artifact): TemplateResult {
    const now = new Date();
    const expiry = retentionExpiry(this.retention, a.expiresAt, now);
    const impact = this.retentionChanged
      ? expiryImpact(expiry, this.links, this.grants, now)
      : { linksCutShort: 0, grantsRemoved: 0 };
    const warning = expiryWarning(impact.linksCutShort, impact.grantsRemoved);
    return html`
      <section aria-labelledby="retention-heading">
        <h3 id="retention-heading"><sl-icon name="clock"></sl-icon>Retention</h3>
        <p class="hint">When the artifact expires it is deleted with its links and grants.</p>
        <div class="row">
          <label for="retention">Expires</label>
          <sl-select
            id="retention"
            size="small"
            .value=${live(this.retention)}
            @sl-change=${(e: Event): void => {
              this.retention = (e.target as HTMLInputElement).value as RetentionChoice;
            }}
          >
            ${a.expiresAt
              ? html`<sl-option value="current">${formatInstant(a.expiresAt)}</sl-option>`
              : nothing}
            <sl-option value="never">Never (keep until deleted)</sl-option>
            ${RETENTION_DAYS.map(
              (d) =>
                html`<sl-option value=${`${d}d`}>In ${d} ${d === 1 ? 'day' : 'days'}</sl-option>`
            )}
          </sl-select>
          ${this.retentionChanged
            ? html`<span class="muted"
                >was: ${a.expiresAt ? formatInstant(a.expiresAt) : 'Never'}</span
              >`
            : nothing}
        </div>
        ${warning && expiry
          ? html`<sl-alert variant="warning" open>
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              <strong>${warning}</strong> when the artifact expires on
              ${formatInstant(expiry.toISOString())}.
            </sl-alert>`
          : nothing}
      </section>
    `;
  }

  private renderBody(a: Artifact): TemplateResult {
    if (this.loading) {
      return html`<div class="row"><sl-spinner></sl-spinner> Loading…</div>`;
    }
    if (this.loadError) {
      return html`<sl-alert variant="danger" open>
        <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
        ${this.loadError}
        <sl-button size="small" @click=${(): void => void this.load()}>Retry</sl-button>
      </sl-alert>`;
    }
    return html`
      ${this.error
        ? html`<sl-alert variant="danger" open role="alert">
            <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
            ${this.error}
          </sl-alert>`
        : nothing}
      ${this.renderLinks(a)} ${this.renderPeople()} ${this.renderRetention(a)}
    `;
  }

  override render(): TemplateResult {
    const a = this.artifact;
    return html`
      <sl-dialog
        label=${a ? `Share “${a.title}”` : 'Share'}
        ?open=${this.open}
        @sl-after-hide=${(e: Event): void => {
          if (e.target === e.currentTarget && this.open) this.close();
        }}
      >
        ${a ? this.renderBody(a) : nothing}
        <div slot="footer" class="footer">
          ${this.retentionChanged
            ? html`
                <sl-button
                  size="small"
                  ?disabled=${this.saving}
                  @click=${(): void => {
                    this.retention = this.initialRetention;
                  }}
                  >Cancel</sl-button
                >
                <sl-button
                  size="small"
                  variant="primary"
                  ?loading=${this.saving}
                  @click=${(): void => void this.saveRetention()}
                  >Save</sl-button
                >
              `
            : html`<sl-button size="small" variant="primary" @click=${(): void => this.close()}
                >Done</sl-button
              >`}
        </div>
      </sl-dialog>
    `;
  }
}

/** Splits "user:<id>" or "agent:<id>" into its kind and id; ['', ''] otherwise. */
export function splitPrincipal(ref: string): ['user' | 'agent' | '', string] {
  const i = ref.indexOf(':');
  if (i <= 0) return ['', ''];
  const kind = ref.slice(0, i);
  if (kind !== 'user' && kind !== 'agent') return ['', ''];
  return [kind, ref.slice(i + 1)];
}

async function searchUsers(q: string): Promise<ShareSubject[]> {
  try {
    const res = await apiFetch(`/api/v1/users?search=${encodeURIComponent(q)}&limit=5`, {
      suppressAccessDeniedToast: true,
    });
    if (!res.ok) return [];
    const body = (await res.json()) as {
      users?: Array<{ id: string; email: string; displayName?: string }>;
    };
    return (body.users ?? []).map((u) => ({
      subjectKind: 'principal' as const,
      subjectRef: `user:${u.id}`,
      label: u.displayName || u.email,
      detail: u.displayName ? u.email : '',
    }));
  } catch {
    return [];
  }
}

async function searchProjects(q: string): Promise<ShareSubject[]> {
  try {
    const res = await apiFetch(`/api/v1/projects?search=${encodeURIComponent(q)}&limit=5`, {
      suppressAccessDeniedToast: true,
    });
    if (!res.ok) return [];
    const body = (await res.json()) as {
      projects?: Array<{ id: string; name?: string; slug?: string }>;
    };
    return (body.projects ?? []).map((p) => ({
      subjectKind: 'scope' as const,
      subjectRef: p.id,
      label: p.name || p.slug || p.id,
      detail: 'project',
    }));
  } catch {
    return [];
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-artifact-share-dialog': ScionArtifactShareDialog;
  }
}
