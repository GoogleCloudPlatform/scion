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
 * Pure helpers of the async create journey, kept apart so the
 * harness-selftest suite can check them without a hub or a browser.
 */

/** Parses a positive millisecond timeout from the environment; throws otherwise. */
export function parseTimeoutMs(raw: string | undefined, fallback: number, name: string): number {
  if (raw === undefined || raw === '') return fallback;
  const value = Number(raw);
  if (!Number.isFinite(value) || value <= 0) {
    throw new Error(`${name} must be a positive number of milliseconds, got "${raw}"`);
  }
  return value;
}

/** What a direct GET of a resource says about its existence. */
export type ReadState = 'not_found' | 'soft_deleted' | 'present' | 'unknown';

/**
 * Classifies a direct GET of one resource (agent, project, template). Only
 * a 404, or a 200 whose record carries a deletedAt (the hub keeps
 * soft-deleted rows readable by id), counts as gone. Any other 200 is
 * present, including a resource the hub is still deleting; any other
 * status is unknown, never gone. wrapperKey names the key the record may
 * be wrapped in (e.g. "agent").
 */
export function classifyRead(status: number, body: unknown, wrapperKey?: string): ReadState {
  if (status === 404) return 'not_found';
  if (status !== 200) return 'unknown';
  const outer = (body ?? {}) as Record<string, unknown>;
  const inner = wrapperKey ? (outer[wrapperKey] as Record<string, unknown> | undefined) : undefined;
  const deletedAt = (inner ?? outer).deletedAt;
  if (typeof deletedAt === 'string' && deletedAt !== '' && !deletedAt.startsWith('0001-01-01')) {
    return 'soft_deleted';
  }
  return 'present';
}

/** Whether a classified read means the resource is gone. */
export function isGone(state: ReadState): boolean {
  return state === 'not_found' || state === 'soft_deleted';
}

/** Token prefixes that must never reach the evidence. */
const SECRET_PREFIXES = [
  'tiwc_',
  'scion_dev_',
  'scion_',
  'ghp_',
  'gho_',
  'github_pat_',
  'sk-',
  'ya29.',
  'eyJ',
];

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * A URL path for the evidence: each segment that could carry a secret is
 * replaced by "<redacted>": a known token prefix, or a long opaque string
 * (40+ token characters) that is not a UUID. No route the journey uses
 * puts a secret in the path (codes and tokens travel in bodies or
 * headers); this is a guard against one appearing.
 */
export function redactPath(pathname: string): string {
  return pathname
    .split('/')
    .map((segment) => {
      let decoded = segment;
      try {
        decoded = decodeURIComponent(segment);
      } catch {
        // Keep the raw segment.
      }
      if (UUID.test(decoded)) return segment;
      const secretLike =
        SECRET_PREFIXES.some((prefix) => decoded.startsWith(prefix)) ||
        (decoded.length >= 40 && /^[A-Za-z0-9_\-.~+=]+$/.test(decoded));
      return secretLike ? '<redacted>' : segment;
    })
    .join('/');
}

/** One step as first seen, with its time. */
export interface StepSeen {
  t: number;
  step: string;
}

/**
 * The ordered steps of a series of records: each non-empty step with the
 * time it first appeared, consecutive repeats dropped. A step that comes
 * back after another one is listed again, so a backwards move shows.
 */
export function stepSequence(
  records: ReadonlyArray<{ t: number; step?: string | null }>
): StepSeen[] {
  const out: StepSeen[] = [];
  for (const r of records) {
    const step = r.step ?? '';
    if (step && out.at(-1)?.step !== step) out.push({ t: r.t, step });
  }
  return out;
}

/**
 * Problems with the launch steps seen, against the canonical step list:
 * an SSE step outside the list, an SSE step that goes back in the list, a
 * page launch-step text that is not the label of a step in the SSE
 * sequence, or page labels out of the SSE order. Empty when all hold; no
 * page label at all is not a problem (a fast launch may show none).
 * label(step) is the text the page shows for a step.
 */
export function launchStepProblems(
  sseSteps: readonly string[],
  uiTexts: readonly string[],
  canonical: readonly string[],
  label: (step: string) => string
): string[] {
  const problems: string[] = [];
  let lastRank = -1;
  for (const step of sseSteps) {
    const rank = canonical.indexOf(step);
    if (rank < 0) {
      problems.push(`SSE step "${step}" is not in the canonical list`);
      continue;
    }
    if (rank <= lastRank) problems.push(`SSE step "${step}" goes back in the canonical order`);
    lastRank = Math.max(lastRank, rank);
  }
  // Each page label must match an SSE step at or after the previous match.
  let from = 0;
  for (const text of uiTexts) {
    const at = sseSteps.findIndex((step, i) => i >= from && label(step) === text);
    if (at >= 0) {
      from = at;
    } else if (sseSteps.some((step) => label(step) === text)) {
      problems.push(`page label "${text}" is out of the SSE step order`);
    } else {
      problems.push(`page label "${text}" matches no SSE step`);
    }
  }
  return problems;
}
