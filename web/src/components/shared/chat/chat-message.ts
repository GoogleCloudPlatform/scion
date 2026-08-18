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
import '../markdown-preview.js';

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
  '.markdown',
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

/**
 * Map fenced code block language tags to CodeMirror language identifiers.
 * Used by the syntax-highlighting post-processor to replace plain
 * `<pre><code>` blocks with `<scion-code-editor readonly>`.
 */
const CODE_BLOCK_LANGUAGE_MAP: Record<string, string> = {
  typescript: 'typescript',
  ts: 'typescript',
  javascript: 'javascript',
  js: 'javascript',
  go: 'go',
  python: 'python',
  py: 'python',
  json: 'json',
  yaml: 'yaml',
  yml: 'yaml',
  html: 'html',
  css: 'css',
  rust: 'rust',
  rs: 'rust',
  markdown: 'markdown',
  md: 'markdown',
  // CodeMirror shell mode is not bundled; fall back to monospace-only display.
  bash: 'plaintext',
  sh: 'plaintext',
  shell: 'plaintext',
};

// ---------------------------------------------------------------------------
// Entity link detection (#1059) — configurable patterns for deep-linking
// entity references in agent messages to their detail pages.
// ---------------------------------------------------------------------------

interface EntityPattern {
  /** A global regex that matches one entity reference. */
  regex: RegExp;
  /** Build the `<a>` tag for a match. Return the full `<a>…</a>` string. */
  linkBuilder: (match: RegExpExecArray) => string;
}

/**
 * Configurable entity patterns for deep-linking. Order matters: the first
 * match wins, so more specific patterns must come before less specific ones.
 */
