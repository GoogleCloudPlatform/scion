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

import { describe, it, expect } from 'vitest';
import { pullLatestErrorMessage } from './pull-latest.js';

describe('pullLatestErrorMessage', () => {
  it('uses the message of a structured error object', () => {
    expect(
      pullLatestErrorMessage({ error: { code: 'pull_failed', message: 'Merge conflict' } })
    ).toBe('Merge conflict');
  });

  it('falls back to Pull failed for an error object without a message', () => {
    expect(pullLatestErrorMessage({ error: { code: 'pull_failed' } })).toBe('Pull failed');
  });

  it('uses detail when the error object has no message', () => {
    expect(
      pullLatestErrorMessage({ error: { code: 'pull_failed' }, detail: 'Not a git repo' })
    ).toBe('Not a git repo');
  });

  it('ignores a non-string message on the error object', () => {
    expect(pullLatestErrorMessage({ error: { message: { text: 'x' } } })).toBe('Pull failed');
  });

  it('uses a string detail', () => {
    expect(pullLatestErrorMessage({ detail: 'Workspace is busy' })).toBe('Workspace is busy');
  });

  it('uses a string error', () => {
    expect(pullLatestErrorMessage({ error: 'Remote unreachable' })).toBe('Remote unreachable');
  });

  it('prefers detail over a string error', () => {
    expect(pullLatestErrorMessage({ detail: 'From detail', error: 'From error' })).toBe(
      'From detail'
    );
  });

  it('falls back to Pull failed when nothing usable is present', () => {
    expect(pullLatestErrorMessage({})).toBe('Pull failed');
    expect(pullLatestErrorMessage(null)).toBe('Pull failed');
    expect(pullLatestErrorMessage('not json object')).toBe('Pull failed');
    expect(pullLatestErrorMessage({ detail: '', error: '' })).toBe('Pull failed');
  });

  it('appends a guidance hint from the structured error', () => {
    expect(
      pullLatestErrorMessage({
        error: { message: 'Local changes', details: { guidance: 'Commit or stash first' } },
      })
    ).toBe('Local changes — Commit or stash first');
  });

  it('always returns a string', () => {
    expect(typeof pullLatestErrorMessage({ error: { details: {} } })).toBe('string');
  });
});
