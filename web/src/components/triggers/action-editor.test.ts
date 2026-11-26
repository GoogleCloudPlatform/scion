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
import './action-editor.js';
import type { ScionTriggerActionEditor } from './action-editor.js';
import type { TriggerActionSpec } from './types.js';

describe('scion-trigger-action-editor', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('never displays unredacted exec command argv', async () => {
    const el = document.createElement('scion-trigger-action-editor') as ScionTriggerActionEditor;
    el.actions = [
      {
        type: 'exec',
        command: ['sh', '-c', 'echo TOP_SECRET_ARGV_VALUE'],
        commandRedacted: true,
        argc: 3,
        commandSha256: 'abcdef1234567890abcdef',
        timeoutSeconds: 60,
      },
    ];
    document.body.appendChild(el);
    await el.updateComplete;

    const redactedBox = el.shadowRoot?.querySelector('[data-action-exec-redacted="0"]');
    expect(redactedBox).toBeTruthy();
    expect(redactedBox?.textContent).toContain('3 args');
    expect(redactedBox?.textContent).toContain('abcdef123456');
    expect(redactedBox?.textContent).toContain('redacted');
    expect(el.shadowRoot?.textContent).not.toContain('TOP_SECRET_ARGV_VALUE');
  });

  it('supports switching to dispatch_agent and configuring offline queue TTL on message', async () => {
    const el = document.createElement('scion-trigger-action-editor') as ScionTriggerActionEditor;
    let latest: TriggerActionSpec[] = [];
    el.addEventListener('actions-changed', (e: Event) => {
      latest = (e as CustomEvent<{ actions: TriggerActionSpec[] }>).detail.actions;
    });
    document.body.appendChild(el);
    await el.updateComplete;

    const offlineSelect = el.shadowRoot?.querySelector(
      '[data-action-offline="0"]'
    ) as HTMLSelectElement;
    offlineSelect.value = 'queue';
    offlineSelect.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await el.updateComplete;

    expect(latest[0].offline).toBe('queue');
    expect(el.shadowRoot?.querySelector('[data-action-queue-ttl="0"]')).toBeTruthy();

    const typeGroup = el.shadowRoot?.querySelector('[data-action-type="0"]') as HTMLInputElement;
    typeGroup.value = 'dispatch_agent';
    typeGroup.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await el.updateComplete;

    expect(latest[0].type).toBe('dispatch_agent');
    expect(el.shadowRoot?.querySelector('[data-action-template="0"]')).toBeTruthy();
  });

  it('supports multiple ordered action rows when multiActionEnabled (P9 hook) is enabled', async () => {
    const el = document.createElement('scion-trigger-action-editor') as ScionTriggerActionEditor;
    el.multiActionEnabled = true;
    document.body.appendChild(el);
    await el.updateComplete;

    const addBtn = el.shadowRoot?.querySelector('[data-add-action-row]') as HTMLElement;
    expect(addBtn).toBeTruthy();
    addBtn.click();
    await el.updateComplete;

    expect(el.shadowRoot?.querySelectorAll('[data-action-row]')).toHaveLength(2);
  });
});
