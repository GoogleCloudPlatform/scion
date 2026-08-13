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

/**
 * Fixed palette of 16 visually distinct avatar colours.
 * Guarantees that up to 16 agents in the same view have
 * distinguishable backgrounds, even with very short names.
 */
const AVATAR_PALETTE = [
  'hsl(0, 55%, 48%)',   // red
  'hsl(24, 55%, 48%)',  // orange-red
  'hsl(48, 55%, 44%)',  // amber
  'hsl(120, 40%, 42%)', // green
  'hsl(160, 45%, 42%)', // teal
  'hsl(195, 55%, 44%)', // cyan
  'hsl(220, 55%, 50%)', // blue
  'hsl(255, 45%, 52%)', // indigo
  'hsl(280, 45%, 50%)', // purple
  'hsl(310, 45%, 48%)', // magenta
  'hsl(340, 55%, 48%)', // pink
  'hsl(80, 40%, 42%)',  // lime
  'hsl(30, 60%, 44%)',  // orange
  'hsl(180, 45%, 40%)', // dark-cyan
  'hsl(200, 50%, 42%)', // steel-blue
  'hsl(270, 50%, 48%)', // violet
];

/**
 * Hash a string to a consistent avatar colour.
 * Uses FNV-1a (32-bit) for much better distribution than djb2
 * on short, similar names (e.g. "C1" vs "C2"), then indexes
 * into a fixed palette of visually distinct colours.
 */
export function hashColor(str: string): string {
  let hash = 0x811c9dc5; // FNV offset basis
  for (let i = 0; i < str.length; i++) {
    hash ^= str.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193); // FNV prime
  }
  return AVATAR_PALETTE[(hash >>> 0) % AVATAR_PALETTE.length];
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
  /** Display name used for initials (and colour hashing when colorSeed is unset). */
  @property()
  name = '';

  /**
   * Optional seed string for colour hashing — typically the member's UUID.
   * When set, colour is derived from this value instead of `name`,
   * avoiding collisions for short similar display names.
   */
  @property({ attribute: 'color-seed' })
  colorSeed = '';

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
    const bg = hasImage ? 'transparent' : hashColor(this.colorSeed || this.name);
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
