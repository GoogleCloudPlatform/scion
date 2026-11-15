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
 * Tests for the best-effort resume lifecycle action (#1868): the shared
 * request-body builder used by the agent-detail, agents, and project-detail
 * pages so that a "Resume (best effort)" click on an error-phase agent asks
 * the Hub's /start endpoint to resume the harness session instead of
 * starting fresh.
 */

import { describe, it, expect } from 'vitest';
import type { Agent, Project } from './types.js';
import {
  getAgentDisplayStatus,
  isEmptyPerAgentWorkspace,
  isRetainedSuspendActivity,
  lifecycleActionRequestInit,
  projectWorkspaceModeIcon,
  RESUME_BEST_EFFORT_CONFIRM_MESSAGE,
} from './types.js';

describe('lifecycleActionRequestInit', () => {
  it('sends forceResume:true in a JSON body for force-resume', () => {
    const init = lifecycleActionRequestInit('force-resume');
    expect(init.method).toBe('POST');
    expect(init.headers).toEqual({ 'Content-Type': 'application/json' });
    expect(init.body).toBe(JSON.stringify({ forceResume: true }));
  });

  it.each(['start', 'stop', 'suspend', 'resume', 'delete'] as const)(
    'posts with no body for %s',
    (action) => {
      const init = lifecycleActionRequestInit(action);
      expect(init).toEqual({ method: 'POST' });
    }
  );
});

describe('RESUME_BEST_EFFORT_CONFIRM_MESSAGE', () => {
  it('warns that resume is best-effort and mentions the fresh-start alternative', () => {
    expect(RESUME_BEST_EFFORT_CONFIRM_MESSAGE).toContain('Resume');
    expect(RESUME_BEST_EFFORT_CONFIRM_MESSAGE).toContain('Start');
  });
});

describe('isEmptyPerAgentWorkspace', () => {
  const label = (mode: string) => ({ 'scion.dev/workspace-mode': mode });

  it('accepts a non-git project labelled per-agent or the raw empty-per-agent value', () => {
    expect(isEmptyPerAgentWorkspace({ labels: label('per-agent') })).toBe(true);
    expect(isEmptyPerAgentWorkspace({ labels: label('empty-per-agent') })).toBe(true);
  });

  it('rejects git projects and other non-git modes', () => {
    expect(
      isEmptyPerAgentWorkspace({ gitRemote: 'github.com/a/b', labels: label('per-agent') })
    ).toBe(false);
    expect(
      isEmptyPerAgentWorkspace({ gitRemote: 'github.com/a/b', labels: label('empty-per-agent') })
    ).toBe(false);
    expect(isEmptyPerAgentWorkspace({ labels: label('shared') })).toBe(false);
    expect(isEmptyPerAgentWorkspace({})).toBe(false);
    expect(isEmptyPerAgentWorkspace(undefined)).toBe(false);
  });
});

describe('projectWorkspaceModeIcon', () => {
  const project = (extra: Partial<Project>): Project => ({
    id: 'p',
    name: 'p',
    slug: 'p',
    agentCount: 0,
    ...extra,
  });
  const mode = (m: string) => ({ 'scion.dev/workspace-mode': m });
  const remote = 'https://github.com/org/repo.git';

  it.each([
    [{ gitRemote: remote }, 'git', 'Git repository, clone per agent'],
    [{ gitRemote: remote, labels: mode('bogus') }, 'git', 'Git repository, clone per agent'],
    [
      { gitRemote: remote, labels: mode('empty-per-agent') },
      'git',
      'Git repository, clone per agent',
    ],
    [{ gitRemote: remote, labels: mode('per-agent') }, 'git', 'Git repository, clone per agent'],
    [
      { gitRemote: remote, labels: mode('clone-per-agent') },
      'git',
      'Git repository, clone per agent',
    ],
    [{ gitRemote: remote, labels: mode('shared') }, 'git', 'Git repository, shared workspace'],
    [
      { gitRemote: remote, labels: mode('worktree-per-agent') },
      'git',
      'Git repository, worktree per agent',
    ],
    [{}, 'folder-fill', 'Shared directory'],
    [{ labels: mode('shared') }, 'folder-fill', 'Shared directory'],
    [{ labels: mode('per-agent') }, 'folder-plus', 'Empty directory per agent'],
    [{ labels: mode('empty-per-agent') }, 'folder-plus', 'Empty directory per agent'],
    [{ projectType: 'linked' }, 'folder-symlink', 'Linked project directory'],
  ] as [Partial<Project>, string, string][])('maps %j to %s', (extra, icon, label) => {
    expect(projectWorkspaceModeIcon(project(extra))).toEqual({ icon, label });
  });

  it('gives empty-per-agent and shared directories different icons', () => {
    expect(projectWorkspaceModeIcon(project({ labels: mode('per-agent') })).icon).not.toBe(
      projectWorkspaceModeIcon(project({})).icon
    );
  });
});

describe('isRetainedSuspendActivity and getAgentDisplayStatus', () => {
  it('recognises completed and idle as retained on suspend', () => {
    expect(isRetainedSuspendActivity('completed')).toBe(true);
    expect(isRetainedSuspendActivity('idle')).toBe(true);
    expect(isRetainedSuspendActivity('working')).toBe(false);
    expect(isRetainedSuspendActivity('blocked')).toBe(false);
    expect(isRetainedSuspendActivity('waiting_for_input')).toBe(false);
    expect(isRetainedSuspendActivity('')).toBe(false);
    expect(isRetainedSuspendActivity(undefined)).toBe(false);
  });

  it('returns suspended as the display status even when activity is retained', () => {
    expect(
      getAgentDisplayStatus({
        id: 'a1',
        name: 'a1',
        phase: 'suspended',
        activity: 'completed',
      } as Agent)
    ).toBe('suspended');
    expect(
      getAgentDisplayStatus({
        id: 'a2',
        name: 'a2',
        phase: 'suspended',
        activity: 'idle' as Agent['activity'],
      } as Agent)
    ).toBe('suspended');
    expect(
      getAgentDisplayStatus({
        id: 'a3',
        name: 'a3',
        phase: 'running',
        activity: 'completed',
      } as Agent)
    ).toBe('completed');
  });
});
