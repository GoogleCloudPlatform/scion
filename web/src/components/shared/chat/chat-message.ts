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
 * Chat message bubble component.
 *
 * Renders one message in the chat thread. Handles:
 * - User messages right-aligned, agent messages left-aligned
 * - Agent avatar (colour + initials hashed from slug)
 * - Markdown content via the shared utility (or preformatted for plain:true)
 * - Code blocks: monospace, horizontal scroll, copy button (no syntax highlighting)
 * - Attachments: chip showing basename, full path on hover, NOT clickable
 * - Text/code attachments: a short read-only preview slice with expand + download
 * - Image attachments: an inline thumbnail with a hover expand + download toolbar
 * - Badges: urgent, broadcasted, channel provenance
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch } from '../../../client/api.js';
import { getMarkdownRenderer } from '../../../utils/markdown.js';
import { getLanguageFromPath } from '../code-editor.js';
import { hashColor, getInitials } from './chat-avatar.js';
import '../code-editor.js';

/** Structured attachment reference from the W7 API. */
export interface AttachmentRefInfo {
  id: string;
  name: string;
  mime: string;
  size: number;
}

/** Image MIME types rendered inline. */
const IMAGE_MIMES = new Set(['image/jpeg', 'image/png', 'image/gif', 'image/webp']);

/** Non-`text/*` MIME types whose bytes are still text. */
const TEXT_MIMES = new Set([
  'application/json',
  'application/toml',
  'application/xml',
  'application/x-yaml',
  'application/yaml',
]);

/**
 * Extensions treated as text even when the MIME type does not say so — a
 * browser labels plenty of code files `application/octet-stream`.
 */
const TEXT_EXTENSIONS = new Set([
  '.bash',
  '.cfg',
  '.conf',
  '.css',
  '.csv',
  '.env',
  '.go',
  '.html',
  '.ini',
  '.js',
  '.json',
  '.jsx',
  '.log',
  '.md',
  '.py',
  '.rs',
  '.sh',
  '.sql',
  '.toml',
  '.ts',
  '.tsx',
  '.txt',
  '.xml',
  '.yaml',
  '.yml',
]);

/** Largest attachment fetched for an inline preview, in bytes. */
const PREVIEW_MAX_BYTES = 512 * 1024;

/** Lines handed to the collapsed slice — the rest needs the overlay. */
const PREVIEW_MAX_LINES = 40;

/** Lines that fit in the 200px slice; beyond this the edge is faded. */
const PREVIEW_VISIBLE_LINES = 9;

/** Lowercase extension including the dot, or '' when the name has none. */
function extensionOf(name: string): string {
  const dot = name.lastIndexOf('.');
  return dot > 0 ? name.slice(dot).toLowerCase() : '';
}

/**
 * True when an attachment holds text small enough to preview inline. Oversized
 * files stay download-only: a preview must not pull megabytes into the thread.
 */
export function isTextPreviewable(ref: AttachmentRefInfo): boolean {
  if (IMAGE_MIMES.has(ref.mime) || ref.size > PREVIEW_MAX_BYTES) return false;
  const mime = ref.mime.split(';')[0].trim().toLowerCase();
  if (mime.startsWith('text/') || TEXT_MIMES.has(mime)) return true;
  return TEXT_EXTENSIONS.has(extensionOf(ref.name));
}

/** First `max` lines of `text`, used for the collapsed slice. */
function firstLines(text: string, max: number): string {
  const lines = text.split('\n');
  return lines.length <= max ? text : lines.slice(0, max).join('\n');
}

/** Load state of one attachment preview. */
interface PreviewState {
  status: 'loading' | 'ready' | 'error';
  text?: string;
  error?: string;
}

/**
 * Fetched attachment bodies keyed by attachment ID. Shared across message
 * instances so scrolling back to a message costs nothing, and bounded so a
 * long-lived thread cannot grow it without limit.
 */
const CONTENT_CACHE_LIMIT = 40;
const contentCache = new Map<string, string>();
const contentInFlight = new Map<string, Promise<string>>();

/** Fetch an attachment body as text, de-duplicating concurrent requests. */
async function fetchAttachmentText(id: string): Promise<string> {
  const cached = contentCache.get(id);
  if (cached !== undefined) return cached;

  let pending = contentInFlight.get(id);
  if (!pending) {
    pending = (async () => {
      const res = await apiFetch(attachmentURL(id));
      if (!res.ok) throw new Error(`Preview unavailable (HTTP ${res.status})`);
      return res.text();
    })();
    contentInFlight.set(id, pending);
  }

  try {
    const text = await pending;
    if (contentCache.size >= CONTENT_CACHE_LIMIT) {
      // A Map iterates in insertion order, so the first key is the oldest.
      for (const oldest of contentCache.keys()) {
        contentCache.delete(oldest);
        break;
      }
    }
    contentCache.set(id, text);
    return text;
  } finally {
    contentInFlight.delete(id);
  }
}

