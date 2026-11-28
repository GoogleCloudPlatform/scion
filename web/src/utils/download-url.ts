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
 * Shared helpers for forced-attachment file downloads (ptone/scion#8958, #9913).
 *
 * The HTML `download` attribute is ignored on cross-origin object-storage
 * redirects, so download links for artifacts and attachments must also carry
 * `?download=1` so the hub responds with `Content-Disposition: attachment`
 * (or signs the object-storage redirect with `response-content-disposition`).
 */

/**
 * Returns `url` with `download=1` set in its query string, preserving any
 * existing query parameters and hash fragment. Idempotent.
 */
export function withDownloadParam(url: string): string {
  const hashIdx = url.indexOf('#');
  const base = hashIdx >= 0 ? url.slice(0, hashIdx) : url;
  const hash = hashIdx >= 0 ? url.slice(hashIdx) : '';
  const qIdx = base.indexOf('?');
  if (qIdx < 0) {
    return `${base}?download=1${hash}`;
  }
  const params = new URLSearchParams(base.slice(qIdx + 1));
  params.set('download', '1');
  return `${base.slice(0, qIdx)}?${params.toString()}${hash}`;
}

/**
 * Triggers a browser file download for a forced-attachment URL via a hidden
 * `<a download>` element.
 */
export function triggerFileDownload(url: string, filename: string): void {
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  a.style.display = 'none';
  document.body.appendChild(a);
  try {
    a.click();
  } finally {
    a.remove();
  }
}
