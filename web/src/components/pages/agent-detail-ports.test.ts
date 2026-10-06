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
 * Exposed Ports card on the agent detail page (ptone/scion#2540): the
 * "Open in new tab" proxy links appear only when the viewer holds the
 * agent's port_access capability. Without it the ports are still listed,
 * with no link.
 */

import { describe, it, expect, beforeAll, vi } from 'vitest';
import { render, type TemplateResult } from 'lit';

import type { Agent } from '../../shared/types.js';
import type { ScionPageAgentDetail } from './agent-detail.js';

// chat-thread (imported by agent-detail) pulls in the app entry point,
// which bootstraps the SPA on load; stub it as the chat tests do.
// Remove once chat-thread stops importing client/main (chat lane, ptone/scion#3118).
vi.mock('../../client/main.js', () => ({
  navigateTo: vi.fn(),
  stateManager: new EventTarget(),
}));

function makeAgent(overrides: Partial<Agent>): Agent {
  return {
    id: 'a-1',
    name: 'agent-1',
    projectId: 'p-1',
    template: 't',
    phase: 'running',
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
    messageMode: 'project',
    exposedPorts: [{ port: 8080, label: 'web' }, { port: 3000 }],
    _capabilities: { actions: ['read'] },
    ...overrides,
  } as Agent;
}

function renderPortsCard(agent: Agent): HTMLElement {
  const el = document.createElement('scion-page-agent-detail') as ScionPageAgentDetail;
  const tpl = (
    el as unknown as { renderExposedPortsCard(a: Agent): TemplateResult }
  ).renderExposedPortsCard(agent);
  const host = document.createElement('div');
  render(tpl, host);
  return host;
}

describe('agent detail Exposed Ports card capability gate', () => {
  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  it('links every port when the viewer has port_access', () => {
    const host = renderPortsCard(
      makeAgent({ _capabilities: { actions: ['read', 'port_access'] } })
    );
    const links = Array.from(host.querySelectorAll('a.port-link'));
    expect(links.map((a) => a.getAttribute('href'))).toEqual([
      '/api/v1/agents/a-1/ports/8080/proxy/',
      '/api/v1/agents/a-1/ports/3000/proxy/',
    ]);
    expect(host.querySelector('.port-no-access')).toBeNull();
  });

  it('lists ports without links when port_access is missing', () => {
    const host = renderPortsCard(makeAgent({ _capabilities: { actions: ['read'] } }));
    expect(host.querySelectorAll('a').length).toBe(0);
    const labels = Array.from(host.querySelectorAll('.info-label')).map((l) =>
      l.textContent?.trim()
    );
    expect(labels).toEqual([':8080 (web)', ':3000']);
    expect(host.querySelectorAll('.port-no-access').length).toBe(1);
  });

  it('fails closed when capabilities are absent', () => {
    const host = renderPortsCard(makeAgent({ _capabilities: undefined }));
    expect(host.querySelectorAll('a').length).toBe(0);
    expect(host.querySelectorAll('.info-item').length).toBe(2);
    expect(host.querySelectorAll('.port-no-access').length).toBe(1);
  });
});
