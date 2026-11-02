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
 * Needs attention panel: server order, severity icons, links only for
 * linkable subjects, and the empty and not-reported states.
 */

import { describe, it, expect, afterEach } from 'vitest';

import {
  attentionHref,
  truncateText,
  pausedReasonLabel,
  PAUSED_REASON_GENERIC,
  SCHEDULE_ERROR_MAX_CHARS,
  type HealthAttentionItem,
  type HealthFailingSchedule,
  type HealthPausedSchedule,
} from './health-attention.js';
import './health-attention.js';
import { hasInPageState } from '../../client/route-history.js';
import { elementStyleRules } from './__fixtures__/css-rules.js';

async function mount(
  items: HealthAttentionItem[] | null,
  integrationsDetail = true,
  failingSchedules: HealthFailingSchedule[] | null = null,
  pausedSchedules: HealthPausedSchedule[] | null = null
): Promise<ShadowRoot> {
  const el = document.createElement('scion-health-attention');
  el.items = items;
  el.integrationsDetail = integrationsDetail;
  el.failingSchedules = failingSchedules;
  el.pausedSchedules = pausedSchedules;
  document.body.appendChild(el);
  await el.updateComplete;
  return el.shadowRoot!;
}

const items: HealthAttentionItem[] = [
  {
    severity: 'critical',
    kind: 'hub_check',
    subject: { type: 'hub', id: 'hub-a 1', name: 'hub-a' },
    message: 'Hub check database is not healthy on instance hub-a',
  },
  {
    severity: 'warning',
    kind: 'hub_check',
    subject: { type: 'hub', id: 'hub-a' },
    message: 'Runtime broker data not available',
  },
  {
    severity: 'warning',
    kind: 'broker_offline',
    subject: { type: 'runtime_broker', id: 'b 1', name: 'zulu' },
    message: 'Runtime broker zulu is offline',
  },
  {
    severity: 'warning',
    kind: 'integration',
    subject: { type: 'integration', id: 'chat', name: 'chat' },
    message: 'Integration chat is unhealthy',
  },
  {
    severity: 'warning',
    kind: 'integration',
    subject: { type: 'integration' },
    message: '2 integrations unhealthy',
  },
  {
    severity: 'warning',
    kind: 'dispatch',
    subject: { type: 'dispatch' },
    message: '1 broker dispatch stuck in progress',
  },
  {
    severity: 'warning',
    kind: 'agents',
    subject: { type: 'agent', id: 'ag1', name: 'w', project_id: 'p' },
    message: 'Agent proj / w is in the error phase',
  },
  {
    severity: 'warning',
    kind: 'agents',
    subject: { type: 'agents' },
    message: '3 more agents in the error phase',
  },
];

