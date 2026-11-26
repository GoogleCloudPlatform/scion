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

import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { nothing, render } from 'lit';

import {
  canForceSuspend,
  forceKeepSuspendedConfirmMessage,
  forceSuspendConfirmMessage,
  isNeverSuspendActive,
  neverSuspendBadge,
} from './never-suspend.js';
import { lifecycleActionRequestInit } from './types.js';
import {
  AGENT_NEVER_SUSPEND_MESSAGE,
  FORCE_SUSPEND_FORBIDDEN_MESSAGE,
  lifecycleActionErrorMessage,
} from '../client/agent-delete.js';
import {
  NEVER_SUSPEND_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../utils/feature-flags.js';
import { agentToFilterRow } from './filter-query/agent-accessors.js';

function hubError(status: number, code: string, message: string): Response {
  return new Response(JSON.stringify({ error: { code, message } }), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('never-suspend helpers', () => {
  beforeEach(() => {
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  afterEach(() => {
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  it('isNeverSuspendActive is false when experiment is off even for flagged agents', () => {
    expect(isNeverSuspendActive({ suspendPolicy: 'never' })).toBe(false);
    setFeatureFlag(NEVER_SUSPEND_FLAG, false);
    expect(isNeverSuspendActive({ suspendPolicy: 'never' })).toBe(false);
  });

  it('isNeverSuspendActive is true only when experiment is on and policy is never', () => {
    setFeatureFlag(NEVER_SUSPEND_FLAG, true);
    expect(isNeverSuspendActive({ suspendPolicy: 'never' })).toBe(true);
    expect(isNeverSuspendActive({ suspendPolicy: 'auto' })).toBe(false);
    expect(isNeverSuspendActive({})).toBe(false);
    expect(isNeverSuspendActive(null)).toBe(false);
  });

  it('canForceSuspend requires lifecycle capability and human user/dev principal', () => {
    const lifecycleCap = { actions: ['read', 'lifecycle'] };
    const readOnlyCap = { actions: ['read'] };

    expect(canForceSuspend(readOnlyCap)).toBe(false);
    expect(canForceSuspend(undefined)).toBe(false);
    expect(canForceSuspend(lifecycleCap)).toBe(true);
    expect(
      canForceSuspend(lifecycleCap, {
        id: 'u1',
        email: 'a@example.com',
        name: 'Alice',
        principalKind: 'user',
      })
    ).toBe(true);
    expect(
      canForceSuspend(lifecycleCap, {
        id: 'dev',
        email: 'dev@example.com',
        name: 'Dev',
        principalKind: 'dev',
      })
    ).toBe(true);
    expect(
      canForceSuspend(lifecycleCap, {
        id: 'f1',
        email: 'v@remote.example',
        name: 'Visitor',
        federated: true,
      })
    ).toBe(false);
    expect(
      canForceSuspend(lifecycleCap, {
        id: 'a1',
        email: '',
        name: 'Agent',
        principalKind: 'agent',
      })
    ).toBe(false);
    expect(
      canForceSuspend(lifecycleCap, {
        id: 'v1',
        email: 'v@remote.example',
        name: 'Visitor',
        principalKind: 'federated',
      })
    ).toBe(false);
  });

  it('neverSuspendBadge renders nothing when inactive and a badge when active', () => {
    expect(neverSuspendBadge({ suspendPolicy: 'never' })).toBe(nothing);

    setFeatureFlag(NEVER_SUSPEND_FLAG, true);
    expect(neverSuspendBadge({ suspendPolicy: 'auto' })).toBe(nothing);

    const container = document.createElement('div');
    render(neverSuspendBadge({ suspendPolicy: 'never' }), container);
    const badge = container.querySelector('[data-testid="never-suspend-badge"]');
    expect(badge).not.toBeNull();
    expect(badge?.textContent).toContain('Never suspend');
    expect(badge?.querySelector('sl-icon')?.getAttribute('name')).toBe('shield-check');
  });

  it('builds force-suspend request init with force=true JSON body', () => {
    const init = lifecycleActionRequestInit('force-suspend');
    expect(init.method).toBe('POST');
    expect(init.headers).toEqual({ 'Content-Type': 'application/json' });
    expect(JSON.parse(init.body as string)).toEqual({ force: true });
  });

  it('formats 409 agent_never_suspend and 403 force_suspend_forbidden sensibly', async () => {
    expect(
      await lifecycleActionErrorMessage(
        hubError(409, 'agent_never_suspend', 'agent "lead" has suspend policy "never"'),
        'fallback'
      )
    ).toBe(AGENT_NEVER_SUSPEND_MESSAGE);

    expect(
      await lifecycleActionErrorMessage(
        hubError(403, 'force_suspend_forbidden', 'only human principals may force-suspend'),
        'fallback'
      )
    ).toBe(FORCE_SUSPEND_FORBIDDEN_MESSAGE);
  });

  it('includes agent name in force-suspend confirmation messages', () => {
    expect(forceSuspendConfirmMessage('lead-agent')).toContain('lead-agent');
    expect(forceKeepSuspendedConfirmMessage('lead-agent')).toContain('lead-agent');
  });

  it('exposes suspendPolicy to client-side filter-query evaluation', () => {
    const flaggedRow = agentToFilterRow({
      id: 'a1',
      name: 'lead',
      projectId: 'p1',
      template: 'default',
      phase: 'running',
      suspendPolicy: 'never',
    });
    const autoRow = agentToFilterRow({
      id: 'a2',
      name: 'worker',
      projectId: 'p1',
      template: 'default',
      phase: 'running',
    });
    expect(flaggedRow.values('suspendPolicy')).toEqual(['never']);
    expect(autoRow.values('suspendPolicy')).toEqual(['auto']);
  });
});
