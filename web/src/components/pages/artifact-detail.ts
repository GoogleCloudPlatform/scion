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
 * Artifact page (experiment hub.artifacts)
 *
 * Shows an artifact's title, owner and versions, and renders the entry of
 * the version shown: markdown in a sandboxed preview frame
 * (<scion-artifact-markdown-frame>), text read-only in <scion-code-editor>,
 * raster images with <img>, and HTML bundles in a sandboxed frame served
 * under a short-lived view capability. Other types are offered as a
 * download. Tabs list the version's files and the artifact's versions;
 * Edit publishes a changed entry as a new version and Upload new version
 * publishes new files.
 * Route: /projects/{projectId}/artifacts/{artifactId}[/v/{seq}]
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, query, state } from 'lit/decorators.js';

import type { PageData } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { navigateTo, stripBasePath } from '../../client/navigation.js';
import { isFeatureEnabled } from '../../utils/feature-flags.js';
import { formatInstant } from '../../utils/time.js';
import {
  ARTIFACTS_FLAG,
  MAX_INLINE_TEXT_BYTES,
  artifactFileUrl,
  artifactPagePath,
  baseName,
  formatBytes,
  isInlineType,
  isRemoteFile,
  listVersions,
  mintView,
  parseArtifactPagePath,
  PublishError,
  publishErrorMessage,
  publishFiles,
  rendererFor,
} from '../../client/artifacts.js';
import type {
  ArtifactFile,
  ArtifactResponse,
  ArtifactRenderer,
  ArtifactVersion,
  PendingPublish,
  PublishFile,
  ViewResponse,
} from '../../client/artifacts.js';
import { principalLabel, principalName } from '../../client/principal-names.js';
import { getLanguageFromPath } from '../shared/code-editor.js';
import '../shared/artifact-markdown-frame.js';
import '../shared/artifact-publish-dialog.js';
import '../shared/code-editor.js';
import './not-found.js';

type Tab = 'preview' | 'files' | 'history';

/** Longest delay setTimeout honours. */
const MAX_TIMER_MS = 2 ** 31 - 1;