describe('scion-health-attention', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('keeps the server order and the server sentences', async () => {
    const root = await mount(items);
    const text = [...root.querySelectorAll('li')].map((li) => li.textContent?.trim());
    expect(text).toEqual(items.map((i) => i.message));
  });

  it('shows a severity icon per item', async () => {
    const root = await mount(items);
    const icons = [...root.querySelectorAll('li sl-icon')];
    expect(icons).toHaveLength(items.length);
    expect(icons[0]!.getAttribute('name')).toBe('exclamation-octagon');
    expect(icons[0]!.getAttribute('label')).toBe('Critical');
    expect(icons[2]!.getAttribute('name')).toBe('exclamation-triangle');
    expect(icons[2]!.getAttribute('label')).toBe('Warning');
  });

  it('links only items whose subject is a hub instance, broker, agent or integration with an ID', async () => {
    const root = await mount(items);
    const hrefs = [...root.querySelectorAll('li')].map(
      (li) => li.querySelector('a')?.getAttribute('href') ?? null
    );
    expect(hrefs).toEqual([
      // An item about one hub instance links to its row on this page.
      '#hub-instance-hub-a%201',
      // The broker list item is about the hub: no broker link.
      null,
      '/brokers/b%201',
      '/admin/integrations/chat',
      // No integration identity (integrations_detail false): no link.
      null,
      null,
      '/agents/ag1',
      null,
    ]);
  });

  it('moves to a hub instance row within the page on click', async () => {
    history.replaceState(null, '', '/health');
    const root = await mount(items);
    const link = root.querySelector('a[href^="#hub-instance-"]')!;
    const click = new MouseEvent('click', {
      bubbles: true,
      composed: true,
      cancelable: true,
      button: 0,
    });
    link.dispatchEvent(click);
    expect(click.defaultPrevented).toBe(true);
    expect(window.location.hash).toBe('#hub-instance-hub-a%201');
    expect(hasInPageState(history.state)).toBe(true);
    history.replaceState(null, '', '/');
  });

  it('leaves other links to the router', async () => {
    const root = await mount(items);
    const link = root.querySelector('a[href="/brokers/b%201"]')!;
    const click = new MouseEvent('click', {
      bubbles: true,
      composed: true,
      cancelable: true,
      button: 0,
    });
    let prevented: boolean | null = null;
    link.addEventListener('click', (e) => {
      prevented = e.defaultPrevented;
      e.preventDefault();
    });
    link.dispatchEvent(click);
    expect(prevented).toBe(false);
  });

  it('links no integration item when the summary has no integration identity', async () => {
    const root = await mount(items, false);
    const row = [...root.querySelectorAll('li')].find(
      (li) => li.textContent?.trim() === 'Integration chat is unhealthy'
    );
    expect(row).toBeTruthy();
    expect(row!.querySelector('a')).toBeNull();
    // Other subjects still link.
    expect(root.querySelector('a[href="/brokers/b%201"]')).not.toBeNull();
    expect(root.querySelector('a[href="/agents/ag1"]')).not.toBeNull();
  });

  it('renders a broker_version_mismatch item as a warning linked to the broker (ptone/scion#5972)', async () => {
    const root = await mount([
      {
        severity: 'warning',
        kind: 'broker_version_mismatch',
        subject: { type: 'runtime_broker', id: 'b 2', name: 'east' },
        message: 'Runtime broker east runs v1.2.0; the hub runs v1.3.0',
      },
    ]);
    const li = root.querySelector('li[data-kind="broker_version_mismatch"]')!;
    expect(li.className).toBe('sev-warning');
    expect(li.textContent).toContain('Runtime broker east runs v1.2.0; the hub runs v1.3.0');
    expect(li.querySelector('a')?.getAttribute('href')).toBe('/brokers/b%202');
  });

  it('shows Nothing needs attention for an empty list', async () => {
    const root = await mount([]);
    expect(root.querySelector('ul')).toBeNull();
    expect(root.textContent).toContain('Nothing needs attention.');
  });

  it('does not claim all clear when the list is missing', async () => {
    const root = await mount(null);
    expect(root.textContent).toContain('Attention data not available');
    expect(root.textContent).not.toContain('Nothing needs attention');
  });

  it('uses theme tokens only, with no hex fallbacks', () => {
    for (const body of elementStyleRules('scion-health-attention').values()) {
      expect(body).not.toMatch(/#[0-9a-f]{3,8}\b|rgba?\(/i);
      for (const m of body.matchAll(/var\((--[\w-]+)/g)) expect(m[1]).toMatch(/^--scion-/);
    }
  });
});

const scheduleItem: HealthAttentionItem = {
  severity: 'warning',
  kind: 'schedule',
  subject: { type: 'schedule', id: 'sc 1', name: 'nightly', project_id: 'p 1' },
  message: 'Scheduled event alpha / nightly failed its last run',
};

const failingRow: HealthFailingSchedule = {
  id: 'sc 1',
  name: 'nightly',
  project_id: 'p 1',
  project_slug: 'alpha',
  event_type: 'message',
  target: 'worker',
  run_count: 7,
  error_count: 3,
  last_failure_at: '2026-10-19T08:00:00Z',
  last_error: 'schedule authority denied',
};

describe('scion-health-attention failing schedules', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('links a schedule item to its project schedules page and shows the detail line', async () => {
    const root = await mount([scheduleItem], true, [failingRow]);
    const li = root.querySelector('li[data-kind="schedule"]')!;
    expect(li.querySelector('a')!.getAttribute('href')).toBe('/projects/p%201/schedules');
    expect(li.querySelector('a')!.textContent).toBe(scheduleItem.message);
    const detail = li.querySelector('.detail')!.textContent!;
    expect(detail).toContain('Target: worker');
    expect(detail).toContain('3 of 7 runs failed');
    expect(detail).toContain('last failure ');
    expect(li.querySelector('.last-error')!.textContent).toBe(
      'Last error: schedule authority denied'
    );
  });

  it('shows no detail line without a matching row', async () => {
    const root = await mount([scheduleItem], true, []);
    expect(root.querySelector('.detail')).toBeNull();
    const none = await mount([scheduleItem], true, null);
    expect(none.querySelector('.detail')).toBeNull();
  });

  it('omits the target and the last error when the row has none', async () => {
    const root = await mount([scheduleItem], true, [
      { ...failingRow, target: '', last_error: '', last_failure_at: null },
    ]);
    const detail = root.querySelector('.detail')!;
    expect(detail.textContent!.trim()).toBe('3 of 7 runs failed');
    expect(detail.querySelector('.last-error')).toBeNull();
  });

  it('shortens a long last error and keeps the full text in the title', async () => {
    const long = 'e'.repeat(SCHEDULE_ERROR_MAX_CHARS + 40);
    const root = await mount([scheduleItem], true, [{ ...failingRow, last_error: long }]);
    const el = root.querySelector('.last-error')!;
    expect(el.textContent).toBe('Last error: ' + 'e'.repeat(SCHEDULE_ERROR_MAX_CHARS) + '\u2026');
    expect(el.getAttribute('title')).toBe(long);
  });

  it('renders markup in the last error and target as text, never as elements', async () => {
    const markup = '<img src=x onerror="window.__scheduleErr=1"><b>bold</b>';
    const root = await mount([scheduleItem], true, [
      { ...failingRow, target: '<i>agent</i>', last_error: markup },
    ]);
    const detail = root.querySelector('.detail')!;
    expect(detail.querySelector('img')).toBeNull();
    expect(detail.querySelector('b')).toBeNull();
    expect(detail.querySelector('i')).toBeNull();
    expect(detail.textContent).toContain('Target: <i>agent</i>');
    expect(root.querySelector('.last-error')!.textContent).toBe('Last error: ' + markup);
    expect(root.querySelector('.last-error')!.getAttribute('title')).toBe(markup);
    expect((window as unknown as { __scheduleErr?: number }).__scheduleErr).toBeUndefined();
  });
});

