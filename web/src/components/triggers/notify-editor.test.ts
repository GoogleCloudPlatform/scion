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
import {
  createDefaultNotifySpec,
  normalizeNotifySpec,
  type ScionTriggerNotifyEditor,
} from './notify-editor.js';
import type { TriggerNotifySpec } from './types.js';

describe('scion-trigger-notify-editor', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('defaults to userNotice on and failed/refused/expired outcomes checked (skipped unchecked)', async () => {
    const el = document.createElement('scion-trigger-notify-editor') as ScionTriggerNotifyEditor;
    el.notify = createDefaultNotifySpec();
    el.agents = [
      { id: 'ag-1', name: 'ops-agent' },
      { id: 'ag-2', name: 'triage-agent' },
    ];
    document.body.appendChild(el);
    await el.updateComplete;

    const userSwitch = el.shadowRoot?.querySelector(
      '[data-notify-user-notice]'
    ) as HTMLInputElement;
    expect(userSwitch?.hasAttribute('checked')).toBe(true);

    const failedCb = el.shadowRoot?.querySelector('[data-notify-outcome="failed"]');
    const refusedCb = el.shadowRoot?.querySelector('[data-notify-outcome="refused"]');
    const expiredCb = el.shadowRoot?.querySelector('[data-notify-outcome="expired"]');
    const skippedCb = el.shadowRoot?.querySelector('[data-notify-outcome="skipped"]');
    expect(failedCb?.hasAttribute('checked')).toBe(true);
    expect(refusedCb?.hasAttribute('checked')).toBe(true);
    expect(expiredCb?.hasAttribute('checked')).toBe(true);
    expect(skippedCb?.hasAttribute('checked')).toBe(false);

    // O2: outcome checkboxes are grouped in a native fieldset with a legend.
    const fieldset = el.shadowRoot?.querySelector('fieldset[data-notify-outcomes-group]');
    expect(fieldset).toBeTruthy();
    expect(fieldset?.querySelector('legend')?.textContent?.trim()).toBe('Notify on outcomes');
  });

  it('sends non-UUID free-text agent input as agentName and UUID as agentId when agents list is empty (O1)', async () => {
    const el = document.createElement('scion-trigger-notify-editor') as ScionTriggerNotifyEditor;
    el.notify = createDefaultNotifySpec();
    el.agents = [];
    const emitted: TriggerNotifySpec[] = [];
    el.addEventListener('notify-changed', (e) => {
      emitted.push((e as CustomEvent<{ notify: TriggerNotifySpec }>).detail.notify);
    });
    document.body.appendChild(el);
    await el.updateComplete;

    const input = el.shadowRoot?.querySelector('[data-notify-agent-input]') as HTMLInputElement;
    expect(input).toBeTruthy();

    // Slug/name goes to agentName, not agentId.
    input.value = 'triage-agent';
    input.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;
    expect(emitted.at(-1)?.agentId).toBe('');
    expect(emitted.at(-1)?.agentName).toBe('triage-agent');

    // UUID goes to agentId.
    const uuid = '11111111-2222-3333-4444-555555555555';
    input.value = uuid;
    input.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await el.updateComplete;
    expect(emitted.at(-1)?.agentId).toBe(uuid);
    expect(emitted.at(-1)?.agentName).toBe('');

    // Clear resets both.
    input.value = '';
    input.dispatchEvent(new Event('sl-clear', { bubbles: true }));
    await el.updateComplete;
    expect(emitted.at(-1)?.agentId).toBe('');
    expect(emitted.at(-1)?.agentName).toBe('');
  });

  it('emits notify-changed when toggling user notice, selecting/clearing agent, and changing outcomes', async () => {
    const el = document.createElement('scion-trigger-notify-editor') as ScionTriggerNotifyEditor;
    el.notify = createDefaultNotifySpec();
    el.agents = [
      { id: 'ag-1', name: 'ops-agent' },
      { id: 'ag-2', name: 'triage-agent' },
    ];
    const emitted: TriggerNotifySpec[] = [];
    el.addEventListener('notify-changed', (e) => {
      emitted.push((e as CustomEvent<{ notify: TriggerNotifySpec }>).detail.notify);
    });
    document.body.appendChild(el);
    await el.updateComplete;

    // 1. Toggle userNotice off
    const userSwitch = el.shadowRoot?.querySelector(
      '[data-notify-user-notice]'
    ) as HTMLInputElement;
    userSwitch.checked = false;
    userSwitch.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await el.updateComplete;
    expect(emitted.at(-1)?.userNotice).toBe(false);

    // 2. Select agent ag-1
    const agentSelect = el.shadowRoot?.querySelector(
      '[data-notify-agent-select]'
    ) as HTMLSelectElement;
    agentSelect.value = 'ag-1';
    agentSelect.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await el.updateComplete;
    expect(emitted.at(-1)?.agentId).toBe('ag-1');
    expect(emitted.at(-1)?.agentName).toBe('ops-agent');

    // 3. Clear agent via sl-clear
    agentSelect.value = '';
    agentSelect.dispatchEvent(new Event('sl-clear', { bubbles: true }));
    await el.updateComplete;
    expect(emitted.at(-1)?.agentId).toBe('');
    expect(emitted.at(-1)?.agentName).toBe('');

    // 4. Enable skipped outcome
    const skippedCb = el.shadowRoot?.querySelector(
      '[data-notify-outcome="skipped"]'
    ) as HTMLInputElement;
    skippedCb.checked = true;
    skippedCb.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await el.updateComplete;
    expect(emitted.at(-1)?.onOutcomes).toEqual(['failed', 'refused', 'expired', 'skipped']);

    // 5. Uncheck expired outcome
    const expiredCb = el.shadowRoot?.querySelector(
      '[data-notify-outcome="expired"]'
    ) as HTMLInputElement;
    expiredCb.checked = false;
    expiredCb.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await el.updateComplete;
    expect(emitted.at(-1)?.onOutcomes).toEqual(['failed', 'refused', 'skipped']);
  });

  it('renders stored agent even when not present in agents list and locks controls when readonly', async () => {
    const el = document.createElement('scion-trigger-notify-editor') as ScionTriggerNotifyEditor;
    el.notify = normalizeNotifySpec({
      onOutcomes: ['failed', 'skipped'],
      userNotice: false,
      agentId: 'ag-deleted',
      agentName: 'retired-ops',
    });
    el.agents = [{ id: 'ag-1', name: 'ops-agent' }];
    el.readonly = true;

    const emitted: TriggerNotifySpec[] = [];
    el.addEventListener('notify-changed', (e) => {
      emitted.push((e as CustomEvent<{ notify: TriggerNotifySpec }>).detail.notify);
    });
    document.body.appendChild(el);
    await el.updateComplete;

    const userSwitch = el.shadowRoot?.querySelector(
      '[data-notify-user-notice]'
    ) as HTMLInputElement;
    expect(userSwitch.hasAttribute('disabled')).toBe(true);
    expect(userSwitch.hasAttribute('checked')).toBe(false);

    const agentSelect = el.shadowRoot?.querySelector(
      '[data-notify-agent-select]'
    ) as HTMLSelectElement;
    expect(agentSelect.hasAttribute('disabled')).toBe(true);
    expect(agentSelect.textContent).toContain('retired-ops');

    const failedCb = el.shadowRoot?.querySelector(
      '[data-notify-outcome="failed"]'
    ) as HTMLInputElement;
    expect(failedCb.hasAttribute('disabled')).toBe(true);
    expect(failedCb.hasAttribute('checked')).toBe(true);

    // Attempted mutation while readonly must not emit
    failedCb.checked = false;
    failedCb.dispatchEvent(new Event('sl-change', { bubbles: true }));
    expect(emitted).toHaveLength(0);
  });
});
