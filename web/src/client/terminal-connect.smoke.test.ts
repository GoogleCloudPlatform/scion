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
/**
 * Synthetic smoke checks for the web terminal's connect, timeout and
 * reconnect paths (ptone/scion#4124).
 *
 * Each case mounts a real <scion-terminal-pane> bound to a real
 * TerminalSessionRegistry, so the production connect code (agent fetch,
 * PTY preflight, WebSocket, connect guards, reconnect jitter) runs
 * unmodified against an in-process stand-in for the Hub
 * (__fixtures__/terminal-smoke-server.ts) over real loopback HTTP and
 * WebSocket connections. Only the xterm renderer is replaced, since it needs
 * a real layout engine.
 *
 * The connect timeouts are production constants of 10 to 60 seconds. The
 * cases fake the clock that the terminal code reads (setTimeout, setInterval
 * and Date) and advance it explicitly, while the server's I/O stays real.
 *
 * Run on demand with `npm run test:terminal-smoke`; not part of `npm test`.
 */
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ScionTerminalPane } from '../components/terminal/terminal-pane.js';
import { PROMPT_RECONNECT_MAX_DELAY_MS, TerminalSessionRegistry } from './terminal-sessions.js';
import {
  flushIo,
  startSmokeHub,
  until,
  type SmokeHub,
} from './__fixtures__/terminal-smoke-server.js';

vi.mock('../utils/toast.js', () => ({ showToast: vi.fn() }));
vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    cols = 80;
    rows = 24;
    dispose = vi.fn();
    reset = vi.fn();
    write = vi.fn();
    focus = vi.fn();
    blur = vi.fn();
    refresh = vi.fn();
    parser = { registerOscHandler: vi.fn() };
    loadAddon = vi.fn();
    open = vi.fn();
    onData = vi.fn();
    onBinary = vi.fn();
    attachCustomKeyEventHandler = vi.fn();
  },
}));
vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class {
    fit = vi.fn();
  },
}));
vi.mock('@xterm/addon-web-links', () => ({ WebLinksAddon: class {} }));
vi.mock('@xterm/xterm/css/xterm.css?inline', () => ({ default: '' }));

const AGENT_ID = '11111111-1111-4111-8111-111111111111';
/** The production initial first-frame timeout (INITIAL_FIRST_FRAME_TIMEOUT_MS). */
const INITIAL_FIRST_FRAME_TIMEOUT_MS = 60_000;
/** The production pre-open timeout (OPEN_TIMEOUT_MS). */
const OPEN_TIMEOUT_MS = 10_000;

const realSetTimeout = globalThis.setTimeout;

let hub: SmokeHub;
let page: ScionTerminalPane;

beforeAll(async () => {
  await import('../components/terminal/terminal-pane.js');
});

beforeEach(async () => {
  hub = await startSmokeHub({
    agent: { id: AGENT_ID, name: 'smoke', phase: 'running', activity: 'idle' },
  });
  // Metadata live updates are not part of the connect path.
  vi.stubGlobal(
    'EventSource',
    class extends EventTarget {
      onopen: (() => void) | null = null;
      close(): void {}
    }
  );
  vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback): void => {
    realSetTimeout(() => callback(0), 0);
  });
  vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockReturnValue(800);
  vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockReturnValue(500);
  vi.useFakeTimers({
    toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date'],
  });
});

afterEach(async () => {
  page?.dispose();
  page?.remove();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  await hub.close();
});

/** Opens a pane for the agent, with the 4503 jitter draw fixed to `random`. */
function mountPane(random = 0.5): ScionTerminalPane {
  const registry = new TerminalSessionRegistry(
    { hubUrl: hub.hubUrl, accountId: 'smoke' },
    { random: () => random }
  );
  page = document.createElement('scion-terminal-pane');
  page.open(registry, AGENT_ID);
  document.body.append(page);
  return page;
}

function root(): ShadowRoot {
  return page.shadowRoot!;
}

/** The toolbar's connection status text. */
function statusText(): string {
  return root().querySelector('.status-indicator')?.textContent?.trim() ?? '';
}

/** Advances the faked clock, then lets the resulting I/O and renders settle. */
async function advance(ms: number): Promise<void> {
  await vi.advanceTimersByTimeAsync(ms);
  await flushIo();
  await page.updateComplete;
}

async function settled(): Promise<void> {
  await flushIo();
  await page.updateComplete;
}

