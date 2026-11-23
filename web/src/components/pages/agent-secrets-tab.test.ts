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

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import type { Agent } from '../../shared/types.js';
import type { ScionSecretList } from '../shared/secret-list.js';
import type { ScionPageAgentDetail } from './agent-detail.js';
import type { ScionPageAgentEdit } from './agent-edit.js';
import { AGENT_SECRETS_FLAG, setFeatureFlag } from '../../utils/feature-flags.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import { hubEditability } from '../../shared/__fixtures__/agent-editability.js';

vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

function makeAgent(capabilities?: { actions: string[] }): Agent {
  return {
    id: 'agent-123',
    name: 'worker-1',
    projectId: 'proj-1',
    template: 'default',
    phase: 'running',
    stateVersion: 1,
    created: '2026-11-20T00:00:00Z',
    updated: '2026-11-21T00:00:00Z',
    appliedConfig: { model: 'claude-opus', inlineConfig: {} },
    editability: hubEditability('running'),
    _capabilities: capabilities,
  } as unknown as Agent;
}

function stubFetch(agent: Agent): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = requestUrl(input);
      if (url.startsWith('/api/v1/secrets')) {
        return Promise.resolve(
          new Response(JSON.stringify({ secrets: [] }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      if (url === `/api/v1/agents/${agent.id}`) {
        return Promise.resolve(
          new Response(JSON.stringify(agent), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      return Promise.resolve(
        new Response(JSON.stringify({}), {
          status: 404,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    })
  );
}

async function flush(el: HTMLElement & { updateComplete: Promise<unknown> }): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

describe('agent detail and edit pages — Secrets tab experiment gate', () => {
  beforeAll(async () => {
    await Promise.all([import('./agent-detail.js'), import('./agent-edit.js')]);
  }, 30_000);

  beforeEach(() => {
    delete window.__SCION_FEATURES__;
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    delete window.__SCION_FEATURES__;
  });

  describe('scion-page-agent-detail', () => {
    async function mountDetail(agent: Agent): Promise<ScionPageAgentDetail> {
      stubFetch(agent);
      const el = document.createElement('scion-page-agent-detail') as ScionPageAgentDetail;
      el.agentId = agent.id;
      document.body.appendChild(el);
      await flush(el);
      return el;
    }

    it('hides the Secrets tab when hub.agent_secrets is off', async () => {
      const el = await mountDetail(makeAgent({ actions: ['read', 'update'] }));
      expect(el.shadowRoot!.querySelector('[data-testid="agent-secrets-tab"]')).toBeNull();
      expect(el.shadowRoot!.querySelector('scion-secret-list')).toBeNull();
    });

    it('renders the Secrets tab with agent-scoped secret-list when hub.agent_secrets is on', async () => {
      window.__SCION_FEATURES__ = { [AGENT_SECRETS_FLAG]: true };
      const el = await mountDetail(makeAgent({ actions: ['read', 'update'] }));

      const tab = el.shadowRoot!.querySelector('[data-testid="agent-secrets-tab"]');
      expect(tab?.textContent?.trim()).toBe('Secrets');
      const list = el.shadowRoot!.querySelector('scion-secret-list') as ScionSecretList | null;
      expect(list).not.toBeNull();
      expect(list!.scope).toBe('agent');
      expect(list!.scopeId).toBe('agent-123');
      expect(list!.readonly).toBe(false);
    });

    it('marks secret-list readonly when viewer lacks agent.update capability', async () => {
      window.__SCION_FEATURES__ = { [AGENT_SECRETS_FLAG]: true };
      const el = await mountDetail(makeAgent({ actions: ['read'] }));
      const list = el.shadowRoot!.querySelector('scion-secret-list') as ScionSecretList | null;
      expect(list).not.toBeNull();
      expect(list!.readonly).toBe(true);
    });

    it('shows the Secrets tab reactively when the feature flag turns on', async () => {
      const el = await mountDetail(makeAgent({ actions: ['read', 'update'] }));
      expect(el.shadowRoot!.querySelector('[data-testid="agent-secrets-tab"]')).toBeNull();

      setFeatureFlag(AGENT_SECRETS_FLAG, true);
      await flush(el);
      expect(el.shadowRoot!.querySelector('[data-testid="agent-secrets-tab"]')).not.toBeNull();
    });
  });

  describe('scion-page-agent-edit', () => {
    async function mountEdit(agent: Agent): Promise<ScionPageAgentEdit> {
      window.history.pushState({}, '', `/agents/${agent.id}/edit`);
      stubFetch(agent);
      const el = document.createElement('scion-page-agent-edit') as ScionPageAgentEdit;
      document.body.appendChild(el);
      await flush(el);
      return el;
    }

    it('hides the Secrets tab when hub.agent_secrets is off', async () => {
      const el = await mountEdit(makeAgent({ actions: ['read', 'update'] }));
      expect(el.shadowRoot!.querySelector('[data-testid="agent-secrets-tab"]')).toBeNull();
      expect(el.shadowRoot!.querySelector('scion-secret-list')).toBeNull();
    });

    it('renders the Secrets tab with agent-scoped secret-list when hub.agent_secrets is on', async () => {
      window.__SCION_FEATURES__ = { [AGENT_SECRETS_FLAG]: true };
      const el = await mountEdit(makeAgent({ actions: ['read', 'update'] }));

      const tab = el.shadowRoot!.querySelector('[data-testid="agent-secrets-tab"]');
      expect(tab?.textContent?.trim()).toBe('Secrets');
      const list = el.shadowRoot!.querySelector('scion-secret-list') as ScionSecretList | null;
      expect(list).not.toBeNull();
      expect(list!.scope).toBe('agent');
      expect(list!.scopeId).toBe('agent-123');
      expect(list!.readonly).toBe(false);
    });

    it('marks secret-list readonly on edit page when viewer lacks agent.update', async () => {
      window.__SCION_FEATURES__ = { [AGENT_SECRETS_FLAG]: true };
      const el = await mountEdit(makeAgent({ actions: ['read'] }));
      const list = el.shadowRoot!.querySelector('scion-secret-list') as ScionSecretList | null;
      expect(list).not.toBeNull();
      expect(list!.readonly).toBe(true);
    });
  });
});
