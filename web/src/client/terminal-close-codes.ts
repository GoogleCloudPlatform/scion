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
 * PTY WebSocket close-code contract (ptone/scion#1811).
 *
 * TypeScript mirror of pkg/wsprotocol/pty_close.go. Both classifiers are
 * checked against pkg/wsprotocol/testdata/pty_close_codes.json; change all
 * three together.
 *
 * Export only: nothing in the web client consumes this yet.
 */

export const PTY_CLOSE = {
  /** Clean detach; the tmux session still exists. Do not retry. */
  NORMAL: 1000,
  /** Server shutting down. Not emitted today. Retry. */
  GOING_AWAY: 1001,
  /** Connection dropped without a close frame (client-synthesized). Retry. */
  ABNORMAL: 1006,
  /** Unexpected server error or unrecognised broker code. Retry. */
  INTERNAL_ERROR: 1011,
  /** Reserved for a future graceful drain. Retry. */
  SERVICE_RESTART: 1012,
  /** Overload. Not emitted today. Retry. */
  TRY_AGAIN_LATER: 1013,
  /** Credentials no longer valid. Reserved. Terminal. */
  AUTH_REQUIRED: 4401,
  /** Attach permission revoked. Reserved. Terminal. */
  FORBIDDEN: 4403,
  /** Broker cannot find the agent or its container. Terminal. */
  AGENT_NOT_FOUND: 4404,
  /** tmux session no longer exists. Terminal. */
  SESSION_GONE: 4410,
  /** The hop behind the Hub is temporarily unavailable. Retry. */
  UPSTREAM_UNAVAILABLE: 4503,
  /** Broker produced no first output in time. Reserved. Retry. */
  UPSTREAM_TIMEOUT: 4504,
} as const;

export type PtyCloseDisposition = 'detached' | 'retry' | 'terminal';

/** Maps a PTY WebSocket close code to what the client should do next. */
export function classifyPtyClose(code: number): PtyCloseDisposition {
  if (code === PTY_CLOSE.NORMAL) return 'detached';
  if (
    code === PTY_CLOSE.AUTH_REQUIRED ||
    code === PTY_CLOSE.FORBIDDEN ||
    code === PTY_CLOSE.AGENT_NOT_FOUND ||
    code === PTY_CLOSE.SESSION_GONE
  )
    return 'terminal';
  if (code === PTY_CLOSE.UPSTREAM_UNAVAILABLE || code === PTY_CLOSE.UPSTREAM_TIMEOUT)
    return 'retry';
  // Unknown application code: fail safe and do not hammer the server.
  if (code >= 4000 && code <= 4999) return 'terminal';
  // Protocol and policy errors will not fix themselves.
  if ([1002, 1003, 1007, 1008, 1009, 1010].includes(code)) return 'terminal';
  // 1001, 1005, 1006, 1011-1015, and anything else.
  return 'retry';
}
