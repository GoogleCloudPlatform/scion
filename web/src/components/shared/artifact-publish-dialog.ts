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
 * Publish dialog for artifacts (experiment hub.artifacts).
 *
 * Uploads a file or a folder through the two-step publish API, either as a
 * new artifact homed in a project or as a new version of an artifact.
 * Fires `artifact-published` with the finalized ArtifactResponse, and
 * `artifact-publish-closed` whenever it closes; the parent owns `open`.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, query, state } from 'lit/decorators.js';

import {
  PublishError,
  formatBytes,
  publishErrorMessage,
  publishFiles,
} from '../../client/artifacts.js';
import type { ArtifactResponse, PendingPublish, PublishFile } from '../../client/artifacts.js';

/** A file picked for upload, with its path inside the bundle. */
export interface PickedFile {
  path: string;
  file: File;
}

/** Entry names preferred, in order, when a folder is picked. */
const ENTRY_PREFERENCE = ['index.html', 'index.htm', 'index.md', 'README.md', 'readme.md'];

/**
 * Turns the files of a picked folder into bundle files: paths relative to
 * the folder, hidden files and folders (a segment starting with '.')
 * skipped. Returns the folder name and the files sorted by path.
 */
export function folderFiles(files: File[]): { folder: string; files: PickedFile[] } {
  let folder = '';
  const out: PickedFile[] = [];
  for (const file of files) {
    const rel = (file as File & { webkitRelativePath?: string }).webkitRelativePath || file.name;
    const parts = rel.split('/');
    if (parts.length > 1) {
      folder ||= parts[0];
      parts.shift();
    }
    if (parts.some((p) => p === '' || p.startsWith('.'))) continue;
    out.push({ path: parts.join('/'), file });
  }
  out.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  return { folder, files: out };
}

/** The entry a folder opens with by default. */
export function defaultEntry(paths: string[]): string {
  for (const name of ENTRY_PREFERENCE) {
    if (paths.includes(name)) return name;
  }
  return paths.find((p) => !p.includes('/')) ?? paths[0] ?? '';
}

@customElement('scion-artifact-publish-dialog')
export class ScionArtifactPublishDialog extends LitElement {
  /** Home project of a new artifact. */
  @property({ type: String }) projectId = '';
  /** Set to publish a new version of this artifact. */
  @property({ type: String }) artifactId = '';
  @property({ type: Boolean, reflect: true }) open = false;

  @state() private mode: 'file' | 'folder' = 'file';
  @state() private picked: PickedFile[] = [];
  @state() private folderName = '';
  @state() private entry = '';
  @state() private titleValue = '';
  @state() private key = '';
  @state() private note = '';
  @state() private busy = false;
  @state() private progress = '';
  @state() private error: string | null = null;
  @state() private dragOver = false;
  /** The version a failed attempt left pending; the next attempt resumes it. */
  private pending: PendingPublish | null = null;

  @query('#file-input') private fileInput?: HTMLInputElement;
  @query('#folder-input') private folderInput?: HTMLInputElement;

  static override styles = css`
    sl-dialog::part(panel) {
      width: min(560px, 95vw);
    }
    .mode {
      margin-bottom: 0.75rem;
    }
    .drop {
      border: 2px dashed var(--scion-border, #cbd5e1);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 1.25rem;
      text-align: center;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.75rem;
    }
    .drop.over {
      border-color: var(--sl-color-primary-500, #3b82f6);
      background: var(--sl-color-primary-50, #eff6ff);
    }
    .drop sl-icon {
      font-size: 1.5rem;
      display: block;
      margin: 0 auto 0.25rem;
    }
    .picked {
      font-size: 0.8125rem;
      background: var(--scion-bg-subtle, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      padding: 0.4rem 0.6rem;
      margin-bottom: 0.75rem;
    }
    .fields {
      display: flex;
      flex-direction: column;
      gap: 0.6rem;
    }
    .progress {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.5rem;
    }
    sl-alert {
      margin-top: 0.75rem;
    }
    input[type='file'] {
      display: none;
    }
  `;

