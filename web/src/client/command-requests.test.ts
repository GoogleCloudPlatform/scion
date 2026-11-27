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
  computeCommandRequestCanonicalHash,
  validateCommandRequestScript,
} from './command-requests.js';

interface HashVector {
  name: string;
  script: string;
  workdir: string;
  timeout_s: number;
  sha256: string;
}

const here = dirname(fileURLToPath(import.meta.url));
const vectorsDoc = JSON.parse(
  readFileSync(
    resolve(here, '../../../pkg/messages/testdata/command_request_hash_vectors.json'),
    'utf8'
  )
) as { version: number; vectors: HashVector[] };

describe('computeCommandRequestCanonicalHash', () => {
  it('matches all Go-generated shared test vectors', async () => {
    expect(vectorsDoc.vectors.length).toBeGreaterThan(0);
    for (const v of vectorsDoc.vectors) {
      const got = await computeCommandRequestCanonicalHash(v.script, v.workdir, v.timeout_s);
      expect(got, v.name).toBe(v.sha256);
    }
  });

  it('changes when script, workdir, or timeout changes', async () => {
    const base = await computeCommandRequestCanonicalHash('echo hi\n', '', 600);
    expect(await computeCommandRequestCanonicalHash('echo bye\n', '', 600)).not.toBe(base);
    expect(await computeCommandRequestCanonicalHash('echo hi\n', '/home/scion', 600)).not.toBe(
      base
    );
    expect(await computeCommandRequestCanonicalHash('echo hi\n', '', 300)).not.toBe(base);
  });
});

describe('validateCommandRequestScript', () => {
  it('accepts normal shell scripts including tabs and printable non-ASCII', () => {
    expect(validateCommandRequestScript('echo "hello\tworld"\n')).toBeNull();
    expect(validateCommandRequestScript("echo 'café 日本語'\n")).toBeNull();
  });

  it('rejects empty, oversized, control, bidi, and invisible whitespace characters', () => {
    expect(validateCommandRequestScript('')).not.toBeNull();
    expect(validateCommandRequestScript('a'.repeat(4097))).not.toBeNull();
    expect(validateCommandRequestScript('echo \x00')).not.toBeNull();
    expect(validateCommandRequestScript('echo \x1b[31m')).not.toBeNull();
    expect(validateCommandRequestScript('echo \u202ehi')).not.toBeNull();
    expect(validateCommandRequestScript('echo \u200b#')).not.toBeNull();
    expect(validateCommandRequestScript('echo \u00a0#')).not.toBeNull();
    expect(validateCommandRequestScript('echo \ufe0f#')).not.toBeNull();
    expect(validateCommandRequestScript('echo \u2800#')).not.toBeNull();
    // O5: Other_Default_Ignorable_Code_Point (Combining Grapheme Joiner U+034F).
    expect(validateCommandRequestScript('echo \u034f#')).not.toBeNull();
  });
});
