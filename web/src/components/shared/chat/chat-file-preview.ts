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
 * Reusable file/attachment preview dialog, shared by the thread's path-link
 * overlay and the message attachment overlay.
 *
 * A single `<scion-chat-file-preview>` instance renders an `sl-dialog` for
 * either an attachment (`GET /api/v1/chat/attachments/{id}`) or a resolved
 * container path (`buildFileApiUrl`), with one shared image/text/markdown/code
 * renderer for both. It owns its own loading/error/download state so callers
 * only ever set or clear `.target`.
 *
 * Each `target` change starts a new "generation" and a fresh
 * `AbortController`; a response for a superseded generation is discarded
 * without ever touching state — never rewritten into an error, since a
 * cancelled or superseded load is not a failure of the load the user is
 * currently waiting on. Object URLs created for image previews are revoked
 * on replacement, close and disconnect.
 * The image is reloaded after reconnect, as disconnect revokes its URL.
 */

import { LitElement, html, css, nothing, type PropertyValues } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch, extractApiError } from '../../../client/api.js';
import { getLanguageFromPath } from '../code-editor.js';
import {
  buildAttachmentApiUrl,
  buildFileApiUrl,
  buildGcsObjectApiUrl,
  buildCloudConsoleUrl,
  isImageFileName,
  isGcsImageContentType,
  isGcsImageExtension,
  isMarkdownFileName,
  isLikelyTextFileName,
  isLikelyTextMime,
  isLikelyBinaryFileName,
  baseMimeType,
  TEXT_PREVIEW_MAX_BYTES,
  type PathLinkTarget,
} from '../../../utils/chat-file-links.js';
import '../code-editor.js';
import '../markdown-preview.js';

/** An attachment target, addressed by its opaque attachment ID. */
export interface AttachmentPreviewTarget {
  kind: 'attachment';
  id: string;
  name: string;
  mime: string;
  size: number;
}

/** A resolved container-path target, addressed by project + parsed location. */
export interface PathPreviewTarget {
  kind: 'path';
  projectId: string;
  containerPath: string;
  location: PathLinkTarget;
  name: string;
}

/**
 * A gs:// object target, addressed by the message whose body the URI
 * appeared in (the hub derives the sender's SA from that message
 * server-side) plus the bucket/object parsed from the link.
 */
export interface GcsPreviewTarget {
  kind: 'gcs';
  messageId: string;
  bucket: string;
  object: string;
  name: string;
}

/** What `<scion-chat-file-preview>` renders: an attachment, a resolved path, or a gs:// object. */
export type PreviewTarget = AttachmentPreviewTarget | PathPreviewTarget | GcsPreviewTarget;

/** Image MIME types rendered inline (mirrors chat-message.ts's IMAGE_MIMES). */
const IMAGE_MIMES = new Set(['image/jpeg', 'image/png', 'image/gif', 'image/webp']);

interface LoadState {
  status: 'loading' | 'ready' | 'error';
  isImage: boolean;
  isMarkdown: boolean;
  isBinary: boolean;
  content?: string;
  objectUrl?: string;
  error?: string;
  /** The failed response's HTTP status, for a gcs target only. */
  errorStatus?: number;
  /** Show the "Open in Cloud Console" fallback for this error (every gcs error except 400). */
  showConsoleLink?: boolean;
  /**
   * Overrides the generic `isBinary` placeholder text below — set only for a
   * gcs target, which distinguishes "not a previewable type" from "too large
   * to preview inline" (an attachment/path target has neither distinction:
   * every one of its `isBinary` states uses the shared generic text).
   */
  binaryMessage?: string;
}

/** A gcs target whose response is application/octet-stream, or an image/* type that does not take the image path. */
const GCS_CANNOT_PREVIEW_MESSAGE = "This file can't be previewed.";

/**
 * A gcs target whose Content-Length exceeds TEXT_PREVIEW_MAX_BYTES, aborted
 * before the response body is read.
 */
const GCS_TOO_LARGE_INLINE_MESSAGE = 'This file is too large to preview inline.';

/** The generic placeholder for every attachment/path `isBinary` state. */
const GENERIC_CANNOT_PREVIEW_MESSAGE =
  "This file can't be shown here — it's too large or not a previewable type. Use the Download button.";

