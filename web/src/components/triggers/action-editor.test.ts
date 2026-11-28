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

  it('supports adding, reordering, and removing up to 8 ordered action rows by default (ptone/scion#9785)', async () => {
    const el = document.createElement('scion-trigger-action-editor') as ScionTriggerActionEditor;
    let latest: TriggerActionSpec[] = [];
    el.addEventListener('actions-changed', (e: Event) => {
      latest = (e as CustomEvent<{ actions: TriggerActionSpec[] }>).detail.actions;
    });
    el.actions = [{ type: 'message', agentId: 'ag-1', message: 'first' }];
    document.body.appendChild(el);
    await el.updateComplete;

    // multiActionEnabled defaults to true: add up to 8 rows.
    for (let i = 1; i < 8; i++) {
      const addBtn = el.shadowRoot?.querySelector('[data-add-action-row]') as HTMLElement;
      expect(addBtn).toBeTruthy();
      addBtn.click();
      await el.updateComplete;
    }
    expect(el.shadowRoot?.querySelectorAll('[data-action-row]')).toHaveLength(8);
    expect(el.shadowRoot?.querySelector('[data-add-action-row]')).toBeNull();

    // Update second row message so we can verify reordering.
    el.actions = el.actions.map((a, idx) => (idx === 1 ? { ...a, message: 'second' } : a));
    await el.updateComplete;

    // Move row 1 ("second") up to index 0.
    const moveUp = el.shadowRoot?.querySelector('[data-move-action-up="1"]') as HTMLElement;
    expect(moveUp).toBeTruthy();
    moveUp.click();
    await el.updateComplete;
    expect(latest[0].message).toBe('second');
    expect(latest[1].message).toBe('first');

    // Remove row 0 ("second"): count drops to 7 and Add button reappears.
    const removeBtn = el.shadowRoot?.querySelector('[data-remove-action="0"]') as HTMLElement;
    expect(removeBtn).toBeTruthy();
    removeBtn.click();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelectorAll('[data-action-row]')).toHaveLength(7);
    expect(latest[0].message).toBe('first');
    expect(el.shadowRoot?.querySelector('[data-add-action-row]')).toBeTruthy();
  });

  it('renders N actions read-only without mutation controls when readonly is true (ptone/scion#9785)', async () => {
    const el = document.createElement('scion-trigger-action-editor') as ScionTriggerActionEditor;
    el.readonly = true;
    el.actions = [
      {
        type: 'message',
        agentId: 'ag-1',
        message: 'Notify ${event.attributes.repo}',
        untrustedVars: ['event.attributes.repo'],
      },
      {
        type: 'dispatch_agent',
        template: 'developer',
        task: 'Fix ${event.payload.commit}',
        untrustedVars: ['event.payload.commit'],
      },
      {
        type: 'github.comment',
        target: 'event',
        body: 'Acked',
      },
    ];
    document.body.appendChild(el);
    await el.updateComplete;

    expect(el.shadowRoot?.querySelectorAll('[data-action-row]')).toHaveLength(3);
    expect(el.shadowRoot?.querySelector('[data-add-action-row]')).toBeNull();
    expect(el.shadowRoot?.querySelector('[data-remove-action="0"]')).toBeNull();
    expect(el.shadowRoot?.querySelector('[data-move-action-up="1"]')).toBeNull();
    expect(el.shadowRoot?.querySelectorAll('scion-trigger-untrusted-vars')).toHaveLength(2);
  });

  it('renders github.comment and unknown action types read-only instead of message fields (O7)', async () => {
    const el = document.createElement('scion-trigger-action-editor') as ScionTriggerActionEditor;
    el.actions = [
      {
        type: 'github.comment',
        target: 'pull_request',
        body: 'Automated review started',
        replyInThread: true,
      },
    ];
    document.body.appendChild(el);
    await el.updateComplete;

    const ro = el.shadowRoot?.querySelector('[data-action-readonly="0"]');
    expect(ro).toBeTruthy();
    expect(ro?.getAttribute('data-action-readonly-type')).toBe('github.comment');
    expect(ro?.textContent).toContain('Automated review started');
    expect(el.shadowRoot?.querySelector('[data-action-message="0"]')).toBeNull();
  });

  it('displays hub-derived untrustedVars read-only via shared component on message and dispatch_agent (ptone/scion#9776)', async () => {
    const el = document.createElement('scion-trigger-action-editor') as ScionTriggerActionEditor;
    el.actions = [
      {
        type: 'message',
        agentId: 'ag-1',
        message: 'CI at ${event.payload.commit}',
        untrustedVars: ['event.payload.commit'],
      },
      {
        type: 'dispatch_agent',
        template: 'developer',
        task: 'Review ${event.attributes.title}',
        untrustedVars: ['event.attributes.title', 'event.payload.sha'],
      },
    ];
    document.body.appendChild(el);
    await el.updateComplete;

    // Editable untrustedVars input must not be rendered on either action type.
    expect(el.shadowRoot?.querySelector('[data-action-untrusted="0"]')).toBeNull();
    expect(el.shadowRoot?.querySelector('[data-action-untrusted="1"]')).toBeNull();

    // Shared read-only display component renders on both message and dispatch_agent.
    const displays = el.shadowRoot?.querySelectorAll('scion-trigger-untrusted-vars');
    expect(displays).toHaveLength(2);

    const msgDisplay = displays![0] as HTMLElement & { updateComplete: Promise<boolean> };
    const dispDisplay = displays![1] as HTMLElement & { updateComplete: Promise<boolean> };
    await msgDisplay.updateComplete;
    await dispDisplay.updateComplete;

    expect(msgDisplay.shadowRoot?.textContent).toContain(
      'Event values inserted as data, not instructions:'
    );
    expect(msgDisplay.shadowRoot?.textContent).toContain('Recomputed on save.');
    expect(msgDisplay.shadowRoot?.textContent).toContain('event.payload.commit');

    expect(dispDisplay.shadowRoot?.textContent).toContain(
      'Event values inserted as data, not instructions:'
    );
    expect(dispDisplay.shadowRoot?.textContent).toContain('Recomputed on save.');
    expect(dispDisplay.shadowRoot?.textContent).toContain('event.attributes.title');
    expect(dispDisplay.shadowRoot?.textContent).toContain('event.payload.sha');
  });

  it('omits untrustedVars from default dispatch_agent action', async () => {
    const el = document.createElement('scion-trigger-action-editor') as ScionTriggerActionEditor;
    let latest: TriggerActionSpec[] = [];
    el.addEventListener('actions-changed', (e: Event) => {
      latest = (e as CustomEvent<{ actions: TriggerActionSpec[] }>).detail.actions;
    });
    document.body.appendChild(el);
    await el.updateComplete;

    const typeGroup = el.shadowRoot?.querySelector('[data-action-type="0"]') as HTMLInputElement;
    typeGroup.value = 'dispatch_agent';
    typeGroup.dispatchEvent(new Event('sl-change', { bubbles: true }));
    await el.updateComplete;

    expect(latest[0].type).toBe('dispatch_agent');
    expect(latest[0].untrustedVars).toBeUndefined();
  });
});
