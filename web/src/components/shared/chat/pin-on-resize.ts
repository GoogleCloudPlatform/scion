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
 * Keeps the message list at the bottom when it gets shorter while it was
 * pinned there: the keyboard opening, a reply bar or an error appearing.
 * The browser keeps `scrollTop` when a scroller shrinks, which leaves the
 * newest messages hidden below. A list the user has scrolled up is left
 * where it is.
 *
 * Only on a phone or tablet layout (the same media query as the
 * composer's cap), where the keyboard shrinks the frame; elsewhere it
 * observes nothing.
 */

import type { ReactiveController, ReactiveControllerHost } from 'lit';
import { COMPOSER_CAP_MEDIA } from './composer-room.js';

export class PinOnResizeController implements ReactiveController {
  private readonly getList: () => HTMLElement | null;
  private readonly isPinned: () => boolean;
  private readonly win: Window;
  private observer: ResizeObserver | null = null;
  private observed: HTMLElement | null = null;
  private media: MediaQueryList | null = null;
  /** The list's height at the last observation, to act only on a shrink. */
  private lastHeight = Number.NaN;

  constructor(
    host: ReactiveControllerHost,
    getList: () => HTMLElement | null,
    isPinned: () => boolean,
    win: Window = window
  ) {
    this.getList = getList;
    this.isPinned = isPinned;
    this.win = win;
    host.addController(this);
  }

  hostConnected(): void {
    this.media = this.win.matchMedia?.(COMPOSER_CAP_MEDIA) ?? null;
    this.media?.addEventListener('change', this.sync);
    this.sync();
  }

  hostUpdated(): void {
    this.sync();
  }

  hostDisconnected(): void {
    this.media?.removeEventListener('change', this.sync);
    this.media = null;
    this.observer?.disconnect();
    this.observer = null;
    this.observed = null;
  }

  /** Observe the current list on a phone or tablet layout, nothing elsewhere. */
  private readonly sync = (): void => {
    const list = this.media?.matches ? this.getList() : null;
    if (list === this.observed) return;
    this.observer?.disconnect();
    this.observed = list;
    this.lastHeight = Number.NaN;
    if (!list || typeof ResizeObserver === 'undefined') return;
    this.observer ??= new ResizeObserver(this.handleResize);
    this.observer.observe(list);
  };

  /** Public for tests. */
  readonly handleResize = (): void => {
    const list = this.observed;
    if (!list) return;
    const height = list.clientHeight;
    const shrank = height < this.lastHeight;
    this.lastHeight = height;
    if (shrank && this.isPinned()) list.scrollTop = list.scrollHeight;
  };
}
