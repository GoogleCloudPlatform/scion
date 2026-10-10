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

// Node-only test fixture: the terminal test projects' types are browser-only.
/// <reference types="node" />

/**
 * An in-process stand-in for the Hub's terminal endpoints, for the terminal
 * connect smoke checks (terminal-connect.smoke.test.ts). It serves the agent
 * GET and the PTY preflight over HTTP, and the PTY WebSocket upgrade, with a
 * per-test choice of how each step behaves: answer, refuse, hold the
 * upgrade unanswered, stay silent after opening, send a frame on demand, or
 * close with a given code.
 *
 * It speaks just enough of RFC 6455 for these checks (the handshake, text
 * and close frames from the server, and the client's close frame); it is not
 * a general WebSocket server. It uses no timers, so a test can fake the
 * clock that the terminal code reads while this server's I/O stays real.
 */

import { createHash } from 'node:crypto';
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import type { AddressInfo, Socket } from 'node:net';

const WS_GUID = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11';

/** The real timer functions, captured before a test can fake them. */
const realSetTimeout = globalThis.setTimeout;
const realSetImmediate = globalThis.setImmediate;
const realNow = performance.now.bind(performance);

/** How the server answers a PTY WebSocket upgrade. */
export type UpgradeMode =
  /** Complete the handshake; the test then drives the socket. */
  | 'accept'
  /** Never answer the upgrade request, so the socket never opens. */
  | 'hold';

/** One PTY upgrade request the server received, in arrival order. */
export interface SmokeSocket {
  readonly path: string;
  /** Whether the handshake was completed (the client saw the socket open). */
  readonly accepted: boolean;
  /** Whether the client has closed or dropped the connection. */
  readonly ended: boolean;
  /** Sends a terminal data frame, as tmux's redraw on attach would. */
  sendData(text: string): void;
  /** Sends a close frame with the given code and reason, then ends the connection. */
  closeWith(code: number, reason?: string): void;
}

export interface SmokeHubOptions {
  /** The agent the GET /api/v1/agents/<id> route returns. */
  agent: { id: string; name: string; phase: string; activity?: string };
}

export interface SmokeHub {
  /** HTTP origin and base path for TerminalScope.hubUrl. */
  readonly hubUrl: string;
  /** Every PTY upgrade received so far. */
  readonly sockets: readonly SmokeSocket[];
  /** How later upgrades are answered (default 'accept'). */
  upgradeMode: UpgradeMode;
  /** Answer later PTY preflights with this status and JSON body (default 200 {}). */
  refusePreflight(status: number, body: unknown): void;
  /** Number of PTY preflight requests received. */
  readonly preflights: number;
  close(): Promise<void>;
}

function frame(opcode: number, payload: Buffer): Buffer {
  const length = payload.length;
  let header: Buffer;
  if (length < 126) {
    header = Buffer.from([0x80 | opcode, length]);
  } else if (length < 65536) {
    header = Buffer.alloc(4);
    header[0] = 0x80 | opcode;
    header[1] = 126;
    header.writeUInt16BE(length, 2);
  } else {
    header = Buffer.alloc(10);
    header[0] = 0x80 | opcode;
    header[1] = 127;
    header.writeBigUInt64BE(BigInt(length), 2);
  }
  return Buffer.concat([header, payload]);
}

function closeFrame(code: number, reason = ''): Buffer {
  const payload = Buffer.alloc(2 + Buffer.byteLength(reason));
  payload.writeUInt16BE(code, 0);
  payload.write(reason, 2);
  return frame(0x8, payload);
}

/** Returns the opcodes of the complete client frames in buffer, and the unread rest. */
function readClientFrames(buffer: Buffer): { opcodes: number[]; rest: Buffer } {
  const opcodes: number[] = [];
  let offset = 0;
  while (buffer.length - offset >= 2) {
    const opcode = buffer[offset] & 0x0f;
    let length = buffer[offset + 1] & 0x7f;
    const masked = (buffer[offset + 1] & 0x80) !== 0;
    let headerLength = 2;
    if (length === 126) {
      if (buffer.length - offset < 4) break;
      length = buffer.readUInt16BE(offset + 2);
      headerLength = 4;
    } else if (length === 127) {
      if (buffer.length - offset < 10) break;
      length = Number(buffer.readBigUInt64BE(offset + 2));
      headerLength = 10;
    }
    const total = headerLength + (masked ? 4 : 0) + length;
    if (buffer.length - offset < total) break;
    opcodes.push(opcode);
    offset += total;
  }
  return { opcodes, rest: buffer.subarray(offset) };
}

