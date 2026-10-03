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

import { afterEach, describe, expect, it } from 'vitest';
import type { Project } from '../../shared/types.js';
import './git-remote-display.js';
import type { ScionGitRemoteDisplay } from './git-remote-display.js';

const BASE: Project = {
  id: 'p-1',
  name: 'Project One',
  path: '',
  status: 'active',
  agentCount: 0,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};

async function mount(project: Partial<Project>): Promise<ScionGitRemoteDisplay> {
  const el = document.createElement('scion-git-remote-display') as ScionGitRemoteDisplay;
  el.project = { ...BASE, ...project };
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function text(el: ScionGitRemoteDisplay): string {
  return (el.shadowRoot?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

afterEach(() => document.body.replaceChildren());

describe('scion-git-remote-display workspace mode', () => {
  it('shows "Empty directory per agent" for a non-git per-agent project', async () => {
    const el = await mount({ labels: { 'scion.dev/workspace-mode': 'per-agent' } });
    expect(text(el)).toBe('Empty directory per agent');
  });

  it('shows "Hub-managed workspace" for a shared workspace directory project', async () => {
    expect(text(await mount({}))).toBe('Hub-managed workspace');
    expect(text(await mount({ labels: { 'scion.dev/workspace-mode': 'shared' } }))).toBe(
      'Hub-managed workspace'
    );
  });

  it('shows "Linked project" for a linked project', async () => {
    const el = await mount({ projectType: 'linked' });
    expect(text(el)).toBe('Linked project');
  });

  it('keeps the clone-per-agent decorator for a git per-agent project', async () => {
    const el = await mount({
      gitRemote: 'https://github.com/acme/widgets.git',
      labels: { 'scion.dev/workspace-mode': 'per-agent' },
    });
    expect(text(el)).not.toContain('Empty directory per agent');
    expect(el.shadowRoot?.querySelector('sl-tooltip')?.getAttribute('content')).toBe(
      'Clone per agent'
    );
  });
});
