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

// Run with `npm run test:e2e-perf` (from web/).

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

import { buildFixture, fieldPaths, schemaOf } from './fixture.mjs';

const schema = JSON.parse(readFileSync(new URL('./fixture-schema.json', import.meta.url), 'utf8'));

test("the generated fixture has exactly the hub's field names (fixture-schema.json)", () => {
  const got = schemaOf(buildFixture());
  assert.equal(got.agents, schema.agents);
  // Per endpoint, so a failure names the request and the differing fields.
  assert.deepEqual(Object.keys(got.endpoints).sort(), Object.keys(schema.endpoints).sort());
  for (const [path, want] of Object.entries(schema.endpoints)) {
    const g = got.endpoints[path];
    assert.equal(g.status, want.status, `${path}: status`);
    assert.deepEqual(
      {
        missing: want.fields.filter((f) => !g.fields.includes(f)),
        extra: g.fields.filter((f) => !want.fields.includes(f)),
      },
      { missing: [], extra: [] },
      `${path}: fields differ from the hub's (update fixture.mjs; see perf-tracing.md)`
    );
  }
});

test('the generated fixture is deterministic', () => {
  assert.deepEqual(buildFixture(), buildFixture());
});

test('fieldPaths joins keys, collapses arrays and sorts byte-wise', () => {
  assert.deepEqual(fieldPaths({ b: [{ x: 1 }, { y: [[1]] }], a: null, Z: { k: 'v' } }), [
    'Z',
    'Z.k',
    'a',
    'b',
    'b[].x',
    'b[].y',
  ]);
});
