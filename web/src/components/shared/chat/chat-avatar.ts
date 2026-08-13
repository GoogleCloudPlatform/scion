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
 * Shared avatar component for chat: renders initials with a hash-seeded
 * colour, optional image URL, and optional presence indicator.
 *
 * Replaces the duplicated hashColor / getInitials helpers in
 * chat-message.ts, chat-space-rail.ts, and chat.ts.
 *
 * Usage:
 *   <scion-chat-avatar
 *     name="Scout"
 *     size="32"
 *     presenceState="active">
 *   </scion-chat-avatar>
 *
 *   <scion-chat-avatar
 *     name="Alice"
 *     avatarUrl="https://..."
 *     size="36"
 *     presenceState="idle">
 *   </scion-chat-avatar>
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property } from 'lit/decorators.js';

/** Hash a string to a consistent HSL colour. */
export function hashColor(str: string): string {
  let hash = 0;
  for (let i = 0; i < str.length; i++) {
    hash = str.charCodeAt(i) + ((hash << 5) - hash);
  }
  const hue = ((hash % 360) + 360) % 360;
  return `hsl(${hue}, 55%, 48%)`;
}

/** Extract initials from a display name. */
export function getInitials(name: string): string {
  const parts = name.split(/[-_\s]+/).filter(Boolean);
  if (parts.length >= 2) {
    return (parts[0][0] + parts[1][0]).toUpperCase();
  }
  return (name.slice(0, 2) || '?').toUpperCase();
}

@customElement('scion-chat-avatar')
export class ScionChatAvatar extends LitElement {
  /** Display name used for initials and colour hashing. */
  @property()
  name = '';

  /** Optional image URL; when set, renders an <img> instead of initials. */
  @property({ attribute: 'avatar-url' })
  avatarUrl = '';

  /** Size in pixels (width and height). Default 32. */
  @property({ type: Number })
  size = 32;

  /** Optional presence state: "active" shows a green dot, "idle" shows
   *  a moon/sleep overlay. Omit for no indicator. */
  @property({ attribute: 'presence-state' })
  presenceState: 'active' | 'idle' | '' = '';

  static override styles = css`
    :host {
      display: inline-block;
      position: relative;
      flex-shrink: 0;
    }

    .avatar {
      display: flex;
      align-items: center;
      justify-content: center;
      border-radius: 50%;
      color: #fff;
      font-weight: 600;
      user-select: none;
      overflow: hidden;
    }

    .avatar img {
      width: 100%;
      height: 100%;
      object-fit: cover;
      border-radius: 50%;
    }

    /* Presence indicator dot */
    .presence-dot {
      position: absolute;
      bottom: 0;
      right: 0;
      border-radius: 50%;
      border: 2px solid var(--scion-surface, #fff);
      box-sizing: border-box;
    }

    .presence-dot.active {
      background: #22c55e;
    }

    .presence-dot.idle {
      background: #f59e0b;
    }
  `;

  override render() {
    const s = this.size;
    const fontSize = Math.max(10, Math.round(s * 0.4));
    const dotSize = Math.max(8, Math.round(s * 0.3));

    const hasImage = this.avatarUrl && this.avatarUrl.length > 0;
    const bg = hasImage ? 'transparent' : hashColor(this.name);
    const initials = getInitials(this.name);

    return html`
      <div
        class="avatar"
        style="width:${s}px;height:${s}px;font-size:${fontSize}px;background:${bg}"
      >
        ${hasImage ? html`<img src="${this.avatarUrl}" alt="${this.name}" />` : initials}
      </div>
      ${this.presenceState
        ? html`<span
            class="presence-dot ${this.presenceState}"
            style="width:${dotSize}px;height:${dotSize}px"
          ></span>`
        : nothing}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-avatar': ScionChatAvatar;
  }
}
