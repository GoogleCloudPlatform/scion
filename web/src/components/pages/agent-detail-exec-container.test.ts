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
 * Agent detail page — Execution container presentation & S5 guards (ptone/scion#8819):
 * - Shows an "Execution container" badge in the header when `hub.exec_containers` is on.
 * - Hides the message button, Messages tab/composer, quick-message dialog, and
 *   task cards for shell agents (S5), regardless of whether the experiment flag
 *   is on or off.
 */

import { describe, it, expect, beforeAll, afterEach, vi } from 'vitest';
import { render, type TemplateResult } from 'lit';

import type { Agent } from '../../shared/types.js';
import { EXEC_CONTAINERS_FLAG, setFeatureFlag } from '../../utils/feature-flags.js';
import type { ScionPageAgentDetail } from './agent-detail.js';

vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

function makeAgent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: 'a-shell',
    name: 'ops-shell',
    projectId: 'p-1',
    template: '',
    phase: 'running',
    activity: 'idle',
    harnessConfig: 'shell',
    taskSummary: 'should stay hidden',
    appliedConfig: {
      harnessConfig: 'shell',
      task: 'should stay hidden',
    },
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
    messageMode: 'project',
    _capabilities: { actions: ['read', 'lifecycle', 'attach', 'message', 'delete'] },
    ...overrides,
  } as Agent;
}

function createDetail(agent: Agent): ScionPageAgentDetail {
  const el = document.createElement('scion-page-agent-detail') as ScionPageAgentDetail;
  el.agentId = agent.id;
  (el as unknown as { loading: boolean; agent: Agent }).loading = false;
  (el as unknown as { agent: Agent }).agent = agent;
  return el;
}

function renderMethod(
  el: ScionPageAgentDetail,
  method:
    | 'render'
    | 'renderHeader'
    | 'renderMessagesPanel'
    | 'renderCurrentTaskCard'
    | 'renderInitialTaskCard',
  ...args: unknown[]
): HTMLDivElement {
  const host = document.createElement('div');
  const fn = (el as unknown as Record<string, (...a: unknown[]) => TemplateResult>)[method];
  render(fn.apply(el, args), host);
  return host;
}

describe('agent detail — execution container (ptone/scion#8819)', () => {
  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  afterEach(() => {
    const features = { ...window.__SCION_FEATURES__ };
    delete features[EXEC_CONTAINERS_FLAG];
    window.__SCION_FEATURES__ = features;
  });

  it('shows the Execution container badge when hub.exec_containers is on, and hides it when off', () => {
    const agent = makeAgent();

    setFeatureFlag(EXEC_CONTAINERS_FLAG, true);
    const onHeader = renderMethod(createDetail(agent), 'renderHeader');
    const badge = onHeader.querySelector('[data-testid="exec-container-badge"]');
    expect(badge).not.toBeNull();
    expect(badge?.textContent).toContain('Execution container');

    setFeatureFlag(EXEC_CONTAINERS_FLAG, false);
    const offHeader = renderMethod(createDetail(agent), 'renderHeader');
    expect(offHeader.querySelector('[data-testid="exec-container-badge"]')).toBeNull();
  });

  it('hides Message button, Messages tab/composer, quick-message dialog, and task cards for shell agents (S5)', () => {
    // S5 hiding applies even when the experiment flag is off.
    setFeatureFlag(EXEC_CONTAINERS_FLAG, false);
    const agent = makeAgent();
    const el = createDetail(agent);

    const full = renderMethod(el, 'render');
    // No Message button in header
    const buttons = Array.from(full.querySelectorAll('.header-actions sl-button')).map((b) =>
      b.textContent?.trim()
    );
    expect(buttons).not.toContain('Message');
    expect(buttons).toContain('Terminal');

    // No message-mode badge, Messages tab, conversation list, or quick-message dialog
    expect(full.querySelector('scion-message-mode-badge')).toBeNull();
    expect(full.querySelector('sl-tab[panel="messages"]')).toBeNull();
    expect(full.querySelector('sl-tab-panel[name="messages"]')).toBeNull();
    expect(full.querySelector('scion-chat-thread')).toBeNull();
    expect(full.querySelector('[data-testid="shared-conversations-panel"]')).toBeNull();
    expect(full.querySelector('scion-agent-message-viewer')).toBeNull();
    expect(full.querySelector('scion-quick-message-dialog')).toBeNull();

    // Direct call to renderMessagesPanel also produces no conversation list or message viewer
    const panel = renderMethod(el, 'renderMessagesPanel');
    expect(panel.querySelector('scion-chat-thread')).toBeNull();
    expect(panel.querySelector('[data-testid="shared-conversations-panel"]')).toBeNull();
    expect(panel.querySelector('scion-agent-message-viewer')).toBeNull();

    // Task cards hidden
    expect(renderMethod(el, 'renderCurrentTaskCard', agent).textContent?.trim()).toBe('');
    expect(renderMethod(el, 'renderInitialTaskCard', agent.appliedConfig).textContent?.trim()).toBe(
      ''
    );
  });

  it('leaves non-shell agents unchanged', () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, true);
    const agent = makeAgent({
      id: 'a-llm',
      name: 'claude-agent',
      template: 'default',
      harnessConfig: 'claude',
      appliedConfig: { harnessConfig: 'claude', task: 'Fix the bug' },
      taskSummary: 'Fixing the bug',
    });
    const el = createDetail(agent);

    const full = renderMethod(el, 'render');
    expect(full.querySelector('[data-testid="exec-container-badge"]')).toBeNull();
    expect(full.querySelector('sl-tab[panel="messages"]')).not.toBeNull();
    expect(full.querySelector('scion-quick-message-dialog')).not.toBeNull();
    expect(renderMethod(el, 'renderCurrentTaskCard', agent).textContent).toContain(
      'Fixing the bug'
    );
    expect(renderMethod(el, 'renderInitialTaskCard', agent.appliedConfig).textContent).toContain(
      'Fix the bug'
    );
  });
});
