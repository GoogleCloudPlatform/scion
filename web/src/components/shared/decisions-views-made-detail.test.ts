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
 * Made detail panel, disclosure and collapsed recommendation marker
 * (ptone/scion#8477, design for ptone/scion#8443 section 4.3).
 */

import { describe, it, expect, vi, afterEach } from 'vitest';
import { html, render } from 'lit';

import type {
  ProjectDecisionLogEntryView,
  ProjectDecisionSummaryView,
} from '../../client/project-decisions.js';
import {
  renderMadeDetailPanel,
  renderMadeItem,
  renderMadeRow,
  renderMadeTableAndMobileList,
  type MadeDisclosure,
} from './decisions-views.js';

const CHOICES = [
  { id: 'eu', label: 'Europe', description: 'Lowest latency for **EU** users' },
  { id: 'us', label: 'United States' },
  { id: 'ap', label: 'Asia Pacific', description: 'Cheapest' },
];

type EntryOverrides = Omit<Partial<ProjectDecisionLogEntryView>, 'decision'> & {
  decision?: Partial<ProjectDecisionSummaryView>;
};

function entry(over: EntryOverrides = {}): ProjectDecisionLogEntryView {
  const { decision: decisionOver, ...rest } = over;
  return {
    answerId: 'ans-1',
    decision: {
      rowId: 'r-1',
      promptMessageId: 'm-1',
      promptAvailable: true,
      key: 'D7',
      ref: 'D7',
      version: 2,
      status: 'decided',
      question: 'Which region?',
      choices: CHOICES,
      defaultChoiceId: 'us',
      askerAgentId: 'a-1',
      askerAgentName: 'infra-bot',
      ...decisionOver,
    },
    choiceId: 'us',
    label: 'United States',
    matchedDefault: true,
    answeredByUserId: 'u-me',
    answeredByName: 'Alice',
    provenance: 'direct',
    outcome: 'answer',
    surface: 'chat',
    at: '2026-11-20T10:00:00Z',
    chatKey: 'topic-infra',
    conversationLabel: '#infra',
    conversationAvailable: true,
    sourceMessageAvailable: false,
    ...rest,
  };
}

let host: HTMLElement | null = null;

function mountTemplate(t: unknown): HTMLElement {
  host = document.createElement('div');
  document.body.appendChild(host);
  render(t, host);
  return host;
}

afterEach(() => {
  host?.remove();
  host = null;
});

function panel(e: ProjectDecisionLogEntryView, projectId = 'p-1'): HTMLElement {
  const root = mountTemplate(renderMadeDetailPanel(e, { id: 'detail-1', projectId }));
  return root.querySelector<HTMLElement>('[data-made-detail]')!;
}

function choiceIds(p: HTMLElement): string[] {
  return Array.from(p.querySelectorAll('[data-detail-choice]')).map(
    (li) => li.getAttribute('data-detail-choice') ?? ''
  );
}

function pickedIds(p: HTMLElement): string[] {
  return Array.from(p.querySelectorAll('[data-detail-choice][data-picked]')).map(
    (li) => li.getAttribute('data-detail-choice') ?? ''
  );
}

function text(el: Element | null | undefined): string {
  return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

function disclosure(
  expanded: string[] = []
): MadeDisclosure & { onToggle: ReturnType<typeof vi.fn> } {
  return { expanded: new Set(expanded), onToggle: vi.fn() };
}

describe('Made detail panel', () => {
  it('lists every offered choice in order with descriptions, recommended tag and pick', () => {
    const p = panel(entry({ choiceId: 'eu', label: 'Europe', matchedDefault: false }));
    expect(p.id).toBe('detail-1');
    expect(choiceIds(p)).toEqual(['eu', 'us', 'ap']);
    expect(pickedIds(p)).toEqual(['eu']);
    const eu = p.querySelector('[data-detail-choice="eu"]')!;
    expect(eu.querySelector('sl-icon[name="check-circle-fill"]')).not.toBeNull();
    expect(text(eu.querySelector('.sr-only'))).toBe('Picked:');
    expect(text(eu.querySelector('[data-detail-desc]'))).toBe('Lowest latency for **EU** users');
    const us = p.querySelector('[data-detail-choice="us"]')!;
    expect(us.hasAttribute('data-recommended')).toBe(true);
    expect(text(us.querySelector('[data-detail-recommended]'))).toBe('recommended');
    expect(us.querySelector('[data-detail-desc]')).toBeNull();
    expect(p.querySelectorAll('[data-detail-recommended]').length).toBe(1);
    expect(p.querySelector('[data-detail-dismissed]')).toBeNull();
  });

  it('marks pick and recommendation on the same choice when they match', () => {
    const p = panel(entry());
    expect(pickedIds(p)).toEqual(['us']);
    const us = p.querySelector('[data-detail-choice="us"]')!;
    expect(us.querySelector('[data-detail-recommended]')).not.toBeNull();
  });

  it('shows no recommended tag without a recommendation', () => {
    const p = panel(entry({ decision: { defaultChoiceId: undefined }, matchedDefault: false }));
    expect(p.querySelector('[data-detail-recommended]')).toBeNull();
  });

  it('dismissed: no choice picked, "Dismissed" with the reason', () => {
    const p = panel(
      entry({
        choiceId: '$dismissed',
        label: '',
        outcome: 'dismiss',
        matchedDefault: false,
        dismissReason: 'No longer relevant',
        decision: { status: 'dismissed' },
      })
    );
    expect(choiceIds(p)).toEqual(['eu', 'us', 'ap']);
    expect(pickedIds(p)).toEqual([]);
    expect(p.querySelector('sl-icon[name="check-circle-fill"]')).toBeNull();
    expect(text(p.querySelector('[data-detail-dismissed]'))).toBe('Dismissed: No longer relevant');
  });

  it('dismissed without a reason says only "Dismissed"', () => {
    const p = panel(entry({ choiceId: '$dismissed', outcome: 'dismiss', matchedDefault: false }));
    expect(text(p.querySelector('[data-detail-dismissed]'))).toBe('Dismissed');
    expect(p.querySelector('[data-detail-dismiss-reason]')).toBeNull();
  });

  it('Other: the Other row is checked with the typed text when picked', () => {
    const p = panel(
      entry({
        choiceId: '$other',
        label: 'Somewhere else',
        otherText: 'South America',
        matchedDefault: false,
        decision: {
          other: { label: 'Somewhere else', description: 'Type a region' },
        },
      })
    );
    expect(choiceIds(p)).toEqual(['eu', 'us', 'ap', '$other']);
    expect(pickedIds(p)).toEqual(['$other']);
    const other = p.querySelector('[data-detail-choice="$other"]')!;
    expect(text(other.querySelector('[data-detail-other-text]'))).toBe('“South America”');
    expect(text(other.querySelector('[data-detail-desc]'))).toBe('Type a region');
  });

  it('Other offered but not picked: unchecked, no typed text', () => {
    const p = panel(entry({ decision: { other: { label: 'Somewhere else' } } }));
    const other = p.querySelector('[data-detail-choice="$other"]')!;
    expect(other.hasAttribute('data-picked')).toBe(false);
    expect(other.querySelector('[data-detail-other-text]')).toBeNull();
    expect(pickedIds(p)).toEqual(['us']);
  });

  it('renders descriptions and Other text as plain text', () => {
    const hostile = '<img src=x onerror="window.__pwned=1"><b>bold</b> `code`';
    const p = panel(
      entry({
        choiceId: '$other',
        label: 'Other',
        otherText: hostile,
        matchedDefault: false,
        decision: {
          choices: [{ id: 'eu', label: '<i>Europe</i>', description: hostile }],
          defaultChoiceId: 'eu',
          other: { label: 'Other', description: hostile },
        },
      })
    );
    expect(p.querySelector('img')).toBeNull();
    expect(p.querySelector('i')).toBeNull();
    expect(p.querySelector('[data-detail-desc] b')).toBeNull();
    expect(p.querySelector('[data-detail-desc] code')).toBeNull();
    expect(text(p.querySelector('[data-detail-choice="eu"] [data-detail-desc]'))).toBe(hostile);
    expect(text(p.querySelector('[data-detail-other-text]'))).toBe(`“${hostile}”`);
    expect(text(p.querySelector('[data-detail-choice="eu"] .lbl'))).toBe('<i>Europe</i>');
  });

  it('shows "was interpreted as X" from the folded interpreted answer', () => {
    const p = panel(
      entry({
        outcome: 'override',
        choiceId: 'eu',
        label: 'Europe',
        matchedDefault: false,
        interpretedAnswer: {
          choiceId: 'ap',
          label: 'Asia Pacific',
          provenance: 'agent_interpreted',
          surface: 'chat',
          responseMessageId: '',
          at: '2026-11-20T09:00:00Z',
        },
      })
    );
    expect(text(p.querySelector('[data-detail-interpreted]'))).toBe(
      'Was interpreted as Asia Pacific'
    );
  });

  it('shows the interpreted Other text and keeps it on a dismiss entry', () => {
    const p = panel(
      entry({
        choiceId: '$dismissed',
        outcome: 'dismiss',
        matchedDefault: false,
        interpretedAnswer: {
          choiceId: '$other',
          label: 'Somewhere else',
          otherText: 'Mars',
          provenance: 'agent_interpreted',
          surface: 'chat',
          responseMessageId: '',
          at: '2026-11-20T09:00:00Z',
        },
      })
    );
    expect(pickedIds(p)).toEqual([]);
    expect(text(p.querySelector('[data-detail-interpreted]'))).toBe(
      'Was interpreted as Somewhere else: “Mars”'
    );
    expect(p.querySelector('[data-detail-dismissed]')).not.toBeNull();
  });

  it('shows "Superseded by vN" from replacedBy', () => {
    const p = panel(
      entry({ decision: { status: 'replaced', replacedBy: { rowId: 'r-2', version: 3 } } })
    );
    expect(text(p.querySelector('[data-detail-superseded]'))).toBe('Superseded by v3');
    expect(panel(entry()).querySelector('[data-detail-superseded]')).toBeNull();
  });

  it('links "Open in chat" to the prompt only when the prompt is available', () => {
    const live = panel(entry());
    const link = live.querySelector<HTMLAnchorElement>('[data-detail-open-in-chat]')!;
    expect(link).not.toBeNull();
    expect(link.getAttribute('href')).toMatch(/#msg-m-1$/);
    expect(link.getAttribute('href')).toContain('p-1');

    expect(
      panel(entry({ decision: { promptAvailable: false } })).querySelector(
        '[data-detail-open-in-chat]'
      )
    ).toBeNull();
    expect(
      panel(entry({ conversationAvailable: false })).querySelector('[data-detail-open-in-chat]')
    ).toBeNull();
    expect(panel(entry(), '').querySelector('[data-detail-open-in-chat]')).toBeNull();
  });

  it('says choices are not available when the entry carries none', () => {
    const p = panel(entry({ decision: { choices: undefined } }));
    expect(p.querySelector('[data-detail-no-choices]')).not.toBeNull();
    expect(p.querySelector('ul')).toBeNull();
  });
});

describe('Made disclosure', () => {
  function row(e: ProjectDecisionLogEntryView, d?: MadeDisclosure): HTMLElement {
    return mountTemplate(
      html`<table>
        <tbody>
          ${renderMadeRow(e, {
            currentUserId: 'u-me',
            showProjectColumn: false,
            defaultProjectId: 'p-1',
            disclosure: d,
          })}
        </tbody>
      </table>`
    );
  }

  it('is a native button with aria-expanded/aria-controls that calls onToggle', () => {
    const d = disclosure();
    const root = row(entry(), d);
    const btn = root.querySelector<HTMLButtonElement>('button[data-made-toggle="ans-1"]')!;
    expect(btn.type).toBe('button');
    expect(btn.getAttribute('aria-expanded')).toBe('false');
    expect(btn.getAttribute('aria-controls')).toBe('made-detail-ans-1');
    expect(btn.getAttribute('aria-label')).toBe('Show choices for D7 v2');
    expect(root.querySelector('[data-detail-row]')).toBeNull();
    btn.click();
    expect(d.onToggle).toHaveBeenCalledWith('ans-1');
  });

  it('renders the panel in a full-width row after the entry when expanded', () => {
    const root = row(entry(), disclosure(['ans-1']));
    const btn = root.querySelector<HTMLButtonElement>('button[data-made-toggle]')!;
    expect(btn.getAttribute('aria-expanded')).toBe('true');
    expect(btn.getAttribute('aria-label')).toBe('Hide choices for D7 v2');
    const rows = Array.from(root.querySelectorAll('tbody > tr'));
    expect(
      rows.map((r) => r.getAttribute('data-answer-row') ?? r.getAttribute('data-detail-row'))
    ).toEqual(['ans-1', 'ans-1']);
    const td = rows[1]!.querySelector('td')!;
    expect(td.getAttribute('colspan')).toBe('8');
    expect(td.querySelector('#made-detail-ans-1[data-made-detail]')).not.toBeNull();
  });

  it('spans the project column too on the profile table', () => {
    const root = mountTemplate(
      html`<table>
        <tbody>
          ${renderMadeRow(entry(), {
            currentUserId: 'u-me',
            showProjectColumn: true,
            disclosure: disclosure(['ans-1']),
          })}
        </tbody>
      </table>`
    );
    expect(root.querySelector('[data-detail-row] td')?.getAttribute('colspan')).toBe('9');
  });

  it('renders no toggle without a disclosure', () => {
    expect(row(entry()).querySelector('[data-made-toggle]')).toBeNull();
  });

  it('mobile item: toggle and panel with their own ids', () => {
    const d = disclosure(['ans-1']);
    const root = mountTemplate(
      renderMadeItem(entry(), {
        currentUserId: 'u-me',
        includeProjectInMeta: false,
        defaultProjectId: 'p-1',
        disclosure: d,
      })
    );
    const btn = root.querySelector<HTMLButtonElement>('button[data-made-toggle]')!;
    expect(btn.getAttribute('aria-controls')).toBe('made-detail-m-ans-1');
    expect(btn.getAttribute('aria-expanded')).toBe('true');
    expect(root.querySelector('#made-detail-m-ans-1[data-made-detail]')).not.toBeNull();
  });

  it('table and mobile list share the expand state', () => {
    const root = mountTemplate(
      renderMadeTableAndMobileList([entry(), entry({ answerId: 'ans-2' })], {
        currentUserId: 'u-me',
        showProjectColumn: false,
        defaultProjectId: 'p-1',
        disclosure: disclosure(['ans-2']),
      })
    );
    expect(Array.from(root.querySelectorAll('[data-made-detail]')).map((p) => p.id)).toEqual([
      'made-detail-ans-2',
      'made-detail-m-ans-2',
    ]);
  });
});

describe('Made collapsed recommendation marker', () => {
  function markers(e: ProjectDecisionLogEntryView): { desktop: string; mobile: string } {
    const desktop = mountTemplate(
      html`<table>
        <tbody>
          ${renderMadeRow(e, {
            currentUserId: 'u-me',
            showProjectColumn: false,
            defaultProjectId: 'p-1',
          })}
        </tbody>
      </table>`
    );
    const d = text(desktop.querySelector('[data-provenance] [data-rec-marker]'));
    desktop.remove();
    const mobile = mountTemplate(
      renderMadeItem(e, {
        currentUserId: 'u-me',
        includeProjectInMeta: false,
        defaultProjectId: 'p-1',
      })
    );
    return { desktop: d, mobile: text(mobile.querySelector('[data-rec-marker]')) };
  }

  const cases: Array<[string, EntryOverrides, string]> = [
    ['plain answer matching', {}, 'recommended'],
    [
      'plain answer not matching',
      { choiceId: 'eu', label: 'Europe', matchedDefault: false },
      'recommended: United States',
    ],
    ['confirmed matching', { outcome: 'confirm' }, 'recommended'],
    [
      'changed away from the recommendation',
      {
        outcome: 'override',
        choiceId: 'ap',
        label: 'Asia Pacific',
        matchedDefault: false,
        previousChoiceLabel: 'United States',
      },
      'recommended: United States',
    ],
    [
      'interpreted awaiting confirmation',
      {
        provenance: 'agent_interpreted',
        choiceId: 'eu',
        label: 'Europe',
        matchedDefault: false,
        awaitingConfirmation: true,
      },
      'recommended: United States',
    ],
    [
      'Other answer',
      { choiceId: '$other', label: 'Other', otherText: 'Mars', matchedDefault: false },
      'recommended: United States',
    ],
    ['dismissed', { choiceId: '$dismissed', outcome: 'dismiss', matchedDefault: false }, ''],
    [
      'no recommendation',
      {
        matchedDefault: false,
        choiceId: 'eu',
        label: 'Europe',
        decision: { defaultChoiceId: undefined },
      },
      '',
    ],
  ];

  for (const [name, over, want] of cases) {
    it(`${name}: "${want || 'none'}" on desktop and mobile`, () => {
      expect(markers(entry(over))).toEqual({ desktop: want, mobile: want });
    });
  }

  it('falls back to the recommended id when its label is unknown', () => {
    expect(
      markers(
        entry({
          choiceId: 'eu',
          matchedDefault: false,
          decision: { choices: [], defaultChoiceId: 'gone' },
        })
      ).desktop
    ).toBe('recommended: gone');
  });

  it('keeps the reply link and "from X" alongside the marker', () => {
    const root = mountTemplate(
      html`<table>
        <tbody>
          ${renderMadeRow(
            entry({
              outcome: 'override',
              choiceId: 'eu',
              label: 'Europe',
              matchedDefault: false,
              previousChoiceLabel: 'Asia Pacific',
            }),
            { currentUserId: 'u-me', showProjectColumn: false, defaultProjectId: 'p-1' }
          )}
        </tbody>
      </table>`
    );
    expect(text(root.querySelector('[data-provenance]'))).toBe(
      'changed from Asia Pacific recommended: United States'
    );
  });
});

describe('Made awaiting confirmation badge', () => {
  it('badges an unconfirmed interpreted entry on both layouts', () => {
    const e = entry({ provenance: 'agent_interpreted', awaitingConfirmation: true });
    const root = mountTemplate(
      renderMadeTableAndMobileList([e], {
        currentUserId: 'u-me',
        showProjectColumn: false,
        defaultProjectId: 'p-1',
      })
    );
    const badges = root.querySelectorAll('[data-awaiting-confirmation]');
    expect(badges.length).toBe(2);
    expect(text(badges[0])).toBe('awaiting confirmation');
  });

  it('has no badge without awaitingConfirmation', () => {
    const root = mountTemplate(
      renderMadeTableAndMobileList([entry({ provenance: 'agent_interpreted' })], {
        currentUserId: 'u-me',
        showProjectColumn: false,
        defaultProjectId: 'p-1',
      })
    );
    expect(root.querySelector('[data-awaiting-confirmation]')).toBeNull();
  });
});
