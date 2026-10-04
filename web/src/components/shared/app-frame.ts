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
 * Shared "frame mode" helper for the app shells (app-shell, chat-shell,
 * profile-shell) and for the terminal workspace root, which is not a Lit
 * shell but has its own top-level visible/hidden state.
 *
 * Frame mode pins the document to `--scion-app-height` and stops it from
 * scrolling, via the `scion-app-frame` class on `<html>` — see the critical
 * CSS shared by `pkg/hub/web.go` and `web/index.html`. Document-scrolling
 * pages (login, invite, onboarding) live outside any shell and never call
 * this, so they keep the ordinary scrolling page.
 *
 * A Lit shell calls {@link enterAppFrame} from `connectedCallback` and
 * {@link exitAppFrame} from `disconnectedCallback`. The terminal workspace
 * root calls them on its own visibility transitions instead, since it is a
 * long-lived singleton rather than a component that connects and
 * disconnects. The module-level reference count means the class is only
 * removed once nothing needs it any more, which covers both a shell route
 * swap (the next shell connects before the previous one disconnects) and
 * the terminal workspace becoming visible while a shell is still mounted.
 */

/** The class on `<html>` that switches the document into frame mode. */
export const APP_FRAME_CLASS = 'scion-app-frame';

let refCount = 0;

/** Mark one more shell as needing frame mode. */
export function enterAppFrame(): void {
  refCount++;
  document.documentElement.classList.add(APP_FRAME_CLASS);
}

/**
 * Mark one shell as no longer needing frame mode. Once every shell that
 * called {@link enterAppFrame} has called this, the class is removed.
 */
export function exitAppFrame(): void {
  refCount = Math.max(0, refCount - 1);
  if (refCount === 0) {
    document.documentElement.classList.remove(APP_FRAME_CLASS);
  }
}

/** Test-only accessor for the current reference count. */
export function _appFrameRefCountForTests(): number {
  return refCount;
}
