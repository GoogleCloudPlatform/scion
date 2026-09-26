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
 * ptone/scion#1970: cross-language parity for the PTY close-code classifier.
 * The same fixture drives TestClassifyPTYClose_Parity in pkg/wsprotocol.
 */
import { describe, expect, it } from 'vitest';
import fixture from '../../../pkg/wsprotocol/testdata/pty_close_codes.json';
import { PTY_CLOSE, classifyPtyClose } from './terminal-close-codes.js';

interface FixtureRow {
  code: number;
  name: string;
  disposition: string;
}

const rows = (fixture as { codes: FixtureRow[] }).codes;

describe('classifyPtyClose parity with pkg/wsprotocol', () => {
  it('has fixture rows', () => {
    expect(rows.length).toBeGreaterThan(0);
  });

  it.each(rows.map((row) => [row.code, row.name, row.disposition] as const))(
    'code %i (%s) is %s',
    (code, _name, disposition) => {
      expect(classifyPtyClose(code)).toBe(disposition);
    }
  );

  it('covers every named contract constant', () => {
    const codes = new Set(rows.map((row) => row.code));
    for (const code of Object.values(PTY_CLOSE)) {
      expect(codes.has(code)).toBe(true);
    }
  });
});
