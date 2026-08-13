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
 * - Send on Enter (Shift+Enter for newline)
 * - Right-click send button for "Send with interruption"
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import type { Agent } from '../../../shared/types.js';
import type { MentionAcceptDetail } from './mention-autocomplete.js';
import './mention-autocomplete.js';

/** Maximum message length in rune count. */
const MAX_MESSAGE_LENGTH = 2000;

/** Uploaded attachment info returned from the server. */
export interface UploadedAttachment {
  id: string;
  name: string;
  mime: string;
  size: number;
  url: string;
}

/** Event detail for the chat-send custom event. */
export interface ChatSendDetail {
  text: string;
  plain: boolean;
  interrupt: boolean;
  onSuccess: () => void;
  mentions: string[];
  /** W7: Attachment IDs to include with the message. */
  attachmentIds: string[];
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
 * Count "runes" (user-perceived characters) in a string.
 * Uses Intl.Segmenter where available, falls back to spread length.
 */
function countRunes(text: string): number {
  if (typeof Intl !== 'undefined' && 'Segmenter' in Intl) {
    const segmenter = new Intl.Segmenter('en', { granularity: 'grapheme' });
    let count = 0;
    // eslint-disable-next-line @typescript-eslint/no-unused-vars
    for (const _ of segmenter.segment(text)) count++;
    return count;
  }
  // Fallback: spread into an array (handles surrogate pairs but not all grapheme clusters)
  return [...text].length;
}

@customElement('scion-chat-composer')
export class ScionChatComposer extends LitElement {
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

  @state() private text = '';
  @state() private runeCount = 0;

  /** Whether the right-click send context menu is visible. */
  @state() private showSendContextMenu = false;

  /** Live mention override for the destination chip. */
  @state() private liveMentionOverride = '';

  /** W7: Pending file uploads before send. */
  @state() private pendingFiles: UploadedAttachment[] = [];

  /** W7: Upload in progress. */
  @state() private uploading = false;

  /** Set of accepted mention slugs. Filtered to those still present on send. */
  private acceptedMentions = new Set<string>();

  static override styles = css`
    :host {
      display: block;
    }

    .composer {
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
      padding: 0.75rem 1rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
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
      font-size: 0.875rem;
      border-radius: 0.75rem;
      background: var(--scion-surface-raised, #ffffff);
      border-color: var(--scion-border, #e2e8f0);
    }

    sl-textarea::part(textarea) {
      resize: none;
      color: var(--scion-text, #1e293b);
    }

    sl-textarea::part(form-control) {
      color: var(--scion-text, #1e293b);
    }

    .send-container {
      position: relative;
      flex-shrink: 0;
    }

    .send-btn {
      flex-shrink: 0;
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
      font-size: 0.8125rem;
      cursor: pointer;
      color: var(--scion-text, #1e293b);
      white-space: nowrap;
    }

    .send-context-item:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .footer-row {
      display: flex;
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
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      white-space: nowrap;
    }

    .char-counter {
      font-size: 0.6875rem;
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
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: 0.5rem 0.5rem 0 0;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-bottom: none;
      margin: 0 1rem;
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

    .destination-chip .mention-override {
      font-weight: 600;
      color: var(--scion-warning-600, #d97706);
    }

    .destination-chip.clickable {
      cursor: pointer;
      transition: background 0.15s;
    }

    .destination-chip.clickable:hover {
      background: var(--scion-border, #e2e8f0);
    }

    .chip-chevron {
      font-size: 0.625rem;
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
      font-size: 1rem;
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
      font-size: 0.6875rem;
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
      font-size: 0.875rem;
    }

    .pending-file .remove-btn:hover {
      color: var(--scion-danger-600, #dc2626);
    }

    .upload-progress {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      padding: 0 0.25rem;
    }
  `;