function corsHeaders(request: IncomingMessage): Record<string, string> {
  const origin = request.headers.origin;
  return origin
    ? {
        'access-control-allow-origin': origin,
        'access-control-allow-credentials': 'true',
        vary: 'Origin',
      }
    : {};
}

/** Starts a server on an ephemeral loopback port. */
export async function startSmokeHub(options: SmokeHubOptions): Promise<SmokeHub> {
  const sockets: Array<SmokeSocket & { destroy(): void }> = [];
  let preflight: { status: number; body: unknown } = { status: 200, body: {} };
  let preflights = 0;
  const agentPath = `/api/v1/agents/${options.agent.id}`;

  const server = createServer((request: IncomingMessage, response: ServerResponse): void => {
    const path = new URL(request.url ?? '/', 'http://localhost').pathname;
    const send = (status: number, body: unknown): void => {
      response.writeHead(status, { 'content-type': 'application/json', ...corsHeaders(request) });
      response.end(JSON.stringify(body));
    };
    if (request.method === 'OPTIONS') {
      response.writeHead(204, {
        ...corsHeaders(request),
        'access-control-allow-methods': 'GET',
        'access-control-allow-headers': request.headers['access-control-request-headers'] ?? '',
      });
      response.end();
    } else if (path === agentPath) {
      send(200, options.agent);
    } else if (path === `${agentPath}/pty`) {
      preflights++;
      send(preflight.status, preflight.body);
    } else {
      send(404, { error: { code: 'not_found', message: 'Not found' } });
    }
  });

  const hub: SmokeHub = {
    hubUrl: '',
    sockets,
    upgradeMode: 'accept',
    refusePreflight(status, body): void {
      preflight = { status, body };
    },
    get preflights(): number {
      return preflights;
    },
    close: () =>
      new Promise<void>((resolve) => {
        for (const socket of sockets) socket.destroy();
        server.close(() => resolve());
        server.closeAllConnections();
      }),
  };

  server.on('upgrade', (request: IncomingMessage, socket: Socket): void => {
    const path = new URL(request.url ?? '/', 'http://localhost').pathname;
    let accepted = false;
    let ended = false;
    let pending = Buffer.alloc(0);
    socket.on('close', () => {
      ended = true;
    });
    // The HTTP server allows half-open sockets, so a client that drops the
    // connection shows up as 'end' first; finish closing our side too.
    socket.on('end', () => {
      ended = true;
      socket.end();
    });
    socket.on('error', () => {
      ended = true;
    });
    const entry: SmokeSocket & { destroy(): void } = {
      path,
      get accepted(): boolean {
        return accepted;
      },
      get ended(): boolean {
        return ended;
      },
      sendData(text: string): void {
        const message = JSON.stringify({
          type: 'data',
          data: Buffer.from(text).toString('base64'),
        });
        socket.write(frame(0x1, Buffer.from(message)));
      },
      closeWith(code: number, reason = ''): void {
        socket.end(closeFrame(code, reason));
      },
      destroy(): void {
        socket.destroy();
      },
    };
    sockets.push(entry);
    if (path !== `${agentPath}/pty` || hub.upgradeMode === 'hold') return;
    const key = request.headers['sec-websocket-key'] ?? '';
    const acceptKey = createHash('sha1')
      .update(key + WS_GUID)
      .digest('base64');
    socket.write(
      'HTTP/1.1 101 Switching Protocols\r\n' +
        'Upgrade: websocket\r\n' +
        'Connection: Upgrade\r\n' +
        `Sec-WebSocket-Accept: ${acceptKey}\r\n\r\n`
    );
    accepted = true;
    socket.on('data', (chunk: Buffer) => {
      const { opcodes, rest } = readClientFrames(Buffer.concat([pending, chunk]));
      pending = rest;
      // Answer the client's close frame, completing the close handshake.
      if (opcodes.includes(0x8) && !socket.writableEnded) socket.end(closeFrame(1000));
    });
  });

  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', () => resolve()));
  const { port } = server.address() as AddressInfo;
  (hub as { hubUrl: string }).hubUrl = `http://127.0.0.1:${port}/`;
  return hub;
}

/**
 * Waits, in real time, until condition() holds, letting real I/O run in
 * between. It never touches the (possibly faked) clock the terminal reads.
 */
export async function until(
  condition: () => boolean,
  what: string,
  timeoutMs = 5_000
): Promise<void> {
  const deadline = realNow() + timeoutMs;
  while (!condition()) {
    if (realNow() > deadline) throw new Error(`Timed out waiting for ${what}.`);
    await new Promise<void>((resolve) => realSetTimeout(resolve, 5));
  }
}

/** Lets pending real I/O callbacks and microtasks run once. */
export function flushIo(): Promise<void> {
  return new Promise<void>((resolve) => realSetImmediate(resolve));
}
