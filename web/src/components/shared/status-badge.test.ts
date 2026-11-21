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

  it('keeps tabindex and aria-label eager but defers sl-tooltip until pointerenter', async () => {
    const tipText =
      'Active and expecting to hear back from another agent, a person, or a system.\nAgent says: waiting for review from X';
    const el = await mount({ status: 'blocked', tooltip: tipText });

    // Before interaction: no sl-tooltip in the shadow DOM (preserves DOM budget),
    // but keyboard focusability and screen-reader label are already present.
    expect(el.shadowRoot?.querySelector('sl-tooltip')).toBeNull();
    const initialBadge = el.shadowRoot?.querySelector('.badge');
    expect(initialBadge).not.toBeNull();
    expect(initialBadge?.getAttribute('tabindex')).toBe('0');
    expect(initialBadge?.getAttribute('aria-label')).toBe(`waiting on others: ${tipText}`);

    const decorative = initialBadge?.querySelector('.emoji, sl-icon');
    expect(decorative?.getAttribute('aria-hidden')).toBe('true');

    // DOM node count on initial render matches a badge without a tooltip.
    const plain = await mount({ status: 'blocked' });
    expect(el.shadowRoot?.querySelectorAll('*').length).toBe(
      plain.shadowRoot?.querySelectorAll('*').length
    );

    // First pointerenter lazily mounts hoisted sl-tooltip and opens it.
    initialBadge?.dispatchEvent(new Event('pointerenter', { bubbles: true }));
    await el.updateComplete;

    const tip = el.shadowRoot?.querySelector('sl-tooltip') as
      | (HTMLElement & {
          content: string;
          hoist: boolean;
          open: boolean;
          updateComplete: Promise<unknown>;
        })
      | null;
    expect(tip).not.toBeNull();
    expect(tip?.content).toBe(tipText);
    expect(tip?.hasAttribute('hoist')).toBe(true);
    await tip?.updateComplete;
    expect(tip?.open).toBe(true);

    const wrappedBadge = tip?.querySelector('.badge');
    expect(wrappedBadge).not.toBeNull();
    expect(wrappedBadge?.getAttribute('tabindex')).toBe('0');
    expect(wrappedBadge?.getAttribute('aria-label')).toBe(`waiting on others: ${tipText}`);
  });

  it('lazily mounts sl-tooltip on keyboard focusin, preserves focus, and opens the tooltip', async () => {
    const tipText = 'Done with any known work.';
    const el = await mount({ status: 'idle', tooltip: tipText });

    expect(el.shadowRoot?.querySelector('sl-tooltip')).toBeNull();
    const initialBadge = el.shadowRoot?.querySelector<HTMLElement>('.badge');
    expect(initialBadge).not.toBeNull();

    initialBadge?.focus();
    initialBadge?.dispatchEvent(new FocusEvent('focusin', { bubbles: true }));
    await el.updateComplete;

    const tip = el.shadowRoot?.querySelector('sl-tooltip') as
      | (HTMLElement & {
          content: string;
          hoist: boolean;
          open: boolean;
          updateComplete: Promise<unknown>;
        })
      | null;
    expect(tip).not.toBeNull();
    expect(tip?.content).toBe(tipText);
    expect(tip?.hasAttribute('hoist')).toBe(true);
    await tip?.updateComplete;
    expect(tip?.open).toBe(true);

    const wrappedBadge = tip?.querySelector<HTMLElement>('.badge');
    expect(wrappedBadge).not.toBeNull();
    expect(el.shadowRoot?.activeElement).toBe(wrappedBadge);
  });

  it('renders markup in tooltip text as plain text, not HTML', async () => {
    const raw = 'Agent says: <img src=x onerror="window.__xss=1"><b>hi</b>';
    const el = await mount({ status: 'blocked', tooltip: raw });

    el.shadowRoot
      ?.querySelector('.badge')
      ?.dispatchEvent(new Event('pointerenter', { bubbles: true }));
    await el.updateComplete;

    const tip = el.shadowRoot?.querySelector('sl-tooltip') as
      | (HTMLElement & { content: string; updateComplete: Promise<unknown> })
      | null;
    expect(tip).not.toBeNull();
    await tip?.updateComplete;
    const body = tip?.shadowRoot?.querySelector('[part~="body"]');
    expect(body?.textContent).toContain(raw);
    expect(body?.querySelector('img, b')).toBeNull();
    expect((window as unknown as { __xss?: number }).__xss).toBeUndefined();
  });

  it('handles tooltip attribute removal and null/undefined tooltip values without throwing', async () => {
    const el = await mount({ status: 'idle' });
    el.setAttribute('tooltip', 'Done with any known work.');
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('sl-tooltip')).toBeNull();

    // Mount the tooltip via interaction, then remove the attribute.
    el.shadowRoot
      ?.querySelector('.badge')
      ?.dispatchEvent(new Event('pointerenter', { bubbles: true }));
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('sl-tooltip')).not.toBeNull();

    // Removing the attribute sets the Lit String property to null (e.g. via ifDefined(undefined)).
    el.removeAttribute('tooltip');
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('sl-tooltip')).toBeNull();
    const badge = el.shadowRoot?.querySelector('.badge');
    expect(badge?.hasAttribute('tabindex')).toBe(false);
    expect(badge?.hasAttribute('aria-label')).toBe(false);

    // Explicitly assigning undefined or null also renders cleanly without a tooltip.
    el.tooltip = undefined as unknown as string;
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('sl-tooltip')).toBeNull();

    el.tooltip = null as unknown as string;
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('sl-tooltip')).toBeNull();
  });
});
