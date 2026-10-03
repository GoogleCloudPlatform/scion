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
 * Event contract between the header's palette button and the terminal
 * workspace root that owns the "Jump to agent" palette — the terminal-view
 * counterpart of `chat-palette-events.ts`'s `CHAT_PALETTE_OPEN_REQUEST_EVENT`.
 * The terminal workspace mounts its own `scion-header` instance (see
 * `TerminalWorkspaceRoot`), so this is a sibling, not an ancestor, of the
 * button that dispatches it — same decoupling reason as the chat event.
 */

/** Dispatched by the header's palette button, on `/terminals`, to request the agents-only palette open. */
export const TERMINAL_PALETTE_OPEN_REQUEST_EVENT = 'scion:terminal-palette-open-request';
