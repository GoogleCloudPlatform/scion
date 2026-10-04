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
 * Access Boundary Schedule Editor (Step 5)
 *
 * Optional activation window with notBefore and expiresAt date/time inputs.
 * Shows viewer time zone label + UTC preview.
 * Validates: notBefore < expiresAt (client-side warning; server validates too).
 * Includes a clear/remove schedule option.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { PropertyValues } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property, state } from 'lit/decorators.js';

import type { Iso8601 } from '../../shared/access-boundaries.js';
import { effectiveTimeZone, parseWallClock, toWallClockInput } from '../../utils/time.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';

export interface ScheduleChangeDetail {
  notBefore: Iso8601 | undefined;
  expiresAt: Iso8601 | undefined;
}

@customElement('scion-access-boundary-schedule-editor')
export class ScionAccessBoundaryScheduleEditor extends LitElement {
  /**
   * Re-renders this editor when the effective display zone changes (review
   * R4-1). By itself this only fixes the "Times in: <zone>" label — see
   * `willUpdate` below for why the cached `notBeforeLocal`/`expiresAtLocal`
   * strings also need to be re-derived, not just re-rendered.
   */
  readonly _zone = new DisplayZoneController(this);

  /** ISO 8601 UTC string for activation start. */
  @property() notBefore: Iso8601 | undefined = undefined;

  /** ISO 8601 UTC string for expiration. */
  @property() expiresAt: Iso8601 | undefined = undefined;

  @state() private hasSchedule = false;
  @state() private notBeforeLocal = '';
  @state() private expiresAtLocal = '';
  @state() private validationError = '';

  /**
   * The zone `notBeforeLocal`/`expiresAtLocal` were last derived for.
   * `willUpdate` compares this against the current effective zone on every
   * update to detect a change (review R4-1). Refreshed in
   * `connectedCallback` too (review R5-1) — see that method.
   */
  private _renderedZone = effectiveTimeZone();

  /**
   * The value of each field this editor last emitted via `schedule-change`
   * and has not yet seen come back through its prop (absent key = nothing
   * pending). A host that feeds `schedule-change` back into `notBefore`/
   * `expiresAt` (admin-access-boundary-editor.ts) echoes the editor's own
   * value; re-deriving from that echo would clobber what the user is typing
   * (e.g. a partial value emits `undefined`, whose echo would clear the
   * field). Consumed on the next change to that prop, echo or not, so a
   * later genuine change back to the same value still re-derives.
   */
  private _pendingEcho: Partial<Record<'notBefore' | 'expiresAt', Iso8601 | undefined>> = {};

  private get viewerTimeZone(): string {
    return effectiveTimeZone();
  }

  override connectedCallback(): void {
    super.connectedCallback();
    // Rebase whatever is already cached (review R6-1) *before* overwriting
    // from props below — see `rebaseCachedStrings`'s doc comment for why a
    // bare `_renderedZone = this.viewerTimeZone` assignment here (R5-1's
    // original fix) was wrong: it marked a retained, not-about-to-be-
    // overwritten string (one with no backing prop) as already being in
    // the current zone without actually converting it.
    this.rebaseCachedStrings();
    // Initialize local fields from props. Only fields with a backing prop
    // are derived here, so a retained typed value with no backing prop
    // survives a detach/reconnect (review R6-1).
    if (this.notBefore || this.expiresAt) {
      this.deriveFromProps({
        notBefore: Boolean(this.notBefore),
        expiresAt: Boolean(this.expiresAt),
      });
    }
  }

