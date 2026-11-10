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
import type { Mock } from 'vitest';

import type { ReviewBeat, ReviewBeatResponse } from './artifacts.js';
import { REVIEW_BEAT_INTERVAL_MS, ReviewHeartbeat } from './review-heartbeat.js';

type PutFn = (id: string, sid: string, beat: ReviewBeat) => Promise<ReviewBeatResponse>;

interface Harness {
  hb: ReviewHeartbeat;
  beats: ReviewBeat[];
  ends: { keepalive?: boolean }[];
  seqs: number[];
  state: { base: number; text: string };
  doc: EventTarget & { visibilityState: string };
  win: EventTarget;
  put: Mock<PutFn>;
}

function harness(opts: { currentSeq?: number; fail?: boolean } = {}): Harness {
  const beats: ReviewBeat[] = [];
  const ends: { keepalive?: boolean }[] = [];
  const seqs: number[] = [];
  const state = { base: 7, text: 'start' };
  const doc = Object.assign(new EventTarget(), { visibilityState: 'visible' });
  const win = new EventTarget();
  const put = vi.fn<PutFn>((_id, _sid, beat) => {
    beats.push(beat);
    return opts.fail
      ? Promise.reject(new Error('offline'))
      : Promise.resolve({ currentSeq: opts.currentSeq ?? 7 });
  });
  const hb = new ReviewHeartbeat({
    artifactId: 'a-1',
    state: () => ({ ...state }),
    onCurrentSeq: (n) => seqs.push(n),
    put,
    end: (_id, _sid, o) => {
      ends.push(o);
      return Promise.resolve();
    },
    newId: () => 'S'.repeat(22),
    doc: doc as unknown as Document,
    win: win as unknown as Window,
  });
  return { hb, beats, ends, seqs, state, doc, win, put };
}

/** Lets the beat's promise settle. */
const settle = async (): Promise<void> => {
  await Promise.resolve();
  await Promise.resolve();
};

describe('ReviewHeartbeat', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('beats on start and then once a minute', async () => {
    const h = harness();
    h.hb.start();
    await settle();
    expect(h.beats).toEqual([{ base: 7, edited: false, dirty: false }]);
    await vi.advanceTimersByTimeAsync(REVIEW_BEAT_INTERVAL_MS - 1);
    expect(h.beats).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(h.beats).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(REVIEW_BEAT_INTERVAL_MS * 3);
    expect(h.beats).toHaveLength(5);
    expect(h.hb.sessionId).toBe('S'.repeat(22));
    h.hb.stop();
  });

  it('reports edited since the last beat and dirty against the start text', async () => {
    const h = harness();
    h.hb.start();
    await settle();
    h.state.text = 'start {++mark++}';
    await vi.advanceTimersByTimeAsync(REVIEW_BEAT_INTERVAL_MS);
    expect(h.beats[1]).toEqual({ base: 7, edited: true, dirty: true });
    await vi.advanceTimersByTimeAsync(REVIEW_BEAT_INTERVAL_MS);
    expect(h.beats[2]).toEqual({ base: 7, edited: false, dirty: true });
    // Marks typed and removed again count as clean.
    h.state.text = 'start';
    await vi.advanceTimersByTimeAsync(REVIEW_BEAT_INTERVAL_MS);
    expect(h.beats[3]).toEqual({ base: 7, edited: true, dirty: false });
    h.hb.stop();
  });

  it('beats when the tab becomes visible, not when it is hidden', async () => {
    const h = harness();
    h.hb.start();
    await settle();
    h.doc.visibilityState = 'hidden';
    h.doc.dispatchEvent(new Event('visibilitychange'));
    await settle();
    expect(h.beats).toHaveLength(1);
    h.doc.visibilityState = 'visible';
    h.doc.dispatchEvent(new Event('visibilitychange'));
    await settle();
    expect(h.beats).toHaveLength(2);
    h.hb.stop();
  });

  it('stop ends the session and beats no more', async () => {
    const h = harness();
    h.hb.start();
    await settle();
    h.hb.stop();
    expect(h.ends).toEqual([{}]);
    await vi.advanceTimersByTimeAsync(REVIEW_BEAT_INTERVAL_MS * 2);
    h.doc.dispatchEvent(new Event('visibilitychange'));
    h.win.dispatchEvent(new Event('pagehide'));
    await settle();
    expect(h.beats).toHaveLength(1);
    expect(h.ends).toHaveLength(1);
    // stop is idempotent.
    h.hb.stop();
    expect(h.ends).toHaveLength(1);
  });

  it('pagehide ends the session with keepalive', async () => {
    const h = harness();
    h.hb.start();
    await settle();
    h.win.dispatchEvent(new Event('pagehide'));
    expect(h.ends).toEqual([{ keepalive: true }]);
    await vi.advanceTimersByTimeAsync(REVIEW_BEAT_INTERVAL_MS);
    expect(h.beats).toHaveLength(1);
    // A later stop (the page tearing down) sends nothing more.
    h.hb.stop();
    expect(h.ends).toHaveLength(1);
  });

  it('rebase measures dirty from the new text and beats at once', async () => {
    const h = harness();
    h.hb.start();
    await settle();
    h.state.text = 'v9 text';
    h.state.base = 9;
    h.hb.rebase();
    await settle();
    expect(h.beats[1]).toEqual({ base: 9, edited: false, dirty: false });
    h.state.text = 'v9 text {>>note<<}';
    await vi.advanceTimersByTimeAsync(REVIEW_BEAT_INTERVAL_MS);
    expect(h.beats[2]).toEqual({ base: 9, edited: true, dirty: true });
    h.hb.stop();
  });

  it('passes the current version on, and survives failed beats', async () => {
    const ok = harness({ currentSeq: 9 });
    ok.hb.start();
    await settle();
    expect(ok.seqs).toEqual([9]);
    ok.hb.stop();

    const bad = harness({ fail: true });
    bad.hb.start();
    await settle();
    await vi.advanceTimersByTimeAsync(REVIEW_BEAT_INTERVAL_MS);
    expect(bad.beats).toHaveLength(2);
    expect(bad.seqs).toEqual([]);
    bad.hb.stop();
  });

  it('skips an interval beat while one is in flight', async () => {
    const h = harness();
    const gate = { release: (): void => undefined };
    h.put.mockImplementationOnce((_id: string, _sid: string, beat: ReviewBeat) => {
      h.beats.push(beat);
      return new Promise((r) => {
        gate.release = () => r({ currentSeq: 7 });
      });
    });
    h.hb.start();
    await vi.advanceTimersByTimeAsync(REVIEW_BEAT_INTERVAL_MS);
    expect(h.beats).toHaveLength(1);
    gate.release();
    await settle();
    await vi.advanceTimersByTimeAsync(REVIEW_BEAT_INTERVAL_MS);
    expect(h.beats).toHaveLength(2);
    h.hb.stop();
  });
});
