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

import { isMacPlatform } from '../../utils/platform.js';

/** Input types that take typed text, where a line-editing key has a native meaning. */
const TEXT_INPUT_TYPES: ReadonlySet<string> = new Set([
  'text',
  'search',
  'email',
  'url',
  'tel',
  'password',
  'number',
]);

/**
 * Whether `target` is an editable text field: a text-taking input, a
 * textarea, or contenteditable content. Read-only and disabled fields are
 * not editable.
 */
export function isEditableTextField(target: EventTarget | null): boolean {
  if (target instanceof HTMLInputElement) {
    return TEXT_INPUT_TYPES.has(target.type) && !target.readOnly && !target.disabled;
  }
  if (target instanceof HTMLTextAreaElement) return !target.readOnly && !target.disabled;
  return target instanceof HTMLElement && target.isContentEditable;
}

/**
 * Whether `e` is a Ctrl chord that belongs to the text field it was typed
 * in: on macOS, Ctrl+K and its kin are line-editing keys in an editable
 * text field (Ctrl+K deletes to the end of the line), and a shortcut there
 * uses Cmd instead. Reads `composedPath()[0]`, since a document listener
 * sees a field inside a shadow root retargeted to its host.
 */
export function isMacTextFieldCtrlKey(e: KeyboardEvent): boolean {
  return e.ctrlKey && isMacPlatform() && isEditableTextField(e.composedPath()[0] ?? null);
}
