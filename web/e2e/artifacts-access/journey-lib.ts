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
 * Helpers of the artifact access journey.
 *
 * Local copies of helpers from the async create journey
 * (web/e2e/async-create/journey-lib.ts and async-create.pw.ts, not on main
 * yet): classifyRead, isGone, redactPath, annotationsOf, firstLine and the
 * browser request recorder. To be consolidated into web/e2e/harness once
 * both suites are on main.
 */

import type { BrowserContext, Request } from '@playwright/test';

/** What a direct GET of a resource says about its existence. */
export type ReadState = 'not_found' | 'soft_deleted' | 'present' | 'unknown';

/**
 * Classifies a direct GET of one resource. Only a 404, or a 200 whose
 * record carries a deletedAt (the hub keeps some soft-deleted rows
 * readable by id), counts as gone. Any other 200 is present; any other
 * status is unknown, never gone. wrapperKey names the key the record may
 * be wrapped in (e.g. "artifact").
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
 * puts a secret in the path; this is a guard against one appearing.
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

/** Playwright annotations as plain records for the evidence. */
export function annotationsOf(
  annotations: ReadonlyArray<{ type: string; description?: string }>
): Array<{ type: string; description: string | null }> {
  return annotations.map((a) => ({ type: a.type, description: a.description ?? null }));
}

/** First line of a thrown value's message. */
export function firstLine(err: unknown): string {
  const msg = err instanceof Error ? err.message : String(err);
  return msg.split('\n')[0];
}

/** One browser request: method, path (no query), status, time. No headers or bodies. */
export interface RequestRecord {
  t: number;
  method: string;
  path: string;
  status?: number;
  /** No response: the request failed (failure holds the browser's reason). */
  failed?: boolean;
  /** Answered, then aborted (e.g. the SSE stream when its page closes). */
  abortedAfterResponse?: boolean;
  failure?: string;
}

/**
 * Records every http(s) request of every page of a context, by path only
 * (no query, headers or bodies), into the returned array.
 */
export function recordRequests(context: BrowserContext): RequestRecord[] {
  const out: RequestRecord[] = [];
  const records = new Map<Request, RequestRecord>();
  context.on('request', (req) => {
    // http(s) only: a data: or blob: URL would put its content in the path.
    if (!/^https?:/.test(req.url())) return;
    const rec: RequestRecord = {
      t: Date.now(),
      method: req.method(),
      path: redactPath(new URL(req.url()).pathname),
    };
    records.set(req, rec);
    out.push(rec);
  });
  context.on('response', (res) => {
    const rec = records.get(res.request());
    if (rec) rec.status = res.status();
  });
  context.on('requestfailed', (req) => {
    const rec = records.get(req);
    if (!rec) return;
    if (rec.status !== undefined) rec.abortedAfterResponse = true;
    else rec.failed = true;
    const reason = req.failure()?.errorText;
    if (reason) rec.failure = reason;
  });
  return out;
}

/**
 * Offsets of secret-shaped strings in text: a known token prefix at a word
 * start followed by 8+ token characters, or a scion_<kind>_ token. Used to
 * check the serialised evidence before it is written.
 */
export function secretLikeStrings(text: string): number[] {
  const pattern =
    /(?<![A-Za-z0-9])(?:tiwc_|scion_dev_|ghp_|gho_|github_pat_|sk-|ya29\.|eyJ)[A-Za-z0-9_\-.]{8,}|(?<![A-Za-z0-9])scion_[a-z]+_[A-Za-z0-9]{16,}/g;
  const out: number[] = [];
  for (const m of text.matchAll(pattern)) out.push(m.index ?? 0);
  return out;
}
