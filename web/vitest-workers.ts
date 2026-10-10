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
 * Vitest worker count from the container's CPU quota.
 *
 * A container can report every host core (os.cpus()) while a cgroup v2
 * quota allows it far fewer, so a worker count taken from the core count
 * oversubscribes the CPUs the container may actually use. This module
 * reads the quota from cpu.max and uses it as maxWorkers.
 *
 * An explicit override still wins: vitest applies the --maxWorkers CLI
 * flag and the VITEST_MAX_WORKERS environment variable over the config
 * value.
 */

import { readFileSync } from 'node:fs';
import { availableParallelism, cpus } from 'node:os';

/** The cgroup v2 CPU bandwidth file for the current cgroup. */
export const CGROUP_CPU_MAX = '/sys/fs/cgroup/cpu.max';

/**
 * Parses the contents of a cgroup v2 cpu.max file ("<quota> <period>",
 * both in microseconds) into a whole CPU count: quota / period, rounded
 * up, at least 1. Returns undefined when there is no quota ("max") or the
 * contents cannot be parsed.
 */
export function parseCpuMax(contents: string): number | undefined {
  const match = /^\s*(\d+)\s+(\d+)\s*$/.exec(contents);
  if (!match) return undefined;
  const quota = Number(match[1]);
  const period = Number(match[2]);
  if (!(quota > 0) || !(period > 0)) return undefined;
  return Math.max(1, Math.ceil(quota / period));
}

/** The core count Node reports for this process. */
function coreCount(): number {
  return typeof availableParallelism === 'function' ? availableParallelism() : cpus().length;
}

/** Reads a file, or returns undefined when it cannot be read. */
function readOptional(path: string): string | undefined {
  try {
    return readFileSync(path, 'utf8');
  } catch {
    return undefined;
  }
}

/**
 * The maxWorkers value for vitest: the cgroup CPU quota when one is set,
 * otherwise the core count. The file reader and core count are injectable
 * for tests.
 */
export function vitestMaxWorkers(
  read: (path: string) => string | undefined = readOptional,
  cores: () => number = coreCount
): number {
  const contents = read(CGROUP_CPU_MAX);
  const quota = contents === undefined ? undefined : parseCpuMax(contents);
  return quota ?? Math.max(1, cores());
}
