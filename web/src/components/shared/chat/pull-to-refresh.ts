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
 * Touch pull-to-refresh for a scroll container.
 *
 * The gesture starts only when the container is scrolled to the top and the
 * finger moves down more than sideways, so ordinary scrolling, horizontal
 * panel swipes, and long-presses are left alone. Mouse and trackpad input is
 * never involved: only touch events are listened to.
 *
 * Once a pull is under way the controller cancels the touchmove's default
 * action, so neither the container's own overscroll bounce nor the browser's
 * page reload fires underneath it. The container should also set
 * `overscroll-behavior: contain` for the cases a listener cannot catch.
 */

/** What the indicator should show. */
export interface PullState {
  /** How far, in CSS pixels, the indicator is pulled down (already damped). */
  distance: number;
  /** The pull has passed the threshold: letting go now refreshes. */
  armed: boolean;
  /** A refresh is running. */
  refreshing: boolean;
}

export interface PullToRefreshOptions {
  /** The scroll container; a pull starts only while it is scrolled to the top. */
  scroller: () => HTMLElement | null | undefined;
  /** Runs the refresh. A rejection is swallowed; the indicator still clears. */
  onRefresh: () => Promise<void>;
  /** Called whenever {@link PullState} changes. */
  onChange: (state: PullState) => void;
  /** Damped pull distance that arms a refresh. */
  threshold?: number;
  /** The furthest the indicator can be pulled. */
  maxDistance?: number;
}

/** Damped distance, in pixels, a pull must reach before letting go refreshes. */
export const PULL_THRESHOLD_PX = 64;
/** The furthest the indicator travels, however far the finger goes. */
export const PULL_MAX_PX = 96;
/** Finger travel before the gesture decides whether it is vertical. */
const SLOP_PX = 8;
/** Finger travel per pixel of indicator travel: a pull feels heavier than a scroll. */
const RESISTANCE = 2;

export class PullToRefreshController {
  private readonly threshold: number;
  private readonly maxDistance: number;
  private target: EventTarget | null = null;

  /** Where the tracked touch started, or null when no touch is tracked. */
  private start: { x: number; y: number; id: number } | null = null;
  /** 'pull' once the touch is a downward pull at the top; 'ignore' once it is anything else. */
  private mode: 'pending' | 'pull' | 'ignore' = 'pending';
  private distance = 0;
  private inFlight: Promise<void> | null = null;

  constructor(private readonly options: PullToRefreshOptions) {
    this.threshold = options.threshold ?? PULL_THRESHOLD_PX;
    this.maxDistance = Math.max(options.maxDistance ?? PULL_MAX_PX, this.threshold);
  }

  /** Listen for touches on `target` (the scroller or an ancestor of it). */
  attach(target: EventTarget): void {
    if (this.target === target) return;
    this.detach();
    this.target = target;
    target.addEventListener('touchstart', this.onTouchStart, { passive: true });
    // Not passive: an active pull cancels the browser's own handling.
    target.addEventListener('touchmove', this.onTouchMove, { passive: false });
    target.addEventListener('touchend', this.onTouchEnd);
    target.addEventListener('touchcancel', this.onTouchCancel);
  }

  detach(): void {
    const target = this.target;
    if (!target) return;
    target.removeEventListener('touchstart', this.onTouchStart);
    target.removeEventListener('touchmove', this.onTouchMove);
    target.removeEventListener('touchend', this.onTouchEnd);
    target.removeEventListener('touchcancel', this.onTouchCancel);
    this.target = null;
    this.reset();
  }

  get state(): PullState {
    return {
      // A running refresh holds the indicator at the threshold.
      distance: this.inFlight ? this.threshold : this.distance,
      armed: this.inFlight === null && this.distance >= this.threshold,
      refreshing: this.inFlight !== null,
    };
  }

  /**
   * Run a refresh now. Single-flight: while one is running, further calls
   * (another pull, or a programmatic refresh) join it.
   */
  refresh(): Promise<void> {
    if (this.inFlight) return this.inFlight;
    const run = (async () => {
      try {
        await this.options.onRefresh();
      } catch {
        // The owner reports its own failures; the indicator just clears.
      } finally {
        this.inFlight = null;
        this.distance = 0;
        this.emit();
      }
    })();
    // The body awaits before its `finally`, so this lands first.
    this.inFlight = run;
    this.emit();
    return run;
  }

  private readonly onTouchStart = (e: Event): void => {
    const touch = (e as TouchEvent).touches;
    // A second finger (pinch) is not a pull.
    if (touch.length !== 1) {
      this.cancelPull();
      return;
    }
    const scroller = this.options.scroller();
    const path = typeof e.composedPath === 'function' ? e.composedPath() : [];
    if (
      !scroller ||
      this.inFlight ||
      scroller.scrollTop > 0 ||
      (path.length > 0 && !path.includes(scroller))
    ) {
      this.start = null;
      this.mode = 'ignore';
      return;
    }
    this.start = { x: touch[0].clientX, y: touch[0].clientY, id: touch[0].identifier };
    this.mode = 'pending';
  };

  private readonly onTouchMove = (e: Event): void => {
    const start = this.start;
    if (!start || this.mode === 'ignore') return;
    const touch = Array.from((e as TouchEvent).touches).find((t) => t.identifier === start.id);
    if (!touch) return;
    const dx = touch.clientX - start.x;
    const dy = touch.clientY - start.y;
    if (this.mode === 'pending') {
      if (Math.abs(dx) < SLOP_PX && Math.abs(dy) < SLOP_PX) {
        // A browser stops letting a touch be cancelled once it has started
        // its own scroll or overscroll, which can be inside this slop: hold
        // it off while the move still looks like a pull.
        if (dy > 0 && dy >= Math.abs(dx) && e.cancelable) e.preventDefault();
        return;
      }
      const scroller = this.options.scroller();
      // Only a mostly-vertical downward move, still at the top, is a pull.
      if (dy <= 0 || Math.abs(dx) >= Math.abs(dy) || !scroller || scroller.scrollTop > 0) {
        this.mode = 'ignore';
        return;
      }
      this.mode = 'pull';
    }
    const next = Math.min(this.maxDistance, Math.max(0, dy / RESISTANCE));
    if (e.cancelable) e.preventDefault();
    if (next !== this.distance) {
      this.distance = next;
      this.emit();
    }
  };

  private readonly onTouchEnd = (e: Event): void => {
    const start = this.start;
    if (!start) return;
    // Another finger lifting is not the end of this pull.
    const stillDown = Array.from((e as TouchEvent).touches ?? []).some(
      (t) => t.identifier === start.id
    );
    if (stillDown) return;
    const fire = this.mode === 'pull' && this.distance >= this.threshold;
    this.start = null;
    this.mode = 'pending';
    if (fire) {
      void this.refresh();
    } else {
      this.cancelPull();
    }
  };

  private readonly onTouchCancel = (): void => {
    this.start = null;
    this.mode = 'pending';
    this.cancelPull();
  };

  /** Drop an unfinished pull's indicator. */
  private cancelPull(): void {
    if (this.inFlight) return;
    if (this.distance !== 0) {
      this.distance = 0;
      this.emit();
    }
  }

  private reset(): void {
    this.start = null;
    this.mode = 'pending';
    if (!this.inFlight) this.distance = 0;
  }

  private emit(): void {
    this.options.onChange(this.state);
  }
}
