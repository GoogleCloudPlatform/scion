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
import { describe, expect, it } from 'vitest';
import {
  INBOX_NOTHING_WAITING,
  formatInboxProse,
  formatInboxProseSegments,
  type ProseSegment,
} from './inbox-prose.js';

interface ProseCase {
  name: string;
  category: string;
  count: number;
  capped: boolean;
  breakdown?: Record<string, number>;
  expected: string;
  segments: ProseSegment[] | null;
}

const THIS_DIR = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = resolve(THIS_DIR, '..', '..', '..');
const FIXTURE_PATH = resolve(REPO_ROOT, 'pkg/hubclient/testdata/inbox_prose.json');

const fixture = JSON.parse(readFileSync(FIXTURE_PATH, 'utf-8')) as { cases: ProseCase[] };

describe('formatInboxProse & formatInboxProseSegments (shared fixture)', () => {
  it('loads non-empty shared fixture', () => {
    expect(fixture.cases.length).toBeGreaterThan(0);
  });

  for (const tc of fixture.cases) {
    it(tc.name, () => {
      expect(tc).toHaveProperty('segments');
      const segments = formatInboxProseSegments(tc.category, tc.count, tc.capped, tc.breakdown);
      if (tc.segments === null) {
        expect(segments).toBe('nothing');
        expect(tc.expected).toBe(INBOX_NOTHING_WAITING);
      } else {
        expect(segments).toEqual(tc.segments);
        const joined = (segments as ProseSegment[]).map((s) => s.text).join('');
        expect(joined).toBe(tc.expected);
      }
      expect(formatInboxProse(tc.category, tc.count, tc.capped, tc.breakdown)).toBe(tc.expected);
    });
  }
});
