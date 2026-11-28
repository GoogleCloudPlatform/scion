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

import { describe, it, expect, afterEach, vi } from 'vitest';
import './reincarnate-dialog.js';
import type { ScionReincarnateDialog, ReincarnateChoiceDetail } from './reincarnate-dialog.js';
import {
  REINCARNATE_CONFIRM_MESSAGE,
  REINCARNATE_PREPARE_MESSAGE,
  reincarnatePrepareAvailability,
} from '../../shared/agent-reincarnate.js';
import type { Agent } from '../../shared/types.js';

function makeAgent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: 'agent-1',
    name: 'worker-1',
    projectId: 'proj-1',
    template: 'default',
    phase: 'running',
    messageMode: 'project',
    _capabilities: { actions: ['read', 'lifecycle', 'message'] },
    ...overrides,
  };
}

describe('reincarnatePrepareAvailability', () => {
  it('returns available for a running messageable agent', () => {
    expect(reincarnatePrepareAvailability(makeAgent({ phase: 'running' }))).toEqual({
      available: true,
      reason: '',
    });
  });

  it('returns available for hub-suspended and user-suspended agents', () => {
    expect(
      reincarnatePrepareAvailability(makeAgent({ phase: 'suspended', suspendReason: 'idle' }))
    ).toEqual({ available: true, reason: '' });
    expect(
      reincarnatePrepareAvailability(makeAgent({ phase: 'suspended', suspendReason: 'user' }))
    ).toEqual({ available: true, reason: '' });
    expect(reincarnatePrepareAvailability(makeAgent({ phase: 'suspended' }))).toEqual({
      available: true,
      reason: '',
    });
  });

  it('returns unavailable for shell agents', () => {
    expect(reincarnatePrepareAvailability(makeAgent({ harnessConfig: 'shell' }))).toEqual({
      available: false,
      reason: "Shell agents can't receive messages.",
    });
    expect(
      reincarnatePrepareAvailability(makeAgent({ appliedConfig: { harnessConfig: 'shell' } }))
    ).toEqual({
      available: false,
      reason: "Shell agents can't receive messages.",
    });
  });

  it('returns unavailable when messageMode is none or messaging is denied', () => {
    expect(reincarnatePrepareAvailability(makeAgent({ messageMode: 'none' }))).toEqual({
      available: false,
      reason: "This agent can't receive messages.",
    });

    const denied = reincarnatePrepareAvailability(
      makeAgent({
        _messageability: { canMessage: false, canReachViewer: false, reason: 'missing_permission' },
      })
    );
    expect(denied.available).toBe(false);
    expect(denied.reason).toContain('permission');

    expect(
      reincarnatePrepareAvailability(makeAgent({ _capabilities: { actions: ['read'] } }))
    ).toEqual({
      available: false,
      reason: "This agent can't receive messages.",
    });
  });

  it.each(['stopped', 'halted', 'error', 'created'] as const)(
    'returns unavailable for %s phase',
    (phase) => {
      expect(reincarnatePrepareAvailability(makeAgent({ phase }))).toEqual({
        available: false,
        reason: 'The agent must be running to prepare its own handoff. Use Reincarnate now.',
      });
    }
  );

  it('returns unavailable while an agent is launching', () => {
    const res = reincarnatePrepareAvailability(
      makeAgent({
        phase: 'provisioning',
        launch: {
          state: 'provisioning',
          kind: 'start',
          startedAt: '2026-11-28T12:00:00Z',
        },
      })
    );
    expect(res.available).toBe(false);
    expect(res.reason).toContain('must be running');
  });

  it('includes the five-step handoff contract in REINCARNATE_PREPARE_MESSAGE', () => {
    expect(REINCARNATE_PREPARE_MESSAGE).toContain('scion reincarnate --handoff-template');
    expect(REINCARNATE_PREPARE_MESSAGE).toContain('scion reincarnate --dry-run');
    expect(REINCARNATE_PREPARE_MESSAGE).toContain('scion reincarnate --handoff-file <file>');
  });
});

