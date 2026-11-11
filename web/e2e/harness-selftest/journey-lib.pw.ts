// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Unit checks of the async create journey's pure helpers (no hub, no
 * browser): timeout parsing, the absence predicate used by its cleanup,
 * the evidence path redaction, the launch step readers and the credential
 * guard.
 */

import { expect, test } from '@playwright/test';

import {
  assertNoHarnessCredentialEnv,
  harnessCredentialEnvNames,
} from '../async-create/credential-guard.js';
import {
  classifyRead,
  isGone,
  launchStepProblems,
  parseTimeoutMs,
  redactPath,
  stepSequence,
} from '../async-create/journey-lib.js';
import { LAUNCH_STEPS, uiStepLabel, uiStepName } from '../async-create/selectors.js';

test('parseTimeoutMs takes a positive number and refuses anything else', () => {
  expect(parseTimeoutMs(undefined, 5, 'X')).toBe(5);
  expect(parseTimeoutMs('', 5, 'X')).toBe(5);
  expect(parseTimeoutMs('1000', 5, 'X')).toBe(1000);
  for (const bad of ['abc', '0', '-1', 'NaN', 'Infinity']) {
    expect(() => parseTimeoutMs(bad, 5, 'X'), bad).toThrow(/X must be a positive number/);
  }
});

test('classifyRead: only a 404 or a soft-deleted record is gone', () => {
  const cases: Array<[number, unknown, string, boolean]> = [
    [404, null, 'not_found', true],
    [200, { agent: { id: 'a' } }, 'present', false],
    [200, { id: 'a' }, 'present', false],
    [200, { agent: { deletedAt: '0001-01-01T00:00:00Z' } }, 'present', false],
    [200, { agent: { deletedAt: '' } }, 'present', false],
    [200, { agent: { deletedAt: '2026-10-12T19:00:00Z' } }, 'soft_deleted', true],
    [200, { deletedAt: '2026-10-12T19:00:00Z' }, 'soft_deleted', true],
    // A record the hub is still deleting is present.
    [200, { agent: { deletion: { state: 'deleting' } } }, 'present', false],
    [403, null, 'unknown', false],
    [401, null, 'unknown', false],
    [500, null, 'unknown', false],
  ];
  for (const [status, body, state, gone] of cases) {
    const got = classifyRead(status, body, 'agent');
    expect(got, `${status} ${JSON.stringify(body)}`).toBe(state);
    expect(isGone(got)).toBe(gone);
  }
});

test('uiStepName turns a launch-step text back into the raw step name', () => {
  expect(uiStepName('Step: pulling the image')).toBe('image_pull');
  expect(uiStepName(' Step:  removing a leftover   from an earlier agent ')).toBe('pre_clean');
  expect(uiStepName('Step: preparing the container (image pull, volume mounts)')).toBe(
    'container_create'
  );
  expect(uiStepName('Step: launching')).toBe('launching');
  // A step without a label is shown with "_" as a space.
  expect(uiStepName('Step: warming up')).toBe('warming_up');
  expect(uiStepName('')).toBe('');
});

test('uiStepLabel and uiStepName are inverse for every canonical step', () => {
  expect(uiStepLabel('pre_clean')).toBe('Step: removing a leftover from an earlier agent');
  expect(uiStepLabel('warming_up')).toBe('Step: warming up');
  for (const step of LAUNCH_STEPS) expect(uiStepName(uiStepLabel(step))).toBe(step);
});