  /**
   * Re-derives `hasSchedule` and the cached wall-clock string of each field
   * in `fields` from its ISO prop, in the current zone. Shared by
   * `connectedCallback` and `willUpdate` (ptone/scion#2581) so the two
   * derivations cannot drift. A field whose prop is unset is cleared.
   * Callers must run `rebaseCachedStrings` first (utils/time.ts ordering
   * contract): kept strings are rebased, then the rest re-derived here.
   */
  private deriveFromProps(fields: { notBefore: boolean; expiresAt: boolean }): void {
    if (fields.notBefore) {
      this.notBeforeLocal = this.notBefore ? this.isoToLocalDatetime(this.notBefore) : '';
    }
    if (fields.expiresAt) {
      this.expiresAtLocal = this.expiresAt ? this.isoToLocalDatetime(this.expiresAt) : '';
    }
    if (this.notBefore || this.expiresAt) {
      this.hasSchedule = true;
    } else if (!this.notBeforeLocal && !this.expiresAtLocal) {
      this.hasSchedule = false;
    }
  }

  /**
   * Re-derives the cached `datetime-local` strings when the effective zone
   * changes between updates (review R4-1). See `rebaseCachedStrings` for
   * what this actually does; `willUpdate` is just one of its two call
   * sites (`connectedCallback` is the other, review R6-1).
   *
   * It also re-derives a field (and `hasSchedule`) through `deriveFromProps`
   * when the host changes its `notBefore`/`expiresAt` prop while connected
   * (ptone/scion#2581), after the zone rebase — except when the new value
   * is the host's echo of what this editor itself last emitted (see
   * `_pendingEcho`), so a value the user is typing is not clobbered.
   */
  override willUpdate(changed: PropertyValues<this>): void {
    super.willUpdate(changed);
    // Ordering contract (utils/time.ts): rebase kept strings to the current
    // zone first, then re-derive the changed fields from their props.
    this.rebaseCachedStrings();

    // A new notBefore/expiresAt from the host (e.g. an async load of an
    // existing boundary) must update the inputs, not just the first value
    // seen at connect (ptone/scion#2581) — unless it is the echo of what
    // this editor itself just emitted.
    const notBefore = changed.has('notBefore') && !this.consumeEcho('notBefore');
    const expiresAt = changed.has('expiresAt') && !this.consumeEcho('expiresAt');
    if (notBefore || expiresAt) {
      this.deriveFromProps({ notBefore, expiresAt });
      this.validate();
    }
  }

  /**
   * True if the just-changed `field` prop equals the value this editor last
   * emitted for it. Clears the pending marker for `field` either way.
   */
  private consumeEcho(field: 'notBefore' | 'expiresAt'): boolean {
    if (!(field in this._pendingEcho)) return false;
    const emitted = this._pendingEcho[field];
    delete this._pendingEcho[field];
    return this[field] === emitted;
  }

  /**
   * Keeps `notBeforeLocal`/`expiresAtLocal` — wall-clock strings cached in
   * state, populated from the `notBefore`/`expiresAt` ISO props at connect
   * and then overwritten directly by the user typing into the fields —
   * valid against whatever the effective zone is *right now*, by rebasing
   * each one still in `_renderedZone` the moment that tracker is about to
   * move on. Called from both `willUpdate` (a zone change while mounted)
   * and `connectedCallback` (one while detached, or before the first
   * connection — review R5-1).
   *
   * **Why "rebase what you keep" must come before "overwrite from props",
   * not just "update the tracker along with whatever you overwrite"
   * (review R6-1).** `connectedCallback`'s R5-1 fix set `_renderedZone` to
   * the current zone unconditionally, then re-derived only the strings
   * whose prop was actually set. A string with **no** backing prop — a
   * value typed into an uncontrolled instance of this editor, still
   * present across a detach/reconnect — was left holding old-zone text
   * while the tracker now claimed it was already current-zone. From then
   * on `willUpdate` saw `zone === _renderedZone` and never rebased it, so
   * `emitChange` parsed stale wall-clock text in the wrong zone — a wrong
   * instant, not just a display glitch. Folding the rebase into one method
   * that both call sites invoke *before* anything else touches
   * `_renderedZone` means every cached string is always either rebased
   * (kept) or re-derived from an instant (overwritten) before the tracker
   * moves on — never silently relabelled.
   *
   * No-op if the zone hasn't changed since the last call, so calling it
   * unconditionally from both lifecycle points is cheap and idempotent.
   */
  private rebaseCachedStrings(): void {
    const zone = this.viewerTimeZone;
    if (zone === this._renderedZone) return;
    const previousZone = this._renderedZone;
    this._renderedZone = zone;

    if (this.notBeforeLocal) {
      const iso = parseWallClock(this.notBeforeLocal, previousZone);
      if (iso) this.notBeforeLocal = toWallClockInput(iso, zone);
    }
    if (this.expiresAtLocal) {
      const iso = parseWallClock(this.expiresAtLocal, previousZone);
      if (iso) this.expiresAtLocal = toWallClockInput(iso, zone);
    }
  }

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .schedule-editor {
        display: flex;
        flex-direction: column;
        gap: 1.5rem;
      }

