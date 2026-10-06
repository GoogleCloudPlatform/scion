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
 * Tests for the composer's measured field room, against stubbed boxes: a
 * flex column where the message list gives up exactly what the field takes.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import type { ReactiveController, ReactiveControllerHost } from 'lit';

import {
  ComposerRoomController,
  COMPOSER_CURVE_BASE_REM,
  COMPOSER_CURVE_SLOPE,
  FIELD_ROOM_PROPERTY,
  MESSAGE_ROW_RESERVE_REM,
  type RoomComposer,
} from './composer-room.js';

const REM = 16;
const RESERVE = MESSAGE_ROW_RESERVE_REM * REM;

/** A chat column laid out in a frame: list on top, composer at the bottom. */
interface Layout {
  frame: number;
  /** Top of the message list. */
  listTop: number;
  /** Composer chrome around the field (chip, bars, padding, footer). */
  chrome: number;
  field: number;
  /** What the tight-frame compaction frees (out of `chrome`). */
  savings: number;
  /** The field's height for two lines (0: no two-line floor). */
  twoLines: number;
}

/** Records what a ResizeObserver was asked to do. */
class StubResizeObserver {
  static instances: StubResizeObserver[] = [];
  observed = new Set<Element>();
  disconnected = false;
  constructor(readonly callback: ResizeObserverCallback) {
    StubResizeObserver.instances.push(this);
  }
  observe(el: Element): void {
    this.observed.add(el);
    this.disconnected = false;
  }
  unobserve(el: Element): void {
    this.observed.delete(el);
  }
  disconnect(): void {
    this.observed.clear();
    this.disconnected = true;
  }
}

let layout: Layout;
let frames: Array<() => void>;
let matches: boolean;
let host: HTMLElement & ReactiveControllerHost;
let messages: HTMLElement;
let composer: RoomComposer;
let controller: ComposerRoomController;
/** The one media-query list the stub window hands out. */
let mediaList: EventTarget;
/** Custom properties as the composer would compute them. */
let vars: Record<string, string>;
let win: EventTarget & { visualViewport: EventTarget };
const originalResizeObserver = globalThis.ResizeObserver;

function rect(top: number, bottom: number): DOMRect {
  return {
    top,
    bottom,
    height: bottom - top,
    left: 0,
    right: 0,
    width: 0,
    x: 0,
    y: top,
  } as DOMRect;
}

function flush(): void {
  const pending = frames;
  frames = [];
  for (const cb of pending) cb();
}

function room(): string {
  return composer.style.getPropertyValue(FIELD_ROOM_PROPERTY);
}

beforeEach(() => {
  layout = { frame: 300, listTop: 111, chrome: 90, field: 41, savings: 0, twoLines: 0 };
  StubResizeObserver.instances = [];
  globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;
  frames = [];
  matches = true;
  vars = {};
  mediaList = new EventTarget();
  Object.defineProperty(mediaList, 'matches', { get: () => matches });

  const controllers: ReactiveController[] = [];
  host = Object.assign(document.createElement('div'), {
    addController: (c: ReactiveController) => controllers.push(c),
    removeController: () => {},
    requestUpdate: () => {},
    updateComplete: Promise.resolve(true),
  });
  messages = document.createElement('div');
  // The list fills what the composer leaves: it shrinks as the field grows.
  messages.getBoundingClientRect = () =>
    rect(layout.listTop, layout.frame - layout.chrome - layout.field);
  composer = Object.assign(document.createElement('div'), {
    fieldHeight: () => layout.field,
    tightSavings: () => layout.savings,
    fieldLinesHeight: (lines: number) => (lines === 2 ? layout.twoLines : 0),
    clippedHeight: () => 0,
  });
  composer.getBoundingClientRect = () =>
    rect(layout.frame - layout.chrome - layout.field, layout.frame);

  win = Object.assign(new EventTarget(), {
    document,
    innerHeight: 667,
    visualViewport: new EventTarget(),
    matchMedia: () => mediaList,
    requestAnimationFrame: (cb: () => void) => frames.push(cb),
    cancelAnimationFrame: () => {},
    getComputedStyle: (el: Element) => ({
      fontSize: `${REM}px`,
      getPropertyValue: (name: string): string => (el === composer ? (vars[name] ?? '') : ''),
    }),
  });
  document.documentElement.style.setProperty('--scion-app-height', `${layout.frame}px`);
  controller = new ComposerRoomController(
    host,
    () => ({ messages, composer }),
    win as unknown as Window
  );
  controller.hostConnected();
});

afterEach(() => {
  controller.hostDisconnected();
  globalThis.ResizeObserver = originalResizeObserver;
  document.documentElement.style.removeProperty('--scion-app-height');
});

