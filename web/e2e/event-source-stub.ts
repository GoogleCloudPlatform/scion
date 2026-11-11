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
 * Shared stand-in for the browser `EventSource`, for the e2e suites that
 * mock the hub and have no real `/events` stream behind them.
 *
 * The stub carries the static and instance `CONNECTING`/`OPEN`/`CLOSED`
 * constants and a real `readyState`, so code that compares the two (the
 * `SSEClient.connected` getter and its reconnect guard in
 * `src/client/sse-client.ts`) sees a stubbed stream the way it would see a
 * live one: `CONNECTING` until the handshake resolves, then `OPEN`, then
 * `CLOSED` after `close()`.
 *
 * Every stream is kept on `window.__eventSourceStub.instances`.
 * {@link emitEventSourceUpdate} delivers a hub `update` event on the open
 * ones. The handshake outcome for streams created from then on can be
 * switched in the page through `window.__eventSourceStub.handshake`.
 */

import type { Page } from '@playwright/test';

/** How a new stream's handshake resolves. */
export type EventSourceStubHandshake = 'open' | 'error';

export interface EventSourceStubOptions {
  /**
   * `'open'` (default): the stream opens on the next microtask.
   * `'error'`: the handshake is refused; the stream goes straight to
   * `CLOSED` and fires `error` without ever opening.
   */
  handshake?: EventSourceStubHandshake;
  /**
   * Also deliver the server's `connected` event just before `open`, as the
   * hub's handshake acknowledgement would. Off by default.
   */
  serverConnectedEvent?: boolean;
}

/** The in-page shape of one stub stream. */
export interface StubEventSource extends EventTarget {
  readonly url: string;
  readonly withCredentials: boolean;
  readyState: number;
  readonly CONNECTING: 0;
  readonly OPEN: 1;
  readonly CLOSED: 2;
  onopen: ((event: Event) => void) | null;
  onmessage: ((event: MessageEvent) => void) | null;
  onerror: ((event: Event) => void) | null;
  close(): void;
}

/** The in-page control object the stub installs on `window`. */
export interface EventSourceStubControl {
  /** Every stream constructed so far, in order, closed ones included. */
  instances: StubEventSource[];
  /** Handshake outcome for streams constructed from now on. */
  handshake: EventSourceStubHandshake;
  /** Whether new streams deliver the server `connected` event before `open`. */
  serverConnectedEvent: boolean;
}

declare global {
  interface Window {
    __eventSourceStub: EventSourceStubControl;
  }
}

/**
 * Replaces `window.EventSource` with the shared stub in every document the
 * page loads. Call before `page.goto`.
 */
export async function installEventSourceStub(
  page: Page,
  options: EventSourceStubOptions = {}
): Promise<void> {
  await page.addInitScript(
    ({ handshake, serverConnectedEvent }) => {
      const control: EventSourceStubControl = {
        instances: [],
        handshake,
        serverConnectedEvent,
      };
      window.__eventSourceStub = control;

      class FixtureEventSource extends EventTarget {
        static readonly CONNECTING = 0;
        static readonly OPEN = 1;
        static readonly CLOSED = 2;
        readonly CONNECTING = 0;
        readonly OPEN = 1;
        readonly CLOSED = 2;
        readonly url: string;
        readonly withCredentials: boolean;
        readyState = 0;
        onopen: ((event: Event) => void) | null = null;
        onmessage: ((event: MessageEvent) => void) | null = null;
        onerror: ((event: Event) => void) | null = null;

        constructor(url: string | URL, init?: { withCredentials?: boolean }) {
          super();
          this.url = String(url);
          this.withCredentials = !!init?.withCredentials;
          control.instances.push(this);
          const outcome = control.handshake;
          const ack = control.serverConnectedEvent;
          queueMicrotask(() => {
            // Closed before the handshake resolved: stay closed, like a
            // real stream would.
            if (this.readyState !== 0) return;
            if (outcome === 'error') {
              this.readyState = 2;
              const event = new Event('error');
              this.dispatchEvent(event);
              this.onerror?.(event);
              return;
            }
            this.readyState = 1;
            if (ack) {
              this.dispatchEvent(
                new MessageEvent('connected', {
                  data: JSON.stringify({ connectionId: 'test', subjects: [] }),
                })
              );
            }
            const event = new Event('open');
            this.dispatchEvent(event);
            this.onopen?.(event);
          });
        }

        close(): void {
          this.readyState = 2;
        }
      }

      window.EventSource = FixtureEventSource as unknown as typeof EventSource;
    },
    {
      handshake: options.handshake ?? 'open',
      serverConnectedEvent: options.serverConnectedEvent ?? false,
    }
  );
}

/**
 * Delivers a hub `update` event (`{ subject, data }`) on every open stub
 * stream.
 */
export async function emitEventSourceUpdate(
  page: Page,
  subject: string,
  data: unknown
): Promise<void> {
  await page.evaluate(
    ({ subject: s, data: d }) => {
      const payload = JSON.stringify({ subject: s, data: d });
      for (const es of window.__eventSourceStub.instances) {
        if (es.readyState !== 1) continue;
        es.dispatchEvent(new MessageEvent('update', { data: payload }));
      }
    },
    { subject, data }
  );
}
