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
 * Chromium: the real `SSEClient` over the shared `EventSource` stub that
 * every mocked-hub suite uses. The client compares `readyState` with the
 * `EventSource.OPEN`/`CONNECTING` constants, so the stub must carry them for
 * an open stubbed stream to read as connected. When the tab is shown again,
 * a connected client keeps its stream; one that misread it as down would
 * close it and open another.
 */

import { test, expect } from '@playwright/test';
import { setupApiMocks } from './mock-api.js';
import type {} from '../event-source-stub.js';

test('the SSE client reads an open stub stream as connected and keeps it when the tab is shown', async ({
  page,
}) => {
  await setupApiMocks(page);
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });

  const result = await page.evaluate(async () => {
    // @ts-expect-error TS2307 - dynamic import runs in Playwright browser context via Vite dev-server
    const { SSEClient } = (await import('/src/client/sse-client.js')) as {
      SSEClient: new () => EventTarget & {
        connect: (subjects: string[]) => void;
        disconnect: () => void;
        readonly connected: boolean;
      };
    };
    const probe = 'stub-connected-probe';
    const probeStreams = (): Array<{ readyState: number }> =>
      window.__eventSourceStub.instances.filter((es) => es.url.includes(probe));

    const client = new SSEClient();
    const opened = new Promise<void>((resolve) =>
      client.addEventListener('connected', () => resolve(), { once: true })
    );
    client.connect([probe]);
    const connectedWhileConnecting = client.connected;
    await opened;
    const connectedWhenOpen = client.connected;

    // Tab shown again: a connected client leaves its live stream alone.
    document.dispatchEvent(new Event('visibilitychange'));
    const streamsAfterShow = probeStreams().length;
    const connectedAfterShow = client.connected;

    client.disconnect();
    return {
      constants: [EventSource.CONNECTING, EventSource.OPEN, EventSource.CLOSED],
      connectedWhileConnecting,
      connectedWhenOpen,
      streamsAfterShow,
      connectedAfterShow,
      readyStateAfterDisconnect: probeStreams()[0]?.readyState,
    };
  });

  expect(result).toEqual({
    constants: [0, 1, 2],
    connectedWhileConnecting: false,
    connectedWhenOpen: true,
    streamsAfterShow: 1,
    connectedAfterShow: true,
    readyStateAfterDisconnect: 2,
  });
});
