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
 * The one Leave project dialog (ptone/scion#7494), shared by the project
 * header, the settings Danger Zone and the members editor's own row.
 *
 * `show()` runs the dry run and renders its consequences, or the refusal
 * the hub gave. Confirm optionally deletes the caller's agents first, one at
 * a time, each waited to the hub's confirmation; a delete that fails or is
 * not confirmed stops the flow before any leave is sent, names the agent and
 * offers Retry or Cancel. Then the real leave is sent.
 *
 * After a leave the dialog shows a toast, drops the project from the cached
 * lists, announces the membership change, and navigates to /projects when
 * no access remains. It then dispatches `scion-project-left` (bubbling,
 * composed) so a host that stays can refresh. The Transfer ownership link of
 * a last-owner refusal dispatches `scion-leave-transfer-ownership`.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import {
  deleteLeaveAgents,
  requestLeave,
  LEAVE_NOT_MEMBER_MESSAGE,
  type LeaveAgentDeleter,
  type LeaveAgentItem,
  type LeaveOutcome,
  type LeaveResult,
} from '../../client/project-leave.js';
import type { AgentDeletionWaitOptions } from '../../client/agent-delete-wait.js';
import { stateManager } from '../../client/state.js';
import { navigateTo } from '../../client/navigation.js';
import { dispatchMembershipChanged } from '../../utils/membership-events.js';
import { showToast } from '../../utils/toast.js';

/** Event a host listens for to refresh after a leave that kept access. */
export const PROJECT_LEFT_EVENT = 'scion-project-left';
/** Event a host listens for to open its transfer ownership dialog. */
export const LEAVE_TRANSFER_OWNERSHIP_EVENT = 'scion-leave-transfer-ownership';

export interface ProjectLeftDetail {
  projectId: string;
  result: LeaveResult;
}

export const LEAVE_LAST_OWNER_GUIDANCE =
  "You're the last owner of this project. Transfer ownership to another member, or delete the project, before you leave.";
export const LEAVE_GROUP_DERIVED_GUIDANCE =
  "You're a member of this project through a group, and leaving removes only direct roles. Leave the group, or ask a group admin to remove you.";
/** The way on from the truncated delete-first refusal: leave without the delete. */
export const LEAVE_TRUNCATED_CLOSE_HINT =
  "You haven't left the project. To leave without deleting your agents, choose Cancel, then open Leave again with the box cleared.";
export const LEAVE_TRUNCATED_NOTE =
  "You have more agents than can be listed here, so they can't be deleted from this dialog. Delete them first, or leave them on hold.";

/** The dialog's phase. */
type Phase =
  | 'closed'
  | 'loading'
  | 'preview'
  | 'refused'
  | 'deleting'
  | 'delete_failed'
  | 'leaving';

/** A count with its noun ("1 access token", "3 access tokens"). */
function countNoun(n: number, noun: string): string {
  return `${n} ${noun}${n === 1 ? '' : 's'}`;
}

/** The access-kept line for a leave that keeps group or hub access. */
export function accessKeptLine(remainingAccess: string): string {
  return remainingAccess === 'hub'
    ? "You'll keep access through your hub role. This removes only your direct role."
    : "You'll keep access through a group. This removes only your direct role.";
}

/**
 * The after-leave steps shared by every entry point: a toast, the project
 * dropped from cached lists (when no access remains), the membership change
 * announced, and /projects when no access remains.
 */
export function completeProjectLeave(
  projectId: string,
  projectName: string,
  result: LeaveResult
): void {
  showToast(`You left ${projectName}`, 'success');
  dispatchMembershipChanged({ kind: 'project', id: projectId });
  if (result.remainingAccess === 'none') {
    stateManager.forgetProject(projectId);
    navigateTo('/projects');
  }
}

@customElement('scion-project-leave-dialog')
export class ScionProjectLeaveDialog extends LitElement {
  @property() projectId = '';
  @property() projectName = '';
  /** Show the Transfer ownership link on a last-owner refusal. */
  @property({ type: Boolean }) transferAvailable = false;

  @state() private phase: Phase = 'closed';
  @state() private result: LeaveResult | null = null;
  @state() private refusal: Exclude<LeaveOutcome, { kind: 'ok' }> | null = null;
  @state() private deleteAgents = false;
  @state() private progress: LeaveAgentItem | null = null;
  @state() private remaining: LeaveAgentItem[] = [];
  @state() private failure: { agent: LeaveAgentItem; message: string } | null = null;

  /** Delete step and poll tuning, replaceable in tests. */
  deleter: LeaveAgentDeleter | undefined;
  waitOptions: AgentDeletionWaitOptions | undefined;

  static override styles = css`
    :host {
      display: contents;
    }
    p {
      margin: 0 0 0.75rem;
    }
    ul.consequences {
      margin: 0 0 0.75rem;
      padding-left: 1.25rem;
    }
    ul.consequences li {
      margin-bottom: 0.5rem;
    }
    ul.agents {
      margin: 0.25rem 0 0;
      padding-left: 1.25rem;
      max-height: 10rem;
      overflow-y: auto;
      font-size: var(--sl-font-size-small);
    }
    .note,
    .roles {
      font-size: var(--sl-font-size-small);
      color: var(--scion-text-muted, var(--sl-color-neutral-600));
    }
    .error {
      color: var(--sl-color-danger-600);
      margin-bottom: 0.75rem;
    }
    .loading {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }
  `;

  /**
   * Set after a not-a-member answer: the project was dropped from the cached
   * lists, and closing the dialog goes to /projects.
   */
  private leaveOnClose = false;

  /** Open the dialog and run the dry run (a no-op while a delete or the leave runs). */
  async show(): Promise<void> {
    if (this.busy) return;
    this.leaveOnClose = false;
    this.result = null;
    this.refusal = null;
    this.deleteAgents = false;
    this.progress = null;
    this.remaining = [];
    this.failure = null;
    this.phase = 'loading';
    const outcome = await requestLeave(this.projectId, true);
    if (this.phase !== 'loading') return; // closed meanwhile
    if (outcome.kind === 'ok') {
      this.result = outcome.result;
      this.phase = 'preview';
    } else {
      this.refuse(outcome);
    }
  }

  /** Close the dialog unless a delete or the leave is running. */
  close(): void {
    if (this.busy) return;
    this.phase = 'closed';
    if (this.leaveOnClose) {
      this.leaveOnClose = false;
      navigateTo('/projects');
    }
  }

  /**
   * Show a refusal. A not-a-member answer means this session can no longer
   * read the project: it is dropped from the cached lists at once, and
   * closing the dialog goes to /projects.
   */
  private refuse(outcome: Exclude<LeaveOutcome, { kind: 'ok' }>): void {
    this.refusal = outcome;
    this.phase = 'refused';
    if (outcome.kind === 'not_member') {
      stateManager.forgetProject(this.projectId);
      this.leaveOnClose = true;
    }
  }

  private get busy(): boolean {
    return this.phase === 'deleting' || this.phase === 'leaving';
  }

  private get label(): string {
    return this.projectName || this.projectId;
  }

  /** Agents are deleted only when they would be held and the list is whole. */
  private get canDeleteAgents(): boolean {
    const r = this.result;
    return (
      !!r &&
      r.remainingAccess === 'none' &&
      r.agents.count > 0 &&
      !r.agents.truncated &&
      r.agents.items.length >= r.agents.count
    );
  }

  private async confirm(): Promise<void> {
    if (!this.result || this.busy) return;
    // A delete was asked for on a list that cannot be deleted in full (the
    // checkbox is disabled then, but its state is not trusted): never leave.
    if (this.deleteAgents && !this.canDeleteAgents) {
      this.failure = null;
      this.phase = 'delete_failed';
      return;
    }
    if (this.deleteAgents) {
      this.remaining = [...this.result.agents.items];
      if (!(await this.runDeletes())) return;
    }
    await this.leave();
  }

  /** Delete `remaining` in order; false when the flow stopped. */
  private async runDeletes(): Promise<boolean> {
    this.phase = 'deleting';
    this.failure = null;
    const res = await deleteLeaveAgents(
      { count: this.remaining.length, truncated: false, items: this.remaining },
      {
        deleter: this.deleter,
        wait: this.waitOptions,
        onProgress: (agent) => {
          this.progress = agent;
        },
      }
    );
    this.progress = null;
    if (res.kind === 'done') {
      this.remaining = [];
      return true;
    }
    if (res.kind === 'stopped') {
      const done = new Set(res.deleted.map((a) => a.id));
      this.remaining = this.remaining.filter((a) => !done.has(a.id));
      this.failure = { agent: res.failed, message: res.message };
    } else {
      this.failure = null;
    }
    this.phase = 'delete_failed';
    return false;
  }

  private async retryDeletes(): Promise<void> {
    if (this.busy || this.remaining.length === 0) return;
    if (await this.runDeletes()) await this.leave();
  }

  private async leave(): Promise<void> {
    this.phase = 'leaving';
    const outcome = await requestLeave(this.projectId, false);
    if (outcome.kind !== 'ok') {
      this.refuse(outcome);
      return;
    }
    this.phase = 'closed';
    completeProjectLeave(this.projectId, this.label, outcome.result);
    this.dispatchEvent(
      new CustomEvent<ProjectLeftDetail>(PROJECT_LEFT_EVENT, {
        detail: { projectId: this.projectId, result: outcome.result },
        bubbles: true,
        composed: true,
      })
    );
  }

  private openTransfer(): void {
    this.phase = 'closed';
    this.dispatchEvent(
      new CustomEvent(LEAVE_TRANSFER_OWNERSHIP_EVENT, {
        detail: { projectId: this.projectId },
        bubbles: true,
        composed: true,
      })
    );
  }

  override render(): TemplateResult | typeof nothing {
    if (this.phase === 'closed') return nothing;
    return html`
      <sl-dialog
        label="Leave ${this.label}?"
        open
        @sl-request-close=${(e: Event) => {
          if (this.busy) {
            e.preventDefault();
            return;
          }
          this.close();
        }}
      >
        ${this.renderBody()} ${this.renderFooter()}
      </sl-dialog>
    `;
  }

  private renderBody(): TemplateResult {
    switch (this.phase) {
      case 'loading':
        return html`<div class="loading" data-leave-loading>
          <sl-spinner></sl-spinner> Checking what leaving changes…
        </div>`;
      case 'refused':
        return this.renderRefusal();
      case 'deleting':
        return html`<div class="loading" data-leave-deleting>
          <sl-spinner></sl-spinner>
          Deleting ${this.progress ? this.progress.name || this.progress.id : 'agents'}…
        </div>`;
      case 'delete_failed':
        return this.renderDeleteFailure();
      case 'leaving':
        return html`<div class="loading" data-leave-leaving>
          <sl-spinner></sl-spinner> Leaving ${this.label}…
        </div>`;
      default:
        return this.renderPreview();
    }
  }

  private renderPreview(): TemplateResult {
    const r = this.result!;
    const kept = r.remainingAccess !== 'none';
    const roles = r.removedRoles.length
      ? html`<p class="roles" data-leave-roles>Roles removed: ${r.removedRoles.join(', ')}</p>`
      : nothing;
    if (kept) {
      return html`
        <p data-leave-kept>${accessKeptLine(r.remainingAccess)}</p>
        ${roles}
      `;
    }
    return html`
      <p data-leave-intro>
        You'll lose access to this project, its agents, chat and files. To come back, an owner or
        admin has to add you again.
      </p>
      ${roles}
      <ul class="consequences">
        ${r.agents.count > 0 ? this.renderAgentsBullet(r) : nothing}
        <li data-leave-secrets>
          Secrets you share with your agents stop reaching agents in this project.
        </li>
        ${r.projectBoundTokens.revoked > 0
          ? html`<li data-leave-tokens>
              <strong>${countNoun(r.projectBoundTokens.revoked, 'access token')}</strong> limited to
              this project will be revoked. If you're added back, you'll need to create new ones.
            </li>`
          : nothing}
        <li data-leave-steward>You lose steward rights over artifacts in this project.</li>
      </ul>
    `;
  }

  private renderAgentsBullet(r: LeaveResult): TemplateResult {
    const more = r.agents.count - r.agents.items.length;
    return html`
      <li data-leave-agents>
        <strong>${r.agents.count} of your ${r.agents.count === 1 ? 'agent' : 'agents'}</strong> in
        this project will be stopped and put on hold. You won't be able to see or delete them after
        you leave, and they stay on hold even if you're added back.
        <div>
          <sl-checkbox
            data-leave-delete-agents
            ?checked=${this.deleteAgents}
            ?disabled=${!this.canDeleteAgents}
            @sl-change=${(e: Event) => {
              this.deleteAgents = (e.target as HTMLInputElement).checked;
            }}
            >Delete these agents before leaving</sl-checkbox
          >
        </div>
        ${r.agents.truncated
          ? html`<div class="note" data-leave-truncated>${LEAVE_TRUNCATED_NOTE}</div>`
          : nothing}
        <ul class="agents" data-leave-agent-list>
          ${r.agents.items.map((a) => html`<li>${a.name || a.id}</li>`)}
          ${more > 0 ? html`<li data-leave-more>+${more} more</li>` : nothing}
        </ul>
      </li>
    `;
  }

  private renderRefusal(): TemplateResult {
    const f = this.refusal!;
    switch (f.kind) {
      case 'last_owner':
        return html`<p data-leave-refusal="last_owner">${LEAVE_LAST_OWNER_GUIDANCE}</p>
          ${this.transferAvailable
            ? html`<sl-button
                data-leave-transfer
                variant="default"
                size="small"
                @click=${() => this.openTransfer()}
              >
                <sl-icon slot="prefix" name="arrow-left-right"></sl-icon>
                Transfer ownership
              </sl-button>`
            : nothing}`;
      case 'group_derived':
        return html`<p data-leave-refusal="group_derived">${LEAVE_GROUP_DERIVED_GUIDANCE}</p>
          ${f.groups.length
            ? html`<p class="note">Groups that give you access:</p>
                <ul class="agents" data-leave-groups>
                  ${f.groups.map((g) => html`<li>${g.name}</li>`)}
                </ul>`
            : nothing}`;
      case 'session_required':
        return html`<p data-leave-refusal="session_required">${f.message}</p>`;
      case 'not_member':
        return html`<p data-leave-refusal="not_member">${LEAVE_NOT_MEMBER_MESSAGE}</p>`;
      case 'unavailable':
        return html`<p data-leave-refusal="unavailable">${f.message}</p>`;
      default:
        return html`<p class="error" data-leave-refusal="error">${f.message}</p>`;
    }
  }

  private renderDeleteFailure(): TemplateResult {
    if (!this.failure) {
      return html`<p class="error" data-leave-delete-failed>${LEAVE_TRUNCATED_NOTE}</p>
        <p class="note" data-leave-truncated-hint>${LEAVE_TRUNCATED_CLOSE_HINT}</p>`;
    }
    const name = this.failure.agent.name || this.failure.agent.id;
    return html`<p class="error" data-leave-delete-failed>
        Couldn't delete agent ${name}: ${this.failure.message}. You haven't left the project.
      </p>
      <p class="note">Retry the delete, or cancel and keep your membership.</p>`;
  }

  private renderFooter(): TemplateResult {
    const cancel = html`<sl-button
      slot="footer"
      data-leave-cancel
      ?disabled=${this.busy}
      @click=${() => this.close()}
      >${this.phase === 'refused' ? 'Close' : 'Cancel'}</sl-button
    >`;
    if (this.phase === 'refused' && this.refusal?.kind === 'error') {
      return html`${cancel}
        <sl-button slot="footer" data-leave-retry variant="primary" @click=${() => this.show()}
          >Retry</sl-button
        >`;
    }
    if (this.phase === 'delete_failed') {
      return html`${cancel}
      ${this.failure
        ? html`<sl-button
            slot="footer"
            data-leave-retry
            variant="primary"
            @click=${() => this.retryDeletes()}
            >Retry</sl-button
          >`
        : nothing}`;
    }
    if (this.phase === 'preview' || this.busy) {
      return html`${cancel}
        <sl-button
          slot="footer"
          data-leave-confirm
          variant="danger"
          ?loading=${this.busy}
          ?disabled=${this.busy}
          @click=${() => this.confirm()}
          >Leave project</sl-button
        >`;
    }
    return cancel;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-project-leave-dialog': ScionProjectLeaveDialog;
  }
}
