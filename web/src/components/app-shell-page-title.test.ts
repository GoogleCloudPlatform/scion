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
 * Header title on routes that carry a query string (ptone/scion#3752): the
 * shell's currentPath includes the query, and the title must follow the
 * pathname alone.
 */

import { beforeAll, describe, expect, it } from 'vitest';

type Shell = HTMLElement & { currentPath: string };

function titleFor(path: string): string {
  const shell = document.createElement('scion-app') as Shell;
  shell.currentPath = path;
  return (shell as unknown as { getPageTitle(): string }).getPageTitle();
}

describe('app-shell page title with a query string', () => {
  beforeAll(async () => {
    await import('./app-shell.js');
  }, 30_000);

  it('names an exact-match route with and without a query string', () => {
    expect(titleFor('/agents')).toBe('Agents');
    expect(titleFor('/agents?phase=running')).toBe('Agents');
    expect(titleFor('/admin/users?q=ann&page=2')).toBe('Users');
  });

  it('names a pattern-match route with a query string or hash', () => {
    expect(titleFor('/projects/p-1/settings?tab=env')).toBe('Project Settings');
    expect(titleFor('/agents/new?project=p-1')).toBe('Create Agent');
    expect(titleFor('/terminals#a-1')).toBe('Terminals');
  });

  it('keeps the not-found title for an unknown route, with or without a query', () => {
    expect(titleFor('/no-such-page')).toBe('Page Not Found');
    expect(titleFor('/no-such-page?x=1')).toBe('Page Not Found');
  });
});
