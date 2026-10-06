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
 * Header title for the artifact page (ptone/scion#3213): "Artifact" when
 * hub.artifacts is on, the generic project title when it is off.
 */

import { afterEach, beforeAll, describe, expect, it } from 'vitest';

type Shell = HTMLElement & { currentPath: string };

function titleFor(path: string): string {
  const shell = document.createElement('scion-app') as Shell;
  shell.currentPath = path;
  return (shell as unknown as { getPageTitle(): string }).getPageTitle();
}

describe('app-shell artifact page title', () => {
  beforeAll(async () => {
    await import('./app-shell.js');
  }, 30_000);

  afterEach(() => {
    delete window.__SCION_FEATURES__;
  });

  it('names the artifact page when the experiment is on', () => {
    window.__SCION_FEATURES__ = { 'hub.artifacts': true };
    expect(titleFor('/projects/p-1/artifacts/a-1')).toBe('Artifact');
  });

  it('falls back to the project title when the experiment is off', () => {
    window.__SCION_FEATURES__ = { 'hub.artifacts': false };
    expect(titleFor('/projects/p-1/artifacts/a-1')).toBe('Project');
  });
});