describe('scion-reincarnate-dialog', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  async function mount(
    props: Partial<ScionReincarnateDialog> = {}
  ): Promise<ScionReincarnateDialog> {
    const el = document.createElement('scion-reincarnate-dialog') as ScionReincarnateDialog;
    el.open = true;
    el.agentName = 'worker-1';
    el.prepareAvailable = true;
    el.prepareUnavailableReason = '';
    el.busy = false;
    Object.assign(el, props);
    document.body.appendChild(el);
    await el.updateComplete;
    return el;
  }

  it('renders dialog heading, both options, and defaults to Reincarnate now', async () => {
    const el = await mount();
    const dialog = el.shadowRoot!.querySelector('sl-dialog')!;
    expect(dialog.getAttribute('label')).toBe('Reincarnate worker-1');

    const nowOpt = el.shadowRoot!.querySelector('[data-testid="reincarnate-now"]')!;
    const prepareOpt = el.shadowRoot!.querySelector('[data-testid="reincarnate-prepare"]')!;
    expect(nowOpt.textContent).toContain('Reincarnate now');
    expect(nowOpt.textContent).toContain(REINCARNATE_CONFIRM_MESSAGE);
    expect(prepareOpt.textContent).toContain('Ask the agent to prepare');
    expect(prepareOpt.hasAttribute('disabled')).toBe(false);

    const onChoice = vi.fn();
    el.addEventListener('reincarnate-choice', (e) =>
      onChoice((e as CustomEvent<ReincarnateChoiceDetail>).detail)
    );
    el.shadowRoot!.querySelector<HTMLElement>('[data-testid="reincarnate-confirm"]')!.click();
    expect(onChoice).toHaveBeenCalledWith({ mode: 'now' });
  });

  it('emits prepare mode when Ask the agent to prepare is selected', async () => {
    const el = await mount();
    const onChoice = vi.fn();
    el.addEventListener('reincarnate-choice', (e) =>
      onChoice((e as CustomEvent<ReincarnateChoiceDetail>).detail)
    );

    const prepareOpt = el.shadowRoot!.querySelector<HTMLElement>(
      '[data-testid="reincarnate-prepare"]'
    )!;
    prepareOpt.click();
    await el.updateComplete;

    el.shadowRoot!.querySelector<HTMLElement>('[data-testid="reincarnate-confirm"]')!.click();
    expect(onChoice).toHaveBeenCalledWith({ mode: 'prepare' });
  });

  it('disables Ask the agent to prepare and displays the reason when unavailable', async () => {
    const el = await mount({
      prepareAvailable: false,
      prepareUnavailableReason: "Shell agents can't receive messages.",
    });
    const prepareOpt = el.shadowRoot!.querySelector('[data-testid="reincarnate-prepare"]')!;
    expect(prepareOpt.hasAttribute('disabled')).toBe(true);
    const reason = el.shadowRoot!.querySelector('[data-testid="reincarnate-prepare-reason"]')!;
    expect(reason.textContent?.trim()).toBe("Shell agents can't receive messages.");
  });

  it('emits reincarnate-cancel on Cancel and ignores bubbled sl-hide from nested elements', async () => {
    const el = await mount();
    const onCancel = vi.fn();
    el.addEventListener('reincarnate-cancel', onCancel);

    // Bubbled sl-hide from a child inside sl-dialog must NOT cancel the dialog.
    const nested = el.shadowRoot!.querySelector('[data-testid="reincarnate-now"]')!;
    nested.dispatchEvent(new CustomEvent('sl-hide', { bubbles: true, composed: true }));
    expect(onCancel).not.toHaveBeenCalled();

    el.shadowRoot!.querySelector<HTMLElement>('[data-testid="reincarnate-cancel"]')!.click();
    expect(onCancel).toHaveBeenCalledTimes(1);
  });
});
