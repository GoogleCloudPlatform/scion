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
 * Names of artifact owners and publishers: labels, and which lookups are
 * remembered.
 */

import { afterEach, describe, expect, it, vi } from 'vitest';

import { principalLabel, principalName, resetPrincipalNames } from './principal-names.js';

function stubStatus(statuses: number[]): string[] {
  const urls: string[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      urls.push(String(input));
      const status = statuses.shift() ?? 200;
      return Promise.resolve(
        status === 200
          ? new Response(JSON.stringify({ name: 'docs-writer' }), { status })
          : new Response('{"error":{"code":"x","message":"no"}}', { status })
      );
    })
  );
  return urls;
}

describe('principal names', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    resetPrincipalNames();
  });

  it('labels the signed-in user, agents and users', () => {
    expect(principalLabel('user', 'u-1', 'Jane', 'u-1')).toBe('You');
    expect(principalLabel('user', 'u-2', 'Jane', 'u-1')).toBe('Jane');
    expect(principalLabel('agent', 'a-1', 'docs-writer')).toBe('docs-writer (agent)');
    expect(principalLabel('agent', '0123456789abcdef', '')).toBe('01234567… (agent)');
  });

  it('remembers a name, a refusal and an absent principal', async () => {
    for (const status of [200, 403, 404]) {
      resetPrincipalNames();
      const urls = stubStatus([status, 200]);
      const first = await principalName('agent', 'a-1');
      const second = await principalName('agent', 'a-1');
      expect(second).toBe(first);
      expect(urls).toHaveLength(1);
    }
  });

  it('looks a principal up again after a server error', async () => {
    const urls = stubStatus([500, 200]);
    expect(await principalName('user', 'u-2')).toBe('');
    expect(await principalName('user', 'u-2')).toBe('docs-writer');
    expect(urls).toEqual(['/api/v1/users/u-2', '/api/v1/users/u-2']);
  });

  it('looks a principal up again after a network error', async () => {
    let fail = true;
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        fail
          ? Promise.reject(new TypeError('network'))
          : Promise.resolve(new Response(JSON.stringify({ slug: 'bot' }), { status: 200 }))
      )
    );
    expect(await principalName('agent', 'a-2')).toBe('');
    fail = false;
    expect(await principalName('agent', 'a-2')).toBe('bot');
  });
});
