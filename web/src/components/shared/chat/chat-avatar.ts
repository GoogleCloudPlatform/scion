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
 * Fixed palette of 24 avatar colours, all legible under white text on a
 * dark surface (lightness stays between 26% and 55%).
 *
 * Built from 12 base hues at 30° spacing, each in two tiers — a mid tier
 * and a deeper, more saturated tier offset by 15° — so that entries which
 * are adjacent in hue still differ clearly in lightness and saturation.
 * The two tiers are interleaved so neighbouring indices are never
 * near-identical either.
 *
 * A 24-entry palette does not make collisions impossible (any hash into a
 * fixed palette collides once the member count grows), but it makes them
 * far rarer for the 8–16 members typically visible in one sidebar.
 */
export const AVATAR_PALETTE = [
  'hsl(0, 58%, 50%)',   // red
  'hsl(15, 70%, 34%)',  // brick
  'hsl(30, 58%, 44%)',  // orange
  'hsl(45, 70%, 30%)',  // bronze
  'hsl(60, 50%, 38%)',  // olive
  'hsl(75, 62%, 26%)',  // dark olive
  'hsl(90, 45%, 38%)',  // lime
  'hsl(105, 60%, 26%)', // forest
  'hsl(120, 42%, 38%)', // green
  'hsl(135, 60%, 26%)', // deep green
  'hsl(150, 45%, 38%)', // emerald
  'hsl(165, 65%, 26%)', // deep emerald
  'hsl(180, 48%, 38%)', // cyan
  'hsl(195, 70%, 28%)', // deep cyan
  'hsl(210, 55%, 45%)', // steel blue
  'hsl(225, 70%, 38%)', // deep blue
  'hsl(240, 55%, 55%)', // blue
  'hsl(255, 65%, 42%)', // indigo
  'hsl(270, 48%, 55%)', // violet
  'hsl(285, 60%, 40%)', // deep violet
  'hsl(300, 45%, 48%)', // magenta
  'hsl(315, 60%, 34%)', // deep magenta
  'hsl(330, 55%, 50%)', // pink
  'hsl(345, 70%, 36%)', // crimson
];

/**
 * Hash a string to a consistent avatar colour.
 *
 * Uses FNV-1a (32-bit), which distributes UUID seeds evenly across the
 * palette. Callers should therefore seed this with a member UUID, not a
 * display name — short similar names ("c1", "c2", "c34") carry too little
 * entropy for any hash to separate reliably.
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