describe('ComposerRoomController', () => {
  it('writes the frame below the list, less a message row and the composer chrome', () => {
    controller.hostUpdated();
    flush();
    expect(room()).toBe(`${300 - 111 - RESERVE - 90}px`);
    expect(controller.writes).toBe(1);
  });

  it('does not feed back: the field growing into the room writes nothing more', () => {
    controller.hostUpdated();
    flush();
    const first = room();
    // The field grows to the cap, a px at a time, each step re-measured as
    // the resize observer would.
    for (let field = 42; field <= parseFloat(first); field++) {
      layout.field = field;
      controller.measure();
    }
    for (let i = 0; i < 50; i++) controller.measure();
    expect(room()).toBe(first);
    expect(controller.writes).toBe(1);
  });

  it('rounds to whole px, so sub-pixel layout noise writes nothing', () => {
    controller.hostUpdated();
    flush();
    layout.listTop = 111.2;
    controller.measure();
    layout.listTop = 111.4;
    controller.measure();
    expect(controller.writes).toBe(1);
  });

  it('counts a reply bar appearing in the composer', () => {
    controller.hostUpdated();
    flush();
    layout.chrome += 20;
    controller.measure();
    expect(room()).toBe(`${300 - 111 - RESERVE - 110}px`);
    expect(controller.writes).toBe(2);
  });

  it('follows the keyboard frame', () => {
    controller.hostUpdated();
    flush();
    layout.frame = 200;
    document.documentElement.style.setProperty('--scion-app-height', '200px');
    controller.measure();
    expect(room()).toBe('0px');
  });

  it('coalesces a burst of events into one read per animation frame', () => {
    controller.hostUpdated();
    flush();
    for (let i = 0; i < 10; i++) {
      win.dispatchEvent(new Event('resize'));
      win.visualViewport.dispatchEvent(new Event('resize'));
      host.dispatchEvent(new Event('focusin'));
    }
    expect(frames).toHaveLength(1);
  });

  it('removes the variable outside the phone and tablet layout', () => {
    controller.hostUpdated();
    flush();
    matches = false;
    controller.measure();
    expect(room()).toBe('');
    expect(controller.writes).toBe(2);
    controller.measure();
    expect(controller.writes).toBe(2);
  });

  /** The column the curve is based on: the frame below the list, plus a hidden top bar. */
  function column(): number {
    const hidden =
      (parseFloat(vars['--scion-chat-top-bar-hidden'] ?? '') || 0) *
      (parseFloat(vars['--scion-chat-top-bar-h'] ?? '') || 0);
    return layout.frame - layout.listTop - hidden;
  }

  /** The curve for the current layout: the composer's share less its chrome. */
  function curve(): number {
    return (
      COMPOSER_CURVE_SLOPE * column() +
      COMPOSER_CURVE_BASE_REM * REM -
      (layout.chrome + layout.savings)
    );
  }

  function setFrame(frame: number): void {
    layout.frame = frame;
    document.documentElement.style.setProperty('--scion-app-height', `${frame}px`);
  }

  it('caps at the curve where it is below the measured room', () => {
    setFrame(800);
    controller.hostUpdated();
    flush();
    expect(room()).toBe(`${Math.floor(curve())}px`);
  });

  it('subtracts all measured chrome from the curve, as from the room', () => {
    setFrame(800);
    controller.hostUpdated();
    flush();
    const bare = parseFloat(room());
    // Any row (a bar, attachments, an error) lowers the curve by its height.
    layout.chrome += 37;
    controller.measure();
    expect(parseFloat(room())).toBe(bare - 37);
  });

  it('adds back what the tight frame takes out of flow, so the field does not move', () => {
    setFrame(800);
    controller.hostUpdated();
    flush();
    const before = room();
    // Crossing into a tight frame: the chip and footer leave the flow
    // (chrome drops by 49) and are reported as savings.
    layout.chrome -= 49;
    layout.savings += 49;
    controller.measure();
    expect(room()).toBe(before);
  });

  it('counts a hidden top bar as shown, so hiding it does not move the field', () => {
    setFrame(800);
    controller.hostUpdated();
    flush();
    const shown = room();
    // The shell hides its 61px top bar: the list moves up by 61.
    layout.listTop -= 61;
    vars['--scion-chat-top-bar-hidden'] = '1';
    vars['--scion-chat-top-bar-h'] = '61px';
    controller.measure();
    expect(room()).toBe(shown);
  });

  it('a row above the list lowers the field the same way in every frame', () => {
    // Where the room binds (a short frame) and where the curve binds (a tall
    // one), a 50px row above the list never raises the field.
    for (const frame of [330, 800]) {
      setFrame(frame);
      layout.listTop = 111;
      controller.measure();
      const before = parseFloat(room());
      layout.listTop = 161;
      controller.measure();
      expect(parseFloat(room())).toBeLessThan(before);
    }
  });

  it('observes the message list and composer, and lets go of both on disconnect', () => {
    controller.hostUpdated();
    const observer = StubResizeObserver.instances.at(-1)!;
    expect([...observer.observed]).toEqual([messages, composer]);

    controller.hostDisconnected();
    expect(observer.disconnected).toBe(true);
    expect(observer.observed.size).toBe(0);
  });

  it('removes every listener it added on disconnect', () => {
    const added: Array<[EventTarget, string, unknown]> = [];
    const removed: Array<[EventTarget, string, unknown]> = [];
    for (const target of [win, win.visualViewport, host, mediaList] as EventTarget[]) {
      const add = target.addEventListener.bind(target);
      const remove = target.removeEventListener.bind(target);
      vi.spyOn(target, 'addEventListener').mockImplementation((type, listener, options) => {
        added.push([target, type, listener]);
        add(type, listener, options);
      });
      vi.spyOn(target, 'removeEventListener').mockImplementation((type, listener, options) => {
        removed.push([target, type, listener]);
        remove(type, listener, options);
      });
    }
    controller.hostDisconnected();
    controller.hostConnected();
    expect(added.map(([target, type]) => [target === mediaList, type])).toContainEqual([
      true,
      'change',
    ]);
    expect(added.length).toBe(4);
    controller.hostDisconnected();
    for (const entry of added) expect(removed).toContainEqual(entry);
  });

  it('a reconnect without a re-render observes again and re-measures', () => {
    controller.hostUpdated();
    flush();
    controller.hostDisconnected();
    layout.chrome += 20;
    controller.hostConnected();
    const observer = StubResizeObserver.instances.at(-1)!;
    expect(observer.disconnected).toBe(false);
    expect([...observer.observed]).toEqual([messages, composer]);
    flush();
    expect(room()).toBe(`${300 - 111 - RESERVE - 110}px`);
  });

  it('phases the freed chrome in: continuous and never falling as the frame grows', () => {
    layout.twoLines = 66;
    controller.hostUpdated();
    flush();
    // A bare chat column: the top bar (61px) hides below 360 and the chip,
    // footer and padding (61px) leave the flow below 290.
    let extra = 0;
    let above = 0;
    const at = (frame: number): number => {
      setFrame(frame);
      const short = frame < 360;
      const tight = frame < 290;
      layout.listTop = (short ? 50 : 111) + above;
      vars['--scion-chat-top-bar-hidden'] = short ? '1' : '';
      vars['--scion-chat-top-bar-h'] = '61px';
      layout.savings = tight ? 61 : 0;
      layout.chrome = (tight ? 15 : 76) + extra;
      layout.field = 41;
      controller.measure();
      return parseFloat(room());
    };
    // With a bar above the list (`above`) and extra chrome around the
    // field (`extra`), the room rather than the curve sets the field near
    // the thresholds.
    for (above of [0, 42]) {
      for (extra of [0, 30, 60]) {
        let previous = -Infinity;
        for (let frame = 150; frame <= 440; frame += 1) {
          const value = at(frame);
          expect(value, `${frame}px, above +${above}, chrome +${extra}`).toBeGreaterThanOrEqual(
            previous - 1
          );
          previous = value;
        }
      }
    }
    extra = 0;
    above = 0;
    // Two lines of draft (about 66px) from a 200px frame up, with a message
    // row kept: the list is what the column leaves the composer.
    for (const frame of [200, 230, 260, 289, 291, 330, 359, 361]) {
      const value = at(frame);
      expect(value, `two lines at ${frame}px`).toBeGreaterThanOrEqual(66);
      const list = frame - layout.listTop - (layout.chrome + value);
      expect(list, `a message row at ${frame}px`).toBeGreaterThanOrEqual(RESERVE);
    }
  });

  it('gives a multi-line draft two lines where a shorter list reserve allows', () => {
    layout.twoLines = 66;
    setFrame(300);
    layout.listTop = 50;
    layout.chrome = 125;
    controller.hostUpdated();
    flush();
    // The room (4rem reserve) and the curve are both under two lines here,
    // but 2.5rem of list leaves room for them.
    const fullReserveRoom = 300 - 50 - RESERVE - 125;
    expect(fullReserveRoom).toBeLessThan(66);
    expect(curve()).toBeLessThan(66);
    expect(fullReserveRoom + 1.5 * REM).toBeGreaterThanOrEqual(66);
    expect(room()).toBe('66px');
  });

  it('never gives the two-line floor more than the shorter reserve leaves', () => {
    layout.twoLines = 66;
    setFrame(200);
    layout.listTop = 50;
    layout.chrome = 120;
    controller.hostUpdated();
    flush();
    const shorterReserveRoom = 200 - 50 - 2.5 * REM - 120;
    expect(parseFloat(room())).toBe(Math.max(0, Math.floor(shorterReserveRoom)));
  });
});
