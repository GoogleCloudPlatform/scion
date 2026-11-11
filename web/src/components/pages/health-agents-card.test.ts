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
 * Health dashboard Agents card (ptone/scion#3587).
 */

import { describe, it, expect, afterEach } from 'vitest';

import {
  ScionHealthAgentsCard,
  agentRefHref,
  agentRefLabel,
  erroredShare,
  moreCount,
  orderGroups,
  visiblePhases,
  GROUP_LABELS,
  ERRORED_SHARE_LABEL,
  type HealthAgentRef,
  type HealthSummaryAgents,
} from './health-agents-card.js';

function ref(over: Partial<HealthAgentRef> = {}): HealthAgentRef {
  return {
    id: 'a1',
    name: 'worker',
    project_id: 'p1',
    project_slug: 'alpha',
    broker_id: 'b1',
    ...over,
  };
}

function agents(over: Partial<HealthSummaryAgents> = {}): HealthSummaryAgents {
  return {
    total: 10,
    active: 7,
    errored: 2,
    considered: 8,
    by_phase: [
      { phase: 'running', count: 6 },
      { phase: 'stopped', count: 2 },
      { phase: 'error', count: 2 },
    ],
    problems: [],
    ...over,
  };
}

const mounted: HTMLElement[] = [];

async function mount(data: HealthSummaryAgents | null): Promise<ScionHealthAgentsCard> {
  const el = document.createElement('scion-health-agents-card') as ScionHealthAgentsCard;
  el.agents = data;
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

describe('health agents card helpers', () => {
  it('keeps the server phase order and drops zero counts', () => {
    const sent = [
      { phase: 'running', count: 3 },
      { phase: 'stopped', count: 0 },
      { phase: 'error', count: 1 },
      { phase: 'legacy', count: 1 },
    ];
    expect(visiblePhases(sent).map((p) => p.phase)).toEqual(['running', 'error', 'legacy']);
  });

  it('labels the errored group and the errored share differently', () => {
    // The share counts error or crashed; the group counts phase error only.
    expect(GROUP_LABELS.errored).not.toBe(ERRORED_SHARE_LABEL);
    expect(GROUP_LABELS.errored).toBe('Error phase');
    expect(ERRORED_SHARE_LABEL).toBe('Error or crashed');
  });

  it('orders groups errored, crashed, offline and drops empty or unknown ones', () => {
    const g = (kind: string, count = 1) => ({ kind, count, items: [] });
    expect(
      orderGroups([g('offline'), g('stalled'), g('crashed', 0), g('errored'), g('suspended')]).map(
        (x) => x.kind
      )
    ).toEqual(['errored', 'offline']);
  });

  it('labels refs project / agent and links to the agent detail page', () => {
    expect(agentRefLabel(ref())).toBe('alpha / worker');
    expect(agentRefLabel(ref({ project_slug: '' }))).toBe('p1 / worker');
    expect(agentRefLabel(ref({ name: '' }))).toBe('alpha / a1');
    expect(agentRefHref(ref({ id: 'x/y' }))).toBe('/agents/x%2Fy');
  });

  it('counts the agents past the cap', () => {
    expect(moreCount({ kind: 'errored', count: 25, items: Array(20).fill(ref()) })).toBe(5);
    expect(moreCount({ kind: 'errored', count: 1, items: [ref()] })).toBe(0);
  });

  it('formats the errored share of non-stopped agents', () => {
    expect(erroredShare({ errored: 1, considered: 20 })).toBe('1 of 20 (5%)');
    expect(erroredShare({ errored: 1, considered: 300 })).toBe('1 of 300 (<1%)');
    expect(erroredShare({ errored: 0, considered: 5 })).toBe('0 of 5 (0%)');
    expect(erroredShare({ errored: 0, considered: 0 })).toBe('0 of 0');
  });
});

describe('scion-health-agents-card', () => {
  it('shows active vs total and phases in the order the server sends', async () => {
    const el = await mount(agents());
    const root = el.shadowRoot!;
    expect(text(root.querySelector('.active'))).toBe('7 active');
    expect(text(root.querySelector('.total'))).toBe('10 total');
    expect(text(root.querySelector('.errored-share'))).toBe('Error or crashed 2 of 8 (25%)');
    expect(root.querySelector('.errored-share')?.getAttribute('title')).toContain('crashed');
    const phases = [...root.querySelectorAll<HTMLElement>('.phases li')].map(
      (li) => li.dataset.phase
    );
    expect(phases).toEqual(['running', 'stopped', 'error']);
  });

  it('keeps the phase order stable across refreshes of the same order', async () => {
    const el = await mount(agents());
    const order = (): (string | undefined)[] =>
      [...el.shadowRoot!.querySelectorAll<HTMLElement>('.phases li')].map((li) => li.dataset.phase);
    const first = order();
    el.agents = agents({ by_phase: agents().by_phase.map((p) => ({ ...p, count: p.count + 1 })) });
    await el.updateComplete;
    expect(order()).toEqual(first);
  });

  it('renders grouped linked project / agent items with +N more', async () => {
    const items = Array.from({ length: 20 }, (_, i) => ref({ id: `e${i}`, name: `w${i}` }));
    const el = await mount(
      agents({
        problems: [
          {
            kind: 'offline',
            count: 1,
            items: [ref({ id: 'o1', name: 'quiet', project_slug: 'beta' })],
          },
          { kind: 'errored', count: 23, items },
        ],
      })
    );
    const root = el.shadowRoot!;
    const groups = [...root.querySelectorAll<HTMLElement>('.group')];
    expect(groups.map((g) => g.dataset.kind)).toEqual(['errored', 'offline']);
    expect(text(groups[0]!.querySelector('.group-head'))).toBe('Error phase 23');
    expect(groups[0]!.querySelectorAll('a')).toHaveLength(20);
    expect(text(groups[0]!.querySelector('.more'))).toBe('+3 more');
    expect(groups[1]!.querySelector('.more')).toBeNull();
    const link = groups[1]!.querySelector('a')!;
    expect(text(link)).toBe('beta / quiet');
    expect(link.getAttribute('href')).toBe('/agents/o1');
  });

  it('tells same-named agents in different projects apart and links each correctly', async () => {
    const el = await mount(
      agents({
        problems: [
          {
            kind: 'errored',
            count: 2,
            items: [
              ref({ id: 'id-a', name: 'worker', project_id: 'pa', project_slug: 'alpha' }),
              ref({ id: 'id-b', name: 'worker', project_id: 'pb', project_slug: 'beta' }),
            ],
          },
        ],
      })
    );
    const links = [...el.shadowRoot!.querySelectorAll('.group a')].map((a) => [
      text(a),
      a.getAttribute('href'),
    ]);
    expect(links).toEqual([
      ['alpha / worker', '/agents/id-a'],
      ['beta / worker', '/agents/id-b'],
    ]);
  });

  it('says nothing needs attention when there are no problem groups', async () => {
    const el = await mount(agents({ problems: [] }));
    expect(text(el.shadowRoot!.querySelector('.empty'))).toBe('No agents need attention');
  });

  it('never shows stalled or suspended groups', async () => {
    const el = await mount(
      agents({
        problems: [
          { kind: 'stalled', count: 3, items: [ref({ name: 'stuck' })] },
          { kind: 'suspended', count: 1, items: [ref({ name: 'paused' })] },
        ],
      })
    );
    const t = text(el.shadowRoot!);
    expect(t).not.toMatch(/stall/i);
    expect(t).not.toContain('paused');
    expect(t).toContain('No agents need attention');
  });

  it('renders a neutral placeholder when agent data is missing', async () => {
    const el = await mount(null);
    expect(text(el.shadowRoot!.querySelector('.empty'))).toBe('Agent data not available');
  });

  it('uses theme tokens only, with no hex colors', () => {
    const css = ScionHealthAgentsCard.styles.toString();
    expect(css).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
  });
});