@customElement('scion-page-artifact-detail')
export class ScionPageArtifactDetail extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @state() private projectId = '';
  @state() private artifactId = '';
  /** Version in the URL; 0 shows the current version. */
  @state() private seq = 0;
  @state() private loading = true;
  @state() private notFound = false;
  @state() private error: string | null = null;
  @state() private data: ArtifactResponse | null = null;
  @state() private entry: ArtifactFile | null = null;
  @state() private text: string | null = null;
  /** Owner and publisher display names by "kind:id"; missing while unknown. */
  @state() private names = new Map<string, string>();
  @state() private copied = false;
  @state() private tab: Tab = 'preview';
  @state() private versions: ArtifactVersion[] = [];
  @state() private versionsNext = 0;
  @state() private versionsError: string | null = null;
  @state() private view: ViewResponse | null = null;
  @state() private viewError: string | null = null;
  @state() private viewExpired = false;
  private viewTimer: ReturnType<typeof setTimeout> | null = null;
  /**
   * Generations of the page load and of the view mint. An answer that
   * arrives after a newer load or mint started is dropped, so a slow
   * earlier request cannot overwrite a newer result.
   */
  private loadGen = 0;
  private viewGen = 0;
  @state() private editing = false;
  @state() private editText = '';
  @state() private editNote = '';
  @state() private editBusy = false;
  @state() private editError: string | null = null;
  @state() private publishOpen = false;
  /** The version a failed Edit publish left pending; the next attempt resumes it. */
  private editPending: PendingPublish | null = null;

  @query('.untrusted') private untrustedFrame?: HTMLElement;

  static override styles = css`
    :host {
      display: block;
      padding: 1.5rem;
      max-width: 1200px;
      margin: 0 auto;
    }
    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.35rem;
      color: var(--sl-color-neutral-600);
      text-decoration: none;
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }
    .back-link:hover {
      color: var(--sl-color-primary-600);
    }
    .header {
      display: flex;
      justify-content: space-between;
      align-items: flex-start;
      gap: 1rem;
      flex-wrap: wrap;
      margin-bottom: 1rem;
    }
    .title {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin: 0 0 0.5rem;
    }
    .title h1 {
      margin: 0;
      font-size: 1.5rem;
      font-weight: 600;
      word-break: break-word;
    }
    .title sl-icon {
      font-size: 1.25rem;
      color: var(--sl-color-neutral-500);
    }
    .meta {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.5rem 1.25rem;
      font-size: 0.8125rem;
      color: var(--sl-color-neutral-600);
    }
    .meta code,
    .ref code {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.75rem;
    }
    .ref {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
    }
    .actions {
      display: flex;
      gap: 0.5rem;
      align-items: center;
      flex-wrap: wrap;
    }
    .menu-note {
      display: block;
      font-size: 0.75rem;
      color: var(--sl-color-neutral-500);
      max-width: 22rem;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }
    .entry-bar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 1rem;
      margin-bottom: 0.5rem;
      font-size: 0.8125rem;
      color: var(--sl-color-neutral-600);
    }
    .entry-bar .buttons {
      display: flex;
      gap: 0.5rem;
    }
    .image-frame {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 1rem;
      background: var(--scion-surface, #ffffff);
      text-align: center;
    }
    .image-frame img {
      max-width: 100%;
      height: auto;
    }
    .notice {
      margin-bottom: 0.75rem;
    }
    .untrusted {
      border: 2px solid var(--sl-color-warning-500, #f59e0b);
      border-radius: var(--scion-radius, 0.5rem);
      overflow: hidden;
      background: var(--scion-surface, #ffffff);
      display: flex;
      flex-direction: column;
    }
    .untrusted-bar {
      display: flex;
      align-items: center;
      gap: 0.4rem;
      padding: 0.3rem 0.75rem;
      font-size: 0.75rem;
      color: var(--sl-color-warning-800, #92400e);
      background: var(--sl-color-warning-50, #fffbeb);
      border-bottom: 1px solid var(--sl-color-warning-300, #fcd34d);
    }
    .untrusted iframe {
      display: block;
      width: 100%;
      height: 70vh;
      border: none;
      background: #ffffff;
    }
    .untrusted:fullscreen iframe {
      height: 100%;
      flex: 1;
    }
    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 0.875rem;
    }
    th {
      text-align: left;
      font-weight: 600;
      font-size: 0.75rem;
      text-transform: uppercase;
      letter-spacing: 0.03em;
      color: var(--sl-color-neutral-600);
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }
    td {
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }
    td.muted,
    .muted {
      color: var(--sl-color-neutral-600);
    }
    td.actions-cell {
      text-align: right;
      white-space: nowrap;
    }
    .file-path {
      display: inline-flex;
      align-items: center;
      gap: 0.4rem;
      word-break: break-all;
    }
    .summary {
      font-size: 0.8125rem;
      color: var(--sl-color-neutral-600);
      margin: 0.5rem 0;
    }
    .single-version {
      text-align: center;
      padding: 1.5rem;
      color: var(--sl-color-neutral-600);
    }
    .single-version .buttons {
      display: flex;
      gap: 0.5rem;
      justify-content: center;
      margin-top: 0.5rem;
    }
    .more {
      text-align: center;
      margin-top: 0.75rem;
    }
    .edit-bar {
      font-size: 0.8125rem;
      color: var(--sl-color-neutral-600);
      margin-bottom: 0.5rem;
    }
    .edit-footer {
      display: flex;
      gap: 0.5rem;
      align-items: flex-end;
      margin-top: 0.75rem;
    }
    .edit-footer sl-input {
      flex: 1;
    }
    sl-tab-group {
      margin-bottom: 0.5rem;
    }
    sl-alert {
      margin-bottom: 0.75rem;
    }
    .download-state,
    .error-state,
    .loading-state {
      text-align: center;
      padding: 3rem;
      color: var(--sl-color-neutral-500);
    }
    .error-state sl-icon {
      font-size: 2rem;
      color: var(--sl-color-danger-500);
      margin-bottom: 0.5rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    const parsed = parseArtifactPagePath(this.pageData?.path || window.location.pathname);
    if (parsed) {
      this.projectId = parsed.projectId;
      this.artifactId = parsed.id;
      this.seq = parsed.seq;
    }
    if (!isFeatureEnabled(ARTIFACTS_FLAG) || !this.artifactId) {
      this.loading = false;
      this.notFound = true;
      return;
    }
    void this.load();
  }

  private get versionPath(): string {
    const id = `/api/v1/artifacts/${encodeURIComponent(this.artifactId)}`;
    return this.seq > 0 ? `${id}/versions/${this.seq}` : id;
  }

  private async load(): Promise<void> {
    const gen = ++this.loadGen;
    this.viewGen++;
    this.loading = true;
    this.error = null;
    this.notFound = false;
    this.text = null;
    this.view = null;
    this.viewError = null;
    this.viewExpired = false;
    if (this.viewTimer) clearTimeout(this.viewTimer);
    this.viewTimer = null;
    try {
      const res = await apiFetch(this.versionPath);
      if (gen !== this.loadGen) return;
      if (res.status === 404) {
        this.notFound = true;
        return;
      }
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      const data = (await res.json()) as ArtifactResponse;
      if (gen !== this.loadGen) return;
      this.data = data;
      dispatchPageTitle(this, data.artifact.title, 'Artifacts');
      const version = data.version;
      this.entry = version?.files.find((f) => f.path === version.entryPath) ?? null;
      this.resolveNames();
      void this.loadVersions(0, gen);
      const kind = this.entry ? rendererFor(this.entry.mediaType) : 'download';
      if ((kind === 'markdown' || kind === 'text') && this.entry) {
        if (this.entry.size <= MAX_INLINE_TEXT_BYTES) {
          await this.loadText(this.entry, gen);
        }
      }
      if (kind === 'html' && version && gen === this.loadGen) {
        void this.loadView(version.seq);
      }
    } catch (err) {
      if (gen !== this.loadGen) return;
      console.error('Failed to load artifact:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load artifact';
    } finally {
      if (gen === this.loadGen) this.loading = false;
    }
  }

  private async loadText(file: ArtifactFile, gen: number): Promise<void> {
    const seq = this.data?.version?.seq ?? 0;
    const res = await apiFetch(artifactFileUrl(this.artifactId, seq, file.path, true));
    if (!res.ok) {
      throw new Error(await extractApiError(res, `HTTP ${res.status}`));
    }
    const text = await res.text();
    if (gen === this.loadGen) this.text = text;
  }

  private async loadView(seq: number): Promise<void> {
    const gen = ++this.viewGen;
    if (this.viewTimer) clearTimeout(this.viewTimer);
    this.viewTimer = null;
    try {
      const view = await mintView(this.artifactId, seq);
      if (gen !== this.viewGen) return;
      this.view = view;
      this.viewExpired = false;
      this.viewError = null;
      // The view URL stops working when it expires: say so then, and mint a
      // new one only when the reader asks, so a page in use is not reloaded.
      const left = Date.parse(view.expiresAt) - Date.now();
      // setTimeout fires at once for delays over 2^31-1 ms; views last far
      // less than that, so a longer delay is simply not scheduled.
      if (Number.isFinite(left) && left <= MAX_TIMER_MS) {
        this.viewTimer = setTimeout(
          () => {
            this.viewExpired = true;
          },
          Math.max(0, left)
        );
      }
    } catch (err) {
      if (gen !== this.viewGen) return;
      this.viewError = err instanceof Error ? err.message : 'Could not open the view';
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.viewTimer) clearTimeout(this.viewTimer);
    this.viewTimer = null;
  }

  private async loadVersions(before = 0, gen = this.loadGen): Promise<void> {
    try {
      const page = await listVersions(this.artifactId, before);
      if (gen !== this.loadGen) return;
      this.versions = before > 0 ? [...this.versions, ...page.versions] : page.versions;
      this.versionsNext = page.nextBefore ?? 0;
      this.resolveNames();
      this.versionsError = null;
    } catch (err) {
      if (gen !== this.loadGen) return;
      this.versionsError = err instanceof Error ? err.message : 'Could not load versions';
    }
  }

  /** Looks up the names of the owner and of the versions' publishers. */
  private resolveNames(): void {
    const refs: [string, string][] = [];
    const a = this.data?.artifact;
    if (a) refs.push([a.ownerKind, a.ownerRef]);
    for (const v of this.versions) {
      if (v.createdByKind && v.createdByRef) refs.push([v.createdByKind, v.createdByRef]);
    }
    const me = this.pageData?.user?.id;
    for (const [kind, ref] of refs) {
      const key = `${kind}:${ref}`;
      if (this.names.has(key) || (kind === 'user' && ref === me)) continue;
      void principalName(kind, ref).then((name) => {
        if (name && this.isConnected && !this.names.has(key)) {
          this.names = new Map(this.names).set(key, name);
        }
      });
    }
  }

  /** How a principal is shown: "You", "<name> (agent)", a name, or a short id. */
  private label(kind: string, ref: string): string {
    return principalLabel(
      kind,
      ref,
      this.names.get(`${kind}:${ref}`) ?? '',
      this.pageData?.user?.id
    );
  }

  private async copyRef(): Promise<void> {
    const ref = this.data?.artifact.ref;
    if (!ref) return;
    try {
      await navigator.clipboard.writeText(ref);
      this.copied = true;
      setTimeout(() => (this.copied = false), 1500);
    } catch {
      // Clipboard unavailable; the ref is visible on the page.
    }
  }

  private get homeProject(): string {
    return this.data?.artifact.scopeRef || this.projectId;
  }

  private goToVersion(seq: number): void {
    const current = this.data?.artifact.currentSeq ?? 0;
    navigateTo(
      artifactPagePath(
        { id: this.artifactId, scopeRef: this.homeProject },
        seq === current ? 0 : seq
      )
    );
  }

  /** The version shown is the current one, so a new version can build on it. */
  private get showsCurrent(): boolean {
    const v = this.data?.version;
    return !!v && v.seq === this.data!.artifact.currentSeq;
  }

  private get canEdit(): boolean {
    const f = this.entry;
    if (!f || !this.showsCurrent || this.text === null) return false;
    const kind = rendererFor(f.mediaType);
    return (kind === 'markdown' || kind === 'text') && f.size <= MAX_INLINE_TEXT_BYTES;
  }

  private startEdit = (): void => {
    this.editText = this.text ?? '';
    this.editNote = '';
    this.editError = null;
    this.editing = true;
  };

  private async publishEdit(): Promise<void> {
    const v = this.data?.version;
    const f = this.entry;
    if (!v || !f) return;
    this.editBusy = true;
    this.editError = null;
    try {
      const files: PublishFile[] = v.files
        .filter((x) => !isRemoteFile(x))
        .map((x) =>
          x.path === f.path
            ? { path: x.path, data: new Blob([this.editText], { type: f.mediaType }) }
            : { path: x.path, size: x.size, sha256: x.sha256 }
        );
      await publishFiles({
        artifactId: this.artifactId,
        entry: v.entryPath,
        note: this.editNote.trim() || undefined,
        files,
        resume: this.editPending,
      });
      this.editPending = null;
      this.showCurrentVersion();
    } catch (err) {
      this.editError = publishErrorMessage(err);
      if (err instanceof PublishError) this.editPending = err.pending;
    } finally {
      this.editBusy = false;
    }
  }

  private onPublished = (): void => {
    this.publishOpen = false;
    this.showCurrentVersion();
  };

  /**
   * Shows the artifact's current version after a publish. The router does
   * nothing when the target is the page already shown, so in that case the
   * page reloads itself.
   */
  private showCurrentVersion(): void {
    const target = artifactPagePath({ id: this.artifactId, scopeRef: this.homeProject });
    this.editing = false;
    if (stripBasePath(window.location.pathname) !== target) {
      navigateTo(target);
      return;
    }
    this.seq = 0;
    this.tab = 'preview';
    this.versions = [];
    this.versionsNext = 0;
    void this.load();
  }

  override render(): TemplateResult | typeof nothing {
    if (this.loading) {
      return html`<div class="loading-state"><sl-spinner></sl-spinner></div>`;
    }
    if (this.notFound) {
      return html`<scion-page-404></scion-page-404>`;
    }
    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <p>${this.error}</p>
          <sl-button size="small" @click=${(): void => void this.load()}>Retry</sl-button>
        </div>
      `;
    }
    if (!this.data) return nothing;
    return html`
      <a href=${`/projects/${encodeURIComponent(this.homeProject)}`} class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Project
      </a>
      ${this.renderHeader()} ${this.editing ? this.renderEditor() : this.renderTabs()}
      <scion-artifact-publish-dialog
        .artifactId=${this.artifactId}
        ?open=${this.publishOpen}
        @artifact-published=${this.onPublished}
        @artifact-publish-closed=${(): void => {
          this.publishOpen = false;
        }}
      ></scion-artifact-publish-dialog>
    `;
  }

  private versionLabel(v: ArtifactVersion | undefined): string {
    if (!v) return 'none';
    return v.seq === this.data!.artifact.currentSeq ? `v${v.seq} (current)` : `v${v.seq}`;
  }

  private renderVersionMenu(): TemplateResult {
    const v = this.data!.version;
    return html`
      <sl-dropdown>
        <sl-button slot="trigger" size="small" caret> Version ${this.versionLabel(v)} </sl-button>
        <sl-menu
          @sl-select=${(e: CustomEvent<{ item: { value: string } }>): void =>
            this.goToVersion(Number(e.detail.item.value))}
        >
          ${this.versionsError
            ? html`<sl-menu-item disabled>${this.versionsError}</sl-menu-item>`
            : this.versions.map(
                (x) => html`
                  <sl-menu-item value=${String(x.seq)} ?checked=${x.seq === v?.seq} type="checkbox">
                    ${this.versionLabel(x)} · ${formatInstant(x.createdAt)}
                    ${x.note ? html`<span class="menu-note">${x.note}</span>` : nothing}
                  </sl-menu-item>
                `
              )}
        </sl-menu>
      </sl-dropdown>
    `;
  }

  private renderHeader(): TemplateResult {
    const { artifact: a, version: v } = this.data!;
    const owner = this.label(a.ownerKind, a.ownerRef);
    const f = this.entry;
    const ownFiles = v ? v.files.filter((x) => !isRemoteFile(x)) : [];
    return html`
      <div class="header">
        <div>
          <div class="title">
            <sl-icon name="file-earmark-richtext"></sl-icon>
            <h1>${a.title}</h1>
          </div>
          <div class="meta">
            <span>Owner: ${owner}</span>
            ${a.key ? html`<span>Key: <code>${a.key}</code></span>` : nothing}
            <span>Updated: ${formatInstant(a.updatedAt)}</span>
            <span class="ref">
              <code>${v ? v.ref : a.ref}</code>
              <sl-tooltip content=${this.copied ? 'Copied' : 'Copy reference'}>
                <sl-icon-button
                  name="clipboard"
                  label="Copy reference"
                  @click=${(): void => void this.copyRef()}
                ></sl-icon-button>
              </sl-tooltip>
            </span>
          </div>
        </div>
        ${this.editing
          ? nothing
          : html`
              <div class="actions">
                ${v ? this.renderVersionMenu() : nothing}
                ${this.canEdit
                  ? html`<sl-button size="small" @click=${this.startEdit}>
                      <sl-icon slot="prefix" name="pencil"></sl-icon>
                      Edit
                    </sl-button>`
                  : nothing}
                ${v && f && ownFiles.length === 1
                  ? html`<sl-button
                      size="small"
                      href=${artifactFileUrl(this.artifactId, v.seq, f.path)}
                      download=${baseName(f.path)}
                    >
                      <sl-icon slot="prefix" name="download"></sl-icon>
                      Download
                    </sl-button>`
                  : nothing}
              </div>
            `}
      </div>
    `;
  }

  private renderTabs(): TemplateResult {
    return html`
      <sl-tab-group
        @sl-tab-show=${(e: CustomEvent<{ name: string }>): void => {
          this.tab = e.detail.name as Tab;
        }}
      >
        <sl-tab slot="nav" panel="preview" ?active=${this.tab === 'preview'}>Preview</sl-tab>
        <sl-tab slot="nav" panel="files" ?active=${this.tab === 'files'}>Files</sl-tab>
        <sl-tab slot="nav" panel="history" ?active=${this.tab === 'history'}>History</sl-tab>
        <sl-tab-panel name="preview">${this.renderEntry()}</sl-tab-panel>
        <sl-tab-panel name="files">${this.renderFiles()}</sl-tab-panel>
        <sl-tab-panel name="history">${this.renderHistory()}</sl-tab-panel>
      </sl-tab-group>
    `;
  }

  private entryAction(f: ArtifactFile, seq: number): TemplateResult {
    const href = artifactFileUrl(this.artifactId, seq, f.path);
    // Inline types open in a new tab (on an object-storage hub the
    // redirect is cross-origin, so download= would be ignored anyway);
    // attachment types download under their base name.
    return isInlineType(f.mediaType)
      ? html`<sl-button size="small" href=${href} target="_blank" rel="noopener noreferrer">
          <sl-icon slot="prefix" name="box-arrow-up-right"></sl-icon>
          Open raw
        </sl-button>`
      : html`<sl-button size="small" href=${href} download=${baseName(f.path)}>
          <sl-icon slot="prefix" name="download"></sl-icon>
          Download
        </sl-button>`;
  }

  private renderEntry(): TemplateResult {
    const v = this.data?.version;
    const f = this.entry;
    if (!v || !f) {
      return html`<div class="download-state">This artifact has no published content.</div>`;
    }
    const kind: ArtifactRenderer = rendererFor(f.mediaType);
    if (kind === 'html') {
      return this.renderHtmlView(v, f);
    }
    const href = artifactFileUrl(this.artifactId, v.seq, f.path);
    const inline = isInlineType(f.mediaType);
    const bar = html`
      <div class="entry-bar">
        <span>${f.path} · ${formatBytes(f.size)}</span>
        ${this.entryAction(f, v.seq)}
      </div>
    `;
    if (kind === 'image') {
      return html`${bar}
        <div class="image-frame"><img src=${href} alt=${this.data!.artifact.title} /></div>`;
    }
    if ((kind === 'markdown' || kind === 'text') && this.text !== null) {
      return kind === 'markdown'
        ? html`${bar}<scion-artifact-markdown-frame
              .content=${this.text}
              .artifactId=${this.artifactId}
              .seq=${v.seq}
              .entryPath=${f.path}
              .files=${v.files}
            ></scion-artifact-markdown-frame>`
        : html`${bar}<scion-code-editor
              .content=${this.text}
              .language=${getLanguageFromPath(f.path)}
              readonly
            ></scion-code-editor>`;
    }
    const tooLarge = (kind === 'markdown' || kind === 'text') && f.size > MAX_INLINE_TEXT_BYTES;
    return html`${bar}
      <div class="download-state">
        <sl-icon name="file-earmark-text" style="font-size: 2rem;"></sl-icon>
        <p>
          ${tooLarge
            ? `This file is too large to show here (${formatBytes(f.size)}). Use ${inline ? 'Open raw' : 'Download'} to open it.`
            : 'This file type is not shown in the browser. Use Download to open it.'}
        </p>
      </div>`;
  }

  private fullScreen = (): void => {
    void this.untrustedFrame?.requestFullscreen?.();
  };

  private renderHtmlView(v: ArtifactVersion, f: ArtifactFile): TemplateResult {
    const count = v.files.filter((x) => !isRemoteFile(x)).length;
    const owner = this.label(this.data!.artifact.ownerKind, this.data!.artifact.ownerRef);
    if (this.viewError) {
      return html`<div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <p>${this.viewError}</p>
        <sl-button size="small" @click=${(): void => void this.loadView(v.seq)}>Retry</sl-button>
      </div>`;
    }
    if (!this.view) {
      return html`<div class="loading-state"><sl-spinner></sl-spinner></div>`;
    }
    return html`
      ${this.view.remoteImages
        ? html`<sl-alert class="notice" variant="warning" open>
            <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
            Remote images are not loaded in HTML artifacts; include them in the bundle.
          </sl-alert>`
        : nothing}
      ${this.viewExpired
        ? html`<sl-alert class="notice expired" variant="neutral" open>
            <sl-icon slot="icon" name="clock-history"></sl-icon>
            This view has expired.
            <sl-button size="small" @click=${(): void => void this.loadView(v.seq)}
              >Reload view</sl-button
            >
          </sl-alert>`
        : nothing}
      <div class="entry-bar">
        <span>${f.path} · ${count === 1 ? '1 file' : `bundle of ${count} files`}</span>
        ${this.viewExpired
          ? nothing
          : html`<span class="buttons">
              <sl-button size="small" @click=${this.fullScreen}>
                <sl-icon slot="prefix" name="arrows-fullscreen"></sl-icon>
                Full screen
              </sl-button>
              <sl-button
                size="small"
                href=${this.view.url}
                target="_blank"
                rel="noopener noreferrer"
              >
                <sl-icon slot="prefix" name="box-arrow-up-right"></sl-icon>
                Open in new tab
              </sl-button>
            </span>`}
      </div>
      <div class="untrusted">
        <div class="untrusted-bar">
          <sl-icon name="shield-lock"></sl-icon>
          Content published by ${owner} · runs sandboxed, isolated from Scion
        </div>
        <iframe
          title=${`${this.data!.artifact.title} (sandboxed)`}
          sandbox="allow-scripts"
          referrerpolicy="no-referrer"
          src=${this.view.url}
        ></iframe>
      </div>
    `;
  }

  private renderFiles(): TemplateResult {
    const v = this.data?.version;
    if (!v) return html`<div class="download-state">This artifact has no published content.</div>`;
    const own = v.files.filter((x) => !isRemoteFile(x));
    const remote = v.files.filter(isRemoteFile);
    const fetched = remote.filter((x) => x.fetchStatus === 'ok').length;
    const total = own.reduce((n, x) => n + x.size, 0);
    return html`
      <div class="summary">
        ${own.length} ${own.length === 1 ? 'file' : 'files'} · ${formatBytes(total)} · version
        ${v.seq}
        ${remote.length > 0
          ? html` · remote images fetched when published: ${fetched}
            ${remote.length > fetched
              ? html`, could not be fetched: ${remote.length - fetched}`
              : nothing}`
          : nothing}
      </div>
      <table>
        <thead>
          <tr>
            <th>Path</th>
            <th>Size</th>
            <th>Type</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          ${own.map((x) => {
            const href = artifactFileUrl(this.artifactId, v.seq, x.path);
            return html`
              <tr>
                <td>
                  <span class="file-path">
                    <sl-icon name="file-earmark"></sl-icon>
                    ${x.path}
                    ${x.path === v.entryPath
                      ? html`<sl-badge pill variant="primary">entry</sl-badge>`
                      : nothing}
                  </span>
                </td>
                <td class="muted">${formatBytes(x.size)}</td>
                <td class="muted">${x.mediaType}</td>
                <td class="actions-cell">
                  ${isInlineType(x.mediaType)
                    ? html`<sl-icon-button
                        name="eye"
                        label=${`Open ${x.path}`}
                        href=${href}
                        target="_blank"
                      ></sl-icon-button>`
                    : html`<sl-icon-button
                        name="eye"
                        label="Not shown in the browser"
                        disabled
                      ></sl-icon-button>`}
                  <sl-icon-button
                    name="download"
                    label=${`Download ${x.path}`}
                    href=${href}
                    download=${baseName(x.path)}
                  ></sl-icon-button>
                </td>
              </tr>
            `;
          })}
        </tbody>
      </table>
    `;
  }

  private createdBy(v: ArtifactVersion): string {
    if (!v.createdByKind) return '';
    return this.label(v.createdByKind, v.createdByRef ?? '');
  }

  private renderHistory(): TemplateResult {
    if (this.versionsError) {
      return html`<div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <p>${this.versionsError}</p>
        <sl-button size="small" @click=${(): void => void this.loadVersions()}>Retry</sl-button>
      </div>`;
    }
    const shown = this.data?.version?.seq;
    return html`
      <table>
        <thead>
          <tr>
            <th>Version</th>
            <th>Published by</th>
            <th>When</th>
            <th>Note</th>
            <th>Files</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          ${this.versions.map(
            (x) => html`
              <tr>
                <td>
                  <strong>v${x.seq}</strong>
                  ${x.seq === this.data!.artifact.currentSeq
                    ? html`<sl-badge pill variant="success">current</sl-badge>`
                    : nothing}
                  ${x.kind !== 'publish'
                    ? html`<sl-badge pill variant="neutral">${x.kind}</sl-badge>`
                    : nothing}
                </td>
                <td>${this.createdBy(x)}</td>
                <td class="muted">${formatInstant(x.createdAt)}</td>
                <td>${x.note ?? ''}</td>
                <td class="muted">${x.fileCount}</td>
                <td class="actions-cell">
                  ${x.seq === shown
                    ? nothing
                    : html`<sl-icon-button
                        name="eye"
                        label=${`View v${x.seq}`}
                        @click=${(): void => this.goToVersion(x.seq)}
                      ></sl-icon-button>`}
                  ${x.fileCount === 1 && x.entryPath
                    ? html`<sl-icon-button
                        name="download"
                        label=${`Download v${x.seq}`}
                        href=${artifactFileUrl(this.artifactId, x.seq, x.entryPath)}
                        download=${baseName(x.entryPath)}
                      ></sl-icon-button>`
                    : nothing}
                </td>
              </tr>
            `
          )}
        </tbody>
      </table>
      ${this.versionsNext > 0
        ? html`<div class="more">
            <sl-button size="small" @click=${(): void => void this.loadVersions(this.versionsNext)}
              >Load more</sl-button
            >
          </div>`
        : nothing}
      ${this.versions.length === 1
        ? html`<div class="single-version">
            Only one version so far.
            <div class="buttons">
              <sl-button
                size="small"
                variant="primary"
                @click=${(): void => {
                  this.publishOpen = true;
                }}
              >
                <sl-icon slot="prefix" name="upload"></sl-icon>
                Upload new version
              </sl-button>
              ${this.canEdit
                ? html`<sl-button size="small" @click=${this.startEdit}>
                    <sl-icon slot="prefix" name="pencil"></sl-icon>
                    Edit
                  </sl-button>`
                : nothing}
            </div>
          </div>`
        : html`<div class="more">
            <sl-button
              size="small"
              @click=${(): void => {
                this.publishOpen = true;
              }}
            >
              <sl-icon slot="prefix" name="upload"></sl-icon>
              Upload new version
            </sl-button>
          </div>`}
    `;
  }

  private renderEditor(): TemplateResult {
    const v = this.data!.version!;
    const f = this.entry!;
    return html`
      <div class="edit-bar">
        Editing ${f.path}, based on v${v.seq}. Publishing saves a new version; v${v.seq} stays as it
        is.
      </div>
      <scion-code-editor
        .content=${this.editText}
        .language=${getLanguageFromPath(f.path)}
        @content-changed=${(e: CustomEvent<{ content: string }>): void => {
          this.editText = e.detail.content;
        }}
      ></scion-code-editor>
      ${this.editError
        ? html`<sl-alert variant="danger" open>
            <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
            ${this.editError}
          </sl-alert>`
        : nothing}
      <div class="edit-footer">
        <sl-input
          size="small"
          placeholder="Note for this version (optional)"
          .value=${this.editNote}
          @sl-input=${(e: Event): void => {
            this.editNote = (e.target as HTMLInputElement).value;
          }}
        ></sl-input>
        <sl-button
          size="small"
          ?disabled=${this.editBusy}
          @click=${(): void => {
            this.editing = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          size="small"
          variant="primary"
          ?loading=${this.editBusy}
          ?disabled=${this.editText === this.text}
          @click=${(): void => void this.publishEdit()}
          >Publish new version</sl-button
        >
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-artifact-detail': ScionPageArtifactDetail;
  }
}
