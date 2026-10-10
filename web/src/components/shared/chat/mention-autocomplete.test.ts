// @vitest-environment happy-dom

import { afterEach, describe, expect, it } from 'vitest';
import {
  ScionMentionAutocomplete,
  clipTopAbove,
  dropdownMaxHeight,
} from './mention-autocomplete.js';

describe('mention candidate deduplication', () => {
  it('keeps the agent mention slug while removing a member with the same normalized slug', () => {
    const autocomplete = new ScionMentionAutocomplete();
    autocomplete.agents = [
      { id: 'agent-1', name: 'Coder One', slug: 'Coder One' },
    ] as typeof autocomplete.agents;
    autocomplete.members = [
      { id: 'member-1', name: 'Coder One', email: 'coder@example.com', kind: 'agent' },
    ];

    const candidates = autocomplete['matchCandidates']('');

    expect(candidates).toHaveLength(1);
    expect(candidates[0].slug).toBe('Coder One');
  });
});

describe('mention dropdown fit', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('fills the room between the composer and the clipping edge above it', () => {
    // 300 - 4px gap - 8px margin - 100 = 188.
    expect(dropdownMaxHeight(300, 100)).toBe(188);
  });

  it('keeps at least one touch-sized row when the room is cramped', () => {
    expect(dropdownMaxHeight(110, 100)).toBe(44);
  });

  it('finds the nearest clipping ancestor across a shadow root', () => {
    const clip = document.createElement('div');
    clip.style.overflowY = 'hidden';
    clip.getBoundingClientRect = () => ({ top: 120 }) as DOMRect;
    const plain = document.createElement('div');
    plain.getBoundingClientRect = () => ({ top: 400 }) as DOMRect;
    const host = document.createElement('div');
    const root = host.attachShadow({ mode: 'open' });
    const anchor = document.createElement('span');
    root.appendChild(anchor);
    plain.appendChild(host);
    clip.appendChild(plain);
    document.body.appendChild(clip);

    // The unclipped ancestor is passed over; the clipping one bounds the room.
    expect(clipTopAbove(anchor)).toBe(120);
  });

  it('is bounded only by the viewport when nothing clips', () => {
    const anchor = document.createElement('span');
    document.body.appendChild(anchor);
    expect(clipTopAbove(anchor)).toBe(window.visualViewport?.offsetTop ?? 0);
  });
});