describe('terminal connect smoke (#4124)', () => {
  it('cold start: a first frame that arrives late but inside the first-frame timeout connects', async () => {
    mountPane();
    await until(() => hub.sockets.some((s) => s.accepted), 'the PTY socket to open');
    await settled();
    expect(page.session!.state.connection).toBe('connecting');

    // A slow cold start: most of the initial first-frame window passes in silence.
    await advance(INITIAL_FIRST_FRAME_TIMEOUT_MS - 5_000);
    expect(page.session!.state.connection).toBe('connecting');

    hub.sockets[0].sendData('\x1b[2J$ ');
    await until(() => page.session!.state.connection === 'connected', 'the session to connect');
    await settled();
    expect(statusText()).toBe('Connected');
    expect(root().querySelector('.reconnect-btn')).toBeNull();
    expect(root().querySelector('.error-banner')).toBeNull();
  });

  it('silent socket: an open socket that never sends a frame ends in the error state with Retry', async () => {
    mountPane();
    await until(() => hub.sockets.some((s) => s.accepted), 'the PTY socket to open');
    await settled();

    await advance(INITIAL_FIRST_FRAME_TIMEOUT_MS - 1);
    expect(page.session!.state.connection).toBe('connecting');
    await advance(1);

    expect(page.session!.state.connection).toBe('disconnected');
    expect(statusText()).toBe('Disconnected');
    expect(root().querySelector('.error-banner')?.textContent).toContain(
      'No response from the terminal stream.'
    );
    await until(() => hub.sockets[0].ended, 'the client to drop the silent socket');
    await expectRetryDials();
  });

  it('never-open socket: an upgrade that is never answered ends in the error state with Retry', async () => {
    hub.upgradeMode = 'hold';
    mountPane();
    await until(() => hub.sockets.length === 1, 'the PTY upgrade request');
    await settled();
    expect(page.session!.state.connection).toBe('connecting');

    await advance(OPEN_TIMEOUT_MS - 1);
    expect(page.session!.state.connection).toBe('connecting');
    await advance(1);

    expect(hub.sockets[0].accepted).toBe(false);
    expect(page.session!.state.connection).toBe('disconnected');
    expect(statusText()).toBe('Disconnected');
    expect(root().querySelector('.error-banner')?.textContent).toContain(
      'No response from the terminal stream.'
    );
    await until(() => hub.sockets[0].ended, 'the client to abandon the upgrade');
    hub.upgradeMode = 'accept';
    await expectRetryDials();
  });

  it("hub preflight refusal: shows a terminal error with the hub's reason", async () => {
    const reason = 'The broker serving this agent has no attach path.';
    hub.refusePreflight(503, { error: { code: 'runtime_attach_unsupported', message: reason } });
    mountPane();
    await until(() => hub.preflights === 1, 'the PTY preflight');
    await until(
      () => root().querySelector('.error-state') !== null,
      'the pane to show the error state'
    );
    await settled();

    const errorState = root().querySelector('.error-state')!;
    expect(errorState.textContent).toContain('Terminal Unavailable');
    expect(errorState.querySelector('.error-detail')?.textContent).toBe(reason);
    const retry = errorState.querySelector('button')!;
    expect(retry.textContent?.trim()).toBe('Retry');
    expect(retry.disabled).toBe(false);
    expect(page.session!.state.disconnectReason).toBe('attach-unsupported');
    // A refused preflight never dials the socket.
    expect(hub.sockets).toHaveLength(0);
  });

  it('4503 close: redials after a jittered delay, showing RECONNECTING during the wait', async () => {
    // A jitter draw of 0.5 puts the redial halfway through the window.
    const delay = Math.floor(0.5 * (PROMPT_RECONNECT_MAX_DELAY_MS + 1));
    mountPane(0.5);
    await until(() => hub.sockets.some((s) => s.accepted), 'the PTY socket to open');
    hub.sockets[0].sendData('$ ');
    await until(() => page.session!.state.connection === 'connected', 'the session to connect');
    await settled();

    hub.sockets[0].closeWith(4503, 'relay restarting');
    await until(
      () => page.session!.state.disconnectReason === 'network',
      'the 4503 close to reach the session'
    );
    await settled();
    expect(page.session!.state.error).toBe('Connection closed (code: 4503)');
    const overlay = (): Element | null => root().querySelector('.disconnected-overlay');
    expect(overlay()?.querySelector('.overlay-title')?.textContent).toBe('RECONNECTING...');
    expect(overlay()?.classList.contains('reconnecting')).toBe(true);
    expect(page.session!.reconnecting).toBe(true);

    // Still waiting just before the jittered delay ends: no redial yet.
    await advance(delay - 1);
    await settled();
    expect(hub.sockets).toHaveLength(1);
    expect(overlay()?.querySelector('.overlay-title')?.textContent).toBe('RECONNECTING...');

    await advance(1);
    await until(() => hub.sockets.length === 2 && hub.sockets[1].accepted, 'the redial');
    expect(overlay()?.querySelector('.overlay-title')?.textContent).toBe('RECONNECTING...');
    hub.sockets[1].sendData('$ ');
    await until(() => page.session!.state.connection === 'connected', 'the session to reconnect');
    await settled();
    expect(statusText()).toBe('Connected');
    expect(overlay()).toBeNull();
  });
});

/**
 * The pane's retry action (the toolbar Reconnect button, shown once a
 * renderer exists) is enabled and starts a new attempt that dials again.
 */
async function expectRetryDials(): Promise<void> {
  const before = hub.sockets.length;
  const retry = root().querySelector<HTMLButtonElement>('.reconnect-btn');
  expect(retry?.textContent?.trim()).toBe('Reconnect');
  expect(retry?.disabled).toBe(false);
  retry!.click();
  await until(() => hub.sockets.length === before + 1, 'the retry to dial again');
}