      .schedule-toggle {
        display: flex;
        align-items: center;
        gap: 0.75rem;
      }

      .schedule-toggle-label {
        font-size: 0.875rem;
        font-weight: 500;
        color: var(--scion-text, #1e293b);
      }

      .schedule-toggle-description {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.25rem;
      }

      .datetime-fields {
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: 1.5rem;
      }

      @media (max-width: 640px) {
        .datetime-fields {
          grid-template-columns: 1fr;
        }
      }

      .datetime-field {
        display: flex;
        flex-direction: column;
        gap: 0.25rem;
      }

      .field-label {
        font-size: 0.8125rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
      }

      .field-help {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }

      .utc-preview {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        font-family: var(--sl-font-mono, monospace);
        margin-top: 0.25rem;
      }

      .timezone-label {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        display: flex;
        align-items: center;
        gap: 0.25rem;
      }

      .clear-section {
        display: flex;
        justify-content: flex-start;
      }

      .validation-warning {
        margin-top: 0;
      }

      .no-schedule-info {
        padding: 1rem;
        text-align: center;
        color: var(--scion-text-muted, #64748b);
        font-size: 0.875rem;
      }

      fieldset {
        border: none;
        margin: 0;
        padding: 0;
      }

      @media (forced-colors: active) {
        .validation-warning {
          border: 2px solid Mark;
        }

        .utc-preview {
          color: ButtonText;
        }
      }
    `,
  ];

  private isoToLocalDatetime(iso: Iso8601): string {
    return toWallClockInput(iso, this.viewerTimeZone);
  }

  private localDatetimeToIso(localValue: string): Iso8601 | undefined {
    if (!localValue) return undefined;
    const iso = parseWallClock(localValue, this.viewerTimeZone);
    return iso || undefined;
  }

  private formatUtcPreview(localValue: string): string {
    const iso = this.localDatetimeToIso(localValue);
    if (!iso) return '';
    return iso.replace('T', ' ').replace('.000Z', ' UTC');
  }

  private validate(): void {
    this.validationError = '';

    const notBeforeIso = this.localDatetimeToIso(this.notBeforeLocal);
    const expiresAtIso = this.localDatetimeToIso(this.expiresAtLocal);

    if (notBeforeIso && expiresAtIso) {
      const nb = new Date(notBeforeIso);
      const ea = new Date(expiresAtIso);
      if (nb >= ea) {
        this.validationError = 'Activation start must be before expiration.';
      }
    }
  }

  private handleNotBeforeChange(e: Event): void {
    this.notBeforeLocal = (e.target as HTMLInputElement).value;
    this.validate();
    this.emitChange();
  }

  private handleExpiresAtChange(e: Event): void {
    this.expiresAtLocal = (e.target as HTMLInputElement).value;
    this.validate();
    this.emitChange();
  }

  private handleToggleSchedule(): void {
    this.hasSchedule = !this.hasSchedule;
    if (!this.hasSchedule) {
      this.notBeforeLocal = '';
      this.expiresAtLocal = '';
      this.validationError = '';
    }
    this.emitChange();
  }

  private handleClearSchedule(): void {
    this.notBeforeLocal = '';
    this.expiresAtLocal = '';
    this.validationError = '';
    this.hasSchedule = false;
    this.emitChange();
  }

  private emitChange(): void {
    const detail: ScheduleChangeDetail = {
      notBefore: this.hasSchedule ? this.localDatetimeToIso(this.notBeforeLocal) : undefined,
      expiresAt: this.hasSchedule ? this.localDatetimeToIso(this.expiresAtLocal) : undefined,
    };
    // Only a value that differs from the current prop can come back as a
    // prop change; an unchanged one would never be consumed.
    this._pendingEcho = {};
    if (detail.notBefore !== this.notBefore) this._pendingEcho.notBefore = detail.notBefore;
    if (detail.expiresAt !== this.expiresAt) this._pendingEcho.expiresAt = detail.expiresAt;
    this.dispatchEvent(
      new CustomEvent<ScheduleChangeDetail>('schedule-change', {
        detail,
        bubbles: true,
        composed: true,
      })
    );
  }

  override render() {
    return html`
      <div class="schedule-editor">
        <div class="schedule-toggle">
          <sl-checkbox ?checked=${this.hasSchedule} @sl-change=${() => this.handleToggleSchedule()}>
            <span class="schedule-toggle-label">Set an activation window</span>
          </sl-checkbox>
        </div>

        <div class="schedule-toggle-description">
          An activation window limits when this access boundary is in effect. Without an activation
          window, the boundary is effective immediately and does not expire.
        </div>

        ${this.hasSchedule
          ? html`
              <div class="timezone-label">
                <sl-icon name="clock"></sl-icon>
                Times in: ${this.viewerTimeZone}
              </div>

              <fieldset>
                <legend class="sr-only">Activation window date and time</legend>
                <div class="datetime-fields">
                  <div class="datetime-field">
                    <label class="field-label" for="not-before">Activation start (optional)</label>
                    <sl-input
                      id="not-before"
                      type="datetime-local"
                      value=${this.notBeforeLocal}
                      aria-describedby=${this.validationError ? 'schedule-validation-msg' : ''}
                      @sl-input=${(e: Event) => this.handleNotBeforeChange(e)}
                      help-text="Constraint is not in effect before this time"
                    ></sl-input>
                    ${this.notBeforeLocal
                      ? html`<div class="utc-preview" aria-label="UTC equivalent">
                          UTC: ${this.formatUtcPreview(this.notBeforeLocal)}
                        </div>`
                      : nothing}
                  </div>

                  <div class="datetime-field">
                    <label class="field-label" for="expires-at">Expiration (optional)</label>
                    <sl-input
                      id="expires-at"
                      type="datetime-local"
                      value=${this.expiresAtLocal}
                      aria-describedby=${this.validationError ? 'schedule-validation-msg' : ''}
                      @sl-input=${(e: Event) => this.handleExpiresAtChange(e)}
                      help-text="Constraint expires at this time"
                    ></sl-input>
                    ${this.expiresAtLocal
                      ? html`<div class="utc-preview" aria-label="UTC equivalent">
                          UTC: ${this.formatUtcPreview(this.expiresAtLocal)}
                        </div>`
                      : nothing}
                  </div>
                </div>
              </fieldset>

              ${this.validationError
                ? html`
                    <sl-alert
                      variant="warning"
                      open
                      class="validation-warning"
                      id="schedule-validation-msg"
                      role="alert"
                    >
                      <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                      ${this.validationError}
                    </sl-alert>
                  `
                : nothing}

              <div class="clear-section">
                <sl-button variant="text" size="small" @click=${() => this.handleClearSchedule()}>
                  <sl-icon name="x-circle" slot="prefix"></sl-icon>
                  Remove activation window
                </sl-button>
              </div>
            `
          : html`
              <div class="no-schedule-info">
                This access boundary will take effect immediately and will not expire.
              </div>
            `}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-access-boundary-schedule-editor': ScionAccessBoundaryScheduleEditor;
  }
}
