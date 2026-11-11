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
 * Header title for the Edit page (ptone/scion#3975): "Edit Agent". The
 * retired Configure page's title is gone; its URL redirects to the Edit page
 * before the shell renders.
 */

import { beforeAll, describe, expect, it } from 'vitest';

type Shell = HTMLElement & { currentPath: string };

function titleFor(path: string): string {
  const shell = document.createElement('scion-app') as Shell;
  shell.currentPath = path;
  return (shell as unknown as { getPageTitle(): string }).getPageTitle();
}

describe('app-shell agent edit page title', () => {
  beforeAll(async () => {
    await import('./app-shell.js');
  }, 30_000);

  it('reads "Edit Agent" on the Edit page', () => {
    expect(titleFor('/agents/a-1/edit')).toBe('Edit Agent');
  });

  it('has no Configure Agent title any more', () => {
    expect(titleFor('/agents/a-1/configure')).not.toBe('Configure Agent');
  });
});
