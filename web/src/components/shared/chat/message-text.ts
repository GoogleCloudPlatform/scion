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
 * Text shown for a chat message where its own text is expected.
 *
 * An attachment-only message (ptone/scion#6146) is stored with no text, so
 * places that show a message's text show ATTACHMENT_ONLY_LABEL instead:
 * the composer reply bar and desktop notifications. The hub uses the same
 * word for reply previews and starred-message excerpts.
 */

/** The label of a message that has attachments and no text. */
export const ATTACHMENT_ONLY_LABEL = 'Attachment';

/** Longest reply bar text, in UTF-16 code units, before "..." is appended. */
const REPLY_TO_MAX = 100;

/**
 * The composer reply bar text for a message with text `text`: the text,
 * shortened to REPLY_TO_MAX with "...", or ATTACHMENT_ONLY_LABEL when the
 * message has no text.
 */
export function replyToContent(text: string | undefined | null): string {
  const t = text ?? '';
  if (!t.trim()) return ATTACHMENT_ONLY_LABEL;
  return t.length > REPLY_TO_MAX ? t.slice(0, REPLY_TO_MAX) + '...' : t;
}
