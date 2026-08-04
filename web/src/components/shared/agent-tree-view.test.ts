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
 * Tests for the <scion-agent-tree-view> keyboard shortcuts:
 *   o     — toggle orientation vertical ⇄ horizontal
 *   f     — fit to view
 *   + / = — zoom in
 *   -     — zoom out
 * Shortcuts must be ignored when a modifier key is held or when the event
 * originates from a text-entry/overlay context (inputs, sl-dialog, etc.).
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeAll, beforeEach, afterEach, vi } from 'vitest';
import './agent-tree-view.js';
import type { ScionAgentTreeView } from './agent-tree-view.js';
import type { Agent } from '../../shared/types.js';

// happy-dom's localStorage is not functional in this setup; the component
// reads/writes the show-users preference from it, so provide a minimal stub.
beforeAll(() => {
  const store = new Map<string, string>();
  vi.stubGlobal('localStorage', {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
  });
});

function agent(id: string, name: string, ancestry?: string[]): Agent {
  return { id, name, ancestry, projectId: 'p1', template: 't', phase: 'running' } as Agent;
}

function pressKey(key: string, init: KeyboardEventInit = {}, target: EventTarget = window): void {
  target.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, composed: true, ...init }));
}

describe('scion-agent-tree-view keyboard shortcuts', () => {
  let el: ScionAgentTreeView;

  beforeEach(async () => {
    el = document.createElement('scion-agent-tree-view') as ScionAgentTreeView;
    el.agents = [agent('r1', 'root', ['user-1']), agent('k1', 'kid', ['user-1', 'r1'])];
    document.body.appendChild(el);
    await el.updateComplete;
  });

  afterEach(() => {
    el.remove();
    document.body.innerHTML = '';
  });

  it('toggles orientation on "t" and dispatches orientation-change', () => {
    let eventOrientation: string | null = null;
    el.addEventListener('orientation-change', e => {
      eventOrientation = (e as CustomEvent<{ orientation: string }>).detail.orientation;
    });

    expect(el.orientation).toBe('vertical');
    pressKey('t');
    expect(el.orientation).toBe('horizontal');
    expect(eventOrientation).toBe('horizontal');
    pressKey('T');
    expect(el.orientation).toBe('vertical');
    expect(eventOrientation).toBe('vertical');
  });

  it('ignores shortcuts when a modifier key is held', () => {
    pressKey('t', { ctrlKey: true });
    pressKey('t', { metaKey: true });
    pressKey('t', { altKey: true });
    expect(el.orientation).toBe('vertical');
  });

  it('ignores shortcuts originating from text-entry targets', () => {
    const input = document.createElement('input');
    document.body.appendChild(input);
    pressKey('t', {}, input);
    expect(el.orientation).toBe('vertical');

    const dialog = document.createElement('sl-dialog');
    const inner = document.createElement('button');
    dialog.appendChild(inner);
    document.body.appendChild(dialog);
    pressKey('t', {}, inner);
    expect(el.orientation).toBe('vertical');

    // Sanity check: the same key from the background still works.
    pressKey('t');
    expect(el.orientation).toBe('horizontal');
  });

  it('zooms in on "+"/"=" and out on "-"', () => {
    // scale is internal state; reach in for assertion purposes only.
    const scaleOf = () => (el as unknown as { scale: number }).scale;
    const initial = scaleOf();
    pressKey('+');
    expect(scaleOf()).toBeCloseTo(initial * 1.25);
    pressKey('=');
    expect(scaleOf()).toBeCloseTo(initial * 1.25 * 1.25);
    pressKey('-');
    expect(scaleOf()).toBeCloseTo(initial * 1.25);
  });

  it('does not throw on "f" (fit) even when the canvas has no size', () => {
    expect(() => pressKey('f')).not.toThrow();
  });

  it('stops handling keys after removal from the DOM', () => {
    el.remove();
    pressKey('t');
    expect(el.orientation).toBe('vertical');
  });
});