/** Download/preview URL for an attachment. */
function attachmentURL(id: string): string {
  return `/api/v1/chat/attachments/${encodeURIComponent(id)}`;
}

/** Format file size for display. */
function formatFileSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/**
 * Spans of rendered markdown that @mention styling must step over: code
 * regions, whose text is literal, and HTML tags, whose attributes must not be
 * mangled. `<pre>` is listed before `<code>` so a `<pre><code>` fence is
 * consumed as one region. Whatever falls *between* matches is text content.
 *
 * Tags are matched as `<[^>]+>` — safe because the input is DOMPurify output,
 * which escapes any `>` appearing inside an attribute value.
 */
const MENTION_SKIP_REGION = '<pre\\b[^>]*>[\\s\\S]*?</pre>|<code\\b[^>]*>[\\s\\S]*?</code>|<[^>]+>';

/**
 * Wrap @mentions in styled, clickable spans.
 *
 * The captured slug is limited to `[\w.-]` so it can never break out of the
 * `data-mention` attribute it is interpolated into.
 */
function styleMentionsInText(text: string): string {
  return text.replace(
    /@([\w.-]+)/g,
    '<span class="mention clickable" data-mention="$1">@$1</span>'
  );
}

/**
 * Post-process rendered markdown to make @mentions clickable, leaving code
 * blocks and inline code untouched — an `@name` inside a fence is literal
 * text, not a reference to anyone.
 */
function styleMentions(htmlStr: string): string {
  // Built per call so the running `lastIndex` is never shared between renders.
  const skip = new RegExp(MENTION_SKIP_REGION, 'gi');
  let out = '';
  let cursor = 0;
  let match: RegExpExecArray | null;
  while ((match = skip.exec(htmlStr)) !== null) {
    out += styleMentionsInText(htmlStr.slice(cursor, match.index)) + match[0];
    cursor = match.index + match[0].length;
  }
  return out + styleMentionsInText(htmlStr.slice(cursor));
}

@customElement('scion-chat-message')
export class ScionChatMessage extends LitElement {
  /** The message body text. */
  @property()
  body = '';

  /** Sender display name. */
  @property()
  sender = '';

  /** Whether this is a message from the agent (left-aligned) or user (right-aligned). */
  @property({ type: Boolean })
  fromAgent = false;

  /** Whether the message is plain text (no markdown rendering). */
  @property({ type: Boolean })
  plain = false;

  /** Agent slug for avatar generation. */
  @property()
  agentSlug = '';

  /** Sender unique ID for avatar colour hashing (avoids collisions from similar names). */
  @property()
  senderId = '';

  /** Sender display name for v2 multi-sender rendering. */
  @property()
  senderName = '';

  /** Timestamp string. */
  @property()
  timestamp = '';

  /** Whether to show the sender header (false when grouped with previous message). */
  @property({ type: Boolean })
  showHeader = true;

  /** Whether this message is marked urgent. */
  @property({ type: Boolean })
  urgent = false;

  /** Whether this message was broadcasted. */
  @property({ type: Boolean })
  broadcasted = false;

  /** Channel provenance (e.g. "discord", "telegram"). */
  @property()
  channel = '';

  /** Visibility level: "normal", "verbose", or "full". */
  @property()
  visibility = 'normal';

  /** Message type (e.g. "assistant-reply", "state-change"). */
  @property()
  messageType = '';

  /** Dispatch state: "pending", "dispatched", or "failed". */
  @property()
  dispatchState = '';

  /** Reason for dispatch failure. */
  @property()
  dispatchFailureReason = '';

  /**
   * True when the DM peer's read watermark has reached this message. Replaces
   * the single-check "Delivered" indicator with a double-check "Seen".
   */
  @property({ type: Boolean })
  seen = false;

  /** Agent slug the message was routed to (shown in bubble header). */
  @property()
  routedTo = '';

  /** File attachment paths (wave-1 agent-style). */
  @property({ type: Array })
  attachments: string[] = [];

  /**
   * Structured attachment refs (wave-2 W7).
   * Each entry has {id, name, mime, size}.
   */
  @property({ type: Array })
  attachmentRefs: AttachmentRefInfo[] = [];

  @state()
  private renderedHtml = '';

  /** Preview load state per attachment ID. Replaced, never mutated. */
  @state()
  private previews: ReadonlyMap<string, PreviewState> = new Map();

