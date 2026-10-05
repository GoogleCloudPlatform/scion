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
 * Chat composer component.
 *
 * Textarea with:
 * - Character counter (rune-aware via Intl.Segmenter where available)
 * - 2000 character limit with visual feedback (AC10)
 * - Always sends formatted (plain: false)
 * - Sends via `chat-send` custom event: {text, plain, interrupt, mentions}
 * - @-mention autocomplete integration (Phase 4)
 * - The composer knows nothing about the network
 * - Send on Enter (Shift+Enter for newline); on touch-primary devices Enter
 *   inserts a newline instead, since there is no Shift+Enter combo
 * - Right-click send button for "Send with interruption"
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { guard } from 'lit/directives/guard.js';
import { live } from 'lit/directives/live.js';
import type { Agent } from '../../../shared/types.js';
import type { MentionAcceptDetail } from './mention-autocomplete.js';
import type { SlashCommandDetail } from './slash-autocomplete.js';
import { blurElement, focusElement } from '../focus-moved.js';
import './mention-autocomplete.js';
import './slash-autocomplete.js';
import { showToast } from '../../../utils/toast.js';
import { LongPressController } from './long-press.js';
import type { ActionSheetItem, ActionSheetSelectDetail } from './chat-action-sheet.js';
import './chat-action-sheet.js';

/** The touch presentation of the send button's right-click menu. */
const SEND_SHEET_ITEMS: ActionSheetItem[] = [
  { id: 'send-interrupt', label: 'Send with interruption', icon: 'lightning-charge' },
];

/** Maximum message length in rune count. */
const MAX_MESSAGE_LENGTH = 2000;

const GRAPHEME_SEGMENTER =
  typeof Intl !== 'undefined' && 'Segmenter' in Intl
    ? new Intl.Segmenter('en', { granularity: 'grapheme' })
    : null;

/** Pastes exceeding this rune count are auto-converted to text attachments. */
export const PASTE_TO_ATTACHMENT_THRESHOLD = 1000;

/** Uploaded attachment info returned from the server. */
export interface UploadedAttachment {
  id: string;
  name: string;
  mime: string;
  size: number;
  url: string;
}

/**
 * A file the server refused. Uploads are per-file, so a batch can come back
 * part stored and part rejected and the composer has to say which is which.
 */
export interface UploadFailure {
  name: string;
  error: string;
}

/**
 * What the file picker offers. An empty string means "all files" — the
 * server enforces a deny-list of dangerous executable extensions (.exe,
 * .bat, .sh, etc.) and dangerous MIME types (text/html,
 * application/javascript), so the frontend no longer needs to duplicate
 * that logic.  Keeping a restrictive accept list here caused file types
 * the server would happily store (e.g. .tar.gz) to be un-selectable in
 * the file picker (#1156).
 */
export const ATTACHMENT_ACCEPT = '';

/** Event detail for the chat-send custom event. */
export interface ChatSendDetail {
  text: string;
  plain: boolean;
  interrupt: boolean;
  onSuccess: () => void;
  /** Restore composer state on send failure. */
  onError?: (errorMsg: string) => void;
  mentions: string[];
  /** W7: Attachment IDs to include with the message. */
  attachmentIds: string[];
  /** Phase-3: Reply-to message ID. */
  replyToId?: string;
  /** Reply-to content for RE_msg_starting metadata. */
  replyToContent?: string;
}

/** Event detail for the chat-edit custom event (Phase 3). */
export interface ChatEditDetail {
  messageId: string;
  text: string;
}

/** Member info for human mention in v2 mode. */
export interface MemberInfo {
  id: string;
  name: string;
  email: string;
  avatarUrl?: string;
  kind: 'user' | 'agent';
}

/**
 * A span of `this.text` occupied by an accepted mention token, `@slug `
 * (including the trailing space inserted on accept). `start`/`end` are
 * indices into `this.text`, kept in sync as the surrounding text changes.
 */
interface MentionRange {
  start: number;
  end: number;
  slug: string;
}

/**
 * Whether the primary input mechanism does not support hover — i.e. touch is
 * the primary way of interacting with this device. Mice and trackpads
 * support hover; fingers do not. This is more accurate than `ontouchstart`
 * presence checks, which also flag laptops with touchscreens where a mouse
 * or trackpad is still the primary input.
 */
let _isPrimaryInputTouchCached: boolean | undefined;
function isPrimaryInputTouch(): boolean {
  if (_isPrimaryInputTouchCached === undefined) {
    _isPrimaryInputTouchCached =
      typeof window !== 'undefined' && window.matchMedia('(hover: none)').matches;
  }
  return _isPrimaryInputTouchCached;
}

/**
 * Count "runes" (user-perceived characters) in a string.
 * Uses Intl.Segmenter where available, falls back to spread length.
 */
function countRunes(text: string): number {
  if (GRAPHEME_SEGMENTER) {
    let count = 0;
    // eslint-disable-next-line @typescript-eslint/no-unused-vars
    for (const _ of GRAPHEME_SEGMENTER.segment(text)) count++;
    return count;
  }
  // Fallback: spread into an array (handles surrogate pairs but not all grapheme clusters)
  return [...text].length;
}

@customElement('scion-chat-composer')
export class ScionChatComposer extends LitElement {
  // Let capable browsers size the input during their normal layout pass.
  // Older engines retain Shoelace's JS autosizing behavior.
  private readonly nativeTextareaSizing =
    typeof CSS !== 'undefined' && CSS.supports('field-sizing', 'content');

  /** Whether the send button should be disabled (e.g. while sending). */
  @property({ type: Boolean })
  disabled = false;

  /** Agents available for @-mention (passed from parent). */
  @property({ type: Array })
  agents: Agent[] = [];

  // ---- Wave-2 v2 properties ----

  /** Members available for @-mention in v2 mode. */
  @property({ type: Array })
  members: MemberInfo[] = [];

  /** Default agent slug for this thread (v2 mode). */
  @property()
  defaultAgent = '';

  /** Conversation mode: 'thread' or 'dm' (v2 mode). */
  @property()
  conversationMode: 'thread' | 'dm' | '' = '';

  /** DM peer name (v2 DM mode). */
  @property()
  peerName = '';

  /** Project ID for upload authz scope (v2 mode). */
  @property()
  projectId = '';

  // ---- Phase-3 properties ----

  /** Reply-to context: shows a reply preview bar above the input. */
  @property({ type: Object })
  replyTo: { messageId: string; senderName: string; content: string } | null = null;

  /** Edit mode: populates the textarea with existing content. */
  @property({ type: Object })
  editMessage: { messageId: string; content: string } | null = null;

  @state() private text = '';
  @state() private runeCount = 0;

  /** Whether the right-click send context menu is visible. */
  @state() private showSendContextMenu = false;

  /** Whether the send menu is open as an action sheet (a long-press on Send). */
  @state() private showSendSheet = false;

  /** "Send with interruption" was chosen from the sheet; sent once it has closed. */
  private sendInterruptOnSheetClose = false;

  private readonly sendLongPress = new LongPressController(this);

  /** W7: Pending file uploads before send. */
  @state() private pendingFiles: UploadedAttachment[] = [];

  /** W7: Upload in progress. */
  @state() private uploading = false;

  /** Files the last upload refused, shown until dismissed or superseded. */
  @state() private uploadFailures: UploadFailure[] = [];

  /** Whether a drag is currently over the composer drop zone. */
  @state() private dragOver = false;

  /** Conversation key used for draft persistence. */
  @property({ type: String })
  conversationKey = '';

  /** Set of accepted mention slugs. Filtered to those still present on send. */
  private acceptedMentions = new Set<string>();

  /**
   * Ranges of `this.text` occupied by an *accepted* (resolved) mention —
   * `@slug ` including its trailing space, as inserted on accept. Backspace/
   * Delete at the edges of one of these ranges removes the whole token in a
   * single keystroke (#1912). Edits that land inside a range invalidate it —
   * it is no longer a clean resolved mention, so it reverts to plain text
   * that deletes one character at a time like any other typed text.
   */
  private mentionRanges: MentionRange[] = [];

  /** Reset per-message mention bookkeeping (ranges + accepted set). */
  private resetMentionTracking(): void {
    this.mentionRanges = [];
    this.acceptedMentions.clear();
  }

