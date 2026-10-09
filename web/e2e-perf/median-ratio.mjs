#!/usr/bin/env node
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
 * median-ratio.mjs -- wall-clock budget check for the perf/bench harness.
 *
 * Compares the median of repeated timed trials (from an apibench report
 * and/or a large-project-bench report) with a stored baseline median per
 * metric, and exits 1 when any metric's median is more than --ratio times
 * its baseline, or has fewer than --min-trials timed samples.
 *
 * Wall-clock numbers depend on the host. Run this only on a quiet,
 * dedicated runner that was booked for the run, with the baseline taken on
 * that same runner. It is never a required check on shared CI hosts. The
 * procedure (seed, warm-up, trial counts) is in perf/bench/README.md,
 * "Wall-clock budgets".
 *
 * Usage (from web/):
 *   node e2e-perf/median-ratio.mjs --baseline ../perf/bench/wallclock-baseline.json \
 *     --api /tmp/scion-bench/api-100.json --browser /tmp/scion-bench/browser-100.json
 *   node e2e-perf/median-ratio.mjs --write-baseline ../perf/bench/wallclock-baseline.json \
 *     --api ... --browser ... --commit <sha> --runner "<runner description>"
 *
 * Options: --ratio (default 1.25), --min-trials (default 10).
 */

import { readFileSync, writeFileSync } from 'node:fs';
import { parseArgs } from 'node:util';

import {
  MEDIAN_RATIO_DEFAULTS,
  buildWallClockBaseline,
  compareMedianRatio,
  wallClockSamples,
} from './lib.mjs';

const { values: args } = parseArgs({
  options: {
    baseline: { type: 'string' },
    'write-baseline': { type: 'string' },
    api: { type: 'string' },
    browser: { type: 'string' },
    ratio: { type: 'string' },
    'min-trials': { type: 'string' },
    commit: { type: 'string' },
    runner: { type: 'string' },
  },
});

const readJSON = (p) => JSON.parse(readFileSync(p, 'utf8'));
const num = (v) => (v == null ? undefined : Number(v));

if (!args.api && !args.browser) {
  console.error('median-ratio: give --api and/or --browser report files');
  process.exit(2);
}
if (!!args.baseline === !!args['write-baseline']) {
  console.error('median-ratio: give exactly one of --baseline or --write-baseline');
  process.exit(2);
}

const samples = wallClockSamples({
  api: args.api ? readJSON(args.api) : null,
  browser: args.browser ? readJSON(args.browser) : null,
});
const opts = { ratio: num(args.ratio), minTrials: num(args['min-trials']) };

if (args['write-baseline']) {
  const body = buildWallClockBaseline(
    samples,
    {
      comment:
        'Wall-clock baseline medians for web/e2e-perf/median-ratio.mjs. ' +
        'Valid only on the runner described here; see perf/bench/README.md.',
      commit: args.commit || '',
      runner: args.runner || '',
      date: new Date().toISOString().slice(0, 10),
    },
    {
      ratio: opts.ratio ?? MEDIAN_RATIO_DEFAULTS.ratio,
      minTrials: opts.minTrials ?? MEDIAN_RATIO_DEFAULTS.minTrials,
    }
  );
  writeFileSync(args['write-baseline'], JSON.stringify(body, null, 2) + '\n');
  console.log(`wrote ${args['write-baseline']}: ${Object.keys(body.metrics).length} metrics`);
  process.exit(0);
}

const baseline = readJSON(args.baseline);
const result = compareMedianRatio(samples, baseline, opts);
console.log(
  `median-ratio: limit ${result.ratio}x baseline median, at least ${result.minTrials} timed trials; ` +
    `baseline ${baseline.commit || '?'} on ${baseline.runner || '?'}`
);
for (const r of result.rows) {
  const ratio = r.ratio == null ? '' : ` ratio=${r.ratio.toFixed(3)}`;
  const base = r.baselineMedianMs == null ? '' : ` baseline=${r.baselineMedianMs}ms`;
  console.log(
    `  ${r.status.padEnd(12)} ${r.metric}: median=${r.medianMs ?? '-'}ms${base}${ratio} trials=${r.trials}`
  );
}
process.exit(result.ok ? 0 : 1);
