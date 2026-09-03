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
 * Browser-compatible file download helper.
 *
 * Creates a Blob from the provided data, synthesises an invisible anchor
 * element with a `download` attribute, clicks it to trigger a browser
 * save-as dialog, and cleans up the object URL after a short delay so
 * the browser has time to initiate the download.
 */

/**
 * Trigger a browser file download for a JSON payload.
 *
 * @param data - Any JSON-serialisable value.
 * @param filename - The suggested filename for the download.
 * @throws If Blob creation, object-URL generation, or anchor interaction
 *         fails. DOM removal and URL revocation are guaranteed even when
 *         an error is thrown.
 */
export function downloadJsonFile(data: unknown, filename: string): void {
  const json = JSON.stringify(data, null, 2);
  const blob = new Blob([json], { type: 'application/json' });
  const url = URL.createObjectURL(blob);

  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = filename;

  // Track whether the click succeeded so we can choose between
  // deferred revocation (happy path) and immediate revocation (error).
  let clicked = false;

  try {
    // Append to body so the click works in all browsers (Firefox
    // requires the anchor to be in the DOM).
    document.body.appendChild(anchor);
    anchor.click();
    clicked = true;
  } finally {
    // Always remove the anchor from the DOM if it was appended.
    if (anchor.parentNode) {
      anchor.parentNode.removeChild(anchor);
    }

    if (clicked) {
      // Happy path: revoke after a short delay so the browser can
      // start the download before the object URL is invalidated.
      setTimeout(() => URL.revokeObjectURL(url), 150);
    } else {
      // Error path: revoke immediately since no download started.
      URL.revokeObjectURL(url);
    }
  }
}