const pausedItem: HealthAttentionItem = {
  severity: 'warning',
  kind: 'schedule',
  subject: { type: 'schedule', id: 'sc 2', name: 'weekly', project_id: 'p 1' },
  message:
    'Scheduled event alpha / weekly was paused by the hub: the agent or user it runs under was deleted',
};

const pausedRow: HealthPausedSchedule = {
  id: 'sc 2',
  name: 'weekly',
  project_id: 'p 1',
  project_slug: 'alpha',
  event_type: 'message',
  target: 'worker',
  reason: 'creator_deleted',
  updated_at: '2026-10-19T08:00:00Z',
};

describe('scion-health-attention hub-paused schedules', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('links a paused item to its project schedules page and shows the reason', async () => {
    const root = await mount([pausedItem], true, [], [pausedRow]);
    const li = root.querySelector('li[data-kind="schedule"]')!;
    expect(li.querySelector('a')!.getAttribute('href')).toBe('/projects/p%201/schedules');
    expect(li.querySelector('a')!.textContent).toBe(pausedItem.message);
    const detail = li.querySelector('.paused-detail')!.textContent!;
    expect(detail).toContain('Paused by the hub: the agent or user it runs under was deleted');
    expect(detail).toContain('Target: worker');
    expect(detail).toContain('updated ');
    expect(li.querySelector('.last-error')).toBeNull();
  });

  it('shows fixed text for each known reason', async () => {
    const root = await mount([pausedItem], true, null, [
      { ...pausedRow, reason: 'cron_zone_unsupported', target: '', updated_at: null },
    ]);
    expect(root.querySelector('.paused-detail')!.textContent!.trim()).toBe(
      'Paused by the hub: its cron expression has a zone prefix, which is not supported: set its time zone field instead'
    );
  });

  it('shows the generic label for an unknown reason and never the code', async () => {
    for (const reason of ['other', 'some_later_reason', '<b>stored</b>', '', 'toString']) {
      const root = await mount([pausedItem], true, null, [{ ...pausedRow, reason, target: '' }]);
      const detail = root.querySelector('.paused-detail')!;
      expect(detail.textContent).toContain('Paused by the hub: ' + PAUSED_REASON_GENERIC);
      expect(detail.querySelector('b')).toBeNull();
      if (reason) expect(detail.textContent).not.toContain(reason);
      document.body.innerHTML = '';
    }
  });

  it('renders markup in the target as text, never as elements', async () => {
    const root = await mount([pausedItem], true, null, [
      { ...pausedRow, target: '<img src=x onerror="window.__pausedErr=1">' },
    ]);
    const detail = root.querySelector('.paused-detail')!;
    expect(detail.querySelector('img')).toBeNull();
    expect(detail.textContent).toContain('Target: <img src=x onerror="window.__pausedErr=1">');
    expect((window as unknown as { __pausedErr?: number }).__pausedErr).toBeUndefined();
  });

  it('shows no detail line without a matching row', async () => {
    const root = await mount([pausedItem], true, null, [{ ...pausedRow, id: 'other' }]);
    expect(root.querySelector('.detail')).toBeNull();
    const none = await mount([pausedItem], true, null, null);
    expect(none.querySelector('.detail')).toBeNull();
  });
});

