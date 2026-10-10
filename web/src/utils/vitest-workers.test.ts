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
 * The vitest worker count comes from the cgroup v2 CPU quota, and falls
 * back to the core count when there is no usable quota.
 */

import { describe, it, expect } from 'vitest';
import { existsSync, readFileSync } from 'node:fs';
import { availableParallelism } from 'node:os';

import { CGROUP_CPU_MAX, parseCpuMax, vitestMaxWorkers } from '../../vitest-workers.js';

describe('parseCpuMax', () => {
  it('divides the quota by the period, rounding up', () => {
    expect(parseCpuMax('200000 100000\n')).toBe(2);
    expect(parseCpuMax('150000 100000')).toBe(2);
    expect(parseCpuMax('100000 100000')).toBe(1);
  });

  it('returns at least 1 for a quota below one CPU', () => {
    expect(parseCpuMax('1000 100000')).toBe(1);
  });

  it('returns undefined when there is no quota', () => {
    expect(parseCpuMax('max 100000\n')).toBeUndefined();
  });

  it('returns undefined for contents it cannot parse', () => {
    for (const contents of [
      '',
      'garbage',
      '200000',
      '200000 abc',
      '-1 100000',
      '0 100000',
      '200000 0',
      '2 1 1',
    ]) {
      expect(parseCpuMax(contents), JSON.stringify(contents)).toBeUndefined();
    }
  });
});

describe('vitestMaxWorkers', () => {
  const cores = (): number => 32;

  it('uses the quota when cpu.max sets one', () => {
    expect(vitestMaxWorkers(() => '200000 100000\n', cores)).toBe(2);
  });

  it('reads the cgroup v2 cpu.max file', () => {
    const paths: string[] = [];
    vitestMaxWorkers((path) => {
      paths.push(path);
      return undefined;
    }, cores);
    expect(paths).toEqual(['/sys/fs/cgroup/cpu.max']);
  });

  it('falls back to the core count when the file is missing', () => {
    expect(vitestMaxWorkers(() => undefined, cores)).toBe(32);
  });

  it('falls back to the core count when there is no quota', () => {
    expect(vitestMaxWorkers(() => 'max 100000\n', cores)).toBe(32);
  });

  it('falls back to the core count when the file cannot be parsed', () => {
    expect(vitestMaxWorkers(() => 'garbage', cores)).toBe(32);
  });

  it('returns at least 1 when the core count is 0', () => {
    expect(
      vitestMaxWorkers(
        () => undefined,
        () => 0
      )
    ).toBe(1);
  });

  // Not an assertion about any particular host: prints what the
  // derivation computes on the machine running the tests, with and
  // without its cpu.max file.
  it('reports the value on this host', () => {
    const present = existsSync(CGROUP_CPU_MAX);
    const contents = present ? readFileSync(CGROUP_CPU_MAX, 'utf8').trim() : '(absent)';
    const withFile = vitestMaxWorkers();
    const withoutFile = vitestMaxWorkers(() => undefined);
    console.info(
      `cpu.max=${contents} maxWorkers=${withFile}; cpu.max absent: maxWorkers=${withoutFile} (availableParallelism=${availableParallelism()})`
    );
    expect(withFile).toBeGreaterThanOrEqual(1);
    expect(withoutFile).toBe(Math.max(1, availableParallelism()));
  });
});
