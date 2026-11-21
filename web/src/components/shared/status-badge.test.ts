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

import { describe, it, expect, afterEach } from 'vitest';
import '@shoelace-style/shoelace/dist/components/tooltip/tooltip.js';
import './status-badge.js';
import type { ScionStatusBadge, StatusType } from './status-badge.js';

describe('scion-status-badge', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  async function mount(props: {
    status: StatusType;
    label?: string;
    tooltip?: string;
    size?: 'small' | 'medium' | 'large';
  }): Promise<ScionStatusBadge> {
    const el = document.createElement('scion-status-badge');
    el.status = props.status;
    if (props.label !== undefined) el.label = props.label;
    if (props.tooltip !== undefined) el.tooltip = props.tooltip;
    if (props.size !== undefined) el.size = props.size;
    document.body.appendChild(el);
    await el.updateComplete;
    return el;
  }

  it('renders completed, idle, blocked (waiting on others), and stalled badges', async () => {
    const cases: Array<{ status: StatusType; label: string; variant: string }> = [
      { status: 'completed', label: 'completed', variant: 'success' },
      { status: 'idle', label: 'idle', variant: 'neutral' },
      { status: 'blocked', label: 'waiting on others', variant: 'neutral' },
      { status: 'stalled', label: 'stalled', variant: 'warning' },
    ];

    for (const c of cases) {
      const el = await mount({ status: c.status });
      const badge = el.shadowRoot?.querySelector('.badge');
      expect(badge).not.toBeNull();
      expect(badge?.classList.contains(c.variant)).toBe(true);
      expect(badge?.textContent).toContain(c.label);
      document.body.innerHTML = '';
    }
  });

  it('renders without sl-tooltip or tabindex when tooltip is empty', async () => {
    const el = await mount({ status: 'working' });
    expect(el.shadowRoot?.querySelector('sl-tooltip')).toBeNull();
    const badge = el.shadowRoot?.querySelector('.badge');
    expect(badge?.hasAttribute('tabindex')).toBe(false);
    expect(badge?.hasAttribute('aria-label')).toBe(false);
  });

  it('wraps the badge in a hoisted, keyboard- and screen-reader-accessible sl-tooltip when tooltip is set', async () => {
    const tipText =
      'Active and expecting to hear back from another agent, a person, or a system.\nAgent says: waiting for review from X';
    const el = await mount({ status: 'blocked', tooltip: tipText });

    const tip = el.shadowRoot?.querySelector('sl-tooltip') as
      | (HTMLElement & { content: string; hoist: boolean; updateComplete: Promise<unknown> })
      | null;
    expect(tip).not.toBeNull();
    expect(tip?.content).toBe(tipText);
    expect(tip?.hasAttribute('hoist')).toBe(true);

    const badge = tip?.querySelector('.badge');
    expect(badge).not.toBeNull();
    expect(badge?.getAttribute('tabindex')).toBe('0');
    expect(badge?.getAttribute('aria-label')).toBe(`waiting on others: ${tipText}`);

    const decorative = badge?.querySelector('.emoji, sl-icon');
    expect(decorative?.getAttribute('aria-hidden')).toBe('true');
  });

  it('renders markup in tooltip text as plain text, not HTML', async () => {
    const raw = 'Agent says: <img src=x onerror="window.__xss=1"><b>hi</b>';
    const el = await mount({ status: 'blocked', tooltip: raw });

    const tip = el.shadowRoot?.querySelector('sl-tooltip') as
      | (HTMLElement & { content: string; updateComplete: Promise<unknown> })
      | null;
    await tip?.updateComplete;
    const body = tip?.shadowRoot?.querySelector('[part~="body"]');
    expect(body?.textContent).toContain(raw);
    expect(body?.querySelector('img, b')).toBeNull();
    expect((window as unknown as { __xss?: number }).__xss).toBeUndefined();
  });
});