describe('pausedReasonLabel', () => {
  it('maps known codes to fixed text and anything else to the generic label', () => {
    expect(pausedReasonLabel('creator_deleted')).toBe(
      'the agent or user it runs under was deleted'
    );
    expect(pausedReasonLabel('cron_zone_unsupported')).toBe(
      'its cron expression has a zone prefix, which is not supported: set its time zone field instead'
    );
    expect(pausedReasonLabel('other')).toBe(PAUSED_REASON_GENERIC);
    expect(pausedReasonLabel('constructor')).toBe(PAUSED_REASON_GENERIC);
    expect(pausedReasonLabel(undefined)).toBe(PAUSED_REASON_GENERIC);
    expect(pausedReasonLabel(null)).toBe(PAUSED_REASON_GENERIC);
  });
});

describe('truncateText', () => {
  it('keeps text at or under the limit', () => {
    expect(truncateText('', 3)).toBe('');
    expect(truncateText('abc', 3)).toBe('abc');
  });

  it('cuts by characters, never splitting one, and adds an ellipsis', () => {
    expect(truncateText('abcd', 3)).toBe('abc\u2026');
    expect(truncateText('\u{1F600}\u{1F600}\u{1F600}\u{1F600}', 2)).toBe(
      '\u{1F600}\u{1F600}\u2026'
    );
  });
});

describe('attentionHref', () => {
  it('returns null for subjects without an ID or of other types', () => {
    for (const subject of [
      { type: 'runtime_broker' },
      { type: 'agent' },
      { type: 'integration' },
      { type: 'hub' },
      { type: 'hub', id: 'x' },
      { type: 'dispatch', id: 'x' },
      { type: 'agents', id: 'x' },
      { type: 'schedule', id: 'x' },
      { type: 'schedules' },
    ]) {
      expect(attentionHref({ ...items[2]!, subject }, true)).toBeNull();
    }
  });

  it('links a hub instance item to the instance row anchor', () => {
    expect(
      attentionHref({ ...items[0]!, subject: { type: 'hub', id: 'hub/a', name: 'a' } }, false)
    ).toBe('#hub-instance-hub%2Fa');
  });

  it('links a schedule to its project schedules page', () => {
    expect(attentionHref(scheduleItem, false)).toBe('/projects/p%201/schedules');
  });

  it('links an integration only when the summary carries integration identity', () => {
    expect(attentionHref(items[3]!, true)).toBe('/admin/integrations/chat');
    expect(attentionHref(items[3]!, false)).toBeNull();
    expect(attentionHref(items[2]!, false)).toBe('/brokers/b%201');
  });
});
