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
 * Event contract between the chat page's quick-switcher buttons (pinned
 * below its spaces rail, and below its members column on phones) and the
 * page's open-request handler. The buttons dispatch this event rather than
 * calling the page directly, so the request goes through the same document
 * listener and guards (route, page visibility, unrelated modal) however it
 * is raised; the unit and e2e tests raise it the same way.
 */

/** Dispatched by the chat page's quick-switcher buttons to request the quick switcher open. */
export const CHAT_PALETTE_OPEN_REQUEST_EVENT = 'scion:chat-palette-open-request';