const IDLE_STATE: LoadState = {
  status: 'loading',
  isImage: false,
  isMarkdown: false,
  isBinary: false,
};

/**
 * The URL used to fetch/download a target's raw bytes (no `?view=` /
 * `?format=` suffix). Throws for an unsafe target — see
 * `buildAttachmentApiUrl`/`buildFileApiUrl`/`buildGcsObjectApiUrl` — so every
 * caller must be prepared for that, not just the ones already inside a
 * try/catch.
 */
function downloadUrlFor(target: PreviewTarget): string {
  if (target.kind === 'attachment') return buildAttachmentApiUrl(target.id);
  if (target.kind === 'gcs')
    return buildGcsObjectApiUrl(target.messageId, target.bucket, target.object);
  return buildFileApiUrl(target.projectId, target.location);
}

/** Classify a non-OK response into a short, actionable message. */
async function describeHttpError(res: Response): Promise<string> {
  if (res.status === 403) return "You don't have permission to view this file.";
  if (res.status === 404) return 'This file could not be found.';
  return extractApiError(res, `Failed to load file (HTTP ${res.status})`);
}

/**
 * Classify a non-OK response from the gcs endpoint into one of the fixed,
 * uniform error texts, plus whether the Cloud Console fallback applies
 * (every state here except 400 — the malformed-request case, which
 * the viewer's own Google identity in the console can't fix either).
 */
async function describeGcsHttpError(
  res: Response
): Promise<{ message: string; showConsoleLink: boolean }> {
  switch (res.status) {
    case 400:
      return { message: "This link isn't a valid gs:// URL.", showConsoleLink: false };
    case 403:
    case 404:
      return {
        message:
          "This object isn't available. It may not exist, or the agent that posted it can't access it.",
        showConsoleLink: true,
      };
    case 429:
      return { message: 'Too many requests, try again shortly.', showConsoleLink: true };
    case 413: {
      let limitMb = '10';
      let sizeText = 'unknown size';
      try {
        const parsed = (await res.json()) as {
          error?: { details?: { size?: number; limit?: number } };
        };
        const details = parsed.error?.details;
        if (typeof details?.limit === 'number') {
          limitMb = (details.limit / (1024 * 1024)).toFixed(1);
        }
        if (typeof details?.size === 'number') {
          sizeText = `${(details.size / (1024 * 1024)).toFixed(1)} MB`;
        }
      } catch {
        // Use the defaults above.
      }
      return {
        message: `This object is too large to open here (${sizeText}, limit ${limitMb} MB).`,
        showConsoleLink: true,
      };
    }
    case 502:
      return { message: "Couldn't fetch this object right now.", showConsoleLink: true };
    default:
      return {
        message: await extractApiError(res, `Failed to load object (HTTP ${res.status})`),
        showConsoleLink: true,
      };
  }
}

@customElement('scion-chat-file-preview')
export class ScionChatFilePreview extends LitElement {
  /** The attachment or path to preview, or null to render nothing. */
  @property({ attribute: false })
  target: PreviewTarget | null = null;

  @state() private loadState: LoadState = IDLE_STATE;

  /** Markdown source/preview toggle, per target (only ever one target open at a time). */
  @state() private showSource = false;

  @state() private copied = false;

  /** Bumped on every target change; a response is applied only if it still matches. */
  private generation = 0;

  private controller: AbortController | null = null;

  private copyTimer: ReturnType<typeof setTimeout> | null = null;

