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
 * Unit tests for <scion-chat-schedule-dialog> opt-in wake checkbox (ptone/scion#7755).
 */

import { describe, it, expect, beforeAll, beforeEach, afterEach } from 'vitest';
import '@shoelace-style/shoelace/dist/components/checkbox/checkbox.js';
import type { ScheduleConfirmDetail } from './chat-schedule-dialog.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

beforeAll(async () => {
  await import('./chat-schedule-dialog.js');
});

describe('scion-chat-schedule-dialog', () => {
  let el: any;

  const wakeCheckbox = (): any => el.shadowRoot.querySelector('.wake-checkbox');
  const confirmBtn = (): HTMLElement => el.shadowRoot.querySelector('.confirm-btn');

  beforeEach(async () => {
    el = document.createElement('scion-chat-schedule-dialog');
    document.body.appendChild(el);
    await el.updateComplete;
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('hides the wake checkbox when wakeAvailable is false and emits wake: false', async () => {
    el.wakeAvailable = false;
    el.open = true;
    await el.updateComplete;

    expect(wakeCheckbox()).toBeNull();

    const confirmed: ScheduleConfirmDetail[] = [];
    el.addEventListener('schedule-confirm', (e: CustomEvent<ScheduleConfirmDetail>) =>
      confirmed.push(e.detail)
    );
    confirmBtn().click();
    await el.updateComplete;

    expect(confirmed).toHaveLength(1);
    expect(confirmed[0]!.wake).toBe(false);
  });

  it('shows the wake checkbox unchecked by default when wakeAvailable is true, emits wake on confirm, and resets on reopen', async () => {
    el.wakeAvailable = true;
    el.open = true;
    await el.updateComplete;

    const cb = wakeCheckbox();
    expect(cb).not.toBeNull();
    expect(cb.textContent).toContain('Wake the agent if it is suspended');
    expect(cb.checked).toBeFalsy();

    const confirmed: ScheduleConfirmDetail[] = [];
    el.addEventListener('schedule-confirm', (e: CustomEvent<ScheduleConfirmDetail>) =>
      confirmed.push(e.detail)
    );

    // Unchecked confirm -> wake: false
    confirmBtn().click();
    await el.updateComplete;
    expect(confirmed).toHaveLength(1);
    expect(confirmed[0]!.wake).toBe(false);

    // Check the box and confirm -> wake: true
    cb.checked = true;
    cb.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
    await el.updateComplete;
    confirmBtn().click();
    await el.updateComplete;
    expect(confirmed).toHaveLength(2);
    expect(confirmed[1]!.wake).toBe(true);

    // Close and reopen -> checkbox resets to unchecked
    el.open = false;
    await el.updateComplete;
    el.open = true;
    await el.updateComplete;
    expect(wakeCheckbox().checked).toBeFalsy();
  });
});
