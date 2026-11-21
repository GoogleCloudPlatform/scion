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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  DRAFT_KEEPALIVE_LIMIT_BYTES,
  DraftApiError,
  DraftConflictError,
  MAX_DRAFT_BODY_BYTES,
  parseRetryAfterMs,
  safeReviewDownloadName,
  type PutReviewDraftRequest,
  type PutReviewDraftResponse,
  type ReviewDraftResponse,
} from './artifacts.js';
import {
  REVIEW_DRAFT_AUTOSAVE_MS,
  REVIEW_DRAFT_RETRY_MAX_MS,
  REVIEW_DRAFT_RETRY_MIN_MS,
  ReviewDraftController,
  draftStatusText,
  formatDraftTime,
  type DraftSnapshot,
} from './review-draft.js';

describe('safeReviewDownloadName (C6)', () => {
  it('uses only the final path element and strips .md', () => {
    expect(safeReviewDownloadName('plan.md', 7)).toBe('plan-review-of-v7.md');
    expect(safeReviewDownloadName('docs/specs/plan.md', 3)).toBe('plan-review-of-v3.md');
    expect(safeReviewDownloadName('docs\\specs\\plan.MD', 4)).toBe('plan-review-of-v4.md');
    expect(safeReviewDownloadName('notes.txt', 2)).toBe('notes.txt-review-of-v2.md');
  });

  it('falls back to review-of-v<base>.md for unsafe or empty entry paths', () => {
    for (const bad of [
      '',
      '.',
      '..',
      '.md',
      '/.md',
      'dir/',
      'dir\\',
      'a/..',
      'a/.',
      'C:',
      'C:plan.md',
      'C:\\',
      '\\\\server\\share\\',
      'bad\x00name.md',
      'bad\nname.md',
      'bad\x7fname.md',
    ]) {
      expect(safeReviewDownloadName(bad, 5), `entry=${JSON.stringify(bad)}`).toBe(
        'review-of-v5.md'
      );
    }
  });
});

describe('parseRetryAfterMs', () => {
  it('parses integer seconds and HTTP-dates', () => {
    expect(parseRetryAfterMs('7')).toBe(7000);
    expect(parseRetryAfterMs('0')).toBe(0);
    expect(
      parseRetryAfterMs('Sat, 21 Nov 2026 18:30:10 GMT', Date.parse('2026-11-21T18:30:00Z'))
    ).toBe(10_000);
    expect(parseRetryAfterMs(null)).toBeUndefined();
    expect(parseRetryAfterMs('not-a-date')).toBeUndefined();
  });
});