  /** Clears the form; called when the dialog opens. */
  reset(): void {
    this.mode = 'file';
    this.picked = [];
    this.folderName = '';
    this.entry = '';
    this.titleValue = '';
    this.key = '';
    this.note = '';
    this.busy = false;
    this.progress = '';
    this.error = null;
    this.pending = null;
    // Clear the pickers so choosing the same file again fires change.
    if (this.fileInput) this.fileInput.value = '';
    if (this.folderInput) this.folderInput.value = '';
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('open') && this.open && !changed.get('open')) {
      this.reset();
    }
  }

  /** Closes the dialog and tells the parent, which owns the open flag. */
  private close(): void {
    this.open = false;
    this.dispatchEvent(
      new CustomEvent('artifact-publish-closed', { bubbles: true, composed: true })
    );
  }

  private get isVersion(): boolean {
    return this.artifactId !== '';
  }

  private setMode(mode: 'file' | 'folder'): void {
    if (mode === this.mode) return;
    this.mode = mode;
    this.picked = [];
    this.folderName = '';
    this.entry = '';
  }

  private pickFile(file: File): void {
    this.picked = [{ path: file.name, file }];
    this.entry = file.name;
    if (!this.titleValue && !this.isVersion) this.titleValue = file.name;
  }

  private onFileInput = (e: Event): void => {
    const file = (e.target as HTMLInputElement).files?.[0];
    if (file) this.pickFile(file);
  };

  private onFolderInput = (e: Event): void => {
    const list = Array.from((e.target as HTMLInputElement).files ?? []);
    const { folder, files } = folderFiles(list);
    this.picked = files;
    this.folderName = folder;
    this.entry = defaultEntry(files.map((f) => f.path));
    if (!this.titleValue && !this.isVersion) this.titleValue = folder;
  };

  private onDrop = (e: DragEvent): void => {
    e.preventDefault();
    this.dragOver = false;
    if (this.mode !== 'file') return;
    const file = e.dataTransfer?.files?.[0];
    if (file) this.pickFile(file);
  };

  private get canPublish(): boolean {
    return (
      !this.busy &&
      this.picked.length > 0 &&
      this.entry !== '' &&
      (this.isVersion || this.titleValue.trim() !== '')
    );
  }

  private async publish(): Promise<void> {
    if (!this.canPublish) return;
    this.busy = true;
    this.error = null;
    this.progress = 'Preparing…';
    try {
      const files: PublishFile[] = this.picked.map((p) => ({ path: p.path, data: p.file }));
      const res: ArtifactResponse = await publishFiles({
        artifactId: this.artifactId || undefined,
        scope: this.projectId || undefined,
        title: this.isVersion ? undefined : this.titleValue.trim(),
        key: this.isVersion ? undefined : this.key.trim() || undefined,
        note: this.note.trim() || undefined,
        entry: this.entry,
        files,
        onProgress: (done, total, path) => {
          this.progress = `Uploaded ${done} of ${total}: ${path}`;
        },
        resume: this.pending,
      });
      this.pending = null;
      this.progress = '';
      this.close();
      this.dispatchEvent(
        new CustomEvent<ArtifactResponse>('artifact-published', {
          detail: res,
          bubbles: true,
          composed: true,
        })
      );
    } catch (err) {
      this.error = publishErrorMessage(err);
      if (err instanceof PublishError) this.pending = err.pending;
      this.progress = '';
    } finally {
      this.busy = false;
    }
  }

  private renderPicked(): TemplateResult | typeof nothing {
    if (this.picked.length === 0) return nothing;
    if (this.mode === 'file') {
      const f = this.picked[0];
      return html`<div class="picked">${f.path} · ${formatBytes(f.file.size)}</div>`;
    }
    const total = this.picked.reduce((n, p) => n + p.file.size, 0);
    return html`
      <div class="picked">
        ${this.folderName || 'Folder'}/ · ${this.picked.length}
        ${this.picked.length === 1 ? 'file' : 'files'} · ${formatBytes(total)} (hidden files
        skipped)
      </div>
      <sl-select
        label="Entry file"
        size="small"
        .value=${String(this.picked.findIndex((p) => p.path === this.entry))}
        @sl-change=${(e: Event): void => {
          const i = Number((e.target as HTMLSelectElement).value);
          this.entry = this.picked[i]?.path ?? this.entry;
        }}
      >
        ${this.picked.map((p, i) => html`<sl-option value=${String(i)}>${p.path}</sl-option>`)}
      </sl-select>
    `;
  }

  override render(): TemplateResult {
    const heading = this.isVersion ? 'Upload new version' : 'New artifact';
    return html`
      <sl-dialog
        label=${heading}
        ?open=${this.open}
        @sl-request-close=${(e: Event): void => {
          if (this.busy) e.preventDefault();
        }}
        @sl-after-hide=${(e: Event): void => {
          if (e.target === e.currentTarget && this.open) this.close();
        }}
      >
        <sl-radio-group
          class="mode"
          size="small"
          value=${this.mode}
          @sl-change=${(e: Event): void =>
            this.setMode((e.target as HTMLInputElement).value as 'file' | 'folder')}
        >
          <sl-radio-button value="file">File</sl-radio-button>
          <sl-radio-button value="folder">Folder</sl-radio-button>
        </sl-radio-group>

        <div
          class="drop ${this.dragOver ? 'over' : ''}"
          @dragover=${(e: DragEvent): void => {
            e.preventDefault();
            this.dragOver = this.mode === 'file';
          }}
          @dragleave=${(): void => {
            this.dragOver = false;
          }}
          @drop=${this.onDrop}
        >
          <sl-icon name="upload"></sl-icon>
          ${this.mode === 'file'
            ? html`Drop a file here, or
                <sl-button variant="text" size="small" @click=${(): void => this.fileInput?.click()}
                  >choose a file</sl-button
                >`
            : html`<sl-button
                variant="text"
                size="small"
                @click=${(): void => this.folderInput?.click()}
                >Choose a folder</sl-button
              >`}
        </div>
        <input id="file-input" type="file" @change=${this.onFileInput} />
        <input
          id="folder-input"
          type="file"
          webkitdirectory
          multiple
          @change=${this.onFolderInput}
        />
        ${this.renderPicked()}

        <div class="fields">
          ${this.isVersion
            ? nothing
            : html`
                <sl-input
                  label="Title"
                  size="small"
                  required
                  .value=${this.titleValue}
                  @sl-input=${(e: Event): void => {
                    this.titleValue = (e.target as HTMLInputElement).value;
                  }}
                ></sl-input>
                <sl-input
                  label="Key (optional)"
                  help-text="Publishing again under the same key adds a version."
                  size="small"
                  .value=${this.key}
                  @sl-input=${(e: Event): void => {
                    this.key = (e.target as HTMLInputElement).value;
                  }}
                ></sl-input>
              `}
          <sl-input
            label="Note (optional)"
            placeholder="Describe this version"
            size="small"
            .value=${this.note}
            @sl-input=${(e: Event): void => {
              this.note = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>
        </div>
        ${this.progress ? html`<div class="progress">${this.progress}</div>` : nothing}
        ${this.error
          ? html`<sl-alert variant="danger" open>
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              ${this.error}
            </sl-alert>`
          : nothing}

        <sl-button slot="footer" ?disabled=${this.busy} @click=${(): void => this.close()}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?disabled=${!this.canPublish}
          ?loading=${this.busy}
          @click=${(): void => void this.publish()}
          >Publish</sl-button
        >
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-artifact-publish-dialog': ScionArtifactPublishDialog;
  }
}
