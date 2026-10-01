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
 * Tests for {@link TouchPrimaryController}: it reflects `matchMedia`,
 * updates its host on a live `change`, stops listening on disconnect, and
 * queries exactly {@link TOUCH_PRIMARY_QUERY}.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, afterEach } from 'vitest';

import { TouchPrimaryController, TOUCH_PRIMARY_QUERY } from './input-modality.js';

/** A minimal fake MediaQueryList the test can flip with `fire()`. */
class FakeMediaQueryList {
  matches: boolean;
  readonly media: string;
  private listeners = new Set<(ev: MediaQueryListEvent) => void>();

  constructor(media: string, matches: boolean) {
    this.media = media;
    this.matches = matches;
  }

  addEventListener(_type: 'change', listener: (ev: MediaQueryListEvent) => void): void {
    this.listeners.add(listener);
  }

  removeEventListener(_type: 'change', listener: (ev: MediaQueryListEvent) => void): void {
    this.listeners.delete(listener);
  }

  /** Simulate the media query's match state changing, firing `change` on every live listener. */
  fire(matches: boolean): void {
    this.matches = matches;
    const event = { matches } as MediaQueryListEvent;
    for (const listener of this.listeners) listener(event);
  }

  get listenerCount(): number {
    return this.listeners.size;
  }
}

/** A minimal fake `ReactiveControllerHost` that records `requestUpdate()` calls. */
class FakeHost {
  requestUpdateCalls = 0;
  private controller: TouchPrimaryController | null = null;

  addController(controller: TouchPrimaryController): void {
    this.controller = controller;
  }

  requestUpdate(): void {
    this.requestUpdateCalls++;
  }

  connect(): void {
    this.controller?.hostConnected();
  }

  disconnect(): void {
    this.controller?.hostDisconnected();
  }
}

function installFakeMatchMedia(initialMatches: boolean): FakeMediaQueryList {
  const mql = new FakeMediaQueryList(TOUCH_PRIMARY_QUERY, initialMatches);
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => {
      expect(query).toBe(TOUCH_PRIMARY_QUERY);
      return mql;
    })
  );
  return mql;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('TouchPrimaryController', () => {
  it('queries exactly TOUCH_PRIMARY_QUERY', () => {
    const mql = installFakeMatchMedia(false);
    const host = new FakeHost();
    new TouchPrimaryController(host);
    host.connect();
    expect(mql.media).toBe('(hover: none) and (pointer: coarse)');
  });

  it('reflects the initial matchMedia() result on connect', () => {
    installFakeMatchMedia(true);
    const host = new FakeHost();
    const controller = new TouchPrimaryController(host);
    expect(controller.isTouch).toBe(false); // not yet connected

    host.connect();
    expect(controller.isTouch).toBe(true);
  });

  it('updates isTouch and requests a host update on a live change event', () => {
    const mql = installFakeMatchMedia(false);
    const host = new FakeHost();
    const controller = new TouchPrimaryController(host);
    host.connect();
    expect(controller.isTouch).toBe(false);
    host.requestUpdateCalls = 0; // isolate the change event's own call below

    mql.fire(true);

    expect(controller.isTouch).toBe(true);
    expect(host.requestUpdateCalls).toBe(1);
  });

  it('does not request a host update when the query result does not actually change', () => {
    const mql = installFakeMatchMedia(false);
    const host = new FakeHost();
    const controller = new TouchPrimaryController(host);
    host.connect();
    host.requestUpdateCalls = 0; // isolate the change event's own call below

    mql.fire(false);

    expect(controller.isTouch).toBe(false);
    expect(host.requestUpdateCalls).toBe(0);
  });

  it('removes the change listener on hostDisconnected — a later flip is not observed', () => {
    const mql = installFakeMatchMedia(false);
    const host = new FakeHost();
    const controller = new TouchPrimaryController(host);
    host.connect();
    expect(mql.listenerCount).toBe(1);

    host.disconnect();
    expect(mql.listenerCount).toBe(0);
    host.requestUpdateCalls = 0; // isolate the (absent) listener call below

    mql.fire(true);
    expect(controller.isTouch).toBe(false);
    expect(host.requestUpdateCalls).toBe(0);
  });

  it('does not throw and leaves isTouch false when window is unavailable', () => {
    vi.stubGlobal('window', undefined);
    const host = new FakeHost();
    const controller = new TouchPrimaryController(host);

    expect(() => host.connect()).not.toThrow();
    expect(controller.isTouch).toBe(false);
  });

  it('requests a host update on reconnect after the query flipped while detached', () => {
    // While detached, hostDisconnected has already torn down the listener,
    // so nothing observes a flip during that window — a host reconnecting
    // afterwards must still pick up the new value *and* trigger a render for
    // it, since Lit does not re-render a bare reconnect on its own unless
    // something actually requests it.
    const mql = installFakeMatchMedia(false);
    const host = new FakeHost();
    const controller = new TouchPrimaryController(host);
    host.connect();
    expect(controller.isTouch).toBe(false);

    host.disconnect();
    mql.matches = true;
    host.requestUpdateCalls = 0; // isolate the reconnect's own call below

    host.connect();

    expect(controller.isTouch).toBe(true);
    expect(host.requestUpdateCalls).toBeGreaterThanOrEqual(1);
  });
});