  /**
   * Set when a disconnect invalidated the loaded state (revoked its object
   * URL or aborted its fetch). A keyed `repeat` move detaches and
   * re-attaches this same instance with an unchanged `target`, so without
   * this the reconnected dialog would render the revoked `blob:` URL.
   */
  private reloadOnConnect = false;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.reloadOnConnect) this.requestUpdate();
  }

  override willUpdate(changed: PropertyValues<this>): void {
    if (this.reloadOnConnect && this.isConnected && !changed.has('target')) {
      this.reloadOnConnect = false;
      void this.load();
    }
    if (changed.has('target')) {
      this.reloadOnConnect = false;
      this.showSource = false;
      this.copied = false;
      // `copied` is already reset above, so a timer still pending from the
      // previous target no longer has anything of its own to reset — clear
      // it anyway so it doesn't linger as a scheduled callback for a target
      // that's no longer showing.
      this.clearCopyTimer();
      void this.load();
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    // Revoking/aborting leaves nothing usable to show, so drop the state and
    // reload it if this instance is ever reconnected.
    const invalidated = !!this.loadState.objectUrl || this.loadState.status === 'loading';
    this.controller?.abort();
    this.revokeObjectUrl();
    if (invalidated) {
      // Discard any in-flight load: a blob() continuation already queued
      // would otherwise create an object URL nothing ever revokes.
      this.generation++;
      this.reloadOnConnect = this.target !== null;
      this.loadState = IDLE_STATE;
    }
    // A copy-feedback timer is a plain JS timer, not tied to the element's
    // connection state, so it keeps running after disconnect regardless.
    // Clearing it alone isn't enough: once cleared, nothing would ever
    // reset `copied` back to false, so a reconnect of this same instance
    // (its state, including `copied`, survives a disconnect) would show
    // "Copied!" indefinitely, however soon or late that reconnect happens.
    // Resetting `copied` here directly is the state that's safe no matter
    // when — or whether — a reconnect follows.
    this.clearCopyTimer();
    this.copied = false;
  }

  private revokeObjectUrl(): void {
    if (this.loadState.objectUrl) {
      URL.revokeObjectURL(this.loadState.objectUrl);
    }
  }

  private clearCopyTimer(): void {
    if (this.copyTimer) {
      clearTimeout(this.copyTimer);
      this.copyTimer = null;
    }
  }

  private async load(): Promise<void> {
    this.controller?.abort();
    this.revokeObjectUrl();
    const gen = ++this.generation;
    const target = this.target;

    if (!target) {
      this.loadState = IDLE_STATE;
      return;
    }

    // A gcs target is never classified as an image before the fetch: doing
    // so correctly requires both the extension AND the response
    // Content-Type to agree, which needs the response in hand — see the
    // dedicated gcs branch inside the try block below, which fetches once
    // and decides image vs. octet-stream vs. text from the real response.
    // This `isImage` is only the pre-fetch candidate used for the initial
    // loading-state placeholder and for an unsafe-target/network error,
    // neither of which renders an image either way.
    const isImage =
      target.kind === 'attachment'
        ? IMAGE_MIMES.has(baseMimeType(target.mime))
        : target.kind === 'gcs'
          ? false
          : isImageFileName(target.name);
    const isMarkdown = !isImage && isMarkdownFileName(target.name);

    // Resolved before the binary classification below, and in its own
    // try/catch separate from the network try/catch further down: a thrown
    // target-URL builder error (see `downloadUrlFor`) is an internal detail
    // (e.g. "buildFileApiUrl: unsafe project id") that must never reach the
    // user directly — it's mapped to the same generic, fixed message a
    // broken/malicious link always gets, never the raw builder message. This
    // must run first: an unsafe target should show that generic error state
    // regardless of what its name/MIME would otherwise classify as, not the
    // binary-placeholder state below (which has no download link to offer).
    // This is also the client-side equivalent of a 400: the request never
    // left the browser, so the Cloud Console fallback does not apply here
    // either.
    let baseUrl: string;
    try {
      baseUrl = downloadUrlFor(target);
    } catch {
      this.loadState = {
        status: 'error',
        isImage,
        isMarkdown,
        isBinary: false,
        error: "This file link can't be opened.",
      };
      return;
    }

    // Classify before fetching so binary bytes never reach the code editor.
    // A path uses a deny-list of known binary extensions: the server already
    // rejects non-UTF-8 workspace content (shown as the error state below),
    // so any other name is fetched and previewed. An attachment is classified
    // by its MIME type; a generic `application/octet-stream` (what agent
    // attachments with an unmapped extension report, see
    // `pkg/hub/attachments_agent.go`) still counts as text when the file name
    // is a recognized text file.
    const isRecognizedText =
      target.kind === 'attachment'
        ? isLikelyTextMime(target.mime) ||
          (baseMimeType(target.mime) === 'application/octet-stream' &&
            isLikelyTextFileName(target.name))
        : target.kind === 'gcs'
          ? true
          : !isLikelyBinaryFileName(target.name);
    if (!isImage && !isRecognizedText) {
      this.loadState = { status: 'ready', isImage, isMarkdown, isBinary: true };
      return;
    }
    this.loadState = { status: 'loading', isImage, isMarkdown, isBinary: false };

    const controller = new AbortController();
    this.controller = controller;

    try {
      if (isImage) {
        const res = await apiFetch(`${baseUrl}?view=true`, {
          signal: controller.signal,
        });
        if (gen !== this.generation) return; // superseded — discard silently
        if (!res.ok) {
          this.loadState = {
            status: 'error',
            isImage,
            isMarkdown,
            isBinary: false,
            error: await describeHttpError(res),
          };
          return;
        }
        const blob = await res.blob();
        if (gen !== this.generation) return; // superseded while reading the body
        this.loadState = {
          status: 'ready',
          isImage,
          isMarkdown,
          isBinary: false,
          objectUrl: URL.createObjectURL(blob),
        };
        return;
      }

      // Text/markdown/code path. Attachments serve the raw body directly;
      // container paths use the existing `?format=json` contract, which also
      // reports size so an oversized file can fall back to download-only;
      // the gcs endpoint serves raw bytes directly, classified from its own
      // response headers (see the dedicated gcs branch just below).
      const url =
        target.kind === 'attachment' || target.kind === 'gcs' ? baseUrl : `${baseUrl}?format=json`;
      const res = await apiFetch(url, { signal: controller.signal });
      if (gen !== this.generation) return;
      if (!res.ok) {
        if (target.kind === 'gcs') {
          const { message, showConsoleLink } = await describeGcsHttpError(res);
          this.loadState = {
            status: 'error',
            isImage,
            isMarkdown,
            isBinary: false,
            error: message,
            errorStatus: res.status,
            showConsoleLink,
          };
          return;
        }
        this.loadState = {
          status: 'error',
          isImage,
          isMarkdown,
          isBinary: false,
          error: await describeHttpError(res),
        };
        return;
      }

      // A gcs target's real classification is only knowable from the
      // response: the client only ever has the object's name (and so its
      // extension) ahead of the fetch, never its bytes or stored metadata.
      // An application/octet-stream, non-previewable image/*, or
      // over-threshold-by-Content-Length response is never read past its
      // headers: `controller.abort()` cancels the in-flight body download
      // rather than reading it.
      if (target.kind === 'gcs') {
        const contentType = baseMimeType(res.headers.get('content-type') ?? '');

        // Image only if the extension AND the response Content-Type are both
        // one of the four supported raster types — an SVG's extension is
        // never in GCS_IMAGE_EXTENSIONS, so it can never reach this branch
        // regardless of its Content-Type, and a `.png` name whose sniffed
        // bytes are anything else (HTML, BMP, …) falls through to the checks
        // below instead.
        if (isGcsImageContentType(contentType) && isGcsImageExtension(target.name)) {
          const blob = await res.blob();
          if (gen !== this.generation) return; // superseded while reading the body
          this.loadState = {
            status: 'ready',
            isImage: true,
            isMarkdown: false,
            isBinary: false,
            objectUrl: URL.createObjectURL(blob),
          };
          return;
        }

        // Any other image/* response is binary the hub positively
        // identified, so it is never decoded as text.
        if (contentType === 'application/octet-stream' || contentType.startsWith('image/')) {
          controller.abort();
          this.loadState = {
            status: 'ready',
            isImage: false,
            isMarkdown: false,
            isBinary: true,
            binaryMessage: GCS_CANNOT_PREVIEW_MESSAGE,
          };
          return;
        }

        const contentLengthHeader = res.headers.get('content-length');
        const contentLength =
          contentLengthHeader !== null ? parseInt(contentLengthHeader, 10) : null;
        if (contentLength !== null && contentLength > TEXT_PREVIEW_MAX_BYTES) {
          controller.abort();
          this.loadState = {
            status: 'ready',
            isImage: false,
            isMarkdown: false,
            isBinary: true,
            binaryMessage: GCS_TOO_LARGE_INLINE_MESSAGE,
          };
          return;
        }

        const content = await res.text();
        if (gen !== this.generation) return;
        // A missing or unparsable Content-Length cannot be checked ahead of
        // the read, so the decoded text is held to the same limit, measured
        // in UTF-16 code units rather than bytes.
        if (content.length > TEXT_PREVIEW_MAX_BYTES) {
          this.loadState = {
            status: 'ready',
            isImage: false,
            isMarkdown: false,
            isBinary: true,
            binaryMessage: GCS_TOO_LARGE_INLINE_MESSAGE,
          };
          return;
        }
        this.loadState = { status: 'ready', isImage: false, isMarkdown, isBinary: false, content };
        return;
      }

      let content: string;
      let size: number;
      if (target.kind === 'attachment') {
        content = await res.text();
        size = target.size;
      } else {
        const data = (await res.json()) as { content: string; size: number };
        content = data.content;
        size = data.size;
      }
      if (gen !== this.generation) return;

      if (size > TEXT_PREVIEW_MAX_BYTES) {
        this.loadState = { status: 'ready', isImage, isMarkdown, isBinary: true };
        return;
      }
      this.loadState = { status: 'ready', isImage, isMarkdown, isBinary: false, content };
    } catch (err) {
      if (gen !== this.generation) return; // superseded — never publish a stale error
      if (err instanceof DOMException && err.name === 'AbortError') return; // intentional cancel
      this.loadState = {
        status: 'error',
        isImage,
        isMarkdown,
        isBinary: false,
        error: err instanceof Error ? err.message : 'Could not reach the server.',
      };
    }
  }

  private retry(): void {
    void this.load();
  }

  private close(): void {
    this.dispatchEvent(
      new CustomEvent('chat-file-preview-close', { bubbles: true, composed: true })
    );
  }

  private toggleSource(): void {
    this.showSource = !this.showSource;
  }

  private async copyContent(): Promise<void> {
    const text = this.loadState.content;
    if (text === undefined) return;
    try {
      await navigator.clipboard.writeText(text);
      this.copied = true;
      this.clearCopyTimer();
      this.copyTimer = setTimeout(() => {
        this.copied = false;
        this.copyTimer = null;
      }, 1500);
    } catch {
      // Clipboard write may fail in insecure contexts; silently ignore.
    }
  }

  private renderBody() {
    const target = this.target;
    const state = this.loadState;
    if (!target) return nothing;

    if (state.status === 'loading') {
      return html`
        <div class="file-preview-placeholder">
          <sl-spinner></sl-spinner>
          Loading file…
        </div>
      `;
    }
    if (state.status === 'error') {
      // The `target.kind === 'gcs'` half is redundant with `state.showConsoleLink`
      // alone: that field is only ever set inside load()'s gcs branch, so it
      // stays undefined (falsy) for every path/attachment error. Kept because
      // buildCloudConsoleUrl below is only valid to call on a GcsPreviewTarget,
      // so TypeScript needs the narrowing either way.
      return html`
        <div class="file-preview-placeholder error">
          <p>${state.error}</p>
          ${target.kind === 'gcs' && state.showConsoleLink
            ? html`<a
                class="console-fallback-link"
                href=${buildCloudConsoleUrl(target.bucket, target.object)}
                target="_blank"
                rel="noopener noreferrer"
              >
                Open in Cloud Console
              </a>`
            : nothing}
          <sl-button size="small" @click=${() => this.retry()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }
    if (state.isImage) {
      return html`<img
        class="file-preview-image"
        src=${state.objectUrl ?? ''}
        alt=${target.name}
      />`;
    }
    if (state.isBinary) {
      return html`
        <div class="file-preview-placeholder">
          ${state.binaryMessage ?? GENERIC_CANNOT_PREVIEW_MESSAGE}
        </div>
      `;
    }
    if (state.isMarkdown && !this.showSource) {
      return html`<scion-markdown-preview
        .content=${state.content ?? ''}
      ></scion-markdown-preview>`;
    }
    return html`
      <scion-code-editor
        .content=${state.content ?? ''}
        language=${getLanguageFromPath(target.name)}
        readonly
      ></scion-code-editor>
    `;
  }

  override render() {
    const target = this.target;
    if (!target) return nothing;
    const state = this.loadState;
    // Computed inside a try, exactly like `load()`'s own call to the same
    // function: an unsafe target makes this throw rather than being
    // normalized into a guessed URL, and `render()` must never propagate
    // that. `load()` hits the identical throw and already puts `state` into
    // `'error'` with a Retry action, so the only thing this component owns
    // here is not also crashing the *rest* of this render over the Download
    // button's href — sl-dialog's own header close button and the error
    // placeholder's Retry stay available either way.
    let downloadUrl: string | null;
    try {
      downloadUrl = downloadUrlFor(target);
    } catch {
      downloadUrl = null;
    }
    const secondary = target.kind === 'path' ? target.containerPath : target.name;
    // A 413 (too large) gcs error has no Download either: the same endpoint
    // would reject the download for the same reason. The `target.kind ===
    // 'gcs'` and `state.status === 'error'` halves are redundant with
    // `state.errorStatus === 413` alone: that field is only ever set inside
    // load()'s gcs branch, alongside status: 'error', so it can only be 413
    // when both other halves already hold.
    const suppressDownload =
      target.kind === 'gcs' && state.status === 'error' && state.errorStatus === 413;

    return html`
      <sl-dialog
        class="file-preview-dialog"
        open
        label=${target.name}
        @sl-after-hide=${(e: Event) => {
          if (e.target === e.currentTarget) this.close();
        }}
      >
        ${this.renderBody()}
        <div slot="footer" class="footer">
          <span class="path" title=${secondary}>${secondary}</span>
          ${state.status === 'ready' && state.isMarkdown
            ? html`
                <sl-button size="small" @click=${() => this.toggleSource()}>
                  <sl-icon slot="prefix" name=${this.showSource ? 'eye' : 'code'}></sl-icon>
                  ${this.showSource ? 'Preview' : 'Source'}
                </sl-button>
              `
            : nothing}
          ${state.status === 'ready' && state.isMarkdown && this.showSource
            ? html`
                <sl-button size="small" @click=${() => this.copyContent()}>
                  <sl-icon slot="prefix" name=${this.copied ? 'check2' : 'clipboard'}></sl-icon>
                  ${this.copied ? 'Copied!' : 'Copy'}
                </sl-button>
              `
            : nothing}
          ${downloadUrl !== null && !suppressDownload
            ? html`
                <sl-button href=${downloadUrl} download=${target.name} size="small">
                  <sl-icon slot="prefix" name="download"></sl-icon>
                  Download
                </sl-button>
              `
            : nothing}
        </div>
      </sl-dialog>
    `;
  }

  static override styles = css`
    :host {
      display: contents;
    }
    /* One dialog serves both a path preview (code/markdown panel) and an
     * attachment preview (full-height image), so its width policy must be
     * generous enough for either content type. */
    .file-preview-dialog::part(panel) {
      width: min(90vw, 900px);
      /* The app frame's height, not the viewport's: it shrinks with the
         iOS keyboard and the browser toolbar (see client/viewport.ts). */
      max-height: calc(var(--scion-app-height, 100dvh) * 0.85);
    }
    .file-preview-dialog::part(body) {
      padding: 0;
      overflow: auto;
    }
    .file-preview-placeholder {
      padding: 2rem;
      text-align: center;
      color: var(--scion-text-muted, #64748b);
      display: flex;
      flex-direction: column;
      align-items: center;
      gap: 0.75rem;
    }
    .file-preview-placeholder.error {
      color: var(--sl-color-danger-600, #dc2626);
    }
    .console-fallback-link {
      color: var(--sl-color-primary-600, #2563eb);
      font-size: var(--chat-fs-base, 0.875rem);
    }
    .file-preview-image {
      display: block;
      margin: 0 auto;
      max-width: 100%;
      max-height: calc(var(--scion-app-height, 100dvh) * 0.75);
      object-fit: contain;
    }
    .footer {
      display: flex;
      gap: 0.5rem;
      align-items: center;
      width: 100%;
    }
    .footer .path {
      flex: 1;
      font-size: var(--chat-fs-base, 0.875rem);
      color: var(--scion-text-muted, #64748b);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
  `;
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-file-preview': ScionChatFilePreview;
  }
}