test('launchStepProblems: canonical SSE order, page labels a matching subsequence', () => {
  const check = (sse: string[], ui: string[]): string[] =>
    launchStepProblems(sse, ui, LAUNCH_STEPS, uiStepLabel);
  const preClean = 'Step: removing a leftover from an earlier agent';
  expect(check(['pre_clean', 'launching'], [preClean, 'Step: launching'])).toEqual([]);
  expect(check(['launching'], [])).toEqual([]);
  expect(check([], [])).toEqual([]);
  expect(check(['pre_clean', 'launching'], ['Step: launching'])).toEqual([]);
  expect(check(['launching', 'pre_clean'], [])).toEqual([
    'SSE step "pre_clean" goes back in the canonical order',
  ]);
  expect(check(['launching', 'pre_clean', 'launching'], [])).toEqual([
    'SSE step "pre_clean" goes back in the canonical order',
    'SSE step "launching" goes back in the canonical order',
  ]);
  expect(check(['warming_up'], [])).toEqual(['SSE step "warming_up" is not in the canonical list']);
  expect(check(['launching'], ['Step: pulling the image'])).toEqual([
    'page label "Step: pulling the image" matches no SSE step',
  ]);
  expect(check(['launching'], ['launching'])).toEqual([
    'page label "launching" matches no SSE step',
  ]);
  expect(check(['pre_clean', 'launching'], ['Step: launching', preClean])).toEqual([
    `page label "${preClean}" is out of the SSE step order`,
  ]);
});

test('stepSequence keeps each step once per run, in order, with its first time', () => {
  expect(
    stepSequence([
      { t: 1, step: '' },
      { t: 2, step: 'pre_clean' },
      { t: 3, step: 'pre_clean' },
      { t: 4 },
      { t: 5, step: 'launching' },
      { t: 6, step: null },
      { t: 7, step: 'pre_clean' },
    ])
  ).toEqual([
    { t: 2, step: 'pre_clean' },
    { t: 5, step: 'launching' },
    // A step that comes back is listed again, so a backwards move shows.
    { t: 7, step: 'pre_clean' },
  ]);
  expect(stepSequence([])).toEqual([]);
});

test('the credential guard knows the shipped harness credential variables', () => {
  const names = harnessCredentialEnvNames();
  for (const name of ['ANTHROPIC_API_KEY', 'GEMINI_API_KEY', 'GOOGLE_APPLICATION_CREDENTIALS']) {
    expect(names).toContain(name);
  }
});

test('the credential guard refuses a set credential variable, naming it but not its value', () => {
  expect(() => assertNoHarnessCredentialEnv({ PATH: '/usr/bin' })).not.toThrow();
  const marker = 'not-a-real-key-7c1e';
  let message = '';
  try {
    assertNoHarnessCredentialEnv({ ANTHROPIC_API_KEY: marker });
  } catch (err) {
    message = (err as Error).message;
  }
  expect(message).toContain('ANTHROPIC_API_KEY');
  expect(message).not.toContain(marker);
  // An empty value counts as unset.
  expect(() => assertNoHarnessCredentialEnv({ ANTHROPIC_API_KEY: '' })).not.toThrow();
});

test('redactPath keeps ordinary paths and redacts secret-like segments', () => {
  const id = '3f2c1a9e-7b4d-4c1e-9a2b-0c1d2e3f4a5b';
  for (const keep of [
    '/api/v1/agents',
    `/api/v1/agents/${id}`,
    `/api/v1/agents/${id}/start`,
    '/assets/main-BisI-P2R.js',
    '/auth/test-identity/redeem',
    '/events',
  ]) {
    expect(redactPath(keep), keep).toBe(keep);
  }
  expect(redactPath('/invite/tiwc_abc')).toBe('/invite/<redacted>');
  expect(redactPath('/x/scion_dev_0123abcd/y')).toBe('/x/<redacted>/y');
  expect(redactPath(`/x/${'a1B2'.repeat(10)}`)).toBe('/x/<redacted>');
  expect(redactPath('/x/eyJhbGciOiJIUzI1NiJ9')).toBe('/x/<redacted>');
  // Percent-encoded: the decoded segment is checked.
  expect(redactPath('/x/tiwc%5Fabc')).toBe('/x/<redacted>');
  // Known limit: a short segment with an unknown prefix is kept.
  expect(redactPath('/x/abc123')).toBe('/x/abc123');
});
