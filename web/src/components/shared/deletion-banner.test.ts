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

// @vitest-environment happy-dom

import { describe, it, expect, afterEach } from 'vitest';
import './deletion-banner.js';
import type { ScionDeletionBanner } from './deletion-banner.js';
import type { DeletionInfo } from '../../shared/types.js';
import { effectiveDeletion } from '../../shared/agent-deletion.js';

const T0 = Date.parse('2026-10-04T12:00:00Z');
const iso = (ms: number): string => new Date(ms).toISOString();

function view(o: Partial<DeletionInfo>): DeletionInfo {
  return { state: 'failed', soft: false, claim: 1, startedAt: iso(T0), ...o };
}

async function mount(
  deletion: DeletionInfo | null,
  props: Partial<Pick<ScionDeletionBanner, 'canDelete' | 'compact' | 'busy' | 'live'>> = {}
): Promise<ScionDeletionBanner> {
  const el = document.createElement('scion-deletion-banner');
  el.deletion = deletion;
  Object.assign(el, { canDelete: true, ...props });
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

const text = (el: ScionDeletionBanner, sel: string): string =>
  (el.shadowRoot?.querySelector(sel)?.textContent ?? '').replace(/\s+/g, ' ').trim();

afterEach(() => {
  document.body.innerHTML = '';
});

describe('scion-deletion-banner', () => {
  it('renders nothing (hidden) for null and for a live deleting view: no Force on deleting', async () => {
    for (const d of [
      null,
      view({ state: 'deleting', leaseExpiresAt: iso(T0 + 60_000) }),
      view({ state: 'deleting', stage: 'finalizing', leaseExpiresAt: iso(T0 + 60_000) }),
    ]) {
      const el = await mount(d);
      expect(el.hasAttribute('hidden')).toBe(true);
      expect(el.shadowRoot?.querySelector('.force')).toBeNull();
      expect(el.shadowRoot?.querySelector('.retry')).toBeNull();
      el.remove();
    }
  });

  const codes: Array<[string, Partial<DeletionInfo>, string, boolean]> = [
    [
      'runtime_error',
      { code: 'runtime_error', error: 'broker refused' },
      'Delete failed: broker refused',
      false,
    ],
    ['conflict', { code: 'conflict' }, 'Delete failed: conflict', false],
    [
      'runtime_unavailable',
      { code: 'runtime_unavailable' },
      'Delete failed: runtime unavailable',
      false,
    ],
    ['abandoned', { code: 'abandoned' }, 'Delete interrupted', false],
    [
      'revoke_failed',
      { code: 'revoke_failed', stage: 'finalizing' },
      'Delete failed: could not revoke credentials',
      true,
    ],
    [
      'finalize_failed',
      { code: 'finalize_failed', stage: 'finalizing' },
      'Delete failed: could not finalize',
      true,
    ],
    ['in_doubt', { code: 'in_doubt' }, 'Delete failed: outcome unknown', true],
    [
      'abandoned while finalizing',
      { code: 'abandoned', stage: 'finalizing' },
      'Delete interrupted',
      true,
    ],
  ];

  for (const [name, o, title, blocked] of codes) {
    it(`${name}: "${title}" with Retry and Force${blocked ? ', and start-blocked text' : ''}`, async () => {
      const el = await mount(view(o));
      expect(el.hasAttribute('hidden')).toBe(false);
      expect(text(el, '.title')).toBe(title);
      expect(el.shadowRoot?.querySelector('.retry')).not.toBeNull();
      expect(el.shadowRoot?.querySelector('.force')).not.toBeNull();
      const detail = text(el, '.detail');
      if (blocked) {
        expect(detail).toMatch(/starting this agent is blocked/i);
        expect(detail).toMatch(/Force delete/);
      } else {
        expect(detail).not.toMatch(/blocked/i);
      }
    });
  }

  it('a client-flipped abandoned view shows Delete interrupted with Retry and Force', async () => {
    const flipped = effectiveDeletion(
      view({ state: 'deleting', leaseExpiresAt: iso(T0 + 20_000) }),
      T0 + 20_001
    );
    const el = await mount(flipped);
    expect(text(el, '.title')).toBe('Delete interrupted');
    expect(el.shadowRoot?.querySelector('.force')).not.toBeNull();
  });

  it('a client-flipped finalizing view keeps the generic text but says start is blocked', async () => {
    const flipped = effectiveDeletion(
      view({ state: 'deleting', stage: 'finalizing', leaseExpiresAt: iso(T0 + 20_000) }),
      T0 + 20_001
    );
    const el = await mount(flipped);
    expect(text(el, '.title')).toBe('Delete interrupted');
    expect(text(el, '.detail')).toMatch(/starting this agent is blocked/i);
    expect(el.shadowRoot?.querySelector('.force')).not.toBeNull();
  });

  it('Retry and Force dispatch their events, without letting the click reach the row', async () => {
    const el = await mount(view({ code: 'runtime_error' }));
    const events: string[] = [];
    el.addEventListener('deletion-retry', () => events.push('retry'));
    el.addEventListener('deletion-force', () => events.push('force'));
    let rowClicks = 0;
    document.body.addEventListener('click', () => rowClicks++);
    (el.shadowRoot?.querySelector('.retry') as HTMLElement).click();
    (el.shadowRoot?.querySelector('.force') as HTMLElement).click();
    expect(events).toEqual(['retry', 'force']);
    expect(rowClicks).toBe(0);
  });

  it('shows text only (no buttons) without the delete capability', async () => {
    const el = await mount(view({ code: 'in_doubt' }), { canDelete: false });
    expect(text(el, '.title')).toBe('Delete failed: outcome unknown');
    expect(el.shadowRoot?.querySelector('.retry')).toBeNull();
    expect(el.shadowRoot?.querySelector('.force')).toBeNull();
  });

  it('busy disables both buttons', async () => {
    const el = await mount(view({ code: 'runtime_error' }), { busy: true });
    expect(el.shadowRoot?.querySelector('.retry')?.hasAttribute('disabled')).toBe(true);
    expect(el.shadowRoot?.querySelector('.force')?.hasAttribute('disabled')).toBe(true);
  });

  it('compact: one line plus the start-blocked hint, full text in title, same buttons', async () => {
    const el = await mount(view({ code: 'in_doubt' }), { compact: true });
    const box = el.shadowRoot?.querySelector('.compact') as HTMLElement;
    expect(box).not.toBeNull();
    expect(text(el, '.title')).toBe('Delete failed: outcome unknown');
    expect(text(el, '.hint')).toMatch(/Start is blocked; Force delete/);
    expect(box.title).toMatch(/starting this agent is blocked/i);
    expect(el.shadowRoot?.querySelector('.retry')).not.toBeNull();
    expect(el.shadowRoot?.querySelector('.force')).not.toBeNull();

    const plain = await mount(view({ code: 'runtime_error', error: 'x' }), { compact: true });
    expect(plain.shadowRoot?.querySelector('.hint')).toBeNull();
  });

  it('compact rows carry the full explanation in a visually hidden span (review nit 2)', async () => {
    const el = await mount(view({ code: 'in_doubt' }), { compact: true });
    const detail = el.shadowRoot?.querySelector('.compact .detail.visually-hidden');
    expect(detail?.textContent).toMatch(/starting this agent is blocked/i);
  });

  it('Retry and Force are labelled with the agent name (review nit 3)', async () => {
    const el = await mount(view({ code: 'runtime_error' }));
    el.agentName = 'alpha';
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.retry')?.getAttribute('aria-label')).toBe(
      'Retry delete of alpha'
    );
    expect(el.shadowRoot?.querySelector('.force')?.getAttribute('aria-label')).toBe(
      'Force delete alpha'
    );
  });

  it('conflict gets code-neutral wording, not "broker error" (review nit 1)', async () => {
    const el = await mount(view({ code: 'conflict' }));
    expect(text(el, '.detail')).not.toMatch(/broker/i);
    expect(text(el, '.detail')).toMatch(/Retry the delete, or force delete/);
  });

  it('role="alert" only when live', async () => {
    const quiet = await mount(view({ code: 'runtime_error' }));
    expect(quiet.shadowRoot?.querySelector('[role="alert"]')).toBeNull();
    const live = await mount(view({ code: 'runtime_error' }), { live: true });
    expect(live.shadowRoot?.querySelector('[role="alert"]')).not.toBeNull();
  });
});
