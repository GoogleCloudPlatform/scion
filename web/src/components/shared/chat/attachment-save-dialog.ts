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
 * "Save as artifact…" dialog for chat attachments (ptone/scion#4598, #4599).
 *
 * Publishes an existing chat attachment as an artifact by reference
 * (`POST /api/v1/artifacts` with `fromAttachment`), copying no blob bytes.
 * Lets the user pick a target home project, choose between creating a new
 * artifact or appending the next version of an existing keyed artifact, and
 * shows a result state with the new artifact chip and an option to post it
 * into the conversation thread.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import {
  artifactPagePath,
  canPublishIn,
  defaultProjectChoice,
  filterPublishable,
  findGlobalProject,
  formatArtifactRef,
  listReadableProjects,
  publishErrorMessage,
  publishFromAttachment,
  READABLE_PROJECTS_CAP,
  type Artifact,
  type ArtifactResponse,
  type ArtifactVersion,
  type ProjectChoice,
} from '../../../client/artifacts.js';
import { formatFileSize } from '../../../utils/chat-file-links.js';
import { copyToClipboard, showCopyFailedToast } from '../../../utils/clipboard.js';

/** Minimal attachment metadata needed by the save dialog. */
export interface SaveAttachmentTarget {
  id: string;
  name: string;
  mime: string;
  size: number;
}

/** Detail of the `attachment-artifact-post` event when posting the saved artifact to the thread. */
export interface AttachmentArtifactPostDetail {
  artifact: Artifact;
  version?: ArtifactVersion | undefined;
  ref: string;
}

type SaveMode = 'new' | 'version';

@customElement('scion-attachment-save-dialog')
export class ScionAttachmentSaveDialog extends LitElement {
  @property({ type: Boolean, reflect: true })
  open = false;

  /** The attachment being saved as an artifact. */
  @property({ attribute: false })
  attachment: SaveAttachmentTarget | null = null;

  /** Conversation's project ID, preselected when the user can publish there. */
  @property({ type: String })
  defaultProjectId = '';

  /** Whether the user can post the resulting artifact chip into the thread. */
  @property({ type: Boolean })
  canPostToThread = true;

  @state() private mode: SaveMode = 'new';
  @state() private titleValue = '';
  @state() private keyValue = '';
  @state() private noteValue = '';
  @state() private busy = false;
  @state() private error: string | null = null;
  @state() private result: ArtifactResponse | null = null;
  @state() private copied = false;
  @state() private posted = false;

  /** Publishable projects for the project picker. */
  @state() private choices: ProjectChoice[] = [];
  @state() private choicesLoading = false;
  @state() private choicesError: string | null = null;
  @state() private chosen = '';
  @state() private choicesTruncated = false;

  private userPicked = false;
  private choicesGen = 0;
  private choicesAbort: AbortController | null = null;
  private copyTimer: ReturnType<typeof setTimeout> | null = null;

  static override styles = css`
    sl-dialog::part(panel) {
      width: min(520px, 95vw);
    }

    .attachment-summary {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      margin-bottom: 0.75rem;
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      font-size: 0.875rem;
      color: var(--scion-text);
    }

    .attachment-summary sl-icon {
      color: var(--scion-primary);
      flex-shrink: 0;
    }

    .attachment-summary .name {
      font-weight: 500;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      flex: 1;
      min-width: 0;
    }

    .attachment-summary .size {
      color: var(--scion-text-muted);
      font-size: 0.8125rem;
      white-space: nowrap;
    }

    .project {
      padding-bottom: 0.75rem;
      margin-bottom: 0.75rem;
      border-bottom: 1px solid var(--scion-border);
    }

    .project sl-alert sl-button {
      margin-left: 0.5rem;
    }

    .choices-note {
      font-size: 0.75rem;
      color: var(--scion-text-muted);
      margin-top: 0.25rem;
    }

    .slug {
      font-family: var(--scion-font-mono);
      font-size: 0.75rem;
      color: var(--scion-text-muted);
    }

    .mode-group {
      margin-bottom: 0.75rem;
    }

    .fields {
      display: flex;
      flex-direction: column;
      gap: 0.625rem;
    }

    .error-alert {
      margin-top: 0.75rem;
    }

    .result-state {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
    }

    .result-intro {
      margin: 0;
      font-size: 0.875rem;
      color: var(--scion-text-muted);
    }

    .result-artifact-chip {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      color: var(--scion-text);
      text-decoration: none;
      font-size: 0.875rem;
      max-width: 100%;
    }

    .result-artifact-chip:hover {
      background: var(--scion-border);
    }

    .result-artifact-chip sl-icon {
      color: var(--scion-primary);
      flex-shrink: 0;
    }

    .result-artifact-chip .artifact-title {
      font-weight: 600;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .result-artifact-chip .artifact-version {
      color: var(--scion-text-muted);
      white-space: nowrap;
    }

    .result-artifact-chip .open-icon {
      margin-left: auto;
      color: var(--scion-text-muted);
    }

    .footer-actions {
      display: flex;
      align-items: center;
      justify-content: flex-end;
      gap: 0.5rem;
      width: 100%;
    }
  `;

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('open') && this.open && !changed.get('open')) {
      this.reset();
      void this.loadChoices();
    } else if (changed.has('open') && !this.open) {
      this.cancelChoices();
      this.clearCopyTimer();
    } else if (changed.has('attachment') && this.open && this.attachment && !this.result) {
      this.titleValue = this.attachment.name;
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.cancelChoices();
    this.clearCopyTimer();
  }

  private reset(): void {
    this.mode = 'new';
    this.titleValue = this.attachment?.name ?? '';
    this.keyValue = '';
    this.noteValue = '';
    this.busy = false;
    this.error = null;
    this.result = null;
    this.copied = false;
    this.posted = false;
    this.clearCopyTimer();
  }

  private clearCopyTimer(): void {
    if (this.copyTimer) {
      clearTimeout(this.copyTimer);
      this.copyTimer = null;
    }
  }

  private cancelChoices(): void {
    this.choicesGen++;
    this.choicesAbort?.abort();
    this.choicesAbort = null;
  }

  private pickDefault(publishable: ProjectChoice[]): string {
    if (this.defaultProjectId && publishable.some((p) => p.id === this.defaultProjectId)) {
      return this.defaultProjectId;
    }
    return defaultProjectChoice(publishable);
  }

  private async loadChoices(): Promise<void> {
    this.cancelChoices();
    const gen = this.choicesGen;
    const abort = new AbortController();
    this.choicesAbort = abort;
    const signal = abort.signal;
    this.choicesLoading = true;
    this.choicesError = null;
    this.choices = [];
    this.chosen = '';
    this.choicesTruncated = false;
    this.userPicked = false;
    try {
      const global = await findGlobalProject(signal);
      if (gen !== this.choicesGen) return;
      let others: ProjectChoice[] = [];
      let globalOk = false;
      const withGlobal = (list: ProjectChoice[]): ProjectChoice[] =>
        global && globalOk ? [global, ...list] : list;
      const live = (): boolean => gen === this.choicesGen && !signal.aborted;
      const globalCheck = async (): Promise<void> => {
        if (!global || !(await canPublishIn(global.id, signal))) return;
        if (!live()) return;
        globalOk = true;
        this.choices = withGlobal(others);
        if (!this.userPicked && !this.chosen && !this.defaultProjectId) {
          this.chosen = global.id;
        }
      };
      const listAndFilter = async (): Promise<void> => {
        const rest = await listReadableProjects({ skip: global?.id, signal });
        if (!live()) return;
        this.choicesTruncated = rest.truncated;
        others = await filterPublishable(rest.projects, {
          onUpdate: (sofar) => {
            if (!live()) return;
            others = sofar;
            const list = withGlobal(sofar);
            this.choices = list;
            if (
              !this.userPicked &&
              this.defaultProjectId &&
              list.some((p) => p.id === this.defaultProjectId)
            ) {
              this.chosen = this.defaultProjectId;
            }
          },
          signal,
        });
      };
      await Promise.all([globalCheck(), listAndFilter()]);
      if (gen !== this.choicesGen) return;
      const publishable = withGlobal(others);
      this.choices = publishable;
      if (!this.userPicked && !this.chosen) {
        this.chosen = this.pickDefault(publishable);
      }
    } catch (err) {
      if (gen !== this.choicesGen) return;
      abort.abort();
      this.chosen = '';
      this.choicesError = err instanceof Error ? err.message : 'Could not load projects';
    } finally {
      if (gen === this.choicesGen) this.choicesLoading = false;
      if (this.choicesAbort === abort) this.choicesAbort = null;
    }
  }

  private get canSubmit(): boolean {
    if (this.busy || !this.attachment || !this.chosen) return false;
    if (this.mode === 'version') {
      return this.keyValue.trim() !== '';
    }
    return this.titleValue.trim() !== '';
  }

  private async submit(): Promise<void> {
    if (!this.canSubmit || !this.attachment) return;
    this.busy = true;
    this.error = null;
    try {
      const req = {
        fromAttachment: this.attachment.id,
        scopeRef: this.chosen,
        ...(this.mode === 'new' && this.titleValue.trim() ? { title: this.titleValue.trim() } : {}),
        ...(this.keyValue.trim() ? { key: this.keyValue.trim() } : {}),
        ...(this.noteValue.trim() ? { note: this.noteValue.trim() } : {}),
      };
      const res = await publishFromAttachment(req);
      this.result = res;
      this.dispatchEvent(
        new CustomEvent<ArtifactResponse>('attachment-artifact-saved', {
          detail: res,
          bubbles: true,
          composed: true,
        })
      );
    } catch (err) {
      this.error = publishErrorMessage(err);
    } finally {
      this.busy = false;
    }
  }

  private async copyResultRef(): Promise<void> {
    if (!this.result) return;
    const ref = formatArtifactRef(this.result.artifact.id, this.result.version?.seq);
    if (!(await copyToClipboard(ref))) {
      showCopyFailedToast();
      return;
    }
    this.copied = true;
    this.clearCopyTimer();
    this.copyTimer = setTimeout(() => {
      this.copied = false;
      this.copyTimer = null;
    }, 1500);
  }

  private postToThread(): void {
    if (!this.result || this.posted) return;
    const ref = formatArtifactRef(this.result.artifact.id);
    this.posted = true;
    this.dispatchEvent(
      new CustomEvent<AttachmentArtifactPostDetail>('attachment-artifact-post', {
        detail: {
          artifact: this.result.artifact,
          version: this.result.version,
          ref,
        },
        bubbles: true,
        composed: true,
      })
    );
    this.close();
  }

  private close(): void {
    this.cancelChoices();
    this.clearCopyTimer();
    this.open = false;
    this.dispatchEvent(
      new CustomEvent('attachment-save-closed', { bubbles: true, composed: true })
    );
  }

  private renderProjectPicker(): TemplateResult {
    if (this.choicesError) {
      return html`
        <div class="project">
          <sl-alert variant="danger" open class="choices-error">
            <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
            Could not load your projects: ${this.choicesError}
            <sl-button size="small" @click=${(): void => void this.loadChoices()}>
              <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
              Retry
            </sl-button>
          </sl-alert>
        </div>
      `;
    }
    const none = !this.choicesLoading && this.choices.length === 0;
    const empty = this.choices.length === 0;
    return html`
      <div class="project">
        <sl-select
          class="project-select"
          label="Project"
          size="small"
          required
          hoist
          placeholder=${this.choicesLoading && empty ? 'Loading projects…' : 'Choose a project'}
          help-text=${none
            ? "You can't publish in any of your projects."
            : "The artifact's home. Members of this project can read it."}
          ?disabled=${this.busy || empty}
          .value=${this.chosen}
          @sl-change=${(e: Event): void => {
            this.chosen = (e.target as HTMLSelectElement).value;
            this.userPicked = true;
          }}
        >
          ${this.choices.map(
            (p) => html`
              <sl-option value=${p.id}>
                ${p.name}
                ${p.slug ? html`<span slot="suffix" class="slug">${p.slug}</span>` : nothing}
              </sl-option>
            `
          )}
        </sl-select>
        ${this.choicesLoading && !empty
          ? html`<div class="choices-note checking">Checking more projects…</div>`
          : nothing}
        ${this.choicesTruncated
          ? html`<div class="choices-note truncated">
              Showing projects from the first ${READABLE_PROJECTS_CAP} you can see.
            </div>`
          : nothing}
      </div>
    `;
  }

  private renderForm(): TemplateResult {
    const att = this.attachment;
    return html`
      ${att
        ? html`
            <div class="attachment-summary">
              <sl-icon name="paperclip"></sl-icon>
              <span class="name" title=${att.name}>${att.name}</span>
              <span class="size">${formatFileSize(att.size)}</span>
            </div>
          `
        : nothing}
      ${this.renderProjectPicker()}
      <sl-radio-group
        class="mode-group"
        label="Save as"
        size="small"
        value=${this.mode}
        @sl-change=${(e: Event): void => {
          this.mode = (e.target as HTMLInputElement).value as SaveMode;
        }}
      >
        <sl-radio-button value="new">New artifact</sl-radio-button>
        <sl-radio-button value="version">Next version of existing key</sl-radio-button>
      </sl-radio-group>

      <div class="fields">
        ${this.mode === 'new'
          ? html`
              <sl-input
                class="title-input"
                label="Title"
                size="small"
                required
                ?disabled=${this.busy}
                .value=${this.titleValue}
                @sl-input=${(e: Event): void => {
                  this.titleValue = (e.target as HTMLInputElement).value;
                }}
              ></sl-input>
              <sl-input
                class="key-input"
                label="Key (optional)"
                help-text="Publishing again under the same key appends a new version."
                size="small"
                ?disabled=${this.busy}
                .value=${this.keyValue}
                @sl-input=${(e: Event): void => {
                  this.keyValue = (e.target as HTMLInputElement).value;
                }}
              ></sl-input>
            `
          : html`
              <sl-input
                class="key-input"
                label="Artifact key"
                help-text="Appends the next version to your existing artifact with this key in the selected project."
                size="small"
                required
                ?disabled=${this.busy}
                .value=${this.keyValue}
                @sl-input=${(e: Event): void => {
                  this.keyValue = (e.target as HTMLInputElement).value;
                }}
              ></sl-input>
            `}
        <sl-input
          class="note-input"
          label="Note (optional)"
          size="small"
          ?disabled=${this.busy}
          .value=${this.noteValue}
          @sl-input=${(e: Event): void => {
            this.noteValue = (e.target as HTMLInputElement).value;
          }}
        ></sl-input>
      </div>

      ${this.error
        ? html`
            <sl-alert class="error-alert" variant="danger" open>
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              ${this.error}
            </sl-alert>
          `
        : nothing}
    `;
  }

  private renderResult(res: ArtifactResponse): TemplateResult {
    const seq = res.version?.seq ?? res.artifact.currentSeq;
    const href = artifactPagePath(res.artifact, seq);
    const ref = formatArtifactRef(res.artifact.id, seq);
    return html`
      <div class="result-state">
        <p class="result-intro">
          ${seq > 1 ? `Published version v${seq} of` : 'Saved as artifact'}
          <strong>${res.artifact.title}</strong>.
        </p>
        <a class="result-artifact-chip" href=${href} title=${ref}>
          <sl-icon name="file-earmark-richtext"></sl-icon>
          <span class="artifact-title">${res.artifact.title}</span>
          <span class="artifact-version">· v${seq}</span>
          <sl-icon class="open-icon" name="box-arrow-up-right"></sl-icon>
        </a>
      </div>
    `;
  }

  override render(): TemplateResult {
    const res = this.result;
    return html`
      <sl-dialog
        label=${res ? 'Artifact saved' : 'Save as artifact'}
        ?open=${this.open}
        @sl-request-close=${(e: Event): void => {
          if (this.busy) e.preventDefault();
        }}
        @sl-after-hide=${(e: Event): void => {
          if (e.target === e.currentTarget && this.open) this.close();
        }}
      >
        ${res ? this.renderResult(res) : this.renderForm()}
        <div slot="footer" class="footer-actions">
          ${res
            ? html`
                <sl-button size="small" @click=${(): void => void this.copyResultRef()}>
                  <sl-icon slot="prefix" name=${this.copied ? 'check2' : 'clipboard'}></sl-icon>
                  ${this.copied ? 'Copied!' : 'Copy reference'}
                </sl-button>
                ${this.canPostToThread
                  ? html`
                      <sl-button
                        class="post-to-thread-btn"
                        size="small"
                        variant="primary"
                        ?disabled=${this.posted}
                        @click=${(): void => this.postToThread()}
                      >
                        <sl-icon slot="prefix" name="send"></sl-icon>
                        Post into thread
                      </sl-button>
                    `
                  : nothing}
                <sl-button size="small" @click=${(): void => this.close()}>Done</sl-button>
              `
            : html`
                <sl-button size="small" ?disabled=${this.busy} @click=${(): void => this.close()}>
                  Cancel
                </sl-button>
                <sl-button
                  class="submit-btn"
                  size="small"
                  variant="primary"
                  ?loading=${this.busy}
                  ?disabled=${!this.canSubmit}
                  @click=${(): void => void this.submit()}
                >
                  ${this.mode === 'version' ? 'Publish version' : 'Save as artifact'}
                </sl-button>
              `}
        </div>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-attachment-save-dialog': ScionAttachmentSaveDialog;
  }
}
