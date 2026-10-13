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
 * Health dashboard Dispatch card (ptone/scion#3589).
 */

import { describe, it, expect, afterEach } from 'vitest';

import {
  ScionHealthDispatchCard,
  dispatchRows,
  type HealthSummaryDispatch,
} from './health-dispatch-card.js';

function dispatch(over: Partial<HealthSummaryDispatch> = {}): HealthSummaryDispatch {
  return { stuck_messages: 0, stuck_broker_dispatch: 0, failed_broker_dispatch_1h: 0, ...over };
}

const mounted: HTMLElement[] = [];

async function mount(data: HealthSummaryDispatch | null): Promise<ScionHealthDispatchCard> {
  const el = document.createElement('scion-health-dispatch-card') as ScionHealthDispatchCard;
  el.dispatch = data;
  document.body.appendChild(el);
  mounted.push(el);
  await el.updateComplete;
  return el;
}

function text(el: Element | null | undefined): string {
  return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

afterEach(() => {
  while (mounted.length) mounted.pop()?.remove();
});

describe('dispatchRows', () => {
  it('lists the three figures in order with their values', () => {
    const rows = dispatchRows(
      dispatch({ stuck_messages: 3, stuck_broker_dispatch: 2, failed_broker_dispatch_1h: 7 })
    );
    expect(rows.map((r) => [r.key, r.value])).toEqual([
      ['stuck_messages', 3],
      ['stuck_broker_dispatch', 2],
      ['failed_broker_dispatch_1h', 7],
    ]);
    expect(rows.map((r) => r.label)).toEqual([
      'Stuck messages',
      'Stuck broker dispatches',
      'Failed broker dispatches (1h)',
    ]);
  });

  it('warns only on non-zero stuck counts; failures stay neutral', () => {
    expect(dispatchRows(dispatch()).map((r) => r.tone)).toEqual(['neutral', 'neutral', 'neutral']);
    expect(
      dispatchRows(
        dispatch({ stuck_messages: 1, stuck_broker_dispatch: 1, failed_broker_dispatch_1h: 5 })
      ).map((r) => r.tone)
    ).toEqual(['warn', 'warn', 'neutral']);
  });
});

describe('scion-health-dispatch-card', () => {
  it('renders the three numbers from the store', async () => {
    const el = await mount(
      dispatch({ stuck_messages: 4, stuck_broker_dispatch: 0, failed_broker_dispatch_1h: 9 })
    );
    const root = el.shadowRoot!;
    expect(text(root.querySelector('.card-title'))).toBe('Dispatch');
    expect(text(root.querySelector('[data-key="stuck_messages"] .value'))).toBe('4');
    expect(text(root.querySelector('[data-key="stuck_broker_dispatch"] .value'))).toBe('0');
    expect(text(root.querySelector('[data-key="failed_broker_dispatch_1h"] .value'))).toBe('9');
    expect(root.querySelector('.empty')).toBeNull();
  });

  it('highlights a stuck count with the warning badge and leaves zeros plain', async () => {
    const el = await mount(dispatch({ stuck_broker_dispatch: 2, failed_broker_dispatch_1h: 3 }));
    const root = el.shadowRoot!;
    expect(
      root.querySelector('[data-key="stuck_broker_dispatch"] .value.tone-warn')
    ).not.toBeNull();
    expect(root.querySelector('[data-key="stuck_messages"] .value.tone-warn')).toBeNull();
    expect(
      root.querySelector('[data-key="failed_broker_dispatch_1h"] .value.tone-warn')
    ).toBeNull();
  });

  it('renders null as not reported (neutral), never as zeros', async () => {
    const el = await mount(null);
    const root = el.shadowRoot!;
    expect(text(root.querySelector('.empty'))).toBe('Dispatch data not available');
    expect(root.querySelectorAll('li').length).toBe(0);
    expect(root.querySelector('.tone-warn')).toBeNull();
    expect(text(root)).not.toMatch(/\b0\b/);
    expect(text(root)).not.toMatch(/not yet available|future update/i);
  });

  it('uses only theme tokens covered by the light/dark contrast check', () => {
    const cssText = ScionHealthDispatchCard.styles
      ? (Array.isArray(ScionHealthDispatchCard.styles)
          ? ScionHealthDispatchCard.styles
          : [ScionHealthDispatchCard.styles]
        )
          .map((s) => (s as { cssText: string }).cssText)
          .join('\n')
      : '';
    // No hardcoded colours: everything resolves through theme.css.
    expect(cssText).not.toMatch(/#[0-9a-f]{3,8}\b|rgba?\(|hsla?\(/i);
    // Text and badge colours are the pairs health-broker-table.test.ts checks
    // for WCAG AA in both themes.
    const colorVars = [...cssText.matchAll(/(?:^|[\s;{])color:\s*var\((--[\w-]+)\)/g)].map(
      (m) => m[1]
    );
    for (const v of colorVars) {
      expect(['--scion-text', '--scion-text-muted', '--scion-badge-warning-text']).toContain(v);
    }
    expect(colorVars).toContain('--scion-badge-warning-text');
  });
});
