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

import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';
import {
  TRAJECTORY_NOTICE_VERSION,
  formatTrajectoryCaptureNotice,
  isTrajectoryCaptureEffective,
} from './trajectory-capture-notice.js';

interface NoticeFixture {
  notice_version: string;
  cases: Array<{
    policy: string;
    enforced_by_hub: boolean;
    effective: boolean;
    notice: string;
  }>;
}

const here = dirname(fileURLToPath(import.meta.url));
const fixturePath = resolve(
  here,
  '../../../pkg/telemetrycontract/testdata/trajectory_capture_notices.json'
);

describe('trajectory-capture-notice parity with Go telemetrycontract', () => {
  it('matches pkg/telemetrycontract/testdata/trajectory_capture_notices.json', () => {
    const fixture = JSON.parse(readFileSync(fixturePath, 'utf8')) as NoticeFixture;
    expect(TRAJECTORY_NOTICE_VERSION).toBe(fixture.notice_version);
    expect(fixture.cases.length).toBeGreaterThan(0);

    for (const tc of fixture.cases) {
      expect(isTrajectoryCaptureEffective(tc.policy)).toBe(tc.effective);
      expect(
        formatTrajectoryCaptureNotice({
          policy: tc.policy,
          enforced_by_hub: tc.enforced_by_hub,
        })
      ).toBe(tc.notice);
    }
  });

  it('returns empty string for undefined status', () => {
    expect(formatTrajectoryCaptureNotice(undefined)).toBe('');
    expect(isTrajectoryCaptureEffective(undefined)).toBe(false);
  });
});