  /** Attachment shown in the full-height overlay, if any. */
  @state()
  private expanded: AttachmentRefInfo | null = null;

  private renderTaskId = 0;

  private previewObserver: IntersectionObserver | null = null;
  private observedPreviews = new WeakSet<Element>();

  static override styles = css`
    :host {
      display: block;
    }

    .message-wrapper {
      display: flex;
      gap: 0.5rem;
      padding: 0.125rem 1rem;
    }

    .message-wrapper.from-user {
      flex-direction: row-reverse;
    }

    .message-wrapper.grouped {
      padding-top: 0;
    }

    /* Avatar */
    .avatar {
      width: 2rem;
      height: 2rem;
      border-radius: 50%;
      display: flex;
      align-items: center;
      justify-content: center;
      font-size: 0.75rem;
      font-weight: 700;
      color: #fff;
      flex-shrink: 0;
      text-transform: uppercase;
      margin-top: 0.125rem;
    }

    .avatar-spacer {
      width: 2rem;
      flex-shrink: 0;
    }

    /* Bubble */
    .bubble {
      max-width: min(70%, 600px);
      min-width: 3rem;
    }

    .bubble-header {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      margin-bottom: 0.125rem;
    }

    .sender-name {
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .msg-time {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
    }

    .routed-to {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.6875rem;
      font-weight: 400;
    }

    .bubble-content {
      padding: 0.5rem 0.75rem;
      border-radius: 0.75rem;
      line-height: 1.5;
      font-size: 0.875rem;
      word-break: break-word;
    }

    .from-agent .bubble-content {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text, #1e293b);
      border-top-left-radius: 0.25rem;
    }

    .from-user .bubble-content {
      background: var(--scion-primary-50, #eff6ff);
      color: var(--scion-text, #1e293b);
      border-top-right-radius: 0.25rem;
    }

    .from-user .bubble-header {
      flex-direction: row-reverse;
    }

    /* Pre-formatted (plain) text */
    .plain-text {
      white-space: pre-wrap;
      font-family: inherit;
    }

    /* Markdown content styles */
    .md-content {
      overflow-wrap: break-word;
    }

    .md-content p {
      margin: 0 0 0.5em;
    }

    .md-content p:last-child {
      margin-bottom: 0;
    }

    .md-content h1,
    .md-content h2,
    .md-content h3,
    .md-content h4 {
      margin: 0.75em 0 0.25em;
      font-weight: 600;
      line-height: 1.3;
    }

    .md-content h1:first-child,
    .md-content h2:first-child,
    .md-content h3:first-child {
      margin-top: 0;
    }

    .md-content h1 {
      font-size: 1.25rem;
    }
    .md-content h2 {
      font-size: 1.125rem;
    }
    .md-content h3 {
      font-size: 1rem;
    }

    .md-content a {
      color: var(--sl-color-primary-600, #2563eb);
      text-decoration: none;
    }

    .md-content a:hover {
      text-decoration: underline;
    }

    .md-content code {
      font-family: var(--scion-font-mono, 'SF Mono', 'Fira Code', monospace);
      font-size: 0.8125em;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.1em 0.3em;
      border-radius: 0.25rem;
      border: 1px solid var(--scion-border, #e2e8f0);
    }

    .md-content pre {
      position: relative;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      padding: 0.75rem;
      overflow-x: auto;
      margin: 0.5em 0;
    }

    .md-content pre code {
      background: none;
      border: none;
      padding: 0;
      font-size: 0.8125rem;
    }

    .copy-btn {
      position: absolute;
      top: 0.375rem;
      right: 0.375rem;
      padding: 0.125rem 0.5rem;
      font-size: 0.6875rem;
      font-family: inherit;
      line-height: 1.25rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.25rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      opacity: 0.6;
      transition: opacity 0.15s;
    }

    .copy-btn:hover {
      opacity: 1;
    }

    .md-content ul,
    .md-content ol {
      margin: 0.25em 0 0.5em;
      padding-left: 1.25em;
    }

    .md-content li {
      margin-bottom: 0.125em;
    }

    .md-content blockquote {
      border-left: 3px solid var(--scion-border, #e2e8f0);
      margin: 0.5em 0;
      padding: 0.25em 0.75em;
      color: var(--scion-text-muted, #64748b);
    }

    .md-content blockquote p:last-child {
      margin-bottom: 0;
    }

    /* @mention pills */
    .md-content .mention {
      background: rgba(59, 130, 246, 0.15);
      color: #60a5fa;
      padding: 0 4px;
      border-radius: 3px;
      font-weight: 500;
    }

    .md-content .mention.clickable {
      cursor: pointer;
    }

    .md-content .mention.clickable:hover {
      background: rgba(59, 130, 246, 0.25);
      text-decoration: underline;
    }

    .md-content table {
      border-collapse: collapse;
      width: 100%;
      margin: 0.5em 0;
      font-size: 0.8125rem;
    }

    .md-content th,
    .md-content td {
      border: 1px solid var(--scion-border, #e2e8f0);
      padding: 0.375em 0.5em;
      text-align: left;
    }

    .md-content th {
      background: var(--scion-bg-subtle, #f1f5f9);
      font-weight: 600;
    }

    /* Badges row */
    .badges {
      display: flex;
      gap: 0.25rem;
      margin-top: 0.25rem;
    }

    .badge {
      display: inline-block;
      padding: 0 0.375rem;
      border-radius: 0.25rem;
      font-size: 0.625rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.03em;
      line-height: 1.25rem;
    }

    .badge-urgent {
      background: var(--scion-danger-50, #fef2f2);
      color: var(--scion-danger-700, #b91c1c);
    }

    .badge-broadcast {
      background: var(--scion-warning-50, #fffbeb);
      color: var(--scion-warning-700, #b45309);
    }

    .badge-channel {
      background: var(--scion-neutral-100, #f1f5f9);
      color: var(--scion-neutral-600, #475569);
    }

    /* Attachments */
    .attachments {
      display: flex;
      flex-wrap: wrap;
      gap: 0.375rem;
      margin-top: 0.375rem;
    }

    .attachment-chip {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.125rem 0.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      cursor: default;
    }

    .attachment-chip sl-icon {
      font-size: 0.75rem;
    }

    /* W7: Inline image attachments */
    .attachment-images {
      display: flex;
      flex-wrap: wrap;
      gap: 0.5rem;
      margin-top: 0.375rem;
    }

    /* Holds the thumbnail's hover toolbar over the image itself. */
    .image-preview-wrapper {
      position: relative;
      display: inline-flex;
    }

    .image-actions {
      position: absolute;
      top: 0.25rem;
      right: 0.25rem;
      display: flex;
      gap: 0.125rem;
      padding: 0.0625rem;
      border-radius: 0.375rem;
      background: rgba(15, 23, 42, 0.65);
      opacity: 0;
      transition: opacity 0.15s ease;
    }

    /* Keyboard users reach the toolbar by tab, which never fires hover. */
    .image-preview-wrapper:hover .image-actions,
    .image-preview-wrapper:focus-within .image-actions {
      opacity: 1;
    }

    /* A touch screen has no hover state to reveal the toolbar with. */
    @media (hover: none) {
      .image-actions {
        opacity: 1;
      }
    }

    .image-actions sl-icon-button::part(base) {
      padding: 0.25rem;
      font-size: 0.875rem;
      color: #ffffff;
    }

    .image-actions sl-icon-button::part(base):hover {
      color: var(--scion-neutral-200, #e2e8f0);
    }

    /* The image is the button: no chrome of its own, just a focus ring. */
    .image-expand {
      display: inline-flex;
      padding: 0;
      border: none;
      background: none;
      cursor: pointer;
      border-radius: 0.5rem;
    }

    .image-expand:focus-visible {
      outline: 2px solid var(--scion-primary-600, #2563eb);
      outline-offset: 2px;
    }

    .attachment-image {
      max-width: 320px;
      max-height: 240px;
      border-radius: 0.5rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      cursor: pointer;
      object-fit: contain;
      background: var(--scion-bg-subtle, #f1f5f9);
      transition: opacity 0.2s ease;
    }

    .attachment-image:hover {
      opacity: 0.85;
    }

    /* W7: Download chips for non-image files */
    .download-chip {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.375rem 0.625rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.5rem;
      font-size: 0.75rem;
      color: var(--scion-text, #1e293b);
      cursor: pointer;
      text-decoration: none;
      transition: background 0.15s ease;
    }

    .download-chip:hover {
      background: var(--scion-border, #e2e8f0);
    }

    .download-chip sl-icon {
      font-size: 0.875rem;
      color: var(--scion-primary, #3b82f6);
    }

    .download-chip .file-name {
      font-weight: 500;
      max-width: 200px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .download-chip .file-size {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.6875rem;
    }

    /* Inline code/text previews for non-image attachments */
    .attachment-preview {
      margin-top: 0.375rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.5rem;
      overflow: hidden;
      background: var(--scion-surface, #ffffff);
    }

    .preview-header {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.125rem 0.25rem 0.125rem 0.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .preview-filename {
      flex: 1;
      min-width: 0;
      font-family: var(--scion-font-mono, 'SF Mono', 'Fira Code', monospace);
      font-size: 0.75rem;
      color: var(--scion-text, #1e293b);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .preview-size {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
    }

    .preview-actions {
      display: flex;
      gap: 0.125rem;
    }

    .preview-actions sl-icon-button::part(base) {
      padding: 0.25rem;
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
    }

    /*
     * The card already draws a frame, so the editor's own border is hidden by
     * blanking the custom property it reads.
     */
    .preview-body {
      position: relative;
      max-height: 200px;
      overflow: hidden;
      --scion-border: transparent;
      --scion-radius: 0;
    }

    /* Fade the clipped edge so it reads as "there is more below". */
    .preview-body.clipped::after {
      content: '';
      position: absolute;
      inset: auto 0 0 0;
      height: 2.5rem;
      background: linear-gradient(transparent, var(--scion-surface, #ffffff));
      pointer-events: none;
    }

    .preview-placeholder {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.75rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .preview-placeholder sl-spinner {
      font-size: 0.875rem;
    }

    .preview-placeholder.error {
      color: var(--scion-danger-600, #dc2626);
    }

    .full-preview::part(panel) {
      width: 90vw;
      max-width: 900px;
    }

    .full-preview::part(body) {
      padding-top: 0;
    }

    .full-preview .preview-placeholder {
      padding: 2rem;
    }

    /* Fit the whole image in the panel rather than scrolling it. */
    .full-preview .full-image {
      display: block;
      margin: 0 auto;
      max-width: 100%;
      max-height: 75vh;
      object-fit: contain;
    }

    /* Verbose (recessed) rendering — no bubble, muted text, small label */
    .message-wrapper.verbose .bubble-content {
      background: none;
      padding: 0.25rem 0.75rem;
      border-radius: 0;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
      font-style: italic;
    }

    .verbose-label {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.625rem;
      font-weight: 500;
      color: var(--scion-text-muted, #94a3b8);
      text-transform: uppercase;
      letter-spacing: 0.04em;
      margin-bottom: 0.125rem;
    }

    .verbose-label sl-icon {
      font-size: 0.6875rem;
    }

    /* Full/trace rendering — collapsed details block */
    .trace-block {
      padding: 0.125rem 1rem;
    }

    .trace-block details {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      background: var(--scion-bg-subtle, #f8fafc);
      max-width: min(80%, 700px);
    }

    .trace-block summary {
      padding: 0.375rem 0.75rem;
      font-size: 0.6875rem;
      font-weight: 500;
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      user-select: none;
      display: flex;
      align-items: center;
      gap: 0.375rem;
    }

    .trace-block summary sl-icon {
      font-size: 0.75rem;
    }

    .trace-content {
      padding: 0.5rem 0.75rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      border-top: 1px solid var(--scion-border, #e2e8f0);
      white-space: pre-wrap;
      font-family: var(--scion-font-mono, 'SF Mono', 'Fira Code', monospace);
      max-height: 300px;
      overflow-y: auto;
    }

    /* Delivery state indicators */
    .delivery-state {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      margin-top: 0.125rem;
      font-size: 0.625rem;
      color: var(--scion-text-muted, #94a3b8);
    }

    .delivery-state sl-icon {
      font-size: 0.6875rem;
    }

    .delivery-state.pending sl-icon {
      color: var(--scion-text-muted, #94a3b8);
    }

    .delivery-state.dispatched sl-icon {
      color: var(--scion-success-500, #22c55e);
    }

    .delivery-state.seen sl-icon {
      color: var(--scion-primary-500, #3b82f6);
    }

    .delivery-state.failed {
      color: var(--scion-danger-600, #dc2626);
    }

    .delivery-state.failed sl-icon {
      color: var(--scion-danger-600, #dc2626);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.renderContent();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.previewObserver?.disconnect();
    this.previewObserver = null;
    this.observedPreviews = new WeakSet();
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('body') || changed.has('plain')) {
      void this.renderContent();
    }
    if (changed.has('renderedHtml')) {
      this.injectCopyButtons();
    }
    this.observePreviews();
  }

  /**
   * Watch each preview slice and fetch its content once it nears the viewport.
   * A long thread would otherwise download every attached file on load.
   * Without an IntersectionObserver (older engines, tests) the content is
   * fetched straight away.
   */
  private observePreviews(): void {
    const nodes = this.shadowRoot?.querySelectorAll<HTMLElement>('.attachment-preview[data-id]');
    if (!nodes || nodes.length === 0) return;

    if (typeof IntersectionObserver !== 'function') {
      nodes.forEach((node) => {
        const id = node.dataset.id;
        if (id) void this.loadPreview(id);
      });
      return;
    }

    if (!this.previewObserver) {
      this.previewObserver = new IntersectionObserver(
        (entries, observer) => {
          for (const entry of entries) {
            if (!entry.isIntersecting) continue;
            observer.unobserve(entry.target);
            const id = (entry.target as HTMLElement).dataset.id;
            if (id) void this.loadPreview(id);
          }
        },
        { rootMargin: '200px' }
      );
    }

    nodes.forEach((node) => {
      if (this.observedPreviews.has(node)) return;
      this.observedPreviews.add(node);
      this.previewObserver?.observe(node);
    });
  }

  /** Fetch one attachment's text, recording load state for the template. */
  private async loadPreview(id: string): Promise<void> {
    if (this.previews.has(id)) return;
    this.setPreview(id, { status: 'loading' });
    try {
      const text = await fetchAttachmentText(id);
      this.setPreview(id, { status: 'ready', text });
    } catch (err) {
      this.setPreview(id, {
        status: 'error',
        error: err instanceof Error ? err.message : 'Preview unavailable',
      });
    }
  }

  private setPreview(id: string, state: PreviewState): void {
    const next = new Map(this.previews);
    next.set(id, state);
    this.previews = next;
  }

  /** Inject copy buttons on all code blocks inside rendered markdown. */
  private injectCopyButtons(): void {
    this.shadowRoot?.querySelectorAll('.md-content pre').forEach((pre) => {
      if (pre.querySelector('.copy-btn')) return;
      const btn = document.createElement('button');
      btn.className = 'copy-btn';
      btn.textContent = 'Copy';
      btn.addEventListener('click', () => {
        const code = pre.querySelector('code')?.textContent ?? pre.textContent ?? '';
        void navigator.clipboard.writeText(code);
        btn.textContent = 'Copied!';
        setTimeout(() => {
          btn.textContent = 'Copy';
        }, 1500);
      });
      pre.appendChild(btn);
    });
  }

  private async renderContent(): Promise<void> {
    if (!this.body || this.plain) {
      this.renderedHtml = '';
      return;
    }
    const taskId = ++this.renderTaskId;
    try {
      const renderer = await getMarkdownRenderer();
      if (taskId !== this.renderTaskId) return;
      this.renderedHtml = styleMentions(renderer.render(this.body));
    } catch {
      if (taskId !== this.renderTaskId) return;
      this.renderedHtml = '';
    }
  }

  /**
   * Clicking an @mention asks the page to open a DM with that entity. The
   * message itself cannot resolve the slug — only the page knows the member
   * roster — so it just reports the slug and lets the page navigate.
   */
  private handleMentionClick(e: MouseEvent): void {
    const target = (e.target as HTMLElement | null)?.closest('.mention[data-mention]');
    if (!target) return;

    const slug = target.getAttribute('data-mention');
    if (!slug) return;

    e.preventDefault();
    e.stopPropagation();

    this.dispatchEvent(
      new CustomEvent('mention-click', {
        bubbles: true,
        composed: true,
        detail: { slug },
      })
    );
  }

  override render() {
    // Full/trace messages render as a collapsed details block.
    if (this.visibility === 'full') {
      return this.renderTraceBlock();
    }

    const dirClass = this.fromAgent ? 'from-agent' : 'from-user';
    const visClass = this.visibility === 'verbose' ? ' verbose' : '';
    const groupClass = !this.showHeader ? ' grouped' : '';

    return html`
      <div class="message-wrapper ${dirClass}${visClass}${groupClass}">
        ${this.showHeader && this.fromAgent
          ? html`<div class="avatar" style="background: ${this.getAvatarColor()}">
              ${this.getInitials()}
            </div>`
          : this.fromAgent
            ? html`<div class="avatar-spacer"></div>`
            : nothing}
        <div class="bubble">
          ${this.visibility === 'verbose'
            ? html`
                <span class="verbose-label">
                  <sl-icon name="arrow-return-right"></sl-icon>
                  assistant reply
                </span>
              `
            : nothing}
          ${this.showHeader && this.fromAgent && this.visibility !== 'verbose'
            ? html`
                <div class="bubble-header">
                  <span class="sender-name">${this.senderName || this.sender}</span>
                  ${this.routedTo
                    ? html`<span class="routed-to"> → 🤖 ${this.routedTo}</span>`
                    : nothing}
                  <span class="msg-time">${this.formatTime()}</span>
                </div>
              `
            : nothing}
          ${this.showHeader && !this.fromAgent && this.routedTo
            ? html`
                <div class="bubble-header">
                  <span class="sender-name">${this.senderName || this.sender}</span>
                  <span class="routed-to"> → 🤖 ${this.routedTo}</span>
                  <span class="msg-time">${this.formatTime()}</span>
                </div>
              `
            : nothing}
          <div class="bubble-content">${this.renderBody()}</div>
          ${this.renderDeliveryState()} ${this.renderBadges()} ${this.renderAttachments()}
        </div>
      </div>
      ${this.renderFullPreview()}
    `;
  }

  /** Render a collapsed trace block for full-visibility messages. */
  private renderTraceBlock() {
    return html`
      <div class="trace-block">
        <details>
          <summary>
            <sl-icon name="code-slash"></sl-icon>
            Trace — ${this.sender} at ${this.formatTime()}
          </summary>
          <div class="trace-content">${this.body}</div>
        </details>
      </div>
    `;
  }

  /** Render delivery state indicator for outbound (user-sent) messages. */
  private renderDeliveryState() {
    // Only show on user-sent messages with a dispatch state.
    if (this.fromAgent || !this.dispatchState) return nothing;

    switch (this.dispatchState) {
      case 'pending':
        return html`
          <div class="delivery-state pending">
            <sl-icon name="clock"></sl-icon>
            Sending
          </div>
        `;
      case 'dispatched':
        return this.seen
          ? html`
              <div class="delivery-state seen">
                <sl-icon name="check2-all"></sl-icon>
                Seen
              </div>
            `
          : html`
              <div class="delivery-state dispatched">
                <sl-icon name="check2"></sl-icon>
                Delivered
              </div>
            `;
      case 'failed':
        return html`
          <sl-tooltip content=${this.dispatchFailureReason || 'Delivery failed'} hoist>
            <div class="delivery-state failed">
              <sl-icon name="exclamation-triangle"></sl-icon>
              Failed
            </div>
          </sl-tooltip>
        `;
      default:
        return nothing;
    }
  }

  private renderBody() {
    if (this.plain || !this.renderedHtml) {
      return html`<div class="plain-text">${this.body}</div>`;
    }
    return html`<div
      class="md-content"
      @click=${this.handleMentionClick}
      .innerHTML=${this.renderedHtml}
    ></div>`;
  }

  private renderBadges() {
    const hasBadges = this.urgent || this.broadcasted || (this.channel && this.channel !== 'web');
    if (!hasBadges) return nothing;

    return html`
      <div class="badges">
        ${this.urgent ? html`<span class="badge badge-urgent">urgent</span>` : nothing}
        ${this.broadcasted ? html`<span class="badge badge-broadcast">broadcast</span>` : nothing}
        ${this.channel && this.channel !== 'web'
          ? html`<span class="badge badge-channel">via ${this.channel}</span>`
          : nothing}
      </div>
    `;
  }

  private renderAttachments() {
    // W7: Render structured attachment refs (v2 mode).
    if (this.attachmentRefs && this.attachmentRefs.length > 0) {
      return this.renderV2Attachments();
    }

    // Wave-1: Render file path chips.
    if (!this.attachments || this.attachments.length === 0) return nothing;

    return html`
      <div class="attachments">
        ${this.attachments.map(
          (path) => html`
            <sl-tooltip content=${path} hoist>
              <span class="attachment-chip">
                <sl-icon name="paperclip"></sl-icon>
                ${this.basename(path)}
              </span>
            </sl-tooltip>
          `
        )}
      </div>
    `;
  }

  /**
   * Render W7 structured attachments: inline images, code previews for text
   * files, and download chips for everything else.
   */
  private renderV2Attachments() {
    const images = this.attachmentRefs.filter((a) => IMAGE_MIMES.has(a.mime));
    const rest = this.attachmentRefs.filter((a) => !IMAGE_MIMES.has(a.mime));
    const previewable = rest.filter(isTextPreviewable);
    const files = rest.filter((a) => !isTextPreviewable(a));

    return html`
      ${images.length > 0
        ? html`
            <div class="attachment-images">
              ${images.map(
                (img) => html`
                  <div class="image-preview-wrapper">
                    <button
                      type="button"
                      class="image-expand"
                      title="${img.name} — click to expand"
                      aria-label="Expand ${img.name}"
                      @click=${() => this.openFullPreview(img)}
                    >
                      <img
                        class="attachment-image"
                        src=${attachmentURL(img.id)}
                        alt=${img.name}
                        loading="lazy"
                      />
                    </button>
                    <div class="image-actions">
                      <sl-icon-button
                        name="arrows-angle-expand"
                        label="Expand ${img.name}"
                        @click=${() => this.openFullPreview(img)}
                      ></sl-icon-button>
                      <sl-icon-button
                        name="download"
                        label="Download ${img.name}"
                        href=${attachmentURL(img.id)}
                        download=${img.name}
                      ></sl-icon-button>
                    </div>
                  </div>
                `
              )}
            </div>
          `
        : nothing}
      ${previewable.map((file) => this.renderCodePreview(file))}
      ${files.length > 0
        ? html`
            <div class="attachments">
              ${files.map(
                (file) => html`
                  <a
                    class="download-chip"
                    href="/api/v1/chat/attachments/${file.id}"
                    download=${file.name}
                    title="Download ${file.name}"
                  >
                    <sl-icon name="file-earmark-arrow-down"></sl-icon>
                    <span class="file-name">${file.name}</span>
                    <span class="file-size">${formatFileSize(file.size)}</span>
                  </a>
                `
              )}
            </div>
          `
        : nothing}
    `;
  }

  /** A short read-only slice of a text attachment, with expand + download. */
  private renderCodePreview(ref: AttachmentRefInfo) {
    const state = this.previews.get(ref.id);
    // Only fade the bottom edge when the file really does run past the slice.
    const clipped =
      state?.status === 'ready' && (state.text ?? '').split('\n').length > PREVIEW_VISIBLE_LINES;
    return html`
      <div class="attachment-preview" data-id=${ref.id}>
        <div class="preview-header">
          <span class="preview-filename" title=${ref.name}>${ref.name}</span>
          <span class="preview-size">${formatFileSize(ref.size)}</span>
          <div class="preview-actions">
            <sl-icon-button
              name="arrows-angle-expand"
              label="Expand ${ref.name}"
              @click=${() => this.openFullPreview(ref)}
            ></sl-icon-button>
            <sl-icon-button
              name="download"
              label="Download ${ref.name}"
              href=${attachmentURL(ref.id)}
              download=${ref.name}
            ></sl-icon-button>
          </div>
        </div>
        <div class="preview-body ${clipped ? 'clipped' : ''}">
          ${this.renderPreviewBody(ref, state)}
        </div>
      </div>
    `;
  }

  /** Editor, spinner or error for one preview, depending on load state. */
  private renderPreviewBody(ref: AttachmentRefInfo, state: PreviewState | undefined, full = false) {
    if (!state || state.status === 'loading') {
      return html`
        <div class="preview-placeholder">
          <sl-spinner></sl-spinner>
          Loading preview…
        </div>
      `;
    }
    if (state.status === 'error') {
      return html`<div class="preview-placeholder error">${state.error}</div>`;
    }
    const text = state.text ?? '';
    return html`
      <scion-code-editor
        .content=${full ? text : firstLines(text, PREVIEW_MAX_LINES)}
        language=${getLanguageFromPath(ref.name)}
        readonly
      ></scion-code-editor>
    `;
  }

  /** Full-height overlay for the expanded attachment, when one is open. */
  private renderFullPreview() {
    const ref = this.expanded;
    if (!ref) return nothing;

    return html`
      <sl-dialog
        class="full-preview"
        open
        label=${ref.name}
        @sl-after-hide=${(e: Event) => {
          if (e.target === e.currentTarget) this.expanded = null;
        }}
      >
        ${IMAGE_MIMES.has(ref.mime)
          ? html`<img class="full-image" src=${attachmentURL(ref.id)} alt=${ref.name} />`
          : this.renderPreviewBody(ref, this.previews.get(ref.id), true)}
        <sl-button slot="footer" href=${attachmentURL(ref.id)} download=${ref.name}>
          <sl-icon slot="prefix" name="download"></sl-icon>
          Download
        </sl-button>
      </sl-dialog>
    `;
  }

  /** Open the overlay, fetching the content if the slice has not yet. */
  private openFullPreview(ref: AttachmentRefInfo): void {
    this.expanded = ref;
    // An image is shown by the browser straight from its URL; only text
    // previews need the body pulled down.
    if (!IMAGE_MIMES.has(ref.mime)) {
      void this.loadPreview(ref.id);
    }
  }

  private formatTime(): string {
    if (!this.timestamp) return '';
    try {
      const d = new Date(this.timestamp);
      return d.toLocaleTimeString('en', {
        hour12: false,
        hour: '2-digit',
        minute: '2-digit',
      });
    } catch {
      return '';
    }
  }

  /** Deterministic colour from the sender ID (preferred) or slug/name fallback. */
  private getAvatarColor(): string {
    return hashColor(this.senderId || this.agentSlug || this.sender || '');
  }

  /** Initials derived from the agent slug or sender name. */
  private getInitials(): string {
    return getInitials(this.agentSlug || this.sender || '');
  }

  /** Extract the file basename from a path. */
  private basename(path: string): string {
    const parts = path.split('/');
    return parts[parts.length - 1] || path;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-message': ScionChatMessage;
  }
}