  /** Phase-3 + Phase-4: Handle editMessage and conversationKey changes. */
  override updated(changedProperties: Map<string, unknown>): void {
    super.updated(changedProperties);
    if (changedProperties.has('editMessage') && this.editMessage) {
      this.text = this.editMessage.content;
      this.runeCount = countRunes(this.editMessage.content);
      // Edited content is historical plain text, not a just-accepted
      // mention — it must not be atomically deletable.
      this.resetMentionTracking();
      this.focusTextarea();
    }
    // A new reply target (including switching from one message to another)
    // must move focus into the textarea, caret at the end of the draft, so
    // the user can start typing the reply immediately. Clearing `replyTo`
    // (cancel/send) must not re-steal focus, hence the truthy check.
    if (changedProperties.has('replyTo') && this.replyTo) {
      this.focusTextareaCaretEnd();
    }
    if (changedProperties.has('conversationKey')) {
      // Save the draft for the OLD conversation immediately before switching.
      const oldKey = changedProperties.get('conversationKey') as string;
      if (oldKey) {
        this.flushDraft(oldKey);
      }
      // Reset text so stale content from the old conversation is not carried over.
      this.text = '';
      this.runeCount = 0;
      this.resetMentionTracking();
      this.restoreDraft();
    }
  }

  /** Debounce timer for saving drafts to localStorage. */
  private _draftTimer: ReturnType<typeof setTimeout> | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    /* The rows are spaced by the context rows' own gap and bottom padding
       and by the footer's top margin, not a gap here, so that context rows
       which have shrunk away (see the end of the styles) leave no gap. */
    .composer {
      display: flex;
      flex-direction: column;
      /* The vertical padding halves in a tight keyboard frame (see the
         tight-frame rules below). */
      --composer-pad-block: calc(0.75rem - 0.375rem * var(--scion-chat-tight, 0));
      padding: var(--composer-pad-block) 1rem;
      /* Clear the home indicator (the page uses viewport-fit=cover). While
         the on-screen keyboard is open the composer sits on the keyboard,
         not the screen edge, so the inset is dropped: --scion-kb-open is 1
         then (set on the root by client/viewport.ts) and 0 otherwise. */
      padding-bottom: max(
        var(--composer-pad-block),
        calc(env(safe-area-inset-bottom, 0px) * (1 - var(--scion-kb-open, 0)))
      );
      border-top: 1px solid var(--scion-border, #e2e8f0);
      /* Clear a landscape phone's notch and rounded corners (the page uses
         viewport-fit=cover) on whichever sides this column meets the screen
         edge. Each inset is a transparent border, so the row's background still
         paints to the screen edge and only its content moves in. The chat page
         sets --chat-inset-left and --chat-inset-right for the edges the
         conversation touches; both are 0 everywhere else. */
      border-left: var(--chat-inset-left, 0px) solid transparent;
      border-right: var(--chat-inset-right, 0px) solid transparent;
      background: var(--scion-surface, #ffffff);
    }

    .input-row {
      display: flex;
      align-items: flex-end;
      gap: 0.5rem;
    }

    .textarea-wrapper {
      flex: 1;
      position: relative;
    }

    sl-textarea::part(base) {
      font-size: var(--chat-fs-lg);
      border-radius: 0.75rem;
      background: var(--scion-surface-raised, #ffffff);
      border-color: var(--scion-border, #e2e8f0);
    }

    sl-textarea::part(textarea) {
      resize: none;
      color: var(--scion-text, #1e293b);
    }

    @supports (field-sizing: content) {
      sl-textarea::part(textarea) {
        field-sizing: content;
        min-width: 0;
      }
    }

    sl-textarea::part(form-control) {
      color: var(--scion-text, #1e293b);
    }

    /* On a phone or tablet the field grows with the draft only so far, then
       scrolls inside itself. Uncapped, a long draft outgrows a frame the open
       keyboard has shrunk: the caret ends up below the frame, and iOS pans
       the page to reveal it, which pushes the header off the top and leaves
       an empty band above the keyboard.
       The cap is --composer-field-room, which the thread works out and
       writes on this element (see composer-room.ts for how: it is a
       continuous, growing function of the frame height, whatever chrome is
       showing). Before the thread has measured, or without one, a simple
       curve of the frame applies instead.
       The floor is exactly one line: the line plus the field's 0.5em top
       and bottom padding. Shoelace's line height is 1.4, which gives the
       2.4em fallback for engines without the lh unit; the lh rule is in
       @supports, since a var() in it would otherwise win the cascade and
       drop the cap where lh is unknown.
       The overflow also overrides Shoelace's own overflow-y: hidden for
       resize="auto". */
    @media (max-width: 768px), (pointer: coarse) {
      sl-textarea::part(textarea) {
        --composer-field-cap: var(
          --composer-field-room,
          calc(var(--scion-app-height, 100dvh) * 0.15 + 2.65rem)
        );
        max-height: max(2.4em, var(--composer-field-cap));
        overflow-y: auto;
      }

      @supports (height: 1lh) {
        sl-textarea::part(textarea) {
          max-height: max(calc(1lh + 1em), var(--composer-field-cap));
        }
      }
    }

    /* In a tight keyboard frame inside the chat shell (it publishes
       --scion-chat-tight, --scion-chat-tight-position and
       --scion-chat-tight-visibility from the state client/viewport.ts sets;
       nothing else does), the composer gives the draft and the thread as
       much room as it can: the destination chip and the footer row (the
       character counter) leave the flow, and the padding tightens. They are
       taken out of flow and hidden (visibility: hidden also takes them out
       of the tab order and the accessibility tree) rather than removed, so
       their height stays measurable: tightSavings() reports what they free,
       and the field's cap adds it back, so crossing the tight threshold
       never moves the field. All of it is back as soon as the keyboard
       closes. Each fallback is the element's own value (the footer row's
       are in its own rule below). */
    :host > sl-dropdown {
      position: var(--scion-chat-tight-position, static);
      visibility: var(--scion-chat-tight-visibility, visible);
    }

    :host > .destination-chip {
      position: var(--scion-chat-tight-position, relative);
      visibility: var(--scion-chat-tight-visibility, visible);
    }

    /* Stop iOS/Android focus-zoom: the composer's inner native textarea
       computes at 16px or more on a coarse (touch) pointer, even though
       the Shoelace font-size custom property (set app-wide in critical
       CSS) only reaches ::part(base), not the inner textarea itself. */
    @media (pointer: coarse) {
      sl-textarea::part(textarea) {
        font-size: max(16px, var(--chat-fs-lg));
      }
    }

    @media (max-width: 768px) {
      .attach-btn::part(base) {
        min-height: 44px;
      }

      /* Icon-only on mobile: a square accent button, freeing the width the
         text label used for the textarea. The label stays in the DOM
         (visually hidden, not removed) so the accessible name is still
         "Send" / "Save Edit" without a separate aria-label. */
      .send-btn::part(base) {
        width: 44px;
        height: 44px;
        min-height: 44px;
        padding: 0;
        justify-content: center;
      }

      .send-btn::part(prefix) {
        margin-inline-end: 0;
      }

      /* The label slot wrapper keeps its own padding even though the
         slotted content (the clip-rect-hidden span) collapses to 1x1 —
         without this, the icon sits visibly off-centre in the square
         button instead of in the middle of it. */
      .send-btn::part(label) {
        padding: 0;
      }

      .send-btn sl-icon {
        font-size: 20px;
      }

      .send-btn .send-label {
        position: absolute;
        width: 1px;
        height: 1px;
        overflow: hidden;
        clip: rect(0, 0, 0, 0);
        white-space: nowrap;
      }
    }

    .send-container {
      position: relative;
      flex-shrink: 0;
    }

    .send-btn {
      flex-shrink: 0;
    }

    /* Long-press on Send opens its menu; keep iOS's callout and text
       selection from claiming the press. */
    @media (hover: none) {
      .send-btn {
        -webkit-touch-callout: none;
        -webkit-user-select: none;
        user-select: none;
      }
    }

    .send-context-overlay {
      position: fixed;
      inset: 0;
      z-index: 99;
    }

    .send-context-menu {
      position: absolute;
      bottom: 100%;
      right: 0;
      margin-bottom: 0.25rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.5rem;
      box-shadow: 0 4px 12px rgba(0, 0, 0, 0.12);
      min-width: 180px;
      padding: 0.25rem 0;
      z-index: 100;
    }

    .send-context-item {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.375rem 0.75rem;
      font-size: var(--chat-fs-md);
      cursor: pointer;
      color: var(--scion-text, #1e293b);
      white-space: nowrap;
    }

    .send-context-item:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .composer-context {
      display: flex;
      flex-direction: column;
      justify-content: flex-end;
    }

    .composer-context-rows {
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
      padding-bottom: 0.375rem;
    }

    .footer-row {
      margin-top: 0.375rem;
      display: flex;
      position: var(--scion-chat-tight-position, static);
      visibility: var(--scion-chat-tight-visibility, visible);
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
    }

    .options {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .options label {
      display: flex;
      align-items: center;
      gap: 0.25rem;
      font-size: var(--chat-fs-base);
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      white-space: nowrap;
    }

    .char-counter {
      font-size: var(--chat-fs-sm);
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
    }

    .char-counter.warn {
      color: var(--scion-warning-600, #d97706);
    }

    .char-counter.over {
      color: var(--scion-danger-600, #dc2626);
      font-weight: 600;
    }

    /* Destination chip (v2) */
    .destination-chip {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.25rem 0.75rem;
      font-size: var(--chat-fs-base);
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: 0.5rem 0.5rem 0 0;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-bottom: none;
      /* Keep the tab over the composer's content, which moves in by the
         chat page's side insets (see .composer). */
      margin: 0 calc(1rem + var(--chat-inset-right, 0px)) 0 calc(1rem + var(--chat-inset-left, 0px));
      margin-bottom: -1px;
      position: relative;
      z-index: 1;
    }

    .destination-chip .arrow {
      font-weight: 700;
      color: var(--scion-primary, #3b82f6);
    }

    .destination-chip .agent-name {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .destination-chip .hint {
      font-style: italic;
      opacity: 0.8;
    }

    .destination-chip.clickable {
      cursor: pointer;
      transition: background 0.15s;
    }

    .destination-chip.clickable:hover {
      background: var(--scion-border, #e2e8f0);
    }

    .chip-chevron {
      font-size: var(--chat-fs-xs);
      margin-left: auto;
      opacity: 0.6;
    }

    .destination-chip.dm {
      background: var(--scion-primary-50, #eff6ff);
    }

    /* W7: File upload styles */
    .attach-btn {
      flex-shrink: 0;
    }

    .attach-btn::part(base) {
      font-size: var(--chat-fs-2xl);
    }

    .pending-files {
      display: flex;
      flex-wrap: wrap;
      gap: 0.375rem;
      padding: 0 0.25rem;
    }

    .pending-file {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.25rem 0.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      font-size: var(--chat-fs-sm);
      color: var(--scion-text, #1e293b);
      max-width: 200px;
    }

    .pending-file img {
      width: 24px;
      height: 24px;
      object-fit: cover;
      border-radius: 0.25rem;
    }

    .pending-file .file-name {
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      flex: 1;
    }

    .pending-file .remove-btn {
      cursor: pointer;
      color: var(--scion-text-muted, #94a3b8);
      padding: 0;
      line-height: 1;
      background: none;
      border: none;
      font-size: var(--chat-fs-lg);
    }

    .pending-file .remove-btn:hover {
      color: var(--scion-danger-600, #dc2626);
    }

    .upload-failures {
      display: flex;
      flex-direction: column;
      gap: 0.125rem;
      padding: 0.25rem;
    }

    .upload-failure {
      display: flex;
      align-items: center;
      gap: 0.25rem;
      font-size: var(--chat-fs-sm);
      color: var(--scion-danger-600, #dc2626);
    }

    .upload-failure .failure-name {
      font-weight: 600;
    }

    .upload-failure .dismiss-btn {
      margin-left: auto;
      cursor: pointer;
      color: var(--scion-text-muted, #94a3b8);
      padding: 0;
      line-height: 1;
      background: none;
      border: none;
      font-size: var(--chat-fs-lg);
    }

    .upload-progress {
      font-size: var(--chat-fs-sm);
      color: var(--scion-text-muted, #64748b);
      padding: 0 0.25rem;
    }

    /* ---- Phase-3: Reply preview bar ---- */
    /* While the keyboard is open inside the chat shell (it publishes
       --scion-chat-kb-open) the bar is one line with less padding. Keying
       this to the keyboard rather than to a frame size keeps the bar the
       same height whatever the frame, so the field's cap stays monotonic. */
    .reply-bar {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: calc(0.375rem - 0.25rem * var(--scion-chat-kb-open, 0)) 0.75rem;
      background: var(--scion-surface-50, #f8fafc);
      border-left: 3px solid var(--scion-primary-400, #60a5fa);
      border-radius: 0 0.25rem 0.25rem 0;
      font-size: var(--chat-fs-base);
      color: var(--scion-neutral-600, #475569);
    }

    /* The sender and a one-line excerpt: on two lines normally, side by
       side while the keyboard is open, where the excerpt's basis drops from
       the full width (which wraps it under the sender) to 0. */
    .reply-bar .reply-info {
      flex: 1;
      overflow: hidden;
      display: flex;
      flex-wrap: wrap;
      column-gap: 0.375rem;
    }

    /* While the keyboard is open the sender takes at most half the bar and
       is cut with an ellipsis, so a long name never wraps the bar back to
       two lines. */
    .reply-bar .reply-sender {
      font-weight: 600;
      color: var(--scion-primary-600, #2563eb);
      max-width: calc(100% - 50% * var(--scion-chat-kb-open, 0));
      min-width: 0;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .reply-bar .reply-content {
      flex: 1 1 calc(100% * (1 - var(--scion-chat-kb-open, 0)));
      min-width: 0;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      display: block;
    }

    .reply-bar sl-icon-button::part(base) {
      padding: 0.125rem;
      font-size: var(--chat-fs-base);
      color: var(--scion-neutral-400, #94a3b8);
    }

    /* ---- Phase-3: Edit mode bar ---- */
    .edit-bar {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: calc(0.375rem - 0.25rem * var(--scion-chat-kb-open, 0)) 0.75rem;
      background: var(--scion-warning-50, #fffbeb);
      border-left: 3px solid var(--scion-warning-400, #fbbf24);
      border-radius: 0 0.25rem 0.25rem 0;
      font-size: var(--chat-fs-base);
      color: var(--scion-neutral-600, #475569);
    }

    .edit-bar .edit-info {
      flex: 1;
      font-weight: 600;
    }

    .edit-bar sl-icon-button::part(base) {
      padding: 0.125rem;
      font-size: var(--chat-fs-base);
      color: var(--scion-neutral-400, #94a3b8);
    }

    /* Drop zone overlay */
    .composer-wrapper {
      position: relative;
    }

    .drop-zone-overlay {
      position: absolute;
      inset: 0;
      z-index: 50;
      display: flex;
      align-items: center;
      justify-content: center;
      background: rgba(59, 130, 246, 0.08);
      border: 2px dashed var(--scion-primary, #3b82f6);
      border-radius: 0.75rem;
      pointer-events: none;
    }

    .drop-zone-overlay span {
      font-size: var(--chat-fs-lg);
      font-weight: 600;
      color: var(--scion-primary, #3b82f6);
    }

    /* (These rules come last so they override the base rules above.)
       On a phone or tablet the column can never push the field out of the
       frame: when there is not room for everything, the composer shrinks,
       and inside it the rows above the input row (a reply or edit bar,
       attachments, upload failures) give way together, clipped from the
       top, while the input row keeps its height. Their wrapper has no
       padding or border, so it can give way entirely. */
    @media (max-width: 768px), (pointer: coarse) {
      :host {
        display: flex;
        flex-direction: column;
        min-height: 0;
      }

      .composer-wrapper {
        display: flex;
        flex-direction: column;
        flex: 0 1 auto;
        min-height: 0;
      }

      .composer {
        flex: 0 1 auto;
        min-height: 0;
      }

      /* Clipped from the top when it has to give way: nothing in it
         paints or takes taps outside it. The composer's top padding moves
         inside it while it shows, so a cancel target can use that space
         (see below) without reaching past the clip. */
      .composer-context {
        flex: 0 1 auto;
        min-height: 0;
        overflow: hidden;
      }

      .composer:has(> .composer-context) {
        padding-top: 0;
      }

      .composer-context-rows {
        flex: none;
        padding-top: 0.75rem;
      }

      .input-row {
        flex-shrink: 0;
      }

      /* Attachments are one row that scrolls sideways, whatever the frame,
         so the row's height never changes as the frame does. */
      .pending-files {
        flex-wrap: nowrap;
        overflow-x: auto;
        overflow-y: hidden;
        scrollbar-width: none;
      }

      .pending-file {
        flex: none;
      }

      /* A reply or edit bar's cancel button is a 44px square target with
         the icon at its usual size. The bar (always the first row) is at
         least 44px less the space above it in the wrapper (the composer's
         0.75rem top padding, which stays whole here even in a tight frame)
         and below it (the rows' 0.375rem gap), and the button reaches into
         both with negative margins: the whole target is inside the
         wrapper's clip, over nothing else interactive. padding-top
         re-centres the icon on the bar. */
      .reply-bar,
      .edit-bar {
        min-height: calc(44px - 1.125rem);
        box-sizing: border-box;
      }

      .reply-bar sl-icon-button,
      .edit-bar sl-icon-button {
        flex: none;
        display: inline-flex;
        align-items: center;
        justify-content: center;
        box-sizing: border-box;
        width: 44px;
        height: 44px;
        margin: -0.75rem -0.75rem -0.375rem 0;
        padding-top: 0.375rem;
      }
    }

    /* Touch targets: an attachment's remove button is 44px square on a
       coarse pointer, inside its chip. */
    @media (pointer: coarse) {
      .pending-file {
        min-height: 44px;
        box-sizing: border-box;
      }

      .pending-file .remove-btn {
        min-width: 44px;
        min-height: 44px;
        margin: -0.25rem -0.5rem -0.25rem 0;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    this.restoreDraft();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    // Flush any pending draft so it is not lost when the component unmounts.
    this.flushDraft(this.conversationKey);
  }

  /** Restore a draft from localStorage for the current conversationKey. */
  private restoreDraft(): void {
    if (!this.conversationKey) return;
    try {
      const key = `scion-chat-draft-${this.conversationKey}`;
      const saved = localStorage.getItem(key);
      if (saved !== null) {
        this.text = saved;
        this.runeCount = countRunes(this.text);
      }
    } catch {
      // localStorage may throw in private browsing mode — silently ignore.
    }
  }

  /** Save the current draft to localStorage (debounced). */
  private saveDraft(): void {
    if (!this.conversationKey) return;
    if (this._draftTimer !== null) clearTimeout(this._draftTimer);
    this._draftTimer = setTimeout(() => {
      try {
        const key = `scion-chat-draft-${this.conversationKey}`;
        if (this.text) {
          localStorage.setItem(key, this.text);
        } else {
          localStorage.removeItem(key);
        }
      } catch {
        // localStorage may throw in private browsing mode — silently ignore.
      }
      this._draftTimer = null;
    }, 500);
  }

  /** Clear the draft from localStorage for the current conversationKey. */
  private clearDraft(): void {
    if (this._draftTimer !== null) {
      clearTimeout(this._draftTimer);
      this._draftTimer = null;
    }
    if (!this.conversationKey) return;
    try {
      localStorage.removeItem(`scion-chat-draft-${this.conversationKey}`);
    } catch {
      // localStorage may throw in private browsing mode — silently ignore.
    }
  }

  /**
   * Immediately persist the current draft text under the given key.
   * Cancels any pending debounced save so it is not double-written. (#1152)
   */
  private flushDraft(key: string): void {
    if (this._draftTimer !== null) {
      clearTimeout(this._draftTimer);
      this._draftTimer = null;
    }
    if (!key) return;
    try {
      const storageKey = `scion-chat-draft-${key}`;
      if (this.text) {
        localStorage.setItem(storageKey, this.text);
      } else {
        localStorage.removeItem(storageKey);
      }
    } catch {
      // localStorage may throw in private browsing mode — silently ignore.
    }
  }

  override render() {
    const isOverLimit = this.runeCount > MAX_MESSAGE_LENGTH;
    const isNearLimit = this.runeCount > MAX_MESSAGE_LENGTH * 0.9;
    const hasContent = this.text.trim().length > 0 || this.pendingFiles.length > 0;
    const inEditMode = !!this.editMessage;
    const canSend = hasContent && !isOverLimit && !this.disabled && !this.uploading;

    const counterClass = isOverLimit ? 'over' : isNearLimit ? 'warn' : '';

    // Phase-3: Send button label changes in edit mode.
    const sendLabel = inEditMode ? 'Save Edit' : 'Send';
    const sendIcon = inEditMode ? 'check-lg' : 'send';
    const sendVariant = inEditMode ? 'warning' : 'primary';

    return html`
      ${guard([this.conversationMode, this.peerName, this.defaultAgent, this.members], () =>
        this.conversationMode ? this.renderDestinationChip() : nothing
      )}
      <div
        class="composer-wrapper"
        @dragover=${this.handleDragOver}
        @dragenter=${this.handleDragEnter}
        @dragleave=${this.handleDragLeave}
        @drop=${this.handleDrop}
      >
        ${this.dragOver
          ? html`<div class="drop-zone-overlay"><span>Drop files here</span></div>`
          : nothing}
        <div class="composer">
          ${this.renderContextRows()}
          <div class="input-row">
            ${this.conversationMode && !inEditMode
              ? html`
                  <sl-icon-button
                    class="attach-btn"
                    name="paperclip"
                    label="Attach file"
                    @click=${this.handleAttachClick}
                    ?disabled=${this.disabled || this.uploading}
                  ></sl-icon-button>
                  <input
                    type="file"
                    multiple
                    style="display:none"
                    @change=${this.handleFileSelected}
                  />
                `
              : nothing}
            <div class="textarea-wrapper">
              <sl-textarea
                enterkeyhint="enter"
                placeholder=${inEditMode ? 'Edit your message...' : 'Send a message...'}
                size="small"
                rows="1"
                resize=${this.nativeTextareaSizing ? 'none' : 'auto'}
                .value=${live(this.text)}
                @sl-input=${this.handleInput}
                @keydown=${this.handleKeydown}
                @paste=${this.handlePaste}
                ?disabled=${this.disabled}
              ></sl-textarea>
              <scion-mention-autocomplete
                .agents=${this.agents}
                .members=${this.members}
                @mention-accept=${this.handleMentionAccept}
              ></scion-mention-autocomplete>
              <scion-slash-autocomplete
                @slash-command=${this.handleSlashCommand}
              ></scion-slash-autocomplete>
            </div>
            <div class="send-container">
              <sl-button
                class="send-btn"
                size="small"
                variant=${sendVariant}
                ?disabled=${!canSend}
                @click=${this.handleSend}
                @pointerdown=${this.handleSendPointerDown}
                @contextmenu=${this.handleSendContextMenu}
              >
                <sl-icon slot="prefix" name=${sendIcon}></sl-icon>
                <span class="send-label">${sendLabel}</span>
              </sl-button>
              ${this.showSendContextMenu && !inEditMode
                ? html`
                    <div class="send-context-overlay" @click=${this.closeSendContextMenu}></div>
                    <div class="send-context-menu">
                      <div class="send-context-item" @click=${this.handleSendWithInterrupt}>
                        <sl-icon name="lightning-charge"></sl-icon>
                        Send with interruption
                      </div>
                    </div>
                  `
                : nothing}
              <scion-action-sheet
                heading="Send options"
                .items=${SEND_SHEET_ITEMS}
                .open=${this.showSendSheet && !inEditMode}
                @action-sheet-select=${this.handleSendSheetSelect}
                @action-sheet-close=${this.handleSendSheetClose}
              ></scion-action-sheet>
            </div>
          </div>
          <div class="footer-row">
            ${this.runeCount > 0 || isNearLimit
              ? html`
                  <span class="char-counter ${counterClass}">
                    ${this.runeCount} / ${MAX_MESSAGE_LENGTH}
                  </span>
                `
              : nothing}
          </div>
        </div>
      </div>
    `;
  }

  /**
   * The rows above the input row: a reply or edit bar, attachments, upload
   * failures and progress. They sit in one wrapper so that on a phone or
   * tablet they can give way together (see the end of the styles): the
   * wrapper has no padding or border of its own, so it can shrink to
   * nothing, clipping from the top.
   */
  private renderContextRows(): TemplateResult | typeof nothing {
    const inEditMode = !!this.editMessage;
    if (
      !this.replyTo &&
      !inEditMode &&
      this.pendingFiles.length === 0 &&
      this.uploadFailures.length === 0 &&
      !this.uploading
    ) {
      return nothing;
    }
    return html`
      <div class="composer-context">
        <div class="composer-context-rows">
          ${this.replyTo ? this.renderReplyBar() : nothing}
          ${inEditMode ? this.renderEditBar() : nothing}
          ${this.pendingFiles.length > 0 ? this.renderPendingFiles() : nothing}
          ${this.uploadFailures.length > 0 ? this.renderUploadFailures() : nothing}
          ${this.uploading ? html`<div class="upload-progress">Uploading...</div>` : nothing}
        </div>
      </div>
    `;
  }

  // ---- Phase-3: Reply and edit bar renderers ----

  /** Render the reply preview bar above the composer input. */
  private renderReplyBar() {
    if (!this.replyTo) return nothing;
    return html`
      <div class="reply-bar">
        <div class="reply-info">
          <span class="reply-sender">${this.replyTo.senderName}</span>
          <span class="reply-content">${this.replyTo.content}</span>
        </div>
        <sl-icon-button
          name="x-lg"
          label="Cancel reply"
          @click=${this.cancelReply}
        ></sl-icon-button>
      </div>
    `;
  }

  /** Render the edit mode bar above the composer input. */
  private renderEditBar() {
    return html`
      <div class="edit-bar">
        <span class="edit-info">Editing message</span>
        <sl-icon-button name="x-lg" label="Cancel edit" @click=${this.cancelEdit}></sl-icon-button>
      </div>
    `;
  }

  private cancelReply(): void {
    // `replyTo` is owned by the parent and pushed down as a property. Clearing
    // it here would be undone the moment the parent re-renders for any reason
    // (an inbound message, a typing tick), so ask the parent to clear instead.
    this.dispatchEvent(new CustomEvent('chat-cancel-reply', { bubbles: true, composed: true }));
    this.focusTextarea();
  }

  private cancelEdit(): void {
    // `text`/`runeCount` are local state and stay here; `editMessage` belongs
    // to the parent (see cancelReply).
    this.text = '';
    this.runeCount = 0;
    this.resetMentionTracking();
    this.dispatchEvent(new CustomEvent('chat-cancel-edit', { bubbles: true, composed: true }));
    this.focusTextarea();
  }

  /** Render the destination chip showing where the message will go. */
  private renderDestinationChip() {
    if (this.conversationMode === 'dm') {
      return html`
        <div class="destination-chip dm">
          <span class="arrow">&rarr;</span>
          <span class="agent-name">@${this.peerName}</span>
        </div>
      `;
    }

    // Thread mode: clickable chip to set/change default agent
    const agentMembers = this.members.filter((m) => m.kind === 'agent');
    const hasAgents = agentMembers.length > 0;

    if (this.defaultAgent) {
      return html`
        <sl-dropdown>
          <div class="destination-chip clickable" slot="trigger">
            <span class="arrow">&rarr;</span>
            <span style="font-size: var(--chat-fs-base)">🤖</span>
            <span class="agent-name">${this.defaultAgent}</span>
            <span class="hint">(thread default)</span>
            ${hasAgents
              ? html`<sl-icon name="chevron-down" class="chip-chevron"></sl-icon>`
              : nothing}
          </div>
          ${hasAgents ? this.renderAgentMenu(agentMembers) : nothing}
        </sl-dropdown>
      `;
    }

    // Thread mode with no default
    return html`
      <sl-dropdown>
        <div class="destination-chip clickable" slot="trigger">
          <span class="arrow">&rarr;</span>
          <span class="hint">no agent</span>
          ${hasAgents
            ? html`<sl-icon name="chevron-down" class="chip-chevron"></sl-icon>`
            : nothing}
        </div>
        ${hasAgents ? this.renderAgentMenu(agentMembers) : nothing}
      </sl-dropdown>
    `;
  }

  /** Render the dropdown menu for selecting a default agent. */
  private renderAgentMenu(agentMembers: MemberInfo[]) {
    return html`
      <sl-menu @sl-select=${this.handleAgentMenuSelect}>
        <sl-menu-label style="padding: 0 var(--sl-spacing-medium);"
          >Set thread default agent</sl-menu-label
        >
        ${agentMembers.map(
          (m) => html`
            <sl-menu-item value=${m.name} ?checked=${this.defaultAgent === m.name}>
              <span slot="prefix" style="font-size: 1.1em;">🤖</span>
              ${m.name}
            </sl-menu-item>
          `
        )}
        <sl-divider></sl-divider>
        <sl-menu-item value="__clear__" ?checked=${!this.defaultAgent}>
          <sl-icon slot="prefix" name="x-circle"></sl-icon>
          No agent
        </sl-menu-item>
      </sl-menu>
    `;
  }

  /** Handle agent selection from the dropdown menu. */
  private handleAgentMenuSelect(e: Event): void {
    const detail = (e as CustomEvent<{ item?: HTMLElement }>).detail;
    const item = detail?.item;
    const value = item?.getAttribute('value') || '';
    const newDefault = value === '__clear__' ? '' : value;

    if (newDefault === this.defaultAgent) return;

    this.dispatchEvent(
      new CustomEvent('default-agent-change', {
        detail: { defaultAgent: newDefault },
        bubbles: true,
        composed: true,
      })
    );
  }

  private handleInput(e: Event): void {
    const target = e.target as HTMLInputElement;
    const oldText = this.text;
    const newText = target.value;
    const caretAfterEdit = this.getTextareaElement()?.selectionStart ?? newText.length;
    this.reconcileMentionRangesForEdit(oldText, newText, caretAfterEdit);
    this.text = newText;
    this.runeCount = countRunes(this.text);

    // Persist draft with debounce.
    this.saveDraft();

    // Dispatch typing event so the parent can send a typing indicator
    if (this.text.length > 0) {
      this.dispatchEvent(new CustomEvent('chat-typing', { bubbles: true, composed: true }));
    }

    // Feed the autocomplete components.
    this.notifyMentionAutocompleteOfTextChange();
    this.notifySlashAutocompleteOfTextChange();
  }

  /** Tell the mention-autocomplete component about the current text/cursor. */
  private notifyMentionAutocompleteOfTextChange(): void {
    const autocomplete = this.shadowRoot?.querySelector('scion-mention-autocomplete') as
      | import('./mention-autocomplete.js').ScionMentionAutocomplete
      | null;
    const textarea = this.getTextareaElement();
    if (autocomplete && textarea) {
      autocomplete.handleInput(this.text, textarea.selectionStart ?? this.text.length, textarea);
    }
  }

  /** Tell the slash-autocomplete component about the current text/cursor. */
  private notifySlashAutocompleteOfTextChange(): void {
    const slashAutocomplete = this.shadowRoot?.querySelector('scion-slash-autocomplete') as
      | import('./slash-autocomplete.js').ScionSlashAutocomplete
      | null;
    if (slashAutocomplete) {
      const cursorPos = this.getTextareaElement()?.selectionStart ?? this.text.length;
      slashAutocomplete.handleInput(this.text, cursorPos);
    }
  }

  /**
   * Keep `mentionRanges` in sync with an arbitrary text edit (typing,
   * pasting, selecting-and-replacing, or a native backspace/delete that we
   * did not intercept as an atomic mention delete).
   *
   * Ranges entirely before or after the edited region are unaffected or
   * shifted by the length delta. A range that overlaps the edited region is
   * no longer a clean `@slug ` token — it has been hand-edited — so it is
   * dropped from `mentionRanges` (it loses atomic delete). Its slug is
   * *not* removed from `acceptedMentions` here: only an atomic delete
   * (`deleteMentionRange`) does that. The send-time
   * `trimmed.includes('@slug')` filter in `doSend` already de-routes text
   * that no longer reads `@slug` for any other reason (R1 in #1912 round 2).
   */
  private reconcileMentionRangesForEdit(
    oldText: string,
    newText: string,
    caretAfterEdit: number
  ): void {
    if (this.mentionRanges.length === 0 || oldText === newText) return;

    const maxPrefix = Math.min(oldText.length, newText.length);
    let prefixLen = 0;
    while (prefixLen < maxPrefix && oldText[prefixLen] === newText[prefixLen]) prefixLen++;

    const delta = newText.length - oldText.length;

    // A plain greedy common-prefix/suffix diff can misplace the edit when an
    // inserted or trailing character happens to match the adjacent range's
    // boundary character (e.g. typing "@" right before an accepted "@slug "
    // mention: the prefix match greedily swallows that shared "@", putting
    // the edit *inside* the range instead of outside it). Anchor the diff to
    // where the caret actually ended up: the edit cannot start later than
    // that, less any inserted length, so it never creeps past the real edit
    // point into an adjacent range.
    const caretCap = Math.max(0, caretAfterEdit - Math.max(delta, 0));
    prefixLen = Math.min(prefixLen, caretCap);

    const maxSuffix = maxPrefix - prefixLen;
    let suffixLen = 0;
    while (
      suffixLen < maxSuffix &&
      oldText[oldText.length - 1 - suffixLen] === newText[newText.length - 1 - suffixLen]
    ) {
      suffixLen++;
    }

    const editStart = prefixLen;
    const editOldEnd = oldText.length - suffixLen;

    const survivors: MentionRange[] = [];
    for (const range of this.mentionRanges) {
      if (range.end <= editStart) {
        survivors.push(range);
      } else if (range.start >= editOldEnd) {
        survivors.push({ ...range, start: range.start + delta, end: range.end + delta });
      }
      // else: the edit landed inside this range — drop it from tracking.
    }
    this.mentionRanges = survivors;
  }

  private handleKeydown(e: KeyboardEvent): void {
    // Backspace/Delete right at the edge of an accepted mention removes the
    // whole token atomically, ahead of the slash/mention autocomplete's own
    // key handling (neither of which claims these keys).
    if ((e.key === 'Backspace' || e.key === 'Delete') && this.tryAtomicMentionDelete(e)) {
      return;
    }

    // Let slash command autocomplete handle keys first.
    const slashAutocomplete = this.shadowRoot?.querySelector('scion-slash-autocomplete') as
      | import('./slash-autocomplete.js').ScionSlashAutocomplete
      | null;
    if (slashAutocomplete?.handleKeydown(e)) {
      return; // consumed by slash autocomplete
    }

    // Then let the mention autocomplete handle keys.
    const autocomplete = this.shadowRoot?.querySelector('scion-mention-autocomplete') as
      | import('./mention-autocomplete.js').ScionMentionAutocomplete
      | null;
    if (autocomplete?.handleKeydown(e)) {
      return; // consumed by autocomplete
    }

    // On touch devices there is no Shift+Enter combo to insert a newline, so
    // Enter is left to its default textarea behavior there instead of sending.
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && !isPrimaryInputTouch()) {
      e.preventDefault();
      this.handleSend();
    }
  }

  /**
   * If the caret sits at the edge of an accepted-mention range with no
   * active selection, delete the whole range in one keystroke: Backspace at
   * the range's end, Delete at its start. Returns true if it handled the
   * key (and called preventDefault), so the caller should not fall through
   * to default text-editing behavior.
   */
  private tryAtomicMentionDelete(e: KeyboardEvent): boolean {
    if (e.shiftKey || e.altKey || e.metaKey || e.ctrlKey || e.isComposing) return false;
    if (this.mentionRanges.length === 0) return false;

    const textarea = this.getTextareaElement();
    if (!textarea) return false;

    const selStart = textarea.selectionStart ?? this.text.length;
    const selEnd = textarea.selectionEnd ?? this.text.length;
    if (selStart !== selEnd) return false; // a real selection deletes normally

    const isBackspace = e.key === 'Backspace';
    const range = this.mentionRanges.find((r) =>
      isBackspace ? r.end === selStart : r.start === selStart
    );
    if (!range) return false;

    e.preventDefault();
    this.deleteMentionRange(range);
    return true;
  }

  /** Remove an accepted-mention range from `this.text` in one shot. */
  private deleteMentionRange(range: MentionRange): void {
    const before = this.text.slice(0, range.start);
    const after = this.text.slice(range.end);
    this.text = before + after;
    this.runeCount = countRunes(this.text);

    const delta = range.start - range.end; // negative: text got shorter
    this.mentionRanges = this.mentionRanges
      .filter((r) => r !== range)
      .map((r) =>
        r.start >= range.end ? { ...r, start: r.start + delta, end: r.end + delta } : r
      );

    // Drop the slug from send-time routing unless the text still literally
    // reads `@slug` elsewhere (e.g. a second mention of the same agent, or a
    // hand-edited remnant that lost its tracked range but kept the text).
    // This mirrors the send-time `trimmed.includes('@slug')` filter exactly,
    // so an atomic delete never de-routes an agent the send-time check would
    // still route (and vice versa).
    if (!this.text.includes('@' + range.slug)) {
      this.acceptedMentions.delete(range.slug);
    }

    this.saveDraft();

    const cursorPos = range.start;
    void this.updateComplete.then(() => {
      const ta = this.getTextareaElement();
      if (ta) {
        ta.setSelectionRange(cursorPos, cursorPos);
      }
      // An atomic delete bypasses the native input event both autocomplete
      // components normally listen to. Tell them directly so their own
      // bookkeeping (e.g. a dismissed-trigger position) doesn't go stale
      // relative to the new text, and each stays closed rather than
      // reopening on stale state (#1912).
      this.notifyMentionAutocompleteOfTextChange();
      this.notifySlashAutocompleteOfTextChange();
    });
  }

  private handleSlashCommand(e: CustomEvent<SlashCommandDetail>): void {
    const { command } = e.detail;
    // Extract args from the current text: `/command arg1 arg2`
    const text = this.text.trim();
    const spaceIdx = text.indexOf(' ');
    const args = spaceIdx >= 0 ? text.slice(spaceIdx + 1).trim() : '';

    this.dispatchEvent(
      new CustomEvent('chat-slash-command', {
        detail: { command, args },
        bubbles: true,
        composed: true,
      })
    );

    // Clear the text input after dispatching.
    this.text = '';
    this.runeCount = 0;
    this.resetMentionTracking();
    this.clearDraft();
    this.focusTextarea();
  }

  private handleMentionAccept(e: CustomEvent<MentionAcceptDetail>): void {
    const { slug, triggerStart } = e.detail;
    const textarea = this.getTextareaElement();
    if (!textarea) return;

    // Compute what to replace: from @-trigger to current cursor position.
    const cursorPos = textarea.selectionStart ?? this.text.length;
    const before = this.text.slice(0, triggerStart);
    const after = this.text.slice(cursorPos);
    const insertion = `@${slug} `;

    this.text = before + insertion + after;
    this.runeCount = countRunes(this.text);

    // Shift any existing mention ranges that sit after the replaced region
    // (there's an accepted mention later in the text and this one was
    // inserted before it). The replaced region itself — the `@partial` query
    // just typed — cannot already contain a range: an accepted mention's
    // trailing space would have blocked the trigger from being found there.
    const delta = insertion.length - (cursorPos - triggerStart);
    this.mentionRanges = this.mentionRanges.map((r) =>
      r.start >= cursorPos ? { ...r, start: r.start + delta, end: r.end + delta } : r
    );

    // Track the newly accepted mention, including its trailing space, so
    // Backspace/Delete at its edges can remove it atomically (#1912).
    this.mentionRanges.push({ start: triggerStart, end: triggerStart + insertion.length, slug });
    this.acceptedMentions.add(slug);

    // Restore cursor position after the inserted text. The value itself is
    // already driven by `this.text` via the `live()`-bound textarea — no
    // need (and no longer safe) to also poke the native element's `.value`
    // directly here; doing so bypassed both Lit's and sl-textarea's own
    // dirty-checking and is what made later keystrokes stop visibly
    // updating the input until an unrelated re-render forced a resync
    // (#1912, and the reason #1689 removed the old liveMentionOverride).
    const newCursorPos = triggerStart + insertion.length;
    void this.updateComplete.then(() => {
      const ta = this.getTextareaElement();
      if (ta) {
        ta.setSelectionRange(newCursorPos, newCursorPos);
        focusElement(ta);
      }
    });
  }

  /** Render the pending uploaded files as previews/chips. */
  private renderPendingFiles() {
    return html`
      <div class="pending-files">
        ${this.pendingFiles.map(
          (file, idx) => html`
            <div class="pending-file">
              ${file.mime.startsWith('image/')
                ? html`<img src=${file.url} alt=${file.name} />`
                : html`<sl-icon name="file-earmark" style="font-size:var(--chat-fs-lg)"></sl-icon>`}
              <span class="file-name" title=${file.name}>${file.name}</span>
              <button class="remove-btn" @click=${() => this.removePendingFile(idx)}>
                &times;
              </button>
            </div>
          `
        )}
      </div>
    `;
  }

  /**
   * Render the files the server would not take. A rejected file is not an
   * error about the message — the rest of the batch is still attached — so it
   * belongs next to the attachments rather than in a toast that replaces them.
   */
  private renderUploadFailures(): TemplateResult {
    return html`
      <div class="upload-failures">
        ${this.uploadFailures.map(
          (failure, index) => html`
            <div class="upload-failure">
              <sl-icon name="exclamation-triangle" style="font-size:var(--chat-fs-base)"></sl-icon>
              <span class="failure-name">${failure.name}</span>
              <span class="failure-reason">${failure.error}</span>
              <button
                class="dismiss-btn"
                aria-label="Dismiss ${failure.name}"
                @click=${(): void => this.dismissUploadFailure(index)}
              >
                &times;
              </button>
            </div>
          `
        )}
      </div>
    `;
  }

  /**
   * Dismiss one row. The × sits on the row, so it has to clear that row —
   * clearing the list would silently throw away the failures the user has not
   * read yet, which is the thing this surface exists to prevent.
   */
  private dismissUploadFailure(index: number): void {
    this.uploadFailures = this.uploadFailures.filter((_, i) => i !== index);
  }

  /** Open the hidden file input. */
  private handleAttachClick(): void {
    const input = this.shadowRoot?.querySelector('input[type="file"]') as HTMLInputElement | null;
    if (input) {
      input.value = '';
      input.click();
    }
  }

  /** Handle file selection from the file picker. */
  private async handleFileSelected(e: Event): Promise<void> {
    const input = e.target as HTMLInputElement;
    const files = input.files;
    if (!files || files.length === 0) return;
    await this.uploadFiles(Array.from(files));
  }

  /**
   * Upload one or more files to the attachment endpoint.
   * Shared by the file picker, paste handler, and drag-and-drop handler.
   */
  async uploadFiles(files: File[]): Promise<void> {
    if (files.length === 0) return;

    // Enforce max attachments.
    if (this.pendingFiles.length + files.length > 10) {
      this.dispatchEvent(
        new CustomEvent('composer-error', {
          detail: { message: 'Maximum 10 attachments per message' },
          bubbles: true,
          composed: true,
        })
      );
      return;
    }

    this.uploading = true;
    try {
      const formData = new FormData();
      formData.append('project_id', this.projectId);
      for (const file of files) {
        formData.append('files', file);
      }

      const { apiFetch } = await import('../../../client/api.js');
      const res = await apiFetch('/api/v1/chat/attachments', {
        method: 'POST',
        body: formData,
      });

      const data = (await res.json().catch(() => ({}))) as {
        attachments?: UploadedAttachment[];
        failures?: UploadFailure[];
        // The hub's error helper nests the reason under `error`; a plain
        // `message` is read too so a handler that answers flat is not silently
        // reduced to "Upload failed".
        error?: { message?: string };
        message?: string;
      };

      // The server reports per file: some may be stored while others are
      // refused. Anything it did take is attached, and the refusals are named
      // rather than collapsed into one "upload failed".
      this.uploadFailures = data.failures ?? [];
      if (data.attachments?.length) {
        this.pendingFiles = [...this.pendingFiles, ...data.attachments];
      }

      // A failure with no per-file detail is about the request itself.
      if (!res.ok && this.uploadFailures.length === 0) {
        this.dispatchEvent(
          new CustomEvent('composer-error', {
            detail: { message: data.error?.message || data.message || 'Upload failed' },
            bubbles: true,
            composed: true,
          })
        );
      }
    } catch (err) {
      this.dispatchEvent(
        new CustomEvent('composer-error', {
          detail: { message: err instanceof Error ? err.message : 'Upload failed' },
          bubbles: true,
          composed: true,
        })
      );
    } finally {
      this.uploading = false;
    }
  }

  /** Handle paste events — extract images from clipboard and upload. */
  private handlePaste(e: ClipboardEvent): void {
    const items = e.clipboardData?.items;
    if (!items) return;

    const imageFiles: File[] = [];
    for (const item of Array.from(items)) {
      if (item.type.startsWith('image/')) {
        const file = item.getAsFile();
        if (file) imageFiles.push(file);
      }
    }

    if (imageFiles.length > 0) {
      e.preventDefault();
      void this.uploadFiles(imageFiles);
      return;
    }

    // Auto-convert large text pastes to attachment (skip in edit mode —
    // edits don't support attachments).
    if (!this.editMessage) {
      const pastedText = e.clipboardData?.getData('text/plain');
      if (pastedText && countRunes(pastedText) > PASTE_TO_ATTACHMENT_THRESHOLD) {
        // Don't convert if already at attachment limit — let text enter textarea.
        if (this.pendingFiles.length >= 10) {
          return;
        }
        e.preventDefault();
        const blob = new Blob([pastedText], { type: 'text/plain' });
        const timestamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
        const file = new File([blob], `pasted-text-${timestamp}.txt`, { type: 'text/plain' });
        void this.uploadFiles([file]);
        showToast('Large paste converted to text attachment', 'primary');
      }
    }
  }

  /** Prevent default on dragover to allow drop. */
  private handleDragOver(e: DragEvent): void {
    e.preventDefault();
  }

  /** Show the drop zone overlay on drag enter. */
  private handleDragEnter(e: DragEvent): void {
    e.preventDefault();
    this.dragOver = true;
  }

  /** Hide the drop zone overlay on drag leave. */
  private handleDragLeave(e: DragEvent): void {
    // Only hide if we're leaving the composer-wrapper, not entering a child.
    const wrapper = e.currentTarget as HTMLElement;
    const related = e.relatedTarget as Node | null;
    if (related && wrapper.contains(related)) return;
    this.dragOver = false;
  }

  /** Handle file drop — extract files and upload. */
  private handleDrop(e: DragEvent): void {
    e.preventDefault();
    this.dragOver = false;
    const files = e.dataTransfer?.files;
    if (files && files.length > 0) {
      void this.uploadFiles(Array.from(files));
    }
  }

  /** Remove a pending file from the list. */
  private removePendingFile(index: number): void {
    this.pendingFiles = this.pendingFiles.filter((_, i) => i !== index);
  }

  private handleSend(): void {
    this.doSend(false);
  }

  /** Send the current message with the given interrupt flag. */
  private doSend(interrupt: boolean): void {
    if (!this.hasSendableContent()) return;
    const trimmed = this.text.trim();

    // Phase-3: If in edit mode, dispatch chat-edit instead of chat-send.
    if (this.editMessage) {
      this.dispatchEvent(
        new CustomEvent<ChatEditDetail>('chat-edit', {
          detail: {
            messageId: this.editMessage.messageId,
            text: trimmed,
          },
          bubbles: true,
          composed: true,
        })
      );
      this.text = '';
      this.runeCount = 0;
      this.resetMentionTracking();
      this.dispatchEvent(new CustomEvent('chat-cancel-edit', { bubbles: true, composed: true }));
      this.settleFocusAfterSend();
      return;
    }

    // Filter accepted mentions to those still literally present in the text.
    const mentions = [...this.acceptedMentions].filter((slug) => trimmed.includes(`@${slug}`));

    // W7: Collect attachment IDs from pending uploads.
    const attachmentIds = this.pendingFiles.map((f) => f.id);

    // Save state for error recovery before clearing.
    const savedText = this.text;
    const savedRuneCount = this.runeCount;
    const savedMentions = new Set(this.acceptedMentions);
    const savedMentionRanges = [...this.mentionRanges];
    const savedPendingFiles = [...this.pendingFiles];
    const savedReplyTo = this.replyTo;

    // Phase-3: Build detail with optional replyToId.
    const detail: ChatSendDetail = {
      text: trimmed,
      plain: false,
      interrupt,
      mentions,
      attachmentIds,
      onSuccess: () => {
        // Input already cleared — nothing to do.
      },
      onError: () => {
        // Restore composer state so the user can retry.
        this.text = savedText;
        this.runeCount = savedRuneCount;
        this.acceptedMentions = savedMentions;
        this.mentionRanges = savedMentionRanges;
        this.pendingFiles = savedPendingFiles;
        if (savedReplyTo) {
          this.replyTo = savedReplyTo;
        }
        // A failed send must not pop the keyboard back up on touch; the
        // user taps to retry or edit instead.
        this.settleFocusAfterSend();
      },
    };
    if (this.replyTo) {
      detail.replyToId = this.replyTo.messageId;
      if (this.replyTo.content) {
        detail.replyToContent = this.replyTo.content;
      }
    }

    // Optimistic clear — input empties immediately so the user can type the
    // next message without waiting for the network round-trip.
    // Note: reply-to state is NOT cleared here. The parent (chat-thread)
    // owns composerReplyTo and manages it: clearing on success, restoring
    // on failure. Dispatching chat-cancel-reply here would clear the
    // parent's state prematurely, making it unrecoverable on send failure.
    this.text = '';
    this.runeCount = 0;
    this.resetMentionTracking();
    this.pendingFiles = [];
    this.clearDraft();
    this.settleFocusAfterSend();

    this.dispatchEvent(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail,
        bubbles: true,
        composed: true,
      })
    );
  }

  /**
   * After a send (successful or failed) or a saved edit, touch devices
   * blur the composer so the on-screen keyboard retracts instead of
   * staying up over the thread the user is waiting to read. Desktop keeps
   * today's re-focus, since Enter still sends there and the user is likely
   * to keep typing.
   */
  private settleFocusAfterSend(): void {
    if (isPrimaryInputTouch()) {
      this.blurTextarea();
    } else {
      this.focusTextarea();
    }
  }

  /** Blur the composer's textarea, retracting the on-screen keyboard. */
  private blurTextarea(): void {
    const slTextarea = this.shadowRoot?.querySelector('sl-textarea');
    blurElement(slTextarea as HTMLElement | null);
  }

  /** Focus the textarea after send/cancel. */
  private focusTextarea(): void {
    void this.updateComplete.then(() => {
      requestAnimationFrame(() => {
        const slTextarea = this.shadowRoot?.querySelector('sl-textarea');
        if (slTextarea) {
          // preventScroll: this is not the fix for the off-screen-panel
          // horizontal drift (overflow:clip + inert on the panels is), but it
          // stops the message list from jumping when this runs while the
          // composer's panel isn't the one on screen.
          focusElement(slTextarea as HTMLElement, { preventScroll: true });
        }
      });
    });
  }

  /**
   * Focus the textarea with the caret placed after the last character of the
   * current draft. Used for entry points (e.g. Reply) that must not disturb
   * the existing draft text but still need the caret at a predictable spot
   * so typing continues the message rather than landing mid-draft.
   */
  private focusTextareaCaretEnd(): void {
    void this.updateComplete.then(() => {
      requestAnimationFrame(() => {
        void this.applyCaretEndFocus();
      });
    });
  }

  /**
   * Does the actual work for `focusTextareaCaretEnd()`. If `<sl-textarea>`
   * has not finished its own first render yet — e.g. `replyTo` is already
   * set on initial mount, before the child element has upgraded —
   * `getTextareaElement()` returns null because its shadow DOM doesn't exist
   * yet. In that case, wait once for the child's own `updateComplete` (if it
   * exposes one) and retry before giving up. If the inner textarea is still
   * unavailable, fall back to focusing the `<sl-textarea>` host so focus is
   * not silently dropped.
   */
  private async applyCaretEndFocus(): Promise<void> {
    let ta = this.getTextareaElement();
    if (!ta) {
      const slTextarea = this.shadowRoot?.querySelector('sl-textarea');
      const pendingUpdate = (slTextarea as { updateComplete?: Promise<unknown> } | null)
        ?.updateComplete;
      if (pendingUpdate) {
        await pendingUpdate;
      }
      ta = this.getTextareaElement();
    }
    if (ta) {
      const end = ta.value.length;
      ta.setSelectionRange(end, end);
      focusElement(ta, { preventScroll: true });
      return;
    }
    const slTextarea = this.shadowRoot?.querySelector('sl-textarea');
    if (slTextarea) {
      focusElement(slTextarea as HTMLElement, { preventScroll: true });
    }
  }

  /** Text or attachments, within the length limit, while the composer is enabled. */
  private hasSendableContent(): boolean {
    const hasContent = this.text.trim() !== '' || this.pendingFiles.length > 0;
    return hasContent && this.runeCount <= MAX_MESSAGE_LENGTH && !this.disabled;
  }

  /** Is there something sendable, so the send menu has an action to offer? */
  private canOfferSendMenu(): boolean {
    return this.hasSendableContent() && !this.editMessage;
  }

  /**
   * Show the send menu: the popup for a right-click, the action sheet for
   * the browser's own touch long-press (Android fires `contextmenu` for it).
   */
  private handleSendContextMenu(e: MouseEvent): void {
    const fromTouchPress = this.sendLongPress.pressing;
    if (this.sendLongPress.contextMenu(e)) return;
    e.preventDefault();
    if (!this.hasSendableContent()) return;
    if (fromTouchPress) {
      if (this.canOfferSendMenu()) this.showSendSheet = true;
      return;
    }
    this.showSendContextMenu = true;
  }

  /**
   * A touch long-press on Send opens the send menu as an action sheet. The
   * long-press swallows the press's own click, so it never also sends.
   */
  private readonly handleSendPointerDown = (e: PointerEvent): void => {
    if (!this.canOfferSendMenu()) {
      this.sendLongPress.cancel();
      return;
    }
    this.sendLongPress.pointerDown(e, () => {
      if (!this.canOfferSendMenu()) return;
      this.showSendContextMenu = false;
      this.showSendSheet = true;
    });
  };

  private readonly handleSendSheetSelect = (e: CustomEvent<ActionSheetSelectDetail>): void => {
    if (e.detail.id === 'send-interrupt') this.sendInterruptOnSheetClose = true;
  };

  /**
   * The sheet has closed, by a choice, Cancel, Esc or the backdrop. The
   * send runs only now: closing the dialog hands focus back to whatever
   * had it before (often the textarea), and sending first would let that
   * restore undo the send's touch blur and bring the keyboard back up.
   * Cancel sends nothing and leaves the draft and focus as they were.
   */
  private readonly handleSendSheetClose = (): void => {
    this.showSendSheet = false;
    if (!this.sendInterruptOnSheetClose) return;
    this.sendInterruptOnSheetClose = false;
    this.doSend(true);
  };

  /** Send the message with interruption from the context menu. */
  private handleSendWithInterrupt(): void {
    this.showSendContextMenu = false;
    this.doSend(true);
  }

  /** Close the send context menu. */
  private closeSendContextMenu(): void {
    this.showSendContextMenu = false;
  }

  /**
   * The text field's border-box height in CSS px, or null before it
   * renders. Read by the thread to work out how much room the field has
   * (composer-room.ts).
   */
  fieldHeight(): number | null {
    const field = this.getTextareaElement();
    return field ? field.getBoundingClientRect().height : null;
  }

  /**
   * The height in CSS px the tight-frame compaction frees (see the
   * tight-frame styles): the destination chip and footer row, taken out of
   * flow but still measurable, plus the padding it trims. 0 outside a tight
   * frame. Read by the thread, which adds it back to the field's cap so
   * crossing the tight threshold never moves the field (composer-room.ts).
   */
  tightSavings(): number {
    const root = this.shadowRoot;
    if (!root) return 0;
    const outOfFlow = (el: Element | null | undefined): el is HTMLElement =>
      !!el && getComputedStyle(el).position === 'absolute';
    let saved = 0;
    const chip = [...root.children].find(
      (el) => el.localName === 'sl-dropdown' || el.classList.contains('destination-chip')
    );
    if (outOfFlow(chip)) saved += chip.getBoundingClientRect().height;
    const footer = root.querySelector('.footer-row');
    if (outOfFlow(footer)) {
      saved +=
        footer.getBoundingClientRect().height +
        (parseFloat(getComputedStyle(footer).marginTop) || 0);
    }
    // The padding the tight frame trims: measured against its usual
    // 0.75rem, top (in the context rows while they show) and bottom.
    const tight = parseFloat(getComputedStyle(this).getPropertyValue('--scion-chat-tight')) || 0;
    const column = root.querySelector('.composer');
    if (tight > 0 && column) {
      const rem = parseFloat(getComputedStyle(document.documentElement).fontSize) || 16;
      const rows = root.querySelector('.composer-context-rows');
      const top = parseFloat(getComputedStyle(rows ?? column).paddingTop) || 0;
      const bottom = parseFloat(getComputedStyle(column).paddingBottom) || 0;
      saved += 0.75 * rem - top + (0.75 * rem - bottom);
    }
    return saved;
  }

  /**
   * How much of the composer is cut off when the frame is too short for it,
   * in CSS px: the context rows' height their wrapper clips away, plus any
   * content past the composer's own box. 0 while everything shows. Read by
   * the thread so it sizes the field from the composer's natural height,
   * not the clipped one (composer-room.ts).
   */
  clippedHeight(): number {
    const root = this.shadowRoot;
    if (!root) return 0;
    let clipped = Math.max(0, this.scrollHeight - this.clientHeight);
    const wrapper = root.querySelector('.composer-context');
    const rows = root.querySelector('.composer-context-rows');
    if (wrapper && rows) {
      clipped += Math.max(
        0,
        rows.getBoundingClientRect().height - wrapper.getBoundingClientRect().height
      );
    }
    return clipped;
  }

  /**
   * The field's border-box height in CSS px for `lines` lines of draft: the
   * lines at the field's line height plus its vertical padding. Read by the
   * thread for the field's two-line floor (composer-room.ts).
   */
  fieldLinesHeight(lines: number): number {
    const field = this.getTextareaElement();
    if (!field) return 0;
    const style = getComputedStyle(field);
    const lineHeight = parseFloat(style.lineHeight) || 1.4 * parseFloat(style.fontSize) || 0;
    return (
      lines * lineHeight +
      (parseFloat(style.paddingTop) || 0) +
      (parseFloat(style.paddingBottom) || 0)
    );
  }

  /**
   * Get the underlying HTMLTextAreaElement from the sl-textarea shadow DOM.
   */
  private getTextareaElement(): HTMLTextAreaElement | null {
    const slTextarea = this.shadowRoot?.querySelector('sl-textarea');
    if (!slTextarea) return null;
    // Shoelace sl-textarea wraps a native <textarea> inside its shadow root.
    return (slTextarea.shadowRoot?.querySelector('textarea') as HTMLTextAreaElement) ?? null;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-composer': ScionChatComposer;
  }
}
