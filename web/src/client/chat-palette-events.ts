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
 * Event contract between the header's palette button and the chat page that
 * owns the quick switcher. The header renders as a sibling of the chat page
 * (slotted into `scion-chat-shell`, not an ancestor), so it has no direct
 * reference to call through — it dispatches this event instead, the same
 * decoupling `TERMINAL_SESSION_COUNT_EVENT` uses between the terminal
 * workspace and the header.
 */

/** Dispatched by the header's palette button to request the quick switcher open. */
export const CHAT_PALETTE_OPEN_REQUEST_EVENT = 'scion:chat-palette-open-request';
