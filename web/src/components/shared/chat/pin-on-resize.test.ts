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

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import type { ReactiveControllerHost } from 'lit';

import { PinOnResizeController } from './pin-on-resize.js';

class StubResizeObserver {
  static instances: StubResizeObserver[] = [];
  observed: Element[] = [];
  disconnected = false;
  constructor(readonly callback: ResizeObserverCallback) {
    StubResizeObserver.instances.push(this);
  }
  observe(el: Element): void {
    this.observed.push(el);
  }
  unobserve(): void {}
  disconnect(): void {
    this.observed = [];
    this.disconnected = true;
  }
}

const originalResizeObserver = globalThis.ResizeObserver;
let list: HTMLElement;
let height: number;
let pinned: boolean;
let matches: boolean;
let controller: PinOnResizeController;
let mediaList: EventTarget;

beforeEach(() => {
  StubResizeObserver.instances = [];
  globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;
  height = 400;
  pinned = true;
  matches = true;
  mediaList = new EventTarget();
  Object.defineProperty(mediaList, 'matches', { get: () => matches });
  list = document.createElement('div');
  Object.defineProperty(list, 'clientHeight', { get: () => height });
  Object.defineProperty(list, 'scrollHeight', { get: () => 2000 });
  list.scrollTop = 1600;
  const host = { addController: () => {} } as unknown as ReactiveControllerHost;
  const win = Object.assign(new EventTarget(), {
    matchMedia: () => mediaList,
  });
  controller = new PinOnResizeController(
    host,
    () => list,
    () => pinned,
    win as unknown as Window
  );
  controller.hostConnected();
  controller.handleResize();
});

afterEach(() => {
  controller.hostDisconnected();
  globalThis.ResizeObserver = originalResizeObserver;
});

describe('PinOnResizeController', () => {
  it('observes the list on a phone or tablet layout', () => {
    expect(StubResizeObserver.instances.at(-1)?.observed).toEqual([list]);
  });

  it('keeps a pinned list at the bottom when it gets shorter', () => {
    height = 250;
    controller.handleResize();
    expect(list.scrollTop).toBe(2000);
  });

  it('leaves a list the user scrolled up where it is', () => {
    pinned = false;
    list.scrollTop = 300;
    height = 250;
    controller.handleResize();
    expect(list.scrollTop).toBe(300);
  });

  it('does nothing when the list grows', () => {
    list.scrollTop = 1500;
    height = 500;
    controller.handleResize();
    expect(list.scrollTop).toBe(1500);
  });

  it('observes nothing on a desktop layout, and disconnects on disconnect', () => {
    const observer = StubResizeObserver.instances.at(-1)!;
    controller.hostDisconnected();
    expect(observer.disconnected).toBe(true);
    matches = false;
    controller.hostConnected();
    expect(StubResizeObserver.instances.at(-1)?.observed ?? []).toEqual([]);
  });

  it('removes its media-query listener on disconnect', () => {
    const removed: string[] = [];
    const remove = mediaList.removeEventListener.bind(mediaList);
    vi.spyOn(mediaList, 'removeEventListener').mockImplementation((type, listener, options) => {
      removed.push(type);
      remove(type, listener, options);
    });
    controller.hostDisconnected();
    expect(removed).toContain('change');
  });
});
