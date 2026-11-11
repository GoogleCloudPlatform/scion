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
 * Self-test of the IAP browser journey's configuration
 * (../harness/iap-journey-env.ts, ptone/scion#4240 X5): the journey skips
 * with a clear reason unless it is opted in and fully configured, and it
 * reads its target only from an IAP profile for an https origin.
 */

import { expect, test } from '@playwright/test';

import { iapJourneyConfig, profileTarget } from '../harness/iap-journey-env.js';

const FULL: NodeJS.ProcessEnv = {
  IAP_JOURNEY: '1',
  IAP_JOURNEY_PROFILE: 't1',
  IAP_JOURNEY_RUN: 'r1',
  SCION_METADATA_MODE: 'assign',
  GCE_METADATA_HOST: '127.0.0.1:8099',
};

function skipOf(env: NodeJS.ProcessEnv): string {
  const r = iapJourneyConfig(env);
  return 'skip' in r ? r.skip : '';
}

test('skips without the opt-in, as in CI and lanes', () => {
  expect(skipOf({})).toContain('operator-run only');
  expect(skipOf({ ...FULL, IAP_JOURNEY: '' })).toContain('operator-run only');
  expect(skipOf({ ...FULL, IAP_JOURNEY: 'true' })).toContain('operator-run only');
});

test('skips naming each missing variable', () => {
  const env = { ...FULL };
  delete env.IAP_JOURNEY_PROFILE;
  delete env.GCE_METADATA_HOST;
  const reason = skipOf(env);
  expect(reason).toContain('IAP_JOURNEY_PROFILE');
  expect(reason).toContain('GCE_METADATA_HOST');
  expect(reason).not.toContain('SCION_METADATA_MODE');
});

test('skips on a wrong metadata mode or a malformed name', () => {
  expect(skipOf({ ...FULL, SCION_METADATA_MODE: 'block' })).toContain('assign mode');
  expect(skipOf({ ...FULL, IAP_JOURNEY_PROFILE: '../x' })).toContain('lowercase');
  expect(skipOf({ ...FULL, IAP_JOURNEY_RUN: 'R1' })).toContain('lowercase');
});

test('a full environment configures the journey', () => {
  expect(iapJourneyConfig(FULL)).toEqual({
    config: { profile: 't1', run: 'r1', scionBin: 'scion' },
  });
  const r = iapJourneyConfig(
    { ...FULL, IAP_JOURNEY_RUN: '', SCION_BIN: '/opt/scion' },
    () => 36 ** 3
  );
  expect(r).toEqual({ config: { profile: 't1', run: 'iapj-1000', scionBin: '/opt/scion' } });
});

test('profileTarget reads an IAP profile for an https origin only', () => {
  const show = (p: object) => JSON.stringify({ profile: p, runs: [] });
  const iap = {
    name: 't1',
    endpoint: 'https://test-hub.example',
    transport: 'iap',
    iapAudience: '1234-abc.apps.googleusercontent.com',
    uatServiceAccount: 't1-uat@proj.iam.gserviceaccount.com',
  };
  expect(profileTarget(show(iap))).toEqual({
    hubURL: 'https://test-hub.example',
    driverServiceAccount: 't1-uat@proj.iam.gserviceaccount.com',
    iapAudience: '1234-abc.apps.googleusercontent.com',
  });
  expect(() => profileTarget(show({ ...iap, transport: 'none' }))).toThrow('transport iap');
  expect(() => profileTarget(show({ ...iap, endpoint: 'http://127.0.0.1:8080' }))).toThrow(
    'https origin'
  );
  expect(() => profileTarget(show({ ...iap, endpoint: 'https://test-hub.example/sub' }))).toThrow(
    'https origin'
  );
  expect(() => profileTarget(show({ ...iap, uatServiceAccount: '' }))).toThrow(
    'IAP service account'
  );
  expect(() => profileTarget(show({ ...iap, iapAudience: '' }))).toThrow('IAP audience');
  expect(() => profileTarget('not json')).toThrow('not JSON');
});
