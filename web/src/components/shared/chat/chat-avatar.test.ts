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
 * Tests for avatar colour hashing.
 *
 * Agents in a chat space routinely have very short, near-identical slugs
 * ("c1", "c2", "c3", "c34"), so the sidebar seeds `hashColor` with the
 * member UUID instead. With UUID seeds the remaining cause of duplicate
 * avatar colours is simply how many colours the palette offers — hence the
 * 24-entry palette these tests pin down.
 */

import { describe, it, expect } from 'vitest';
import { AVATAR_PALETTE, hashColor, getInitials } from './chat-avatar.js';

/** Deterministic stand-in for the UUIDs the server assigns to members. */
function uuid(n: number): string {
  const hex = n.toString(16).padStart(12, '0');
  return `3f2a91${hex.slice(0, 2)}-4c1d-4e8b-9a7f-${hex}`;
}

describe('AVATAR_PALETTE', () => {
  it('has 24 unique entries', () => {
    expect(AVATAR_PALETTE).toHaveLength(24);
    expect(new Set(AVATAR_PALETTE).size).toBe(24);
  });

  it('keeps every colour dark enough for white text', () => {
    for (const colour of AVATAR_PALETTE) {
      const lightness = Number(/,\s*(\d+)%\)$/.exec(colour)?.[1]);
      expect(lightness, colour).toBeGreaterThanOrEqual(20);
      expect(lightness, colour).toBeLessThanOrEqual(55);
    }
  });
});

describe('hashColor', () => {
  it('is deterministic for the same seed', () => {
    expect(hashColor(uuid(7))).toBe(hashColor(uuid(7)));
  });

  it('always returns a palette entry', () => {
    for (let i = 0; i < 200; i++) {
      expect(AVATAR_PALETTE).toContain(hashColor(uuid(i)));
    }
  });

  it('gives mostly distinct colours to a sidebar-sized set of member UUIDs', () => {
    // 12 members is a typical busy space. A fixed palette cannot guarantee
    // uniqueness — 24 slots and 12 draws collide sometimes — but the bulk of
    // the space must be separated, which 16 slots failed to manage.
    const colours = new Set(Array.from({ length: 12 }, (_, i) => hashColor(uuid(i))));
    expect(colours.size).toBeGreaterThanOrEqual(10);
  });

  it('uses the whole palette across many seeds', () => {
    const colours = new Set(Array.from({ length: 500 }, (_, i) => hashColor(uuid(i))));
    expect(colours.size).toBe(AVATAR_PALETTE.length);
  });
});

describe('getInitials', () => {
  it('takes the first letter of the first two words', () => {
    expect(getInitials('native-chat-lead')).toBe('NC');
    expect(getInitials('Ada Lovelace')).toBe('AL');
  });

  it('falls back to the first two characters of a single word', () => {
    expect(getInitials('c34')).toBe('C3');
  });

  it('returns a placeholder for an empty name', () => {
    expect(getInitials('')).toBe('?');
  });
});
