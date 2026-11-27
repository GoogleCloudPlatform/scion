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

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { nothing, render } from 'lit';

const confirmMock = vi.fn<(message: string, opts?: unknown) => Promise<boolean>>();
vi.mock('../components/shared/confirm-dialog.js', () => ({
  showConfirm: (message: string, opts?: unknown): Promise<boolean> => confirmMock(message, opts),
}));

import {
  canForceSuspend,
  FORCE_SUSPEND_CONFIRM_OPTIONS,
  forceKeepSuspendedConfirmMessage,
  forceSuspendConfirmMessage,
  isNeverSuspendActive,
  neverSuspendBadge,
  resolveSuspendAction,
  showSuspendAction,
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
    confirmMock.mockReset();
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

  it('canForceSuspend requires lifecycle capability', () => {
    expect(canForceSuspend({ actions: ['read', 'lifecycle'] })).toBe(true);
    expect(canForceSuspend({ actions: ['read'] })).toBe(false);
    expect(canForceSuspend({ actions: [] })).toBe(false);
    expect(canForceSuspend(undefined)).toBe(false);
  });

  it('showSuspendAction requires resumable harness and lifecycle capability', () => {
    const lifecycleCap = { actions: ['read', 'lifecycle'] };
    const readOnlyCap = { actions: ['read'] };

    expect(showSuspendAction({ suspendPolicy: 'never', _capabilities: lifecycleCap })).toBe(true);
    expect(showSuspendAction({ suspendPolicy: 'never', _capabilities: readOnlyCap })).toBe(false);
    expect(showSuspendAction({ suspendPolicy: 'auto', _capabilities: readOnlyCap })).toBe(false);
    expect(
      showSuspendAction({
        suspendPolicy: 'never',
        _capabilities: lifecycleCap,
        harnessCapabilities: { resume: { support: 'no' } },
      })
    ).toBe(false);
    expect(
      showSuspendAction({ suspendPolicy: 'never', _capabilities: readOnlyCap }, lifecycleCap)
    ).toBe(true);
  });

  it('resolveSuspendAction confirms flagged agents when experiment is on and skips dialog otherwise', async () => {
    // Experiment off + flagged -> plain suspend, no dialog
    expect(await resolveSuspendAction({ name: 'lead', suspendPolicy: 'never' })).toBe('suspend');
    expect(confirmMock).not.toHaveBeenCalled();

    // Experiment on + unflagged -> plain suspend, no dialog
    setFeatureFlag(NEVER_SUSPEND_FLAG, true);
    expect(await resolveSuspendAction({ name: 'worker', suspendPolicy: 'auto' })).toBe('suspend');
    expect(await resolveSuspendAction({ name: 'worker' })).toBe('suspend');
    expect(confirmMock).not.toHaveBeenCalled();

    // Experiment on + flagged + confirm -> force-suspend
    confirmMock.mockResolvedValueOnce(true);
    expect(await resolveSuspendAction({ name: 'lead', suspendPolicy: 'never' })).toBe(
      'force-suspend'
    );
    expect(confirmMock).toHaveBeenCalledWith(
      forceSuspendConfirmMessage('lead'),
      FORCE_SUSPEND_CONFIRM_OPTIONS
    );

    // Experiment on + flagged + cancel -> null (no request)
    confirmMock.mockResolvedValueOnce(false);
    expect(await resolveSuspendAction({ name: 'lead', suspendPolicy: 'never' })).toBeNull();

    // Keep suspended uses keep-suspended confirm copy
    confirmMock.mockResolvedValueOnce(true);
    expect(
      await resolveSuspendAction({ name: 'lead', suspendPolicy: 'never' }, { keepSuspended: true })
    ).toBe('force-suspend');
    expect(confirmMock).toHaveBeenLastCalledWith(
      forceKeepSuspendedConfirmMessage('lead'),
      FORCE_SUSPEND_CONFIRM_OPTIONS
    );
  });

  it('neverSuspendBadge renders nothing when inactive and a quiet text pill without icon when active', () => {
    expect(neverSuspendBadge({ suspendPolicy: 'never' })).toBe(nothing);

    setFeatureFlag(NEVER_SUSPEND_FLAG, true);
    expect(neverSuspendBadge({ suspendPolicy: 'auto' })).toBe(nothing);

    const container = document.createElement('div');
    render(neverSuspendBadge({ suspendPolicy: 'never' }), container);
    const badge = container.querySelector('[data-testid="never-suspend-badge"]');
    expect(badge).not.toBeNull();
    expect(badge?.tagName.toLowerCase()).toBe('span');
    expect(badge?.textContent?.trim()).toBe('Never suspend');
    expect(badge?.querySelector('sl-icon')).toBeNull();
    expect(badge?.getAttribute('title')).toContain('Never suspend');
    expect(badge?.getAttribute('aria-label')).toContain('Never suspend');
    expect(badge?.classList.contains('never-suspend-badge')).toBe(true);
    expect(badge?.classList.contains('never-suspend-badge--compact')).toBe(false);

    const compactContainer = document.createElement('div');
    render(neverSuspendBadge({ suspendPolicy: 'never' }, { compact: true }), compactContainer);
    const compactBadge = compactContainer.querySelector('[data-testid="never-suspend-badge"]');
    expect(compactBadge?.classList.contains('never-suspend-badge--compact')).toBe(true);
  });

  it('builds force-suspend request init with force=true JSON body', () => {
    const init = lifecycleActionRequestInit('force-suspend');
    expect(init.method).toBe('POST');
    expect(init.headers).toEqual({ 'Content-Type': 'application/json' });
    expect(JSON.parse(init.body as string)).toEqual({ force: true });
  });

  it('formats 409 agent_never_suspend (with reload hint) and 403 force_suspend_forbidden sensibly', async () => {
    const msg409 = await lifecycleActionErrorMessage(
      hubError(409, 'agent_never_suspend', 'agent "lead" has suspend policy "never"'),
      'fallback'
    );
    expect(msg409).toBe(AGENT_NEVER_SUSPEND_MESSAGE);
    expect(msg409).toContain('Reload to see the current policy');

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
