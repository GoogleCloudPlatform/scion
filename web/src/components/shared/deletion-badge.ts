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
 * "Deleting…" / "Delete failed: …" badge and the per-view lease timer for
 * the backend-driven delete lifecycle (ptone/scion#2483 phase 1b).
 */

import { LitElement, html, css, nothing } from 'lit';
import type { ReactiveController, ReactiveControllerHost, TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import type { Agent, DeletionInfo } from '../../shared/types.js';
import {
  deletionBadgeLabel,
  nextDeletionDeadline,
  effectiveDeletion,
  isDeletionActive,
} from '../../shared/agent-deletion.js';

/** Injectable clock for {@link DeletionLeaseController} (tests pass a fake). */
export interface DeletionClock {
  now(): number;
  setTimeout(fn: () => void, ms: number): unknown;
  clearTimeout(handle: unknown): void;
}

const systemClock: DeletionClock = {
  now: () => Date.now(),
  setTimeout: (fn, ms) => setTimeout(fn, ms),
  clearTimeout: (handle) => clearTimeout(handle as ReturnType<typeof setTimeout>),
};

/**
 * Longest delay `setTimeout` accepts; larger values overflow and fire at
 * once.
 */
const MAX_TIMER_DELAY_MS = 2 ** 31 - 1;

/**
 * One timer per view, armed for the next deletion deadline among the view's
 * agents (N4): the earliest `leaseExpiresAt` of a `deleting` view or
 * `expiresAt` of a `failed` one. When it fires the host re-renders, and
 * `effectiveDeletion` then reads an expired lease as failed/abandoned, or
 * an expired failure as gone, without waiting for a refetch. The timer is
 * recomputed after every host update, so a renewal delta (which re-renders
 * the host with a later lease) re-arms it, and a cleared or removed agent
 * disarms it.
 */
export class DeletionLeaseController implements ReactiveController {
  private handle: unknown = null;
  private armedFor: number | null = null;

  constructor(
    private readonly host: ReactiveControllerHost,
    private readonly getAgents: () => Iterable<Pick<Agent, 'deletion'>>,
    private readonly clock: DeletionClock = systemClock
  ) {
    host.addController(this);
  }

  /** Current time on this controller's clock; render with this. */
  now(): number {
    return this.clock.now();
  }

  /** The deletion view for `agent` right now (lease expiry applied). */
  view(agent: Pick<Agent, 'deletion'>): DeletionInfo | null {
    return effectiveDeletion(agent.deletion, this.clock.now());
  }

  /** Whether `agent` has a live delete right now; hide lifecycle actions when true. */
  isDeleting(agent: Pick<Agent, 'deletion'>): boolean {
    return isDeletionActive(agent, this.clock.now());
  }

  hostConnected(): void {
    this.rearm();
  }

  hostUpdated(): void {
    this.rearm();
  }

  hostDisconnected(): void {
    this.disarm();
  }

  /** Re-evaluate the next deadline and (re)arm the single timer. */
  rearm(): void {
    const next = nextDeletionDeadline(this.getAgents(), this.clock.now());
    if (next === this.armedFor) return;
    this.disarm();
    if (next === null) return;
    this.armedFor = next;
    this.handle = this.clock.setTimeout(
      () => {
        this.handle = null;
        this.armedFor = null;
        this.host.requestUpdate();
      },
      Math.min(Math.max(0, next - this.clock.now()), MAX_TIMER_DELAY_MS)
    );
  }

  private disarm(): void {
    if (this.handle !== null) this.clock.clearTimeout(this.handle);
    this.handle = null;
    this.armedFor = null;
  }
}

/**
 * Renders nothing for `null`, "Deleting…" for a live delete, and "Delete
 * failed: …" (or "Delete interrupted" for `abandoned`) for a failed one.
 * Pass the *effective* view (`DeletionLeaseController.view`), not the raw
 * `agent.deletion`, so an expired lease shows as interrupted.
 */
@customElement('scion-deletion-badge')
export class ScionDeletionBadge extends LitElement {
  @property({ attribute: false })
  deletion: DeletionInfo | null = null;

  @property({ type: String })
  size: 'small' | 'medium' = 'medium';

  /**
   * Announce changes to assistive tech through a `role="status"` live
   * region. Set it only on a page's single primary badge (agent-detail's
   * header); list rows rely on the visible text and `title`, so a page of
   * rows does not flood the live region. The region is rendered even
   * while there is no deletion (visually hidden, out of layout), because
   * screen readers announce changes only inside a region that already
   * exists: the first "Deleting…" must land in it, not arrive with it.
   */
  @property({ type: Boolean })
  live = false;

  static override styles = css`
    :host {
      display: inline-flex;
      min-width: 0;
    }

    :host([hidden]) {
      display: none;
    }

    /* A live badge with nothing to show: keep the (empty) live region in
       the accessibility tree, but take no space and no flex gap. */
    :host([live][empty]) {
      position: absolute;
      width: 1px;
      height: 1px;
      overflow: hidden;
      clip: rect(0 0 0 0);
      clip-path: inset(50%);
      white-space: nowrap;
    }

    .live {
      display: inline-flex;
      min-width: 0;
    }

    .badge {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.25rem 0.625rem;
      border-radius: 9999px;
      font-weight: 500;
      font-size: 0.875rem;
      white-space: nowrap;
      max-width: 22rem;
      min-width: 0;
    }

    .badge.small {
      font-size: 0.8125rem;
      padding: 0.125rem 0.5rem;
      gap: 0.25rem;
      max-width: 16rem;
    }

    .label {
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .badge sl-icon {
      flex: none;
    }

    .badge.deleting {
      background: var(--scion-badge-warning-bg, #fef3c7);
      color: var(--scion-badge-warning-text, #92400e);
    }

    .badge.failed {
      background: var(--scion-badge-danger-bg, #fee2e2);
      color: var(--scion-badge-danger-text, #991b1b);
    }
  `;

  protected override willUpdate(): void {
    // Take no space (and no flex gap) when there is nothing to show. A live
    // badge stays rendered (visually hidden) so its live region persists.
    this.toggleAttribute('hidden', !this.deletion && !this.live);
    this.toggleAttribute('empty', !this.deletion);
  }

  override render(): TemplateResult | typeof nothing {
    const badge = this.renderBadge();
    return this.live ? html`<span class="live" role="status">${badge}</span>` : badge;
  }

  private renderBadge(): TemplateResult | typeof nothing {
    const d = this.deletion;
    if (!d) return nothing;
    const label = deletionBadgeLabel(d);
    const deleting = d.state === 'deleting';
    return html`
      <span
        class="badge ${deleting ? 'deleting' : 'failed'} ${this.size}"
        title=${label}
        data-state=${d.state}
      >
        <sl-icon name=${deleting ? 'trash' : 'exclamation-triangle'}></sl-icon>
        <span class="label">${label}</span>
      </span>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-deletion-badge': ScionDeletionBadge;
  }
}