  override render() {
    const isOverLimit = this.runeCount > MAX_MESSAGE_LENGTH;
    const isNearLimit = this.runeCount > MAX_MESSAGE_LENGTH * 0.9;
    const hasContent = this.text.trim().length > 0 || this.pendingFiles.length > 0;
    const canSend = hasContent && !isOverLimit && !this.disabled && !this.uploading;

    const counterClass = isOverLimit ? 'over' : isNearLimit ? 'warn' : '';

    return html`
      ${this.conversationMode ? this.renderDestinationChip() : nothing}
      <div class="composer">
        ${this.pendingFiles.length > 0 ? this.renderPendingFiles() : nothing}
        ${this.uploading ? html`<div class="upload-progress">Uploading...</div>` : nothing}
        <div class="input-row">
          ${this.conversationMode
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
                  accept="image/jpeg,image/png,image/gif,image/webp,application/pdf,text/plain,text/markdown,application/zip"
                  style="display:none"
                  @change=${this.handleFileSelected}
                />
              `
            : nothing}
          <div class="textarea-wrapper">
            <sl-textarea
              placeholder="Send a message..."
              size="small"
              rows="1"
              resize="auto"
              .value=${this.text}
              @sl-input=${this.handleInput}
              @keydown=${this.handleKeydown}
              ?disabled=${this.disabled}
            ></sl-textarea>
            <scion-mention-autocomplete
              .agents=${this.agents}
              .members=${this.members}
              @mention-accept=${this.handleMentionAccept}
            ></scion-mention-autocomplete>
          </div>
          <div class="send-container">
            <sl-button
              class="send-btn"
              size="small"
              variant="primary"
              ?disabled=${!canSend}
              @click=${this.handleSend}
              @contextmenu=${this.handleSendContextMenu}
            >
              <sl-icon slot="prefix" name="send"></sl-icon>
              Send
            </sl-button>
            ${this.showSendContextMenu
              ? html`
                  <div
                    class="send-context-overlay"
                    @click=${this.closeSendContextMenu}
                  ></div>
                  <div class="send-context-menu">
                    <div class="send-context-item" @click=${this.handleSendWithInterrupt}>
                      <sl-icon name="lightning-charge"></sl-icon>
                      Send with interruption
                    </div>
                  </div>
                `
              : nothing}
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
    `;
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

    // Thread mode with live mention override
    if (this.liveMentionOverride) {
      return html`
        <div class="destination-chip">
          <span class="arrow">&rarr;</span>
          <span class="mention-override">@${this.liveMentionOverride}</span>
          <span class="hint">(mention)</span>
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
            <span style="font-size: 0.75rem">\u{1F916}</span>
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
        <sl-menu-label style="padding: 0 var(--sl-spacing-medium);">Set thread default agent</sl-menu-label>
        ${agentMembers.map(
          (m) => html`
            <sl-menu-item value=${m.name} ?checked=${this.defaultAgent === m.name}>
              <span slot="prefix" style="font-size: 1.1em;">\u{1F916}</span>
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
    this.text = target.value;
    this.runeCount = countRunes(this.text);

    // Dispatch typing event so the parent can send a typing indicator
    if (this.text.length > 0) {
      this.dispatchEvent(new CustomEvent('chat-typing', { bubbles: true, composed: true }));
    }

    // Update live mention override for destination chip
    this.updateLiveMentionOverride();

    // Feed the autocomplete component.
    const autocomplete = this.shadowRoot?.querySelector('scion-mention-autocomplete') as
      | import('./mention-autocomplete.js').ScionMentionAutocomplete
      | null;
    if (autocomplete) {
      const textarea = this.getTextareaElement();
      if (textarea) {
        autocomplete.handleInput(this.text, textarea.selectionStart ?? this.text.length, textarea);
      }
    }
  }

  /** Update live mention override based on @mentions in the text. */
  private updateLiveMentionOverride(): void {
    if (!this.conversationMode || this.conversationMode === 'dm') {
      this.liveMentionOverride = '';
      return;
    }
    // Find the first @mention in the text
    const mentionMatch = this.text.match(/@(\S+)/);
    if (mentionMatch) {
      const slug = mentionMatch[1];
      // Check if this matches a known agent
      const matchedAgent = this.agents.find(
        (a) => (a.slug || a.name || '').toLowerCase() === slug.toLowerCase()
      );
      if (matchedAgent) {
        this.liveMentionOverride = matchedAgent.slug || matchedAgent.name || slug;
        return;
      }
    }
    this.liveMentionOverride = '';
  }

  private handleKeydown(e: KeyboardEvent): void {
    // Let the autocomplete handle keys first.
    const autocomplete = this.shadowRoot?.querySelector('scion-mention-autocomplete') as
      | import('./mention-autocomplete.js').ScionMentionAutocomplete
      | null;
    if (autocomplete?.handleKeydown(e)) {
      return; // consumed by autocomplete
    }

    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      this.handleSend();
    }
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

    // Track the accepted mention.
    this.acceptedMentions.add(slug);

    // Restore cursor position after the inserted text.
    const newCursorPos = triggerStart + insertion.length;
    void this.updateComplete.then(() => {
      const ta = this.getTextareaElement();
      if (ta) {
        ta.value = this.text;
        ta.setSelectionRange(newCursorPos, newCursorPos);
        ta.focus();
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
                : html`<sl-icon name="file-earmark" style="font-size:0.875rem"></sl-icon>`}
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
      for (const file of Array.from(files)) {
        formData.append('files', file);
      }

      const { apiFetch } = await import('../../../client/api.js');
      const res = await apiFetch('/api/v1/chat/attachments', {
        method: 'POST',
        body: formData,
      });

      if (!res.ok) {
        const errData = await res.json().catch(() => ({ message: 'Upload failed' }));
        this.dispatchEvent(
          new CustomEvent('composer-error', {
            detail: { message: (errData as Record<string, string>).message || 'Upload failed' },
            bubbles: true,
            composed: true,
          })
        );
        return;
      }

      const data = (await res.json()) as {
        attachments: UploadedAttachment[];
      };
      this.pendingFiles = [...this.pendingFiles, ...data.attachments];
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

  /** Remove a pending file from the list. */
  private removePendingFile(index: number): void {
    this.pendingFiles = this.pendingFiles.filter((_, i) => i !== index);
  }

  private handleSend(): void {
    this.doSend(false);
  }

  /** Send the current message with the given interrupt flag. */
  private doSend(interrupt: boolean): void {
    const trimmed = this.text.trim();
    const hasAttachments = this.pendingFiles.length > 0;
    if ((!trimmed && !hasAttachments) || this.runeCount > MAX_MESSAGE_LENGTH || this.disabled)
      return;

    // Filter accepted mentions to those still literally present in the text.
    const mentions = [...this.acceptedMentions].filter((slug) => trimmed.includes(`@${slug}`));

    // W7: Collect attachment IDs from pending uploads.
    const attachmentIds = this.pendingFiles.map((f) => f.id);

    this.dispatchEvent(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: trimmed,
          plain: false,
          interrupt,
          mentions,
          attachmentIds,
          onSuccess: () => {
            this.text = '';
            this.runeCount = 0;
            this.acceptedMentions.clear();
            this.pendingFiles = [];
            // Restore focus to the textarea after send.
            // Use requestAnimationFrame to ensure the Lit render cycle
            // and Shoelace's internal DOM update have completed.
            void this.updateComplete.then(() => {
              requestAnimationFrame(() => {
                const slTextarea = this.shadowRoot?.querySelector('sl-textarea');
                if (slTextarea) {
                  // Shoelace components expose their own .focus() method.
                  (slTextarea as HTMLElement).focus();
                }
              });
            });
          },
        },
        bubbles: true,
        composed: true,
      })
    );
  }

  /** Show the right-click send context menu. */
  private handleSendContextMenu(e: MouseEvent): void {
    e.preventDefault();
    const trimmed = this.text.trim();
    const hasAttachments = this.pendingFiles.length > 0;
    if ((!trimmed && !hasAttachments) || this.runeCount > MAX_MESSAGE_LENGTH || this.disabled)
      return;
    this.showSendContextMenu = true;
  }

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