const ENTITY_PATTERNS: EntityPattern[] = [
  // Session IDs: sess_ prefix followed by hex/uuid chars
  {
    regex: /\bsess_([0-9a-fA-F-]{4,36})\b/g,
    linkBuilder: (m) => {
      const id = m[0]; // full match including prefix
      return `<a class="entity-link" href="/sessions/${encodeURIComponent(id)}" title="Open session ${id}">${id}</a>`;
    },
  },
  // Bare UUIDs preceded by "session" (case-insensitive)
  {
    regex: /\bsession\s+([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\b/gi,
    linkBuilder: (m) => {
      const uuid = m[1];
      return `session <a class="entity-link" href="/sessions/${encodeURIComponent(uuid)}" title="Open session ${uuid}">${uuid}</a>`;
    },
  },
  // Commit SHAs: "commit" followed by 7-40 hex chars
  {
    regex: /\bcommit\s+([0-9a-fA-F]{7,40})\b/gi,
    linkBuilder: (m) => {
      const sha = m[1];
      return `commit <a class="entity-link" href="/commits/${encodeURIComponent(sha)}" title="View commit ${sha}">${sha}</a>`;
    },
  },
  // File paths: workspace-absolute or relative paths with extensions
  {
    regex: /(?:\/workspace\/|(?:^|(?<=\s)))([a-zA-Z0-9_.-]+(?:\/[a-zA-Z0-9_.-]+)+\.[a-zA-Z]{1,10})\b/g,
    linkBuilder: (m) => {
      const full = m[0];
      const path = full.startsWith('/workspace/') ? full.slice('/workspace/'.length) : full.trimStart();
      return `<a class="entity-link" href="/files/${encodeURIComponent(path)}" title="Open ${full.trim()}">${full}</a>`;
    },
  },
];

/**
 * Apply entity link patterns to a text segment (outside code/HTML regions).
 * Each match is replaced by the pattern's linkBuilder output. Patterns are
 * tried left-to-right through the string; the first match at each position
 * wins. This is intentionally simple: it scans linearly and does not try to
 * handle overlapping matches from different patterns.
 */
function styleEntityLinksInText(text: string): string {
  // Collect all matches from all patterns, then sort by position.
  const replacements: { start: number; end: number; replacement: string }[] = [];

  for (const pattern of ENTITY_PATTERNS) {
    // Clone the regex so each call starts from index 0.
    // Ensure the global flag is always set so re.exec() advances lastIndex
    // and cannot loop infinitely on a zero-width or non-advancing match.
    const flags = pattern.regex.flags.includes('g') ? pattern.regex.flags : pattern.regex.flags + 'g';
    const re = new RegExp(pattern.regex.source, flags);
    let m: RegExpExecArray | null;
    while ((m = re.exec(text)) !== null) {
      const prevIndex = re.lastIndex;
      replacements.push({
        start: m.index,
        end: m.index + m[0].length,
        replacement: pattern.linkBuilder(m),
      });
      // Safety: if lastIndex did not advance (e.g. zero-length match or
      // missing global flag), break to avoid an infinite loop.
      if (re.lastIndex === prevIndex) break;
    }
  }

  if (replacements.length === 0) return text;

  // Sort by start position, pick non-overlapping winners (first match wins).
  replacements.sort((a, b) => a.start - b.start);
  let out = '';
  let cursor = 0;
  for (const rep of replacements) {
    if (rep.start < cursor) continue; // overlaps with a prior replacement
    out += text.slice(cursor, rep.start) + rep.replacement;
    cursor = rep.end;
  }
  return out + text.slice(cursor);
}

/**
 * Post-process rendered markdown to turn entity references into clickable
 * deep links, leaving code blocks and inline code untouched — an entity ID
 * inside a fence is literal text, not a navigation target.
 *
 * Follows the exact same skip-region approach as `styleMentions()`.
 */
function styleEntityLinks(htmlStr: string): string {
  const skip = new RegExp(MENTION_SKIP_REGION, 'gi');
  let out = '';
  let cursor = 0;
  let match: RegExpExecArray | null;
  while ((match = skip.exec(htmlStr)) !== null) {
    out += styleEntityLinksInText(htmlStr.slice(cursor, match.index)) + match[0];
    cursor = match.index + match[0].length;
  }
  return out + styleEntityLinksInText(htmlStr.slice(cursor));
}

/** Lowercase extension including the dot, or '' when the name has none. */
function extensionOf(name: string): string {
  const dot = name.lastIndexOf('.');
  return dot > 0 ? name.slice(dot).toLowerCase() : '';
}

/** True when the file is a Markdown document. */
function isMarkdownFile(name: string): boolean {
  const ext = extensionOf(name);
  return ext === '.md' || ext === '.markdown';
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

  // ---- Phase-3 properties ----

  /** Whether this is the current user's message. */
  @property({ type: Boolean })
  isOwn = false;

  /** Whether edit is allowed (no agent in reply chain). */
  @property({ type: Boolean })
  canEdit = false;

  /** Whether delete is allowed (no agent in reply chain). */
  @property({ type: Boolean })
  canDelete = false;

  /** Message ID for copy-link and event dispatch. */
  @property()
  messageId = '';

  /** Reply preview data: the message this one is replying to. */
  @property({ type: Object })
  replyPreview: { messageId: string; senderName: string; content: string } | null = null;

  /** When set, indicates the message was edited at this timestamp. */
  @property()
  editedAt = '';

  /** When set, indicates the message was soft-deleted. */
  @property()
  deletedAt = '';

  @state()
  private renderedHtml = '';

  /** Whether the action bar is pinned visible (for touch devices). */
  @state()
  private actionBarPinned = false;

  /** Preview load state per attachment ID. Replaced, never mutated. */
  @state()
  private previews: ReadonlyMap<string, PreviewState> = new Map();

  /** Attachment shown in the full-height overlay, if any. */
  @state()
  private expanded: AttachmentRefInfo | null = null;

  /**
   * Per-attachment toggle: true = show raw source, false/absent = show
   * rendered preview (only meaningful for markdown files).
   */
  @state()
  private mdSourceView: ReadonlyMap<string, boolean> = new Map();

  /**
   * Tracks which attachment IDs have a "Copied!" feedback timer active,
   * so the button shows a check icon instead of the clipboard icon.
   */
  @state()
  private copiedIds: ReadonlySet<string> = new Set();

  private copyTimers = new Map<string, ReturnType<typeof setTimeout>>();

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
      position: relative;
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
      position: relative;
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

    /* Syntax-highlighted code blocks (#1049) */
    .code-block-editor {
      display: block;
      margin: 0.5em 0;
      max-height: 400px;
      overflow: auto;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
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

    /* Markdown preview body — let the component fill the card. */
    .md-preview-body {
      padding: 0;
      --scion-border: transparent;
      --scion-radius: 0;
    }

    .md-preview-body scion-markdown-preview {
      --scion-radius: 0;
    }

    .md-preview-body scion-markdown-preview::part(container) {
      border: none;
      border-radius: 0;
      max-height: 200px;
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

    /* ---- Phase-3: Message action bar ---- */
    .message-actions {
      position: absolute;
      top: -12px;
      right: 8px;
      display: flex;
      gap: 0.0625rem;
      padding: 0.125rem;
      border-radius: 0.375rem;
      background: var(--scion-surface-100, #f1f5f9);
      border: 1px solid var(--scion-neutral-200, #e2e8f0);
      box-shadow: 0 1px 3px rgba(0,0,0,0.1);
      opacity: 0;
      visibility: hidden;
      pointer-events: none;
      transition: opacity 0.15s ease, visibility 0.15s ease;
      z-index: 10;
    }

    .message-wrapper:hover .message-actions,
    .message-wrapper:focus-within .message-actions,
    .message-actions.pinned {
      opacity: 1;
      visibility: visible;
      pointer-events: auto;
    }

    @media (hover: none) {
      .message-actions {
        opacity: 0;
        visibility: hidden;
        pointer-events: none;
      }
      .message-actions.pinned {
        opacity: 1;
        visibility: visible;
        pointer-events: auto;
      }
    }

    .message-actions sl-icon-button::part(base) {
      padding: 0.25rem;
      font-size: 0.875rem;
      color: var(--scion-neutral-600, #475569);
    }

    .message-actions sl-icon-button::part(base):hover {
      color: var(--scion-primary-600, #2563eb);
    }

    /* ---- Phase-3: Reply preview quote block ---- */
    .reply-preview {
      display: flex;
      align-items: flex-start;
      gap: 0.375rem;
      padding: 0.25rem 0.5rem;
      margin-bottom: 0.25rem;
      border-left: 3px solid var(--scion-primary-400, #60a5fa);
      background: var(--scion-surface-50, #f8fafc);
      border-radius: 0 0.25rem 0.25rem 0;
      cursor: pointer;
      font-size: 0.75rem;
      color: var(--scion-neutral-500, #64748b);
      max-width: 100%;
      overflow: hidden;
    }

    .reply-preview:hover {
      background: var(--scion-surface-100, #f1f5f9);
    }

    .reply-preview .reply-sender {
      font-weight: 600;
      color: var(--scion-primary-600, #2563eb);
      white-space: nowrap;
    }

    .reply-preview .reply-content {
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    /* ---- Phase-3: Deleted message ---- */
    .deleted-message {
      font-style: italic;
      color: var(--scion-neutral-400, #94a3b8);
    }

    /* ---- Phase-3: Edited label ---- */
    .edited-label {
      font-size: 0.625rem;
      color: var(--scion-neutral-400, #94a3b8);
      margin-left: 0.25rem;
    }

    /* ---- Entity deep links (#1059) ---- */
    .md-content .entity-link {
      color: var(--sl-color-primary-600, #2563eb);
      text-decoration: none;
      border-bottom: 1px dotted var(--sl-color-primary-400, #60a5fa);
    }

    .md-content .entity-link:hover {
      text-decoration: underline;
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    /* ---- Rich output: diff blocks (#1060) ---- */
    .diff-block {
      font-family: var(--scion-font-mono, 'SF Mono', 'Fira Code', monospace);
      font-size: 0.8125rem;
      line-height: 1.5;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      padding: 0.75rem;
      overflow-x: auto;
      margin: 0.5em 0;
    }

    .diff-block .diff-line {
      display: block;
      white-space: pre;
    }

    .diff-block .diff-add {
      background: rgba(34, 197, 94, 0.15);
      color: var(--scion-success-700, #15803d);
    }

    .diff-block .diff-remove {
      background: rgba(239, 68, 68, 0.15);
      color: var(--scion-danger-700, #b91c1c);
    }

    .diff-block .diff-hunk {
      background: rgba(59, 130, 246, 0.1);
      color: var(--scion-primary-600, #2563eb);
      font-weight: 500;
    }

    /* ---- Rich output: collapsible blocks (#1060) ---- */
    .collapsible-block {
      position: relative;
      margin: 0.5em 0;
    }

    .collapsible-block.collapsed .collapsible-content {
      max-height: calc(10 * 1.5em + 1.5rem);
      overflow: hidden;
    }

    .collapsible-block.collapsed .collapsible-content::after {
      content: '';
      position: absolute;
      bottom: 0;
      left: 0;
      right: 0;
      height: 2rem;
      background: linear-gradient(transparent, var(--scion-bg-subtle, #f1f5f9));
      pointer-events: none;
    }

    .collapsible-toggle {
      display: block;
      width: 100%;
      padding: 0.25rem 0.75rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-top: none;
      border-radius: 0 0 0.375rem 0.375rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--sl-color-primary-600, #2563eb);
      font-size: 0.75rem;
      font-family: inherit;
      cursor: pointer;
      text-align: center;
    }

    .collapsible-toggle:hover {
      background: var(--scion-border, #e2e8f0);
    }

    /* ---- Rich output: test results (#1060) ---- */
    .test-results {
      font-family: var(--scion-font-mono, 'SF Mono', 'Fira Code', monospace);
      font-size: 0.8125rem;
      line-height: 1.5;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      padding: 0.75rem;
      overflow-x: auto;
      margin: 0.5em 0;
    }

    .test-results .test-line {
      display: block;
      white-space: pre;
    }

    .test-results .test-pass {
      color: var(--scion-success-700, #15803d);
    }

    .test-results .test-fail {
      color: var(--scion-danger-700, #b91c1c);
      font-weight: 600;
    }

    .test-results .test-summary {
      font-weight: 600;
      margin-top: 0.25em;
      padding-top: 0.25em;
      border-top: 1px solid var(--scion-border, #e2e8f0);
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
    // Clean up touch long-press timer to prevent leaks on disconnect.
    if (this.touchTimer) {
      clearTimeout(this.touchTimer);
      this.touchTimer = null;
    }
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('body') || changed.has('plain')) {
      void this.renderContent();
    }
    if (changed.has('renderedHtml')) {
      this.injectCopyButtons();
      this.injectSyntaxHighlighting();
      this.injectDiffBlocks();
      this.injectCollapsibleBlocks();
      this.injectTestResults();
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
        const prev = (btn as any)._copyTimer as ReturnType<typeof setTimeout> | undefined;
        if (prev) clearTimeout(prev);
        (btn as any)._copyTimer = setTimeout(() => {
          if (!this.isConnected) return;
          btn.textContent = 'Copy';
        }, 1500);
      });
      pre.appendChild(btn);
    });
  }

  /**
   * Replace fenced code blocks that have a language class with
   * `<scion-code-editor readonly>` for syntax highlighting.
   *
   * `marked` produces `<pre><code class="language-typescript">` for
   * ` ```typescript ` blocks. We detect the class, extract the language,
   * and swap the `<pre>` with a readonly code editor.
   */
  private injectSyntaxHighlighting(): void {
    const pres = this.shadowRoot?.querySelectorAll('.md-content pre');
    if (!pres) return;

    pres.forEach((pre) => {
      // Skip if already replaced.
      if (pre.getAttribute('data-highlighted') === 'true') return;

      const codeEl = pre.querySelector('code');
      if (!codeEl) return;

      // Extract language from class="language-xxx" set by marked.
      const langClass = Array.from(codeEl.classList).find((c) => c.startsWith('language-'));
      if (!langClass) return; // No language tag — keep plain styling.

      const langTag = langClass.replace('language-', '').toLowerCase();
      const language = CODE_BLOCK_LANGUAGE_MAP[langTag];
      if (!language) return; // Unknown language — keep plain styling.

      const content = codeEl.textContent ?? '';
      pre.setAttribute('data-highlighted', 'true');

      // Create a readonly code editor and replace the <pre> in-place.
      const editor = document.createElement('scion-code-editor') as import('../code-editor.js').ScionCodeEditor;
      editor.content = content;
      editor.language = language;
      editor.readonly = true;
      editor.classList.add('code-block-editor');

      pre.replaceWith(editor);
    });
  }

  /**
   * Replace `<pre>` blocks whose content looks like a unified diff with a
   * colour-coded diff view. Lines starting with `+` are additions, `-` are
   * removals, and `@@` are hunk headers.
   */
  private injectDiffBlocks(): void {
    const pres = this.shadowRoot?.querySelectorAll('.md-content pre');
    if (!pres) return;

    pres.forEach((pre) => {
      if (pre.getAttribute('data-diff') === 'true') return;
      const codeEl = pre.querySelector('code');
      const text = (codeEl ?? pre).textContent ?? '';
      const lines = text.split('\n');

      // Heuristic: require a ---/+++ header pair or @@ hunk headers to
      // avoid false-positives on YAML lists, markdown checklists, and
      // code with @ decorators.
      const hasHeaders = lines.some((l) => l.startsWith('--- ')) && lines.some((l) => l.startsWith('+++ '));
      const hasHunks = lines.some((l) => l.startsWith('@@ '));
      const diffLineCount = lines.filter((l) => /^[-+@]/.test(l)).length;
      const looksLikeDiff = hasHeaders || (hasHunks && diffLineCount >= 3);
      if (!looksLikeDiff) return;

      pre.setAttribute('data-diff', 'true');

      const container = document.createElement('div');
      container.className = 'diff-block';

      for (const line of lines) {
        const span = document.createElement('span');
        span.className = 'diff-line';
        if (line.startsWith('@@')) {
          span.classList.add('diff-hunk');
        } else if (line.startsWith('+')) {
          span.classList.add('diff-add');
        } else if (line.startsWith('-')) {
          span.classList.add('diff-remove');
        }
        span.textContent = line;
        container.appendChild(span);
      }
      pre.replaceWith(container);
    });
  }

  /**
   * Wrap `<pre>` blocks that exceed 20 lines in a collapsible container that
   * shows only the first 10 lines by default with a "Show more" toggle.
   */
  private injectCollapsibleBlocks(): void {
    const pres = this.shadowRoot?.querySelectorAll('.md-content pre');
    if (!pres) return;

    pres.forEach((pre) => {
      if (pre.getAttribute('data-collapsible') === 'true') return;
      if (pre.closest('.collapsible-block') || pre.closest('.diff-block')) return;
      const codeEl = pre.querySelector('code');
      const text = (codeEl ?? pre).textContent ?? '';
      const lineCount = text.split('\n').length;

      if (lineCount <= 20) return;

      pre.setAttribute('data-collapsible', 'true');

      const wrapper = document.createElement('div');
      wrapper.className = 'collapsible-block collapsed';

      const contentWrapper = document.createElement('div');
      contentWrapper.className = 'collapsible-content';

      // Move the <pre> inside the content wrapper.
      pre.replaceWith(wrapper);
      contentWrapper.appendChild(pre);
      wrapper.appendChild(contentWrapper);

      const btn = document.createElement('button');
      btn.className = 'collapsible-toggle';
      const hiddenLines = lineCount - 10;
      btn.textContent = `Show more (${hiddenLines} lines)`;
      btn.addEventListener('click', () => {
        const isCollapsed = wrapper.classList.contains('collapsed');
        if (isCollapsed) {
          wrapper.classList.remove('collapsed');
          btn.textContent = 'Show less';
        } else {
          wrapper.classList.add('collapsed');
          btn.textContent = `Show more (${hiddenLines} lines)`;
        }
      });
      wrapper.appendChild(btn);
    });
  }

  /**
   * Detect Go/generic test output patterns in `<pre>` blocks and wrap them
   * in a colour-coded `.test-results` container.
   */
  private injectTestResults(): void {
    const pres = this.shadowRoot?.querySelectorAll('.md-content pre');
    if (!pres) return;

    pres.forEach((pre) => {
      if (pre.getAttribute('data-test-results') === 'true') return;
      if (pre.closest('.collapsible-block') || pre.closest('.diff-block')) return;
      const codeEl = pre.querySelector('code');
      const text = (codeEl ?? pre).textContent ?? '';
      const lines = text.split('\n');

      // Heuristic: test output if it contains PASS/FAIL/ok lines that look
      // like Go test output or a generic "Tests:" summary line.
      const testIndicators = lines.filter(
        (l) =>
          /^(ok\s|PASS|FAIL|--- PASS|--- FAIL|Tests:)/.test(l.trimStart())
      ).length;
      if (testIndicators < 2) return;

      pre.setAttribute('data-test-results', 'true');

      const container = document.createElement('div');
      container.className = 'test-results';

      for (const line of lines) {
        const span = document.createElement('span');
        span.className = 'test-line';
        const trimmed = line.trimStart();
        if (/^(PASS|--- PASS|ok\s)/.test(trimmed)) {
          span.classList.add('test-pass');
        } else if (/^(FAIL|--- FAIL)/.test(trimmed)) {
          span.classList.add('test-fail');
        } else if (/^Tests:/.test(trimmed)) {
          span.classList.add('test-summary');
        }
        span.textContent = line;
        container.appendChild(span);
      }
      pre.replaceWith(container);
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
      let rendered = renderer.render(this.body);
      rendered = styleMentions(rendered);
      rendered = styleEntityLinks(rendered);
      this.renderedHtml = rendered;
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

  // ---- Phase-3: Action bar and event helpers ----

  /** Render the hover action bar with contextual actions. */
  private renderActionBar() {
    const pinnedClass = this.actionBarPinned ? ' pinned' : '';
    return html`
      <div class="message-actions${pinnedClass}">
        <sl-icon-button
          name="reply"
          label="Reply"
          title="Reply"
          @click=${this.handleReply}
        ></sl-icon-button>
        ${this.isOwn && this.canEdit
          ? html`<sl-icon-button
              name="pencil"
              label="Edit"
              title="Edit"
              @click=${this.handleEdit}
            ></sl-icon-button>`
          : nothing}
        ${this.isOwn && this.canDelete
          ? html`<sl-icon-button
              name="trash"
              label="Delete"
              title="Delete"
              @click=${this.handleDelete}
            ></sl-icon-button>`
          : nothing}
        <sl-icon-button
          name="clipboard"
          label="Copy text"
          title="Copy message"
          @click=${this.handleCopyText}
        ></sl-icon-button>
        <sl-icon-button
          name="link-45deg"
          label="Copy link"
          title="Copy link"
          @click=${this.handleCopyLink}
        ></sl-icon-button>
      </div>
    `;
  }

  /** Render the reply preview block above the bubble content. */
  private renderReplyPreview() {
    if (!this.replyPreview) return nothing;
    const { messageId, senderName, content } = this.replyPreview;
    return html`
      <div class="reply-preview" @click=${() => this.handleScrollToMessage(messageId)}>
        <span class="reply-sender">${senderName}</span>
        <span class="reply-content">${content}</span>
      </div>
    `;
  }

  private handleReply() {
    this.dispatchEvent(
      new CustomEvent('message-reply', {
        bubbles: true,
        composed: true,
        detail: {
          messageId: this.messageId,
          senderName: this.senderName || this.sender,
          content: this.body,
        },
      })
    );
  }

  private handleEdit() {
    this.dispatchEvent(
      new CustomEvent('message-edit', {
        bubbles: true,
        composed: true,
        detail: {
          messageId: this.messageId,
          content: this.body,
        },
      })
    );
  }

  private handleDelete() {
    this.dispatchEvent(
      new CustomEvent('message-delete', {
        bubbles: true,
        composed: true,
        detail: { messageId: this.messageId },
      })
    );
  }

  private handleCopyText() {
    navigator.clipboard.writeText(this.body).catch(() => {
      // Fallback: ignore clipboard failure silently.
    });
  }

  private handleCopyLink() {
    this.dispatchEvent(
      new CustomEvent('message-copy-link', {
        bubbles: true,
        composed: true,
        detail: { messageId: this.messageId },
      })
    );
  }

  private handleScrollToMessage(messageId: string) {
    this.dispatchEvent(
      new CustomEvent('scroll-to-message', {
        bubbles: true,
        composed: true,
        detail: { messageId },
      })
    );
  }

  /** Touch handler for long-press to toggle action bar on mobile. */
  private touchTimer: ReturnType<typeof setTimeout> | null = null;

  private handleTouchStart() {
    this.touchTimer = setTimeout(() => {
      this.actionBarPinned = !this.actionBarPinned;
    }, 500);

    const clearTimer = () => {
      if (this.touchTimer) {
        clearTimeout(this.touchTimer);
        this.touchTimer = null;
      }
      window.removeEventListener('touchend', clearTimer);
      window.removeEventListener('touchmove', clearTimer);
      window.removeEventListener('touchcancel', clearTimer);
    };
    window.addEventListener('touchend', clearTimer, { once: true });
    window.addEventListener('touchmove', clearTimer, { once: true });
    window.addEventListener('touchcancel', clearTimer, { once: true });
  }

  override render() {
    // Full/trace messages render as a collapsed details block.
    if (this.visibility === 'full') {
      return this.renderTraceBlock();
    }

    const dirClass = this.fromAgent ? 'from-agent' : 'from-user';
    const visClass = this.visibility === 'verbose' ? ' verbose' : '';
    const groupClass = !this.showHeader ? ' grouped' : '';
    const isDeleted = !!this.deletedAt;

    return html`
      <div
        class="message-wrapper ${dirClass}${visClass}${groupClass}"
        @touchstart=${this.handleTouchStart}
      >
        ${this.showHeader && this.fromAgent
          ? html`<div class="avatar" style="background: ${this.getAvatarColor()}">
              ${this.getInitials()}
            </div>`
          : this.fromAgent
            ? html`<div class="avatar-spacer"></div>`
            : nothing}
        <div class="bubble">
          ${!isDeleted ? this.renderActionBar() : nothing}
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
                    ? html`<span class="routed-to"> &rarr; ${this.routedTo}</span>`
                    : nothing}
                  <span class="msg-time">${this.formatTime()}</span>
                  ${this.editedAt ? html`<span class="edited-label">(edited)</span>` : nothing}
                </div>
              `
            : nothing}
          ${this.showHeader && !this.fromAgent && this.routedTo
            ? html`
                <div class="bubble-header">
                  <span class="sender-name">${this.senderName || this.sender}</span>
                  <span class="routed-to"> &rarr; ${this.routedTo}</span>
                  <span class="msg-time">${this.formatTime()}</span>
                  ${this.editedAt ? html`<span class="edited-label">(edited)</span>` : nothing}
                </div>
              `
            : nothing}
          ${this.replyPreview ? this.renderReplyPreview() : nothing}
          ${isDeleted
            ? html`<div class="bubble-content"><span class="deleted-message">This message was deleted</span></div>`
            : html`<div class="bubble-content">${this.renderBody()}</div>`}
          ${isDeleted ? nothing : this.renderDeliveryState()}
          ${isDeleted ? nothing : this.renderBadges()}
          ${isDeleted ? nothing : this.renderAttachments()}
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
    const isMd = isMarkdownFile(ref.name);
    const showSource = isMd && (this.mdSourceView.get(ref.id) ?? false);

    // Only fade the bottom edge when the file really does run past the slice
    // and we are NOT showing rendered markdown (rendered view scrolls itself).
    const hasText = state?.status === 'ready' && !!state.text;
    const exceedsLimit =
      hasText &&
      (() => {
        let count = 0;
        let pos = -1;
        const text = state.text!;
        while ((pos = text.indexOf('\n', pos + 1)) !== -1) {
          if (++count >= PREVIEW_VISIBLE_LINES) return true;
        }
        return false;
      })();
    const clipped = !isMd && exceedsLimit;
    const sourceClipped = showSource && exceedsLimit;

    return html`
      <div class="attachment-preview" data-id=${ref.id}>
        <div class="preview-header">
          <span class="preview-filename" title=${ref.name}>${ref.name}</span>
          <span class="preview-size">${formatFileSize(ref.size)}</span>
          <div class="preview-actions">
            ${isMd
              ? html`
                  <sl-icon-button
                    name=${showSource ? 'eye' : 'code'}
                    label=${showSource ? 'Preview' : 'Source'}
                    @click=${() => this.toggleMdSource(ref.id)}
                  ></sl-icon-button>
                `
              : nothing}
            ${isMd && showSource
              ? html`
                  <sl-icon-button
                    name=${this.copiedIds.has(ref.id) ? 'check2' : 'clipboard'}
                    label="Copy to clipboard"
                    @click=${() => this.copyAttachmentText(ref.id)}
                  ></sl-icon-button>
                `
              : nothing}
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
        ${isMd && !showSource
          ? html`<div class="md-preview-body">${this.renderMdPreviewBody(ref, state)}</div>`
          : html`<div class="preview-body ${clipped || sourceClipped ? 'clipped' : ''}">
              ${this.renderPreviewBody(ref, state)}
            </div>`}
      </div>
    `;
  }

  /** Toggle between rendered and source view for a markdown attachment. */
  private toggleMdSource(id: string): void {
    const next = new Map(this.mdSourceView);
    next.set(id, !(next.get(id) ?? false));
    this.mdSourceView = next;
  }

  /** Copy attachment text to clipboard with brief "Copied!" feedback. */
  private async copyAttachmentText(id: string): Promise<void> {
    const state = this.previews.get(id);
    if (!state || state.status !== 'ready' || !state.text) return;
    try {
      await navigator.clipboard.writeText(state.text);
      const next = new Set(this.copiedIds);
      next.add(id);
      this.copiedIds = next;
      const existing = this.copyTimers.get(id);
      if (existing) clearTimeout(existing);
      const timer = setTimeout(() => {
        if (!this.isConnected) return;
        const after = new Set(this.copiedIds);
        after.delete(id);
        this.copiedIds = after;
        this.copyTimers.delete(id);
      }, 1500);
      this.copyTimers.set(id, timer);
    } catch {
      // Clipboard write may fail in insecure contexts; silently ignore.
    }
  }

  /** Rendered markdown body for a markdown attachment preview. */
  private renderMdPreviewBody(_ref: AttachmentRefInfo, state: PreviewState | undefined) {
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
    return html`<scion-markdown-preview .content=${state.text ?? ''}></scion-markdown-preview>`;
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

    const isMd = isMarkdownFile(ref.name);
    const showSource = isMd && (this.mdSourceView.get(ref.id) ?? false);
    const state = this.previews.get(ref.id);

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
          : isMd && !showSource
            ? this.renderMdPreviewBody(ref, state)
            : this.renderPreviewBody(ref, state, true)}
        <div slot="footer" style="display:flex;gap:0.5rem;align-items:center">
          ${isMd
            ? html`
                <sl-button size="small" @click=${() => this.toggleMdSource(ref.id)}>
                  <sl-icon slot="prefix" name=${showSource ? 'eye' : 'code'}></sl-icon>
                  ${showSource ? 'Preview' : 'Source'}
                </sl-button>
              `
            : nothing}
          ${isMd && showSource
            ? html`
                <sl-button size="small" @click=${() => this.copyAttachmentText(ref.id)}>
                  <sl-icon
                    slot="prefix"
                    name=${this.copiedIds.has(ref.id) ? 'check2' : 'clipboard'}
                  ></sl-icon>
                  ${this.copiedIds.has(ref.id) ? 'Copied!' : 'Copy'}
                </sl-button>
              `
            : nothing}
          <sl-button href=${attachmentURL(ref.id)} download=${ref.name}>
            <sl-icon slot="prefix" name="download"></sl-icon>
            Download
          </sl-button>
        </div>
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
