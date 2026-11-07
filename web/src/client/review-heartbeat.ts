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
 * The heartbeat of an open review (ptone/scion#6849). While Review mode is
 * open the page tells the hub, once a minute, that this tab is reviewing
 * a version, so a publisher who would overtake the review is warned first.
 * A beat goes out when the review opens, every minute while it stays open,
 * when the tab becomes visible again, and after the review moves to a newer
 * version. Closing the review (Save, Cancel) or the page ends the session.
 *
 * Presence is best effort: a failed beat is dropped, and the hub stops
 * counting a session at most five minutes after its last beat.
 */

import {
  endReviewSession,
  newReviewSessionId,
  putReviewSession,
  type ReviewBeat,
  type ReviewBeatResponse,
} from './artifacts.js';

/** How often an open review beats. */
export const REVIEW_BEAT_INTERVAL_MS = 60_000;

/** What the page reports about its open review. */
export interface ReviewHeartbeatState {
  /** The version under review. */
  base: number;
  /** The current review text. */
  text: string;
}

export interface ReviewHeartbeatOptions {
  artifactId: string;
  /** Read at each beat. */
  state: () => ReviewHeartbeatState;
  /** Told the artifact's current version after each answered beat. */
  onCurrentSeq?: (seq: number) => void;
  intervalMs?: number;
  /** Test seams; default to the hub calls and the page. */
  put?: (artifactId: string, sessionId: string, beat: ReviewBeat) => Promise<ReviewBeatResponse>;
  end?: (artifactId: string, sessionId: string, opts: { keepalive?: boolean }) => Promise<void>;
  newId?: () => string;
  doc?: Document;
  win?: Window;
}

/** One open review's heartbeat. Use start() once, then stop() once. */
export class ReviewHeartbeat {
  readonly sessionId: string;
  private readonly opts: ReviewHeartbeatOptions;
  private readonly put: NonNullable<ReviewHeartbeatOptions['put']>;
  private readonly end: NonNullable<ReviewHeartbeatOptions['end']>;
  private readonly doc: Document;
  private readonly win: Window;
  private timer: ReturnType<typeof setInterval> | null = null;
  private running = false;
  private inFlight = false;
  /** The text the review started from: dirty compares against it. */
  private startText = '';
  /** The text at the last beat: edited compares against it. */
  private lastText = '';

  constructor(opts: ReviewHeartbeatOptions) {
    this.opts = opts;
    this.put = opts.put ?? putReviewSession;
    this.end = opts.end ?? endReviewSession;
    this.doc = opts.doc ?? document;
    this.win = opts.win ?? window;
    this.sessionId = (opts.newId ?? newReviewSessionId)();
  }

  /** Starts beating: one beat now, then every interval. */
  start(): void {
    if (this.running) return;
    this.running = true;
    const { text } = this.opts.state();
    this.startText = text;
    this.lastText = text;
    this.timer = setInterval(
      () => void this.beat(),
      this.opts.intervalMs ?? REVIEW_BEAT_INTERVAL_MS
    );
    this.doc.addEventListener('visibilitychange', this.onVisibility);
    this.win.addEventListener('pagehide', this.onPageHide);
    void this.beat();
  }

  /**
   * The review moved to another version, starting again from its text:
   * dirty is measured from the new text, and a beat goes out now.
   */
  rebase(): void {
    if (!this.running) return;
    const { text } = this.opts.state();
    this.startText = text;
    this.lastText = text;
    void this.beat(true);
  }

  /** Stops beating and ends the session on the hub. */
  stop(): void {
    if (!this.running) return;
    this.halt();
    void this.end(this.opts.artifactId, this.sessionId, {});
  }

  /** Sends one beat; unless forced, a beat already in flight makes this one a no-op. */
  async beat(force = false): Promise<void> {
    if (!this.running || (this.inFlight && !force)) return;
    const { base, text } = this.opts.state();
    const beat: ReviewBeat = {
      base,
      edited: text !== this.lastText,
      dirty: text !== this.startText,
    };
    this.lastText = text;
    this.inFlight = true;
    try {
      const res = await this.put(this.opts.artifactId, this.sessionId, beat);
      if (this.running && typeof res.currentSeq === 'number')
        this.opts.onCurrentSeq?.(res.currentSeq);
    } catch {
      // Best effort: the next beat tries again.
    } finally {
      this.inFlight = false;
    }
  }

  private halt(): void {
    this.running = false;
    if (this.timer) clearInterval(this.timer);
    this.timer = null;
    this.doc.removeEventListener('visibilitychange', this.onVisibility);
    this.win.removeEventListener('pagehide', this.onPageHide);
  }

  private onVisibility = (): void => {
    if (this.doc.visibilityState === 'visible') void this.beat();
  };

  /** The page is going away: end the session with a request that outlives it. */
  private onPageHide = (): void => {
    if (!this.running) return;
    this.halt();
    void this.end(this.opts.artifactId, this.sessionId, { keepalive: true });
  };
}