describe('ReviewDraftController', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-11-21T18:00:00Z'));
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  function makeHarness(opts?: {
    initialBody?: string;
    initialNote?: string;
    initialRev?: number;
    initialSavedAt?: string | null;
    put?: (
      id: string,
      req: PutReviewDraftRequest,
      o?: { keepalive?: boolean; signal?: AbortSignal }
    ) => Promise<PutReviewDraftResponse>;
    get?: (id: string) => Promise<ReviewDraftResponse>;
    del?: (id: string, o?: { rev?: number }) => Promise<void>;
  }) {
    let body = opts?.initialBody ?? 'Original text.\n';
    let note = opts?.initialNote ?? '';
    const snapshots: DraftSnapshot[] = [];
    const puts: Array<{ req: PutReviewDraftRequest; keepalive?: boolean | undefined }> = [];
    let revCounter = opts?.initialRev ?? 0;

    const put =
      opts?.put ??
      ((_id, req, o) => {
        puts.push({ req, keepalive: o?.keepalive });
        revCounter = req.rev + 1;
        return Promise.resolve({
          rev: revCounter,
          savedAt: new Date(Date.now()).toISOString(),
          currentSeq: 1,
        });
      });
    const get =
      opts?.get ??
      (() =>
        Promise.resolve({
          draft: null,
          currentSeq: 1,
        }));
    const del = opts?.del ?? (() => Promise.resolve());

    const doc = document;
    const win = window;
    const controller = new ReviewDraftController({
      artifactId: 'art-1',
      base: 1,
      entry: 'plan.md',
      initialBody: body,
      initialNote: note,
      ...(opts?.initialRev !== undefined ? { initialRev: opts.initialRev } : {}),
      ...(opts?.initialSavedAt !== undefined ? { initialSavedAt: opts.initialSavedAt } : {}),
      readState: () => ({ body, note }),
      onChange: (snap) => snapshots.push(snap),
      put,
      get,
      del,
      doc,
      win,
    });

    return {
      controller,
      snapshots,
      puts,
      setBody(next: string) {
        body = next;
        controller.notifyChange();
      },
      setNote(next: string) {
        note = next;
        controller.notifyChange();
      },
    };
  }

  it('writes nothing until the first change, then autosaves at ~30 s without resetting on typing', async () => {
    const h = makeHarness();
    expect(draftStatusText(h.controller.getSnapshot())).toBe('');

    // No changes -> no PUT even after 60 s.
    await vi.advanceTimersByTimeAsync(60_000);
    expect(h.puts).toHaveLength(0);

    // First edit starts the 30 s timer.
    h.setBody('Original text.{++ first++}\n');
    expect(draftStatusText(h.controller.getSnapshot())).toBe('Unsaved changes');

    // Typing at 10 s and 25 s does NOT reset the timer.
    await vi.advanceTimersByTimeAsync(10_000);
    h.setBody('Original text.{++ first second++}\n');
    await vi.advanceTimersByTimeAsync(15_000);
    h.setBody('Original text.{++ first second third++}\n');
    expect(h.puts).toHaveLength(0);

    // At t = 30 s from the first edit, it PUTs the latest buffer once.
    await vi.advanceTimersByTimeAsync(5_000);
    expect(h.puts).toHaveLength(1);
    expect(h.puts[0]!.req.rev).toBe(0);
    expect(h.puts[0]!.req.body).toBe('Original text.{++ first second third++}\n');
    const snap = h.controller.getSnapshot();
    expect(snap.status).toBe('saved');
    expect(snap.rev).toBe(1);
    expect(draftStatusText(snap)).toBe(`Draft saved ${formatDraftTime(snap.savedAt!)}`);

    h.controller.dispose();
  });

  it('keeps only one PUT in flight at a time and coalesces edits made during flight', async () => {
    let resolveFirst!: (res: PutReviewDraftResponse) => void;
    const reqs: PutReviewDraftRequest[] = [];
    const h = makeHarness({
      put: (_id, req) => {
        reqs.push(req);
        if (reqs.length === 1) {
          return new Promise<PutReviewDraftResponse>((r) => {
            resolveFirst = r;
          });
        }
        return Promise.resolve({
          rev: req.rev + 1,
          savedAt: new Date(Date.now()).toISOString(),
          currentSeq: 1,
        });
      },
    });

    h.setBody('v1');
    const savePromise = h.controller.saveNow();
    expect(reqs).toHaveLength(1);
    expect(h.controller.getSnapshot().status).toBe('saving');

    // Multiple edits while PUT #1 is in flight do not start a second PUT.
    h.setBody('v2');
    h.setBody('v3');
    expect(reqs).toHaveLength(1);

    resolveFirst({ rev: 1, savedAt: new Date(Date.now()).toISOString(), currentSeq: 1 });
    await savePromise;

    // Coalesced edits schedule one fresh 30 s autosave timer.
    expect(h.controller.getSnapshot().status).toBe('unsaved');
    expect(reqs).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(REVIEW_DRAFT_AUTOSAVE_MS);
    expect(reqs).toHaveLength(2);
    expect(reqs[1]!.rev).toBe(1);
    expect(reqs[1]!.body).toBe('v3');
    expect(h.controller.getSnapshot().status).toBe('saved');

    h.controller.dispose();
  });

  it('saves with keepalive on visibilitychange -> hidden only when under 60 KiB', async () => {
    const h = makeHarness();
    h.setBody('small edit');

    const desc = Object.getOwnPropertyDescriptor(document, 'visibilityState');
    try {
      Object.defineProperty(document, 'visibilityState', {
        configurable: true,
        get: () => 'hidden',
      });
      document.dispatchEvent(new Event('visibilitychange'));
      await vi.advanceTimersByTimeAsync(0);
      expect(h.puts).toHaveLength(1);
      expect(h.puts[0]!.keepalive).toBe(true);

      // Oversize (> 60 KiB) skips keepalive fetch on visibilitychange.
      h.setBody('x'.repeat(DRAFT_KEEPALIVE_LIMIT_BYTES + 1024));
      document.dispatchEvent(new Event('visibilitychange'));
      await vi.advanceTimersByTimeAsync(0);
      expect(h.puts).toHaveLength(1);
    } finally {
      if (desc) Object.defineProperty(document, 'visibilityState', desc);
      else delete (document as unknown as Record<string, unknown>).visibilityState;
      h.controller.dispose();
    }
  });

  it('arms beforeunload while dirty and disarms it once acknowledged', async () => {
    const h = makeHarness();

    const fireBeforeUnload = (): boolean => {
      const ev = new Event('beforeunload', { cancelable: true }) as BeforeUnloadEvent;
      window.dispatchEvent(ev);
      return ev.defaultPrevented;
    };

    expect(fireBeforeUnload()).toBe(false);
    h.setBody('unsaved edit');
    expect(fireBeforeUnload()).toBe(true);

    await h.controller.saveNow();
    expect(fireBeforeUnload()).toBe(false);

    h.controller.dispose();
  });

  it('honours Retry-After on 429 rate_limited and backs off 2 s -> 60 s on 5xx', async () => {
    let calls = 0;
    const h = makeHarness({
      put: (_id, req) => {
        calls++;
        if (calls === 1) {
          return Promise.reject(
            new DraftApiError('rate limited', 429, 'rate_limited', { retryAfterMs: 5000 })
          );
        }
        if (calls === 2 || calls === 3) {
          return Promise.reject(new DraftApiError('server error', 503, 'internal'));
        }
        return Promise.resolve({
          rev: req.rev + 1,
          savedAt: new Date(Date.now()).toISOString(),
          currentSeq: 1,
        });
      },
    });

    h.setBody('edited');
    await h.controller.saveNow();
    expect(draftStatusText(h.controller.getSnapshot())).toBe('Not saved: retrying');
    expect(calls).toBe(1);

    // Honours Retry-After: 5000 ms.
    await vi.advanceTimersByTimeAsync(4999);
    expect(calls).toBe(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(calls).toBe(2);

    // First 5xx retries after 2 s.
    await vi.advanceTimersByTimeAsync(REVIEW_DRAFT_RETRY_MIN_MS - 1);
    expect(calls).toBe(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(calls).toBe(3);

    // Second 5xx retries after 4 s (exponential backoff capped at 60 s).
    expect(REVIEW_DRAFT_RETRY_MAX_MS).toBe(60_000);
    await vi.advanceTimersByTimeAsync(REVIEW_DRAFT_RETRY_MIN_MS * 2 - 1);
    expect(calls).toBe(3);
    await vi.advanceTimersByTimeAsync(1);
    expect(calls).toBe(4);
    expect(h.controller.getSnapshot().status).toBe('saved');

    h.controller.dispose();
  });

  it('pauses on 409 conflict and supports Keep and Load resolutions', async () => {
    let mode: 'conflict' | 'ok' = 'conflict';
    const puts: PutReviewDraftRequest[] = [];
    const h = makeHarness({
      put: (_id, req) => {
        puts.push(req);
        if (mode === 'conflict') {
          return Promise.reject(new DraftConflictError('conflict', 5, '2026-11-21T17:58:00Z', 1));
        }
        return Promise.resolve({
          rev: req.rev + 1,
          savedAt: new Date(Date.now()).toISOString(),
          currentSeq: 1,
        });
      },
      get: () =>
        Promise.resolve({
          draft: {
            rev: 5,
            base: 1,
            entry: 'plan.md',
            body: 'Remote draft body',
            note: 'Remote note',
            carried: null,
            savedAt: '2026-11-21T17:58:00Z',
            createdAt: '2026-11-21T17:50:00Z',
          },
          currentSeq: 1,
        }),
    });

    h.setBody('Local body');
    await h.controller.saveNow();
    expect(h.controller.getSnapshot().status).toBe('conflict');
    expect(h.controller.getSnapshot().conflict).toEqual({
      rev: 5,
      savedAt: '2026-11-21T17:58:00Z',
      base: 1,
    });

    // Keep re-PUTs with the server's rev (5).
    mode = 'ok';
    await h.controller.resolveConflictKeep();
    expect(puts[1]!.rev).toBe(5);
    expect(h.controller.getSnapshot().rev).toBe(6);
    expect(h.controller.getSnapshot().status).toBe('saved');

    // Load fetches and adopts the server's draft.
    const loaded = await h.controller.resolveConflictLoad();
    expect(loaded?.body).toBe('Remote draft body');
    expect(h.controller.currentRev).toBe(5);

    h.controller.dispose();
  });

  it('pauses on 413 draft_too_large until text shrinks under 1 MiB, and on 429 too_many_drafts / 403', async () => {
    let errToThrow: Error | null = new DraftApiError('too large', 413, 'draft_too_large', {
      limit: MAX_DRAFT_BODY_BYTES,
    });
    const h = makeHarness({
      put: (_id, req) => {
        if (errToThrow) return Promise.reject(errToThrow);
        return Promise.resolve({
          rev: req.rev + 1,
          savedAt: new Date(Date.now()).toISOString(),
          currentSeq: 1,
        });
      },
    });

    h.setBody('x'.repeat(MAX_DRAFT_BODY_BYTES + 10));
    await h.controller.saveNow();
    expect(draftStatusText(h.controller.getSnapshot())).toContain('over 1 MB');

    // Shrinking below cap re-enables autosave.
    errToThrow = null;
    h.setBody('small again');
    await vi.advanceTimersByTimeAsync(REVIEW_DRAFT_AUTOSAVE_MS);
    expect(h.controller.getSnapshot().status).toBe('saved');

    // 429 too_many_drafts
    errToThrow = new DraftApiError('too many', 429, 'too_many_drafts');
    h.setBody('edit 2');
    await h.controller.saveNow();
    expect(draftStatusText(h.controller.getSnapshot())).toContain('200 saved draft reviews');

    // 403 access lost
    errToThrow = new DraftApiError('forbidden', 403, 'forbidden');
    h.setBody('edit 3');
    await h.controller.saveNow();
    expect(draftStatusText(h.controller.getSnapshot())).toContain('no longer have access');

    h.controller.dispose();
  });

  it('quiesce awaits an in-flight PUT without aborting it and blocks new saves until resumed (R1/S2)', async () => {
    let resolveFirst!: (res: PutReviewDraftResponse) => void;
    const puts: PutReviewDraftRequest[] = [];
    const h = makeHarness({
      put: (_id, req) => {
        puts.push(req);
        if (puts.length === 1) {
          return new Promise<PutReviewDraftResponse>((r) => {
            resolveFirst = r;
          });
        }
        return Promise.resolve({
          rev: req.rev + 1,
          savedAt: new Date(Date.now()).toISOString(),
          currentSeq: 1,
        });
      },
    });

    h.setBody('first edit');
    const save1 = h.controller.saveNow();
    expect(puts).toHaveLength(1);

    // Calling saveNow() while PUT #1 is in flight awaits PUT #1 and returns true (S2).
    const save2 = h.controller.saveNow();
    resolveFirst({ rev: 1, savedAt: new Date(Date.now()).toISOString(), currentSeq: 1 });
    expect(await save1).toBe(true);
    expect(await save2).toBe(true);
    expect(h.controller.currentRev).toBe(1);

    // Start PUT #2 (held), then quiesce(): quiesce settles PUT #2 and blocks new timers.
    let resolveSecond!: (res: PutReviewDraftResponse) => void;
    const h2 = makeHarness({
      put: (_id, req) =>
        new Promise<PutReviewDraftResponse>((r) => {
          resolveSecond = r;
          void req;
        }),
    });
    h2.setBody('pending rev-0 edit');
    const p2 = h2.controller.saveNow();
    let quiesced = false;
    const quiescePromise = h2.controller.quiesce().then(() => {
      quiesced = true;
    });
    // quiesce() must remain pending until the in-flight PUT resolves.
    await Promise.resolve();
    await Promise.resolve();
    expect(quiesced).toBe(false);

    resolveSecond({ rev: 1, savedAt: new Date(Date.now()).toISOString(), currentSeq: 1 });
    await quiescePromise;
    await p2;
    expect(quiesced).toBe(true);
    expect(h2.controller.currentRev).toBe(1);

    // While quiesced, edits do not schedule autosave; resumeFromQuiesce re-arms autosave.
    h2.setBody('edit after refused submit');
    await vi.advanceTimersByTimeAsync(REVIEW_DRAFT_AUTOSAVE_MS * 2);
    expect(h2.controller.getSnapshot().status).toBe('saved');

    h2.controller.resumeFromQuiesce();
    expect(h2.controller.getSnapshot().status).toBe('unsaved');

    h.controller.dispose();
    h2.controller.dispose();
  });

  it('never writes draft data to localStorage, sessionStorage, IndexedDB, or Cache API (C8)', async () => {
    const lsSpy = vi.spyOn(Storage.prototype, 'setItem');
    const idbOpen = vi.fn();
    const cachesOpen = vi.fn();
    const origIdb = Object.getOwnPropertyDescriptor(globalThis, 'indexedDB');
    const origCaches = Object.getOwnPropertyDescriptor(globalThis, 'caches');
    Object.defineProperty(globalThis, 'indexedDB', {
      configurable: true,
      value: { open: idbOpen },
    });
    Object.defineProperty(globalThis, 'caches', {
      configurable: true,
      value: { open: cachesOpen },
    });

    const h = makeHarness();
    try {
      h.setBody('Secret unpublished review comment.{>>private<<}\n');
      h.setNote('Private note');
      await h.controller.saveNow();
      await h.controller.deleteAfterSubmit();

      expect(lsSpy).not.toHaveBeenCalled();
      expect(idbOpen).not.toHaveBeenCalled();
      expect(cachesOpen).not.toHaveBeenCalled();
      expect(window.localStorage.length).toBe(0);
      expect(window.sessionStorage.length).toBe(0);
    } finally {
      h.controller.dispose();
      if (origIdb) Object.defineProperty(globalThis, 'indexedDB', origIdb);
      else delete (globalThis as Record<string, unknown>).indexedDB;
      if (origCaches) Object.defineProperty(globalThis, 'caches', origCaches);
      else delete (globalThis as Record<string, unknown>).caches;
    }
  });
});
