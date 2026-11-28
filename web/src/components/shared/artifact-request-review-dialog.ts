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
 * Request review dialog (experiment hub.artifacts_review_requests,
 * ptone/scion#9306): lets a publisher pick one or more users (reusing the
 * Share dialog's user search), add an optional note (up to 1000 runes),
 * submit POST /api/v1/artifacts/{id}/review-requests, and inspect the
 * per-reviewer outcome (created / reminded / unchanged / refused with code).
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import {
  BOUNDED_REVIEW_CREATOR,
  countRunes,
  createReviewRequests,
  listGrants,
  MAX_REVIEW_REQUEST_NOTE_RUNES,
  MAX_REVIEW_REQUEST_REVIEWERS,
  reviewRequestRefusalText,
} from '../../client/artifacts.js';
import type {
  Artifact,
  ArtifactGrant,
  CreateReviewRequestsResponse,
  ReviewRequestOutcome,
  ReviewRequestResult,
} from '../../client/artifacts.js';
import { searchUsers } from './artifact-share-dialog.js';
import type { ShareSubject } from './artifact-share-dialog.js';

/** Delay before the user search runs. */
const SEARCH_DELAY_MS = 250;

/** Fewest characters the user search runs for. */
const MIN_SEARCH_LENGTH = 2;

/** Per-user access hint shown for each picked reviewer (design §4.12, ptone/scion#9311). */
export type ReviewerAccessHint = 'existing' | 'raised' | 'bounded';

export const REVIEWER_ACCESS_HINT_LABELS: Record<ReviewerAccessHint, string> = {
  existing: 'can already review',
  raised: 'will be given Can review',
  bounded: 'can review while they have a project role',
};

/**
 * Computes the picker access hint for a selected user reviewer from the
 * artifact metadata and (when the caller may administer the artifact) its
 * loaded grant list. Returns null when access cannot be determined.
 */
export function reviewerAccessHint(
  subjectRef: string,
  artifact: Artifact | null,
  grants: readonly ArtifactGrant[],
  grantsLoaded: boolean,
  canAdminister: boolean
): ReviewerAccessHint | null {
  if (!artifact) return null;
  if (artifact.ownerKind === 'user' && `user:${artifact.ownerRef}` === subjectRef) {
    return 'existing';
  }
  if (!grantsLoaded) return null;
  const grant = grants.find((g) => g.subjectKind === 'principal' && g.subjectRef === subjectRef);
  if (grant) {
    if (grant.permission === 'review' && grant.createdBy === BOUNDED_REVIEW_CREATOR) {
      return 'bounded';
    }
    if (
      grant.permission === 'review' ||
      grant.permission === 'write' ||
      grant.permission === 'admin'
    ) {
      return 'existing';
    }
  }
  if (canAdminister) {
    return 'raised';
  }
  return null;
}

const OUTCOME_BADGES: Record<
  ReviewRequestOutcome,
  { label: string; variant: 'success' | 'primary' | 'neutral' | 'danger' }
> = {
  created: { label: 'Requested', variant: 'success' },
  reminded: { label: 'Reminded', variant: 'primary' },
  unchanged: { label: 'Already requested', variant: 'neutral' },
  refused: { label: 'Refused', variant: 'danger' },
};

@customElement('scion-artifact-request-review-dialog')
export class ScionArtifactRequestReviewDialog extends LitElement {
  @property({ type: Object }) artifact: Artifact | null = null;
  @property({ type: Boolean, reflect: true }) open = false;
  /** The signed-in user id, excluded from picker suggestions. */
  @property({ type: String }) currentUserId = '';
  /** Whether the caller may administer grants (canManage or canShare). */
  @property({ type: Boolean }) canAdminister = false;

  @state() private query = '';
  @state() private suggestions: ShareSubject[] = [];
  @state() private selected: ShareSubject[] = [];
  @state() private grants: ArtifactGrant[] = [];
  @state() private grantsLoaded = false;
  @state() private note = '';
  @state() private submitting = false;
  @state() private error: string | null = null;
  @state() private results: ReviewRequestResult[] | null = null;

  private searchTimer: ReturnType<typeof setTimeout> | null = null;
  private searchGen = 0;
  private grantsGen = 0;

  static override styles = css`
    sl-dialog::part(panel) {
      width: min(560px, 95vw);
    }
    .hint {
      margin: 0 0 0.75rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted);
    }
    .field {
      margin-bottom: 0.85rem;
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
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: calc(var(--scion-radius) * 0.75);
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
      color: var(--scion-text);
      text-align: left;
      cursor: pointer;
    }
    .suggestions button:hover,
    .suggestions button:focus-visible {
      background: var(--scion-bg-subtle);
    }
    .muted {
      color: var(--scion-text-muted);
    }
    .selected-list {
      list-style: none;
      margin: 0.5rem 0 0;
      padding: 0;
      border: 1px solid var(--scion-border);
      border-radius: calc(var(--scion-radius) * 0.75);
      overflow: hidden;
    }
    .selected-row {
      display: flex;
      align-items: center;
      gap: 0.6rem;
      padding: 0.45rem 0.65rem;
      border-bottom: 1px solid var(--scion-border);
      font-size: 0.8125rem;
    }
    .selected-row:last-child {
      border-bottom: none;
    }
    .selected-row > sl-icon {
      color: var(--sl-color-neutral-500);
      flex-shrink: 0;
    }
    .selected-who {
      flex: 1;
      min-width: 0;
    }
    .selected-name {
      display: flex;
      align-items: baseline;
      gap: 0.4rem;
      flex-wrap: wrap;
    }
    .selected-hint {
      display: block;
      margin-top: 0.1rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted);
    }
    .note-meta {
      display: flex;
      justify-content: space-between;
      align-items: center;
      margin-top: 0.25rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted);
    }
    .note-meta .over-limit {
      color: var(--scion-status-danger);
      font-weight: 500;
    }
    .results {
      list-style: none;
      margin: 0.5rem 0 0;
      padding: 0;
      border: 1px solid var(--scion-border);
      border-radius: calc(var(--scion-radius) * 0.75);
      overflow: hidden;
    }
    .result-row {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      gap: 0.75rem;
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border);
      font-size: 0.8125rem;
    }
    .result-row:last-child {
      border-bottom: none;
    }
    .result-who {
      min-width: 0;
      flex: 1;
    }
    .result-detail {
      display: block;
      margin-top: 0.15rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted);
    }
    .result-code {
      font-family: var(--scion-font-mono);
      font-size: 0.75rem;
    }
    sl-alert {
      margin-bottom: 0.75rem;
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
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('open') && this.open) {
      this.reset();
      if (this.canAdminister && this.artifact) {
        void this.loadGrants(this.artifact.id);
      }
    }
  }

  private reset(): void {
    this.query = '';
    this.suggestions = [];
    this.selected = [];
    this.grants = [];
    this.grantsLoaded = false;
    this.note = '';
    this.submitting = false;
    this.error = null;
    this.results = null;
    this.searchGen++;
    this.grantsGen++;
    if (this.searchTimer) {
      clearTimeout(this.searchTimer);
      this.searchTimer = null;
    }
  }

  private async loadGrants(artifactId: string): Promise<void> {
    const gen = ++this.grantsGen;
    try {
      const res = await listGrants(artifactId);
      if (gen !== this.grantsGen || !this.open) return;
      this.grants = res.grants;
      this.grantsLoaded = true;
    } catch {
      if (gen !== this.grantsGen) return;
      this.grants = [];
      this.grantsLoaded = false;
    }
  }

  private close(): void {
    this.dispatchEvent(
      new CustomEvent('artifact-request-review-closed', { bubbles: true, composed: true })
    );
  }

  private onQuery(e: Event): void {
    this.query = (e.target as HTMLInputElement).value;
    if (this.searchTimer) clearTimeout(this.searchTimer);
    const q = this.query.trim();
    if (q.length < MIN_SEARCH_LENGTH) {
      this.searchGen++;
      this.suggestions = [];
      return;
    }
    this.searchTimer = setTimeout(() => void this.runSearch(q), SEARCH_DELAY_MS);
  }

  private async runSearch(q: string): Promise<void> {
    const gen = ++this.searchGen;
    const users = await searchUsers(q);
    if (gen !== this.searchGen) return;
    const picked = new Set(this.selected.map((s) => s.subjectRef));
    const selfRef = this.currentUserId ? `user:${this.currentUserId}` : '';
    this.suggestions = users.filter(
      (u) => !picked.has(u.subjectRef) && (!selfRef || u.subjectRef !== selfRef)
    );
  }

  private pick(s: ShareSubject): void {
    if (this.selected.length >= MAX_REVIEW_REQUEST_REVIEWERS) return;
    if (this.selected.some((x) => x.subjectRef === s.subjectRef)) return;
    this.selected = [...this.selected, s];
    this.query = '';
    this.suggestions = [];
  }

  private removeSelected(ref: string): void {
    this.selected = this.selected.filter((x) => x.subjectRef !== ref);
  }

  private get noteRunes(): number {
    return countRunes(this.note);
  }

  private get noteTooLong(): boolean {
    return this.noteRunes > MAX_REVIEW_REQUEST_NOTE_RUNES;
  }

  private async submit(): Promise<void> {
    const a = this.artifact;
    if (!a || this.submitting || this.selected.length === 0 || this.noteTooLong) return;
    this.submitting = true;
    this.error = null;
    try {
      const trimmedNote = this.note.trim();
      const res = await createReviewRequests(a.id, {
        reviewers: this.selected.map((s) => s.subjectRef),
        ...(trimmedNote ? { note: trimmedNote } : {}),
      });
      this.results = res.requests;
      this.dispatchEvent(
        new CustomEvent<CreateReviewRequestsResponse>('artifact-review-requested', {
          detail: res,
          bubbles: true,
          composed: true,
        })
      );
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Could not send review requests';
    } finally {
      this.submitting = false;
    }
  }

  private reviewerDisplayName(r: ReviewRequestResult): string {
    const fromResponse = r.request?.reviewer.displayName;
    if (fromResponse) return fromResponse;
    const picked = this.selected.find((s) => s.subjectRef === r.reviewer);
    if (picked) return picked.label;
    return r.reviewer;
  }

  private renderResultRow(r: ReviewRequestResult): TemplateResult {
    const badge = OUTCOME_BADGES[r.outcome] ?? { label: r.outcome, variant: 'neutral' as const };
    const name = this.reviewerDisplayName(r);
    let detail: TemplateResult | typeof nothing = nothing;
    if (r.outcome === 'refused') {
      detail = html`<span class="result-detail">
        ${reviewRequestRefusalText(r.code)}
        ${r.code ? html`(<code class="result-code">${r.code}</code>)` : nothing}
      </span>`;
    } else if (r.outcome === 'unchanged') {
      detail = html`<span class="result-detail"
        >Open request already exists — no new reminder sent (24-hour cooldown)</span
      >`;
    } else {
      const parts: string[] = [];
      if (r.outcome === 'reminded') {
        if (r.notifySkipped) {
          parts.push(
            `Reminder recorded · notification skipped (${r.notifySkipped.replace(/_/g, ' ')})`
          );
        } else if (r.notified) {
          parts.push('Reminder sent');
        } else {
          parts.push('Reminder recorded');
        }
      } else if (r.notifySkipped) {
        parts.push(`Notification skipped (${r.notifySkipped.replace(/_/g, ' ')})`);
      } else if (r.notified) {
        parts.push('Notified');
      }
      if (r.access === 'raised') {
        parts.push('Gave Can review');
      } else if (r.access === 'bounded') {
        parts.push('Can review while they have a project role');
      }
      if (parts.length > 0) {
        detail = html`<span class="result-detail">${parts.join(' · ')}</span>`;
      }
    }
    return html`
      <li class="result-row" data-reviewer=${r.reviewer} data-outcome=${r.outcome}>
        <div class="result-who">
          <strong>${name}</strong>
          ${detail}
        </div>
        <sl-badge pill variant=${badge.variant}>${badge.label}</sl-badge>
      </li>
    `;
  }

  private renderForm(): TemplateResult {
    const atLimit = this.selected.length >= MAX_REVIEW_REQUEST_REVIEWERS;
    return html`
      <p class="hint">Ask people to review the current version.</p>
      ${this.error
        ? html`<sl-alert variant="danger" open role="alert">
            <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
            ${this.error}
          </sl-alert>`
        : nothing}
      <div class="field">
        <div class="picker">
          <sl-input
            class="reviewer-search"
            size="small"
            label="Reviewers"
            placeholder=${atLimit
              ? `Maximum of ${MAX_REVIEW_REQUEST_REVIEWERS} reviewers selected`
              : 'Search people by name or email…'}
            .value=${this.query}
            ?disabled=${this.submitting || atLimit}
            @sl-input=${(e: Event): void => this.onQuery(e)}
          ></sl-input>
          ${this.suggestions.length > 0
            ? html`<ul class="suggestions" role="listbox" aria-label="User suggestions">
                ${this.suggestions.map(
                  (s) =>
                    html`<li>
                      <button type="button" @click=${(): void => this.pick(s)}>
                        <sl-icon name="person"></sl-icon>
                        <span>${s.label}</span>
                        ${s.detail ? html`<span class="muted">${s.detail}</span>` : nothing}
                      </button>
                    </li>`
                )}
              </ul>`
            : nothing}
        </div>
        ${this.selected.length > 0
          ? html`<ul class="selected-list" aria-label="Selected reviewers">
              ${this.selected.map((s) => {
                const hint = reviewerAccessHint(
                  s.subjectRef,
                  this.artifact,
                  this.grants,
                  this.grantsLoaded,
                  this.canAdminister
                );
                return html`<li class="selected-row" data-reviewer=${s.subjectRef}>
                  <sl-icon name="person"></sl-icon>
                  <div class="selected-who">
                    <span class="selected-name">
                      <strong>${s.label}</strong>
                      ${s.detail ? html`<span class="muted">${s.detail}</span>` : nothing}
                    </span>
                    ${hint
                      ? html`<span class="selected-hint" data-access-hint=${hint}
                          >${REVIEWER_ACCESS_HINT_LABELS[hint]}</span
                        >`
                      : nothing}
                  </div>
                  <sl-icon-button
                    name="x-lg"
                    label=${`Remove ${s.label}`}
                    @click=${(): void => this.removeSelected(s.subjectRef)}
                  ></sl-icon-button>
                </li>`;
              })}
            </ul>`
          : nothing}
      </div>
      <div class="field">
        <sl-textarea
          class="request-note"
          size="small"
          rows="3"
          label="Note (optional)"
          placeholder="What should reviewers focus on?"
          .value=${this.note}
          ?disabled=${this.submitting}
          @sl-input=${(e: Event): void => {
            this.note = (e.target as HTMLTextAreaElement).value;
          }}
        ></sl-textarea>
        <div class="note-meta">
          <span>Reviewers will see your note.</span>
          <span class=${this.noteTooLong ? 'over-limit' : ''}>
            ${this.noteRunes} / ${MAX_REVIEW_REQUEST_NOTE_RUNES}
          </span>
        </div>
      </div>
    `;
  }

  private renderResults(results: ReviewRequestResult[]): TemplateResult {
    return html`
      <p class="hint">Review request results:</p>
      <ul class="results" aria-label="Review request results">
        ${results.map((r) => this.renderResultRow(r))}
      </ul>
    `;
  }

  override render(): TemplateResult {
    const a = this.artifact;
    const label = a ? `Request review: “${a.title}”` : 'Request review';
    return html`
      <sl-dialog
        label=${label}
        ?open=${this.open}
        @sl-after-hide=${(e: Event): void => {
          if (e.target === e.currentTarget && this.open) this.close();
        }}
      >
        ${this.results ? this.renderResults(this.results) : this.renderForm()}
        <div slot="footer" class="footer">
          ${this.results
            ? html`<sl-button size="small" variant="primary" @click=${(): void => this.close()}>
                Done
              </sl-button>`
            : html`
                <sl-button
                  size="small"
                  ?disabled=${this.submitting}
                  @click=${(): void => this.close()}
                >
                  Cancel
                </sl-button>
                <sl-button
                  size="small"
                  variant="primary"
                  class="submit-request-btn"
                  ?loading=${this.submitting}
                  ?disabled=${this.selected.length === 0 || this.noteTooLong}
                  @click=${(): void => void this.submit()}
                >
                  Request review
                </sl-button>
              `}
        </div>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-artifact-request-review-dialog': ScionArtifactRequestReviewDialog;
  }
}
